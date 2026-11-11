// Node signatures, nonces and join tokens (nodeauth.ts), and nodes in D1
// (nodes.ts), against a D1 in memory (d1.ts).
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import { fakeD1 } from "./d1.ts";

const auth = await import("../src/nodeauth.ts");
const nodes = await import("../src/nodes.ts");
const { ruleFor } = await import("../src/rules.ts");
const { NodeClose } = await import("@cloudex/schema/node");

const DB = fakeD1();
const fixture = JSON.parse(readFileSync(new URL("../../../packages/schema/fixtures/node/protocol.json", import.meta.url), "utf8"));
const FACTS = { arch: "amd64", runtime: "docker", isolation: { firewall: "unchecked", unisolated: false }, cpus: 2, memoryMb: 4096, diskFreeGb: null } as const;

const b64 = (b: ArrayBuffer | Uint8Array) => Buffer.from(b instanceof Uint8Array ? b : new Uint8Array(b)).toString("base64");

async function keypair() {
  const k = (await crypto.subtle.generateKey({ name: "Ed25519" }, true, ["sign", "verify"])) as CryptoKeyPair;
  return { privateKey: k.privateKey, publicKey: b64(await crypto.subtle.exportKey("raw", k.publicKey)) };
}

const hex = (n: number) => [...crypto.getRandomValues(new Uint8Array(n))].map((b) => b.toString(16).padStart(2, "0")).join("");

// A request signed as the agent signs it (go/internal/node/sign.go).
async function signed(
  key: CryptoKey,
  o: { node?: string; method?: string; url?: string; ts?: number; nonce?: string; signPath?: string; sandbox?: string; signNode?: string } = {},
) {
  const url = new URL(o.url ?? "https://api.example/v1/node/connect");
  const method = o.method ?? "GET";
  const ts = String(o.ts ?? Math.floor(Date.now() / 1000));
  const nonce = o.nonce ?? hex(16);
  const message = auth.signingString(method, o.signPath ?? url.pathname + url.search, o.signNode ?? o.node ?? "", o.sandbox ?? "", ts, nonce);
  const sig = b64(await crypto.subtle.sign("Ed25519", key, new TextEncoder().encode(message)));
  const headers: Record<string, string> = { "x-cx-ts": ts, "x-cx-nonce": nonce, "x-cx-sig": sig };
  if (o.node !== undefined) headers["x-cx-node"] = o.node;
  if (o.sandbox) headers["x-cx-sandbox"] = o.sandbox;
  return new Request(url, { method, headers });
}

test("the signing example in the shared fixture verifies, and nothing else does", async () => {
  const s = fixture.signing;
  const message = auth.signingString(s.method, s.path, s.node, s.sandbox, s.ts, s.nonce);
  assert.equal(message, s.string);
  assert.ok(await auth.verifyEd25519(s.publicKey, message, s.signature));
  assert.equal(await auth.verifyEd25519(s.publicKey, message.replace("connect", "connecT"), s.signature), false);
  assert.equal(await auth.verifyEd25519("AAAA", message, s.signature), false);
});

test("a node's signature is checked over method, path and query, time and nonce", async () => {
  const k = await keypair();
  const lookup = async (id: string) => (id === "nd-aaaa1111" ? k.publicKey : null);
  const ok = await auth.verifyNodeRequest(await signed(k.privateKey, { node: "nd-aaaa1111", url: "https://api.example/v1/node/connect?x=1" }), lookup);
  assert.ok("signed" in ok && ok.signed.node === "nd-aaaa1111");
  // Signed for another path or query, or with another method.
  for (const r of [
    await signed(k.privateKey, { node: "nd-aaaa1111", url: "https://api.example/v1/node/connect?x=2", signPath: "/v1/node/connect?x=1" }),
    await signed(k.privateKey, { node: "nd-aaaa1111", signPath: "/v1/node/join" }),
  ]) {
    const v = await auth.verifyNodeRequest(r, lookup);
    assert.ok("error" in v && v.status === 401 && v.error === auth.BAD_SIGNATURE);
  }
  // Another key.
  const other = await keypair();
  const v = await auth.verifyNodeRequest(await signed(other.privateKey, { node: "nd-aaaa1111" }), lookup);
  assert.ok("error" in v && v.error === auth.BAD_SIGNATURE);
});

test("an unknown node and a bad signature get the same answer", async () => {
  const k = await keypair();
  const lookup = async (id: string) => (id === "nd-aaaa1111" ? k.publicKey : null);
  const unknown = await auth.verifyNodeRequest(await signed(k.privateKey, { node: "nd-zzzz9999" }), lookup);
  const bad = await auth.verifyNodeRequest(await signed((await keypair()).privateKey, { node: "nd-aaaa1111" }), lookup);
  assert.deepEqual(unknown, bad);
  // Even signed with the key the check uses for unknown nodes, whose private half is public.
  const seed = Buffer.from(fixture.signing.seed, "hex");
  const pkcs8 = Buffer.concat([Buffer.from("302e020100300506032b657004220420", "hex"), seed]);
  const nobody = await crypto.subtle.importKey("pkcs8", pkcs8, { name: "Ed25519" }, false, ["sign"]);
  assert.deepEqual(await auth.verifyNodeRequest(await signed(nobody, { node: "nd-zzzz9999" }), lookup), bad);
});

test("a clock more than 60 s off is refused, and the answer says by how much", async () => {
  const k = await keypair();
  const lookup = async () => k.publicKey;
  const now = Date.now();
  for (const off of [-61, 61, 3600]) {
    const v = await auth.verifyNodeRequest(await signed(k.privateKey, { node: "nd-aaaa1111", ts: Math.floor(now / 1000) + off }), lookup, now);
    assert.ok("error" in v && v.status === 401 && v.error.includes(`${Math.abs(off)} s ${off > 0 ? "ahead" : "behind"}`), JSON.stringify(v));
  }
  for (const off of [-59, 59]) {
    const v = await auth.verifyNodeRequest(await signed(k.privateKey, { node: "nd-aaaa1111", ts: Math.floor(now / 1000) + off }), lookup, now);
    assert.ok("signed" in v);
  }
});

test("malformed headers are refused before any lookup", async () => {
  const k = await keypair();
  let lookups = 0;
  const lookup = async () => {
    lookups++;
    return k.publicKey;
  };
  for (const node of ["", "nd-AAAA1111", "nd-aaaa11112", "../x"]) {
    const v = await auth.verifyNodeRequest(await signed(k.privateKey, { node }), lookup);
    assert.ok("error" in v && v.status === 401);
  }
  const r = await signed(k.privateKey, { node: "nd-aaaa1111" });
  r.headers.set("x-cx-nonce", "not-hex");
  assert.ok("error" in (await auth.verifyNodeRequest(r, lookup)));
  assert.equal(lookups, 0);
});

test("a nonce works once while it can still be in the skew window", () => {
  const seen = new Map<string, number>();
  const store = {
    insert: (n: string, at: number) => (seen.has(n) ? false : (seen.set(n, at), true)),
    prune: (before: number) => {
      for (const [n, at] of seen) if (at < before) seen.delete(n);
    },
  };
  const t = 1_000_000;
  assert.equal(auth.rememberNonce(store, "a".repeat(32), t), true);
  assert.equal(auth.rememberNonce(store, "a".repeat(32), t + 1000), false);
  assert.equal(auth.rememberNonce(store, "a".repeat(32), t + 119_000), false);
  // Gone once a request carrying it is refused for its time anyway.
  assert.ok(auth.NONCE_TTL_MS >= 2 * auth.SKEW_SEC * 1000);
  assert.equal(auth.rememberNonce(store, "a".repeat(32), t + auth.NONCE_TTL_MS + 1), true);
});

// 2026-11-12 review, W6: a request passes the skew check for up to 121 s of
// the Worker's clock (from ts - 60 to the last millisecond of ts + 60); its
// nonce must be remembered for all of it.
test("a nonce is remembered for the whole time its request passes the clock check", async () => {
  const k = await keypair();
  const lookup = async () => k.publicKey;
  const ts = 2_000_000_000;
  const first = (ts - auth.SKEW_SEC) * 1000;
  const last = (ts + auth.SKEW_SEC + 1) * 1000 - 1;
  const r = await signed(k.privateKey, { node: "nd-aaaa1111", ts });
  assert.ok("signed" in (await auth.verifyNodeRequest(r, lookup, first)));
  assert.ok("signed" in (await auth.verifyNodeRequest(r, lookup, last)), "still in the window");
  const seen = new Map<string, number>();
  const store = {
    insert: (n: string, at: number) => (seen.has(n) ? false : (seen.set(n, at), true)),
    prune: (before: number) => {
      for (const [n, at] of seen) if (at < before) seen.delete(n);
    },
  };
  assert.equal(auth.rememberNonce(store, "b".repeat(32), first), true);
  assert.equal(auth.rememberNonce(store, "b".repeat(32), last), false, "a replay at the window's end is refused");
});

test("the node's id is signed: a signature made for one node doesn't pass as another's with the same key", async () => {
  const k = await keypair();
  const lookup = async () => k.publicKey; // two nodes that share a key, as if one was copied
  const forged = await signed(k.privateKey, { node: "nd-bbbb2222", signNode: "nd-aaaa1111" });
  const v = await auth.verifyNodeRequest(forged, lookup);
  assert.ok("error" in v && v.error === auth.BAD_SIGNATURE);
  assert.ok("signed" in (await auth.verifyNodeRequest(await signed(k.privateKey, { node: "nd-bbbb2222" }), lookup)));
});

test("a removed node's own agent hears that it was removed (410); anyone else gets the usual 401", async () => {
  const k = await keypair();
  const keys = async (id: string) => ({ key: null, revokedKey: id === "nd-aaaa1111" ? k.publicKey : null });
  const own = await auth.verifyNodeRequest(await signed(k.privateKey, { node: "nd-aaaa1111" }), keys);
  assert.ok("error" in own && own.status === 410 && own.revoked === true && /removed from the deployment/.test(own.error));
  const other = await auth.verifyNodeRequest(await signed((await keypair()).privateKey, { node: "nd-aaaa1111" }), keys);
  assert.deepEqual(other, { status: 401, error: auth.BAD_SIGNATURE });
  const unknown = await auth.verifyNodeRequest(await signed(k.privateKey, { node: "nd-zzzz9999" }), keys);
  assert.deepEqual(unknown, other);
});

test("a join must be signed with the key it brings", async () => {
  const k = await keypair();
  const r = await signed(k.privateKey, { method: "POST", url: "https://api.example/v1/node/join" });
  assert.equal(await auth.verifyJoinSignature(r, k.publicKey), null);
  const other = await keypair();
  assert.equal((await auth.verifyJoinSignature(r, other.publicKey))?.status, 401);
  const late = await signed(k.privateKey, { method: "POST", url: "https://api.example/v1/node/join", ts: Math.floor(Date.now() / 1000) - 120 });
  assert.match((await auth.verifyJoinSignature(late, k.publicKey))?.error ?? "", /clock/);
});

test("join tokens work once, for 30 minutes, and are stored hashed", async () => {
  const node = await nodes.addNode(DB, "box1");
  const t0 = Date.now();
  const { token, expiresAt } = await auth.issueJoinToken(DB, node.id, t0);
  assert.match(token, /^cxj_[a-z2-7]{40}$/);
  assert.equal(expiresAt, t0 + 30 * 60_000);
  const rows = (await DB.prepare("SELECT * FROM node_joins").all<{ token_hash: string }>()).results;
  assert.ok(rows.every((r) => !JSON.stringify(r).includes(token.slice(4))));
  // Expired: a minute past its 30.
  assert.equal(await auth.useJoinToken(DB, token, t0 + 31 * 60_000), null);
  assert.equal(await auth.useJoinToken(DB, token, t0 + 60_000), node.id);
  // Used.
  assert.equal(await auth.useJoinToken(DB, token, t0 + 120_000), null);
  // Made up, or malformed.
  assert.equal(await auth.useJoinToken(DB, `cxj_${"a".repeat(40)}`, t0), null);
  assert.equal(await auth.useJoinToken(DB, "cxj_' OR 1=1 --", t0), null);
});

test("two joins racing with one token: one wins", async () => {
  const node = await nodes.addNode(DB, "box2");
  const { token } = await auth.issueJoinToken(DB, node.id);
  const got = await Promise.all(Array.from({ length: 8 }, () => auth.useJoinToken(DB, token)));
  assert.deepEqual(
    got.filter((g) => g !== null),
    [node.id],
  );
});

test("a new token for a node replaces its old one", async () => {
  const node = await nodes.addNode(DB, "box3");
  const first = await auth.issueJoinToken(DB, node.id);
  const again = await nodes.addNode(DB, "box3");
  assert.equal(again.id, node.id, "a pending node keeps its id");
  const second = await auth.issueJoinToken(DB, node.id);
  assert.equal(await auth.useJoinToken(DB, first.token), null);
  assert.equal(await auth.useJoinToken(DB, second.token), node.id);
});

test("a node joins once; revoking it clears its key, and its name makes a new node", async () => {
  const k = await keypair();
  const node = await nodes.addNode(DB, "box4");
  const join = { token: "x", publicKey: k.publicKey, version: "v0.2.2", facts: FACTS };
  assert.equal(await nodes.nodePublicKey(DB, node.id), null);
  assert.deepEqual(await nodes.joinNode(DB, node.id, join), { id: node.id, name: "box4" });
  assert.equal(await nodes.joinNode(DB, node.id, { ...join, publicKey: (await keypair()).publicKey }), null, "joins once");
  assert.equal(await nodes.nodePublicKey(DB, node.id), k.publicKey);
  const joined = (await nodes.findNode(DB, "box4"))!;
  assert.equal(joined.state, "offline");
  assert.deepEqual(joined.isolation, FACTS.isolation);
  await assert.rejects(nodes.addNode(DB, "box4"), /\[409\]/);

  await nodes.mirrorNode(DB, node.id, { state: "online", lastSeenAt: 5, colo: "MIA", continent: "NA", locationHint: "enam" });
  assert.equal((await nodes.findNode(DB, node.id))!.state, "online");
  await nodes.revokeNode(DB, node.id);
  assert.equal(await nodes.nodePublicKey(DB, node.id), null);
  assert.deepEqual(await nodes.nodeKeys(DB, node.id), { key: null, revokedKey: k.publicKey }, "kept for telling its agent");
  // A Node object reporting late doesn't bring it back.
  await nodes.mirrorNode(DB, node.id, { state: "online", lastSeenAt: 6 });
  const revoked = (await nodes.findNode(DB, "box4"))!;
  assert.equal(revoked.state, "revoked");
  assert.ok(revoked.revokedAt);
  const fresh = await nodes.addNode(DB, "box4");
  assert.notEqual(fresh.id, node.id);
  assert.equal(fresh.state, "pending");
  // The revoked key outlives the forgotten node, and is no other node's.
  assert.deepEqual(await nodes.nodeKeys(DB, node.id), { key: null, revokedKey: k.publicKey });
  await assert.rejects(nodes.joinNode(DB, fresh.id, join), /\[409\] that agent's key belongs to another node/);
});

test("a join with a key another node has is refused", async () => {
  const k = await keypair();
  const a = await nodes.addNode(DB, "box7");
  const b = await nodes.addNode(DB, "box8");
  const join = { token: "x", publicKey: k.publicKey, version: "v0.2.2", facts: FACTS };
  assert.ok(await nodes.joinNode(DB, a.id, join));
  await assert.rejects(nodes.joinNode(DB, b.id, join), /\[409\]/);
  assert.equal((await nodes.findNode(DB, b.id))!.state, "pending");
});

test("a node counts the sandboxes on it", async () => {
  const { upsertSandbox } = await import("../src/db.ts");
  const node = await nodes.addNode(DB, "box5");
  const s = { id: "cx-aaa111", name: null, size: "standard-1", state: "running", idleMinutes: 30, createdAt: 1, updatedAt: 1, startedAt: 1, stoppedAt: null, savedAt: null, savedBytes: null, error: null } as const;
  await upsertSandbox(DB, { ...s, node: node.id });
  // A later write that doesn't name the node keeps it.
  await upsertSandbox(DB, { ...s, state: "stopped" });
  assert.equal((await nodes.findNode(DB, node.id))!.machines, 1);
});

test("facts are checked again before they reach D1", async () => {
  const node = await nodes.addNode(DB, "box6");
  await assert.rejects(nodes.mirrorNode(DB, node.id, { facts: { ...FACTS, cpus: -1 } }));
});

test("anyone may list nodes; only admins add or remove them", () => {
  assert.deepEqual(ruleFor("GET", "/v1/nodes/box"), { need: "read", notOwnOnly: false, ref: null });
  assert.equal(ruleFor("GET", "/v1/nodes").need, "read");
  assert.equal(ruleFor("POST", "/v1/nodes").need, "admin");
  assert.equal(ruleFor("DELETE", "/v1/nodes/box").need, "admin");
  // The agent's routes never reach the token rules: they come before the middleware.
  assert.equal(ruleFor("GET", "/v1/node/connect").need, "admin");
  assert.equal(NodeClose.revoked, 4003);
});

test("a body over the cap isn't read past it", async () => {
  const big = new Request("https://x/", { method: "POST", body: "x".repeat(20_000) });
  assert.equal(await auth.readCapped(big, 16_384), null);
  const said = new Request("https://x/", { method: "POST", body: "x", headers: { "content-length": "999999" } });
  assert.equal(await auth.readCapped(said, 16_384), null);
  const stream = new ReadableStream({
    pull(c) {
      c.enqueue(new Uint8Array(8192));
    },
  });
  const endless = new Request("https://x/", { method: "POST", body: stream, duplex: "half" } as RequestInit);
  assert.equal(await auth.readCapped(endless, 16_384), null);
  assert.equal(await auth.readCapped(new Request("https://x/", { method: "POST", body: "{}" }), 16_384), "{}");
});
