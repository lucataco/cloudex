---
name: cloudex
description: Run code, servers and coding agents in Cloudex sandboxes, Ubuntu machines in the user's own Cloudflare account, with the cloudex CLI. Use when a task needs a clean Linux machine, a long or risky job away from this computer, an HTTPS link to a running app for the user, or several copies of one environment working in parallel.
---

# Cloudex

`cloudex` drives sandboxes: Ubuntu 24.04 machines with sudo, Docker, Node, Python, git, `gh`, Claude Code and Codex, running in the user's Cloudflare account. Commands print JSON when their output isn't a terminal, so read the JSON. `cloudex <command> --help` lists every flag.

## Sandboxes

- `cloudex new --name <name>` creates a sandbox, waits until it runs, and makes it the current one: later commands without a sandbox name use it. `--repo https://github.com/owner/repo` clones a repo into the home directory, `--env NAME=value` sets a variable for every shell, and `--size standard-4` gives 4 vCPU and 12 GiB (the default `standard-2` has 1 vCPU and 6 GiB).
- `cloudex list` and `cloudex info <sandbox>` show sandboxes and their state.
- `cloudex stop <sandbox>` saves the whole disk and stops; `cloudex resume <sandbox>` starts it again from the save. Programs don't start again by themselves: commands in `~/.cloudex/start` inside the sandbox run at every start.
- A sandbox stops by itself after 30 minutes without activity. `cloudex set <sandbox> --idle 2h` changes that.
- `cloudex fork <sandbox> --count 3 --name try` makes copies with the same disk, named try-1 to try-3, for parallel work.
- `cloudex delete <sandbox> --yes` deletes a sandbox and its data for good. Ask the user before deleting one you didn't create.
- Sandboxes cost the user money while they run. Stop or delete the ones you no longer need.

## Running things

- `cloudex exec <sandbox> -- '<shell command>'` runs a command with bash as the cloudex user in its home directory and returns `exitCode`, `stdout` and `stderr`. It waits up to 10 minutes by default; `--timeout 1h` is the most. `--root` runs as root.
- Start servers in the background so exec returns, for example `cloudex exec web -- 'cd app && nohup npm run dev >dev.log 2>&1 &'`, then run `cloudex host web 5173` for an HTTPS link to give the user. Servers that listen only on localhost work. `cloudex host web 5173 --revoke` makes a link stop working.
- Docker works inside, with `--network host` only.
- For files, run `cloudex ssh-config >> ~/.ssh/config` once; then `scp` and `rsync` reach `<sandbox>.cloudex`, for example `rsync -a ./app/ web.cloudex:app/`.

## Agents in a sandbox

- `cloudex agents start <sandbox> "<task>"` starts Claude Code there (`--agent codex` for Codex) with no terminal and prints what it does until the turn ends; `-d` returns at once.
- `cloudex agents logs <sandbox> -f` follows the newest agent, `cloudex agents send <sandbox> "<message>"` continues its conversation, and `cloudex agents interrupt <sandbox>` stops its turn.
- Inside a sandbox, `cloudex` manages only that sandbox: `cloudex host <port>`, `cloudex info`, `cloudex set --idle <time>` and `cloudex stop`.

## When something breaks

If `cloudex` fails in a way that looks like a bug in Cloudex (it suggests `cloudex feedback`, the API answers with a 5xx error, or the CLI crashes), tell the user what happened. With their OK, run `cloudex feedback --last-error "what you were doing"`: without a terminal it sends nothing, and prints the report and a `cloudex feedback --send <id>` command. Show the user that report, and run the `--send` command only if they agree; it sends exactly that report. Reports are public GitHub issues, filed under the user's GitHub account when `gh` is signed in, so never put keys or private details in the message. Security problems go to https://github.com/lucataco/cloudex/security/advisories/new instead.
