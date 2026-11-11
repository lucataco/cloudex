#!/usr/bin/env node
// The dashboard in a real browser, for verify.sh's browser area: headless
// Chrome over the DevTools protocol or headless Firefox over WebDriver BiDi,
// with Node's built-ins only. See USAGE in util.mjs, or --help.
//
// It signs in the way a user does, with `cloudex dashboard --no-open --json`
// (the CLI and config verify.sh uses: CLOUDEX_BIN, CLOUDEX_CONFIG_DIR), and
// never reads the admin key. It makes a sandbox named <prefix>-<browser>, and
// when it ends, even after a failure or Ctrl-C, it signs out every browser
// session it started, revokes them and its tokens, refuses sign-ins still
// waiting, deletes the sandbox (unless --keep), closes the browser and
// removes its profile. Nothing it prints holds a sign-in link, token or secret.
import { execFile, spawn } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { killAllSync, launch } from "./driver.mjs";
import { ORDER } from "./scenarios.mjs";
import { SCENARIOS, USAGE, findBrowser, makeRedactor, oneLine, parseArgs, poll, reporter, stamp } from "./util.mjs";

let opts;
try {
  opts = parseArgs(process.argv.slice(2));
} catch (e) {
  console.error(`${e.message}\n\n${USAGE}`);
  process.exit(2);
}
if (opts.help) {
  console.log(USAGE);
  process.exit(0);
}
if (opts.find) {
  const exe = findBrowser(opts.find);
  if (!exe) process.exit(1);
  console.log(exe);
  process.exit(0);
}

const kind = opts.browser;
const CX = process.env.CLOUDEX_BIN || "cloudex";
const started = Date.now();
const prefix = `${process.env.VERIFY_PREFIX || `vr${stamp()}`}-${kind}`;
const redact = makeRedactor();
const work = fs.mkdtempSync(path.join(os.tmpdir(), "cloudex-vr-browser-"));
const dl = path.join(work, "downloads");
fs.mkdirSync(dl);
const out = opts.out ?? (process.env.VERIFY_OUT ? path.join(process.env.VERIFY_OUT, `browser-${kind}`) : path.join(work, "out"));
fs.mkdirSync(out, { recursive: true });
// With --out or VERIFY_OUT the screenshots outlive the run; otherwise they'd
// go with the work directory, so keep them next to it.
const shots = opts.out || process.env.VERIFY_OUT ? out : path.join(os.tmpdir(), `cloudex-vr-shots-${prefix}`);
const R = reporter({ results: opts.results, redact });

// What has to be undone at the end.
const state = {
  box: null, // { id, name }
  sessions: new Set(), // dashboard session ids seen in any of our browser contexts
  codes: new Set(), // device sign-in codes started and not yet approved
  procs: new Set(), // cloudex login and the like
  contexts: new Set(), // browser contexts other than the main one
  before: null, // session ids that existed before the run (admin only)
};
let browser = null;

// ---------------------------------------------------------------- the CLI

function cx(args, env = {}, ms = 300_000) {
  return new Promise((resolve) => {
    const child = execFile(CX, args, { env: { ...process.env, CLOUDEX_NO_UPDATE_CHECK: "1", ...env }, timeout: ms, maxBuffer: 64 << 20 }, (err, stdout, stderr) => {
      let json;
      try {
        json = JSON.parse(stdout);
      } catch {}
      resolve({ code: err ? (typeof err.code === "number" ? err.code : 1) : 0, out: String(stdout), err: oneLine(redact(stderr), 300), json });
    });
    child.stdin?.end();
  });
}

function spawnCx(args, env = {}) {
  const c = { out: "", err: "", exited: false, status: null };
  c.proc = spawn(CX, args, { env: { ...process.env, CLOUDEX_NO_UPDATE_CHECK: "1", ...env }, stdio: ["ignore", "pipe", "pipe"] });
  c.proc.stdout.on("data", (d) => (c.out += d));
  c.proc.stderr.on("data", (d) => (c.err = oneLine(redact(c.err + d), 300)));
  c.proc.on("exit", (status) => {
    c.exited = true;
    c.status = status;
    state.procs.delete(c);
  });
  c.proc.on("error", (e) => {
    c.exited = true;
    c.status = -1;
    c.err = e.message;
    state.procs.delete(c);
  });
  state.procs.add(c);
  return c;
}

// ---------------------------------------------------------------- what scenarios get

const T = {
  kind,
  prefix,
  work,
  dl,
  state,
  redact,
  secrets: [],
  cx: (args, env) => cx(args, env),
  spawnCx,
  secret(s) {
    redact.add(s);
    T.secrets.push(s);
  },
  // A fresh one-time sign-in link, held only in memory.
  async link() {
    const r = await cx(["dashboard", "--no-open", "--json"]);
    const url = r.json?.url;
    if (r.code !== 0 || !url) throw new Error(`cloudex dashboard --no-open --json failed: ${r.err || "no link in its output"}`);
    T.secret(url);
    const code = /link=([^&#]+)/.exec(url)?.[1];
    if (code) T.secret(code);
    return url;
  },
  // Who the page is signed in as, or null; every session seen is noted for the end.
  async me(page) {
    const me = await page.ev(() => fetch("/v1/me", { headers: { "x-cloudex": "1" }, cache: "no-store" }).then((r) => (r.ok ? r.json() : null)));
    if (me?.token?.kind === "session") state.sessions.add(me.token.id);
    return me ?? null;
  },
  async signIn(page) {
    await page.go(await T.link());
    await page.waitFor(() => location.pathname !== "/login" && !!document.querySelector("header.top .who"), [], { ms: 30_000, what: "the dashboard, signed in" });
    const me = await T.me(page);
    if (!me?.token) throw new Error("signed in, but /v1/me shows no session");
    return me;
  },
  async newContext() {
    const ctx = await browser.newContext();
    state.contexts.add(ctx);
    return { ctx, page: await browser.newPage(ctx) };
  },
  // Signs the context out from one of its pages, then closes it.
  async endContext(ctx) {
    const pages = [...browser.pages].filter((p) => p.ctx === ctx && !p.closed);
    for (const p of pages) {
      if (await signOut(p)) break;
    }
    await browser.disposeContext(ctx);
    state.contexts.delete(ctx);
  },
  scenario(name) {
    let broken = null;
    const S = {
      page: T.main,
      async check(id, desc, fn, { alone = false } = {}) {
        if (broken && !alone) return R.skip(id, desc, `${broken} failed first`);
        try {
          await fn();
          R.pass(id, desc);
        } catch (e) {
          if (!alone) broken = id;
          await shoot(S.page, id);
          R.fail(id, desc, e?.message ?? String(e));
        }
      },
      skip: (id, desc, why) => R.skip(id, desc, why),
    };
    return S;
  },
};

const within = (p, ms) => Promise.race([p, new Promise((r) => setTimeout(() => r(undefined), ms))]);

async function signOut(page) {
  const r = await within(
    page
      .ev(() =>
        location.protocol.startsWith("http")
          ? fetch("/v1/auth/session", { method: "DELETE", headers: { "x-cloudex": "1" } }).then((x) => x.status)
          : null,
      )
      .catch(() => null),
    5_000,
  );
  return typeof r === "number";
}

async function shoot(page, id) {
  if (!page || page.closed) return;
  fs.mkdirSync(shots, { recursive: true });
  await within(page.shot(path.join(shots, `${id}.png`)).catch(() => {}), 20_000);
}

// ---------------------------------------------------------------- the end, whatever happens

let cleaning = null;
function cleanup() {
  cleaning ??= (async () => {
    for (const c of state.procs) c.proc.kill("SIGTERM");
    if (browser) {
      for (const ctx of [...state.contexts]) await within(T.endContext(ctx).catch(() => {}), 20_000);
      if (T.main && !T.main.closed) await signOut(T.main);
      await within(browser.close().catch(() => {}), 20_000);
    }
    killAllSync();
    const admin = T.admin;
    if (admin) {
      for (const id of state.sessions) await cx(["tokens", "revoke", id]);
      const list = await cx(["tokens", "--json"]);
      const tokens = Array.isArray(list.json) ? list.json : [];
      for (const t of tokens) if (t.name?.startsWith(`${prefix}-`)) await cx(["tokens", "revoke", t.id]);
      // A session made between a sign-in and reading its id (a crash in
      // between) has no id on record: one made during the run, by this
      // browser, that wasn't there before goes too.
      const word = kind === "chrome" ? "Chrome" : "Firefox";
      if (state.before)
        for (const t of tokens)
          if (t.kind === "session" && !state.before.has(t.id) && !state.sessions.has(t.id) && t.createdAt >= started - 5_000 && t.name?.startsWith(`dashboard (${word}`))
            await cx(["tokens", "revoke", t.id]);
      for (const c of state.codes) await cx(["tokens", "deny", c]);
    }
    if (state.box && !opts.keep) {
      await cx(["delete", state.box.id, "--yes"]);
      const l = await cx(["list", "--json"]);
      for (const s of Array.isArray(l.json) ? l.json : []) if (s.name?.startsWith(prefix)) await cx(["delete", s.id, "--yes"]);
    }
    fs.rmSync(work, { recursive: true, force: true });
    if (opts.keep && state.box) console.log(`kept the sandbox ${state.box.name}; the browser's sessions and tokens were ended anyway`);
  })();
  return cleaning;
}

// Nothing of this run's left on the deployment.
async function leftovers() {
  const left = [];
  if (!opts.keep) {
    const l = await cx(["list", "--json"]);
    for (const s of Array.isArray(l.json) ? l.json : []) if (s.name?.startsWith(prefix)) left.push(`sandbox ${s.name}`);
  }
  if (T.admin) {
    const t = await cx(["tokens", "--json"]);
    for (const x of Array.isArray(t.json) ? t.json : []) if (x.name?.startsWith(`${prefix}-`) || state.sessions.has(x.id)) left.push(`${x.kind} ${x.id}`);
    const w = await cx(["tokens", "approve", "--json"]);
    for (const d of Array.isArray(w.json) ? w.json : []) if (state.codes.has(d.userCode) || d.name?.startsWith(`${prefix}-`)) left.push(`sign-in ${d.userCode}`);
  }
  return left;
}

process.on("exit", () => {
  for (const c of state.procs) c.proc.kill("SIGKILL");
  killAllSync();
  try {
    fs.rmSync(work, { recursive: true, force: true });
  } catch {}
});
for (const sig of ["SIGINT", "SIGTERM", "SIGHUP"])
  process.on(sig, async () => {
    console.error(`\n${sig}: cleaning up`);
    setTimeout(() => process.exit(130), 120_000).unref();
    await cleanup().catch(() => {});
    // Die of the signal, so a shell running this (verify.sh) stops too.
    process.removeAllListeners(sig);
    process.kill(process.pid, sig);
  });

// ---------------------------------------------------------------- the run

async function main() {
  const exe = findBrowser(kind);
  if (!exe) {
    R.skip("B00", `the dashboard in ${kind}`, `no ${kind} found; set ${kind.toUpperCase()} to its path`);
    return;
  }
  const st = await cx(["status", "--json"]);
  if (st.code !== 0 || !st.json?.apiUrl) {
    R.fail("B00", "cloudex status, to find the deployment", st.err || "no apiUrl in its output");
    return;
  }
  T.api = st.json.apiUrl.replace(/\/+$/, "");
  T.admin = st.json.me?.role === "admin";
  if (T.admin) {
    const t = await cx(["tokens", "--json"]);
    if (Array.isArray(t.json)) state.before = new Set(t.json.map((x) => x.id));
  }
  if (process.env.VERIFY_AGENTS === "1") {
    const k = await cx(["keys", "--json"]);
    const has = Array.isArray(k.json) && k.json.some((x) => x.provider === "anthropic");
    T.agents = has ? { on: true } : { on: false, why: "no anthropic key on the deployment (cloudex keys set anthropic)" };
  } else T.agents = { on: false, why: "it spends provider tokens: set VERIFY_AGENTS=1, or run verify.sh with the agents area" };
  // VERIFY_NODE: the run's sandbox is on that node (v0.2.2), where agents
  // run as on Cloudflare since M6 (provider calls go through the node's agent).
  T.node = process.env.VERIFY_NODE || "";

  console.log(`browser checks in ${kind === "chrome" ? "Chrome" : "Firefox"} (${exe}), sandbox ${prefix}, screenshots of failures in ${shots}`);
  const made = await cx(["new", "--name", prefix, "--no-use", "--json", ...(T.node ? ["--on", T.node] : [])]);
  if (made.code !== 0 || !made.json?.id) {
    R.fail("B00", "a sandbox for the browser checks", made.err || "cloudex new gave no id");
    // It may exist even so: cleanup looks it up by name.
    state.box = { id: prefix, name: prefix };
    return;
  }
  state.box = T.box = { id: made.json.id, name: made.json.name ?? prefix };

  try {
    browser = T.browser = await launch(kind, exe, { downloads: dl, logFile: path.join(out, "browser.log") });
    T.main = await browser.newPage();
  } catch (e) {
    R.fail("B00", `start ${kind} headless`, e.message);
    return;
  }

  if (!(await ORDER.signin(T))) return; // nothing else works signed out
  for (const name of SCENARIOS) {
    if (name === "signin" || name === "console") continue;
    if (opts.only && !opts.only.includes(name)) continue;
    try {
      await ORDER[name](T);
    } catch (e) {
      // A scenario's own setup failed outside its checks.
      R.fail("B00", `the ${name} checks`, e?.message ?? String(e));
    }
  }
  await ORDER.console(T);
}

let crashed = null;
try {
  await main();
} catch (e) {
  crashed = e;
  R.fail("B00", "the browser checks ran to the end", e?.stack ?? String(e));
}
await cleanup().catch((e) => console.error(`cleanup: ${redact(e.message)}`));
if (state.box) {
  const left = await leftovers().catch((e) => [`couldn't check: ${e.message}`]);
  if (left.length) R.fail("B40", "the run left no sandbox, session, token or sign-in behind", left.join(", "));
  else R.pass("B40", "the run left no sandbox, session, token or sign-in behind");
}
const { PASS, FAIL, SKIP } = R.counts;
const pictures = fs.existsSync(shots) ? fs.readdirSync(shots).filter((f) => f.endsWith(".png")).length : 0;
console.log(`${PASS + FAIL + SKIP} browser checks in ${kind}, ${FAIL} failed, ${SKIP} skipped${pictures ? `; ${pictures} screenshot(s) in ${shots}` : ""}`);
process.exit(FAIL || crashed ? 1 : 0);
