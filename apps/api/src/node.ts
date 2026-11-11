// One Durable Object per node (nodes.ts), named by the node's id. It holds the
// control WebSocket its agent keeps open (go/internal/node), and is the only
// way the deployment reaches the node: there's no inbound port on the host.
//
// - The Worker checks the agent's signature before anything reaches here
//   (index.ts, nodeauth.ts). This object refuses a reused nonce and a node
//   that has been revoked, and then accepts the socket with the Hibernation
//   API, so an idle node costs nothing: the agent's "ping" every 10 s is
//   answered "pong" by the runtime without waking the object.
// - A new connection replaces the old ones (close code 4000). Which socket is
//   the node's, and whether it's online, follow nodestate.ts.
// - Messages are JSON of the protocol in @cloudex/schema/node, at most 64 KiB.
//   Anything else closes the socket (4400). hello and facts are kept here and
//   copied to D1, which lists nodes.
// - op() sends a request to the agent and waits for its answer. Ops are short
//   (ping, facts, and M3's adopt, release and streams); a restart of this
//   object loses its waiters, and their callers get an error. Nothing long
//   runs as an op: an exec has a stream socket of its own (nodestream.ts),
//   whose loss the agent sees and acts on.
// - Stream tickets (M3): the agent asks for them on the control socket, and
//   this object, which knows the socket is the node's (signed and nonce-checked
//   at connect, closed on revoke), issues them. Each opens one stream to one
//   sandbox, once, within 60 s (nodestream.ts).
// - revoke() refuses the node from now on and closes its socket (4003).
// - A node may connect at most CONNECTS_PER_MINUTE times a minute; more get
//   429, so a broken or hostile agent can't keep this object and D1 busy.
//
// Later milestones add reconciling machines on hello (M5) and the tombstones of
// machines deleted while the node was offline.
import { DurableObject } from "cloudflare:workers";
import {
  NODE_MESSAGE_MAX,
  NODE_PROTOCOL,
  NodeClose,
  type NodeFacts,
  NodeMessage,
  type NodeMachine,
  type NodeOpName,
  type WorkerMessage,
} from "@cloudex/schema/node";
import { BAD_SIGNATURE, rememberNonce } from "./nodeauth";
import { issueTicket } from "./nodestream";
import { mirrorNode } from "./nodes";
import { type ControlSocket, lastSign, newest, pick } from "./nodestate";
import { fail } from "./util";

// Headers the Worker sets on the connect it passes here, after the signature
// checked out; any the caller sent are overwritten.
export const CONNECT = {
  node: "x-cx-node",
  nonce: "x-cx-nonce",
  colo: "x-cx-internal-colo",
  continent: "x-cx-internal-continent",
  hint: "x-cx-internal-hint",
} as const;

type Attachment = { gen: number; acceptedAt: number; lastMessageAt: number | null; hello?: boolean };

export type Hello = { version: string; protocol: number; facts: NodeFacts; machines: NodeMachine[]; at: number };

export type NodeStatus = { online: boolean; lastSeenAt: number | null; connectedAt: number | null; colo: string | null; hello: Hello | null };

// Close codes 1005, 1006 and 1015 can't be sent (spike: a peer that drops
// arrives as 1006, and passing it on to close() throws).
const sendable = (code: number) => (code === 1005 || code === 1006 || code === 1015 ? 1000 : code);

export const CONNECTS_PER_MINUTE = 12;

type Waiter = { resolve: (v: Record<string, unknown>) => void; reject: (e: Error) => void; timer: ReturnType<typeof setTimeout> };

export class Node extends DurableObject<Env> {
  private waiters = new Map<string, Waiter>();

  constructor(ctx: DurableObjectState, env: Env) {
    super(ctx, env);
    ctx.setWebSocketAutoResponse(new WebSocketRequestResponsePair("ping", "pong"));
    ctx.storage.sql.exec("CREATE TABLE IF NOT EXISTS nonces (nonce TEXT PRIMARY KEY, at INTEGER NOT NULL)");
  }

  private sockets(): ControlSocket<WebSocket>[] {
    return this.ctx.getWebSockets("control").map((ws) => {
      const a = (ws.deserializeAttachment() ?? { gen: 0, acceptedAt: 0, lastMessageAt: null }) as Attachment;
      return {
        ws,
        gen: a.gen,
        open: ws.readyState === WebSocket.OPEN,
        acceptedAt: a.acceptedAt,
        lastMessageAt: a.lastMessageAt,
        lastPingAt: this.ctx.getWebSocketAutoResponseTimestamp(ws)?.getTime() ?? null,
        hello: a.hello === true,
      };
    });
  }

  // The socket to use, closing the ones that are left over or gone quiet.
  private current(now = Date.now()) {
    const all = this.sockets();
    const best = newest(all);
    const { use, online, close } = pick(all, now);
    let alive: ControlSocket<WebSocket> | null = best;
    for (const s of close) {
      const stale = s === best;
      if (stale) alive = null;
      this.close(s.ws, stale ? NodeClose.stale : NodeClose.replaced, stale ? "no ping for 45 s" : "replaced by a newer connection");
      if (stale) void this.wentOffline(lastSign(s));
    }
    return { use, online, alive };
  }

  private close(ws: WebSocket, code: number, reason: string) {
    try {
      ws.close(sendable(code), reason.slice(0, 120));
    } catch {
      // closed already
    }
  }

  private async wentOffline(lastSeenAt: number) {
    const id = await this.ctx.storage.get<string>("id");
    await this.ctx.storage.put("lastSeenAt", lastSeenAt);
    this.rejectAll("[503] the node went offline");
    if (id) await mirrorNode(this.env.DB, id, { state: "offline", lastSeenAt }).catch((e) => console.error(`[${id}] ${e}`));
  }

  // The agent's control socket, passed on by the Worker (index.ts) once its
  // signature checked out.
  async fetch(req: Request): Promise<Response> {
    const h = req.headers;
    const id = h.get(CONNECT.node) ?? "";
    const nonce = h.get(CONNECT.nonce) ?? "";
    if (req.headers.get("upgrade")?.toLowerCase() !== "websocket" || !id || !nonce) return Response.json({ error: "not found" }, { status: 404 });
    const known = await this.ctx.storage.get<string>("id");
    if (known && known !== id) return Response.json({ error: "this node object belongs to another node" }, { status: 500 });
    // After revoke(), whatever D1 said when the Worker looked.
    if (await this.ctx.storage.get<boolean>("revoked")) return Response.json({ error: BAD_SIGNATURE }, { status: 401 });
    const now = Date.now();
    const sql = this.ctx.storage.sql;
    const fresh = rememberNonce(
      {
        insert: (n, at) => sql.exec("INSERT OR IGNORE INTO nonces (nonce, at) VALUES (?, ?)", n, at).rowsWritten > 0,
        prune: (before) => void sql.exec("DELETE FROM nonces WHERE at < ?", before),
      },
      nonce,
      now,
    );
    if (!fresh) return Response.json({ error: "unauthorized: that nonce was used already; each connect needs a new one" }, { status: 401 });
    const recent = ((await this.ctx.storage.get<number[]>("connects")) ?? []).filter((t) => now - t < 60_000);
    if (recent.length >= CONNECTS_PER_MINUTE) {
      const wait = Math.max(1, Math.ceil((recent[0]! + 60_000 - now) / 1000));
      return Response.json(
        { error: `this node connected ${recent.length} times in the last minute; wait ${wait} s` },
        { status: 429, headers: { "retry-after": String(wait) } },
      );
    }
    await this.ctx.storage.put("connects", [...recent, now]);
    if (!known) await this.ctx.storage.put("id", id);

    for (const s of this.sockets()) this.close(s.ws, NodeClose.replaced, "replaced by a newer connection");
    const gen = ((await this.ctx.storage.get<number>("gen")) ?? 0) + 1;
    const { 0: client, 1: server } = new WebSocketPair();
    this.ctx.acceptWebSocket(server, ["control", `gen:${gen}`]);
    server.serializeAttachment({ gen, acceptedAt: now, lastMessageAt: null } satisfies Attachment);
    const where = {
      colo: h.get(CONNECT.colo) || null,
      continent: h.get(CONNECT.continent) || null,
      locationHint: h.get(CONNECT.hint) || null,
    };
    await this.ctx.storage.put({ gen, connectedAt: now, lastSeenAt: now, ...where });
    // Online once it says hello (nodestate.ts).
    await mirrorNode(this.env.DB, id, { lastSeenAt: now, ...where }).catch((e) => console.error(`[${id}] ${e}`));
    return new Response(null, { status: 101, webSocket: client });
  }

  async webSocketMessage(ws: WebSocket, msg: string | ArrayBuffer) {
    const now = Date.now();
    const { alive } = this.current(now);
    if (alive?.ws !== ws) return this.close(ws, NodeClose.replaced, "replaced by a newer connection");
    if (typeof msg !== "string" || msg.length > NODE_MESSAGE_MAX) return this.close(ws, NodeClose.badMessage, "messages are JSON text of at most 64 KiB");
    let parsed;
    try {
      parsed = NodeMessage.safeParse(JSON.parse(msg));
    } catch {
      return this.close(ws, NodeClose.badMessage, "a message that isn't JSON");
    }
    if (!parsed.success) {
      const i = parsed.error.issues[0];
      return this.close(ws, NodeClose.badMessage, `bad message: ${i?.path.join(".")}: ${i?.message}`);
    }
    const a = ws.deserializeAttachment() as Attachment;
    const m = parsed.data;
    if (!a.hello && m.type !== "hello") return this.close(ws, NodeClose.badMessage, "the first message on a connection is hello");
    ws.serializeAttachment({ ...a, lastMessageAt: now, hello: a.hello || m.type === "hello" } satisfies Attachment);
    const id = (await this.ctx.storage.get<string>("id"))!;
    switch (m.type) {
      case "hello": {
        if (m.protocol !== NODE_PROTOCOL) {
          return this.close(ws, NodeClose.badMessage, `this deployment speaks node protocol ${NODE_PROTOCOL}; update the agent`);
        }
        const hello: Hello = { version: m.version, protocol: m.protocol, facts: m.facts, machines: m.machines, at: now };
        await this.ctx.storage.put("hello", hello);
        await mirrorNode(this.env.DB, id, { state: "online", lastSeenAt: now, version: m.version, protocol: m.protocol, facts: m.facts });
        return;
      }
      case "facts": {
        const hello = await this.ctx.storage.get<Hello>("hello");
        if (hello) await this.ctx.storage.put("hello", { ...hello, facts: m.facts });
        await mirrorNode(this.env.DB, id, { lastSeenAt: now, facts: m.facts });
        return;
      }
      case "state":
        // M5 passes it to the machine's Sandbox object, which acts on it only
        // if its record names this node. Nothing is stored for it here, so a
        // node can't fill this object's storage with made-up machines.
        console.log(`[${id}] machine ${m.machine.id} is ${m.machine.state}`);
        return;
      case "tickets": {
        const revoked = await this.ctx.storage.get<boolean>("revoked");
        const tickets = revoked ? [] : await Promise.all(Array.from({ length: m.count }, () => issueTicket(this.env.ADMIN_KEY, id, m.sandbox, now)));
        const answer: WorkerMessage = { type: "tickets", id: m.id, tickets, ...(revoked ? { error: "this node was removed" } : {}) };
        try {
          ws.send(JSON.stringify(answer));
        } catch {}
        return;
      }
      case "result": {
        const w = this.waiters.get(m.id);
        if (!w) return; // answered after its deadline, or to an instance that's gone
        this.waiters.delete(m.id);
        clearTimeout(w.timer);
        if (m.ok) w.resolve(m.value ?? {});
        else w.reject(new Error(`[502] the node's agent answered: ${m.error ?? "an error"}`));
      }
    }
  }

  async webSocketClose(ws: WebSocket, code: number, reason: string) {
    this.close(ws, code, reason);
    await this.closed(ws);
  }

  async webSocketError(ws: WebSocket) {
    this.close(ws, 1011, "error");
    await this.closed(ws);
  }

  // The node's own socket went: it's offline unless a newer one is up.
  private async closed(ws: WebSocket) {
    const a = ws.deserializeAttachment() as Attachment | null;
    if (!a || this.sockets().some((s) => s.ws !== ws && s.open && s.gen > a.gen)) return;
    const ping = this.ctx.getWebSocketAutoResponseTimestamp(ws)?.getTime() ?? null;
    await this.wentOffline(lastSign({ ws, gen: a.gen, open: false, acceptedAt: a.acceptedAt, lastMessageAt: a.lastMessageAt, lastPingAt: ping, hello: a.hello === true }));
  }

  // Whether the node is online now, for listing (index.ts).
  async status(): Promise<NodeStatus> {
    const { use, online } = this.current();
    const s = await this.ctx.storage.get(["lastSeenAt", "connectedAt", "colo", "hello"]);
    return {
      online,
      lastSeenAt: use ? lastSign(use) : ((s.get("lastSeenAt") as number | undefined) ?? null),
      connectedAt: (s.get("connectedAt") as number | undefined) ?? null,
      colo: (s.get("colo") as string | undefined) ?? null,
      hello: (s.get("hello") as Hello | undefined) ?? null,
    };
  }

  // A request to the agent, answered within timeoutMs.
  async op(name: NodeOpName, args: Record<string, unknown> = {}, timeoutMs = 10_000): Promise<Record<string, unknown>> {
    const { use } = this.current();
    if (!use) {
      const last = await this.ctx.storage.get<number>("lastSeenAt");
      fail(503, `the node is offline${last ? ` (last seen ${new Date(last).toISOString()})` : ""}`);
    }
    const id = `op-${crypto.randomUUID().replaceAll("-", "").slice(0, 16)}`;
    const answer = new Promise<Record<string, unknown>>((resolve, reject) => {
      const timer = setTimeout(() => {
        this.waiters.delete(id);
        reject(new Error(`[504] the node's agent didn't answer ${name} within ${Math.round(timeoutMs / 1000)} s`));
      }, timeoutMs);
      this.waiters.set(id, { resolve, reject, timer });
    });
    try {
      use.ws.send(JSON.stringify({ type: "op", id, name, args }));
    } catch (e) {
      const w = this.waiters.get(id);
      if (w) clearTimeout(w.timer);
      this.waiters.delete(id);
      fail(503, `the node's connection broke: ${e instanceof Error ? e.message : e}`);
    }
    return answer;
  }

  // cloudex node rm: the node is refused from now on, and its socket closes.
  async revoke() {
    await this.ctx.storage.put("revoked", true);
    for (const s of this.sockets()) this.close(s.ws, NodeClose.revoked, "this node was removed");
    this.rejectAll("[410] the node was removed");
  }

  private rejectAll(why: string) {
    for (const [, w] of this.waiters) {
      clearTimeout(w.timer);
      w.reject(new Error(why));
    }
    this.waiters.clear();
  }
}
