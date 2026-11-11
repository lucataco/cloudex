// The route that takes a node's guest traffic (nodeegress.ts, v0.2.2 M6): a
// request binds to one sandbox on the signing node, always carries that
// sandbox's run token on to Egress, goes only to cloudex.internal or a
// provider, and the real key never comes back in an answer (redact.ts).
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import "./d1.ts";

const { nodeEgress, egressTarget, guestHeaders, cachedKey } = await import("../src/nodeegress.ts");
const { signingString, BAD_SIGNATURE } = await import("../src/nodeauth.ts");
const { withoutKey, ERROR_BODY_MAX } = await import("../src/redact.ts");
const { HELPER } = await import("../src/backup.ts");
const { EGRESS_BODY_MAX } = await import("@cloudex/schema/node");

const fixture = JSON.parse(readFileSync(new URL("../../../packages/schema/fixtures/node/protocol.json", import.meta.url), "utf8"));

const b64 = (b: ArrayBuffer) => Buffer.from(new Uint8Array(b)).toString("base64");
const hex = (n: number) => [...crypto.getRandomValues(new Uint8Array(n))].map((b) => b.toString(16).padStart(2, "0")).join("");

async function keypair() {
  const k = (await crypto.subtle.generateKey({ name: "Ed25519" }, true, ["sign", "verify"])) as CryptoKeyPair;
  return { key: k.privateKey, pub: b64(await crypto.subtle.exportKey("raw", k.publicKey)) };
}

const NODE = "nd-aaaaaaaa";
const OTHER = "nd-bbbbbbbb";

// A request as the agent sends it (go/internal/node/egress.go).
async function fromAgent(key: CryptoKey, o: { node?: string; sandbox?: string; target: string; method?: string; body?: string; headers?: Record<string, string>; signTarget?: string }) {
  const url = new URL(`https://api.example/v1/node/egress?target=${encodeURIComponent(o.target)}`);
  const signedUrl = new URL(`https://api.example/v1/node/egress?target=${encodeURIComponent(o.signTarget ?? o.target)}`);
  const method = o.method ?? "POST";
  const ts = String(Math.floor(Date.now() / 1000));
  const nonce = hex(16);
  const sig = b64(await crypto.subtle.sign("Ed25519", key, new TextEncoder().encode(signingString(method, signedUrl.pathname + signedUrl.search, o.sandbox ?? "", ts, nonce))));
  const headers: Record<string, string> = { "x-cx-node": o.node ?? NODE, "x-cx-ts": ts, "x-cx-nonce": nonce, "x-cx-sig": sig, ...(o.headers ?? {}) };
  if (o.sandbox) headers["x-cx-sandbox"] = o.sandbox;
  return new Request(url, { method, headers, body: o.body ?? null });
}

type Info = { node: string | null; selfToken: string | null; state: string; revoked: boolean };

function deps(pub: string, sandboxes: Record<string, Info>) {
  const calls: { props: { id: string; selfToken: string; node: string }; url: string; method: string; headers: Headers; body: string }[] = [];
  return {
    calls,
    d: {
      publicKeyOf: async (n: string) => (n === NODE ? pub : null),
      info: async (s: string) => sandboxes[s] ?? null,
      egress: (props: { id: string; selfToken: string; node: string }) => ({
        fetch: async (req: Request) => {
          calls.push({ props, url: req.url, method: req.method, headers: req.headers, body: await req.text() });
          return new Response("egress answered");
        },
      }),
    },
  };
}

const running = (node: string | null, selfToken: string | null = "tok-1"): Info => ({ node, selfToken, state: "running", revoked: false });

test("a request goes to Egress for the sandbox it was signed for, with that run's token and the node", async () => {
  const { key, pub } = await keypair();
  const { d, calls } = deps(pub, { "cx-aaa111": running(NODE) });
  const req = await fromAgent(key, {
    sandbox: "cx-aaa111",
    target: "http://cloudex.internal/v1/sandboxes/self",
    method: "PATCH",
    body: '{"idleMinutes":5}',
    headers: { "x-cloudex-self": "tok-1", "cf-connecting-ip": "203.0.113.9", "x-forwarded-for": "203.0.113.9" },
  });
  const res = await nodeEgress(req, d);
  assert.equal(res.status, 200);
  assert.equal(calls.length, 1);
  assert.deepEqual(calls[0]!.props, { id: "cx-aaa111", selfToken: "tok-1", node: NODE });
  assert.equal(calls[0]!.url, "http://cloudex.internal/v1/sandboxes/self");
  assert.equal(calls[0]!.method, "PATCH");
  assert.equal(calls[0]!.body, '{"idleMinutes":5}');
  assert.equal(calls[0]!.headers.get("x-cloudex-self"), "tok-1");
  // The node's own headers and the edge's don't go on.
  for (const h of ["x-cx-node", "x-cx-sig", "x-cx-sandbox", "cf-connecting-ip", "x-forwarded-for"]) assert.equal(calls[0]!.headers.get(h), null, h);
});

test("a node can't act for a sandbox on another node, on Cloudflare, or none", async () => {
  const { key, pub } = await keypair();
  const { d, calls } = deps(pub, { "cx-bbb222": running(OTHER), "cx-ccc333": running(null) });
  for (const sandbox of ["cx-bbb222", "cx-ccc333", "cx-ddd444"]) {
    const res = await nodeEgress(await fromAgent(key, { sandbox, target: "http://cloudex.internal/activity" }), d);
    assert.equal(res.status, 403, sandbox);
  }
  // Nor with no sandbox named, and the sandbox is signed: changing it breaks the signature.
  assert.equal((await nodeEgress(await fromAgent(key, { target: "http://cloudex.internal/activity" }), d)).status, 400);
  const req = await fromAgent(key, { sandbox: "cx-bbb222", target: "http://cloudex.internal/activity" });
  const forged = new Request(req, { headers: new Headers(req.headers) });
  forged.headers.set("x-cx-sandbox", "cx-ccc333");
  assert.equal((await nodeEgress(forged, d)).status, 401);
  assert.equal(calls.length, 0);
});

test("the target is signed, and is only ever cloudex.internal or a provider's host", async () => {
  const { key, pub } = await keypair();
  const { d, calls } = deps(pub, { "cx-aaa111": running(NODE) });
  // Signed for one target, sent with another.
  const swapped = await fromAgent(key, { sandbox: "cx-aaa111", target: "https://api.openai.com/v1/models", signTarget: "http://cloudex.internal/activity" });
  assert.equal((await nodeEgress(swapped, d)).status, 401);
  for (const t of fixture.egress.targets.invalid as string[]) {
    const res = await nodeEgress(await fromAgent(key, { sandbox: "cx-aaa111", target: t }), d);
    assert.equal(res.status, 400, t);
  }
  for (const t of fixture.egress.targets.valid as string[]) assert.ok(egressTarget(t), t);
  assert.equal(calls.length, 0);
});

test("a sandbox that isn't running, a removed node, and a run without a token are refused; the token is never skipped", async () => {
  const { key, pub } = await keypair();
  const { d, calls } = deps(pub, {
    "cx-sto111": { node: NODE, selfToken: "t", state: "stopped", revoked: false },
    "cx-rev111": { node: NODE, selfToken: "t", state: "running", revoked: true },
    "cx-tok111": running(NODE, null),
    "cx-emp111": running(NODE, ""),
    "cx-sta111": { node: NODE, selfToken: "t", state: "starting", revoked: false },
  });
  const status = async (sandbox: string) => (await nodeEgress(await fromAgent(key, { sandbox, target: "http://cloudex.internal/v1/sandboxes/self", method: "GET" }), d)).status;
  assert.equal(await status("cx-sto111"), 409);
  assert.equal(await status("cx-rev111"), 401);
  assert.equal(await status("cx-tok111"), 403);
  assert.equal(await status("cx-emp111"), 403);
  // A start in progress restores saves through here.
  assert.equal(await status("cx-sta111"), 200);
  assert.equal(calls.length, 1);
  assert.ok(calls.every((c) => c.props.selfToken.length > 0));
});

test("an unknown node and a bad signature look the same", async () => {
  const { key, pub } = await keypair();
  const { d } = deps(pub, { "cx-aaa111": running(NODE) });
  const unknown = await nodeEgress(await fromAgent(key, { node: "nd-zzzzzzzz", sandbox: "cx-aaa111", target: "http://cloudex.internal/activity" }), d);
  const other = await keypair();
  const badSig = await nodeEgress(await fromAgent(other.key, { sandbox: "cx-aaa111", target: "http://cloudex.internal/activity" }), d);
  assert.equal(unknown.status, 401);
  assert.equal(badSig.status, 401);
  assert.equal((await unknown.json()).message, BAD_SIGNATURE);
  assert.equal((await badSig.json()).message, BAD_SIGNATURE);
});

test("guest headers drop the node's and the edge's, and get the machine's own User-Agent back", () => {
  const h = guestHeaders(new Headers({ "x-cx-sandbox": "cx-x", "cf-ray": "1", "content-type": "text/plain", authorization: "Bearer p", "user-agent": "cloudex-node/v1", "x-cx-guest-ua": "Python-urllib/3.12" }));
  assert.deepEqual([...h.keys()].sort(), ["authorization", "content-type", "user-agent"]);
  assert.equal(h.get("user-agent"), "Python-urllib/3.12");
  assert.equal(guestHeaders(new Headers({ "user-agent": "cloudex-node/v1" })).get("user-agent"), null);
});

test("the node's key is looked up once a minute, and an unknown node every time", async () => {
  let reads = 0;
  const look = async (n: string) => {
    reads++;
    return n === NODE ? "k" : null;
  };
  const t0 = Date.now();
  await cachedKey(look, t0)(NODE);
  await cachedKey(look, t0 + 1000)(NODE);
  assert.equal(reads, 1);
  await cachedKey(look, t0 + 61_000)(NODE);
  assert.equal(reads, 2);
  await cachedKey(look, t0)(OTHER);
  await cachedKey(look, t0)(OTHER);
  assert.equal(reads, 4);
});

test("the real key never comes back in a provider's refusal, its headers or its body", async () => {
  const key = "sk-proj-REALKEYabcdefghijklmnopqrstuvwxyz0123456789XYZ";
  const echo = new Response(JSON.stringify({ error: { message: `Incorrect API key provided: ${key}. Also ${key.slice(8, -4)}.` } }), {
    status: 400,
    headers: { "content-type": "application/json", "x-echo": `Bearer ${key}` },
  });
  const out = await withoutKey(echo, key);
  const text = await out.text();
  assert.equal(out.status, 400);
  assert.ok(!text.includes(key) && !text.includes(key.slice(8, -4)), text);
  assert.ok(!(out.headers.get("x-echo") ?? "").includes(key.slice(8, -4)));
  // Too big to read whole: replaced.
  const big = await withoutKey(new Response(`${"x".repeat(ERROR_BODY_MAX)}${key}`, { status: 500 }), key);
  assert.ok(!(await big.text()).includes(key));
});

test("save and backup parts fit under the limit the agent and the edge apply", () => {
  const part = /PART, JOBS = (\d+) << (\d+)/.exec(HELPER);
  assert.ok(part);
  const bytes = Number(part[1]) * 2 ** Number(part[2]);
  assert.equal(bytes, fixture.egress.savePartMax);
  assert.ok(bytes <= EGRESS_BODY_MAX);
});
