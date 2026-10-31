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
import { VERSION } from "./util";

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
import hashlib
import json
import os
import re
import stat
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

API = "http://cloudex.internal/v1/sandboxes/self"
COMMANDS = ("info", "host", "set", "stop", "feedback")
REPO = "lucataco/cloudex"
MARK = "<!-- cloudex feedback v1 -->"
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
  feedback "<message>"    report a bug or an idea to the Cloudex maintainers, as a public
                          GitHub issue. Without a terminal it only prints the report and
                          saves it; feedback --send <id> then sends exactly that

Add --json for JSON, the default when output isn't a terminal."""

opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))


def die(msg, code=1):
    print("error: " + msg, file=sys.stderr)
    sys.exit(code)


def call(method, path="", body=None, quiet=False, timeout=120):
    """The API's answer; on a failure, exits with its message, or with quiet, None."""
    data = None if body is None else json.dumps(body).encode()
    headers = {"content-type": "application/json"} if data else {}
    try:
        with open("/run/cloudex/self-token") as f:
            headers["x-cloudex-self"] = f.read().strip()
    except OSError:
        pass
    req = urllib.request.Request(API + path, data=data, method=method, headers=headers)
    try:
        with opener.open(req, timeout=timeout) as r:
            return json.load(r)
    except urllib.error.HTTPError as e:
        if quiet:
            return None
        try:
            msg = json.load(e).get("error")
        except Exception:
            msg = None
        die(msg or "the API answered %d" % e.code)
    except (urllib.error.URLError, OSError, ValueError) as e:
        if quiet:
            return None
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


SECRETS = [
    (re.compile(r"-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----[\s\S]*?(-----END [A-Z0-9 ]*PRIVATE KEY-----|$)"), "[private key]"),
    (re.compile(r"(?i)\b([a-z][a-z0-9+.-]*://)[^\s/@:]+:[^\s/@]+@"), r"\1[credentials]@"),
    (re.compile(r"(?i)(\d{1,5})-(cx-[a-z0-9]{6})-[a-z0-9]{10}\b"), r"\1-\2-[token]"),
    (re.compile(r"(?i)\b(?:[a-z0-9-]+\.)?[a-z0-9-]+\.workers\.dev\b"), "[subdomain].workers.dev"),
    (re.compile(r"[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(\.[A-Za-z0-9-]+)*\.[A-Za-z]{2,}"), "[email]"),
    (re.compile(r"\b(sk-ant-[A-Za-z0-9_-]{8,}|sk-[A-Za-z0-9_-]{16,}|gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|glpat-[A-Za-z0-9_-]{16,}|[sr]k_(?:live|test)_[A-Za-z0-9]{16,}|AKIA[0-9A-Z]{16}|xox[abprs]-[A-Za-z0-9-]{10,}|cxk_[0-9a-f]{8,})"), "[secret]"),
    (re.compile(r"""(?i)(^|[^A-Za-z0-9])([A-Za-z0-9_-]*?(?:api[_-]?key|key|token|secret|passw(?:or)?d|pwd|credentials?|auth)(?:[_-]?(?:key|token|secret|id))?["']?\s*[=:]\s*["']?)[^\s"',;}]{4,}"""), r"\1\2[secret]"),
    (re.compile(r"(?i)(--?(?:password|passwd|token|secret|api-key|key)[ =]+)\S{4,}"), r"\1[secret]"),
    (re.compile(r"(^|\s)(-p)([A-Za-z]\S{3,})"), r"\1\2[secret]"),
    (re.compile(r"(?i)\b(bearer|basic|token)(\s+)[A-Za-z0-9._~+/=-]{8,}"), r"\1\2[secret]"),
    (re.compile(r"\b[A-Za-z0-9_-]{32,}\b"), "[secret]"),
]
BASE64 = re.compile(r"[A-Za-z0-9+/]{32,}={0,2}")
IPV4 = re.compile(r"\b(?:25[0-5]|2[0-4]\d|1?\d?\d)(?:\.(?:25[0-5]|2[0-4]\d|1?\d?\d)){3}\b")
IPV6 = re.compile(r"(?i)\b(?:[0-9a-f]{0,4}:){2,7}[0-9a-f]{0,4}\b")
DRAFT = os.path.expanduser("~/.cloudex/feedback-draft.json")


def platform():
    try:
        with open("/etc/cloudex/platform.json") as f:
            return json.load(f)
    except (OSError, ValueError):
        return {}


def private_ip(ip):
    ip = ip.lower()
    if ip.startswith(("127.", "10.", "192.168.", "169.254.", "0.0.0.0", "fd", "fe80:")) or ip in ("::1", "::"):
        return True
    m = re.match(r"172\.(\d+)\.", ip)
    return bool(m and 16 <= int(m.group(1)) <= 31)


def keep_ip(m):
    ip = m.group(0)
    # Times (12:34:56) and file positions (main.py:12:3): all digits, at most three groups.
    if ":" in ip and "::" not in ip and re.fullmatch(r"[\d:]+", ip) and ip.count(":") <= 2:
        return ip
    return ip if private_ip(ip) else "[ip]"


def keep_base64(m):
    t = m.group(0)
    # A secret mixes cases and digits; a path is mostly lowercase.
    if re.search(r"\d", t) and re.search(r"[A-Z]", t) and re.search(r"[a-z]", t):
        return "[secret]"
    return t


def setting_values():
    """The sandbox's own variables and their values, from its settings."""
    try:
        with open("/etc/cloudex/env.sh") as f:
            names = re.findall(r"^export ([A-Za-z_][A-Za-z0-9_]*)=", f.read(), re.M)
        r = subprocess.run(["bash", "-c", "set -a; . /etc/cloudex/env.sh; env -0"], capture_output=True, timeout=10)
    except (OSError, subprocess.TimeoutExpired):
        return []
    env = dict(e.split("=", 1) for e in r.stdout.decode("utf-8", "replace").split("\0") if "=" in e)
    return [(n, env[n]) for n in names if n in env]


def whole(value):
    """A pattern for value in any case, not inside a longer word."""
    p = re.escape(value)
    if re.match(r"\w", value):
        p = r"\b" + p
    if re.search(r"\w$", value):
        p += r"\b"
    return re.compile(p, re.I)


def scrub(text, name=None):
    """What a public report mustn't hold: the sandbox's variables, its id and
    name, the preview domain, link tokens, IP addresses and anything that looks
    like a secret. Shapes first, while a link or a key is still whole."""
    for pattern, replacement in SECRETS:
        text = pattern.sub(replacement, text)
    text = BASE64.sub(keep_base64, text)
    values = []
    for var, value in setting_values():
        for v in {value, *value.splitlines()}:
            if len(v.strip()) >= 6:
                values.append((v.strip(), "[" + var + "]"))
    if platform().get("zone"):
        values.append((platform()["zone"], "[preview domain]"))
    own = os.environ.get("CLOUDEX_SANDBOX", "")
    if own:
        values.append((own, "[sandbox id]"))
    for value, label in sorted(values, key=lambda v: len(v[0]), reverse=True):
        text = whole(value).sub(lambda m: label, text)
    text = IPV6.sub(keep_ip, IPV4.sub(keep_ip, text))
    # A name that can't be an ordinary word: a dash, a digit, or 8 letters or more.
    if name and (len(name) >= 8 or re.search(r"[-0-9]", name)):
        text = whole(name).sub("[sandbox]", text)
    return text


def gh(stdin, *args):
    try:
        r = subprocess.run(["gh", *args], input=stdin, capture_output=True, text=True, timeout=60)
    except (OSError, subprocess.TimeoutExpired):
        return None
    return r.stdout if r.returncode == 0 else None


def gh_login():
    return (gh("", "api", "--hostname", "github.com", "user", "--jq", ".login") or "").strip()


def closed(body):
    """body with its code fence and details closed, and a note that it was cut."""
    fence = chr(96)  # a backtick, which this file can't hold
    opened = 0
    for line in body.split("\n"):
        t = line.strip()
        n = len(t) - len(t.lstrip(fence))
        if n < 3:
            continue
        if not opened:
            opened = n
        elif n >= opened and not t.strip(fence):
            opened = 0
    if opened:
        body += "\n" + fence * opened
    if body.count("<details>") > body.count("</details>"):
        body += "\n\n</details>"
    return body + "\n\n_(Cut to fit in a link; the rest is in the sandbox that sent it.)_"


def issue_link(title, body):
    """GitHub's new-issue page with the report filled in, cut to fit."""
    def build(b):
        return "https://github.com/%s/issues/new?%s" % (REPO, urllib.parse.urlencode({"title": title, "body": b}))
    link = build(body)
    if len(link) <= 7000:
        return link
    lines = body.split("\n")
    while len(lines) > 2:
        lines = lines[: len(lines) * 4 // 5]
        link = build(closed("\n".join(lines)))
        if len(link) <= 7000:
            return link
    text = "\n".join(lines)
    while text:
        text = text[: len(text) * 4 // 5]
        link = build(closed(text))
        if len(link) <= 7000:
            break
    return link


def how(report):
    return "gh, as @" + report["login"] if report.get("login") else "GitHub's new-issue page, for you to submit"


def send(report, as_json):
    """Sends a report the way it said it would go."""
    if report.get("login"):
        now = gh_login()
        if now != report["login"]:
            die("this report was to go with gh as @%s, but gh is %s now; run cloudex feedback again"
                % (report["login"], "signed in as @" + now if now else "not signed in"))
        out = gh(report["body"], "issue", "create", "--repo", "github.com/" + REPO, "--title", report["title"], "--body-file", "-")
        if not out:
            die("gh couldn't file it; file it from this link instead:\n" + issue_link(report["title"], report["body"]))
        issue = out.strip().splitlines()[-1]
        try:
            os.remove(DRAFT)
        except OSError:
            pass
        print(json.dumps({"sent": True, "url": issue}, indent=2) if as_json else "Filed %s\nThank you." % issue)
        return
    link = issue_link(report["title"], report["body"])
    print(json.dumps({"sent": False, "url": link}, indent=2) if as_json else "Submit the report on GitHub, where it's filled in:\n\n  " + link)


def feedback(argv, as_json):
    # Flags go before the message, so a -y inside it is just text.
    yes, send_id, rest = False, None, list(argv)
    while rest and rest[0].startswith("-") and rest[0] != "-":
        flag = rest.pop(0)
        if flag in ("-y", "--yes"):
            yes = True
        elif flag == "--send" and rest:
            send_id = rest.pop(0)
        elif flag.startswith("--send="):
            send_id = flag.split("=", 1)[1]
        elif flag in ("-h", "--help"):
            print('usage: cloudex feedback [-y] "message" | cloudex feedback --send ID')
            return
        else:
            die("unknown flag %s" % flag, 2)
    if len(rest) > 1:
        die('put the message in quotes: cloudex feedback "what happened"', 2)
    message = rest[0] if rest else None
    if send_id:
        if message:
            die("--send sends a saved report as it is: give no message with it", 2)
        try:
            with open(DRAFT) as f:
                report = json.load(f)
        except (OSError, ValueError):
            report = {}
        if report.get("id") != send_id:
            die("no saved report %s: run cloudex feedback again to make one" % send_id)
        if time.time() - report.get("at", 0) > 86400:
            die("report %s is more than a day old: run cloudex feedback again" % send_id)
        return send(report, as_json)
    piped = message == "-"
    if piped:
        message = sys.stdin.read(65536)
    me = call("GET", quiet=True, timeout=5) or {}
    message = scrub((message or "").strip(), me.get("name"))
    if not message:
        die('say what happened, for example: cloudex feedback "the preview link answers 502"', 2)
    title = message.splitlines()[0].strip()
    if len(title) > 72:
        title = title[:71] + "…"
    os_name = "Linux"
    try:
        with open("/etc/os-release") as f:
            os_name = dict(l.rstrip().split("=", 1) for l in f if "=" in l).get("PRETTY_NAME", os_name).strip('"')
    except OSError:
        pass
    person = sys.stdin.isatty() and sys.stderr.isatty() and not piped
    body = "\n".join([
        MARK, message, "", "**Setup**",
        "- Inside a sandbox (%s), API %s" % (me.get("size", "size unknown"), platform().get("version", "unknown")),
        "- " + os_name,
        "- Sent from " + ("a terminal" if person and sys.stdout.isatty() else "a script or an agent"),
        "", "_Sent with cloudex feedback from inside a sandbox, which removed keys, variables and addresses first._", "",
    ])
    report = {"id": hashlib.sha256((title + "\0" + body).encode()).hexdigest()[:8], "title": title, "body": body,
              "at": time.time(), "login": gh_login()}
    if yes:
        print("Sending this report, with %s:\n\n%s\n\n%s" % (how(report), title, body), file=sys.stderr)
        return send(report, as_json)
    if not person:
        os.makedirs(os.path.dirname(DRAFT), exist_ok=True)
        with open(DRAFT, "w") as f:
            json.dump(report, f)
        command = "cloudex feedback --send " + report["id"]
        if as_json:
            print(json.dumps({"sent": False, "id": report["id"], "title": title, "body": body,
                              "url": issue_link(title, body), "via": how(report), "send": command}, indent=2))
        else:
            print(title + "\n\n" + body)
        print("Not sent. Show this report to the person you work for. With their OK, send it as a public\n"
              "issue on github.com/%s, with %s, by running: %s" % (REPO, how(report), command), file=sys.stderr)
        return
    print("Title: " + title + "\n\n" + body, file=sys.stderr)
    print("This becomes a public issue on github.com/%s, filed with %s. Send it? [y/N] " % (REPO, how(report)),
          end="", file=sys.stderr, flush=True)
    if input().strip().lower() not in ("y", "yes"):
        print("Not sent.", file=sys.stderr)
        return
    send(report, as_json)


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
    if argv[0] == "feedback":
        feedback(argv[1:], as_json or not sys.stdout.isatty())
        return 0
    if argv[0] not in COMMANDS:
        die("%s isn't available inside a sandbox: here cloudex only manages this sandbox (%s). "
            "Use cloudex on your own computer for the rest." % (argv[0], ", ".join(COMMANDS)), 2)
    p = argparse.ArgumentParser(prog="cloudex " + argv[0], add_help=True, allow_abbrev=False)
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
    "- If `cloudex` or the sandbox fails in a way that looks like a bug in Cloudex, tell your user. With their OK, run `cloudex feedback \"what happened\"`: it sends nothing, and prints a report and a `cloudex feedback --send <id>` command. Show them the report, and run that command only if they agree; it files the report as a public GitHub issue.",
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
install -d -m 755 /usr/local/lib/cloudex /etc/claude-code /etc/cloudex
jq -n --arg v "$(jq -r .version <<<"$in")" --arg z "$zone" '{version: $v, zone: $z}' >/etc/cloudex/platform.json
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

export const PLATFORM_FILES = JSON.stringify({
  relay: RELAY,
  cli: IN_SANDBOX_CLI,
  note: agentNote(true),
  noteNoPreviews: agentNote(false),
  version: VERSION,
});

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
