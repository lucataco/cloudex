// cloudex-api: the /v1 control plane, plus preview links when a zone is set.
// v0.1 authenticates with one admin key that `cloudex deploy` mints and stores
// as a Worker secret.
import { Hono } from "hono";
import {
  AgentMessage,
  CreateSandbox,
  DesktopRequest,
  ExecRequest,
  ForkRequest,
  HostRequest,
  Name,
  Provider,
  ResumeRequest,
  SetProviderKey,
  StartAgent,
  UpdateSandbox,
  UpdateSettings,
  type Health,
} from "@cloudex/schema";
import { z } from "zod";
import { openapi } from "@cloudex/schema/openapi";
import { isAgentId } from "./agents";
import { backupBucket } from "./backup";
import { idForName, listSandboxes, nameTaken } from "./db";
import { hostLink, hostLinks, newMethod, onError, portParam, revokeLinks } from "./http";
import { deleteKey, listKeys, setKey } from "./keys";
import { LINK_HEADER, errorStatus, fail, isSandboxId, newAgentId, newSandboxId, previewToken, sameSecret } from "./util";

export { Sandbox } from "./sandbox";
export { Egress } from "./egress";

// Set by scripts/bundle-api.sh for releases.
declare const CLOUDEX_VERSION: string | undefined;
const VERSION = typeof CLOUDEX_VERSION === "string" ? CLOUDEX_VERSION : "dev";

type Ctx = { Bindings: Env };
const app = new Hono<Ctx>();

app.onError(onError);

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
  const input = CreateSandbox.parse(await c.req.json().catch(() => ({})));
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

app.delete("/v1/sandboxes/:ref", async (c) =>
  c.json(await (await sandbox(c.env, c.req.param("ref"))).stub.remove()),
);

app.post("/v1/sandboxes/:ref/exec", async (c) => {
  const input = ExecRequest.parse(await c.req.json());
  return c.json(await (await sandbox(c.env, c.req.param("ref"))).stub.exec(input));
});

// Forks (see ForkRequest). All of them start from the same save, at the same
// time. One that fails to start is kept in state error with the reason, like a
// failed new, and resuming it tries again.
app.post("/v1/sandboxes/:ref/fork", async (c) => {
  const body = await c.req.text();
  const input = ForkRequest.parse(body ? JSON.parse(body) : {});
  const ref = c.req.param("ref");
  const names = Array.from({ length: input.count }, (_, i) =>
    !input.name ? null : input.count === 1 ? input.name : `${input.name}-${i + 1}`,
  );
  for (const name of names) {
    if (!name) continue;
    const ok = Name.safeParse(name);
    if (!ok.success) return c.json({ error: `can't name a fork ${name}: ${ok.error.issues[0]?.message}` }, 400);
    if (await nameTaken(c.env.DB, name)) return c.json({ error: `the name ${name} is taken` }, 409);
  }
  const { stub } = await sandbox(c.env, ref);
  const original = await stub.get();
  const from = await newMethod(ref, stub.forkPoint());
  const size = input.size ?? original.size;
  const idleMinutes = input.idleMinutes ?? original.idleMinutes;
  const forks = await Promise.all(
    names.map(async (name) => {
      const id = newSandboxId();
      const fork = c.env.SANDBOX.getByName(id);
      try {
        return await fork.create(id, { name, size, idleMinutes }, from);
      } catch (e) {
        console.error(`[${id}] fork of ${original.id} didn't start: ${errorStatus(e).message}`);
        return await fork.get();
      }
    }),
  );
  return c.json(forks, 201);
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

const EventsQuery = z.object({
  after: z.coerce.number().int().min(0).default(0),
  wait: z.coerce.number().int().min(0).max(60).default(0),
});

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
