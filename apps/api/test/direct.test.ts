// The direct path (v0.2.3, src/direct.ts): the node's secret, tickets against
// the vectors the Go agent checks too (fixtures/node/protocol.json), the
// candidates a client is given, the object's refusals, and the sessions it keeps.
import { fakeD1 } from "./d1.ts";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

const {
  SEEN_RENEW_MS,
  TOKEN_FORGET_MS,
  TOKEN_TICKETS_MAX,
  TOKENS_MINT_MAX,
  TOKENS_REPORTED_MAX,
  TOKENS_CHECKED_PER_REPORT,
  mergeTokens,
  directCandidates,
  directRefusal,
  directSecret,
  endedTokens,
  mintDirectTicket,
  mintToken,
  privateOrShared,
  readDirectTicket,
  reportTokens,
  sweepTokens,
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
  valid: { name: string; ticket: string; claims: { n: string; s: string; p: number; i: string; t: string; e: number } }[];
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
    const t = await mintDirectTicket(v.secret, { node: e.claims.n, sandbox: e.claims.s, port: e.claims.p, token: e.claims.t || null, session: e.claims.i }, v.nowMs);
    assert.equal(t.ticket, e.ticket, e.name);
    assert.equal(t.exp, e.claims.e);
    const c = await readDirectTicket(v.secret, e.ticket, v.nowMs);
    assert.deepEqual(c, { node: e.claims.n, sandbox: e.claims.s, port: e.claims.p, session: e.claims.i, token: e.claims.t, exp: e.claims.e }, e.name);
    assert.ok(await readDirectTicket(v.secret, e.ticket, v.nowMs + DIRECT_TICKET_SEC * 1000 - 1));
    assert.equal(await readDirectTicket(v.secret, e.ticket, v.nowMs + DIRECT_TICKET_SEC * 1000), null, `${e.name}: expired`);
  }
});

test("every invalid ticket in the fixture is refused", async () => {
  for (const e of v.invalid) assert.equal(await readDirectTicket(v.secret, e.ticket, v.nowMs), null, e.name);
});

test("a ticket names its node, sandbox and port, and works only under that node's secret", async () => {
  const t = await mintDirectTicket(v.secret, { node: v.node, sandbox: "cx-abc123", port: 65535, token: "tk-zzzz7777" });
  assert.match(t.session, /^[0-9a-f]{16}$/);
  assert.ok(t.ticket.length <= DIRECT_TICKET_MAX);
  const c = (await readDirectTicket(v.secret, t.ticket))!;
  assert.deepEqual([c.node, c.sandbox, c.port, c.token], [v.node, "cx-abc123", 65535, "tk-zzzz7777"]);
  const other = await directSecret(v.adminKey, "nd-zzzz9999", v.publicKey);
  assert.equal(await readDirectTicket(other, t.ticket), null);
  // Each mint is its own session.
  const t2 = await mintDirectTicket(v.secret, { node: v.node, sandbox: "cx-abc123", port: 22, token: null });
  assert.equal((await readDirectTicket(v.secret, t2.ticket))!.token, "", "the admin key's");
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

test("tokens: kept from the mint, renewed by reports now and then, forgotten an hour after the last evidence", async () => {
  const now = 1_000_000_000_000;
  const e = Math.floor(now / 1000) + 60;
  let s = mintToken({}, "tk-aaaa1111", e, now) as Record<string, { m: number[]; s: number }>;
  assert.deepEqual(s, { "tk-aaaa1111": { m: [e], s: now } });
  assert.equal(mintToken(s, null, e, now), s, "the admin key isn't kept");
  // A report renews only now and then, and adds a token it names that wasn't minted here.
  const yes = async () => true;
  assert.equal((await reportTokens(s, ["tk-aaaa1111"], now + 30_000, yes)).next, s);
  assert.equal((await reportTokens(s, ["tk-aaaa1111"], now + SEEN_RENEW_MS, yes)).next["tk-aaaa1111"]!.s, now + SEEN_RENEW_MS);
  s = (await reportTokens(s, ["tk-bbbb2222"], now, yes)).next;
  assert.deepEqual(s["tk-bbbb2222"], { m: [], s: now, r: 1 });
  // Expired ticket times go; a token with no evidence for an hour goes.
  assert.deepEqual(sweepTokens(s, e * 1000)["tk-aaaa1111"], { m: [], s: now });
  assert.ok(sweepTokens(s, now + TOKEN_FORGET_MS - 1)["tk-aaaa1111"]);
  assert.equal(sweepTokens(s, now + TOKEN_FORGET_MS)["tk-aaaa1111"], undefined);
});

test("a mint over a token's caps is refused, never made room for by forgetting another", () => {
  const now = 1_000_000_000_000;
  const e = Math.floor(now / 1000) + 60;
  let s: Record<string, { m: number[]; s: number }> = {};
  for (let i = 0; i < TOKEN_TICKETS_MAX; i++) s = mintToken(s, "tk-aaaa1111", e, now) as typeof s;
  assert.match(mintToken(s, "tk-aaaa1111", e, now) as string, /direct tickets for the sandbox from the last minute/);
  // Once those expire, it may mint again.
  assert.equal(typeof mintToken(s, "tk-aaaa1111", e + 60, e * 1000), "object");
  // Distinct tokens: refused past the cap while the others' entries last.
  s = {};
  for (let i = 0; i < TOKENS_MINT_MAX; i++) s = mintToken(s, `tk-${i.toString().padStart(8, "a")}`, e, now) as typeof s;
  assert.match(mintToken(s, "tk-zzzz9999", e, now) as string, /tokens have used the direct path/);
  assert.equal(typeof mintToken(sweepTokens(s, now + TOKEN_FORGET_MS), "tk-zzzz9999", e, now + TOKEN_FORGET_MS), "object");
});

test("review D-M1: a token that minted 256 more tickets after the one it used, then was revoked, is ended", async () => {
  // Before: the object kept 256 sessions and dropped the oldest unopened to
  // mint more, so the first ticket's session was forgotten and never ended.
  const now = 1_000_000_000_000;
  let s: Record<string, { m: number[]; s: number }> | string = {};
  let refused = 0;
  for (let i = 0; i < 257; i++) {
    const next = mintToken(s as Record<string, { m: number[]; s: number }>, "tk-evil0001", Math.floor(now / 1000) + 60, now + i);
    if (typeof next === "string") refused++;
    else s = next;
  }
  assert.equal(refused, 257 - TOKEN_TICKETS_MAX, "mints past the cap are refused");
  s = sweepTokens(s as Record<string, { m: number[]; s: number }>, now + 5 * 60_000);
  assert.deepEqual(await endedTokens(s, async () => false), ["tk-evil0001"]);
});

test("review D-M1: a session whose open report was lost is still ended, by the token its next report or the mint names", async () => {
  const now = 1_000_000_000_000;
  // The mint alone keeps the token for an hour: past the 3 minutes after
  // which an unopened session used to be forgotten, and past the agent's
  // 5-minute quiet report.
  let s = mintToken({}, "tk-quiet001", Math.floor(now / 1000) + 60, now) as Record<string, { m: number[]; s: number }>;
  s = sweepTokens(s, now + 10 * 60_000);
  assert.deepEqual(await endedTokens(s, async () => false), ["tk-quiet001"]);
  // An object that has forgotten it (or never minted it) takes it from a report.
  // A forgotten token that still works is kept for the alarm; one that ended is ended at once.
  s = (await reportTokens({}, ["tk-quiet001"], now + 2 * TOKEN_FORGET_MS, async () => true)).next;
  assert.deepEqual(await endedTokens(s, async () => false), ["tk-quiet001"]);
  assert.deepEqual(await reportTokens({}, ["tk-quiet001"], now + 2 * TOKEN_FORGET_MS, async () => false), { next: {}, end: ["tk-quiet001"] });
  // A failed check leaves it for the next alarm.
  assert.deepEqual(await endedTokens(s, async () => Promise.reject(new Error("D1"))), []);
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
  // The node's key, for cloudex doctor's pinned handshake.
  assert.equal((await findNode(db as never, "box"))!.publicKey, "KEY");
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
  assert.equal("publicKey" in (await findNode(db as never, "box"))!, false);
});

test("re-review 3: a node naming made-up tokens can't grow the record or lock real tokens out", async () => {
  // Before: 120 reports of 64 made-up ids gave 7,680 entries (315 KB), and a
  // real token's mint was refused for an hour.
  const now = 1_000_000_000_000;
  let all: Record<string, { m: number[]; s: number; r?: 1 }> = {};
  const real = new Set(["tk-real0001"]);
  let ended = 0;
  let asked = 0;
  for (let r = 0; r < 120; r++) {
    const ts = Array.from({ length: 64 }, (_, i) => `tk-${(r * 64 + i).toString(36).padStart(8, "0")}`);
    const out = await reportTokens(all, ts, now + r * 500, async (t) => (asked++, real.has(t)));
    all = out.next;
    ended += out.end.length;
  }
  assert.equal(Object.keys(all).length, 0, "made-up tokens aren't kept");
  assert.equal(asked, 120 * TOKENS_CHECKED_PER_REPORT, "D1 is asked a bounded number of times a report");
  assert.equal(ended, asked, "their sessions are ended at once");
  assert.equal(typeof mintToken(all, "tk-real0001", Math.floor(now / 1000) + 120, now + 61_000), "object");
  // Tokens that do work, reported and not minted here, are capped apart from minted ones.
  all = {};
  for (let r = 0; r < 10; r++) {
    const ts = Array.from({ length: 16 }, (_, i) => `tk-${(r * 16 + i).toString(36).padStart(8, "0")}`);
    all = (await reportTokens(all, ts, now, async () => true)).next;
  }
  assert.equal(Object.keys(all).length, TOKENS_REPORTED_MAX);
  assert.ok(JSON.stringify(all).length < 128 * 64);
  for (let i = 0; i < TOKENS_MINT_MAX; i++) all = mintToken(all, `tk-mint${i.toString(36).padStart(4, "0")}`, Math.floor(now / 1000) + 60, now) as typeof all;
  assert.equal(Object.keys(all).length, TOKENS_REPORTED_MAX + TOKENS_MINT_MAX);
});

test("re-review 2: what a mint or report wrote while the alarm awaited D1 is merged, not undone", () => {
  const read = { "tk-aaaa1111": { m: [], s: 1 }, "tk-gone0001": { m: [], s: 1 } };
  // Meanwhile a mint added a token and renewed another.
  const now = { ...read, "tk-aaaa1111": { m: [99], s: 5 }, "tk-new00001": { m: [99], s: 5 } };
  assert.deepEqual(mergeTokens(now, {}, ["tk-gone0001"]), { "tk-aaaa1111": { m: [99], s: 5 }, "tk-new00001": { m: [99], s: 5 } });
  // A report's addition merged into a record a mint changed meanwhile keeps the mint's times.
  assert.deepEqual(mergeTokens(now, { "tk-aaaa1111": { m: [], s: 9, r: 1 }, "tk-rep00001": { m: [], s: 9, r: 1 } }, []), {
    "tk-aaaa1111": { m: [99], s: 9 },
    "tk-gone0001": { m: [], s: 1 },
    "tk-new00001": { m: [99], s: 5 },
    "tk-rep00001": { m: [], s: 9, r: 1 },
  });
});
