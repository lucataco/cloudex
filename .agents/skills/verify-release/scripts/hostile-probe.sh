#!/bin/bash
# A hostile machine on a node (v0.2.2 M6): run as root with NET_ADMIN inside a
# machine, or in a container on that machine's network, to try what such a
# machine must not manage through the node's agent. verify.sh N42 runs it.
#   hostile-probe.sh OWN_AGENT OTHER_AGENT OTHER_SUBNET OTHER_ID CONTROL_ADDR...
# OWN_AGENT is the agent's address on this machine's network (cloudex.internal
# here), OTHER_* another machine's network, its agent's address and its
# sandbox id, CONTROL_ADDR the agent's own addresses outside machine networks.
# Prints one "name result" line per probe; verify.sh judges them.
set -u
own=$1 other=$2 osub=$3 oid=$4
shift 4
ctl=("$@")
c() { curl -s -m 6 -o /dev/null -w '%{http_code}' "$@" 2>/dev/null || true; }
body() { curl -s -m 6 "$@" 2>/dev/null | head -c 300 | tr '\n' ' '; }
say() { printf '%s %s\n' "$1" "${2:-none}"; }
tok=$(cat /run/cloudex/self-token 2>/dev/null || true)

# What it may do: its own cloudex.internal, which answers /save-on-term itself.
say own-save-on-term "$(body -X POST "http://$own/save-on-term")"
# Its own self API needs the run's token, which only the cloudex command reads.
say own-self-no-token "$(c "http://$own/v1/sandboxes/self")"
# With its token, it still manages only itself: another sandbox's id is refused,
# and the headers the agent signs with are its own, not the machine's.
say own-token-other-sandbox "$(c -H "x-cloudex-self: $tok" -H "x-cx-sandbox: $oid" "http://$own/v1/sandboxes/$oid")"
# Not a proxy: an absolute URL goes to cloudex.internal's path, CONNECT and
# WebSockets are refused, TLS ends only for the providers' names.
say proxy-absolute "$(body -x "http://$own:80" http://example.com/)"
say proxy-connect "$(curl -s -m 6 -o /dev/null -w '%{http_connect}' -p -x "http://$own:80" https://example.com/ 2>/dev/null)"
say proxy-websocket "$(c -H 'Upgrade: websocket' -H 'Connection: Upgrade' "http://$own/v1/sandboxes/self")"
# API_HOST: the deployment's own host, which the node CA must not cover either.
for h in github.com pypi.org example.com "${API_HOST:-api.example.workers.dev}"; do
	say "tls-$h" "$(c -k --resolve "$h:443:$own" "https://$h/")"
done
say tls-provider-other-host "$(c -k --resolve api.openai.com:443:$own -H 'Host: example.com' https://api.openai.com/v1/models)"
# No R2 backups from a node, and no save's objects without a grant.
say backup-put "$(c -X PUT --data x "http://$own/backup/1/0")"
say save-put-no-grant "$(c -X PUT --data x "http://$own/saves/sv-aaaaaaaaaa/0")"
# A body over the edge's limit is refused by the agent itself.
say body-over-limit "$(head -c 100000001 /dev/zero | curl -s -m 60 -o /dev/null -w '%{http_code}' -X PUT -H 'content-type: application/octet-stream' --data-binary @- "http://$own/saves/sv-aaaaaaaaaa/0" 2>/dev/null || true)"

# What NET_ADMIN tries: reach another machine's agent address through its own
# agent (it arrives from this network's /28, so it's refused), forge a source
# in the other network (no handshake), and the agent's control address
# (nothing listens there).
ip route add "$osub" via "$own" 2>/dev/null
say routed-other-agent-80 "$(c -X POST "http://$other/save-on-term")"
say routed-other-agent-443 "$(c -k --resolve "api.anthropic.com:443:$other" https://api.anthropic.com/v1/models)"
forged=$(python3 -c "import ipaddress,sys; print(ipaddress.ip_network(sys.argv[1])[5])" "$osub")
dev=$(ip -o route get "$own" | sed -n 's/.* dev \([^ ]*\).*/\1/p')
ip addr add "$forged/32" dev "$dev" 2>/dev/null
say forged-source "$(c --interface "$forged" -X POST "http://$other/save-on-term")"
ip addr del "$forged/32" dev "$dev" 2>/dev/null
ip route del "$osub" via "$own" 2>/dev/null
for a in "${ctl[@]}"; do
	ip route add "$a/32" via "$own" 2>/dev/null
	say "control-$a-80" "$(c "http://$a/")"
	say "control-$a-443" "$(c -k "https://$a/")"
	ip route del "$a/32" via "$own" 2>/dev/null
done
