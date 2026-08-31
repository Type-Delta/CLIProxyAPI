# CLIProxyAPI Fork

This fork keeps per-API-key request and token usage limits and an optional web-console Docker deployment that are not supplied by upstream.

## Divergence Log

This is a current-state record only. Each entry describes a surviving difference between `HEAD` and the current upstream base, `81e1b5374f99c212f196f34956eeed964a46b8fa`. The 2026-08-31 integration merged that upstream commit without rewriting the six published fork commits `2037ab99`, `04cfb113`, `5758371b`, `b67c5e31`, `45a589fb`, and `53866c01`. Their former shared base was `a14dfc779f43aed588e68b31fb34ab5ced700851`.

At the Gate 0 control-plane baseline, `git rev-list --left-right --count HEAD...upstream/main` reports `10 0`. The fork contains all commits through the recorded upstream base, the six original fork commits, the upstream merge, the hermetic media test, and two CPAMC submodule commits. `HEAD...origin/main` reports `0 0` at pushed commit `dae4267c70c835d323b00bfd9b2baaeb8386e92e`.

Keep stable IDs when updating this section; gaps are intentional. When upstream absorbs a difference, remove or rewrite the entry rather than preserving chronology here. Update its behavior, implementation evidence, and validation when the surviving difference changes.

### DL001 - Per-API-key request and token usage limits

Inbound client API keys may be configured as structured entries with optional request and token caps while bare-string keys remain compatible and unlimited. A key can use a lifetime window or a UTC hourly, daily, weekly, or monthly reset cadence; request and token limits share that window.

The proxy enforces limits across the OpenAI-, Anthropic-, and Gemini-compatible API surfaces, including the direct OpenAI Realtime routes. The `/v1`, `/openai/v1`, and `/backend-api/codex` groups use the OpenAI HTTP `429` envelope, while `/v1beta` uses the Gemini envelope; all include rate-limit headers. Model discovery, token counting, and asynchronous video status or content routes do not consume quota. Requests count when admitted, while tokens are recorded from normalized response usage after completion.

Usage counters persist under the authentication directory at `state/usage-limits.json`. Persisted entries identify keys only by SHA-256 hash, survive restarts, and remain local to each proxy instance. Configuration hot reload applies limit changes immediately, preserves counters unless the reset cadence changes, and removes counters for deleted or unlimited keys.

The management API exposes current limited-key consumption through `GET /v0/management/api-key-limits` and resets one key's counters through `POST /v0/management/api-key-limits/reset`. A reset for a configured key that has no recorded usage succeeds as a no-op, and only an unconfigured key returns `404`.

`PATCH /v0/management/api-keys` creates, rotates, and re-limits one key in a single call. It resolves the target from an in-range `index`, then from `old` or `match`, and otherwise appends a new entry, so a body that carries only `new` creates a key instead of failing. Its `limits` field distinguishes three states: omitted preserves the existing limits, an explicit `null` clears them, and an object replaces them as a whole. An object that resolves to no caps is stored as no limits, which keeps the key a bare string in `config.yaml`; a reset cadence without a cap counts as no caps, because a cadence alone enforces nothing and the usage tracker ignores it. An out-of-range index, a blank key on create, a `limits` value that is neither an object nor `null`, and an invalid cap or cadence return `400` with a descriptive message and leave the configuration unchanged. Key selection through `old` or `match` compares trimmed key strings.

The TUI API Keys tab accepts mixed bare and structured key representations and displays each limited key's request and token usage, reset cadence, and reset time, including keys that have configured limits but no recorded usage. Adding and editing a key use a four-field form covering the key, the request cap, the token cap in millions, and a reset-cadence selector; limits stay optional, and blank caps with a cadence of never clear the limits, while a cadence without a cap is rejected in the form. Rows carry their original position in the configured list, so editing or deleting a row targets that entry even when the list contains blank entries. The tab keeps keys masked and keeps the confirmed per-key usage reset action, which is now available for every key with configured limits.

**Implementation evidence:** `internal/config/{api_key_entry.go,config_load.go,parse.go,sdk_config.go}`, `internal/usagelimit/`, `internal/api/middleware/usage_limit.go`, `internal/api/usage_limit.go`, `internal/api/{server.go,server_routes.go,server_reload.go,server_management.go,server_middleware.go}`, `internal/api/handlers/management/{api_key_limits.go,config_lists.go,handler.go}`, `internal/access/config_access/provider.go`, `internal/tui/{client.go,keys_tab.go,i18n.go}`, `cmd/server/main.go`, `config.example.yaml`, and `docs/sdk-access.md`.

**Recorded validation:** the 2026-08-31 sync ran focused config, usage-limit tracker, middleware, management API, server API, TUI, and server-entrypoint Go tests; `go test ./...`; and the required disposable `cmd/server` compile check. Earlier validation also includes a live management-API run against a temporary config covering create, rotate, limit edit, limit clear, rejected limit values, out-of-range index, usage reset, and the resulting `config.yaml`.

**Last updated:** 2026-08-31

### DL002 - Two-service web-console Docker deployment

`docker-compose-web.yml` runs CLIProxyAPI behind an Nginx service on port `8317`. Nginx serves a current Cli-Proxy-API-Management-Center build at `/` and proxies every other path to CLIProxyAPI, including streaming and WebSocket traffic. The Management Center ref and public web port can be selected through `MANAGEMENT_CENTER_REF` and `CLI_PROXY_WEB_PORT`.

`Dockerfile.web` builds the Management Center from its GitHub repository with the Bun version declared by that project, then copies its single-file production output into an Nginx image. The deployment retains the existing CLIProxyAPI configuration, auth, log, plugin, and OAuth callback mounts and ports.

**Implementation evidence:** `docker-compose-web.yml`, `Dockerfile.web`, and `nginx-web.conf`.

**Recorded validation:** Docker Compose configuration rendering, both image builds, and a live two-container request check covering the Management Center at `/`, the proxied health endpoint at `/healthz`, and the authenticated Management API.

**Last updated:** 2026-08-27

### DL003 - Hermetic Codex Live media relay test

The Codex Live audio and data-channel bridge integration test creates every test peer on loopback. This keeps the test independent of host and container interface routing while leaving production ICE candidate selection unchanged.

**Implementation evidence:** `internal/client/codex/live/media_test.go`.

**Recorded validation:** `go test -count=3 -run TestPionMediaRelayBridgesAudioAndDataChannel ./internal/client/codex/live` passes on a runner where the unmodified test failed three consecutive times while every peer remained in the WebRTC `connecting` state.

**Last updated:** 2026-08-31

### DL004 - Pinned Type-Delta management client

CPA includes the Type-Delta CPAMC fork as a required submodule at `web/management-center`. The initial gitlink pinned `d249ff008e0bc2803deb23fb3e2c62418a1e8d17`; the synchronized gitlink pins pushed commit `1f77aaeb126c44e69ff51ccbcac6b2d5ebde9ee3`, which merges official CPAMC through `e0ee7123dfb5aa89a14ff73ac5a5c3bf4db658e0`.

The CPAMC checkout keeps Type-Delta as `origin` and the official repository as `upstream`. Its own `AGENTS.md` and `FORK.md` record the shared CPA, CPAUK, and CPAMC glossary, validation commands, current divergences, and append-only merge history.

**Implementation evidence:** `.gitmodules`, the `web/management-center` gitlink, and CPAMC commits `c1a2044` and `1f77aaeb`.

**Recorded validation:** exact Bun 1.3.14 verification passed 424 tests, ESLint, TypeScript compilation, and the Vite production build. After the push, `git rev-list --left-right --count origin/main...upstream/main` in CPAMC returned `2 0`, and the submodule worktree matched `origin/main` without changes.

**Last updated:** 2026-08-31

### DL005 - Embedded CPA Usage Keeper contracts and load gate

CPA carries the CPAUK analytics implementation behind the `internal/cpauk` package boundary. Its first frozen contract imports the observable behavior needed from upstream CPA Usage Keeper v1.15.0 at commit `696a4659ce1d5d6f2d2d0530e3205eb51fbce889` without importing its GORM, CGO SQLite, HTTP server, authentication, or process-lifecycle architecture. Attribution, source mapping, rejected designs, exact nano-USD arithmetic, privacy-safe key identity, encrypted pagination cursors, and fixture provenance are recorded beside the package.

The versioned fixtures define events, token categories, deterministic credential identities, key display identities, ranges, pricing, limit windows, viewer isolation, maintenance envelopes, imports, reconciliation, and stable leaderboard ordering. `test/perf` defines the analytics load-gate contract and median-of-five aggregation; the production adapter and dedicated-runner result are release-gate work and are not claimed by this contract commit.

**Implementation evidence:** `internal/cpauk/model`, `internal/cpauk/testdata/upstream-v1.15.0`, `internal/cpauk/LICENSE.upstream`, `internal/cpauk/UPSTREAM.md`, `internal/cpauk/provenance.json`, and `test/perf`.

**Recorded validation:** focused contract and fixture tests passed normally and under the race detector; focused load-contract and aggregation tests passed for five runs and under the race detector; `go vet` and the fixture provenance/hash checks passed. The full production-adapter load profile remains open until the runtime implementation is present.

**Last updated:** 2026-08-31

## Merge History

This is an append-only historical decision record. It provides context for integrations but never, by itself, establishes an ongoing fork divergence; use the current Divergence Log for that determination.

### 2026-08-31 - Merge upstream `main` at `81e1b537`

Merged upstream `main` into the fork without rebasing or rewriting the six published fork commits. The merge resolved upstream's server and configuration file splits by moving DL001 hooks into the new route, reload, management, middleware, and config-load files instead of restoring the pre-refactor monoliths. Direct OpenAI Realtime routes receive the limiter after authentication. DL002 files remain unchanged from `53866c01`.

The integration resolved content conflicts in `internal/api/server.go`, `internal/config/config.go`, and `internal/config/parse.go`. It kept upstream's refactored `server.go` and `config.go`, then reapplied the fork behavior in the split files listed under DL001.
