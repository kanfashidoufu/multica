#!/usr/bin/env bash
set -euo pipefail

if [[ $# != 4 ]]; then
  echo "usage: verify-version-delivery.sh <repo-path> <version-branch> <pr-url> <validated-head-sha>" >&2
  exit 2
fi
repo_path=$1
version_branch=$2
pr_url=$3
validated_head=$4

git check-ref-format "refs/heads/$version_branch" >/dev/null
if [[ ! "$validated_head" =~ ^[0-9a-f]{40}$ ]]; then
  echo "validated head must be a full commit SHA" >&2
  exit 2
fi

# A PR in another repository can have the same branch name and even commits
# (for example, a fork). Resolve the explicit origin instead of gh's default.
origin_url=$(git -C "$repo_path" remote get-url origin)
repo_url=$(cd "$repo_path" && gh repo view "$origin_url" --json url --jq .url)
pr_number=${pr_url#"$repo_url"/pull/}
if [[ -z "$repo_url" || "$pr_number" == "$pr_url" || ! "$pr_number" =~ ^[0-9]+$ ]]; then
  echo "delivery unverified: PR does not belong to the checkout's origin repository" >&2
  exit 4
fi

# A successful merge command can merely enable auto-merge or enter a queue.
# Read the provider's actual state and bind it to the previously tested head.
snapshot=$(cd "$repo_path" && gh pr view "$pr_url" \
  --json state,baseRefName,headRefOid,mergeCommit,mergedAt,url \
  --jq '[.state, .baseRefName, .headRefOid, (.mergeCommit.oid // "missing"), (.mergedAt // "missing"), .url] | .[]')
fields=()
while IFS= read -r line; do fields+=("$line"); done <<< "$snapshot"
if [[ ${#fields[@]} != 6 || "${fields[0]}" != MERGED ||
      "${fields[1]}" != "$version_branch" || "${fields[2]}" != "$validated_head" ||
      ! "${fields[3]}" =~ ^[0-9a-f]{40}$ || "${fields[4]}" == missing ||
      "${fields[5]}" != "$pr_url" ]]; then
  echo "delivery unverified: PR must be merged into the confirmed branch at the validated head" >&2
  exit 4
fi
merge_commit=${fields[3]}

# Check the merge commit (not the old PR head, which squash/rebase can replace).
# An exact fetch handles narrow refspecs and refuses a deleted target branch.
# FETCH_HEAD avoids force-updating the daemon's shared remote-tracking refs.
git -C "$repo_path" fetch --no-tags origin "refs/heads/$version_branch" >&2
version_tip=$(git -C "$repo_path" rev-parse 'FETCH_HEAD^{commit}')
if ! git -C "$repo_path" merge-base --is-ancestor "$merge_commit" "$version_tip"; then
  echo "delivery unverified: merge commit is absent from the remote version branch (or history is shallow)" >&2
  exit 5
fi
printf 'delivery=merged\nversion_branch=%s\nversion_tip=%s\nmerge_commit=%s\nvalidated_head=%s\npr_url=%s\n' \
  "$version_branch" "$version_tip" "$merge_commit" "$validated_head" "$pr_url"
