# Checks that need eyes

Go through these after `verify.sh` passes, on the build you are about to ship. Write down each one's result. Use sandboxes named `vr-manual-*` and delete them at the end.

## Terminal (use the control-cli skill)

1. **Human output.** Run `cloudex list`, `info`, `saves`, `commands`, `env`, `keys`, `host --list` and `status` in a real terminal. Tables line up, times read as "3m ago", and sandboxes show by name.
2. **Interactive ssh.** `cloudex ssh`: you get a prompt, colors and line editing work, `Ctrl-D` exits. Then resize the window and run `tput cols`.
3. **ssh-config.** `cloudex ssh-config >> ~/.ssh/config`, then `ssh <name>.cloudex`. Do it again after `cloudex delete <name>` and `cloudex new --name <name>`.
4. **prompt and attach.** Do this with a provider key set. `cloudex prompt "list the files here"` opens Claude Code in tmux and you watch it work. Detach with `Ctrl-b d`, then `cloudex attach`. Do the same with `--agent codex`.
5. **agents start in a terminal.** The output is readable text, not raw JSON. `Ctrl-C` stops following and leaves the agent running.
6. **Confirmations.** `cloudex delete` and `cloudex saves rm` without `--yes` ask first. Answering no keeps the sandbox.
7. **Errors.** Name a sandbox that doesn't exist. Run `cloudex exec` on a stopped one. Run `cloudex host` without a port. Each error says what to do next, in one line.

## Browser (use the control-ui skill or the Chrome DevTools MCP)

8. **Desktop.** `cloudex desktop` opens the browser on noVNC. Chrome and the taskbar are visible, and typing into Chrome works. Do `--quality 0` and `--size 1920x1080` too. After `Ctrl-C`, `cloudex desktop` again reconnects in seconds at the same size.
9. **Preview link.** Run a dev server (for example `npx vite` in a new app) with `exec -d`, then `cloudex host <port>`. The page loads, and hot reload works over its WebSocket.

## Setup paths

10. **Install.** In a new shell, `curl -fsSL https://cloudex.build/install | sh` installs the CLI. `cloudex version` then works, or the installer says how to put it on PATH for this shell.
11. **Update from the previous release.** Install the previous release into a temp directory (see the triage-feedback skill, step 4). Run it against the new deployment with `verify.sh setup lifecycle connect` (`CLOUDEX_BIN=<that binary>`). Then `cloudex update` moves it to the new release and offers `cloudex deploy`.
12. **Deploy twice.** `cloudex deploy` on the already-deployed test account keeps sandboxes, saves, the key, provider keys and the preview domain. Check `cloudex keys` and `cloudex list` afterwards.
13. **Feedback.** `cloudex feedback "test, please ignore"` in a terminal shows the report and asks. Answer no. Check that no key, URL or sandbox name appears in it.
