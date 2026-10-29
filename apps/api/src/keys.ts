// AI provider keys that sandboxes use without holding them. The deployment
// keeps each key in D1, encrypted with a key derived from the admin key. A
// sandbox started while a provider has a key gets a placeholder key instead,
// and its HTTPS traffic to that provider passes through the outbound handler
// (egress.ts), which puts the real key in when a request carries the
// placeholder or no key at all. Keys sandboxes bring themselves, and
// subscription logins, pass through untouched.
import type { Provider } from "@cloudex/schema";
import { ensureSchema } from "./db";

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

// AES-GCM with a key derived from the admin key, so a copy of the D1 database
// alone doesn't reveal the provider keys.
async function cipherKey(adminKey: string) {
  const base = await crypto.subtle.importKey("raw", new TextEncoder().encode(adminKey), "HKDF", false, ["deriveKey"]);
  return crypto.subtle.deriveKey(
    { name: "HKDF", hash: "SHA-256", salt: new TextEncoder().encode("cloudex"), info: new TextEncoder().encode("provider keys v1") },
    base,
    { name: "AES-GCM", length: 256 },
    false,
    ["encrypt", "decrypt"],
  );
}

const b64 = (bytes: Uint8Array) => btoa(String.fromCharCode(...bytes));
const unb64 = (text: string) => Uint8Array.from(atob(text), (c) => c.charCodeAt(0));

async function seal(adminKey: string, plain: string) {
  const iv = crypto.getRandomValues(new Uint8Array(12));
  const sealed = await crypto.subtle.encrypt({ name: "AES-GCM", iv }, await cipherKey(adminKey), new TextEncoder().encode(plain));
  return `v1:${b64(iv)}:${b64(new Uint8Array(sealed))}`;
}

async function open(adminKey: string, value: string) {
  const [version, iv, data] = value.split(":");
  if (version !== "v1" || !iv || !data) throw new Error("unknown key format");
  const plain = await crypto.subtle.decrypt({ name: "AES-GCM", iv: unb64(iv) }, await cipherKey(adminKey), unb64(data));
  return new TextDecoder().decode(plain);
}

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
    .bind(provider, await seal(env.ADMIN_KEY, key), row.hint, row.updatedAt)
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

// The real keys, cached for a minute per isolate: provider calls come often,
// and a changed key needs to reach running sandboxes only soon, not at once.
const cache = new Map<Provider, { key: string | null; at: number }>();

export async function providerKey(env: Env, provider: Provider): Promise<string | null> {
  const hit = cache.get(provider);
  if (hit && Date.now() - hit.at < 60_000) return hit.key;
  await ensureSchema(env.DB);
  const value = await env.DB.prepare("SELECT value FROM provider_keys WHERE provider = ?").bind(provider).first<string>("value");
  const key = value ? await open(env.ADMIN_KEY, value) : null;
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
if [ -z "$1" ]; then rm -f "$profile"; exit 0; fi
ca=/etc/cloudflare/certs/cloudflare-containers-ca.crt
for _ in $(seq 150); do [ -s "$ca" ] && break; sleep 0.1; done
[ -s "$ca" ] || { echo "the platform's interception certificate didn't appear" >&2; exit 1; }
cp "$ca" /usr/local/share/ca-certificates/cloudflare-containers-ca.crt
update-ca-certificates >/dev/null 2>&1
bundle=/etc/ssl/certs/ca-certificates.crt
{
  echo "# Written by Cloudex at each start. These keys are placeholders: the real"
  echo "# ones stay outside the sandbox and are added to requests on the way out."
  echo "export NODE_EXTRA_CA_CERTS=$bundle SSL_CERT_FILE=$bundle REQUESTS_CA_BUNDLE=$bundle"
  case $providers in *" anthropic "*) echo "export ANTHROPIC_API_KEY=$2" ;; esac
  case $providers in *" openai "*) echo "export OPENAI_API_KEY=$3 CODEX_API_KEY=$3" ;; esac
} >"$profile"
case $providers in
  *" anthropic "*)
    # Claude Code asks once before using a key from the environment, and
    # remembers the answer by the key's last 20 characters. With a key there's
    # no login to choose, so first-run setup is skipped too, keeping any
    # answers already given. Its Bypass Permissions warning, which cloudex
    # prompt runs into, is left for a person to accept.
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
