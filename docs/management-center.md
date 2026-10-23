# Management center packaging

CPA serves the CPAMC control panel from one of two places: a mutable panel that the
auto-updater downloads into the writable static directory, or a panel embedded in the
binary at build time. The embedded panel is build output; it is not committed to this
repository.

The `web/management-center` submodule is therefore a REQUIRED build dependency for
any CPA build that ships a panel. A plain `go build` still compiles without it and
serves an explanatory error page instead of a panel; a build that is expected to
provide a working panel (release archive, container image, PR build) has to build the
bundle first.

## Clone and verify

```bash
git clone --recurse-submodules https://github.com/Type-Delta/CLIProxyAPI.git
cd CLIProxyAPI
git submodule update --init --recursive
scripts/verify-submodules.sh
```

To repair an uninitialized checkout without changing the pinned commit:

```bash
git submodule sync --recursive
git submodule update --init --recursive
scripts/verify-submodules.sh
```

## Bundle layout

`scripts/build-management-center.sh` writes the following, and git ignores everything
in it except the placeholder:

```
internal/managementasset/bundled/
  .gitkeep                                 tracked; the only tracked file
  management.html                          CPAMC dist/index.html, single file
  management-artifact.json                 compatibility manifest
  management-assets/mascots/<family>.webp  optional public assets
```

`<family>` is one of claude, deepseek, gemini, glm, gpt, grok, kimi, qwen.

## Build the bundle locally

```bash
scripts/build-management-center.sh
```

The script builds the panel in the digest-pinned Bun 1.3.14 container described by
`Dockerfile.management` so native Rolldown differences on the host cannot change the
released bytes. It requires Docker and `jq`. The submodule must be clean; for a local
build from a dirty panel checkout, set `ALLOW_DIRTY_PANEL=1`:

```bash
ALLOW_DIRTY_PANEL=1 scripts/build-management-center.sh
```

The script rejects a build whose `dist/` contains anything besides `index.html` and
`management-assets/`, so a panel that starts emitting stray files outside the single
file fails the build instead of shipping a broken bundle.

A checkout without a bundle still builds:

```bash
go build -o cli-proxy-api ./cmd/server
```

That binary serves HTTP 503 with a short instruction page at `/management.html`, and
404 for every `/management-assets/` request. Management panel auto-update keeps
working and can still install a verified mutable panel at runtime; the 503 page only
appears while neither panel is available.

## Panel assets outside management.html

`management.html` stays a single self-contained file. The only files that may live
next to it are optional public assets from
`web/management-center/public/management-assets/`, which Vite copies to `dist/`
verbatim and serves at the same path in development.

The panel references them with relative URLs such as
`management-assets/mascots/claude.webp` (no leading slash). CPAMC uses hash routing,
so the document path never changes and the relative URL resolves to the Vite dev
server in development and to CPA in production.

CPA serves `GET /management-assets/*` from the embedded bundle only, with the same
gating as `/management.html`: 404 while home mode is enabled or the control panel is
disabled, no management key, and `Cache-Control: public, max-age=86400`. Any missing
file is a 404, which makes the panel fall back to the lab logo rather than showing a
broken image, so assets are always optional.

## Container images and release archives

- `Dockerfile.management` is the canonical panel builder. It exports
  `management.html` and `management-assets/`; the script adds the manifest with the
  CPAMC commit time and the HTML digest.
- `Dockerfile` and `Dockerfile.web` build the panel from the pinned submodule in
  their own Bun stage instead of copying committed bytes. They generate the manifest
  inside the build from the `CPAMC_COMMIT` and `BUILD_DATE` build arguments, because
  `.dockerignore` excludes `.git`.
- The release workflow builds the bundle once on Linux, uploads it as an artifact, and
  every matrix target (including the macOS and Windows runners, which have no Docker)
  downloads it before `go build`. Archives ship `management.html`,
  `management-artifact.json`, and `management-assets/` next to the binary.

Example image build:

```bash
docker build \
  --build-arg CPAMC_COMMIT="$(git -C web/management-center rev-parse HEAD)" \
  --build-arg PANEL_BUILT_AT="$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  -t cli-proxy-api .
```

`CPAMC_COMMIT` is required because the image cannot read `.git`; `PANEL_BUILT_AT` only
labels the generated manifest and defaults to the Unix epoch. Docker Compose passes
`CPAMC_COMMIT` the same way:

```bash
CPAMC_COMMIT="$(git -C web/management-center rev-parse HEAD)" docker compose build
```

## Update the pinned panel

Make and validate CPAMC changes inside `web/management-center`, commit and push them
to the Type-Delta CPAMC fork, and only then advance the CPA gitlink. Never commit a
gitlink that refers only to an unpushed local commit. Review the rebuilt bundle locally
with `scripts/build-management-center.sh`; only the gitlink commit is committed, the
bundle stays ignored.

## Runtime selection and rollback

CPA serves a mutable `management.html` only when the adjacent
`management-artifact.json` has a compatible Management API range and its SHA-256
matches. Otherwise CPA serves the panel embedded in the binary. The updater accepts the
same verified pair and has no undigested network fallback.

To roll back an installed mutable panel, disable panel auto-updates, install a
previously released `management.html` and its matching manifest together in the active
`WRITABLE_PATH/static` or config-relative `static` directory, verify the digest, and
restart CPA. Removing only those two named mutable files through the deployment platform
also returns service to the embedded panel; do not delete the static directory or
analytics data.

Mascot assets are immutable build output and have no mutable counterpart: a fallback
mutable panel resolves them from the binary's embedded bundle, and a name the bundle
does not contain simply falls back to the logo.

## Verification

```bash
scripts/build-management-center.sh
go build -o test-output ./cmd/server && unlink test-output
go test ./internal/managementasset/... ./internal/api/...
```
