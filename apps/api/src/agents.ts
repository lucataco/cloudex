// Agents through the API: Claude Code or Codex working on a task with no
// terminal (see Agent and AgentEvent in @cloudex/schema). Each message is one
// turn, run headless (claude -p --output-format stream-json, codex exec --json);
// later turns resume the agent's own session. Everything lives in the sandbox,
// in ~/.cloudex/agents/<id>, so saves, resumes and forks keep it:
//
//   meta.json     id, agent, cwd, model, createdAt
//   state.json    turns, sessionId, lastMessage, lastExitCode, error, updatedAt,
//                 and current: the turn in progress, if any
//   events.jsonl  the log, one event per line; seq is the line number
//   queue/        messages waiting, one file each, named by time
//
// A runner (the same script, op run) takes queued messages one turn at a time
// and exits when the queue is empty. It holds run.lock while it lives, which is
// how the other operations tell it's there. Messages are queued under q.lock,
// which the runner also holds when it decides to exit, so none is left behind.
//
// The API sends the script with each call (AGENT_BOOT installs it), so any
// image can run agents, like the desktop.

export const isAgentId = (s: string) => /^ag-[a-z0-9]{8}$/.test(s);

// Exit codes the script uses for errors meant for the caller.
export const AGENT_NOT_FOUND = 3;
export const AGENT_BAD_REQUEST = 4;

// $1 is the script. Installs it as ~/.cloudex/agent.sh when it changed (the
// runner runs from the file), then runs it with the remaining arguments.
export const AGENT_BOOT = String.raw`
f=$HOME/.cloudex/agent.sh
mkdir -p "$HOME/.cloudex"
printf %s "$1" | cmp -s - "$f" 2>/dev/null || { printf %s "$1" >"$f.$$" && mv "$f.$$" "$f"; }
shift
exec bash "$f" "$@"
`;

// Run as the cloudex user from a login shell, so agents see the provider key
// placeholders. Operations print JSON on stdout:
//   start <id> <claude|codex> <cwd or ''> <model or ''>   prompt on stdin
//   send <id> <interrupt 0|1>                              prompt on stdin
//   interrupt <id> | get <id> | list
//   events <id> <after> <seconds to wait for one>
export const AGENT_SCRIPT = String.raw`
set -uo pipefail
op=$1
shift
root=$HOME/.cloudex/agents
mkdir -p "$root"

now() { echo $(($(date +%s%N) / 1000000)); }
fail() { echo "$2" >&2; exit "$1"; }
open() {
  d=$root/$1
  [ -f "$d/meta.json" ] || fail 3 "no agent $1"
}
# The runner holds run.lock while it lives.
running() { ! flock -n "$1/run.lock" true; }
busy() { running "$1" || [ -n "$(ls "$1/queue")" ]; }
locked() {
  exec 9>"$d/q.lock"
  flock 9
  "$@"
  exec 9>&-
}
set_state() { jq -c "$@" "$d/state.json" >"$d/state.new" && mv "$d/state.new" "$d/state.json"; }
end_turn() { # turn, exit code or null, interrupted, error or ''
  jq -cn --argjson turn "$1" --argjson at "$(now)" --argjson code "$2" --argjson int "$3" --arg err "$4" \
    '{type: "end", turn: $turn, at: $at, exitCode: $code, interrupted: $int, error: (if $err == "" then null else $err end)}
     | if .exitCode == null then del(.exitCode) else . end' >>"$d/events.jsonl"
}
launch() {
  running "$d" || setsid -f bash "$0" run "$d" </dev/null >/dev/null 2>>"$d/runner.log" 9>&-
}

show() {
  local state=idle
  busy "$1" && state=running
  jq -c -n --slurpfile m "$1/meta.json" --slurpfile s "$1/state.json" --arg state "$state" \
    --argjson queued "$(ls "$1/queue" | wc -l)" --argjson events "$(wc -l <"$1/events.jsonl")" \
    '$m[0] + {state: $state} + ($s[0] | del(.current)) + {queued: $queued, events: $events}'
}

# Under q.lock. A turn whose runner is gone without ending it was cut off by a
# stop or a fork; queued messages go with it. Messages queued without a runner
# (one that failed to start) get one.
reconcile() {
  running "$d" && return 0
  local cur
  cur=$(jq '.current // empty' "$d/state.json")
  if [ -n "$cur" ]; then
    rm -f "$d"/queue/*.json
    local err="this turn didn't finish: the sandbox stopped, or was forked, while it ran"
    end_turn "$cur" null true "$err"
    set_state --arg err "$err" --argjson at "$(now)" '.current = null | .lastMessage = null | .lastExitCode = null | .error = $err | .updatedAt = $at'
  elif [ -n "$(ls "$d/queue")" ]; then
    launch
  fi
}

# Under q.lock: drops queued messages and stops the running turn's process group.
stop_turn() {
  rm -f "$d"/queue/*.json
  running "$d" || return 0
  touch "$d/interrupted"
  local pg
  pg=$(cat "$d/turn.pid" 2>/dev/null) || return 0
  kill -INT -- "-$pg" 2>/dev/null || return 0
  for sig in TERM KILL; do
    for _ in $(seq 30); do
      kill -0 -- "-$pg" 2>/dev/null || return 0
      sleep 0.1
    done
    kill -"$sig" -- "-$pg" 2>/dev/null
  done
}

# Under q.lock; the prompt on stdin.
enqueue() {
  reconcile
  [ "$1" = 1 ] && stop_turn
  jq -Rsc '{prompt: .}' >"$d/queue/.new" && mv "$d/queue/.new" "$d/queue/$(date +%s%N).json"
  launch
}

# One line of agent output as an event: its JSON event, or the line as text.
wrap='select(length > 0)
  | (if length > 262144 then {type: "output", text: .[0:4096], truncated: true}
     else (try fromjson catch null) as $e
     | if ($e | type) == "object" then {type: "event", event: $e} else {type: "output", text: .} end
     end) as $x
  | {type: $x.type, turn: $turn, at: (now * 1000 | floor)} + $x'

# The agent's conversation id, its final message, and its own account of a
# failure, from one turn's events.
summary='map(select(.type == "event") | .event) as $ev
  | {sessionId: ([$ev[] | .session_id // .thread_id // empty] | last),
     lastMessage: ([$ev[] | if .type == "result" and .is_error != true then .result
                            elif .type == "item.completed" and .item.type == "agent_message" then .item.text
                            else empty end] | last),
     failure: ([$ev[] | if .type == "result" and .is_error == true then (.result // .subtype)
                        elif .type == "turn.failed" then .error.message
                        else empty end] | last),
     lastError: ([$ev[] | select(.type == "error") | .message] | last)}'

# Counts as activity for idle stop while the turn writes events.
pinger() {
  while sleep 60; do
    kill -0 $$ 2>/dev/null || exit 0
    [ -z "$(find events.jsonl -mmin -10)" ] || curl -fsS -m 10 -X POST http://cloudex.internal/activity >/dev/null 2>&1 || true
  done
}

run_turn() { # turn
  local turn=$1 from sid code=130 interrupted=false sum err ping
  from=$(wc -l <events.jsonl)
  jq -cn --argjson turn "$turn" --argjson at "$(now)" --rawfile text prompt.txt \
    '{type: "prompt", turn: $turn, at: $at, text: $text}' >>events.jsonl
  sid=$(jq -r '.sessionId // ""' state.json)
  case $agent in
    claude)
      set -- claude -p --output-format stream-json --verbose --dangerously-skip-permissions
      [ -z "$model" ] || set -- "$@" --model "$model"
      [ -z "$sid" ] || set -- "$@" --resume "$sid"
      ;;
    codex)
      set -- codex exec --json --dangerously-bypass-approvals-and-sandbox --skip-git-repo-check
      [ -z "$model" ] || set -- "$@" -m "$model"
      [ -z "$sid" ] || set -- "$@" resume "$sid"
      set -- "$@" -
      ;;
  esac
  : >turn.err
  if [ ! -e interrupted ]; then
    # Only the runner holds run.lock (fd 8): something the agent leaves running
    # mustn't keep the agent looking busy.
    pinger 8>&- &
    ping=$!
    # Its own process group (turn.pid), so an interrupt reaches everything it started.
    (cd "$cwd" && exec setsid bash -c 'echo $$ >"$0/turn.pid" && exec "$@"' "$d" "$@") <prompt.txt 2>turn.err 8>&- |
      jq -cR --unbuffered --argjson turn "$turn" "$wrap" >>events.jsonl 8>&-
    code=$?
    kill "$ping" 2>/dev/null
  fi
  rm -f turn.pid
  cat turn.err >>stderr.log
  [ -e interrupted ] && interrupted=true
  sum=$(tail -n +"$((from + 1))" events.jsonl | jq -cs "$summary")
  err=$(jq -r '.failure // ""' <<<"$sum")
  if $interrupted; then
    err=interrupted
  elif [ "$code" != 0 ] && [ -z "$err" ]; then
    err=$(jq -r '.lastError // ""' <<<"$sum")
    [ -n "$err" ] || err=$(grep -v '^[[:space:]]*$' turn.err | tail -n 1 | cut -c1-2000)
    [ -n "$err" ] || err="$agent exited with code $code"
  fi
  end_turn "$turn" "$code" "$interrupted" "$err"
  set_state --argjson s "$sum" --argjson code "$code" --arg err "$err" --argjson at "$(now)" \
    '.sessionId = ($s.sessionId // .sessionId) | .lastMessage = $s.lastMessage | .lastExitCode = $code
     | .error = (if $err == "" then null else $err end) | .current = null | .updatedAt = $at'
}

case $op in
  start)
    id=$1 agent=$2 cwd=$3 model=$4
    command -v "$agent" >/dev/null || fail 4 "$agent isn't installed in this sandbox"
    cd "$HOME" && cwd=$(cd "$cwd" 2>/dev/null && pwd) || fail 4 "there's no directory $3 in the sandbox"
    d=$root/$id
    mkdir -p "$d/queue"
    at=$(now)
    jq -cn --argjson at "$at" '{turns: 0, sessionId: null, lastMessage: null, lastExitCode: null, error: null, updatedAt: $at, current: null}' >"$d/state.json"
    : >"$d/events.jsonl"
    jq -cn --arg id "$id" --arg agent "$agent" --arg cwd "$cwd" --arg model "$model" --argjson at "$at" \
      '{id: $id, agent: $agent, cwd: $cwd, model: (if $model == "" then null else $model end), createdAt: $at}' >"$d/meta.json"
    locked enqueue 0
    show "$d"
    ;;
  send)
    open "$1"
    locked enqueue "$2"
    show "$d"
    ;;
  interrupt)
    open "$1"
    locked stop_turn
    # Give the runner a moment to end the turn, so the answer shows it.
    for _ in $(seq 50); do running "$d" || break; sleep 0.1; done
    show "$d"
    ;;
  get)
    open "$1"
    locked reconcile
    show "$d"
    ;;
  list)
    for m in "$root"/*/meta.json; do
      [ -e "$m" ] || continue
      d=$(dirname "$m")
      locked reconcile
      show "$d"
    done | jq -cs 'sort_by(-.createdAt)'
    ;;
  events)
    open "$1"
    after=$2
    locked reconcile
    for _ in $(seq $(($3 * 4))); do
      [ "$(wc -l <"$d/events.jsonl")" -gt "$after" ] && break
      busy "$d" || break
      sleep 0.25
    done
    LC_ALL=C awk -v after="$after" -v max=524288 \
      'NR > after { if (n && bytes + length($0) > max) exit; print; bytes += length($0) + 1; n++ }' "$d/events.jsonl" |
      jq -cs --argjson after "$after" --argjson agent "$(show "$d")" \
        '{events: [to_entries[] | {seq: (.key + $after + 1)} + .value], next: ($after + length), agent: $agent}'
    ;;
  run)
    d=$1
    exec 8>"$d/run.lock"
    flock -w 10 8 || exit 0
    cd "$d" || exit 1
    agent=$(jq -r .agent meta.json) cwd=$(jq -r .cwd meta.json) model=$(jq -r '.model // ""' meta.json)
    while :; do
      exec 9>q.lock
      flock 9
      next=$(ls queue | head -n 1)
      if [ -z "$next" ]; then
        flock -u 8
        exit 0
      fi
      turn=$(($(jq .turns state.json) + 1))
      jq -j .prompt "queue/$next" >prompt.txt
      rm -f "queue/$next" interrupted turn.pid
      set_state --argjson t "$turn" --argjson at "$(now)" '.turns = $t | .current = $t | .updatedAt = $at'
      exec 9>&-
      run_turn "$turn"
    done
    ;;
  *) fail 2 "unknown operation $op" ;;
esac
`;
