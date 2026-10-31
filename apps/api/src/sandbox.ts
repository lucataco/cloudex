// One Durable Object per sandbox. It owns the container's whole lifecycle and
// is the source of truth for its state; D1 only mirrors it for listing.
//
// Rules from the Phase 0 spikes:
// - Every container call gets a deadline: start() hangs while a new image propagates.
// - setInactivityTimeout only after the box is ready. Alarms and guest pings
//   don't extend it, so it is set to the platform maximum and this object stops
//   idle boxes itself from an alarm.
// - Save (snapshotContainer) before destroy(); snapshots need a running box.
// - On SIGTERM from the platform the guest asks for a save through the outbound
//   handler (see egress.ts); the host allows 15 minutes.
// - After each save, /home is also backed up to R2 when the deployment has a
//   bucket (backup.ts). Saves expire; backups don't, and restore onto any image.
// - The desktop (desktop.ts) is installed and started on demand by a script, so
//   it works on any image.
// - AI provider keys (keys.ts) are set up at each start for the providers that
//   have one then: the sandbox gets placeholders, and the real keys are added
//   to its HTTPS calls to those providers on the way out.
// - Settings (settings.ts) are kept sealed here and applied at every start,
//   after the keys; a repo in them is cloned once, when the sandbox is created.
// - Agents started through the API (agents.ts) run and keep their state in the
//   guest; this object only passes each call to a script there.
// - Cloudex's own tools in the guest (guest.ts: the localhost relay, the
//   cloudex command, notes for agents) are put there at each start, after the
//   settings, then the user's ~/.cloudex/start runs.
// - Preview link tokens are checked here, against the links revoked so far.
import { DurableObject } from "cloudflare:workers";
import type {
  Desktop,
  ExecResult,
  Repo,
  Sandbox as SandboxRecord,
  SandboxSettings,
  SandboxSize,
  UpdateSandbox,
  UpdateSettings,
} from "@cloudex/schema";
import {
  BACKUP_SCRIPT,
  type BackupManifest,
  HELPER,
  RESTORE_SCRIPT,
  backupBucket,
  deleteBackups,
  deleteGeneration,
  latestBackup,
  writeManifest,
} from "./backup";
import { AGENT_BAD_REQUEST, AGENT_BOOT, AGENT_NOT_FOUND, AGENT_SCRIPT } from "./agents";
import { deleteSandbox, renameSandbox, upsertSandbox } from "./db";
import { DESKTOP_PORT, DESKTOP_SCRIPT, DESKTOP_STOP_SCRIPT, DESKTOP_VIEWER } from "./desktop";
import { PLATFORM_FILES, PLATFORM_SCRIPT, PLATFORM_VERSION, RELAY_NOTHING, RELAY_SCRIPT, START_SCRIPT } from "./guest";
import { KEYS_SCRIPT, PROVIDERS, listKeys } from "./keys";
import {
  CLONE_SCRIPT,
  SETTINGS_SCRIPT,
  type StoredSettings,
  UnreadableSettings,
  merge,
  readSecret,
  repoSpec,
  store,
  view,
} from "./settings";
import { LINK_HEADER, errorStatus, fail, previewToken, sameSecret, sleep, withTimeout } from "./util";

const READY_CHECK = '[ "$(cat /run/cloudex-ready 2>/dev/null)" = "$(cat /proc/sys/kernel/random/boot_id)" ]';
const READY_TIMEOUT_MS = 10 * 60_000; // a host without the image pulls it first
const SAVE_TIMEOUT_MS = 15 * 60_000; // 15 GiB took 179 s in the spikes
const BACKUP_TIMEOUT_MS = 20 * 60_000; // about 110 MiB/s each way in the spikes
const DESKTOP_TIMEOUT_MS = 10 * 60_000; // the first start installs about 950 MB
// A save expires 30 days after it was made or last restored; past this, resume
// restores the backup instead of trying the save.
const SAVE_LIFETIME_MS = 29 * 24 * 3600_000;
const PLATFORM_MAX_INACTIVITY_MS = 6 * 3600_000;
const ALARM_EVERY_MS = 60_000;
const GUEST_USER = "1000:1000"; // the image's cloudex user
const GUEST_HOME = "/home/cloudex";
const OUTPUT_LIMIT = 1 << 20;
const SSH_KEY = /^(ssh-ed25519|ecdsa-sha2-nistp(256|384|521)|ssh-rsa) [A-Za-z0-9+/]+={0,3}( [^\n]*)?$/;

type CreateInput = {
  name: string | null;
  size: SandboxSize;
  idleMinutes: number;
  env?: Record<string, string>;
  files?: Record<string, string>;
  repo?: string | Repo;
};
export type ForkSource = { sandbox: string; snapshot: { id: string }; settings?: StoredSettings };
// Preview links handed out: a version for all of them, which revoking them all
// bumps, and one per port, bumped by revoking that port's link. at is when the
// port's current link was first handed out, null once revoked.
type Links = { epoch: number; ports: Record<string, { gen: number; at: number | null }> };
type ExecInput = { cmd: string; root: boolean; cwd?: string; timeoutSec: number };

const message = (e: unknown) => (e instanceof Error ? e.message : String(e)).replace(/^\[\d{3}\] /, "");

function clip(buf: ArrayBuffer) {
  const text = new TextDecoder().decode(buf.byteLength > OUTPUT_LIMIT ? buf.slice(0, OUTPUT_LIMIT) : buf);
  return buf.byteLength > OUTPUT_LIMIT ? `${text}\n[cloudex: output cut at 1 MiB of ${buf.byteLength} bytes]` : text;
}

// How long exec keeps reading output after the command exits. Something it
// started in the background, such as `npm run dev &`, holds the output open
// for as long as it runs, and exec doesn't wait for that.
const EXIT_GRACE_MS = 500;

// Reads a process's stdout and stderr until it exits, then for EXIT_GRACE_MS
// more, keeping the first OUTPUT_LIMIT bytes of each.
async function readOutput(p: ExecProcess) {
  let stop!: () => void;
  const stopped = new Promise<void>((resolve) => (stop = resolve));
  const read = async (stream: ReadableStream | null) => {
    const kept: Uint8Array[] = [];
    let size = 0;
    let total = 0;
    if (stream) {
      const reader = stream.getReader();
      void stopped.then(() => reader.cancel().catch(() => {}));
      try {
        for (;;) {
          const { done, value } = await reader.read();
          if (done) break;
          const chunk = value instanceof Uint8Array ? value : new Uint8Array(value as ArrayBuffer);
          total += chunk.byteLength;
          if (size < OUTPUT_LIMIT) {
            const part = chunk.subarray(0, OUTPUT_LIMIT - size);
            kept.push(part);
            size += part.byteLength;
          }
        }
      } catch {
        // cancelled after the grace period
      }
    }
    const text = new TextDecoder().decode(await new Blob(kept).arrayBuffer());
    return total > OUTPUT_LIMIT ? `${text}\n[cloudex: output cut at 1 MiB of ${total} bytes]` : text;
  };
  const output = Promise.all([read(p.stdout), read(p.stderr)]);
  let timer: ReturnType<typeof setTimeout> | null = null;
  try {
    const exitCode = await p.exitCode;
    timer = setTimeout(stop, EXIT_GRACE_MS);
    const [stdout, stderr] = await output;
    return { exitCode, stdout, stderr };
  } finally {
    if (timer) clearTimeout(timer);
    stop();
  }
}

export class Sandbox extends DurableObject<Env> {
  private record: SandboxRecord | undefined;
  private queue: Promise<unknown> = Promise.resolve();
  private lastActivity = Date.now();
  private lastActivitySaved = 0;
  private exitedAt = 0;
  // Guest ports that listen only on localhost, and the port of the relay for
  // each, in this run.
  private relayed = new Map<number, number>();
  // How to end each WebSocket and streamed answer open through a preview link,
  // per guest port, so that revoking a link ends the connections it let in, a
  // desktop's too. revocations counts revokes, for requests in flight meanwhile.
  private open = new Map<number, Set<() => void>>();
  private revocations = 0;
  // A reinstall of the guest tools that a preview started (see relay).
  private platformFix: Promise<void> | undefined;
  // Set while remove() runs, so that nothing outside the lifecycle queue
  // writes the record back.
  private removing = false;

  constructor(ctx: DurableObjectState, env: Env) {
    super(ctx, env);
    void ctx.blockConcurrencyWhile(async () => {
      this.record = await ctx.storage.get<SandboxRecord>("sandbox");
      this.lastActivity = (await ctx.storage.get<number>("lastActivity")) ?? Date.now();
      // After an eviction or deploy, reattach the exit watcher to a running box.
      if (ctx.container?.running) this.watch();
    });
  }

  private get container() {
    const c = this.ctx.container;
    if (!c) throw new Error("no container is configured for this Durable Object");
    return c;
  }

  private mustRecord(): SandboxRecord {
    if (!this.record || this.removing) fail(404, "no such sandbox");
    return this.record;
  }

  // The record of a running sandbox, for work inside it, which counts as activity.
  private mustRun(): SandboxRecord {
    const r = this.mustRecord();
    if (r.state !== "running") fail(409, `${r.name ?? r.id} is ${r.state}; resume it first`);
    this.touch();
    return r;
  }

  // Lifecycle changes run one at a time.
  private exclusive<T>(fn: () => Promise<T>): Promise<T> {
    const run = this.queue.then(fn, fn);
    this.queue = run.catch(() => {});
    return run;
  }

  private async update(patch: Partial<SandboxRecord>) {
    const r = { ...this.mustRecord(), ...patch, updatedAt: Date.now() };
    this.record = r;
    await this.ctx.storage.put("sandbox", r);
    try {
      await upsertSandbox(this.env.DB, r);
    } catch (e) {
      console.error(`[${r.id}] index update failed: ${message(e)}`);
    }
  }

  private touch() {
    this.lastActivity = Date.now();
    if (this.lastActivity - this.lastActivitySaved > 30_000) {
      this.lastActivitySaved = this.lastActivity;
      void this.ctx.storage.put("lastActivity", this.lastActivity);
    }
  }

  private watch() {
    this.container.monitor().then(
      () => this.onExit(),
      (e) => this.onExit(e),
    );
  }

  // Runs whenever the container stops. Stops we start ourselves have already
  // moved the state on, so only an unexpected exit of a running box acts here.
  private async onExit(err?: unknown) {
    this.exitedAt = Date.now();
    const r = this.record;
    if (!r || r.state !== "running") return;
    const savedThisRun = (r.savedAt ?? 0) >= (r.startedAt ?? 0);
    console.log(`[${r.id}] container exited${err ? `: ${message(err)}` : ""}; saved this run: ${savedThisRun}`);
    await this.ctx.storage.deleteAlarm();
    await this.update({
      state: "stopped",
      stoppedAt: Date.now(),
      error: savedThisRun ? null : "stopped by the platform before it could save; resume starts from the previous save",
    });
  }

  // With from, the sandbox is a fork and starts from that save (see forkPoint).
  async create(id: string, input: CreateInput, from?: ForkSource): Promise<SandboxRecord> {
    return this.exclusive(async () => {
      if (this.record) fail(409, `${id} already exists`);
      // A fork gets the original's settings; checked before anything is kept.
      const settings =
        from?.settings ??
        (input.env || input.files || input.repo
          ? await store(
              this.env.ADMIN_KEY,
              merge({ env: {}, files: {} }, { env: input.env, files: input.files }),
              input.repo ? repoSpec(input.repo) : null,
            )
          : undefined);
      const now = Date.now();
      this.record = {
        id,
        name: input.name,
        size: input.size,
        state: "starting",
        idleMinutes: input.idleMinutes,
        createdAt: now,
        updatedAt: now,
        startedAt: null,
        stoppedAt: null,
        savedAt: null,
        savedBytes: null,
        error: null,
        ...(from ? { forkedFrom: from.sandbox } : {}),
      };
      await this.update({});
      if (settings) await this.ctx.storage.put("settings", settings);
      if (from) {
        // The fork's save until it makes its own, so that a resume after a
        // failed start, or after the platform stops it first, starts here too.
        await this.ctx.storage.put("snapshot", from.snapshot);
        await this.ctx.storage.put("saveTouchedAt", Date.now());
      }
      await this.boot(from?.snapshot);
      if (settings?.repo && !from) await this.clone(settings.repo);
      await this.startHook();
      return this.mustRecord();
    });
  }

  private async clone(repo: NonNullable<StoredSettings["repo"]>) {
    const t0 = Date.now();
    const out = await this.asGuest([CLONE_SCRIPT, repo.url, repo.dir, repo.branch ?? ""], "cloning", 10 * 60_000);
    const why = new TextDecoder().decode(out.stderr).trim().split("\n").slice(-2).join(" ");
    if (out.exitCode === 0) {
      console.log(`[${this.mustRecord().id}] cloned ${repo.url} in ${Date.now() - t0} ms${why ? `: ${why}` : ""}`);
      return;
    }
    // GitHub asks for a login for a private repo, and for one that isn't there.
    const hint = /could not read Username/.test(why) ? " If it's private, set GH_TOKEN in the sandbox's env." : "";
    const notice = `Couldn't clone ${repo.url} into ~/${repo.dir}: ${why || `git exited with code ${out.exitCode}`}.${hint}`;
    await this.update({ notice: [this.mustRecord().notice, notice].filter(Boolean).join(" ") });
  }

  // Names and paths only; values and contents stay sealed.
  async settings(): Promise<SandboxSettings> {
    this.mustRecord();
    return view(await this.ctx.storage.get<StoredSettings>("settings"));
  }

  // Applied at once to a running sandbox; a stopped one gets them at its next start.
  async updateSettings(change: UpdateSettings): Promise<SandboxSettings> {
    return this.exclusive(async () => {
      const r = this.mustRecord();
      const stored = await this.ctx.storage.get<StoredSettings>("settings");
      // Settings sealed under an earlier admin key are lost; start over.
      const secret = await readSecret(this.env.ADMIN_KEY, stored).catch((e) => {
        if (e instanceof UnreadableSettings) return { env: {}, files: {} };
        throw e;
      });
      const next = await store(this.env.ADMIN_KEY, merge(secret, change), stored?.repo ?? null);
      await this.ctx.storage.put("settings", next);
      if (r.state === "running") {
        this.touch();
        await this.applySettings();
        await this.applyPlatform().catch((e) => console.error(`[${r.id}] ${message(e)}`));
      }
      return view(next);
    });
  }

  private async applySettings() {
    const secret = await readSecret(this.env.ADMIN_KEY, await this.ctx.storage.get<StoredSettings>("settings"));
    await this.runScript(SETTINGS_SCRIPT, [], "applying the settings", 120_000, JSON.stringify(secret));
  }

  // Cloudex's tools in the guest (guest.ts). Recorded, so that a sandbox already
  // running when the deployment is updated gets the new ones (see alarm).
  private async applyPlatform() {
    const zone = this.env.PREVIEW_ZONE || "";
    const selfToken = (await this.ctx.storage.get<string>("selfToken")) ?? "";
    await this.runScript(PLATFORM_SCRIPT, [zone, selfToken], "setting up the cloudex command", 60_000, PLATFORM_FILES);
    await this.ctx.storage.put("platform", `${PLATFORM_VERSION}:${zone}`);
  }

  // The user's ~/.cloudex/start, if there is one, in the background.
  private async startHook() {
    const r = this.mustRecord();
    try {
      const out = await this.asGuest([START_SCRIPT], "starting ~/.cloudex/start", 60_000);
      if (out.exitCode !== 0) throw new Error(new TextDecoder().decode(out.stderr).trim().split("\n").pop() || `exit ${out.exitCode}`);
    } catch (e) {
      console.error(`[${r.id}] ~/.cloudex/start: ${message(e)}`);
      await this.update({ notice: [this.mustRecord().notice, `~/.cloudex/start didn't run: ${message(e)}.`].filter(Boolean).join(" ") });
    }
  }

  // The disk a fork starts from: a fresh save of a running sandbox, which keeps
  // running (the filesystem freezes for a moment), or a stopped one's last save.
  async forkPoint(): Promise<ForkSource> {
    return this.exclusive(async () => {
      const r = this.mustRecord();
      if (r.state === "running") {
        this.touch();
        const t0 = Date.now();
        const snap = await withTimeout(
          this.container.snapshotContainer({ name: `${r.id}-fork-${t0}` }),
          SAVE_TIMEOUT_MS,
          "saving the disk",
        );
        console.log(`[${r.id}] saved ${snap.size} bytes for a fork in ${Date.now() - t0} ms`);
        return { sandbox: r.id, snapshot: { id: snap.id }, settings: await this.ctx.storage.get<StoredSettings>("settings") };
      }
      if (r.state !== "stopped" && r.state !== "error") fail(409, `${r.id} is ${r.state}; try again when it has finished`);
      const snapshot = await this.ctx.storage.get<{ id: string }>("snapshot");
      if (!snapshot) fail(409, `${r.id} has no saved disk to fork; resume it, then fork it`);
      const touched = (await this.ctx.storage.get<number>("saveTouchedAt")) ?? r.savedAt ?? 0;
      if (Date.now() - touched > SAVE_LIFETIME_MS) {
        fail(409, `${r.id}'s save has expired; resume it (which restores its backup), then fork it`);
      }
      return { sandbox: r.id, snapshot: { id: snapshot.id }, settings: await this.ctx.storage.get<StoredSettings>("settings") };
    });
  }

  async get(): Promise<SandboxRecord> {
    return this.mustRecord();
  }

  // With size, the sandbox runs on that size from now on.
  async resume(fromBackup = false, size?: SandboxSize): Promise<SandboxRecord> {
    return this.exclusive(async () => {
      const r = this.mustRecord();
      if (r.state === "running") {
        if (size && size !== r.size) fail(409, `${r.name ?? r.id} is running on ${r.size}; stop it, then resume it on ${size}`);
        return r;
      }
      if (r.state !== "stopped" && r.state !== "error") fail(409, `${r.id} is ${r.state}`);
      if (!size || size === r.size) return this.resumeFrom(fromBackup);
      await this.update({ size });
      try {
        return await this.resumeFrom(fromBackup);
      } catch (e) {
        // One that didn't start on the new size stays on its old one.
        await this.update({ size: r.size });
        const { status, message: why } = errorStatus(e);
        fail(status, `${why}. It stays on ${r.size}`);
      }
    });
  }

  private async resumeFrom(fromBackup: boolean): Promise<SandboxRecord> {
    const r = this.mustRecord();
    const snapshot = await this.ctx.storage.get("snapshot");
    const touched = (await this.ctx.storage.get<number>("saveTouchedAt")) ?? r.savedAt ?? 0;
    const expired = Boolean(snapshot) && Date.now() - touched > SAVE_LIFETIME_MS;
    if (fromBackup || expired) {
      const bucket = backupBucket(this.env);
      const backup = bucket ? await latestBackup(bucket, r.id) : null;
      if (backup) {
        await this.bootFromBackup(backup, fromBackup ? "as asked" : "its save had expired");
        await this.startHook();
        return this.mustRecord();
      }
      if (fromBackup) fail(404, bucket ? `${r.id} has no backup yet` : "backups are off: this deployment has no R2 bucket");
    }
    await this.boot(snapshot);
    // Restoring a save restarts its 30 days.
    if (snapshot) await this.ctx.storage.put("saveTouchedAt", Date.now());
    await this.startHook();
    return this.mustRecord();
  }

  private async bootFromBackup(b: BackupManifest, why: string) {
    const r = this.mustRecord();
    await this.boot(undefined, async () => {
      const t0 = Date.now();
      await this.runScript(RESTORE_SCRIPT, [HELPER, String(b.gen), String(b.parts)], "restoring the backup");
      console.log(`[${r.id}] restored backup ${b.gen} (${b.bytes} bytes) in ${Date.now() - t0} ms`);
    });
    await this.ctx.storage.delete(["snapshot", "saveTouchedAt"]);
    const when = new Date(b.createdAt).toISOString().slice(0, 16).replace("T", " ");
    await this.update({
      notice: `Restored /home from the backup of ${when} UTC (${why}). Anything installed outside /home needs installing again.`,
    });
  }

  async stop(): Promise<SandboxRecord> {
    return this.exclusive(async () => {
      const r = this.mustRecord();
      if (r.state === "stopped" || r.state === "error") return r;
      if (r.state !== "running") fail(409, `${r.id} is ${r.state}`);
      await this.saveAndDestroy();
      return this.mustRecord();
    });
  }

  // A stop asked for from inside the sandbox: its request has to be answered
  // before the container goes, so the alarm does it a moment later.
  async requestStop(): Promise<SandboxRecord> {
    const r = this.mustRecord();
    if (r.state !== "running") fail(409, `${r.name ?? r.id} is ${r.state}`);
    await this.ctx.storage.put("stopRequested", true);
    await this.ctx.storage.setAlarm(Date.now() + 1000);
    return r;
  }

  // A new name, or a new idle time, which counts from now.
  async set(patch: UpdateSandbox): Promise<SandboxRecord> {
    const r = this.mustRecord();
    const change: Partial<SandboxRecord> = {};
    if (patch.name !== undefined && patch.name !== r.name) {
      if (!(await renameSandbox(this.env.DB, r.id, patch.name))) fail(409, `the name ${patch.name} is taken`);
      change.name = patch.name;
    }
    if (patch.idleMinutes !== undefined) change.idleMinutes = patch.idleMinutes;
    if (r.state === "running") this.touch();
    await this.update(change);
    return this.mustRecord();
  }

  private async linkState(): Promise<Links> {
    return (await this.ctx.storage.get<Links>("links")) ?? { epoch: 0, ports: {} };
  }

  // Until a port's link has been revoked, its token is the HMAC of sandbox and
  // port alone, as tokens were before links could be revoked.
  private linkToken(links: Links, port: number, id = this.mustRecord().id) {
    const gen = links.ports[port]?.gen ?? 0;
    return previewToken(this.env.ADMIN_KEY, id, port, links.epoch || gen ? `${links.epoch}.${gen}` : undefined);
  }

  // The token for a port's preview link, which is listed from now on.
  async link(port: number): Promise<{ token: string; createdAt: number }> {
    this.mustRecord();
    const links = await this.linkState();
    const entry = (links.ports[port] ??= { gen: 0, at: null });
    entry.at ??= Date.now();
    await this.ctx.storage.put("links", links);
    return { token: await this.linkToken(links, port), createdAt: entry.at };
  }

  async links(): Promise<{ port: number; token: string; createdAt: number }[]> {
    this.mustRecord();
    const links = await this.linkState();
    const listed = Object.entries(links.ports).filter(([, e]) => e.at !== null);
    const out = await Promise.all(
      listed.map(async ([p, e]) => ({ port: Number(p), token: await this.linkToken(links, Number(p)), createdAt: e.at! })),
    );
    return out.sort((a, b) => a.port - b.port);
  }

  // A revoked link stops working at once; asking for the port's link again
  // gives a new one. Without a port, every link to the sandbox goes, including
  // ones handed out before links were listed.
  async revokeLinks(port?: number): Promise<{ revoked: number[]; all: boolean }> {
    this.mustRecord();
    const links = await this.linkState();
    let revoked: number[];
    if (port === undefined) {
      links.epoch += 1;
      revoked = Object.entries(links.ports).filter(([, e]) => e.at !== null).map(([p]) => Number(p));
      for (const e of Object.values(links.ports)) e.at = null;
    } else {
      const entry = (links.ports[port] ??= { gen: 0, at: null });
      entry.gen += 1;
      entry.at = null;
      revoked = [port];
    }
    await this.ctx.storage.put("links", links);
    this.revocations++;
    for (const [p, ends] of this.open) {
      if (port === undefined || p === port) for (const end of [...ends]) end();
    }
    return { revoked: revoked.sort((a, b) => a - b), all: port === undefined };
  }

  async checkLink(port: number, token: string): Promise<boolean> {
    const r = this.record;
    if (!r || this.removing || !token) return false;
    return sameSecret(token, await this.linkToken(await this.linkState(), port, r.id));
  }

  async remove(): Promise<{ deleted: string }> {
    return this.exclusive(async () => {
      const r = this.mustRecord();
      this.record = { ...r, state: "stopping" }; // so the exit watcher stays out of it
      this.removing = true;
      try {
        return await this.removeAll(r);
      } finally {
        this.removing = false;
      }
    });
  }

  private async removeAll(r: SandboxRecord): Promise<{ deleted: string }> {
    if (this.ctx.container?.running) await withTimeout(this.container.destroy(), 60_000, "destroy");
    const bucket = backupBucket(this.env);
    if (bucket) {
      try {
        await deleteBackups(bucket, r.id);
      } catch (e) {
        console.error(`[${r.id}] deleting backups failed: ${message(e)}`);
      }
    }
    await this.ctx.storage.deleteAlarm();
    await this.ctx.storage.deleteAll();
    await deleteSandbox(this.env.DB, r.id);
    this.record = undefined;
    // Its backups are gone. The last save can't be deleted from a Worker; it
    // expires on its own 30 days after it was made or last restored.
    return { deleted: r.id };
  }

  private async boot(snapshot?: unknown, afterReady?: () => Promise<void>) {
    const r = this.mustRecord();
    const c = this.container;
    const t0 = Date.now();
    // A new run: no relays yet, and no stop asked for from inside the last one.
    this.relayed.clear();
    await this.ctx.storage.delete("stopRequested");
    await this.update({ state: "starting", error: null, notice: null });
    let providers: (keyof typeof PROVIDERS)[] = [];
    try {
      providers = (await listKeys(this.env.DB)).map((k) => k.provider);
    } catch (e) {
      console.error(`[${r.id}] reading provider keys failed: ${message(e)}`);
    }
    let keysProblem = "";
    let settingsProblem = "";
    let platformProblem = "";
    try {
      const images = c.images as Record<string, string>;
      c.start({
        ...(snapshot ? { containerSnapshot: snapshot } : { image: images.base }),
        instance: r.size,
        enableInternet: true,
        env: { CLOUDEX_SANDBOX: r.id },
        labels: { sandbox: r.id },
      } as ContainerStartupOptions);
      this.watch();
      // The cloudex command in the sandbox shows this run's token to the API
      // the sandbox has for itself (self.ts), so that an app in the sandbox
      // that fetches URLs for its visitors can't be made to call it.
      const selfToken = [...crypto.getRandomValues(new Uint8Array(24))].map((b) => b.toString(16).padStart(2, "0")).join("");
      await this.ctx.storage.put("selfToken", selfToken);
      const egress = this.ctx.exports.Egress({ props: { id: r.id, selfToken } });
      await withTimeout(c.interceptOutboundHttp("cloudex.internal", egress), 30_000, "routing cloudex.internal");
      for (const p of providers) {
        await withTimeout(c.interceptOutboundHttps(PROVIDERS[p].host, egress), 30_000, `routing ${PROVIDERS[p].host}`);
      }
      await this.waitReady(t0);
      if (afterReady) await afterReady();
      // After any restore of /home, which holds Claude Code's settings. Also run
      // with no providers, to clear placeholders a save may still hold.
      try {
        await this.runScript(
          KEYS_SCRIPT,
          [providers.join(" "), PROVIDERS.anthropic.placeholder, PROVIDERS.openai.placeholder],
          "setting up provider keys",
          60_000,
        );
      } catch (e) {
        keysProblem = message(e);
        console.error(`[${r.id}] ${keysProblem}`);
      }
      // After the keys, so that a variable in the settings wins over a placeholder.
      try {
        await this.applySettings();
      } catch (e) {
        settingsProblem = message(e);
        console.error(`[${r.id}] ${settingsProblem}`);
      }
      // After the settings, whose files may replace ones it adjusts.
      try {
        await this.applyPlatform();
      } catch (e) {
        platformProblem = message(e);
        console.error(`[${r.id}] ${platformProblem}`);
      }
    } catch (e) {
      try {
        if (c.running) await withTimeout(c.destroy(), 60_000, "destroy");
      } catch {}
      const why = message(e);
      await this.update({ state: r.savedAt ? "stopped" : "error", error: `start failed: ${why}` });
      fail(503, `${r.id} didn't start: ${why}`);
    }
    try {
      await withTimeout(c.setInactivityTimeout(PLATFORM_MAX_INACTIVITY_MS), 30_000, "setInactivityTimeout");
    } catch (e) {
      console.error(`[${r.id}] ${message(e)}`);
    }
    console.log(`[${r.id}] ready in ${Date.now() - t0} ms (${snapshot ? "from save" : "fresh"})`);
    this.touch();
    await this.update({
      state: "running",
      startedAt: Date.now(),
      stoppedAt: null,
      providerKeys: keysProblem ? [] : providers,
      ...(keysProblem || settingsProblem || platformProblem
        ? {
            notice: [
              keysProblem && `Provider keys aren't set up in this run: ${keysProblem}.`,
              settingsProblem && `The sandbox's settings aren't applied in this run: ${settingsProblem}.`,
              platformProblem && `The cloudex command and the localhost relay aren't set up in this run: ${platformProblem}.`,
            ]
              .filter(Boolean)
              .join(" "),
          }
        : {}),
    });
    await this.ctx.storage.setAlarm(Date.now() + ALARM_EVERY_MS);
  }

  // Ready means the entrypoint wrote this boot's id. exec() waits while the VM boots.
  private async waitReady(since: number) {
    const c = this.container;
    let last = "";
    while (Date.now() - since < READY_TIMEOUT_MS) {
      if (this.exitedAt > since) throw new Error("the container exited while booting");
      try {
        const p = await withTimeout(c.exec(["sh", "-c", READY_CHECK]), 60_000, "readiness check");
        if ((await p.exitCode) === 0) return;
      } catch (e) {
        last = message(e);
      }
      await sleep(250);
    }
    throw new Error(
      `not ready after ${READY_TIMEOUT_MS / 60_000} minutes (${last || "no answer"}). ` +
        "A newly deployed image can take a while to reach every host; try again shortly",
    );
  }

  private async save() {
    const r = this.mustRecord();
    const t0 = Date.now();
    const snap = await withTimeout(
      this.container.snapshotContainer({ name: `${r.id}-${t0}` }),
      SAVE_TIMEOUT_MS,
      "saving the disk",
    );
    await this.ctx.storage.put("snapshot", snap);
    await this.ctx.storage.put("saveTouchedAt", Date.now());
    await this.update({ savedAt: Date.now(), savedBytes: snap.size ?? null });
    console.log(`[${r.id}] saved ${snap.size} bytes in ${Date.now() - t0} ms`);
  }

  private async saveAndDestroy() {
    const r = this.mustRecord();
    await this.update({ state: "stopping" });
    try {
      await this.save();
    } catch (e) {
      await this.update({ state: "running", error: `save failed: ${message(e)}` });
      fail(500, `couldn't save ${r.id}, so it is still running: ${message(e)}`);
    }
    await this.backup();
    await withTimeout(this.container.destroy(), 60_000, "destroy");
    await this.ctx.storage.deleteAlarm();
    await this.update({ state: "stopped", stoppedAt: Date.now(), error: null });
  }

  // Called through the outbound handler when the platform sends SIGTERM.
  async guestTerm(): Promise<{ saved: boolean }> {
    return this.exclusive(async () => {
      const r = this.record;
      if (!r || r.state !== "running") return { saved: false }; // our own stop is already saving
      try {
        await this.save();
        await this.backup();
        return { saved: true };
      } catch (e) {
        console.error(`[${r.id}] save on SIGTERM failed: ${message(e)}`);
        return { saved: false };
      }
    });
  }

  // Backs up /home to R2 after a save. A failure is recorded, not fatal: the save stands.
  private async backup() {
    const bucket = backupBucket(this.env);
    if (!bucket) return;
    const r = this.mustRecord();
    const gen = Date.now();
    try {
      const out = await this.runScript(BACKUP_SCRIPT, [HELPER, String(gen)], "backing up /home");
      const last = out.trim().split("\n").pop() ?? "";
      const { parts, bytes } = JSON.parse(last) as { parts?: number; bytes?: number };
      if (!Number.isInteger(parts) || !parts || !Number.isInteger(bytes)) throw new Error(`unexpected helper output: ${last.slice(0, 200)}`);
      await writeManifest(bucket, r.id, { gen, parts, bytes: bytes!, createdAt: gen, ms: Date.now() - gen });
      await deleteBackups(bucket, r.id, gen);
      await this.update({ backupAt: gen, backupBytes: bytes!, backupError: null });
      console.log(`[${r.id}] backed up ${bytes} bytes in ${parts} parts in ${Date.now() - gen} ms`);
    } catch (e) {
      console.error(`[${r.id}] backup failed: ${message(e)}`);
      await this.update({ backupError: message(e) });
      await deleteGeneration(bucket, r.id, gen).catch(() => {});
    }
  }

  // Runs one of the guest scripts as root, with args as $1, $2 and so on, and
  // stdin if given. Returns stdout.
  private async runScript(script: string, args: string[], what: string, timeoutMs = BACKUP_TIMEOUT_MS, stdin?: string): Promise<string> {
    const ctl = new AbortController();
    const timer = setTimeout(() => ctl.abort(), timeoutMs);
    try {
      const p = await this.container.exec(["bash", "-c", script, "cloudex", ...args], {
        cwd: "/",
        env: { HOME: "/root", LANG: "C.UTF-8" },
        ...(stdin === undefined ? {} : { stdin: new Response(stdin).body! }),
        signal: ctl.signal,
      });
      const out = await p.output();
      if (out.exitCode !== 0) {
        const tail = clip(out.stderr).trim().split("\n").slice(-3).join(" | ");
        throw new Error(`${what} failed (exit ${out.exitCode}): ${tail}`);
      }
      return new TextDecoder().decode(out.stdout);
    } catch (e) {
      if (ctl.signal.aborted) throw new Error(`${what} took longer than ${timeoutMs / 60_000} minutes`);
      throw e;
    } finally {
      clearTimeout(timer);
    }
  }

  // Activity from inside the guest, for example a running agent.
  async ping() {
    this.touch();
  }

  async exec(input: ExecInput): Promise<ExecResult> {
    this.mustRun();
    const ctl = new AbortController();
    let timedOut = false;
    const timer = setTimeout(() => {
      timedOut = true;
      ctl.abort();
    }, input.timeoutSec * 1000);
    const t0 = Date.now();
    try {
      const p = await this.container.exec(["bash", "-lc", input.cmd], {
        cwd: input.cwd ?? (input.root ? "/root" : GUEST_HOME),
        env: input.root
          ? { HOME: "/root", USER: "root", LANG: "C.UTF-8", TERM: "dumb" }
          : { HOME: GUEST_HOME, USER: "cloudex", LOGNAME: "cloudex", LANG: "C.UTF-8", TERM: "dumb" },
        ...(input.root ? {} : { user: GUEST_USER }),
        signal: ctl.signal,
      });
      const out = await readOutput(p);
      return { ...out, timedOut, ms: Date.now() - t0 };
    } finally {
      clearTimeout(timer);
      this.touch();
    }
  }

  // One operation of the agent script (agents.ts), as the guest user from a
  // login shell, so agents get the provider key placeholders. A prompt goes in
  // on standard input. Returns the script's JSON.
  async agentOp(op: string, args: string[], prompt?: string, waitSec = 0): Promise<unknown> {
    const r = this.mustRun();
    try {
      const out = await this.asGuest([AGENT_BOOT, AGENT_SCRIPT, op, ...args], `agent ${op}`, (waitSec + 60) * 1000, prompt);
      const why = new TextDecoder().decode(out.stderr).trim().split("\n").pop() ?? "";
      if (out.exitCode === AGENT_NOT_FOUND) fail(404, `${why} in ${r.name ?? r.id}`);
      if (out.exitCode === AGENT_BAD_REQUEST) fail(400, why);
      if (out.exitCode !== 0) throw new Error(`agent ${op} failed (exit ${out.exitCode}): ${why}`);
      return JSON.parse(new TextDecoder().decode(out.stdout));
    } finally {
      this.touch();
    }
  }

  // Runs a script as the guest user from a login shell (so it gets the
  // settings' variables and the provider key placeholders): argv[0] is the
  // script, the rest its $1, $2 and so on.
  private async asGuest(argv: string[], what: string, timeoutMs: number, stdin?: string) {
    const ctl = new AbortController();
    const timer = setTimeout(() => ctl.abort(), timeoutMs);
    try {
      const p = await this.container.exec(["bash", "-lc", argv[0]!, "cloudex", ...argv.slice(1)], {
        cwd: GUEST_HOME,
        env: { HOME: GUEST_HOME, USER: "cloudex", LOGNAME: "cloudex", LANG: "C.UTF-8" },
        user: GUEST_USER,
        ...(stdin === undefined ? {} : { stdin: new Response(stdin).body! }),
        signal: ctl.signal,
      });
      return await p.output();
    } catch (e) {
      if (ctl.signal.aborted) throw new Error(`${what} took longer than ${Math.round(timeoutMs / 1000)} s`);
      throw e;
    } finally {
      clearTimeout(timer);
    }
  }

  // Starts the desktop, installing it the first time, or resizes a running one.
  async desktop(size: string): Promise<Desktop> {
    const r = this.mustRun();
    const t0 = Date.now();
    try {
      const out = await this.runScript(DESKTOP_SCRIPT, [size, r.name ?? r.id], "starting the desktop", DESKTOP_TIMEOUT_MS);
      const last = out.trim().split("\n").pop() ?? "";
      const { installed, started } = JSON.parse(last) as { installed: boolean; started: boolean };
      console.log(`[${r.id}] desktop ${started ? "started" : "running"} at ${size}${installed ? ", installed" : ""} in ${Date.now() - t0} ms`);
      return { port: DESKTOP_PORT, path: DESKTOP_VIEWER, size, installed, started };
    } finally {
      this.touch();
    }
  }

  async stopDesktop(): Promise<{ stopped: boolean }> {
    const r = this.mustRecord();
    if (r.state !== "running") return { stopped: false };
    this.touch();
    await this.runScript(DESKTOP_STOP_SCRIPT, [], "stopping the desktop", 60_000);
    return { stopped: true };
  }

  // Idle stop. The platform's own inactivity timer only sees incoming
  // connections, so this is what actually ends a quiet box. An open bridge
  // counts only while its client sends something (ssh keepalives every 30 s,
  // a desktop viewer's update requests): at the edge a dropped connection can
  // look open for half an hour, and an open one alone would hold the box up.
  async alarm() {
    const r = this.record;
    const stopRequested = await this.ctx.storage.get<boolean>("stopRequested");
    if (stopRequested) await this.ctx.storage.delete("stopRequested");
    if (!r || r.state !== "running") return;
    if (!this.ctx.container?.running) return this.onExit();
    if (stopRequested) {
      try {
        await this.exclusive(async () => {
          if (this.record?.state === "running") await this.saveAndDestroy();
        });
        console.log(`[${r.id}] stopped from inside`);
        return;
      } catch (e) {
        console.error(`[${r.id}] stop from inside failed: ${message(e)}`);
      }
    }
    // A sandbox running when the deployment was updated gets the new tools.
    if ((await this.ctx.storage.get("platform")) !== `${PLATFORM_VERSION}:${this.env.PREVIEW_ZONE || ""}`) {
      await this.exclusive(async () => {
        if (this.record?.state === "running") await this.applyPlatform();
      }).catch((e) => console.error(`[${r.id}] ${message(e)}`));
    }
    const idleMs = r.idleMinutes * 60_000;
    if (idleMs > 0 && Date.now() - this.lastActivity > idleMs) {
      try {
        await this.exclusive(async () => {
          if (this.record?.state === "running") await this.saveAndDestroy();
        });
        console.log(`[${r.id}] stopped after ${r.idleMinutes} idle minutes`);
        return;
      } catch (e) {
        console.error(`[${r.id}] idle stop failed: ${message(e)}`);
      }
    }
    // Sooner if a stop was asked for from inside while this ran.
    const asked = await this.ctx.storage.get<boolean>("stopRequested");
    await this.ctx.storage.setAlarm(Date.now() + (asked ? 1000 : ALARM_EVERY_MS));
  }

  // The Worker forwards two kinds of request here:
  //   /tcp/<port>[?key=<ssh public key>]  WebSocket bridged to a guest TCP port
  //   /http/<port>/<path>                 an HTTP preview of a guest port
  async fetch(req: Request): Promise<Response> {
    try {
      return await this.serve(req);
    } catch (e) {
      const { status, message: why } = errorStatus(e);
      return new Response(`${why}\n`, { status: status === 500 ? 502 : status });
    }
  }

  private async serve(req: Request): Promise<Response> {
    const r = this.record;
    if (!r || this.removing) return new Response("no such sandbox", { status: 404 });
    const url = new URL(req.url);
    const m = /^\/(tcp|http)\/(\d{1,5})(\/.*)?$/.exec(url.pathname);
    if (!m) return new Response("not found", { status: 404 });
    const port = Number(m[2]);
    const token = req.headers.get(LINK_HEADER) ?? "";
    const revocations = this.revocations;
    if (m[1] === "http" && !(await this.checkLink(port, token))) {
      return new Response("not found", { status: 404 });
    }
    if (r.state !== "running") {
      return new Response(`${r.id} is ${r.state}. Run: cloudex resume ${r.name ?? r.id}\n`, { status: 409 });
    }
    this.touch();
    if (m[1] === "http") {
      // Host is the preview link's host name, so that dev servers that check it
      // (Vite) and apps that build links from it get the real one.
      const host = req.headers.get("x-forwarded-host") ?? "";
      const name = /^[a-z0-9.-]{1,253}$/.test(host) ? host : "container";
      const res = await this.preview(port, `http://${name}${m[3] ?? "/"}${url.search}`, req);
      // A link revoked while this request was on its way doesn't get it.
      if (this.revocations !== revocations && !(await this.checkLink(port, token))) {
        res.webSocket?.accept();
        res.webSocket?.close(1000, "the link was revoked");
        await res.body?.cancel().catch(() => {});
        return new Response("not found", { status: 404 });
      }
      return this.carry(port, res);
    }
    if (req.headers.get("Upgrade")?.toLowerCase() !== "websocket") {
      return new Response("expected a WebSocket upgrade", { status: 426 });
    }
    const key = url.searchParams.get("key");
    if (key) {
      if (!SSH_KEY.test(key)) return new Response("that isn't an SSH public key", { status: 400 });
      await this.authorize(key);
    }
    return this.bridge(port);
  }

  // A preview request for a guest port. A server listening only on localhost
  // doesn't answer at the sandbox's address, so then a relay (guest.ts) starts
  // for it, and requests for the port go through the relay while it runs. A
  // request is sent again after a miss only without a body, which can't be.
  private async preview(port: number, target: string, req: Request): Promise<Response> {
    const r = this.mustRecord();
    const send = (to: number) => {
      const fwd = new Request(target, req);
      fwd.headers.delete(LINK_HEADER);
      return this.container.getTcpPort(to).fetch(fwd);
    };
    let why = "";
    const relay = this.relayed.get(port);
    for (const to of relay ? [relay, port] : [port]) {
      try {
        return await send(to);
      } catch (e) {
        why = message(e);
        if (to !== port) this.relayed.delete(port);
        if (req.body) break;
      }
    }
    const addr = /not listening in the TCP address (\d{1,3}(?:\.\d{1,3}){3}):\d+/.exec(why)?.[1];
    if (addr) {
      const through = await this.relay(port, addr);
      if (through === null) {
        return new Response(`nothing is listening on port ${port} in ${r.name ?? r.id}\n`, { status: 502 });
      }
      if (through) {
        if (through !== port) this.relayed.set(port, through);
        if (req.body) {
          return new Response(`port ${port} in ${r.name ?? r.id} only just started answering; send that again\n`, {
            status: 503,
            headers: { "retry-after": "1" },
          });
        }
        try {
          return await send(through);
        } catch (e) {
          why = message(e);
          this.relayed.delete(port);
        }
      }
    }
    return new Response(`nothing is answering on port ${port} in ${r.name ?? r.id}: ${why}\n`, { status: 502 });
  }

  // A WebSocket or streamed answer passes through here, rather than straight
  // to the visitor, so that revoking the port's link can end it (revokeLinks).
  // Answers of a known length, most of a dev server's, go straight through.
  private carry(port: number, res: Response): Response {
    const ends = this.open.get(port) ?? new Set<() => void>();
    this.open.set(port, ends);
    const upstream = res.webSocket;
    if (res.status === 101 && upstream) {
      const pair = new WebSocketPair();
      const [client, server] = [pair[0], pair[1]];
      upstream.accept();
      server.accept();
      upstream.binaryType = server.binaryType = "arraybuffer";
      let ended = false;
      const end = (code = 1000, reason = "") => {
        if (ended) return;
        ended = true;
        ends.delete(end);
        // close() takes only 1000 and 3000-4999; an abnormal end gets 4000.
        const sendable = code === 1000 || (code >= 3000 && code <= 4999) ? code : 4000;
        for (const ws of [server, upstream]) {
          try {
            ws.close(sendable, reason);
          } catch {}
        }
      };
      ends.add(end);
      upstream.addEventListener("message", (e) => {
        try {
          server.send(e.data as ArrayBuffer | string);
        } catch {
          end(4000);
        }
      });
      server.addEventListener("message", (e) => {
        this.touch();
        try {
          upstream.send(e.data as ArrayBuffer | string);
        } catch {
          end(4000);
        }
      });
      for (const ws of [server, upstream]) {
        ws.addEventListener("close", (e) => end(e.code, e.reason));
        ws.addEventListener("error", () => end(4000));
      }
      const headers = new Headers();
      const protocol = res.headers.get("sec-websocket-protocol");
      if (protocol) headers.set("sec-websocket-protocol", protocol);
      return new Response(null, { status: 101, webSocket: client, headers });
    }
    if (!res.body || res.headers.has("content-length")) return res;
    const stop = new AbortController();
    const { readable, writable } = new TransformStream();
    const end = () => stop.abort();
    ends.add(end);
    void res.body
      .pipeTo(writable, { signal: stop.signal })
      .catch(() => {})
      .finally(() => ends.delete(end));
    return new Response(readable, res);
  }

  // The guest port that answers for port at addr: port itself, or the port of
  // a relay to it from localhost (guest.ts). Null when nothing listens on port,
  // false if the relay didn't start. A sandbox started before the relay
  // existed gets it first.
  private async relay(port: number, addr: string, again = true): Promise<number | null | false> {
    const id = this.record?.id;
    try {
      const p = await withTimeout(
        this.container.exec(["bash", "-c", RELAY_SCRIPT, "cloudex", String(port), addr], { cwd: "/", env: { LANG: "C.UTF-8" } }),
        15_000,
        "starting the relay",
      );
      const out = await withTimeout(p.output(), 15_000, "starting the relay");
      const through = Number(new TextDecoder().decode(out.stdout).trim());
      if (out.exitCode === 0 && Number.isInteger(through) && through > 0 && through < 65536) return through;
      if (out.exitCode === RELAY_NOTHING) return null;
      if (out.exitCode === 127 && again) {
        // One reinstall, in the lifecycle queue, however many previews ask.
        this.platformFix ??= this.exclusive(async () => {
          if (this.record?.state === "running") await this.applyPlatform();
        }).finally(() => (this.platformFix = undefined));
        await this.platformFix;
        return this.relay(port, addr, false);
      }
      console.error(`[${id}] the relay for port ${port} exited ${out.exitCode}: ${clip(out.stderr).trim().slice(-300)}`);
    } catch (e) {
      console.error(`[${id}] the relay for port ${port}: ${message(e)}`);
    }
    return false;
  }

  // Adds an SSH public key for the cloudex user if it isn't there yet.
  private async authorize(key: string) {
    const script =
      'mkdir -p ~/.ssh && touch ~/.ssh/authorized_keys && chmod 700 ~/.ssh && chmod 600 ~/.ssh/authorized_keys && ' +
      '{ grep -qxF "$KEY" ~/.ssh/authorized_keys || printf "%s\\n" "$KEY" >> ~/.ssh/authorized_keys; }';
    const p = await this.container.exec(["sh", "-c", script], {
      user: GUEST_USER,
      env: { HOME: GUEST_HOME, KEY: key },
    });
    const code = await p.exitCode;
    if (code !== 0) throw new Error(`adding the SSH key failed (exit ${code})`);
  }

  // WebSocket <-> guest TCP. Reads from the guest are coalesced into frames of
  // up to 256 KiB, which took scp downloads from 6-8 to 17 MiB/s in the spikes.
  private async bridge(port: number): Promise<Response> {
    const sock = this.container.getTcpPort(port).connect(`10.0.0.1:${port}`);
    try {
      await withTimeout(sock.opened, 15_000, `connecting to port ${port}`);
    } catch (e) {
      return new Response(`nothing is listening on port ${port}: ${message(e)}`, { status: 502 });
    }
    const pair = new WebSocketPair();
    const [client, server] = [pair[0], pair[1]];
    server.accept();
    // Newer compatibility dates deliver binary messages as Blob unless told otherwise.
    server.binaryType = "arraybuffer";
    const writer = sock.writable.getWriter();
    let closed = false;
    const done = () => {
      if (closed) return;
      closed = true;
      this.touch();
      writer.close().catch(() => {});
    };
    server.addEventListener("message", (e) => {
      this.touch(); // data from the client is what shows the connection is alive
      const chunk = typeof e.data === "string" ? new TextEncoder().encode(e.data) : new Uint8Array(e.data as ArrayBuffer);
      writer.write(chunk).catch(() => server.close(1011, "guest connection failed"));
    });
    server.addEventListener("close", done);
    server.addEventListener("error", done);

    let pending: Uint8Array[] = [];
    let pendingBytes = 0;
    let timer: ReturnType<typeof setTimeout> | undefined;
    const flush = () => {
      if (timer) clearTimeout(timer);
      timer = undefined;
      if (!pendingBytes) return;
      let frame = pending[0]!;
      if (pending.length > 1) {
        frame = new Uint8Array(pendingBytes);
        let offset = 0;
        for (const part of pending) {
          frame.set(part, offset);
          offset += part.byteLength;
        }
      }
      pending = [];
      pendingBytes = 0;
      server.send(frame);
    };
    void (async () => {
      const reader = sock.readable.getReader();
      try {
        for (;;) {
          const { value, done: eof } = await reader.read();
          if (eof) break;
          pending.push(value);
          pendingBytes += value.byteLength;
          if (pendingBytes >= 256 * 1024) flush();
          else timer ??= setTimeout(flush, 1);
        }
      } catch {
        // the guest closed the connection
      } finally {
        flush();
        try {
          server.close(1000, "closed");
        } catch {}
        done();
      }
    })();
    return new Response(null, { status: 101, webSocket: client });
  }
}
