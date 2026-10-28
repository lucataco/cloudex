# Cloudex

Open-source agent computers that run in your own Cloudflare account. Each sandbox
is a Linux machine with a shell, Docker, SSH and coding agents, saved and resumed
on demand. You pay Cloudflare directly; there is no Cloudex service in between.

**Status: pre-alpha.** Nothing here works end to end yet. v0.1 is under way:
install the CLI, run `cloudex deploy` against your own account, then create and
SSH into sandboxes. Site: [cloudex.build](https://cloudex.build).

## Layout

| Path | What |
|---|---|
| `apps/api` | Control plane Worker: the `/v1` API and one `Sandbox` Durable Object per sandbox, each owning one container |
| `images/base` | The default sandbox image: Ubuntu 24.04, Node, Python, Docker, sshd, Claude Code and Codex |
| `go/cmd/cloudex` | The `cloudex` CLI |
| `packages/schema` | Zod models shared by the API and, later, generated SDKs |

## Requirements

- A Cloudflare account on **Workers Paid** ($5/month). Containers need it.
- Node 22+ and pnpm, Go 1.27+, and Docker to build the image.

## Develop

```sh
pnpm install
pnpm typecheck

# API on localhost:8787 without building the container image
echo 'ADMIN_KEY=dev-admin-key' > apps/api/.dev.vars
pnpm --filter @cloudex/api exec wrangler dev --enable-containers=false

# CLI
cd go && go test ./... && go run ./cmd/cloudex status --api-url http://localhost:8787
```

## Deploy by hand (until `cloudex deploy` exists)

```sh
cd apps/api
npx wrangler secret put ADMIN_KEY
npx wrangler deploy
```

The first deploy builds and pushes the image. A newly pushed image can take
15–20 minutes before a sandbox can start on it.

## License

[MIT](LICENSE)
