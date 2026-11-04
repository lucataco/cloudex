---
name: cloudex
description: Run code, servers and coding agents in Cloudex sandboxes, Ubuntu machines in the user's own Cloudflare account, with the cloudex CLI. Use when a task needs a clean Linux machine, a long or risky job away from this computer, an HTTPS link to a running app for the user, or several copies of one environment working in parallel.
---

# Cloudex

`cloudex` drives sandboxes: Ubuntu 24.04 machines with sudo, Docker, Node, Python, git, `gh`, Claude Code and Codex, running in the user's Cloudflare account. Commands print JSON when their output isn't a terminal, so read the JSON; `exec`, `cp` and `run` print the command's own output instead. `cloudex <command> --help` lists every flag.

## Sandboxes

- `cloudex new --name <name>` creates a sandbox, waits until it runs, and makes it the current one: later commands without a sandbox name use it. `--repo https://github.com/owner/repo` clones a repo into the home directory, `--env NAME=value` sets a variable for every shell, and `--size standard-4` gives 4 vCPU and 12 GiB (the default `standard-2` has 1 vCPU and 6 GiB).
- `cloudex list` and `cloudex info <sandbox>` show sandboxes and their state.
- `cloudex stop <sandbox>` saves the whole disk and stops; `cloudex resume <sandbox>` starts it again from the save. Programs don't start again by themselves: commands in `~/.cloudex/start` inside the sandbox run at every start.
- A sandbox stops by itself after 30 minutes without activity. `cloudex set <sandbox> --idle 2h` changes that.
- `cloudex fork <sandbox> --count 3 --name try` makes copies with the same disk, named try-1 to try-3, for parallel work.
- `cloudex save <sandbox> <name>` keeps a copy of a running sandbox's disk under a name until it's deleted; `cloudex new --from <name>` starts sandboxes from it, so an environment set up once can be reused. `cloudex saves` lists the saves. Saving under a name that already exists replaces that save: ask the user before replacing one you didn't make, or before deleting one (`cloudex saves rm <name> --yes`).
- `cloudex delete <sandbox> --yes` deletes a sandbox and its data for good. Ask the user before deleting one you didn't create.
- Sandboxes cost the user money while they run. Stop or delete the ones you no longer need.

## Running things

- `cloudex exec <sandbox> -- '<shell command>'` runs a command with bash as the cloudex user in its home directory, prints its output as it comes, and exits with its exit code. It stops the command after 10 minutes by default; `--timeout 1h` is the most. `--root` runs as root, `--cwd app` runs in ~/app. Several arguments after `--` are quoted for you: `cloudex exec web -- python3 -c 'print(1)'`.
- Long jobs and servers go in the background: `cloudex exec -d web -- 'cd app && npm run dev'` prints a command id (cm-...). `cloudex commands web` lists them, `cloudex commands logs -f web <id>` follows the output until it ends and exits with its code, and `cloudex commands kill web <id>` stops it. Then `cloudex host web 5173` gives an HTTPS link to share with the user. Servers that listen only on localhost work. `cloudex host web 5173 --revoke` makes a link stop working.
- `cloudex cp ./file web:path/` and `cloudex cp web:path ./local` copy files and directories in and out (`:path` is the current sandbox), up to 100 MB at a time.
- `cloudex run -- '<command>'` runs a command in a fresh sandbox with the current directory's files (what git would commit), prints its output, and deletes the sandbox; `--from <save>` starts from a named save, `--out dist` copies a result back, and it exits with the command's code. Use it for tests and builds that shouldn't run on this computer.
- Docker works inside, with `--network host` only.
- For many files or repeated syncs, run `cloudex ssh-config >> ~/.ssh/config` once; then `scp` and `rsync` reach `<sandbox>.cloudex`, for example `rsync -a ./app/ web.cloudex:app/`.

## Agents in a sandbox

- `cloudex agents start <sandbox> "<task>"` starts Claude Code there (`--agent codex` for Codex) with no terminal and prints what it does until the turn ends; `-d` returns at once.
- `cloudex agents logs <sandbox> -f` follows the newest agent, `cloudex agents send <sandbox> "<message>"` continues its conversation, and `cloudex agents interrupt <sandbox>` stops its turn.
- Inside a sandbox, `cloudex` manages only that sandbox: `cloudex host <port>`, `cloudex info`, `cloudex set --idle <time>` and `cloudex stop`.

## When something breaks

If `cloudex` fails in a way that looks like a bug in Cloudex (it suggests `cloudex feedback`, the API answers with a 5xx error, or the CLI crashes), tell the user what happened. With their OK, run `cloudex feedback --last-error "what you were doing"`: without a terminal it sends nothing, and prints the report and a `cloudex feedback --send <id>` command. Show the user that report, and run the `--send` command only if they agree; it sends exactly that report. Reports are public GitHub issues, filed under the user's GitHub account when `gh` is signed in, so never put keys or private details in the message. Security problems go to https://github.com/lucataco/cloudex/security/advisories/new instead.
