#!/usr/bin/env sh
set -eu

repository_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
panel_root="$repository_root/web/management-center"
output_root="$repository_root/internal/managementasset/bundled"

if ! git -C "$panel_root" diff --quiet || ! git -C "$panel_root" diff --cached --quiet; then
  echo "management-center submodule is dirty" >&2
  exit 1
fi

if ! command -v docker >/dev/null 2>&1; then
  echo "docker is required for the canonical management-center build" >&2
  exit 1
fi

cpamc_commit=$(git -C "$panel_root" rev-parse HEAD)
build_root=$(mktemp -d)
manifest_temp=""
cleanup() {
  if [ -n "$manifest_temp" ] && [ -f "$manifest_temp" ]; then
    unlink "$manifest_temp"
  fi
  find "$build_root" -depth -type f -exec unlink {} \;
  find "$build_root" -depth -type l -exec unlink {} \;
  find "$build_root" -depth -type d -exec rmdir {} \;
}
trap cleanup EXIT HUP INT TERM

docker build \
  --build-arg "CPAMC_COMMIT=$cpamc_commit" \
  --file "$repository_root/Dockerfile.management" \
  --output "type=local,dest=$build_root" \
  "$repository_root"

mkdir -p "$output_root"
install -m 0644 "$build_root/management.html" "$output_root/management.html"

if command -v sha256sum >/dev/null 2>&1; then
  digest=$(sha256sum "$output_root/management.html" | cut -d' ' -f1)
else
  digest=$(shasum -a 256 "$output_root/management.html" | cut -d' ' -f1)
fi

built_at=$(git -C "$panel_root" show -s --format=%cI HEAD)
manifest_temp=$(mktemp "$output_root/management-artifact.XXXXXX")

jq -n \
  --arg cpamc_commit "$cpamc_commit" \
  --arg built_at "$built_at" \
  --arg digest "$digest" \
  '{schema_version: 1, cpamc_commit: $cpamc_commit, management_api: {min: 1, max: 1}, built_at: $built_at, sha256: $digest}' \
  > "$manifest_temp"
install -m 0644 "$manifest_temp" "$output_root/management-artifact.json"
unlink "$manifest_temp"
manifest_temp=""
cleanup
trap - EXIT HUP INT TERM

echo "CPAMC $cpamc_commit -> $digest"
