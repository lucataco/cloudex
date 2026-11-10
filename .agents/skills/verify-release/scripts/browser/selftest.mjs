// Checks the browser checks' pure parts, with no browser and no deployment:
//   node selftest.mjs
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { wrap } from "./driver.mjs";
import { ORDER } from "./scenarios.mjs";
import {
  SCENARIOS,
  browserCandidates,
  findBrowser,
  leakyUrls,
  makeRedactor,
  normalizeRows,
  parseArgs,
  parseBidiListening,
  parseDevToolsActivePort,
  poll,
  reporter,
} from "./util.mjs";

const here = path.dirname(new URL(import.meta.url).pathname);
let n = 0;
const test = async (name, fn) => {
  try {
    await fn();
    n++;
  } catch (e) {
    console.error(`selftest: ${name}: ${e.message}`);
    process.exit(1);
  }
};

await test("arguments", () => {
  assert.deepEqual(parseArgs([]), { browser: "chrome", only: null, keep: false, out: null, results: null, find: null, help: false });
  const o = parseArgs(["--browser", "firefox", "--only=terminal,code", "--keep", "--out", "/x"]);
  assert.equal(o.browser, "firefox");
  assert.deepEqual(o.only, ["terminal", "code"]);
  assert.equal(o.keep, true);
  assert.equal(o.out, "/x");
  assert.throws(() => parseArgs(["--browser", "safari"]), /no browser safari/);
  assert.throws(() => parseArgs(["--only", "nosuch"]), /no scenario nosuch/);
  assert.throws(() => parseArgs(["--out"]), /needs a value/);
  assert.throws(() => parseArgs(["--wat"]), /unknown argument/);
});

await test("finding a browser", () => {
  assert.deepEqual(browserCandidates("chrome", { env: { CHROME: "/opt/c" } }), ["/opt/c"]);
  assert.ok(browserCandidates("chrome", { env: {}, platform: "darwin", home: "/h" }).includes("/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"));
  assert.ok(browserCandidates("firefox", { env: {}, platform: "darwin", home: "/h" }).includes("/h/Applications/Firefox.app/Contents/MacOS/firefox"));
  assert.ok(browserCandidates("firefox", { env: {}, platform: "linux" }).includes("firefox"));
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "vr-selftest-"));
  try {
    const exe = path.join(dir, "chromium");
    fs.writeFileSync(exe, "#!/bin/sh\n");
    fs.chmodSync(exe, 0o755);
    fs.writeFileSync(path.join(dir, "google-chrome"), "not runnable");
    assert.equal(findBrowser("chrome", { env: { PATH: dir }, platform: "linux" }), exe);
    assert.equal(findBrowser("chrome", { env: { CHROME: exe, PATH: "" } }), exe);
    assert.equal(findBrowser("firefox", { env: { PATH: dir }, platform: "linux" }), null);
    assert.equal(findBrowser("chrome", { env: { CHROME: path.join(dir, "missing"), PATH: dir } }), null);
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
  }
});

await test("where the browsers listen", () => {
  assert.deepEqual(parseDevToolsActivePort("41234\n/devtools/browser/ab-cd\n"), { port: 41234, path: "/devtools/browser/ab-cd" });
  assert.equal(parseDevToolsActivePort("41234\n"), null);
  assert.equal(parseDevToolsActivePort(""), null);
  assert.equal(parseBidiListening("*** You are running in headless mode.\nWebDriver BiDi listening on ws://127.0.0.1:9444\n"), "ws://127.0.0.1:9444");
  assert.equal(parseBidiListening("nothing yet"), null);
});

await test("secrets never print", () => {
  const r = makeRedactor();
  r.add("abcdefgh2345");
  assert.equal(r("https://api.example/login#link=zz77aa&next=%2Fs%2Fx"), "https://api.example/login#link=<secret>&next=%2Fs%2Fx");
  assert.equal(r("the code abcdefgh2345 went"), "the code <secret> went");
  assert.equal(r("token cx_AbC123_xyz-9 here"), "token cx_<secret> here");
  assert.equal(r("cookie: __Host-cx_session=s3cr3t; Path=/"), "cookie: __Host-cx_session=<secret>; Path=/");
  assert.equal(r("/login?code=ABCD-EFGH"), "/login?code=ABCD-EFGH");
  r.add("abc"); // too short to hide everywhere
  assert.equal(r("abc"), "abc");
});

await test("verify.sh's line format", () => {
  let s = "";
  const file = path.join(os.tmpdir(), `vr-selftest-${process.pid}.tsv`);
  fs.writeFileSync(file, "");
  try {
    const R = reporter({ write: (x) => (s += x), results: file, color: false, redact: makeRedactor() });
    R.pass("B01", "it works");
    R.fail("B02", "it breaks", "because\nof cx_secretsecret");
    R.skip("B16", "it costs", "no key");
    assert.equal(s, "PASS B01   it works\nFAIL B02   it breaks: because of cx_<secret>\nSKIP B16   it costs: no key\n");
    assert.equal(fs.readFileSync(file, "utf8"), "B01\tPASS\tit works\t\nB02\tFAIL\tit breaks\tbecause of cx_<secret>\nB16\tSKIP\tit costs\tno key\n");
    assert.deepEqual(R.counts, { PASS: 1, FAIL: 1, SKIP: 1 });
    let c = "";
    reporter({ write: (x) => (c += x) }).fail("B03", "x", "y");
    assert.equal(c, "\x1b[31mFAIL\x1b[0m B03   x: y\n");
  } finally {
    fs.rmSync(file, { force: true });
  }
});

await test("waiting says what for", async () => {
  let i = 0;
  assert.equal(await poll(() => ++i >= 3 && "yes", { ms: 2_000, every: 5 }), "yes");
  await assert.rejects(poll(() => false, { ms: 50, every: 10, what: "the moon" }), /timed out after 0 s waiting for the moon$/);
  await assert.rejects(
    poll(() => {
      throw new Error("not yet");
    }, { ms: 50, every: 10, what: "x" }),
    /waiting for x; last error: not yet/,
  );
  await assert.rejects(poll(() => Promise.reject(Object.assign(new Error("gone"), { fatal: true })), { ms: 5_000 }), /^Error: gone$/);
});

await test("leaky URLs", () => {
  const ok = ["https://a.example/login", "https://a.example/login#link=abcdef", "wss://a.example/v1/sandboxes/cx-1/terminal?session=dash-1&cols=80", "https://a.example/login?code=ABCD-EFGH&next=%2F"];
  assert.deepEqual(leakyUrls(ok, ["s3cr3tvalue"]), []);
  const bad = ["https://a.example/x?token=1", "https://a.example/v1/me?key=1", "https://a.example/cx_abcdefghijk", "https://a.example/p?q=s3cr3tvalue", "https://a.example/login?link=zz"];
  assert.deepEqual(leakyUrls(bad, ["s3cr3tvalue"]), bad);
});

await test("terminal rows", () => {
  assert.equal(normalizeRows(["a$  ", "U=héllo-世界   ", ""]), "a$\nU=héllo-世界\n");
});

await test("page expressions", async () => {
  const v = await eval(wrap((a, b) => ({ sum: a + b, s: `${a}` }), [2, 3]));
  assert.deepEqual(JSON.parse(v), { sum: 5, s: "2" });
  assert.equal(await eval(wrap(() => undefined, [])), "null");
  assert.equal(JSON.parse(await eval(wrap((s) => s, ["quote ' \" ` ${x} \\ done"]))), "quote ' \" ` ${x} \\ done");
  assert.equal(JSON.parse(await eval(wrap(async () => 7, []))), 7);
});

await test("scenarios and ids", () => {
  assert.deepEqual(Object.keys(ORDER), SCENARIOS);
  const src = fs.readFileSync(path.join(here, "scenarios.mjs"), "utf8");
  const ids = [...new Set([...src.matchAll(/"(B\d\d)"/g)].map((m) => m[1]))].sort();
  const want = Array.from({ length: 39 }, (_, i) => `B${String(i + 1).padStart(2, "0")}`);
  assert.deepEqual(ids, want);
  const run = fs.readFileSync(path.join(here, "run.mjs"), "utf8");
  assert.ok(run.includes('"B40"') && run.includes('"B00"'));
});

console.log(`selftest: ${n} groups passed`);
