// Enough of D1 and the Workers runtime, over node:sqlite, to run tokens.ts in
// node --test. src/ imports its modules without extensions, as wrangler's
// bundler takes them, so a resolve hook adds .ts for node. Import this before
// importing anything from src/, and that with import().
import { registerHooks } from "node:module";
import { DatabaseSync, type SQLInputValue } from "node:sqlite";
import { timingSafeEqual } from "node:crypto";

registerHooks({
  resolve(spec, ctx, next) {
    try {
      return next(spec, ctx);
    } catch (e) {
      if (spec.startsWith(".") && !spec.endsWith(".ts")) return next(`${spec}.ts`, ctx);
      throw e;
    }
  },
});

// Workers' crypto.subtle has this; node's has it in node:crypto.
const subtle = crypto.subtle as SubtleCrypto & { timingSafeEqual?: (a: ArrayBuffer, b: ArrayBuffer) => boolean };
subtle.timingSafeEqual ??= (a, b) => a.byteLength === b.byteLength && timingSafeEqual(new Uint8Array(a), new Uint8Array(b));

class Statement {
  db: DatabaseSync;
  sql: string;
  params: SQLInputValue[];
  constructor(db: DatabaseSync, sql: string, params: SQLInputValue[] = []) {
    this.db = db;
    this.sql = sql;
    this.params = params;
  }
  bind(...params: SQLInputValue[]) {
    return new Statement(this.db, this.sql, params);
  }
  async run() {
    if (/\bRETURNING\b/i.test(this.sql)) {
      const results = this.db.prepare(this.sql).all(...this.params);
      return { results, meta: { changes: results.length } };
    }
    const r = this.db.prepare(this.sql).run(...this.params);
    return { results: [], meta: { changes: Number(r.changes) } };
  }
  async all() {
    return { results: this.db.prepare(this.sql).all(...this.params) };
  }
  async first(col?: string) {
    const row = this.db.prepare(this.sql).get(...this.params) as Record<string, unknown> | undefined;
    if (!row) return null;
    return col ? (row[col] ?? null) : row;
  }
}

// One per test file: tokens.ts makes its tables once per isolate (db.ts
// ensureSchema), so a second database wouldn't get them.
export function fakeD1() {
  const db = new DatabaseSync(":memory:");
  return {
    prepare: (sql: string) => new Statement(db, sql),
    batch: async (stmts: Statement[]) => {
      const out = [];
      for (const s of stmts) out.push(/^\s*(SELECT|.*\bRETURNING\b)/is.test(s.sql) ? await s.all() : await s.run());
      return out;
    },
    exec: (sql: string) => db.exec(sql),
  } as unknown as D1Database & { exec(sql: string): void };
}
