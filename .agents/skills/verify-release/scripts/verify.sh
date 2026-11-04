#!/usr/bin/env bash
# Checks what Cloudex ships against a live deployment, the way a user runs it.
#   verify.sh [area...]    areas: setup lifecycle connect inside agents sdk (default: all)
# Env: CLOUDEX_BIN (default: cloudex on PATH), CLOUDEX_CONFIG_DIR (the deployment to use,
# default ~/.config/cloudex), VERIFY_OUT (results directory), VERIFY_KEEP=1 keeps sandboxes.
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
pass() { printf '\033[32mPASS\033[0m %-5s %s\n' "$1" "$2"; note "$1" PASS "$2" ""; }
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
	if "$@" >/dev/null 2>&1; then pass "$id" "$desc"; else fail "$id" "$desc" "code=$CODE out=${OUT:0:300} err=${ERR:0:300}"; fi
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
	echo "results: $results"
}
trap cleanup EXIT

want() { [[ $# -eq 0 || " ${areas[*]} " == *" $1 "* ]]; }
areas=("$@")
[[ ${#areas[@]} -eq 0 ]] && areas=(setup lifecycle connect inside agents sdk)
echo "Cloudex verify: $("$CX" version 2>&1 | head -1) against $API_URL, prefix $P, areas: ${areas[*]}"

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
	out=$OUT
	cx info "$P-f-1"
	ok L30 "a quiet foreground exec keeps a sandbox from idling" [ "$out" = alive -a "$(field .state)" = running ]
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
		cx exec "$A" -- 'stat -c %u /'
		ok C22c "/ is owned by root, so tools that check it work" is "$OUT" 0
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
		for pair in "anthropic claude" "openai codex"; do
			set -- $pair
			[[ " $providers " == *" $1 "* ]] || { skip "A-$2" "$2" "no $1 key"; continue; }
			cx exec "$P-ag" -- "case $1 in anthropic) curl -s -o /dev/null -w '%{http_code}' https://api.anthropic.com/v1/models -H \"x-api-key: \$ANTHROPIC_API_KEY\" -H 'anthropic-version: 2023-06-01';; openai) curl -s -o /dev/null -w '%{http_code}' https://api.openai.com/v1/models -H \"Authorization: Bearer \$OPENAI_API_KEY\";; esac"
			ok "A1-$2" "$1 key works from a sandbox through the placeholder" is "$OUT" 200
			cx exec "$P-ag" -- "env | grep -E '^(ANTHROPIC|OPENAI)_API_KEY=' | grep -cE '=(sk-ant-api|sk-proj-)[A-Za-z0-9_-]{40,}'"
			ok "A2-$2" "the real $1 key is not in the sandbox" is "$OUT" 0
			cx agents start "$P-ag" --agent "$2" "Reply with exactly the word VERIFYOK and nothing else. Do not run any tools."
			code=$CODE
			cx agents "$P-ag"
			ok "A3-$2" "agents start $2 finishes a turn with the answer" [ "$code" = 0 -a "$(field 'max_by(.updatedAt).lastMessage')" = VERIFYOK ]
			cx agents send "$P-ag" "Now reply with exactly the word you replied with before, then the word SECONDOK, separated by a space."
			cx agents "$P-ag"
			ok "A4-$2" "agents send continues the same conversation" [ "$(field 'max_by(.updatedAt).lastMessage')" = "VERIFYOK SECONDOK" ]
			ok "A5-$2" "agents lists it" [ "$(field 'max_by(.updatedAt).agent')" = "$2" ]
			cx agents logs "$P-ag"
			ok "A6-$2" "agents logs prints both turns" [ "$(grep -c '"type":"end"' <<<"$OUT")" -ge 2 ]
			cx agents start "$P-ag" --agent "$2" -d "Count from 1 to 100000 in a bash loop, printing each number, then summarize."
			sleep 15
			cx agents interrupt "$P-ag"
			ok "A7-$2" "agents interrupt stops a running turn" [ "$CODE" = 0 ]
		done
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
