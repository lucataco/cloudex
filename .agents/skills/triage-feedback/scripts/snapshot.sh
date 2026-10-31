#!/usr/bin/env bash
# Lists the open issues on lucataco/cloudex for a triage run, feedback reports
# first, and marks the ones new or changed since the last run. State lives in
# .triage/ at the top of the repo, which git ignores.
#   snapshot.sh        start a run: refuses while another is going (under 6 hours old)
#   snapshot.sh done   end the run: the issues as they are now, with this run's own
#                      replies and labels, become "the last run"
set -euo pipefail
repo=${CLOUDEX_REPO:-lucataco/cloudex}
cd "$(git rev-parse --show-toplevel)"
mkdir -p .triage

# The open issues, as a JSON array in $1; fails without touching $1.
fetch() {
  local tmp
  tmp=$(mktemp .triage/fetch.XXXXXX)
  if gh issue list --repo "$repo" --state open --limit 300 \
    --json number,title,labels,updatedAt,createdAt,comments,author >"$tmp" &&
    jq -e 'type == "array"' "$tmp" >/dev/null; then
    mv "$tmp" "$1"
  else
    rm -f "$tmp"
    echo "couldn't list the issues on $repo (is gh signed in?)" >&2
    return 1
  fi
}

if [ "${1:-}" = done ]; then
  # The issues as they are now, with this run's own replies and labels. An
  # issue someone else commented on during the run keeps its start-of-run
  # state, so the next run sees it as changed.
  start=$(cat .triage/running 2>/dev/null || echo 0)
  me=$(gh api --hostname github.com user --jq .login)
  fetch .triage/now.json
  [ -f .triage/current.json ] || echo '[]' >.triage/current.json
  jq --slurpfile before .triage/current.json --arg me "$me" --argjson start "$start" '
    ($before[0] | map({key: (.number | tostring), value: .updatedAt}) | from_entries) as $then
    | map(if any(.comments[]; .author.login != $me and ((.createdAt | fromdateiso8601) >= $start))
          then .updatedAt = ($then[.number | tostring] // "") else . end)
  ' .triage/now.json >.triage/last.json
  rm -f .triage/running .triage/current.json .triage/now.json
  echo "Run ended."
  exit 0
fi

started=$(cat .triage/running 2>/dev/null || true)
if [[ $started =~ ^[0-9]+$ ]] && [ $(($(date +%s) - started)) -lt 21600 ]; then
  echo "Another triage run is going (started $(( ($(date +%s) - started) / 60 )) minutes ago); end it with: $0 done" >&2
  exit 1
fi
date +%s >.triage/running
trap 'rm -f .triage/running' ERR
fetch .triage/current.json
trap - ERR
jq -e 'type == "array"' .triage/last.json >/dev/null 2>&1 || echo '[]' >.triage/last.json

seen='($last[0] // [] | map({key: (.number | tostring), value: .updatedAt}) | from_entries) as $seen'
jq -r --slurpfile last .triage/last.json "$seen"'
  | sort_by((.labels | map(.name) | index("feedback") | not), .number)
  | .[]
  | (if ($seen[.number | tostring] // null) == null then "NEW    "
     elif $seen[.number | tostring] != .updatedAt then "CHANGED"
     else "       " end) as $mark
  | "\($mark) #\(.number)  [\(.labels | map(.name) | join(","))]  \(.title)  (\(.comments | length) comments, by \(.author.login))"
' .triage/current.json

new=$(jq --slurpfile last .triage/last.json "$seen"' | map(select(($seen[.number | tostring] // null) != .updatedAt)) | length' .triage/current.json)
echo
echo "$(jq length .triage/current.json) open, $new new or changed since the last run."
[ "$new" -gt 0 ] || echo "Nothing to do; end the run with: $0 done"
