// What the admin API (index.ts) and the API a sandbox gets for itself
// (self.ts) share: error answers, and the calls behind the routes both have.
import type { Context } from "hono";
import { ZodError } from "zod";
import type { HostLink, RevokedLinks } from "@cloudex/schema";
import { errorStatus, fail, previewToken, previewUrl } from "./util";

export function onError(e: Error, c: Context) {
  if (e instanceof ZodError) {
    // A bad record key (an environment variable's name, a file's path) carries its reason inside.
    const why = (i: ZodError["issues"][number]) => (i.code === "invalid_key" && i.issues[0]?.message) || i.message;
    return c.json({ error: e.issues.map((i) => `${i.path.join(".") || "body"}: ${why(i)}`).join("; ") }, 400);
  }
  if (e instanceof SyntaxError) return c.json({ error: "the request body isn't valid JSON" }, 400);
  const { status, message } = errorStatus(e);
  if (status >= 500) console.error(message);
  return c.json({ error: message }, status as 400);
}

// A sandbox that's busy (a connection open, a command running) during a deploy
// keeps its old Durable Object code for 3-5 minutes, so it may not have
// methods added since.
export async function newMethod<T>(ref: string, call: Promise<T>): Promise<T> {
  try {
    return await call;
  } catch (e) {
    if (e instanceof Error && /does not implement/.test(e.message)) {
      fail(409, `${ref} is still running code from before this deployment was updated, so it can't do this yet. A busy sandbox moves to the new code within about 5 minutes; try again then`);
    }
    throw e;
  }
}

type Stub = DurableObjectStub<import("./sandbox").Sandbox>;

function zone(env: Env) {
  if (!env.PREVIEW_ZONE) fail(400, "preview links need a domain: run cloudex deploy --preview-domain <your domain>");
  return env.PREVIEW_ZONE;
}

export async function hostLink(env: Env, id: string, stub: Stub, port: number): Promise<{ url: string }> {
  const z = zone(env);
  try {
    const { token } = await stub.link(port);
    return { url: previewUrl(z, id, port, token) };
  } catch (e) {
    if (!(e instanceof Error && /does not implement/.test(e.message))) throw e;
  }
  // A sandbox still on code from before links could be revoked has revoked
  // none, so its links are the HMAC of sandbox and port alone.
  await stub.get();
  return { url: previewUrl(z, id, port, await previewToken(env.ADMIN_KEY, id, port)) };
}

export async function hostLinks(env: Env, id: string, stub: Stub): Promise<HostLink[]> {
  const z = zone(env);
  const links = await newMethod(id, stub.links());
  return links.map((l) => ({ port: l.port, url: previewUrl(z, id, l.port, l.token), createdAt: l.createdAt }));
}

export function revokeLinks(id: string, stub: Stub, port?: number): Promise<RevokedLinks> {
  return newMethod(id, stub.revokeLinks(port));
}

export function portParam(text: string) {
  const port = Number(text);
  if (!Number.isInteger(port) || port < 1 || port > 65535) fail(400, `bad port ${text}`);
  return port;
}
