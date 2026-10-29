// Backups of a sandbox's /home (and its SSH host keys) in the account's R2
// bucket. A save (snapshot) expires 30 days after it was made or last restored,
// and only resumes onto the same image; a backup does neither.
//
// The guest does the work, so nothing large passes through the Durable Object:
// tar | zstd streams into a small Python helper that sends 64 MiB parts, four
// at a time, to http://cloudex.internal/backup/<gen>/<n> (egress.ts), which
// writes them to R2 under the sandbox's own prefix. This is the path that
// moved about 110 MiB/s each way in the Phase 0 spikes.
//
// Layout: <id>/<gen>/part-00000 ... and <id>/<gen>/manifest.json, where gen is
// the backup's start time in ms. A backup counts only once its manifest is
// written, and older generations are deleted after that.

export interface BackupManifest {
  gen: number;
  parts: number;
  bytes: number; // compressed
  createdAt: number;
  ms: number;
}

export const backupBucket = (env: Env): R2Bucket | undefined => (env as { BACKUPS?: R2Bucket }).BACKUPS;

export const partKey = (id: string, gen: number | string, n: number | string) =>
  `${id}/${gen}/part-${String(n).padStart(5, "0")}`;

const manifestKey = (id: string, gen: number) => `${id}/${gen}/manifest.json`;

export async function writeManifest(bucket: R2Bucket, id: string, m: BackupManifest) {
  await bucket.put(manifestKey(id, m.gen), JSON.stringify(m), {
    httpMetadata: { contentType: "application/json" },
  });
}

async function listAll(bucket: R2Bucket, prefix: string): Promise<string[]> {
  const keys: string[] = [];
  let cursor: string | undefined;
  do {
    const page = await bucket.list({ prefix, cursor });
    keys.push(...page.objects.map((o) => o.key));
    cursor = page.truncated ? page.cursor : undefined;
  } while (cursor);
  return keys;
}

// The newest complete backup, if any.
export async function latestBackup(bucket: R2Bucket, id: string): Promise<BackupManifest | null> {
  const gens = (await listAll(bucket, `${id}/`))
    .filter((k) => k.endsWith("/manifest.json"))
    .map((k) => Number(k.split("/")[1]))
    .filter(Number.isFinite)
    .sort((a, b) => b - a);
  for (const gen of gens) {
    const obj = await bucket.get(manifestKey(id, gen));
    if (obj) return (await obj.json()) as BackupManifest;
  }
  return null;
}

// Deletes one generation, such as a backup that failed halfway.
export async function deleteGeneration(bucket: R2Bucket, id: string, gen: number) {
  const keys = await listAll(bucket, `${id}/${gen}/`);
  for (let i = 0; i < keys.length; i += 1000) await bucket.delete(keys.slice(i, i + 1000));
}

// Deletes every object of this sandbox except generation keep. Returns how many went.
export async function deleteBackups(bucket: R2Bucket, id: string, keep?: number): Promise<number> {
  const doomed = (await listAll(bucket, `${id}/`)).filter((k) => keep === undefined || k.split("/")[1] !== String(keep));
  for (let i = 0; i < doomed.length; i += 1000) await bucket.delete(doomed.slice(i, i + 1000));
  return doomed.length;
}

// The guest side. Runs as root with python3 from the base image.
//   put GEN          stdin -> parts; prints {"parts": N, "bytes": B}
//   get GEN PARTS    parts -> stdout, in order, four in flight
export const HELPER = String.raw`
import concurrent.futures as cf, http.client, json, sys
PART, JOBS = 64 << 20, 4

def call(method, path, body=None):
    err = None
    for _ in range(3):
        try:
            c = http.client.HTTPConnection("cloudex.internal", 80, timeout=600)
            headers = {"content-type": "application/octet-stream"}
            if body is not None:
                headers["content-length"] = str(len(body))
            c.request(method, path, body=body, headers=headers)
            r = c.getresponse()
            data = r.read()
            c.close()
            if r.status == 200:
                return data
            err = RuntimeError(f"{method} {path}: {r.status} {data[:200]!r}")
        except OSError as e:
            err = e
    raise err

def put(gen):
    parts = total = 0
    with cf.ThreadPoolExecutor(JOBS) as ex:
        pending = set()
        while True:
            data = sys.stdin.buffer.read(PART)
            if not data:
                break
            pending.add(ex.submit(call, "PUT", f"/backup/{gen}/{parts}", data))
            parts += 1
            total += len(data)
            if len(pending) >= JOBS:
                done, pending = cf.wait(pending, return_when=cf.FIRST_COMPLETED)
                for f in done:
                    f.result()
        for f in pending:
            f.result()
    print(json.dumps({"parts": parts, "bytes": total}))

def get(gen, parts):
    out = sys.stdout.buffer
    with cf.ThreadPoolExecutor(JOBS) as ex:
        futures = {}
        for n in range(parts):
            for m in range(n, min(n + JOBS, parts)):
                if m not in futures:
                    futures[m] = ex.submit(call, "GET", f"/backup/{gen}/{m}")
            out.write(futures.pop(n).result())
    out.flush()

if sys.argv[1] == "put":
    put(sys.argv[2])
else:
    get(sys.argv[2], int(sys.argv[3]))
`;

// $1 is HELPER, $2 the generation. Caches are left out; tar's exit status 1
// only means a file changed while it was read, which a running box allows.
export const BACKUP_SCRIPT = String.raw`
set -euo pipefail
cd /
paths=(home)
for f in etc/ssh/ssh_host_*; do [ -e "$f" ] && paths+=("$f"); done
{ tar --numeric-owner --xattrs --warning=no-file-changed --exclude='home/*/.cache' -cf - "${"$"}{paths[@]}" || [ $? -eq 1 ]; } |
  zstd -q -T0 -3 | python3 -c "$1" put "$2"
`;

// $1 is HELPER, $2 the generation, $3 its part count. sshd reloads the restored host keys.
export const RESTORE_SCRIPT = String.raw`
set -euo pipefail
python3 -c "$1" get "$2" "$3" | zstd -q -d | tar --numeric-owner --xattrs -C / -xpf -
pkill -HUP -x sshd || true
`;
