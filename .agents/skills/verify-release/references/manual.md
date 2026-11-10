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

## Dashboard (by hand in Safari; the rest is scripted)

`verify.sh browser` (`scripts/browser/run.mjs`) does most of these in headless Chrome, and with `VERIFY_BROWSERS="chrome firefox"` in Firefox too; the check ids it covers are in brackets. Do in Safari, by hand, what a line marks **Safari**, and everything marked **By eye** in every browser. Open the console first and keep it open: through all of these there must be no CSP violations or errors, and no request may carry a key or a token in its URL (scripted as B38 and B39).

10. **Sign in three ways.** [Scripted: B01–B04, B27–B30.] `cloudex dashboard` opens the sandbox list, signed in, and the address bar no longer holds the link's code. Opening the same link again in a private window says it was used; a new link opened in a browser that's already signed in keeps its session. On the login page in a private window, **Get a code** and approve it with `cloudex tokens approve`; then sign in again by pasting a `read` token, and check that the read session has no New sandbox, Terminal or Desktop. **Safari:** all of it.
11. **Approve in the dashboard.** [Scripted: B31, B32.] `cloudex login --url <API URL>` on another config dir (`CLOUDEX_CONFIG_DIR=$(mktemp -d)`) prints a `/login?code=` link. Opening it in the signed-in browser shows where the sign-in comes from, the role starts at `user` even when the CLI asked for more, and Approve signs the CLI in. `next` on the login page goes only to dashboard routes [B33]. **Safari:** the approve page.
12. **Terminal.** [Scripted: B05–B12.] Run `vim` (`:wq` saves), `htop` (quits with `q`), and `tput cols` before and after resizing the window. Open a second tab and a root tab. Then reload the page: the same shells come back, with what was on them. Close a tab, and check with `tmux -L cloudex-dash ls` that its session is gone. **Safari:** all of it. **By eye:** colors, the cursor, and htop's drawing (the script only reads the text).
13. **Terminal across a deploy.** [A dropped socket is scripted: B13. The deploy itself is not.] With a terminal open, `cloudex deploy` again. Within some minutes (up to about 45) the tab shows "reconnecting…", then the same shell (`echo $$` gives the same number).
14. **Desktop.** [Scripted: starting it and showing its screen, B14; the size menu, B15.] **By eye, in every browser:** typing into Chrome on it works, **Match this window's size** follows the window, and text copied on the desktop shows under Clipboard. **Safari:** all of it.
15. **Agents.** [Scripted with the agents area or `VERIFY_AGENTS=1`: B16–B18.] Start an OpenCode agent with `anthropic/claude-haiku-4-5` and a task that runs a command. The command, the end of its output, the reply and "turn 1 done" show as text. Send a second message. Interrupt a long turn. **Safari:** one turn.
16. **Files.** [Scripted: B20–B24.] Upload two files by dropping them, download one, download a directory as tar.gz, delete a file. A file named like HTML (the script uses `<img src=x onerror=…>.html`; a name can't hold a `/`) shows as that text, and downloading it saves it rather than opening it. **Safari:** a real drag and drop from Finder (the script sends the drop event from the page).
17. **Sign out and revoke.** [Scripted: B35–B37, and B30 for a token.] Sign out: the next click goes to the login page. Sign in again, then revoke that session with `cloudex tokens revoke`: the page goes to the login page within a few seconds, and the server closes that session's open terminals within about a minute. Revoke the token a session was signed in with, and that session ends too. **Safari:** sign out and revoke.

## Setup paths

18. **Install.** In a new shell, `curl -fsSL https://cloudex.build/install | sh` installs the CLI. `cloudex version` then works, or the installer says how to put it on PATH for this shell.
19. **Update from the previous release.** Install the previous release into a temp directory (see the triage-feedback skill, step 4). Run it against the new deployment with `verify.sh setup lifecycle connect` (`CLOUDEX_BIN=<that binary>`). Then `cloudex update` moves it to the new release and offers `cloudex deploy`.
20. **Deploy twice.** `cloudex deploy` on the already-deployed test account keeps sandboxes, saves, the key, provider keys and the preview domain. Check `cloudex keys` and `cloudex list` afterwards.
21. **Feedback.** `cloudex feedback "test, please ignore"` in a terminal shows the report and asks. Answer no. Check that no key, URL or sandbox name appears in it.
