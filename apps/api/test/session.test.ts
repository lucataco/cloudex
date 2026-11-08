// The checks that keep another site from using the dashboard's cookie.
import assert from "node:assert/strict";
import test from "node:test";
import { notFromDashboard, readSession, sessionCookie, sessionName } from "../src/session.ts";

const API = "https://cloudex-api.example.workers.dev";
const req = (path: string, headers: Record<string, string>, method = "GET") => new Request(API + path, { method, headers });

test("the dashboard's own calls pass", () => {
  assert.equal(notFromDashboard(req("/v1/me", { "x-cloudex": "1", "sec-fetch-site": "same-origin" })), null);
  assert.equal(notFromDashboard(req("/v1/sandboxes", { "x-cloudex": "1", origin: API }, "POST")), null);
  // a WebSocket can't carry the header; its Origin must be there
  assert.equal(notFromDashboard(req("/v1/sandboxes/web/terminal", { upgrade: "websocket", origin: API })), null);
  // a download the page opens as a link
  assert.equal(notFromDashboard(req("/v1/sandboxes/web/files/content?path=a", { "sec-fetch-site": "same-origin" })), null);
});

test("another site's calls don't", () => {
  assert.match(notFromDashboard(req("/v1/me", {}))!, /X-Cloudex/);
  assert.match(notFromDashboard(req("/v1/me", { "x-cloudex": "1", origin: "https://evil.example" }))!, /evil.example/);
  assert.match(notFromDashboard(req("/v1/me", { "x-cloudex": "1", "sec-fetch-site": "cross-site" }))!, /another site/);
  assert.match(notFromDashboard(req("/v1/me", { "x-cloudex": "1", "sec-fetch-site": "same-site" }))!, /another site/);
  assert.ok(notFromDashboard(req("/v1/sandboxes/web/terminal", { upgrade: "websocket" })));
  assert.ok(notFromDashboard(req("/v1/sandboxes/web/terminal", { upgrade: "websocket", origin: "https://evil.example" })));
  assert.ok(notFromDashboard(req("/v1/sandboxes/web/files/content?path=a", {})));
  assert.ok(notFromDashboard(req("/v1/sandboxes/web/files", { "sec-fetch-site": "same-origin" }, "DELETE")));
});

test("signing in needs both the header and this Origin", () => {
  assert.equal(notFromDashboard(req("/v1/auth/session", { "x-cloudex": "1", origin: API }, "POST"), true), null);
  assert.ok(notFromDashboard(req("/v1/auth/session", { "x-cloudex": "1" }, "POST"), true));
  assert.ok(notFromDashboard(req("/v1/auth/session", { origin: API }, "POST"), true));
});

test("the cookie", () => {
  assert.equal(readSession(req("/", { cookie: "a=1; __Host-cx_session=cx_abc_def; b=2" })), "cx_abc_def");
  assert.equal(readSession(req("/", { cookie: "x__Host-cx_session=no" })), null);
  assert.equal(readSession(req("/", {})), null);
  assert.equal(sessionCookie("s", 60), "__Host-cx_session=s; Path=/; Secure; HttpOnly; SameSite=Strict; Max-Age=60");
  assert.match(sessionCookie(null), /__Host-cx_session=; .*Max-Age=0$/);
});

test("session names", () => {
  const mac = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/157.0.0.0 Safari/537.36";
  assert.equal(sessionName(mac), "dashboard (Chrome on macOS)");
  assert.equal(sessionName("Mozilla/5.0 (X11; Linux x86_64; rv:140.0) Gecko/20100101 Firefox/140.0"), "dashboard (Firefox on Linux)");
  assert.equal(sessionName(undefined), "dashboard (a browser)");
});
