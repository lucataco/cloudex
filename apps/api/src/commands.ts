// Background commands (Command in @cloudex/schema): started through the API,
// then left running in the sandbox, with their output kept on its disk. A
// script in the guest (COMMAND_SCRIPT, run as root) keeps everything in
// /var/lib/cloudex/commands/<id>:
//
//   meta.json   id, cmd, root, cwd, timeoutSec, startedAt, boot (the boot id)
//   output      standard output and standard error, as written
//   pid         the command's process group, while the runner waits for it
//   exit.json   how it ended: exitCode or signal, timedOut, endedAt
//
// A runner (the same script, op run, in its own session) starts the command
// in a new process group as the cloudex user or root, waits for it and writes
// exit.json. A command with no exit.json whose runner is gone, or which started
// in an earlier boot, was cut off by a stop: it's lost. Forks keep the
// directory, as they keep the disk; named saves leave it out (saves.ts).
//
// While a command writes output, the runner counts it as activity for idle
// stop, as agents do; one that prints nothing for 10 minutes lets the sandbox
// stop when nothing else keeps it up.

export const isCommandId = (s: string) => /^cm-[a-z0-9]{8}$/.test(s);

// Exit codes the script uses for errors meant for the caller.
export const COMMAND_NOT_FOUND = 3;
export const COMMAND_BAD_REQUEST = 4;
export const COMMAND_CONFLICT = 5;

export const COMMANDS_DIR = "/var/lib/cloudex/commands";

// $1 is the script. Installs it where the runner can run it from when it
// changed, then runs it with the remaining arguments.
export const COMMAND_BOOT = String.raw`
d=/var/lib/cloudex/commands
f=$d/.command.py
mkdir -p "$d"
chmod 755 /var/lib/cloudex "$d"
printf %s "$1" | cmp -s - "$f" 2>/dev/null || { printf %s "$1" >"$f.$$" && mv "$f.$$" "$f"; }
shift
exec python3 -I "$f" "$@"
`;

// Operations print JSON on stdout:
//   start <id> <root 0|1> <cwd or ''> <timeoutSec>   the command on stdin
//   list | get <id> | forget <id>
//   output <id> <after> <seconds to wait for more>
//   kill <id> <TERM|INT|HUP|KILL>
export const COMMAND_SCRIPT = String.raw`
import codecs, json, os, pwd, signal, subprocess, sys, threading, time, urllib.request

ROOT = "/var/lib/cloudex/commands"
USER = "cloudex"
MAX_READ = 1 << 20


def now():
    return int(time.time() * 1000)


def fail(code, message):
    print(message, file=sys.stderr)
    sys.exit(code)


def boot_id():
    with open("/proc/sys/kernel/random/boot_id") as f:
        return f.read().strip()


def path(cid, name=""):
    return os.path.join(ROOT, cid, name)


def load(cid):
    try:
        with open(path(cid, "meta.json")) as f:
            return json.load(f)
    except FileNotFoundError:
        fail(3, "no command " + cid)


def write_json(p, value):
    with open(p + ".new", "w") as f:
        json.dump(value, f)
    os.replace(p + ".new", p)


def alive(pid):
    try:
        os.kill(pid, 0)
        return True
    except ProcessLookupError:
        return False
    except PermissionError:
        return True


def read_int(p):
    try:
        with open(p) as f:
            return int(f.read().strip())
    except (FileNotFoundError, ValueError):
        return None


def show(meta):
    cid = meta["id"]
    try:
        with open(path(cid, "exit.json")) as f:
            end = json.load(f)
    except FileNotFoundError:
        end = None
    if end is None:
        runner = read_int(path(cid, "runner"))
        if meta["boot"] == boot_id() and (runner is None or alive(runner)):
            state = "running"
        elif os.path.exists(path(cid, "exit.json")):
            return show(meta)  # it ended just now
        else:
            state = "lost"
    else:
        state = "killed" if end.get("signal") else "exited"
    try:
        size = os.path.getsize(path(cid, "output"))
    except FileNotFoundError:
        size = 0
    end = end or {}
    return {
        "id": cid,
        "cmd": meta["cmd"],
        "root": meta["root"],
        "cwd": meta["cwd"],
        "state": state,
        "exitCode": end.get("exitCode"),
        "signal": end.get("signal"),
        "timedOut": bool(end.get("timedOut")),
        "timeoutSec": meta["timeoutSec"],
        "startedAt": meta["startedAt"],
        "endedAt": end.get("endedAt"),
        "outputBytes": size,
    }


def start(cid, root, cwd, timeout):
    cmd = sys.stdin.read()
    if not cmd.strip():
        fail(4, "the command is empty")
    user = pwd.getpwnam("root" if root == "1" else USER)
    if cwd == "~" or cwd.startswith("~/"):
        cwd = cwd[2:]
    cwd = os.path.join(user.pw_dir, cwd or ".")
    cwd = os.path.normpath(cwd)
    if not os.path.isdir(cwd):
        fail(4, "there's no directory " + cwd + " in the sandbox")
    d = path(cid)
    os.makedirs(d, mode=0o755)
    with open(os.path.join(d, "cmd"), "w") as f:
        f.write(cmd)
    meta = {"id": cid, "cmd": cmd, "root": root == "1", "cwd": cwd, "timeoutSec": int(timeout),
            "startedAt": now(), "boot": boot_id()}
    write_json(path(cid, "meta.json"), meta)
    open(path(cid, "output"), "w").close()
    runner = subprocess.Popen([sys.executable, "-I", os.path.abspath(__file__), "run", cid],
                              stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL,
                              stderr=open(path(cid, "runner.log"), "w"), start_new_session=True, close_fds=True)
    with open(path(cid, "runner"), "w") as f:
        f.write(str(runner.pid))
    # Answer once the command is running, or has already ended.
    for _ in range(100):
        if os.path.exists(path(cid, "pid")) or os.path.exists(path(cid, "exit.json")):
            break
        time.sleep(0.05)
    print(json.dumps(show(meta)))


def run(cid):
    meta = load(cid)
    root = meta["root"]
    user = pwd.getpwnam("root" if root else USER)
    env = {k: v for k, v in os.environ.items() if k not in ("HOME", "USER", "LOGNAME", "MAIL", "OLDPWD", "PWD")}
    env.update(HOME=user.pw_dir, USER=user.pw_name, LOGNAME=user.pw_name, LANG="C.UTF-8", TERM="dumb")
    argv = ["bash", "-lc", meta["cmd"]]
    if not root:
        # As exec does (sandbox.ts): the user's groups, and no capabilities left.
        argv = ["setpriv", "--reuid=%d" % user.pw_uid, "--regid=%d" % user.pw_gid, "--init-groups",
                "--inh-caps=-all", "--"] + argv
    out = open(path(cid, "output"), "ab")
    timed_out = threading.Event()
    try:
        p = subprocess.Popen(argv, cwd=meta["cwd"], env=env, stdin=subprocess.DEVNULL,
                             stdout=out, stderr=subprocess.STDOUT, start_new_session=True, close_fds=True)
    except OSError as e:
        out.write(("cloudex: couldn't start the command: %s\n" % e).encode())
        out.close()
        write_json(path(cid, "exit.json"), {"exitCode": 127, "signal": None, "timedOut": False, "endedAt": now()})
        return
    out.close()
    with open(path(cid, "pid"), "w") as f:
        f.write(str(p.pid))

    def killer():
        timed_out.set()
        try:
            os.killpg(p.pid, signal.SIGTERM)
            time.sleep(10)
            os.killpg(p.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass

    if meta["timeoutSec"] > 0:
        t = threading.Timer(meta["timeoutSec"], killer)
        t.daemon = True
        t.start()

    def pinger():
        while True:
            time.sleep(60)
            try:
                if time.time() - os.path.getmtime(path(cid, "output")) < 600:
                    urllib.request.urlopen(urllib.request.Request("http://cloudex.internal/activity", method="POST"), timeout=10).read()
            except Exception:
                pass

    threading.Thread(target=pinger, daemon=True).start()
    _, status = os.waitpid(p.pid, 0)
    end = {"exitCode": None, "signal": None, "timedOut": timed_out.is_set(), "endedAt": now()}
    if os.WIFSIGNALED(status):
        n = os.WTERMSIG(status)
        try:
            end["signal"] = signal.Signals(n).name[3:]
        except ValueError:
            end["signal"] = str(n)  # a real-time signal
    else:
        end["exitCode"] = os.WEXITSTATUS(status)
    write_json(path(cid, "exit.json"), end)
    try:
        os.unlink(path(cid, "pid"))
    except FileNotFoundError:
        pass


def all_commands():
    try:
        names = os.listdir(ROOT)
    except FileNotFoundError:
        return []
    out = []
    for n in names:
        try:
            with open(path(n, "meta.json")) as f:
                out.append(show(json.load(f)))
        except (FileNotFoundError, NotADirectoryError, ValueError):
            pass
    return sorted(out, key=lambda c: -c["startedAt"])


def output(cid, after, wait):
    meta = load(cid)
    deadline = time.time() + wait
    while True:
        c = show(meta)
        try:
            size = os.path.getsize(path(cid, "output"))
        except FileNotFoundError:
            size = 0
        after = min(after, size)
        with open(path(cid, "output"), "rb") as f:
            f.seek(after)
            data = f.read(MAX_READ)
        final = c["state"] != "running" and after + len(data) >= size
        dec = codecs.getincrementaldecoder("utf-8")("replace")
        text = dec.decode(data, final)
        pending = len(dec.getstate()[0])
        if text or c["state"] != "running" or time.time() >= deadline:
            break
        time.sleep(0.25)
    print(json.dumps({"data": text, "offset": after, "next": after + len(data) - pending, "command": c}, ensure_ascii=False))


def kill(cid, sig):
    meta = load(cid)
    c = show(meta)
    if c["state"] != "running":
        print(json.dumps(c))
        return
    pid = read_int(path(cid, "pid"))
    if pid is not None:
        try:
            os.killpg(pid, getattr(signal, "SIG" + sig))
        except ProcessLookupError:
            pass
    # Give it a moment to end, so the answer shows it.
    for _ in range(50):
        c = show(meta)
        if c["state"] != "running":
            break
        time.sleep(0.1)
    print(json.dumps(c))


def forget(cid):
    meta = load(cid)
    c = show(meta)
    if c["state"] == "running":
        fail(5, "the command " + cid + " is still running; kill it first")
    import shutil
    shutil.rmtree(path(cid))
    print(json.dumps(c))


op, args = sys.argv[1], sys.argv[2:]
if op == "start":
    start(*args)
elif op == "run":
    run(args[0])
elif op == "list":
    print(json.dumps(all_commands()))
elif op == "get":
    print(json.dumps(show(load(args[0]))))
elif op == "output":
    output(args[0], int(args[1]), int(args[2]))
elif op == "kill":
    kill(args[0], args[1])
elif op == "forget":
    forget(args[0])
else:
    fail(2, "unknown operation " + op)
`;
