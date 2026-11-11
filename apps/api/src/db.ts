// D1 is only the index behind `cloudex list` and name lookups. Each Sandbox
// Durable Object owns its own state and writes its row here when it changes.
// It also holds the names of named saves (saves.ts), the provider keys, and
// access tokens with the sandboxes each one made (tokens.ts), and the nodes
// with their join tokens (nodes.ts).
import type { Sandbox } from "@cloudex/schema";

const SCHEMA = [
  `CREATE TABLE IF NOT EXISTS sandboxes (
    id TEXT PRIMARY KEY,
    name TEXT UNIQUE,
    size TEXT NOT NULL,
    state TEXT NOT NULL,
    idle_minutes INTEGER NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    started_at INTEGER,
    stopped_at INTEGER,
    saved_at INTEGER,
    saved_bytes INTEGER,
    error TEXT
  )`,
  // Named saves (saves.ts): the name, and the id of its objects in R2.
  `CREATE TABLE IF NOT EXISTS saves (
    name TEXT PRIMARY KEY,
    id TEXT NOT NULL,
    sandbox TEXT NOT NULL,
    kind TEXT NOT NULL,
    size TEXT NOT NULL,
    idle_minutes INTEGER NOT NULL,
    files INTEGER NOT NULL,
    raw_bytes INTEGER NOT NULL,
    bytes INTEGER NOT NULL,
    created_at INTEGER NOT NULL,
    ms INTEGER NOT NULL
  )`,
  // AI provider keys (keys.ts), encrypted.
  `CREATE TABLE IF NOT EXISTS provider_keys (
    provider TEXT PRIMARY KEY,
    value TEXT NOT NULL,
    hint TEXT NOT NULL,
    updated_at INTEGER NOT NULL
  )`,
  // Access tokens (tokens.ts): the SHA-256 of each secret, never the secret.
  `CREATE TABLE IF NOT EXISTS tokens (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    kind TEXT NOT NULL,
    role TEXT NOT NULL,
    own_only INTEGER NOT NULL,
    hash TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    created_by TEXT NOT NULL,
    expires_at INTEGER,
    last_used_at INTEGER
  )`,
  // The token that made each sandbox, for ownOnly tokens. Apart from the
  // index, which a Sandbox object rewrites, so that its owner never goes with
  // a rewrite.
  `CREATE TABLE IF NOT EXISTS sandbox_owners (
    id TEXT PRIMARY KEY,
    owner TEXT NOT NULL
  )`,
  // Device logins waiting for approval: the device code's SHA-256, and the
  // user code, which only signed-in callers see.
  `CREATE TABLE IF NOT EXISTS device_requests (
    device_hash TEXT PRIMARY KEY,
    user_code TEXT NOT NULL UNIQUE,
    name TEXT NOT NULL,
    role TEXT,
    ip TEXT,
    country TEXT,
    user_agent TEXT,
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL,
    last_poll_at INTEGER,
    status TEXT NOT NULL,
    grant_role TEXT,
    grant_own_only INTEGER,
    grant_expires_in INTEGER,
    granted_by TEXT
  )`,
  // One-time dashboard sign-in links (POST /v1/auth/links): the code's
  // SHA-256, and the session it makes. They last 60 seconds.
  `CREATE TABLE IF NOT EXISTS login_links (
    hash TEXT PRIMARY KEY,
    role TEXT NOT NULL,
    own_only INTEGER NOT NULL,
    parent TEXT,
    created_by TEXT NOT NULL,
    expires_at INTEGER NOT NULL
  )`,
  // Nodes (nodes.ts): a user's own hosts that run sandboxes. public_key is the
  // agent's Ed25519 key, set when it joins and cleared when it's revoked. The
  // rest mirrors what the agent last reported, which its Node object writes.
  `CREATE TABLE IF NOT EXISTS nodes (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL UNIQUE,
    public_key TEXT,
    state TEXT NOT NULL,
    last_seen_at INTEGER,
    version TEXT,
    protocol INTEGER,
    arch TEXT,
    runtime TEXT,
    isolation TEXT,
    cpus INTEGER,
    memory_mb INTEGER,
    disk_free_gb REAL,
    colo TEXT,
    continent TEXT,
    location_hint TEXT,
    created_at INTEGER NOT NULL,
    revoked_at INTEGER
  )`,
  // A node's join token: its SHA-256, never the token. Used once, within 30 minutes.
  `CREATE TABLE IF NOT EXISTS node_joins (
    token_hash TEXT PRIMARY KEY,
    node_id TEXT NOT NULL,
    expires_at INTEGER NOT NULL,
    used_at INTEGER
  )`,
];

// Changes to tables made by an earlier version, each run once per isolate. A
// column that's already there is fine.
const MIGRATIONS = [
  // v0.2.1: the token a dashboard session acts for.
  "ALTER TABLE tokens ADD COLUMN parent TEXT",
  // v0.2.1: the end a device approval set for a session, past which it isn't renewed.
  "ALTER TABLE tokens ADD COLUMN ends_at INTEGER",
  // v0.2.2: the node a sandbox runs on; null on Cloudflare.
  "ALTER TABLE sandboxes ADD COLUMN node TEXT",
];

async function migrate(db: D1Database) {
  await db.batch(SCHEMA.map((s) => db.prepare(s)));
  for (const m of MIGRATIONS) {
    try {
      await db.prepare(m).run();
    } catch (e) {
      if (!/duplicate column/i.test(String(e))) throw e;
    }
  }
}

// Created on first use so a fresh deployment needs no migration step.
let ready: Promise<unknown> | undefined;
export function ensureSchema(db: D1Database) {
  ready ??= migrate(db).catch((e) => {
    ready = undefined;
    throw e;
  });
  return ready;
}

type Row = {
  id: string;
  name: string | null;
  size: Sandbox["size"];
  state: Sandbox["state"];
  idle_minutes: number;
  created_at: number;
  updated_at: number;
  started_at: number | null;
  stopped_at: number | null;
  saved_at: number | null;
  saved_bytes: number | null;
  error: string | null;
};

const fromRow = (r: Row): Sandbox => ({
  id: r.id,
  name: r.name,
  size: r.size,
  state: r.state,
  idleMinutes: r.idle_minutes,
  createdAt: r.created_at,
  updatedAt: r.updated_at,
  startedAt: r.started_at,
  stoppedAt: r.stopped_at,
  savedAt: r.saved_at,
  savedBytes: r.saved_bytes,
  error: r.error,
});

// node: the node it runs on (nodes.ts), which a sandbox keeps for life. A
// record that doesn't say leaves the column as it was.
export async function upsertSandbox(db: D1Database, s: Sandbox & { node?: string | null }) {
  await ensureSchema(db);
  await db
    .prepare(
      `INSERT INTO sandboxes (id, name, size, state, idle_minutes, created_at, updated_at, started_at, stopped_at, saved_at, saved_bytes, error, node)
       VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
       ON CONFLICT(id) DO UPDATE SET name = excluded.name, size = excluded.size, state = excluded.state,
         idle_minutes = excluded.idle_minutes, updated_at = excluded.updated_at, started_at = excluded.started_at,
         stopped_at = excluded.stopped_at, saved_at = excluded.saved_at, saved_bytes = excluded.saved_bytes,
         error = excluded.error, node = COALESCE(excluded.node, sandboxes.node)`,
    )
    .bind(s.id, s.name, s.size, s.state, s.idleMinutes, s.createdAt, s.updatedAt, s.startedAt, s.stoppedAt, s.savedAt, s.savedBytes, s.error, s.node ?? null)
    .run();
}

// Before the Sandbox object takes a new name: the index's unique name column is
// what keeps two sandboxes from sharing one. False if the name is taken.
export async function renameSandbox(db: D1Database, id: string, name: string): Promise<boolean> {
  await ensureSchema(db);
  try {
    const { meta } = await db.prepare("UPDATE sandboxes SET name = ? WHERE id = ?").bind(name, id).run();
    // A sandbox missing from the index gets its row back with its next
    // update; until then the name must be free.
    return meta.changes > 0 || !(await nameTaken(db, name));
  } catch (e) {
    if (/UNIQUE constraint failed/.test(e instanceof Error ? e.message : String(e))) return false;
    throw e;
  }
}

export async function deleteSandbox(db: D1Database, id: string): Promise<boolean> {
  await ensureSchema(db);
  const [row] = await db.batch([
    db.prepare("DELETE FROM sandboxes WHERE id = ?").bind(id),
    db.prepare("DELETE FROM sandbox_owners WHERE id = ?").bind(id),
  ]);
  return (row?.meta.changes ?? 0) > 0;
}

// owner: only the sandboxes that token made.
export async function listSandboxes(db: D1Database, owner?: string): Promise<Sandbox[]> {
  await ensureSchema(db);
  const { results } = await (owner
    ? db
        .prepare("SELECT s.* FROM sandboxes s JOIN sandbox_owners o ON o.id = s.id WHERE o.owner = ? ORDER BY s.created_at DESC")
        .bind(owner)
    : db.prepare("SELECT * FROM sandboxes ORDER BY created_at DESC")
  ).all<Row>();
  return results.map(fromRow);
}

export async function idForName(db: D1Database, name: string): Promise<string | null> {
  await ensureSchema(db);
  return (await db.prepare("SELECT id FROM sandboxes WHERE name = ?").bind(name).first<string>("id")) ?? null;
}

export async function nameTaken(db: D1Database, name: string) {
  return (await idForName(db, name)) !== null;
}

// The sandboxes on a node (nodes.ts), by id.
export async function sandboxesOnNode(db: D1Database, node: string): Promise<string[]> {
  await ensureSchema(db);
  const { results } = await db.prepare("SELECT id FROM sandboxes WHERE node = ?").bind(node).all<{ id: string }>();
  return results.map((r) => r.id);
}
