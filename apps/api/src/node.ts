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
//   (ping, facts, M3's streams, and M4's create, start, stop and delete, which
//   wait at most for a machine's entrypoint, and selftest); a restart of this
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
// - On a socket (2026-11-12 review): hello comes once (a second closes it);
//   facts, state and ticket requests are limited to PER_MINUTE each, more are
//   dropped (tickets refused); facts go to D1 only when they change, at most
//   every MIRROR_MIN_MS; and a state or ticket request for a sandbox that isn't
//   on this node (D1's index, re-read at most every MEMBERS_FRESH_MS on a
//   miss) is dropped without waking that sandbox's object.
//
// - After hello comes welcome (M4): the image the deployment's machines run
//   (NODE_IMAGE), which the agent pulls and checks against its own list; and
//   (M6) the provider hosts its node CA is limited to.
//
// - Sandboxes on the node (M5). On hello it reconciles: each sandbox the
//   index has on the node hears its machine's state (Sandbox.nodeReport),
//   which decides what to do; machines of sandboxes deleted while the node
//   was offline (tombstones, kept here) are removed; machines of no sandbox
//   are left alone and listed (status). Each state message (a machine
//   started or stopped) goes to its sandbox the same way, and the node going
//   offline or coming back is told to every sandbox on it. Nothing here
//   decides what runs: the Sandbox objects do (placement.ts).
// - While a socket is open, an alarm every 3 minutes checks it's still
//   answering, so that a node whose link died without a close shows offline
//   without anything polling this object, which would keep it awake (an
//   object kept busy drops its sockets a few minutes after each deploy).
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
import { REVOKED, rememberNonce } from "./nodeauth";
import { issueTicket } from "./nodestream";
import { sandboxesOnNode } from "./db";
import { PROVIDERS } from "./keys";
import { mirrorNode } from "./nodes";
import { type ControlSocket, lastSign, newest, pick } from "./nodestate";
import { type MachineReport, reconcilePlan } from "./placement";
import { errorStatus, fail } from "./util";

// Headers the Worker sets on the connect it passes here, after the signature
// checked out; any the caller sent are overwritten.
export const CONNECT = {
  node: "x-cx-node",
  nonce: "x-cx-nonce",
  colo: "x-cx-internal-colo",
  continent: "x-cx-internal-continent",
  hint: "x-cx-internal-hint",
  skew: "x-cx-internal-skew", // the agent's clock less the Worker's, in seconds, at connect
} as const;

// rl: this socket's messages in the current minute, by kind (limits below).
type Attachment = { gen: number; acceptedAt: number; lastMessageAt: number | null; hello?: boolean; rl?: Counts };
type Counts = { at: number; facts: number; state: number; tickets: number; dropped: number };

// What one control socket may send in a minute, after its hello (2026-11-12
// review, W1): an honest agent sends facts when they change (an image pull's
// progress every 5 s at most), a state per machine start or stop, and a
// ticket request per spare stream it opens. More is dropped (tickets are
// refused), so a node can't turn its socket into D1 writes, reconciles or
// woken Sandbox objects.
export const PER_MINUTE = { facts: 20, state: 120, tickets: 240 } as const;
// The facts go to D1 when they change, at most this often; a change in
// between goes with the next write or the alarm.
export const MIRROR_MIN_MS = 10_000;
// How long the list of this node's sandboxes (from D1) is trusted before a
// state or ticket for a sandbox not on it reads D1 again.
export const MEMBERS_FRESH_MS = 5_000;

// Counts a message against its socket's limit; false when it's over.
export function allow(c: Counts | undefined, kind: keyof typeof PER_MINUTE, now: number): { ok: boolean; counts: Counts } {
  const counts = c && now - c.at < 60_000 ? { ...c } : { at: now, facts: 0, state: 0, tickets: 0, dropped: 0 };
  if (counts[kind] >= PER_MINUTE[kind]) {
    counts.dropped++;
    return { ok: false, counts };
  }
  counts[kind]++;
  return { ok: true, counts };
}

export type Hello = { version: string; protocol: number; facts: NodeFacts; machines: NodeMachine[]; machinesError?: string; at: number };

export type NodeStatus = {
  online: boolean;
  lastSeenAt: number | null;
  connectedAt: number | null;
  colo: string | null;
  hello: Hello | null;
  // Sandboxes deleted while the node was offline, whose machines it removes when it's back.
  tombstones: string[];
  // How far the agent's clock was from the Worker's at its last connect, in seconds.
  clockSkewSec: number | null;
};

const STALE_CHECK_MS = 3 * 60_000;
// How long ops under way wait for the agent to come back after its socket went.
const OP_GRACE_MS = 10_000;
const TOMB = "tomb:";

// Close codes 1005, 1006 and 1015 can't be sent (spike: a peer that drops
// arrives as 1006, and passing it on to close() throws).
const sendable = (code: number) => (code === 1005 || code === 1006 || code === 1015 ? 1000 : code);

export const CONNECTS_PER_MINUTE = 12;

type Waiter = { resolve: (v: Record<string, unknown>) => void; reject: (e: Error) => void; timer: ReturnType<typeof setTimeout> };

export class Node extends DurableObject<Env> {
  private waiters = new Map<string, Waiter>();
  // The sandboxes on this node, as D1's index had them at loadedAt, plus the
  // ones made or started through op() since; for dropping a node's state or
  // ticket request for a sandbox that isn't its own without waking it.
  private members: { ids: Set<string>; loadedAt: number } | null = null;

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
    // Ops under way wait a little: an agent whose connection dropped (an
    // edge reset, a deploy) is back within a second or two, and answers
    // them on its new connection.
    if (this.waiters.size) {
      setTimeout(() => {
        if (!this.current().use) this.rejectAll(`[503] the node went offline (last seen ${new Date(lastSeenAt).toISOString()})`);
      }, OP_GRACE_MS);
    }
    if (!id) return;
    await mirrorNode(this.env.DB, id, { state: "offline", lastSeenAt }).catch((e) => console.error(`[${id}] ${e}`));
    if (await this.ctx.storage.get<boolean>("revoked")) return;
    this.ctx.waitUntil(this.tellSandboxes(id, () => ({ online: false, lastSeenAt })));
  }

  // Tells each sandbox the index has on this node.
  private async tellSandboxes(id: string, report: (sandbox: string) => { online: boolean; lastSeenAt: number | null; machine?: MachineReport } | null) {
    let ids: string[];
    try {
      ids = await sandboxesOnNode(this.env.DB, id);
    } catch (e) {
      console.error(`[${id}] listing its sandboxes: ${errorStatus(e).message}`);
      return;
    }
    await Promise.all(
      ids.map(async (sb) => {
        const r = report(sb);
        if (!r) return;
        await this.env.SANDBOX.getByName(sb)
          .nodeReport(id, r)
          .catch((e: unknown) => console.error(`[${id}] telling ${sb}: ${errorStatus(e).message}`));
      }),
    );
  }

  // On hello: the node's machines against its sandboxes (placement.ts).
  private async reconcile(id: string, hello: Hello) {
    const tombstones = await this.tombstones();
    let sandboxes: string[];
    try {
      sandboxes = await sandboxesOnNode(this.env.DB, id);
    } catch (e) {
      console.error(`[${id}] reconcile: listing its sandboxes: ${errorStatus(e).message}`);
      return;
    }
    const plan = reconcilePlan({ machines: hello.machines, machinesKnown: !hello.machinesError, sandboxes, tombstones });
    for (const sb of plan.deletes) this.ctx.waitUntil(this.removeTombstoned(sb));
    const reports = new Map(plan.reports.map((r) => [r.id, r.machine]));
    await this.tellSandboxes(id, (sb) => (plan.deletes.includes(sb) ? null : { online: true, lastSeenAt: hello.at, ...(reports.has(sb) ? { machine: reports.get(sb)! } : {}) }));
    if (plan.orphans.length) console.log(`[${id}] machines of no sandbox, left alone: ${plan.orphans.join(" ")}`);
  }

  private async tombstones(): Promise<string[]> {
    return [...(await this.ctx.storage.list<number>({ prefix: TOMB })).keys()].map((k) => k.slice(TOMB.length));
  }

  // A sandbox on this node was deleted while the node was offline (sandbox.ts
  // removeMachine): its machine is removed when the node is back, at hello,
  // or now if it's back already.
  async tombstone(sandbox: string) {
    if (!/^cx-[a-z0-9]{6}$/.test(sandbox)) fail(400, "not a sandbox id");
    await this.ctx.storage.put(`${TOMB}${sandbox}`, Date.now());
    if (this.current().use) this.ctx.waitUntil(this.removeTombstoned(sandbox));
  }

  private async removeTombstoned(sandbox: string) {
    try {
      await this.op("delete", { sandbox }, 90_000);
      await this.ctx.storage.delete(`${TOMB}${sandbox}`);
      console.log(`removed the machine of ${sandbox}, deleted while the node was offline`);
    } catch (e) {
      console.error(`removing the machine of ${sandbox}: ${errorStatus(e).message}; again at the next hello`);
    }
  }

  // Every few minutes while a socket is open: one that stopped answering
  // without a close is closed, and the node shows offline (current()).
  async alarm() {
    this.current();
    if (await this.ctx.storage.get<boolean>("mirrorDue")) {
      const [id, hello] = await Promise.all([this.ctx.storage.get<string>("id"), this.ctx.storage.get<Hello>("hello")]);
      if (id && hello) await this.mirrorFacts(id, hello.facts, Date.now()).catch((e) => console.error(`[${id}] ${e}`));
    }
    if (this.sockets().length) await this.ctx.storage.setAlarm(Date.now() + STALE_CHECK_MS);
  }

  // The facts to D1, when they changed, at most every MIRROR_MIN_MS; a change
  // sooner is written by the alarm.
  private async mirrorFacts(id: string, facts: NodeFacts, now: number) {
    const s = await this.ctx.storage.get(["mirroredFacts", "mirroredAt"]);
    const text = JSON.stringify(facts);
    if (s.get("mirroredFacts") === text) return void (await this.ctx.storage.put("mirrorDue", false));
    const at = (s.get("mirroredAt") as number | undefined) ?? 0;
    if (now - at < MIRROR_MIN_MS) {
      await this.ctx.storage.put("mirrorDue", true);
      const due = at + MIRROR_MIN_MS;
      const alarm = await this.ctx.storage.getAlarm();
      if (alarm === null || alarm > due) await this.ctx.storage.setAlarm(due);
      return;
    }
    await this.ctx.storage.put({ mirroredFacts: text, mirroredAt: now, mirrorDue: false });
    await mirrorNode(this.env.DB, id, { lastSeenAt: now, facts });
  }

  // Whether a sandbox is on this node: from memory, or D1's index when it's
  // not there and the copy is older than MEMBERS_FRESH_MS.
  private async isMember(id: string, sandbox: string, now: number): Promise<boolean> {
    if (this.members?.ids.has(sandbox)) return true;
    if (this.members && now - this.members.loadedAt < MEMBERS_FRESH_MS) return false;
    try {
      this.members = { ids: new Set(await sandboxesOnNode(this.env.DB, id)), loadedAt: now };
    } catch (e) {
      console.error(`[${id}] listing its sandboxes: ${errorStatus(e).message}`);
      return false;
    }
    return this.members.ids.has(sandbox);
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
    // After revoke(), whatever D1 said when the Worker looked; the Worker
    // checked the signature with the node's own key, so it may hear why.
    if (await this.ctx.storage.get<boolean>("revoked")) return Response.json({ error: REVOKED, revoked: true }, { status: 410 });
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
    const skew = Number(h.get(CONNECT.skew));
    await this.ctx.storage.put({ gen, connectedAt: now, lastSeenAt: now, ...where, ...(Number.isFinite(skew) && h.get(CONNECT.skew) ? { clockSkewSec: Math.round(skew) } : {}) });
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
    if (a.hello && m.type === "hello") return this.close(ws, NodeClose.badMessage, "hello comes once on a connection");
    let rl = a.rl;
    if (m.type === "facts" || m.type === "state" || m.type === "tickets") {
      const r = allow(a.rl, m.type, now);
      rl = r.counts;
      if (!r.ok) {
        ws.serializeAttachment({ ...a, lastMessageAt: now, rl } satisfies Attachment);
        if (r.counts.dropped === 1) console.log(`[${await this.ctx.storage.get<string>("id")}] over ${PER_MINUTE[m.type]} ${m.type} messages a minute; dropping more`);
        if (m.type === "tickets") {
          try {
            ws.send(JSON.stringify({ type: "tickets", id: m.id, tickets: [], error: "too many ticket requests; wait a minute" } satisfies WorkerMessage));
          } catch {}
        }
        return;
      }
    }
    ws.serializeAttachment({ ...a, lastMessageAt: now, hello: a.hello || m.type === "hello", ...(rl ? { rl } : {}) } satisfies Attachment);
    const id = (await this.ctx.storage.get<string>("id"))!;
    switch (m.type) {
      case "hello": {
        if (m.protocol !== NODE_PROTOCOL) {
          return this.close(ws, NodeClose.badMessage, `this deployment speaks node protocol ${NODE_PROTOCOL}; update the agent`);
        }
        const hello: Hello = {
          version: m.version,
          protocol: m.protocol,
          facts: m.facts,
          machines: m.machines,
          ...(m.machinesError ? { machinesError: m.machinesError } : {}),
          at: now,
        };
        await this.ctx.storage.put({ hello, mirroredFacts: JSON.stringify(m.facts), mirroredAt: now, mirrorDue: false });
        await mirrorNode(this.env.DB, id, { state: "online", lastSeenAt: now, version: m.version, protocol: m.protocol, facts: m.facts });
        if (this.env.NODE_IMAGE) {
          // providers (M6): the hosts the node CA is limited to, every one the
          // deployment could hold a key for, so that a key set later works
          // without a new CA.
          const welcome: WorkerMessage = { type: "welcome", image: this.env.NODE_IMAGE, providers: Object.values(PROVIDERS).map((p) => p.host) };
          try {
            ws.send(JSON.stringify(welcome));
          } catch {}
        }
        if ((await this.ctx.storage.getAlarm()) === null) await this.ctx.storage.setAlarm(now + STALE_CHECK_MS);
        // Not awaited here: a sandbox busy starting waits on an op answer
        // that comes on this socket.
        this.ctx.waitUntil(this.reconcile(id, hello));
        return;
      }
      case "facts": {
        const hello = await this.ctx.storage.get<Hello>("hello");
        if (hello) await this.ctx.storage.put("hello", { ...hello, facts: m.facts });
        await this.mirrorFacts(id, m.facts, now);
        return;
      }
      case "state": {
        // To the machine's Sandbox object, which acts on it only if its
        // record names this node. Nothing is stored for it here, so a node
        // can't fill this object's storage with made-up machines.
        const sb = m.machine.id;
        if (!(await this.isMember(id, sb, now))) {
          console.log(`[${id}] a state for ${sb}, which isn't on this node: dropped`);
          return;
        }
        console.log(`[${id}] machine ${sb} is ${m.machine.state}`);
        this.ctx.waitUntil(
          this.env.SANDBOX.getByName(sb)
            .nodeReport(id, { online: true, lastSeenAt: now, machine: m.machine.state })
            .catch((e: unknown) => console.error(`[${id}] telling ${sb}: ${errorStatus(e).message}`)),
        );
        return;
      }
      case "tickets": {
        const revoked = await this.ctx.storage.get<boolean>("revoked");
        // A ticket for a sandbox that isn't this node's would only wake that
        // sandbox's object to be refused there.
        const why = revoked ? "this node was removed" : (await this.isMember(id, m.sandbox, now)) ? null : `${m.sandbox} isn't a sandbox on this node`;
        const tickets = why ? [] : await Promise.all(Array.from({ length: m.count }, () => issueTicket(this.env.ADMIN_KEY, id, m.sandbox, now)));
        const answer: WorkerMessage = { type: "tickets", id: m.id, tickets, ...(why ? { error: why } : {}) };
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
    const s = await this.ctx.storage.get(["lastSeenAt", "connectedAt", "colo", "hello", "clockSkewSec"]);
    return {
      online,
      lastSeenAt: use ? lastSign(use) : ((s.get("lastSeenAt") as number | undefined) ?? null),
      connectedAt: (s.get("connectedAt") as number | undefined) ?? null,
      colo: (s.get("colo") as string | undefined) ?? null,
      hello: (s.get("hello") as Hello | undefined) ?? null,
      tombstones: await this.tombstones(),
      clockSkewSec: (s.get("clockSkewSec") as number | undefined) ?? null,
    };
  }

  // A request to the agent, answered within timeoutMs.
  async op(name: NodeOpName, args: Record<string, unknown> = {}, timeoutMs = 10_000): Promise<Record<string, unknown>> {
    if (await this.ctx.storage.get<boolean>("revoked")) fail(410, "the node was removed");
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
    if ((name === "create" || name === "start") && typeof args.sandbox === "string") {
      this.members ??= { ids: new Set(), loadedAt: 0 };
      this.members.ids.add(args.sandbox);
    }
    const out = await answer;
    // A removed machine is no longer the node's, for the orphans listed in
    // cloudex node info, until its next hello lists them all again.
    if (name === "delete" && typeof args.sandbox === "string") await this.forgetMachine(args.sandbox);
    return out;
  }

  private async forgetMachine(sandbox: string) {
    const hello = await this.ctx.storage.get<Hello>("hello");
    if (hello?.machines.some((m) => m.id === sandbox)) {
      await this.ctx.storage.put("hello", { ...hello, machines: hello.machines.filter((m) => m.id !== sandbox) });
    }
  }

  // cloudex node rm: the node is refused from now on, and its socket closes.
  async revoke() {
    await this.ctx.storage.put("revoked", true);
    await this.ctx.storage.deleteAlarm();
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
