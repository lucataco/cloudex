import assert from "node:assert/strict";
import { test } from "node:test";
import { CreateSandbox, DesktopRequest, Name, UpdateSettings } from "../src/index.ts";

const issue = (r: { success: boolean; error?: { issues: { message: string }[] } }) => r.error?.issues[0]?.message;

test("a rejected name gets the whole rule", () => {
  const rule = "2-32 lowercase letters, digits and dashes, starting with a letter and not ending with a dash";
  for (const bad of ["1abc", "web-", "Web", "a"]) assert.equal(issue(Name.safeParse(bad)), rule, bad);
  assert.ok(Name.safeParse("web-2").success);
});

test("a repo URL with a user name or token in it is refused, pointing at GH_TOKEN", () => {
  for (const repo of [
    "https://x-access-token:ghp_abc@github.com/a/b",
    "https://me@github.com/a/b#main",
    { url: "https://me:pw@github.com/a/b" },
  ]) {
    const message = issue(CreateSandbox.safeParse({ repo }));
    assert.ok(message?.includes("GH_TOKEN"), `${JSON.stringify(repo)}: ${message}`);
  }
  for (const repo of ["https://github.com/a/b#main", "https://registry.example/@scope/pkg", { url: "https://github.com/a/b" }]) {
    assert.ok(CreateSandbox.safeParse({ repo }).success, JSON.stringify(repo));
  }
  assert.ok(UpdateSettings.safeParse({ env: { GH_TOKEN: "ghp_abc" } }).success);
});

test("a desktop request without a size leaves the screen as it is", () => {
  assert.equal(DesktopRequest.parse({}).size, undefined);
  assert.equal(DesktopRequest.parse({ size: "1920x1080" }).size, "1920x1080");
});
