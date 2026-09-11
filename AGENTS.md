# AGENTS.md

Go 1.26+ proxy server providing OpenAI/Gemini/Claude/Codex compatible APIs with OAuth and round-robin load balancing.

## Glossary

- **CPA**: This CLIProxyAPI fork, including the Go proxy, its management API, and its release packaging.
- **CPAUK**: CPA Usage Keeper. In this repository, the name normally means the embedded analytics module ported from the upstream `cpa-usage-keeper` project. Use "upstream CPAUK" for the standalone source project.
- **CPAMC**: Cli-Proxy-API-Management-Center, the web management client shipped with CPA from the Type-Delta fork.

## Repository

- Fork: https://github.com/Type-Delta/CLIProxyAPI
- Upstream: https://github.com/router-for-me/CLIProxyAPI

## Syncing this fork with upstream repository

If you are tasked with syncing this fork with the upstream repository, please do it with the following considerations:
- Keep features from both sides
- If both fixes the same issue, prefer fixes from theirs.
- If some feature conflicts in a way that it is best to choose either theirs or ours, pause and ask me.
- If you are unsure about how to proceed, please ask me for guidance.
- This is not a "fix merge conflicts" task, you have to make sure that all features are behaving as expected and that those features make sense together both functionally, aesthetically and user experience. An app that works isn't necessarily a good app.
- After all the code related changes are done (merged, fixed, verified, finalized/cleanup), do the following:
  - Update "Divergence Log" and "Merge History" sections in FORK.md with the latest changes.
  - Commit merge changes in a single commit with a clear message describing the merge decisions made.
  - Move the "base" tag to the commit you just created. Force pushing this tag is fine since its only used for reference. (if `gdx` are available, you can use `gdx tag mv base ~0` to move "base" to the latest commit)

## Commands
```bash
gofmt -w . # Format (required after Go changes)
go build -o cli-proxy-api ./cmd/server # Build
go run ./cmd/server # Run dev server
go test ./... # Run all tests
go test -v -run TestName ./path/to/pkg # Run single test
go build -o test-output ./cmd/server && unlink test-output # Verify compile (REQUIRED after changes)
```
- Common flags: `--config <path>`, `--tui`, `--standalone`, `--local-model`, `--no-browser`, `--oauth-callback-port <port>`

## Remote-reachable dev server (BlackAmber)

Use this when the owner needs to try a change in a real browser. It runs CPA from source with the owner's `config.yaml` and publishes it on the tailnet through Tailscale Serve (`aila` is the Serve operator). Nothing here is reachable from the public internet.

```bash
# 1. Build and render the dev config (auth-dir points at the repo's ./auths; config.yaml is git-ignored, never commit it)
go build -o cli-proxy-api ./cmd/server
mkdir -p /tmp/cpa-dev
sed 's#auth-dir: .*#auth-dir: "/home/aila/Workspaces/Forks/CLIProxyAPI/auths"#' config.yaml > /tmp/cpa-dev/config.dev.yaml

# 2. Start CPA on :8317 (detached; logs in /tmp/cpa-dev/cpa.log)
setsid nohup ./cli-proxy-api -config /tmp/cpa-dev/config.dev.yaml > /tmp/cpa-dev/cpa.log 2>&1 < /dev/null &

# 3. Publish on the tailnet: https://blackamber.tailc5ef75.ts.net/ -> CPA
tailscale serve --bg --https=443 http://127.0.0.1:8317
tailscale serve status

# 4. Verify (401 means up and unauthenticated)
curl -s -o /dev/null -w '%{http_code}\n' https://blackamber.tailc5ef75.ts.net/v0/management/capabilities

# Stop: kill CPA and remove the Serve mapping
pkill -x cli-proxy-api; tailscale serve --https=443 off
```

- UI: `https://blackamber.tailc5ef75.ts.net/management.html`. The owner logs in with just the management password; the panel uses the page origin as API base.
- CPA serves the bundled CPAMC panel from `internal/managementasset/bundled/`. To test CPAMC changes, rebuild it (`bun run build` in `web/management-center`, then copy `dist/index.html` to `<static dir>/management.html` with a matching `management-artifact.json`, or run `scripts/build-management-center.sh` for the canonical artifact) and restart CPA. Set `MANAGEMENT_STATIC_PATH=<dir>` to serve an artifact from outside the repo.
- Do not point the tailnet at the Vite dev server (`bun run dev`). `vite.config.ts` has no `/v0` proxy, so every management call from that origin fails and credentials appear missing. If Vite is needed for hot reload, the browser must be given CPA's URL as a custom connection URL on the login page, and CPA must be published on a second port (`tailscale serve --bg --https=8443 http://127.0.0.1:8317`).
- `/tmp` is tmpfs and is wiped on reboot; keep nothing there that you cannot regenerate from the commands above.
- Secrets such as the Z.ai key live in `pass` (`services/cli-proxy-api/zai-api-key`); render them into `config.yaml` at setup time, never into tracked files.

## Config
- Default config: `config.yaml` (template: `config.example.yaml`)
- `.env` is auto-loaded from the working directory
- Auth material defaults under `auths/`
- Storage backends: file-based default; optional Postgres/git/object store (`PGSTORE_*`, `GITSTORE_*`, `OBJECTSTORE_*`)

## Architecture
- `cmd/server/` — Server entrypoint
- `internal/api/` — Gin HTTP API (routes, middleware, modules)
- `internal/api/modules/amp/` — Amp integration (Amp-style routes + reverse proxy)
- `internal/thinking/` — Main thinking/reasoning pipeline. `ApplyThinking()` (apply.go) parses suffixes (`suffix.go`, suffix overrides body), normalizes config to canonical `ThinkingConfig` (`types.go`), normalizes and validates centrally (`validate.go`/`convert.go`), then applies provider-specific output via `ProviderApplier`. Do not break this "canonical representation → per-provider translation" architecture.
- `internal/runtime/executor/` — Per-provider runtime executors (incl. Codex WebSocket)
- `internal/translator/` — Provider protocol translators (and shared `common`)
- `internal/registry/` — Model registry + remote updater (`StartModelsUpdater`); `--local-model` disables remote updates
- `internal/store/` — Storage implementations and secret resolution
- `internal/managementasset/` — Config snapshots and management assets
- `internal/cache/` — Request signature caching
- `internal/watcher/` — Config hot-reload and watchers
- `internal/wsrelay/` — WebSocket relay sessions
- `internal/usage/` — Usage and token accounting
- `internal/tui/` — Bubbletea terminal UI (`--tui`, `--standalone`)
- `sdk/cliproxy/` — Embeddable SDK entry (service/builder/watchers/pipeline)
- `test/` — Cross-module integration tests

## Code Conventions
- Keep changes small and simple (KISS)
- Comments in English only
- If editing code that already contains non-English comments, translate them to English (don’t add new non-English comments)
- For user-visible strings, keep the existing language used in that file/area
- New Markdown docs should be in English unless the file is explicitly language-specific (e.g. `README_CN.md`)
- As a rule, do not make standalone changes to `internal/translator/`. You may modify it only as part of broader changes elsewhere.
- If a task requires changing only `internal/translator/`, run `gh repo view --json viewerPermission -q .viewerPermission` to confirm you have `WRITE`, `MAINTAIN`, or `ADMIN`. If you do, you may proceed; otherwise, file a GitHub issue including the goal, rationale, and the intended implementation code, then stop further work.
- `internal/runtime/executor/` should contain executors and their unit tests only. Place any helper/supporting files under `internal/runtime/executor/helps/`.
- Follow `gofmt`; keep imports goimports-style; wrap errors with context where helpful
- Do not use `log.Fatal`/`log.Fatalf` (terminates the process); prefer returning errors and logging via logrus
- Shadowed variables: use method suffix (`errStart := server.Start()`)
- Wrap defer errors: `defer func() { if err := f.Close(); err != nil { log.Errorf(...) } }()`
- Use logrus structured logging; avoid leaking secrets/tokens in logs
- Avoid panics in HTTP handlers; prefer logged errors and meaningful HTTP status codes
- Timeouts are allowed only during credential acquisition; after an upstream connection is established, do not set timeouts for any subsequent network behavior. Intentional exceptions that must remain allowed are the Codex websocket liveness deadlines in `internal/runtime/executor/codex_websockets_executor.go`, the wsrelay session deadlines in `internal/wsrelay/session.go`, the management APICall timeout in `internal/api/handlers/management/api_tools.go`, and the `cmd/fetch_antigravity_models` utility timeouts
- This is the fork of original CLIProxyAPI; Changes diverge from the original should be documented in FORK.md. After making code changes, please update FORK.md with your changes.
- Avoid wall-clock `time.Sleep` in TTL, expiration, ordering, or cache-eviction unit tests due to platform timer granularity (e.g. Windows default timer resolution of ~15.6ms) and CI jitter under load; prefer controllable clocks (`nowFunc` / mock clock), explicit timestamp manipulation, or deterministic synchronization primitives.
