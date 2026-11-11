// The Node object's control socket (node.ts) against a node that floods it
// (2026-11-12 review, W1): a second hello closes the socket; facts, state and
// ticket requests are limited per node, across its connections; facts reach
// D1 only when they change, at most every MIRROR_MIN_MS; a state or ticket
// request for a sandbox that isn't on the node wakes no Sandbox object; and a
// node that reconnects over and over doesn't wake its sandboxes each time.
import { registerHooks } from "node:module";
import assert from "node:assert/strict";
import test, { mock } from "node:test";
import { fakeD1 } from "./d1.ts";

registerHooks({
  resolve(spec, ctx, next) {
    if (spec === "cloudflare:workers") {
      const src = "export class DurableObject { constructor(ctx, env) { this.ctx = ctx; this.env = env; } }";
      return { url: `data:text/javascript,${encodeURIComponent(src)}`, shortCircuit: true };
    }
    return next(spec, ctx);
  },
});

(globalThis as Record<string, unknown>).WebSocketRequestResponsePair = class {};
const { Node, PER_MINUTE, MIRROR_MIN_MS, CONNECTS_PER_MINUTE } = await import("../src/node.ts");

const NODE = "nd-abcd1234";
const db = fakeD1();
// Every statement that writes the nodes table.
let nodeWrites = 0;
let memberReads = 0;
// Set, reading the node's sandboxes fails, as D1 does now and then.
let membersFail = false;
const counted = {
  ...db,
  prepare(sql: string) {
    if (/^\s*UPDATE nodes/i.test(sql)) nodeWrites++;
    if (/SELECT id FROM sandboxes WHERE node/i.test(sql)) {
      memberReads++;
      if (membersFail) return { bind: () => ({ all: async () => Promise.reject(new Error("D1_ERROR: overloaded")) }) };
    }
    return db.prepare(sql);
  },
  batch: db.batch,
} as unknown as D1Database;

class FakeWs {
  readyState = 1;
  sent: string[] = [];
  closed: { code: number; reason: string } | null = null;
  att: unknown;
  constructor(att: unknown) {
    this.att = att;
  }
  send(m: string) {
    this.sent.push(m);
  }
  close(code: number, reason: string) {
    this.closed = { code, reason };
    this.readyState = 3;
  }
  serializeAttachment(a: unknown) {
    this.att = structuredClone(a);
  }
  deserializeAttachment() {
    return this.att;
  }
}

function fakeStorage() {
  const kv = new Map<string, unknown>();
  let alarm: number | null = null;
  return {
    kv,
    sql: { exec: () => ({ rowsWritten: 1 }) },
    async get(k: string | string[]) {
      if (Array.isArray(k)) return new Map(k.filter((x) => kv.has(x)).map((x) => [x, kv.get(x)]));
      return kv.get(k);
    },
    async put(k: string | Record<string, unknown>, v?: unknown) {
      if (typeof k === "string") kv.set(k, v);
      else for (const [a, b] of Object.entries(k)) kv.set(a, b);
    },
    async delete(k: string) {
      kv.delete(k);
    },
    async list({ prefix }: { prefix: string }) {
      return new Map([...kv].filter(([k]) => k.startsWith(prefix)));
    },
    async getAlarm() {
      return alarm;
    },
    async setAlarm(t: number) {
      alarm = t;
    },
    async deleteAlarm() {
      alarm = null;
    },
  };
}

function setup() {
  const storage = fakeStorage();
  storage.kv.set("id", NODE);
  const sockets: FakeWs[] = [];
  const woken: string[] = [];
  const reports: [string, unknown][] = [];
  const waits: Promise<unknown>[] = [];
  const ctx = {
    storage,
    setWebSocketAutoResponse() {},
    // As the runtime does, a closed socket isn't listed.
    getWebSockets: () => sockets.filter((w) => w.readyState !== 3),
    acceptWebSocket: (ws: FakeWs) => void sockets.push(ws),
    getWebSocketAutoResponseTimestamp: () => null,
    waitUntil: (p: Promise<unknown>) => void waits.push(p),
  };
  const env = {
    DB: counted,
    ADMIN_KEY: "admin-key-for-tests",
    NODE_IMAGE: "",
    SANDBOX: { getByName: (sb: string) => ({ nodeReport: async (_id: string, r: unknown) => void (woken.push(sb), reports.push([sb, r])) }) },
  };
  const node = new Node(ctx as never, env as never);
  const ws = new FakeWs({ gen: 1, acceptedAt: Date.now(), lastMessageAt: null });
  sockets.push(ws);
  const say = (m: unknown) => node.webSocketMessage(ws as never, JSON.stringify(m));
  return { node, ws, say, woken, reports, waits, storage, sockets };
}

// A connect, as the Worker passes it on once its signature checked out
// (node.fetch). Node's Response refuses the runtime's 101, so it's a 200 here.
(globalThis as Record<string, unknown>).WebSocketPair = class {
  0 = new FakeWs(null);
  1 = new FakeWs(null);
};
const RealResponse = Response;
globalThis.Response = class extends RealResponse {
  constructor(body?: BodyInit | null, init?: ResponseInit) {
    super(body, init?.status === 101 ? { ...init, status: 200 } : init);
  }
} as typeof Response;
let nonces = 0;
async function connect(t: ReturnType<typeof setup>) {
  const headers = new Headers({ upgrade: "websocket", "x-cx-node": NODE, "x-cx-nonce": `nonce-${++nonces}` });
  const res = await t.node.fetch({ headers } as unknown as Request);
  assert.equal(res.status, 200, await res.clone().text());
  const ws = t.sockets.at(-1)!;
  return { ws, say: (m: unknown) => t.node.webSocketMessage(ws as never, JSON.stringify(m)) };
}

const facts = (cpus: number) => ({ arch: "amd64", runtime: "docker", isolation: { firewall: "pass", unisolated: false }, cpus, memoryMb: 1024, diskFreeGb: 50 });
const hello = { type: "hello", protocol: 1, version: "v0.2.2", facts: facts(2), machines: [] };

const { ensureSchema } = await import("../src/db.ts");
await ensureSchema(db);
db.exec(`INSERT INTO nodes (id, name, state, created_at, public_key) VALUES ('${NODE}', 'vm', 'offline', 0, 'k')`);
db.exec(`INSERT INTO sandboxes (id, size, state, idle_minutes, created_at, updated_at, node) VALUES ('cx-mine01', 'standard-2', 'running', 10, 0, 0, '${NODE}')`);
db.exec(`INSERT INTO sandboxes (id, size, state, idle_minutes, created_at, updated_at, node) VALUES ('cx-other1', 'standard-2', 'running', 10, 0, 0, 'nd-zzzz9999')`);

test("a second hello on one socket closes it", async () => {
  const { ws, say, waits } = setup();
  await say(hello);
  assert.equal(ws.closed, null);
  await say(hello);
  assert.equal(ws.closed?.code, 4400);
  assert.match(ws.closed?.reason ?? "", /hello comes once/);
  await Promise.all(waits);
});

test("facts reach D1 only when they change, at most every MIRROR_MIN_MS, and a flood is cut at the limit", async () => {
  const { say, waits, storage, node } = setup();
  await say(hello);
  await Promise.all(waits);
  nodeWrites = 0;
  for (let i = 0; i < 500; i++) await say({ type: "facts", facts: facts(2 + (i % 2)) });
  assert.ok(nodeWrites <= 1, `D1 writes for 500 facts in a burst: ${nodeWrites}`);
  assert.equal(storage.kv.get("mirrorDue"), true, "the change left over goes with the alarm");
  // The alarm writes it.
  const burst = nodeWrites;
  storage.kv.set("mirroredAt", Date.now() - MIRROR_MIN_MS);
  await node.alarm();
  assert.equal(storage.kv.get("mirrorDue"), false);
  assert.equal(nodeWrites, burst + 1);
  // The same facts again, later: nothing to write.
  storage.kv.set("mirroredAt", 0);
  const last = ((await storage.get("hello")) as { facts: unknown }).facts;
  await say({ type: "facts", facts: last });
  assert.equal(nodeWrites, burst + 1);
});

test("state and ticket requests for a sandbox not on the node wake nothing; a flood is cut at the limit", async () => {
  const { ws, say, woken, waits } = setup();
  await say(hello);
  await Promise.all(waits);
  woken.length = 0;
  // Many at once, as they arrive from a socket: D1 is read once for them all.
  memberReads = 0;
  await Promise.all(Array.from({ length: 100 }, () => say({ type: "state", machine: { id: "cx-other1", state: "running" } })));
  assert.equal(memberReads, 1, "one read of the node's sandboxes for a burst");
  for (let i = 0; i < 200; i++) await say({ type: "state", machine: { id: "cx-other1", state: "running" } });
  for (let i = 0; i < 50; i++) await say({ type: "state", machine: { id: `cx-x${String(i).padStart(5, "0")}`, state: "running" } });
  await Promise.all(waits);
  assert.deepEqual(woken, [], "no other sandbox's object is woken");

  await say({ type: "tickets", id: "tk-1", sandbox: "cx-other1", count: 2 });
  const answer = JSON.parse(ws.sent.at(-1)!);
  assert.deepEqual(answer.tickets, []);
  assert.match(answer.error, /isn't a sandbox on this node/);

  // Its own: woken, up to PER_MINUTE.state a minute in all (the 350 above counted).
  const fresh = setup();
  await fresh.say(hello);
  await Promise.all(fresh.waits);
  fresh.woken.length = 0;
  for (let i = 0; i < 1000; i++) await fresh.say({ type: "state", machine: { id: "cx-mine01", state: "running" } });
  await Promise.all(fresh.waits);
  assert.equal(fresh.woken.length, PER_MINUTE.state);
  for (let i = 0; i < PER_MINUTE.tickets + 5; i++) await fresh.say({ type: "tickets", id: `tk-${i}`, sandbox: "cx-mine01", count: 1 });
  const last = JSON.parse(fresh.ws.sent.at(-1)!);
  assert.match(last.error ?? "", /too many ticket requests/);
  const good = fresh.ws.sent.map((m) => JSON.parse(m)).filter((m) => m.type === "tickets" && m.tickets.length === 1);
  assert.equal(good.length, PER_MINUTE.tickets);
});

const state = (id: string) => ({ type: "state", machine: { id, state: "running" } });

test("the limits are the node's: a new connection, or one after a close, carries on the count", async () => {
  const t = setup();
  await t.say(hello);
  await Promise.all(t.waits);
  t.woken.length = 0;
  for (let i = 0; i < PER_MINUTE.state; i++) await t.say(state("cx-mine01"));
  await Promise.all(t.waits);
  assert.equal(t.woken.length, PER_MINUTE.state);
  // Replaced by a new connection.
  const second = await connect(t);
  assert.equal(t.ws.closed?.code, 4000);
  await second.say(hello);
  for (let i = 0; i < 10; i++) await second.say(state("cx-mine01"));
  // Closed, and connected again.
  await t.node.webSocketClose(second.ws as never, 1006, "");
  const third = await connect(t);
  await third.say(hello);
  for (let i = 0; i < 10; i++) await third.say(state("cx-mine01"));
  await Promise.all(t.waits);
  assert.equal(t.woken.length, PER_MINUTE.state, "no more in the minute");
});

test("a node that reconnects over and over wakes its sandboxes once; staying away is told once, by the alarm; coming back at once", async () => {
  const t = setup();
  for (let i = 0; i < CONNECTS_PER_MINUTE; i++) {
    const c = await connect(t);
    await c.say(hello);
    await t.node.webSocketClose(c.ws as never, 1006, "");
  }
  await Promise.all(t.waits);
  assert.equal(t.woken.length, 1, `sandbox objects woken by ${CONNECTS_PER_MINUTE} connects and closes`);
  const off = t.storage.kv.get("offlineDue") as { at: number; lastSeenAt: number };
  assert.ok(off.at >= Date.now() + 5_000, "not before the grace");
  // Its time comes, and the node is still away: told once.
  t.storage.kv.set("offlineDue", { ...off, at: Date.now() });
  await t.node.alarm();
  await Promise.all(t.waits);
  assert.equal(t.woken.length, 2);
  await t.node.alarm();
  await Promise.all(t.waits);
  assert.equal(t.woken.length, 2);
  // Back after they heard it was offline: told at once.
  t.storage.kv.delete("connects");
  const back = await connect(t);
  await back.say(hello);
  await Promise.all(t.waits);
  assert.equal(t.woken.length, 3);
  // A drop and a reconnect within the grace: nothing told for the drop.
  await t.node.webSocketClose(back.ws as never, 1006, "");
  const again = await connect(t);
  await again.say(hello);
  t.storage.kv.set("offlineDue", { ...(t.storage.kv.get("offlineDue") as object), at: Date.now() });
  await t.node.alarm();
  await Promise.all(t.waits);
  assert.equal(t.woken.length, 3);
  // Its hello's reconcile comes with the alarm, TELL_MIN_MS after the last.
  t.storage.kv.set("reconcileDue", Date.now());
  await t.node.alarm();
  await Promise.all(t.waits);
  assert.equal(t.woken.length, 4);
});

test("a deleted sandbox drops off the node's list", async () => {
  db.exec(`INSERT INTO sandboxes (id, size, state, idle_minutes, created_at, updated_at, node) VALUES ('cx-gone01', 'standard-2', 'running', 10, 0, 0, '${NODE}')`);
  db.exec(`INSERT INTO sandboxes (id, size, state, idle_minutes, created_at, updated_at, node) VALUES ('cx-gone02', 'standard-2', 'running', 10, 0, 0, '${NODE}')`);
  const t = setup();
  await t.say(hello);
  await Promise.all(t.waits);
  t.woken.length = 0;
  await t.say(state("cx-gone01"));
  await t.say(state("cx-gone02"));
  await Promise.all(t.waits);
  assert.deepEqual(t.woken, ["cx-gone01", "cx-gone02"]);
  // Its machine removed through the node: off the list at once.
  db.exec("DELETE FROM sandboxes WHERE id = 'cx-gone01'");
  const removing = t.node.op("delete", { sandbox: "cx-gone01" }, 2000);
  await new Promise((r) => setImmediate(r));
  const op = JSON.parse(t.ws.sent.at(-1)!);
  assert.equal(op.name, "delete");
  await t.say({ type: "result", id: op.id, ok: true });
  await removing;
  await t.say(state("cx-gone01"));
  await Promise.all(t.waits);
  assert.deepEqual(t.woken, ["cx-gone01", "cx-gone02"], "not woken once its machine was removed");
  // Deleted otherwise: off the list once it's MEMBERS_MAX_AGE_MS old.
  db.exec("DELETE FROM sandboxes WHERE id = 'cx-gone02'");
  mock.timers.enable({ apis: ["Date"], now: Date.now() });
  try {
    // (A message in between, so that the socket isn't stale.)
    mock.timers.tick(40_000);
    await t.say({ type: "facts", facts: facts(2) });
    mock.timers.tick(21_000);
    await t.say(state("cx-gone02"));
    await t.say(state("cx-gone01"));
    await t.say(state("cx-mine01"));
    await Promise.all(t.waits);
    assert.deepEqual(t.woken, ["cx-gone01", "cx-gone02", "cx-mine01"], "neither woken again");
  } finally {
    mock.timers.reset();
  }
});

test("a failed read of the node's list of sandboxes isn't tried again at every message", async () => {
  const f = setup();
  await f.say(hello);
  await Promise.all(f.waits);
  membersFail = true;
  memberReads = 0;
  try {
    for (let i = 0; i < 20; i++) await f.say(state(`cx-y${String(i).padStart(5, "0")}`));
  } finally {
    membersFail = false;
  }
  assert.equal(memberReads, 1, "one read of D1 for 20 messages while it fails");
});

test("a hello's note for a machine the agent stopped itself reaches its sandbox with the reconcile", async () => {
  // The state message that told it is lost when the link is down then, as
  // after a flush of the host's rules (review H3, seen live under Sysbox).
  const t = setup();
  await t.say({ ...hello, machines: [{ id: "cx-mine01", state: "stopped", note: "this node's firewall table (inet cloudex) disappeared" }] });
  await Promise.all(t.waits);
  const r = t.reports.find(([sb]) => sb === "cx-mine01")?.[1] as { machine?: string; note?: string } | undefined;
  assert.equal(r?.machine, "stopped");
  assert.match(r?.note ?? "", /firewall table/);
});
