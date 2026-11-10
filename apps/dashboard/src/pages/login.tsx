// /login: signing in, and approving another computer's sign-in.
//   /login#link=CODE&next=/s/web   a one-time link from cloudex dashboard; a
//                                  browser signed in already keeps its session
//                                  and goes to next, leaving the link unused
//   /login?code=ABCD-EFGH          a device login to approve (cloudex login
//                                  shows this link), once signed in
// Signing in without a link: get a code here and approve it with cloudex
// tokens approve on a computer that's signed in, or paste a token or the
// admin key.
import type { DeviceCode, DeviceRequest, Me, Role } from "@cloudex/schema";
import { useEffect, useRef, useState } from "preact/hooks";
import { ApiError, api, get, post } from "../api";
import { safeNext } from "@cloudex/schema/paths";
import { navigate } from "../router";
import { Act, Cmd, Problem, ago } from "../ui";

const start = (body: object) => api<Me>("POST", "/v1/auth/session", body, { login: false });

function browserName() {
  const s = navigator.userAgent;
  const b = /Edg\//.test(s) ? "Edge" : /Firefox\//.test(s) ? "Firefox" : /Chrome\//.test(s) ? "Chrome" : /Safari\//.test(s) ? "Safari" : "a browser";
  const os = /iPhone|iPad/.test(s) ? " on iOS" : /Android/.test(s) ? " on Android" : /Mac OS X/.test(s) ? " on macOS" : /Windows/.test(s) ? " on Windows" : /Linux/.test(s) ? " on Linux" : "";
  return `dashboard (${b}${os})`;
}

export function Login() {
  const query = new URLSearchParams(location.search);
  const approving = query.get("code");
  const [me, setMe] = useState<Me | null | undefined>(undefined);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    const hash = new URLSearchParams(location.hash.slice(1));
    const link = hash.get("link");
    // The code goes out of the address bar and history at once.
    if (link) history.replaceState(null, "", location.pathname + location.search);
    api<Me>("GET", "/v1/me", undefined, { login: false }).then(
      (m) => {
        // Signed in already: a link never replaces the session, since someone
        // else's link would sign this browser in as them. It stays unused; to
        // use one, sign out first.
        if (link) location.replace(safeNext(hash.get("next")));
        else if (approving) setMe(m);
        else location.replace(safeNext(query.get("next")));
      },
      () => {
        if (!link) return setMe(null);
        start({ link }).then(
          () => location.replace(safeNext(hash.get("next"))),
          (e) => {
            setError(e.message);
            setMe(null);
          },
        );
      },
    );
  }, []);

  if (me === undefined && !error) return <main class="page narrow"><p class="muted">Signing in…</p></main>;
  if (me && approving) return <Approve code={approving} me={me} />;

  const next = approving ? `/login?code=${encodeURIComponent(approving)}` : safeNext(query.get("next"));
  return (
    <main class="page narrow login">
      <h1 class="mark big">
        <i aria-hidden="true" />
        Cloudex
      </h1>
      {approving ? (
        <p class="note">
          Sign in here first, to approve the sign-in with the code <b class="mono">{approving}</b>.
        </p>
      ) : null}
      <Problem error={error} />
      <section class="card">
        <h2>From a computer with the CLI</h2>
        <p>Run this on a computer that's signed in to this deployment. It opens this page signed in.</p>
        <Cmd text="cloudex dashboard" />
      </section>
      <WithCode next={next} />
      <WithToken next={next} />
    </main>
  );
}

function WithCode(props: { next: string }) {
  const [code, setCode] = useState<DeviceCode | null>(null);
  const [error, setError] = useState<string | null>(null);
  const timer = useRef<ReturnType<typeof setTimeout>>();
  useEffect(() => () => clearTimeout(timer.current), []);

  const poll = (d: DeviceCode, every: number) => {
    timer.current = setTimeout(async () => {
      try {
        await start({ device_code: d.device_code });
        location.replace(props.next);
      } catch (e) {
        const why = e instanceof ApiError ? e.message : String(e);
        if (why === "authorization_pending") poll(d, every);
        else if (why === "slow_down") poll(d, every + 5);
        else {
          setCode(null);
          setError(
            why === "access_denied"
              ? "That sign-in was refused."
              : why === "expired_token"
                ? "The code expired (codes last 10 minutes). Get a new one."
                : why,
          );
        }
      }
    }, every * 1000);
  };

  const begin = async () => {
    const d = await post<DeviceCode>("/v1/auth/device", { name: browserName() });
    setCode(d);
    poll(d, d.interval);
  };

  return (
    <section class="card">
      <h2>With a code</h2>
      {code ? (
        <>
          <p>Approve this code on a computer that's signed in. This page carries on by itself.</p>
          <p class="code">{code.user_code}</p>
          <Cmd text={`cloudex tokens approve ${code.user_code}`} />
          <p class="muted">Or open {code.verification_uri_complete} in a browser that's signed in.</p>
        </>
      ) : (
        <>
          <p>Get a code here, then approve it with cloudex tokens approve, or in a dashboard that's signed in.</p>
          <Act run={begin} onError={setError} kind="primary">
            Get a code
          </Act>
        </>
      )}
      <Problem error={error} />
    </section>
  );
}

function WithToken(props: { next: string }) {
  const [token, setToken] = useState("");
  const [error, setError] = useState<string | null>(null);
  const submit = async () => {
    await start({ token });
    location.replace(props.next);
  };
  return (
    <section class="card">
      <h2>With a token or the admin key</h2>
      <p>
        The browser gets a session of its own with the same role; what you paste isn't kept. <span class="mono">cloudex status --show-key</span> shows the
        admin key.
      </p>
      <form
        onSubmit={(e) => {
          e.preventDefault();
          setError(null);
          submit().catch((x) => setError(x.message));
        }}
      >
        <input type="password" autocomplete="off" spellcheck={false} placeholder="cx_… or the admin key" value={token} onInput={(e) => setToken(e.currentTarget.value)} />
        <button type="submit" class="btn btn-primary" disabled={!token.trim()}>
          Sign in
        </button>
      </form>
      <Problem error={error} />
    </section>
  );
}

// Approving another computer's sign-in, with what it said about itself.
function Approve(props: { code: string; me: Me }) {
  const [req, setReq] = useState<DeviceRequest | null | undefined>(undefined);
  const [role, setRole] = useState<Role>("user");
  const [own, setOwn] = useState(false);
  const [days, setDays] = useState("90");
  const [error, setError] = useState<string | null>(null);
  const [done, setDone] = useState<string | null>(null);
  const norm = props.code.toUpperCase().replace(/[^A-Z]/g, "");
  const code = norm.length === 8 ? `${norm.slice(0, 4)}-${norm.slice(4)}` : props.code;

  useEffect(() => {
    if (props.me.role !== "admin") {
      setReq(null);
      return;
    }
    get<DeviceRequest[]>("/v1/auth/device").then(
      (all) => {
        const r = all.find((x) => x.userCode === code) ?? null;
        setReq(r);
        // Admin only when chosen here: anyone can ask for it without a key.
        if (r?.role === "read") setRole("read");
      },
      (e) => setError(e.message),
    );
  }, []);

  if (done) {
    return (
      <main class="page narrow">
        <p class="note">{done}</p>
        <button type="button" class="btn" onClick={() => navigate("/")}>
          Go to the dashboard
        </button>
      </main>
    );
  }
  if (props.me.role !== "admin") {
    return (
      <main class="page narrow">
        <Problem error="Only an admin can approve sign-ins, and this browser's session isn't one." />
      </main>
    );
  }
  if (req === undefined) return <main class="page narrow"><p class="muted">Looking up {code}…</p></main>;
  if (req === null) {
    return (
      <main class="page narrow">
        <Problem error={error ?? `No sign-in is waiting with the code ${code}. It may have expired (codes last 10 minutes), or been approved already.`} />
      </main>
    );
  }
  const decide = async (approve: boolean) => {
    if (approve) {
      await post("/v1/auth/device/approve", {
        user_code: code,
        role,
        ownOnly: own,
        ...(days ? { expiresInSec: Number(days) * 86_400 } : {}),
      });
      setDone(`Approved. ${req.name} is signed in as ${role}${own ? ", with its own sandboxes only" : ""}.`);
    } else {
      await post("/v1/auth/device/deny", { user_code: code });
      setDone(`Refused. ${req.name} isn't signed in.`);
    }
  };
  return (
    <main class="page narrow">
      <h1>Approve a sign-in</h1>
      <p class="warn">Only approve a code you started yourself, on a computer you're using. Anyone who gets you to approve their code gets in.</p>
      <dl class="facts">
        <dt>Code</dt>
        <dd class="mono">{req.userCode}</dd>
        <dt>Calls itself</dt>
        <dd>{req.name}</dd>
        <dt>From</dt>
        <dd>
          {req.ip ?? "?"}
          {req.country ? ` (${req.country})` : ""}
        </dd>
        <dt>Program</dt>
        <dd class="mono small">{req.userAgent ?? "?"}</dd>
        <dt>Asked</dt>
        <dd>{ago(req.createdAt)}</dd>
        <dt>Asked for</dt>
        <dd>{req.role ? `the role ${req.role}` : "no role in particular"}</dd>
      </dl>
      {req.role === "admin" ? (
        <p class="warn">
          This sign-in asks for admin, which can do everything, including making tokens and approving other sign-ins. It starts as user below; choose admin
          only if you know this computer and it needs it.
        </p>
      ) : null}
      <div class="form">
        <label>
          Role
          <select value={role} onChange={(e) => setRole(e.currentTarget.value as Role)}>
            <option value="user">user: sandboxes and saves</option>
            <option value="read">read: look, don't touch</option>
            <option value="admin">admin: everything</option>
          </select>
        </label>
        <label class="check">
          <input type="checkbox" checked={own} onChange={(e) => setOwn(e.currentTarget.checked)} disabled={role === "admin"} />
          Only the sandboxes it makes
        </label>
        <label>
          Expires after (days; empty for never)
          <input type="number" min="1" max="3650" value={days} onInput={(e) => setDays(e.currentTarget.value)} />
        </label>
      </div>
      <div class="row">
        <Act run={() => decide(true)} onError={setError} kind="primary">
          Approve
        </Act>
        <Act run={() => decide(false)} onError={setError} kind="danger">
          Refuse
        </Act>
      </div>
      <Problem error={error} />
    </main>
  );
}
