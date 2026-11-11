// Which control socket is a node's, whether it's online, and its location hint (nodestate.ts).
import assert from "node:assert/strict";
import test from "node:test";
import { type ControlSocket, ONLINE_WITHIN_MS, locationHint, newest, pick } from "../src/nodestate.ts";

const NOW = 10_000_000;
const sock = (name: string, gen: number, o: Partial<ControlSocket<string>> = {}): ControlSocket<string> => ({
  ws: name,
  gen,
  open: true,
  acceptedAt: NOW - 120_000,
  lastMessageAt: null,
  lastPingAt: NOW - 5_000,
  hello: true,
  ...o,
});

test("the node's socket is the newest open one, wherever it is in the list", () => {
  // The spike's bug: getWebSockets()[0] was a dead socket left by an edge reset.
  const old = sock("old", 1);
  const dead = sock("dead", 3, { open: false });
  const cur = sock("cur", 2);
  assert.equal(newest([old, dead, cur])?.ws, "cur");
  const r = pick([old, dead, cur], NOW);
  assert.equal(r.use?.ws, "cur");
  assert.equal(r.online, true);
  assert.deepEqual(
    r.close.map((s) => s.ws),
    ["old"],
  );
});

test("an open socket that has gone quiet is offline, and is closed", () => {
  // The other half of the spike's bug: online with a pong four minutes old.
  const quiet = sock("quiet", 4, { lastPingAt: NOW - 4 * 60_000 });
  const r = pick([quiet], NOW);
  assert.equal(r.online, false);
  assert.equal(r.use, null);
  assert.deepEqual(
    r.close.map((s) => s.ws),
    ["quiet"],
  );
  // An older socket that still pings doesn't make it online: it was replaced.
  assert.equal(pick([sock("older", 1), quiet], NOW).online, false);
});

test("a socket counts as alive from its newest sign: accept, message or ping", () => {
  const edge = NOW - ONLINE_WITHIN_MS + 1;
  assert.equal(pick([sock("a", 1, { acceptedAt: edge, lastPingAt: null })], NOW).online, true, "just accepted, no ping yet");
  assert.equal(pick([sock("a", 1, { lastPingAt: NOW - 120_000, lastMessageAt: edge })], NOW).online, true);
  assert.equal(pick([sock("a", 1, { lastPingAt: NOW - ONLINE_WITHIN_MS })], NOW).online, false);
  assert.equal(pick([], NOW).online, false);
  assert.equal(pick([sock("a", 1, { open: false })], NOW).online, false);
});

test("a socket that hasn't said hello isn't online yet, and isn't closed for it", () => {
  // Listing a node the moment it connects would show no protocol or facts.
  const fresh = sock("new", 2, { acceptedAt: NOW - 100, lastPingAt: null, hello: false });
  const r = pick([sock("old", 1), fresh], NOW);
  assert.equal(r.online, false);
  assert.equal(r.use, null);
  assert.deepEqual(
    r.close.map((s) => s.ws),
    ["old"],
  );
  // One that never says hello goes once it's quiet, like any other.
  assert.deepEqual(
    pick([sock("mute", 1, { acceptedAt: NOW - 2 * ONLINE_WITHIN_MS, lastPingAt: null, hello: false })], NOW).close.map((s) => s.ws),
    ["mute"],
  );
});

test("the location hint splits continents the way Durable Object regions do", () => {
  const cases: [Record<string, unknown> | undefined, string | null][] = [
    [{ continent: "NA", longitude: "-122.4" }, "wnam"], // SJC
    [{ continent: "NA", longitude: "-96.8" }, "wnam"], // DFW
    [{ continent: "NA", longitude: "-87.6" }, "enam"], // ORD
    [{ continent: "NA", longitude: "-74.0" }, "enam"], // EWR
    [{ continent: "NA" }, "enam"],
    [{ continent: "EU", longitude: "-0.4" }, "weur"], // LHR
    [{ continent: "EU", longitude: "21.0" }, "eeur"], // WAW
    [{ continent: "AS", longitude: "139.7" }, "apac"], // NRT
    [{ continent: "AS", longitude: "55.3" }, null], // DXB
    [{ continent: "OC", longitude: "151.2" }, "oc"],
    [{ continent: "SA", longitude: "-46.6" }, null],
    [{ continent: "AF", longitude: "18.4" }, null],
    [{ continent: "T1" }, null],
    [undefined, null],
  ];
  for (const [cf, want] of cases) assert.equal(locationHint(cf), want, JSON.stringify(cf));
});
