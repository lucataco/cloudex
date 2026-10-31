// Cloudex's own tools inside every sandbox, put there by the API at each start
// (PLATFORM_SCRIPT) rather than baked into the image, so that a sandbox resumed
// from a save made on an older image gets them too:
// - relay.py, which lets preview links reach servers that listen only on
//   localhost (Sandbox.fetch starts it when a preview finds one);
// - /usr/local/bin/cloudex, which manages the sandbox from inside it through
//   http://cloudex.internal/v1/sandboxes/self (egress.ts, self.ts), with no key;
// - a note that tells Claude Code and Codex about both;
// - Vite's allowed hosts, so its dev server answers on the preview domain;
// - ~/.cloudex/start, the user's own commands for every start (START_SCRIPT).

// Python, run as root. Preview links reach the sandbox at its own address
// (10.0.0.1), so a server listening only on 127.0.0.1 or ::1, as Vite,
// Django, Flask, Rails and uvicorn do by default, doesn't answer them. When a
// preview finds that, the API runs "relay.py ensure PORT ADDRESS": if something
// listens on localhost at PORT, a relay listens on a spare port of its own at
// ADDRESS, prints that port, and passes raw TCP from it to PORT, so HTTP and
// WebSockets (hot reload) work. Leaving PORT itself alone means a server can
// restart on it at any moment, on any address. The relay stops when nothing
// listens on localhost at PORT any more.
const RELAY = String.raw`#!/usr/bin/python3 -I
# Cloudex's localhost relay, rewritten at each start. See guest.ts in the API.
#   relay.py ensure PORT ADDRESS  prints the port that answers for PORT at ADDRESS
#                                 (PORT itself, or a relay's); exit 3: nothing listens on PORT
#   relay.py serve PORT ADDRESS   the relay itself
import asyncio
import fcntl
import os
import socket
import subprocess
import sys
import time

RUN = "/run/cloudex/relay"
LOG = "/var/log/cloudex-relay.log"
WILDCARD = {"0.0.0.0", "::"}


def log(msg):
    with open(LOG, "a") as f:
        f.write("[relay] %s %s\n" % (time.strftime("%H:%M:%S"), msg))


def decode(hexaddr):
    raw = bytes.fromhex(hexaddr)
    if len(raw) == 4:
        return socket.inet_ntop(socket.AF_INET, raw[::-1])
    # /proc/net/tcp6 prints four 32-bit words, each in little-endian order.
    return socket.inet_ntop(socket.AF_INET6, b"".join(raw[i : i + 4][::-1] for i in range(0, 16, 4)))


def listening(port):
    """The addresses with a TCP socket listening on port."""
    found = set()
    for path in ("/proc/net/tcp", "/proc/net/tcp6"):
        try:
            with open(path) as f:
                lines = f.read().splitlines()[1:]
        except OSError:
            continue
        for line in lines:
            fields = line.split()
            if len(fields) > 3 and fields[3] == "0A":
                addr, _, hexport = fields[1].partition(":")
                if int(hexport, 16) == port:
                    found.add(decode(addr))
    return found


def on_localhost(port):
    return any(a.startswith("127.") or a in ("::1", "::ffff:127.0.0.1") for a in listening(port))


def running(port):
    """The port of the relay for port, while one runs: it holds the lock."""
    try:
        with open("%s/%d.port" % (RUN, port)) as f:
            through = int(f.read())
        with open("%s/%d.lock" % (RUN, port), "a") as lock:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        return None  # the lock was free, so no relay holds it
    except BlockingIOError:
        return through
    except (OSError, ValueError):
        return None


def ensure(port, addr):
    addrs = listening(port)
    if addr in addrs or addrs & WILDCARD:
        print(port)
        return 0
    if not on_localhost(port):
        return 3
    through = running(port)
    if not through:
        os.makedirs(RUN, exist_ok=True)
        with open(LOG, "a") as errors:
            subprocess.Popen(
                [sys.executable, "-I", os.path.abspath(__file__), "serve", str(port), addr],
                stdin=subprocess.DEVNULL,
                stdout=subprocess.DEVNULL,
                stderr=errors,
                start_new_session=True,
            )
        for _ in range(60):
            through = running(port)
            if through and addr in listening(through):
                break
            time.sleep(0.05)
        else:
            return 1
    print(through)
    return 0


async def pipe(reader, writer):
    try:
        while True:
            data = await reader.read(65536)
            if not data:
                break
            writer.write(data)
            await writer.drain()
        if writer.can_write_eof():
            writer.write_eof()
    except (ConnectionError, OSError):
        writer.close()


def global_v6():
    """One of the sandbox's own global IPv6 addresses, if it has one."""
    try:
        with open("/proc/net/if_inet6") as f:
            for line in f:
                hexaddr, _, _, scope, _, name = line.split()
                if scope == "00" and name != "lo":
                    return socket.inet_ntop(socket.AF_INET6, bytes.fromhex(hexaddr))
    except (OSError, ValueError):
        pass
    return None


async def handle(client_r, client_w, port, addr, addr6):
    # From the sandbox's own addresses, never a loopback one: apps trust a peer
    # on localhost (Rails' web console, Django's INTERNAL_IPS), and visitors
    # through a preview link aren't that.
    upstream = None
    for host, local in (("127.0.0.1", addr), ("::1", addr6)):
        if not local:
            continue
        try:
            upstream = await asyncio.open_connection(host, port, local_addr=(local, 0))
            break
        except OSError:
            pass
    if upstream is None:
        client_w.close()
        return
    up_r, up_w = upstream
    await asyncio.gather(pipe(client_r, up_w), pipe(up_r, client_w))
    client_w.close()
    up_w.close()


async def serve(port, addr):
    os.makedirs(RUN, exist_ok=True)
    lock = open("%s/%d.lock" % (RUN, port), "a")
    try:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except OSError:
        return 0  # another relay has this port
    sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    sock.bind((addr, 0))
    sock.listen(128)
    through = sock.getsockname()[1]
    addr6 = global_v6()
    server = await asyncio.start_server(lambda r, w: handle(r, w, port, addr, addr6), sock=sock)
    path = "%s/%d.port" % (RUN, port)
    with open(path + ".new", "w") as f:
        f.write(str(through))
    os.replace(path + ".new", path)
    log("port %d: relaying from %s:%d" % (port, addr, through))
    while on_localhost(port):
        await asyncio.sleep(0.5)
    server.close()
    os.unlink(path)
    log("port %d: nothing listens on localhost any more; relay stopped" % port)
    return 0


if __name__ == "__main__":
    if len(sys.argv) != 4 or sys.argv[1] not in ("ensure", "serve"):
        sys.exit("usage: relay.py ensure|serve PORT ADDRESS")
    port, addr = int(sys.argv[2]), sys.argv[3]
    if sys.argv[1] == "ensure":
        sys.exit(ensure(port, addr))
    sys.exit(asyncio.run(serve(port, addr)))
`;

// The cloudex command inside a sandbox, in Python (every image has it). It
// manages only the sandbox it runs in. Output matches the CLI's: JSON when
// stdout isn't a terminal or with --json.
const IN_SANDBOX_CLI = String.raw`#!/usr/bin/python3 -I
# cloudex-in-sandbox: manage this sandbox from inside it. Cloudex rewrites this
# file at each start; it replaces only a file carrying this mark.
import argparse
import json
import re
import sys
import time
import urllib.error
import urllib.request

API = "http://cloudex.internal/v1/sandboxes/self"
COMMANDS = ("info", "host", "set", "stop")
HELP = """Manage this sandbox from inside it. To create, fork or delete sandboxes, use
cloudex on your own computer.

Commands:
  info                    show this sandbox
  host <port>             print an HTTPS link to a port, on the preview domain
  host <port> --revoke    make the port's link stop working (asking again gives a new one)
  host --list             list the links made since Cloudex v0.1.14 that still work
  host --revoke-all       make every link to this sandbox stop working, the desktop's too
  set --idle <duration>   stop after this long without activity from now, such as 30m or 2h
                          (0 leaves only the platform's 6-hour limit)
  set --name <name>       rename the sandbox
  stop                    save the disk and stop; everything running ends

Add --json for JSON, the default when output isn't a terminal."""

opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))


def die(msg, code=1):
    print("error: " + msg, file=sys.stderr)
    sys.exit(code)


def call(method, path="", body=None):
    data = None if body is None else json.dumps(body).encode()
    headers = {"content-type": "application/json"} if data else {}
    try:
        with open("/run/cloudex/self-token") as f:
            headers["x-cloudex-self"] = f.read().strip()
    except OSError:
        pass
    req = urllib.request.Request(API + path, data=data, method=method, headers=headers)
    try:
        with opener.open(req, timeout=120) as r:
            return json.load(r)
    except urllib.error.HTTPError as e:
        try:
            msg = json.load(e).get("error")
        except Exception:
            msg = None
        die(msg or "the API answered %d" % e.code)
    except (urllib.error.URLError, OSError) as e:
        die("can't reach the Cloudex API from this sandbox: %s" % getattr(e, "reason", e))


def minutes(text):
    m = re.fullmatch(r"0|(?:(\d+)h)?(?:(\d+)m)?(?:(\d+)s)?", text)
    if not text or not m:
        die("bad duration %r: use one such as 30m, 2h or 1h30m" % text, 2)
    h, mi, s = (int(x or 0) for x in m.groups())
    seconds = h * 3600 + mi * 60 + s
    if 0 < seconds < 60:
        die("--idle must be 0 or at least 1m", 2)
    return seconds // 60


def ago(ms):
    s = max(0, int(time.time() - ms / 1000))
    for unit, n in (("day", 86400), ("hour", 3600), ("minute", 60)):
        if s >= n:
            k = s // n
            return "%d %s%s ago" % (k, unit, "" if k == 1 else "s")
    return "just now"


def idle_text(n):
    return "after %d minutes" % n if n else "off (6-hour platform limit)"


def print_sandbox(s):
    rows = [("ID", s["id"])]
    if s.get("name"):
        rows.append(("Name", s["name"]))
    rows += [("State", s["state"]), ("Size", s["size"]), ("Idle stop", idle_text(s["idleMinutes"]))]
    if s.get("startedAt") and s["state"] == "running":
        rows.append(("Started", ago(s["startedAt"])))
    if s.get("forkedFrom"):
        rows.append(("Forked from", s["forkedFrom"]))
    for k, v in rows:
        print("%-12s%s" % (k, v))


def main(argv):
    as_json = "--json" in argv
    argv = [a for a in argv if a != "--json"]
    if not argv or argv[0] in ("-h", "--help", "help"):
        print(HELP)
        return 0
    if argv[0] not in COMMANDS:
        die("%s isn't available inside a sandbox: here cloudex only manages this sandbox (%s). "
            "Use cloudex on your own computer for the rest." % (argv[0], ", ".join(COMMANDS)), 2)
    p = argparse.ArgumentParser(prog="cloudex " + argv[0], add_help=True)
    if argv[0] == "host":
        p.add_argument("port", nargs="?", type=int)
        p.add_argument("--revoke", action="store_true")
        p.add_argument("--list", action="store_true")
        p.add_argument("--revoke-all", action="store_true")
    elif argv[0] == "set":
        p.add_argument("--idle")
        p.add_argument("--name")
    a = p.parse_args(argv[1:])
    as_json = as_json or not sys.stdout.isatty()
    if argv[0] == "host":
        if a.list + a.revoke + a.revoke_all > 1:
            die("use one of --list, --revoke and --revoke-all", 2)
        if (a.list or a.revoke_all) and a.port is not None:
            die("--list and --revoke-all take no port", 2)
    out = lambda v: print(json.dumps(v, indent=2))

    if argv[0] == "info":
        s = call("GET")
        out(s) if as_json else print_sandbox(s)
    elif argv[0] == "stop":
        s = call("POST", "/stop")
        if as_json:
            out(s)
        else:
            print("Saving and stopping this sandbox in a moment; everything running in it ends.")
            print("Resume it from your computer with: cloudex resume %s" % (s.get("name") or s["id"]))
    elif argv[0] == "set":
        body = {}
        if a.idle is not None:
            body["idleMinutes"] = minutes(a.idle)
        if a.name is not None:
            body["name"] = a.name
        if not body:
            die("give --idle or --name", 2)
        s = call("PATCH", "", body)
        out(s) if as_json else print_sandbox(s)
    elif a.list:
        links = call("GET", "/hosts")
        if as_json:
            out(links)
        elif not links:
            print("No links made since Cloudex v0.1.14. Make one with: cloudex host <port>")
        else:
            for link in links:
                print("%-6d%s" % (link["port"], link["url"]))
    elif a.revoke_all:
        r = call("DELETE", "/hosts")
        out(r) if as_json else print("Every link to this sandbox has stopped working, the desktop's too.")
    elif a.port is None or not 1 <= a.port <= 65535:
        die("give a port from 1 to 65535, such as: cloudex host 3000", 2)
    elif a.revoke:
        r = call("DELETE", "/hosts/%d" % a.port)
        out(r) if as_json else print("The link to port %d has stopped working. Run cloudex host %d for a new one." % (a.port, a.port))
    else:
        r = call("POST", "/hosts", {"port": a.port})
        out(r) if as_json else print(r["url"])
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
`;

// What Claude Code (as managed memory) and Codex (in ~/.codex/AGENTS.md) read
// at the start of every conversation in a sandbox. @ZONE@ is the preview domain.
const agentNote = (previews: boolean) =>
  [
    "# Cloudex sandbox",
    "",
    "You are working in a Cloudex sandbox: an Ubuntu machine in your user's Cloudflare account, with passwordless sudo. The `cloudex` command manages it from inside:",
    "",
    ...(previews
      ? [
          "- `cloudex host <port>` prints an HTTPS link to a server in the sandbox, which your user can open from anywhere while the sandbox runs. Servers that listen only on localhost work. When you start a server for your user to look at, give them its link. Vite accepts the link's host name already; for dev servers that check it, such as Django (ALLOWED_HOSTS) or Rails (config.hosts), allow `.@ZONE@`.",
        ]
      : ["- This deployment has no preview domain, so `cloudex host` can't make links to servers in the sandbox."]),
    "- `cloudex info` shows the sandbox. `cloudex set --idle 2h` keeps it running longer without activity, and `cloudex stop` saves its disk and stops it, ending everything running in it.",
    "- Docker works, with `--network host` only.",
    "- When the sandbox stops, its whole disk is saved, but programs don't start again by themselves on resume. Commands in `~/.cloudex/start` run at every start.",
    "",
  ].join("\n");

// Run as root at each start, after the settings, and again when the settings
// change. $1 is the preview domain or '', $2 the run's token for the API the
// sandbox has for itself, or ''. stdin is the JSON of PLATFORM_FILES.
export const PLATFORM_SCRIPT = String.raw`
set -euo pipefail
in=$(cat)
zone=$1
# Readable by the cloudex user and root only; /run goes with the run.
install -d -m 755 /run/cloudex
if [ -n "$2" ]; then
  (umask 027 && printf '%s
' "$2" >/run/cloudex/self-token.new)
  chown root:cloudex /run/cloudex/self-token.new && mv /run/cloudex/self-token.new /run/cloudex/self-token
else
  rm -f /run/cloudex/self-token
fi
put() { # put PATH KEY MODE: write a file from the input, replacing it whole
  jq -j --arg k "$2" '.[$k]' <<<"$in" >"$1.new" && chmod "$3" "$1.new" && mv "$1.new" "$1"
}
install -d -m 755 /usr/local/lib/cloudex /etc/claude-code
put /usr/local/lib/cloudex/relay.py relay 755
# A cloudex installed in the sandbox by its user stays; ours carries the mark
# at the top. (The real CLI holds the mark too, deep inside, in its copy of the API.)
cli=/usr/local/bin/cloudex
if [ ! -e "$cli" ]; then
  put "$cli" cli 755
else
  case $(head -c 200 "$cli" | tr -d '\000') in *"# cloudex-in-sandbox:"*) put "$cli" cli 755 ;; esac
fi
note=$(jq -j --arg k "$([ -n "$zone" ] && echo note || echo noteNoPreviews)" '.[$k]' <<<"$in" | sed "s/@ZONE@/$zone/g")
printf '%s' "$note" >/etc/claude-code/CLAUDE.md.new && chmod 644 /etc/claude-code/CLAUDE.md.new && mv /etc/claude-code/CLAUDE.md.new /etc/claude-code/CLAUDE.md

# Codex's notes for every project; only the block between the marks is ours.
runuser -u cloudex -- env HOME=/home/cloudex NOTE="$note" bash -c '
  f=$HOME/.codex/AGENTS.md
  begin="<!-- Cloudex: about this sandbox, rewritten at each start -->" end="<!-- Cloudex: end -->"
  mkdir -p "$HOME/.codex" && touch "$f"
  sed -i --follow-symlinks "/^$begin\$/,/^$end\$/d" "$f"
  { [ -n "$(tail -c1 "$f")" ] && echo; echo "$begin"; printf "%s\n" "$NOTE"; echo "$end"; } >>"$f"' || true

# cloudex prompt runs Claude Code with --dangerously-skip-permissions, the
# sandbox being the boundary, so its warning about that is accepted ahead of
# time. After the settings, which may hold a ~/.claude/settings.json of their
# own; a setting already there, true or false, stays.
# Written in place, so that a symlink or the file's mode stays.
runuser -u cloudex -- env HOME=/home/cloudex bash -c '
  f=$HOME/.claude/settings.json
  mkdir -p "$HOME/.claude"
  [ -s "$f" ] || echo "{}" >"$f"
  jq -e "has(\"skipDangerousModePermissionPrompt\")" "$f" >/dev/null 2>&1 && exit 0
  umask 077
  t=$(mktemp "$f.XXXXXX")
  jq ".skipDangerousModePermissionPrompt = true" "$f" >"$t" && [ -s "$t" ] && cat "$t" >"$f"
  rm -f "$t"' || true

# Vite's dev server refuses requests for host names it doesn't know; this adds
# the preview domain. For login shells, and for ssh commands through sshd.
profile=/etc/profile.d/cloudex-platform.sh
begin="# Cloudex: the preview domain, rewritten at each start" end="# Cloudex: end of the preview domain"
touch /etc/environment
sed -i "/^$begin\$/,/^$end\$/d" /etc/environment
[ -z "$(tail -c1 /etc/environment)" ] || echo >>/etc/environment
if [ -n "$zone" ]; then
  printf '# Written by Cloudex at each start.\nexport __VITE_ADDITIONAL_SERVER_ALLOWED_HOSTS=.%s\n' "$zone" >"$profile"
  printf '%s\n__VITE_ADDITIONAL_SERVER_ALLOWED_HOSTS=.%s\n%s\n' "$begin" "$zone" "$end" >>/etc/environment
else
  rm -f "$profile"
fi
`;

export const PLATFORM_FILES = JSON.stringify({ relay: RELAY, cli: IN_SANDBOX_CLI, note: agentNote(true), noteNoPreviews: agentNote(false) });

// $1 the port, $2 the address previews reach. Prints the port that answers for
// it there: the port itself, or its relay's. Exit 3: nothing listens on it at all.
export const RELAY_SCRIPT = String.raw`
r=/usr/local/lib/cloudex/relay.py
[ -f "$r" ] || exit 127
exec /usr/bin/python3 -I "$r" ensure "$1" "$2"`;
export const RELAY_NOTHING = 3;

// Run as the cloudex user from a login shell after each start (and after the
// first clone of a new sandbox): starts ~/.cloudex/start, if there is one, in
// the background, its output in ~/.cloudex/start.log.
export const START_SCRIPT = String.raw`
f=$HOME/.cloudex/start
[ -f "$f" ] || exit 0
log=$HOME/.cloudex/start.log
echo "[cloudex] $(date -u '+%F %T') UTC: running ~/.cloudex/start" >"$log"
cd "$HOME"
setsid -f bash -l "$f" >>"$log" 2>&1 </dev/null
`;

// Changes whenever the tools do, so running sandboxes get the new ones.
function hash(s: string) {
  let h = 0x811c9dc5;
  for (let i = 0; i < s.length; i++) h = Math.imul(h ^ s.charCodeAt(i), 0x01000193);
  return (h >>> 0).toString(36);
}
export const PLATFORM_VERSION = hash(PLATFORM_SCRIPT + PLATFORM_FILES);
