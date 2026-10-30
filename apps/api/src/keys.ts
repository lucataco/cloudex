// AI provider keys that sandboxes use without holding them. The deployment
// keeps each key in D1, encrypted with a key derived from the admin key. A
// sandbox started while a provider has a key gets a placeholder key instead,
// and its HTTPS traffic to that provider passes through the outbound handler
// (egress.ts), which puts the real key in when a request carries the
// placeholder or no key at all. Keys sandboxes bring themselves, and
// subscription logins, pass through untouched.
import type { Provider } from "@cloudex/schema";
import { ensureSchema } from "./db";
import { open, seal } from "./seal";

interface ProviderSpec {
  host: string;
  placeholder: string;
  // The key a request carries, if any.
  sent(h: Headers): string | null;
  // Puts the real key in place of the placeholder or no key.
  set(h: Headers, key: string): void;
}

const bearer = (h: Headers) => h.get("authorization")?.replace(/^Bearer\s+/i, "") || null;

export const PROVIDERS: Record<Provider, ProviderSpec> = {
  anthropic: {
    host: "api.anthropic.com",
    placeholder: "sk-ant-cloudex-placeholder-the-real-key-is-added-on-the-way-out",
    sent: (h) => h.get("x-api-key") || bearer(h),
    set(h, key) {
      h.delete("authorization");
      h.set("x-api-key", key);
    },
  },
  openai: {
    host: "api.openai.com",
    placeholder: "sk-cloudex-placeholder-the-real-key-is-added-on-the-way-out",
    sent: bearer,
    set: (h, key) => h.set("authorization", `Bearer ${key}`),
  },
};

export const providerForHost = (host: string) =>
  (Object.keys(PROVIDERS) as Provider[]).find((p) => PROVIDERS[p].host === host);

// Sealed with the admin key (seal.ts), so a copy of the D1 database alone
// doesn't reveal the provider keys.
const LABEL = "provider keys v1";

type Row = { provider: Provider; value: string; hint: string; updated_at: number };

export async function listKeys(db: D1Database) {
  await ensureSchema(db);
  const { results } = await db.prepare("SELECT provider, hint, updated_at FROM provider_keys ORDER BY provider").all<Row>();
  return results.map((r) => ({ provider: r.provider, hint: r.hint, updatedAt: r.updated_at }));
}

export async function setKey(env: Env, provider: Provider, key: string) {
  await ensureSchema(env.DB);
  const row = { provider, hint: `…${key.slice(-4)}`, updatedAt: Date.now() };
  await env.DB.prepare(
    `INSERT INTO provider_keys (provider, value, hint, updated_at) VALUES (?, ?, ?, ?)
     ON CONFLICT(provider) DO UPDATE SET value = excluded.value, hint = excluded.hint, updated_at = excluded.updated_at`,
  )
    .bind(provider, await seal(env.ADMIN_KEY, LABEL, key), row.hint, row.updatedAt)
    .run();
  cache.clear();
  return row;
}

export async function deleteKey(db: D1Database, provider: Provider) {
  await ensureSchema(db);
  const { meta } = await db.prepare("DELETE FROM provider_keys WHERE provider = ?").bind(provider).run();
  cache.clear();
  return meta.changes > 0;
}

// A key stored under an earlier admin key, which cloudex deploy --yes replaces.
export class UnreadableKey extends Error {}

// The real keys, cached for a minute per isolate: provider calls come often,
// and a changed key needs to reach running sandboxes only soon, not at once.
const cache = new Map<Provider, { key: string | null; at: number }>();

export async function providerKey(env: Env, provider: Provider): Promise<string | null> {
  const hit = cache.get(provider);
  if (hit && Date.now() - hit.at < 60_000) return hit.key;
  await ensureSchema(env.DB);
  const value = await env.DB.prepare("SELECT value FROM provider_keys WHERE provider = ?").bind(provider).first<string>("value");
  let key: string | null = null;
  if (value) key = await open(env.ADMIN_KEY, LABEL, value).catch(() => Promise.reject(new UnreadableKey(provider)));
  cache.set(provider, { key, at: Date.now() });
  return key;
}


// Run as root after each start. $1 is the providers with keys, space-separated
// (possibly none); $2 and $3 the Anthropic and OpenAI placeholders. Trusts the
// platform's interception certificate, which appears once interception is on,
// and writes the placeholders where login shells (ssh, exec, agents) see them.
export const KEYS_SCRIPT = String.raw`
set -euo pipefail
providers=" $1 "
profile=/etc/profile.d/cloudex-keys.sh
# Not about keys, but it needs the same after-restore timing: cloudex prompt
# runs Claude Code with --dangerously-skip-permissions, the sandbox being the
# boundary, so its warning about that is accepted ahead of time. A setting
# already there, true or false, stays.
runuser -u cloudex -- env HOME=/home/cloudex bash -c '
  f=$HOME/.claude/settings.json
  mkdir -p "$HOME/.claude"
  [ -s "$f" ] || echo "{}" >"$f"
  jq "if has(\"skipDangerousModePermissionPrompt\") then . else .skipDangerousModePermissionPrompt = true end" "$f" >"$f.new" &&
    mv "$f.new" "$f" || rm -f "$f.new"' || true
# Codex opens on a sign-in screen, even with CODEX_API_KEY set, unless it has
# a login. With an OpenAI key and no login of its own (ChatGPT or a key given
# to it in the sandbox), it gets an API-key login with the placeholder, which
# goes when the OpenAI key does.
codex_auth=/home/cloudex/.codex/auth.json
case $providers in
  *" openai "*)
    if [ ! -s "$codex_auth" ] && command -v codex >/dev/null; then
      printf %s "$3" | runuser -u cloudex -- env HOME=/home/cloudex codex login --with-api-key >/dev/null 2>&1 || true
    fi
    ;;
  *) [ "$(jq -r '.OPENAI_API_KEY // empty' "$codex_auth" 2>/dev/null)" != "$3" ] || rm -f "$codex_auth" ;;
esac
# Shells read the placeholders from the profile. An ssh session that runs a
# command without one, as cloudex prompt does, or an editor's remote server,
# gets them from /etc/environment, which sshd reads for every session.
begin="# Cloudex: placeholder provider keys, rewritten at each start" end="# Cloudex: end"
sed -i "/^$begin\$/,/^$end\$/d" /etc/environment
if [ -z "$1" ]; then rm -f "$profile"; exit 0; fi
ca=/etc/cloudflare/certs/cloudflare-containers-ca.crt
for _ in $(seq 150); do [ -s "$ca" ] && break; sleep 0.1; done
[ -s "$ca" ] || { echo "the platform's interception certificate didn't appear" >&2; exit 1; }
cp "$ca" /usr/local/share/ca-certificates/cloudflare-containers-ca.crt
update-ca-certificates >/dev/null 2>&1
bundle=/etc/ssl/certs/ca-certificates.crt
# NAME=value pairs; none has a space.
vars="NODE_EXTRA_CA_CERTS=$bundle SSL_CERT_FILE=$bundle REQUESTS_CA_BUNDLE=$bundle"
case $providers in *" anthropic "*) vars="$vars ANTHROPIC_API_KEY=$2" ;; esac
case $providers in *" openai "*) vars="$vars OPENAI_API_KEY=$3 CODEX_API_KEY=$3" ;; esac
{
  echo "# Written by Cloudex at each start. These keys are placeholders: the real"
  echo "# ones stay outside the sandbox and are added to requests on the way out."
  echo "export $vars"
} >"$profile"
{ echo "$begin"; printf '%s\n' $vars; echo "$end"; } >>/etc/environment
case $providers in
  *" anthropic "*)
    # Claude Code asks once before using a key from the environment, and
    # remembers the answer by the key's last 20 characters. With a key there's
    # no login to choose, so first-run setup is skipped too, keeping any
    # answers already given.
    runuser -u cloudex -- env HOME=/home/cloudex TAIL="$(printf %s "$2" | tail -c 20)" bash -c '
      f=$HOME/.claude.json
      [ -s "$f" ] || echo "{}" >"$f"
      jq --arg k "$TAIL" ".customApiKeyResponses.approved = ((.customApiKeyResponses.approved // []) + [\$k] | unique)
        | .customApiKeyResponses.rejected = (.customApiKeyResponses.rejected // [])
        | .hasCompletedOnboarding //= true | .theme //= \"dark\"
        | .projects[\"/home/cloudex\"].hasTrustDialogAccepted //= true" "$f" >"$f.new" && mv "$f.new" "$f"'
    ;;
esac
`;
