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

## Connecting

`Cloudex(api_url, api_key)` takes the API URL and key that `cloudex deploy`
printed (`cloudex status --show-key` shows them again). Without them it reads
`$CLOUDEX_API_URL` and `$CLOUDEX_API_KEY`, then the deployment the CLI saved.
The key gives full control of your sandboxes.

## API

| Call | Does |
|---|---|
| `cx.create(name=, size=, idle_minutes=)` | Create a sandbox; returns once it's running |
| `cx.get(ref)`, `cx.list()`, `cx.health()` | Look up by id or name, list, check the deployment |
| `box.exec(cmd, root=, cwd=, timeout_sec=)` | Run shell code, or an argument list, and wait; check `exit_code` |
| `box.fork(count=, name=, size=, idle_minutes=)` | Up to 10 sandboxes starting from a copy of the disk |
| `box.host(port)` | HTTPS link to a port on your preview domain |
| `box.desktop(size=)`, `box.stop_desktop()` | Start the desktop (Chrome, a terminal) and get a link to it |
| `box.stop()`, `box.resume(from_backup=)`, `box.delete()` | Save and stop, start again, delete |
| `box.refresh()` | Fetch the latest state; `box.id`, `box.name`, `box.state` and `box.info` hold it |

Errors from the API raise `CloudexError`, with `status` and the API's message.
The full API is described at `<your API URL>/v1/openapi.json`.

Fetching a preview link with `urllib` needs a `User-Agent` header: Cloudflare's
Browser Integrity Check, on by default for your domain, answers urllib's default
one with error 1010.
