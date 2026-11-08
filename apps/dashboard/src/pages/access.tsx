// /access, for admins: tokens and dashboard sessions, sign-ins waiting for
// approval, and AI provider keys (shown by their last 4 characters only).
import type { DeviceRequest, Me, NewToken, Provider, ProviderKey, Role, Token } from "@cloudex/schema";
import { useState } from "preact/hooks";
import { del, get, post, put } from "../api";
import { Act, Copy, Loading, Problem, ago, until, usePoll } from "../ui";

const PROVIDERS: Provider[] = ["anthropic", "openai", "openrouter", "google", "groq", "mistral", "xai", "deepseek"];

export function Access(props: { me: Me }) {
  if (props.me.role !== "admin") return <Problem error="Only an admin can see this page." />;
  return (
    <>
      <div class="head">
        <h1>Access</h1>
      </div>
      <Waiting />
      <Tokens me={props.me} />
      <Keys />
    </>
  );
}

function Waiting() {
  const list = usePoll(() => get<DeviceRequest[]>("/v1/auth/device"), [], 5000);
  const [error, setError] = useState<string | null>(null);
  if (!list.data?.length && !list.error) return null;
  return (
    <section class="card">
      <h2>Waiting for approval</h2>
      <p class="warn small">Only approve a sign-in you started yourself, whose code you can see on that computer.</p>
      <Problem error={error ?? list.error} />
      <table class="list compact">
        <tbody>
          {(list.data ?? []).map((r) => (
            <tr key={r.userCode}>
              <td class="mono strong">{r.userCode}</td>
              <td>
                {r.name}
                <div class="small muted">
                  {r.ip ?? "?"}
                  {r.country ? ` (${r.country})` : ""} · {ago(r.createdAt)}
                  {r.role ? ` · asks for ${r.role}` : ""}
                </div>
              </td>
              <td class="acts">
                <a class="btn" href={`/login?code=${encodeURIComponent(r.userCode)}`}>
                  Review
                </a>
                <Act
                  run={async () => {
                    await post("/v1/auth/device/deny", { user_code: r.userCode });
                    await list.reload();
                  }}
                  onError={setError}
                  kind="danger"
                >
                  Refuse
                </Act>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </section>
  );
}

function Tokens(props: { me: Me }) {
  const list = usePoll(() => get<Token[]>("/v1/tokens"), []);
  const [error, setError] = useState<string | null>(null);
  const [made, setMade] = useState<NewToken | null>(null);
  const [name, setName] = useState("");
  const [role, setRole] = useState<Role>("user");
  const [own, setOwn] = useState(false);
  const [days, setDays] = useState("90");
  const mine = props.me.token?.id;
  const create = async () => {
    const t = await post<NewToken>("/v1/tokens", {
      name,
      role,
      ownOnly: own,
      ...(days ? { expiresInSec: Number(days) * 86_400 } : {}),
    });
    setMade(t);
    setName("");
    await list.reload();
  };
  const names = new Map((list.data ?? []).map((t) => [t.id, t.name]));
  return (
    <section class="card">
      <h2>Tokens and sessions</h2>
      <p class="muted small">Revoking one stops it at once. Revoking a token also signs out the dashboard sessions it signed in.</p>
      <Problem error={error ?? list.error} />
      {made ? (
        <div class="note">
          <p>
            The secret of <b>{made.name}</b>. It isn't shown again:
          </p>
          <div class="cmd">
            <code>{made.secret}</code>
            <Copy text={made.secret} />
          </div>
          <button type="button" class="btn btn-quiet" onClick={() => setMade(null)}>
            Done
          </button>
        </div>
      ) : null}
      {!list.data ? (
        <Loading />
      ) : (
        <table class="list compact">
          <thead>
            <tr>
              <th>Name</th>
              <th>Role</th>
              <th>Last used</th>
              <th>Expires</th>
              <th>Made by</th>
              <th />
            </tr>
          </thead>
          <tbody>
            {list.data.map((t) => (
              <tr key={t.id}>
                <td>
                  <span class="strong">{t.name}</span>
                  <span class="id">{t.id}</span>
                  <div class="small muted">
                    {t.kind === "session" ? "dashboard session" : "token"}
                    {t.id === mine ? " · this browser" : ""}
                    {t.parent ? ` · for ${names.get(t.parent) ?? t.parent}` : ""}
                  </div>
                </td>
                <td>
                  {t.role}
                  {t.ownOnly ? <div class="small muted">own sandboxes</div> : null}
                </td>
                <td>{ago(t.lastUsedAt)}</td>
                <td>{until(t.expiresAt)}</td>
                <td class="small">{t.createdBy === "admin" ? "the admin key" : (names.get(t.createdBy) ?? t.createdBy)}</td>
                <td class="acts">
                  <Act
                    run={async () => {
                      await del(`/v1/tokens/${t.id}`);
                      if (t.id === mine) location.assign("/login");
                      else await list.reload();
                    }}
                    onError={setError}
                    kind="danger"
                    confirm={t.id === mine ? "This is this browser's session: revoking it signs you out here. Go on?" : `Revoke ${t.name}?`}
                  >
                    Revoke
                  </Act>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      <h3>New token</h3>
      <div class="form grid">
        <label>
          Name
          <input value={name} placeholder="ci" onInput={(e) => setName(e.currentTarget.value)} />
        </label>
        <label>
          Role
          <select value={role} onChange={(e) => setRole(e.currentTarget.value as Role)}>
            <option value="user">user: sandboxes and saves</option>
            <option value="read">read: look, don't touch</option>
            <option value="admin">admin: everything</option>
          </select>
        </label>
        <label>
          Expires after (days; empty for never)
          <input type="number" min="1" max="3650" value={days} onInput={(e) => setDays(e.currentTarget.value)} />
        </label>
        <label class="check">
          <input type="checkbox" checked={own} onChange={(e) => setOwn(e.currentTarget.checked)} disabled={role === "admin"} />
          Only the sandboxes it makes
        </label>
      </div>
      <Act run={create} onError={setError} kind="primary" disabled={!name.trim()}>
        Make the token
      </Act>
    </section>
  );
}

function Keys() {
  const list = usePoll(() => get<ProviderKey[]>("/v1/keys"), []);
  const [error, setError] = useState<string | null>(null);
  const [provider, setProvider] = useState<Provider>("anthropic");
  const [key, setKey] = useState("");
  const set = new Map((list.data ?? []).map((k) => [k.provider, k]));
  return (
    <section class="card">
      <h2>AI provider keys</h2>
      <p class="muted small">
        Sandboxes get a stand-in, and the real key is added to their requests to the provider on the way out, so it never enters a sandbox. A sandbox started or
        resumed after a key is set gets it.
      </p>
      <Problem error={error ?? list.error} />
      <table class="list compact">
        <tbody>
          {PROVIDERS.map((p) => {
            const k = set.get(p);
            return (
              <tr key={p}>
                <td class="strong">{p}</td>
                <td class="mono small">{k ? k.hint : <span class="muted">not set</span>}</td>
                <td class="small">{k ? `set ${ago(k.updatedAt)}` : ""}</td>
                <td class="acts">
                  {k ? (
                    <Act
                      run={async () => {
                        await del(`/v1/keys/${p}`);
                        await list.reload();
                      }}
                      onError={setError}
                      kind="danger"
                      confirm={`Remove the ${p} key? Sandboxes lose it at their next start.`}
                    >
                      Remove
                    </Act>
                  ) : null}
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>
      <form
        class="row"
        onSubmit={(e) => {
          e.preventDefault();
          setError(null);
          put(`/v1/keys/${provider}`, { key: key.trim() })
            .then(() => {
              setKey("");
              return list.reload();
            })
            .catch((x) => setError(x.message));
        }}
      >
        <select value={provider} onChange={(e) => setProvider(e.currentTarget.value as Provider)} aria-label="Provider">
          {PROVIDERS.map((p) => (
            <option key={p}>{p}</option>
          ))}
        </select>
        <input type="password" autocomplete="off" spellcheck={false} placeholder="the key" value={key} onInput={(e) => setKey(e.currentTarget.value)} />
        <button type="submit" class="btn btn-primary" disabled={key.trim().length < 8}>
          Set
        </button>
      </form>
    </section>
  );
}
