// Where a sandbox's machine runs. The Sandbox object (sandbox.ts) owns the
// lifecycle and asks this for everything it does to the machine itself, so
// that the container binding is used here and nowhere else. Two kinds:
// - CloudflareCompute: a Cloudflare container owned by the Durable Object.
// - NodeCompute (v0.2.2 M3): a machine on a node, a user's own Docker host,
//   reached through stream sockets its agent opens into this object
//   (nodestream.ts). Its lifecycle (create, start, stop, delete) goes through
//   the node's Node object as ops, which sandbox.ts calls itself (M5), so
//   start, snapshot and destroy here are never used for it.
//
// Each call keeps the shape of the platform call it stands for. Deadlines stay
// with the caller (sandbox.ts), except where noted.
import type { SandboxSize } from "@cloudex/schema";
import { StreamChannel } from "@cloudex/schema/node";
import { type Framed, type NodeStreams, StreamGone } from "./nodestream";
import { fail, withTimeout } from "./util";

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
  readonly kind: "cloudflare" | "node";
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
  readonly kind = "cloudflare";
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

// ---------------------------------------------------------------- nodes

// A sandbox's place on a node (v0.2.2 M5): which node (its id, and its name
// for messages), the machine's address there (empty until its first start),
// and the limits it was made with.
export type NodePlacement = { node: string; name: string; address: string; cpus?: number; memoryMb?: number };

// A preview's request headers, as the node's agent sends them on.
function headerList(req: Request): [string, string][] {
  const out: [string, string][] = [["host", new URL(req.url).host]];
  for (const [k, v] of req.headers) if (k !== "host") out.push([k, v]);
  return out;
}

const NO_BODY = new Set([101, 204, 205, 304]);

export class NodeCompute implements Compute {
  readonly kind = "node";
  // The node CA (v0.2.2 M6): the agent ends its machines' TLS to the AI
  // providers with it, and gives its certificate with each start (ca), which
  // KEYS_SCRIPT writes here and trusts.
  readonly caPath = "/etc/cloudex/node-ca.crt";
  ca: string | null = null;
  // Never a container of this object's: what runs on the node is the node's
  // Node object's to report (sandbox.ts nodeReport), so the checks that ask
  // Cloudflare's binding whether a box is left over stay out of it.
  readonly running = false;

  constructor(
    private readonly streams: NodeStreams,
    readonly placement: NodePlacement,
  ) {}

  get guestAddress() {
    return this.placement.address;
  }

  start(): void {
    fail(500, "a node's machine is started through its Node object");
  }
  monitor(): Promise<void> {
    // Exits come from the node's state messages (sandbox.ts nodeReport).
    return new Promise(() => {});
  }
  async route() {
    // On a node, cloudex.internal and the providers' hosts are the agent's
    // own listeners (go/internal/node/egress.go), which pass requests on to
    // the Worker's /v1/node/egress (nodeegress.ts), and so to Egress.
  }
  async keepAlive() {
    // A node has no platform timer, and no 6-hour limit.
  }
  snapshot(): Promise<{ id: string; size?: number }> {
    return fail(501, "a sandbox on a node has no disk snapshots");
  }
  async destroy() {
    // A node's machine is removed through its Node object (sandbox.ts removeMachine).
    this.streams.closeAll(1000, "the sandbox was deleted");
  }

  async exec(argv: string[], o: ExecOpts = {}): Promise<Proc> {
    const pipe = o.stdin === "pipe";
    const given = o.stdin && o.stdin !== "pipe" ? o.stdin : null;
    if (o.signal?.aborted) throw new DOMException("aborted", "AbortError");
    const f = await this.streams.open({
      exec: {
        argv,
        ...(o.cwd ? { cwd: o.cwd } : {}),
        ...(o.env ? { env: o.env } : {}),
        ...(o.user ? { user: o.user } : {}),
        ...(o.pty ? { pty: { cols: o.pty.cols, rows: o.pty.rows } } : {}),
        stdin: pipe || given !== null,
        stderr: o.stderr !== "ignore" && !o.pty,
      },
    });
    const onAbort = () => f.end(1000, "aborted"); // the agent hangs the process up
    o.signal?.addEventListener("abort", onAbort, { once: true });
    f.exited.finally(() => o.signal?.removeEventListener("abort", onAbort)).catch(() => {});
    const stdin = pipe || given ? f.writable(StreamChannel.stdin) : null;
    if (given && stdin) void given.pipeTo(stdin).catch(() => {});
    return new NodeProc(f, pipe ? stdin : null, o.stderr === "ignore" || Boolean(o.pty));
  }

  // A TCP connection to a guest port, framed with credits. ssh (port 22) is
  // spliced instead (sandbox.ts bridge, NodeStreams.splice), so that it can
  // hibernate.
  connect(port: number): Stream {
    const ready = this.streams.open({ tcp: { port, splice: false } });
    let f: Framed | null = null;
    const opened = ready.then((x) => {
      f = x;
      return x.opened.then((m) => {
        if ("refused" in m) throw new NotListening(m.refused, `not listening in the TCP address ${m.refused}:${port}`);
        return m;
      });
    });
    opened.catch(() => {});
    const { readable, writable } = new TransformStream<Uint8Array, Uint8Array>();
    void opened.then(
      () => f!.inbox[StreamChannel.stdout]!.readable.pipeTo(writable).catch(() => {}),
      () => writable.abort().catch(() => {}),
    );
    const out = new WritableStream<Uint8Array>({
      write: async (chunk) => (await ready).data(StreamChannel.stdin, chunk),
      close: async () => (await ready).control({ eof: 0 }),
    });
    return {
      opened,
      readable,
      writable: out,
      close: async () => {
        (await ready.catch(() => null))?.end(1000, "closed");
      },
    };
  }

  async fetch(port: number, req: Request): Promise<Response> {
    const url = new URL(req.url);
    const f = await this.streams.open({
      http: { port, method: req.method, path: url.pathname + url.search, headers: headerList(req), body: req.body !== null },
    });
    if (req.body) void req.body.pipeTo(f.writable(StreamChannel.stdin)).catch(() => {});
    const m = await (await this.streams.answered(f, false)).opened;
    if ("refused" in m) {
      throw new NotListening(m.refused, `The container is not listening in the TCP address ${m.refused}:${port}`);
    }
    if (!("status" in m)) throw new Error("the node's agent answered a preview with something else");
    const headers = new Headers();
    for (const [k, v] of m.status.headers) {
      try {
        headers.append(k, v);
      } catch {
        // a header the runtime won't carry
      }
    }
    if (m.status.code === 101) return this.upgrade(f, headers);
    const body = NO_BODY.has(m.status.code) || req.method === "HEAD" ? null : f.inbox[StreamChannel.stdout]!.readable;
    if (!body) f.inbox[StreamChannel.stdout]!.readable.cancel().catch(() => {});
    // The stream closes once the body is read; there's no exit for HTTP.
    f.exitCode = 0;
    return new Response(body, { status: m.status.code, headers });
  }

  // A preview's WebSocket: the app's messages relayed one for one, which
  // carry() in sandbox.ts passes to the visitor.
  private upgrade(f: Framed, headers: Headers): Response {
    const pair = new WebSocketPair();
    const [client, server] = [pair[0], pair[1]];
    server.accept();
    server.binaryType = "arraybuffer";
    let ended = false;
    const end = (code: number, reason: string) => {
      if (ended) return;
      ended = true;
      try {
        server.close(code === 1000 || (code >= 3000 && code <= 4999) ? code : 1000, reason.slice(0, 120));
      } catch {}
      f.end(1000, "closed");
    };
    f.onWsMessage = (data) => {
      try {
        server.send(data);
      } catch {
        end(1011, "");
      }
    };
    f.onWsClose = (code, reason) => end(code, reason);
    server.addEventListener("message", (e) => {
      const text = typeof e.data === "string";
      const data = text ? new TextEncoder().encode(e.data as string) : new Uint8Array(e.data as ArrayBuffer);
      f.data(text ? StreamChannel.textIn : StreamChannel.stdin, data).catch(() => end(1011, ""));
    });
    server.addEventListener("close", (e) => {
      f.control({ close: { code: e.code >= 1000 && e.code <= 4999 && ![1005, 1006, 1015].includes(e.code) ? e.code : 1000, reason: (e.reason ?? "").slice(0, 120) } });
      end(1000, "");
    });
    server.addEventListener("error", () => end(1011, ""));
    headers.delete("content-length");
    return new Response(null, { status: 101, webSocket: client, headers });
  }
}

// A process on a node, over its stream.
class NodeProc implements Proc {
  readonly stdout: ReadableStream | null;
  readonly stderr: ReadableStream | null;

  constructor(
    private readonly f: Framed,
    readonly stdin: WritableStream | null,
    noStderr: boolean,
  ) {
    this.stdout = f.inbox[StreamChannel.stdout]!.readable;
    this.stderr = noStderr ? null : f.inbox[StreamChannel.stderr]!.readable;
  }

  get exitCode() {
    return this.f.exited;
  }

  async output() {
    const read = async (s: ReadableStream | null) => (s ? await new Response(s).arrayBuffer() : new ArrayBuffer(0));
    const [stdout, stderr] = await Promise.all([read(this.stdout).catch(() => new ArrayBuffer(0)), read(this.stderr).catch(() => new ArrayBuffer(0))]);
    return { exitCode: await this.f.exited, stdout, stderr };
  }

  kill(signal = 15) {
    this.f.control({ signal });
  }

  resize(cols: number, rows: number) {
    this.f.control({ resize: { cols, rows } });
  }
}

export { StreamGone };
