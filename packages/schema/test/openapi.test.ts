import assert from "node:assert/strict";
import { test } from "node:test";
import { openapi } from "../src/openapi.ts";

test("every $ref in the OpenAPI document resolves", () => {
  const doc = openapi("test");
  const refs: string[] = [];
  const walk = (v: unknown): void => {
    if (Array.isArray(v)) v.forEach(walk);
    else if (v && typeof v === "object") {
      for (const [k, x] of Object.entries(v)) k === "$ref" ? refs.push(x as string) : walk(x);
    }
  };
  walk(doc);
  assert.ok(refs.length > 10);
  for (const r of refs) {
    const name = r.replace("#/components/schemas/", "");
    assert.ok(name in doc.components.schemas, `${r} doesn't resolve`);
  }
});

test("every operation has an id and a success response", () => {
  const ids = new Set<string>();
  for (const [path, item] of Object.entries(openapi("test").paths)) {
    for (const [method, op] of Object.entries(item)) {
      if (method === "parameters") continue;
      const o = op as { operationId: string; responses: Record<string, unknown> };
      assert.ok(o.operationId && !ids.has(o.operationId), `${method} ${path}: operationId`);
      ids.add(o.operationId);
      assert.ok(Object.keys(o.responses).some((c) => /^[12]\d\d$/.test(c)), `${method} ${path}: no success response`);
    }
  }
  assert.equal(ids.size, 46);
});

test("responses take fields added later", () => {
  const closed: string[] = [];
  const walk = (v: unknown, at: string): void => {
    if (Array.isArray(v)) v.forEach((x, i) => walk(x, `${at}/${i}`));
    else if (v && typeof v === "object") {
      for (const [k, x] of Object.entries(v)) {
        if (k === "additionalProperties" && x === false) closed.push(at);
        walk(x, `${at}/${k}`);
      }
    }
  };
  const { schemas } = openapi("test").components;
  for (const name of ["Sandbox", "Health", "ExecEvent", "AgentEvents", "Error"]) walk(schemas[name], name);
  assert.deepEqual(closed, []);
});

test("request bodies leave defaulted fields optional", () => {
  const create = openapi("test").components.schemas.CreateSandbox as { required?: string[] };
  assert.deepEqual(create.required ?? [], []);
  const exec = openapi("test").components.schemas.ExecRequest as { required?: string[] };
  assert.deepEqual(exec.required, ["cmd"]);
  const start = openapi("test").components.schemas.StartAgent as { required?: string[] };
  assert.deepEqual(start.required, ["prompt"]);
});
