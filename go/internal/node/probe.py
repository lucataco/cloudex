# The self-test's probe (selftest.go), run inside a probe machine with
# python3 -c. Its one argument is JSON: a list of phases, each a list of steps
# run at once; phases run in order. It prints {step id: result} as JSON.
#   {"id", "kind": "tcp", "host", "port", "src"?}   open[:first line] | refused | timeout | unreachable | err:...
#   {"id", "kind": "udp53", "host"}                    answered | timeout | err:...
#   {"id", "kind": "dns", "name"}                      resolved:<address> | err:...
#   {"id", "kind": "ip", "args": [...]}                ok | err:... (runs ip(8); the probe with NET_ADMIN only)
#   {"id", "kind": "inside"}                           what the machine's root can do, as JSON
import errno, json, os, re, socket, struct, subprocess, sys, threading

T = 2.5

def tcp(s):
    fam = socket.AF_INET6 if ":" in s["host"] else socket.AF_INET
    c = socket.socket(fam, socket.SOCK_STREAM)
    c.settimeout(T)
    try:
        if s.get("src"):
            c.bind((s["src"], 0))
        c.connect((s["host"], int(s["port"])))
        try:
            c.settimeout(1.5)
            d = c.recv(200)
        except Exception:
            d = b""
        line = d.decode("utf-8", "replace").splitlines()[0].strip() if d else ""
        return "open" + (":" + line if line else "")
    except socket.timeout:
        return "timeout"
    except OSError as e:
        if e.errno == errno.ECONNREFUSED:
            return "refused"
        if e.errno in (errno.EHOSTUNREACH, errno.ENETUNREACH):
            return "unreachable"
        return "err:" + (e.strerror or str(e))
    finally:
        c.close()

def udp53(s):
    q = struct.pack(">HHHHHH", 0x4358, 0x0100, 1, 0, 0, 0) + b"\x07example\x03com\x00\x00\x01\x00\x01"
    c = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    c.settimeout(T)
    try:
        c.sendto(q, (s["host"], 53))
        c.recvfrom(512)
        return "answered"
    except socket.timeout:
        return "timeout"
    except OSError as e:
        return "err:" + (e.strerror or str(e))
    finally:
        c.close()

def dns(s):
    try:
        return "resolved:" + socket.getaddrinfo(s["name"], 443, socket.AF_INET)[0][4][0]
    except Exception as e:
        return "err:" + str(e)

def ip(s):
    r = subprocess.run(["ip"] + s["args"], capture_output=True, text=True, timeout=10)
    return "ok" if r.returncode == 0 else "err:" + (r.stderr.strip() or str(r.returncode))

def attempt(f):
    try:
        f()
        return "allowed"
    except Exception as e:
        return "denied:" + type(e).__name__

def inside(s):
    st = {}
    for line in open("/proc/self/status"):
        k, _, v = line.partition(":")
        if k in ("CapEff", "CapBnd", "Seccomp", "NoNewPrivs"):
            st[k] = v.strip()
    out = {"capEff": st.get("CapEff"), "capBnd": st.get("CapBnd"), "seccomp": st.get("Seccomp")}
    out["uidMap"] = " ".join(open("/proc/self/uid_map").read().split()[:3])
    # The host's Docker (or containerd) socket would be a bind mount; under
    # Sysbox the machine's own dockerd makes a socket of its own, which isn't.
    out["dockerSocket"] = any(re.search(r"(docker|containerd)[^/]*\.sock$", l.split()[4]) for l in open("/proc/self/mountinfo"))
    out["socketFiles"] = [p for p in ("/var/run/docker.sock", "/run/docker.sock") if os.path.exists(p)]
    def w(path, data):
        with open(path, "w") as f:
            f.write(data)
    out["procSysWrite"] = attempt(lambda: w("/proc/sys/vm/drop_caches", "1"))
    out["sysrqWrite"] = attempt(lambda: w("/proc/sysrq-trigger", "h"))
    out["sysWrite"] = attempt(lambda: w("/sys/kernel/mm/ksm/run", "0"))
    out["kcoreRead"] = attempt(lambda: open("/proc/kcore", "rb").read(1))
    out["rawSocket"] = attempt(lambda: socket.socket(socket.AF_INET, socket.SOCK_RAW, socket.IPPROTO_ICMP).close())
    out["packetSocket"] = attempt(lambda: socket.socket(socket.AF_PACKET, socket.SOCK_RAW).close())
    out["mknod"] = attempt(lambda: os.mknod("/tmp/.cx-probe-dev", 0o600 | 0o60000, os.makedev(8, 0)))
    try:
        os.unlink("/tmp/.cx-probe-dev")
    except OSError:
        pass
    r = subprocess.run(["mount", "-t", "tmpfs", "cxprobe", "/mnt"], capture_output=True)
    out["mount"] = "allowed" if r.returncode == 0 else "denied"
    if r.returncode == 0:
        subprocess.run(["umount", "/mnt"])
    r = subprocess.run(["ip", "route", "add", "192.0.2.1", "dev", "eth0"], capture_output=True)
    out["routeChange"] = "allowed" if r.returncode == 0 else "denied"
    if r.returncode == 0:
        subprocess.run(["ip", "route", "del", "192.0.2.1", "dev", "eth0"])
    out["devices"] = sorted(os.listdir("/dev"))
    try:
        out["ipv6"] = open("/proc/sys/net/ipv6/conf/eth0/disable_ipv6").read().strip() != "1"
    except OSError:
        out["ipv6"] = False
    out["ipv6Addresses"] = [l.split()[5] + ":" + l.split()[0] for l in open("/proc/net/if_inet6") if l.split()[5] != "lo"] if os.path.exists("/proc/net/if_inet6") else []
    return json.dumps(out, sort_keys=True)

KINDS = {"tcp": tcp, "udp53": udp53, "dns": dns, "ip": ip, "inside": inside}

def main():
    phases = json.loads(sys.argv[1])
    out = {}
    for phase in phases:
        ts = []
        for s in phase:
            def run(s=s):
                try:
                    out[s["id"]] = KINDS[s["kind"]](s)
                except Exception as e:
                    out[s["id"]] = "err:" + str(e)
            if s["kind"] == "ip":
                run()  # in order: they change the routes the next steps use
            else:
                t = threading.Thread(target=run)
                t.start()
                ts.append(t)
        for t in ts:
            t.join()
    print(json.dumps(out))

main()
