# Cloudex SDK for Python

Create, fork and drive [Cloudex](https://cloudex.build) sandboxes: agent
computers in your own Cloudflare account. Python 3.9+, standard library only.

```python
from concurrent.futures import ThreadPoolExecutor
from cloudex import Cloudex

cx = Cloudex()  # the deployment saved by `cloudex login`
box = cx.create(name="web")
box.exec("git clone https://github.com/you/app && cd app && pip install -e .")

# Ten copies of that disk, each trying something different.
forks = box.fork(count=10, name="try")
with ThreadPoolExecutor(10) as pool:
    results = list(pool.map(lambda f: f.exec(["pytest", "-x", "app"]), forks))

print(forks[0].host(8000))  # an HTTPS link to a port
for f in forks:
    f.delete()
box.stop()  # saves the disk; box.resume() picks up where it left off
```

## Agents

`box.prompt()` starts Claude Code, Codex or OpenCode on a task with no terminal. Each
message is one turn of the agent's own conversation, which lives on the
sandbox's disk.

```python
agent = box.prompt("make the tests pass", cwd="app")
for e in agent.events():
    # e["type"] is prompt, event (the agent's own JSON in e["event"]), output or end
    if e["type"] == "event":
        print(e["event"])
print(agent.last_message, agent.error)

agent.send("now update the docs")  # waits for a running turn
agent.send("stop, fix the build first", interrupt=True)
agent.wait()
```

`events()` waits up to 25 seconds per request, so give `Cloudex(timeout=)` more
than that if you set one.

## Commands

`box.exec()` waits for a command and returns its output, cut at 1 MiB.
`box.exec_stream()` gives the output as it comes, uncut, and ends with the exit
code. Leaving the loop early stops the command.

```python
for e in box.exec_stream("pytest", cwd="~/app"):
    if e.type == "exit":
        print("exit code", e.exit_code)
    else:
        print(e.data, end="")  # e.type is stdout or stderr
```

For servers and long jobs, start a command in the background. It keeps
running after your program ends, with its output kept in the sandbox, until it
ends, you kill it or the sandbox stops.

```python
cmd = box.start_command("make build", cwd="app")
for part in box.follow_command(cmd.id):
    print(part.data, end="")
print(part.command.state, part.command.exit_code)

server = box.start_command("python -m http.server 8000")
print(box.host(8000))
box.kill_command(server.id)  # TERM; or "INT", "HUP", "KILL"
```

`exec_stream()` hears from the API at least every 15 seconds and
`follow_command()` waits up to 25 seconds per request, so give
`Cloudex(timeout=)` more than that if you set one.

## Files

Paths are in the home directory unless they start with `/`. `root=True`
reads and writes as root.

```python
box.write_file("app/.env", "PORT=8000\n")
box.write_file("bin/tool", data, mode=0o755)  # bytes, str or a binary file
text = box.read_file("app/setup.cfg").decode()
listing = box.stat("app").entries
box.delete_file("app/build", recursive=True)

with open("app.tar.gz", "rb") as f:
    box.upload_archive("app", f)  # unpacks into ~/app
tgz = box.download_archive("app/dist")
```

## Connecting

`Cloudex(api_url, api_key)` takes the API URL that `cloudex deploy` printed and
the key that `cloudex status --show-key` shows. Without them it reads
`$CLOUDEX_API_URL` and `$CLOUDEX_API_KEY`, then the deployment the CLI saved.
The saved key is used only with the saved URL: for any other URL, pass `api_key`
or set `$CLOUDEX_API_KEY`. The key gives full control of your sandboxes.

## API

| Call | Does |
|---|---|
| `cx.create(name=, size=, idle_minutes=, env=, files=, repo=)` | Create a sandbox; returns once it's running (and the repo is cloned) |
| `cx.get(ref)`, `cx.list()`, `cx.health()` | Look up by id or name, list, check the deployment |
| `box.exec(cmd, root=, cwd=, timeout_sec=)` | Run shell code, or an argument list, and wait; check `exit_code` |
| `box.exec_stream(cmd, root=, cwd=, timeout_sec=)` | The same, as a generator of `ExecEvent`s that ends with `exit` |
| `box.start_command(cmd, root=, cwd=, timeout_sec=)` | Start a command in the background; returns a `Command` at once |
| `box.list_commands()`, `box.get_command(id)` | The sandbox's commands, newest first; one by id |
| `box.command_output(id, after=, wait=)`, `box.follow_command(id, after=)` | Read a command's output from a byte offset; follow it to the end |
| `box.kill_command(id, signal=)`, `box.forget_command(id)` | Signal a running command; delete a finished one's record and output |
| `box.read_file(path, root=)`, `box.write_file(path, data, mode=, root=)` | Download a file as bytes; upload one whole |
| `box.stat(path, root=)`, `box.delete_file(path, recursive=, root=)` | A path's details, with a directory's entries; delete one |
| `box.download_archive(path, root=)`, `box.upload_archive(path, data, root=)` | A directory as a tar.gz; unpack one into a directory |
| `box.fork(count=, name=, size=, idle_minutes=)` | Up to 10 sandboxes starting from a copy of the disk |
| `box.save(name)` | Keep a copy of the running sandbox's disk under a name until it's deleted; a taken name's save is replaced (needs R2) |
| `cx.start_from_save(name, count=, name=, size=, idle_minutes=)` | Up to 10 sandboxes starting from a named save |
| `cx.list_saves()`, `cx.get_save(name)`, `cx.delete_save(name)` | The named saves, newest first; one by name; delete one |
| `box.host(port)` | HTTPS link to a port on your preview domain; servers on localhost work too |
| `box.links()`, `box.revoke_links(port=None)` | The links that work; make one port's link, or all of them, stop working |
| `box.update(name=, idle_minutes=)` | Rename, or change the idle time |
| `box.desktop(size=)`, `box.stop_desktop()` | Start the desktop (Chrome, a terminal) and get a link to it |
| `box.stop()`, `box.resume(from_backup=, size=)`, `box.delete()` | Save and stop, start again (on another size with `size`), delete |
| `box.refresh()` | Fetch the latest state; `box.id`, `box.name`, `box.state` and `box.info` hold it |
| `box.settings()`, `box.update_settings(env=, files=)` | Environment variables and files the sandbox gets at every start; `None` removes one |
| `cx.set_key(provider, key)`, `cx.list_keys()`, `cx.delete_key(provider)` | AI provider keys sandboxes use without holding them (`"anthropic"`, `"openai"`, `"openrouter"`, `"google"`, `"groq"`, `"mistral"`, `"xai"`, `"deepseek"`) |
| `box.prompt(prompt, agent=, cwd=, model=)` | Start Claude Code (default), Codex or OpenCode on a task; returns an `Agent` at once |
| `box.agents()`, `box.agent(id)` | The sandbox's agents, newest first; one by id |
| `agent.events(after=, follow=)` | Iterate over its events (dicts); follows until the agent is idle |
| `agent.send(prompt, interrupt=)`, `agent.interrupt()`, `agent.wait()` | Continue, stop the running turn, wait until idle |

`cx.list()` returns a summary of each sandbox. `cx.get()` and `box.refresh()`
return every field, including `info["notice"]`, `info["backupAt"]` and the other
`backup*` keys, `forked_from`, `from_save` and `provider_keys`. On a sandbox from
`list()` those are empty until you call `box.refresh()`.

Errors from the API raise `CloudexError`, with `status` and the API's message.
When there was no answer (the API can't be reached, a request timed out, a
response broke off), `status` is 0 and the message starts with `can't reach` and
the API URL. The full API is described at `<your API URL>/v1/openapi.json`.

Fetching a preview link with `urllib` needs a `User-Agent` header: Cloudflare's
Browser Integrity Check, on by default for your domain, answers urllib's default
one with error 1010.
