// Named saves (Save in @cloudex/schema): a copy of a sandbox's disk, kept in the
// account's R2 bucket under a name, that new sandboxes start from. Unlike a
// sandbox's own save, which Cloudflare drops 30 days after it was last made or
// restored, a named save stays until it's deleted or saved over.
//
// The image lists its own files (images/base/bin/cloudex-files), so a save
// holds only what a sandbox changed: new and changed files anywhere on the
// disk, including installed packages and Docker's data, and the list of the
// image's files it deleted. The guest streams it to R2 the way backups go
// (backup.ts), through http://cloudex.internal/saves/<id>/... (egress.ts),
// which only the sandbox making or restoring that save may use, and only then.
//
// A sandbox started from a save boots with CLOUDEX_HOLD=1: its entrypoint
// waits before starting anything, the save is restored onto the disk, and then
// it carries on. That needs the same image, so the hash of the image's list is
// kept with the save; onto another image, or one too old to wait, only /home
// is restored. An image without a list (before v0.1.15) makes saves of /home.
//
// Layout: saves/<id>/part-00000 ..., saves/<id>/meta (the deleted paths,
// gzipped) and saves/<id>/manifest.json. D1 maps names to ids. Saving under a
// taken name makes a new id, and the old id's objects go once it's replaced.
import type { Save, SaveKind, SandboxSize } from "@cloudex/schema";
import { listAll } from "./backup";
import { ensureSchema } from "./db";
import type { StoredSettings } from "./settings";

export interface SaveManifest {
  id: string;
  name: string;
  sandbox: string;
  kind: SaveKind;
  image: string | null; // SHA-256 of the image's list of files
  parts: number;
  bytes: number;
  rawBytes: number;
  files: number;
  home: number; // paths under /home
  deleted: number;
  size: SandboxSize;
  idleMinutes: number;
  createdAt: number;
  ms: number;
  settings?: StoredSettings;
}

// What the save script prints, last.
export type SaveSummary = Pick<SaveManifest, "kind" | "image" | "parts" | "bytes" | "rawBytes" | "files" | "home" | "deleted">;

export const SAVE_PATH = /^\/saves\/(sv-[a-z0-9]{10})\/(\d{1,6}|meta)$/;

export const saveObjectKey = (id: string, part: string) =>
  part === "meta" ? `saves/${id}/meta` : `saves/${id}/part-${part.padStart(5, "0")}`;

const manifestKey = (id: string) => `saves/${id}/manifest.json`;

export async function readManifest(bucket: R2Bucket, id: string): Promise<SaveManifest | null> {
  const obj = await bucket.get(manifestKey(id));
  return obj ? ((await obj.json()) as SaveManifest) : null;
}

export async function deleteSaveObjects(bucket: R2Bucket, id: string) {
  const keys = await listAll(bucket, `saves/${id}/`);
  for (let i = 0; i < keys.length; i += 1000) await bucket.delete(keys.slice(i, i + 1000));
}

type Row = {
  name: string;
  id: string;
  sandbox: string;
  kind: SaveKind;
  size: SandboxSize;
  idle_minutes: number;
  files: number;
  raw_bytes: number;
  bytes: number;
  created_at: number;
  ms: number;
};

const fromRow = (r: Row): Save => ({
  name: r.name,
  id: r.id,
  sandbox: r.sandbox,
  kind: r.kind,
  size: r.size,
  idleMinutes: r.idle_minutes,
  files: r.files,
  rawBytes: r.raw_bytes,
  bytes: r.bytes,
  createdAt: r.created_at,
  ms: r.ms,
});

export const saveView = (m: SaveManifest): Save => ({
  name: m.name,
  id: m.id,
  sandbox: m.sandbox,
  kind: m.kind,
  size: m.size,
  idleMinutes: m.idleMinutes,
  files: m.files,
  rawBytes: m.rawBytes,
  bytes: m.bytes,
  createdAt: m.createdAt,
  ms: m.ms,
});

// Writes a finished save's manifest, then points its name at it, in one
// transaction with reading what the name pointed at before, whose objects then
// go. Returns whether it replaced a save.
export async function recordSave(db: D1Database, bucket: R2Bucket, m: SaveManifest): Promise<boolean> {
  try {
    await bucket.put(manifestKey(m.id), JSON.stringify(m), { httpMetadata: { contentType: "application/json" } });
    await ensureSchema(db);
  } catch (e) {
    await deleteSaveObjects(bucket, m.id).catch(() => {});
    throw e;
  }
  const [before] = await db.batch<{ id: string }>([
    db.prepare("SELECT id FROM saves WHERE name = ?").bind(m.name),
    db
      .prepare(
        `INSERT INTO saves (name, id, sandbox, kind, size, idle_minutes, files, raw_bytes, bytes, created_at, ms)
         VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
         ON CONFLICT(name) DO UPDATE SET id = excluded.id, sandbox = excluded.sandbox, kind = excluded.kind,
           size = excluded.size, idle_minutes = excluded.idle_minutes, files = excluded.files,
           raw_bytes = excluded.raw_bytes, bytes = excluded.bytes, created_at = excluded.created_at, ms = excluded.ms`,
      )
      .bind(m.name, m.id, m.sandbox, m.kind, m.size, m.idleMinutes, m.files, m.rawBytes, m.bytes, m.createdAt, m.ms),
  ]).catch(async (e) => {
    await deleteSaveObjects(bucket, m.id).catch(() => {});
    throw e;
  });
  const old = before?.results[0]?.id;
  if (old && old !== m.id) {
    await deleteSaveObjects(bucket, old).catch((e) => console.error(`deleting the replaced save ${old} failed: ${e}`));
  }
  return Boolean(old);
}

export async function listSaves(db: D1Database): Promise<Save[]> {
  await ensureSchema(db);
  const { results } = await db.prepare("SELECT * FROM saves ORDER BY created_at DESC").all<Row>();
  return results.map(fromRow);
}

export async function getSave(db: D1Database, name: string): Promise<Save | null> {
  await ensureSchema(db);
  const row = await db.prepare("SELECT * FROM saves WHERE name = ?").bind(name).first<Row>();
  return row ? fromRow(row) : null;
}

// False if there was no save of that name.
export async function deleteSave(db: D1Database, bucket: R2Bucket, name: string): Promise<boolean> {
  await ensureSchema(db);
  const row = await db.prepare("DELETE FROM saves WHERE name = ? RETURNING id").bind(name).first<{ id: string }>();
  if (!row) return false;
  await deleteSaveObjects(bucket, row.id);
  return true;
}

// $1 is HELPER (backup.ts), $2 the save's id. Run as root in the running
// sandbox. Prints a SaveSummary as its last line. tar's exit status 1 only
// means a file changed while it was read, which a running sandbox allows, and
// files that vanish between listing and reading are skipped.
export const SAVE_SCRIPT = String.raw`
set -euo pipefail
dir=/run/cloudex/save
rm -rf "$dir"
install -d -m 700 "$dir"
trap 'rm -rf "$dir"' EXIT
cd /
if [ -x /usr/local/bin/cloudex-files ] && [ -s /usr/share/cloudex/image-files.gz ]; then
  summary=$(/usr/local/bin/cloudex-files diff "$dir")
  kind=disk
  recursion=--no-recursion
else
  # An image without a list of its files: /home only, as backups hold.
  printf 'home\0' >"$dir/changed"
  : >"$dir/deleted"
  summary=$(jq -cn --argjson files "$(find home -xdev | wc -l)" --argjson raw "$(du -sbx home | cut -f1)" \
    '{image: null, files: $files, rawBytes: $raw, home: $files, deleted: 0}')
  kind=home
  recursion=--recursion
fi
gzip -c "$dir/deleted" | python3 -c "$1" send "/saves/$2/meta"
up=$({ tar --null "$recursion" -T "$dir/changed" --numeric-owner --xattrs --xattrs-include='*' --sparse \
  --ignore-failed-read --warning=no-file-changed --warning=no-failed-read -cf - || [ $? -eq 1 ]; } |
  zstd -q -T0 -3 | python3 -c "$1" put "/saves/$2")
jq -cn --argjson a "$summary" --argjson b "$up" --arg kind "$kind" '$a + $b + {kind: $kind}'
`;

// $1 is HELPER, $2 the save's id, $3 its part count, $4 the hash of the image
// it was made on (empty for a save of /home), $5 how many of its paths are
// under /home, and $6 "held" while the entrypoint waits. The whole save is
// restored only onto a held sandbox of the same image; otherwise only /home,
// through Python's tar filters, which keep every member and link inside /home
// (extended attributes aren't restored there). Paths the save deleted go
// first, so that one that became another kind of file can be put back.
// Prints "all" or "home", what was restored.
export const RESTORE_SAVE_SCRIPT = String.raw`
set -euo pipefail
list=/usr/share/cloudex/image-files.gz
mine=
[ -s "$list" ] && mine=$(sha256sum "$list" | cut -d' ' -f1)
scope=home
[ "$6" = held ] && [ -n "$4" ] && [ "$4" = "$mine" ] && scope=all
python3 -c "$1" fetch "/saves/$2/meta" | gzip -dc | python3 -I -c '
import os, shutil, sys
home = sys.argv[1] == "home"
for p in sys.stdin.buffer.read().split(b"\0"):
    parts = p.split(b"/")
    if not p or any(c in (b"", b".", b"..") for c in parts) or (home and (parts[0] != b"home" or len(parts) < 2)):
        continue
    p = b"/" + p
    try:
        if os.path.isdir(p) and not os.path.islink(p):
            shutil.rmtree(p)
        elif os.path.lexists(p):
            os.unlink(p)
    except OSError as e:
        print("cloudex: could not remove " + os.fsdecode(p) + ": " + str(e), file=sys.stderr)
' "$scope"
if [ "$scope" = all ]; then
  python3 -c "$1" get "/saves/$2" "$3" | zstd -q -d | tar -C / --numeric-owner --xattrs --xattrs-include='*' --sparse -xpf -
elif [ "$5" != 0 ]; then
  python3 -c "$1" get "/saves/$2" "$3" | zstd -q -d | python3 -I -c '
import os, sys, tarfile
dest = "/home"
os.makedirs(dest, exist_ok=True)
with tarfile.open(fileobj=sys.stdin.buffer, mode="r|") as tar:
    for m in tar:
        name = m.name
        if not name.startswith("home/"):
            continue
        m.name = name[5:]
        if m.islnk():
            if not m.linkname.startswith("home/"):
                continue
            m.linkname = m.linkname[5:]
        try:
            tarfile.data_filter(m, dest)
            target = os.path.join(dest, m.name)
            if not m.isdir() and os.path.lexists(target) and not os.path.isdir(target):
                os.unlink(target)
            tar.extract(m, dest, set_attrs=True, numeric_owner=True, filter="fully_trusted")
        except (tarfile.FilterError, tarfile.ExtractError, OSError) as e:
            print("cloudex: left out " + name + ": " + str(e), file=sys.stderr)
'
fi
echo "$scope"
`;
