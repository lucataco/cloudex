// Placing sandboxes on nodes, and reconciling them with what their agent
// reports (placement.ts; v0.2.2 M5).
import assert from "node:assert/strict";
import test from "node:test";
import type { Node } from "@cloudex/schema";
import { machineAction, nodeIsolation, offlineMessage, placeOn, reconcilePlan, refused, seenText } from "../src/placement.ts";

const node = (o: Partial<Node> = {}): Node => ({
  id: "nd-abcd1234",
  name: "box",
  state: "online",
  createdAt: 0,
  lastSeenAt: Date.UTC(2026, 10, 11, 14, 2),
  version: "v0.2.2",
  protocol: 1,
  arch: "amd64",
  runtime: "sysbox",
  isolation: { firewall: "pass", unisolated: false },
  cpus: 2,
  memoryMb: 7936,
  diskFreeGb: 80,
  colo: "IAD",
  continent: "NA",
  locationHint: "enam",
  machines: 0,
  revokedAt: null,
  ...o,
});

const user = { ownOnly: false };
const ci = { ownOnly: true };
const std2 = { size: "standard-2" as const };

const refusal = (p: ReturnType<typeof placeOn>) => (refused(p) ? p : assert.fail(`placed: ${JSON.stringify(p)}`));
const placed = (p: ReturnType<typeof placeOn>) => (refused(p) ? assert.fail(`refused: ${p.error}`) : p);

test("an online node with a passing self-test takes a sandbox, with the size's limits capped at the host", () => {
  assert.deepEqual(placed(placeOn(node(), user, std2)), { cpus: 1, memoryMb: 3968, isolation: "sysbox" });
  // standard-4 is 4 CPUs and 12 GiB: the host has 2 and 7936 MiB (half for memory).
  assert.deepEqual(placed(placeOn(node(), user, { size: "standard-4" })), { cpus: 2, memoryMb: 3968, isolation: "sysbox" });
  // On a big host, the size's own.
  assert.deepEqual(placed(placeOn(node({ cpus: 32, memoryMb: 131072 }), user, std2)), { cpus: 1, memoryMb: 6144, isolation: "sysbox" });
  // Asked for, up to the host's.
  assert.deepEqual(placed(placeOn(node(), user, { ...std2, cpus: 2, memoryMb: 2048 })), { cpus: 2, memoryMb: 2048, isolation: "sysbox" });
});

test("cpus and memory beyond the node's own are refused with a 400", () => {
  const c = refusal(placeOn(node(), user, { ...std2, cpus: 4 }));
  assert.equal(c.status, 400);
  assert.match(c.error, /node box has 2 CPUs/);
  const m = refusal(placeOn(node(), user, { ...std2, memoryMb: 16384 }));
  assert.equal(m.status, 400);
  assert.match(m.error, /7936 MiB/);
});

test("roles: any token that may make sandboxes may place them, except own-only ones on plain Docker or none", () => {
  // Decision 4: CI tokens run code from pull requests.
  assert.equal(placed(placeOn(node(), ci, std2)).isolation, "sysbox");
  const docker = refusal(placeOn(node({ runtime: "docker" }), ci, std2));
  assert.equal(docker.status, 403);
  assert.match(docker.error, /own-only token can't place sandboxes on node box: it runs them under plain Docker/);
  const none = refusal(placeOn(node({ runtime: "docker", isolation: { firewall: "fail", unisolated: true } }), ci, std2));
  assert.equal(none.status, 403);
  assert.match(none.error, /no isolation/);
  // Under Sysbox but joined --unisolated: still none.
  assert.equal(refusal(placeOn(node({ isolation: { firewall: "unchecked", unisolated: true } }), ci, std2)).status, 403);
  // A token that isn't own-only may use a plain-Docker node, and an unisolated one.
  assert.equal(placed(placeOn(node({ runtime: "docker" }), user, std2)).isolation, "docker");
  assert.equal(placed(placeOn(node({ isolation: { firewall: "fail", unisolated: true } }), user, std2)).isolation, "none");
});

test("an offline, pending or removed node is refused, naming it", () => {
  const off = refusal(placeOn(node({ state: "offline" }), user, std2));
  assert.equal(off.status, 503);
  assert.equal(off.error, "node box is offline (last seen 2026-11-11 14:02 UTC)");
  assert.equal(refusal(placeOn(node({ state: "pending" }), user, std2)).status, 409);
  const gone = refusal(placeOn(node({ state: "revoked" }), user, std2));
  assert.equal(gone.status, 404);
  assert.match(gone.error, /was removed/);
});

test("a node whose self-test fails takes no sandbox, unless it joined --unisolated; one that hasn't run is left to the agent", () => {
  const fail = refusal(placeOn(node({ isolation: { firewall: "fail", unisolated: false, failed: ["metadata 169.254.169.254:80 answered"] } }), user, std2));
  assert.equal(fail.status, 503);
  assert.match(fail.error, /self-test fails \(metadata 169\.254\.169\.254:80 answered\), so it runs no sandboxes/);
  // The agent waits up to a minute for its first verdict, and refuses without a pass.
  assert.equal(placed(placeOn(node({ isolation: { firewall: "unchecked", unisolated: false } }), user, std2)).isolation, "sysbox");
  assert.equal(refusal(placeOn(node({ isolation: null, runtime: null }), user, std2)).status, 503);
});

test("a node still pulling the image says so", () => {
  const p = refusal(placeOn(node({ image: { ref: "ghcr.io/lucataco/cloudex-base:v0.2.2", state: "pulling", progress: 0.42 } }), user, std2));
  assert.equal(p.status, 503);
  assert.match(p.error, /still pulling the sandbox image \(42%\)/);
  assert.equal(placed(placeOn(node({ image: { ref: "x", state: "present" } }), user, std2)).cpus, 1);
});

test("isolation labels", () => {
  assert.equal(nodeIsolation(node()), "sysbox");
  assert.equal(nodeIsolation(node({ runtime: "docker" })), "docker");
  assert.equal(nodeIsolation(node({ isolation: { firewall: "pass", unisolated: true } })), "none");
  assert.equal(nodeIsolation(node({ isolation: null })), null);
});

test("offline messages name the node and when it was last seen", () => {
  assert.equal(seenText(null), "never seen");
  assert.equal(offlineMessage("box", Date.UTC(2026, 0, 2, 3, 4, 59)), "node box is offline (last seen 2026-01-02 03:04 UTC)");
});

test("a report changes a sandbox only as its own state says it should", () => {
  // The sandbox decides what runs; the agent only reports.
  assert.equal(machineAction("running", "running"), "none");
  assert.equal(machineAction("running", "stopped"), "mark-stopped"); // a reboot, an OOM, a docker stop
  assert.equal(machineAction("running", "missing"), "mark-missing");
  assert.equal(machineAction("stopped", "running"), "stop-machine"); // started by hand, or a cut-off stop
  assert.equal(machineAction("error", "running"), "stop-machine"); // a start cut off by a deploy
  assert.equal(machineAction("stopped", "stopped"), "none");
  assert.equal(machineAction("stopped", "missing"), "note-missing");
  assert.equal(machineAction("error", "missing"), "note-missing");
  // A start or stop under way decides for itself.
  for (const m of ["running", "stopped", "missing"] as const) {
    assert.equal(machineAction("starting", m), "none");
    assert.equal(machineAction("stopping", m), "none");
  }
});

test("reconcile on hello: tombstones are removed, each sandbox hears of its machine, orphans are left alone", () => {
  const plan = reconcilePlan({
    machines: [
      { id: "cx-aaaaaa", state: "running" },
      { id: "cx-bbbbbb", state: "stopped" },
      { id: "cx-dddddd", state: "running" }, // deleted while offline
      { id: "cx-oooooo", state: "stopped" }, // of no sandbox
    ],
    machinesKnown: true,
    sandboxes: ["cx-aaaaaa", "cx-bbbbbb", "cx-cccccc"],
    tombstones: ["cx-dddddd", "cx-eeeeee"],
  });
  assert.deepEqual(plan.deletes, ["cx-dddddd", "cx-eeeeee"]); // a tombstone with no machine goes too (delete is idempotent)
  assert.deepEqual(plan.reports, [
    { id: "cx-aaaaaa", machine: "running" },
    { id: "cx-bbbbbb", machine: "stopped" },
    { id: "cx-cccccc", machine: "missing" },
  ]);
  assert.deepEqual(plan.orphans, ["cx-oooooo"]);
});

test("a hello whose machine list failed takes no sandbox for gone", () => {
  const plan = reconcilePlan({ machines: [], machinesKnown: false, sandboxes: ["cx-aaaaaa"], tombstones: [] });
  assert.deepEqual(plan.reports, []);
  assert.deepEqual(plan.orphans, []);
});

test("a sandbox deleted with --force while the index still lists it isn't reported, only removed", () => {
  // The race of cloudex delete --force with the node's reconnect: the
  // tombstone is written before the index row goes.
  const plan = reconcilePlan({ machines: [{ id: "cx-aaaaaa", state: "running" }], machinesKnown: true, sandboxes: ["cx-aaaaaa"], tombstones: ["cx-aaaaaa"] });
  assert.deepEqual(plan.deletes, ["cx-aaaaaa"]);
  assert.deepEqual(plan.reports, []);
  assert.deepEqual(plan.orphans, []);
});

// 2026-11-12 review, W4: a running sandbox on a node that stays offline.
test("a sandbox whose node stays offline asks its node less and less often, at most every 6 hours", async () => {
  const { offlineAlarm, nodeRetryAt, NODE_RETRY_MAX_MS } = await import("../src/placement.ts");
  const t0 = 1_000_000_000;
  let down: { since: number; tries?: number; retryAt?: number } | null = { since: t0 };
  let now = t0;
  let asks = 0;
  const end = t0 + 48 * 3600_000;
  while (now < end) {
    const plan = offlineAlarm(down, now, 60_000);
    if (plan.ask) {
      asks++; // and the node is still offline: nodeOp's 503
      const tries: number = (down!.tries ?? 0) + 1;
      down = { since: t0, tries, retryAt: nodeRetryAt(now, tries) };
    }
    now = plan.next;
  }
  assert.ok(asks <= 12, `asked ${asks} times in 48 hours`);
  assert.ok(asks >= 6, `but it does keep trying (${asks})`);
  assert.equal(nodeRetryAt(0, 30), NODE_RETRY_MAX_MS);
  assert.equal(offlineAlarm(null, 5, 60_000).ask, true);
});

// 2026-11-12 review, W7: a save's provenance survives a trip through Cloudflare.
test("a save names the node it was made on, or the node its sandbox's disk came from", async () => {
  await import("./d1.ts");
  const { saveProvenance } = await import("../src/saves.ts");
  const a = { id: "nd-aaaa1111", name: "box-a" };
  const b = { id: "nd-bbbb2222", name: "box-b" };
  assert.deepEqual(saveProvenance(null, undefined), {});
  assert.deepEqual(saveProvenance(b, undefined), { node: b });
  assert.deepEqual(saveProvenance(null, a), { node: a }, "on Cloudflare, started from a's save");
  assert.deepEqual(saveProvenance(b, a), { node: b }, "made on b: b put it there last");
});
