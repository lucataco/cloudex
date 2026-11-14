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
// - Sessions. Each ticket names the token that asked for it, and the agent
//   keeps that with the session. The object keeps the tokens that minted
//   tickets (DirectTokens below), and the agent reports the open sessions for
//   the sandbox with their tokens (the direct message, through the Node
//   object); the object's alarm checks those tokens as it checks a splice's,
//   and asks the agent to end every session of one that ended (the directEnd
//   op, by token).
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

export type DirectClaims = { node: string; sandbox: string; port: number; session: string; token: string; exp: number };

const SESSION = /^[0-9a-f]{16}$/;
const NODE_ID = /^nd-[a-z0-9]{8}$/;
const SANDBOX_ID = /^cx-[a-z0-9]{6}$/;
// The token claim: a token's id, or "" for the admin key (which has no row).
const TOKEN_CLAIM = /^(tk-[a-z0-9]{8})?$/;

export const newSession = () => [...crypto.getRandomValues(new Uint8Array(8))].map((b) => b.toString(16).padStart(2, "0")).join("");

const secretKey = (secret: string, usages: ("sign" | "verify")[]) => hmacKey(Uint8Array.from(atob(secret), (c) => c.charCodeAt(0)), usages);

// A ticket for one connection to port on sandbox, on node, asked for by token
// (its id; null or "" for the admin key), good for DIRECT_TICKET_SEC from
// nowMs. session is chosen here unless given (tests). The token is in the
// claims so that the agent can end the sessions of a token that ended on its
// own evidence (review D-M1), whatever the object remembers.
export async function mintDirectTicket(
  secret: string,
  c: { node: string; sandbox: string; port: number; token: string | null; session?: string },
  nowMs = Date.now(),
): Promise<{ ticket: string; session: string; exp: number }> {
  const session = c.session ?? newSession();
  const exp = Math.floor(nowMs / 1000) + DIRECT_TICKET_SEC;
  const payload = b64url(enc.encode(JSON.stringify({ n: c.node, s: c.sandbox, p: c.port, i: session, t: c.token ?? "", e: exp })));
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
    if (Object.keys(p).sort().join() !== "e,i,n,p,s,t") return null;
    const { n, s, p: port, i, t, e } = p;
    if (typeof n !== "string" || !NODE_ID.test(n) || typeof s !== "string" || !SANDBOX_ID.test(s)) return null;
    if (typeof i !== "string" || !SESSION.test(i) || typeof e !== "number" || !Number.isInteger(e)) return null;
    if (typeof t !== "string" || !TOKEN_CLAIM.test(t)) return null;
    if (typeof port !== "number" || !Number.isInteger(port) || port < 1 || port > 65535) return null;
    if (e * 1000 <= nowMs) return null;
    return { node: n, sandbox: s, port, session: i, token: t, exp: e };
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

// ---------------------------------------------------------------- tokens (review D-M1)

// The object keeps, per token (never the admin key), what it needs to end
// that token's direct sessions when it ends: m, the expiries (unix s) of its
// tickets for this sandbox that haven't expired, which also caps how many it
// may have; s, the last evidence it may have a session open (ms): a mint, or
// an agent's report naming it.
//
// Before the review the object kept each session instead, at most 256, and
// dropped the oldest unopened one to mint more; ids it didn't remember were
// ignored in reports. So a token could mint 256 more tickets after its first,
// use the first, and keep that session after it was revoked; and a lost
// "open" report (the node over its report budget, its link down) let a quiet
// session be forgotten 3 minutes after its mint. Now nothing is dropped to
// make room (a mint over the caps is refused instead), the tokens in a report
// are taken whether or not the object remembers them, an entry lasts an hour
// after its last evidence (the agent reports quiet sessions every 5 minutes,
// and again after each welcome), and the agent ends sessions by token.
export type DirectToken = { m: number[]; s: number };
export type DirectTokens = Record<string, DirectToken>;

// Tickets a token may hold for one sandbox that haven't expired: as many as
// the sandbox may have sessions open on the direct port.
export const TOKEN_TICKETS_MAX = DIRECT_SESSIONS_MAX;
// Tokens one sandbox mints for while their entries last; reports may add as
// many again (an agent names at most DIRECT_SESSIONS_MAX open sessions), so
// the record stays bounded at twice this.
export const TOKENS_MINT_MAX = 64;
// An entry is forgotten this long after its last evidence.
export const TOKEN_FORGET_MS = 60 * 60_000;
// A report renews s only when it's older than this, so that the reports of a
// long session don't each write the object's storage.
export const SEEN_RENEW_MS = 5 * 60_000;

// A mint for token (null: the admin key, which isn't kept) whose ticket
// expires at exp: the new record, or why it's refused (429).
export function mintToken(all: DirectTokens, token: string | null, exp: number, nowMs: number): DirectTokens | string {
  if (token === null) return all;
  const cur = all[token];
  const live = (cur?.m ?? []).filter((e) => e * 1000 > nowMs);
  if (live.length >= TOKEN_TICKETS_MAX) return `this token has ${TOKEN_TICKETS_MAX} direct tickets for the sandbox from the last minute; wait a moment`;
  if (!cur && Object.keys(all).length >= TOKENS_MINT_MAX) return `${TOKENS_MINT_MAX} tokens have used the direct path to this sandbox in the last hour`;
  return { ...all, [token]: { m: [...live, exp], s: nowMs } };
}

// An agent's report naming the tokens of the sandbox's open sessions: each is
// kept, known or not, with its evidence renewed.
export function reportTokens(all: DirectTokens, tokens: string[], nowMs: number): DirectTokens {
  let next = all;
  for (const t of new Set(tokens)) {
    const cur = next[t];
    if (cur && nowMs - cur.s < SEEN_RENEW_MS) continue;
    next = { ...next, [t]: { m: cur?.m ?? [], s: nowMs } };
  }
  return next;
}

// Drops expired ticket times, and tokens with no evidence for TOKEN_FORGET_MS.
export function sweepTokens(all: DirectTokens, nowMs: number): DirectTokens {
  const next: DirectTokens = {};
  for (const [t, e] of Object.entries(all)) {
    if (nowMs - e.s >= TOKEN_FORGET_MS) continue;
    const m = e.m.filter((x) => x * 1000 > nowMs);
    next[t] = m.length === e.m.length ? e : { ...e, m };
  }
  return next;
}

// The tokens that ended, among those kept. alive answers whether a token
// still works; a failed check counts as alive until the next one.
export async function endedTokens(all: DirectTokens, alive: (token: string) => Promise<boolean>): Promise<string[]> {
  const out: string[] = [];
  for (const t of Object.keys(all)) if (!(await alive(t).catch(() => true))) out.push(t);
  return out;
}

export const sameTokens = (a: DirectTokens, b: DirectTokens) => JSON.stringify(a) === JSON.stringify(b);
