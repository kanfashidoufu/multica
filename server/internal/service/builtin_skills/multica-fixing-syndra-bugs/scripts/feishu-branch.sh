#!/usr/bin/env bash
set -euo pipefail
set +x
case "${MULTICA_TOKEN:-}" in mat_*) ;; *) echo 'Requires the current Feishu chat task credential.' >&2; exit 2;; esac
[[ "$MULTICA_TOKEN" != *$'\n'* && "$MULTICA_TOKEN" != *$'\r'* ]] || exit 2
base=${MULTICA_SERVER_URL:?Missing task server URL}
case "$base" in http://*|https://*) ;; *) echo 'Invalid task server URL.' >&2; exit 2;; esac
endpoint="${base%/}/api/issues/syndra-feishu-branches"
args=(--max-time 25)
case "${1:-}" in
  options)
    [[ $# == 1 || $# == 2 ]] || exit 2
    if [[ $# == 2 ]]; then args+=(--get --data-urlencode "issue=$2"); fi ;;
  choose|replace)
    [[ $# == 5 ]] || exit 2
    body=$(jq -cn --arg issue "$2" --arg repo "$3" --arg branch "$4" --arg fragment "$5" --arg op "$1" '{issue:$issue,repo:$repo,branch:$branch,fragment:$fragment,replace:($op=="replace")}')
    args+=(--header 'Content-Type: application/json' --data-binary "$body") ;;
  *) echo 'Use options, or choose|replace ISSUE REPO BRANCH CURRENT_USER_WORDS.' >&2; exit 2 ;;
esac
# The bearer stays off argv. Never follow redirects or load a personal curlrc.
printf 'Authorization: Bearer %s\n' "$MULTICA_TOKEN" |
  curl --disable --silent --show-error --fail-with-body --proto '=https,http' --header @- "${args[@]}" "$endpoint"
