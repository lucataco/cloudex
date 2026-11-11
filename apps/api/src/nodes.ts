// Nodes in D1: a user's own hosts that run sandboxes, each with an agent that
// keeps a control socket open to its Node object (node.ts). D1 holds the
// node's name, its agent's public key, and a copy of what the agent last
// reported, which its Node object writes; whether it's online right now is the
// Node object's to say (status()).
//
// A node is pending from cloudex node add until its agent joins with the
// token (nodeauth.ts), then offline or online, and revoked after cloudex node
// rm, which clears its key. A revoked node stays listed until it's removed
// again; adding its name again makes a new node.
import { type Node, NodeFacts, NodeIsolation, type NodeJoin, type NodeState } from "@cloudex/schema";
import { ensureSchema } from "./db";
import { fail } from "./util";

type Row = {
  id: string;
  name: string;
  public_key: string | null;
  state: NodeState;
  last_seen_at: number | null;
  version: string | null;
  protocol: number | null;
  arch: string | null;
  runtime: Node["runtime"];
  isolation: string | null;
  cpus: number | null;
  memory_mb: number | null;
  disk_free_gb: number | null;
  colo: string | null;
  continent: string | null;
  location_hint: string | null;
  created_at: number;
  revoked_at: number | null;
  machines: number;
};

function isolation(text: string | null) {
  if (!text) return null;
  try {
    const r = NodeIsolation.safeParse(JSON.parse(text));
    return r.success ? r.data : null;
  } catch {
    return null;
  }
}

const fromRow = (r: Row): Node => ({
  id: r.id,
  name: r.name,
  state: r.state,
  createdAt: r.created_at,
  lastSeenAt: r.last_seen_at,
  version: r.version,
  protocol: r.protocol,
  arch: r.arch,
  runtime: r.runtime,
  isolation: isolation(r.isolation),
  cpus: r.cpus,
  memoryMb: r.memory_mb,
  diskFreeGb: r.disk_free_gb,
  colo: r.colo,
  continent: r.continent,
  locationHint: r.location_hint,
  machines: r.machines ?? 0,
  revokedAt: r.revoked_at,
});

const SELECT = "SELECT n.*, (SELECT COUNT(*) FROM sandboxes s WHERE s.node = n.id) AS machines FROM nodes n";

const ALPHABET = "abcdefghijklmnopqrstuvwxyz0123456789";
export const newNodeId = () => `nd-${[...crypto.getRandomValues(new Uint8Array(8))].map((b) => ALPHABET[b % 36]).join("")}`;
export const isNodeId = (s: string) => /^nd-[a-z0-9]{8}$/.test(s);

export async function listNodeRows(db: D1Database): Promise<Node[]> {
  await ensureSchema(db);
  const { results } = await db.prepare(`${SELECT} ORDER BY n.created_at`).all<Row>();
  return results.map(fromRow);
}

// By name or id.
export async function findNode(db: D1Database, ref: string): Promise<Node | null> {
  await ensureSchema(db);
  const row = await db.prepare(`${SELECT} WHERE ${isNodeId(ref) ? "n.id" : "n.name"} = ?`).bind(ref).first<Row>();
  return row ? fromRow(row) : null;
}

export async function mustFindNode(db: D1Database, ref: string): Promise<Node> {
  const n = await findNode(db, ref);
  if (!n) fail(404, `no node named ${ref}`);
  return n;
}

// A new pending node, or a pending or revoked one of that name again: a
// pending one keeps its id, and a revoked one is replaced by a new node.
export async function addNode(db: D1Database, name: string, now = Date.now()): Promise<Node> {
  await ensureSchema(db);
  const old = await findNode(db, name);
  if (old?.state === "pending") return old;
  if (old && old.state !== "revoked") fail(409, `the name ${name} is taken by a node that has joined; cloudex node rm ${name} first`);
  if (old) await forgetNode(db, old.id);
  const id = newNodeId();
  try {
    await db.prepare("INSERT INTO nodes (id, name, state, created_at) VALUES (?, ?, 'pending', ?)").bind(id, name, now).run();
  } catch (e) {
    if (/UNIQUE/.test(String(e))) fail(409, `the name ${name} was just taken; try again`);
    throw e;
  }
  return (await findNode(db, id))!;
}

// The agent's key, and (2026-11-12 review, R3) the key a removed node had, in
// one D1 read: the connect route tells a removed node's own agent so.
export async function nodeKeys(db: D1Database, id: string): Promise<{ key: string | null; revokedKey: string | null }> {
  await ensureSchema(db);
  const r = await db
    .prepare(
      "SELECT (SELECT public_key FROM nodes WHERE id = ?1 AND state != 'revoked') AS key, (SELECT public_key FROM node_revocations WHERE node_id = ?1) AS revoked_key",
    )
    .bind(id)
    .first<{ key: string | null; revoked_key: string | null }>();
  return { key: r?.key ?? null, revokedKey: r?.revoked_key ?? null };
}

// The agent's public key, while the node has joined and isn't revoked.
export async function nodePublicKey(db: D1Database, id: string): Promise<string | null> {
  await ensureSchema(db);
  return (await db.prepare("SELECT public_key FROM nodes WHERE id = ? AND state != 'revoked'").bind(id).first<string>("public_key")) ?? null;
}

// A pending node's agent joins with its key. Null when the node was removed
// in the meantime, or has joined already. A key another node has (or had,
// until it was removed) is refused (2026-11-12 review, W6): each node's key is
// its own, so a signature names one node.
export async function joinNode(db: D1Database, id: string, j: NodeJoin, now = Date.now()): Promise<{ id: string; name: string } | null> {
  const f = j.facts;
  if (await keyTaken(db, j.publicKey, id)) fail(409, "that agent's key belongs to another node of this deployment: each node needs its own (remove the agent's state volume, cloudex-node, and run the installer again)");
  const row = await db
    .prepare(
      `UPDATE nodes SET public_key = ?, state = 'offline', version = ?, arch = ?, runtime = ?, isolation = ?, cpus = ?, memory_mb = ?,
         disk_free_gb = ?, last_seen_at = ?
       WHERE id = ? AND state = 'pending' AND public_key IS NULL
         AND NOT EXISTS (SELECT 1 FROM nodes WHERE public_key = ?) AND NOT EXISTS (SELECT 1 FROM node_revocations WHERE public_key = ?)
       RETURNING id, name`,
    )
    .bind(j.publicKey, j.version, f.arch, f.runtime, JSON.stringify(f.isolation), f.cpus, f.memoryMb, f.diskFreeGb, now, id, j.publicKey, j.publicKey)
    .first<{ id: string; name: string }>();
  return row ? { id: row.id, name: row.name } : null;
}

// What a Node object writes as its agent connects, reports and goes. Every
// value has been through the models in @cloudex/schema; this only binds them.
export type Mirror = {
  state?: "online" | "offline";
  lastSeenAt?: number;
  version?: string;
  protocol?: number;
  facts?: NodeFacts;
  colo?: string | null;
  continent?: string | null;
  locationHint?: string | null;
};

export async function mirrorNode(db: D1Database, id: string, m: Mirror) {
  await ensureSchema(db);
  const set: string[] = [];
  const values: (string | number | null)[] = [];
  const put = (col: string, v: string | number | null) => {
    set.push(`${col} = ?`);
    values.push(v);
  };
  if (m.state) put("state", m.state);
  if (m.lastSeenAt !== undefined) put("last_seen_at", m.lastSeenAt);
  if (m.version !== undefined) put("version", m.version);
  if (m.protocol !== undefined) put("protocol", m.protocol);
  if (m.facts) {
    const f = NodeFacts.parse(m.facts);
    put("arch", f.arch);
    put("runtime", f.runtime);
    put("isolation", JSON.stringify(f.isolation));
    put("cpus", f.cpus);
    put("memory_mb", f.memoryMb);
    put("disk_free_gb", f.diskFreeGb);
  }
  if (m.colo !== undefined) put("colo", m.colo);
  if (m.continent !== undefined) put("continent", m.continent);
  if (m.locationHint !== undefined) put("location_hint", m.locationHint);
  if (set.length === 0) return;
  // Never brings a revoked or pending node back.
  await db
    .prepare(`UPDATE nodes SET ${set.join(", ")} WHERE id = ? AND state IN ('online', 'offline')`)
    .bind(...values, id)
    .run();
}

async function keyTaken(db: D1Database, key: string, id: string) {
  const r = await db
    .prepare("SELECT 1 AS x FROM nodes WHERE public_key = ?1 AND id != ?2 UNION ALL SELECT 1 FROM node_revocations WHERE public_key = ?1 AND node_id != ?2 LIMIT 1")
    .bind(key, id)
    .first<{ x: number }>();
  return r !== null;
}

// The node's key goes; a copy stays in node_revocations (nodeKeys).
export async function revokeNode(db: D1Database, id: string, now = Date.now()) {
  await ensureSchema(db);
  await db.batch([
    db
      .prepare("INSERT OR REPLACE INTO node_revocations (node_id, public_key, revoked_at) SELECT id, public_key, ? FROM nodes WHERE id = ? AND public_key IS NOT NULL")
      .bind(now, id),
    db.prepare("UPDATE nodes SET state = 'revoked', public_key = NULL, revoked_at = ? WHERE id = ?").bind(now, id),
    db.prepare("DELETE FROM node_joins WHERE node_id = ?").bind(id),
  ]);
}

export async function forgetNode(db: D1Database, id: string) {
  await ensureSchema(db);
  await db.batch([db.prepare("DELETE FROM nodes WHERE id = ?").bind(id), db.prepare("DELETE FROM node_joins WHERE node_id = ?").bind(id)]);
}
