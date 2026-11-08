// The Cloudex dashboard: sandboxes, their terminals, desktops, agents,
// commands and files, named saves, and access, for this one deployment.
import "@xterm/xterm/css/xterm.css";
import "./style.css";
import type { Me } from "@cloudex/schema";
import { render } from "preact";
import { useEffect, useState } from "preact/hooks";
import { api, version, whoAmI } from "./api";
import { Link, match, navigate, useLocation } from "./router";
import { Access } from "./pages/access";
import { Login } from "./pages/login";
import { Saves } from "./pages/saves";
import { SandboxPage } from "./pages/sandbox";
import { Sandboxes } from "./pages/sandboxes";
import { Problem } from "./ui";

function Shell() {
  const { path } = useLocation();
  const [me, setMe] = useState<Me | null>(null);
  const [error, setError] = useState<string | null>(null);
  const login = path === "/login";

  useEffect(() => {
    if (login) return;
    whoAmI().then(setMe, (e) => setError(e.message));
  }, [login]);

  if (login) return <Login />;
  if (!me) return <main class="page">{error ? <Problem error={error} /> : null}</main>;

  const s = match("/s/:ref/*", path);
  let page;
  if (path === "/") page = <Sandboxes me={me} />;
  else if (s) page = <SandboxPage ref_={s.ref!} me={me} />;
  else if (path === "/saves") page = <Saves me={me} />;
  else if (path === "/access") page = <Access me={me} />;
  else page = <p class="muted">There's no page at {path}.</p>;

  const signOut = async () => {
    await api("DELETE", "/v1/auth/session", undefined, { login: false }).catch(() => {});
    location.assign("/login");
  };
  const nav = (href: string, label: string, on: boolean) => (
    <Link href={href} class={on ? "on" : ""}>
      {label}
    </Link>
  );
  const who = me.token ? `${me.token.name}` : "admin key";
  return (
    <>
      <header class="top">
        <Link href="/" class="mark">
          <i aria-hidden="true" />
          Cloudex
        </Link>
        <nav>
          {nav("/", "Sandboxes", path === "/" || !!s)}
          {nav("/saves", "Saves", path === "/saves")}
          {me.role === "admin" ? nav("/access", "Access", path === "/access") : null}
        </nav>
        <div class="who">
          <span title={me.token ? `session ${me.token.id}` : ""}>
            {who} · {me.role}
            {me.ownOnly ? ", own sandboxes" : ""}
          </span>
          <button type="button" class="btn btn-quiet" onClick={signOut}>
            Sign out
          </button>
        </div>
      </header>
      <main class="page">{page}</main>
      <footer class="foot">
        <span>Cloudex {version()}</span>
        <a href="https://cloudex.build/docs/dashboard" target="_blank" rel="noopener noreferrer">
          Docs
        </a>
      </footer>
    </>
  );
}

// Old links to /sandboxes/x go to /s/x.
if (location.pathname.startsWith("/sandboxes/")) navigate(location.pathname.replace(/^\/sandboxes\//, "/s/"), true);
render(<Shell />, document.getElementById("app")!);
