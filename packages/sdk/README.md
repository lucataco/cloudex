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

## Connecting

`new Cloudex({ apiUrl, apiKey })` takes the API URL and key that `cloudex
deploy` printed (`cloudex status --show-key` shows them again). Without them it
reads `$CLOUDEX_API_URL` and `$CLOUDEX_API_KEY`, then, in Node, Bun and Deno,
the deployment the CLI saved. In Workers and browsers, pass them in. The key
gives full control of your sandboxes; keep it on a server.

## API

| Call | Does |
|---|---|
| `cx.create({ name?, size?, idleMinutes? })` | Create a sandbox; resolves once it's running |
| `cx.get(ref)`, `cx.list()`, `cx.health()` | Look up by id or name, list, check the deployment |
| `box.exec(cmd, { root?, cwd?, timeoutSec? })` | Run shell code, or an argument list, and wait; check `exitCode` |
| `box.fork({ count?, name?, size?, idleMinutes? })` | Up to 10 sandboxes starting from a copy of the disk |
| `box.host(port)` | HTTPS link to a port on your preview domain |
| `box.desktop({ size? })`, `box.stopDesktop()` | Start the desktop (Chrome, a terminal) and get a link to it |
| `box.stop()`, `box.resume({ fromBackup? })`, `box.delete()` | Save and stop, start again, delete |
| `box.refresh()` | Fetch the latest state; `box.id`, `box.name`, `box.state` and `box.info` hold it |

Errors from the API throw `CloudexError`, with `status` and the API's message.
The full API is described at `<your API URL>/v1/openapi.json`.
