# Keystroke round trips through ssh (verify.sh N11 and N12):
#   python3 keys.py HOST N SSH_CONFIG            sends one byte N times, 10 ms apart, through
#                                                `ssh HOST cat`; prints {"p50","p90","max"} in ms
#   python3 keys.py HOST 0 SSH_CONFIG --idle S   one byte, S seconds of nothing, one more byte;
#                                                prints {"alive": true} if the session held
import json, subprocess, sys, time

host, n, cfg = sys.argv[1], int(sys.argv[2]), sys.argv[3]
p = subprocess.Popen(["ssh", "-F", cfg, "-o", "BatchMode=yes", "-T", host, "cat"], stdin=subprocess.PIPE, stdout=subprocess.PIPE, bufsize=0)
p.stdin.write(b"x")
p.stdout.read(1)
if len(sys.argv) > 5 and sys.argv[4] == "--idle":
    time.sleep(int(sys.argv[5]))
    alive = p.poll() is None
    if alive:
        try:
            p.stdin.write(b"y")
            alive = p.stdout.read(1) == b"y"
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
