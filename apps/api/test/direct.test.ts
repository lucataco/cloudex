// The direct path (v0.2.3, src/direct.ts): the node's secret, tickets against
// the vectors the Go agent checks too (fixtures/node/protocol.json), the
// candidates a client is given, the object's refusals, and the sessions it keeps.
import { fakeD1 } from "./d1.ts";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

const {
  MINTED_GRACE_MS,
  OPEN_FORGET_MS,
  SEEN_RENEW_MS,
  SESSIONS_KEPT,
  directCandidates,
  directRefusal,
  directSecret,
  endedSessions,
  mintDirectTicket,
  mintSession,
  privateOrShared,
  readDirectTicket,
  reportSessions,
  sweepSessions,
} = await import("../src/direct.ts");
const { DIRECT_TICKET_MAX, DIRECT_TICKET_SEC } = await import("@cloudex/schema/node");

type Vectors = {
  label: string;
  adminKey: string;
  node: string;
  publicKey: string;
  secret: string;
  nowMs: number;
  ttlSec: number;
  max: number;
  valid: { name: string; ticket: string; claims: { n: string; s: string; p: number; i: string; e: number } }[];
  invalid: { name: string; ticket: string }[];
};
const v = (JSON.parse(readFileSync(new URL("../../../packages/schema/fixtures/node/protocol.json", import.meta.url), "utf8")) as { direct: Vectors }).direct;

test("the secret is the fixture's, and depends on the admin key, the node and its key", async () => {
  assert.equal(v.ttlSec, DIRECT_TICKET_SEC);
  assert.equal(v.max, DIRECT_TICKET_MAX);
  assert.equal(await directSecret(v.adminKey, v.node, v.publicKey), v.secret);
  assert.equal(atob(v.secret).length, 32);
  for (const other of [
    await directSecret("another", v.node, v.publicKey),
    await directSecret(v.adminKey, "nd-zzzz9999", v.publicKey),
    await directSecret(v.adminKey, v.node, "AAAA"),
  ]) {
    assert.notEqual(other, v.secret);
  }
});

test("the Worker mints exactly the fixture's tickets, and reads them back until they expire", async () => {
  for (const e of v.valid) {
    const t = await mintDirectTicket(v.secret, { node: e.claims.n, sandbox: e.claims.s, port: e.claims.p, session: e.claims.i }, v.nowMs);
    assert.equal(t.ticket, e.ticket, e.name);
    assert.equal(t.exp, e.claims.e);
    const c = await readDirectTicket(v.secret, e.ticket, v.nowMs);
    assert.deepEqual(c, { node: e.claims.n, sandbox: e.claims.s, port: e.claims.p, session: e.claims.i, exp: e.claims.e }, e.name);
    assert.ok(await readDirectTicket(v.secret, e.ticket, v.nowMs + DIRECT_TICKET_SEC * 1000 - 1));
    assert.equal(await readDirectTicket(v.secret, e.ticket, v.nowMs + DIRECT_TICKET_SEC * 1000), null, `${e.name}: expired`);
  }
});

test("every invalid ticket in the fixture is refused", async () => {
  for (const e of v.invalid) assert.equal(await readDirectTicket(v.secret, e.ticket, v.nowMs), null, e.name);
});

test("a ticket names its node, sandbox and port, and works only under that node's secret", async () => {
  const t = await mintDirectTicket(v.secret, { node: v.node, sandbox: "cx-abc123", port: 22 });
  assert.match(t.session, /^[0-9a-f]{16}$/);
  assert.ok(t.ticket.length <= DIRECT_TICKET_MAX);
  const c = (await readDirectTicket(v.secret, t.ticket))!;
  assert.deepEqual([c.node, c.sandbox, c.port], [v.node, "cx-abc123", 22]);
  const other = await directSecret(v.adminKey, "nd-zzzz9999", v.publicKey);
  assert.equal(await readDirectTicket(other, t.ticket), null);
  // Each mint is its own session.
  const t2 = await mintDirectTicket(v.secret, { node: v.node, sandbox: "cx-abc123", port: 22 });
  assert.notEqual(t2.session, t.session);
  assert.equal(await readDirectTicket(v.secret, "x".repeat(DIRECT_TICKET_MAX + 1)), null);
});

test("clients are given only private and tailnet addresses of a node that reports listening", () => {
  assert.equal(privateOrShared("10.1.2.3") && privateOrShared("172.31.0.1") && privateOrShared("192.168.65.2") && privateOrShared("100.124.24.58"), true);
  for (const ip of ["172.32.0.1", "100.128.0.1", "8.8.8.8", "203.0.113.9", "127.0.0.1", "169.254.1.1", "0.0.0.0", "192.169.0.1", "10.0.0.256", "fd7a::1"]) {
    assert.equal(privateOrShared(ip), false, ip);
  }
  const d = (x: object) => JSON.stringify({ port: 7022, addrs: ["192.168.1.20", "8.8.8.8", "100.124.24.58", "192.168.1.20"], state: "listening", ...x });
  assert.deepEqual(directCandidates(d({})), { port: 7022, addrs: ["192.168.1.20", "100.124.24.58"] });
  assert.equal(directCandidates(d({ state: "failed" })), null);
  assert.equal(directCandidates(d({ state: "off" })), null);
  assert.equal(directCandidates(d({ addrs: ["8.8.8.8"] })), null, "only public addresses");
  assert.equal(directCandidates(d({ addrs: [] })), null);
  assert.equal(directCandidates(null), null, "an agent older than v0.2.3");
  assert.equal(directCandidates("not json"), null);
  assert.equal(directCandidates(d({ port: 0 })), null);
});

test("the object refuses as serve() does: moved, removed, not running", () => {
  const base = { state: "running", onNode: "nd-abcd1234", asked: "nd-abcd1234", revokedNode: null };
  assert.equal(directRefusal(base), null);
  assert.equal(directRefusal({ ...base, state: "stopped" }), "not-running");
  assert.equal(directRefusal({ ...base, state: "starting" }), "not-running");
  assert.equal(directRefusal({ ...base, revokedNode: "nd-abcd1234", state: "error" }), "removed");
  assert.equal(directRefusal({ ...base, revokedNode: "nd-other000" }), null);
  assert.equal(directRefusal({ ...base, onNode: null }), "moved", "on Cloudflare");
  assert.equal(directRefusal({ ...base, asked: "nd-other000" }), "moved");
});

test("sessions: reported open, closed when a report leaves them out, minted ones forgotten after their ticket", () => {
  const now = 1_000_000_000_000;
  const e = Math.floor(now / 1000) + 60;
  let s = mintSession({}, "a000000000000000", { t: "tk-1", e });
  s = mintSession(s, "b000000000000000", { t: null, e });
  s = reportSessions(s, ["a000000000000000", "ffffffffffffffff"], now);
  assert.equal(s["a000000000000000"]!.o, now);
  assert.equal(s["ffffffffffffffff"], undefined, "an id it didn't mint is ignored");
  assert.equal(s["b000000000000000"]!.o, undefined, "minted, not open yet");
  // Reports while it stays open renew o only now and then.
  assert.equal(reportSessions(s, ["a000000000000000"], now + 30_000)["a000000000000000"]!.o, now);
  assert.equal(reportSessions(s, ["a000000000000000"], now + SEEN_RENEW_MS)["a000000000000000"]!.o, now + SEEN_RENEW_MS);
  // Closed: left out of a report.
  const closed = reportSessions(s, [], now + 1000);
  assert.deepEqual(Object.keys(closed), ["b000000000000000"]);
  // Swept: the minted one after its ticket plus the grace, the open one when no report named it for long.
  assert.ok(sweepSessions(s, e * 1000 + MINTED_GRACE_MS - 1)["b000000000000000"]);
  assert.equal(sweepSessions(s, e * 1000 + MINTED_GRACE_MS)["b000000000000000"], undefined);
  assert.ok(sweepSessions(s, now + OPEN_FORGET_MS - 1)["a000000000000000"]);
  assert.equal(sweepSessions(s, now + OPEN_FORGET_MS)["a000000000000000"], undefined);
});

test("sessions kept are bounded: minting drops the oldest that aren't open", () => {
  const now = 1_000_000_000_000;
  let s = mintSession({}, "0".repeat(16), { t: "tk-1", e: 1 });
  s = reportSessions(s, ["0".repeat(16)], now);
  for (let i = 1; i <= SESSIONS_KEPT + 10; i++) s = mintSession(s, i.toString(16).padStart(16, "0"), { t: "tk-1", e: 100 + i });
  assert.equal(Object.keys(s).length, SESSIONS_KEPT);
  assert.ok(s["0".repeat(16)], "the open one stays");
  assert.ok(s[(SESSIONS_KEPT + 10).toString(16).padStart(16, "0")], "the newest stays");
  assert.equal(s[(1).toString(16).padStart(16, "0")], undefined, "the oldest went");
});

test("sessions whose token ended are ended: open ones, and unused tickets still good; each token asked once", async () => {
  const now = 1_000_000_000_000;
  const live = Math.floor(now / 1000) + 30;
  let s = mintSession({}, "a000000000000000", { t: "tk-gone", e: 1 });
  s = reportSessions(s, ["a000000000000000"], now - 1000); // open, ticket long expired
  s = mintSession(s, "b000000000000000", { t: "tk-gone", e: live }); // unused, still good
  s = mintSession(s, "c000000000000000", { t: "tk-gone", e: 1 }); // unused, expired: nothing to end
  s = mintSession(s, "d000000000000000", { t: "tk-live", e: live });
  s = mintSession(s, "e000000000000000", { t: null, e: live }); // the admin key
  const asked: string[] = [];
  const ended = await endedSessions(s, async (t) => (asked.push(t), t === "tk-live"), now);
  assert.deepEqual(ended.sort(), ["a000000000000000", "b000000000000000"]);
  assert.deepEqual(asked.sort(), ["tk-gone", "tk-live"]);
  // A D1 error leaves them for the next check.
  assert.deepEqual(await endedSessions(s, async () => Promise.reject(new Error("D1")), now), []);
});

test("GET /v1/sandboxes/{ref}/direct needs what tcp needs: a read token gets 403, a user token passes", async () => {
  const { ruleFor } = await import("../src/rules.ts");
  const { authorize } = await import("../src/tokens.ts");
  assert.deepEqual(ruleFor("GET", "/v1/sandboxes/n1/direct"), ruleFor("GET", "/v1/sandboxes/n1/tcp/22"));
  assert.equal(ruleFor("GET", "/v1/sandboxes/n1/direct").need, "write");
  const token = (role: string) => ({ kind: "token", token: { id: "tk-1", role, ownOnly: false } }) as never;
  await assert.rejects(authorize({} as never, token("read"), "GET", "/v1/sandboxes/n1/direct", async () => "cx-abc123"), /^Error: \[403\]/);
  await authorize({} as never, token("user"), "GET", "/v1/sandboxes/n1/direct", async () => "cx-abc123");
});

test("the node's direct fact reaches D1 with its facts, and the route's one read finds it", async () => {
  const db = fakeD1();
  const { ensureSchema, directRow } = await import("../src/db.ts");
  const { mirrorNode, findNode, revokeNode } = await import("../src/nodes.ts");
  await ensureSchema(db as never);
  db.exec("INSERT INTO nodes (id, name, state, created_at, public_key) VALUES ('nd-abcd1234', 'box', 'online', 0, 'KEY')");
  db.exec("INSERT INTO sandboxes (id, size, state, idle_minutes, created_at, updated_at, node) VALUES ('cx-abc123', 'standard-2', 'running', 10, 0, 0, 'nd-abcd1234')");
  db.exec("INSERT INTO sandboxes (id, size, state, idle_minutes, created_at, updated_at) VALUES ('cx-cf0001', 'standard-2', 'running', 10, 0, 0)");
  const facts = { arch: "amd64", runtime: "docker", isolation: { firewall: "pass", unisolated: false }, cpus: 2, memoryMb: 1024, diskFreeGb: 50 } as const;
  const direct = { port: 7022, addrs: ["192.168.65.2"], state: "listening" } as const;
  await mirrorNode(db as never, "nd-abcd1234", { facts: { ...facts, direct } });
  const row = (await directRow(db as never, "cx-abc123"))!;
  assert.deepEqual({ ...row, direct: JSON.parse(row.direct!) }, { node: "nd-abcd1234", name: "box", publicKey: "KEY", direct });
  assert.deepEqual(directCandidates(row.direct), { port: 7022, addrs: ["192.168.65.2"] });
  assert.deepEqual((await findNode(db as never, "box"))!.direct, direct);
  assert.deepEqual(await directRow(db as never, "cx-cf0001"), { node: null, name: null, publicKey: null, direct: null });
  assert.equal(await directRow(db as never, "cx-none00"), null);
  // An agent older than v0.2.3 reports none: cleared.
  await mirrorNode(db as never, "nd-abcd1234", { facts });
  assert.equal((await directRow(db as never, "cx-abc123"))!.direct, null);
  assert.equal("direct" in (await findNode(db as never, "box"))!, false);
  // Removed: no key, no direct.
  await mirrorNode(db as never, "nd-abcd1234", { facts: { ...facts, direct } });
  await revokeNode(db as never, "nd-abcd1234");
  assert.deepEqual(await directRow(db as never, "cx-abc123"), { node: "nd-abcd1234", name: "box", publicKey: null, direct: null });
});
