# Decisions the owner already made

Follow these when you sort and answer issues. Don't ask the owner again; escalate only what the
SKILL.md says to.

## What Cloudex is

- Open source (MIT). Each user deploys it into their own Cloudflare account with `cloudex deploy`.
  One deployment has one owner and one admin key.
- **No hosted service.** Cloudflare Containers cost several times what a hosted price would need,
  and Cloudflare's self-serve terms don't allow reselling access. "Please host this" gets a kind no,
  with the reason. An offer of money or a partnership goes to the owner.

## Not bugs: Cloudflare's limits

Answer these with https://cloudex.build/docs/limits and close them (label `wontfix` only if the
reporter asked for a change):

- no public IP, no inbound TCP or UDP from the internet (only HTTPS preview links and the API's bridge)
- at most 4 vCPU, 12 GiB of memory and 20 GB of disk; no GPU, no /dev/kvm
- Docker inside works with `--network host` only
- Cloudflare stops a container after 6 hours without an incoming connection
- saves expire 30 days after they were made or last restored; backups of /home don't

## By design

- Anyone with a preview link can open it; `cloudex host --revoke` ends a link.
- Sandboxes use the deployment's provider keys without holding them; any sandbox can spend them.
  Scoped keys are on the roadmap.
- Claude Code and Codex run without their own permission prompts in a sandbox; the sandbox is the
  boundary, and Claude Code's bypass-permissions warning is accepted ahead of time.
- The cloudex command inside a sandbox manages only that sandbox.
- Feedback is public GitHub issues, filed with `cloudex feedback`. A private intake service was
  considered and deferred.

## Roadmap (say "planned", label `enhancement`, link nothing private)

1. Saves that don't expire: named snapshots stored in R2, new sandboxes from a backup.
2. v0.2: OpenCode as a third agent, scoped API keys with device login, a web dashboard, webhooks.
3. Later: running sandboxes on your own machines (homelab or VPS nodes).

Other agent harnesses, more AI providers and bigger machines are ideas to collect, not promises.

## How work ships

- The owner tags releases and publishes the docs site. Fixes reach users in the next release;
  `cloudex update` installs it and offers to update the deployment.
- Docs live in lucataco/cloudex-web and describe only what has shipped.
