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
import { DurableObject } from "cloudflare:workers";
import type { Desktop, ExecResult, Sandbox as SandboxRecord, SandboxSize } from "@cloudex/schema";
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
import { deleteSandbox, upsertSandbox } from "./db";
import { DESKTOP_PORT, DESKTOP_SCRIPT, DESKTOP_STOP_SCRIPT, DESKTOP_VIEWER } from "./desktop";
import { fail, sleep, withTimeout } from "./util";

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

type CreateInput = { name: string | null; size: SandboxSize; idleMinutes: number };
type ExecInput = { cmd: string; root: boolean; cwd?: string; timeoutSec: number };

const message = (e: unknown) => (e instanceof Error ? e.message : String(e)).replace(/^\[\d{3}\] /, "");

function clip(buf: ArrayBuffer) {
  const text = new TextDecoder().decode(buf.byteLength > OUTPUT_LIMIT ? buf.slice(0, OUTPUT_LIMIT) : buf);
  return buf.byteLength > OUTPUT_LIMIT ? `${text}\n[cloudex: output cut at 1 MiB of ${buf.byteLength} bytes]` : text;
}

export class Sandbox extends DurableObject<Env> {
  private record: SandboxRecord | undefined;
  private queue: Promise<unknown> = Promise.resolve();
  private connections = 0;
  private lastActivity = Date.now();
  private lastActivitySaved = 0;
  private exitedAt = 0;

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
    if (!this.record) fail(404, "no such sandbox");
    return this.record;
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

  async create(id: string, input: CreateInput): Promise<SandboxRecord> {
    return this.exclusive(async () => {
      if (this.record) fail(409, `${id} already exists`);
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
      };
      await this.update({});
      await this.boot();
      return this.mustRecord();
    });
  }

  async get(): Promise<SandboxRecord> {
    return this.mustRecord();
  }

  async resume(fromBackup = false): Promise<SandboxRecord> {
    return this.exclusive(async () => {
      const r = this.mustRecord();
      if (r.state === "running") return r;
      if (r.state !== "stopped" && r.state !== "error") fail(409, `${r.id} is ${r.state}`);
      const snapshot = await this.ctx.storage.get("snapshot");
      const touched = (await this.ctx.storage.get<number>("saveTouchedAt")) ?? r.savedAt ?? 0;
      const expired = Boolean(snapshot) && Date.now() - touched > SAVE_LIFETIME_MS;
      if (fromBackup || expired) {
        const bucket = backupBucket(this.env);
        const backup = bucket ? await latestBackup(bucket, r.id) : null;
        if (backup) {
          await this.bootFromBackup(backup, fromBackup ? "as asked" : "its save had expired");
          return this.mustRecord();
        }
        if (fromBackup) fail(404, bucket ? `${r.id} has no backup yet` : "backups are off: this deployment has no R2 bucket");
      }
      await this.boot(snapshot);
      // Restoring a save restarts its 30 days.
      if (snapshot) await this.ctx.storage.put("saveTouchedAt", Date.now());
      return this.mustRecord();
    });
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

  async remove(): Promise<{ deleted: string }> {
    return this.exclusive(async () => {
      const r = this.mustRecord();
      this.record = { ...r, state: "stopping" }; // so the exit watcher stays out of it
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
    });
  }

  private async boot(snapshot?: unknown, afterReady?: () => Promise<void>) {
    const r = this.mustRecord();
    const c = this.container;
    const t0 = Date.now();
    await this.update({ state: "starting", error: null, notice: null });
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
      const egress = this.ctx.exports.Egress({ props: { id: r.id } });
      await withTimeout(c.interceptOutboundHttp("cloudex.internal", egress), 30_000, "routing cloudex.internal");
      await this.waitReady(t0);
      if (afterReady) await afterReady();
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
    await this.update({ state: "running", startedAt: Date.now(), stoppedAt: null });
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

  // Runs one of the guest scripts as root, with args as $1, $2 and so on. Returns stdout.
  private async runScript(script: string, args: string[], what: string, timeoutMs = BACKUP_TIMEOUT_MS): Promise<string> {
    const ctl = new AbortController();
    const timer = setTimeout(() => ctl.abort(), timeoutMs);
    try {
      const p = await this.container.exec(["bash", "-c", script, "cloudex", ...args], {
        cwd: "/",
        env: { HOME: "/root", LANG: "C.UTF-8" },
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
    const r = this.mustRecord();
    if (r.state !== "running") fail(409, `${r.id} is ${r.state}; resume it first`);
    this.touch();
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
      const out = await p.output();
      return {
        exitCode: out.exitCode,
        stdout: clip(out.stdout),
        stderr: clip(out.stderr),
        timedOut,
        ms: Date.now() - t0,
      };
    } finally {
      clearTimeout(timer);
      this.touch();
    }
  }

  // Starts the desktop, installing it the first time, or resizes a running one.
  async desktop(size: string): Promise<Desktop> {
    const r = this.mustRecord();
    if (r.state !== "running") fail(409, `${r.id} is ${r.state}; resume it first`);
    this.touch();
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
  // connections, so this is what actually ends a quiet box.
  async alarm() {
    const r = this.record;
    if (!r || r.state !== "running") return;
    if (!this.ctx.container?.running) return this.onExit();
    const idleMs = r.idleMinutes * 60_000;
    if (idleMs > 0 && this.connections === 0 && Date.now() - this.lastActivity > idleMs) {
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
    await this.ctx.storage.setAlarm(Date.now() + ALARM_EVERY_MS);
  }

  // The Worker forwards two kinds of request here:
  //   /tcp/<port>[?key=<ssh public key>]  WebSocket bridged to a guest TCP port
  //   /http/<port>/<path>                 an HTTP preview of a guest port
  async fetch(req: Request): Promise<Response> {
    const r = this.record;
    if (!r) return new Response("no such sandbox", { status: 404 });
    const url = new URL(req.url);
    const m = /^\/(tcp|http)\/(\d{1,5})(\/.*)?$/.exec(url.pathname);
    if (!m) return new Response("not found", { status: 404 });
    const port = Number(m[2]);
    if (r.state !== "running") {
      return new Response(`${r.id} is ${r.state}. Run: cloudex resume ${r.name ?? r.id}\n`, { status: 409 });
    }
    this.touch();
    if (m[1] === "http") {
      const target = new URL(`http://container${m[3] ?? "/"}${url.search}`);
      try {
        return await this.container.getTcpPort(port).fetch(new Request(target, req));
      } catch (e) {
        return new Response(`nothing is answering on port ${port} in ${r.name ?? r.id}: ${message(e)}\n`, { status: 502 });
      }
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
    this.connections++;
    const writer = sock.writable.getWriter();
    let closed = false;
    const done = () => {
      if (closed) return;
      closed = true;
      this.connections--;
      this.touch();
      writer.close().catch(() => {});
    };
    server.addEventListener("message", (e) => {
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
