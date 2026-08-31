#!/usr/bin/env sh
set -eu

status=$(git submodule status --recursive)
if [ -z "$status" ]; then
  echo "required submodules are missing" >&2
  exit 1
fi

if ! printf '%s\n' "$status" | awk 'substr($0, 1, 1) != " " { invalid = 1 } END { exit invalid }'; then
  echo "$status" >&2
  echo "submodule is uninitialized, conflicted, or not at its pinned commit" >&2
  exit 1
fi

git submodule foreach --recursive 'test -z "$(git status --porcelain)"'
