// Where a sandbox's machine runs. The Sandbox object (sandbox.ts) owns the
// lifecycle and asks this for everything it does to the machine itself, so
// that the container binding is used here and nowhere else. CloudflareCompute
// is the only kind so far: a Cloudflare container owned by the Durable Object.
//
// Each call keeps the shape of the platform call it stands for. Deadlines stay
// with the caller (sandbox.ts), except where noted.
import type { SandboxSize } from "@cloudex/schema";
import { withTimeout } from "./util";

export type ExecOpts = {
  cwd?: string;
  env?: Record<string, string>;
  user?: string;
  stdin?: ReadableStream | "pipe";
  pty?: { cols: number; rows: number };
  stderr?: "ignore";
  signal?: AbortSignal;
};

export type Proc = {
  readonly stdin: WritableStream | null; // with stdin: "pipe"
  readonly stdout: ReadableStream | null;
  readonly stderr: ReadableStream | null;
  readonly exitCode: Promise<number>;
  output(): Promise<{ readonly exitCode: number; readonly stdout: ArrayBuffer; readonly stderr: ArrayBuffer }>;
  kill(signal?: number): void;
  resize(cols: number, rows: number): void;
};

// A TCP connection to a guest port. It opens even when nothing listens there,
// and then ends at once with nothing read (see bridge in sandbox.ts).
export type Stream = {
  readonly opened: Promise<unknown>;
  readonly readable: ReadableStream;
  readonly writable: WritableStream;
  close(): Promise<void>;
};

export type StartOpts = {
  snapshot?: unknown; // a save from snapshot(); without one, the image
  size: SandboxSize;
  env: Record<string, string>;
  labels: Record<string, string>;
};

// Nothing listens on the port at the guest's address; addr is the address
// the request went to. The message is the platform's own.
export class NotListening extends Error {
  constructor(
    readonly addr: string,
    message: string,
  ) {
    super(message);
  }
}

export interface Compute {
  // Where the guest's servers answer, for connections from this object.
  readonly guestAddress: string;
  // The guest's path of the CA that intercepted HTTPS calls (route) are
  // signed with, which KEYS_SCRIPT (keys.ts) waits for and trusts.
  readonly caPath: string;
  // Whether a machine is up, from the last run or this one.
  readonly running: boolean;
  start(o: StartOpts): void;
  // Settles when the machine stops, rejecting if it failed.
  monitor(): Promise<void>;
  // cloudex.internal over HTTP, and hosts over HTTPS, go to egress (egress.ts),
  // one after another, each with a deadline of its own.
  route(egress: Fetcher, hosts: string[]): Promise<void>;
  // Keeps the platform from stopping a running machine before this object does.
  keepAlive(): Promise<void>;
  snapshot(name: string): Promise<{ id: string; size?: number }>;
  destroy(): Promise<void>;
  exec(argv: string[], o?: ExecOpts): Promise<Proc>;
  connect(port: number): Stream;
  // Rejects with NotListening when nothing listens on the port.
  fetch(port: number, req: Request): Promise<Response>;
}

const PLATFORM_MAX_INACTIVITY_MS = 6 * 3600_000;

export class CloudflareCompute implements Compute {
  readonly guestAddress = "10.0.0.1";
  readonly caPath = "/etc/cloudflare/certs/cloudflare-containers-ca.crt";

  constructor(private readonly ctx: DurableObjectState) {}

  private get c() {
    const c = this.ctx.container;
    if (!c) throw new Error("no container is configured for this Durable Object");
    return c;
  }

  get running() {
    return Boolean(this.ctx.container?.running);
  }

  start(o: StartOpts) {
    const c = this.c;
    const images = c.images as Record<string, string>;
    c.start({
      ...(o.snapshot ? { containerSnapshot: o.snapshot } : { image: images.base }),
      instance: o.size,
      enableInternet: true,
      env: o.env,
      labels: o.labels,
    } as ContainerStartupOptions);
  }

  monitor() {
    return this.c.monitor();
  }

  async route(egress: Fetcher, hosts: string[]) {
    const c = this.c;
    await withTimeout(c.interceptOutboundHttp("cloudex.internal", egress), 30_000, "routing cloudex.internal");
    for (const host of hosts) {
      await withTimeout(c.interceptOutboundHttps(host, egress), 30_000, `routing ${host}`);
    }
  }

  // Set to the platform maximum, because alarms and guest pings don't extend
  // it; the Sandbox object stops idle machines itself.
  keepAlive() {
    return this.c.setInactivityTimeout(PLATFORM_MAX_INACTIVITY_MS);
  }

  snapshot(name: string) {
    return this.c.snapshotContainer({ name });
  }

  destroy() {
    return this.c.destroy();
  }

  exec(argv: string[], o?: ExecOpts) {
    return o === undefined ? this.c.exec(argv) : this.c.exec(argv, o);
  }

  connect(port: number) {
    return this.c.getTcpPort(port).connect(`${this.guestAddress}:${port}`);
  }

  async fetch(port: number, req: Request) {
    try {
      return await this.c.getTcpPort(port).fetch(req);
    } catch (e) {
      const why = e instanceof Error ? e.message : String(e);
      const addr = /not listening in the TCP address (\d{1,3}(?:\.\d{1,3}){3}):\d+/.exec(why)?.[1];
      if (addr) throw new NotListening(addr, why);
      throw e;
    }
  }
}
