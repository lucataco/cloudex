// Node streams (nodestream.ts): stream tickets, and a framed stream's credit,
// output and exit, against a stand-in for the agent's socket.
import assert from "node:assert/strict";
import test from "node:test";
import { STREAM_WINDOW } from "@cloudex/schema/node";
import { Framed, NodeStreams, OVERRUN_SLACK, SPLICE_AHEAD_MAX, SPLICE_CLIENT_CREDIT, SPLICE_EARLY_MAX, STREAMS_MAX, STREAMS_WAIT, TICKET_TTL_SEC, WS_RELAY_RATE, issueTicket, readTicket, sendable } from "../src/nodestream.ts";

const KEY = "admin-key-for-tests";

test("a ticket reads back as the node, sandbox and stream it was issued for, until it expires", async () => {
  const now = Date.UTC(2026, 10, 11);
  const t = await issueTicket(KEY, "nd-abcd1234", "cx-abc123", now);
  const r = await readTicket(KEY, t, now + 1000);
  assert.equal(r?.node, "nd-abcd1234");
  assert.equal(r?.sandbox, "cx-abc123");
  assert.match(r?.stream ?? "", /^[0-9a-f]{16}$/);
  assert.equal(r?.exp, Math.floor(now / 1000) + TICKET_TTL_SEC);
  assert.equal(await readTicket(KEY, t, now + TICKET_TTL_SEC * 1000), null, "expired");
  // Every ticket has a stream id of its own.
  assert.notEqual((await readTicket(KEY, await issueTicket(KEY, "nd-abcd1234", "cx-abc123", now), now))?.stream, r?.stream);
});

test("a ticket that was changed, or made with another key, reads as nothing", async () => {
  const now = Date.now();
  const t = await issueTicket(KEY, "nd-abcd1234", "cx-abc123", now);
  const [payload, mac] = t.split(".") as [string, string];
  // Another sandbox in the payload, with the old MAC.
  const forged = btoa(JSON.stringify({ n: "nd-abcd1234", s: "cx-zzz999", i: "0123456789abcdef", e: Math.floor(now / 1000) + 60 }))
    .replace(/\+/g, "-")
    .replace(/\//g, "_")
    .replace(/=+$/, "");
  assert.equal(await readTicket(KEY, `${forged}.${mac}`, now), null);
  assert.equal(await readTicket(KEY, `${payload}.${mac.slice(0, -2)}AA`, now), null);
  assert.equal(await readTicket("another-admin-key", t, now), null);
  assert.equal(await readTicket(KEY, `${t}.x`, now), null);
  assert.equal(await readTicket(KEY, "x".repeat(600), now), null);
  assert.equal(await readTicket(KEY, "", now), null);
});

test("close codes the runtime can't send are mapped", () => {
  assert.equal(sendable(1006), 4404);
  assert.equal(sendable(1005), 1000);
  assert.equal(sendable(1015), 1000);
  assert.equal(sendable(1001), 1000);
  assert.equal(sendable(4008), 4008);
  assert.equal(sendable(1000), 1000);
});

// A stand-in for the agent's half of a stream socket.
class FakeSocket {
  sent: (string | Uint8Array)[] = [];
  closed: { code: number; reason: string } | null = null;
  send(m: string | Uint8Array) {
    if (this.closed) throw new Error("closed");
    this.sent.push(m);
  }
  close(code: number, reason: string) {
    this.closed = { code, reason };
  }
  controls() {
    return this.sent.filter((m): m is string => typeof m === "string").map((m) => JSON.parse(m));
  }
  data() {
    return this.sent.filter((m): m is Uint8Array => typeof m !== "string");
  }
}

const frame = (ch: number, text: string) => {
  const b = new TextEncoder().encode(text);
  const f = new Uint8Array(b.length + 1);
  f[0] = ch;
  f.set(b, 1);
  return f;
};

function stream() {
  const ws = new FakeSocket();
  let forgotten = false;
  const f = new Framed("0123456789abcdef", ws as unknown as WebSocket, () => (forgotten = true));
  return { f, ws, forgotten: () => forgotten };
}

test("output arrives on its channel, the exit after it, and the stream closes once both are read", async () => {
  const { f, ws, forgotten } = stream();
  f.onText('{"ok":true}');
  f.onBinary(frame(1, "hello "));
  f.onBinary(frame(2, "oops"));
  f.onBinary(frame(1, "world"));
  f.onText('{"eof":1}');
  f.onText('{"eof":2}');
  f.onText('{"exit":3}');
  assert.equal(await f.exited, 3);
  assert.equal(ws.closed, null, "not before its output is read");
  const [out, err] = await Promise.all([new Response(f.inbox[1]!.readable).text(), new Response(f.inbox[2]!.readable).text()]);
  assert.equal(out, "hello world");
  assert.equal(err, "oops");
  await new Promise((r) => setTimeout(r, 0));
  assert.equal(ws.closed?.code, 1000);
  assert.ok(forgotten());
});

test("a reader that cancels after the exit (exec's grace period) closes the stream", async () => {
  const { f, ws } = stream();
  f.onBinary(frame(1, "a"));
  f.onText('{"exit":0}');
  const reader = f.inbox[1]!.readable.getReader();
  assert.equal(new TextDecoder().decode((await reader.read()).value), "a");
  await reader.cancel();
  await f.inbox[2]!.readable.cancel();
  await new Promise((r) => setTimeout(r, 0));
  assert.equal(ws.closed?.code, 1000);
});

test("output is credited back as it's read, a quarter window at a time", async () => {
  const { f, ws } = stream();
  const chunk = "x".repeat(256 * 1024);
  for (let i = 0; i < 8; i++) f.onBinary(frame(1, chunk));
  assert.deepEqual(ws.controls(), [], "nothing is credited before it's read");
  const reader = f.inbox[1]!.readable.getReader();
  for (let i = 0; i < 4; i++) await reader.read();
  assert.deepEqual(ws.controls(), [{ credit: { ch: 1, n: STREAM_WINDOW / 4 } }]);
});

test("stdin waits for the agent's credit, and eof goes when it's closed", async () => {
  const { f, ws } = stream();
  const w = f.writable(0).getWriter();
  const big = new Uint8Array(STREAM_WINDOW + 100);
  let written = false;
  const p = w.write(big).then(() => (written = true));
  await new Promise((r) => setTimeout(r, 10));
  const sent = ws.data().reduce((n, b) => n + b.byteLength - 1, 0);
  assert.equal(sent, STREAM_WINDOW, "a window's worth, then it waits");
  assert.equal(written, false);
  assert.ok(ws.data().every((b) => b[0] === 0 && b.byteLength <= 256 * 1024 + 1));
  f.onText('{"credit":{"ch":0,"n":65536}}');
  await p;
  assert.equal(ws.data().reduce((n, b) => n + b.byteLength - 1, 0), STREAM_WINDOW + 100);
  await w.close();
  assert.deepEqual(ws.controls().at(-1), { eof: 0 });
});

test("a stream that drops before the exit fails what's waiting, with the agent's reason if it gave one", async () => {
  const { f } = stream();
  f.onBinary(frame(1, "partial"));
  f.gone(1006, "");
  await assert.rejects(f.exited, /closed before the command exited \(1006/);
  const r = f.inbox[1]!.readable.getReader();
  assert.equal(new TextDecoder().decode((await r.read()).value), "partial");
  await assert.rejects(r.read(), /closed/);

  const g = stream();
  g.f.onText('{"error":"no such container"}');
  await assert.rejects(g.f.exited, /\[502\] the node couldn't start it: no such container/);
  assert.equal(g.ws.closed?.code, 1000);
});

test("a control message outside the protocol closes the stream", () => {
  const { f, ws } = stream();
  f.onText('{"exit":256}');
  assert.equal(ws.closed?.code, 4400);
  const g = stream();
  g.f.onText("not json");
  assert.equal(g.ws.closed?.code, 4400);
});

test("signals and resizes go to the agent as control messages", () => {
  const { f, ws } = stream();
  f.control({ signal: 15 });
  f.control({ resize: { cols: 100, rows: 30 } });
  assert.deepEqual(ws.controls(), [{ signal: 15 }, { resize: { cols: 100, rows: 30 } }]);
});

test("a reader that cancels stdout before the exit ends the stream, so the agent hangs the process up", async () => {
  const { f, ws } = stream();
  f.onBinary(frame(1, "part of a download"));
  await f.inbox[1]!.readable.cancel();
  await new Promise((r) => setTimeout(r, 0));
  assert.equal(ws.closed?.code, 1000);
  await assert.rejects(f.exited);
});

// 2026-11-12 review, W2: a node can't make the object buffer without limit.
test("a node that sends past its window, or on a channel nobody reads, has its stream closed", () => {
  const { f, ws } = stream();
  const chunk = "x".repeat(256 * 1024);
  for (let i = 0; i < STREAM_WINDOW / chunk.length; i++) f.onBinary(frame(1, chunk));
  assert.equal(ws.closed, null, "a full window is fine");
  f.onBinary(frame(1, "y".repeat(OVERRUN_SLACK + 1)));
  assert.equal(ws.closed?.code, 4400);
  assert.match(ws.closed?.reason ?? "", /past its credit/);

  // An exec without stderr: frames on 2 (and 0, 3, 9) are refused.
  for (const ch of [2, 0, 3, 9]) {
    const g = stream();
    g.f.control({ exec: { argv: ["true"], stdin: false, stderr: false } });
    g.f.onBinary(frame(1, "ok"));
    assert.equal(g.ws.closed, null);
    g.f.onBinary(frame(ch, "nobody reads this"));
    assert.equal(g.ws.closed?.code, 4400, `channel ${ch}`);
  }
  // A TCP connection uses stdout only; HTTP stdout, and the WebSocket's text
  // once the answer is an upgrade (review W2).
  const t = stream();
  t.f.control({ tcp: { port: 80, splice: false } });
  t.f.onBinary(frame(4, "x"));
  assert.equal(t.ws.closed?.code, 4400);
  const h = stream();
  h.f.control({ http: { port: 80, method: "GET", path: "/", headers: [], body: false } });
  h.f.onText('{"status":{"code":101,"headers":[]}}');
  h.f.onBinary(frame(4, "x"));
  h.f.onBinary(frame(1, "x"));
  assert.equal(h.ws.closed, null);
  h.f.onBinary(new Uint8Array(0));
  assert.equal(h.ws.closed?.code, 4400, "an empty frame has no channel");
});

test("an HTTP answer that isn't a WebSocket has its stream closed for a frame on the WebSocket's text channel", () => {
  for (const answer of [null, '{"status":{"code":200,"headers":[]}}']) {
    const h = stream();
    h.f.control({ http: { port: 80, method: "GET", path: "/", headers: [], body: false } });
    if (answer) h.f.onText(answer);
    h.f.onBinary(frame(1, "the body"));
    assert.equal(h.ws.closed, null);
    h.f.onBinary(frame(4, "nobody reads this"));
    assert.equal(h.ws.closed?.code, 4400, `after ${answer ?? "no answer"}`);
    assert.match(h.ws.closed?.reason ?? "", /channel 4/);
  }
});

test("a preview WebSocket's messages are capped in size, and their credit is paced", async () => {
  const { f, ws } = stream();
  f.control({ http: { port: 80, method: "GET", path: "/", headers: [], body: false } });
  f.onText('{"status":{"code":101,"headers":[]}}');
  const got: number[] = [];
  f.onWsMessage = (d) => got.push(typeof d === "string" ? d.length : d.byteLength);
  const mib = "z".repeat(1 << 20);
  for (let i = 0; i < 10; i++) f.onBinary(frame(4, mib));
  assert.equal(got.length, 10);
  const credited = () => ws.controls().filter((m) => m.credit).reduce((n, m) => n + m.credit.n, 0);
  assert.equal(credited(), WS_RELAY_RATE, "a second's worth of credit at once");
  await new Promise((r) => setTimeout(r, 1100));
  assert.equal(credited(), 10 << 20, "the rest a second later");
  f.onBinary(frame(4, mib + "!"));
  assert.equal(ws.closed?.code, 4400);
});

// A stand-in for the Durable Object's WebSocket bookkeeping, for splices.
function spliceHost() {
  const sockets: { ws: FakeSocket & { readyState: number; deserializeAttachment(): unknown }; tag: string }[] = [];
  const add = (tag: string, role?: string) => {
    let att: unknown = role ? { role, at: Date.now() } : { tokenId: null, lastAt: Date.now() };
    const ws = Object.assign(new FakeSocket(), { readyState: 1, deserializeAttachment: () => att, serializeAttachment: (a: unknown) => void (att = a) });
    sockets.push({ ws, tag });
    return ws;
  };
  const ctx = {
    getWebSockets: (tag?: string) => sockets.filter((s) => s.ws.closed === null && (!tag || s.tag === tag)).map((s) => s.ws),
    getTags: (ws: unknown) => [sockets.find((s) => s.ws === ws)!.tag],
    getWebSocketAutoResponseTimestamp: () => new Date(),
  };
  const ns = new NodeStreams({ ctx, env: {}, sandbox: () => "cx-abc123", node: () => "nd-abcd1234", more: async () => {}, touch: () => {} } as never);
  return { ns, add };
}

test("a splice's node can't run more than SPLICE_AHEAD_MAX ahead of its client, or send much before it's open", () => {
  const { ns, add } = spliceHost();
  const node = add("n:aaaa", "splice");
  const client = add("c:aaaa");
  const mib = new Uint8Array(1 << 20).buffer;
  for (let i = 0; i < SPLICE_AHEAD_MAX >> 20; i++) ns.onMessage(node as never, mib);
  assert.equal(node.closed, null);
  // Each message from the client (a window adjust, or a keystroke) lets it
  // run SPLICE_CLIENT_CREDIT further, not SPLICE_AHEAD_MAX (review W2).
  ns.onMessage(client as never, new Uint8Array(10).buffer);
  for (let i = 0; i < SPLICE_CLIENT_CREDIT >> 20; i++) ns.onMessage(node as never, mib);
  assert.equal(node.closed, null, "a client message's worth more");
  ns.onMessage(node as never, mib);
  assert.equal(node.closed?.code, 4400);
  assert.equal(client.closed?.code, 4400);

  // An honest node, whose client answers each window: never closed.
  const h = spliceHost();
  const hn = h.add("n:hhhh", "splice");
  const hc = h.add("c:hhhh");
  for (let i = 0; i < 200; i++) {
    h.ns.onMessage(hn as never, new Uint8Array(2 << 20).buffer);
    h.ns.onMessage(hc as never, new Uint8Array(40).buffer);
  }
  assert.equal(hn.closed, null);

  const b = spliceHost();
  const early = b.add("n:bbbb", "splice");
  (b.ns as unknown as { early: Map<string, ArrayBuffer[]> }).early.set("bbbb", []);
  b.ns.onMessage(early as never, new Uint8Array(SPLICE_EARLY_MAX).buffer);
  assert.equal(early.closed, null);
  b.ns.onMessage(early as never, new Uint8Array(1).buffer);
  assert.equal(early.closed?.code, 4400);
});

test("a sandbox's object holds at most STREAMS_MAX open streams to its node; more wait, then are refused", async () => {
  const wait = STREAMS_WAIT.ms;
  STREAMS_WAIT.ms = 200;
  const { ns, add } = spliceHost();
  for (let i = 0; i < STREAMS_MAX; i++) {
    const id = i.toString(16).padStart(4, "0");
    add(`n:${id}`, "splice");
    add(`c:${id}`);
  }
  const spare = add("n:ffff", "spare");
  try {
    const t0 = Date.now();
    await assert.rejects(ns.open({ exec: { argv: ["true"], stdin: false, stderr: false } }), /\[429\] this sandbox has 64 streams/);
    assert.ok(Date.now() - t0 >= 200, "it waited for one to end first");
    await assert.rejects(ns.splice(22, null), /\[429\]/);
    assert.deepEqual(spare.sent, [], "the spare is left alone");
  } finally {
    STREAMS_WAIT.ms = wait;
  }
});
