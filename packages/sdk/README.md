# Cloudex SDK for TypeScript and JavaScript

Create, fork and drive [Cloudex](https://cloudex.build) sandboxes: agent
computers in your own Cloudflare account. It runs anywhere `fetch` does (Node
18+, Bun, Deno, Workers, browsers) and has no dependencies.

```sh
npm install cloudex
```

```ts
import { Cloudex } from "cloudex";

const cx = new Cloudex(); // the deployment saved by `cloudex login`
const box = await cx.create({ name: "web" });
await box.exec("git clone https://github.com/you/app && cd app && npm install");

// Ten copies of that disk, each trying something different.
const forks = await box.fork({ count: 10, name: "try" });
const results = await Promise.all(forks.map((f, i) => f.exec(["npm", "test", "--", `--shard=${i + 1}/10`])));

console.log(await forks[0]!.host(3000)); // an HTTPS link to a port
await Promise.all(forks.map((f) => f.delete()));
await box.stop(); // saves the disk; box.resume() picks up where it left off
```

## Agents

`box.prompt()` starts Claude Code, or Codex, on a task with no terminal. Each
message is one turn of the agent's own conversation, which lives on the
sandbox's disk.

```ts
const agent = await box.prompt("make the tests pass", { cwd: "app" });
for await (const e of agent.events()) {
  // e.type is prompt, event (the agent's own JSON in e.event), output or end
  if (e.type === "event") console.log(e.event);
}
console.log(agent.lastMessage, agent.error);

await agent.send("now update the docs"); // waits for a running turn
await agent.send("stop, fix the build first", { interrupt: true });
await agent.wait();
```

## Connecting

`new Cloudex({ apiUrl, apiKey })` takes the API URL and key that `cloudex
deploy` printed (`cloudex status --show-key` shows them again). Without them it
reads `$CLOUDEX_API_URL` and `$CLOUDEX_API_KEY`, then, in Node, Bun and Deno,
the deployment the CLI saved. In Workers and browsers, pass them in. The key
gives full control of your sandboxes; keep it on a server.

## API

| Call | Does |
|---|---|
| `cx.create({ name?, size?, idleMinutes?, env?, files?, repo? })` | Create a sandbox; resolves once it's running (and the repo is cloned) |
| `cx.get(ref)`, `cx.list()`, `cx.health()` | Look up by id or name, list, check the deployment |
| `box.exec(cmd, { root?, cwd?, timeoutSec? })` | Run shell code, or an argument list, and wait; check `exitCode` |
| `box.fork({ count?, name?, size?, idleMinutes? })` | Up to 10 sandboxes starting from a copy of the disk |
| `box.host(port)` | HTTPS link to a port on your preview domain; servers on localhost work too |
| `box.links()`, `box.revokeLinks(port?)` | The links that work; make one port's link, or all of them, stop working |
| `box.update({ name?, idleMinutes? })` | Rename, or change the idle time |
| `box.desktop({ size? })`, `box.stopDesktop()` | Start the desktop (Chrome, a terminal) and get a link to it |
| `box.stop()`, `box.resume({ fromBackup?, size? })`, `box.delete()` | Save and stop, start again (on another size with `size`), delete |
| `box.refresh()` | Fetch the latest state; `box.id`, `box.name`, `box.state` and `box.info` hold it |
| `box.settings()`, `box.updateSettings({ env?, files? })` | Environment variables and files the sandbox gets at every start; `null` removes one |
| `cx.setKey(provider, key)`, `cx.listKeys()`, `cx.deleteKey(provider)` | AI provider keys sandboxes use without holding them (`"anthropic"`, `"openai"`) |
| `box.prompt(prompt, { agent?, cwd?, model? })` | Start Claude Code (default) or Codex on a task; returns an `Agent` at once |
| `box.agents()`, `box.agent(id)` | The sandbox's agents, newest first; one by id |
| `agent.events({ after?, follow? })` | Async iterator over its events; follows until the agent is idle |
| `agent.send(prompt, { interrupt? })`, `agent.interrupt()`, `agent.wait()` | Continue, stop the running turn, wait until idle |

Errors from the API throw `CloudexError`, with `status` and the API's message.
The full API is described at `<your API URL>/v1/openapi.json`.
