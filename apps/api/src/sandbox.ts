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
// - Named saves (saves.ts) are made from a running sandbox by a script, into
//   R2. A sandbox started from one boots held, so that the save is restored
//   before anything in it runs, and keeps the save to restore again until it
//   has a save of its own.
// - Background commands (commands.ts) and file operations (files.ts) run
//   scripts in the guest too; file contents and streamed exec output pass
//   through as streams.
import { DurableObject } from "cloudflare:workers";
import type {
  ArchiveResult,
  Desktop,
  ExecEvent,
  ExecResult,
  FileInfo,
  Repo,
  Save,
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
import { COMMAND_BAD_REQUEST, COMMAND_BOOT, COMMAND_CONFLICT, COMMAND_NOT_FOUND, COMMAND_SCRIPT } from "./commands";
import { CloudflareCompute, type Compute, NodeCompute, type NodePlacement, NotListening, type Proc } from "./compute";
import { deleteSandbox, renameSandbox, upsertSandbox } from "./db";
import { DESKTOP_PORT, DESKTOP_SCRIPT, DESKTOP_STOP_SCRIPT, DESKTOP_VIEWER, VNC_PORT } from "./desktop";
import { ARCHIVE_SCRIPT, FILE_BAD_REQUEST, FILE_FORBIDDEN, FILE_NOT_FOUND, FILE_SCRIPT, UNPACK_SCRIPT } from "./files";
import { PLATFORM_FILES, PLATFORM_SCRIPT, PLATFORM_VERSION, RELAY_NOTHING, RELAY_SCRIPT, START_SCRIPT } from "./guest";
import { KEYS_SCRIPT, PROVIDERS, listKeys, placeholderVars } from "./keys";
import {
  RESTORE_SAVE_SCRIPT,
  SAVE_SCRIPT,
  type SaveManifest,
  type SaveSummary,
  deleteSaveObjects,
  getSave,
  readManifest,
  recordSave,
  saveView,
} from "./saves";
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
import { NodeStreams } from "./nodestream";
import { tokenAlive } from "./tokens";
import { CREDENTIAL_ENDED, LINK_HEADER, TOKEN_HEADER, errorStatus, fail, previewToken, sameSecret, sleep, withTimeout } from "./util";

// Exits 0 once the entrypoint is ready in this boot, and 3 while it holds for
// a restore (CLOUDEX_HOLD).
const BOOT_CHECK =
  'b=$(cat /proc/sys/kernel/random/boot_id); [ "$(cat /run/cloudex-ready 2>/dev/null)" = "$b" ] && exit 0; ' +
  '[ "$(cat /run/cloudex-held 2>/dev/null)" = "$b" ] && exit 3; exit 1';
const READY_TIMEOUT_MS = 10 * 60_000; // a host without the image pulls it first
const SAVE_TIMEOUT_MS = 15 * 60_000; // 15 GiB took 179 s in the spikes
const BACKUP_TIMEOUT_MS = 20 * 60_000; // about 110 MiB/s each way in the spikes
const DESKTOP_TIMEOUT_MS = 10 * 60_000; // the first start installs about 950 MB
// Named saves move at about 110 MiB/s each way, as backups do; a held
// entrypoint gives up after 30 minutes.
const NAMED_SAVE_TIMEOUT_MS = 30 * 60_000;
const RESTORE_SAVE_TIMEOUT_MS = 25 * 60_000;
// A save expires 30 days after it was made or last restored; past this, resume
// restores the backup instead of trying the save.
const SAVE_LIFETIME_MS = 29 * 24 * 3600_000;
const ALARM_EVERY_MS = 60_000;
// How often an open terminal, desktop or TCP WebSocket checks that its token still works.
const CREDENTIAL_CHECK_MS = 60_000;
const GUEST_USER = "1000:1000"; // the image's cloudex user
// Runs the rest of a command line as the cloudex user, as an ssh login would.
// exec's own user option keeps every capability root has (as ambient ones) and
// drops the user's groups (sudo, docker), so commands start as root and switch.
const AS_GUEST = ["setpriv", "--reuid=1000", "--regid=1000", "--init-groups", "--inh-caps=-all", "--"];
const guestArgv = (argv: string[], root = false) => (root ? argv : [...AS_GUEST, ...argv]);
const GUEST_HOME = "/home/cloudex";

// Dashboard terminals: a tmux server of their own, set up to act like a plain
// terminal. No prefix key and no status line, so keys all reach the shell; the
// mouse wheel scrolls back through tmux's history.
const TMUX_SOCKET = "cloudex-dash";
const TMUX_SESSION = /^[A-Za-z0-9_-]{1,40}$/;
const tmuxArgv = (session: string) => [
  "tmux", "-L", TMUX_SOCKET, "-f", "/dev/null", "start-server",
  ";", "set", "-g", "prefix", "None",
  ";", "set", "-g", "status", "off",
  ";", "set", "-g", "mouse", "on",
  ";", "set", "-g", "history-limit", "50000",
  ";", "set", "-g", "default-terminal", "screen-256color",
  ";", "set", "-sg", "escape-time", "10",
  ";", "set", "-g", "set-clipboard", "on",
  ";", "set", "-gw", "aggressive-resize", "on",
  ";", "new-session", "-A", "-s", session,
];

// How long a new bridge waits to see whether its port is answering at all.
const PROBE_MS = 250;
const OUTPUT_LIMIT = 1 << 20;
const FILE_TIMEOUT_MS = 30 * 60_000; // an upload or unpack of up to 100 MB
const KEEPALIVE_MS = 15_000; // the longest a streamed exec stays quiet
const REPLAY_LIMIT = 1 << 20;
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

export const NO_SUCH_SANDBOX = "no such sandbox";

const message = (e: unknown) => (e instanceof Error ? e.message : String(e)).replace(/^\[\d{3}\] /, "");

function wrongState(r: SandboxRecord) {
  const name = r.name ?? r.id;
  if (r.state === "starting" || r.state === "stopping") return `${name} is ${r.state}; try again in a moment`;
  if (r.state === "error") return `${name} didn't start; resume it to try again, or delete it`;
  return `${name} is ${r.state}; resume it first`;
}

const unsavedStop = (r: SandboxRecord, how: string) =>
  r.savedAt
    ? `${how} before it could save; resume starts from its last save or backup`
    : `${how} before its first save; resume starts it over, without its changes`;

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
async function readOutput(p: Proc) {
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

// Whether a command ended because exec's timeout stopped it: GNU timeout exits
// 124, or 137 when it had to kill.
const timedOut = (code: number, t0: number, timeoutSec: number) =>
  (code === 124 || code === 137) && Date.now() - t0 >= timeoutSec * 1000 - 1000;

export class Sandbox extends DurableObject<Env> {
  private record: SandboxRecord | undefined;
  private queue: Promise<unknown> = Promise.resolve();
  private lastActivity = Date.now();
  private lastActivitySaved = 0;
  private execs = 0;
  private exitedAt = 0;
  private exited: Promise<void> = Promise.resolve();
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
  private inactivitySet = false;
  // The machine (compute.ts): a container on Cloudflare, or a machine on a
  // node when the sandbox has a placement there (v0.2.2 M3: development
  // machines only), reached through the node's stream sockets.
  private readonly cloudflare = new CloudflareCompute(this.ctx);
  private onNode: NodeCompute | undefined;
  private get compute(): Compute {
    return this.onNode ?? this.cloudflare;
  }
  private readonly streams = new NodeStreams({
    ctx: this.ctx,
    env: this.env,
    sandbox: () => this.record?.id ?? null,
    node: () => this.onNode?.placement.node ?? null,
    more: async (count) => {
      const p = this.onNode?.placement;
      if (!p) throw new Error("this sandbox isn't on a node");
      await this.env.NODE.getByName(p.node).op("streams", { sandbox: this.record!.id, count }, 5000);
    },
    touch: () => this.touch(),
  });

  private place(p: NodePlacement | undefined) {
    this.onNode = p
      ? new NodeCompute(this.streams, p, async () => {
          await this.env.NODE.getByName(p.node).op("release", { sandbox: this.record?.id ?? "" }, 10_000);
        })
      : undefined;
  }

  constructor(ctx: DurableObjectState, env: Env) {
    super(ctx, env);
    // Node stream heartbeats, answered without waking the object
    // (nodestream.ts). Only node streams use the Hibernation API here.
    ctx.setWebSocketAutoResponse(new WebSocketRequestResponsePair("cx-hb", "cx-hb-ok"));
    void ctx.blockConcurrencyWhile(async () => {
      this.record = await ctx.storage.get<SandboxRecord>("sandbox");
      this.place(await ctx.storage.get<NodePlacement>("node"));
      this.lastActivity = (await ctx.storage.get<number>("lastActivity")) ?? Date.now();
      // After an eviction or deploy, reattach the exit watcher to a running box.
      if (this.compute.running) this.watch();
      if (this.record?.state === "starting" || this.record?.state === "stopping") await this.repair(this.record);
      if (this.record?.state === "running" && this.compute.running && (await ctx.storage.getAlarm()) === null) {
        await ctx.storage.setAlarm(Date.now() + ALARM_EVERY_MS);
      }
    });
  }

  // A start or stop that an eviction or a deploy cut off has nobody left to
  // finish it. A stop whose box still runs carries on running. A cut-off
  // start's box goes, because a restore in it may be half done, which its
  // next save would keep over the good copy, and its keys, settings and tools
  // aren't set up. Either way it counts as stopped, or as a failed start if it
  // never made a save.
  private async repair(r: SandboxRecord) {
    const running = this.compute.running;
    if (r.state === "stopping" && running) {
      await this.update({ state: "running" });
      await this.ctx.storage.setAlarm(Date.now() + ALARM_EVERY_MS);
    } else {
      const savedThisRun = r.state === "stopping" && (r.savedAt ?? 0) >= (r.startedAt ?? 0);
      await this.update({
        state: r.savedAt ? "stopped" : "error",
        stoppedAt: Date.now(),
        error: savedThisRun
          ? null
          : r.state === "starting"
            ? "start failed: it was cut off partway by an update or restart of the deployment"
            : unsavedStop(r, "stopped"),
      });
      // Queued, not awaited, because blockConcurrencyWhile resets the object
      // after 30 s; the alarm finishes the job if this object goes first.
      if (running) {
        await this.ctx.storage.setAlarm(Date.now() + ALARM_EVERY_MS);
        void this.exclusive(() => this.destroyContainer(r.id)).catch((e) => console.error(`[${r.id}] ${message(e)}`));
      }
    }
    console.log(`[${r.id}] was ${r.state} after a restart; now ${this.record?.state}`);
  }

  private mustRecord(): SandboxRecord {
    if (!this.record || this.removing) fail(404, NO_SUCH_SANDBOX);
    return this.record;
  }

  // The record of a running sandbox, for work inside it, which counts as activity.
  private mustRun(): SandboxRecord {
    const r = this.mustRecord();
    if (r.state !== "running") fail(409, wrongState(r));
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
      await upsertSandbox(this.env.DB, { ...r, node: this.onNode?.placement.node ?? null });
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
    this.exited = this.compute.monitor().then(
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
      error: savedThisRun ? null : unsavedStop(r, "stopped by the platform"),
    });
  }

  // With from, the sandbox is a fork and starts from that save (see forkPoint);
  // with save, it starts from that named save.
  async create(id: string, input: CreateInput, from?: ForkSource, save?: SaveManifest): Promise<SandboxRecord> {
    return this.exclusive(async () => {
      if (this.record) fail(409, `${id} already exists`);
      // A fork gets the original's settings, and a sandbox started from a save
      // the settings saved with it; checked before anything is kept.
      const settings =
        from?.settings ??
        save?.settings ??
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
        ...(save ? { fromSave: save.name } : {}),
      };
      await this.update({});
      if (settings) await this.ctx.storage.put("settings", settings);
      if (from) {
        // The fork's save until it makes its own, so that a resume after a
        // failed start, or after the platform stops it first, starts here too.
        await this.ctx.storage.put("snapshot", from.snapshot);
        await this.ctx.storage.put("saveTouchedAt", Date.now());
      }
      // The same for a named save, which is restored again until then.
      if (save) await this.ctx.storage.put("fromSave", save);
      try {
        await this.boot(from?.snapshot, undefined, save);
      } catch (e) {
        const { status, message: why } = errorStatus(e);
        const r = this.mustRecord();
        fail(status, `${why.replace(/\.$/, "")}. ${r.name ?? r.id} is kept, in state ${r.state}: resume it to try again, or delete it`);
      }
      if (settings?.repo && !from && !save) await this.clone(settings.repo);
      await this.startHook();
      return this.mustRecord();
    });
  }

  private async clone(repo: NonNullable<StoredSettings["repo"]>) {
    const t0 = Date.now();
    let why: string;
    let hint = "";
    try {
      const out = await this.asGuest([CLONE_SCRIPT, repo.url, repo.dir, repo.branch ?? ""], "cloning", 10 * 60_000);
      const err = new TextDecoder().decode(out.stderr).trim().split("\n").slice(-2).join(" ");
      if (out.exitCode === 0) {
        console.log(`[${this.mustRecord().id}] cloned ${repo.url} in ${Date.now() - t0} ms${err ? `: ${err}` : ""}`);
        return;
      }
      why = err || `git exited with code ${out.exitCode}`;
      // GitHub asks for a login for a private repo, and for one that isn't there.
      if (/could not read Username/.test(err)) hint = " If it's private, set GH_TOKEN in the sandbox's env.";
    } catch (e) {
      why = message(e);
    }
    const notice = `Couldn't clone ${repo.url} into ~/${repo.dir}: ${why}.${hint}`;
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
      if (this.onNode) this.onNode.snapshot(); // 501: forks aren't on nodes in v0.2.2
      if (r.state === "running") {
        this.touch();
        const t0 = Date.now();
        const snap = await withTimeout(
          this.compute.snapshot(`${r.id}-fork-${t0}`),
          SAVE_TIMEOUT_MS,
          "saving the disk",
        );
        console.log(`[${r.id}] saved ${snap.size} bytes for a fork in ${Date.now() - t0} ms`);
        this.touch();
        return { sandbox: r.id, snapshot: { id: snap.id }, settings: await this.ctx.storage.get<StoredSettings>("settings") };
      }
      if (r.state !== "stopped" && r.state !== "error") fail(409, wrongState(r));
      const snapshot = await this.ctx.storage.get<{ id: string }>("snapshot");
      if (!snapshot) fail(409, `${r.name ?? r.id} has no saved disk to fork; resume it, then fork it`);
      const touched = (await this.ctx.storage.get<number>("saveTouchedAt")) ?? r.savedAt ?? 0;
      if (Date.now() - touched > SAVE_LIFETIME_MS) {
        fail(409, `${r.name ?? r.id}'s save has expired; resume it (which restores its backup), then fork it`);
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
        if (fromBackup) fail(409, `${r.name ?? r.id} is running; stop it, then resume it from its backup`);
        if (size && size !== r.size) fail(409, `${r.name ?? r.id} is running on ${r.size}; stop it, then resume it on ${size}`);
        return r;
      }
      if (r.state !== "stopped" && r.state !== "error") fail(409, wrongState(r));
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
    const fromSave = snapshot ? undefined : await this.ctx.storage.get<SaveManifest>("fromSave");
    const touched = (await this.ctx.storage.get<number>("saveTouchedAt")) ?? r.savedAt ?? 0;
    const expired = Boolean(snapshot) && Date.now() - touched > SAVE_LIFETIME_MS;
    const homeOnlyInBackup = !snapshot && !fromSave && r.savedAt !== null;
    if (fromBackup || expired || homeOnlyInBackup) {
      const bucket = backupBucket(this.env);
      const backup = bucket ? await latestBackup(bucket, r.id) : null;
      if (backup) {
        await this.bootFromBackup(backup, fromBackup ? "as asked" : expired ? "its save had expired" : "it stopped before it could save");
        await this.startHook();
        return this.mustRecord();
      }
      if (fromBackup) fail(404, bucket ? `${r.name ?? r.id} has no backup yet` : "backups are off: this deployment has no R2 bucket");
    }
    // Started from a named save, and stopped before it made a save of its own:
    // it starts from that save again, or from the newest save of its name if
    // it has been saved over, or from the image if it's gone.
    if (fromSave) {
      const m = await this.currentSave(fromSave);
      let note = "";
      if (!m) {
        await this.ctx.storage.delete("fromSave");
        note = `The save ${fromSave.name} it started from has been deleted, and it had no save of its own yet, so it started from the image.`;
      } else if (m.id !== fromSave.id) {
        await this.ctx.storage.put("fromSave", m);
        note = `The save ${m.name} was saved over after it started, and it had no save of its own yet, so it started from the newer save.`;
      }
      await this.boot(undefined, undefined, m ?? undefined);
      if (note) await this.update({ notice: [note, this.mustRecord().notice].filter(Boolean).join(" ") });
      await this.startHook();
      return this.mustRecord();
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
      if (this.onNode) this.onNode.snapshot(); // 501 until M5
      if (r.state === "stopped" || r.state === "error") return r;
      if (r.state !== "running") fail(409, wrongState(r));
      await this.saveAndDestroy();
      return this.mustRecord();
    });
  }

  // A stop asked for from inside the sandbox: its request has to be answered
  // before the container goes, so the alarm does it a moment later.
  async requestStop(): Promise<SandboxRecord> {
    const r = this.mustRecord();
    if (r.state !== "running") fail(409, wrongState(r));
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
        const out = await this.removeAll(r);
        this.place(undefined);
        return out;
      } catch (e) {
        // Undo the stopping mark so it can be deleted again. A container
        // already destroyed shows up as an exit at the next alarm.
        this.record = r;
        throw e;
      } finally {
        this.removing = false;
      }
    });
  }

  private async removeAll(r: SandboxRecord): Promise<{ deleted: string }> {
    if (this.compute.running) await withTimeout(this.compute.destroy(), 60_000, "destroy");
    // Before its storage, so that a later failure leaves a sandbox to delete
    // again, not an index row holding its name.
    await deleteSandbox(this.env.DB, r.id);
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
    this.record = undefined;
    // Its backups are gone. The last save can't be deleted from a Worker; it
    // expires on its own 30 days after it was made or last restored.
    return { deleted: r.id };
  }

  // With restore, the sandbox starts from the image and that named save is
  // restored onto it first.
  private async boot(snapshot?: unknown, afterReady?: () => Promise<void>, restore?: SaveManifest) {
    const r = this.mustRecord();
    const c = this.compute;
    // A stop whose destroy failed can leave the last run's container up. It
    // goes before t0, so that waitReady doesn't take its exit for this run's.
    if (c.running) await this.destroyContainer(r.id);
    if (c.running) fail(503, `${r.name ?? r.id} didn't start: its last run hasn't stopped yet; try again in a minute`);
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
    let restoreNotice = "";
    try {
      c.start({
        snapshot,
        size: r.size,
        // CLOUDEX_HOLD: the entrypoint waits for the restore before starting anything.
        env: { CLOUDEX_SANDBOX: r.id, ...(restore ? { CLOUDEX_HOLD: "1" } : {}) },
        labels: { sandbox: r.id },
      });
      this.watch();
      // The cloudex command in the sandbox shows this run's token to the API
      // the sandbox has for itself (self.ts), so that an app in the sandbox
      // that fetches URLs for its visitors can't be made to call it.
      const selfToken = [...crypto.getRandomValues(new Uint8Array(24))].map((b) => b.toString(16).padStart(2, "0")).join("");
      await this.ctx.storage.put("selfToken", selfToken);
      const egress = this.ctx.exports.Egress({ props: { id: r.id, selfToken } });
      await c.route(egress, providers.map((p) => PROVIDERS[p].host));
      const held = await this.waitReady(t0, Boolean(restore));
      if (restore) {
        restoreNotice = await this.restoreSave(restore, held);
        if (held) {
          await this.runScript("touch /run/cloudex-go", [], "letting the sandbox start", 30_000);
          await this.waitReady(Date.now());
        }
      }
      if (afterReady) await afterReady();
      // After any restore of /home, which holds Claude Code's settings. Also run
      // with no providers, to clear placeholders a save may still hold.
      try {
        await this.runScript(
          KEYS_SCRIPT,
          [providers.join(" "), PROVIDERS.anthropic.placeholder, PROVIDERS.openai.placeholder, c.caPath, ...placeholderVars(providers)],
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
      if (c.running) await this.destroyContainer(r.id);
      const why = message(e);
      await this.update({ state: r.savedAt ? "stopped" : "error", error: `start failed: ${why}` });
      fail(503, `${r.name ?? r.id} didn't start: ${why}`);
    }
    await this.setInactivity(r.id);
    console.log(`[${r.id}] ready in ${Date.now() - t0} ms (${snapshot ? "from save" : restore ? `from the save ${restore.name}` : "fresh"})`);
    this.touch();
    await this.update({
      state: "running",
      startedAt: Date.now(),
      stoppedAt: null,
      providerKeys: keysProblem ? [] : providers,
      ...(keysProblem || settingsProblem || platformProblem || restoreNotice
        ? {
            notice: [
              restoreNotice,
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

  // Ready means the entrypoint wrote this boot's id. exec() waits while the VM
  // boots. With hold, also returns once the entrypoint holds for a restore:
  // true then, false when it's ready (an image too old to hold).
  private async waitReady(since: number, hold = false): Promise<boolean> {
    const c = this.compute;
    let last = "";
    while (Date.now() - since < READY_TIMEOUT_MS) {
      if (this.exitedAt > since) throw new Error("the container exited while booting");
      try {
        const p = await withTimeout(c.exec(["sh", "-c", BOOT_CHECK]), 60_000, "readiness check");
        const code = await p.exitCode;
        if (code === 0) return false;
        if (code === 3 && hold) return true;
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

  // The named save m, or the one that has replaced it under its name since;
  // null if the name is gone.
  private async currentSave(m: SaveManifest): Promise<SaveManifest | null> {
    const bucket = backupBucket(this.env);
    if (!bucket) return null;
    if (await readManifest(bucket, m.id)) return m;
    const now = await getSave(this.env.DB, m.name);
    return now ? readManifest(bucket, now.id) : null;
  }

  // Restores a named save onto this boot: all of it while the entrypoint is
  // held, if the image is the one it was made on; otherwise /home. Returns a
  // notice for a partial restore.
  private async restoreSave(m: SaveManifest, held: boolean): Promise<string> {
    const r = this.mustRecord();
    const bucket = backupBucket(this.env);
    if (!bucket || !(await readManifest(bucket, m.id))) {
      throw new Error(`the save ${m.name} it starts from has been deleted or saved over since`);
    }
    const t0 = Date.now();
    await this.ctx.storage.put("saveGrant", { id: m.id, mode: "read" });
    let scope: string;
    try {
      const args = [HELPER, m.id, String(m.parts), m.image ?? "", String(m.home), held ? "held" : "running"];
      scope = (await this.runScript(RESTORE_SAVE_SCRIPT, args, `restoring the save ${m.name}`, RESTORE_SAVE_TIMEOUT_MS)).trim();
    } catch (e) {
      if (/No space left on device/.test(message(e))) {
        throw new Error(`the save ${m.name} (${m.rawBytes} bytes) doesn't fit on ${r.size}'s disk; start it on a bigger size`);
      }
      throw e;
    } finally {
      await this.ctx.storage.delete("saveGrant");
    }
    console.log(`[${r.id}] restored ${scope === "all" ? "the save" : "/home from the save"} ${m.name} (${m.bytes} bytes) in ${Date.now() - t0} ms`);
    if (scope === "all") return "";
    if (m.kind === "home") return `Restored /home from the save ${m.name}, which holds only /home: it was made on an image from before v0.1.15.`;
    return `Restored /home from the save ${m.name}. It was made on another version of the sandbox image, so anything installed outside /home needs installing again.`;
  }

  // Saves this running sandbox under a name (saves.ts), replacing any save of
  // that name once it's done. The sandbox keeps running.
  async saveAs(name: string, id: string): Promise<Save> {
    return this.exclusive(async () => {
      const r = this.mustRun();
      const bucket = backupBucket(this.env);
      if (!bucket) fail(501, "named saves need an R2 bucket, and this deployment has none: turn R2 on in the Cloudflare dashboard, then run cloudex deploy");
      const t0 = Date.now();
      await this.ctx.storage.put("saveGrant", { id, mode: "write" });
      let out: string;
      try {
        out = await this.runScript(SAVE_SCRIPT, [HELPER, id], `saving ${r.name ?? r.id}`, NAMED_SAVE_TIMEOUT_MS);
      } catch (e) {
        await deleteSaveObjects(bucket, id).catch(() => {});
        throw e;
      } finally {
        await this.ctx.storage.delete("saveGrant");
        this.touch();
      }
      const last = out.trim().split("\n").pop() ?? "";
      let s: SaveSummary | undefined;
      try {
        s = JSON.parse(last) as SaveSummary;
      } catch {}
      const counts = s ? [s.parts, s.bytes, s.rawBytes, s.files, s.home, s.deleted] : [];
      if (!s || !counts.every(Number.isInteger) || !s.parts || (s.kind !== "disk" && s.kind !== "home")) {
        await deleteSaveObjects(bucket, id).catch(() => {});
        throw new Error(`unexpected output from the save script: ${last.slice(0, 200)}`);
      }
      const m: SaveManifest = {
        id,
        name,
        sandbox: r.id,
        kind: s.kind,
        image: s.image || null,
        parts: s.parts,
        bytes: s.bytes,
        rawBytes: s.rawBytes,
        files: s.files,
        home: s.home,
        deleted: s.deleted,
        size: r.size,
        idleMinutes: r.idleMinutes,
        createdAt: t0,
        ms: Date.now() - t0,
        settings: await this.ctx.storage.get<StoredSettings>("settings"),
      };
      const replaced = await recordSave(this.env.DB, bucket, m);
      console.log(`[${r.id}] saved as ${name} (${s.kind}, ${s.files} paths, ${s.bytes} bytes) in ${m.ms} ms`);
      return { ...saveView(m), replaced };
    });
  }

  // Whether this sandbox may read or write a named save's objects now: only
  // while this object restores or makes that save (egress.ts asks).
  async saveAccess(id: string): Promise<"read" | "write" | null> {
    const grant = await this.ctx.storage.get<{ id: string; mode: "read" | "write" }>("saveGrant");
    return grant?.id === id ? grant.mode : null;
  }

  private async save() {
    const r = this.mustRecord();
    const t0 = Date.now();
    const snap = await withTimeout(
      this.compute.snapshot(`${r.id}-${t0}`),
      SAVE_TIMEOUT_MS,
      "saving the disk",
    );
    await this.ctx.storage.put("snapshot", snap);
    await this.ctx.storage.put("saveTouchedAt", Date.now());
    // A sandbox started from a named save resumes from its own save from now on.
    await this.ctx.storage.delete("fromSave");
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
      // An alarm that came while it was stopping didn't set the next one.
      await this.ctx.storage.setAlarm(Date.now() + ALARM_EVERY_MS);
      fail(500, `couldn't save ${r.name ?? r.id}, so it is still running: ${message(e)}`);
    }
    await this.backup();
    await this.ctx.storage.deleteAlarm();
    // The save and the backup are done, so it counts as stopped either way.
    await this.destroyContainer(r.id);
    await this.update({ state: "stopped", stoppedAt: Date.now(), error: null });
  }

  // A container that doesn't go keeps billing, so the alarm tries again.
  private async destroyContainer(id: string) {
    try {
      await withTimeout(this.compute.destroy(), 60_000, "destroy");
      // The exit watcher may never settle for a box that never got an instance.
      await Promise.race([this.exited, sleep(10_000)]);
    } catch (e) {
      console.error(`[${id}] ${message(e)}; trying again in a minute`);
      await this.ctx.storage.setAlarm(Date.now() + ALARM_EVERY_MS);
    }
  }

  private async setInactivity(id: string) {
    try {
      await withTimeout(this.compute.keepAlive(), 30_000, "setInactivityTimeout");
      this.inactivitySet = true;
    } catch (e) {
      console.error(`[${id}] ${message(e)}`);
    }
  }

  private leftOver() {
    const state = this.record?.state;
    return (state === "stopped" || state === "error") && this.compute.running;
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
      const p = await this.compute.exec(["bash", "-c", script, "cloudex", ...args], {
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

  private async startExec(input: ExecInput, signal: AbortSignal): Promise<Proc> {
    const home = input.root ? "/root" : GUEST_HOME;
    // A relative directory, or one starting with ~, is in the home directory.
    const cwd = !input.cwd ? home : input.cwd.startsWith("/") ? input.cwd : `${home}/${input.cwd.replace(/^~(\/|$)/, "")}`;
    // The container runtime reports a missing directory as a missing command.
    if (input.cwd) {
      const found = this.compute.exec(["test", "-d", cwd], { cwd: "/", signal }).then((p) => p.exitCode);
      if ((await withTimeout(found, 30_000, "checking the directory")) !== 0) fail(400, `there's no directory ${cwd} in the sandbox`);
    }
    // GNU timeout puts the command in a process group of its own and stops the
    // whole group, so what it started in the background goes too; the abort is
    // a backstop.
    return this.compute.exec(guestArgv(["timeout", "-k", "10", String(input.timeoutSec), "bash", "-lc", input.cmd], input.root), {
      cwd,
      env: input.root
        ? { HOME: "/root", USER: "root", LANG: "C.UTF-8", TERM: "dumb" }
        : { HOME: GUEST_HOME, USER: "cloudex", LOGNAME: "cloudex", LANG: "C.UTF-8", TERM: "dumb" },
      signal,
    });
  }

  async exec(input: ExecInput): Promise<ExecResult> {
    this.mustRun();
    const ctl = new AbortController();
    const timer = setTimeout(() => ctl.abort(), input.timeoutSec * 1000 + 30_000);
    const t0 = Date.now();
    this.execs++;
    try {
      const p = await this.startExec(input, ctl.signal);
      const out = await readOutput(p);
      return { ...out, timedOut: timedOut(out.exitCode, t0, input.timeoutSec) || ctl.signal.aborted, ms: Date.now() - t0 };
    } finally {
      clearTimeout(timer);
      this.execs--;
      this.touch();
    }
  }

  // exec, answering with its output as it comes (ExecEvent lines). Output
  // isn't cut. A client that goes away stops the command.
  async execStream(input: ExecInput): Promise<ReadableStream<Uint8Array>> {
    this.mustRun();
    const ctl = new AbortController();
    const timer = setTimeout(() => ctl.abort(), input.timeoutSec * 1000 + 30_000);
    const t0 = Date.now();
    let p: Proc;
    this.execs++;
    try {
      p = await this.startExec(input, ctl.signal);
    } catch (e) {
      clearTimeout(timer);
      this.execs--;
      throw e;
    }
    // Counted until the command exits, not until a client that stopped
    // reading has taken all its output.
    const exited = p.exitCode.finally(() => {
      this.execs--;
      this.touch();
    });
    const { readable, writable } = new TransformStream<Uint8Array, Uint8Array>();
    const writer = writable.getWriter();
    const enc = new TextEncoder();
    let gone = false;
    const hangUp = () => {
      if (gone) return;
      gone = true;
      try {
        p.kill(15); // timeout passes it on to the command's whole process group
      } catch {
        // already ended
      }
    };
    // The client going away cancels the stream, even while the command is quiet.
    writer.closed.catch(hangUp);
    let lastSent = Date.now();
    const send = async (ev: ExecEvent) => {
      if (gone) return;
      lastSent = Date.now();
      await writer.write(enc.encode(`${JSON.stringify(ev)}\n`)).catch(hangUp);
    };
    // Something between the client and Cloudflare can drop a connection that
    // has been quiet for a few minutes, so a quiet command sends keepalives.
    const keepalive = setInterval(() => {
      if (Date.now() - lastSent >= KEEPALIVE_MS) void send({ type: "keepalive" });
    }, KEEPALIVE_MS / 3);
    let stop!: () => void;
    const stopped = new Promise<void>((resolve) => (stop = resolve));
    const pump = async (stream: ReadableStream | null, type: "stdout" | "stderr") => {
      if (!stream) return;
      const reader = stream.getReader();
      void stopped.then(() => reader.cancel().catch(() => {}));
      const dec = new TextDecoder();
      try {
        for (;;) {
          const { done, value } = await reader.read();
          if (done) break;
          const data = dec.decode(value instanceof Uint8Array ? value : new Uint8Array(value as ArrayBuffer), { stream: true });
          if (data) await send({ type, data });
          this.touch();
        }
      } catch {
        // cancelled after the grace period
      }
      const rest = dec.decode();
      if (rest) await send({ type, data: rest });
    };
    const pumps = Promise.all([pump(p.stdout, "stdout"), pump(p.stderr, "stderr")]);
    const done = (async () => {
      let grace: ReturnType<typeof setTimeout> | null = null;
      try {
        const exitCode = await exited;
        grace = setTimeout(stop, EXIT_GRACE_MS);
        await pumps;
        await send({ type: "exit", exitCode, timedOut: timedOut(exitCode, t0, input.timeoutSec) || ctl.signal.aborted, ms: Date.now() - t0 });
      } catch (e) {
        await send({ type: "error", error: message(e) });
      } finally {
        if (grace) clearTimeout(grace);
        clearInterval(keepalive);
        stop();
        clearTimeout(timer);
        this.touch();
        await writer.close().catch(() => {});
      }
    })();
    this.ctx.waitUntil(done);
    return readable;
  }

  // Runs a guest program as the cloudex user or root, the way exec runs
  // commands, and answers its standard output. Exit codes in `codes` become
  // HTTP statuses with the last line of standard error as the message.
  private async guest(
    argv: string[],
    o: { root?: boolean; what: string; timeoutMs: number; stdin?: ReadableStream | string; codes: Record<number, number> },
  ): Promise<string> {
    const ctl = new AbortController();
    const timer = setTimeout(() => ctl.abort(), o.timeoutMs);
    try {
      const p = await this.compute.exec(guestArgv(argv, o.root), {
        cwd: o.root ? "/root" : GUEST_HOME,
        env: o.root
          ? { HOME: "/root", USER: "root", LANG: "C.UTF-8" }
          : { HOME: GUEST_HOME, USER: "cloudex", LOGNAME: "cloudex", LANG: "C.UTF-8" },
        ...(o.stdin === undefined ? {} : { stdin: typeof o.stdin === "string" ? new Response(o.stdin).body! : o.stdin }),
        signal: ctl.signal,
      });
      const out = await p.output();
      if (out.exitCode !== 0) {
        const why = clip(out.stderr).trim().split("\n").pop() ?? "";
        const status = o.codes[out.exitCode];
        if (status) fail(status, why);
        throw new Error(`${o.what} failed (exit ${out.exitCode}): ${why}`);
      }
      return new TextDecoder().decode(out.stdout);
    } catch (e) {
      if (ctl.signal.aborted) throw new Error(`${o.what} took longer than ${Math.round(o.timeoutMs / 1000)} s`);
      throw e;
    } finally {
      clearTimeout(timer);
    }
  }

  // One operation of the background command script (commands.ts), as root.
  async commandOp(op: string, args: string[], stdin?: string, waitSec = 0): Promise<unknown> {
    this.mustRun();
    try {
      const out = await this.guest(["bash", "-c", COMMAND_BOOT, "cloudex", COMMAND_SCRIPT, op, ...args], {
        root: true,
        what: `command ${op}`,
        timeoutMs: (waitSec + 60) * 1000,
        stdin,
        codes: { [COMMAND_NOT_FOUND]: 404, [COMMAND_BAD_REQUEST]: 400, [COMMAND_CONFLICT]: 409 },
      });
      return JSON.parse(out);
    } finally {
      this.touch();
    }
  }

  private fileOp(op: string, args: string[], root: boolean, stdin?: ReadableStream, timeoutMs = 60_000) {
    return this.guest(["python3", "-I", "-c", FILE_SCRIPT, op, ...args], {
      root,
      what: `file ${op}`,
      timeoutMs,
      stdin,
      codes: { [FILE_NOT_FOUND]: 404, [FILE_BAD_REQUEST]: 400, [FILE_FORBIDDEN]: 403 },
    });
  }

  async fileInfo(path: string, root: boolean): Promise<FileInfo> {
    this.mustRun();
    return JSON.parse(await this.fileOp("stat", [path], root));
  }

  async deleteFile(path: string, root: boolean, recursive: boolean): Promise<FileInfo> {
    this.mustRun();
    return JSON.parse(await this.fileOp("delete", [path, recursive ? "1" : "0"], root, undefined, 10 * 60_000));
  }

  async writeFile(path: string, root: boolean, mode: string | null, body: ReadableStream | null, bytes?: number): Promise<FileInfo> {
    this.mustRun();
    try {
      const args = [path, mode ?? "", bytes === undefined ? "" : String(bytes)];
      return JSON.parse(await this.fileOp("write", args, root, body ?? new Response("").body!, FILE_TIMEOUT_MS));
    } finally {
      this.touch();
    }
  }

  // A file's contents, or with archive a tar.gz of a directory or file, as it
  // streams out of the sandbox.
  async readFile(path: string, root: boolean, archive: boolean): Promise<{ info: FileInfo; body: ReadableStream }> {
    this.mustRun();
    const info = JSON.parse(await this.fileOp("check", [path, archive ? "any" : "file"], root)) as FileInfo;
    const argv = archive ? ["bash", "-c", ARCHIVE_SCRIPT, "cloudex", info.path] : ["cat", "--", info.path];
    // A deadline, in case the file was swapped for a pipe that never ends.
    const ctl = new AbortController();
    const timer = setTimeout(() => ctl.abort(), FILE_TIMEOUT_MS);
    const p = await this.compute.exec(guestArgv(argv, root), {
      cwd: "/",
      env: { HOME: root ? "/root" : GUEST_HOME, LANG: "C.UTF-8" },
      stderr: "ignore",
      signal: ctl.signal,
    });
    this.ctx.waitUntil(p.exitCode.then(() => this.touch(), () => {}).finally(() => clearTimeout(timer)));
    return { info, body: p.stdout! };
  }

  // Unpacks a tar.gz into a directory, making it first if need be.
  async writeArchive(path: string, root: boolean, body: ReadableStream | null): Promise<ArchiveResult> {
    this.mustRun();
    if (!body) fail(400, "send a tar.gz archive as the request body");
    try {
      const dir = JSON.parse(await this.fileOp("mkdir", [path], root)) as FileInfo;
      const out = await this.guest(["bash", "-c", UNPACK_SCRIPT, "cloudex", dir.path], {
        root,
        what: "unpacking the archive",
        timeoutMs: FILE_TIMEOUT_MS,
        stdin: body,
        codes: {},
      }).catch((e: unknown) => fail(400, `couldn't unpack the archive: ${message(e).replace(/^unpacking the archive failed \(exit \d+\): /, "")}`));
      return { path: dir.path, entries: Number(out.trim()) || 0 };
    } finally {
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
      const p = await this.compute.exec(guestArgv(["bash", "-lc", argv[0]!, "cloudex", ...argv.slice(1)]), {
        cwd: GUEST_HOME,
        env: { HOME: GUEST_HOME, USER: "cloudex", LOGNAME: "cloudex", LANG: "C.UTF-8" },
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

  async desktop(size?: string): Promise<Desktop> {
    const r = this.mustRun();
    const t0 = Date.now();
    try {
      const out = await this.runScript(DESKTOP_SCRIPT, [size ?? "", r.name ?? r.id], "starting the desktop", DESKTOP_TIMEOUT_MS);
      const last = out.trim().split("\n").pop() ?? "";
      const { installed, started, size: shown } = JSON.parse(last) as { installed: boolean; started: boolean; size: string };
      console.log(`[${r.id}] desktop ${started ? "started" : "running"} at ${shown}${installed ? ", installed" : ""} in ${Date.now() - t0} ms`);
      return { port: DESKTOP_PORT, path: DESKTOP_VIEWER, size: shown, installed, started };
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

  private idle() {
    const r = this.record;
    const idleMs = (r?.idleMinutes ?? 0) * 60_000;
    return r?.state === "running" && idleMs > 0 && this.execs === 0 && Date.now() - this.lastActivity > idleMs;
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
    if (r && this.leftOver()) {
      return this.exclusive(async () => {
        if (this.leftOver()) await this.destroyContainer(r.id);
      });
    }
    if (!r || r.state !== "running") return;
    if (this.onNode) await this.nodeAlarm();
    if (!this.compute.running) return this.onExit();
    // An object that restarted, as after a deploy, leaves its running box on
    // the platform's short default until it's set again.
    if (!this.inactivitySet) await this.setInactivity(r.id);
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
    if (this.idle() && !this.onNode) {
      try {
        // Asked again in the queue: what ran ahead of it, such as a long save,
        // may have been activity.
        const stopped = await this.exclusive(async () => {
          if (!this.idle()) return false;
          await this.saveAndDestroy();
          return true;
        });
        if (stopped) {
          console.log(`[${r.id}] stopped after ${r.idleMinutes} idle minutes`);
          return;
        }
      } catch (e) {
        console.error(`[${r.id}] idle stop failed: ${message(e)}`);
      }
    }
    // Sooner if a stop was asked for from inside while this ran.
    const asked = await this.ctx.storage.get<boolean>("stopRequested");
    // None after a stop or delete that ran ahead of it in the queue, since an
    // alarm brings a deleted sandbox's storage back.
    if (this.record?.state !== "running") return;
    await this.ctx.storage.setAlarm(Date.now() + (asked ? 1000 : ALARM_EVERY_MS));
  }

  // The Worker forwards these requests here:
  //   /tcp/<port>[?key=<ssh public key>]  WebSocket bridged to a guest TCP port
  //   /http/<port>/<path>                 an HTTP preview of a guest port
  //   /term?session=&root=&cols=&rows=    WebSocket to a shell under a pty
  //   /vnc                                WebSocket to the desktop's VNC server
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
    if (url.pathname === "/node-stream") {
      if (!this.onNode) return new Response("this sandbox isn't on a node\n", { status: 403 });
      return this.streams.accept(req);
    }
    if (this.onNode) this.streams.reap();
    const tokenId = req.headers.get(TOKEN_HEADER);
    if (url.pathname === "/term" || url.pathname === "/vnc") {
      if (r.state !== "running") return new Response(`${wrongState(r)}\n`, { status: 409 });
      if (req.headers.get("Upgrade")?.toLowerCase() !== "websocket") {
        return new Response("expected a WebSocket upgrade", { status: 426 });
      }
      this.touch();
      if (url.pathname === "/term") return this.terminal(url, tokenId);
      // The VNC server listens on localhost only, so through the relay previews use.
      const through = await this.relay(VNC_PORT, this.compute.guestAddress);
      if (through === null) return new Response("the desktop isn't running: start it first\n", { status: 409 });
      if (!through) return new Response("couldn't reach the desktop's VNC server\n", { status: 502 });
      return this.bridge(through, tokenId);
    }
    const m = /^\/(tcp|http)\/(\d{1,5})(\/.*)?$/.exec(url.pathname);
    if (!m) return new Response("not found", { status: 404 });
    const port = Number(m[2]);
    const token = req.headers.get(LINK_HEADER) ?? "";
    const revocations = this.revocations;
    if (m[1] === "http" && !(await this.checkLink(port, token))) {
      return new Response("not found", { status: 404 });
    }
    if (r.state !== "running") {
      return new Response(`${wrongState(r)}\n`, { status: 409 });
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
    return this.bridge(port, tokenId);
  }

  // The Worker checked the token that opened a WebSocket at the upgrade only,
  // so this checks it about once a minute while the socket is open, and ends
  // the socket with CREDENTIAL_ENDED once the token (or the token a session
  // acts for) is revoked or expires, or the session signs out. No id: the
  // admin key, which has no row. A D1 error leaves it open until the next check.
  // Returns what stops the checks, which closing the socket does too.
  private watchCredential(ws: WebSocket, tokenId: string | null, ended: () => void): () => void {
    if (!tokenId) return () => {};
    const timer = setInterval(async () => {
      if (!(await tokenAlive(this.env.DB, tokenId).catch(() => true))) {
        clearInterval(timer);
        try {
          ws.close(CREDENTIAL_ENDED, "signed out: the token or session that opened this has ended");
        } catch {}
        ended();
      }
    }, CREDENTIAL_CHECK_MS);
    const stop = () => clearInterval(timer);
    ws.addEventListener("close", stop);
    ws.addEventListener("error", stop);
    return stop;
  }

  // A preview request for a guest port. A server listening only on localhost
  // doesn't answer at the sandbox's address, so then a relay (guest.ts) starts
  // for it, and requests for the port go through the relay while it runs. A
  // body with a content-length up to REPLAY_LIMIT is held here, so that the
  // request can be sent again after a miss; any other goes only once.
  private async preview(port: number, target: string, req: Request): Promise<Response> {
    const r = this.mustRecord();
    const length = req.headers.get("content-length");
    const held = req.body && length !== null && Number(length) <= REPLAY_LIMIT ? await req.arrayBuffer() : null;
    const once = held === null && req.body !== null;
    const send = (to: number) => {
      const fwd = held
        ? new Request(target, { method: req.method, headers: req.headers, body: held, redirect: req.redirect })
        : new Request(target, req);
      fwd.headers.delete(LINK_HEADER);
      return this.compute.fetch(to, fwd);
    };
    let why = "";
    let addr: string | undefined;
    const relay = this.relayed.get(port);
    for (const to of relay ? [relay, port] : [port]) {
      try {
        return await send(to);
      } catch (e) {
        why = message(e);
        addr = e instanceof NotListening ? e.addr : undefined;
        if (to !== port) this.relayed.delete(port);
        if (once) break;
      }
    }
    if (addr) {
      const through = await this.relay(port, addr);
      if (through === null) {
        return new Response(`nothing is listening on port ${port} in ${r.name ?? r.id}\n`, { status: 502 });
      }
      if (through) {
        if (through !== port) this.relayed.set(port, through);
        if (once) {
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
        this.compute.exec(["bash", "-c", RELAY_SCRIPT, "cloudex", String(port), addr], { cwd: "/", env: { LANG: "C.UTF-8" } }),
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
    const p = await this.compute.exec(["sh", "-c", script], {
      user: GUEST_USER,
      env: { HOME: GUEST_HOME, KEY: key },
    });
    const code = await p.exitCode;
    if (code !== 0) throw new Error(`adding the SSH key failed (exit ${code})`);
  }

  // A shell under a pty, bridged to a WebSocket (S3 in research/v0.2-plan.md).
  // Binary messages are the terminal's bytes; text messages are JSON control:
  // {"resize":[cols,rows]}, {"ping":1}, {"kill":1}. The shell's exit code
  // comes as {"exit":N} before the socket closes. With session, the shell runs
  // in that tmux session (made if it's new) on a tmux server of the
  // dashboard's own, so a socket that drops, as a deploy does, reconnects to
  // the same shell. Closing the socket hangs up, as closing ssh does: a plain
  // shell ends, a tmux session stays until it's killed or its shell exits.
  // Only messages from the browser count as activity, so the dashboard pings
  // while its tab is visible.
  private async terminal(url: URL, tokenId: string | null): Promise<Response> {
    const size = (v: unknown, d: number) => Math.min(1000, Math.max(2, Math.floor(Number(v)) || d));
    const cols = size(url.searchParams.get("cols"), 80);
    const rows = size(url.searchParams.get("rows"), 24);
    const root = ["1", "true"].includes(url.searchParams.get("root") ?? "");
    const session = url.searchParams.get("session");
    if (session !== null && !TMUX_SESSION.test(session)) {
      return new Response("a session name is 1-40 letters, digits, - and _\n", { status: 400 });
    }
    const home = root ? "/root" : GUEST_HOME;
    const user = root ? "root" : "cloudex";
    const env = { HOME: home, USER: user, LOGNAME: user, SHELL: "/bin/bash", TERM: "xterm-256color", LANG: "C.UTF-8", COLORTERM: "truecolor" };
    const argv = session === null ? ["bash", "-l"] : tmuxArgv(session);
    const p = await this.compute.exec(guestArgv(argv, root), { cwd: home, env, pty: { cols, rows }, stdin: "pipe" });
    const pair = new WebSocketPair();
    const [client, server] = [pair[0], pair[1]];
    server.accept();
    server.binaryType = "arraybuffer";
    const writer = p.stdin!.getWriter();
    let closed = false;
    const done = () => {
      if (closed) return;
      closed = true;
      unwatch();
      this.touch();
      writer.close().catch(() => {});
      try {
        p.kill(1); // SIGHUP, as a closed terminal sends
      } catch {}
    };
    server.addEventListener("message", (e) => {
      this.touch();
      if (typeof e.data !== "string") {
        writer.write(new Uint8Array(e.data as ArrayBuffer)).catch(() => server.close(1011, "the shell is gone"));
        return;
      }
      let m: { resize?: unknown; kill?: unknown };
      try {
        m = JSON.parse(e.data);
      } catch {
        return;
      }
      if (Array.isArray(m.resize)) {
        try {
          p.resize(size(m.resize[0], cols), size(m.resize[1], rows));
        } catch {}
      }
      if (m.kill && session !== null) {
        void this.compute
          .exec(guestArgv(["tmux", "-L", TMUX_SOCKET, "kill-session", "-t", `=${session}`], root), { cwd: home, env })
          .then((k) => k.exitCode)
          .catch(() => {});
      }
    });
    server.addEventListener("close", done);
    server.addEventListener("error", done);
    const unwatch = this.watchCredential(server, tokenId, () => done());
    void (async () => {
      const reader = p.stdout!.getReader();
      try {
        for (;;) {
          const { value, done: eof } = await reader.read();
          if (eof) break;
          server.send(value);
        }
      } catch {
        // the shell's output ended
      } finally {
        const code = await p.exitCode.catch(() => -1);
        try {
          server.send(JSON.stringify({ exit: code }));
          server.close(1000, "exited");
        } catch {}
        done();
      }
    })();
    return new Response(null, { status: 101, webSocket: client });
  }

  // WebSocket <-> guest TCP. Reads from the guest are coalesced into frames of
  // up to 256 KiB, which took scp downloads from 6-8 to 17 MiB/s in the spikes.
  private async bridge(port: number, tokenId: string | null): Promise<Response> {
    // On a node, ssh is a splice that lets this object hibernate while the
    // session is idle; other ports are framed streams with flow control.
    if (this.onNode && port === 22) return this.streams.splice(port, tokenId);
    const sock = this.compute.connect(port);
    try {
      await withTimeout(sock.opened, 15_000, `connecting to port ${port}`);
    } catch (e) {
      return new Response(`nothing is listening on port ${port}: ${message(e)}`, { status: 502 });
    }
    // The connection opens even when nothing listens on the port, and then
    // ends at once with nothing read. A server that speaks first (ssh, VNC)
    // answers within the wait; a quiet one costs it once per connection.
    const reader = sock.readable.getReader();
    const first = reader.read().catch(() => ({ done: true as const, value: undefined }));
    const early = await Promise.race([first, sleep(PROBE_MS).then(() => null)]);
    if (early?.done) {
      reader.releaseLock();
      await sock.close().catch(() => {});
      return new Response(`nothing is listening on port ${port}\n`, { status: 502 });
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
      unwatch();
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
    const unwatch = this.watchCredential(server, tokenId, () => done());

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
      try {
        for (let read = first; ; read = reader.read()) {
          const { value, done: eof } = await read;
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

  // ---------------------------------------------------------------- nodes (v0.2.2 M3)

  // The Hibernation API's handlers. Only a node's stream sockets (and the
  // clients spliced to them) are accepted that way here (nodestream.ts).
  async webSocketMessage(ws: WebSocket, msg: string | ArrayBuffer) {
    if (this.streams.handles(ws)) this.streams.onMessage(ws, msg);
  }

  async webSocketClose(ws: WebSocket, code: number, reason: string) {
    if (this.streams.handles(ws)) this.streams.onClose(ws, code, reason);
  }

  async webSocketError(ws: WebSocket) {
    if (this.streams.handles(ws)) this.streams.onClose(ws, 1011, "error");
  }

  // From the 60 s alarm: dead streams go, and spliced sessions whose token
  // was revoked end, as watchCredential ends the others.
  private async nodeAlarm() {
    const closed = this.streams.reap();
    if (closed) console.log(`[${this.record?.id}] closed ${closed} dead node streams`);
    for (const c of this.streams.clients()) {
      if (!(await tokenAlive(this.env.DB, c.tokenId).catch(() => true))) {
        this.streams.endClient(c.id, CREDENTIAL_ENDED, "signed out: the token or session that opened this has ended");
      }
    }
  }

  // M3's development path (POST /v1/nodes/{node}/dev-sandboxes, admins only):
  // a sandbox whose machine is a container the node's agent was told to adopt
  // with --dev-machine. It runs from the start and can't be stopped, saved or
  // forked; deleting it releases the container, which stays on the node.
  async adoptDev(id: string, node: string, name: string | null): Promise<SandboxRecord> {
    return this.exclusive(async () => {
      if (this.record) fail(409, `${id} already exists`);
      const answer = (await this.env.NODE.getByName(node).op("adopt", { sandbox: id }, 15_000)) as Record<string, unknown>;
      const address = typeof answer.address === "string" && /^\d{1,3}(\.\d{1,3}){3}$/.test(answer.address) ? answer.address : null;
      if (!address) fail(502, "the node's agent didn't say where the machine is");
      const placement: NodePlacement = { node, address, dev: true };
      const now = Date.now();
      this.record = {
        id,
        name,
        size: "standard-2",
        state: "running",
        idleMinutes: 0,
        createdAt: now,
        updatedAt: now,
        startedAt: now,
        stoppedAt: null,
        savedAt: null,
        savedBytes: null,
        error: null,
        notice: "A development machine on a node (v0.2.2 M3): it can't be stopped, saved or forked.",
      };
      this.place(placement);
      await this.ctx.storage.put("node", placement);
      await this.ctx.storage.put("selfToken", crypto.randomUUID());
      await this.update({});
      await this.ctx.storage.setAlarm(Date.now() + ALARM_EVERY_MS);
      return this.mustRecord();
    });
  }

  // The node was removed (cloudex node rm): its streams close, and no more are taken.
  async nodeRevoked(node: string) {
    if (this.onNode?.placement.node !== node) return;
    await this.ctx.storage.put("revokedNode", node);
    this.streams.closeAll(4003, "the node was removed");
  }

  // Counts of this sandbox's node streams, for the release checks.
  async nodeStreams(): Promise<{ spares: number; open: number } | null> {
    return this.onNode ? this.streams.count() : null;
  }
}
