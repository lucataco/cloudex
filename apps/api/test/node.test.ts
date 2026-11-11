// The Node object's control socket (node.ts) against a node that floods it
// (2026-11-12 review, W1): a second hello closes the socket; facts, state and
// ticket requests are limited per socket; facts reach D1 only when they
// change, at most every MIRROR_MIN_MS; and a state or ticket request for a
// sandbox that isn't on the node wakes no Sandbox object.
import { registerHooks } from "node:module";
import assert from "node:assert/strict";
import test from "node:test";
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
const { Node, PER_MINUTE, MIRROR_MIN_MS } = await import("../src/node.ts");

const NODE = "nd-abcd1234";
const db = fakeD1();
// Every statement that writes the nodes table.
let nodeWrites = 0;
const counted = {
  ...db,
  prepare(sql: string) {
    if (/^\s*UPDATE nodes/i.test(sql)) nodeWrites++;
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
  const waits: Promise<unknown>[] = [];
  const ctx = {
    storage,
    setWebSocketAutoResponse() {},
    getWebSockets: () => sockets,
    getWebSocketAutoResponseTimestamp: () => null,
    waitUntil: (p: Promise<unknown>) => void waits.push(p),
  };
  const env = {
    DB: counted,
    ADMIN_KEY: "admin-key-for-tests",
    NODE_IMAGE: "",
    SANDBOX: { getByName: (sb: string) => ({ nodeReport: async () => void woken.push(sb) }) },
  };
  const node = new Node(ctx as never, env as never);
  const ws = new FakeWs({ gen: 1, acceptedAt: Date.now(), lastMessageAt: null });
  sockets.push(ws);
  const say = (m: unknown) => node.webSocketMessage(ws as never, JSON.stringify(m));
  return { node, ws, say, woken, waits, storage };
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
  for (let i = 0; i < 300; i++) await say({ type: "state", machine: { id: "cx-other1", state: "running" } });
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
