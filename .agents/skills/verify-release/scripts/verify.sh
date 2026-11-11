#!/usr/bin/env bash
# Checks what Cloudex ships against a live deployment, the way a user runs it.
#   verify.sh [area...]    areas: setup lifecycle connect inside agents tokens dashboard browser sdk node (default: all)
# Env: CLOUDEX_BIN (default: cloudex on PATH), CLOUDEX_CONFIG_DIR (the deployment to use,
# default ~/.config/cloudex), VERIFY_OUT (results directory), VERIFY_KEEP=1 keeps sandboxes,
# VERIFY_BROWSERS (browser area: "chrome", the default, "firefox", or both), CHROME and FIREFOX
# (where they are, when not in the usual places), VERIFY_AGENTS=1 (the browser's agent turn
# without the agents area). The node area: VERIFY_NODE_SSH (a command prefix that runs a
# command on the host for the agent, such as "ssh box"; without it the area is a SKIP),
# VERIFY_NODE_SUDO=1 (passwordless sudo there: the installer runs as root with the
# firewall; without it, --unisolated, and the root-only checks SKIP), VERIFY_NODE_AGENT_IMAGE
# (the agent's image, if the host has it; else it's built there from this checkout),
# VERIFY_NODE_MACHINE_IMAGE (the installer's --machine-image), VERIFY_NODE_DEPLOY=1 (N07,
# which runs cloudex deploy with $CLOUDFLARE_API_TOKEN), VERIFY_NODE_LONG=1 (N12's 30 idle
# minutes, and with VERIFY_NODE_DEPLOY=1 N13's deploy during an scp), VERIFY_NODE_REBOOT=1
# (N23 reboots the host), and with VERIFY_NODE_DEPLOY=1, N44's deploy during a node save;
# VERIFY_NODE_PARTS (default "m3 m4 m5 m6") runs only those milestones' checks after the install.
# VERIFY_NODE=<node> (v0.2.2): the lifecycle, connect, inside, agents, tokens, dashboard, sdk
# and browser areas make their sandboxes on that node, which must be online (cloudex node
# add, then its installer), instead of on Cloudflare; what isn't on nodes yet (forks, sizes
# and backups) SKIPs, saying why, once the API has refused it as it should.
# Prints one PASS/FAIL/SKIP line per check, writes results.tsv, exits 1 on any FAIL.
set -uo pipefail

here=$(cd "$(dirname "$0")" && pwd)
repo=$(cd "$here/../../../.." && pwd)
CX=${CLOUDEX_BIN:-cloudex}
command -v "$CX" >/dev/null || { echo "no cloudex binary: $CX" >&2; exit 2; }
command -v jq >/dev/null || { echo "jq is needed" >&2; exit 2; }
src_cfg=${CLOUDEX_CONFIG_DIR:-$HOME/.config/cloudex}
[[ -f $src_cfg/config.json ]] || { echo "no deployment in $src_cfg (run cloudex deploy or login)" >&2; exit 2; }

# Our own config dir: the run never changes the user's current sandbox or known_hosts.
work=$(mktemp -d "${TMPDIR:-/tmp}/cloudex-verify.XXXXXX")
export CLOUDEX_CONFIG_DIR=$work/config CLOUDEX_NO_UPDATE_CHECK=1
mkdir -p "$CLOUDEX_CONFIG_DIR" && cp "$src_cfg/config.json" "$CLOUDEX_CONFIG_DIR/" && chmod 700 "$CLOUDEX_CONFIG_DIR"
out=${VERIFY_OUT:-$work/out}
mkdir -p "$out"
results=$out/results.tsv
printf 'id\tresult\tcheck\tdetail\n' >"$results"
P=vr$(date +%H%M%S)
# VERIFY_NODE: the areas' sandboxes go on that node (see the top).
ON=()
[[ -n ${VERIFY_NODE:-} ]] && ON=(--on "$VERIFY_NODE")
on_node() { [[ -n ${VERIFY_NODE:-} ]]; }
NOT_ON_NODES="not on nodes in v0.2.2"
API_URL=$(jq -r .apiUrl "$CLOUDEX_CONFIG_DIR/config.json")
ZONE=$(jq -r '.previewZone // empty' "$CLOUDEX_CONFIG_DIR/config.json")
fails=0

note() { printf '%s\t%s\t%s\t%s\n' "$1" "$2" "$3" "${4//$'\n'/ }" >>"$results"; }
pass() { printf '\033[32mPASS\033[0m %-5s %s\n' "$1" "$2"; note "$1" PASS "$2" "${3:-}"; }
fail() { printf '\033[31mFAIL\033[0m %-5s %s: %s\n' "$1" "$2" "$3"; note "$1" FAIL "$2" "$3"; fails=$((fails + 1)); }
skip() { printf '\033[33mSKIP\033[0m %-5s %s: %s\n' "$1" "$2" "$3"; note "$1" SKIP "$2" "$3"; }

# cx runs the CLI, keeping stdout in $OUT, stderr in $ERR and the exit code in $CODE.
cx() {
	"$CX" "$@" >"$work/o" 2>"$work/e" </dev/null
	CODE=$?
	OUT=$(cat "$work/o")
	ERR=$(cat "$work/e")
}
# ok ID DESC CONDITION... : passes when the condition (a command) succeeds.
ok() {
	local id=$1 desc=$2
	shift 2
	# A check that set OUT itself (CODE="") keeps it in results.tsv when it passes too.
	if "$@" >/dev/null 2>&1; then pass "$id" "$desc" "$([ -z "${CODE-x}" ] && echo "$OUT")"; else fail "$id" "$desc" "code=$CODE out=${OUT:0:300} err=${ERR:0:300}"; fi
}
is() { [[ $1 == "$2" ]]; }
has() { [[ $1 == *"$2"* ]]; }
field() { jq -r "$1" <<<"$OUT" 2>/dev/null; }
http() { curl -s -o /dev/null -w '%{http_code}' --max-time 30 "$1"; }

cleanup() {
	[[ ${VERIFY_KEEP:-} == 1 ]] && { echo "kept sandboxes and saves named $P-*; config in $CLOUDEX_CONFIG_DIR"; return; }
	local ids
	ids=$("$CX" list --json 2>/dev/null | jq -r --arg p "$P-" '.[] | select((.name // "") | startswith($p)) | .id')
	for i in $ids; do "$CX" delete "$i" --yes >/dev/null 2>&1; done
	for s in $("$CX" saves --json 2>/dev/null | jq -r --arg p "$P-" '.[] | select(.name | startswith($p)) | .name'); do
		"$CX" saves rm "$s" --yes >/dev/null 2>&1
	done
	for t in $("$CX" tokens --json 2>/dev/null | jq -r --arg p "$P-" '.[]? | select(.name | startswith($p)) | .id'); do
		"$CX" tokens revoke "$t" >/dev/null 2>&1
	done
	for c in $("$CX" tokens approve --json 2>/dev/null | jq -r --arg p "$P-" '.[]? | select(.name | startswith($p)) | .userCode'); do
		"$CX" tokens deny "$c" >/dev/null 2>&1
	done
	# Nodes: revoked by the first rm, forgotten by the second. The agent on the
	# host and its directory go too; nothing else there is touched.
	for n in $("$CX" node list --json 2>/dev/null | jq -r --arg p "$P-" '.[]? | select(.name | startswith($p)) | .name'); do
		"$CX" node rm "$n" --yes --force >/dev/null 2>&1
		"$CX" node rm "$n" --yes >/dev/null 2>&1
	done
	[[ -n ${NODE_DIR:-} ]] && node_stop_all
	echo "results: $results"
}
trap cleanup EXIT

want() { [[ $# -eq 0 || " ${areas[*]} " == *" $1 "* ]]; }
areas=("$@")
[[ ${#areas[@]} -eq 0 ]] && areas=(setup lifecycle connect inside agents tokens dashboard browser sdk node)
echo "Cloudex verify: $("$CX" version --json 2>/dev/null | jq -r '.version // empty' 2>/dev/null) against $API_URL, prefix $P, areas: ${areas[*]}${VERIFY_NODE:+, sandboxes on node $VERIFY_NODE}"
if on_node && [[ $("$CX" node info "$VERIFY_NODE" --json 2>/dev/null | jq -r .state) != online ]]; then
	echo "VERIFY_NODE=$VERIFY_NODE isn't an online node of this deployment (cloudex node list)" >&2
	exit 2
fi

A=$P-a
need_a() {
	[[ -n ${A_ID:-} ]] && return
	cx new --name "$A" ${ON[@]+"${ON[@]}"}
	A_ID=$(field .id)
}

# ---------------------------------------------------------------- setup
if want setup; then
	cx version
	ok S01 "version prints a version" has "$OUT$ERR" "v"
	cx status
	ok S02 "status names the API and its version" [ "$(field .apiUrl)" = "$API_URL" -a -n "$(field .apiVersion)" ]
	cx doctor
	ok S03 "doctor passes" [ "$CODE" = 0 ]
	bad=""
	for c in $("$CX" --help | awk '/^  [a-z-]+  /{print $1}'); do
		"$CX" "$c" --help >/dev/null 2>&1 || bad="$bad $c"
	done
	ok S04 "every command's --help works" [ -z "$bad" ]
	cx nosuchcommand
	ok S05 "an unknown command fails and says so" [ "$CODE" != 0 -a -n "$ERR" ]
	key=$(jq -r .apiKey "$src_cfg/config.json")
	second=$work/second
	if printf '%s\n' "$key" | CLOUDEX_CONFIG_DIR=$second "$CX" login --url "$API_URL" >/dev/null 2>&1 &&
		CLOUDEX_CONFIG_DIR=$second "$CX" status --json 2>/dev/null | jq -e .apiVersion >/dev/null; then
		pass S06 "login on a second computer with a piped key"
	else
		fail S06 "login on a second computer with a piped key" "status failed in the new config dir"
	fi
	printf '%s\n' wrong | CLOUDEX_CONFIG_DIR=$work/third "$CX" login --url "$API_URL" >/dev/null 2>"$work/e"
	CODE=$? ERR=$(cat "$work/e")
	ok S07 "login with a wrong key fails clearly" [ "$CODE" != 0 ]
	cx feedback "verify-release dry run, please ignore"
	ok S08 "feedback without a terminal sends nothing and offers --send" has "$OUT$ERR" "--send"
	cx update --check
	ok S09 "update --check answers" [ "$CODE" = 0 ]
	cx info nosuchsandbox-$P
	ok S10 "a wrong sandbox name says so" has "$ERR" "no sandbox named"
	code=$(curl -s -o "$work/b" -w '%{http_code}' -X POST "$API_URL/v1/sandboxes" -H "authorization: Bearer $(jq -r .apiKey "$CLOUDEX_CONFIG_DIR/config.json")" -d '{')
	ok S13 "the API refuses malformed JSON on create with a 400" [ "$code" = 400 ]
	code=$(curl -s -o "$work/b" -w '%{http_code}' "$API_URL/v1/no-such-route" -H "authorization: Bearer $(jq -r .apiKey "$CLOUDEX_CONFIG_DIR/config.json")")
	ok S14 "an unknown route is a JSON 404 the clients can tell from a missing sandbox" [ "$code" = 404 -a "$(jq -r .error "$work/b")" = "not found" ]
	cx info cx-zzzzzz
	ok S15 "a missing sandbox id is named in the error" has "$ERR" "cx-zzzzzz"
	cx --version
	ok S11 "cloudex --version works" [ "$CODE" = 0 ]
	if printf '%s\n' "$key" | CLOUDEX_CONFIG_DIR=$work/fourth "$CX" login --url "${API_URL#https://}" >/dev/null 2>&1; then
		pass S12 "login accepts an API URL without https://"
	else
		fail S12 "login accepts an API URL without https://" "login failed"
	fi
fi

# ---------------------------------------------------------------- lifecycle
if want lifecycle; then
	echo "file-content-$P" >"$work/dotfile"
	cx new --name "$A" --env VR_ONE=1 --file ".vr/dot=$work/dotfile" --repo https://github.com/octocat/Hello-World ${ON[@]+"${ON[@]}"}
	A_ID=$(field .id)
	ok L01 "new starts a running sandbox with the name${VERIFY_NODE:+, on node $VERIFY_NODE}" [ "$(field .state)" = running -a "$(field .name)" = "$A" -a "$(field '.placement.node.name // "cloudflare"')" = "${VERIFY_NODE:-cloudflare}" ]
	cx list
	ok L02 "list shows it" [ "$(jq -r --arg n "$A" '[.[] | select(.name == $n)] | length' <<<"$OUT")" = 1 ]
	cx exec "$A" -- 'echo $VR_ONE; cat ~/.vr/dot; test -f ~/Hello-World/README && echo repo'
	ok L03 "--env, --file and --repo reach the sandbox" is "$OUT" "$(printf '1\nfile-content-%s\nrepo' "$P")"
	cx env set "$A" VR_TWO=2
	cx exec "$A" -- 'echo $VR_TWO'
	ok L04 "env set reaches a running sandbox at once" is "$OUT" 2
	cx env unset "$A" VR_TWO
	cx env "$A"
	ok L05 "env unset removes it, env lists names only" [ "$(field '.env | join(",")')" = VR_ONE ]
	cx new --name "$A" ${ON[@]+"${ON[@]}"}
	ok L06 "new with a taken name fails" [ "$CODE" != 0 ]
	cx set "$A" --idle 2h
	ok L07 "set --idle" [ "$(field .idleMinutes)" = 120 ]
	cx set "$A" --name "$A-renamed"
	cx info "$A-renamed"
	ok L08 "set --name renames" [ "$(field .id)" = "$A_ID" ]
	cx set "$A-renamed" --name "$A"
	cx use "$A"
	cx status
	ok L09 "use makes it current" has "$OUT" "$A_ID"
	cx exec "$A" -- 'mkdir -p ~/.cloudex && echo "touch /tmp/vr-started" >~/.cloudex/start && echo kept >~/vr-home && sudo sh -c "echo kept >/opt/vr-sys"'
	cx stop "$A"
	if on_node; then
		# On a node the disk stays there when it stops: no save, no backup (L13 checks the disk).
		ok L10 "stop on a node stops it, keeping its disk there ($NOT_ON_NODES: saves and backups on stop)" [ "$(field .state)" = stopped -a "$(field .savedBytes)" = null ]
	else
		ok L10 "stop saves and backs up" [ "$(field .state)" = stopped -a "$(field .savedBytes)" != null -a "$(field .backupError)" = null ]
	fi
	cx exec "$A" -- true
	ok L11 "exec on a stopped sandbox says to resume" [ "$CODE" = 1 -a "$(has "$ERR" resume && echo y)" = y ]
	cx resume "$A"
	ok L12 "resume runs it again" [ "$(field .state)" = running ]
	cx exec "$A" -- 'cat ~/vr-home /opt/vr-sys; test -f /tmp/vr-started && echo started; echo $VR_ONE'
	ok L13 "resume keeps the disk, runs ~/.cloudex/start, keeps env" is "$OUT" "$(printf 'kept\nkept\nstarted\n1')"
	if on_node; then
		# Refused, as they should be (N34 checks the messages): then SKIP, saying why.
		# What follows uses $P-f-1 and $P-s, which are made on the node instead.
		cx fork "$A" --count 2 --name "$P-f"
		if [[ $CODE == 0 ]]; then fail L14 "a fork on a node is refused" "it was made: ${OUT:0:200}"; else
			skip L14 "fork --count 2 makes two named forks" "$NOT_ON_NODES: forks (refused: $(grep -m1 "^error:" <<<"$ERR" | cut -c8-127))"
			skip L15 "a fork has the disk and settings" "$NOT_ON_NODES: forks"
		fi
		# Named saves (M6): made on the node, and started from on it.
		cx save "$A" "$P-base"
		ok L16 "save makes a named save, and says it was made on the node" [ "$(field .name)" = "$P-base" -a "$(field .replaced)" = false -a "$(field .node.name)" = "$VERIFY_NODE" ]
		cx save "$A" "$P-base"
		ok L17 "saving under a taken name replaces it" [ "$(field .replaced)" = true ]
		cx saves
		ok L18 "saves lists it" [ "$(jq -r --arg n "$P-base" '[.[] | select(.name == $n)] | length' <<<"$OUT")" = 1 ]
		cx new --from "$P-base" --name "$P-s" "${ON[@]}"
		ok L19 "new --from a save, on the node" [ "$(field '.[0].state')" = running -a "$(field '.[0].fromSave')" = "$P-base" -a "$(field '.[0].placement.node.name')" = "$VERIFY_NODE" ]
		cx exec "$P-s" -- 'cat ~/vr-home /opt/vr-sys; echo $VR_ONE'
		ok L20 "a sandbox from a save has the disk and settings" is "$OUT" "$(printf 'kept\nkept\n1')"
		for c in "L21 resume --size changes the size" "L22 resume --from-backup says what it restored" "L23 the backup brings /home back and nothing else"; do
			skip "${c%% *}" "${c#* }" "$NOT_ON_NODES: sizes and backups are Cloudflare's; resume --size and --from-backup refuse with a 400 (N34)"
		done
		"$CX" new --name "$P-f-1" --no-use "${ON[@]}" >/dev/null 2>&1
	else
		cx fork "$A" --count 2 --name "$P-f"
		ok L14 "fork --count 2 makes two named forks" [ "$(field 'map(.name) | sort | join(",")')" = "$P-f-1,$P-f-2" -a "$(field '.[0].forkedFrom')" = "$A_ID" ]
		cx exec "$P-f-1" -- 'cat ~/vr-home /opt/vr-sys; echo $VR_ONE'
		ok L15 "a fork has the disk and settings" is "$OUT" "$(printf 'kept\nkept\n1')"
		cx save "$A" "$P-base"
		ok L16 "save makes a named save" [ "$(field .name)" = "$P-base" -a "$(field .replaced)" = false ]
		cx save "$A" "$P-base"
		ok L17 "saving under a taken name replaces it" [ "$(field .replaced)" = true ]
		cx saves
		ok L18 "saves lists it" [ "$(jq -r --arg n "$P-base" '[.[] | select(.name == $n)] | length' <<<"$OUT")" = 1 ]
		cx new --from "$P-base" --name "$P-s"
		ok L19 "new --from a save" [ "$(field '.[0].state')" = running -a "$(field '.[0].fromSave')" = "$P-base" ]
		cx exec "$P-s" -- 'cat ~/vr-home /opt/vr-sys; echo $VR_ONE'
		ok L20 "a sandbox from a save has the disk and settings" is "$OUT" "$(printf 'kept\nkept\n1')"
		cx stop "$P-f-2"
		cx resume "$P-f-2" --size standard-4
		cx exec "$P-f-2" -- nproc
		ok L21 "resume --size changes the size" is "$OUT" 4
		cx stop "$P-f-2"
		cx resume "$P-f-2" --from-backup
		ok L22 "resume --from-backup says what it restored" has "$(field .notice)" "backup"
		cx exec "$P-f-2" -- 'cat ~/vr-home; test -e /opt/vr-sys && echo has-sys || echo no-sys'
		ok L23 "the backup brings /home back and nothing else" is "$OUT" "$(printf 'kept\nno-sys')"
	fi
	cx delete "$P-s" --yes
	cx info "$P-s"
	ok L24 "delete removes it" [ "$CODE" != 0 ]
	cx saves rm "$P-base" --yes
	cx saves
	ok L25 "saves rm deletes the save" [ "$(jq -r --arg n "$P-base" '[.[] | select(.name == $n)] | length' <<<"$OUT")" = 0 ]
	cx new --from "$P-nosave" --name "$P-x"
	ok L26 "new --from something missing fails clearly" [ "$CODE" != 0 ]
	cx new --name "$P-cred" --repo "https://user:token@github.com/octocat/Hello-World" ${ON[@]+"${ON[@]}"}
	ok L29 "new refuses a repo URL with a token in it and points to GH_TOKEN" [ "$CODE" != 0 -a "$(has "$ERR" GH_TOKEN && echo y)" = y ]
	cx set "$P-f-1" --idle 1m
	cx exec "$P-f-1" --timeout 5m -- 'sleep 150; echo alive'
	quiet=$OUT
	cx info "$P-f-1"
	ok L30 "a quiet foreground exec keeps a sandbox from idling" [ "$quiet" = alive -a "$(field .state)" = running ]
	cx set "$P-f-1" --idle 30m
	echo f >"$work/ef"
	cx env set "$P-f-1" --file ".vr-e=$work/ef"
	cx env unset "$P-f-1" --file .vr-e
	cx env "$P-f-1"
	ok L27 "env unset <sandbox> --file removes the file from that sandbox" [ "$CODE" = 0 -a "$(field '.files | index("~/.vr-e")')" = null ]
	cx resume "$P-f-1" --from-backup
	if on_node; then
		skip L28 "resume --from-backup on a running sandbox refuses" "$NOT_ON_NODES: backups; it refuses on a node whatever the state ($(grep -m1 "^error:" <<<"$ERR" | cut -c8-107))"
	else
		ok L28 "resume --from-backup on a running sandbox refuses" [ "$CODE" != 0 ]
	fi
fi

# ---------------------------------------------------------------- connect
if want connect; then
	need_a
	cx exec "$A" -- 'echo out; echo err >&2; exit 3'
	ok C01 "exec passes stdout, stderr and the exit code" [ "$CODE" = 3 -a "$OUT" = out -a "$ERR" = err ]
	cx exec "$A" --root -- whoami
	ok C02 "exec --root" is "$OUT" root
	cx exec "$A" --cwd /tmp -- pwd
	ok C03 "exec --cwd" is "$OUT" /tmp
	cx exec "$A" --timeout 2s -- 'sleep 10'
	ok C04 "exec --timeout stops the command with 124" [ "$CODE" = 124 ]
	cx exec "$A" -- python3 -c 'print("a b")'
	ok C05 "exec quotes several arguments" is "$OUT" "a b"
	n=$("$CX" exec "$A" -- 'seq 1 200000' 2>/dev/null | wc -l | tr -d ' ')
	ok C06 "exec streams large output without losing lines" is "$n" 200000
	got=$(printf 'from-stdin\n' | "$CX" exec "$A" -- 'cat; echo done' 2>/dev/null)
	ok C07 "exec with piped stdin doesn't hang, and its help says stdin isn't sent" [ "$got" = done -a -n "$("$CX" exec --help | grep -i 'standard input')" ]
	cx exec "$A" --cwd vr-no-such-dir -- pwd
	ok C07b "exec --cwd with a missing directory says so" has "$ERR" "no directory"
	cx exec -d "$A" -- 'for i in 1 2 3; do echo line $i; sleep 1; done; exit 7'
	cid=$(field .id)
	ok C08 "exec -d prints a command id" has "$cid" cm-
	cx commands "$A"
	ok C09 "commands lists it" [ "$(jq -r --arg c "$cid" '[.[] | select(.id == $c)] | length' <<<"$OUT")" = 1 ]
	cx commands logs -f "$A" "$cid"
	ok C10 "commands logs -f follows to the end with its exit code" [ "$CODE" = 7 -a "$(printf '%s' "$OUT" | wc -l | tr -d ' ')" -ge 2 ]
	cx exec -d "$A" -- 'sleep 600'
	kid=$(field .id)
	cx commands kill "$A" "$kid"
	cx commands "$A"
	ok C11 "commands kill stops it" [ "$(jq -r --arg c "$kid" '.[] | select(.id == $c) | .state' <<<"$OUT")" != running ]
	mkdir -p "$work/tree/sub" && echo one >"$work/tree/a.txt" && echo two >"$work/tree/sub/b.txt"
	cx cp "$work/tree" "$A:vr-tree"
	cx cp "$A:vr-tree" "$work/back"
	ok C12 "cp copies a directory in and out" diff -r "$work/tree" "$work/back"
	cx cp "$A:vr-tree/a.txt" -
	ok C13 "cp to - prints the file" is "$OUT" one
	cx cp "$A:vr-missing" "$work/x"
	ok C14 "cp of a missing file fails" [ "$CODE" != 0 ]
	got=$("$CX" ssh "$A" -- 'echo via-ssh' </dev/null 2>&1)
	ok C15 "ssh runs a command" is "$got" via-ssh
	"$CX" ssh-config >"$work/ssh_config" 2>/dev/null
	echo scp-me >"$work/scp-me"
	if scp -F "$work/ssh_config" -o BatchMode=yes -q "$work/scp-me" "$A.cloudex:vr-scp" 2>"$work/e"; then
		cx exec "$A" -- cat vr-scp
		ok C16 "ssh-config lets scp reach <name>.cloudex" is "$OUT" scp-me
	else
		fail C16 "ssh-config lets scp reach <name>.cloudex" "$(cat "$work/e")"
	fi
	cx new --name "$P-reuse" --no-use ${ON[@]+"${ON[@]}"}
	ssh -F "$work/ssh_config" -o BatchMode=yes "$P-reuse.cloudex" true >/dev/null 2>&1
	cx delete "$P-reuse" --yes
	cx new --name "$P-reuse" --no-use ${ON[@]+"${ON[@]}"}
	got=$(ssh -F "$work/ssh_config" -o BatchMode=yes -o LogLevel=ERROR "$P-reuse.cloudex" 'echo reached' 2>&1)
	ok C17 "ssh-config reaches a new sandbox that reuses a name" is "$got" reached
	cx delete "$P-reuse" --yes
	if [[ -n $ZONE ]]; then
		cx exec -d "$A" -- 'mkdir -p /tmp/www && echo served >/tmp/www/index.html && cd /tmp/www && exec python3 -m http.server 8123 --bind 127.0.0.1'
		sleep 2
		cx host "$A" 8123
		url=$(field .url)
		ok C18 "host links a localhost server" [ "$(curl -s --max-time 30 "$url")" = served ]
		cx exec -d "$A" -- 'exec python3 -c "
import http.server
class H(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        b = self.rfile.read(int(self.headers.get(\"content-length\", 0)))
        self.send_response(200); self.end_headers(); self.wfile.write(b\"got \" + b)
http.server.HTTPServer((\"127.0.0.1\", 8127), H).serve_forever()"'
		sleep 2
		cx host "$A" 8127
		ok C18b "the first POST to a localhost server through a new link works" [ "$(curl -s --max-time 30 -X POST -d hello "$(field .url)")" = "got hello" ]
		cx host "$A" --list
		ok C19 "host --list shows the link" has "$OUT" "$url"
		cx host "$A" 8123 --revoke
		ok C20 "host --revoke ends the link" [ "$(http "$url")" = 404 ]
		cx host "$A" 8123
		url2=$(field .url)
		cx host "$A" --revoke-all
		ok C21 "host --revoke-all ends every link" [ "$(http "$url2")" = 404 -a "$url" != "$url2" ]
		cx desktop "$A" --link
		durl=$(field .url)
		ok C22 "desktop --link serves noVNC" [ "$(http "$durl")" = 200 ]
		cx desktop "$A" --link --size 1024x768
		cx desktop "$A" --link
		ok C22b "desktop again without --size keeps the size" is "$(field .size)" 1024x768
		cx desktop "$A" --stop
		ok C23 "desktop --stop" [ "$(field .stopped)" = true ]
	else
		skip C18 "host and desktop links" "no preview domain on this deployment"
	fi
	before=$("$CX" list --json 2>/dev/null | jq length)
	run=$work/run
	mkdir -p "$run" && (cd "$run" && git init -q && echo 'print("ran")' >t.py && echo x >skip.log && echo '*.log' >.gitignore && git add -A && git -c user.email=v@v -c user.name=v commit -qm x)
	(cd "$run" && "$CX" run ${ON[@]+"${ON[@]}"} -- 'python3 t.py; test ! -e skip.log && echo no-ignored; exit 5' >"$work/o" 2>"$work/e")
	CODE=$? OUT=$(cat "$work/o") ERR=$(cat "$work/e")
	ok C24 "run copies what git would commit and exits with the code" [ "$CODE" = 5 -a "$OUT" = "$(printf 'ran\nno-ignored')" ]
	(cd "$run" && "$CX" run ${ON[@]+"${ON[@]}"} --out out -- 'mkdir out && echo built >out/r.txt' >"$work/o" 2>"$work/e")
	CODE=$?
	ok C25 "run --out copies results back" [ "$CODE" = 0 -a "$(cat "$run/out/r.txt" 2>/dev/null)" = built ]
	(cd "$run" && "$CX" run ${ON[@]+"${ON[@]}"} --out cov/deep/lcov.info -- 'mkdir -p cov/deep && echo lcov >cov/deep/lcov.info' >"$work/o" 2>"$work/e")
	CODE=$? ERR=$(cat "$work/e")
	ok C27 "run --out of a nested file makes the local directories" [ "$CODE" = 0 -a "$(cat "$run/cov/deep/lcov.info" 2>/dev/null)" = lcov ]
	ln -sfn "$work/tree" "$work/link"
	cx cp "$work/link" "$A:vr-link"
	cx exec "$A" -- 'cat vr-link/a.txt'
	ok C28 "cp of a symlinked directory copies what it points to" is "$OUT" one
	cx cp "$A:vr-tree/a.txt" "$work/newdir/a.txt"
	ok C29 "cp into a local directory that doesn't exist yet" [ "$(cat "$work/newdir/a.txt" 2>/dev/null)" = one ]
	cx list
	ok C26 "run deletes its sandboxes" [ "$(jq length <<<"$OUT")" = "$before" ]
fi

# ---------------------------------------------------------------- inside
# On a node (v0.2.2 M6), the cloudex command reaches the API through the
# node's agent (cloudex.internal there), and the same checks hold.
if want inside; then
	need_a
	cx exec "$A" -- 'stat -c %u /'
	ok I07 "/ is owned by root, so tools that check it work" is "$OUT" 0
	cx exec "$A" -- 'cloudex info --json'
	ok I01 "cloudex inside a sandbox knows itself" [ "$(field .id)" = "$A_ID" ]
	cx exec "$A" -- 'cloudex set --idle 45m --json'
	ok I02 "cloudex set --idle inside" [ "$(field .idleMinutes)" = 45 ]
	cx exec "$A" -- 'cloudex set --idle 7h'
	ok I06 "cloudex set --idle over 6h inside says the limit" [ "$CODE" != 0 -a "$(has "$OUT$ERR" "6h" && echo y)" = y ]
	cx exec "$A" -- 'cloudex list'
	ok I03 "cloudex inside refuses other sandboxes clearly" [ "$CODE" != 0 -a -n "$ERR$OUT" ]
	if [[ -n $ZONE ]]; then
		cx exec -d "$A" -- 'mkdir -p /tmp/in && echo inside >/tmp/in/index.html && cd /tmp/in && exec python3 -m http.server 8124'
		sleep 2
		cx exec "$A" -- 'cloudex host 8124 --json'
		ok I04 "cloudex host inside prints a working link" [ "$(curl -s --max-time 30 "$(field .url)")" = inside ]
	fi
	cx exec "$A" -- 'cloudex feedback "verify-release dry run from inside"'
	ok I05 "cloudex feedback inside sends nothing without a terminal" has "$OUT$ERR" "--send"
fi

# ---------------------------------------------------------------- agents
if want agents; then
	cx status
	apiv=$(field .apiVersion)
	# Before v0.1.17 the API stored any key, so this check would replace the real one.
	if [[ $(printf '%s\nv0.1.17\n' "${apiv%%-*}" | sort -V | head -1) == v0.1.17 ]]; then
		before=$("$CX" keys --json 2>/dev/null | jq -c 'map([.provider, .updatedAt])')
		printf 'sk-ant-api03-notarealkey000000000000\n' | "$CX" keys set anthropic >"$work/o" 2>"$work/e"
		CODE=$? OUT=$(cat "$work/o") ERR=$(cat "$work/e")
		after=$("$CX" keys --json 2>/dev/null | jq -c 'map([.provider, .updatedAt])')
		ok A0 "keys set refuses a key the provider rejects, and keeps the stored one" [ "$CODE" != 0 -a "$(has "$ERR" rejected && echo y)" = y -a "$before" = "$after" ]
	else
		skip A0 "keys set refuses a rejected key" "the API is $apiv, which stores any key"
	fi
	cx keys
	providers=$(field 'map(.provider) | join(" ")' 2>/dev/null)
	# On a node (v0.2.2 M6), the agents' provider calls go through the node's
	# agent to the deployment, which adds the key.
	if [[ -z $providers || $providers == null ]]; then
		skip A00 "agents" "no provider keys on the deployment (cloudex keys set anthropic|openai)"
	else
		need_a
		cx new --name "$P-ag" --no-use ${ON[@]+"${ON[@]}"}
		# OpenCode gets a small model: on its own it picks one, often the dearest.
		for pair in "anthropic claude" "openai codex" "anthropic opencode anthropic/claude-haiku-4-5"; do
			set -- $pair
			model=()
			[[ -z ${3:-} ]] || model=(--model "$3")
			[[ " $providers " == *" $1 "* ]] || { skip "A-$2" "$2" "no $1 key"; continue; }
			cx exec "$P-ag" -- "case $1 in anthropic) curl -s -o /dev/null -w '%{http_code}' https://api.anthropic.com/v1/models -H \"x-api-key: \$ANTHROPIC_API_KEY\" -H 'anthropic-version: 2023-06-01';; openai) curl -s -o /dev/null -w '%{http_code}' https://api.openai.com/v1/models -H \"Authorization: Bearer \$OPENAI_API_KEY\";; esac"
			ok "A1-$2" "$1 key works from a sandbox through the placeholder" is "$OUT" 200
			cx exec "$P-ag" -- "env | grep -E '^(ANTHROPIC|OPENAI)_API_KEY=' | grep -cE '=(sk-ant-api|sk-proj-)[A-Za-z0-9_-]{40,}'"
			ok "A2-$2" "the real $1 key is not in the sandbox" is "$OUT" 0
			cx agents start "$P-ag" --agent "$2" ${model[@]+"${model[@]}"} "Reply with exactly the word VERIFYOK and nothing else. Do not run any tools."
			code=$CODE
			cx agents "$P-ag"
			ok "A3-$2" "agents start $2 finishes a turn with the answer" [ "$code" = 0 -a "$(field 'max_by(.updatedAt).lastMessage')" = VERIFYOK ]
			cx agents send "$P-ag" "Now reply with exactly the word you replied with before, then the word SECONDOK, separated by a space."
			cx agents "$P-ag"
			ok "A4-$2" "agents send continues the same conversation" [ "$(field 'max_by(.updatedAt).lastMessage')" = "VERIFYOK SECONDOK" ]
			ok "A5-$2" "agents lists it" [ "$(field 'max_by(.updatedAt).agent')" = "$2" ]
			cx agents logs "$P-ag"
			ok "A6-$2" "agents logs prints both turns" [ "$(grep -c '"type":"end"' <<<"$OUT")" -ge 2 ]
			cx agents start "$P-ag" --agent "$2" ${model[@]+"${model[@]}"} -d "Count from 1 to 100000 in a bash loop, printing each number, then summarize."
			sleep 15
			cx agents interrupt "$P-ag"
			ok "A7-$2" "agents interrupt stops a running turn" [ "$CODE" = 0 ]
		done
	fi
fi

# ---------------------------------------------------------------- tokens
if want tokens; then
	# as SECRET CMD...: the CLI with a token instead of the deployment's key, in a config dir of its own.
	as() {
		local secret=$1
		shift
		CLOUDEX_CONFIG_DIR=$work/tok-$RANDOM CLOUDEX_API_URL=$API_URL CLOUDEX_API_KEY=$secret "$CX" "$@" >"$work/o" 2>"$work/e" </dev/null
		CODE=$? OUT=$(cat "$work/o") ERR=$(cat "$work/e")
	}
	cx status
	if [[ $(field '.me.role // empty') != admin ]]; then
		skip K00 "tokens" "the deployment has no tokens yet, or this config doesn't hold the admin key"
	else
		need_a
		cx tokens create "$P-own" --own --expires 1h
		own=$(field .secret)
		ok K01 "tokens create prints a secret once" has "$own" "cx_"
		cx tokens create "$P-ro" --role read --expires 1h
		ro=$(field .secret)
		own_on=()
		if on_node; then
			iso=$("$CX" node info "$VERIFY_NODE" --json 2>/dev/null | jq -r 'if .isolation.unisolated then "none" else .runtime end')
			if [[ $iso == sysbox ]]; then
				own_on=("${ON[@]}")
			else
				# Decision 4: own-only (CI) tokens stay off plain-Docker nodes; its sandbox goes on Cloudflare.
				as "$own" new --name "$P-own0" --no-use "${ON[@]}"
				ok K02n "an own-only token can't place a sandbox on a $iso node" [ "$CODE" != 0 -a "$(has "$ERR" "own-only token can't place sandboxes on node" && echo y)" = y ]
			fi
		fi
		as "$own" new --name "$P-own1" --no-use ${own_on[@]+"${own_on[@]}"}
		ok K02 "an own-only token makes a sandbox${own_on[*]:+ on node $VERIFY_NODE}" [ "$CODE" = 0 ]
		as "$own" list
		ok K03 "an own-only token lists only its own sandboxes" is "$(field 'map(.name) | join(" ")')" "$P-own1"
		as "$own" info "$A"
		ok K04 "an own-only token can't see another sandbox" has "$ERR" "no sandbox named"
		as "$own" exec "$P-own1" -- echo own-ok
		ok K05 "an own-only token runs commands in its own sandbox" is "$OUT" own-ok
		as "$own" save "$P-own1" "$P-own-save"
		ok K06 "an own-only token can't make a named save" has "$ERR" "named saves"
		as "$own" keys
		ok K07 "a user token can't read provider keys" has "$ERR" "admin"
		as "$ro" info "$A"
		ok K08 "a read token shows a sandbox" is "$(field .name)" "$A"
		as "$ro" exec "$A" -- echo no
		ok K09 "a read token can't run commands" has "$ERR" "only reads"
		as "$own" delete "$P-own1" --yes
		ok K10 "an own-only token deletes its own sandbox" [ "$CODE" = 0 ]
		cx tokens
		id=$(jq -r --arg n "$P-ro" '.[] | select(.name == $n) | .id' <<<"$OUT")
		cx tokens revoke "$id"
		as "$ro" list
		ok K11 "a revoked token stops working at once" has "$ERR" "unauthorized"
		# Device sign-in: a second computer asks, this one approves.
		dev=$work/dev
		mkdir -p "$dev"
		(CLOUDEX_CONFIG_DIR=$dev CLOUDEX_API_KEY= "$CX" login --url "$API_URL" --name "$P-device" --json </dev/null >"$dev/out" 2>"$dev/err"; echo "rc=$?" >>"$dev/out") &
		code=""
		for _ in $(seq 20); do
			code=$(head -1 "$dev/out" 2>/dev/null | jq -r '.userCode // empty' 2>/dev/null)
			[[ -n $code ]] && break
			sleep 0.5
		done
		cx tokens approve "$code" --yes --role read --expires 1h
		ok K12 "tokens approve approves a device sign-in" [ "$CODE" = 0 ]
		for _ in $(seq 30); do grep -q '^rc=' "$dev/out" && break; sleep 0.5; done
		OUT=$(CLOUDEX_CONFIG_DIR=$dev CLOUDEX_API_KEY= CLOUDEX_API_URL= "$CX" status --json 2>/dev/null)
		ok K13 "the device signed in with a read token of its own" [ "$(field .me.token.name)" = "$P-device" -a "$(field .me.role)" = read ]
	fi
fi

# ---------------------------------------------------------------- dashboard
if want dashboard; then
	key=$(jq -r .apiKey "$CLOUDEX_CONFIG_DIR/config.json")
	curl -s -D "$work/h" -o "$work/b" --max-time 30 "$API_URL/"
	csp=$(grep -ci "^content-security-policy: default-src 'self'; script-src 'self';" "$work/h")
	nostore=$(grep -ci '^cache-control: no-store' "$work/h")
	ok D01 "the API URL serves the dashboard page, uncached, with a strict CSP" [ "$csp" = 1 -a "$nostore" = 1 ]
	js=$(grep -o '/assets/app\.[0-9a-f]*\.js' "$work/b" | head -1)
	ok D02 "the dashboard's script is served and cached for good" \
		[ -n "$js" -a "$(curl -s -o /dev/null -w '%{http_code}' "$API_URL$js")" = 200 ]
	cx dashboard --json
	link=$(field .url)
	ok D03 "cloudex dashboard --json makes a one-time sign-in link" has "$link" "/login#link="
	lc=$(sed -E 's/.*link=([a-z2-7]+).*/\1/' <<<"$link")
	signin() { curl -s -D "$work/h" -o "$work/b" -w '%{http_code}' --max-time 30 -X POST -H 'content-type: application/json' -H 'x-cloudex: 1' -H "origin: $API_URL" -d "$1" "$API_URL/v1/auth/session"; }
	code=$(signin "{\"link\":\"$lc\"}")
	cookie=$(grep -i '^set-cookie: __Host-cx_session=' "$work/h" | sed -E 's/^[^:]*: (__Host-cx_session=[^;]+).*/\1/' | tr -d '\r')
	sid=$(jq -r '.token.id // empty' "$work/b")
	ok D04 "the link signs a browser in with an HttpOnly, SameSite=Strict session cookie" \
		[ "$code" = 200 -a -n "$cookie" -a -n "$(grep -i '^set-cookie:.*HttpOnly; SameSite=Strict' "$work/h")" ]
	ok D05 "a sign-in link works only once" [ "$(signin "{\"link\":\"$lc\"}")" = 401 ]
	cc() { curl -s -o "$work/b" -w '%{http_code}' --max-time 30 -H "cookie: $cookie" "$@"; }
	ok D06 "the session cookie works from the dashboard, with X-Cloudex: 1" [ "$(cc -H 'x-cloudex: 1' "$API_URL/v1/me")" = 200 ]
	ok D07 "the cookie alone, or from another site, is refused" \
		[ "$(cc "$API_URL/v1/me")" = 403 -a "$(cc -H 'x-cloudex: 1' -H 'origin: https://example.com' "$API_URL/v1/me")" = 403 ]
	"$CX" tokens --json >"$work/o" 2>/dev/null
	ok D08 "the browser's session is listed with the tokens" [ -n "$sid" -a "$(jq -r --arg id "$sid" '.[] | select(.id == $id) | .kind' "$work/o")" = session ]
	# A link made from a session would outlive it, signed out or revoked.
	ok D15 "a dashboard session can't make a sign-in link" \
		[ "$(cc -X POST -H 'x-cloudex: 1' -H "origin: $API_URL" -H 'content-type: application/json' -d '{}' "$API_URL/v1/auth/links")" = 403 ]
	cc -X DELETE -H 'x-cloudex: 1' -H "origin: $API_URL" "$API_URL/v1/auth/session" >/dev/null
	ok D09 "signing out ends the session" [ "$(cc -H 'x-cloudex: 1' "$API_URL/v1/me")" = 401 ]
	cx tokens create "$P-dash-read" --role read
	rs=$(field .secret)
	code=$(signin "{\"token\":\"$rs\"}")
	cookie=$(grep -i '^set-cookie: __Host-cx_session=' "$work/h" | sed -E 's/^[^:]*: (__Host-cx_session=[^;]+).*/\1/' | tr -d '\r')
	ok D10 "a pasted read token gives a read session, which can't make a sandbox" \
		[ "$code" = 200 -a "$(jq -r .role "$work/b")" = read -a "$(cc -X POST -H 'x-cloudex: 1' -H 'content-type: application/json' -d '{}' "$API_URL/v1/sandboxes")" = 403 ]
	cx tokens revoke "$(field .id)"
	ok D11 "revoking a token ends the sessions it signed in" [ "$(cc -H 'x-cloudex: 1' "$API_URL/v1/me")" = 401 ]
	# Device sign-ins need no key, so one address may keep only three waiting.
	# The first asks for admin, which an approval that names no role must not give.
	devs=() dcodes=()
	for i in 1 2 3 4; do
		role=user
		[[ $i == 1 ]] && role=admin
		dc=$(curl -s -o "$work/d$i" -w '%{http_code}' --max-time 30 -X POST -H 'content-type: application/json' \
			-d "{\"name\":\"$P-dev-$i\",\"role\":\"$role\"}" "$API_URL/v1/auth/device")
		dcodes+=("$dc")
		[[ $dc == 200 ]] && devs+=("$i")
	done
	ok D16 "a fourth sign-in waiting from one address is refused with a 429" is "${dcodes[*]}" "200 200 200 429"
	admin_code=$(jq -r '.user_code // empty' "$work/d1" 2>/dev/null)
	ap() { curl -s -o "$work/b" -w '%{http_code}' --max-time 30 -X POST -H "authorization: Bearer $key" -H 'content-type: application/json' -d "$2" "$API_URL/v1/auth/device/$1"; }
	poll_dev() { curl -s -o "$work/t" --max-time 30 -X POST -H 'content-type: application/json' -d "{\"device_code\":\"$(jq -r .device_code "$work/d$1")\"}" "$API_URL/v1/auth/token"; }
	# A denied one stops waiting at once, though its computer hasn't asked for the answer yet.
	ap deny "{\"user_code\":\"$(jq -r '.user_code // empty' "$work/d3")\"}" >/dev/null
	dc=$(curl -s -o "$work/d5" -w '%{http_code}' --max-time 30 -X POST -H 'content-type: application/json' \
		-d "{\"name\":\"$P-dev-5\"}" "$API_URL/v1/auth/device")
	[[ $dc == 200 ]] && devs+=(5)
	OUT="the next sign-in got $dc" CODE=""
	ok D18 "a denied sign-in its computer hasn't collected no longer holds a place" is "$dc" 200
	granted="" got=""
	if [[ -n $admin_code && $(ap approve "{\"user_code\":\"$admin_code\",\"expiresInSec\":3600}") == 200 ]]; then
		granted=$(jq -r '.role // empty' "$work/b")
		poll_dev 1
		got=$(jq -r '.token.role // empty' "$work/t")
		tid=$(jq -r '.token.id // empty' "$work/t")
		[[ -n $tid ]] && "$CX" tokens revoke "$tid" >/dev/null 2>&1
	fi
	OUT="approved as ${granted:-?}, token ${got:-none}" CODE=""
	ok D17 "a sign-in that asked for admin, approved without a role, gets user" [ "$granted" = user -a "${got:-}" = user ]
	# Deny the others, and collect the answer so nothing is left waiting.
	for i in "${devs[@]}"; do
		[[ $i == 1 ]] && continue
		ap deny "{\"user_code\":\"$(jq -r .user_code "$work/d$i")\"}" >/dev/null # 404 for 3, denied already
		poll_dev "$i"
	done
	need_a
	tc() { CLOUDEX_API_KEY=$key node "$here/term-check.mjs" "$API_URL" "$A_ID" "$1" >"$work/o" 2>&1; CODE=$?; OUT=$(cat "$work/o"); }
	tc echo
	ok D12 "the terminal WebSocket runs a shell as cloudex at the asked size, and sends its exit code" [ "$CODE" = 0 ]
	tc tmux
	ok D13 "a terminal in a tmux session gets the same shell back after reconnecting" [ "$CODE" = 0 ]
	tc dead
	ok D14 "a TCP port nothing listens on is refused, not opened" [ "$CODE" = 0 ]
fi

# ---------------------------------------------------------------- browser
# The dashboard in headless Chrome (or Firefox), as a user goes through it:
# browser/run.mjs signs in with cloudex dashboard, makes its own $P-<browser>
# sandbox and ends every session and token it starts. Its checks are B01 on.
if want browser; then
	for b in ${VERIFY_BROWSERS:-chrome}; do
		up=$(tr '[:lower:]' '[:upper:]' <<<"$b")
		if ! command -v node >/dev/null; then
			skip "B-$b" "the dashboard in $b" "node is needed"
			continue
		fi
		if ! node "$here/browser/run.mjs" --find "$b" >/dev/null 2>&1; then
			skip "B-$b" "the dashboard in $b" "no $b found; set $up to its path"
			continue
		fi
		# The agent turn spends provider tokens, as the agents area does: it runs with that area.
		ag=${VERIFY_AGENTS:-}
		want agents && ag=1
		keep=()
		[[ ${VERIFY_KEEP:-} == 1 ]] && keep=(--keep)
		before=$(grep -c $'\tFAIL\t' "$results")
		CLOUDEX_BIN=$CX VERIFY_PREFIX=$P VERIFY_AGENTS=$ag node "$here/browser/run.mjs" --browser "$b" \
			--results "$results" --out "$out/browser-$b" ${keep[@]+"${keep[@]}"} </dev/null
		rc=$?
		after=$(grep -c $'\tFAIL\t' "$results")
		fails=$((fails + after - before))
		[[ $rc != 0 && $after == "$before" ]] && fail "B-$b" "the browser checks in $b ran to the end" "run.mjs exited $rc"
	done
fi

# ---------------------------------------------------------------- node
# Nodes (v0.2.2): the agent, cloudex-node, joins with a token and keeps its
# control socket open (M2, N01-N08); streams and exec over the link (M3,
# N09-N16); the Docker runtime, the firewall, the self-test and the installer
# (M4, N17-N25); sandboxes on nodes, offline, reconnects and removal (M5,
# N26-N35); cloudex.internal, provider keys, the node CA, saves and moving,
# and a hostile machine (M6, N36-N44), all through ordinary sandboxes
# (cloudex new --on). It runs on the
# host VERIFY_NODE_SSH reaches, as the
# installer sets it up: scripts/node-install.sh, as root with
# VERIFY_NODE_SUDO=1 (passwordless sudo there), else --unisolated (testing
# only: no firewall, and the root-only checks are a SKIP). The agent's image
# is VERIFY_NODE_AGENT_IMAGE if the host has it, else built there from this
# checkout. The installer's --uninstall runs at the end, and N25 compares the
# host with how it was before.
NODE_DIR=""
NODE_UP=""
node_sh() { "${NODE_SSH[@]}" "$*" </dev/null; }
node_stop_all() {
	if [[ -n $NODE_UP ]]; then
		node_sh "$NODE_SUDO bash $NODE_DIR/node-install.sh --uninstall --yes --keep-images" >/dev/null 2>&1
		NODE_UP=""
	fi
	[[ -n ${NODE_BUILT:-} ]] && node_sh "docker rmi $AIMG" >/dev/null 2>&1
	node_sh "rm -rf $NODE_DIR" >/dev/null 2>&1
}
now_s() { perl -MTime::HiRes=time -e 'printf "%.2f", time'; }
since() { awk -v a="$1" -v b="$(now_s)" 'BEGIN { printf "%.2f", b - a }'; }
node_state() { "$CX" node list --json 2>/dev/null | jq -r --arg n "$1" '.[] | select(.name == $n) | .state'; }
# wait_state NAME STATE SECONDS: polls until the node is in STATE; sets WAITED.
wait_state() {
	local t0
	t0=$(now_s)
	while :; do
		[[ $(node_state "$1") == "$2" ]] && { WAITED=$(since "$t0"); return 0; }
		[[ $(awk -v w="$(since "$t0")" -v m="$3" 'BEGIN { print (w > m) }') == 1 ]] && { WAITED=$(since "$t0"); return 1; }
		sleep 0.25
	done
}
# wait_iso T: waits (up to 3 minutes) for the node's self-test to have a
# result from after T (ms), as when the agent restarts; prints the firewall.
wait_iso() {
	local f
	for _ in $(seq 1 90); do
		f=$("$CX" node info "$N" --json 2>/dev/null | jq -r --argjson t "$1" 'select((.isolation.checkedAt // 0) > $t and .isolation.firewall != "unchecked") | .isolation.firewall')
		[[ -n $f ]] && { echo "$f"; return 0; }
		sleep 2
	done
	return 1
}
# The host's view, for N25: Docker's objects without the build.cloudex labels, and the firewall.
host_snapshot() {
	node_sh "docker ps -a --format '{{.Names}}|{{.Labels}}' | awk -F'|' '\$2 !~ /build[.]cloudex[.]/ {print \$1}' | sort;
		echo ---; docker network ls --format '{{.Name}}|{{.Labels}}' | awk -F'|' '\$2 !~ /build[.]cloudex[.]/ {print \$1}' | sort;
		echo ---; docker volume ls --format '{{.Name}}|{{.Labels}}' | awk -F'|' '\$2 !~ /build[.]cloudex[.]/ {print \$1}' | sort;
		echo ---; ${NODE_SUDO:+sudo nft list tables 2>/dev/null | sort}"
}
M3_CHECKS=("N09 exec over the link: exit codes, stdout and stderr, the user, cwd, and a round trip p50 under 150 ms"
	"N10 data each way: 256 MiB with scp, 90 MiB with cp, and a preview's body and WebSocket, unchanged"
	"N11 a reader at 2.5 MiB/s keeps the machine's output within 16 MiB of it, and keystrokes in another ssh session stay under 200 ms"
	"N12 an idle ssh session holds for 30 minutes, and its Sandbox object is active under 60 s in that time"
	"N13 a deploy during a busy scp ends the machine's side of it when it ends the client's"
	"N14 exec --timeout 2s -- sleep 10 exits 124, and the sleep is gone"
	"N15 the dashboard terminal on a node: a shell and its exit code, tmux across reconnects, resize, htop and vim"
	"N16 nothing is left on the machine after a terminal closes, a client drops an exec, or the agent dies mid-exec; and a stream ticket works once")
M4_CHECKS=("N17 the installer, as root: the firewall unit is loaded, the agent comes online and passes its self-test, a throwaway machine boots, and node info shows the runtime, the isolation and the image"
	"N18 from inside a machine: the internet and DNS answer; the host's addresses, its gateway and DNS, Docker's bridges, the tailnet range, metadata, another machine and the agent's other addresses don't"
	"N19 with route-changing rights (the self-test's NET_ADMIN probe, now): routes through the agent reach nothing, a forged source address finishes no handshake, and the agent takes nothing from another network for a machine"
	"N20 a machine's container has exactly its flags and limits (docker inspect); inside, root has Docker's default capabilities less NET_RAW, MKNOD and SETFCAP, no Docker socket, read-only /proc/sys and /sys, no devices, raw sockets or mknod, no IPv6 outside lo; and a restart gives it a new start id"
	"N21 the firewall survives systemctl restart docker: its table is loaded, the self-test passes, the sandbox shows stopped, and once resumed it reaches the internet"
	"N22 a restart of the agent leaves its machines running, and exec works again within 10 s"
	"N23 after the host reboots: the firewall is loaded, the agent is back with its self-test passing, the machine and its sandbox are stopped, with its disk, and it resumes with its files"
	"N24 a node whose self-test fails refuses new machines and says why; with --unisolated (testing only) it runs them and shows isolation none"
	"N25 nothing on the host changed outside the build.cloudex labels, and the uninstaller removes the agent, its machines, networks, volume and firewall")
m3_skip() { for c in "${M3_CHECKS[@]}"; do skip "${c%% *}" "${c#* }" "$1"; done; }
m4_skip() { for c in "${M4_CHECKS[@]}"; do [[ -z $2 || " $2 " == *" ${c%% *} "* ]] && skip "${c%% *}" "${c#* }" "$1"; done; }
m3_desc() { for c in "${M3_CHECKS[@]}" "${M4_CHECKS[@]}"; do [[ ${c%% *} == "$1" ]] && echo "${c#* }"; done; }
now_ms() { perl -MTime::HiRes=time -e 'printf "%d", time * 1000'; }
# Active seconds of one Durable Object from $1 to $2 (ISO times), from the GraphQL analytics.
do_active() {
	local acc q
	cf_fresh
	acc=$(cf_account)
	q='query($acc:String!,$from:Time!,$to:Time!,$ids:[String!]){viewer{accounts(filter:{accountTag:$acc}){durableObjectsPeriodicGroups(limit:1000,filter:{datetimeMinute_geq:$from,datetimeMinute_leq:$to,objectId_in:$ids}){sum{activeTime}}}}}'
	jq -n --arg q "$q" --arg acc "$acc" --arg from "$1" --arg to "$2" --arg id "$3" '{query:$q, variables:{acc:$acc, from:$from, to:$to, ids:[$id]}}' |
		curl -s https://api.cloudflare.com/client/v4/graphql -H "Authorization: Bearer $CLOUDFLARE_API_TOKEN" -H 'content-type: application/json' --data @- |
		jq -r '[.data.viewer.accounts[0].durableObjectsPeriodicGroups[]?.sum.activeTime] | add // 0 | . / 1e6'
}
# The agent's own commands, in its container (it holds the node's key and the socket).
in_agent() { node_sh "docker exec cloudex-node cloudex-node $*"; }
# One-off runs of the agent's image with the node's volume, while the agent is stopped.
probe() { node_sh "docker run --rm -v cloudex-node:/var/lib/cloudex-node $AIMG probe $*" 2>/dev/null | head -1; }
start_agent() { node_sh "docker start cloudex-node" >/dev/null 2>&1; }
kill_agent() { node_sh "docker kill cloudex-node" >/dev/null 2>&1; }
# install_node TOKEN ARGS...: the installer, with the join token in its
# environment from stdin, as cloudex node add prints it: never in a process's
# arguments on the host (review H9, N53).
install_node() {
	local tok=$1
	shift
	"${NODE_SSH[@]}" "read -r t; export CLOUDEX_JOIN_TOKEN=\$t; ${NODE_SUDO:+sudo --preserve-env=CLOUDEX_JOIN_TOKEN} bash $NODE_DIR/node-install.sh $*" <<<"$tok"
}
# A fresh CLOUDFLARE_API_TOKEN from VERIFY_CF_TOKEN_CMD when it's set (wrangler's
# own login gives tokens that last an hour, less than a node run).
cf_fresh() {
	if [[ -n ${VERIFY_CF_TOKEN_CMD:-} ]]; then
		local t
		t=$(eval "$VERIFY_CF_TOKEN_CMD" 2>/dev/null | tail -1)
		[[ -n $t ]] && export CLOUDFLARE_API_TOKEN=$t
	fi
	return 0
}
# The account the deployment is in (do_active, d1_writes): the token may see others.
cf_account() {
	local acc=${CLOUDFLARE_ACCOUNT_ID:-$(jq -r '.accountId // empty' "$CLOUDEX_CONFIG_DIR/config.json")}
	[[ -n $acc ]] || acc=$(curl -s https://api.cloudflare.com/client/v4/accounts -H "Authorization: Bearer $CLOUDFLARE_API_TOKEN" | jq -r 'if (.result | length) == 1 then .result[0].id else empty end')
	echo "$acc"
}

# Streams and exec over the link (M3), against a sandbox on the node.
node_m3() {
	local NM=$P-m key M_ID ssh_cfg big ok_all why MC
	key=$(jq -r .apiKey "$src_cfg/config.json")
	cx new --on "$N" --name "$NM" --no-use
	M_ID=$(field .id)
	if [[ $CODE != 0 || -z $M_ID ]]; then
		m3_skip "couldn't make a sandbox on the node: ${ERR:0:200}"
		return
	fi
	MC=cloudex-$M_ID
	nck() { CLOUDEX_API_KEY=$key node "$here/node-check.mjs" "$API_URL" "$M_ID" "$@" >"$work/o" 2>&1; CODE=$?; OUT=$(cat "$work/o"); }
	in_m() { "$CX" exec "$NM" -- "$1" 2>/dev/null; }
	ssh_cfg=$work/ssh_config_m
	"$CX" ssh-config >"$ssh_cfg" 2>/dev/null

	# N09: exec's semantics, and the round trip.
	cx exec "$NM" -- 'echo out-$((6*7)); echo err >&2; exit 3'
	local e1="code $CODE out $OUT err $ERR"
	ok_all=$([ "$CODE" = 3 -a "$OUT" = out-42 -a "$ERR" = err ] && echo yes)
	cx exec "$NM" --root -- 'whoami; pwd'
	local e2=$OUT
	[ "$(tr '\n' ' ' <<<"$OUT")" = "root /root " ] || ok_all=""
	cx exec "$NM" --cwd /tmp -- 'whoami; pwd'
	local e3=$OUT
	[ "$(tr '\n' ' ' <<<"$OUT")" = "cloudex /tmp " ] || ok_all=""
	nck rtt 100
	local rtt=$OUT p50
	p50=$(jq -r .p50 <<<"$rtt" 2>/dev/null)
	OUT="[$e1] [$(tr '\n' ' ' <<<"$e2")] [$(tr '\n' ' ' <<<"$e3")] round trips: $rtt" CODE=""
	ok N09 "$(m3_desc N09)" [ "$ok_all" = yes -a -n "$p50" -a "${p50:-999}" -lt 150 ]

	# N10: data each way, unchanged.
	big=$work/big256
	head -c 268435456 /dev/urandom >"$big"
	local a b c d up down cup cdown pv=""
	a=$(now_ms)
	scp -F "$ssh_cfg" -o BatchMode=yes -q "$big" "$NM.cloudex:/tmp/vr-big" 2>"$work/e"
	b=$(now_ms)
	scp -F "$ssh_cfg" -o BatchMode=yes -q "$NM.cloudex:/tmp/vr-big" "$big.back" 2>>"$work/e"
	c=$(now_ms)
	up=$(awk -v ms=$((b - a)) 'BEGIN { printf "%.1f", 256000 / ms }') down=$(awk -v ms=$((c - b)) 'BEGIN { printf "%.1f", 256000 / ms }')
	ok_all=$(cmp -s "$big" "$big.back" && echo yes)
	head -c 94371840 "$big" >"$big.90"
	a=$(now_ms)
	"$CX" cp "$big.90" "$NM:/tmp/vr-90" >/dev/null 2>>"$work/e"
	b=$(now_ms)
	"$CX" cp "$NM:/tmp/vr-90" "$big.90.back" >/dev/null 2>>"$work/e"
	c=$(now_ms)
	cup=$(awk -v ms=$((b - a)) 'BEGIN { printf "%.1f", 90000 / ms }') cdown=$(awk -v ms=$((c - b)) 'BEGIN { printf "%.1f", 90000 / ms }')
	cmp -s "$big.90" "$big.90.back" || ok_all=""
	if [[ -n $ZONE ]]; then
		"$CX" cp "$here/node-app.js" "$NM:vr-app.js" >/dev/null 2>&1
		"$CX" exec "$NM" -d -- 'node vr-app.js 3000' >/dev/null 2>&1
		sleep 2
		local link want
		link=$("$CX" host "$NM" 3000 --json 2>/dev/null | jq -r .url)
		want="posted:94371840:$(md5sum <"$big.90" 2>/dev/null | cut -d' ' -f1)"
		[[ $want == posted:94371840: ]] && want="posted:94371840:$(md5 -q "$big.90")"
		local g po w
		for _ in $(seq 1 15); do
			g=$(curl -s --max-time 30 "$link/a?b=1")
			[[ $g == hello-preview* ]] && break
			sleep 1
		done
		po=$(curl -s --max-time 120 -X POST --data-binary @"$big.90" "$link/p")
		CLOUDEX_API_KEY=$key node "$here/node-check.mjs" "$API_URL" "$M_ID" ws 3000 "$link" >"$work/o" 2>&1
		w=$(cat "$work/o")
		pv="; preview GET '$g', POST '${po:0:60}', WebSocket $w"
		[[ $g == "hello-preview /a?b=1" && $po == "$want" && $w == *'"ok":true'* ]] || ok_all=""
	else
		pv="; no preview domain, so no preview checks"
	fi
	OUT="scp 256 MiB up $up MB/s, down $down MB/s; cp 90 MiB up $cup MB/s, down $cdown MB/s$pv; $(head -c 300 "$work/e")" CODE=""
	ok N10 "$(m3_desc N10)" [ "$ok_all" = yes ]
	rm -f "$big.back" "$big.90" "$big.90.back"

	# N11: a slow reader, and keystrokes meanwhile.
	rm -f "$work/slow"
	curl -s -H "authorization: Bearer $key" --limit-rate 2500k -o "$work/slow" "$API_URL/v1/sandboxes/$M_ID/files/content?path=/tmp/vr-big" &
	local cpid=$! aheads="" worst=0 got pos
	python3 "$here/keys.py" "$NM.cloudex" 300 "$ssh_cfg" >"$work/keys" 2>&1 &
	local kpid=$!
	for t in 10 20; do
		sleep 10
		got=$(wc -c <"$work/slow" 2>/dev/null || echo 0)
		pos=$("$CX" exec "$NM" -- 'p=$(pgrep -x cat | head -1); [ -n "$p" ] && grep ^pos /proc/$p/fdinfo/3 | tr -dc 0-9' 2>/dev/null)
		local ahead=$(((${pos:-0} - got) / 1048576))
		aheads="$aheads t=${t}s: the client has $((got / 1048576)) MiB, cat is $ahead MiB ahead;"
		((ahead > worst)) && worst=$ahead
		[[ -z $pos ]] && worst=999
	done
	kill "$cpid" 2>/dev/null
	wait "$cpid" 2>/dev/null
	wait "$kpid"
	local keys kmax
	keys=$(cat "$work/keys") kmax=$(jq -r '.max | floor' <"$work/keys" 2>/dev/null)
	OUT="$aheads keystrokes $keys" CODE=""
	ok N11 "$(m3_desc N11)" [ "$worst" -le 16 -a -n "$kmax" -a "${kmax:-999}" -lt 200 ]

	# N12: an idle ssh session for 30 minutes, and the object's active time.
	if [[ ${VERIFY_NODE_LONG:-} == 1 && -n ${CLOUDFLARE_API_TOKEN:-} ]]; then
		local oid from to act alive
		# The diagnostic route for a node sandbox's streams (admins only) has its object's id.
		oid=$(curl -s -H "authorization: Bearer $key" "$API_URL/v1/sandboxes/$M_ID/node-streams" | jq -r .objectId)
		from=$(date -u -v+1M +%Y-%m-%dT%H:%M:00Z 2>/dev/null || date -u -d '+1 min' +%Y-%m-%dT%H:%M:00Z)
		alive=$(python3 "$here/keys.py" "$NM.cloudex" 0 "$ssh_cfg" --idle 1800 2>/dev/null | jq -r .alive)
		to=$(date -u -v-2M +%Y-%m-%dT%H:%M:00Z 2>/dev/null || date -u -d '-2 min' +%Y-%m-%dT%H:%M:00Z)
		sleep 120 # the analytics lag
		act=$(do_active "$from" "$to" "$oid")
		OUT="session alive after 30 min: $alive; active ${act} s from $from to $to" CODE=""
		ok N12 "$(m3_desc N12)" [ "$alive" = true -a "$(awk -v a="$act" 'BEGIN { print (a < 60) }')" = 1 ]
	else
		skip N12 "$(m3_desc N12)" "takes 30 minutes: set VERIFY_NODE_LONG=1 and CLOUDFLARE_API_TOKEN"
	fi

	# N13: a deploy during a busy scp: the machine's side ends with the client's.
	if [[ ${VERIFY_NODE_DEPLOY:-} == 1 && ${VERIFY_NODE_LONG:-} == 1 ]]; then
		local base spid t_cut t_gone n
		base=$(node_sh "docker exec $MC sh -c 'pgrep -c -f \"^sshd: cloudex\" || true'")
		scp -F "$ssh_cfg" -o BatchMode=yes -q -l 4000 "$big" "$NM.cloudex:/tmp/vr-slow" 2>/dev/null &
		spid=$!
		sleep 5
		cf_fresh
		"$CX" deploy --yes --skip-check >"$work/deploy2.log" 2>&1
		t_cut="" t_gone=""
		for _ in $(seq 1 420); do
			[[ -z $t_cut ]] && ! kill -0 "$spid" 2>/dev/null && t_cut=$(now_ms)
			n=$(node_sh "docker exec $MC sh -c 'pgrep -c -f \"^sshd: cloudex\" || true'")
			[[ -z $t_gone && ${n:-9} -le ${base:-0} ]] && t_gone=$(now_ms)
			[[ -n $t_cut && -n $t_gone ]] && break
			sleep 1
		done
		kill "$spid" 2>/dev/null
		OUT="scp ended at $t_cut, the machine's ssh session at $t_gone (sessions before: $base)" CODE=""
		local lag=999
		[[ -n $t_cut && -n $t_gone ]] && lag=$(((t_gone - t_cut) / 1000))
		ok N13 "$(m3_desc N13)" [ "$lag" -le 5 ]
	else
		skip N13 "$(m3_desc N13)" "needs a deploy and up to 6 minutes: set VERIFY_NODE_DEPLOY=1 and VERIFY_NODE_LONG=1"
	fi
	rm -f "$big"

	# N14: exec's timeout.
	cx exec "$NM" --timeout 2s -- 'sleep 10.0914'
	local tcode=$CODE left
	sleep 1
	left=$(in_m 'ps -eo args | grep -c "[s]leep 10.0914"')
	OUT="exit $tcode, $left left" CODE=""
	ok N14 "$(m3_desc N14)" [ "$tcode" = 124 -a "$left" = 0 ]

	# N15: the terminal.
	local t1 t2 t3 t4 t5
	CLOUDEX_API_KEY=$key node "$here/term-check.mjs" "$API_URL" "$M_ID" echo >"$work/o" 2>&1 && t1=ok
	t1="echo ${t1:-$(cat "$work/o")}"
	CLOUDEX_API_KEY=$key node "$here/term-check.mjs" "$API_URL" "$M_ID" tmux >"$work/o" 2>&1 && t2=ok
	t2="tmux ${t2:-$(cat "$work/o")}"
	nck resize && t3=ok
	t3="resize ${t3:-$OUT}"
	nck tui && t4=ok
	t4="htop and vim ${t4:-$OUT}"
	OUT="$t1; $t2; $t3; $t4" CODE=""
	ok N15 "$(m3_desc N15)" [ "$t1$t2$t3$t4" = "echo oktmux okresize okhtop and vim ok" ]

	# N16: clean-up when either side drops, and tickets.
	local h1 h2 h3 probe_codes
	nck hangup 30.0916
	sleep 15
	h1=$(in_m 'ps -eo args | grep -c "[s]leep 30.0916"')
	("$CX" exec "$NM" -- 'sleep 30.0917' >/dev/null 2>&1 &
		echo $! >"$work/cli.pid"
		wait 2>/dev/null) 2>/dev/null &
	sleep 4
	kill -9 "$(cat "$work/cli.pid")" 2>/dev/null
	sleep 20
	h2=$(in_m 'ps -eo args | grep -c "[s]leep 30.0917"')
	("$CX" exec "$NM" -- 'sleep 60.0918' >/dev/null 2>&1 &)
	sleep 4
	kill_agent
	probe_codes=$(probe stream --sandbox "$M_ID")
	start_agent
	wait_state "$N" online 30
	sleep 15
	h3=$(in_m 'ps -eo args | grep -c "[s]leep 60.0918"')
	OUT="left after a terminal closed: $h1; after the client dropped an exec: $h2; after the agent died mid-exec and came back: $h3; a ticket as issued, replayed, altered, for another stream: $probe_codes" CODE=""
	ok N16 "$(m3_desc N16)" [ "$h1" = 0 -a "$h2" = 0 -a "$h3" = 0 -a "$probe_codes" = "101 401 401 401" ]
	in_m 'pkill -f "[v]r-app.js"; true' >/dev/null
	M3_SANDBOX=$NM M3_ID=$M_ID
}

# The Docker runtime, the firewall and the self-test (M4), against the
# sandbox node_m3 made ($M3_SANDBOX) and a second one.
node_m4() {
	local NM=${M3_SANDBOX:-} MID=${M3_ID:-} M2=$P-m2 M2ID MC ok_all detail
	if [[ -z $NM ]]; then
		m4_skip "no sandbox on the node (N09's failed)" "N18 N19 N20 N21 N22 N23 N24"
		return
	fi
	MC=cloudex-$MID
	wait_iso 0 >/dev/null
	cx new --on "$N" --name "$M2" --cpus 0.5 --memory 1G --no-use
	M2ID=$(field .id)

	# N18: the matrix from inside a real machine, through the product's exec.
	local hostinfo src gw dns tail agentaddrs m2ip targets plan res
	hostinfo=$(node_sh "ip -4 route get 1.1.1.1 | sed -n 's/.* src \([0-9.]*\).*/\1/p' | head -1; ip -4 route show default | awk '{print \$3; exit}';
		awk '/^nameserver [0-9.]+\$/ {print \$2}' /run/systemd/resolve/resolv.conf /etc/resolv.conf 2>/dev/null | grep -v '^127\.' | sort -u | tr '\n' ' '; echo;
		ip -4 -o addr show | awk '{print \$4}' | cut -d/ -f1 | grep -E '^100\.(6[4-9]|[7-9][0-9]|1[01][0-9]|12[0-7])\.' | tr '\n' ' '; echo;
		docker inspect cloudex-node -f '{{range \$k, \$v := .NetworkSettings.Networks}}{{\$v.IPAddress}}={{\$v.Gateway}} {{end}}'; echo;
		docker inspect cloudex-$M2ID -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}';
		ip -4 -o addr show docker0 2>/dev/null | awk '{print \$4}' | cut -d/ -f1")
	{ read -r src; read -r gw; read -r dns; read -r tail; read -r agentaddrs; read -r m2ip; read -r docker0; } <<<"$hostinfo"
	local own_gw="" own_agent=""
	own_agent=$(node_sh "docker inspect $MC -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}'" | awk -F. '{printf "%s.%s.%s.%d", $1, $2, $3, $4 - 1}')
	own_gw=$(awk -F. '{printf "%s.%s.%s.%d", $1, $2, $3, $4 - 1}' <<<"$own_agent")
	local steps="[]" n=0
	addt() { steps=$(jq -c --arg id "$1" --arg h "$2" --argjson p "$3" '. + [{id: $id, kind: "tcp", host: $h, port: $p}]' <<<"$steps"); }
	addu() { steps=$(jq -c --arg id "$1" --arg h "$2" '. + [{id: $id, kind: "udp53", host: $h}]' <<<"$steps"); }
	addt "open internet 1.1.1.1:443" 1.1.1.1 443
	steps=$(jq -c '. + [{id: "open DNS example.com", kind: "dns", name: "example.com"}]' <<<"$steps")
	addu "open DNS 1.1.1.1" 1.1.1.1
	[[ -n $src ]] && for p in 22 80 443; do addt "closed the host $src:$p" "$src" "$p"; done
	[[ -n $gw ]] && { addt "closed the LAN gateway $gw:80" "$gw" 80; addt "closed the LAN gateway $gw:53" "$gw" 53; addu "closed LAN gateway DNS $gw udp" "$gw"; }
	for d in $dns; do
		case $d in 10.* | 172.1[6-9].* | 172.2[0-9].* | 172.3[01].* | 192.168.* | 100.*) ;; *) continue ;; esac # a public resolver is the internet
		addt "closed host DNS $d:53" "$d" 53; addu "closed host DNS $d udp" "$d"
	done
	for t in $tail; do addt "closed host tailnet address $t:22" "$t" 22; done
	addt "closed tailnet 100.100.100.100:53" 100.100.100.100 53
	addt "closed metadata 169.254.169.254:80" 169.254.169.254 80
	addt "closed the host on the machine gateway $own_gw:22" "$own_gw" 22
	[[ -n $docker0 ]] && addt "closed the host on docker0 $docker0:22" "$docker0" 22
	[[ -n $m2ip ]] && addt "closed another machine $m2ip:22" "$m2ip" 22
	for pair in $agentaddrs; do
		local ip=${pair%%=*} g=${pair#*=}
		[[ $ip == "$own_agent" || -z $ip ]] && continue
		addt "closed the agent at $ip:22" "$ip" 22
		[[ -n $g && $g != "$own_gw" ]] && addt "closed the host at $g:22" "$g" 22
	done
	"$CX" cp "$repo/go/internal/node/probe.py" "$NM:/tmp/vr-probe.py" >/dev/null 2>&1
	res=$("$CX" exec "$NM" --root -- "python3 /tmp/vr-probe.py \"\$(echo $(printf '[%s]' "$steps" | base64 | tr -d '\n') | base64 -d)\"" 2>"$work/e")
	local bad="" all=""
	for id in $(jq -r '.[].id | @base64' <<<"$steps"); do
		id=$(base64 -d <<<"$id")
		local got
		got=$(jq -r --arg k "$id" '.[$k] // "missing"' <<<"$res" 2>/dev/null)
		all="$all $id=$got;"
		case $id in
		open*) [[ $got == open* || $got == resolved* || $got == answered ]] || bad="$bad $id=$got;" ;;
		closed*) [[ $got == timeout || $got == unreachable ]] || bad="$bad $id=$got;" ;;
		esac
	done
	OUT="${bad:+FAILED:$bad }all:$all" CODE=""
	if [[ -n $NODE_SUDO ]]; then
		ok N18 "$(m3_desc N18)" [ -z "$bad" -a -n "$res" ]
	else
		skip N18 "$(m3_desc N18)" "the node runs --unisolated (no VERIFY_NODE_SUDO=1), so there is no firewall to test; what was reached: ${bad:0:300}"
	fi

	# N19: the self-test's NET_ADMIN probe, run now.
	in_agent selftest >"$work/selftest.json" 2>"$work/e"
	local nchecks nbad
	nchecks=$(jq '[.checks[] | select(.name | startswith("NET_ADMIN") or startswith("the agent tells"))] | length' "$work/selftest.json" 2>/dev/null)
	nbad=$(jq -r '[.checks[] | select((.name | startswith("NET_ADMIN") or startswith("the agent tells")) and (.ok | not)) | "\(.name): \(.got)"] | join("; ")' "$work/selftest.json" 2>/dev/null)
	OUT="runtime $(jq -r .runtime "$work/selftest.json" 2>/dev/null), $nchecks checks: $(jq -r '[.checks[] | select(.name | startswith("NET_ADMIN") or startswith("the agent tells")) | "\(.name) -> \(.got)"] | join("; ")' "$work/selftest.json" 2>/dev/null)" CODE=""
	cp "$work/selftest.json" "$out/node-selftest.json" 2>/dev/null
	if [[ -n $NODE_SUDO ]]; then
		ok N19 "$(m3_desc N19)" [ "${nchecks:-0}" -ge 6 -a -z "$nbad" -a "$(jq -r .pass "$work/selftest.json" 2>/dev/null)" = true ]
	else
		skip N19 "$(m3_desc N19)" "the node runs --unisolated; failed: ${nbad:0:300}"
	fi

	# N20: the container's flags, and what root can do inside.
	local insp inside boot1 boot2
	insp=$(node_sh "docker inspect $MC -f '{{json .HostConfig}}'")
	local hc_ok
	hc_ok=$(jq -r '[.Privileged == false, (.CapAdd // [] | length) == 0, .CapDrop == ["NET_RAW","MKNOD","SETFCAP"], (.Binds // [] | length) == 0,
		(.Mounts // [] | length) == 0, (.Devices // [] | length) == 0, .PidMode == "", .IpcMode == "private", .UsernsMode == "",
		(.NetworkMode | startswith("cloudex-cx-")), .RestartPolicy.Name == "no", .PidsLimit == 4096, .Memory > 0, .NanoCpus > 0,
		.Tmpfs["/run"] != null, (.SecurityOpt // [] | map(select(test("unconfined|privileged"))) | length) == 0] | all' <<<"$insp" 2>/dev/null)
	inside=$("$CX" exec "$NM" --root -- "python3 /tmp/vr-probe.py \"\$(echo $(printf '[[{"id":"i","kind":"inside"}]]' | base64 | tr -d '\n') | base64 -d)\"" 2>/dev/null | jq -r .i 2>/dev/null)
	local runtime
	runtime=$(jq -r .runtime "$work/selftest.json" 2>/dev/null)
	local in_ok
	if [[ $runtime == sysbox ]]; then
		in_ok=$(jq -r '[(.uidMap | startswith("0 0 ") | not), .dockerSocket == false, (.sysrqWrite | startswith("denied")), (.ipv6Addresses | length) == 0] | all' <<<"$inside" 2>/dev/null)
	else
		in_ok=$(jq -r '[.capEff == "00000000200405fb", .dockerSocket == false, (.procSysWrite | startswith("denied")), (.sysWrite | startswith("denied")),
			(.sysrqWrite | startswith("denied")), (.kcoreRead | startswith("denied")), (.rawSocket | startswith("denied")), (.packetSocket | startswith("denied")),
			(.mknod | startswith("denied")), .mount == "denied", .routeChange == "denied", (.ipv6Addresses | length) == 0, .ipv6 == false] | all' <<<"$inside" 2>/dev/null)
	fi
	boot1=$(in_m 'cat /run/cloudex-boot')
	"$CX" stop "$NM" >/dev/null 2>&1
	"$CX" resume "$NM" >/dev/null 2>&1
	boot2=$(in_m 'cat /run/cloudex-boot')
	# The start ids first, since the rest of the detail is long.
	local ids="start ids '$boot1' then '$boot2'" ids_ok=""
	[[ -n $boot1 && -n $boot2 && $boot1 != "$boot2" ]] && ids_ok=yes
	if [[ -z $boot1 && -z $boot2 && -z $(in_m 'grep -l cloudex-boot /usr/local/bin/* /entrypoint* 2>/dev/null | head -1') ]]; then
		# An image from before v0.2.2 writes no /run/cloudex-boot (M4): nothing to compare.
		ids="no start ids: the machine image predates them (set VERIFY_NODE_MACHINE_IMAGE to a v0.2.2 base image to check them)"
		ids_ok=n/a
	fi
	OUT="$ids; flags ok: $hc_ok ($(jq -c '{Privileged,CapDrop,PidsLimit,Memory,NanoCpus,Tmpfs,NetworkMode,Runtime}' <<<"$insp" 2>/dev/null)); inside ($runtime) ok: $in_ok ($inside)" CODE=""
	ok N20 "$(m3_desc N20)" [ "$hc_ok" = true -a "$in_ok" = true -a -n "$ids_ok" ]

	# N21: a Docker restart.
	if [[ -n $NODE_SUDO ]]; then
		node_sh "sudo systemctl restart docker" >/dev/null 2>&1
		wait_state "$N" online 120
		local tbl st net
		tbl=$(node_sh "sudo nft list table inet cloudex >/dev/null && echo loaded")
		st=$(in_agent selftest 2>/dev/null | jq -r .pass)
		wait_iso 0 >/dev/null # the agent's own, after its restart with Docker's
		# Docker's restart stopped the machine; its record says so (the agent's
		# events, or the reconcile at its hello), and resume starts it again.
		local rst=""
		for _ in $(seq 1 30); do rst=$("$CX" info "$NM" --json 2>/dev/null | jq -r .state); [[ $rst == stopped ]] && break; sleep 1; done
		"$CX" resume "$NM" >/dev/null 2>&1
		net=$(in_m "curl -s -o /dev/null -w '%{http_code}' --max-time 10 https://example.com")
		OUT="table: ${tbl:-missing}; self-test pass: $st; the sandbox after Docker's restart: $rst; the machine's https after resume: $net" CODE=""
		ok N21 "$(m3_desc N21)" [ "$tbl" = loaded -a "$st" = true -a "$rst" = stopped -a "$net" = 200 ]
	else
		skip N21 "$(m3_desc N21)" "needs root on the node host (VERIFY_NODE_SUDO=1)"
	fi

	# N22: the agent restarts; the machine doesn't.
	local t0 before after works
	before=$(node_sh "docker inspect $MC -f '{{.State.StartedAt}}'")
	node_sh "docker restart cloudex-node" >/dev/null 2>&1
	t0=$(now_s)
	works=""
	for _ in $(seq 1 40); do
		[[ $(in_m 'echo alive') == alive ]] && { works=$(since "$t0"); break; }
		sleep 0.5
	done
	after=$(node_sh "docker inspect $MC -f '{{.State.StartedAt}}'")
	OUT="machine started at $before, and at $after after the agent's restart; exec works again after ${works:-never} s" CODE=""
	ok N22 "$(m3_desc N22)" [ -n "$works" -a "$before" = "$after" -a "$(awk -v w="${works:-99}" 'BEGIN { print (w <= 10) }')" = 1 ]

	# N23: a reboot of the host.
	if [[ ${VERIFY_NODE_REBOOT:-} == 1 && -n $NODE_SUDO ]]; then
		in_m 'echo vr-reboot-$$ > ~/vr-reboot.txt; sync; cat ~/vr-reboot.txt' >"$work/mark"
		node_sh "sudo systemctl reboot" >/dev/null 2>&1
		REBOOTED=1
		sleep 20
		local back="" t1
		t1=$(now_s)
		for _ in $(seq 1 120); do
			node_sh true >/dev/null 2>&1 && { back=$(since "$t1"); break; }
			sleep 5
		done
		wait_state "$N" online 300
		local on=$WAITED tbl2 iso mstate files
		tbl2=$(node_sh "sudo nft list table inet cloudex >/dev/null && echo loaded")
		for _ in $(seq 1 60); do
			iso=$("$CX" node info "$N" --json 2>/dev/null | jq -r '.isolation.firewall')
			[[ $iso != unchecked ]] && break
			sleep 2
		done
		mstate=$(node_sh "docker inspect $MC -f '{{.State.Status}}'")
		local rstate=""
		for _ in $(seq 1 30); do rstate=$("$CX" info "$NM" --json 2>/dev/null | jq -r .state); [[ $rstate == stopped ]] && break; sleep 1; done
		"$CX" resume "$NM" >/dev/null 2>&1
		files=$(in_m 'cat ~/vr-reboot.txt')
		OUT="ssh back after ${back:-never} s; the node online $on s later; table: ${tbl2:-missing}; isolation: $iso; the machine: $mstate, its sandbox: $rstate; its file after resume: $files (was $(cat "$work/mark"))" CODE=""
		ok N23 "$(m3_desc N23)" [ "$tbl2" = loaded -a "$iso" = pass -a "$mstate" = exited -a "$rstate" = stopped -a -n "$files" -a "$files" = "$(cat "$work/mark")" ]
	else
		skip N23 "$(m3_desc N23)" "reboots the host: set VERIFY_NODE_REBOOT=1 (and VERIFY_NODE_SUDO=1)"
	fi

	# N24: a failing self-test refuses machines; --unisolated runs them.
	if [[ -n $NODE_SUDO ]]; then
		# Without the firewall's table, the agent's self-test (it runs one
		# when it starts) fails, and the node refuses a new machine.
		local f1 r1 f2 t
		node_sh "sudo nft delete table inet cloudex" >/dev/null 2>&1
		t=$(now_ms)
		node_sh "docker restart cloudex-node" >/dev/null 2>&1
		f1=$(wait_iso "$t")
		f1="$f1, failed: $("$CX" node info "$N" --json 2>/dev/null | jq -r '.isolation.failed // [] | length') checks"
		cx new --on "$N" --name "$P-m3" --no-use
		r1="code $CODE: ${ERR:0:240}"
		[[ $CODE == 0 ]] && "$CX" delete "$P-m3" --yes >/dev/null 2>&1
		node_sh "sudo systemctl restart cloudex-node-firewall.service" >/dev/null 2>&1
		t=$(now_ms)
		node_sh "docker restart cloudex-node" >/dev/null 2>&1
		f2=$(wait_iso "$t")
		OUT="without the table: isolation $f1 (state, failed checks); a new machine: $r1; with it again: $f2" CODE=""
		ok N24 "$(m3_desc N24)" [ "${f1%%,*}" = fail -a "${r1:0:6}" != "code 0" -a -n "$(grep -i 'self-test fails' <<<"$r1")" -a "$f2" = pass ]
	else
		local iso
		wait_iso 0 >/dev/null
		iso=$("$CX" node info "$N" --json 2>/dev/null | jq -r '[.isolation.unisolated, .isolation.firewall] | join(" ")')
		OUT="unisolated, firewall: $iso; machines ran in N09-N22 above (node list labels such a node none (testing): CLI unit tests)" CODE=""
		ok N24 "$(m3_desc N24)" [ "$iso" = "true fail" ]
	fi
	"$CX" delete "$M2" --yes >/dev/null 2>&1
}

M5_CHECKS=("N26 a node that goes offline shows offline, with when it was last seen; exec, ssh and a new sandbox on it are a 503 that names it; it comes back by itself with its running sandbox still running"
	"N27 a machine stopped by hand on the host while its node was offline shows stopped once the node is back, and resumes with its files"
	"N28 deleting a sandbox while its node is offline is a 503 unless --force; with --force it goes at once, and the node removes its machine and network when it's back"
	"N29 --idle 2m stops a sandbox on a node by itself, keeping its disk"
	"N30 stop and resume on a node each take under 2 s, and keep the disk"
	"N31 the WHERE column says where each sandbox runs: the node, then the node (offline) while it's offline"
	"N32 new --on --cpus 2 --memory 2G gives the machine exactly those limits (docker inspect), and info shows them"
	"N33 an own-only token can't place a sandbox on a node whose isolation is plain Docker (or none); a token that isn't own-only can"
	"N34 forks, resume --size and --from-backup on a node refuse with a message that says why, and so do a fork onto a node and new --from <sandbox> --on"
	"N35 removing a node with a sandbox needs --force; then its machine is deleted, and the sandbox stays, in error, saying so, until it's deleted")
m5_skip() { for c in "${M5_CHECKS[@]}"; do skip "${c%% *}" "${c#* }" "$1"; done; }
m5_desc() { for c in "${M5_CHECKS[@]}"; do [[ ${c%% *} == "$1" ]] && echo "${c#* }"; done; }
# The line of a sandbox in cloudex list (the human table), squeezed.
list_line() { "$CX" list --json=false 2>/dev/null | grep -E "^. +[^ ]+ +$1 " | tr -s ' '; }

# Sandboxes on nodes (M5): ordinary sandboxes, their lifecycle on the node,
# the node going offline and coming back, deletes while it's offline,
# idle stop, limits, who may place sandboxes, and what's refused.
node_m5() {
	local S5=$P-n5 S5B=$P-n5b S5D=$P-n5d S5ID S5BID S5DID ms_stop ms_res kept t0
	wait_iso 0 >/dev/null
	cx new --on "$N" --name "$S5" --no-use
	S5ID=$(field .id)
	if [[ $CODE != 0 || -z $S5ID ]]; then
		m5_skip "couldn't make a sandbox on the node: ${ERR:0:200}"
		return
	fi
	cx new --on "$N" --name "$S5B" --no-use
	S5BID=$(field .id)
	cx new --on "$N" --name "$S5D" --no-use
	S5DID=$(field .id)
	local e5d=""
	[[ $CODE != 0 ]] && e5d="making it failed: $(grep -m1 '^error' <<<"$ERR" | head -c 200); "

	# N30: stop and resume, timed as the CLI sees them.
	"$CX" exec "$S5B" -- 'echo n30 > ~/n30; sudo sh -c "echo sys > /opt/n30"' >/dev/null 2>&1
	local st1 st2
	t0=$(now_ms)
	cx stop "$S5B"
	ms_stop=$(($(now_ms) - t0))
	st1=$(field .state)
	t0=$(now_ms)
	cx resume "$S5B"
	ms_res=$(($(now_ms) - t0))
	st2=$(field .state)
	kept=$("$CX" exec "$S5B" -- 'cat ~/n30 /opt/n30' 2>/dev/null | tr '\n' ' ')
	OUT="stop $ms_stop ms ($st1), resume $ms_res ms ($st2), files after: $kept" CODE=""
	ok N30 "$(m5_desc N30)" [ "$st1" = stopped -a "$st2" = running -a "$ms_stop" -lt 2000 -a "$ms_res" -lt 2000 -a "$kept" = "n30 sys " ]

	# N32: the machine's limits.
	cx new --on "$N" --name "$P-n5c" --cpus 2 --memory 2G --no-use
	local cid lim shown
	cid=$(field .id)
	shown=$(field '"\(.placement.cpus) \(.placement.memoryMb)"')
	lim=$(node_sh "docker inspect cloudex-$cid -f '{{.HostConfig.NanoCpus}} {{.HostConfig.Memory}}'")
	"$CX" delete "$P-n5c" --yes >/dev/null 2>&1
	OUT="docker inspect: NanoCpus Memory = $lim; the API: cpus memoryMb = $shown" CODE=""
	ok N32 "$(m5_desc N32)" [ "$lim" = "2000000000 2147483648" -a "$shown" = "2 2048" ]

	# N34: what's refused on a node, and why (the refusal table in the plan's M5 Built).
	local r=""
	refused() { # refused WHAT WANT CMD...: the command must fail with WANT in its error
		local what=$1 want=$2
		shift 2
		cx "$@"
		[[ $CODE != 0 && $ERR == *"$want"* ]] || r="$r $what: code $CODE, ${ERR:0:120};"
	}
	refused fork "forks aren't available for a sandbox on a node" fork "$S5" --name "$P-n5f"
	refused "resume --size" "sizes are for Cloudflare" resume "$S5" --size standard-4
	"$CX" stop "$S5" >/dev/null 2>&1
	refused "resume --from-backup" "so it has no backups" resume "$S5" --from-backup
	refused "new --from <sandbox> --on" "forks can't start on a node" new --on "$N" --from "$S5"
	"$CX" resume "$S5" >/dev/null 2>&1
	# A fork of a Cloudflare sandbox onto a node, through the API (the CLI refuses it first).
	local fc
	fc=$(curl -s -o "$work/b" -w '%{http_code}' --max-time 30 -X POST -H "authorization: Bearer $(jq -r .apiKey "$src_cfg/config.json")" \
		-H 'content-type: application/json' -d "{\"node\":\"$N\"}" "$API_URL/v1/sandboxes/$S5ID/fork")
	[[ $fc == 400 && $(jq -r .error "$work/b") == *"not on a node"* ]] || r="$r API fork with node: $fc $(head -c 120 "$work/b");"
	OUT="${r:-all refused, each saying why}" CODE=""
	ok N34 "$(m5_desc N34)" [ -z "$r" ]

	# N33: decision 4, who may place sandboxes on a node.
	local iso own usr o1 o2
	iso=$("$CX" node info "$N" --json 2>/dev/null | jq -r 'if .isolation.unisolated then "none" else .runtime end')
	cx tokens create "$P-n5own" --own --expires 1h
	own=$(field .secret)
	cx tokens create "$P-n5usr" --expires 1h
	usr=$(field .secret)
	CLOUDEX_CONFIG_DIR=$work/tok-n5a CLOUDEX_API_URL=$API_URL CLOUDEX_API_KEY=$own "$CX" new --on "$N" --name "$P-n5o" --no-use >"$work/o" 2>"$work/e" </dev/null
	o1="code $? $(head -c 200 "$work/e")"
	CLOUDEX_CONFIG_DIR=$work/tok-n5b CLOUDEX_API_URL=$API_URL CLOUDEX_API_KEY=$usr "$CX" new --on "$N" --name "$P-n5u" --no-use >"$work/o" 2>"$work/e" </dev/null
	o2="code $?"
	"$CX" delete "$P-n5o" --yes >/dev/null 2>&1
	"$CX" delete "$P-n5u" --yes >/dev/null 2>&1
	for t in "$P-n5own" "$P-n5usr"; do
		"$CX" tokens revoke "$("$CX" tokens --json 2>/dev/null | jq -r --arg n "$t" '.[] | select(.name == $n) | .id')" >/dev/null 2>&1
	done
	OUT="node isolation $iso; own-only token: $o1; user token: $o2" CODE=""
	if [[ $iso == sysbox ]]; then
		ok N33 "$(m5_desc N33)" [ "${o1:0:6}" = "code 0" -a "$o2" = "code 0" ]
	else
		ok N33 "$(m5_desc N33)" [ "${o1:0:6}" = "code 1" -a -n "$(grep "own-only token can't place sandboxes on node $N" <<<"$o1")" -a "$o2" = "code 0" ]
	fi

	# N26, N27, N28 and N31: the node goes offline. S5 keeps running there;
	# S5B's machine is stopped by hand on the host meanwhile; S5D is deleted.
	local w_on w_off e_exec c_exec e_ssh e_new code_api d1 d2 after
	w_on=$(list_line "$S5")
	"$CX" exec "$S5B" -- 'echo n27 > ~/n27; sync' >/dev/null 2>&1
	kill_agent
	wait_state "$N" offline 75
	local went=$WAITED
	w_off=$(list_line "$S5")
	cx exec "$S5" -- true
	e_exec=$ERR c_exec=$CODE
	code_api=$(curl -s -o "$work/b" -w '%{http_code}' --max-time 30 -X POST -H "authorization: Bearer $(jq -r .apiKey "$src_cfg/config.json")" \
		-H 'content-type: application/json' -d '{"cmd":"true"}' "$API_URL/v1/sandboxes/$S5ID/exec")
	e_ssh=$("$CX" ssh "$S5" -- true </dev/null 2>&1 | head -1)
	cx new --on "$N" --name "$P-n5x" --no-use
	e_new=$ERR
	local info_off
	info_off=$("$CX" info "$S5" --json 2>/dev/null | jq -c '{state, online: .placement.node.online, seen: .placement.node.lastSeenAt}')
	node_sh "docker stop -t 5 cloudex-$S5BID" >/dev/null 2>&1
	cx delete "$S5D" --yes
	d1="code $CODE: ${ERR:0:160}"
	cx delete "$S5D" --yes --force
	d2="code $CODE"
	local pend
	pend=$("$CX" node info "$N" --json 2>/dev/null | jq -c '.pendingDeletes // []')
	start_agent
	wait_state "$N" online 60
	local back=$WAITED works="" sbst=""
	for _ in $(seq 1 40); do [[ $("$CX" exec "$S5" -- 'echo up' 2>/dev/null) == up ]] && { works=yes; break; }; sleep 0.5; done
	for _ in $(seq 1 40); do sbst=$("$CX" info "$S5B" --json 2>/dev/null | jq -r .state); [[ $sbst == stopped ]] && break; sleep 1; done
	after="" # S5D's container and network
	for _ in $(seq 1 40); do
		after=$(node_sh "docker ps -aq --filter label=build.cloudex.sandbox=$S5DID | wc -l; docker network ls -q --filter label=build.cloudex.sandbox=$S5DID | wc -l" | tr '\n' ' ')
		[[ $after == "0 0 " ]] && break
		sleep 1
	done
	local s5state
	s5state=$("$CX" info "$S5" --json 2>/dev/null | jq -r .state)
	OUT="offline after $went s; info then: $info_off; exec: code $c_exec, ${e_exec:0:120}; API: $code_api $(jq -r .error "$work/b" 2>/dev/null | head -c 80); ssh: ${e_ssh:0:100}; new --on: ${e_new:0:100}; back after $back s, $S5 $s5state, exec works: ${works:-no}" CODE=""
	ok N26 "$(m5_desc N26)" [ "$c_exec" = 1 -a -n "$(grep "node $N is offline (last seen" <<<"$e_exec")" -a "$code_api" = 503 -a -n "$(grep "node $N is offline" <<<"$e_ssh")" \
		-a -n "$(grep "node $N is offline" <<<"$e_new")" -a "$(jq -r .state <<<"$info_off")" = running -a "$(jq -r .online <<<"$info_off")" = false \
		-a "$(jq -r .seen <<<"$info_off")" != null -a "$works" = yes -a "$s5state" = running ]
	local n27files
	n27files=$("$CX" resume "$S5B" >/dev/null 2>&1; "$CX" exec "$S5B" -- 'cat ~/n27' 2>/dev/null)
	OUT="$S5B after the node came back: $sbst; notice: $("$CX" info "$S5B" --json 2>/dev/null | jq -r '.notice // "none"' | head -c 120); after resume: $n27files" CODE=""
	ok N27 "$(m5_desc N27)" [ "$sbst" = stopped -a "$n27files" = n27 ]
	OUT="${e5d}without --force: $d1; with --force: $d2; pending on the node: $pend; its container and network after the node came back: $after" CODE=""
	ok N28 "$(m5_desc N28)" [ "${d1:0:6}" = "code 1" -a -n "$(grep "can't be removed now" <<<"$d1")" -a "$d2" = "code 0" -a "$pend" = "[\"$S5DID\"]" -a "$after" = "0 0 " ]
	OUT="online: '$w_on'; offline: '$w_off'" CODE=""
	ok N31 "$(m5_desc N31)" [ -n "$(grep -E "^[* ]? ?$S5ID $S5 running $N - " <<<"$w_on")" -a -n "$(grep -E "^[* ]? ?$S5ID $S5 running $N \\(offline\\) - " <<<"$w_off")" ]

	# N29: idle stop, from the sandbox's 60 s alarm.
	"$CX" exec "$S5B" -- 'echo n29 > ~/n29' >/dev/null 2>&1
	"$CX" set "$S5B" --idle 2m >/dev/null 2>&1
	t0=$(now_s)
	local ist=""
	for _ in $(seq 1 60); do ist=$("$CX" info "$S5B" --json 2>/dev/null | jq -r .state); [[ $ist == stopped ]] && break; sleep 5; done
	local itook mst n29
	itook=$(since "$t0")
	mst=$(node_sh "docker inspect cloudex-$S5BID -f '{{.State.Status}}'")
	n29=$("$CX" resume "$S5B" >/dev/null 2>&1; "$CX" exec "$S5B" -- 'cat ~/n29' 2>/dev/null)
	OUT="stopped after $itook s idle ($ist; the machine $mst); its file after resume: $n29" CODE=""
	ok N29 "$(m5_desc N29)" [ "$ist" = stopped -a "$mst" = exited -a "$n29" = n29 ]
	"$CX" delete "$S5B" --yes >/dev/null 2>&1
	"$CX" delete "$S5" --yes >/dev/null 2>&1
}

M6_CHECKS=("N36 with fake keys, claude -p, codex exec and opencode run on a node get the provider's own 401 through its agent and the Worker, and the placeholder gets a 200"
	"N37 the deployment's real provider keys are in no file in a node machine or the agent's volume, in no process's environment there or on the host, in no container's config and in no agent log line"
	"N38 a node machine sees the node CA's certificate for api.anthropic.com; certificates that CA signs for any other name (the API's host, github.com, pypi.org, example.com, a provider's subdomain) fail there with openssl, curl, Python and Node"
	"N39 the cloudex command inside a node machine: info, set and host work, and stop from inside stops it, keeping its disk"
	"N40 guest activity reaches the deployment: a background command's pings keep a sandbox with --idle 2m running, and /save-on-term is answered on the node"
	"N41 a named save made on a node starts on Cloudflare with its files, and one made there starts back on the node, and saves names the node for both (the second's content came from it)"
	"N42 a machine with NET_ADMIN can't reach another machine's agent address or forge its source, finds nothing on the agent's control address, can't use the agent as a proxy or for another sandbox's API, gets no backups or saves without a grant, and a body over 100 MB is refused"
	"N43 the agent listens only on its machine networks' addresses, on 80 and 443: never on 0.0.0.0 or its control address"
	"N44 a deploy during a save of a node sandbox ends cleanly: the sandbox keeps running, and the save either finished or left no objects in R2")
m6_skip() { for c in "${M6_CHECKS[@]}"; do skip "${c%% *}" "${c#* }" "$1"; done; }
m6_desc() { for c in "${M6_CHECKS[@]}"; do [[ ${c%% *} == "$1" ]] && echo "${c#* }"; done; }

# cloudex.internal, provider keys and named saves on nodes (M6): the agent's
# listeners, the node CA, the Worker's /v1/node/egress, and moving between a
# node and Cloudflare.
node_m6() {
	local S6=$P-n6 S6B=$P-n6b S6ID S6BID MC MCB keys providers
	cx new --on "$N" --name "$S6" --no-use
	S6ID=$(field .id)
	if [[ $CODE != 0 || -z $S6ID ]]; then
		m6_skip "couldn't make a sandbox on the node: ${ERR:0:200}"
		return
	fi
	MC=cloudex-$S6ID
	keys=$("$CX" keys --json 2>/dev/null)
	providers=$(jq -r 'map(.provider) | join(" ")' <<<"$keys" 2>/dev/null)

	# N36: the three agents' own clients, with keys the provider refuses.
	if [[ " $providers " != *" anthropic "* || " $providers " != *" openai "* ]]; then
		skip N36 "$(m6_desc N36)" "needs anthropic and openai keys on the deployment (cloudex keys), which point their hosts at the agent"
	else
		local a36
		a36=$("$CX" exec "$S6" --timeout 5m -- 'cd /tmp; grep -c "^[0-9.]* api.anthropic.com$" /etc/hosts
			echo "== claude"; ANTHROPIC_API_KEY=sk-ant-api03-cloudexfakekey0000 timeout 90 claude -p --model claude-haiku-4-5 hi 2>&1 | tail -2
			echo "== codex"; CODEX_API_KEY=sk-cloudexfakekey0000 timeout 150 codex exec --skip-git-repo-check -m gpt-5-mini hi 2>&1 | grep -F api.openai.com/v1/responses | tail -1 | cut -c1-160
			echo "== opencode"; ANTHROPIC_API_KEY=sk-ant-api03-cloudexfakekey0000 timeout 90 opencode run -m anthropic/claude-haiku-4-5 hi 2>&1 | tail -1
			echo "== placeholder"; curl -s -o /dev/null -w "%{http_code}\n" https://api.anthropic.com/v1/models -H "x-api-key: $ANTHROPIC_API_KEY" -H "anthropic-version: 2023-06-01"' 2>&1)
		OUT=$(tr '\n' ' ' <<<"$a36" | sed 's/\x1b\[[0-9;]*m//g' | head -c 600) CODE=""
		ok N36 "$(m6_desc N36)" [ "$(head -1 <<<"$a36")" = 1 -a -n "$(grep -A1 '^== claude' <<<"$a36" | grep 'Invalid API key')" \
			-a -n "$(grep -A1 '^== codex' <<<"$a36" | grep '401 Unauthorized: Incorrect API key')" -a -n "$(grep -A1 '^== opencode' <<<"$a36" | grep 'invalid x-api-key')" \
			-a "$(grep -A1 '^== placeholder' <<<"$a36" | tail -1)" = 200 ]
	fi

	# N37: the real keys, by shape and their last four characters.
	local re="" p hint pre
	while read -r p hint; do
		[[ -n $p ]] || continue
		case $p in anthropic) pre='sk-ant-' ;; openrouter) pre='sk-or-' ;; google) pre='AIza' ;; groq) pre='gsk_' ;; xai) pre='xai-' ;; openai | deepseek) pre='sk-' ;; *) pre='' ;; esac
		re="${re:+$re|}${pre}[A-Za-z0-9_-]{16,}${hint}"
	done < <(jq -r '.[] | "\(.provider) \(.hint[-4:])"' <<<"$keys" 2>/dev/null)
	if [[ -z $re ]]; then
		skip N37 "$(m6_desc N37)" "no provider keys on the deployment"
	else
		"${NODE_SSH[@]}" "cat > $NODE_DIR/keygrep.sh" <"$here/keygrep.sh"
		local g
		g=$(node_sh "bash $NODE_DIR/keygrep.sh '$re' $MC $NODE_SUDO" 2>/dev/null | tr '\n' ' ')
		OUT="matches by place (counts only): $g" CODE=""
		ok N37 "$(m6_desc N37)" [ -n "$g" -a -z "$(tr ' ' '\n' <<<"$g" | grep -E '^[0-9]+$' | grep -v '^0$')" -a "$(wc -w <<<"$g")" -ge 10 ]
	fi

	# N38: the node CA in the machine, and its name constraints.
	local api_host names c38 iss
	api_host=$(sed -E 's#^https?://([^/:]+).*#\1#' <<<"$API_URL")
	names="api.anthropic.com $api_host github.com pypi.org example.com x.api.anthropic.com"
	iss=$("$CX" exec "$S6" -- 'echo | openssl s_client -connect api.anthropic.com:443 -servername api.anthropic.com 2>/dev/null | sed -n "s/^issuer=//p"' 2>/dev/null)
	for n in $names; do
		node_sh "docker exec cloudex-node cloudex-node ca test-cert $n | docker exec -i $MC sh -c 'mkdir -p /tmp/cx-ca && cat > /tmp/cx-ca/$n.pem'" >/dev/null 2>&1
	done
	c38=$("${NODE_SSH[@]}" "docker exec -i $MC bash -l -s $names" <"$here/ca-probe.sh" 2>/dev/null)
	OUT="issuer of api.anthropic.com: $iss; $(tr '\n' ';' <<<"$c38")" CODE=""
	ok N38 "$(m6_desc N38)" [ -n "$(grep "Cloudex node CA" <<<"$iss")" -a "$(grep -c . <<<"$c38")" = 6 \
		-a "$(grep '^api.anthropic.com ' <<<"$c38")" = "api.anthropic.com ok ok ok ok" \
		-a -z "$(grep -v '^api.anthropic.com ' <<<"$c38" | grep -w ok)" ]

	# N39: the cloudex command inside, and stop from inside.
	local i1 i2 i3="" stin=""
	local e39=""
	for _ in 1 2; do
		i1=$("$CX" exec "$S6" -- 'cloudex info --json' 2>"$work/e39" | jq -r .id 2>/dev/null)
		[[ $i1 == "$S6ID" ]] && break
		e39="$e39 first try: $(head -c 200 "$work/e39");"
		sleep 3
	done
	i2=$("$CX" exec "$S6" -- 'cloudex set --idle 45m --json' 2>/dev/null | jq -r .idleMinutes)
	if [[ -n $ZONE ]]; then
		"$CX" exec -d "$S6" -- 'mkdir -p /tmp/n39 && echo n39 >/tmp/n39/index.html && cd /tmp/n39 && exec python3 -m http.server 8125' >/dev/null 2>&1
		sleep 2
		i3=$(curl -s --max-time 30 "$("$CX" exec "$S6" -- 'cloudex host 8125 --json' 2>/dev/null | jq -r .url)")
	fi
	"$CX" exec "$S6" -- 'echo n39 > ~/n39; cloudex stop' >/dev/null 2>&1
	local t39
	t39=$(now_s)
	for _ in $(seq 1 30); do stin=$("$CX" info "$S6" --json 2>/dev/null | jq -r .state); [[ $stin == stopped ]] && break; sleep 5; done
	t39=$(since "$t39")
	local kept39
	kept39=$("$CX" resume "$S6" >/dev/null 2>&1; "$CX" exec "$S6" -- 'cat ~/n39' 2>/dev/null)
	OUT="info: $i1${e39:+ ($e39)}; set --idle: $i2; host: ${i3:-no preview domain}; stopped from inside after $t39 s ($stin); its file after resume: $kept39" CODE=""
	ok N39 "$(m6_desc N39)" [ "$i1" = "$S6ID" -a "$i2" = 45 -a \( -z "$ZONE" -o "$i3" = n39 \) -a "$stin" = stopped -a "$kept39" = n39 ]

	# N40: activity from inside (a background command pings cloudex.internal
	# every minute while it prints); without it, --idle 2m stops it by 3 minutes.
	local sot st40
	sot=$("$CX" exec "$S6" -- 'curl -s -m 10 -X POST http://cloudex.internal/save-on-term' 2>/dev/null)
	"$CX" set "$S6" --idle 2m >/dev/null 2>&1
	"$CX" exec -d "$S6" -- 'for i in $(seq 1 14); do date; sleep 20; done' >/dev/null 2>&1
	sleep 240
	st40=$("$CX" info "$S6" --json 2>/dev/null | jq -r .state)
	"$CX" set "$S6" --idle 30m >/dev/null 2>&1
	OUT="after 240 s with --idle 2m and a background command printing: $st40; /save-on-term: $sot" CODE=""
	ok N40 "$(m6_desc N40)" [ "$st40" = running -a "$sot" = '{"saved":false}' ]

	# N41: moving, both ways.
	local t41 sv1 cf1 sv2 nb1 f41a f41b mv1 mv2
	"$CX" exec "$S6" -- 'echo moved > ~/n41; sudo sh -c "echo sys > /opt/n41"' >/dev/null 2>&1
	cx save "$S6" "$P-n6s"
	sv1=$(field .node.name)
	t41=$(now_s)
	cx new --from "$P-n6s" --name "$P-n6cf" --no-use
	mv1=$(since "$t41")
	local e41=""
	[[ $CODE != 0 ]] && e41=" (new said: $(grep -m1 '^error' <<<"$ERR" | head -c 200))"
	cf1=$(field '.[0].placement.provider')
	[[ -n $cf1 ]] || cf1=$("$CX" info "$P-n6cf" --json 2>/dev/null | jq -r .placement.provider)
	f41a=$("$CX" exec "$P-n6cf" -- 'cat ~/n41 /opt/n41; ls /usr/local/share/ca-certificates; grep -c "Cloudex: provider hosts" /etc/hosts' 2>/dev/null | tr '\n' ' ')
	"$CX" exec "$P-n6cf" -- 'echo back >> ~/n41' >/dev/null 2>&1
	cx save "$P-n6cf" "$P-n6c"
	sv2=$(field '.node.name // "cloudflare"')
	t41=$(now_s)
	cx new --from "$P-n6c" --on "$N" --name "$P-n6b2" --no-use
	mv2=$(since "$t41")
	nb1=$(field '.[0].placement.node.name')
	f41b=$("$CX" exec "$P-n6b2" -- 'cat ~/n41 /opt/n41; ls /usr/local/share/ca-certificates; grep -c "^[0-9.]* api.anthropic.com$" /etc/hosts' 2>/dev/null | tr '\n' ' ')
	local listed
	listed=$("$CX" saves --json=false 2>/dev/null | grep -E "^$P-n6[sc] " | awk '{print $1, $NF}' | sort | tr '\n' ' ')
	OUT="node save made on $sv1, started on $cf1 in $mv1 s$e41: $f41a; Cloudflare save made on $sv2, started on node $nb1 in $mv2 s: $f41b; saves: $listed" CODE=""
	local cfwant="moved sys cloudflare-containers-ca.crt 0 " ndwant="moved back sys cloudex-node-ca.crt 1 "
	[[ -z $providers ]] && cfwant="moved sys 0 " ndwant="moved back sys 0 "
	# The second save was made on Cloudflare, from the node's: it still names
	# the node its content came from (review W7).
	ok N41 "$(m6_desc N41)" [ "$sv1" = "$N" -a "$cf1" = cloudflare -a "$f41a" = "$cfwant" -a "$sv2" = "$N" -a "$nb1" = "$N" -a "$f41b" = "$ndwant" \
		-a "$listed" = "$P-n6c $N $P-n6s $N " ]
	"$CX" delete "$P-n6cf" --yes >/dev/null 2>&1
	"$CX" delete "$P-n6b2" --yes >/dev/null 2>&1
	"$CX" saves rm "$P-n6c" --yes >/dev/null 2>&1

	# N42: a hostile machine with NET_ADMIN, on S6's network: a container the
	# host starts there under plain Docker, or S6's own root under Sysbox.
	cx new --on "$N" --name "$S6B" --no-use
	S6BID=$(field .id)
	MCB=cloudex-$S6BID
	local runtime own other osub ctl h42
	runtime=$("$CX" node info "$N" --json 2>/dev/null | jq -r .runtime)
	own=$(node_sh "docker inspect $MC -f '{{(index .NetworkSettings.Networks \"$MC\").IPAddress}}'" | awk -F. '{printf "%s.%s.%s.%d", $1, $2, $3, $4 - 1}')
	other=$(node_sh "docker inspect $MCB -f '{{(index .NetworkSettings.Networks \"$MCB\").IPAddress}}'" | awk -F. '{printf "%s.%s.%s.%d", $1, $2, $3, $4 - 1}')
	osub=$(node_sh "docker network inspect $MCB -f '{{(index .IPAM.Config 0).Subnet}}'")
	ctl=$(node_sh "docker inspect cloudex-node -f '{{range \$k, \$v := .NetworkSettings.Networks}}{{\$k}}={{\$v.IPAddress}} {{end}}'" | tr ' ' '\n' | grep -v '^cloudex-cx-' | cut -d= -f2 | tr '\n' ' ')
	"${NODE_SSH[@]}" "cat > $NODE_DIR/hostile-probe.sh" <"$here/hostile-probe.sh"
	if [[ $runtime == sysbox ]]; then
		"$CX" cp "$here/hostile-probe.sh" "$S6:hostile-probe.sh" >/dev/null 2>&1
		h42=$("$CX" exec "$S6" --root --timeout 5m -- "API_HOST=$api_host bash /home/cloudex/hostile-probe.sh $own $other $osub $S6BID $ctl" 2>&1)
	else
		local img sub5
		img=$(node_sh "docker inspect $MC -f '{{.Config.Image}}'")
		sub5=$(awk -F. '{printf "%s.%s.%s.%d", $1, $2, $3, $4 + 3}' <<<"$own")
		h42=$(node_sh "docker run --rm --label build.cloudex.node=verify --label build.cloudex.role=check --network $MC --ip $sub5 --cap-add NET_ADMIN -e API_HOST=$api_host -v $NODE_DIR/hostile-probe.sh:/p.sh:ro --entrypoint bash $img /p.sh $own $other $osub $S6BID $ctl" 2>&1)
	fi
	cp <(echo "$h42") "$out/hostile-probe.txt"
	local want42="own-save-on-term {\"saved\":false}
own-self-no-token 403
own-token-other-sandbox 403
proxy-absolute not found
proxy-connect 405
proxy-websocket 426
tls-github.com 000
tls-pypi.org 000
tls-example.com 000
tls-$api_host 000
tls-provider-other-host 421
backup-put 501
save-put-no-grant 403
body-over-limit 413
routed-other-agent-80 000
routed-other-agent-443 000
forged-source 000"
	local got42
	got42=$(grep -v '^control-' <<<"$h42" | sed 's/ *$//')
	local ctl42
	ctl42=$(grep '^control-' <<<"$h42" | awk '{print $2}' | sort -u | tr '\n' ' ')
	OUT="on $runtime: $(tr '\n' ';' <<<"$h42" | head -c 900)" CODE=""
	ok N42 "$(m6_desc N42)" [ "$got42" = "$want42" -a "$ctl42" = "000 " ]

	# N43: the agent's listening sockets, from its own network namespace.
	local l43 bad43
	l43=$(node_sh "docker run --rm --network container:cloudex-node --entrypoint ss $(node_sh "docker inspect $MC -f '{{.Config.Image}}'") -Hltn" 2>/dev/null | awk '{print $4}' | sort | tr '\n' ' ')
	# A listener bound to its network's interface (review H4) shows as addr%eth1:port.
	local bound43
	bound43=$(tr ' ' '\n' <<<"$l43" | grep -c '%')
	l43=$(sed -E 's/%[^: ]+:/:/g' <<<"$l43")
	# 127.0.0.11 is Docker's embedded DNS resolver, which dockerd runs in every
	# container's namespace on a user network; it isn't the agent's.
	l43=$(tr ' ' '\n' <<<"$l43" | grep . | grep -v '^127\.0\.0\.11:' | tr '\n' ' ')
	bad43=$(tr ' ' '\n' <<<"$l43" | grep . | grep -vE '^10\.|^172\.' ; tr ' ' '\n' <<<"$l43" | grep . | grep -vE ':(80|443)$'; for a in $ctl; do tr ' ' '\n' <<<"$l43" | grep -F "$a:"; done)
	OUT="listening: $l43($bound43 bound to their network's interface)" CODE=""
	ok N43 "$(m6_desc N43)" [ -n "$(grep -F "$own:80" <<<"$l43")" -a -n "$(grep -F "$own:443" <<<"$l43")" -a -z "$bad43" ]

	# N44: a deploy during a node save.
	if [[ ${VERIFY_NODE_DEPLOY:-} == 1 ]]; then
		"$CX" exec "$S6" --timeout 10m -- 'head -c 3000000000 /dev/urandom > ~/n44.bin' >/dev/null 2>&1
		("$CX" save "$S6" "$P-n6d" >"$work/o44" 2>"$work/e44"; echo $? >"$work/rc44") &
		local spid=$!
		sleep 8
		cf_fresh
		"$CX" deploy --yes --skip-check >"$work/deploy44.log" 2>&1
		wait "$spid"
		local rc44 st44 saved44 acc objs ids
		rc44=$(cat "$work/rc44")
		sleep 20
		st44=$("$CX" info "$S6" --json 2>/dev/null | jq -r .state)
		saved44=$("$CX" saves --json 2>/dev/null | jq -r --arg n "$P-n6d" '.[] | select(.name == $n) | .id')
		cf_fresh
		acc=$(cf_account)
		ids=$("$CX" saves --json 2>/dev/null | jq -r '.[].id' | tr '\n' '|')
		curl -s "https://api.cloudflare.com/client/v4/accounts/$acc/r2/buckets/cloudex-backups/objects?prefix=saves/&per_page=1000" -H "Authorization: Bearer $CLOUDFLARE_API_TOKEN" >"$work/r2.json"
		local listed44
		listed44=$(jq -r '.success' "$work/r2.json")
		objs=$(jq -r '.result[]?.key' "$work/r2.json" | cut -d/ -f2 | sort -u | grep -vE "^(${ids%|})$" | tr '\n' ' ')
		OUT="save exit $rc44 ($(tail -1 "$work/e44" | head -c 160)); deploy: $(grep -c 'API deployed' "$work/deploy44.log"); the sandbox: $st44; the save: ${saved44:-not made}; R2 listed: $listed44; save ids in R2 that no save names: ${objs:-none}" CODE=""
		ok N44 "$(m6_desc N44)" [ "$st44" = running -a "$listed44" = true -a -z "$objs" ]
		"$CX" saves rm "$P-n6d" --yes >/dev/null 2>&1
	else
		skip N44 "$(m6_desc N44)" "set VERIFY_NODE_DEPLOY=1 (and CLOUDFLARE_API_TOKEN) to redeploy during the save"
	fi
	"$CX" saves rm "$P-n6s" --yes >/dev/null 2>&1
	"$CX" delete "$S6B" --yes >/dev/null 2>&1
	"$CX" delete "$S6" --yes >/dev/null 2>&1
}

# ---- node_review_host: the 2026-11-12 review's host checks (H1-H4, H6, H8, H9), N45-N51 and N53, from inside
# hostile machines on the node; N52 (R3) runs after N25 with a node of its own. host-review.py does the
# machine's side. N46-N48 and N50 need root on the host (VERIFY_NODE_SUDO=1). N47 flushes the host's
# nftables: it saves the iptables(-nft) rules first, arms a timed restore, and puts them back, so it SKIPs on
# a host with nftables tables of its own (only iptables' and inet cloudex are restored).
REVIEW_HOST_CHECKS=("N45 a machine whose root replaced /bin/sh can't hold its agent: endless output fails its start at once, a ready check that never ends fails it within 100 s, the agent stays under 200 MiB, and another machine's exec and the node's link keep working"
	"N46 a machine filling Docker's disk is stopped by the agent's watchdog once the free space is under the node's floor, before it falls 1.5 GB further, and its sandbox's notice says why"
	"N47 after systemctl restart nftables, and after nft flush ruleset, a running machine reaches the host for at most 15 s: the table comes back (at once, or within 40 s), the agent stops the machine and says why, and the self-test passes again"
	"N48 a self-test that finds the firewall open stops the running machines, saying why, not only new ones"
	"N49 a machine holding thousands of connections to its agent gets at most 256 at once, the agent stays under 200 MiB, and another machine's cloudex.internal and exec and the node's link keep working"
	"N50 a machine that takes another machine's address can't have answers sent there: UDP from the forged source draws no answer onto the other machine's network"
	"N51 a deleted machine's network isn't given to the next machine, and the agent no longer listens on its address"
	"N52 a node removed while its agent is offline: once the agent is back it stops its machines, which keep their disks, says it was removed, and the installer refuses to run over it until --uninstall"
	"N53 the join token is in no process's arguments or container's config on the host, and the node CA excludes every IP address")
review_host_skip() { for c in "${REVIEW_HOST_CHECKS[@]}"; do [[ -z ${2:-} || " $2 " == *" ${c%% *} "* ]] && skip "${c%% *}" "${c#* }" "$1"; done; }
review_host_desc() { for c in "${REVIEW_HOST_CHECKS[@]}"; do [[ ${c%% *} == "$1" ]] && echo "${c#* }"; done; }
# The agent's memory in MiB every 2 s for $1 s, into $NODE_DIR/mem-$2.log, in the background.
agent_mem_watch() {
	node_sh "nohup sh -c 'end=\$((\$(date +%s) + $1)); while [ \$(date +%s) -lt \$end ]; do docker stats --no-stream --format \"{{.MemUsage}}\" cloudex-node | awk \"{v=\\\$1; u=v; gsub(/[0-9.]/, \\\"\\\", u); sub(/[A-Za-z]+\\\$/, \\\"\\\", v); printf \\\"%.0f\\\\n\\\", (u == \\\"GiB\\\" ? v * 1024 : (u == \\\"KiB\\\" ? v / 1024 : (u == \\\"B\\\" ? 0 : v)))}\"; sleep 2; done' > $NODE_DIR/mem-$2.log 2>&1 &"
}
agent_mem_max() { node_sh "sort -n $NODE_DIR/mem-$1.log | tail -1"; }
# A sandbox on the node for a check: sets RID and RMC (its container).
review_new() {
	cx new --on "$N" --name "$1" --no-use
	RID=$(field .id)
	RMC=cloudex-$RID
	[[ $CODE == 0 && -n $RID ]]
}
machine_state() { node_sh "docker inspect $1 -f '{{.State.Status}}'" 2>/dev/null; }
# The agent's listening sockets on an address (dotted), from its network namespace.
agent_listens_on() {
	local hex
	hex=$(awk -F. '{printf "%02X%02X%02X%02X", $4, $3, $2, $1}' <<<"$1")
	node_sh "$NODE_SUDO cat /proc/\$(docker inspect cloudex-node -f '{{.State.Pid}}')/net/tcp" | awk -v h="$hex" '$4 == "0A" && index($2, h ":") == 1' | wc -l | tr -d ' '
}
# Saves the host's iptables(-nft) rules and arms their restore (with the firewall unit's) in $1 s.
# (NODE_DIR names $HOME, which is root's own in a command root runs: the
# timed restore gets the path as it is here.)
fw_save() {
	node_sh "d=$NODE_DIR; sudo iptables-save > \$d/ipt4 && sudo ip6tables-save > \$d/ipt6 && sudo systemd-run --quiet --unit cx-verify-restore-$P --on-active=$1 /bin/sh -c \"iptables-restore < \$d/ipt4; ip6tables-restore < \$d/ipt6; systemctl restart cloudex-node-firewall.service\""
}
fw_restore() {
	node_sh "sudo iptables-restore < $NODE_DIR/ipt4 && sudo ip6tables-restore < $NODE_DIR/ipt6 && sudo systemctl restart cloudex-node-firewall.service"
}
fw_disarm() { node_sh "sudo systemctl stop cx-verify-restore-$P.timer 2>/dev/null; sudo systemctl reset-failed cx-verify-restore-$P.service 2>/dev/null; true"; }
# Seconds after $2 (epoch) that the log $1 (host-review.py reach) last saw one of the host's addresses open, before $3 (epoch); "none" if never.
open_window() { awk -v t0="$2" -v t1="$3" '$1 >= t0 && $1 < t1 && $2 !~ /^169[.]254/ && $3 == "open" { w = $1 - t0 } END { if (w == "") print "none"; else printf "%.1f", w }' <<<"$1"; }
# Wait (up to $2 s) for the machine $1 to stop; prints the seconds, or nothing.
wait_stopped() {
	local t0
	t0=$(now_s)
	while [[ $(awk -v w="$(since "$t0")" -v m="$2" 'BEGIN { print (w > m) }') == 0 ]]; do
		[[ $(machine_state "$1") != running ]] && { since "$t0"; return 0; }
		sleep 0.5
	done
	return 1
}

node_review_host() {
	local h t0 r1 r1took r2 r2took okb execs bad mem notice
	if [[ $(node_state "$N") != online ]]; then
		review_host_skip "the node isn't online" "N45 N46 N47 N48 N49 N50 N51 N53"
		return
	fi
	"${NODE_SSH[@]}" "cat > $NODE_DIR/host-review.py" <"$here/host-review.py"
	local host_ip
	host_ip=$(node_sh "ip -4 route get 1.1.1.1" | sed -n 's/.* src \([0-9.]*\).*/\1/p' | head -1)

	# N45 (H1): /bin/sh replaced in A, first by endless output, then by a ready check that never ends.
	local A B
	if review_new "$P-h1a" && A=$RID && review_new "$P-h1b" && B=$RID; then
		local endless=$'#!/bin/dash\n# A hostile machine\'s sh (review H1): endless output for the agent\'s own commands.\ncase "$*" in *cloudex-ready*|*CX_EXEC*|*kill\\ -*) exec /usr/bin/yes "$(head -c 4000 /dev/zero | tr \'\\0\' A)";; esac\nexec /bin/dash "$@"\n'
		h=$(printf '%s' "$endless" | base64 | tr -d '\n')
		"$CX" exec "$P-h1a" --root -- "echo $h | base64 -d > /usr/local/sbin/cx-sh && chmod 755 /usr/local/sbin/cx-sh && ln -sf /usr/local/sbin/cx-sh /usr/bin/sh" >/dev/null 2>&1
		cx stop "$P-h1a"
		agent_mem_watch 200 h1
		t0=$(now_s)
		cx resume "$P-h1a"
		r1took=$(since "$t0") r1="code $CODE: ${ERR:0:160}"
		printf '#!/bin/dash\ncase "$*" in *cloudex-ready*) exec /usr/bin/sleep 100000;; esac\nexec /bin/dash "$@"\n' >"$work/hang-sh"
		"${NODE_SSH[@]}" "cat > $NODE_DIR/hang-sh && chmod 755 $NODE_DIR/hang-sh && docker cp $NODE_DIR/hang-sh cloudex-$A:/usr/local/sbin/cx-sh" <"$work/hang-sh"
		t0=$(now_s)
		("$CX" resume "$P-h1a" >"$work/h1o" 2>&1; echo "code $?" >>"$work/h1o"; since "$t0" >"$work/h1t") &
		execs=0 bad=""
		for _ in $(seq 1 10); do
			sleep 9
			if [[ $(timeout 30 "$CX" exec "$P-h1b" -- 'echo ok' 2>/dev/null) == ok ]]; then execs=$((execs + 1)); else bad="$bad exec-failed"; fi
			[[ $(node_state "$N") == online ]] || bad="$bad offline"
			[[ -s $work/h1t ]] && break
		done
		wait
		r2took=$(cat "$work/h1t") r2=$(tr '\n' ' ' <"$work/h1o" | grep -o 'error:.*' | head -c 160)
		mem=$(agent_mem_max h1)
		OUT="endless output: $r1 after ${r1took}s; never-ending check: ${r2:-$(tail -1 "$work/h1o")} after ${r2took}s; exec in another machine ok $execs times${bad:+, then$bad}; agent at most ${mem} MiB" CODE=""
		ok N45 "$(review_host_desc N45)" [ -n "$(grep 'printed more than 64 KiB' <<<"$r1")" -a "$(awk -v w="$r1took" 'BEGIN { print (w <= 30) }')" = 1 \
			-a -n "$(grep "wasn't ready within" <<<"$r2")" -a "$(awk -v w="${r2took:-999}" 'BEGIN { print (w <= 100) }')" = 1 -a -z "$bad" -a "$execs" -ge 5 -a "${mem:-999}" -lt 200 ]
		"$CX" delete "$P-h1a" --yes >/dev/null 2>&1
	else
		skip N45 "$(review_host_desc N45)" "couldn't make its sandboxes: ${ERR:0:200}"
	fi

	# N49 (H4): a flood from A, with B (made for N45) as the bystander.
	if review_new "$P-h4a"; then
		A=$RID
		"$CX" cp "$here/host-review.py" "$P-h4a:host-review.py" >/dev/null 2>&1
		agent_mem_watch 100 h4
		("$CX" exec "$P-h4a" --timeout 3m -- 'python3 host-review.py flood 3000 70' >"$work/h4o" 2>&1) &
		local codes="" conns=0 c
		execs=0 bad=""
		for _ in $(seq 1 6); do
			sleep 10
			c=$(timeout 30 "$CX" exec "$P-h1b" -- 'curl -s -m 10 -o /dev/null -w "%{http_code}" -X POST http://cloudex.internal/save-on-term' 2>/dev/null)
			codes="$codes $c"
			[[ $c == 200 ]] && execs=$((execs + 1))
			[[ $(node_state "$N") == online ]] || bad="$bad offline"
			c=$(node_sh "$NODE_SUDO cat /proc/\$(docker inspect cloudex-node -f '{{.State.Pid}}')/net/tcp" | awk '$4 == "01"' | wc -l | tr -d ' ')
			((c > conns)) && conns=$c
		done
		wait
		mem=$(agent_mem_max h4)
		local most
		most=$(grep '^{' "$work/h4o" | jq -r .most)
		OUT="flood: $(grep '^{' "$work/h4o" || tail -2 "$work/h4o"); the agent's established connections at most $conns; the other machine's cloudex.internal:$codes${bad:+;$bad}; agent at most ${mem} MiB" CODE=""
		ok N49 "$(review_host_desc N49)" [ -n "$most" -a "${most:-9999}" -le 256 -a "$conns" -le 300 -a "$execs" -ge 5 -a -z "$bad" -a "${mem:-999}" -lt 200 ]
		"$CX" delete "$P-h4a" --yes >/dev/null 2>&1
	else
		skip N49 "$(review_host_desc N49)" "couldn't make its sandbox: ${ERR:0:200}"
	fi

	# N51 (H8): A's /28, then A deleted and C made.
	if review_new "$P-h8a"; then
		local sub1 sub2 aaddr listen1 listen2
		sub1=$(node_sh "docker network inspect $RMC -f '{{(index .IPAM.Config 0).Subnet}}'")
		aaddr=$(awk -F'[./]' '{printf "%s.%s.%s.%d", $1, $2, $3, $4 + 2}' <<<"$sub1")
		listen1=$(agent_listens_on "$aaddr")
		cx delete "$P-h8a" --yes
		listen2=$(agent_listens_on "$aaddr")
		review_new "$P-h8c"
		sub2=$(node_sh "docker network inspect $RMC -f '{{(index .IPAM.Config 0).Subnet}}'")
		OUT="deleted machine's network $sub1 (agent listening on $aaddr: $listen1, after the delete: $listen2); the next machine's $sub2" CODE=""
		ok N51 "$(review_host_desc N51)" [ -n "$sub1" -a -n "$sub2" -a "$sub1" != "$sub2" -a "$listen1" = 2 -a "$listen2" = 0 ]
		"$CX" delete "$P-h8c" --yes >/dev/null 2>&1
	else
		skip N51 "$(review_host_desc N51)" "couldn't make its sandbox: ${ERR:0:200}"
	fi

	# N53 (H9): the token, then the CA's constraints.
	local leak cons
	leak=$(node_sh "docker inspect cloudex-node -f '{{json .Config.Cmd}} {{json .Args}} {{json .Config.Env}}'; ps -eo args" | grep -c 'cxj_')
	cons=$(in_agent ca show 2>/dev/null | sed -n '/BEGIN/,/END/p' | "${NODE_SSH[@]}" "openssl x509 -noout -text" 2>/dev/null | sed -n '/Excluded/,/Signature Algorithm/p' | grep -v 'Signature Algorithm' | tr -s ' \n' ' ')
	OUT="lines with a join token: $leak; the CA's exclusions: ${cons:-none}" CODE=""
	ok N53 "$(review_host_desc N53)" [ "$leak" = 0 -a -n "$(grep 'IP:0.0.0.0/0.0.0.0' <<<"$cons")" -a -n "$(grep 'IP:0:0:0:0:0:0:0:0/0:0:0:0:0:0:0:0' <<<"$cons")" ]

	if [[ -z $NODE_SUDO ]]; then
		review_host_skip "needs root on the node host (VERIFY_NODE_SUDO=1)" "N46 N47 N48 N50"
		"$CX" delete "$P-h1b" --yes >/dev/null 2>&1
		return
	fi

	# N50 (H6): A takes B's address (Sysbox: its own root; plain Docker: a NET_ADMIN container on A's network).
	if review_new "$P-h6a"; then
		A=$RID
		local amc=$RMC bmc=cloudex-$B vip vbr sp cap drops runtime
		runtime=$("$CX" node info "$N" --json 2>/dev/null | jq -r .runtime)
		vip=$(node_sh "docker inspect $bmc -f '{{(index .NetworkSettings.Networks \"$bmc\").IPAddress}}'")
		vbr=$(node_sh "docker network inspect $bmc -f '{{index .Options \"com.docker.network.bridge.name\"}}'")
		node_sh "d=$NODE_DIR; sudo sh -c \"timeout 15 tcpdump -n -i $vbr -c 20 udp and src host 1.1.1.1 > \$d/h6cap 2> \$d/h6err &\""
		sleep 2
		if [[ $runtime == sysbox ]]; then
			"$CX" cp "$here/host-review.py" "$P-h6a:host-review.py" >/dev/null 2>&1
			sp=$("$CX" exec "$P-h6a" --root -- "ip addr add $vip/32 dev eth0 && python3 /home/cloudex/host-review.py spoof $vip" 2>&1 | tail -1)
		else
			local img own5
			img=$(node_sh "docker inspect $amc -f '{{.Config.Image}}'")
			own5=$(node_sh "docker inspect $amc -f '{{(index .NetworkSettings.Networks \"$amc\").IPAddress}}'" | awk -F. '{printf "%s.%s.%s.%d", $1, $2, $3, $4 + 5}')
			sp=$(node_sh "docker run --rm --label build.cloudex.node=verify --label build.cloudex.role=check --network $amc --ip $own5 --cap-add NET_ADMIN -v $NODE_DIR/host-review.py:/r.py:ro --entrypoint bash $img -c 'ip addr add $vip/32 dev eth0 && python3 /r.py spoof $vip'" 2>&1 | tail -1)
		fi
		sleep 14
		cap=$(node_sh "sudo cat $NODE_DIR/h6cap | grep -c '1.1.1.1.53 >'")
		local listening
		listening=$(node_sh "sudo cat $NODE_DIR/h6err" | grep -c "listening on $vbr")
		drops=$(node_sh "sudo nft list chain inet cloudex forward" | grep "isn't on the bridge" | grep -o 'packets [0-9]*' | awk '{print $2}')
		OUT="on $runtime, as $vip ($bmc on $vbr): $sp; tcpdump on that bridge listening: $listening, answers it saw: $cap; the firewall's drop counted $drops" CODE=""
		ok N50 "$(review_host_desc N50)" [ -n "$(grep 'sent 5 from' <<<"$sp")" -a "$listening" = 1 -a "$cap" = 0 -a "${drops:-0}" -ge 5 ]
		"$CX" delete "$P-h6a" --yes >/dev/null 2>&1
	else
		skip N50 "$(review_host_desc N50)" "couldn't make its sandbox: ${ERR:0:200}"
	fi
	"$CX" delete "$P-h1b" --yes >/dev/null 2>&1

	# N46 (H2): the host's disk filled to the floor + 4 GB, then A writes.
	if review_new "$P-h2"; then
		local floor droot fill took least
		floor=$(node_sh "docker inspect cloudex-node -f '{{json .Args}}'" | jq -r '(index(["--keep-free-gb"])) as $i | if $i then .[$i + 1] else "5" end')
		droot=$(node_sh "docker info -f '{{.DockerRootDir}}'")
		fill="$(dirname "$droot")/cloudex-verify-fill-$P"
		node_sh "sudo fallocate -l \$(df -B1 --output=avail $droot | tail -1 | awk '{printf \"%d\", \$1 - ($floor + 4) * 1e9}') $fill"
		"$CX" exec "$P-h2" -- 'nohup dd if=/dev/zero of=/home/cloudex/fill bs=4M status=none >/dev/null 2>&1 &' >/dev/null 2>&1
		least=999 took=""
		t0=$(now_s)
		while [[ $(awk -v w="$(since "$t0")" 'BEGIN { print (w > 150) }') == 0 ]]; do
			local f
			f=$(node_sh "df -B1 --output=avail $droot | tail -1" | awk '{printf "%.2f", $1 / 1e9}')
			least=$(awk -v a="$f" -v m="$least" 'BEGIN { print (a < m ? a : m) }')
			[[ $(machine_state "$RMC") != running ]] && { took=$(since "$t0"); break; }
			if [[ $(awk -v a="$f" 'BEGIN { print (a < 1.5) }') == 1 ]]; then
				node_sh "docker stop -t 1 $RMC" >/dev/null 2>&1
				break
			fi
			sleep 1
		done
		node_sh "sudo rm -f $fill"
		sleep 3
		notice=$("$CX" info "$P-h2" --json 2>/dev/null | jq -r '.state + ": " + (.notice // "")')
		OUT="floor $floor GB; the machine stopped $([ -n "$took" ] && echo "after $took s" || echo "only at the safety stop of this check"), the least free was $least GB; the sandbox: ${notice:0:240}" CODE=""
		ok N46 "$(review_host_desc N46)" [ -n "$took" -a "$(awk -v l="$least" -v f="$floor" 'BEGIN { print (l >= f - 1.5) }')" = 1 \
			-a -n "$(grep '^stopped: .*under its floor' <<<"$notice")" ]
		"$CX" delete "$P-h2" --yes >/dev/null 2>&1
	else
		skip N46 "$(review_host_desc N46)" "couldn't make its sandbox: ${ERR:0:200}"
	fi

	# N47 (H3): restart nftables, then flush the ruleset, with A probing the host meanwhile.
	local others
	others=$(node_sh "sudo nft list tables" | grep -v -E '^table (ip|ip6|inet|arp|bridge) (filter|nat|mangle|raw|security)$|^table inet cloudex$' | tr '\n' ' ')
	if [[ -n $others ]]; then
		skip N47 "$(review_host_desc N47)" "the host has nftables tables of its own ($others), which a flush would lose"
	elif ! node_sh "command -v iptables-save && systemctl cat nftables.service" >/dev/null 2>&1; then
		skip N47 "$(review_host_desc N47)" "the host has no iptables-save or nftables.service"
	elif review_new "$P-h3"; then
		local gw nft_was log3 t1 t2 tb1 tb2 st2 w1 w2 iso1 iso3 dl
		gw=$(node_sh "docker network inspect $RMC -f '{{(index .IPAM.Config 0).Gateway}}'")
		nft_was=$(node_sh "systemctl is-active nftables")
		"$CX" cp "$here/host-review.py" "$P-h3:host-review.py" >/dev/null 2>&1
		"$CX" exec "$P-h3" -- "nohup python3 host-review.py reach 150 $host_ip:22 $gw:22 169.254.169.254:80 > /home/cloudex/reach.log 2>&1 &" >/dev/null 2>&1
		fw_save 300
		sleep 5
		t1=$(now_s)
		node_sh "sudo systemctl restart nftables"
		tb1=""
		for _ in $(seq 1 80); do node_sh "sudo nft list table inet cloudex" >/dev/null 2>&1 && { tb1=$(since "$t1"); break; }; sleep 0.5; done
		sleep 10
		# Docker's rules went with the rest: back as they were, then the agent
		# restarted to test again, as systemctl restart docker would do.
		fw_restore
		t0=$(now_ms)
		node_sh "docker restart cloudex-node" >/dev/null
		iso1=$(wait_iso "$t0")
		wait_state "$N" online 60 >/dev/null
		if [[ $(machine_state "$RMC") != running ]]; then
			"$CX" resume "$P-h3" >/dev/null 2>&1
			"$CX" exec "$P-h3" -- "nohup python3 host-review.py reach 90 $host_ip:22 $gw:22 > /home/cloudex/reach2.log 2>&1 &" >/dev/null 2>&1
		fi
		sleep 5
		t2=$(now_s)
		node_sh "sudo nft flush ruleset"
		st2=$(wait_stopped "$RMC" 30)
		tb2=""
		for _ in $(seq 1 100); do node_sh "sudo nft list table inet cloudex" >/dev/null 2>&1 && { tb2=$(since "$t2"); break; }; sleep 0.5; done
		fw_restore
		[[ $nft_was != active ]] && { node_sh "sudo systemctl stop nftables"; fw_restore; }
		fw_disarm
		t0=$(now_ms)
		node_sh "docker restart cloudex-node" >/dev/null
		iso3=$(wait_iso "$t0")
		wait_state "$N" online 60 >/dev/null
		log3=$(node_sh "docker cp $RMC:/home/cloudex/reach.log - | tar xO; docker cp $RMC:/home/cloudex/reach2.log - 2>/dev/null | tar xO" 2>/dev/null)
		dl=$(awk -v t="$t2" '$1 >= t' <<<"$log3" | wc -l | tr -d ' ')
		w1=$(open_window "$log3" "$t1" "$t2") w2=$(open_window "$log3" "$t2" 9999999999)
		notice=$("$CX" info "$P-h3" --json 2>/dev/null | jq -r '.state + ": " + (.notice // "")')
		OUT="restart nftables: table back after ${tb1:-never} s, the host open to the machine until +$w1 s; flush ruleset: machine stopped after ${st2:-never (30 s)} s, the host open until +$w2 s ($dl probes after the flush), table back after ${tb2:-never} s; self-tests once Docker's rules were back: ${iso1:-none}, ${iso3:-none}; the sandbox: ${notice:0:200}" CODE=""
		ok N47 "$(review_host_desc N47)" [ -n "$tb1" -a -n "$tb2" -a "$(awk -v w="$tb2" 'BEGIN { print (w <= 40) }')" = 1 -a -n "$st2" \
			-a \( "$w1" = none -o "$(awk -v w="$w1" 'BEGIN { print (w <= 15) }')" = 1 \) -a \( "$w2" = none -o "$(awk -v w="$w2" 'BEGIN { print (w <= 15) }')" = 1 \) \
			-a "$dl" -gt 0 -a "$iso1" = pass -a "$iso3" = pass -a -n "$(grep '^stopped: .*firewall table' <<<"$notice")" ]
		"$CX" delete "$P-h3" --yes >/dev/null 2>&1
	else
		skip N47 "$(review_host_desc N47)" "couldn't make its sandbox: ${ERR:0:200}"
	fi

	# N48 (H3): the table loaded without its drop for the host itself; the agent restarted to test it.
	if review_new "$P-h3b"; then
		local v48 st48 iso48
		node_sh "sudo sh -c \"grep -v 'the host itself' /etc/cloudex-node/firewall.nft > $NODE_DIR/weak.nft && nft -f $NODE_DIR/weak.nft\""
		t0=$(now_ms)
		node_sh "docker restart cloudex-node" >/dev/null
		v48=$(wait_iso "$t0")
		st48=$(wait_stopped "$RMC" 60)
		node_sh "sudo systemctl restart cloudex-node-firewall.service"
		t0=$(now_ms)
		node_sh "docker restart cloudex-node" >/dev/null
		iso48=$(wait_iso "$t0")
		notice=$("$CX" info "$P-h3b" --json 2>/dev/null | jq -r '.state + ": " + (.notice // "")')
		OUT="self-test with the host open: ${v48:-none}; the running machine stopped after ${st48:-never (60 s)} s; with the table back: ${iso48:-none}; the sandbox: ${notice:0:200}" CODE=""
		ok N48 "$(review_host_desc N48)" [ "$v48" = fail -a -n "$st48" -a "$iso48" = pass -a -n "$(grep '^stopped: .*self-test found the firewall open' <<<"$notice")" ]
		"$CX" delete "$P-h3b" --yes >/dev/null 2>&1
	else
		skip N48 "$(review_host_desc N48)" "couldn't make its sandbox: ${ERR:0:200}"
	fi
}

# N52 (R3), after N25: a node of its own, removed while its agent is down.
node_review_r3() {
	local r3 tok irc rid rmc st52 file52 stat52 again52
	cx node add "$P-r3" --json
	tok=$(field .joinToken)
	local inst=(--api "$API_URL" --agent-image "$AIMG")
	[[ -n ${VERIFY_NODE_MACHINE_IMAGE:-} ]] && inst+=(--machine-image "$VERIFY_NODE_MACHINE_IMAGE")
	[[ -z $NODE_SUDO ]] && inst+=(--unisolated)
	install_node "$tok" "${inst[@]}" >"$work/install-r3.log" 2>&1
	irc=$?
	NODE_UP=1
	if [[ $irc != 0 ]] || ! wait_state "$P-r3" online 30; then
		skip N52 "$(review_host_desc N52)" "the second install didn't come up: $(grep -v cxj_ "$work/install-r3.log" | tail -2 | tr '\n' ' ')"
	else
		cx new --on "$P-r3" --name "$P-r3s" --no-use
		rid=$(field .id) rmc=cloudex-$(field .id)
		"$CX" exec "$P-r3s" -- 'echo kept > /home/cloudex/r3.txt' >/dev/null 2>&1
		kill_agent
		wait_state "$P-r3" offline 75 >/dev/null
		cx node rm "$P-r3" --yes --force
		local rm52="node rm: code $CODE"
		start_agent
		st52=$(wait_stopped "$rmc" 60)
		file52=$(node_sh "docker cp $rmc:/home/cloudex/r3.txt - | tar xO" 2>/dev/null)
		stat52=$(in_agent status 2>/dev/null | head -1)
		node_sh "$NODE_SUDO bash $NODE_DIR/node-install.sh ${inst[*]}" >"$work/o" 2>&1
		again52="installer exit $?: $(grep -o 'was removed from its deployment.*Uninstall first (--uninstall)' "$work/o" | head -1)"
		OUT="$rm52; the machine stopped $([ -n "$st52" ] && echo "after $st52 s" || echo "not within 60 s"), its file: ${file52:-gone}; status: ${stat52:0:160}; $again52" CODE=""
		ok N52 "$(review_host_desc N52)" [ -n "$st52" -a "$file52" = kept -a -n "$(grep 'REMOVED' <<<"$stat52")" -a -n "$(grep '^installer exit [1-9]: was removed.*--uninstall' <<<"$again52")" ]
		"$CX" delete "$P-r3s" --yes >/dev/null 2>&1
	fi
	node_sh "$NODE_SUDO bash $NODE_DIR/node-install.sh --uninstall --yes --keep-images" >/dev/null 2>&1
	NODE_UP=""
	"$CX" node rm "$P-r3" --yes --force >/dev/null 2>&1
}
# ---- end node_review_host

# ---- node_review_worker: the 2026-11-12 review's Worker checks (W1, W2), N55-N56.
# Misbehaving agents, run with the node's own key while its agent is stopped
# (go/internal/node/probe_flood.go): N55 floods the control socket, N56 plays
# a node whose streams send past their credit, on a channel nothing reads,
# and down an ssh splice nobody reads. Needs the node installed ($N online)
# and $AIMG; starts the agent again at the end. With CLOUDFLARE_API_TOKEN,
# N55 also counts D1's write queries in the flood's minutes (analytics).
REVIEW_WORKER_CHECKS=("N55 a node that floods its control socket (facts, state and ticket requests for a sandbox that isn't its own, then a second hello) is cut off: no tickets for another's sandbox, the second hello closes the socket, and D1 and the Sandbox objects see at most a few writes and requests"
	"N56 a node whose streams misbehave is cut off without the Sandbox object buffering it: an exec's output past its credit, a frame on a channel nothing reads, and an ssh splice nobody reads each close with 4400")
review_worker_skip() { for c in "${REVIEW_WORKER_CHECKS[@]}"; do skip "${c%% *}" "${c#* }" "$1"; done; }
review_desc() { for c in "${REVIEW_WORKER_CHECKS[@]}"; do [[ ${c%% *} == "$1" ]] && echo "${c#* }"; done; }
d1_writes() {
	local w
	for _ in 1 2 3 4 5; do
		w=$(d1_writes_once "$@")
		[[ $w =~ ^[0-9]+$ ]] && break
		sleep 30
	done
	echo "$w"
}
d1_writes_once() {
	local acc db q
	cf_fresh
	acc=$(cf_account)
	db=$(curl -s "https://api.cloudflare.com/client/v4/accounts/$acc/d1/database?name=cloudex" -H "Authorization: Bearer $CLOUDFLARE_API_TOKEN" | jq -r '[.result[]? | select(.name == "cloudex")][0].uuid // empty')
	# No account or database: nothing was counted, which mustn't read as no writes.
	[[ -n $acc && -n $db ]] || { echo unknown; return; }
	q='query($acc:String!,$db:String!,$from:Time!,$to:Time!){viewer{accounts(filter:{accountTag:$acc}){d1AnalyticsAdaptiveGroups(limit:1000,filter:{databaseId:$db,datetimeMinute_geq:$from,datetimeMinute_leq:$to}){sum{writeQueries}}}}}'
	jq -n --arg q "$q" --arg acc "$acc" --arg db "$db" --arg from "$1" --arg to "$2" '{query:$q, variables:{acc:$acc, db:$db, from:$from, to:$to}}' |
		curl -s https://api.cloudflare.com/client/v4/graphql -H "Authorization: Bearer $CLOUDFLARE_API_TOKEN" -H 'content-type: application/json' --data @- |
		jq -r 'if .data.viewer.accounts[0] then [.data.viewer.accounts[0].d1AnalyticsAdaptiveGroups[]?.sum.writeQueries] | add // 0 else "unknown (\(.errors // [] | map(.message) | join("; ") | .[0:120]))" end'
}
# Requests to one Durable Object class of the deployment's Worker from $1 to
# $2 (ISO minutes), from the GraphQL analytics, with a few tries.
do_requests() {
	local acc ns q r
	for _ in 1 2 3 4 5; do
		cf_fresh
		acc=$(cf_account)
		ns=$(curl -s "https://api.cloudflare.com/client/v4/accounts/$acc/workers/durable_objects/namespaces?per_page=100" -H "Authorization: Bearer $CLOUDFLARE_API_TOKEN" |
			jq -r --arg w "$(jq -r .name "$repo/apps/api/wrangler.jsonc" 2>/dev/null || echo cloudex-api)" --arg c "$3" '[.result[]? | select(.script == $w and .class == $c)][0].id // empty')
		q='query($acc:String!,$ns:String!,$from:Time!,$to:Time!){viewer{accounts(filter:{accountTag:$acc}){durableObjectsInvocationsAdaptiveGroups(limit:1000,filter:{namespaceId:$ns,datetimeMinute_geq:$from,datetimeMinute_leq:$to}){sum{requests}}}}}'
		r=unknown
		[[ -n $acc && -n $ns ]] && r=$(jq -n --arg q "$q" --arg acc "$acc" --arg ns "$ns" --arg from "$1" --arg to "$2" '{query:$q, variables:{acc:$acc, ns:$ns, from:$from, to:$to}}' |
			curl -s https://api.cloudflare.com/client/v4/graphql -H "Authorization: Bearer $CLOUDFLARE_API_TOKEN" -H 'content-type: application/json' --data @- |
			jq -r 'if .data.viewer.accounts[0] then [.data.viewer.accounts[0].durableObjectsInvocationsAdaptiveGroups[]?.sum.requests] | add // 0 else "unknown" end')
		[[ $r =~ ^[0-9]+$ ]] && break
		sleep 30
	done
	echo "$r"
}
node_review_worker() {
	local other f55 t0 t1 w55 r55 fk s56 l56 ov ch sp
	if [[ $(node_state "$N") != online ]]; then
		review_worker_skip "the node isn't online"
		return
	fi
	# N55: 300 of each, for a sandbox id that's no sandbox of this node's.
	kill_agent
	wait_state "$N" offline 75 >/dev/null
	other=cx-$(LC_ALL=C tr -dc 'a-z0-9' </dev/urandom | head -c 6)
	t0=$(date -u +%Y-%m-%dT%H:%M:00Z)
	f55=$(probe flood --other "$other" --count 300)
	t1=$(date -u -v+1M +%Y-%m-%dT%H:%M:00Z 2>/dev/null || date -u -d '+1 min' +%Y-%m-%dT%H:%M:00Z)
	w55="not counted (no CLOUDFLARE_API_TOKEN)" r55=$w55
	if [[ -n ${CLOUDFLARE_API_TOKEN:-} ]]; then
		sleep 150 # the analytics lag
		w55=$(d1_writes "$t0" "$t1")
		# Every Sandbox object's requests in those minutes: the 300 states and
		# 300 ticket requests for another's sandbox must wake none (W1).
		r55=$(do_requests "$t0" "$t1" Sandbox)
	fi
	OUT="flood: ${f55:-nothing}; in $t0..$t1, D1 write queries: $w55, requests to Sandbox objects: $r55" CODE=""
	ok N55 "$(review_desc N55)" [ "$(jq -r .ticketsGiven <<<"$f55")" = 0 -a -n "$(jq -r .ticketError <<<"$f55" | grep "isn't a sandbox on this node")" \
		-a "$(jq -r .secondHello <<<"$f55")" = 4400 -a \( -z "${CLOUDFLARE_API_TOKEN:-}" -o "$(awk -v w="$w55" -v r="$r55" 'BEGIN { print (w ~ /^[0-9]+$/ && w <= 30 && r ~ /^[0-9]+$/ && r <= 30) }')" = 1 \) ]
	# N56: the fake agent, with the node's identity, and a sandbox on it.
	fk=$P-fake
	node_sh "docker run -d --name $fk -v cloudex-node:/var/lib/cloudex-node $AIMG probe fake-agent --splice --for 8m" >/dev/null 2>&1
	wait_state "$N" online 30 >/dev/null
	cx new --on "$N" --name "$P-n56" --no-use
	s56=$(field .state)
	if [[ $s56 == running ]]; then
		(timeout 90 "$CX" exec "$P-n56" -- cx-flood-overrun | sleep 45) >/dev/null 2>&1
		timeout 30 "$CX" exec "$P-n56" -- cx-flood-channel >/dev/null 2>&1
		(sleep 60 | timeout 60 "$CX" proxy "$P-n56" 22 | sleep 55) >/dev/null 2>&1
	fi
	sleep 3
	l56=$(node_sh "docker logs $fk 2>&1")
	local fkst
	fkst="the fake agent $(node_sh "docker inspect $fk -f '{{.State.Status}}'" 2>/dev/null || echo gone), its last lines: $(grep -v '^{' <<<"$l56" | tail -3 | tr '\n' ' ')"
	l56=$(grep '^{' <<<"$l56")
	# The real agent deletes the sandbox (the fake one may have gone: an edge
	# reset ends it), so that N35 finds only its own.
	node_sh "docker rm -f $fk" >/dev/null 2>&1
	start_agent
	wait_state "$N" online 30 >/dev/null
	"$CX" delete "$P-n56" --yes >/dev/null 2>&1 || "$CX" delete "$P-n56" --yes --force >/dev/null 2>&1
	ov=$(jq -c 'select(.kind == "overrun")' <<<"$l56" | head -1) ch=$(jq -c 'select(.kind == "channel")' <<<"$l56" | head -1) sp=$(jq -c 'select(.kind == "splice")' <<<"$l56" | head -1)
	OUT="sandbox on the fake agent: ${s56:-none} ${ERR:0:120}; overrun $ov; channel $ch; splice $sp; $fkst" CODE=""
	ok N56 "$(review_desc N56)" [ "$(jq -r .closeCode <<<"$ov")" = 4400 -a "$(jq -r '.sentBytes < 32*1048576' <<<"$ov")" = true \
		-a "$(jq -r .closeCode <<<"$ch")" = 4400 -a "$(jq -r .closeCode <<<"$sp")" = 4400 -a "$(jq -r '.sentBytes < 64*1048576' <<<"$sp")" = true ]
}
# ---- end node_review_worker

if want node; then
	if [[ -z ${VERIFY_NODE_SSH:-} ]]; then
		skip N00 "the node area" "set VERIFY_NODE_SSH to a command that runs a command on a host for the agent, such as 'ssh box'"
		m3_skip "no node host: set VERIFY_NODE_SSH"
		m4_skip "no node host: set VERIFY_NODE_SSH" ""
		m5_skip "no node host: set VERIFY_NODE_SSH"
		m6_skip "no node host: set VERIFY_NODE_SSH"
		review_host_skip "no node host: set VERIFY_NODE_SSH"
		review_worker_skip "no node host: set VERIFY_NODE_SSH"
	else
		read -ra NODE_SSH <<<"$VERIFY_NODE_SSH"
		NODE_DIR="\$HOME/.cloudex-verify-$P"
		NODE_SUDO=""
		[[ ${VERIFY_NODE_SUDO:-} == 1 ]] && NODE_SUDO=sudo
		AIMG=${VERIFY_NODE_AGENT_IMAGE:-cloudex-node:verify-$P}
		ok_up=1 why=""
		if node_sh "docker container inspect cloudex-node" >/dev/null 2>&1; then
			ok_up=0 why="the host runs a node agent already (cloudex-node); the checks install their own"
		elif [[ -z ${VERIFY_NODE_AGENT_IMAGE:-} ]]; then
			# Built there from this checkout, with images/node/Dockerfile.
			if (cd "$repo" && COPYFILE_DISABLE=1 tar czf "$work/node-src.tgz" go images/node) &&
				"${NODE_SSH[@]}" "mkdir -p $NODE_DIR/src && tar xzf - -C $NODE_DIR/src 2>/dev/null && cd $NODE_DIR/src && docker build -q -f images/node/Dockerfile --build-arg VERSION=verify-$P -t $AIMG . >/dev/null" <"$work/node-src.tgz" 2>"$work/e"; then
				NODE_BUILT=1
			else
				ok_up=0 why="couldn't build the agent's image there: $(tail -3 "$work/e")"
			fi
		elif ! node_sh "docker image inspect $AIMG" >/dev/null 2>&1; then
			ok_up=0 why="the host has no image $AIMG"
		fi
		[[ $ok_up == 1 ]] && { "${NODE_SSH[@]}" "mkdir -p $NODE_DIR && cat > $NODE_DIR/node-install.sh" <"$repo/scripts/node-install.sh" || ok_up=0; }
		if [[ $ok_up == 0 ]]; then
			fail N00 "the agent's image and the installer are on the node host" "${why:-could not copy them with: $VERIFY_NODE_SSH}"
		else
			N=$P-n
			BEFORE=$(host_snapshot)

			# A token that was withdrawn (its pending node removed), or never made.
			cx node add "$P-w" --json
			wtok=$(field .joinToken)
			cx node rm "$P-w" --yes
			c1=$(node_sh "docker run --rm $AIMG probe join --api $API_URL --token $wtok" 2>/dev/null | head -1)
			c2=$(node_sh "docker run --rm $AIMG probe join --api $API_URL --token cxj_$(printf 'a%.0s' $(seq 40))" 2>/dev/null | head -1)
			OUT="withdrawn $c1, made up $c2" CODE=""
			ok N01 "a join token that was withdrawn or never made gets a 401 (an expired one: unit tests)" [ "$c1" = 401 -a "$c2" = 401 ]

			# The installer (N17), then N04 from the agent's own log.
			cx node add "$N" --json
			tok=$(field .joinToken) nid=$(field .node.id)
			inst=(--api "$API_URL" --agent-image "$AIMG")
			[[ -n ${VERIFY_NODE_MACHINE_IMAGE:-} ]] && inst+=(--machine-image "$VERIFY_NODE_MACHINE_IMAGE")
			[[ -z $NODE_SUDO ]] && inst+=(--unisolated)
			t0=$(now_s)
			install_node "$tok" "${inst[@]}" >"$work/install.log" 2>&1
			irc=$?
			itook=$(since "$t0")
			NODE_UP=1
			cp "$work/install.log" "$out/node-install.log"
			wait_state "$N" online 30
			up_ms=$(node_sh "docker logs cloudex-node 2>&1" | sed -n 's/.*connected in \([0-9.]*\)\(m*s\).*/\1 \2/p' | head -1)
			cx node info "$N"
			facts=$([ "$(field .id)" = "$nid" -a "$(field .protocol)" = 1 -a "$(field '.cpus > 0')" = true -a "$(field '.rttMs >= 0')" = true ] && echo yes)
			OUT="the agent connected in $up_ms after it started; info: ${OUT:0:300}" CODE=""
			upsec=$(awk -v v="${up_ms% *}" -v u="${up_ms#* }" 'BEGIN { print (u == "ms" ? v / 1000 : v) }')
			ok N04 "node list shows the node online within 2 s of the agent starting, and info its facts and a round trip" \
				[ -n "$up_ms" -a "$(awk -v w="${upsec:-99}" 'BEGIN { print (w <= 2) }')" = 1 -a "$facts" = yes ]
			info=$("$CX" node info "$N" --json 2>/dev/null)
			OUT="installer exit $irc in $itook s; $(grep -E 'throwaway|ready in|online' "$work/install.log" | tr '\n' ' ' | head -c 300); node: $(jq -c '{runtime, isolation, image}' <<<"$info")" CODE=""
			if [[ -n $NODE_SUDO ]]; then
				tbl=$(node_sh "systemctl is-enabled cloudex-node-firewall.service; sudo nft list table inet cloudex >/dev/null && echo loaded" | tr '\n' ' ')
				OUT="$OUT; firewall: $tbl"
				ok N17 "$(m3_desc N17)" [ "$irc" = 0 -a "$(awk -v w="$itook" 'BEGIN { print (w <= 600) }')" = 1 -a "$tbl" = "enabled loaded " \
					-a "$(jq -r .isolation.firewall <<<"$info")" = pass -a "$(jq -r .image.state <<<"$info")" = present -a -n "$(grep 'a machine started and was ready' "$work/install.log")" ]
			else
				skip N17 "$(m3_desc N17)" "needs root on the node host (VERIFY_NODE_SUDO=1); the --unisolated install: $OUT"
			fi

			c=$(node_sh "docker run --rm $AIMG probe join --api $API_URL --token $tok" 2>/dev/null | head -1)
			OUT="got $c" CODE=""
			ok N02 "a join token that was used gets a 401" is "$c" 401

			# Kill it as docker kill does: no goodbye.
			kill_agent
			wait_state "$N" offline 75
			gone=$?
			OUT="offline after $WAITED s" CODE=""
			ok N05 "node list shows the node offline within 70 s of the agent being killed" [ "$gone" = 0 ]

			# With the agent down, so that the first use of the replayed nonce
			# replaces nothing.
			bad=$(probe connect --fault badsig) skewed=$(probe connect --fault skew) replay=$(probe connect --fault replay)
			badmsg=$(probe connect --fault badmsg)
			OUT="bad signature $bad, clock 120 s behind $skewed, reused nonce $replay, a message outside the protocol closed with $badmsg" CODE=""
			ok N03 "a connect with a bad signature, a skewed clock or a reused nonce gets a 401, and a bad message closes it" \
				[ "$bad" = 401 -a "$skewed" = 401 -a "$replay" = 401 -a "$badmsg" = 4400 ]

			start_agent
			wait_state "$N" online 30
			OUT="online again after $WAITED s" CODE=""
			ok N06 "the agent comes back by itself, as the same node, without a new token" [ "$(node_state "$N")" = online ]
			# It runs its self-test again at start; machines wait for it.
			for _ in $(seq 1 60); do
				[[ $("$CX" node info "$N" --json 2>/dev/null | jq -r --argjson t "$(($(now_ms) - 120000))" '(.isolation.checkedAt // 0) > $t') == true ]] && break
				sleep 2
			done
			if [[ ${VERIFY_NODE_DEPLOY:-} == 1 ]]; then
				# Poll while a deploy runs; the longest run of offline answers counts.
				cf_fresh
				("$CX" deploy --yes --skip-check >"$work/deploy.log" 2>&1; echo $? >"$work/deploy.rc") &
				dpid=$!
				worst=0 down_since=""
				while kill -0 "$dpid" 2>/dev/null || [[ -n $down_since ]]; do
					s=$(node_state "$N")
					if [[ $s == online ]]; then
						down_since=""
						kill -0 "$dpid" 2>/dev/null || break
					elif [[ -z $down_since ]]; then
						down_since=$(now_s)
					else
						worst=$(awk -v a="$worst" -v b="$(since "$down_since")" 'BEGIN { print (b > a ? b : a) }')
						[[ $(awk -v w="$worst" 'BEGIN { print (w > 120) }') == 1 ]] && break
					fi
					sleep 0.5
				done
				wait "$dpid"
				# The Worker was replaced once deploy says "API deployed". A token
				# without DNS Write (wrangler's own login has none) then fails the
				# later preview DNS step, which has nothing to do with nodes: that
				# counts as a deploy, and the detail says so.
				drc=$(cat "$work/deploy.rc")
				replaced=$(grep -q "API deployed at" "$work/deploy.log" && echo yes)
				dns_only=$([ "$drc" != 0 ] && [ "$(grep -c '^error:' "$work/deploy.log")" = 1 ] && grep -q '^error: preview DNS record' "$work/deploy.log" && echo yes)
				OUT="deploy exit $drc$([ -n "$dns_only" ] && echo " after installing the Worker, at the preview DNS record step"), longest offline ${worst} s" CODE=""
				ok N07 "a cloudex deploy doesn't show the node offline for more than 5 s" \
					[ "$replaced" = yes -a \( "$drc" = 0 -o "$dns_only" = yes \) -a "$(awk -v w="$worst" 'BEGIN { print (w <= 5) }')" = 1 ]
			else
				skip N07 "a cloudex deploy doesn't show the node offline for more than 5 s" "set VERIFY_NODE_DEPLOY=1 (and CLOUDFLARE_API_TOKEN) to redeploy during the check"
			fi

			M3_SANDBOX="" M3_ID="" REBOOTED=""
			# VERIFY_NODE_PARTS picks the milestones' checks to run after the
			# install (default all): m3 m4 m5 m6.
			parts=" ${VERIFY_NODE_PARTS:-m3 m4 m5 m6 review} "
			if [[ $parts == *" m3 "* ]]; then node_m3; else m3_skip "not in VERIFY_NODE_PARTS"; fi
			if [[ $parts == *" m4 "* ]]; then node_m4; else m4_skip "not in VERIFY_NODE_PARTS" "N18 N19 N20 N21 N22 N23 N24"; fi
			if [[ $parts == *" m5 "* ]]; then node_m5; else m5_skip "not in VERIFY_NODE_PARTS"; fi
			if [[ $parts == *" m6 "* ]]; then node_m6; else m6_skip "not in VERIFY_NODE_PARTS"; fi
			if [[ $parts == *" review "* ]]; then
				node_review_host
				node_review_worker
			else
				review_host_skip "not in VERIFY_NODE_PARTS" "N45 N46 N47 N48 N49 N50 N51 N53"
				review_worker_skip "not in VERIFY_NODE_PARTS"
			fi
			[[ -n $M3_SANDBOX ]] && "$CX" delete "$M3_SANDBOX" --yes >/dev/null 2>&1
			left=$(node_sh "docker ps -aq --filter label=build.cloudex.role=machine | wc -l; docker network ls -q --filter label=build.cloudex.role=machine | wc -l" | tr '\n' ' ')
			# N35: a node with a sandbox is removed only with --force, which
			# deletes its machine first; the sandbox stays, in error, until deleted.
			cx new --on "$N" --name "$P-n5r" --no-use
			rid=$(field .id)
			cx node rm "$N" --yes
			r35="without --force: code $CODE, ${ERR:0:120}"
			cx node rm "$N" --yes --force
			sleep 2
			st=$(node_state "$N")
			log=$(node_sh "docker logs --tail 5 cloudex-node 2>&1")
			kill_agent
			again=$(probe connect --fault none)
			OUT="state $st, connect $again, agent: ${log: -200}" CODE=""
			ok N08 "after node rm the agent is disconnected, its next connect is refused with 410 (revoked, since it holds the node's key), and the node shows revoked" \
				[ "$st" = revoked -a "$again" = 410 -a -n "$(grep 'removed from the deployment' <<<"$log")" ]
			rrec=$("$CX" info "$P-n5r" --json 2>/dev/null | jq -c '{state, error}')
			rleft=$(node_sh "docker ps -aq --filter label=build.cloudex.sandbox=$rid | wc -l" | tr -d ' ')
			"$CX" delete "$P-n5r" --yes >/dev/null 2>&1
			rgone=$("$CX" info "$P-n5r" >/dev/null 2>&1 && echo still-there || echo deleted)
			OUT="$r35; then the sandbox: $rrec; its machines left: $rleft; delete: $rgone" CODE=""
			ok N35 "$(m5_desc N35)" [ -n "$(grep "still has 1 sandbox" <<<"$r35")" -a "$(jq -r .state <<<"$rrec")" = error \
				-a -n "$(jq -r .error <<<"$rrec" | grep 'was removed (cloudex node rm --force); its machine was deleted')" -a "$rleft" = 0 -a "$rgone" = deleted ]

			# N25: the host as it was, outside the labels; then the uninstaller.
			MID_RUN=$(host_snapshot)
			node_sh "$NODE_SUDO bash $NODE_DIR/node-install.sh --uninstall --yes --keep-images" >"$work/uninstall.log" 2>&1
			urc=$?
			NODE_UP=""
			AFTER=$(host_snapshot)
			leftover=$(node_sh "docker ps -aq --filter label=build.cloudex.node | wc -l; docker network ls -q --filter label=build.cloudex.node | wc -l; docker volume ls -q --filter name=^cloudex-node\$ | wc -l; ls /etc/systemd/system/cloudex-node-firewall.service /etc/cloudex-node 2>/dev/null | wc -l" | tr '\n' ' ')
			# A reboot (N23) lets the host's own services rebuild their nftables
			# tables (a VPN's, for one) differently; then only Docker's objects,
			# and the absence of inet cloudex, count.
			cut_nft() { if [[ -n ${REBOOTED:-} ]]; then awk '/^---$/ { n++ } n < 3'; else cat; fi; }
			nb=$(diff <(grep -v 'inet cloudex' <<<"$BEFORE" | cut_nft) <(grep -v 'inet cloudex' <<<"$MID_RUN" | cut_nft) | head -5)
			na=$(diff <(cut_nft <<<"$BEFORE") <(cut_nft <<<"$AFTER") | head -5)
			grep -q 'inet cloudex' <<<"$AFTER" && na="$na; inet cloudex is still loaded"
			[[ -n ${REBOOTED:-} ]] && na="${na:-no change in the Docker objects, and no inet cloudex (the nftables tables were compared only without a reboot)}"
			OUT="machines and networks after deleting the sandboxes: $left; outside the labels during the run: ${nb:-no change}; uninstall exit $urc; left by it (containers, networks, volume, unit files): $leftover; before and after: ${na:-no change}" CODE=""
			ok N25 "$(m3_desc N25)" [ "$left" = "0 0 " -a -z "$nb" -a "$urc" = 0 -a "$leftover" = "0 0 0 0 " -a \( -z "$na" -o -n "$(grep '^no change in the Docker' <<<"$na")" \) ]
			if [[ $parts == *" review "* ]]; then node_review_r3; else review_host_skip "not in VERIFY_NODE_PARTS" N52; fi
			node_stop_all
		fi
	fi
fi

# ---------------------------------------------------------------- sdk
if want sdk; then
	if (cd "$repo" && pnpm --filter cloudex build >/dev/null 2>&1); then
		if node "$here/sdk-smoke.mjs" "$repo/packages/sdk/dist/index.js" "$P-ts" >"$work/o" 2>&1; then pass T01 "TypeScript SDK smoke"; else fail T01 "TypeScript SDK smoke" "$(tail -5 "$work/o")"; fi
		cp "$work/o" "$out/sdk-ts.log"
	else
		fail T01 "TypeScript SDK smoke" "couldn't build packages/sdk"
	fi
	if PYTHONPATH=$repo/python/src python3 "$here/sdk_smoke.py" "$P-py" >"$work/o" 2>&1; then pass Y01 "Python SDK smoke"; else fail Y01 "Python SDK smoke" "$(tail -5 "$work/o")"; fi
	cp "$work/o" "$out/sdk-py.log"
fi

total=$(($(wc -l <"$results") - 1))
echo "$total checks, $fails failed. Results: $results"
[[ $fails -eq 0 ]]
