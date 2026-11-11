// Pure helpers for the browser checks: arguments, finding a browser, the
// PASS/FAIL/SKIP lines, hiding secrets, and waiting for a condition.
// Nothing here starts a process or touches the network, so selftest.mjs can
// check all of it without a browser or a deployment.
import fs from "node:fs";
import net from "node:net";
import os from "node:os";
import path from "node:path";

export const SCENARIOS = ["signin", "terminal", "reconnect", "desktop", "agent", "work", "nodes", "code", "next", "phone", "revoke", "console"];

export const USAGE = `node run.mjs [--browser chrome|firefox] [--only <scenario>[,<scenario>...]] [--keep]
            [--out <dir>] [--results <results.tsv>]
node run.mjs --find chrome|firefox     print the browser it would use, or exit 1
node selftest.mjs                      check the pure parts, without a browser

Scenarios, run in this order: ${SCENARIOS.join(" ")}.
signin always runs (the others need its signed-in browser), and so does the
console check at the end.
Env: CLOUDEX_BIN, CLOUDEX_CONFIG_DIR (as verify.sh), CHROME, FIREFOX (the browser
to use), VERIFY_AGENTS=1 (run the agent turn, which spends provider tokens),
VERIFY_PREFIX (name prefix; verify.sh passes its own), VERIFY_OUT.`;

export function parseArgs(argv) {
  const o = { browser: "chrome", only: null, keep: false, out: null, results: null, find: null, help: false };
  for (let i = 0; i < argv.length; i++) {
    let a = argv[i];
    let inline;
    if (a.startsWith("--") && a.includes("=")) [a, inline] = [a.slice(0, a.indexOf("=")), a.slice(a.indexOf("=") + 1)];
    const val = () => {
      const v = inline ?? argv[++i];
      if (v === undefined || v === "" || (inline === undefined && v.startsWith("--"))) throw new Error(`${a} needs a value`);
      return v;
    };
    if (a === "--browser") o.browser = val();
    else if (a === "--only") o.only = val().split(",").map((s) => s.trim()).filter(Boolean);
    else if (a === "--keep") o.keep = true;
    else if (a === "--out") o.out = val();
    else if (a === "--results") o.results = val();
    else if (a === "--find") o.find = val();
    else if (a === "--help" || a === "-h") o.help = true;
    else throw new Error(`unknown argument ${a}`);
  }
  for (const b of [o.browser, o.find].filter(Boolean)) if (b !== "chrome" && b !== "firefox") throw new Error(`no browser ${b}: chrome or firefox`);
  for (const s of o.only ?? []) if (!SCENARIOS.includes(s)) throw new Error(`no scenario ${s}: ${SCENARIOS.join(", ")}`);
  return o;
}

// Where Chrome and Firefox usually are. A bare name is looked up on PATH.
export function browserCandidates(kind, { env = process.env, platform = process.platform, home = os.homedir() } = {}) {
  const given = kind === "chrome" ? env.CHROME : env.FIREFOX;
  if (given) return [given];
  if (platform === "darwin") {
    const apps = kind === "chrome"
      ? [
          "Google Chrome.app/Contents/MacOS/Google Chrome",
          "Google Chrome for Testing.app/Contents/MacOS/Google Chrome for Testing",
          "Chromium.app/Contents/MacOS/Chromium",
          "Google Chrome Canary.app/Contents/MacOS/Google Chrome Canary",
        ]
      : ["Firefox.app/Contents/MacOS/firefox", "Firefox Developer Edition.app/Contents/MacOS/firefox", "Firefox Nightly.app/Contents/MacOS/firefox"];
    return apps.flatMap((a) => [`/Applications/${a}`, path.join(home, "Applications", a)]);
  }
  return kind === "chrome"
    ? ["google-chrome", "google-chrome-stable", "chromium", "chromium-browser", "chrome", "/opt/google/chrome/chrome", "/snap/bin/chromium"]
    : ["firefox", "firefox-esr", "/opt/firefox/firefox", "/snap/bin/firefox"];
}

const runnable = (p) => {
  try {
    fs.accessSync(p, fs.constants.X_OK);
    return fs.statSync(p).isFile();
  } catch {
    return false;
  }
};

export function findBrowser(kind, opts = {}) {
  const env = opts.env ?? process.env;
  const dirs = (env.PATH ?? "").split(path.delimiter).filter(Boolean);
  for (const c of browserCandidates(kind, opts)) {
    if (c.includes("/")) {
      if (runnable(c)) return c;
    } else {
      for (const d of dirs) if (runnable(path.join(d, c))) return path.join(d, c);
    }
  }
  return null;
}

// Chrome writes the port it chose and its browser target's path here.
export function parseDevToolsActivePort(text) {
  const [port, p] = String(text).split("\n").map((s) => s.trim());
  if (!/^\d+$/.test(port ?? "") || !p?.startsWith("/devtools/browser/")) return null;
  return { port: Number(port), path: p };
}

// Firefox says where WebDriver BiDi listens on stderr.
export function parseBidiListening(text) {
  return /WebDriver BiDi listening on (ws:\/\/[^\s]+)/.exec(String(text))?.[1] ?? null;
}

export const oneLine = (s, n = 300) => {
  const t = String(s ?? "").replace(/\s+/g, " ").trim();
  return t.length > n ? t.slice(0, n - 1) + "…" : t;
};

// Hides sign-in codes, tokens and session cookies in anything printed. The
// run also adds each secret it holds, so even an odd shape is caught.
export function makeRedactor() {
  const secrets = new Set();
  const f = (s) => {
    let t = String(s ?? "");
    for (const x of secrets) t = t.split(x).join("<secret>");
    return t
      .replace(/(__Host-cx_session=)[^;\s"']+/g, "$1<secret>")
      .replace(/([#?&](?:link|device_code|token|key|secret)=)[^&\s"'#]+/gi, "$1<secret>")
      .replace(/(?<![-\w])cx_(?!session=)[A-Za-z0-9_-]{4,}/g, "cx_<secret>");
  };
  f.add = (x) => {
    if (x && String(x).length >= 6) secrets.add(String(x));
  };
  return f;
}

// The same lines verify.sh prints, and its results.tsv rows.
export function reporter({ write = (s) => process.stdout.write(s), results = null, color = !process.env.NO_COLOR, redact = (s) => s } = {}) {
  const counts = { PASS: 0, FAIL: 0, SKIP: 0 };
  const lines = [];
  const C = { PASS: 32, FAIL: 31, SKIP: 33 };
  const line = (res, id, desc, detail) => {
    counts[res]++;
    const d = res === "PASS" ? "" : oneLine(redact(detail));
    const what = redact(desc);
    lines.push({ res, id, desc: what, detail: d });
    write(`${color ? `\x1b[${C[res]}m${res}\x1b[0m` : res} ${id.padEnd(5)} ${what}${res === "PASS" ? "" : `: ${d}`}\n`);
    if (results) fs.appendFileSync(results, `${id}\t${res}\t${what}\t${d}\n`);
  };
  return {
    counts,
    lines,
    pass: (id, desc) => line("PASS", id, desc, ""),
    fail: (id, desc, detail) => line("FAIL", id, desc, detail || "failed"),
    skip: (id, desc, why) => line("SKIP", id, desc, why || "skipped"),
  };
}

// Waits for fn() to give something truthy, every `every` ms, for at most ms.
// An error from fn counts as "not yet"; the last one goes in the timeout's message.
export async function poll(fn, { ms = 15_000, every = 200, what = "a condition" } = {}) {
  const end = Date.now() + ms;
  let last = null;
  for (;;) {
    try {
      const v = await fn();
      if (v) return v;
    } catch (e) {
      if (e?.fatal) throw e;
      last = e;
    }
    if (Date.now() >= end) {
      const why = last ? `; last error: ${oneLine(last.message, 160)}` : "";
      throw new Error(`timed out after ${Math.round(ms / 1000)} s waiting for ${what}${why}`);
    }
    await new Promise((r) => setTimeout(r, Math.min(every, Math.max(0, end - Date.now()))));
  }
}

export function freePort() {
  return new Promise((resolve, reject) => {
    const s = net.createServer();
    s.unref();
    s.on("error", reject);
    s.listen(0, "127.0.0.1", () => {
      const { port } = s.address();
      s.close(() => resolve(port));
    });
  });
}

export const stamp = (d = new Date()) => [d.getHours(), d.getMinutes(), d.getSeconds()].map((n) => String(n).padStart(2, "0")).join("");

// The text of a terminal screen as the page shows it, one row a line,
// without trailing blanks: what termText() in the page does, for tests.
export const normalizeRows = (rows) => rows.map((r) => r.replace(/ /g, " ").trimEnd()).join("\n");

// Is the path a dashboard route that `next` may send a browser to?
// The check (B33) expects the login page to agree with this.
export const dashboardRoute = (p) => /^\/(s\/[^/?#]+(\/[^?#]*)?|saves|access)?$/.test(p);

// The request URLs (fragments left out: they never leave the browser) that
// carry a token, secret or sign-in code.
export function leakyUrls(urls, secrets = []) {
  return urls.filter((u) => {
    const sent = String(u).split("#")[0];
    if (/[?&](link|device_code|token|key|secret|access_token)=/i.test(sent)) return true;
    if (/\bcx_[A-Za-z0-9_-]{8,}/.test(sent)) return true;
    return secrets.some((s) => s && s.length >= 6 && sent.includes(s));
  });
}
