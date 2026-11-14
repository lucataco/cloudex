// The direct path (v0.2.3): ssh and other TCP connections straight from a
// client to a node's agent over the LAN or a tailnet, with the Worker path as
// the fallback. The protocol is in @cloudex/schema/node ("the direct path").
//
// - The secret. Each node has its own, derived from ADMIN_KEY, its id and its
//   public key (directSecret), and sent to its agent in welcome at every
//   connect. Nothing is stored: a node joined again has a new id and key, so
//   a new secret; a new ADMIN_KEY changes every secret, and each agent gets
//   its new one at its next welcome (tickets then in flight fail, and the CLI
//   falls back to the Worker path).
// - Tickets. The sandbox's object mints one per GET /v1/sandboxes/{ref}/direct
//   (sandbox.ts direct), for one node, sandbox, port and session, good for 60 s.
//   The agent checks it with the secret it holds, so a node whose link to
//   Cloudflare blinks stays reachable directly. go/internal/direct checks the
//   same format, against the vectors in fixtures/node/protocol.json.
// - Sessions. The object keeps each ticket's session with the token that
//   asked for it (DirectSessions below). The agent reports the open ones for
//   the sandbox (the direct message, through the Node object); the object's
//   alarm checks their tokens as it checks a splice's, and asks the agent to
//   end those whose token ended (the directEnd op). Sessions it minted and
//   never saw open are forgotten 2 minutes after their ticket expired.
// - The node's key and addresses come from D1 (nodes.public_key and
//   nodes.direct), never from the Node object, which mustn't be woken per
//   connection (research/v0.2.3-plan.md, item 9).
import { DIRECT_SESSIONS_MAX, DIRECT_TICKET_MAX, DIRECT_TICKET_SEC, NodeDirect } from "@cloudex/schema/node";

const enc = new TextEncoder();
const b64url = (b: Uint8Array) => btoa(String.fromCharCode(...b)).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
const unb64url = (s: string) => {
  if (!/^[A-Za-z0-9_-]*$/.test(s)) throw new Error("not base64url");
  return Uint8Array.from(atob(s.replace(/-/g, "+").replace(/_/g, "/")), (c) => c.charCodeAt(0));
};
const b64 = (b: Uint8Array) => btoa(String.fromCharCode(...b));

const hmacKey = (raw: Uint8Array | ArrayBuffer, usages: ("sign" | "verify")[]) => crypto.subtle.importKey("raw", raw, { name: "HMAC", hash: "SHA-256" }, false, usages);

export const DIRECT_LABEL = "cloudex node direct tickets v1";

const derived = new Map<string, Promise<CryptoKey>>();
// A key of its own for direct secrets, derived from the admin key (as the
// stream tickets' key is, under another label).
function rootKey(adminKey: string): Promise<CryptoKey> {
  let k = derived.get(adminKey);
  if (!k) {
    k = (async () => {
      const root = await hmacKey(enc.encode(adminKey), ["sign"]);
      return hmacKey(await crypto.subtle.sign("HMAC", root, enc.encode(DIRECT_LABEL)), ["sign"]);
    })();
    derived.set(adminKey, k);
  }
  return k;
}

// The node's secret for direct tickets: HMAC-SHA256 under the derived key,
// over "<node id>\n<public key>". 32 bytes, in base64 as welcome carries it.
export async function directSecret(adminKey: string, node: string, publicKey: string): Promise<string> {
  const mac = await crypto.subtle.sign("HMAC", await rootKey(adminKey), enc.encode(`${node}\n${publicKey}`));
  return b64(new Uint8Array(mac));
}

export type DirectClaims = { node: string; sandbox: string; port: number; session: string; exp: number };

const SESSION = /^[0-9a-f]{16}$/;
const NODE_ID = /^nd-[a-z0-9]{8}$/;
const SANDBOX_ID = /^cx-[a-z0-9]{6}$/;

export const newSession = () => [...crypto.getRandomValues(new Uint8Array(8))].map((b) => b.toString(16).padStart(2, "0")).join("");

const secretKey = (secret: string, usages: ("sign" | "verify")[]) => hmacKey(Uint8Array.from(atob(secret), (c) => c.charCodeAt(0)), usages);

// A ticket for one connection to port on sandbox, on node, good for
// DIRECT_TICKET_SEC from nowMs. session is chosen here unless given (tests).
export async function mintDirectTicket(
  secret: string,
  c: { node: string; sandbox: string; port: number; session?: string },
  nowMs = Date.now(),
): Promise<{ ticket: string; session: string; exp: number }> {
  const session = c.session ?? newSession();
  const exp = Math.floor(nowMs / 1000) + DIRECT_TICKET_SEC;
  const payload = b64url(enc.encode(JSON.stringify({ n: c.node, s: c.sandbox, p: c.port, i: session, e: exp })));
  const mac = new Uint8Array(await crypto.subtle.sign("HMAC", await secretKey(secret, ["sign"]), enc.encode(payload)));
  return { ticket: `${payload}.${b64url(mac)}`, session, exp };
}

// The ticket's claims, if its MAC checks out under secret, its claims are
// well formed, and it hasn't expired at nowMs; null otherwise. The agent does
// the same in Go (go/internal/direct), and also checks the node, the sandbox,
// the port and that the session wasn't used before.
export async function readDirectTicket(secret: string, ticket: string, nowMs = Date.now()): Promise<DirectClaims | null> {
  if (ticket.length > DIRECT_TICKET_MAX) return null;
  const [payload, mac, extra] = ticket.split(".");
  if (!payload || !mac || extra !== undefined) return null;
  try {
    const ok = await crypto.subtle.verify("HMAC", await secretKey(secret, ["verify"]), unb64url(mac), enc.encode(payload));
    if (!ok) return null;
    const p = JSON.parse(new TextDecoder().decode(unb64url(payload))) as Record<string, unknown>;
    if (Object.keys(p).sort().join() !== "e,i,n,p,s") return null;
    const { n, s, p: port, i, e } = p;
    if (typeof n !== "string" || !NODE_ID.test(n) || typeof s !== "string" || !SANDBOX_ID.test(s)) return null;
    if (typeof i !== "string" || !SESSION.test(i) || typeof e !== "number" || !Number.isInteger(e)) return null;
    if (typeof port !== "number" || !Number.isInteger(port) || port < 1 || port > 65535) return null;
    if (e * 1000 <= nowMs) return null;
    return { node: n, sandbox: s, port, session: i, exp: e };
  } catch {
    return null;
  }
}

// ---------------------------------------------------------------- candidates

// RFC 1918, and 100.64/10 (shared address space, which holds the tailnet).
export function privateOrShared(ip: string): boolean {
  const m = /^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/.exec(ip);
  if (!m) return false;
  const [a, b] = [Number(m[1]), Number(m[2])];
  if ([a, b, Number(m[3]), Number(m[4])].some((x) => x > 255)) return false;
  return a === 10 || (a === 172 && b >= 16 && b <= 31) || (a === 192 && b === 168) || (a === 100 && b >= 64 && b <= 127);
}

// What a client may try, from the node's direct fact as D1 holds it (JSON or
// null): null unless the agent reports listening, and only the private and
// tailnet addresses among those it reports, whatever a node says.
export function directCandidates(text: string | null): { port: number; addrs: string[] } | null {
  if (!text) return null;
  let d: NodeDirect;
  try {
    const r = NodeDirect.safeParse(JSON.parse(text));
    if (!r.success) return null;
    d = r.data;
  } catch {
    return null;
  }
  if (d.state !== "listening") return null;
  const addrs = [...new Set(d.addrs.filter(privateOrShared))];
  return addrs.length ? { port: d.port, addrs } : null;
}

// ---------------------------------------------------------------- refusals

// Why the sandbox's object won't mint a ticket, as serve() refuses a tcp
// connection: it isn't on the node the index named ("moved": the index is
// behind; 409), its node was removed ("removed", 410), or it isn't running
// ("not-running", 409, with the state's own message).
export type DirectRefusal = "moved" | "removed" | "not-running";
export function directRefusal(s: { state: string; onNode: string | null; asked: string; revokedNode: string | null }): DirectRefusal | null {
  if (s.onNode === null || s.onNode !== s.asked) return "moved";
  if (s.revokedNode === s.onNode) return "removed";
  if (s.state !== "running") return "not-running";
  return null;
}

// ---------------------------------------------------------------- sessions

// Each session the sandbox's object minted: t, the token that asked (null:
// the admin key, which has no row to check); e, its ticket's expiry (unix s);
// o, when the agent last reported it open (ms), absent until then.
export type DirectSession = { t: string | null; e: number; o?: number };
export type DirectSessions = Record<string, DirectSession>;

// Minted sessions not seen open are forgotten this long after their ticket
// expired; open ones that no report has named for OPEN_FORGET_MS (a node away
// that long) are forgotten too, so that the record stays bounded.
export const MINTED_GRACE_MS = 2 * 60_000;
export const OPEN_FORGET_MS = 60 * 60_000;
// The most sessions one sandbox's object keeps: minting more drops the oldest
// that aren't open.
export const SESSIONS_KEPT = 4 * DIRECT_SESSIONS_MAX;

export function mintSession(all: DirectSessions, id: string, s: DirectSession): DirectSessions {
  const next = { ...all, [id]: s };
  const ids = Object.keys(next);
  if (ids.length <= SESSIONS_KEPT) return next;
  const unopened = ids.filter((i) => next[i]!.o === undefined && i !== id).sort((a, b) => next[a]!.e - next[b]!.e);
  for (const i of unopened.slice(0, ids.length - SESSIONS_KEPT)) delete next[i];
  return next;
}

// An agent's report of the sandbox's open sessions: those it names that were
// minted here are open now; those open before that it no longer names have
// closed and go. Ids this object didn't mint are ignored.
// o is renewed only when it's older than SEEN_RENEW_MS, so that the 30 s
// reports of a long session don't each write the object's storage.
export const SEEN_RENEW_MS = 5 * 60_000;
export function reportSessions(all: DirectSessions, open: string[], nowMs: number): DirectSessions {
  const named = new Set(open);
  const next: DirectSessions = {};
  for (const [id, s] of Object.entries(all)) {
    if (named.has(id)) next[id] = s.o !== undefined && nowMs - s.o < SEEN_RENEW_MS ? s : { ...s, o: nowMs };
    else if (s.o === undefined) next[id] = s;
  }
  return next;
}

// Drops minted sessions whose ticket expired more than MINTED_GRACE_MS ago,
// and open ones not reported for OPEN_FORGET_MS.
export function sweepSessions(all: DirectSessions, nowMs: number): DirectSessions {
  const next: DirectSessions = {};
  for (const [id, s] of Object.entries(all)) {
    const keep = s.o === undefined ? s.e * 1000 + MINTED_GRACE_MS > nowMs : nowMs - s.o < OPEN_FORGET_MS;
    if (keep) next[id] = s;
  }
  return next;
}

// The sessions to end because their token ended: open ones, and minted ones
// whose ticket could still be used (the agent refuses those when it's told).
// alive answers whether a token still works; each token is asked once.
export async function endedSessions(all: DirectSessions, alive: (token: string) => Promise<boolean>, nowMs: number): Promise<string[]> {
  const asked = new Map<string, Promise<boolean>>();
  const out: string[] = [];
  for (const [id, s] of Object.entries(all)) {
    if (s.t === null) continue;
    if (s.o === undefined && s.e * 1000 <= nowMs) continue;
    let a = asked.get(s.t);
    if (!a) asked.set(s.t, (a = alive(s.t).catch(() => true)));
    if (!(await a)) out.push(id);
  }
  return out;
}

export const sameSessions = (a: DirectSessions, b: DirectSessions) => JSON.stringify(a) === JSON.stringify(b);
