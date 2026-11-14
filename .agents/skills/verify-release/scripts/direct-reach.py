# What a machine gets from the node's direct port (verify.sh N62, v0.2.3): run
# inside a machine (as its user, or as root with route-changing rights), or in
# a container on a machine's network.
#   python3 direct-reach.py [--via GW] HOST:PORT...
# With --via, each HOST first gets a route through GW (the agent's address on
# this machine's network), as a machine with NET_ADMIN can add, and loses it
# after. Prints "HOST:PORT result" per target, where result is one of
#   timeout | refused | unreachable   no TCP connection
#   closed | stalled                  a TCP connection, then nothing (no TLS)
#   tls                               a TLS handshake finished: a session could follow
import socket, ssl, subprocess, sys

args = sys.argv[1:]
via = None
if args[:1] == ["--via"]:
    via, args = args[1], args[2:]


def reach(host, port):
    try:
        s = socket.create_connection((host, port), timeout=4)
    except socket.timeout:
        return "timeout"
    except ConnectionRefusedError:
        return "refused"
    except OSError:
        return "unreachable"
    ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
    ctx.check_hostname = False
    ctx.verify_mode = ssl.CERT_NONE
    ctx.set_alpn_protocols(["cloudex-direct/1"])
    try:
        t = ctx.wrap_socket(s)
        t.close()
        return "tls"
    except socket.timeout:
        return "stalled"
    except (ssl.SSLError, OSError):
        return "closed"
    finally:
        s.close()


for target in args:
    host, port = target.rsplit(":", 1)
    if via:
        subprocess.run(["ip", "route", "add", host + "/32", "via", via], stderr=subprocess.DEVNULL)
    print(target, reach(host, int(port)), flush=True)
    if via:
        subprocess.run(["ip", "route", "del", host + "/32", "via", via], stderr=subprocess.DEVNULL)
