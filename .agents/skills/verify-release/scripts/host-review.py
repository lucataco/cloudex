# The 2026-11-12 review's host checks (verify.sh N45-N53), from inside a machine or a container beside one.
#
#   reach SECONDS HOST:PORT...  every 0.5 s, try TCP to each; print "epoch host:port open|refused|timeout|<errno>"
#   flood N SECONDS             hold up to N connections to this machine's agent (cloudex.internal :80, and :443
#                               with TLS for api.anthropic.com), each after one answered request, reopening the
#                               ones the agent closes; print one JSON line of counts
#   spoof SOURCE                send 5 DNS queries to 1.1.1.1:53 from SOURCE (another machine's address, added
#                               to this interface first); print whether an answer came back here
import errno
import json
import resource
import socket
import ssl
import struct
import sys
import time


def reach(secs, targets):
    targets = [(t.rsplit(":", 1)[0], int(t.rsplit(":", 1)[1])) for t in targets]
    end = time.time() + secs
    while time.time() < end:
        for h, p in targets:
            s = socket.socket()
            s.settimeout(0.4)
            try:
                s.connect((h, p))
                r = "open"
            except ConnectionRefusedError:
                r = "refused"
            except socket.timeout:
                r = "timeout"
            except OSError as e:
                r = errno.errorcode.get(e.errno, str(e.errno))
            finally:
                s.close()
            print(f"{time.time():.1f} {h}:{p} {r}", flush=True)
        time.sleep(0.5)


def flood(n, secs):
    resource.setrlimit(resource.RLIMIT_NOFILE, (n * 2 + 100, n * 2 + 100))
    host = socket.gethostbyname("cloudex.internal")
    ctx = ssl.create_default_context()
    ctx.check_hostname = False
    ctx.verify_mode = ssl.CERT_NONE
    req = b"POST /save-on-term HTTP/1.1\r\nHost: cloudex.internal\r\nContent-Length: 0\r\n\r\n"
    held, st = [], {"opened": 0, "answered": 0, "failed": 0, "most": 0}

    def one(i):
        s = socket.create_connection((host, 443 if i % 2 else 80), timeout=3)
        if i % 2:
            s = ctx.wrap_socket(s, server_hostname="api.anthropic.com")
        s.sendall(req)
        if not s.recv(4096):
            raise OSError("closed")
        st["answered"] += 1
        return s

    end, i = time.time() + secs, 0
    while time.time() < end:
        alive = []
        for s in held:
            try:
                s.setblocking(False)
                if s.recv(1) == b"":
                    s.close()
                    continue
            except (BlockingIOError, ssl.SSLWantReadError):
                pass
            except Exception:
                s.close()
                continue
            alive.append(s)
        held = alive
        tried = 0
        while len(held) < n and time.time() < end and tried < 500:
            tried += 1
            try:
                held.append(one(i))
                st["opened"] += 1
            except Exception:
                st["failed"] += 1
            i += 1
        st["most"] = max(st["most"], len(held))
        time.sleep(0.5)
    print(json.dumps(st))


def spoof(src):
    q = struct.pack(">HHHHHH", 0x4242, 0x0100, 1, 0, 0, 0) + b"\x07example\x03com\x00" + struct.pack(">HH", 1, 1)
    s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    s.bind((src, 53535))
    for _ in range(5):
        s.sendto(q, ("1.1.1.1", 53))
        time.sleep(0.3)
    s.settimeout(2)
    try:
        s.recvfrom(2048)
        print("sent 5 from", src, "and an answer came back here")
    except socket.timeout:
        print("sent 5 from", src)


if __name__ == "__main__":
    mode = sys.argv[1]
    if mode == "reach":
        reach(float(sys.argv[2]), sys.argv[3:])
    elif mode == "flood":
        flood(int(sys.argv[2]), float(sys.argv[3]))
    elif mode == "spoof":
        spoof(sys.argv[2])
    else:
        sys.exit("usage: host-review.py reach|flood|spoof ...")
