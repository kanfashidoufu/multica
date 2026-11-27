#!/usr/bin/env bash
set -euo pipefail

[[ $# == 2 && "$2" =~ ^[a-zA-Z0-9][a-zA-Z0-9._-]*$ ]] || {
  echo "usage: ssh-worktree.sh <exact-workspace-registry-url> <new-task-directory-name>" >&2; exit 2;
}
registered=$1
slot=$2
multica repo list --output json | jq -e --arg repo "$registered" 'any(.[]; .url == $repo)' >/dev/null || {
  echo "repository not confirmed in the current workspace registry" >&2; exit 2;
}
host='[a-zA-Z0-9][a-zA-Z0-9.-]*'
path='[a-zA-Z0-9._-]+(/[a-zA-Z0-9._-]+)+'
if [[ $registered =~ ^https://($host)(:443)?/($path)$ ]]; then
  ssh_url="git@${BASH_REMATCH[1]}:${BASH_REMATCH[3]}"
  repo_path=${BASH_REMATCH[3]}
elif [[ $registered =~ ^git@($host):($path)$ ]]; then
  ssh_url=$registered; repo_path=${BASH_REMATCH[2]}
elif [[ $registered =~ ^ssh://git@($host)(:[0-9]+)?/($path)$ ]]; then
  ssh_url=$registered; repo_path=${BASH_REMATCH[3]}
else
  echo "cannot infer a credential-free SSH endpoint; ask for the registered SSH URL" >&2; exit 2
fi
case "/$repo_path/" in */../*|*/./*) echo "invalid repository path" >&2; exit 2;; esac

# mkdir, not mkdir -p: another bug or resumed run may own files here.
mkdir -- "$slot" || { echo "destination exists; inspect it and preserve prior work" >&2; exit 1; }
export GIT_ALLOW_PROTOCOL=ssh GIT_TERMINAL_PROMPT=0
export GIT_SSH_COMMAND="${GIT_SSH_COMMAND:-ssh -o BatchMode=yes -o StrictHostKeyChecking=yes}"
git clone --no-checkout --origin origin -- "$ssh_url" "$slot/source"
git -C "$slot/source" config protocol.https.allow never
git -C "$slot/source" config protocol.http.allow never
git -C "$slot/source" worktree add --detach ../worktree HEAD
printf 'worktree=%s/%s/worktree\n' "$PWD" "$slot"
