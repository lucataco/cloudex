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

Areas: `setup`, `lifecycle`, `connect`, `inside` (the `cloudex` command in a sandbox), `agents` (skipped without provider keys; see `cloudex keys`), `sdk` (`sdk-smoke.mjs` and `sdk_smoke.py`). A full run takes about 10 minutes. It prints `PASS`, `FAIL` or `SKIP` with a check id, writes `results.tsv` to `$VERIFY_OUT` (default: a temp directory), and exits 1 if anything failed. `VERIFY_KEEP=1` keeps the sandboxes so you can look at a failure.

A `SKIP` isn't a pass. Report each one with its reason.

## Test a branch before it ships

1. Push the branch, then build its image: `gh workflow run image.yml --ref <branch>`, and wait for it with `gh run watch`. This also moves the `edge` tag. The image is `ghcr.io/lucataco/cloudex-base:sha-<full commit sha>`. Skip this step when nothing under `images/` changed. Deploy then reuses the current release's image.
2. Build the CLI with the branch's API inside: `scripts/bundle-api.sh v0.1.X-dev && (cd go && go build -ldflags "-X main.version=v0.1.X-dev" -o bin/cloudex ./cmd/cloudex)`.
3. Deploy it: `CLOUDFLARE_API_TOKEN=... go/bin/cloudex deploy --image <image from step 1, or ghcr.io/lucataco/cloudex-base:<latest release tag>>`. A new image can take up to 20 minutes to start on every host.
4. Run `verify.sh` with `CLOUDEX_BIN=go/bin/cloudex`.
5. Before tagging, also run the installed release CLI (`cloudex`) against the new deployment with `verify.sh setup lifecycle connect`. Users update the deployment and the CLI at different times, so both directions must work.

## Checks that need eyes

The scripts can't see a terminal UI or a picture. Go through [references/manual.md](references/manual.md) before a release. Use the `control-cli` skill for terminals and the `control-ui` skill (or the Chrome DevTools MCP) for the desktop.

## When a check fails

1. Rerun only that area. A single failure can be a flaky network.
2. If it fails again, reproduce it by hand with the commands the check runs. Then decide: a Cloudex bug, a harness bug, or a platform or zone setting. For example, a zone's Browser Integrity Check gives Python's urllib a 403 (error 1010) on preview links.
3. Fix a Cloudex bug with a unit test where the code has a test layer. The `triage-feedback` skill's rules for branches and tests apply.
4. Fix a harness bug in `verify.sh` in its own commit.

## Adding a check

Every bug a user could hit gets a live check. Add it to the area it belongs to in `verify.sh`, using the `cx` and `ok` helpers, with the next free id. Run it against the old build first and watch it fail, then against the fix and watch it pass. Write the description as the behavior a user expects ("cp into a local directory that doesn't exist yet"), not as the bug.
