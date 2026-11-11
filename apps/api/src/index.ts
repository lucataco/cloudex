// cloudex-api: the /v1 control plane, plus preview links when a zone is set.
// Callers authenticate with the admin key that `cloudex deploy` mints and
// stores as a Worker secret, or with an access token (tokens.ts).
import { Hono } from "hono";
import {
  AgentMessage,
  CreateLoginLink,
  CreateSandbox,
  CreateSave,
  CreateToken,
  DeviceApprove,
  DeviceDeny,
  DevicePoll,
  DeviceStart,
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
  StartSession,
  UpdateSandbox,
  UpdateSettings,
  type Health,
  CreateNode,
  type NewNode,
  type Node as ApiNode,
  NodeJoin,
  type NodeJoined,
  NODE_PROTOCOL,
} from "@cloudex/schema";
import { z } from "zod";
import { openapi } from "@cloudex/schema/openapi";
import { isAgentId } from "./agents";
import { isCommandId } from "./commands";
import { backupBucket } from "./backup";
import { deleteSandbox, idForName, listSandboxes, nameTaken, sandboxesOnNode } from "./db";
import {
  DEVICE_INTERVAL,
  SESSION_TTL_SEC,
  type Who,
  authenticate,
  authenticateSecret,
  authorize,
  createLoginLink,
  createToken,
  decideDevice,
  listTokens,
  me,
  ownerKey,
  pollDevice,
  refuseSession,
  revokeToken,
  setOwner,
  startDevice,
  startSession,
  useLoginLink,
  waitingDevices,
} from "./tokens";
import { serveDashboard } from "./dashboard";
import { CONNECT } from "./node";
import { HEADERS, issueJoinToken, readCapped, useJoinToken, verifyJoinSignature, verifyNodeRequest } from "./nodeauth";
import { addNode, findNode, forgetNode, joinNode, listNodeRows, mustFindNode, nodeKeys, nodePublicKey, nodeView, revokeNode, installCommand } from "./nodes";
import { placeOn, refused } from "./placement";
import { locationHint } from "./nodestate";
import { cachedKey, nodeEgress } from "./nodeegress";
import { readTicket } from "./nodestream";
import { notFromDashboard, readSession, sessionCookie, sessionName } from "./session";
import { hostLink, hostLinks, newMethod, onError, portParam, revokeLinks } from "./http";
import { deleteKey, keyRefusal, listKeys, setKey } from "./keys";
import { NO_SUCH_SANDBOX, type ForkSource, type NodeChoice } from "./sandbox";
import { type SaveManifest, deleteSave, getSave, listSaves, readManifest } from "./saves";
import {
  LINK_HEADER,
  TOKEN_HEADER,
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
  withTimeout,
} from "./util";

export { Sandbox } from "./sandbox";
export { Node } from "./node";
export { Egress } from "./egress";


type Ctx = { Bindings: Env; Variables: { who: Who } };
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

// Device login (RFC 8628), for a computer without a key: these two take none.
// The form-encoded bodies of the RFC work as well as JSON.
async function body(c: { req: { header(n: string): string | undefined; text(): Promise<string> } }) {
  const text = await c.req.text();
  if (/application\/x-www-form-urlencoded/.test(c.req.header("content-type") ?? "")) return Object.fromEntries(new URLSearchParams(text));
  return text ? JSON.parse(text) : {};
}

app.post("/v1/auth/device", async (c) => {
  const input = DeviceStart.parse(await body(c));
  const cf = (c.req.raw as { cf?: { country?: string } }).cf;
  const d = await startDevice(c.env.DB, {
    ...input,
    ip: c.req.header("cf-connecting-ip") ?? null,
    country: cf?.country ?? null,
    userAgent: c.req.header("user-agent") ?? null,
  });
  const origin = new URL(c.req.url).origin;
  return c.json({
    device_code: d.deviceCode,
    user_code: d.userCode,
    verification_uri: `${origin}/login`,
    verification_uri_complete: `${origin}/login?code=${d.userCode}`,
    expires_in: d.expiresIn,
    interval: DEVICE_INTERVAL,
  });
});

app.post("/v1/auth/token", async (c) => {
  const { device_code } = DevicePoll.parse(await body(c));
  const r = await pollDevice(c.env.DB, device_code);
  if ("error" in r) return c.json({ error: r.error }, 400);
  const { secret, ...token } = r.token;
  return c.json({ access_token: secret, token_type: "Bearer", token });
});

// The dashboard's sign-in (session.ts): the login page trades a one-time
// link's code, a pasted token or key, or an approved device code for a
// session cookie. The session never has more than what signed it in.
app.post("/v1/auth/session", async (c) => {
  const bad = notFromDashboard(c.req.raw, true);
  if (bad) return c.json({ error: `forbidden: ${bad}` }, 403);
  const input = StartSession.parse(await c.req.json());
  const name = sessionName(c.req.header("user-agent"));
  let made;
  if ("link" in input) {
    made = await useLoginLink(c.env.DB, input.link, name);
    if (!made) return c.json({ error: "that sign-in link was used already or has expired (they last 60 seconds): run cloudex dashboard again" }, 401);
  } else if ("token" in input) {
    const who = await authenticateSecret(c.env, input.token.trim());
    if (!who) return c.json({ error: "that isn't this deployment's admin key or a working token" }, 401);
    refuseSession(who);
    made = await startSession(c.env.DB, who, name);
  } else {
    const r = await pollDevice(c.env.DB, input.device_code, { name });
    if ("error" in r) return c.json({ error: r.error }, 400);
    made = r.token;
  }
  const { secret, ...token } = made;
  c.header("set-cookie", sessionCookie(secret, SESSION_TTL_SEC));
  c.header("cache-control", "no-store");
  return c.json(me({ kind: "token", token }));
});

app.delete("/v1/auth/session", async (c) => {
  const secret = readSession(c.req.raw);
  const bad = secret && notFromDashboard(c.req.raw);
  if (bad) return c.json({ error: `forbidden: ${bad}` }, 403);
  const who = secret ? await authenticateSecret(c.env, secret) : null;
  if (who?.kind === "token" && who.token.kind === "session") await revokeToken(c.env.DB, who.token.id);
  c.header("set-cookie", sessionCookie(null));
  return c.json({ signedOut: true });
});

// The node agent's two routes (nodeauth.ts). They take no bearer token and no
// cookie, only the node's signature, so they come before the middleware below;
// and that middleware never takes a node's signature. Before a caller has
// proven it holds a node's key, it costs at most a 16 KiB body, a signature
// check and one D1 read or write, never a socket or a Durable Object.
const JOIN_BODY_MAX = 16_384;

app.post("/v1/node/join", async (c) => {
  const text = await readCapped(c.req.raw, JOIN_BODY_MAX);
  if (text === null) return c.json({ error: `a join is at most ${JOIN_BODY_MAX} bytes` }, 413);
  let raw: unknown;
  try {
    raw = JSON.parse(text);
  } catch {
    return c.json({ error: "the body isn't JSON" }, 400);
  }
  const parsed = NodeJoin.safeParse(raw);
  if (!parsed.success) {
    const i = parsed.error.issues[0];
    return c.json({ error: `${i?.path.join(".") || "body"}: ${i?.message}` }, 400);
  }
  const j = parsed.data;
  const refusal = await verifyJoinSignature(c.req.raw, j.publicKey);
  if (refusal) return c.json({ error: refusal.error }, refusal.status);
  const nodeId = await useJoinToken(c.env.DB, j.token);
  const node = nodeId && (await joinNode(c.env.DB, nodeId, j));
  if (!node) {
    return c.json({ error: "unauthorized: that join token is unknown, used or expired (each works once, for 30 minutes); cloudex node add <name> makes a new one" }, 401);
  }
  return c.json({ node } satisfies NodeJoined);
});

app.get("/v1/node/connect", async (c) => {
  if (c.req.header("upgrade")?.toLowerCase() !== "websocket") return c.json({ error: "the node's control socket is a WebSocket" }, 426);
  const protocol = c.req.header(HEADERS.protocol) ?? "";
  if (protocol !== String(NODE_PROTOCOL)) {
    return c.json(
      { error: `this deployment speaks node protocol ${NODE_PROTOCOL} and the agent ${protocol || "doesn't say"}: install the agent of the deployment's version (${VERSION})` },
      426,
    );
  }
  const v = await verifyNodeRequest(c.req.raw, (id) => nodeKeys(c.env.DB, id));
  if (!("signed" in v)) return c.json({ error: v.error, ...(v.revoked ? { revoked: true } : {}) }, v.status);
  const cf = (c.req.raw as { cf?: { colo?: unknown; continent?: unknown; longitude?: unknown } }).cf;
  const fwd = new Request("http://node/connect", c.req.raw);
  fwd.headers.set(CONNECT.node, v.signed.node);
  fwd.headers.set(CONNECT.nonce, v.signed.nonce);
  fwd.headers.set(CONNECT.colo, typeof cf?.colo === "string" ? cf.colo.slice(0, 8) : "");
  fwd.headers.set(CONNECT.continent, typeof cf?.continent === "string" ? cf.continent.slice(0, 4) : "");
  fwd.headers.set(CONNECT.hint, locationHint(cf) ?? "");
  fwd.headers.set(CONNECT.skew, String(Number(v.signed.ts) - Math.floor(Date.now() / 1000)));
  return c.env.NODE.getByName(v.signed.node).fetch(fwd);
});

// A stream socket from a node's agent (nodestream.ts): an exec, a connection
// or a preview request to one of its sandboxes. Its ticket, issued by the
// node's object over the control socket, is checked here without D1, and
// again in the sandbox's object, which also refuses a stream id it has seen.
app.get("/v1/node/streams/:sandbox/:stream", async (c) => {
  if (c.req.header("upgrade")?.toLowerCase() !== "websocket") return c.json({ error: "a node's stream is a WebSocket" }, 426);
  const t = await readTicket(c.env.ADMIN_KEY, c.req.header("x-cx-ticket") ?? "");
  if (!t || t.sandbox !== c.req.param("sandbox") || t.stream !== c.req.param("stream")) {
    return c.json({ error: "unauthorized: that stream ticket is unknown, expired, or for another stream" }, 401);
  }
  return c.env.SANDBOX.getByName(t.sandbox).fetch(new Request("http://sandbox/node-stream", c.req.raw));
});

// What a sandbox on a node sends to cloudex.internal and to the AI providers,
// passed on by its node's agent (nodeegress.ts, v0.2.2 M6): signed by the
// node, bound to one of its sandboxes, then handled by Egress as a Cloudflare
// container's request is.
const egressKey = (env: Env) => cachedKey((id) => nodePublicKey(env.DB, id));
app.all("/v1/node/egress", (c) =>
  nodeEgress(c.req.raw, {
    publicKeyOf: egressKey(c.env),
    info: (sandbox) => c.env.SANDBOX.getByName(sandbox).nodeEgress(),
    egress: (props) => c.executionCtx.exports.Egress({ props }),
  }),
);

// Every other /v1 route: the admin key or a token as the bearer, or the
// dashboard's session cookie.
app.use("/v1/*", async (c, next) => {
  const header = c.req.header("authorization");
  const cookie = header ? null : readSession(c.req.raw);
  if (cookie) {
    const bad = notFromDashboard(c.req.raw);
    if (bad) return c.json({ error: `forbidden: ${bad}` }, 403);
  }
  let who = cookie ? await authenticateSecret(c.env, cookie, c.executionCtx) : await authenticate(c.env, header ?? "", c.executionCtx);
  // A cookie holds a session, never a token of another kind or the admin key.
  if (cookie && !(who?.kind === "token" && who.token.kind === "session")) who = null;
  if (!who) {
    const why = cookie
      ? "the dashboard's session has ended (it was signed out or revoked, or wasn't used for 30 days): sign in again"
      : header
        ? "the key isn't this deployment's admin key or a working token (a token may have been revoked, or have expired)"
        : "no key: send Authorization: Bearer <the admin key or a token>";
    if (cookie) c.header("set-cookie", sessionCookie(null));
    return c.json({ error: `unauthorized: ${why}` }, 401);
  }
  await authorize(c.env, who, c.req.method, new URL(c.req.url).pathname, async (ref) => (await resolve(c.env, ref))?.id ?? null);
  c.set("who", who);
  await next();
  // A session's 30 days started again: so does its cookie's.
  if (cookie && who.kind === "token" && who.renewed && c.res.status !== 101) {
    try {
      c.res.headers.append("set-cookie", sessionCookie(cookie, SESSION_TTL_SEC));
    } catch {
      // an answer passed through with fixed headers; the next one sets it
    }
  }
});

// A one-time link that signs a browser in as the caller: cloudex dashboard.
app.post("/v1/auth/links", async (c) => {
  refuseSession(c.get("who"));
  const text = await c.req.text();
  const { path } = CreateLoginLink.parse(text ? JSON.parse(text) : {});
  const { code, expiresIn } = await createLoginLink(c.env.DB, c.get("who"));
  const origin = new URL(c.req.url).origin;
  const next = path === "/" ? "" : `&next=${encodeURIComponent(path)}`;
  return c.json({ url: `${origin}/login#link=${code}${next}`, expiresIn }, 201);
});

app.get("/v1/me", (c) => c.json(me(c.get("who"))));

// Tokens: for admins (tokens.ts RULES).
app.post("/v1/tokens", async (c) => {
  const input = CreateToken.parse(await c.req.json());
  const who = c.get("who");
  return c.json(await createToken(c.env.DB, { ...input, createdBy: who.kind === "admin" ? "admin" : who.token.id }), 201);
});

app.get("/v1/tokens", async (c) => c.json(await listTokens(c.env.DB)));

app.delete("/v1/tokens/:id", async (c) => {
  const id = c.req.param("id");
  const t = await revokeToken(c.env.DB, id);
  if (!t) fail(404, `no token ${id}`);
  return c.json({ revoked: id });
});

app.get("/v1/auth/device", async (c) => c.json(await waitingDevices(c.env.DB)));

app.post("/v1/auth/device/approve", async (c) => {
  const { user_code, ...grant } = DeviceApprove.parse(await c.req.json());
  return c.json(await decideDevice(c.env.DB, c.get("who"), user_code, { approve: true, ...grant }));
});

app.post("/v1/auth/device/deny", async (c) => {
  const { user_code } = DeviceDeny.parse(await c.req.json());
  return c.json(await decideDevice(c.env.DB, c.get("who"), user_code, { approve: false }));
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

// On a node (v0.2.2 M5): it must be online, pass its self-test and allow
// the caller (placement.ts). Its Sandbox object is made near the node, a
// hint that counts only when an object is created (M0 Q3).
async function placeOnNode(
  env: Env,
  who: Who,
  o: { node?: string; cpus?: number; memoryMb?: number; size: SandboxSize },
): Promise<{ on?: NodeChoice; hint: DurableObjectLocationHint | null }> {
  if (!o.node && (o.cpus !== undefined || o.memoryMb !== undefined)) {
    fail(400, "cpus and memoryMb go with node: on Cloudflare, pick a size instead");
  }
  if (!o.node) return { hint: null };
  const found = await findNode(env.DB, o.node);
  if (!found) fail(404, `no node named ${o.node}`);
  const n = await live(env, found, "some");
  const placed = placeOn(n, { ownOnly: who.kind === "token" && who.token.ownOnly }, { size: o.size, cpus: o.cpus, memoryMb: o.memoryMb });
  if (refused(placed)) fail(placed.status as 400, placed.error);
  return { on: { node: n.id, name: n.name, cpus: placed.cpus, memoryMb: placed.memoryMb }, hint: n.locationHint as DurableObjectLocationHint | null };
}

app.post("/v1/sandboxes", async (c) => {
  const body = await c.req.text();
  const { node, cpus, memoryMb, ...input } = CreateSandbox.parse(body ? JSON.parse(body) : {});
  const { on, hint } = await placeOnNode(c.env, c.get("who"), { node, cpus, memoryMb, size: input.size });
  if (input.name && (await nameTaken(c.env.DB, input.name))) {
    return c.json({ error: `the name ${input.name} is taken` }, 409);
  }
  const id = newSandboxId();
  await setOwner(c.env.DB, id, c.get("who"));
  const stub = c.env.SANDBOX.getByName(id, hint ? { locationHint: hint } : undefined);
  try {
    return c.json(await stub.create(id, { ...input, name: input.name ?? null }, undefined, undefined, on), 201);
  } catch (e) {
    // A deploy (or a restart of its object) that cut the start off: the
    // sandbox is kept, in the state its object settled on.
    if (!on || errorStatus(e).status !== 500) throw e;
    const now = await stub.get().catch(() => null);
    if (!now) throw e;
    fail(503, `the deployment was updated or restarted while ${now.name ?? now.id} was starting on node ${on.name}; it's kept, in state ${now.state}: resume it, or delete it`);
  }
});

app.get("/v1/sandboxes", async (c) => {
  const who = c.get("who");
  return c.json(await listSandboxes(c.env.DB, who.kind === "token" && who.token.ownOnly ? ownerKey(who.token) : undefined));
});

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
  // force: a sandbox on a node that's offline goes now, and its node removes
  // the machine when it's back.
  const force = ["1", "true"].includes(c.req.query("force") ?? "");
  try {
    return c.json(await stub.remove(force));
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
      // A sandbox's file never runs as a page of this host, where the
      // dashboard's cookie works.
      "content-security-policy": "default-src 'none'; sandbox",
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
  const length = c.req.header("content-length");
  const bytes = length && /^\d{1,15}$/.test(length) ? Number(length) : undefined;
  const ref = c.req.param("ref");
  return c.json(await newMethod(ref, (await sandbox(c.env, ref)).stub.writeFile(path, root, mode, c.req.raw.body, bytes)));
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
  who: Who,
  names: (string | null)[],
  opts: { size: SandboxSize; idleMinutes: number },
  of: string,
  from?: ForkSource,
  save?: SaveManifest,
  place: { on?: NodeChoice; hint: DurableObjectLocationHint | null } = { hint: null },
) {
  return Promise.all(
    names.map(async (name) => {
      const id = newSandboxId();
      const copy = env.SANDBOX.getByName(id, place.hint ? { locationHint: place.hint } : undefined);
      await setOwner(env.DB, id, who);
      try {
        return await copy.create(id, { name, ...opts }, from, save, place.on);
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
  if (input.node || input.cpus !== undefined || input.memoryMb !== undefined) {
    fail(400, "a fork starts where Cloudflare keeps its disk snapshots, so not on a node: save the sandbox (cloudex save), then start sandboxes from the save on the node (cloudex new --from <save> --on <node>)");
  }
  const ref = c.req.param("ref");
  const names = await copyNames(c.env, input);
  const { stub } = await sandbox(c.env, ref);
  const original = await stub.get();
  const from = await newMethod(ref, stub.forkPoint());
  const opts = { size: input.size ?? original.size, idleMinutes: input.idleMinutes ?? original.idleMinutes };
  return c.json(await startCopies(c.env, c.get("who"), names, opts, original.id, from), 201);
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
  const place = await placeOnNode(c.env, c.get("who"), { node: input.node, cpus: input.cpus, memoryMb: input.memoryMb, size: input.size ?? save.size });
  const names = await copyNames(c.env, input);
  const opts = { size: input.size ?? save.size, idleMinutes: input.idleMinutes ?? save.idleMinutes };
  return c.json(await startCopies(c.env, c.get("who"), names, opts, `the save ${save.name}`, undefined, manifest, place), 201);
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

// The WebSockets below are checked only here, at the upgrade, so the Sandbox
// object is told which token opened one (its id, never the secret) and ends
// it when that token goes (sandbox.ts watchCredential). The admin key has no
// row to check.
function socketRequest(c: { req: { raw: Request }; get(k: "who"): Who }, to: string) {
  const fwd = new Request(to, c.req.raw);
  fwd.headers.delete(TOKEN_HEADER);
  const who = c.get("who");
  if (who.kind === "token") fwd.headers.set(TOKEN_HEADER, who.token.id);
  return fwd;
}

// A shell in the sandbox under a pty, over a WebSocket (sandbox.ts terminal).
app.get("/v1/sandboxes/:ref/terminal", async (c) => {
  const { stub } = await sandbox(c.env, c.req.param("ref"));
  const url = new URL(c.req.url);
  return stub.fetch(socketRequest(c, `http://sandbox/term${url.search}`));
});

// The desktop's VNC server over a WebSocket, for the dashboard's noVNC.
app.get("/v1/sandboxes/:ref/desktop/vnc", async (c) => {
  const { stub } = await sandbox(c.env, c.req.param("ref"));
  return stub.fetch(socketRequest(c, "http://sandbox/vnc"));
});

// WebSocket to a guest TCP port: `cloudex ssh` and `cloudex proxy` use port 22.
app.get("/v1/sandboxes/:ref/tcp/:port{[0-9]+}", async (c) => {
  const { stub } = await sandbox(c.env, c.req.param("ref"));
  const url = new URL(c.req.url);
  return stub.fetch(socketRequest(c, `http://sandbox/tcp/${c.req.param("port")}${url.search}`));
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

// Nodes (nodes.ts, node.ts): adding and removing are for admins (rules.ts).
// D1 has what each agent last reported; whether it's online is its Node
// object's to say.
// With details: the machine image as the agent last reported it, and the
// machines it runs that belong to no sandbox, and the sandboxes deleted while
// it was offline (GET /v1/nodes/{node}, and placing a sandbox).
async function live(env: Env, n: ApiNode, details: false | "some" | "all" = false): Promise<ApiNode> {
  if (n.state !== "online" && n.state !== "offline") return n;
  try {
    const s = await withTimeout(env.NODE.getByName(n.id).status(), 3000, "the node's status");
    const out: ApiNode = { ...n, state: s.online ? "online" : "offline", lastSeenAt: s.lastSeenAt ?? n.lastSeenAt };
    if (!details) return out;
    const image = s.hello?.facts.image;
    const more = { ...out, ...(image ? { image } : {}), ...(s.clockSkewSec !== null ? { clockSkewSec: s.clockSkewSec } : {}) };
    // Machines of other users' sandboxes are for admins only (2026-11-12 review, W5).
    if (details !== "all") return more;
    const mine = new Set(await sandboxesOnNode(env.DB, n.id));
    const tomb = new Set(s.tombstones);
    const orphans = (s.hello?.machines ?? []).map((m) => m.id).filter((id) => !mine.has(id) && !tomb.has(id));
    return { ...more, orphans, pendingDeletes: s.tombstones };
  } catch (e) {
    console.error(`[${n.id}] status: ${errorStatus(e).message}`);
    return n;
  }
}


app.post("/v1/nodes", async (c) => {
  const { name } = CreateNode.parse(await c.req.json());
  const node = await addNode(c.env.DB, name);
  const { token, expiresAt } = await issueJoinToken(c.env.DB, node.id);
  const origin = new URL(c.req.url).origin;
  return c.json({ node, joinToken: token, expiresAt, install: installCommand(origin, token, VERSION, c.env.NODE_AGENT ?? "") } satisfies NewNode, 201);
});

// live=false answers from the index alone, as each Node object last wrote
// it, without asking the objects: the dashboard polls that way, since an
// object kept busy drops its node's socket a few minutes after each deploy.
app.get("/v1/nodes", async (c) => {
  const rows = await listNodeRows(c.env.DB);
  if (["0", "false"].includes(c.req.query("live") ?? "")) return c.json(rows);
  return c.json(await Promise.all(rows.map((n) => live(c.env, n))));
});

// orphans, pendingDeletes and the round trip to the agent (which wakes the
// node's object and the agent) are for admins: other callers may see a node,
// not other users' sandbox ids, nor make it ping its agent (2026-11-12 review, W5).
app.get("/v1/nodes/:node", async (c) => {
  const admin = me(c.get("who")).role === "admin";
  const n = await live(c.env, await mustFindNode(c.env.DB, c.req.param("node")), admin ? "all" : "some");
  if (n.state !== "online" || !admin) return c.json(nodeView(n, admin));
  const t0 = Date.now();
  try {
    await c.env.NODE.getByName(n.id).op("ping", {}, 5000);
    return c.json({ ...n, rttMs: Date.now() - t0 });
  } catch (e) {
    console.error(`[${n.id}] ping: ${errorStatus(e).message}`);
    return c.json(n);
  }
});

app.delete("/v1/nodes/:node", async (c) => {
  const n = await mustFindNode(c.env.DB, c.req.param("node"));
  // A node that never joined, or was revoked, is forgotten.
  if (n.state === "revoked" || n.state === "pending") {
    await forgetNode(c.env.DB, n.id);
    return c.json({ deleted: n.id });
  }
  const force = ["1", "true"].includes(c.req.query("force") ?? "");
  if (n.machines > 0 && !force) {
    fail(409, `node ${n.name} still has ${n.machines} sandbox${n.machines === 1 ? "" : "es"}: delete ${n.machines === 1 ? "it" : "them"} first, or remove the node anyway with --force`);
  }
  const ids = await sandboxesOnNode(c.env.DB, n.id);
  // Removed anyway (force) while it's online: the machines go first, so
  // nothing is left running on a host that can't be reached any more.
  const deleted = new Set<string>();
  const online = ids.length > 0 && (await live(c.env, n)).state === "online";
  if (online) {
    await Promise.all(
      ids.map(async (id) => {
        try {
          await c.env.NODE.getByName(n.id).op("delete", { sandbox: id }, 90_000);
          deleted.add(id);
        } catch (e) {
          console.error(`[${n.id}] removing the machine of ${id}: ${errorStatus(e).message}`);
        }
      }),
    );
  }
  // D1 first, so its key stops working at once; then the socket closes, and
  // its sandboxes close the node's stream sockets, take no more, and show
  // that their node is gone.
  await revokeNode(c.env.DB, n.id);
  await c.env.NODE.getByName(n.id).revoke();
  for (const id of ids) {
    await c.env.SANDBOX.getByName(id)
      .nodeRevoked(n.id, deleted.has(id))
      .catch((e: unknown) => console.error(`[${id}] node revoked: ${errorStatus(e).message}`));
  }
  return c.json({ revoked: n.id });
});

// A sandbox's node streams (spares and open ones), where its object runs and
// its object's id, for the release checks and measurements (admins only:
// rules.ts leaves it to its default; not in the OpenAPI document).
app.get("/v1/sandboxes/:ref/node-streams", async (c) => {
  const s = await resolve(c.env, c.req.param("ref"));
  const out = s ? await s.stub.nodeStreams() : null;
  if (!out) fail(404, `no sandbox on a node named ${c.req.param("ref")}`);
  return c.json(out);
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
  const refusal = await keyRefusal(p, key);
  if (refusal) fail(400, refusal);
  return c.json(await setKey(c.env, p, key));
});

app.delete("/v1/keys/:provider", async (c) => {
  const p = provider(c.req.param("provider"));
  if (!(await deleteKey(c.env.DB, p))) return c.json({ error: `no ${p} key is set` }, 404);
  return c.json({ deleted: p });
});

app.all("/v1/*", (c) => c.json({ error: "not found" }, 404));

// The dashboard (dashboard.ts) is everything else on this host.
app.on(["GET", "HEAD"], "*", (c) => serveDashboard(c.req.raw));

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
  fwd.headers.delete(TOKEN_HEADER);
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
