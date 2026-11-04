// cloudex-api: the /v1 control plane, plus preview links when a zone is set.
// v0.1 authenticates with one admin key that `cloudex deploy` mints and stores
// as a Worker secret.
import { Hono } from "hono";
import {
  AgentMessage,
  CreateSandbox,
  CreateSave,
  DesktopRequest,
  ExecRequest,
  ForkRequest,
  KillCommand,
  StartCommand,
  HostRequest,
  Name,
  Provider,
  ResumeRequest,
  type SandboxSize,
  SetProviderKey,
  StartAgent,
  UpdateSandbox,
  UpdateSettings,
  type Health,
} from "@cloudex/schema";
import { z } from "zod";
import { openapi } from "@cloudex/schema/openapi";
import { isAgentId } from "./agents";
import { isCommandId } from "./commands";
import { backupBucket } from "./backup";
import { deleteSandbox, idForName, listSandboxes, nameTaken } from "./db";
import { hostLink, hostLinks, newMethod, onError, portParam, revokeLinks } from "./http";
import { deleteKey, listKeys, setKey } from "./keys";
import { NO_SUCH_SANDBOX, type ForkSource } from "./sandbox";
import { type SaveManifest, deleteSave, getSave, listSaves, readManifest } from "./saves";
import {
  LINK_HEADER,
  VERSION,
  errorStatus,
  fail,
  isSandboxId,
  newAgentId,
  newCommandId,
  newSandboxId,
  newSaveId,
  previewToken,
  sameSecret,
} from "./util";

export { Sandbox } from "./sandbox";
export { Egress } from "./egress";


type Ctx = { Bindings: Env };
const app = new Hono<Ctx>();

app.onError((e, c) => {
  const ref = c.req.param("ref");
  return onError(ref && e.message === `[404] ${NO_SUCH_SANDBOX}` ? new Error(`[404] no sandbox ${ref}`) : e, c);
});

app.get("/v1/health", (c) =>
  c.json({ ok: true, version: VERSION, previewZone: c.env.PREVIEW_ZONE || null, backups: Boolean(backupBucket(c.env)) } satisfies Health),
);

let spec: object | undefined;
app.get("/v1/openapi.json", (c) => c.json((spec ??= openapi(VERSION))));

app.use("/v1/*", async (c, next) => {
  const key = c.env.ADMIN_KEY;
  if (!key || !(await sameSecret(c.req.header("authorization") ?? "", `Bearer ${key}`))) {
    return c.json({ error: "unauthorized" }, 401);
  }
  await next();
});

// after and wait for long polls: agent events and command output.
const EventsQuery = z.object({
  after: z.coerce.number().int().min(0).default(0),
  wait: z.coerce.number().int().min(0).max(60).default(0),
});

// Sandboxes are addressed by id (cx-xxxxxx) or by name.
async function resolve(env: Env, ref: string) {
  const id = isSandboxId(ref) ? ref : await idForName(env.DB, ref);
  if (!id) return null;
  return { id, stub: env.SANDBOX.getByName(id) };
}

async function sandbox(env: Env, ref: string) {
  const s = await resolve(env, ref);
  if (!s) throw new Error(`[404] no sandbox named ${ref}`);
  return s;
}

app.post("/v1/sandboxes", async (c) => {
  const body = await c.req.text();
  const input = CreateSandbox.parse(body ? JSON.parse(body) : {});
  if (input.name && (await nameTaken(c.env.DB, input.name))) {
    return c.json({ error: `the name ${input.name} is taken` }, 409);
  }
  const id = newSandboxId();
  const s = await c.env.SANDBOX.getByName(id).create(id, { ...input, name: input.name ?? null });
  return c.json(s, 201);
});

app.get("/v1/sandboxes", async (c) => c.json(await listSandboxes(c.env.DB)));

app.get("/v1/sandboxes/:ref", async (c) => c.json(await (await sandbox(c.env, c.req.param("ref"))).stub.get()));

app.patch("/v1/sandboxes/:ref", async (c) => {
  const change = UpdateSandbox.parse(await c.req.json());
  const ref = c.req.param("ref");
  return c.json(await newMethod(ref, (await sandbox(c.env, ref)).stub.set(change)));
});

app.post("/v1/sandboxes/:ref/stop", async (c) => c.json(await (await sandbox(c.env, c.req.param("ref"))).stub.stop()));

app.post("/v1/sandboxes/:ref/resume", async (c) => {
  const body = await c.req.text();
  const { fromBackup, size } = ResumeRequest.parse(body ? JSON.parse(body) : {});
  return c.json(await (await sandbox(c.env, c.req.param("ref"))).stub.resume(fromBackup, size));
});

app.delete("/v1/sandboxes/:ref", async (c) => {
  const { id, stub } = await sandbox(c.env, c.req.param("ref"));
  try {
    return c.json(await stub.remove());
  } catch (e) {
    // An index row left behind by a delete that failed partway goes too, so
    // that it doesn't keep holding its name.
    if (errorStatus(e).status === 404 && (await deleteSandbox(c.env.DB, id))) return c.json({ deleted: id });
    throw e;
  }
});

app.post("/v1/sandboxes/:ref/exec", async (c) => {
  const { stream, ...input } = ExecRequest.parse(await c.req.json());
  const ref = c.req.param("ref");
  const { stub } = await sandbox(c.env, ref);
  if (!stream) return c.json(await stub.exec(input));
  const body = await newMethod(ref, stub.execStream(input));
  return new Response(body, { headers: { "content-type": "application/x-ndjson", "cache-control": "no-store" } });
});

// Background commands (commands.ts). Each call runs a script in the sandbox,
// which keeps the commands and their output.
async function commandOp(env: Env, ref: string, op: string, args: string[], stdin?: string, waitSec?: number) {
  const { stub } = await sandbox(env, ref);
  return newMethod(ref, stub.commandOp(op, args, stdin, waitSec));
}

function commandId(id: string) {
  if (!isCommandId(id)) fail(404, `no command ${id}`);
  return id;
}

app.post("/v1/sandboxes/:ref/commands", async (c) => {
  const input = StartCommand.parse(await c.req.json());
  const args = [newCommandId(), input.root ? "1" : "0", input.cwd ?? "", String(input.timeoutSec)];
  return c.json(await commandOp(c.env, c.req.param("ref"), "start", args, input.cmd), 201);
});

app.get("/v1/sandboxes/:ref/commands", async (c) => c.json(await commandOp(c.env, c.req.param("ref"), "list", [])));

app.get("/v1/sandboxes/:ref/commands/:cmd", async (c) =>
  c.json(await commandOp(c.env, c.req.param("ref"), "get", [commandId(c.req.param("cmd"))])),
);

// Long polling: with wait, answers as soon as there's output after `after`,
// the command ends, or the seconds pass.
app.get("/v1/sandboxes/:ref/commands/:cmd/output", async (c) => {
  const { after, wait } = EventsQuery.parse(c.req.query());
  const args = [commandId(c.req.param("cmd")), String(after), String(wait)];
  return c.json(await commandOp(c.env, c.req.param("ref"), "output", args, undefined, wait));
});

app.post("/v1/sandboxes/:ref/commands/:cmd/kill", async (c) => {
  const body = await c.req.text();
  const { signal } = KillCommand.parse(body ? JSON.parse(body) : {});
  return c.json(await commandOp(c.env, c.req.param("ref"), "kill", [commandId(c.req.param("cmd")), signal]));
});

app.delete("/v1/sandboxes/:ref/commands/:cmd", async (c) =>
  c.json(await commandOp(c.env, c.req.param("ref"), "forget", [commandId(c.req.param("cmd"))])),
);

// Files (files.ts), named by the path query parameter; root=true acts as root.
const FileQuery = z.object({
  path: z.string().min(1, "give a path").max(4096),
  root: z.enum(["true", "false", "1", "0"]).default("false").transform((v) => v === "true" || v === "1"),
});

const fileQuery = (c: { req: { query(): Record<string, string> } }) => FileQuery.parse(c.req.query());

app.get("/v1/sandboxes/:ref/files", async (c) => {
  const { path, root } = fileQuery(c);
  const ref = c.req.param("ref");
  return c.json(await newMethod(ref, (await sandbox(c.env, ref)).stub.fileInfo(path, root)));
});

app.delete("/v1/sandboxes/:ref/files", async (c) => {
  const { path, root } = fileQuery(c);
  const recursive = ["true", "1"].includes(c.req.query("recursive") ?? "");
  const ref = c.req.param("ref");
  return c.json(await newMethod(ref, (await sandbox(c.env, ref)).stub.deleteFile(path, root, recursive)));
});

async function download(env: Env, ref: string, query: Record<string, string>, archive: boolean) {
  const { path, root } = FileQuery.parse(query);
  const { stub } = await sandbox(env, ref);
  const { info, body } = await newMethod(ref, stub.readFile(path, root, archive));
  const name = encodeURIComponent(info.path.split("/").pop() || "root") + (archive ? ".tar.gz" : "");
  return new Response(body, {
    headers: {
      "content-type": archive ? "application/gzip" : "application/octet-stream",
      "content-disposition": `attachment; filename*=UTF-8''${name}`,
      "cache-control": "no-store",
      "x-cloudex-path": encodeURIComponent(info.path),
    },
  });
}

app.get("/v1/sandboxes/:ref/files/content", (c) => download(c.env, c.req.param("ref"), c.req.query(), false));

app.get("/v1/sandboxes/:ref/files/archive", (c) => download(c.env, c.req.param("ref"), c.req.query(), true));

app.put("/v1/sandboxes/:ref/files/content", async (c) => {
  const { path, root } = fileQuery(c);
  const mode = c.req.query("mode") || null;
  if (mode !== null && !/^[0-7]{3,4}$/.test(mode)) fail(400, `bad mode ${mode}: give it in octal, such as 644`);
  const ref = c.req.param("ref");
  return c.json(await newMethod(ref, (await sandbox(c.env, ref)).stub.writeFile(path, root, mode, c.req.raw.body)));
});

app.put("/v1/sandboxes/:ref/files/archive", async (c) => {
  const { path, root } = fileQuery(c);
  const ref = c.req.param("ref");
  return c.json(await newMethod(ref, (await sandbox(c.env, ref)).stub.writeArchive(path, root, c.req.raw.body)));
});

// The names ForkRequest asks for, checked before anything starts.
async function copyNames(env: Env, input: z.output<typeof ForkRequest>): Promise<(string | null)[]> {
  const names = Array.from({ length: input.count }, (_, i) =>
    !input.name ? null : input.count === 1 ? input.name : `${input.name}-${i + 1}`,
  );
  for (const name of names) {
    if (!name) continue;
    const ok = Name.safeParse(name);
    if (!ok.success) fail(400, `can't name a sandbox ${name}: ${ok.error.issues[0]?.message}`);
    if (await nameTaken(env.DB, name)) fail(409, `the name ${name} is taken`);
  }
  return names;
}

// Starts sandboxes from one fork point or named save, all at once. One that
// fails to start is kept in state error with the reason, like a failed new,
// and resuming it tries again.
function startCopies(
  env: Env,
  names: (string | null)[],
  opts: { size: SandboxSize; idleMinutes: number },
  of: string,
  from?: ForkSource,
  save?: SaveManifest,
) {
  return Promise.all(
    names.map(async (name) => {
      const id = newSandboxId();
      const copy = env.SANDBOX.getByName(id);
      try {
        return await copy.create(id, { name, ...opts }, from, save);
      } catch (e) {
        console.error(`[${id}] start from ${of} failed: ${errorStatus(e).message}`);
        return await copy.get();
      }
    }),
  );
}

// Forks (see ForkRequest), which all start from the same save, at the same time.
app.post("/v1/sandboxes/:ref/fork", async (c) => {
  const body = await c.req.text();
  const input = ForkRequest.parse(body ? JSON.parse(body) : {});
  const ref = c.req.param("ref");
  const names = await copyNames(c.env, input);
  const { stub } = await sandbox(c.env, ref);
  const original = await stub.get();
  const from = await newMethod(ref, stub.forkPoint());
  const opts = { size: input.size ?? original.size, idleMinutes: input.idleMinutes ?? original.idleMinutes };
  return c.json(await startCopies(c.env, names, opts, original.id, from), 201);
});

// Named saves (saves.ts): made from a running sandbox, which keeps running;
// sandboxes start from one like forks, with the same options.
function bucket(env: Env) {
  const b = backupBucket(env);
  if (!b) fail(501, "named saves need an R2 bucket, and this deployment has none: turn R2 on in the Cloudflare dashboard, then run cloudex deploy");
  return b;
}

async function namedSave(env: Env, name: string) {
  const save = await getSave(env.DB, name);
  if (!save) fail(404, `no save named ${name}`);
  return save;
}

app.post("/v1/sandboxes/:ref/saves", async (c) => {
  const { name } = CreateSave.parse(await c.req.json());
  bucket(c.env);
  const ref = c.req.param("ref");
  return c.json(await newMethod(ref, (await sandbox(c.env, ref)).stub.saveAs(name, newSaveId())), 201);
});

app.get("/v1/saves", async (c) => c.json(await listSaves(c.env.DB)));

app.get("/v1/saves/:name", async (c) => c.json(await namedSave(c.env, c.req.param("name"))));

app.delete("/v1/saves/:name", async (c) => {
  const name = c.req.param("name");
  if (!(await deleteSave(c.env.DB, bucket(c.env), name))) fail(404, `no save named ${name}`);
  return c.json({ deleted: name });
});

app.post("/v1/saves/:name/sandboxes", async (c) => {
  const body = await c.req.text();
  const input = ForkRequest.parse(body ? JSON.parse(body) : {});
  const save = await namedSave(c.env, c.req.param("name"));
  const manifest = await readManifest(bucket(c.env), save.id);
  if (!manifest) fail(409, `the save ${save.name} was saved over or deleted just now; try again`);
  const names = await copyNames(c.env, input);
  const opts = { size: input.size ?? save.size, idleMinutes: input.idleMinutes ?? save.idleMinutes };
  return c.json(await startCopies(c.env, names, opts, `the save ${save.name}`, undefined, manifest), 201);
});

// The desktop: POST starts it (installing it the first time) or resizes it; DELETE stops it.
app.post("/v1/sandboxes/:ref/desktop", async (c) => {
  const body = await c.req.text();
  const { size } = DesktopRequest.parse(body ? JSON.parse(body) : {});
  const ref = c.req.param("ref");
  return c.json(await newMethod(ref, (await sandbox(c.env, ref)).stub.desktop(size)));
});

app.delete("/v1/sandboxes/:ref/desktop", async (c) => {
  const ref = c.req.param("ref");
  return c.json(await newMethod(ref, (await sandbox(c.env, ref)).stub.stopDesktop()));
});

// Agents: Claude Code or Codex on a task with no terminal (agents.ts). Each
// call runs a script in the sandbox, which keeps the agent's state.
async function agentOp(env: Env, ref: string, op: string, args: string[], prompt?: string, waitSec?: number) {
  const { stub } = await sandbox(env, ref);
  return newMethod(ref, stub.agentOp(op, args, prompt, waitSec));
}

function agentId(id: string) {
  if (!isAgentId(id)) fail(404, `no agent ${id}`);
  return id;
}


app.post("/v1/sandboxes/:ref/agents", async (c) => {
  const input = StartAgent.parse(await c.req.json());
  const args = [newAgentId(), input.agent, input.cwd ?? "", input.model ?? ""];
  return c.json(await agentOp(c.env, c.req.param("ref"), "start", args, input.prompt), 201);
});

app.get("/v1/sandboxes/:ref/agents", async (c) => c.json(await agentOp(c.env, c.req.param("ref"), "list", [])));

app.get("/v1/sandboxes/:ref/agents/:agent", async (c) =>
  c.json(await agentOp(c.env, c.req.param("ref"), "get", [agentId(c.req.param("agent"))])),
);

// Long polling: with wait, answers as soon as there are events after `after`,
// the agent goes idle, or the seconds pass.
app.get("/v1/sandboxes/:ref/agents/:agent/events", async (c) => {
  const { after, wait } = EventsQuery.parse(c.req.query());
  const args = [agentId(c.req.param("agent")), String(after), String(wait)];
  return c.json(await agentOp(c.env, c.req.param("ref"), "events", args, undefined, wait));
});

app.post("/v1/sandboxes/:ref/agents/:agent/messages", async (c) => {
  const id = agentId(c.req.param("agent"));
  const { prompt, interrupt } = AgentMessage.parse(await c.req.json());
  return c.json(await agentOp(c.env, c.req.param("ref"), "send", [id, interrupt ? "1" : "0"], prompt));
});

app.post("/v1/sandboxes/:ref/agents/:agent/interrupt", async (c) =>
  c.json(await agentOp(c.env, c.req.param("ref"), "interrupt", [agentId(c.req.param("agent"))])),
);

// WebSocket to a guest TCP port: `cloudex ssh` and `cloudex proxy` use port 22.
app.get("/v1/sandboxes/:ref/tcp/:port{[0-9]+}", async (c) => {
  const { stub } = await sandbox(c.env, c.req.param("ref"));
  const url = new URL(c.req.url);
  return stub.fetch(new Request(`http://sandbox/tcp/${c.req.param("port")}${url.search}`, c.req.raw));
});

// Preview links: POST hands out a port's link, GET lists the ones that work,
// DELETE revokes one port's link or, without a port, all of them.
app.post("/v1/sandboxes/:ref/hosts", async (c) => {
  const { port } = HostRequest.parse(await c.req.json());
  const { id, stub } = await sandbox(c.env, c.req.param("ref"));
  return c.json(await hostLink(c.env, id, stub, port));
});

app.get("/v1/sandboxes/:ref/hosts", async (c) => {
  const { id, stub } = await sandbox(c.env, c.req.param("ref"));
  return c.json(await hostLinks(c.env, id, stub));
});

app.delete("/v1/sandboxes/:ref/hosts", async (c) => {
  const { id, stub } = await sandbox(c.env, c.req.param("ref"));
  return c.json(await revokeLinks(id, stub));
});

app.delete("/v1/sandboxes/:ref/hosts/:port", async (c) => {
  const port = portParam(c.req.param("port"));
  const { id, stub } = await sandbox(c.env, c.req.param("ref"));
  return c.json(await revokeLinks(id, stub, port));
});

// Settings (settings.ts): environment variables and files, applied at every
// start and at once to a running sandbox. Names and paths only come back.
app.get("/v1/sandboxes/:ref/settings", async (c) => {
  const ref = c.req.param("ref");
  return c.json(await newMethod(ref, (await sandbox(c.env, ref)).stub.settings()));
});

app.patch("/v1/sandboxes/:ref/settings", async (c) => {
  const change = UpdateSettings.parse(await c.req.json());
  const ref = c.req.param("ref");
  return c.json(await newMethod(ref, (await sandbox(c.env, ref)).stub.updateSettings(change)));
});

// AI provider keys (keys.ts). Keys are write-only: listing shows a hint.
app.get("/v1/keys", async (c) => c.json(await listKeys(c.env.DB)));

function provider(name: string) {
  const p = Provider.safeParse(name);
  if (!p.success) fail(404, `unknown provider ${name}: use ${Provider.options.join(" or ")}`);
  return p.data;
}

app.put("/v1/keys/:provider", async (c) => {
  const p = provider(c.req.param("provider"));
  const { key } = SetProviderKey.parse(await c.req.json());
  return c.json(await setKey(c.env, p, key));
});

app.delete("/v1/keys/:provider", async (c) => {
  const p = provider(c.req.param("provider"));
  if (!(await deleteKey(c.env.DB, p))) return c.json({ error: `no ${p} key is set` }, 404);
  return c.json({ deleted: p });
});

app.all("/v1/*", (c) => c.json({ error: "not found" }, 404));

// Preview links: https://<port>-<id>-<token>.<zone>. One label, so the zone's
// Universal SSL certificate covers it.
const PREVIEW_HOST = /^(\d{1,5})-(cx-[a-z0-9]{6})-([a-z0-9]{10})\./;

// Proxy headers apps trust, which a visitor could otherwise send themselves.
const SPOOFABLE = [
  "forwarded",
  "x-forwarded-for",
  "x-forwarded-host",
  "x-forwarded-port",
  "x-forwarded-prefix",
  "x-forwarded-proto",
  "x-forwarded-scheme",
  "x-forwarded-server",
  "x-forwarded-ssl",
  "x-real-ip",
  "client-ip",
  "true-client-ip",
  "x-client-ip",
];

// The Sandbox object checks the token against the links revoked so far. One it
// didn't make itself before it could revoke, the HMAC of sandbox and port, is
// checked here first, so that a sandbox still on code from before revoking
// never serves a token nobody made.
async function preview(req: Request, env: Env, m: RegExpExecArray): Promise<Response> {
  const [, portText, id, token] = m as unknown as [string, string, string, string];
  const port = Number(portText);
  const stub = env.SANDBOX.getByName(id);
  const first = await sameSecret(token, await previewToken(env.ADMIN_KEY, id, port));
  if (!first && !(await stub.checkLink(port, token).catch(() => false))) {
    return new Response("not found", { status: 404 });
  }
  const url = new URL(req.url);
  const fwd = new Request(`http://sandbox/http/${port}${url.pathname}${url.search}`, req);
  fwd.headers.set(LINK_HEADER, token);
  // The app sees the link's own host name and the visitor's address, as
  // behind any proxy; not hosts or addresses the visitor claims.
  for (const h of SPOOFABLE) fwd.headers.delete(h);
  fwd.headers.set("x-forwarded-host", url.hostname);
  fwd.headers.set("x-forwarded-proto", "https");
  fwd.headers.set("x-forwarded-port", "443");
  const visitor = req.headers.get("cf-connecting-ip");
  if (visitor) {
    fwd.headers.set("x-forwarded-for", visitor);
    fwd.headers.set("x-real-ip", visitor);
  }
  return stub.fetch(fwd);
}

export default {
  async fetch(req, env, ctx) {
    const host = new URL(req.url).hostname;
    const zone = env.PREVIEW_ZONE;
    if (zone && host.endsWith(`.${zone}`)) {
      const m = PREVIEW_HOST.exec(host);
      if (m && env.ADMIN_KEY) return preview(req, env, m);
      // The preview route (*.zone/*) also catches the zone's other subdomains,
      // like www. Send those on to their origin untouched.
      return fetch(req);
    }
    return app.fetch(req, env, ctx);
  },
} satisfies ExportedHandler<Env>;
