import assert from "node:assert/strict";
import { mkdtemp, writeFile } from "node:fs/promises";
import { createServer, type IncomingMessage } from "node:http";
import type { AddressInfo } from "node:net";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { after, before, beforeEach, test } from "node:test";
import { Cloudex, CloudexError, shellQuote } from "../src/index.ts";

const info = (id: string, extra: object = {}) => ({
  id,
  name: null,
  size: "standard-2",
  state: "running",
  idleMinutes: 30,
  createdAt: 1,
  updatedAt: 1,
  startedAt: 1,
  stoppedAt: null,
  savedAt: null,
  savedBytes: null,
  error: null,
  ...extra,
});

// A fake API: answers by "METHOD /path" and records what it was sent.
type Seen = { route: string; auth?: string; body?: unknown };
let seen: Seen[] = [];
let routes: Record<string, [number, unknown]> = {};
let url = "";
const server = createServer(async (req: IncomingMessage, res) => {
  let text = "";
  for await (const chunk of req) text += chunk;
  const route = `${req.method} ${req.url}`;
  seen.push({ route, auth: req.headers.authorization, body: text ? JSON.parse(text) : undefined });
  const [status, body] = routes[route] ?? [404, { error: `no route ${route}` }];
  res.writeHead(status, { "content-type": "application/json" }).end(JSON.stringify(body));
});

before(async () => {
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  url = `http://127.0.0.1:${(server.address() as AddressInfo).port}`;
});
after(() => server.close());
beforeEach(() => {
  seen = [];
  routes = {};
  delete process.env.CLOUDEX_API_URL;
  delete process.env.CLOUDEX_API_KEY;
  delete process.env.CLOUDEX_CONFIG_DIR;
});

test("create sends the options with the key and returns a sandbox", async () => {
  routes["POST /v1/sandboxes"] = [201, info("cx-aaa111", { name: "web" })];
  const box = await new Cloudex({ apiUrl: url + "/", apiKey: "k1" }).create({ name: "web", idleMinutes: 5 });
  assert.equal(box.id, "cx-aaa111");
  assert.equal(box.name, "web");
  assert.equal(box.state, "running");
  assert.deepEqual(seen, [{ route: "POST /v1/sandboxes", auth: "Bearer k1", body: { name: "web", idleMinutes: 5 } }]);
  assert.deepEqual(JSON.parse(JSON.stringify(box)), info("cx-aaa111", { name: "web" }));
});

test("API errors throw CloudexError with the status and message", async () => {
  routes["POST /v1/sandboxes/cx-aaa111/stop"] = [409, { error: "cx-aaa111 is starting" }];
  routes["GET /v1/sandboxes/cx-aaa111"] = [200, info("cx-aaa111")];
  const box = await new Cloudex({ apiUrl: url, apiKey: "k" }).get("cx-aaa111");
  await assert.rejects(box.stop(), (e: unknown) => {
    assert.ok(e instanceof CloudexError);
    assert.equal(e.status, 409);
    assert.equal(e.message, "cx-aaa111 is starting");
    return true;
  });
});

test("lifecycle calls update the sandbox's fields", async () => {
  routes["GET /v1/sandboxes/my%20box"] = [200, info("cx-aaa111", { name: "my box" })];
  routes["POST /v1/sandboxes/cx-aaa111/stop"] = [200, info("cx-aaa111", { state: "stopped", savedAt: 9 })];
  routes["POST /v1/sandboxes/cx-aaa111/resume"] = [200, info("cx-aaa111", { notice: "restored" })];
  routes["DELETE /v1/sandboxes/cx-aaa111"] = [200, { deleted: "cx-aaa111" }];
  const box = await new Cloudex({ apiUrl: url, apiKey: "k" }).get("my box");
  assert.equal((await box.stop()).state, "stopped");
  assert.equal(box.info.savedAt, 9);
  await box.resume({ fromBackup: true });
  assert.equal(box.info.notice, "restored");
  await box.delete();
  assert.deepEqual(seen.map((s) => s.route).slice(-3), [
    "POST /v1/sandboxes/cx-aaa111/stop",
    "POST /v1/sandboxes/cx-aaa111/resume",
    "DELETE /v1/sandboxes/cx-aaa111",
  ]);
  assert.deepEqual(seen.at(-2)?.body, { fromBackup: true });
});

test("exec quotes an argument list and passes options", async () => {
  routes["GET /v1/sandboxes/web"] = [200, info("cx-aaa111")];
  const result = { exitCode: 1, stdout: "", stderr: "no", timedOut: false, ms: 3 };
  routes["POST /v1/sandboxes/cx-aaa111/exec"] = [200, result];
  const box = await new Cloudex({ apiUrl: url, apiKey: "k" }).get("web");
  assert.deepEqual(await box.exec(["echo", "it's", "a b", "--x=1"], { root: true }), result);
  assert.deepEqual(seen.at(-1)?.body, { cmd: `echo 'it'\\''s' 'a b' --x=1`, root: true });
  await box.exec("ls ~ | wc -l");
  assert.deepEqual(seen.at(-1)?.body, { cmd: "ls ~ | wc -l" });
});

test("fork returns working sandboxes", async () => {
  routes["GET /v1/sandboxes/web"] = [200, info("cx-aaa111")];
  routes["POST /v1/sandboxes/cx-aaa111/fork"] = [
    201,
    [info("cx-fff111", { name: "a-1", forkedFrom: "cx-aaa111" }), info("cx-fff222", { name: "a-2", state: "error", error: "no room" })],
  ];
  routes["POST /v1/sandboxes/cx-fff111/stop"] = [200, info("cx-fff111", { state: "stopped" })];
  const box = await new Cloudex({ apiUrl: url, apiKey: "k" }).get("web");
  const forks = await box.fork({ name: "a", count: 2 });
  assert.deepEqual(seen.at(-1)?.body, { name: "a", count: 2 });
  assert.deepEqual(forks.map((f) => [f.name, f.state, f.info.forkedFrom ?? null]), [["a-1", "running", "cx-aaa111"], ["a-2", "error", null]]);
  assert.equal((await forks[0]!.stop()).state, "stopped");
  assert.equal(seen.at(-1)?.auth, "Bearer k");
});

test("desktop returns a link to the viewer", async () => {
  routes["GET /v1/sandboxes/web"] = [200, info("cx-aaa111")];
  routes["POST /v1/sandboxes/cx-aaa111/desktop"] = [
    200,
    { port: 6080, path: "vnc.html?autoconnect=1", size: "1920x1080", installed: true, started: true },
  ];
  routes["POST /v1/sandboxes/cx-aaa111/hosts"] = [200, { url: "https://6080-cx-aaa111-tok.example.com" }];
  const box = await new Cloudex({ apiUrl: url, apiKey: "k" }).get("web");
  const d = await box.desktop({ size: "1920x1080" });
  assert.equal(d.url, "https://6080-cx-aaa111-tok.example.com/vnc.html?autoconnect=1");
  assert.equal(d.installed, true);
  assert.deepEqual(seen.at(-2)?.body, { size: "1920x1080" });
  assert.deepEqual(seen.at(-1)?.body, { port: 6080 });
});

test("without options it uses the environment, then the CLI's saved deployment", async () => {
  routes["GET /v1/health"] = [200, { ok: true, version: "v0", previewZone: null }];
  process.env.CLOUDEX_API_URL = url;
  process.env.CLOUDEX_API_KEY = "from-env";
  await new Cloudex().health();
  assert.equal(seen.at(-1)?.auth, "Bearer from-env");

  delete process.env.CLOUDEX_API_URL;
  delete process.env.CLOUDEX_API_KEY;
  const dir = await mkdtemp(join(tmpdir(), "cloudex-sdk-"));
  await writeFile(join(dir, "config.json"), JSON.stringify({ apiUrl: url, apiKey: "from-config" }));
  process.env.CLOUDEX_CONFIG_DIR = dir;
  assert.equal((await new Cloudex().health()).version, "v0");
  assert.equal(seen.at(-1)?.auth, "Bearer from-config");
});

test("no deployment is a clear error", async () => {
  process.env.CLOUDEX_CONFIG_DIR = await mkdtemp(join(tmpdir(), "cloudex-sdk-"));
  await assert.rejects(new Cloudex().list(), (e: unknown) => {
    assert.ok(e instanceof CloudexError);
    assert.equal(e.status, 0);
    assert.match(e.message, /cloudex login/);
    return true;
  });
});

test("shellQuote", () => {
  assert.equal(shellQuote("plain/path-1.txt"), "plain/path-1.txt");
  assert.equal(shellQuote(""), "''");
  assert.equal(shellQuote("$HOME"), "'$HOME'");
});

test("provider keys: set, list, delete", async () => {
  routes["PUT /v1/keys/openai"] = [200, { provider: "openai", hint: "…abcd", updatedAt: 5 }];
  routes["GET /v1/keys"] = [200, [{ provider: "openai", hint: "…abcd", updatedAt: 5 }]];
  routes["DELETE /v1/keys/openai"] = [200, { deleted: "openai" }];
  const cx = new Cloudex({ apiUrl: url, apiKey: "k" });
  assert.deepEqual(await cx.setKey("openai", "sk-x-abcd"), { provider: "openai", hint: "…abcd", updatedAt: 5 });
  assert.deepEqual(seen.at(-1)?.body, { key: "sk-x-abcd" });
  assert.equal((await cx.listKeys())[0]?.hint, "…abcd");
  await cx.deleteKey("openai");
  assert.equal(seen.at(-1)?.route, "DELETE /v1/keys/openai");
});

const agentInfo = (extra: object = {}) => ({
  id: "ag-aaaa1111",
  agent: "claude",
  state: "running",
  cwd: "/home/cloudex",
  model: null,
  turns: 1,
  queued: 0,
  events: 1,
  sessionId: null,
  lastMessage: null,
  lastExitCode: null,
  error: null,
  createdAt: 1,
  updatedAt: 1,
  ...extra,
});

test("agents: prompt, follow the events to idle, send, interrupt", async () => {
  const path = "/v1/sandboxes/cx-aaa111/agents/ag-aaaa1111";
  const ev = (seq: number, type: string, extra: object = {}) => ({ seq, turn: 1, at: seq, type, ...extra });
  routes["GET /v1/sandboxes/web"] = [200, info("cx-aaa111")];
  routes["POST /v1/sandboxes/cx-aaa111/agents"] = [201, agentInfo({ turns: 0, events: 0 })];
  routes[`GET ${path}/events?after=0&wait=30`] = [
    200,
    { events: [ev(1, "prompt", { text: "fix it" }), ev(2, "event", { event: { type: "system" } })], next: 2, agent: agentInfo({ events: 3 }) },
  ];
  routes[`GET ${path}/events?after=2&wait=30`] = [
    200,
    { events: [ev(3, "end", { exitCode: 0 })], next: 3, agent: agentInfo({ state: "idle", events: 3, lastMessage: "fixed" }) },
  ];
  routes[`POST ${path}/messages`] = [200, agentInfo({ queued: 1 })];
  routes[`POST ${path}/interrupt`] = [200, agentInfo({ state: "idle", error: "interrupted" })];

  const box = await new Cloudex({ apiUrl: url, apiKey: "k" }).get("web");
  const agent = await box.prompt("fix it", { agent: "codex", cwd: "app" });
  assert.deepEqual(seen.at(-1)?.body, { prompt: "fix it", agent: "codex", cwd: "app" });
  const types: string[] = [];
  for await (const e of agent.events()) types.push(e.type);
  assert.deepEqual(types, ["prompt", "event", "end"]);
  assert.equal(agent.state, "idle");
  assert.equal(agent.lastMessage, "fixed");

  await agent.send("and the docs", { interrupt: true });
  assert.deepEqual(seen.at(-1)?.body, { prompt: "and the docs", interrupt: true });
  assert.equal(agent.info.queued, 1);
  await agent.interrupt();
  assert.equal(agent.error, "interrupted");
});

test("agents: wait reads only new events, and events without follow stop at the newest", async () => {
  const path = "/v1/sandboxes/cx-aaa111/agents/ag-aaaa1111";
  routes[`GET /v1/sandboxes/cx-aaa111/agents`] = [200, [agentInfo({ events: 7 })]];
  routes[`GET ${path}/events?after=7&wait=30`] = [200, { events: [], next: 7, agent: agentInfo({ state: "idle", events: 7, lastMessage: "done" }) }];
  routes[`GET ${path}/events?after=5&wait=0`] = [200, { events: [], next: 7, agent: agentInfo({ events: 7 }) }];
  routes["GET /v1/sandboxes/cx-aaa111"] = [200, info("cx-aaa111")];
  const [agent] = await (await new Cloudex({ apiUrl: url, apiKey: "k" }).get("cx-aaa111")).agents();
  await agent!.wait();
  assert.equal(agent!.lastMessage, "done");
  const quick = [];
  for await (const e of agent!.events({ after: 5, follow: false })) quick.push(e);
  assert.deepEqual(quick, []);
  assert.equal(seen.at(-1)?.route, `GET ${path}/events?after=5&wait=0`);
});
