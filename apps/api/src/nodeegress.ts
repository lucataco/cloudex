// What a sandbox on a node sends to cloudex.internal and to the AI providers
// (v0.2.2 M6). Its node's agent ends the machine's connection (go/internal/
// node/egress.go) and passes the request on to /v1/node/egress?target=<url>,
// signed with the node's key and naming the sandbox it came from. Here:
//
// 1. the signature is checked (nodeauth.ts); the sandbox it names is part of
//    what was signed;
// 2. the sandbox's object says which node it's on, its run's token for its
//    own API, and whether it runs (nodeEgress in sandbox.ts), asked every
//    time, so that a stop, a restart (a new token) or the node's removal
//    counts at once;
// 3. a node that isn't the sandbox's own, a sandbox that isn't running, or a
//    run without a token is refused;
// 4. the target is cloudex.internal over HTTP or a provider host over HTTPS,
//    nothing else, and the request goes to Egress (egress.ts) exactly as a
//    Cloudflare container's would, with the sandbox's id, its token and the
//    node's id as props, so the grants Egress checks (saveGrant, the self
//    token) hold as they do on Cloudflare. The real provider key is added
//    there, so it never leaves the Worker.
//
// Kept apart from index.ts so that it can be tested without the runtime.
import { providerForHost } from "./keys";
import { BAD_SIGNATURE, HEADERS, verifyNodeRequest } from "./nodeauth";

// What a sandbox's object says about itself to the route (nodeEgress).
export type NodeEgressInfo = { node: string | null; selfToken: string | null; state: string; revoked: boolean };

export type EgressDeps = {
  publicKeyOf(node: string): Promise<string | null>;
  info(sandbox: string): Promise<NodeEgressInfo | null>;
  egress(props: { id: string; selfToken: string; node: string }): { fetch(req: Request): Promise<Response> };
  now?: number;
};

const json = (status: number, error: string) => Response.json({ type: "error", error: { type: "cloudex_error", message: error }, message: error }, { status });

// The URL a request is for: http://cloudex.internal/... or https://<a
// provider's host>/..., with no port and no user; null for anything else.
export function egressTarget(raw: string | null | undefined): URL | null {
  if (!raw || raw.length > 8192) return null;
  let u: URL;
  try {
    u = new URL(raw);
  } catch {
    return null;
  }
  if (u.username || u.password || u.port || u.hash) return null;
  if (u.protocol === "http:" && u.hostname === "cloudex.internal") return u;
  if (u.protocol === "https:" && providerForHost(u.hostname)) return u;
  return null;
}

// Headers the edge adds to the agent's request, and the node's own, none of
// which go on to Egress or a provider.
const EDGE = new Set(["cf-connecting-ip", "cf-connecting-ipv6", "cf-ipcountry", "cf-ray", "cf-visitor", "cf-worker", "cf-ew-via", "cdn-loop", "x-forwarded-for", "x-forwarded-proto", "x-real-ip", "true-client-ip"]);

export function guestHeaders(h: Headers): Headers {
  const out = new Headers();
  for (const [k, v] of h) {
    if (k.startsWith("x-cx-") || EDGE.has(k)) continue;
    out.append(k, v);
  }
  return out;
}

// The node's public key, kept for a minute per isolate: one D1 read per node
// a minute, not per request. A removed node's requests still fail at once,
// because its sandboxes say so (revoked).
const keys = new Map<string, { key: string | null; at: number }>();
export function cachedKey(lookup: (node: string) => Promise<string | null>, now = Date.now()) {
  return async (node: string) => {
    const hit = keys.get(node);
    if (hit && hit.key && now - hit.at < 60_000) return hit.key;
    const key = await lookup(node);
    keys.set(node, { key, at: now });
    if (keys.size > 1000) keys.delete(keys.keys().next().value!);
    return key;
  };
}

export async function nodeEgress(req: Request, deps: EgressDeps): Promise<Response> {
  const v = await verifyNodeRequest(req, deps.publicKeyOf, deps.now);
  if (!("signed" in v)) return json(v.status, v.error);
  const { node, sandbox } = v.signed;
  if (!sandbox) return json(400, `a node's guest request names its sandbox in ${HEADERS.sandbox}`);
  const target = egressTarget(new URL(req.url).searchParams.get("target"));
  if (!target) return json(400, "a node's guest request goes to http://cloudex.internal or to a provider's host the deployment holds a key for, and nowhere else");
  const info = await deps.info(sandbox).catch(() => null);
  // Another node's sandbox, a Cloudflare one or none at all look the same.
  if (!info || info.node !== node) return json(403, `forbidden: ${sandbox} isn't a sandbox on this node`);
  if (info.revoked) return json(401, BAD_SIGNATURE);
  if (info.state !== "running" && info.state !== "starting") return json(409, `${sandbox} isn't running`);
  if (!info.selfToken) return json(403, `${sandbox}'s run has no token for its own API; restart it`);
  const method = req.method.toUpperCase();
  const body = method === "GET" || method === "HEAD" ? null : req.body;
  const inner = new Request(target, { method, headers: guestHeaders(req.headers), body, redirect: "manual", duplex: "half" } as RequestInit);
  return deps.egress({ id: sandbox, selfToken: info.selfToken, node }).fetch(inner);
}
