#!/usr/bin/env bash
set -euo pipefail

if [[ $# != 3 ]]; then
  echo "usage: verify-test-promotion.sh <repo-path> <test-branch> <version-tip-sha>" >&2
  exit 2
fi

repo_path=$1
test_branch=$2
version_tip=$3

git check-ref-format "refs/heads/$test_branch" >/dev/null
if [[ ! "$version_tip" =~ ^[0-9a-f]{40}$ ]]; then
  echo "version tip must be a full commit SHA" >&2
  exit 2
fi

git -C "$repo_path" fetch --no-tags origin "refs/heads/$test_branch" >&2
test_tip=$(git -C "$repo_path" rev-parse 'FETCH_HEAD^{commit}')
if ! git -C "$repo_path" merge-base --is-ancestor "$version_tip" "$test_tip"; then
  echo "promotion unverified: test does not contain the confirmed version tip" >&2
  exit 4
fi

printf 'promotion=merged\ntest_branch=%s\ntest_tip=%s\nversion_tip=%s\n' \
  "$test_branch" "$test_tip" "$version_tip"
