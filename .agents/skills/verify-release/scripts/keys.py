# Keystroke round trips through ssh (verify.sh N11 and N12):
#   python3 keys.py HOST N SSH_CONFIG            sends one byte N times, 10 ms apart, through
#                                                `ssh HOST cat`; prints {"p50","p90","max"} in ms
#   python3 keys.py HOST 0 SSH_CONFIG --idle S   one byte, S seconds of nothing, one more byte;
#                                                prints {"alive": true} if the session held
#   python3 keys.py HOST 0 SSH_CONFIG --until F  the same, idle until the file F exists (at most
#                                                30 minutes), then 5 s more (verify.sh N65)
# KEYS_CMD, when set, is the command to run instead of `ssh -F SSH_CONFIG HOST cat` (such as
# "ssh box cat", for host ssh's own keystrokes to compare with).
import json, os, select, shlex, subprocess, sys, time

host, n, cfg = sys.argv[1], int(sys.argv[2]), sys.argv[3]
cmd = shlex.split(os.environ["KEYS_CMD"]) if os.environ.get("KEYS_CMD") else ["ssh", "-F", cfg, "-o", "BatchMode=yes", "-T", host, "cat"]
p = subprocess.Popen(cmd, stdin=subprocess.PIPE, stdout=subprocess.PIPE, bufsize=0)
p.stdin.write(b"x")
p.stdout.read(1)
if len(sys.argv) > 5 and sys.argv[4] in ("--idle", "--until"):
    if sys.argv[4] == "--idle":
        time.sleep(int(sys.argv[5]))
    else:
        end = time.time() + 1800
        while not os.path.exists(sys.argv[5]) and time.time() < end:
            time.sleep(1)
        time.sleep(5)
    alive = p.poll() is None
    if alive:
        try:
            p.stdin.write(b"y")
            # A session that hangs instead of closing counts as dropped.
            alive = bool(select.select([p.stdout], [], [], 30)[0]) and p.stdout.read(1) == b"y"
        except OSError:
            alive = False
    print(json.dumps({"alive": alive}))
    p.kill()
    sys.exit(0)
ms = []
for _ in range(n):
    t = time.perf_counter()
    p.stdin.write(b"k")
    p.stdout.read(1)
    ms.append((time.perf_counter() - t) * 1000)
    time.sleep(0.01)
p.stdin.close()
p.wait()
ms.sort()
print(json.dumps({"p50": round(ms[len(ms) // 2], 1), "p90": round(ms[int(len(ms) * 0.9)], 1), "max": round(ms[-1], 1)}))
