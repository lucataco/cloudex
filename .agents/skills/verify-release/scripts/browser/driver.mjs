// A small driver for headless Chrome (DevTools protocol) and headless Firefox
// (WebDriver BiDi), with Node's own WebSocket and nothing else. Both give the
// same page object: go, ev, waitFor, click, setValue, typeText, press, size,
// shot, front, setFiles, reload, and the console lines and request URLs seen.
//
// A browser always starts with a fresh temporary profile and goes with it:
// close() ends it and removes the profile, and killAllSync() does the same
// from an exit handler.
import { spawn } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { freePort, oneLine, parseBidiListening, parseDevToolsActivePort, poll } from "./util.mjs";

// In every page before its own scripts: WebSockets are kept in __vrWs (so the
// reconnect check can close one from the page; CDP's offline mode leaves an
// open socket alone), and CSP violations go to the console, where both
// browsers report them. The hook makes plain WebSockets, not a subclass's:
// noVNC takes a socket only if send and the rest are on its own prototype.
const PRELOAD_BODY = `
  if (window.__vrHooked) return;
  window.__vrHooked = true;
  window.__vrWs = [];
  window.WebSocket = new Proxy(window.WebSocket, {
    construct(W, a) { const w = Reflect.construct(W, a); window.__vrWs.push(w); return w; },
  });
  document.addEventListener("securitypolicyviolation", (e) =>
    console.error("[csp] " + e.violatedDirective + " blocked " + String(e.blockedURI).split("?")[0]));
`;

const KEYS = {
  Enter: { key: "Enter", code: "Enter", keyCode: 13, text: "\r", bidi: "" },
  Escape: { key: "Escape", code: "Escape", keyCode: 27, bidi: "" },
  Backspace: { key: "Backspace", code: "Backspace", keyCode: 8, bidi: "" },
};

// An expression that runs fn(...args) in the page and gives back JSON, so
// both protocols return plain values the same way.
export const wrap = (fn, args) =>
  `(async () => { const v = await (${String(fn)})(...${JSON.stringify(args)}); return v === undefined ? "null" : JSON.stringify(v); })()`;

const live = new Set();

// For process.on("exit"): no waiting, just kill and remove.
export function killAllSync() {
  for (const b of live) b.killSync();
}

// One JSON-RPC connection over a WebSocket: CDP and BiDi differ only in how
// answers and events look.
class Rpc {
  static open(url, kind) {
    return new Promise((resolve, reject) => {
      const ws = new WebSocket(url);
      const t = setTimeout(() => reject(new Error(`no answer from the browser at ${url} within 20 s`)), 20_000);
      ws.onopen = () => (clearTimeout(t), resolve(new Rpc(ws, kind)));
      ws.onerror = () => (clearTimeout(t), reject(new Error(`couldn't connect to the browser at ${url}`)));
    });
  }
  constructor(ws, kind) {
    this.ws = ws;
    this.kind = kind;
    this.n = 0;
    this.pending = new Map();
    this.listeners = new Set();
    this.closed = false;
    ws.onmessage = (e) => {
      let m;
      try {
        m = JSON.parse(typeof e.data === "string" ? e.data : Buffer.from(e.data).toString("utf8"));
      } catch {
        return;
      }
      if (m.id !== undefined && this.pending.has(m.id)) {
        const p = this.pending.get(m.id);
        this.pending.delete(m.id);
        clearTimeout(p.timer);
        if (this.kind === "cdp") m.error ? p.reject(new Error(`${p.method}: ${m.error.message}`)) : p.resolve(m.result ?? {});
        else m.type === "error" ? p.reject(new Error(`${p.method}: ${m.error} ${m.message ?? ""}`.trim())) : p.resolve(m.result ?? {});
      } else if (m.method) for (const l of this.listeners) l(m);
    };
    ws.onclose = () => {
      this.closed = true;
      for (const p of this.pending.values()) (clearTimeout(p.timer), p.reject(Object.assign(new Error("the browser's connection closed"), { fatal: true })));
      this.pending.clear();
    };
  }
  send(method, params = {}, sessionId, ms = 60_000) {
    if (this.closed) return Promise.reject(Object.assign(new Error("the browser's connection is closed"), { fatal: true }));
    const id = ++this.n;
    return new Promise((resolve, reject) => {
      const timer = setTimeout(() => {
        this.pending.delete(id);
        reject(new Error(`the browser didn't answer ${method} within ${ms / 1000} s`));
      }, ms);
      this.pending.set(id, { resolve, reject, timer, method });
      this.ws.send(JSON.stringify({ id, method, params, ...(sessionId ? { sessionId } : {}) }));
    });
  }
  on(fn) {
    this.listeners.add(fn);
    return () => this.listeners.delete(fn);
  }
  close() {
    try {
      this.ws.close();
    } catch {}
  }
}

// What both browsers' pages share.
class Page {
  constructor(browser, ctx) {
    this.browser = browser;
    this.ctx = ctx;
    this.logs = []; // { level, kind, text }
    this.urls = []; // every request and WebSocket URL
    this.closed = false;
  }
  async ev(fn, ...args) {
    const s = await this._eval(wrap(fn, args));
    return s == null ? undefined : JSON.parse(s);
  }
  waitFor(fn, args = [], { ms = 15_000, what = "the page", every = 200 } = {}) {
    return poll(() => this.ev(fn, ...args), { ms, every, what });
  }
  async go(url) {
    const now = await this.ev(() => location.href).catch(() => "");
    // Only the fragment differs: that wouldn't load the page again.
    if (url.includes("#") && now.split("#")[0] === url.split("#")[0]) await this._navigate("about:blank");
    await this.ev(() => void (window.__vrOld = 1)).catch(() => {});
    await this._navigate(url);
    const where = url.replace(/[#?].*$/, "").replace(/^\w+:\/\/[^/]+/, "") || url;
    await this.waitFor(() => !window.__vrOld && document.readyState !== "loading", [], { ms: 30_000, what: `${where} to load` });
  }
  async reload() {
    await this.ev(() => void (window.__vrOld = 1)).catch(() => {});
    await this._reload();
    await this.waitFor(() => !window.__vrOld && document.readyState !== "loading", [], { ms: 30_000, what: "the page to load again" });
  }
  // Clicks the first enabled button or link whose text is exactly `text`.
  click(text, { selector = "button, a", ms = 15_000 } = {}) {
    return this.waitFor(
      (sel, t) => {
        const b = [...document.querySelectorAll(sel)].find((x) => x.textContent.trim() === t && !x.disabled && x.getClientRects().length > 0);
        if (!b) return false;
        b.click();
        return true;
      },
      [selector, text],
      { ms, what: `a "${text}" ${selector === "button, a" ? "button or link" : selector} to click` },
    );
  }
  // Sets an input, textarea or select the way typing does, so Preact sees it.
  setValue(selector, value, { ms = 15_000 } = {}) {
    return this.waitFor(
      (sel, v) => {
        const el = document.querySelector(sel);
        if (!el) return false;
        Object.getOwnPropertyDescriptor(Object.getPrototypeOf(el), "value").set.call(el, v);
        el.dispatchEvent(new Event(el.tagName === "SELECT" ? "change" : "input", { bubbles: true }));
        return true;
      },
      [selector, value],
      { ms, what: `${selector} to fill in` },
    );
  }
  text() {
    return this.ev(() => document.body?.innerText ?? "");
  }
  // A hidden or unfocused tab stalls timers and xterm's drawing: the
  // terminal checks call this first.
  async front() {
    await this._front();
    await this.waitFor(() => document.visibilityState === "visible", [], { ms: 10_000, what: "the tab to be visible" });
  }
}

// ---------------------------------------------------------------- Chrome

class ChromePage extends Page {
  constructor(browser, ctx, targetId, sessionId) {
    super(browser, ctx);
    this.targetId = targetId;
    this.sid = sessionId;
  }
  call(method, params = {}, ms) {
    return this.browser.rpc.send(method, params, this.sid, ms);
  }
  async init() {
    this.off = this.browser.rpc.on((m) => {
      if (m.sessionId !== this.sid) return;
      const p = m.params ?? {};
      if (m.method === "Runtime.exceptionThrown") this.logs.push({ level: "error", kind: "exception", text: p.exceptionDetails?.exception?.description ?? p.exceptionDetails?.text ?? "" });
      else if (m.method === "Runtime.consoleAPICalled") this.logs.push({ level: p.type, kind: "console", text: (p.args ?? []).map((a) => a.value ?? a.description ?? "").join(" ") });
      else if (m.method === "Log.entryAdded") this.logs.push({ level: p.entry.level, kind: p.entry.source, text: p.entry.text ?? "" });
      else if (m.method === "Network.requestWillBeSent") this.urls.push(p.request.url);
      else if (m.method === "Network.webSocketCreated") this.urls.push(p.url);
      else if (m.method === "Page.javascriptDialogOpening") this.call("Page.handleJavaScriptDialog", { accept: true }).catch(() => {});
    });
    for (const d of ["Page.enable", "Runtime.enable", "Log.enable", "Network.enable", "DOM.enable"]) await this.call(d);
    await this.call("Page.addScriptToEvaluateOnNewDocument", { source: `(() => {${PRELOAD_BODY}})();` });
    await this.size(1300, 820);
    await this.call("Emulation.setFocusEmulationEnabled", { enabled: true }).catch(() => {});
  }
  async _eval(expression) {
    // As a user's gesture: Chrome holds back an .html download, among other
    // things, when a script clicks it on its own.
    const r = await this.call("Runtime.evaluate", { expression, returnByValue: true, awaitPromise: true, userGesture: true });
    if (r.exceptionDetails) throw new Error(oneLine(r.exceptionDetails.exception?.description?.split("\n")[0] ?? r.exceptionDetails.text, 200));
    return r.result?.value ?? null;
  }
  async _navigate(url) {
    const r = await this.call("Page.navigate", { url });
    if (r.errorText) throw new Error(`couldn't load the page: ${r.errorText}`);
  }
  _reload() {
    return this.call("Page.reload", {});
  }
  typeText(text) {
    return this.call("Input.insertText", { text });
  }
  async press(name) {
    const k = KEYS[name];
    await this.call("Input.dispatchKeyEvent", { type: "keyDown", key: k.key, code: k.code, windowsVirtualKeyCode: k.keyCode, ...(k.text ? { text: k.text } : {}) });
    await this.call("Input.dispatchKeyEvent", { type: "keyUp", key: k.key, code: k.code, windowsVirtualKeyCode: k.keyCode });
  }
  size(width, height, mobile = false) {
    return this.call("Emulation.setDeviceMetricsOverride", { width, height, deviceScaleFactor: 1, mobile });
  }
  async shot(file) {
    const r = await this.call("Page.captureScreenshot", { format: "png" }, 20_000);
    fs.writeFileSync(file, Buffer.from(r.data, "base64"));
  }
  async _front() {
    await this.call("Page.bringToFront");
    await this.call("Emulation.setFocusEmulationEnabled", { enabled: true }).catch(() => {});
  }
  async setFiles(selector, files) {
    const r = await this.call("Runtime.evaluate", { expression: `document.querySelector(${JSON.stringify(selector)})` });
    if (!r.result?.objectId) throw new Error(`no ${selector} on the page`);
    const { node } = await this.call("DOM.describeNode", { objectId: r.result.objectId });
    await this.call("DOM.setFileInputFiles", { files, backendNodeId: node.backendNodeId });
  }
  async close() {
    if (this.closed) return;
    this.closed = true;
    this.off?.();
    await this.browser.rpc.send("Target.closeTarget", { targetId: this.targetId }, undefined, 10_000).catch(() => {});
  }
}

class Chrome {
  name = "Chrome";
  static async launch(exe, { downloads, logFile }) {
    const profile = fs.mkdtempSync(path.join(os.tmpdir(), "cloudex-vr-chrome-"));
    const args = [
      "--headless=new",
      "--remote-debugging-port=0",
      `--user-data-dir=${profile}`,
      "--no-first-run",
      "--no-default-browser-check",
      "--disable-background-timer-throttling",
      "--disable-backgrounding-occluded-windows",
      "--disable-renderer-backgrounding",
      "--disable-features=Translate,MediaRouter,OptimizationHints",
      "--password-store=basic",
      "--use-mock-keychain",
      "--window-size=1300,820",
      ...(process.getuid?.() === 0 ? ["--no-sandbox"] : []),
      "about:blank",
    ];
    const b = new Chrome(spawn(exe, args, { stdio: ["ignore", "ignore", "pipe"], detached: true }), profile, logFile);
    try {
      const info = await poll(
        () => {
          if (b.exited) throw Object.assign(new Error(`Chrome exited at once: ${oneLine(b.stderr.slice(-400))}`), { fatal: true });
          try {
            return parseDevToolsActivePort(fs.readFileSync(path.join(profile, "DevToolsActivePort"), "utf8"));
          } catch {
            return null;
          }
        },
        { ms: 30_000, what: "Chrome to start and say its DevTools port" },
      );
      b.rpc = await Rpc.open(`ws://127.0.0.1:${info.port}${info.path}`, "cdp");
      await b.rpc.send("Browser.setDownloadBehavior", { behavior: "allow", downloadPath: downloads, eventsEnabled: true });
      return b;
    } catch (e) {
      await b.close();
      throw e;
    }
  }
  constructor(proc, profile, logFile) {
    this.proc = proc;
    this.profile = profile;
    this.stderr = "";
    this.exited = false;
    this.pages = new Set();
    live.add(this);
    proc.stderr.on("data", (d) => {
      this.stderr = (this.stderr + d).slice(-20_000);
      if (logFile) fs.appendFileSync(logFile, d);
    });
    proc.on("exit", () => (this.exited = true));
  }
  async newContext() {
    const { browserContextId } = await this.rpc.send("Target.createBrowserContext", {});
    return { id: browserContextId };
  }
  async disposeContext(ctx) {
    for (const p of this.pages) if (p.ctx === ctx) p.closed = true;
    await this.rpc.send("Target.disposeBrowserContext", { browserContextId: ctx.id }, undefined, 15_000).catch(() => {});
  }
  async newPage(ctx = null) {
    const { targetId } = await this.rpc.send("Target.createTarget", { url: "about:blank", ...(ctx ? { browserContextId: ctx.id } : {}) });
    const { sessionId } = await this.rpc.send("Target.attachToTarget", { targetId, flatten: true });
    const p = new ChromePage(this, ctx, targetId, sessionId);
    this.pages.add(p);
    await p.init();
    return p;
  }
  killSync() {
    try {
      if (!this.exited) process.kill(-this.proc.pid, "SIGKILL");
    } catch {}
    try {
      fs.rmSync(this.profile, { recursive: true, force: true });
    } catch {}
    live.delete(this);
  }
  async close() {
    if (this.rpc && !this.rpc.closed) await this.rpc.send("Browser.close", {}, undefined, 5_000).catch(() => {});
    await endProcess(this);
  }
}

// Waits for the browser to go, kills what's left of it, removes the profile.
async function endProcess(b) {
  b.rpc?.close();
  await poll(() => b.exited, { ms: 5_000, every: 100, what: "the browser to exit" }).catch(() => {});
  try {
    process.kill(-b.proc.pid, "SIGKILL"); // its helper processes too
  } catch {}
  await poll(() => b.exited, { ms: 5_000, every: 100, what: "the browser to be killed" }).catch(() => {});
  // A browser can write a last file as it goes: try a few times.
  await poll(
    () => {
      fs.rmSync(b.profile, { recursive: true, force: true });
      return !fs.existsSync(b.profile);
    },
    { ms: 5_000, every: 250, what: "the browser profile to be removed" },
  ).catch(() => {});
  live.delete(b);
}

// ---------------------------------------------------------------- Firefox

class FirefoxPage extends Page {
  constructor(browser, ctx, context) {
    super(browser, ctx);
    this.context = context;
  }
  call(method, params = {}, ms) {
    return this.browser.rpc.send(method, params, undefined, ms);
  }
  async init() {
    this.off = this.browser.rpc.on((m) => {
      const p = m.params ?? {};
      if (m.method === "log.entryAdded" && p.source?.context === this.context) this.logs.push({ level: p.level, kind: p.type === "javascript" ? "exception" : p.type, text: p.text ?? "" });
      else if (m.method === "network.beforeRequestSent" && p.context === this.context) this.urls.push(p.request?.url ?? "");
      else if (m.method === "browsingContext.userPromptOpened" && p.context === this.context) this.call("browsingContext.handleUserPrompt", { context: this.context, accept: true }).catch(() => {});
    });
    await this.call("script.addPreloadScript", { functionDeclaration: `() => {${PRELOAD_BODY}}`, contexts: [this.context] });
    await this.size(1300, 820);
  }
  async _eval(expression) {
    const r = await this.call("script.evaluate", { expression, target: { context: this.context }, awaitPromise: true, resultOwnership: "none" });
    if (r.type === "exception") throw new Error(oneLine(r.exceptionDetails?.text, 200));
    return r.result?.type === "string" ? r.result.value : null;
  }
  async _navigate(url) {
    // The sign-in pages move on by themselves, which can abort the first load.
    await this.call("browsingContext.navigate", { context: this.context, url, wait: "interactive" }).catch((e) => {
      if (!/abort|cancel|NS_BINDING/i.test(e.message)) throw e;
    });
  }
  _reload() {
    return this.call("browsingContext.reload", { context: this.context, wait: "interactive" });
  }
  keys(values) {
    const actions = values.flatMap((value) => [
      { type: "keyDown", value },
      { type: "keyUp", value },
    ]);
    return this.call("input.performActions", { context: this.context, actions: [{ type: "key", id: "vr-keys", actions }] });
  }
  typeText(text) {
    return this.keys([...text]);
  }
  press(name) {
    return this.keys([KEYS[name].bidi]);
  }
  size(width, height) {
    return this.call("browsingContext.setViewport", { context: this.context, viewport: { width, height }, devicePixelRatio: 1 });
  }
  async shot(file) {
    const r = await this.call("browsingContext.captureScreenshot", { context: this.context }, 20_000);
    fs.writeFileSync(file, Buffer.from(r.data, "base64"));
  }
  _front() {
    return this.call("browsingContext.activate", { context: this.context });
  }
  async setFiles(selector, files) {
    const r = await this.call("script.evaluate", {
      expression: `document.querySelector(${JSON.stringify(selector)})`,
      target: { context: this.context },
      awaitPromise: false,
      resultOwnership: "root",
    });
    const sharedId = r.result?.sharedId;
    if (!sharedId) throw new Error(`no ${selector} on the page`);
    await this.call("input.setFiles", { context: this.context, element: { sharedId }, files });
  }
  async close() {
    if (this.closed) return;
    this.closed = true;
    this.off?.();
    await this.call("browsingContext.close", { context: this.context }, 10_000).catch(() => {});
  }
}

class Firefox {
  name = "Firefox";
  static async launch(exe, { downloads, logFile }) {
    const profile = fs.mkdtempSync(path.join(os.tmpdir(), "cloudex-vr-firefox-"));
    const prefs = {
      "browser.shell.checkDefaultBrowser": false,
      "browser.startup.homepage_override.mstone": "ignore",
      "browser.aboutwelcome.enabled": false,
      "datareporting.policy.dataSubmissionEnabled": false,
      "toolkit.telemetry.reportingpolicy.firstRun": false,
      "app.update.auto": false,
      "app.update.enabled": false,
      "browser.download.folderList": 2,
      "browser.download.dir": downloads,
      "browser.download.useDownloadDir": true,
      "browser.download.always_ask_before_handling_new_types": false,
      "browser.download.manager.showWhenStarting": false,
      "browser.download.open_pdf_attachments_inline": false,
      "browser.helperApps.neverAsk.saveToDisk": "application/octet-stream,application/gzip,application/x-gzip,application/x-tar,text/html,text/plain",
      "pdfjs.disabled": true,
      "dom.disable_open_during_load": false,
    };
    fs.writeFileSync(path.join(profile, "user.js"), Object.entries(prefs).map(([k, v]) => `user_pref(${JSON.stringify(k)}, ${JSON.stringify(v)});`).join("\n") + "\n");
    const port = await freePort();
    const proc = spawn(exe, ["-headless", "-no-remote", "-profile", profile, "--remote-debugging-port", String(port), "about:blank"], {
      stdio: ["ignore", "pipe", "pipe"],
      detached: true,
      env: { ...process.env, MOZ_HEADLESS: "1", MOZ_CRASHREPORTER_DISABLE: "1" },
    });
    const b = new Firefox(proc, profile, logFile);
    try {
      const url = await poll(
        () => {
          if (b.exited) throw Object.assign(new Error(`Firefox exited at once: ${oneLine(b.stderr.slice(-400))}`), { fatal: true });
          return parseBidiListening(b.stderr);
        },
        { ms: 60_000, what: "Firefox to start WebDriver BiDi" },
      );
      b.rpc = await Rpc.open(`${url}/session`, "bidi");
      await b.rpc.send("session.new", { capabilities: { alwaysMatch: { unhandledPromptBehavior: "accept" } } });
      await b.rpc.send("session.subscribe", { events: ["log.entryAdded", "network.beforeRequestSent", "browsingContext.userPromptOpened"] });
      return b;
    } catch (e) {
      await b.close();
      throw e;
    }
  }
  constructor(proc, profile, logFile) {
    this.proc = proc;
    this.profile = profile;
    this.stderr = "";
    this.exited = false;
    this.pages = new Set();
    live.add(this);
    const take = (d) => {
      this.stderr = (this.stderr + d).slice(-20_000);
      if (logFile) fs.appendFileSync(logFile, d);
    };
    proc.stderr.on("data", take);
    proc.stdout.on("data", take);
    proc.on("exit", () => (this.exited = true));
  }
  async newContext() {
    const { userContext } = await this.rpc.send("browser.createUserContext", {});
    return { id: userContext };
  }
  async disposeContext(ctx) {
    for (const p of this.pages) if (p.ctx === ctx) p.closed = true;
    await this.rpc.send("browser.removeUserContext", { userContext: ctx.id }, undefined, 15_000).catch(() => {});
  }
  async newPage(ctx = null) {
    const { context } = await this.rpc.send("browsingContext.create", { type: "tab", ...(ctx ? { userContext: ctx.id } : {}) });
    const p = new FirefoxPage(this, ctx, context);
    this.pages.add(p);
    await p.init();
    return p;
  }
  killSync() {
    Chrome.prototype.killSync.call(this);
  }
  async close() {
    if (this.rpc && !this.rpc.closed) {
      await this.rpc.send("browser.close", {}, undefined, 5_000).catch(() => {});
    }
    await endProcess(this);
  }
}

export function launch(kind, exe, opts) {
  return kind === "chrome" ? Chrome.launch(exe, opts) : Firefox.launch(exe, opts);
}
