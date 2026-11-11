// Stream sockets from a node's agent into a Sandbox object (v0.2.2 M3). The
// protocol is in @cloudex/schema/node ("streams"); the agent's side is
// go/internal/node (spares.go, exec.go, httpstream.go).
//
// - Tickets. The agent opens a stream with a ticket that the Node object
//   issued over its control socket, which is signed and nonce-checked at
//   connect and closed on revoke. A ticket is an HMAC (keyed from ADMIN_KEY)
//   over the node, the sandbox, a stream id the Node object chose, and an
//   expiry 60 s ahead. The Worker checks it with no D1 read, the Sandbox object
//   checks it again, and refuses a stream id it has seen before, so a ticket
//   opens one stream once: a replay inside the 60 s gets 401, as does any
//   ticket after it.
// - Spares. Accepted streams are parked here with the Hibernation API (tag
//   n:<stream>, role "spare" in the attachment) until the object needs one,
//   so opening a stream costs one message, not a new WebSocket from the node.
//   The agent replaces each one taken. When none is left, the object asks the
//   agent for more through the Node object and waits.
// - Liveness, from the spikes: the agent sends "cx-hb" every 15 s, answered
//   "cx-hb-ok" by auto-response without waking this object. A node half whose
//   last heartbeat is over 45 s old is dead, and so is its client half. That's
//   checked on every stream event and from the sandbox's 60 s alarm (reap),
//   with unsendable close codes mapped first.
// - Two kinds of open stream:
//   - framed (role "proc"): an exec, an HTTP exchange or a TCP connection,
//     held in memory as a Framed while it runs, with credit windows on every
//     channel. It can't outlive this object: after a restart its socket is
//     closed, and the agent hangs the process up.
//   - a splice (role "splice", ssh on port 22): the client's WebSocket (tag
//     c:<stream>) and the node's are relayed byte for byte from
//     webSocketMessage, so an idle session lets the object hibernate. SSH's own
//     windows bound it, so it has no credits.
import {
  STREAM_WINDOW,
  StreamChannel,
  StreamClose,
  StreamFromAgent,
  type StreamOpen,
  type StreamToAgent,
} from "@cloudex/schema/node";

export const TICKET_TTL_SEC = 60;
export const HB_DEAD_MS = 45_000;
// A splice's client half that has sent nothing for this long is gone (cloudex
// ssh sends a keepalive every 30 s).
export const CLIENT_IDLE_MS = 10 * 60_000;
// The most spares one sandbox keeps; more are refused.
export const SPARES_MAX = 16;
const SPARE_WAIT_MS = 15_000;
const OPEN_ANSWER_MS = 30_000;

// ---------------------------------------------------------------- tickets

const enc = new TextEncoder();
const b64url = (b: Uint8Array) => btoa(String.fromCharCode(...b)).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
const unb64url = (s: string) => Uint8Array.from(atob(s.replace(/-/g, "+").replace(/_/g, "/")), (c) => c.charCodeAt(0));

const keys = new Map<string, Promise<CryptoKey>>();
// A key of its own for tickets, derived from the admin key.
function ticketKey(adminKey: string): Promise<CryptoKey> {
  let k = keys.get(adminKey);
  if (!k) {
    k = (async () => {
      const root = await crypto.subtle.importKey("raw", enc.encode(adminKey), { name: "HMAC", hash: "SHA-256" }, false, ["sign"]);
      const derived = await crypto.subtle.sign("HMAC", root, enc.encode("cloudex node stream tickets v1"));
      return crypto.subtle.importKey("raw", derived, { name: "HMAC", hash: "SHA-256" }, false, ["sign", "verify"]);
    })();
    keys.set(adminKey, k);
  }
  return k;
}

export type Ticket = { node: string; sandbox: string; stream: string; exp: number };

const STREAM_ID = /^[0-9a-f]{16}$/;

export async function issueTicket(adminKey: string, node: string, sandbox: string, nowMs = Date.now()): Promise<string> {
  const stream = [...crypto.getRandomValues(new Uint8Array(8))].map((b) => b.toString(16).padStart(2, "0")).join("");
  const payload = b64url(enc.encode(JSON.stringify({ n: node, s: sandbox, i: stream, e: Math.floor(nowMs / 1000) + TICKET_TTL_SEC })));
  const mac = new Uint8Array(await crypto.subtle.sign("HMAC", await ticketKey(adminKey), enc.encode(payload)));
  return `${payload}.${b64url(mac)}`;
}

// The ticket's claims, if its MAC checks out and it hasn't expired; null otherwise.
export async function readTicket(adminKey: string, ticket: string, nowMs = Date.now()): Promise<Ticket | null> {
  if (ticket.length > 512) return null;
  const [payload, mac, extra] = ticket.split(".");
  if (!payload || !mac || extra !== undefined) return null;
  try {
    const ok = await crypto.subtle.verify("HMAC", await ticketKey(adminKey), unb64url(mac), enc.encode(payload));
    if (!ok) return null;
    const p = JSON.parse(new TextDecoder().decode(unb64url(payload))) as { n?: unknown; s?: unknown; i?: unknown; e?: unknown };
    if (typeof p.n !== "string" || typeof p.s !== "string" || typeof p.i !== "string" || typeof p.e !== "number") return null;
    if (!STREAM_ID.test(p.i) || p.e * 1000 <= nowMs) return null;
    return { node: p.n, sandbox: p.s, stream: p.i, exp: p.e };
  } catch {
    return null;
  }
}

// ---------------------------------------------------------------- framing

// Close codes 1005, 1006 and 1015 can't be sent, and close() takes only 1000
// and 3000-4999 (spikes: a peer that drops arrives as 1006).
export const sendable = (code: number) => (code === 1000 || (code >= 3000 && code <= 4999) ? code : code === 1006 ? 4404 : 1000);

export function closeQuietly(ws: WebSocket | undefined, code: number, reason: string) {
  try {
    ws?.close(sendable(code), reason.slice(0, 120));
  } catch {
    // closed already
  }
}

function frame(ch: number, data: Uint8Array): Uint8Array {
  const f = new Uint8Array(data.byteLength + 1);
  f[0] = ch;
  f.set(data, 1);
  return f;
}

const bytes = (msg: string | ArrayBuffer | Uint8Array) =>
  typeof msg === "string" ? enc.encode(msg) : msg instanceof Uint8Array ? msg : new Uint8Array(msg);

// A channel's data from the agent, as a stream whose reader pulls it: credit
// goes back to the agent as data is read, so a slow reader slows the source.
class Inbox {
  readonly readable: ReadableStream<Uint8Array>;
  private queue: Uint8Array[] = [];
  private ended = false;
  private failed: Error | null = null;
  private wake: (() => void) | null = null;
  private owed = 0;
  cancelled = false;

  private readonly credit: (n: number) => void;
  private readonly settled: () => void;

  constructor(credit: (n: number) => void, settled: () => void) {
    this.credit = credit;
    this.settled = settled;
    this.readable = new ReadableStream<Uint8Array>(
      {
        pull: async (c) => {
          while (!this.queue.length && !this.ended && !this.failed) await new Promise<void>((r) => (this.wake = r));
          const next = this.queue.shift();
          if (next) {
            c.enqueue(next);
            this.owed += next.byteLength;
            if (this.owed >= STREAM_WINDOW / 4) {
              this.credit(this.owed);
              this.owed = 0;
            }
            return;
          }
          if (this.failed) c.error(this.failed);
          else c.close();
          this.settled();
        },
        cancel: () => {
          this.cancelled = true;
          this.queue = [];
          this.settled();
        },
      },
      { highWaterMark: 0 },
    );
  }

  get done() {
    return this.cancelled || ((this.ended || this.failed !== null) && this.queue.length === 0);
  }

  push(b: Uint8Array) {
    if (this.cancelled) return;
    this.queue.push(b);
    this.poke();
  }
  end() {
    this.ended = true;
    this.poke();
  }
  fail(e: Error) {
    if (!this.ended) this.failed = e;
    this.poke();
  }
  private poke() {
    const w = this.wake;
    this.wake = null;
    w?.();
  }
}

// What may still be sent on a channel, as the agent grants it.
class Outbox {
  private avail = STREAM_WINDOW;
  private waiting: { want: number; resolve: (n: number) => void; reject: (e: Error) => void }[] = [];
  private dead: Error | null = null;

  take(want: number): Promise<number> {
    if (this.dead) return Promise.reject(this.dead);
    if (this.avail > 0 && !this.waiting.length) {
      const n = Math.min(want, this.avail);
      this.avail -= n;
      return Promise.resolve(n);
    }
    return new Promise((resolve, reject) => this.waiting.push({ want, resolve, reject }));
  }
  grant(n: number) {
    this.avail += n;
    while (this.avail > 0 && this.waiting.length) {
      const w = this.waiting.shift()!;
      const k = Math.min(w.want, this.avail);
      this.avail -= k;
      w.resolve(k);
    }
  }
  kill(e: Error) {
    this.dead = e;
    for (const w of this.waiting.splice(0)) w.reject(e);
  }
}

export class StreamGone extends Error {}

// One open, framed stream (an exec, an HTTP exchange or a TCP connection).
export class Framed {
  readonly inbox: Inbox[] = [];
  private outbox: Outbox[] = [];
  private answer: { resolve: (m: StreamFromAgent) => void; reject: (e: Error) => void } | null = null;
  private early: StreamFromAgent | null = null;
  private exit: { resolve: (n: number) => void; reject: (e: Error) => void; promise: Promise<number> };
  private closed = false;
  // The agent's reason, when it couldn't start what was opened.
  private failure: string | null = null;
  // A WebSocket relay of a preview, which gets messages, not bytes.
  onWsMessage: ((data: string | ArrayBuffer) => void) | null = null;
  onWsClose: ((code: number, reason: string) => void) | null = null;
  readonly opened: Promise<StreamFromAgent>;
  exitCode: number | null = null;

  readonly id: string;
  private readonly ws: WebSocket;
  private readonly forget: () => void;

  constructor(id: string, ws: WebSocket, forget: () => void) {
    this.id = id;
    this.ws = ws;
    this.forget = forget;
    for (let ch = 0; ch <= 4; ch++) {
      this.inbox.push(new Inbox((n) => this.control({ credit: { ch, n } }), () => queueMicrotask(() => this.maybeDone())));
      this.outbox.push(new Outbox());
    }
    let resolve!: (n: number) => void;
    let reject!: (e: Error) => void;
    const promise = new Promise<number>((r, j) => ((resolve = r), (reject = j)));
    promise.catch(() => {});
    this.exit = { resolve, reject, promise };
    this.opened = new Promise<StreamFromAgent>((resolve, reject) => {
      if (this.early) resolve(this.early);
      else this.answer = { resolve, reject };
    });
    this.opened.catch(() => {});
  }

  get exited() {
    return this.exit.promise;
  }

  control(m: StreamToAgent) {
    if (this.closed) return;
    try {
      this.ws.send(JSON.stringify(m));
    } catch {
      this.gone(1006, "the node's stream broke");
    }
  }

  // Sends data on a channel within the agent's credit.
  async data(ch: number, chunk: Uint8Array) {
    let at = 0;
    while (at < chunk.byteLength) {
      const n = await this.outbox[ch]!.take(Math.min(chunk.byteLength - at, 256 * 1024));
      if (this.closed) throw new StreamGone("the node's stream closed");
      this.ws.send(frame(ch, chunk.subarray(at, at + n)));
      at += n;
    }
  }

  // A writable that sends on a channel, and eof when it's closed.
  writable(ch: number = StreamChannel.stdin): WritableStream<Uint8Array> {
    return new WritableStream<Uint8Array>({
      write: (chunk) => this.data(ch, bytes(chunk)),
      close: () => this.control({ eof: 0 }),
      abort: () => this.control({ eof: 0 }),
    });
  }

  onBinary(b: Uint8Array) {
    const ch = b[0]!;
    const data = b.subarray(1);
    if (this.onWsMessage && (ch === StreamChannel.stdout || ch === StreamChannel.textOut)) {
      this.onWsMessage(ch === StreamChannel.textOut ? new TextDecoder().decode(data) : data.slice().buffer);
      // A relayed message is passed on at once; its credit goes back with it.
      this.control({ credit: { ch, n: Math.max(1, data.byteLength) } });
      return;
    }
    this.inbox[ch]?.push(data.slice());
  }

  onText(text: string) {
    let m: StreamFromAgent;
    try {
      m = StreamFromAgent.parse(JSON.parse(text));
    } catch {
      return this.end(StreamClose.badMessage, "a control message outside the protocol");
    }
    if ("credit" in m) return this.outbox[m.credit.ch]?.grant(m.credit.n);
    if ("eof" in m) return this.inbox[m.eof]?.end();
    if ("exit" in m) {
      this.exitCode = m.exit;
      this.exit.resolve(m.exit);
      return this.maybeDone();
    }
    if ("close" in m) return this.onWsClose?.(m.close.code, m.close.reason);
    // ok, refused, error or status: the answer to the open.
    if (this.answer) {
      this.answer.resolve(m);
      this.answer = null;
    } else this.early ??= m;
    if ("error" in m) this.failure = m.error;
    if ("refused" in m || "error" in m) this.end(1000, "done");
  }

  // Once the process has exited and both output channels have ended (or their
  // readers have cancelled), the stream closes. One whose output something it
  // started holds open closes when its reader stops reading (exec's grace
  // period), or when the agent closes it. Output nobody reads never holds it:
  // the agent closes the stream when the output ends.
  maybeDone() {
    if (this.exitCode === null) return;
    if ([StreamChannel.stdout, StreamChannel.stderr].every((ch) => this.inbox[ch]!.done)) this.end(1000, "done");
  }

  end(code: number, reason: string) {
    if (this.closed) return;
    closeQuietly(this.ws, code, reason);
    this.gone(code, reason);
  }

  // The socket went (or was closed): everything still waiting fails.
  gone(code: number, reason: string) {
    if (this.closed) return;
    this.closed = true;
    this.forget();
    const e = new StreamGone(`the node's stream closed (${code}${reason ? ` ${reason}` : ""})`);
    for (const i of this.inbox) i.fail(e);
    for (const o of this.outbox) o.kill(e);
    this.answer?.reject(e);
    this.answer = null;
    this.exit.reject(
      this.failure !== null
        ? new Error(`[502] the node couldn't start it: ${this.failure}`)
        : new StreamGone(`the node's stream closed before the command exited (${code}${reason ? ` ${reason}` : ""})`),
    );
    this.onWsClose?.(code === 1000 ? 1000 : 1011, reason);
  }
}

// ---------------------------------------------------------------- the object's side

type Role = "spare" | "proc" | "splice";
type NodeAttachment = { role: Role; at: number };
type ClientAttachment = { tokenId: string | null; lastAt: number };

export type StreamHost = {
  ctx: DurableObjectState;
  env: Env;
  sandbox: () => string | null;
  node: () => string | null;
  // Asks the node's agent for count more spares; rejects when it's offline.
  more: (count: number) => Promise<void>;
  touch: () => void;
};

export class NodeStreams {
  private framed = new Map<string, Framed>();
  private early = new Map<string, ArrayBuffer[]>();
  private spliceAnswer = new Map<string, (m: StreamFromAgent | Error) => void>();
  private waiters: { resolve: (ws: WebSocket) => void; reject: (e: Error) => void; timer: ReturnType<typeof setTimeout> }[] = [];
  private asked = 0;
  private seenReady = false;

  private readonly h: StreamHost;

  constructor(h: StreamHost) {
    this.h = h;
  }

  private get ctx() {
    return this.h.ctx;
  }

  private seen() {
    if (this.seenReady) return;
    this.ctx.storage.sql.exec("CREATE TABLE IF NOT EXISTS node_streams (id TEXT PRIMARY KEY, exp INTEGER NOT NULL)");
    this.seenReady = true;
  }

  // The agent's stream socket, passed on by the Worker (index.ts) with its
  // ticket, which is checked again here.
  async accept(req: Request): Promise<Response> {
    if (req.headers.get("upgrade")?.toLowerCase() !== "websocket") return new Response("expected a WebSocket upgrade\n", { status: 426 });
    const t = await readTicket(this.h.env.ADMIN_KEY, req.headers.get("x-cx-ticket") ?? "");
    const node = this.h.node();
    if (!t || t.sandbox !== this.h.sandbox() || !node || t.node !== node) {
      return new Response("this stream's ticket doesn't open a stream to this sandbox\n", { status: 403 });
    }
    if ((await this.ctx.storage.get<string>("revokedNode")) === t.node) return new Response("this node was removed\n", { status: 403 });
    this.seen();
    const now = Date.now();
    const sql = this.ctx.storage.sql;
    sql.exec("DELETE FROM node_streams WHERE exp < ?", Math.floor(now / 1000));
    if (sql.exec("INSERT OR IGNORE INTO node_streams (id, exp) VALUES (?, ?)", t.stream, t.exp).rowsWritten === 0) {
      return new Response("that ticket was used already\n", { status: 401 });
    }
    this.reap();
    const spares = this.spares();
    if (spares.length >= SPARES_MAX) return new Response("this sandbox has enough spare streams\n", { status: 429 });
    const pair = new WebSocketPair();
    const server = pair[1];
    this.ctx.acceptWebSocket(server, [`n:${t.stream}`]);
    server.serializeAttachment({ role: "spare", at: now } satisfies NodeAttachment);
    const w = this.waiters.shift();
    if (w) {
      clearTimeout(w.timer);
      w.resolve(server);
    }
    return new Response(null, { status: 101, webSocket: pair[0] });
  }

  private attachment(ws: WebSocket): NodeAttachment {
    return (ws.deserializeAttachment() ?? { role: "spare", at: 0 }) as NodeAttachment;
  }

  private alive(ws: WebSocket, now = Date.now()) {
    if (ws.readyState !== WebSocket.OPEN) return false;
    const hb = this.ctx.getWebSocketAutoResponseTimestamp(ws)?.getTime() ?? 0;
    return now - Math.max(hb, this.attachment(ws).at) < HB_DEAD_MS;
  }

  private spares() {
    return this.ctx.getWebSockets().filter((ws) => this.ctx.getTags(ws)[0]?.startsWith("n:") && this.attachment(ws).role === "spare");
  }

  // A live spare, the most recently heard from first, or one the agent opens
  // within SPARE_WAIT_MS.
  private async take(role: Role): Promise<{ ws: WebSocket; id: string }> {
    const now = Date.now();
    const live = this.spares()
      .filter((ws) => this.alive(ws, now))
      .sort((a, b) => (this.ctx.getWebSocketAutoResponseTimestamp(b)?.getTime() ?? 0) - (this.ctx.getWebSocketAutoResponseTimestamp(a)?.getTime() ?? 0));
    let ws = live[0];
    if (live.length <= 1) this.askMore(2);
    if (!ws) {
      ws = await new Promise<WebSocket>((resolve, reject) => {
        const timer = setTimeout(() => {
          this.waiters = this.waiters.filter((w) => w.timer !== timer);
          reject(new Error(`[503] the node didn't open a stream to this sandbox within ${SPARE_WAIT_MS / 1000} s`));
        }, SPARE_WAIT_MS);
        this.waiters.push({ resolve, reject, timer });
      });
    }
    ws.serializeAttachment({ role, at: Date.now() } satisfies NodeAttachment);
    return { ws, id: this.ctx.getTags(ws)[0]!.slice(2) };
  }

  private askMore(n: number) {
    // At most once a second; the agent also keeps more on its own when they go fast.
    if (Date.now() - this.asked < 1000) return;
    this.asked = Date.now();
    this.h.more(n).catch((e: unknown) => {
      const why = e instanceof Error ? e.message : String(e);
      for (const w of this.waiters.splice(0)) {
        clearTimeout(w.timer);
        w.reject(new Error(why.startsWith("[") ? why : `[503] ${why}`));
      }
    });
  }

  // Opens a framed stream: an exec, an HTTP exchange or a TCP connection.
  // It waits for the agent's answer only for TCP. An HTTP answer comes after
  // the request's body, which the caller sends first; an exec's stdin and
  // output can flow at once, which saves a round trip to the node on every
  // exec, and a failure to start comes as its exit code's rejection.
  async open(o: StreamOpen): Promise<Framed> {
    this.reap();
    const { ws, id } = await this.take("proc");
    const f = new Framed(id, ws, () => this.framed.delete(id));
    this.framed.set(id, f);
    f.control(o);
    if ("http" in o || "exec" in o) return f;
    return this.answered(f);
  }

  // The agent's answer to an open, within OPEN_ANSWER_MS; with no deadline
  // for HTTP, whose answer may come after a long body or a long poll, as it
  // may from a container (a dead node ends it through the heartbeat).
  async answered(f: Framed, deadline = true): Promise<Framed> {
    let timer: ReturnType<typeof setTimeout> | undefined;
    const late = new Promise<never>((_, reject) => {
      if (deadline) timer = setTimeout(() => reject(new Error(`[504] the node's agent didn't answer within ${OPEN_ANSWER_MS / 1000} s`)), OPEN_ANSWER_MS);
    });
    try {
      const m = await Promise.race([f.opened, late]);
      if ("error" in m) throw new Error(`[502] the node couldn't start it: ${m.error}`);
      return f;
    } catch (e) {
      f.end(1000, "failed");
      throw e;
    } finally {
      if (timer !== undefined) clearTimeout(timer);
    }
  }

  // ssh: the client's WebSocket spliced to a TCP connection on the node, byte
  // for byte, so the object can hibernate while the session is idle. Answers
  // 502 when nothing listens.
  async splice(port: number, tokenId: string | null): Promise<Response> {
    this.reap();
    const { ws, id } = await this.take("splice");
    this.early.set(id, []);
    const answer = new Promise<StreamFromAgent | Error>((resolve) => this.spliceAnswer.set(id, resolve));
    try {
      ws.send(JSON.stringify({ tcp: { port, splice: true } } satisfies StreamToAgent));
    } catch {
      this.spliceAnswer.set(id, () => {});
    }
    const timer = setTimeout(() => this.spliceAnswer.get(id)?.(new Error("the node's agent didn't answer within 15 s")), 15_000);
    const m = await answer;
    clearTimeout(timer);
    this.spliceAnswer.delete(id);
    if (m instanceof Error || !("ok" in m)) {
      this.early.delete(id);
      closeQuietly(ws, 1000, "not opened");
      const why = m instanceof Error ? m.message : "refused" in m ? "nothing is listening" : "error" in m ? m.error : "no answer";
      return new Response(`nothing is listening on port ${port}: ${why}\n`, { status: 502 });
    }
    const pair = new WebSocketPair();
    const client = pair[1];
    this.ctx.acceptWebSocket(client, [`c:${id}`]);
    client.serializeAttachment({ tokenId, lastAt: Date.now() } satisfies ClientAttachment);
    for (const b of this.early.get(id) ?? []) client.send(b);
    this.early.delete(id);
    this.h.touch();
    return new Response(null, { status: 101, webSocket: pair[0] });
  }

  // The Hibernation API's handlers (the Sandbox class passes them on).
  handles(ws: WebSocket) {
    const tag = this.ctx.getTags(ws)[0] ?? "";
    return tag.startsWith("n:") || tag.startsWith("c:");
  }

  onMessage(ws: WebSocket, msg: string | ArrayBuffer) {
    const tag = this.ctx.getTags(ws)[0] ?? "";
    const id = tag.slice(2);
    if (tag.startsWith("c:")) {
      this.h.touch();
      const a = ws.deserializeAttachment() as ClientAttachment | null;
      if (a && Date.now() - a.lastAt > 30_000) ws.serializeAttachment({ ...a, lastAt: Date.now() } satisfies ClientAttachment);
      const peer = this.ctx.getWebSockets(`n:${id}`)[0];
      if (!peer || !this.alive(peer)) return this.closeBoth(id, StreamClose.lost, "the node's side is gone");
      try {
        peer.send(bytes(msg));
      } catch {
        this.closeBoth(id, StreamClose.lost, "the node's side is gone");
      }
      return;
    }
    const role = this.attachment(ws).role;
    if (role === "proc") {
      const f = this.framed.get(id);
      if (!f) return closeQuietly(ws, StreamClose.lost, "the sandbox object restarted");
      if (typeof msg === "string") f.onText(msg);
      else f.onBinary(new Uint8Array(msg));
      return;
    }
    if (role === "splice") {
      const waiting = this.spliceAnswer.get(id);
      if (typeof msg === "string") {
        let m: StreamFromAgent | null = null;
        try {
          m = StreamFromAgent.parse(JSON.parse(msg));
        } catch {}
        if (waiting && m) waiting(m);
        else if (!m) closeQuietly(ws, StreamClose.badMessage, "a control message outside the protocol");
        return;
      }
      const early = this.early.get(id);
      if (early) return void early.push(msg);
      const peer = this.ctx.getWebSockets(`c:${id}`)[0];
      if (!peer) return this.closeBoth(id, StreamClose.lost, "the client's side is gone");
      try {
        peer.send(msg);
      } catch {
        this.closeBoth(id, StreamClose.lost, "the client's side is gone");
      }
      return;
    }
    // A spare speaks only in heartbeats, which the runtime answers.
    closeQuietly(ws, StreamClose.badMessage, "a spare stream sent something before it was opened");
  }

  onClose(ws: WebSocket, code: number, reason: string) {
    const tag = this.ctx.getTags(ws)[0] ?? "";
    const id = tag.slice(2);
    closeQuietly(ws, code, reason);
    if (tag.startsWith("c:")) return this.closeBoth(id, code, reason || "the client closed");
    const role = this.attachment(ws).role;
    if (role === "proc") this.framed.get(id)?.gone(code, reason);
    if (role === "splice") {
      this.spliceAnswer.get(id)?.(new Error(`the node's stream closed (${code})`));
      this.closeBoth(id, code, reason || "the node closed");
    }
  }

  private closeBoth(id: string, code: number, reason: string) {
    for (const ws of [...this.ctx.getWebSockets(`c:${id}`), ...this.ctx.getWebSockets(`n:${id}`)]) closeQuietly(ws, code, reason);
    this.framed.get(id)?.gone(code, reason);
    this.early.delete(id);
  }

  // Closes what's dead: node halves with no heartbeat for 45 s (and their
  // clients), client halves with no node half or silent for CLIENT_IDLE_MS,
  // and open streams this instance no longer holds (after a restart).
  // Returns how many streams it closed.
  reap(now = Date.now()): number {
    let n = 0;
    for (const ws of this.ctx.getWebSockets()) {
      const tag = this.ctx.getTags(ws)[0] ?? "";
      const id = tag.slice(2);
      if (tag.startsWith("n:")) {
        if (ws.readyState !== WebSocket.OPEN) continue;
        const role = this.attachment(ws).role;
        if (!this.alive(ws, now)) {
          this.closeBoth(id, StreamClose.heartbeat, "no heartbeat for 45 s");
          n++;
        } else if (role === "proc" && !this.framed.has(id)) {
          closeQuietly(ws, StreamClose.lost, "the sandbox object restarted");
          n++;
        } else if (role === "splice" && !this.early.has(id) && !this.spliceAnswer.has(id) && !this.ctx.getWebSockets(`c:${id}`).some((c) => c.readyState === WebSocket.OPEN)) {
          closeQuietly(ws, StreamClose.lost, "the client's side is gone");
          n++;
        }
      } else if (tag.startsWith("c:")) {
        if (ws.readyState !== WebSocket.OPEN) continue;
        const peer = this.ctx.getWebSockets(`n:${id}`)[0];
        const a = ws.deserializeAttachment() as ClientAttachment | null;
        if (!peer || peer.readyState !== WebSocket.OPEN || (a && now - a.lastAt > CLIENT_IDLE_MS)) {
          this.closeBoth(id, StreamClose.lost, peer ? "the client was silent for 10 minutes" : "the node's side is gone");
          n++;
        }
      }
    }
    return n;
  }

  // The token ids that opened spliced client sockets, for the alarm's check
  // that they still work (sandbox.ts watchCredential does it in memory for
  // the sockets that can't hibernate).
  clients(): { ws: WebSocket; tokenId: string; id: string }[] {
    const out = [];
    for (const ws of this.ctx.getWebSockets()) {
      const tag = this.ctx.getTags(ws)[0] ?? "";
      const a = tag.startsWith("c:") ? (ws.deserializeAttachment() as ClientAttachment | null) : null;
      if (a?.tokenId) out.push({ ws, tokenId: a.tokenId, id: tag.slice(2) });
    }
    return out;
  }

  endClient(id: string, code: number, reason: string) {
    this.closeBoth(id, code, reason);
  }

  // Every stream socket: the sandbox is deleted, or its node was removed.
  closeAll(code: number, reason: string) {
    for (const ws of this.ctx.getWebSockets()) if (this.handles(ws)) closeQuietly(ws, code, reason);
    for (const f of [...this.framed.values()]) f.gone(code, reason);
    for (const w of this.waiters.splice(0)) {
      clearTimeout(w.timer);
      w.reject(new Error(`[503] ${reason}`));
    }
  }

  count(): { spares: number; open: number } {
    let spares = 0;
    let open = 0;
    for (const ws of this.ctx.getWebSockets()) {
      const tag = this.ctx.getTags(ws)[0] ?? "";
      if (!tag.startsWith("n:") || ws.readyState !== WebSocket.OPEN) continue;
      if (this.attachment(ws).role === "spare") spares++;
      else open++;
    }
    return { spares, open };
  }
}
