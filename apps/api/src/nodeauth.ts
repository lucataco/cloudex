// How a node's agent proves who it is. Nodes carry no bearer token: every
// request after joining is signed with the node's own Ed25519 key, which it
// made itself and never sends. The Worker holds only the public key (D1
// nodes.public_key), and revoking a node clears it.
//
// The signed string is METHOD\nPATH?QUERY\nSANDBOX\nTS\nNONCE: the request's
// method and path with its query as sent, the sandbox it acts on (empty for
// join and connect), the time in unix seconds, and 32 random hex digits. The
// Worker allows 60 s of clock skew either way. A nonce on connect works once:
// the Node object keeps the ones it has seen for two minutes, which outlasts
// the skew window (see rememberNonce).
//
// Join tokens are cxj_ and 40 base32 characters, made by POST /v1/nodes. D1
// keeps their SHA-256, and a join uses one in a single UPDATE, so two joins
// racing with the same token can't both win. They work for 30 minutes.
//
// To a caller that isn't a node, every failure looks the same: an unknown
// node, a revoked one and a bad signature all get one answer, after the same
// work, so the routes can't be used to find out which nodes exist.
import { JOIN_TOKEN } from "@cloudex/schema/node";
import { sha256 } from "./tokens";

export const SKEW_SEC = 60;
export const NONCE_TTL_MS = 2 * 60_000;
export const JOIN_TTL_MS = 30 * 60_000;

export const HEADERS = {
  node: "x-cx-node",
  ts: "x-cx-ts",
  nonce: "x-cx-nonce",
  sig: "x-cx-sig",
  sandbox: "x-cx-sandbox",
  protocol: "x-cx-protocol",
} as const;

const NODE_ID = /^nd-[a-z0-9]{8}$/;
const NONCE = /^[0-9a-f]{32}$/;
const TS = /^[0-9]{1,12}$/;
const SIG = /^[A-Za-z0-9+/]{86}==$/;
const PUBLIC_KEY = /^[A-Za-z0-9+/]{43}=$/;

export const signingString = (method: string, pathQuery: string, sandbox: string, ts: string, nonce: string) =>
  `${method}\n${pathQuery}\n${sandbox}\n${ts}\n${nonce}`;

export type Signed = { node: string; ts: string; nonce: string; sig: string; sandbox: string };

// What a request claims, if it's well formed; null otherwise.
export function readSigned(headers: Headers, needNode = true): Signed | null {
  const node = headers.get(HEADERS.node) ?? "";
  const ts = headers.get(HEADERS.ts) ?? "";
  const nonce = headers.get(HEADERS.nonce) ?? "";
  const sig = headers.get(HEADERS.sig) ?? "";
  const sandbox = headers.get(HEADERS.sandbox) ?? "";
  if ((needNode && !NODE_ID.test(node)) || !TS.test(ts) || !NONCE.test(nonce) || !SIG.test(sig)) return null;
  if (sandbox !== "" && !/^cx-[a-z0-9]{6}$/.test(sandbox)) return null;
  return { node, ts, nonce, sig, sandbox };
}

// How far off the node's clock is, in seconds, when that's more than allowed; null when it's fine.
export function skew(ts: string, nowMs: number): number | null {
  const off = Number(ts) - Math.floor(nowMs / 1000);
  return Math.abs(off) > SKEW_SEC ? off : null;
}

const b64 = (s: string) => Uint8Array.from(atob(s), (c) => c.charCodeAt(0));

export async function verifyEd25519(publicKey: string, message: string, sig: string): Promise<boolean> {
  if (!PUBLIC_KEY.test(publicKey) || !SIG.test(sig)) return false;
  try {
    const key = await crypto.subtle.importKey("raw", b64(publicKey), { name: "Ed25519" }, false, ["verify"]);
    return await crypto.subtle.verify("Ed25519", key, b64(sig), new TextEncoder().encode(message));
  } catch {
    return false;
  }
}

// An unknown node's request is checked against this key, so that it takes as
// long as a known one's. It's RFC 8032's first test key, whose private half is
// public, so a match never counts (see key === null below).
const NOBODY = "11qYAYKxCrfVS/7TyWQHOg7hcvPapiMlrwIaaPcHURo=";

export type Refusal = { status: 401 | 400; error: string };

export const BAD_SIGNATURE = "unauthorized: the node's signature didn't check out (an unknown or removed node, or another key)";

// Checks a request signed by an existing node. publicKeyOf looks the node's
// key up, null when there's no such node or it was revoked.
export async function verifyNodeRequest(
  req: Request,
  publicKeyOf: (node: string) => Promise<string | null>,
  nowMs = Date.now(),
): Promise<{ signed: Signed } | Refusal> {
  const signed = readSigned(req.headers);
  if (!signed) return { status: 401, error: "unauthorized: a node request needs the headers x-cx-node, x-cx-ts, x-cx-nonce and x-cx-sig" };
  const off = skew(signed.ts, nowMs);
  if (off !== null) return { status: 401, error: clockError(off) };
  const key = await publicKeyOf(signed.node);
  const url = new URL(req.url);
  const message = signingString(req.method, url.pathname + url.search, signed.sandbox, signed.ts, signed.nonce);
  const ok = await verifyEd25519(key ?? NOBODY, message, signed.sig);
  if (!ok || key === null) return { status: 401, error: BAD_SIGNATURE };
  return { signed };
}

export const clockError = (off: number) =>
  `unauthorized: the node's clock is ${Math.abs(off)} s ${off > 0 ? "ahead" : "behind"}; more than ${SKEW_SEC} s is refused. Set its time with NTP`;

// A join, signed with the key it brings: the agent proves it holds the key.
export async function verifyJoinSignature(req: Request, publicKey: string, nowMs = Date.now()): Promise<Refusal | null> {
  const signed = readSigned(req.headers, false);
  if (!signed) return { status: 401, error: "unauthorized: a join needs the headers x-cx-ts, x-cx-nonce and x-cx-sig" };
  const off = skew(signed.ts, nowMs);
  if (off !== null) return { status: 401, error: clockError(off) };
  const url = new URL(req.url);
  const ok = await verifyEd25519(publicKey, signingString(req.method, url.pathname + url.search, "", signed.ts, signed.nonce), signed.sig);
  return ok ? null : { status: 401, error: "unauthorized: the join isn't signed with the key it brings" };
}

// Nonces seen on connect. The store is the Node object's SQLite (node.ts);
// insert answers false when the nonce is there already.
export type NonceStore = { insert(nonce: string, at: number): boolean; prune(before: number): void };

export function rememberNonce(store: NonceStore, nonce: string, nowMs: number): boolean {
  store.prune(nowMs - NONCE_TTL_MS);
  return store.insert(nonce, nowMs);
}

// ---------------------------------------------------------------- join tokens

const BASE32 = "abcdefghijklmnopqrstuvwxyz234567";
export const newJoinToken = () => `cxj_${[...crypto.getRandomValues(new Uint8Array(40))].map((b) => BASE32[b & 31]).join("")}`;

export const isJoinToken = (t: string) => JOIN_TOKEN.test(t);

// A request's body as text, or null when it's more than max bytes: what an
// unauthenticated caller can make the Worker read is bounded.
export async function readCapped(req: Request, max: number): Promise<string | null> {
  if (Number(req.headers.get("content-length") ?? "0") > max) return null;
  if (!req.body) return "";
  const reader = req.body.getReader();
  const chunks: Uint8Array[] = [];
  let size = 0;
  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    size += value.byteLength;
    if (size > max) {
      await reader.cancel().catch(() => {});
      return null;
    }
    chunks.push(value);
  }
  const all = new Uint8Array(size);
  let at = 0;
  for (const c of chunks) {
    all.set(c, at);
    at += c.byteLength;
  }
  return new TextDecoder().decode(all);
}

export async function issueJoinToken(db: D1Database, nodeId: string, nowMs = Date.now()) {
  const token = newJoinToken();
  const expiresAt = nowMs + JOIN_TTL_MS;
  await db.batch([
    // A node has one token at a time, and spent ones go.
    db.prepare("DELETE FROM node_joins WHERE node_id = ? OR expires_at < ?").bind(nodeId, nowMs),
    db.prepare("INSERT INTO node_joins (token_hash, node_id, expires_at, used_at) VALUES (?, ?, ?, NULL)").bind(await sha256(token), nodeId, expiresAt),
  ]);
  return { token, expiresAt };
}

// The node a token joins, using it up; null for a token that's malformed,
// unknown, used or expired. One statement, so it's used at most once.
export async function useJoinToken(db: D1Database, token: string, nowMs = Date.now()): Promise<string | null> {
  if (!isJoinToken(token)) return null;
  const row = await db
    .prepare("UPDATE node_joins SET used_at = ? WHERE token_hash = ? AND used_at IS NULL AND expires_at > ? RETURNING node_id")
    .bind(nowMs, await sha256(token), nowMs)
    .first<{ node_id: string }>();
  return row?.node_id ?? null;
}
