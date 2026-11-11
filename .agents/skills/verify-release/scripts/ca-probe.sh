#!/bin/bash
# The node CA's name constraints, in a machine on a node (v0.2.2 M6): run as
# root in a login shell there (so NODE_EXTRA_CA_CERTS is set), after
# /tmp/cx-ca/<name>.pem holds a certificate and key the node CA signed for
# each name (cloudex-node ca test-cert). Each is served on 127.0.0.1 and tried
# as that name with openssl verify, curl, Python and Node, which trust what the
# machine trusts. verify.sh N38 runs it. Prints "<name> openssl curl python node",
# each ok or fail.
port=9443
for n in "$@"; do
	f=/tmp/cx-ca/$n.pem
	openssl s_server -quiet -accept "$port" -cert "$f" -key "$f" -www >/dev/null 2>&1 &
	pid=$!
	for _ in $(seq 20); do (exec 3<>"/dev/tcp/127.0.0.1/$port") 2>/dev/null && break; sleep 0.1; done
	o=$(openssl verify -CAfile /etc/cloudex/node-ca.crt "$f" >/dev/null 2>&1 && echo ok || echo fail)
	c=$(curl -s -o /dev/null -m 5 --resolve "$n:$port:127.0.0.1" "https://$n:$port/" && echo ok || echo fail)
	py=$(python3 -I - "$n" "$port" <<'PY'
import socket, ssl, sys
try:
    with socket.create_connection(("127.0.0.1", int(sys.argv[2])), timeout=5) as s:
        with ssl.create_default_context().wrap_socket(s, server_hostname=sys.argv[1]):
            print("ok")
except Exception:
    print("fail")
PY
)
	nd=$(node -e '
const s = require("tls").connect({ host: "127.0.0.1", port: +process.argv[2], servername: process.argv[1] }, () => { console.log("ok"); s.end(); });
s.on("error", () => console.log("fail"));' "$n" "$port" 2>/dev/null || echo fail)
	kill "$pid" 2>/dev/null
	wait "$pid" 2>/dev/null
	echo "$n $o $c $py $nd"
	port=$((port + 1))
done
