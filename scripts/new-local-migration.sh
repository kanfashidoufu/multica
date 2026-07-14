#!/usr/bin/env bash
set -euo pipefail

name="${1:-}"
if [[ -z "$name" ]]; then
  echo "Usage: make migration-new-local NAME=<snake_case_name>" >&2
  exit 2
fi
if [[ ! "$name" =~ ^[a-z][a-z0-9_]*$ ]]; then
  echo "Local migration NAME must be lower snake_case and start with a letter: $name" >&2
  exit 2
fi

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
migrations_dir="${MIGRATIONS_DIR:-$repo_root/server/migrations}"
local_prefix_start=900000
local_prefix_end=999999

if [[ ! -d "$migrations_dir" ]]; then
  echo "Migrations directory not found: $migrations_dir" >&2
  exit 1
fi

latest=$((local_prefix_start - 1))
shopt -s nullglob
for file in "$migrations_dir"/[0-9]*_local_*.up.sql; do
  base="${file##*/}"
  prefix="${base%%_*}"
  if [[ "$prefix" =~ ^[0-9]+$ ]]; then
    prefix_number=$((10#$prefix))
    if ((prefix_number >= local_prefix_start && prefix_number <= local_prefix_end && prefix_number > latest)); then
      latest=$prefix_number
    fi
  fi
done

next=$((latest + 1))
if ((next > local_prefix_end)); then
  echo "Local migration prefix range $local_prefix_start-$local_prefix_end is exhausted" >&2
  exit 1
fi

prefix="$(printf '%06d' "$next")"
stem="${prefix}_local_${name}"
up_file="$migrations_dir/$stem.up.sql"
down_file="$migrations_dir/$stem.down.sql"

if [[ -e "$up_file" || -e "$down_file" ]]; then
  echo "Migration already exists: $stem" >&2
  exit 1
fi

printf '%s\n' '-- TODO(local-migration): add forward migration SQL.' >"$up_file"
printf '%s\n' '-- TODO(local-migration): add rollback migration SQL.' >"$down_file"

echo "Created local migration:"
echo "  $up_file"
echo "  $down_file"
echo "Replace both TODO placeholders, then run: make migration-lint"
