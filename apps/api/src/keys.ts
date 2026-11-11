// AI provider keys that sandboxes use without holding them. The deployment
// keeps each key in D1, encrypted with a key derived from the admin key. A
// sandbox started while a provider has a key gets a placeholder key instead,
// and its HTTPS traffic to that provider passes through the outbound handler
// (egress.ts), which puts the real key in when a request carries the
// placeholder or no key at all. Keys sandboxes bring themselves pass through
// untouched.
import type { Provider } from "@cloudex/schema";
import { ensureSchema } from "./db";
import { open, seal } from "./seal";

interface ProviderSpec {
  label: string;
  host: string;
  placeholder: string;
  // Where clients look for the key in the sandbox; each gets the placeholder.
  env: string[];
  // A URL on the host that answers 401 (or one of badKey) to a key it refuses.
  probe: string;
  badKey?: number[];
  // The key a request carries, if any.
  sent(h: Headers, url: URL): string | null;
  // Puts the real key in place of the placeholder or no key.
  set(h: Headers, key: string, url: URL): void;
  probeHeaders?: Record<string, string>;
}

const bearer = (h: Headers) => h.get("authorization")?.replace(/^Bearer\s+/i, "") || null;
const setBearer = (h: Headers, key: string) => h.set("authorization", `Bearer ${key}`);

// An OpenAI-style provider: the key as a bearer token.
const openaiLike = (label: string, host: string, placeholder: string, env: string[], probe: string, badKey?: number[]): ProviderSpec => ({
  label,
  host,
  placeholder,
  env,
  probe,
  badKey,
  sent: bearer,
  set: setBearer,
});

export const PROVIDERS: Record<Provider, ProviderSpec> = {
  anthropic: {
    label: "Anthropic",
    host: "api.anthropic.com",
    placeholder: "sk-ant-cloudex-placeholder-the-real-key-is-added-on-the-way-out",
    env: ["ANTHROPIC_API_KEY"],
    probe: "/v1/models",
    sent: (h) => h.get("x-api-key") || bearer(h),
    set(h, key) {
      h.delete("authorization");
      h.set("x-api-key", key);
    },
    probeHeaders: { "anthropic-version": "2023-06-01" },
  },
  // Codex reads CODEX_API_KEY, not OPENAI_API_KEY.
  openai: openaiLike(
    "OpenAI",
    "api.openai.com",
    "sk-cloudex-placeholder-the-real-key-is-added-on-the-way-out",
    ["OPENAI_API_KEY", "CODEX_API_KEY"],
    "/v1/models",
  ),
  // /api/v1/models answers without a key; /api/v1/key describes the key.
  openrouter: openaiLike(
    "OpenRouter",
    "openrouter.ai",
    "sk-or-cloudex-placeholder-the-real-key-is-added-on-the-way-out",
    ["OPENROUTER_API_KEY"],
    "/api/v1/key",
  ),
  // Google's clients send the key in x-goog-api-key, or the older ones as
  // ?key=; a key it refuses gets a 400.
  google: {
    label: "Google",
    host: "generativelanguage.googleapis.com",
    placeholder: "AIza-cloudex-placeholder-the-real-key-is-added-on-the-way-out",
    env: ["GEMINI_API_KEY", "GOOGLE_API_KEY", "GOOGLE_GENERATIVE_AI_API_KEY"],
    probe: "/v1beta/models",
    badKey: [400, 401, 403],
    sent: (h, url) => h.get("x-goog-api-key") || url.searchParams.get("key") || bearer(h),
    set(h, key, url) {
      url.searchParams.delete("key");
      h.delete("authorization");
      h.set("x-goog-api-key", key);
    },
  },
  groq: openaiLike("Groq", "api.groq.com", "gsk_cloudex_placeholder_the_real_key_is_added_on_the_way_out", ["GROQ_API_KEY"], "/openai/v1/models"),
  mistral: openaiLike("Mistral", "api.mistral.ai", "cloudex-mistral-placeholder-the-real-key-is-added-on-the-way-out", ["MISTRAL_API_KEY"], "/v1/models"),
  xai: openaiLike("xAI", "api.x.ai", "xai-cloudex-placeholder-the-real-key-is-added-on-the-way-out", ["XAI_API_KEY"], "/v1/models", [400, 401, 403]),
  deepseek: openaiLike(
    "DeepSeek",
    "api.deepseek.com",
    "sk-cloudex-deepseek-placeholder-the-real-key-is-added-on-the-way-out",
    ["DEEPSEEK_API_KEY"],
    "/models",
  ),
};

export const providerForHost = (host: string) =>
  (Object.keys(PROVIDERS) as Provider[]).find((p) => PROVIDERS[p].host === host);

// NAME=placeholder for each variable of the given providers, for KEYS_SCRIPT.
export const placeholderVars = (providers: Provider[]) =>
  providers.flatMap((p) => PROVIDERS[p].env.map((v) => `${v}=${PROVIDERS[p].placeholder}`));

// What a provider said when it refused a key: error.message, error or detail.
function refusalText(body: unknown): string | null {
  if (!body || typeof body !== "object") return null;
  const b = body as { error?: unknown; detail?: unknown; message?: unknown };
  const e = b.error as { message?: unknown } | string | undefined;
  for (const v of [typeof e === "object" ? e?.message : e, b.detail, b.message]) {
    if (typeof v === "string" && v) return v;
  }
  return null;
}

// A provider that can't be reached, or answers with anything but one of its
// bad-key statuses, may just be down, so that doesn't count against the key.
export async function keyRefusal(provider: Provider, key: string): Promise<string | null> {
  const spec = PROVIDERS[provider];
  const url = new URL(`https://${spec.host}${spec.probe}`);
  const headers = new Headers(spec.probeHeaders);
  spec.set(headers, key, url);
  let res: Response;
  try {
    res = await fetch(url, { headers, signal: AbortSignal.timeout(8_000) });
  } catch {
    return null;
  }
  if (!(spec.badKey ?? [401, 403]).includes(res.status)) {
    await res.body?.cancel();
    return null;
  }
  const said = refusalText(await res.json().catch(() => null));
  const why = said ? `${res.status}: ${said.slice(0, 300)}` : `HTTP ${res.status}`;
  const next =
    provider === "anthropic" && key.startsWith("sk-ant-oat")
      ? "A key starting with sk-ant-oat is a Claude subscription token, which can't be used this way. Make an API key in the Anthropic Console instead."
      : "Check the key and try again.";
  return `${spec.label} rejected that key (${why}), so it wasn't stored. ${next}`;
}

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
// (possibly none); $2 the Anthropic and $3 the OpenAI placeholder; $4 the path
// of the certificate that intercepted calls are signed with (Compute.caPath);
// $5 "node" on a node (v0.2.2 M6), where that certificate, the node CA, comes
// on stdin and $6, those providers' hosts, are pointed at the node's agent
// (cloudex.internal) in /etc/hosts; the rest NAME=placeholder for every
// variable of those providers (placeholderVars). Trusts that certificate,
// which on Cloudflare appears once interception is on, and only that one of
// the two (a save made on one and restored on the other brings the other's),
// and writes the placeholders where login shells (ssh, exec, agents) see them.
export const KEYS_SCRIPT = String.raw`
set -euo pipefail
providers=" $1 "
mode=$5
hosts=$6
store=/usr/local/share/ca-certificates
cf_ca=$store/cloudflare-containers-ca.crt
node_ca=$store/cloudex-node-ca.crt
# The CA this start trusts for provider calls ($1, a file in the store, or
# none) and no other of the two.
trust() {
  local changed=
  for f in "$cf_ca" "$node_ca"; do
    if [ "$f" != "$1" ] && [ -e "$f" ]; then rm -f "$f"; changed=1; fi
  done
  if [ -n "$1" ] && ! cmp -s "$2" "$1"; then cp "$2" "$1"; changed=1; fi
  [ -z "$changed" ] || update-ca-certificates >/dev/null 2>&1
}
# On a node, /etc/hosts is the machine's own (a bind mount Docker writes), so
# it is rewritten in place, and only when it holds the block.
hb="# Cloudex: provider hosts at the node's agent, rewritten at each start" he="# Cloudex: end of provider hosts"
if grep -qxF "$hb" /etc/hosts 2>/dev/null; then
  kept=$(sed "/^$hb\$/,/^$he\$/d" /etc/hosts)
  printf '%s
' "$kept" >/etc/hosts
fi
profile=/etc/profile.d/cloudex-keys.sh
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
[ -z "$(tail -c1 /etc/environment)" ] || echo >>/etc/environment
ca=$4
if [ -z "$1" ]; then
  rm -f "$profile"
  if [ "$mode" = node ]; then trust ""; elif [ -e "$node_ca" ]; then rm -f "$node_ca"; update-ca-certificates >/dev/null 2>&1; fi
  exit 0
fi
if [ "$mode" = node ]; then
  install -d -m 755 "$(dirname "$ca")"
  cat >"$ca.new"
  grep -q "BEGIN CERTIFICATE" "$ca.new" || { rm -f "$ca.new"; echo "the node's agent gave no CA" >&2; exit 1; }
  mv "$ca.new" "$ca"
  addr=$(getent hosts cloudex.internal | awk '{print $1; exit}')
  [ -n "$addr" ] || { echo "cloudex.internal has no address in this machine" >&2; exit 1; }
  kept=$(cat /etc/hosts)
  { printf '%s
%s
' "$kept" "$hb"; for h in $hosts; do echo "$addr $h"; done; echo "$he"; } >/etc/hosts
  trust "$node_ca" "$ca"
else
  for _ in $(seq 150); do [ -s "$ca" ] && break; sleep 0.1; done
  [ -s "$ca" ] || { echo "the platform's interception certificate didn't appear" >&2; exit 1; }
  trust "$cf_ca" "$ca"
fi
bundle=/etc/ssl/certs/ca-certificates.crt
# NAME=value pairs; none has a space.
vars="NODE_EXTRA_CA_CERTS=$bundle SSL_CERT_FILE=$bundle REQUESTS_CA_BUNDLE=$bundle"
shift 6
vars="$vars $*"
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
