#!/usr/bin/env bash
# Checks what Cloudex ships against a live deployment, the way a user runs it.
#   verify.sh [area...]    areas: setup lifecycle connect inside agents tokens dashboard browser sdk node (default: all)
# Env: CLOUDEX_BIN (default: cloudex on PATH), CLOUDEX_CONFIG_DIR (the deployment to use,
# default ~/.config/cloudex), VERIFY_OUT (results directory), VERIFY_KEEP=1 keeps sandboxes,
# VERIFY_BROWSERS (browser area: "chrome", the default, "firefox", or both), CHROME and FIREFOX
# (where they are, when not in the usual places), VERIFY_AGENTS=1 (the browser's agent turn
# without the agents area). The node area: VERIFY_NODE_SSH (a command prefix that runs a
# command on the host for the agent, such as "ssh box"; without it the area is a SKIP),
# VERIFY_NODE_AGENT (cloudex-node's path on that host, if it's there; else it's built for
# linux/$VERIFY_NODE_ARCH, default amd64, and copied over), VERIFY_NODE_DEPLOY=1 (N07, which
# runs cloudex deploy with $CLOUDFLARE_API_TOKEN), VERIFY_NODE_DEV_MACHINE (a running
# container on that host for N09-N16, the agent's --dev-machine), VERIFY_NODE_LONG=1 (N12's
# 30 idle minutes, and with VERIFY_NODE_DEPLOY=1 N13's deploy during an scp).
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
echo "Cloudex verify: $("$CX" version --json 2>/dev/null | jq -r '.version // empty' 2>/dev/null) against $API_URL, prefix $P, areas: ${areas[*]}"

A=$P-a
need_a() {
	[[ -n ${A_ID:-} ]] && return
	cx new --name "$A"
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
	cx new --name "$A" --env VR_ONE=1 --file ".vr/dot=$work/dotfile" --repo https://github.com/octocat/Hello-World
	A_ID=$(field .id)
	ok L01 "new starts a running sandbox with the name" [ "$(field .state)" = running -a "$(field .name)" = "$A" ]
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
	cx new --name "$A"
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
	ok L10 "stop saves and backs up" [ "$(field .state)" = stopped -a "$(field .savedBytes)" != null -a "$(field .backupError)" = null ]
	cx exec "$A" -- true
	ok L11 "exec on a stopped sandbox says to resume" [ "$CODE" = 1 -a "$(has "$ERR" resume && echo y)" = y ]
	cx resume "$A"
	ok L12 "resume runs it again" [ "$(field .state)" = running ]
	cx exec "$A" -- 'cat ~/vr-home /opt/vr-sys; test -f /tmp/vr-started && echo started; echo $VR_ONE'
	ok L13 "resume keeps the disk, runs ~/.cloudex/start, keeps env" is "$OUT" "$(printf 'kept\nkept\nstarted\n1')"
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
	cx delete "$P-s" --yes
	cx info "$P-s"
	ok L24 "delete removes it" [ "$CODE" != 0 ]
	cx saves rm "$P-base" --yes
	cx saves
	ok L25 "saves rm deletes the save" [ "$(jq -r --arg n "$P-base" '[.[] | select(.name == $n)] | length' <<<"$OUT")" = 0 ]
	cx new --from "$P-nosave" --name "$P-x"
	ok L26 "new --from something missing fails clearly" [ "$CODE" != 0 ]
	cx new --name "$P-cred" --repo "https://user:token@github.com/octocat/Hello-World"
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
	ok L28 "resume --from-backup on a running sandbox refuses" [ "$CODE" != 0 ]
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
	cx new --name "$P-reuse" --no-use
	ssh -F "$work/ssh_config" -o BatchMode=yes "$P-reuse.cloudex" true >/dev/null 2>&1
	cx delete "$P-reuse" --yes
	cx new --name "$P-reuse" --no-use
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
	(cd "$run" && "$CX" run -- 'python3 t.py; test ! -e skip.log && echo no-ignored; exit 5' >"$work/o" 2>"$work/e")
	CODE=$? OUT=$(cat "$work/o") ERR=$(cat "$work/e")
	ok C24 "run copies what git would commit and exits with the code" [ "$CODE" = 5 -a "$OUT" = "$(printf 'ran\nno-ignored')" ]
	(cd "$run" && "$CX" run --out out -- 'mkdir out && echo built >out/r.txt' >"$work/o" 2>"$work/e")
	CODE=$?
	ok C25 "run --out copies results back" [ "$CODE" = 0 -a "$(cat "$run/out/r.txt" 2>/dev/null)" = built ]
	(cd "$run" && "$CX" run --out cov/deep/lcov.info -- 'mkdir -p cov/deep && echo lcov >cov/deep/lcov.info' >"$work/o" 2>"$work/e")
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
	if [[ -z $providers || $providers == null ]]; then
		skip A00 "agents" "no provider keys on the deployment (cloudex keys set anthropic|openai)"
	else
		need_a
		cx new --name "$P-ag" --no-use
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
		as "$own" new --name "$P-own1" --no-use
		ok K02 "an own-only token makes a sandbox" [ "$CODE" = 0 ]
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
# Nodes (v0.2.2 M2): the agent, cloudex-node, joins with a token and keeps its
# control socket open. It runs as a plain process on the host VERIFY_NODE_SSH
# reaches, in ~/.cloudex-verify-<prefix> with its own state directory, and
# needs no Docker and no root yet. Each probe runs there too, with the node's
# own key (cloudex-node probe).
NODE_DIR=""
node_sh() { "${NODE_SSH[@]}" "$*" </dev/null; }
node_stop_all() {
	node_sh "[ -f $NODE_DIR/pid ] && kill \$(cat $NODE_DIR/pid) 2>/dev/null; rm -rf $NODE_DIR" >/dev/null 2>&1
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
# Streams and exec over the link (v0.2.2 M3, N09-N16), against a development
# machine: a running container on the node host, named by
# VERIFY_NODE_DEV_MACHINE, which the agent adopts for one sandbox
# (cloudex-node run --dev-machine; POST /v1/nodes/{node}/dev-sandboxes, for
# admins). Without it they are a SKIP. N12 takes 30 minutes (VERIFY_NODE_LONG=1),
# and N13 a deploy and up to 6 minutes (VERIFY_NODE_DEPLOY=1 and VERIFY_NODE_LONG=1);
# both read Durable Object analytics with $CLOUDFLARE_API_TOKEN.
M3_CHECKS=("N09 exec over the link: exit codes, stdout and stderr, the user, cwd, and a round trip p50 under 150 ms"
	"N10 data each way: 256 MiB with scp, 90 MiB with cp, and a preview's body and WebSocket, unchanged"
	"N11 a reader at 2.5 MiB/s keeps the machine's output within 16 MiB of it, and keystrokes in another ssh session stay under 200 ms"
	"N12 an idle ssh session holds for 30 minutes, and its Sandbox object is active under 60 s in that time"
	"N13 a deploy during a busy scp ends the machine's side of it when it ends the client's"
	"N14 exec --timeout 2s -- sleep 10 exits 124, and the sleep is gone"
	"N15 the dashboard terminal on a node: a shell and its exit code, tmux across reconnects, resize, htop and vim"
	"N16 nothing is left on the machine after a terminal closes, a client drops an exec, or the agent dies mid-exec; and a stream ticket works once")
m3_skip() { for c in "${M3_CHECKS[@]}"; do skip "${c%% *}" "${c#* }" "$1"; done; }
m3_desc() { for c in "${M3_CHECKS[@]}"; do [[ ${c%% *} == "$1" ]] && echo "${c#* }"; done; }
now_ms() { perl -MTime::HiRes=time -e 'printf "%d", time * 1000'; }
# Active seconds of one Durable Object from $1 to $2 (ISO times), from the GraphQL analytics.
do_active() {
	local acc q
	acc=$(curl -s https://api.cloudflare.com/client/v4/accounts -H "Authorization: Bearer $CLOUDFLARE_API_TOKEN" | jq -r '.result[0].id')
	q='query($acc:String!,$from:Time!,$to:Time!,$ids:[String!]){viewer{accounts(filter:{accountTag:$acc}){durableObjectsPeriodicGroups(limit:1000,filter:{datetimeMinute_geq:$from,datetimeMinute_leq:$to,objectId_in:$ids}){sum{activeTime}}}}}'
	jq -n --arg q "$q" --arg acc "$acc" --arg from "$1" --arg to "$2" --arg id "$3" '{query:$q, variables:{acc:$acc, from:$from, to:$to, ids:[$id]}}' |
		curl -s https://api.cloudflare.com/client/v4/graphql -H "Authorization: Bearer $CLOUDFLARE_API_TOKEN" -H 'content-type: application/json' --data @- |
		jq -r '[.data.viewer.accounts[0].durableObjectsPeriodicGroups[]?.sum.activeTime] | add // 0 | . / 1e6'
}
node_m3() {
	if [[ -z $DEV ]]; then
		m3_skip "set VERIFY_NODE_DEV_MACHINE to a running container on the node host for the agent to adopt (SKILL.md, node area)"
		return
	fi
	local NM=$P-m key M_ID ssh_cfg big ok_all why
	key=$(jq -r .apiKey "$src_cfg/config.json")
	cx node dev-sandbox "$N" --name "$NM"
	M_ID=$(field .id)
	if [[ $CODE != 0 || -z $M_ID ]]; then
		m3_skip "couldn't make a sandbox on the node's dev machine: ${ERR:0:200}"
		return
	fi
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
		g=$(curl -s --max-time 30 "$link/a?b=1") po=$(curl -s --max-time 120 -X POST --data-binary @"$big.90" "$link/p")
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
		oid=$("$CX" node dev-streams "$N" "$NM" 2>/dev/null | jq -r .objectId)
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
		base=$(node_sh "docker exec $DEV sh -c 'pgrep -c -f \"^sshd: cloudex\" || true'")
		scp -F "$ssh_cfg" -o BatchMode=yes -q -l 4000 "$big" "$NM.cloudex:/tmp/vr-slow" 2>/dev/null &
		spid=$!
		sleep 5
		"$CX" deploy --yes --skip-check >"$work/deploy2.log" 2>&1
		t_cut="" t_gone=""
		for _ in $(seq 1 420); do
			[[ -z $t_cut ]] && ! kill -0 "$spid" 2>/dev/null && t_cut=$(now_ms)
			n=$(node_sh "docker exec $DEV sh -c 'pgrep -c -f \"^sshd: cloudex\" || true'")
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
	node_sh "kill -9 \$(cat $NODE_DIR/pid)" >/dev/null 2>&1
	probe_codes=$(node_sh "$agent --state-dir $NODE_DIR/state probe stream --sandbox $M_ID" 2>/dev/null | head -1)
	start_agent
	wait_state "$N" online 30
	sleep 15
	h3=$(in_m 'ps -eo args | grep -c "[s]leep 60.0918"')
	OUT="left after a terminal closed: $h1; after the client dropped an exec: $h2; after the agent died mid-exec and came back: $h3; a ticket as issued, replayed, altered, for another stream: $probe_codes" CODE=""
	ok N16 "$(m3_desc N16)" [ "$h1" = 0 -a "$h2" = 0 -a "$h3" = 0 -a "$probe_codes" = "101 401 401 401" ]

	"$CX" delete "$NM" --yes >/dev/null 2>&1
}
if want node; then
	if [[ -z ${VERIFY_NODE_SSH:-} ]]; then
		skip N00 "the node area" "set VERIFY_NODE_SSH to a command that runs a command on a host for the agent, such as 'ssh box'"
		m3_skip "no node host: set VERIFY_NODE_SSH"
	else
		read -ra NODE_SSH <<<"$VERIFY_NODE_SSH"
		NODE_DIR="\$HOME/.cloudex-verify-$P"
		agent="$NODE_DIR/cloudex-node"
		ok_up=1
		if [[ -n ${VERIFY_NODE_AGENT:-} ]]; then
			node_sh "mkdir -p $NODE_DIR && cp '$VERIFY_NODE_AGENT' $agent" >/dev/null 2>&1 || ok_up=0
		elif (cd "$repo/go" && CGO_ENABLED=0 GOOS=linux GOARCH=${VERIFY_NODE_ARCH:-amd64} go build -o "$work/cloudex-node" ./cmd/cloudex-node) 2>"$work/e"; then
			"${NODE_SSH[@]}" "mkdir -p $NODE_DIR && cat > $agent && chmod 755 $agent" <"$work/cloudex-node" >/dev/null 2>&1 || ok_up=0
		else
			ok_up=0
		fi
		if [[ $ok_up == 0 ]] || ! node_sh "$agent --version" >/dev/null 2>&1; then
			fail N00 "the agent runs on the node host" "couldn't put cloudex-node on it with: $VERIFY_NODE_SSH"
		else
			N=$P-n
			probe() { node_sh "$agent --state-dir $NODE_DIR/state probe $*" 2>/dev/null | head -1; }
			DEV=${VERIFY_NODE_DEV_MACHINE:-}
			start_agent() { node_sh "cd $NODE_DIR && (nohup $agent run --state-dir $NODE_DIR/state ${DEV:+--dev-machine $DEV} $* </dev/null >>$NODE_DIR/agent.log 2>&1 & echo \$! > $NODE_DIR/pid)"; }

			# A token that was withdrawn (its pending node removed), or never made.
			cx node add "$P-w" --json
			wtok=$(field .joinToken)
			cx node rm "$P-w" --yes
			c1=$(probe join --api "$API_URL" --token "$wtok")
			c2=$(probe join --api "$API_URL" --token "cxj_$(printf 'a%.0s' $(seq 40))")
			OUT="withdrawn $c1, made up $c2" CODE=""
			ok N01 "a join token that was withdrawn or never made gets a 401 (an expired one: unit tests)" [ "$c1" = 401 -a "$c2" = 401 ]

			cx node add "$N" --json
			tok=$(field .joinToken) nid=$(field .node.id)
			t0=$(now_s)
			start_agent --join "$tok" --api "$API_URL"
			wait_state "$N" online 30
			up=$WAITED
			cx node info "$N"
			facts=$([ "$(field .id)" = "$nid" -a "$(field .protocol)" = 1 -a "$(field '.cpus > 0')" = true -a "$(field '.rttMs >= 0')" = true ] && echo yes)
			OUT="online $up s after the agent started ($(since "$t0") s with the ssh round trip); info: ${OUT:0:300}" CODE=""
			ok N04 "node list shows the node online within 2 s of the agent starting, and info its facts and a round trip" \
				[ "$(awk -v w="$up" 'BEGIN { print (w <= 2) }')" = 1 -a "$facts" = yes ]

			c=$(probe join --api "$API_URL" --token "$tok")
			OUT="got $c" CODE=""
			ok N02 "a join token that was used gets a 401" is "$c" 401


			# Kill it as docker kill would: no goodbye.
			node_sh "kill -9 \$(cat $NODE_DIR/pid)" >/dev/null 2>&1
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
			if [[ ${VERIFY_NODE_DEPLOY:-} == 1 ]]; then
				# Poll while a deploy runs; the longest run of offline answers counts.
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

			node_m3
			cx node rm "$N" --yes
			sleep 2
			st=$(node_state "$N")
			log=$(node_sh "tail -5 $NODE_DIR/agent.log" 2>/dev/null)
			again=$(probe connect --fault none)
			OUT="state $st, connect $again, agent: ${log: -200}" CODE=""
			ok N08 "after node rm the agent is disconnected, its next connect is refused, and the node shows revoked" \
				[ "$st" = revoked -a "$again" = 401 -a -n "$(grep 'removed from the deployment' <<<"$log")" ]
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
