// Every API route gets a conscious rule for access tokens: the OpenAPI
// document lists the routes, and each one is either in RULES (src/rules.ts)
// or meant for admins only, as listed here. A new route that is in neither
// fails this test until someone decides what tokens may do with it.
import assert from "node:assert/strict";
import test from "node:test";
import { openapi } from "@cloudex/schema/openapi";
import { RULES, ruleFor } from "../src/rules.ts";

const PUBLIC = new Set(["health", "openapi", "startDeviceLogin", "pollDeviceLogin", "startSession", "endSession"]);
const ADMIN_ONLY = new Set([
  "listKeys",
  "setKey",
  "deleteKey",
  "createToken",
  "listTokens",
  "revokeToken",
  "listDeviceLogins",
  "approveDeviceLogin",
  "denyDeviceLogin",
]);

// /v1/sandboxes/{ref}/tcp/{port} as a request would have it.
const concrete = (path: string) => path.replace(/\{[^}]+\}/g, (p) => (p === "{port}" ? "22" : "x1"));

type Doc = { paths: Record<string, Record<string, { operationId?: string }>> };
const ops = Object.entries((openapi("test") as Doc).paths).flatMap(([path, item]) =>
  Object.entries(item)
    .filter(([m]) => ["get", "post", "put", "patch", "delete"].includes(m))
    .map(([method, op]) => ({ method: method.toUpperCase(), path, id: op.operationId! })),
);

test("each route is public, for admins only, or has a rule for tokens", () => {
  for (const o of ops) {
    if (PUBLIC.has(o.id)) continue;
    const rule = ruleFor(o.method, concrete(o.path));
    if (ADMIN_ONLY.has(o.id)) assert.equal(rule.need, "admin", `${o.id} should be for admins only`);
    else assert.notEqual(rule.need, "admin", `${o.id} (${o.method} ${o.path}) has no rule in src/rules.ts`);
  }
});

test("every rule is for a route that exists", () => {
  for (const [method, path] of RULES) {
    const openapiPath = path.replace(/:([a-z]+)/g, (_, n) => `{${n === "id" ? "*" : n}}`);
    const found = ops.some(
      (o) => o.method === method && o.path.replace(/\{[^}]+\}/g, "{*}") === openapiPath.replace(/\{[^}]+\}/g, "{*}"),
    );
    assert.ok(found, `${method} ${path} matches no route`);
  }
});

test("reading routes never need more than read, and :ref is captured", () => {
  assert.deepEqual(ruleFor("GET", "/v1/sandboxes/web/agents/ag-1/events"), { need: "read", notOwnOnly: false, ref: "web" });
  assert.equal(ruleFor("POST", "/v1/sandboxes/web/exec").need, "write");
  assert.equal(ruleFor("GET", "/v1/sandboxes/web/hosts").need, "write");
  assert.equal(ruleFor("POST", "/v1/sandboxes/web/saves").notOwnOnly, true);
  assert.equal(ruleFor("GET", "/v1/tokens").need, "admin");
  assert.equal(ruleFor("GET", "/v1/sandboxes/web/nothing-here").need, "admin");
  assert.equal(ruleFor("HEAD", "/v1/saves").need, "read");
  assert.deepEqual(ruleFor("GET", "/v1/sandboxes/web/terminal"), { need: "write", notOwnOnly: false, ref: "web" });
  assert.equal(ruleFor("GET", "/v1/sandboxes/web/desktop/vnc").need, "write");
  assert.equal(ruleFor("POST", "/v1/auth/links").need, "any");
});
