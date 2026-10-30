// Per-sandbox settings (UpdateSettings and SandboxSettings in @cloudex/schema):
// environment variables and files the sandbox gets at every start, and a repo
// cloned when it's created. The Sandbox object keeps them sealed with the admin
// key (seal.ts), next to the names and paths, which the API shows; values and
// contents never leave it except into the sandbox. Forks get a copy.
import type { Repo, SandboxSettings, UpdateSettings } from "@cloudex/schema";
import { open, seal } from "./seal";
import { fail } from "./util";

const LABEL = "sandbox settings v1";
const MAX_BYTES = 1 << 20;

type Secret = { env: Record<string, string>; files: Record<string, string> };

export type StoredSettings = {
  sealed: string;
  env: string[];
  files: string[]; // relative to the home directory
  repo: { url: string; dir: string; branch: string | null } | null;
};

export class UnreadableSettings extends Error {}

// ~/.npmrc, .npmrc and /home/cloudex/.npmrc are all ".npmrc".
export function homePath(path: string): string {
  const rel = path
    .replace(/^~\//, "")
    .replace(/^\/home\/cloudex\//, "")
    .split("/")
    .filter((p) => p && p !== ".")
    .join("/");
  if (!rel || path.endsWith("/")) fail(400, `${path} isn't a file path in the home directory`);
  return rel;
}

export function repoSpec(repo: string | Repo): NonNullable<StoredSettings["repo"]> {
  const spec = typeof repo === "string" ? { url: repo.replace(/#.*$/, ""), branch: /#(.+)$/.exec(repo)?.[1] } : repo;
  const name = new URL(spec.url).pathname.split("/").filter(Boolean).pop()?.replace(/\.git$/, "");
  const dir = spec.dir ?? name;
  if (!dir) fail(400, `can't tell where to clone ${spec.url}: give repo.dir`);
  return { url: spec.url, dir: homePath(dir), branch: spec.branch ?? null };
}

export const view = (s: StoredSettings | undefined): SandboxSettings => ({
  env: s?.env ?? [],
  files: (s?.files ?? []).map((p) => `~/${p}`),
  repo: s?.repo ? { ...s.repo, dir: `~/${s.repo.dir}` } : null,
});

export async function readSecret(adminKey: string, s: StoredSettings | undefined): Promise<Secret> {
  if (!s) return { env: {}, files: {} };
  try {
    return JSON.parse(await open(adminKey, LABEL, s.sealed)) as Secret;
  } catch {
    throw new UnreadableSettings(
      "its environment variables and files were sealed under an earlier admin key, so they can't be read; set them again",
    );
  }
}

export async function store(adminKey: string, secret: Secret, repo: StoredSettings["repo"]): Promise<StoredSettings> {
  const env = Object.keys(secret.env).sort();
  const files = Object.keys(secret.files).sort();
  if (env.length > 100) fail(400, "a sandbox can have at most 100 environment variables");
  if (files.length > 20) fail(400, "a sandbox can have at most 20 files");
  const plain = JSON.stringify(secret);
  if (new TextEncoder().encode(plain).length > MAX_BYTES) fail(400, "a sandbox's variables and files can take up at most 1 MB");
  return { sealed: await seal(adminKey, LABEL, plain), env, files, repo };
}

// A change (null removes) on top of what's set.
export function merge(secret: Secret, change: UpdateSettings): Secret {
  const env = { ...secret.env };
  for (const [k, v] of Object.entries(change.env ?? {})) {
    if (v === null || v === undefined) delete env[k];
    else env[k] = v;
  }
  const files = { ...secret.files };
  for (const [p, v] of Object.entries(change.files ?? {})) {
    const rel = homePath(p);
    if (v === null || v === undefined) delete files[rel];
    else files[rel] = v;
  }
  return { env, files };
}

// Run as root at each start, after the provider keys, and when the settings
// change. The settings arrive as JSON on stdin: {env, files}, file paths
// relative to the home directory.
export const SETTINGS_SCRIPT = String.raw`
set -euo pipefail
in=$(cat)
install -d -m 755 /etc/cloudex

# Variables, shell-quoted, for every shell: login shells (exec, agents, cloudex
# ssh) read profile.d, and bash reads bash.bashrc for an ssh command (cloudex
# prompt, editors' remote servers), where /etc/environment can't hold a #.
# Sourced after the provider key placeholders, so a variable set here wins.
f=/etc/cloudex/env.sh
{
  echo "# Written by Cloudex at each start and when the sandbox's settings change."
  jq -r '.env | to_entries[] | "export \(.key)=\(.value | @sh)"' <<<"$in"
} >"$f.new"
chown root:cloudex "$f.new"
chmod 640 "$f.new"
mv "$f.new" "$f"
echo '[ -r /etc/cloudex/env.sh ] && . /etc/cloudex/env.sh' >/etc/profile.d/cloudex-settings.sh
grep -q 'cloudex/env.sh' /etc/bash.bashrc || sed -i '1i [ -r /etc/cloudex/env.sh ] \&\& . /etc/cloudex/env.sh' /etc/bash.bashrc

# Files in the home directory, written as the cloudex user so a symlink there
# can't point the write elsewhere. One dropped from the settings is removed.
list=/etc/cloudex/files
jq -r '.files | keys[]' <<<"$in" >"$list.new"
if [ -f "$list" ]; then
  while IFS= read -r p; do
    grep -qxF -- "$p" "$list.new" || runuser -u cloudex -- rm -f -- "/home/cloudex/$p"
  done <"$list"
fi
while IFS= read -r p; do
  jq -j --arg p "$p" '.files[$p]' <<<"$in" |
    runuser -u cloudex -- bash -c 'umask 077 && mkdir -p -- "$(dirname -- "$1")" && cat >"$1" && chmod 600 -- "$1"' _ "/home/cloudex/$p"
done <"$list.new"
mv "$list.new" "$list"

# git over HTTPS to github.com answers with $GH_TOKEN or $GITHUB_TOKEN, so a
# token in the settings lets the sandbox clone and push private repos.
cat >/usr/local/bin/cloudex-git-credential <<'EOF'
#!/bin/sh
# git credential helper for https://github.com: $GH_TOKEN or $GITHUB_TOKEN.
[ "$1" = get ] || exit 0
t=$GH_TOKEN
[ -n "$t" ] || t=$GITHUB_TOKEN
[ -n "$t" ] || exit 0
echo username=x-access-token
echo "password=$t"
EOF
chmod 755 /usr/local/bin/cloudex-git-credential
git config --system credential.https://github.com.helper /usr/local/bin/cloudex-git-credential
`;

// Run as the cloudex user from a login shell, so the settings' variables (a
// GitHub token) apply: $1 the URL, $2 the directory, $3 the branch or ''.
export const CLONE_SCRIPT = String.raw`
set -euo pipefail
url=$1 dir=$2 branch=$3
cd "$HOME"
if [ -e "$dir" ]; then echo "$dir is already there" >&2; exit 0; fi
if [ -n "$branch" ]; then set -- --branch "$branch"; else set --; fi
GIT_TERMINAL_PROMPT=0 exec git clone --quiet "$@" -- "$url" "$dir"
`;
