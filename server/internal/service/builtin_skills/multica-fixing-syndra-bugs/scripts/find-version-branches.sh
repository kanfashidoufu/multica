#!/usr/bin/env bash
set -euo pipefail

if [[ $# -lt 1 || $# -gt 2 ]]; then
  echo "usage: find-version-branches.sh <version-name> [repo-path]" >&2
  exit 2
fi

version_name=$1
repo_path=${2:-.}

# Extract complete dotted tokens, never a prefix of a longer version. Multiple
# distinct versions need a human decision, even if only one has a remote branch.
versions=$(printf '%s\n' "$version_name" | LC_ALL=C grep -Eo '[0-9]+(\.[0-9]+)+' | LC_ALL=C sort -u || true)
if [[ -z "$versions" || "$versions" == *$'\n'* ]]; then
  echo "version is missing or ambiguous; request human version/branch confirmation" >&2
  exit 3
fi
version_regex=${versions//./\\.}

# Cached refs can be stale after a failed daemon fetch. Fail on remote errors
# instead of reporting zero matches or selecting a deleted cached branch.
remote_refs=$(git -C "$repo_path" ls-remote --heads origin)
matches=()
while IFS=$'\t' read -r sha ref; do
  [[ "$ref" == refs/heads/* ]] || continue
  branch=${ref#refs/heads/}
  if [[ "$branch" =~ (^|[^0-9.])${version_regex}([^0-9.]|$) ]]; then
    matches+=("$branch")
  fi
done <<< "$remote_refs"

printf 'version_token=%s\n' "$versions"
printf 'match_count=%d\n' "${#matches[@]}"
if (( ${#matches[@]} > 0 )); then
  printf 'branch=%s\n' "${matches[@]}"
fi
