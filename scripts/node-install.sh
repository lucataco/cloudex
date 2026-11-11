#!/usr/bin/env bash
# Installs cloudex-node, the agent that lets this host run Cloudex sandboxes
# (a "node"), and the firewall that keeps its machines off the host and its
# network. Published at https://cloudex.build/node; cloudex node add prints
# the command, which is:
#
#   curl -fsSL https://cloudex.build/node | sudo bash -s -- --api <url> --token <cxj_...> [--version vX.Y.Z]
#
# As root, on Linux (amd64) with systemd, nftables and Docker 24 or later, it:
#   1. checks those, and whether Docker has Sysbox (machines then run under
#      sysbox-runc, and root in a machine isn't root on the host);
#   2. picks a /16 for machine networks that collides with no route and no
#      Docker network here;
#   3. writes /etc/cloudex-node/firewall.nft and cloudex-node-firewall.service,
#      which loads it now and at every boot (one nftables table, inet cloudex,
#      that touches only the machines' cx* bridges);
#   4. joins the deployment with the token, and starts the agent's container,
#      cloudex-node, with the Docker socket (it has no port of its own);
#   5. waits for it to be online and for its isolation self-test to pass;
#   6. boots a throwaway machine, checks from inside it that the internet
#      answers and the host and metadata don't, and removes it.
# It says it worked only if all of that did.
#
# Again with --uninstall, it removes all of it: the agent, every machine and
# network it made (their disks too), its volume, the firewall and its unit,
# and the images it pulled. Running it again without --uninstall updates the
# agent (with --version) and keeps the node's identity and its machines.
#
# --unisolated is for testing only, on a host where you have no root: no
# firewall, so machines reach the host, its LAN and its tailnet.
set -euo pipefail

API="" TOKEN="" VERSION="latest" AGENT_IMAGE="" POOL="" MACHINE_IMAGE=""
UNISOLATED=0 UNINSTALL=0 YES=0 KEEP_IMAGES=0
ALLOW=() BLOCK=()
NAME=cloudex-node
UNIT=cloudex-node-firewall.service

usage() {
	sed -n '2,/^set -euo/p' "$0" 2>/dev/null | sed '$d' | sed 's/^# \{0,1\}//'
	cat <<'EOF'
Options:
  --api URL             the deployment's URL
  --token cxj_...       the join token cloudex node add printed
  --version vX.Y.Z      the agent's version (default: latest)
  --agent-image REF     the agent's image (default: ghcr.io/lucataco/cloudex-node:<version>)
  --pool CIDR           the machine pool, such as 10.250.0.0/16 (default: one that collides with nothing)
  --machine-image REF   run machines from this image instead of the deployment's
  --allow-image REPO    another image repository machines may run (repeatable)
  --block CIDR          another IPv4 range machines may not reach (repeatable)
  --unisolated          testing only, without root: no firewall
  --uninstall           remove the agent, its machines, the firewall and the images it pulled
  --keep-images         with --uninstall, keep the images
  --yes                 don't ask before removing machines
EOF
}

while [[ $# -gt 0 ]]; do
	case $1 in
	--api) API=$2; shift 2 ;;
	--token) TOKEN=$2; shift 2 ;;
	--version) VERSION=$2; shift 2 ;;
	--agent-image) AGENT_IMAGE=$2; shift 2 ;;
	--pool) POOL=$2; shift 2 ;;
	--machine-image) MACHINE_IMAGE=$2; shift 2 ;;
	--allow-image) ALLOW+=("$2"); shift 2 ;;
	--block) BLOCK+=("$2"); shift 2 ;;
	--unisolated) UNISOLATED=1; shift ;;
	--uninstall) UNINSTALL=1; shift ;;
	--keep-images) KEEP_IMAGES=1; shift ;;
	--yes | -y) YES=1; shift ;;
	-h | --help) usage; exit 0 ;;
	*) echo "unknown option $1 (--help lists them)" >&2; exit 2 ;;
	esac
done
[[ -n $AGENT_IMAGE ]] || AGENT_IMAGE=ghcr.io/lucataco/cloudex-node:$VERSION

say() { printf '\033[1m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[33mwarning:\033[0m %s\n' "$*" >&2; }
die() { printf '\033[31merror:\033[0m %s\n' "$*" >&2; exit 1; }

ROOT=0
[[ $(id -u) == 0 ]] && ROOT=1
if [[ $ROOT == 1 ]]; then ETC=/etc/cloudex-node; else ETC=${XDG_STATE_HOME:-$HOME/.local/state}/cloudex-node; fi

command -v docker >/dev/null || die "Docker isn't installed (https://docs.docker.com/engine/install/)"
docker info >/dev/null 2>&1 || die "can't reach Docker: is it running, and may this user use it?"

# ---------------------------------------------------------------- uninstall
if [[ $UNINSTALL == 1 ]]; then
	[[ $ROOT == 1 || ! -f /etc/cloudex-node/install.env ]] || die "the firewall was installed as root; uninstall as root (sudo)"
	machines=$(docker ps -aq --filter label=build.cloudex.node | wc -l)
	if [[ $YES != 1 ]]; then
		[[ -t 0 ]] || die "this removes the agent and its $machines machines, with their disks; pass --yes to confirm"
		read -r -p "Remove the agent and its $machines machines, with their disks? [y/N] " a
		[[ $a == y || $a == yes ]] || die "nothing removed"
	fi
	images_before=""
	agent_image=""
	if [[ -f $ETC/install.env ]]; then
		# shellcheck disable=SC1091
		images_before=$(sed -n 's/^IMAGES_BEFORE=//p' "$ETC/install.env")
		agent_image=$(sed -n 's/^AGENT_IMAGE=//p' "$ETC/install.env")
	fi
	say "removing the agent"
	docker rm -f "$NAME" >/dev/null 2>&1 || true
	say "removing its machines and their networks"
	for c in $(docker ps -aq --filter label=build.cloudex.node); do docker rm -f -v "$c" >/dev/null; done
	for n in $(docker network ls -q --filter label=build.cloudex.node); do docker network rm "$n" >/dev/null; done
	for n in $(docker network ls -q --filter label=build.cloudex.agent); do docker network rm "$n" >/dev/null; done
	docker volume rm "$NAME" >/dev/null 2>&1 || true
	if [[ $ROOT == 1 ]]; then
		say "removing the firewall"
		if [[ -f /etc/systemd/system/$UNIT ]]; then
			systemctl disable --now "$UNIT" >/dev/null 2>&1 || true
			rm -f "/etc/systemd/system/$UNIT"
			systemctl daemon-reload
		fi
		command -v nft >/dev/null && nft delete table inet cloudex 2>/dev/null || true
	fi
	if [[ $KEEP_IMAGES != 1 ]]; then
		# The agent's and the machines' images, unless they were here before the install.
		refs=$(docker image ls --format '{{.Repository}}:{{.Tag}} {{.ID}}' | awk '$1 ~ /(^|\/)cloudex-(node|base):/')
		[[ -n $agent_image ]] && refs+=$'\n'"$agent_image $(docker image inspect -f '{{.Id}}' "$agent_image" 2>/dev/null | cut -c8-19)"
		while read -r ref id; do
			[[ -n $ref && -n $id ]] || continue
			case " $images_before " in *" $id "*) continue ;; esac
			say "removing the image $ref"
			docker rmi "$ref" >/dev/null 2>&1 || true
		done <<<"$refs"
	fi
	rm -rf "$ETC"
	say "uninstalled: no agent, machines, networks, volume or firewall of cloudex-node are left"
	exit 0
fi

# ---------------------------------------------------------------- 1. checks
say "checking this host"
[[ $(uname -s) == Linux ]] || die "nodes run on Linux"
case $(uname -m) in x86_64 | amd64) ;; *) die "nodes are amd64 only for now (this is $(uname -m))" ;; esac
dv=$(docker version -f '{{.Server.Version}}')
[[ ${dv%%.*} -ge 24 ]] 2>/dev/null || die "Docker $dv is too old; nodes need Docker 24 or later"
if [[ $UNISOLATED == 1 ]]; then
	warn "--unisolated: no firewall. Machines on this node reach the host, its LAN and its tailnet. For testing only."
else
	[[ $ROOT == 1 ]] || die "run it as root (sudo), which the firewall needs; --unisolated (testing only) skips the firewall"
	[[ -d /run/systemd/system ]] || die "nodes need systemd, which loads the firewall at every boot"
	command -v nft >/dev/null || die "nodes need nftables (apt install nftables, or dnf install nftables)"
	command -v ip >/dev/null || die "nodes need ip(8) (iproute2)"
fi
RUNTIME=docker
if docker info -f '{{json .Runtimes}}' | grep -q '"sysbox-runc"'; then
	RUNTIME=sysbox
	say "Sysbox is installed: machines run under sysbox-runc, and Docker works inside them"
fi
if [[ -z $API || -z $TOKEN ]] && ! docker volume inspect "$NAME" >/dev/null 2>&1; then
	die "give --api and --token (cloudex node add prints them)"
fi
mkdir -p "$ETC"
chmod 700 "$ETC"
[[ -f $ETC/install.env ]] || printf 'IMAGES_BEFORE=%s\n' "$(docker image ls -q --no-trunc | cut -c8-19 | sort -u | tr '\n' ' ')" >"$ETC/install.env"

if ! docker image inspect "$AGENT_IMAGE" >/dev/null 2>&1; then
	say "pulling $AGENT_IMAGE"
	docker pull -q "$AGENT_IMAGE" >/dev/null
fi
SOCK=/var/run/docker.sock
[[ -S $SOCK ]] || die "no Docker socket at $SOCK"
GID=$(stat -c %g "$SOCK")
agent() { docker run --rm --group-add "$GID" -v "$SOCK:$SOCK" "$@"; }

# ---------------------------------------------------------------- 2. the pool
if [[ -z $POOL ]]; then
	POOL=$(sed -n 's/^POOL=//p' "$ETC/install.env")
	[[ -n $POOL ]] || POOL=$(agent --network host "$AGENT_IMAGE" pick-pool)
fi
say "machine networks: $POOL"

# ---------------------------------------------------------------- 3. the firewall
if [[ $UNISOLATED != 1 ]]; then
	say "installing the firewall ($UNIT)"
	onlink=()
	for p in $(ip -4 -o route show scope link proto kernel | awk '{print $1}'); do onlink+=(--onlink "$p"); done
	block=()
	for b in "${BLOCK[@]}"; do block+=(--block "$b"); done
	docker run --rm "$AGENT_IMAGE" firewall --pool "$POOL" "${onlink[@]}" "${block[@]}" >"$ETC/firewall.nft.new"
	nft -c -f "$ETC/firewall.nft.new" || die "nftables refused the rules in $ETC/firewall.nft.new"
	mv "$ETC/firewall.nft.new" "$ETC/firewall.nft"
	NFT=$(command -v nft)
	cat >"/etc/systemd/system/$UNIT" <<EOF
# Written by the cloudex-node installer. The firewall for the machines of this
# Cloudex node: one nftables table, inet cloudex, that touches only traffic
# to and from their bridges (cx*). Docker's restarts don't touch it.
[Unit]
Description=Cloudex node: the firewall for machine networks
Documentation=https://cloudex.build/docs/nodes
DefaultDependencies=no
After=network-pre.target nftables.service
Before=docker.service network.target
Wants=network-pre.target

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=$NFT -f $ETC/firewall.nft
ExecReload=$NFT -f $ETC/firewall.nft
ExecStop=-$NFT delete table inet cloudex

[Install]
WantedBy=multi-user.target docker.service
EOF
	systemctl daemon-reload
	systemctl enable "$UNIT" >/dev/null 2>&1
	systemctl restart "$UNIT"
	nft list table inet cloudex >/dev/null || die "the firewall didn't load: systemctl status $UNIT"
fi

# ---------------------------------------------------------------- 4. the agent
probes=()
if [[ $UNISOLATED != 1 ]]; then
	# What the self-test must find closed from inside a machine, besides what
	# it knows itself: this host's own address, its gateway and DNS, and any
	# tailnet address.
	src=$(ip -4 route get 1.1.1.1 2>/dev/null | sed -n 's/.* src \([0-9.]*\).*/\1/p' | head -1)
	gw=$(ip -4 route show default 2>/dev/null | awk '{print $3; exit}')
	[[ -n $src ]] && probes+=(--probe "$src:22" --probe "$src:80" --probe "$src:443")
	[[ -n $gw ]] && probes+=(--probe "$gw:80" --probe "$gw:53" --probe "udp53=$gw")
	for f in /run/systemd/resolve/resolv.conf /etc/resolv.conf; do
		[[ -f $f ]] || continue
		for d in $(awk '/^nameserver [0-9.]+$/ {print $2}' "$f"); do
			case $d in 127.* | 1.1.1.1 | 8.8.8.8 | 1.0.0.1 | 8.8.4.4) continue ;; esac
			probes+=(--probe "$d:53" --probe "udp53=$d")
		done
	done
	for a in $(ip -4 -o addr show | awk '{print $4}' | cut -d/ -f1); do
		case $a in 100.6[4-9].* | 100.[7-9][0-9].* | 100.1[01][0-9].* | 100.12[0-7].*) probes+=(--probe "$a:22") ;; esac
	done
fi
run_args=(--pool "$POOL" --docker-root /docker-root "${probes[@]}")
[[ -n $MACHINE_IMAGE ]] && run_args+=(--machine-image "$MACHINE_IMAGE")
for a in "${ALLOW[@]}"; do run_args+=(--allow-image "$a"); done

docker network inspect "$NAME" >/dev/null 2>&1 || docker network create --label build.cloudex.agent=1 "$NAME" >/dev/null
docker volume inspect "$NAME" >/dev/null 2>&1 || docker volume create --label build.cloudex.agent=1 "$NAME" >/dev/null
if ! agent -v "$NAME:/var/lib/cloudex-node" "$AGENT_IMAGE" status --json 2>/dev/null | grep -q '"joined": true'; then
	say "joining $API"
	join=(join --api "$API" --token "$TOKEN")
	[[ $UNISOLATED == 1 ]] && join+=(--unisolated)
	docker run --rm -v "$NAME:/var/lib/cloudex-node" "$AGENT_IMAGE" "${join[@]}" >/dev/null || die "the join failed (a token works once, for 30 minutes: cloudex node add <name> again gives a new one)"
else
	say "this host has joined already; keeping its identity"
fi
if docker container inspect "$NAME" >/dev/null 2>&1; then
	say "replacing the running agent (its machines keep running)"
	docker rm -f "$NAME" >/dev/null
fi
DROOT=$(docker info -f '{{.DockerRootDir}}')
say "starting the agent"
STARTED_MS=$(($(date +%s) * 1000))
docker run -d --name "$NAME" --restart unless-stopped --label build.cloudex.agent=1 \
	--network "$NAME" \
	--sysctl net.ipv4.ip_forward=0 --sysctl net.ipv4.conf.all.rp_filter=1 --sysctl net.ipv4.conf.default.rp_filter=1 \
	--group-add "$GID" --read-only --tmpfs /tmp --cap-drop ALL --security-opt no-new-privileges \
	--log-opt max-size=10m --log-opt max-file=3 \
	-e CLOUDEX_NODE_CONTAINER="$NAME" \
	-v "$SOCK:$SOCK" -v "$NAME:/var/lib/cloudex-node" \
	--mount "type=bind,src=$DROOT/network,dst=/docker-root,readonly" \
	"$AGENT_IMAGE" run "${run_args[@]}" >/dev/null
{
	grep '^IMAGES_BEFORE=' "$ETC/install.env"
	printf 'AGENT_IMAGE=%s\nPOOL=%s\nRUNTIME=%s\nUNISOLATED=%s\nINSTALLED_AT=%s\n' "$AGENT_IMAGE" "$POOL" "$RUNTIME" "$UNISOLATED" "$(date -u +%FT%TZ)"
} >"$ETC/install.env.new" && mv "$ETC/install.env.new" "$ETC/install.env"

# ---------------------------------------------------------------- 5. online, isolated
say "waiting for the agent to connect and pass its isolation self-test (it pulls the machine image first)"
# Only a self-test of this agent counts: the status file in the volume may
# still hold the result of the agent it replaced.
if ! docker exec "$NAME" cloudex-node status --wait 600s --since "$STARTED_MS"; then
	docker logs --tail 20 "$NAME" >&2 || true
	if [[ $UNISOLATED == 1 ]]; then
		warn "as expected without the firewall, the self-test fails; machines still run, because the node joined --unisolated"
	else
		die "the node isn't ready (above); it refuses machines until its self-test passes"
	fi
fi

# ---------------------------------------------------------------- 6. a throwaway machine
say "booting a throwaway machine"
msg=$(docker exec "$NAME" cloudex-node selftest --machine 2>&1) || die "the throwaway machine failed: $msg"
say "$msg"

echo
say "this node is installed and online"
if [[ $UNISOLATED == 1 ]]; then
	warn "it has no isolation (--unisolated): machines reach this host, its LAN and its tailnet. For testing only."
elif [[ $RUNTIME == docker ]]; then
	warn "machines on this node run under plain Docker. Root in a machine is root on this host if code escapes the container. Use this node only for code you trust, or install Sysbox (https://github.com/nestybox/sysbox) and run this installer again."
fi
echo "Uninstall with: curl -fsSL https://cloudex.build/node | sudo bash -s -- --uninstall"
