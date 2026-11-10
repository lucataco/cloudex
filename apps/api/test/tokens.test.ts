// Device approvals, sessions and sign-in links, against a D1 in memory (d1.ts).
import assert from "node:assert/strict";
import test from "node:test";
import { fakeD1 } from "./d1.ts";

const { authenticateSecret, createLoginLink, createToken, decideDevice, pollDevice, refuseSession, revokeToken, startDevice, startSession, tokenAlive } =
  await import("../src/tokens.ts");
const { CreateLoginLink } = await import("@cloudex/schema");

const DB = fakeD1();
const env = { DB, ADMIN_KEY: "the-admin-key" } as unknown as Env;
const admin = { kind: "admin" } as const;
const DAY = 86_400_000;

async function clear() {
  // Before the first call to tokens.ts, there are no tables yet.
  for (const t of ["tokens", "device_requests", "login_links"]) await DB.prepare(`DELETE FROM ${t}`).run().catch(() => {});
}

// A device request that asked for role, from an address of its own,
// approved by who with grant.
let ips = 0;
async function approved(role: "admin" | "user" | "read" | undefined, grant: { role?: "admin" | "user" | "read"; expiresInSec?: number }, who: Parameters<typeof decideDevice>[1] = admin) {
  const d = await startDevice(DB, { name: "laptop", role, ip: `10.0.0.${++ips}`, country: null, userAgent: null });
  const r = await decideDevice(DB, who, d.userCode, { approve: true, ownOnly: false, ...grant });
  return { d, r };
}

test("a device that asks for admin gets user unless the approver names admin", async () => {
  await clear();
  assert.equal((await approved("admin", {})).r.role, "user");
  assert.equal((await approved("read", {})).r.role, "read");
  assert.equal((await approved("user", {})).r.role, "user");
  assert.equal((await approved(undefined, {})).r.role, "user");
  assert.equal((await approved("read", { role: "admin" })).r.role, "admin");
  const { d } = await approved("admin", {});
  const r = await pollDevice(DB, d.deviceCode);
  assert.ok("token" in r && r.token.role === "user");
});

test("an approver can't grant more than its own role", async () => {
  await clear();
  const user = await createToken(DB, { name: "u", role: "user", ownOnly: false, createdBy: "admin" });
  const who = { kind: "token", token: user } as const;
  await assert.rejects(approved("admin", { role: "admin" }, who), /\[403\]/);
  assert.equal((await approved("admin", {}, who)).r.role, "user");
});

test("a session can't make a sign-in link or another session", async () => {
  await clear();
  const session = await startSession(DB, admin, "dashboard");
  assert.throws(() => refuseSession({ kind: "token", token: session }), /^Error: \[403\] a dashboard session can't/);
  // Pasted, as the login page would send it.
  const pasted = await authenticateSecret(env, session.secret);
  assert.ok(pasted);
  assert.throws(() => refuseSession(pasted), /\[403\]/);
  // Tokens and the admin key still can.
  const token = await createToken(DB, { name: "t", role: "user", ownOnly: false, createdBy: "admin" });
  refuseSession({ kind: "token", token });
  refuseSession(admin);
  assert.ok((await createLoginLink(DB, { kind: "token", token })).code);
});

test("a session from a device approval never outlives the approval", async () => {
  await clear();
  const { d } = await approved(undefined, { expiresInSec: 3600 });
  const r = await pollDevice(DB, d.deviceCode, { name: "dashboard" });
  assert.ok("token" in r);
  const now = Date.now();
  assert.ok(r.token.expiresAt! <= now + 3600_000 + 1000, "capped when made");
  // Used again after a while: renewed, but not past the hour.
  await DB.prepare("UPDATE tokens SET last_used_at = ? WHERE id = ?").bind(now - 120_000, r.token.id).run();
  const who = await authenticateSecret(env, r.token.secret);
  assert.ok(who?.kind === "token" && who.renewed);
  assert.ok(who.token.expiresAt! <= now + 3600_000 + 1000, `renewed to ${who.token.expiresAt! - now} ms`);
  const row = await DB.prepare("SELECT expires_at FROM tokens WHERE id = ?").bind(r.token.id).first<number>("expires_at");
  assert.ok(row! <= now + 3600_000 + 1000);
  // Past it, it's gone.
  await DB.prepare("UPDATE tokens SET expires_at = ?, last_used_at = ? WHERE id = ?").bind(now - 1, now, r.token.id).run();
  assert.equal(await authenticateSecret(env, r.token.secret), null);
});

test("other sessions still slide to 30 days, capped by their token", async () => {
  await clear();
  const s = await startSession(DB, admin, "dashboard");
  const now = Date.now();
  await DB.prepare("UPDATE tokens SET last_used_at = ?, expires_at = ? WHERE id = ?").bind(now - 120_000, now + DAY, s.id).run();
  const who = await authenticateSecret(env, s.secret);
  assert.ok(who?.kind === "token" && who.token.expiresAt! > now + 29 * DAY);
  const parent = await createToken(DB, { name: "p", role: "user", ownOnly: false, expiresInSec: 7200, createdBy: "admin" });
  const child = await startSession(DB, { kind: "token", token: parent }, "dashboard");
  await DB.prepare("UPDATE tokens SET last_used_at = ? WHERE id = ?").bind(now - 120_000, child.id).run();
  const w = await authenticateSecret(env, child.secret);
  assert.ok(w?.kind === "token" && w.token.expiresAt! <= parent.expiresAt!);
});

test("waiting device requests are capped per address", async () => {
  await clear();
  const from = (ip: string | null) => startDevice(DB, { name: "x", ip, country: null, userAgent: null });
  for (let i = 0; i < 3; i++) await from("1.2.3.4");
  await assert.rejects(from("1.2.3.4"), /^Error: \[429\] too many sign-ins from this address/);
  await from("5.6.7.8");
  // No address recorded: one bucket for all of them.
  for (let i = 0; i < 3; i++) await from(null);
  await assert.rejects(from(null), /\[429\]/);
  // Expired ones don't count.
  await DB.prepare("UPDATE device_requests SET expires_at = ? WHERE ip = '1.2.3.4'").bind(Date.now() - 1).run();
  await from("1.2.3.4");
});

test("a denied or approved sign-in its device hasn't collected isn't waiting", async () => {
  await clear();
  const from = () => startDevice(DB, { name: "x", ip: "9.9.9.9", country: null, userAgent: null });
  const a = await from();
  const b = await from();
  await from();
  await decideDevice(DB, admin, a.userCode, { approve: false });
  await decideDevice(DB, admin, b.userCode, { approve: true, ownOnly: false });
  // Two places free again, though neither device has asked for its answer.
  await from();
  await from();
  await assert.rejects(from(), /\[429\]/);
  // The answers are still there for the devices.
  assert.deepEqual(await pollDevice(DB, a.deviceCode), { error: "access_denied" });
  assert.ok("token" in (await pollDevice(DB, b.deviceCode)));
});

test("tokenAlive: what an open WebSocket checks", async () => {
  await clear();
  const t = await createToken(DB, { name: "t", role: "user", ownOnly: false, createdBy: "admin" });
  const s = await startSession(DB, { kind: "token", token: t }, "dashboard");
  assert.equal(await tokenAlive(DB, t.id), true);
  assert.equal(await tokenAlive(DB, s.id), true);
  // The parent going ends the session, even before the session's row goes.
  await DB.prepare("DELETE FROM tokens WHERE id = ?").bind(t.id).run();
  assert.equal(await tokenAlive(DB, s.id), false);
  const e = await createToken(DB, { name: "e", role: "user", ownOnly: false, createdBy: "admin" });
  await DB.prepare("UPDATE tokens SET expires_at = ? WHERE id = ?").bind(Date.now() - 1, e.id).run();
  assert.equal(await tokenAlive(DB, e.id), false);
  const r = await createToken(DB, { name: "r", role: "user", ownOnly: false, createdBy: "admin" });
  await revokeToken(DB, r.id);
  assert.equal(await tokenAlive(DB, r.id), false);
});

test("a sign-in link opens only a dashboard page", () => {
  for (const ok of ["/", "/s/web", "/s/web/terminal", "/s/cx-abc123/files?path=%2Ftmp&root=1", "/saves", "/access"]) {
    assert.ok(CreateLoginLink.safeParse({ path: ok }).success, ok);
  }
  for (const bad of [
    "/v1/sandboxes/web/files/content?path=/etc/passwd",
    "/v1/me",
    "//evil.example",
    "/s/../v1/sandboxes/web/files/content",
    "/s/%2e%2e/v1/me",
    "/login",
    "/assets/app.js",
    "https://evil.example/",
  ]) {
    assert.ok(!CreateLoginLink.safeParse({ path: bad }).success, bad);
  }
});
