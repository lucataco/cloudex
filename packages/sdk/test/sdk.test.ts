import assert from "node:assert/strict";
import { mkdtemp, writeFile } from "node:fs/promises";
import { createServer, type IncomingMessage, type ServerResponse } from "node:http";
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

// A fake API: answers by "METHOD /path" and records what it was sent. A body
// that isn't JSON is kept as bytes, with its type. A route can also be a
// function that answers by itself.
type Seen = { route: string; auth?: string; body?: unknown; type?: string };
let seen: Seen[] = [];
let routes: Record<string, [number, unknown] | ((res: ServerResponse) => void)> = {};
let url = "";
const server = createServer(async (req: IncomingMessage, res) => {
  const chunks: Buffer[] = [];
  for await (const chunk of req) chunks.push(chunk as Buffer);
  const raw = Buffer.concat(chunks);
  const route = `${req.method} ${req.url}`;
  const type = req.headers["content-type"];
  if (!raw.length) seen.push({ route, auth: req.headers.authorization, body: undefined });
  else if (type === "application/json") seen.push({ route, auth: req.headers.authorization, body: JSON.parse(raw.toString()) });
  else seen.push({ route, auth: req.headers.authorization, body: new Uint8Array(raw), type });
  const answer = routes[route] ?? [404, { error: `no route ${route}` }];
  if (typeof answer === "function") return answer(res);
  const [status, body] = answer;
  if (body instanceof Uint8Array) res.writeHead(status, { "content-type": "application/octet-stream" }).end(body);
  else res.writeHead(status, { "content-type": "application/json" }).end(JSON.stringify(body));
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
  assert.deepEqual(seen.at(-1)?.body, { cmd: `echo 'it'\\''s' 'a b' --x=1`, root: true, stream: true });
  await box.exec("ls ~ | wc -l");
  assert.deepEqual(seen.at(-1)?.body, { cmd: "ls ~ | wc -l", stream: true });
});

const ndjson = (lines: object[]) => (res: ServerResponse) =>
  res.writeHead(200, { "content-type": "application/x-ndjson" }).end(lines.map((l) => JSON.stringify(l) + "\n").join(""));
const MiB = 1024 * 1024;

test("exec collects the API's stream into one result", async () => {
  routes["GET /v1/sandboxes/web"] = [200, info("cx-aaa111")];
  routes["POST /v1/sandboxes/cx-aaa111/exec"] = ndjson([
    { type: "stdout", data: "hi\n" },
    { type: "keepalive" },
    { type: "stderr", data: "err\n" },
    { type: "stdout", data: "there\n" },
    { type: "exit", exitCode: 3, timedOut: false, ms: 9 },
  ]);
  const box = await new Cloudex({ apiUrl: url, apiKey: "k" }).get("web");
  assert.deepEqual(await box.exec("echo hi"), { exitCode: 3, stdout: "hi\nthere\n", stderr: "err\n", timedOut: false, ms: 9 });
  assert.deepEqual(seen.at(-1)?.body, { cmd: "echo hi", stream: true });
});

test("exec cuts each stream at 1 MiB with the API's note, and throws if the stream breaks off", async () => {
  routes["GET /v1/sandboxes/web"] = [200, info("cx-aaa111")];
  const chunk = { type: "stdout", data: "a".repeat(400_000) };
  routes["POST /v1/sandboxes/cx-aaa111/exec"] = ndjson([
    chunk,
    chunk,
    chunk,
    { type: "stderr", data: "a" + "é".repeat(MiB / 2) },
    { type: "exit", exitCode: 0, timedOut: false, ms: 1 },
  ]);
  const box = await new Cloudex({ apiUrl: url, apiKey: "k" }).get("web");
  const r = await box.exec("yes");
  assert.equal(r.stdout, `${"a".repeat(MiB)}\n[cloudex: output cut at 1 MiB of 1200000 bytes]`);
  // The cut falls between the two bytes of the last é.
  assert.equal(r.stderr, `a${"é".repeat(MiB / 2 - 1)}\uFFFD\n[cloudex: output cut at 1 MiB of ${MiB + 1} bytes]`);

  routes["POST /v1/sandboxes/cx-aaa111/exec"] = ndjson([{ type: "stdout", data: "a" }]);
  await assert.rejects(box.exec("sleep 1"), (e: unknown) => e instanceof CloudexError && e.status === 0);
});

test("exec on an API without streaming returns its answer as it is", async () => {
  routes["GET /v1/sandboxes/web"] = [200, info("cx-aaa111")];
  const stdout = `${"a".repeat(MiB)}\n[cloudex: output cut at 1 MiB of 3000000 bytes]`;
  const result = { exitCode: 0, stdout, stderr: "", timedOut: false, ms: 4 };
  routes["POST /v1/sandboxes/cx-aaa111/exec"] = [200, result];
  const box = await new Cloudex({ apiUrl: url, apiKey: "k" }).get("web");
  assert.deepEqual(await box.exec("yes"), result);
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

test("named saves: save, list, start from and delete", async () => {
  const save = {
    name: "base",
    id: "sv-aaaaaaaaaa",
    sandbox: "cx-aaa111",
    kind: "disk",
    size: "standard-2",
    idleMinutes: 30,
    files: 12,
    rawBytes: 2048,
    bytes: 1024,
    createdAt: 1,
    ms: 900,
  };
  routes["GET /v1/sandboxes/web"] = [200, info("cx-aaa111")];
  routes["POST /v1/sandboxes/cx-aaa111/saves"] = [201, { ...save, replaced: false }];
  routes["GET /v1/saves"] = [200, [save]];
  routes["POST /v1/saves/base/sandboxes"] = [201, [info("cx-bbb111", { name: "b", fromSave: "base" })]];
  routes["DELETE /v1/saves/base"] = [200, { deleted: "base" }];
  const cx = new Cloudex({ apiUrl: url, apiKey: "k" });
  const box = await cx.get("web");
  assert.equal((await box.save("base")).replaced, false);
  assert.deepEqual(seen.at(-1)?.body, { name: "base" });
  assert.deepEqual(await cx.listSaves(), [save]);
  const [copy] = await cx.startFromSave("base", { name: "b" });
  assert.deepEqual(seen.at(-1)?.body, { name: "b" });
  assert.equal(copy!.info.fromSave, "base");
  assert.equal(copy!.state, "running");
  // v0.2.2 M6: on a node, and a save that names the node it was made on.
  routes["GET /v1/saves/onbox"] = [200, { ...save, name: "onbox", node: { id: "nd-abcd1234", name: "box" } }];
  assert.equal((await cx.getSave("onbox")).node?.name, "box");
  await cx.startFromSave("base", { name: "n", node: "box", cpus: 2, memoryMb: 2048 });
  assert.deepEqual(seen.at(-1)?.body, { name: "n", node: "box", cpus: 2, memoryMb: 2048 });
  await cx.deleteSave("base");
  assert.equal(seen.at(-1)?.route, "DELETE /v1/saves/base");
  await assert.rejects(cx.getSave("gone"), (e: CloudexError) => e.status === 404);
});

test("desktop returns a link to the viewer", async () => {
  routes["GET /v1/sandboxes/web"] = [200, info("cx-aaa111")];
  routes["GET /v1/health"] = [200, { ok: true, version: "v0", previewZone: "example.com" }];
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

test("the saved key goes to the saved URL however either is written, as in the CLI", async () => {
  const dir = await mkdtemp(join(tmpdir(), "cloudex-sdk-"));
  await writeFile(join(dir, "config.json"), JSON.stringify({ apiUrl: "https://Cloudex-API.x.workers.dev:443/", apiKey: "saved" }));
  process.env.CLOUDEX_CONFIG_DIR = dir;
  const sent: { url: string; auth: unknown }[] = [];
  const fetch = async (input: string, init: RequestInit) => {
    sent.push({ url: input, auth: (init.headers as Record<string, string>).authorization });
    return new Response(JSON.stringify({ ok: true, version: "v0", previewZone: null }), { headers: { "content-type": "application/json" } });
  };
  for (const apiUrl of [" CLOUDEX-API.X.WORKERS.DEV ", "HTTPS://cloudex-api.x.workers.dev//", "https://cloudex-api.x.workers.dev:443"]) {
    await new Cloudex({ apiUrl, fetch }).health();
    assert.deepEqual(sent.pop(), { url: "https://cloudex-api.x.workers.dev/v1/health", auth: "Bearer saved" }, apiUrl);
  }
  await assert.rejects(new Cloudex({ apiUrl: "http://cloudex-api.x.workers.dev", fetch }).health(), /no API key for http:/);
  assert.equal(sent.length, 0);
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

test("tokens: create, list, revoke and me", async () => {
  const t = { id: "tk-aaaa2222", name: "ci", kind: "token", role: "user", ownOnly: true, createdAt: 1, createdBy: "admin", expiresAt: null, lastUsedAt: null };
  routes["POST /v1/tokens"] = [201, { ...t, secret: "cx_aaaa2222_s" }];
  routes["GET /v1/tokens"] = [200, [t]];
  routes["DELETE /v1/tokens/tk-aaaa2222"] = [200, { revoked: "tk-aaaa2222" }];
  routes["GET /v1/me"] = [200, { role: "user", ownOnly: true, token: t }];
  const cx = new Cloudex({ apiUrl: url, apiKey: "k" });
  assert.equal((await cx.createToken({ name: "ci", ownOnly: true, expiresInSec: 3600 })).secret, "cx_aaaa2222_s");
  assert.deepEqual(seen.at(-1)?.body, { name: "ci", ownOnly: true, expiresInSec: 3600 });
  assert.equal((await cx.listTokens())[0]?.id, "tk-aaaa2222");
  assert.equal((await cx.me()).token?.name, "ci");
  await cx.revokeToken("tk-aaaa2222");
  assert.equal(seen.at(-1)?.route, "DELETE /v1/tokens/tk-aaaa2222");
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

test("settings: create sends them, settings() and updateSettings() read and change them", async () => {
  const view = { env: ["A"], files: ["~/.npmrc"], repo: { url: "https://github.com/you/app", dir: "~/app", branch: null } };
  routes["POST /v1/sandboxes"] = [201, info("cx-aaa111")];
  routes["GET /v1/sandboxes/cx-aaa111/settings"] = [200, view];
  routes["PATCH /v1/sandboxes/cx-aaa111/settings"] = [200, { ...view, env: [] }];
  const cx = new Cloudex({ apiUrl: url, apiKey: "k" });
  const box = await cx.create({ env: { A: "1" }, files: { "~/.npmrc": "x" }, repo: { url: "https://github.com/you/app" } });
  assert.deepEqual(seen.at(-1)?.body, { env: { A: "1" }, files: { "~/.npmrc": "x" }, repo: { url: "https://github.com/you/app" } });
  assert.deepEqual(await box.settings(), view);
  assert.deepEqual((await box.updateSettings({ env: { A: null } })).env, []);
  assert.deepEqual(seen.at(-1), { route: "PATCH /v1/sandboxes/cx-aaa111/settings", auth: "Bearer k", body: { env: { A: null } } });
});

test("update, resume on another size, and links: list and revoke", async () => {
  routes["GET /v1/sandboxes/web"] = [200, info("cx-aaa111", { name: "web" })];
  routes["PATCH /v1/sandboxes/cx-aaa111"] = [200, info("cx-aaa111", { name: "site", idleMinutes: 120 })];
  routes["POST /v1/sandboxes/cx-aaa111/resume"] = [200, info("cx-aaa111", { size: "standard-4" })];
  routes["GET /v1/sandboxes/cx-aaa111/hosts"] = [200, [{ port: 3000, url: "https://3000-cx-aaa111-t.example.com", createdAt: 1 }]];
  routes["DELETE /v1/sandboxes/cx-aaa111/hosts/3000"] = [200, { revoked: [3000], all: false }];
  routes["DELETE /v1/sandboxes/cx-aaa111/hosts"] = [200, { revoked: [3000], all: true }];
  const box = await new Cloudex({ apiUrl: url, apiKey: "k" }).get("web");
  await box.update({ name: "site", idleMinutes: 120 });
  assert.equal(box.name, "site");
  assert.equal(box.info.idleMinutes, 120);
  await box.resume({ size: "standard-4" });
  assert.equal(box.info.size, "standard-4");
  assert.deepEqual(await box.links(), [{ port: 3000, url: "https://3000-cx-aaa111-t.example.com", createdAt: 1 }]);
  assert.deepEqual(await box.revokeLinks(3000), { revoked: [3000], all: false });
  assert.deepEqual(await box.revokeLinks(), { revoked: [3000], all: true });
  assert.deepEqual(
    seen.map((s) => [s.route, s.body]),
    [
      ["GET /v1/sandboxes/web", undefined],
      ["PATCH /v1/sandboxes/cx-aaa111", { name: "site", idleMinutes: 120 }],
      ["POST /v1/sandboxes/cx-aaa111/resume", { size: "standard-4" }],
      ["GET /v1/sandboxes/cx-aaa111/hosts", undefined],
      ["DELETE /v1/sandboxes/cx-aaa111/hosts/3000", undefined],
      ["DELETE /v1/sandboxes/cx-aaa111/hosts", undefined],
    ],
  );
});

test("execStream yields output as it comes and ends with exit, skipping keepalives", async () => {
  routes["GET /v1/sandboxes/web"] = [200, info("cx-aaa111")];
  routes["POST /v1/sandboxes/cx-aaa111/exec"] = (res) => {
    res.writeHead(200, { "content-type": "application/x-ndjson" });
    // A line split across writes, and a character split across them too.
    const text = '{"type":"stdout","data":"hi\\n"}\n{"type":"keepalive"}\n{"type":"stderr","data":"é"}\n{"type":"exit","exitCode":3,"timedOut":false,"ms":9}\n';
    const bytes = Buffer.from(text);
    const cut = bytes.indexOf(Buffer.from("é")) + 1;
    res.write(bytes.subarray(0, 10));
    res.write(bytes.subarray(10, cut));
    setTimeout(() => res.end(bytes.subarray(cut)), 10);
  };
  const box = await new Cloudex({ apiUrl: url, apiKey: "k" }).get("web");
  const events = [];
  for await (const e of box.execStream(["npm", "test"], { cwd: "~/app" })) events.push(e);
  assert.deepEqual(events, [
    { type: "stdout", data: "hi\n" },
    { type: "stderr", data: "é" },
    { type: "exit", exitCode: 3, timedOut: false, ms: 9 },
  ]);
  assert.deepEqual(seen.at(-1)?.body, { cmd: "npm test", cwd: "~/app", stream: true });
});

test("execStream on an API without streaming turns its one answer into events", async () => {
  routes["GET /v1/sandboxes/web"] = [200, info("cx-aaa111")];
  routes["POST /v1/sandboxes/cx-aaa111/exec"] = [200, { exitCode: 0, stdout: "out", stderr: "", timedOut: false, ms: 4 }];
  const box = await new Cloudex({ apiUrl: url, apiKey: "k" }).get("web");
  const events = [];
  for await (const e of box.execStream("echo out")) events.push(e);
  assert.deepEqual(events, [
    { type: "stdout", data: "out" },
    { type: "exit", exitCode: 0, timedOut: false, ms: 4 },
  ]);
});

test("execStream throws when the stream breaks off, and leaving early hangs up", async () => {
  routes["GET /v1/sandboxes/web"] = [200, info("cx-aaa111")];
  routes["POST /v1/sandboxes/cx-aaa111/exec"] = (res) => {
    res.writeHead(200, { "content-type": "application/x-ndjson" });
    res.end('{"type":"stdout","data":"a"}\n{"type":"error","error":"the sandbox stopped"}\n');
  };
  const box = await new Cloudex({ apiUrl: url, apiKey: "k" }).get("web");
  const got: string[] = [];
  await assert.rejects(
    (async () => {
      for await (const e of box.execStream("sleep 1")) if (e.type === "stdout") got.push(e.data);
    })(),
    (e: unknown) => e instanceof CloudexError && e.status === 0 && e.message === "the sandbox stopped",
  );
  assert.deepEqual(got, ["a"]);

  let hungUp!: () => void;
  const closed = new Promise<void>((resolve) => (hungUp = resolve));
  routes["POST /v1/sandboxes/cx-aaa111/exec"] = (res) => {
    res.on("close", hungUp);
    res.writeHead(200, { "content-type": "application/x-ndjson" }).write('{"type":"stdout","data":"tick"}\n');
  };
  for await (const e of box.execStream("yes tick")) {
    assert.equal(e.type, "stdout");
    break;
  }
  await closed;
});

const command = (extra: object = {}) => ({
  id: "cm-aaaa1111",
  cmd: "npm run build",
  root: false,
  cwd: "/home/cloudex/app",
  state: "running",
  exitCode: null,
  signal: null,
  timedOut: false,
  timeoutSec: 0,
  startedAt: 1,
  endedAt: null,
  outputBytes: 0,
  ...extra,
});

test("commands: start, follow the output to the end, kill, forget", async () => {
  const path = "/v1/sandboxes/cx-aaa111/commands/cm-aaaa1111";
  routes["GET /v1/sandboxes/web"] = [200, info("cx-aaa111")];
  routes["POST /v1/sandboxes/cx-aaa111/commands"] = [201, command()];
  routes["GET /v1/sandboxes/cx-aaa111/commands"] = [200, [command()]];
  routes[`GET ${path}`] = [200, command({ outputBytes: 6 })];
  routes[`GET ${path}/output?after=0&wait=30`] = [200, { data: "one\n", offset: 0, next: 4, command: command({ outputBytes: 4 }) }];
  routes[`GET ${path}/output?after=4&wait=30`] = [200, { data: "", offset: 4, next: 4, command: command({ outputBytes: 4 }) }];
  routes[`GET ${path}/output?after=2&wait=0`] = [200, { data: "e\n", offset: 2, next: 4, command: command({ outputBytes: 4 }) }];
  routes[`POST ${path}/kill`] = [200, command({ state: "killed", signal: "INT", endedAt: 5 })];
  routes[`DELETE ${path}`] = [200, command({ state: "killed", signal: "INT", endedAt: 5 })];
  const box = await new Cloudex({ apiUrl: url, apiKey: "k" }).get("web");
  const cmd = await box.startCommand(["npm", "run", "build"], { cwd: "app", timeoutSec: 60 });
  assert.equal(cmd.id, "cm-aaaa1111");
  assert.deepEqual(seen.at(-1)?.body, { cmd: "npm run build", cwd: "app", timeoutSec: 60 });
  assert.equal((await box.listCommands()).length, 1);
  assert.equal((await box.getCommand(cmd.id)).outputBytes, 6);
  assert.equal((await box.commandOutput(cmd.id, { after: 2 })).data, "e\n");

  // A wait that ends with no output isn't yielded; the command ending is.
  const answers = [
    { data: "", offset: 4, next: 4, command: command({ outputBytes: 4 }) },
    { data: "two\n", offset: 4, next: 8, command: command({ state: "exited", exitCode: 0, endedAt: 2, outputBytes: 8 }) },
  ];
  routes[`GET ${path}/output?after=4&wait=30`] = (res) =>
    res.writeHead(200, { "content-type": "application/json" }).end(JSON.stringify(answers.shift()));
  const pages = [];
  for await (const page of box.followCommand(cmd.id)) pages.push(page);
  assert.deepEqual(pages.map((p) => p.data), ["one\n", "two\n"]);
  assert.equal(pages.at(-1)?.command.exitCode, 0);

  assert.equal((await box.killCommand(cmd.id, "INT")).signal, "INT");
  assert.deepEqual(seen.at(-1)?.body, { signal: "INT" });
  await box.killCommand(cmd.id);
  assert.deepEqual(seen.at(-1)?.body, {});
  assert.equal((await box.forgetCommand(cmd.id)).state, "killed");
  assert.equal(seen.at(-1)?.route, `DELETE ${path}`);
});

test("commands: following an ended command with no output gives one empty piece", async () => {
  const path = "/v1/sandboxes/cx-aaa111/commands/cm-aa%2F1";
  routes["GET /v1/sandboxes/cx-aaa111"] = [200, info("cx-aaa111")];
  routes[`GET ${path}/output?after=0&wait=30`] = [200, { data: "", offset: 0, next: 0, command: command({ state: "lost" }) }];
  const box = await new Cloudex({ apiUrl: url, apiKey: "k" }).get("cx-aaa111");
  const pages = [];
  for await (const page of box.followCommand("cm-aa/1")) pages.push(page);
  assert.deepEqual(pages.map((p) => [p.data, p.command.state]), [["", "lost"]]);
});

const fileInfo = (path: string, extra: object = {}) => ({ path, type: "file", size: 256, mode: "644", mtime: 1, ...extra });

test("files: write and read back binary bytes, stat, delete", async () => {
  const bytes = Uint8Array.from({ length: 256 }, (_, i) => i);
  const files = "/v1/sandboxes/cx-aaa111/files";
  routes["GET /v1/sandboxes/web"] = [200, info("cx-aaa111")];
  routes[`PUT ${files}/content?path=bin/my%20tool&mode=755`] = [200, fileInfo("/home/cloudex/bin/my tool", { mode: "755" })];
  routes[`PUT ${files}/content?path=~/.npmrc`] = [200, fileInfo("/home/cloudex/.npmrc", { size: 3 })];
  routes[`GET ${files}/content?path=bin/my%20tool`] = [200, bytes];
  routes[`GET ${files}?path=/etc&root=true`] = [
    200,
    fileInfo("/etc", { type: "dir", size: 4096, mode: "755", entries: [{ name: "hosts", type: "file", size: 9, mode: "644", mtime: 1 }] }),
  ];
  routes[`DELETE ${files}?path=build&recursive=true`] = [200, fileInfo("/home/cloudex/build", { type: "dir" })];
  routes[`GET ${files}?path=gone`] = [404, { error: "no such file: /home/cloudex/gone" }];
  const box = await new Cloudex({ apiUrl: url, apiKey: "k" }).get("web");

  assert.equal((await box.writeFile("bin/my tool", bytes, { mode: 0o755 })).mode, "755");
  assert.deepEqual(seen.at(-1)?.body, bytes);
  assert.equal(seen.at(-1)?.type, "application/octet-stream");
  await box.writeFile("~/.npmrc", "hé\n");
  assert.deepEqual(seen.at(-1)?.body, new TextEncoder().encode("hé\n"));
  await box.writeFile("~/.npmrc", new Blob([bytes]));
  assert.deepEqual(seen.at(-1)?.body, bytes);
  await box.writeFile("~/.npmrc", new Blob([bytes]).stream());
  assert.deepEqual(seen.at(-1)?.body, bytes);
  assert.deepEqual(await box.readFile("bin/my tool"), bytes);

  const etc = await box.stat("/etc", { root: true });
  assert.equal(etc.type, "dir");
  assert.equal(etc.entries?.[0]?.name, "hosts");
  assert.equal((await box.deleteFile("build", { recursive: true })).type, "dir");
  await assert.rejects(box.stat("gone"), (e: CloudexError) => e.status === 404 && /no such file/.test(e.message));
});

test("files: upload and download an archive", async () => {
  const tgz = Uint8Array.from([0x1f, 0x8b, 8, 0, 0, 0, 0, 0, 0, 3, 0xff]);
  const files = "/v1/sandboxes/cx-aaa111/files";
  routes["GET /v1/sandboxes/web"] = [200, info("cx-aaa111")];
  routes[`PUT ${files}/archive?path=app`] = [200, { path: "/home/cloudex/app", entries: 12 }];
  routes[`GET ${files}/archive?path=app`] = [200, tgz];
  const box = await new Cloudex({ apiUrl: url, apiKey: "k" }).get("web");
  assert.deepEqual(await box.uploadArchive("app", tgz), { path: "/home/cloudex/app", entries: 12 });
  assert.deepEqual(seen.at(-1)?.body, tgz);
  assert.equal(seen.at(-1)?.type, "application/gzip");
  assert.deepEqual(await box.downloadArchive("app"), tgz);
});

test("a failed connection is a CloudexError with the URL", async () => {
  const closed = createServer();
  await new Promise<void>((resolve) => closed.listen(0, "127.0.0.1", resolve));
  const gone = `http://127.0.0.1:${(closed.address() as AddressInfo).port}`;
  await new Promise((resolve) => closed.close(resolve));
  const noScheme = gone.replace("http://", "");
  for (const [apiUrl, tried] of [[gone, gone], [noScheme, `https://${noScheme}`]] as const) {
    await assert.rejects(new Cloudex({ apiUrl, apiKey: "k" }).list(), (e: unknown) => {
      assert.ok(e instanceof CloudexError);
      assert.equal(e.status, 0);
      assert.ok(e.message.startsWith(`can't reach ${tried}: `), e.message);
      return true;
    });
  }
});

test("a body that breaks off is a CloudexError with the URL", async () => {
  routes["GET /v1/sandboxes"] = (res) => {
    res.writeHead(200, { "content-type": "application/json", "content-length": "100" });
    res.write("[{");
    setTimeout(() => res.destroy(), 10);
  };
  await assert.rejects(new Cloudex({ apiUrl: url, apiKey: "k" }).list(), (e: unknown) => {
    assert.ok(e instanceof CloudexError);
    assert.equal(e.status, 0);
    assert.ok(e.message.startsWith(`can't reach ${url}: `), e.message);
    return true;
  });
});

test("an HTML error page becomes its status and title", async () => {
  const page = "<html>\n<head><title>502 Bad Gateway</title></head>\n<body>nginx</body></html>";
  routes["GET /v1/sandboxes"] = (res) => res.writeHead(502, { "content-type": "text/html" }).end(page);
  routes["GET /v1/saves"] = (res) => res.writeHead(503, { "content-type": "text/html" }).end("<html><title>\n Just a  moment </title></html>");
  routes["GET /v1/keys"] = (res) => res.writeHead(500, { "content-type": "text/plain" }).end("x".repeat(600));
  const cx = new Cloudex({ apiUrl: url, apiKey: "k" });
  await assert.rejects(cx.list(), (e: unknown) => e instanceof CloudexError && e.status === 502 && e.message === "502 Bad Gateway");
  await assert.rejects(cx.listSaves(), (e: unknown) => e instanceof CloudexError && e.message === "503 Just a moment");
  await assert.rejects(cx.listKeys(), (e: unknown) => e instanceof CloudexError && e.message === `${"x".repeat(500)}…`);
});

test("an unknown route says the deployment is older; a missing thing keeps its own message", async () => {
  routes["GET /v1/saves"] = [404, { error: "not found" }];
  routes["GET /v1/saves/gone"] = [404, { error: "no save named gone" }];
  const cx = new Cloudex({ apiUrl: url, apiKey: "k" });
  await assert.rejects(cx.listSaves(), (e: unknown) => {
    assert.ok(e instanceof CloudexError);
    assert.equal(e.status, 404);
    assert.match(e.message, /older than the SDK.*cloudex deploy/);
    return true;
  });
  await assert.rejects(cx.getSave("gone"), (e: unknown) => e instanceof CloudexError && e.message === "no save named gone");
});

test("the CLI's saved key goes only to the saved deployment", async () => {
  routes["GET /v1/health"] = [200, { ok: true, version: "v0", previewZone: null }];
  const dir = await mkdtemp(join(tmpdir(), "cloudex-sdk-"));
  await writeFile(join(dir, "config.json"), JSON.stringify({ apiUrl: url + "/", apiKey: "saved" }));
  process.env.CLOUDEX_CONFIG_DIR = dir;

  await new Cloudex({ apiUrl: url }).health();
  assert.equal(seen.at(-1)?.auth, "Bearer saved");

  seen.length = 0;
  const other = url.replace("127.0.0.1", "localhost");
  await assert.rejects(new Cloudex({ apiUrl: other }).health(), (e: unknown) => {
    assert.ok(e instanceof CloudexError);
    assert.equal(e.status, 0);
    assert.match(e.message, /no API key for http:\/\/localhost:\d+: pass apiKey or set CLOUDEX_API_KEY/);
    return true;
  });
  process.env.CLOUDEX_API_URL = other;
  await assert.rejects(new Cloudex().health(), /no API key/);
  assert.equal(seen.length, 0);

  await new Cloudex({ apiUrl: other, apiKey: "mine" }).health();
  assert.equal(seen.at(-1)?.auth, "Bearer mine");
  process.env.CLOUDEX_API_KEY = "from-env";
  await new Cloudex().health();
  assert.equal(seen.at(-1)?.auth, "Bearer from-env");
});

test("desktop on a deployment with no preview domain fails before starting anything", async () => {
  routes["GET /v1/sandboxes/web"] = [200, info("cx-aaa111")];
  routes["GET /v1/health"] = [200, { ok: true, version: "v0", previewZone: null }];
  const box = await new Cloudex({ apiUrl: url, apiKey: "k" }).get("web");
  await assert.rejects(box.desktop(), (e: unknown) => {
    assert.ok(e instanceof CloudexError);
    assert.equal(e.status, 400);
    assert.match(e.message, /preview links need a domain/);
    return true;
  });
  assert.ok(!seen.some((s) => s.route.includes("/desktop")));
});

test("a file mode below 0o100 is sent with three digits", async () => {
  routes["GET /v1/sandboxes/web"] = [200, info("cx-aaa111")];
  routes["PUT /v1/sandboxes/cx-aaa111/files/content?path=f&mode=077"] = [200, fileInfo("/home/cloudex/f", { mode: "077" })];
  routes["PUT /v1/sandboxes/cx-aaa111/files/content?path=g&mode=007"] = [200, fileInfo("/home/cloudex/g", { mode: "007" })];
  const box = await new Cloudex({ apiUrl: url, apiKey: "k" }).get("web");
  assert.equal((await box.writeFile("f", "x", { mode: 0o77 })).mode, "077");
  assert.equal((await box.writeFile("g", "x", { mode: 7 })).mode, "007");
});

test("nodes: create on a node, placement, list (live or from the index), get, add and remove", async () => {
  const placement = { provider: "node", node: { id: "nd-abcd1234", name: "box", online: true, state: "online", lastSeenAt: 5 }, cpus: 2, memoryMb: 2048 };
  routes = {
    "POST /v1/sandboxes": [201, info("cx-abc123", { placement, isolation: "sysbox" })],
    "GET /v1/nodes": [200, [{ id: "nd-abcd1234", name: "box", state: "online" }]],
    "GET /v1/nodes?live=false": [200, [{ id: "nd-abcd1234", name: "box", state: "offline" }]],
    "GET /v1/nodes/box": [200, { id: "nd-abcd1234", name: "box", state: "online", orphans: [], pendingDeletes: [] }],
    "POST /v1/nodes": [201, { node: { id: "nd-abcd1234", name: "box2" }, joinToken: "cxj_x", expiresAt: 9, install: "curl ..." }],
    "DELETE /v1/nodes/box?force=1": [200, { revoked: "nd-abcd1234" }],
    "DELETE /v1/nodes/box2": [200, { deleted: "nd-abcd1234" }],
    "DELETE /v1/sandboxes/cx-abc123?force=1": [200, { deleted: "cx-abc123" }],
  };
  const cx = new Cloudex({ apiUrl: url, apiKey: "k" });
  const s = await cx.create({ node: "box", cpus: 2, memoryMb: 2048 });
  assert.deepEqual(seen[0]!.body, { node: "box", cpus: 2, memoryMb: 2048 });
  assert.equal(s.placement.node?.name, "box");
  assert.equal(s.info.isolation, "sysbox");
  assert.equal((await cx.nodes.list())[0]!.state, "online");
  assert.equal((await cx.nodes.list({ live: false }))[0]!.state, "offline");
  assert.deepEqual((await cx.nodes.get("box")).pendingDeletes, []);
  assert.equal((await cx.nodes.add("box2")).install, "curl ...");
  await cx.nodes.remove("box", { force: true });
  await cx.nodes.remove("box2");
  await s.delete({ force: true });
  assert.deepEqual(seen.map((x) => x.route).slice(-3), ["DELETE /v1/nodes/box?force=1", "DELETE /v1/nodes/box2", "DELETE /v1/sandboxes/cx-abc123?force=1"]);
  // A sandbox from a deployment before v0.2.2 runs on Cloudflare.
  routes["GET /v1/sandboxes/old"] = [200, info("cx-old000")];
  assert.deepEqual((await cx.get("old")).placement, { provider: "cloudflare" });
});

test("a refusal on a node comes as a CloudexError with the API's status and message", async () => {
  routes = { "POST /v1/sandboxes": [503, { error: "node box is offline (last seen 2026-11-11 14:02 UTC)" }] };
  const cx = new Cloudex({ apiUrl: url, apiKey: "k" });
  await assert.rejects(cx.create({ node: "box" }), (e: unknown) => e instanceof CloudexError && e.status === 503 && /node box is offline/.test(e.message));
});
