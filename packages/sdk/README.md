# Cloudex SDK for TypeScript and JavaScript

Create, fork and drive [Cloudex](https://cloudex.build) sandboxes: agent
computers in your own Cloudflare account. It runs anywhere `fetch` does (Node
18+, Bun, Deno, Workers) and has no dependencies. Use it from a server or a
script: the API sends no CORS headers, so a browser page can't call it.

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

`box.prompt()` starts Claude Code, Codex or OpenCode on a task with no terminal. Each
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

## Commands

`box.exec()` waits for a command and returns its output, cut at 1 MiB.
`box.execStream()` gives the output as it comes, uncut, and ends with the exit
code. Leaving the loop early stops the command.

```ts
for await (const e of box.execStream("npm test", { cwd: "~/app" })) {
  if (e.type === "exit") console.log("exit code", e.exitCode);
  else process.stdout.write(e.data); // e.type is stdout or stderr
}
```

For servers and long jobs, start a command in the background. It keeps
running after your program ends, with its output kept in the sandbox, until it
ends, you kill it or the sandbox stops.

```ts
const cmd = await box.startCommand("npm run build", { cwd: "app" });
for await (const part of box.followCommand(cmd.id)) {
  process.stdout.write(part.data);
  if (part.command.state !== "running") console.log(part.command.state, part.command.exitCode);
}

const server = await box.startCommand("npm start", { cwd: "app" });
console.log(await box.host(3000));
await box.killCommand(server.id); // TERM; or "INT", "HUP", "KILL"
```

## Files

Paths are in the home directory unless they start with `/`. `{ root: true }`
reads and writes as root.

```ts
await box.writeFile("app/.env", "PORT=3000\n");
await box.writeFile("bin/tool", bytes, { mode: "755" }); // a Uint8Array, Blob or stream works too
const text = new TextDecoder().decode(await box.readFile("app/package.json"));
const dir = await box.stat("app"); // dir.entries lists what's in it
await box.deleteFile("app/dist", { recursive: true });

await box.uploadArchive("app", await fs.readFile("app.tar.gz")); // fs from node:fs/promises; unpacks into ~/app
const tgz = await box.downloadArchive("app/dist");
```

## Connecting

`new Cloudex({ apiUrl, apiKey })` takes the API URL that `cloudex deploy`
printed and the key that `cloudex status --show-key` shows. Without them it
reads `$CLOUDEX_API_URL` and `$CLOUDEX_API_KEY`, then, in Node, Bun and Deno,
the deployment the CLI saved. The saved key is used only with the saved URL: for
any other URL, pass `apiKey` or set `$CLOUDEX_API_KEY`. In Workers, pass them
in. The key gives full control of your sandboxes; keep it on a server.

## API

| Call | Does |
|---|---|
| `cx.create({ name?, size?, idleMinutes?, env?, files?, repo? })` | Create a sandbox; resolves once it's running (and the repo is cloned) |
| `cx.get(ref)`, `cx.list()`, `cx.health()` | Look up by id or name, list, check the deployment |
| `box.exec(cmd, { root?, cwd?, timeoutSec? })` | Run shell code, or an argument list, and wait; check `exitCode` |
| `box.execStream(cmd, { root?, cwd?, timeoutSec? })` | The same, as an async iterator over output events that ends with `exit` |
| `box.startCommand(cmd, { root?, cwd?, timeoutSec? })` | Start a command in the background; returns a `Command` at once |
| `box.listCommands()`, `box.getCommand(id)` | The sandbox's commands, newest first; one by id |
| `box.commandOutput(id, { after?, wait? })`, `box.followCommand(id, { after? })` | Read a command's output from a byte offset; follow it to the end |
| `box.killCommand(id, signal?)`, `box.forgetCommand(id)` | Signal a running command; delete a finished one's record and output |
| `box.readFile(path, { root? })`, `box.writeFile(path, data, { mode?, root? })` | Download a file as a `Uint8Array`; upload one whole |
| `box.stat(path, { root? })`, `box.deleteFile(path, { recursive?, root? })` | A path's details, with a directory's entries; delete one |
| `box.downloadArchive(path, { root? })`, `box.uploadArchive(path, tarGz, { root? })` | A directory as a tar.gz; unpack one into a directory |
| `box.fork({ count?, name?, size?, idleMinutes? })` | Up to 10 sandboxes starting from a copy of the disk |
| `box.save(name)` | Keep a copy of the running sandbox's disk under a name until it's deleted; a taken name's save is replaced (needs R2) |
| `cx.startFromSave(name, { count?, name?, size?, idleMinutes? })` | Up to 10 sandboxes starting from a named save |
| `cx.listSaves()`, `cx.getSave(name)`, `cx.deleteSave(name)` | The named saves, newest first; one by name; delete one |
| `box.host(port)` | HTTPS link to a port on your preview domain; servers on localhost work too |
| `box.links()`, `box.revokeLinks(port?)` | The links that work; make one port's link, or all of them, stop working |
| `box.update({ name?, idleMinutes? })` | Rename, or change the idle time |
| `box.desktop({ size? })`, `box.stopDesktop()` | Start the desktop (Chrome, a terminal) and get a link to it |
| `box.stop()`, `box.resume({ fromBackup?, size? })`, `box.delete()` | Save and stop, start again (on another size with `size`), delete |
| `box.refresh()` | Fetch the latest state; `box.id`, `box.name`, `box.state` and `box.info` hold it |
| `box.settings()`, `box.updateSettings({ env?, files? })` | Environment variables and files the sandbox gets at every start; `null` removes one |
| `cx.setKey(provider, key)`, `cx.listKeys()`, `cx.deleteKey(provider)` | AI provider keys sandboxes use without holding them (`"anthropic"`, `"openai"`, `"openrouter"`, `"google"`, `"groq"`, `"mistral"`, `"xai"`, `"deepseek"`) |
| `box.prompt(prompt, { agent?, cwd?, model? })` | Start Claude Code (default), Codex or OpenCode on a task; returns an `Agent` at once |
| `box.agents()`, `box.agent(id)` | The sandbox's agents, newest first; one by id |
| `agent.events({ after?, follow? })` | Async iterator over its events; follows until the agent is idle |
| `agent.send(prompt, { interrupt? })`, `agent.interrupt()`, `agent.wait()` | Continue, stop the running turn, wait until idle |

`cx.list()` returns a summary of each sandbox. `cx.get()` and `box.refresh()`
return every field, including `info.notice`, `info.forkedFrom`, `info.fromSave`,
`info.providerKeys` and the `info.backup*` fields. On a sandbox from `list()`
those are missing until you call `box.refresh()`.

Errors from the API throw `CloudexError`, with `status` and the API's message.
When there was no answer (the API can't be reached, a request timed out, a
response broke off), `status` is 0 and the message starts with `can't reach` and
the API URL. The full API is described at `<your API URL>/v1/openapi.json`.
