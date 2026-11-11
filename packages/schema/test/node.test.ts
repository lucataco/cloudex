// The node agent's messages against the examples both sides are checked
// with (fixtures/node/protocol.json; the Go side is go/internal/node).
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { test } from "node:test";
import {
  EGRESS_ACTIVITY_SEC,
  EGRESS_BODY_MAX,
  EGRESS_ROUTE,
  NODE_PROTOCOL,
  NodeClose,
  NodeHello,
  NodeIsolation,
  NodeJoin,
  NodeJoined,
  NodeMessage,
  NodeName,
  NodeResult,
  STREAM_WINDOW,
  StreamClose,
  StreamFromAgent,
  StreamToAgent,
  WorkerMessage,
} from "../src/node.ts";

type Example = { name: string; model: keyof typeof MODELS; message: unknown };
const fixture = JSON.parse(readFileSync(new URL("../fixtures/node/protocol.json", import.meta.url), "utf8")) as {
  protocol: number;
  close: Record<string, number>;
  streamClose: Record<string, number>;
  window: number;
  egress: { route: string; bodyMax: number; activityEverySec: number; savePartMax: number; targets: { valid: string[]; invalid: string[] } };
  valid: Example[];
  invalid: Example[];
};
const MODELS = { NodeJoin, NodeJoined, NodeMessage, WorkerMessage, StreamToAgent, StreamFromAgent };

test("the protocol version and close codes are the ones in the fixture", () => {
  assert.equal(fixture.protocol, NODE_PROTOCOL);
  assert.deepEqual(fixture.close, NodeClose);
  assert.deepEqual(fixture.streamClose, StreamClose);
  assert.equal(fixture.window, STREAM_WINDOW);
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

test("the egress route and its limits are the ones in the fixture (M6)", () => {
  assert.equal(fixture.egress.route, EGRESS_ROUTE);
  assert.equal(fixture.egress.bodyMax, EGRESS_BODY_MAX);
  assert.equal(fixture.egress.activityEverySec, EGRESS_ACTIVITY_SEC);
  // Save and backup parts (backup.ts PART) fit under the edge's limit.
  assert.ok(fixture.egress.savePartMax <= EGRESS_BODY_MAX);
});

// 2026-11-12 review, W3: what a node writes for people to read loses its control characters.
test("a node's text loses its control characters, but keeps newlines and tabs", () => {
  const evil = "fail\u001b[2J\u001b]0;pwned\u0007 ‮exe.txt\nnext\tline";
  const r = NodeResult.parse({ type: "result", id: "op-abc", ok: false, error: evil });
  assert.equal(r.error, "fail?[2J?]0;pwned? ?exe.txt\nnext\tline");
  const iso = NodeIsolation.parse({ firewall: "fail", unisolated: false, failed: [evil] });
  assert.ok(!/[\u001b\u0007‮]/.test(iso.failed![0]!));
  const out = StreamFromAgent.parse({ error: "\u001b[31mred" });
  assert.deepEqual(out, { error: "?[31mred" });
  const h = NodeHello.parse({ type: "hello", protocol: 1, version: "v1", facts: { arch: "amd64", runtime: "docker", isolation: { firewall: "pass", unisolated: false }, cpus: 1, memoryMb: 1, diskFreeGb: 1, image: { ref: "x\u001b", state: "present" } }, machines: [], machinesError: "\u0000" });
  assert.equal(h.machinesError, "?");
  assert.equal(h.facts.image?.ref, "x?");
});
