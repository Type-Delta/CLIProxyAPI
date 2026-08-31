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

Key management now reports a deterministic configuration revision and each row's full SHA-256 key ID. Revisioned clients mutate by configuration index plus the expected revision, so duplicate raw keys remain independently editable and stale writes return `409`. Full-YAML writes from older panels that would flatten structured limits or unknown key fields are rejected rather than silently losing them. Authorized administrators can still retrieve raw keys, but management clients are expected to conceal them until an explicit per-row reveal; analytics and logs use only key IDs. Weak-key warnings identify short or predictable configured secrets without logging their values.

The TUI API Keys tab accepts mixed bare and structured key representations and displays each limited key's request and token usage, reset cadence, and reset time, including keys that have configured limits but no recorded usage. Adding and editing a key use a four-field form covering the key, the request cap, the token cap in millions, and a reset-cadence selector; limits stay optional, and blank caps with a cadence of never clear the limits, while a cadence without a cap is rejected in the form. Rows carry their original position in the configured list, so editing or deleting a row targets that entry even when the list contains blank entries. The tab keeps keys masked and keeps the confirmed per-key usage reset action, which is now available for every key with configured limits.

**Implementation evidence:** `internal/config/{api_key_entry.go,config_load.go,parse.go,sdk_config.go}`, `internal/usagelimit/`, `internal/api/middleware/usage_limit.go`, `internal/api/usage_limit.go`, `internal/api/{server.go,server_routes.go,server_reload.go,server_management.go,server_middleware.go}`, `internal/api/handlers/management/{api_key_limits.go,config_lists.go,handler.go}`, `internal/access/config_access/provider.go`, `internal/tui/{client.go,keys_tab.go,i18n.go}`, `cmd/server/main.go`, `config.example.yaml`, and `docs/sdk-access.md`.

**Recorded validation:** focused config, usage-limit tracker, middleware, management API, server API, TUI, revision-conflict, duplicate-key, compatibility-guard, weak-key, and server-entrypoint tests pass, along with `go test ./...`, the focused race suite, and the required disposable `cmd/server` compile check. Earlier validation also includes a live management-API run against a temporary config covering create, rotate, limit edit, limit clear, rejected limit values, out-of-range index, usage reset, and the resulting `config.yaml`.

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

### DL005 - Failure-isolated embedded CPA Usage Keeper

CPA carries optional CPAUK analytics behind the `internal/cpauk` package boundary. The port adapts observable behavior from upstream CPA Usage Keeper v1.15.0 at commit `696a4659ce1d5d6f2d2d0530e3205eb51fbce889` without importing its GORM, CGO SQLite, HTTP server, authentication, or process-lifecycle architecture. Analytics are disabled by default and have independent startup, health, circuit, maintenance, and shutdown states. Bounded, lossy observer delivery runs only after synchronous per-key accounting; storage errors, panics, queue saturation, and invalid analytics configuration cannot change proxy readiness or request success.

The module sanitizes usage records before enqueue, persists full SHA-256 key identities in a separate pure-Go SQLite database, and retains a separate random identity key for credential pseudonyms and encrypted cursors. Checksummed migrations, quota reserves, verified backup and restore, corruption guards, hourly and daily rollups, monotonic retention checkpoints, resumable import and rollback, confirmed purge batches, repair jobs, and identity-epoch recovery are isolated from CPA's auth and limit state. Event detail is indexed; event APIs reject retained-away ranges rather than returning partial history. Summary, time series, dimensions, event pages, pricing, provider quotas, key catalog, and token- or known-cost leaderboards share bounded multi-key query contracts and explicitly report unpriced tokens.

Versioned fixtures record events, token categories, deterministic credential identities, key display identities, ranges, pricing, limit windows, viewer isolation, maintenance envelopes, imports, reconciliation, and stable pagination. `test/perf` includes a production adapter covering HTTP, streaming, WebSocket, retries, blocked SQLite, and saturated queues. Its long median-of-five certification remains opt-in because it requires the documented dedicated-runner metadata and power controls.

**Implementation evidence:** `internal/cpauk`, `internal/cpauk/testdata/upstream-v1.15.0`, `sdk/cliproxy/usage/manager.go`, and `test/perf`.

**Recorded validation:** all CPAUK, usage-delivery, API integration, deterministic load-adapter, retention/reopen, migration, backup/restore, import/rollback, and failure-containment tests pass. The required focused race suite and ordinary full repository suite pass. The dedicated approximately 110-minute load certification remains an environment-specific release result and is not claimed by this checkout.

**Last updated:** 2026-08-31

### DL006 - Reproducible embedded CPAMC artifact

CPA builds CPAMC from the pinned `web/management-center` checkout with Bun 1.3.14 and embeds the resulting single-file application in the Go binary. The main and Nginx images verify their panel build byte-for-byte against that canonical artifact; release archives carry the same HTML, compatibility manifest, and CPAUK/CPAMC license notices. No product build clones or floats on a remote CPAMC branch.

Mutable and downloaded panels require an adjacent `management-artifact.json` whose Management API range accepts this CPA build and whose SHA-256 matches the exact HTML bytes. An invalid, missing, incompatible, or tampered mutable panel falls back to the immutable bundled artifact. The former undigested network fallback is removed, and updates default to the Type-Delta CPAMC fork.

**Implementation evidence:** `internal/managementasset/bundle.go`, `internal/managementasset/bundled`, `scripts/build-management-center.sh`, `scripts/verify-submodules.sh`, both Dockerfiles, `nginx-web.conf`, release workflows, and `docs/management-center.md`.

**Recorded validation:** the exact Bun 1.3.14 Docker build reproduced SHA-256 `bcd83e9c326e948e74d459c3a64257d5328a969df19f76666e219ff73a125433`; the Nginx image served those identical bytes from `/` and `/management.html`, with `no-store` and `no-referrer` headers on the management route. Mutable artifact validation and bundled fallback passed focused Go tests.

**Last updated:** 2026-08-31

### DL007 - Analytics management and shared-view control plane

CPA exposes capability-gated analytics management routes for health, summary, time series, dimensions, events, indexed event detail, pricing, provider quotas, key catalog, stable leaderboards, bounded CSV exports, backup, restore, import, rollback, purge, repair, maintenance jobs, and shared viewers. Key identity filters are accepted only in bounded JSON bodies; every analytics route rejects key IDs in URLs, and request logging masks accidental `key_id` and `key_ids` query parameters before recording them. Expensive admin reads and maintenance starts have independent bounded peer rate limits.

Shared viewers are stored in a separate bounded, atomic JSON document containing only credential and session hashes plus fixed key scope. The file and its parent-directory rename are synchronized before success. An administrator receives a viewer credential once, can list durable non-secret viewer metadata after a restart, and can selectively revoke the viewer and all its sessions. Viewer exchange requires direct HTTPS or verified forwarded HTTPS from a configured immediate proxy, issues a short-lived `HttpOnly`, `Secure`, `SameSite=Strict` cookie, and removes credentials from request logging. Viewer routes have a fixed CORS allowlist, cannot accept a management credential, and cannot select another key ID. Configuration reload invalidates active sessions and reports restart-required trust changes without taking proxying down.

**Implementation evidence:** `internal/api/analytics_*`, `internal/api/handlers/management/analytics*`, `internal/api/server*.go`, `internal/api/middleware/request_logging.go`, `internal/config/analytics.go`, `internal/util/provider.go`, and `config.example.yaml`.

**Recorded validation:** management/viewer authorization matrices, CORS, TLS and trusted-proxy cases, normal and error-only logging, low-entropy key fixtures, reload failure and recovery, viewer persistence and revocation, URL identity rejection, cursor/error mapping, indexed retained-event handling, CSV injection, throttling, and maintenance API tests pass normally and in the focused race suite.

**Last updated:** 2026-08-31

## Merge History

This is an append-only historical decision record. It provides context for integrations but never, by itself, establishes an ongoing fork divergence; use the current Divergence Log for that determination.

### 2026-08-31 - Merge upstream `main` at `81e1b537`

Merged upstream `main` into the fork without rebasing or rewriting the six published fork commits. The merge resolved upstream's server and configuration file splits by moving DL001 hooks into the new route, reload, management, middleware, and config-load files instead of restoring the pre-refactor monoliths. Direct OpenAI Realtime routes receive the limiter after authentication. DL002 files remain unchanged from `53866c01`.

The integration resolved content conflicts in `internal/api/server.go`, `internal/config/config.go`, and `internal/config/parse.go`. It kept upstream's refactored `server.go` and `config.go`, then reapplied the fork behavior in the split files listed under DL001.
