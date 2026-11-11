---
name: verify-release
description: Check that everything Cloudex ships works on a live deployment, the way users run it, before tagging a release or after deploying a branch to the test deployment. Runs scripted checks for the CLI, the API, both SDKs and agents, plus a short list of checks that need eyes. Use when asked to "verify the release", "run the release checks", "is main shippable", before pushing a v* tag, or after fixing a bug that a live check should cover.
---

# Verify a release

Cloudex has unit tests, but most of what breaks lives between the CLI, the Worker, the Durable Object and the container. Only a live run catches it. This skill runs that live check against the owner's test deployment and reports one line per check.

## Rules

- Run only against the owner's test deployment, never a user's.
- The scripts use a copy of the CLI config in a temp directory. They never change the owner's current sandbox or `known_hosts`. Every sandbox and save they make is named `vr<HHMMSS>-*`, and they delete them on exit, even after a failure. Check `cloudex list` and `cloudex saves` afterwards anyway.
- Never print the deployment's API key, URL or preview links in commits, issues or PRs.
- A run costs a few cents of container time. The agents area also spends provider tokens.

## Run it

```sh
.agents/skills/verify-release/scripts/verify.sh                 # every area
.agents/skills/verify-release/scripts/verify.sh connect inside  # some areas
CLOUDEX_BIN=go/bin/cloudex .agents/skills/verify-release/scripts/verify.sh   # a dev build
```

Areas: `setup`, `lifecycle`, `connect`, `inside` (the `cloudex` command in a sandbox), `agents` (skipped without provider keys; see `cloudex keys`), `tokens`, `dashboard` (the dashboard's API, sign-in and terminal socket, with curl), `browser` (the dashboard in a real browser; see below), `sdk` (`sdk-smoke.mjs` and `sdk_smoke.py`), `node` (nodes, v0.2.2: N01–N08 join tokens, signed connects, online and offline, `node rm`; N09–N16 streams and exec over the link: exec, scp, cp, previews, flow control, the terminal and clean-up; N17–N25 the Docker runtime: the installer, the isolation probes from inside a machine plain and with route-changing rights, the machine's flags, a Docker restart, an agent restart, a reboot, a failing self-test, and the uninstaller leaving the host as it was). It installs the agent on the host `VERIFY_NODE_SSH` reaches (such as `VERIFY_NODE_SSH="ssh box"`) with `scripts/node-install.sh`, as root with `VERIFY_NODE_SUDO=1` (passwordless sudo there), else `--unisolated` (testing only; the root-only checks are a `SKIP`), and uninstalls it at the end; it refuses a host that runs a `cloudex-node` already. The agent's image is `VERIFY_NODE_AGENT_IMAGE` when the host has it, else it's built there from this checkout; `VERIFY_NODE_MACHINE_IMAGE` is the installer's `--machine-image`. Without `VERIFY_NODE_SSH` the area is a `SKIP`. N07 redeploys, so it runs only with `VERIFY_NODE_DEPLOY=1`; N12 holds an idle ssh session for 30 minutes and N13 deploys during an scp, so they run only with `VERIFY_NODE_LONG=1` (N13 also with `VERIFY_NODE_DEPLOY=1`), and both read Durable Object analytics with `CLOUDFLARE_API_TOKEN`; N23 reboots the host, so it runs only with `VERIFY_NODE_REBOOT=1`). A full run takes about 10 minutes, and the browser area adds about 5 to 10 (the desktop's first start is most of it). It prints `PASS`, `FAIL` or `SKIP` with a check id, writes `results.tsv` to `$VERIFY_OUT` (default: a temp directory), and exits 1 if anything failed. `VERIFY_KEEP=1` keeps the sandboxes so you can look at a failure.

A `SKIP` isn't a pass. Report each one with its reason.

### The dashboard in a browser

The `browser` area runs `scripts/browser/run.mjs`, which drives headless Chrome over the DevTools protocol (or Firefox over WebDriver BiDi) with nothing but Node 24. It signs in the way a user does, with `cloudex dashboard --no-open --json`, and goes through the sign-in pages, terminal (vi, htop, resize, UTF-8, tabs, reload, a dropped socket), desktop, commands, files, saves, access, code sign-in and approving `/login?code=`, phone width, sign-out and revoking, then checks the console for CSP violations and request URLs for secrets. Its checks are `B01`–`B40`, in the same `PASS`/`FAIL`/`SKIP` lines. It makes its own sandbox, `vr<HHMMSS>-chrome`, and ends every session, token and sign-in it starts, even after a failure or `Ctrl-C`; `B40` fails if anything is left. The agent turn (`B16`–`B18`) spends provider tokens, so it runs only with the `agents` area or `VERIFY_AGENTS=1`. Chrome is the default; `VERIFY_BROWSERS="chrome firefox"` runs both, and `CHROME`/`FIREFOX` point at a browser outside the usual places. Without one, the area is a `SKIP`. On its own, with the same `CLOUDEX_BIN` and `CLOUDEX_CONFIG_DIR`:

```sh
node .agents/skills/verify-release/scripts/browser/run.mjs --browser firefox --only terminal,reconnect
node .agents/skills/verify-release/scripts/browser/selftest.mjs   # its pure parts, no browser
```

A failure saves `<id>.png` to `$VERIFY_OUT/browser-<browser>` (or the directory it names), with the browser's log. `--keep` keeps the sandbox.

## Test a branch before it ships

1. Push the branch, then build its image: `gh workflow run image.yml --ref <branch>`, and wait for it with `gh run watch`. This also moves the `edge` tag. The image is `ghcr.io/lucataco/cloudex-base:sha-<full commit sha>`. Skip this step when nothing under `images/` changed. Deploy then reuses the current release's image.
2. Build the CLI with the branch's API inside: `scripts/bundle-api.sh v0.1.X-dev && (cd go && go build -ldflags "-X main.version=v0.1.X-dev" -o bin/cloudex ./cmd/cloudex)`.
3. Deploy it: `CLOUDFLARE_API_TOKEN=... go/bin/cloudex deploy --image <image from step 1, or ghcr.io/lucataco/cloudex-base:<latest release tag>>`. A new image can take up to 20 minutes to start on every host.
4. Run `verify.sh` with `CLOUDEX_BIN=go/bin/cloudex`.
5. Before tagging, also run the installed release CLI (`cloudex`) against the new deployment with `verify.sh setup lifecycle connect`. Users update the deployment and the CLI at different times, so both directions must work.

## Checks that need eyes

The scripts can't see a terminal UI or a picture, and the browser checks don't run Safari. Go through [references/manual.md](references/manual.md) before a release; it marks what the `browser` area already covers. Use the `control-cli` skill for terminals and the `control-ui` skill (or the Chrome DevTools MCP) for the desktop.

## When a check fails

1. Rerun only that area. A single failure can be a flaky network.
2. If it fails again, reproduce it by hand with the commands the check runs. Then decide: a Cloudex bug, a harness bug, or a platform or zone setting. For example, a zone's Browser Integrity Check gives Python's urllib a 403 (error 1010) on preview links.
3. Fix a Cloudex bug with a unit test where the code has a test layer. The `triage-feedback` skill's rules for branches and tests apply.
4. Fix a harness bug in `verify.sh` in its own commit.

## Adding a check

Every bug a user could hit gets a live check. Add it to the area it belongs to in `verify.sh`, using the `cx` and `ok` helpers, with the next free id. Run it against the old build first and watch it fail, then against the fix and watch it pass. Write the description as the behavior a user expects ("cp into a local directory that doesn't exist yet"), not as the bug.
