// The TypeScript SDK against a live deployment: node sdk-smoke.mjs <path to dist/index.js> <name prefix>
// Uses the deployment in $CLOUDEX_CONFIG_DIR. Deletes what it makes. With $VERIFY_NODE, its
// sandbox runs on that node, and what isn't on nodes in v0.2.2 (forks, named saves,
// agents) must be refused with a 501 instead.
import assert from "node:assert/strict";

const [, , dist, prefix] = process.argv;
const { Cloudex, CloudexError } = await import(dist);
const cx = new Cloudex();
const made = [];
const saves = [];
const step = (s) => console.log("ok", s);
const node = process.env.VERIFY_NODE || "";
// On a node: the call must be refused, not available yet (501), saying why.
const refused = async (what, call) => {
  await assert.rejects(call, (e) => e instanceof CloudexError && e.status === 501, `${what} on a node`);
  console.log("skip", what, "(not on nodes in v0.2.2; refused with 501 as it should be)");
};

try {
  const health = await cx.health();
  assert.ok(health.version, "health has a version");
  step("health");

  const box = await cx.create({ name: `${prefix}-a`, env: { SDK_VAR: "ts" }, files: { "sdk.txt": "from-ts\n" }, ...(node ? { node } : {}) });
  made.push(box);
  assert.equal(box.state, "running");
  if (node) {
    assert.equal(box.placement.provider, "node");
    assert.equal(box.placement.node?.name, node);
    assert.ok((await cx.nodes.list({ live: false })).some((n) => n.name === node), "nodes.list has it");
    step("create on a node, placement, nodes.list");
  }
  assert.equal((await cx.get(`${prefix}-a`)).id, box.id, "get by name");
  assert.ok((await cx.list()).some((b) => b.id === box.id), "list has it");
  step("create, get, list");

  const r = await box.exec("echo $SDK_VAR; cat ~/sdk.txt; echo err >&2; exit 4");
  assert.equal(r.exitCode, 4);
  assert.equal(r.stdout, "ts\nfrom-ts\n");
  assert.equal(r.stderr, "err\n");
  assert.equal((await box.exec(["python3", "-c", "print('a b')"])).stdout, "a b\n", "argument list");
  step("exec, env and files");

  let streamed = "";
  let code;
  for await (const e of box.execStream("for i in 1 2 3; do echo $i; done; exit 2")) {
    if (e.type === "exit") code = e.exitCode;
    else streamed += e.data;
  }
  assert.equal(streamed, "1\n2\n3\n");
  assert.equal(code, 2);
  step("execStream");

  const cmd = await box.startCommand("echo bg; exit 6");
  let out = "";
  let last;
  for await (const part of box.followCommand(cmd.id)) {
    out += part.data;
    last = part.command;
  }
  assert.equal(out, "bg\n");
  assert.equal(last.exitCode, 6);
  const sleeper = await box.startCommand("sleep 600");
  const killed = await box.killCommand(sleeper.id);
  assert.ok((await box.listCommands()).some((c) => c.id === sleeper.id));
  assert.notEqual((await box.getCommand(killed.id)).state, "running");
  step("startCommand, followCommand, killCommand");

  await box.writeFile("d/f.txt", "file-body");
  assert.equal(new TextDecoder().decode(await box.readFile("d/f.txt")), "file-body");
  const st = await box.stat("d");
  assert.ok(st.entries.some((e) => e.name === "f.txt"), "stat lists entries");
  const tgz = await box.downloadArchive("d");
  await box.uploadArchive("d2", tgz);
  assert.equal(new TextDecoder().decode(await box.readFile("d2/f.txt")), "file-body", "archive round trip");
  await box.deleteFile("d", { recursive: true });
  await assert.rejects(box.readFile("d/f.txt"), CloudexError);
  step("files");

  await box.updateSettings({ env: { SDK_TWO: "2" } });
  assert.equal((await box.exec("echo $SDK_TWO")).stdout, "2\n");
  assert.ok((await box.settings()).env.includes("SDK_TWO"));
  await box.update({ idleMinutes: 90 });
  assert.equal(box.info.idleMinutes, 90);
  step("settings and update");

  if (health.previewZone) {
    await box.startCommand("mkdir -p /tmp/w && echo hosted > /tmp/w/index.html && cd /tmp/w && exec python3 -m http.server 8125 --bind 127.0.0.1");
    await new Promise((r) => setTimeout(r, 2000));
    const url = await box.host(8125);
    assert.equal(await (await fetch(url)).text(), "hosted\n");
    assert.ok((await box.links()).some((l) => l.url === url));
    await box.revokeLinks(8125);
    assert.equal((await fetch(url)).status, 404);
    step("host, links, revokeLinks");
  }

  await box.exec("echo forked > ~/marker");
  if (node) {
    await refused("fork", box.fork({ count: 2, name: `${prefix}-f` }));
    await refused("save", box.save(`${prefix}-save`));
  } else {
    const forks = await box.fork({ count: 2, name: `${prefix}-f` });
    made.push(...forks);
    assert.equal(forks.length, 2);
    assert.equal((await forks[1].exec("cat ~/marker")).stdout, "forked\n");
    step("fork");

    const save = await box.save(`${prefix}-save`);
    saves.push(save.name);
    assert.ok((await cx.listSaves()).some((s) => s.name === save.name));
    const [fromSave] = await cx.startFromSave(save.name, { name: `${prefix}-s` });
    made.push(fromSave);
    assert.equal((await fromSave.exec("cat ~/marker")).stdout, "forked\n");
    step("save, listSaves, startFromSave");
  }

  await box.stop();
  assert.equal(box.state, "stopped");
  await box.resume();
  assert.equal(box.state, "running");
  assert.equal((await box.exec("cat ~/marker")).stdout, "forked\n");
  step("stop and resume");

  await assert.rejects(cx.get(`${prefix}-missing`), (e) => e instanceof CloudexError && e.status === 404);
  step("errors are CloudexError with status");

  if (node) {
    await refused("prompt", box.prompt("Reply with exactly the word SDKOK and nothing else."));
  } else if ((await cx.listKeys()).some((k) => k.provider === "anthropic")) {
    const agent = await box.prompt("Reply with exactly the word SDKOK and nothing else. Do not run any tools.");
    await agent.wait();
    assert.match(agent.lastMessage ?? "", /SDKOK/, `agent said: ${agent.lastMessage} ${agent.error}`);
    await agent.send("Reply with exactly the word AGAINOK and nothing else.");
    await agent.wait();
    assert.match(agent.lastMessage ?? "", /AGAINOK/);
    const types = new Set();
    for await (const e of agent.events({ follow: false })) types.add(e.type);
    assert.ok(types.has("end"), "events include end");
    step("prompt, send, events");
  } else {
    console.log("skip agents: no anthropic key");
  }
} finally {
  for (const b of made) await b.delete().catch(() => {});
  for (const s of saves) await cx.deleteSave(s).catch(() => {});
}
console.log("done");
