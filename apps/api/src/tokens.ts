// Access tokens (Token in @cloudex/schema): API keys other than the admin key,
// each with a role, maybe limited to the sandboxes it makes (ownOnly), maybe
// expiring. D1 holds the SHA-256 of each secret, never the secret, so a copy
// of the database doesn't let anyone in. Revoking deletes the row, which every
// request reads, so it takes effect at once.
//
// A secret is cx_<8 characters of id>_<32 random characters>: the id finds the
// row, and the hash of the whole secret must match it.
//
// What each route needs is in RULES below, matched by the middleware in
// index.ts before any route runs; a route that isn't listed is for admins.
import type { Me, Role, Token } from "@cloudex/schema";
import { ensureSchema } from "./db";
import { type Need, ruleFor } from "./rules";
import { fail, sameSecret } from "./util";

export type Who =
  | { kind: "admin" }
  | { kind: "token"; token: Token };

const BASE32 = "abcdefghijklmnopqrstuvwxyz234567";

// Unbiased: 32 letters, 5 bits from each byte.
function random32(n: number) {
  return [...crypto.getRandomValues(new Uint8Array(n))].map((b) => BASE32[b & 31]).join("");
}

export async function sha256(s: string) {
  const d = new Uint8Array(await crypto.subtle.digest("SHA-256", new TextEncoder().encode(s)));
  return [...d].map((b) => b.toString(16).padStart(2, "0")).join("");
}

const SECRET = /^cx_([a-z2-7]{8})_([a-z2-7]{32})$/;

type Row = {
  id: string;
  name: string;
  kind: Token["kind"];
  role: Role;
  own_only: number;
  hash: string;
  created_at: number;
  created_by: string;
  expires_at: number | null;
  last_used_at: number | null;
};

const fromRow = (r: Row): Token => ({
  id: r.id,
  name: r.name,
  kind: r.kind,
  role: r.role,
  ownOnly: r.own_only === 1,
  createdAt: r.created_at,
  createdBy: r.created_by,
  expiresAt: r.expires_at,
  lastUsedAt: r.last_used_at,
});

export async function createToken(
  db: D1Database,
  t: { name: string; kind?: Token["kind"]; role: Role; ownOnly: boolean; expiresInSec?: number; createdBy: string },
): Promise<Token & { secret: string }> {
  await ensureSchema(db);
  const short = random32(8);
  const secret = `cx_${short}_${random32(32)}`;
  const now = Date.now();
  const token: Token = {
    id: `tk-${short}`,
    name: t.name,
    kind: t.kind ?? "token",
    role: t.role,
    // An admin token reaches everything anyway.
    ownOnly: t.role !== "admin" && t.ownOnly,
    createdAt: now,
    createdBy: t.createdBy,
    expiresAt: t.expiresInSec ? now + t.expiresInSec * 1000 : null,
    lastUsedAt: null,
  };
  await db
    .prepare(
      `INSERT INTO tokens (id, name, kind, role, own_only, hash, created_at, created_by, expires_at, last_used_at)
       VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, NULL)`,
    )
    .bind(token.id, token.name, token.kind, token.role, token.ownOnly ? 1 : 0, await sha256(secret), now, token.createdBy, token.expiresAt)
    .run();
  return { ...token, secret };
}

export async function listTokens(db: D1Database): Promise<Token[]> {
  await ensureSchema(db);
  // Expired ones go when listed.
  await db.prepare("DELETE FROM tokens WHERE expires_at IS NOT NULL AND expires_at < ?").bind(Date.now()).run();
  const { results } = await db.prepare("SELECT * FROM tokens ORDER BY created_at DESC").all<Row>();
  return results.map(fromRow);
}

export async function revokeToken(db: D1Database, id: string): Promise<Token | null> {
  await ensureSchema(db);
  const row = await db.prepare("DELETE FROM tokens WHERE id = ? RETURNING *").bind(id).first<Row>();
  return row ? fromRow(row) : null;
}

// Who a request's Authorization header says it is: the admin, a token, or no
// one (null). Notes the token's use, at most once a minute.
export async function authenticate(env: Env, header: string, ctx?: { waitUntil(p: Promise<unknown>): void }): Promise<Who | null> {
  const given = /^Bearer\s+(\S+)$/i.exec(header)?.[1];
  if (!given) return null;
  if (env.ADMIN_KEY && (await sameSecret(given, env.ADMIN_KEY))) return { kind: "admin" };
  const m = SECRET.exec(given);
  if (!m) return null;
  await ensureSchema(env.DB);
  const row = await env.DB.prepare("SELECT * FROM tokens WHERE id = ?").bind(`tk-${m[1]}`).first<Row>();
  if (!row || !(await sameSecret(await sha256(given), row.hash))) return null;
  const now = Date.now();
  if (row.expires_at !== null && row.expires_at < now) return null;
  if (row.last_used_at === null || now - row.last_used_at > 60_000) {
    const p = env.DB.prepare("UPDATE tokens SET last_used_at = ? WHERE id = ?").bind(now, row.id).run();
    if (ctx) ctx.waitUntil(p.catch(() => {}));
    else await p;
    row.last_used_at = now;
  }
  return { kind: "token", token: fromRow(row) };
}

export const me = (who: Who): Me =>
  who.kind === "admin" ? { role: "admin", ownOnly: false, token: null } : { role: who.token.role, ownOnly: who.token.ownOnly, token: who.token };

// The token that made a sandbox, and the one a new sandbox gets.
export async function ownerOf(db: D1Database, id: string): Promise<string | null> {
  await ensureSchema(db);
  return (await db.prepare("SELECT owner FROM sandbox_owners WHERE id = ?").bind(id).first<string>("owner")) ?? null;
}

export async function setOwner(db: D1Database, id: string, who: Who) {
  if (who.kind !== "token") return;
  await ensureSchema(db);
  await db.prepare("INSERT OR REPLACE INTO sandbox_owners (id, owner) VALUES (?, ?)").bind(id, who.token.id).run();
}

const RANK: Record<Role, number> = { read: 1, user: 2, admin: 3 };
const NEED_RANK: Record<Need, number> = { any: 0, read: 1, write: 2, admin: 3 };

// Fails with 403 when the token can't make this call, or 404 when its ref is
// a sandbox an ownOnly token didn't make, as if there were none.
export async function authorize(
  env: Env,
  who: Who,
  method: string,
  path: string,
  resolveId: (ref: string) => Promise<string | null>,
): Promise<void> {
  if (who.kind === "admin") return;
  const t = who.token;
  const rule = ruleFor(method, path);
  if (RANK[t.role] < NEED_RANK[rule.need]) {
    const what =
      rule.need === "admin"
        ? "only the admin key or an admin token can do this"
        : `this token only reads (role read), so it can't do this`;
    fail(403, `${what}. Its role: ${t.role}${t.ownOnly ? ", own sandboxes only" : ""}`);
  }
  if (!t.ownOnly) return;
  if (rule.notOwnOnly) fail(403, "this token is limited to its own sandboxes, so it can't make, replace or delete named saves");
  if (rule.ref) {
    const ref = decodeURIComponent(rule.ref);
    const id = await resolveId(ref);
    if (!id || (await ownerOf(env.DB, id)) !== t.id) fail(404, `no sandbox named ${ref}`);
  }
}

// Device login (RFC 8628). A request is good for 10 minutes; there can be a
// few waiting at once, so that nobody can fill the table without a key.
const DEVICE_TTL = 10 * 60_000;
export const DEVICE_INTERVAL = 5;
const MAX_WAITING = 20;
// No vowels, so codes don't spell words; no letters that look like digits.
const CODE_LETTERS = "BCDFGHJKLMNPQRSTVWXZ";

function userCode() {
  const pick = () => {
    // 20 letters: reject bytes past the last whole multiple, so each is equally likely.
    for (;;) {
      const b = crypto.getRandomValues(new Uint8Array(1))[0]!;
      if (b < 240) return CODE_LETTERS[b % 20]!;
    }
  };
  const s = Array.from({ length: 8 }, pick).join("");
  return `${s.slice(0, 4)}-${s.slice(4)}`;
}

// ABCD-EFGH however it's typed: case, spaces and dashes don't matter.
export const normalizeCode = (s: string) => {
  const c = s.toUpperCase().replace(/[^A-Z]/g, "");
  return c.length === 8 ? `${c.slice(0, 4)}-${c.slice(4)}` : c;
};

type DeviceRow = {
  device_hash: string;
  user_code: string;
  name: string;
  role: Role | null;
  ip: string | null;
  country: string | null;
  user_agent: string | null;
  created_at: number;
  expires_at: number;
  last_poll_at: number | null;
  status: "pending" | "approved" | "denied";
  grant_role: Role | null;
  grant_own_only: number | null;
  grant_expires_in: number | null;
  granted_by: string | null;
};

export async function startDevice(
  db: D1Database,
  req: { name: string; role?: Role; ip: string | null; country: string | null; userAgent: string | null },
) {
  await ensureSchema(db);
  const now = Date.now();
  await db.prepare("DELETE FROM device_requests WHERE expires_at < ?").bind(now).run();
  const waiting = await db.prepare("SELECT COUNT(*) AS n FROM device_requests").first<number>("n");
  if ((waiting ?? 0) >= MAX_WAITING) fail(429, "too many sign-ins are waiting for approval; try again in a few minutes");
  const deviceCode = random32(40);
  for (let i = 0; ; i++) {
    const code = userCode();
    try {
      await db
        .prepare(
          `INSERT INTO device_requests (device_hash, user_code, name, role, ip, country, user_agent, created_at, expires_at, status)
           VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending')`,
        )
        .bind(await sha256(deviceCode), code, req.name, req.role ?? null, req.ip, req.country, req.userAgent?.slice(0, 200) ?? null, now, now + DEVICE_TTL)
        .run();
      return { deviceCode, userCode: code, expiresIn: DEVICE_TTL / 1000 };
    } catch (e) {
      if (i < 3 && /UNIQUE/.test(String(e))) continue;
      throw e;
    }
  }
}

const requestFromRow = (r: DeviceRow) => ({
  userCode: r.user_code,
  name: r.name,
  role: r.role,
  ip: r.ip,
  country: r.country,
  userAgent: r.user_agent,
  createdAt: r.created_at,
  expiresAt: r.expires_at,
});

export async function waitingDevices(db: D1Database) {
  await ensureSchema(db);
  const { results } = await db
    .prepare("SELECT * FROM device_requests WHERE status = 'pending' AND expires_at >= ? ORDER BY created_at DESC")
    .bind(Date.now())
    .all<DeviceRow>();
  return results.map(requestFromRow);
}

// The approver chooses the role (by default the one asked for, or user), but
// can't grant more than they have.
export async function decideDevice(
  db: D1Database,
  who: Who,
  code: string,
  decision: { approve: true; role?: Role; ownOnly: boolean; expiresInSec?: number } | { approve: false },
) {
  await ensureSchema(db);
  const row = await db
    .prepare("SELECT * FROM device_requests WHERE user_code = ? AND status = 'pending' AND expires_at >= ?")
    .bind(normalizeCode(code), Date.now())
    .first<DeviceRow>();
  if (!row) fail(404, `no sign-in is waiting with the code ${code}; it may have expired (codes last 10 minutes)`);
  const by = who.kind === "admin" ? "admin" : who.token.id;
  if (!decision.approve) {
    await db.prepare("UPDATE device_requests SET status = 'denied', granted_by = ? WHERE device_hash = ?").bind(by, row.device_hash).run();
    return requestFromRow(row);
  }
  const role = decision.role ?? row.role ?? "user";
  if (who.kind === "token" && RANK[role] > RANK[who.token.role]) fail(403, `this token can't grant the role ${role}, which is more than its own`);
  await db
    .prepare("UPDATE device_requests SET status = 'approved', grant_role = ?, grant_own_only = ?, grant_expires_in = ?, granted_by = ? WHERE device_hash = ?")
    .bind(role, decision.ownOnly ? 1 : 0, decision.expiresInSec ?? 90 * 86_400, by, row.device_hash)
    .run();
  return { ...requestFromRow(row), role };
}

// What the device gets when it asks: RFC 8628's error codes until there's a
// decision, then the token, once.
export async function pollDevice(db: D1Database, deviceCode: string) {
  await ensureSchema(db);
  const hash = await sha256(deviceCode);
  const row = await db.prepare("SELECT * FROM device_requests WHERE device_hash = ?").bind(hash).first<DeviceRow>();
  const now = Date.now();
  if (!row || row.expires_at < now) {
    if (row) await db.prepare("DELETE FROM device_requests WHERE device_hash = ?").bind(hash).run();
    return { error: "expired_token" as const };
  }
  if (row.status === "denied") {
    await db.prepare("DELETE FROM device_requests WHERE device_hash = ?").bind(hash).run();
    return { error: "access_denied" as const };
  }
  if (row.status === "pending") {
    const early = row.last_poll_at !== null && now - row.last_poll_at < DEVICE_INTERVAL * 1000 - 1000;
    await db.prepare("UPDATE device_requests SET last_poll_at = ? WHERE device_hash = ?").bind(now, hash).run();
    return { error: early ? ("slow_down" as const) : ("authorization_pending" as const) };
  }
  // Approved: the row goes as the token is made, so a second poll gets nothing.
  const gone = await db.prepare("DELETE FROM device_requests WHERE device_hash = ? AND status = 'approved'").bind(hash).run();
  if (gone.meta.changes === 0) return { error: "expired_token" as const };
  const token = await createToken(db, {
    name: row.name,
    role: row.grant_role ?? "user",
    ownOnly: row.grant_own_only === 1,
    expiresInSec: row.grant_expires_in ?? undefined,
    createdBy: row.granted_by ?? "admin",
  });
  return { token };
}
