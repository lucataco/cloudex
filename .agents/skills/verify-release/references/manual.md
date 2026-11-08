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

## Dashboard (use the Chrome DevTools MCP, or headless Chrome over CDP; then by hand in Safari and Firefox)

Open the browser's console first, and keep it open: through all of these there must be no CSP violations or errors, and no request may carry a key or a token in its URL.

10. **Sign in three ways.** `cloudex dashboard` opens the sandbox list, signed in, and the address bar no longer holds the link's code. Opening the same link again says it was used. On the login page in a private window, **Get a code** and approve it with `cloudex tokens approve`; then sign in again by pasting a `read` token, and check that the read session has no New sandbox, Terminal or Desktop.
11. **Approve in the dashboard.** `cloudex login --url <API URL>` on another config dir (`CLOUDEX_CONFIG_DIR=$(mktemp -d)`) prints a `/login?code=` link. Opening it in the signed-in browser shows where the sign-in comes from, and Approve signs the CLI in.
12. **Terminal.** Run `vim` (`:wq` saves), `htop` (quits with `q`), and `tput cols` before and after resizing the window. Open a second tab and a root tab. Then reload the page: the same shells come back, with what was on them. Close a tab, and check with `tmux -L cloudex-dash ls` that its session is gone.
13. **Terminal across a deploy.** With a terminal open, `cloudex deploy` again. Within some minutes (up to about 45) the tab shows "reconnecting…", then the same shell (`echo $$` gives the same number).
14. **Desktop.** The desktop tab starts the desktop and shows it. Typing into Chrome on it works, the size menu resizes it, and **Match this window's size** follows the window. Copy text on the desktop and it shows under Clipboard.
15. **Agents.** Start an OpenCode agent with `anthropic/claude-haiku-4-5` and a task that runs a command. The command, the end of its output, the reply and "turn 1 done" show as text. Send a second message. Interrupt a long turn.
16. **Files.** Upload two files by dropping them, download one, download a directory as tar.gz, delete a file. A file named `<b>x</b>.html` shows as that text, and downloading it saves it rather than opening it.
17. **Sign out and revoke.** Sign out: the next click goes to the login page. Sign in again, then revoke that session with `cloudex tokens revoke`: the page goes to the login page within a few seconds. Revoke the token a session was signed in with, and that session ends too.

## Setup paths

18. **Install.** In a new shell, `curl -fsSL https://cloudex.build/install | sh` installs the CLI. `cloudex version` then works, or the installer says how to put it on PATH for this shell.
19. **Update from the previous release.** Install the previous release into a temp directory (see the triage-feedback skill, step 4). Run it against the new deployment with `verify.sh setup lifecycle connect` (`CLOUDEX_BIN=<that binary>`). Then `cloudex update` moves it to the new release and offers `cloudex deploy`.
20. **Deploy twice.** `cloudex deploy` on the already-deployed test account keeps sandboxes, saves, the key, provider keys and the preview domain. Check `cloudex keys` and `cloudex list` afterwards.
21. **Feedback.** `cloudex feedback "test, please ignore"` in a terminal shows the report and asks. Answer no. Check that no key, URL or sandbox name appears in it.
