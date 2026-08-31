#!/usr/bin/env sh
set -eu

repository_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
panel_root="$repository_root/web/management-center"
output_root="$repository_root/internal/managementasset/bundled"
required_bun=1.3.14

if ! git -C "$panel_root" diff --quiet || ! git -C "$panel_root" diff --cached --quiet; then
  echo "management-center submodule is dirty" >&2
  exit 1
fi

actual_bun=$(bun --version)
if [ "$actual_bun" != "$required_bun" ]; then
  echo "bun $required_bun is required, found $actual_bun" >&2
  exit 1
fi

(cd "$panel_root" && bun install --frozen-lockfile && bun run build)
mkdir -p "$output_root"
install -m 0644 "$panel_root/dist/index.html" "$output_root/management.html"

if command -v sha256sum >/dev/null 2>&1; then
  digest=$(sha256sum "$output_root/management.html" | cut -d' ' -f1)
else
  digest=$(shasum -a 256 "$output_root/management.html" | cut -d' ' -f1)
fi

cpamc_commit=$(git -C "$panel_root" rev-parse HEAD)
built_at=$(git -C "$panel_root" show -s --format=%cI HEAD)
manifest_temp=$(mktemp "$output_root/management-artifact.XXXXXX")
trap 'unlink "$manifest_temp" 2>/dev/null || true' EXIT HUP INT TERM

jq -n \
  --arg cpamc_commit "$cpamc_commit" \
  --arg built_at "$built_at" \
  --arg digest "$digest" \
  '{schema_version: 1, cpamc_commit: $cpamc_commit, management_api: {min: 1, max: 1}, built_at: $built_at, sha256: $digest}' \
  > "$manifest_temp"
install -m 0644 "$manifest_temp" "$output_root/management-artifact.json"
unlink "$manifest_temp"
trap - EXIT HUP INT TERM

echo "CPAMC $cpamc_commit -> $digest"
