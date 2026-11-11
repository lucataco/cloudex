// The node agent's messages against the examples both sides are checked
// with (fixtures/node/protocol.json; the Go side is go/internal/node).
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { test } from "node:test";
import { NODE_PROTOCOL, NodeClose, NodeJoin, NodeJoined, NodeMessage, NodeName, NodeResult, WorkerMessage } from "../src/node.ts";

type Example = { name: string; model: keyof typeof MODELS; message: unknown };
const fixture = JSON.parse(readFileSync(new URL("../fixtures/node/protocol.json", import.meta.url), "utf8")) as {
  protocol: number;
  close: Record<string, number>;
  valid: Example[];
  invalid: Example[];
};
const MODELS = { NodeJoin, NodeJoined, NodeMessage, WorkerMessage };

test("the protocol version and close codes are the ones in the fixture", () => {
  assert.equal(fixture.protocol, NODE_PROTOCOL);
  assert.deepEqual(fixture.close, NodeClose);
});

test("every valid example parses, unchanged", () => {
  for (const e of fixture.valid) {
    const r = MODELS[e.model].safeParse(e.message);
    assert.ok(r.success, `${e.name}: ${r.error?.issues.map((i) => `${i.path.join(".")}: ${i.message}`).join("; ")}`);
    assert.deepEqual(r.data, e.message, e.name);
  }
});

test("every invalid example is refused", () => {
  for (const e of fixture.invalid) assert.equal(MODELS[e.model].safeParse(e.message).success, false, e.name);
});

test("fields the models don't know are dropped, never kept", () => {
  const hello = fixture.valid.find((e) => e.name === "hello")!.message as Record<string, unknown>;
  const parsed = NodeMessage.parse({ ...hello, extra: "x".repeat(10), facts: { ...(hello.facts as object), owner: "<b>" } });
  assert.equal("extra" in parsed, false);
  assert.equal("owner" in (parsed as { facts: object }).facts, false);
});

test("an op's error is bounded", () => {
  assert.equal(NodeResult.safeParse({ type: "result", id: "op-a", ok: false, error: "e".repeat(2001) }).success, false);
});

test("node names follow sandbox names, and can't look like node ids", () => {
  assert.ok(NodeName.safeParse("box").success);
  assert.equal(NodeName.safeParse("nd-box").success, false);
  assert.equal(NodeName.safeParse("Box").success, false);
});
