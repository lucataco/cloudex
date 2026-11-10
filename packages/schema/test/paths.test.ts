// Where the login page may send a browser after signing in (paths.ts).
import assert from "node:assert/strict";
import { test } from "node:test";
import { isDashboardPath, safeNext } from "../src/paths.ts";

test("the dashboard's own pages pass", () => {
  for (const p of ["/", "/saves", "/access", "/s/web", "/s/cx-abc123/", "/s/web/agents/ag-12345678", "/s/web/files?path=~%2Fsrc&root=1", "/s/my%20box/terminal"]) {
    assert.ok(isDashboardPath(p), p);
    assert.equal(safeNext(p), p);
  }
});

test("anything else becomes /", () => {
  for (const p of [
    "/v1/sandboxes/web/files/content?path=/etc/passwd",
    "/v1/me",
    "/v1/auth/session",
    "/assets/app.js",
    "/login?code=BCDF-GHJK",
    "/s",
    "/s/",
    "/s//x",
    "/s/../v1/me",
    "/s/./x",
    "/s/%2E%2e/v1/me",
    "/s/web/../../v1/me",
    "/saves/../v1/me",
    "//evil.example",
    "/\\evil.example",
    "https://evil.example",
    "javascript:alert(1)",
    "/s/web#x",
    "/s/web?a=<b>",
    "",
  ]) {
    assert.ok(!isDashboardPath(p), p);
    assert.equal(safeNext(p), "/", p);
  }
  assert.equal(safeNext(null), "/");
  assert.equal(safeNext(undefined), "/");
});
