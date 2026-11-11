#!/bin/bash
# Where a node could hold the deployment's real provider keys (v0.2.2 M6): run
# on the node's host. REGEX matches the real keys by their shape and the last
# four characters cloudex keys shows, so the keys themselves are never read;
# only counts are printed, never a match. verify.sh N37 runs it.
#   keygrep.sh REGEX MACHINE_CONTAINER [sudo]
re=$1 mc=$2 sudo=${3:-}
img=$(docker inspect "$mc" -f '{{.Config.Image}}')
echo "machine-files $(docker exec "$mc" sh -c 'grep -rlaE -e "$1" / --exclude-dir=proc --exclude-dir=sys --exclude-dir=dev 2>/dev/null | wc -l' sh "$re")"
echo "machine-environ $(docker exec "$mc" sh -c 'for f in /proc/[0-9]*/environ; do tr "\0" "\n" <"$f"; done 2>/dev/null | grep -caE -e "$1"' sh "$re")"
echo "agent-volume $(docker run --rm -v cloudex-node:/v:ro --entrypoint sh "$img" -c 'grep -rlaE -e "$1" /v 2>/dev/null | wc -l' sh "$re")"
echo "agent-log $(docker logs cloudex-node 2>&1 | grep -caE -e "$re")"
echo "container-config $(docker inspect $(docker ps -aq) 2>/dev/null | grep -caE -e "$re")"
if [ -n "$sudo" ]; then
	echo "host-environ $(sudo sh -c 'for f in /proc/[0-9]*/environ; do tr "\0" "\n" <"$f"; done 2>/dev/null' | grep -caE -e "$re")"
fi
