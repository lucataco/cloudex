// D1 is only the index behind `cloudex list` and name lookups. Each Sandbox
// Durable Object owns its own state and writes its row here when it changes.
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
];

// Created on first use so a fresh deployment needs no migration step.
let ready: Promise<unknown> | undefined;
export function ensureSchema(db: D1Database) {
  ready ??= db.batch(SCHEMA.map((s) => db.prepare(s))).catch((e) => {
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

export async function upsertSandbox(db: D1Database, s: Sandbox) {
  await ensureSchema(db);
  await db
    .prepare(
      `INSERT INTO sandboxes (id, name, size, state, idle_minutes, created_at, updated_at, started_at, stopped_at, saved_at, saved_bytes, error)
       VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
       ON CONFLICT(id) DO UPDATE SET name = excluded.name, size = excluded.size, state = excluded.state,
         idle_minutes = excluded.idle_minutes, updated_at = excluded.updated_at, started_at = excluded.started_at,
         stopped_at = excluded.stopped_at, saved_at = excluded.saved_at, saved_bytes = excluded.saved_bytes,
         error = excluded.error`,
    )
    .bind(s.id, s.name, s.size, s.state, s.idleMinutes, s.createdAt, s.updatedAt, s.startedAt, s.stoppedAt, s.savedAt, s.savedBytes, s.error)
    .run();
}

export async function deleteSandbox(db: D1Database, id: string) {
  await ensureSchema(db);
  await db.prepare("DELETE FROM sandboxes WHERE id = ?").bind(id).run();
}

export async function listSandboxes(db: D1Database): Promise<Sandbox[]> {
  await ensureSchema(db);
  const { results } = await db.prepare("SELECT * FROM sandboxes ORDER BY created_at DESC").all<Row>();
  return results.map(fromRow);
}

export async function idForName(db: D1Database, name: string): Promise<string | null> {
  await ensureSchema(db);
  return (await db.prepare("SELECT id FROM sandboxes WHERE name = ?").bind(name).first<string>("id")) ?? null;
}

export async function nameTaken(db: D1Database, name: string) {
  return (await idForName(db, name)) !== null;
}
