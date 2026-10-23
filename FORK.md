# CLIProxyAPI Fork

This fork keeps per-API-key request and token usage limits, failure-isolated CPAUK analytics, a pinned CPAMC client, and reproducible web-console packaging that are not supplied by upstream.

## Divergence Log

This is a current-state record only. Each entry describes a surviving difference from upstream. Do not record merge or sync details, branch history, current commit pins, or other volatile integration references here; keep those in **Merge History** or the relevant build metadata.

Update this section only when a new fork feature or behavior is introduced, or when an existing divergence changes. Keep IDs stable and unique: gaps and out-of-order IDs are intentional when entries are removed. If duplicate IDs are found, remove the duplicate when the content is the same; when the content is different, assign the distinct entry the next unused ID. Remove or rewrite an entry when upstream absorbs its difference rather than preserving chronology.

### DL001 - Per-API-key request and token usage limits

Inbound client API keys may be configured as structured entries with optional request and token caps while bare-string keys remain compatible and unlimited. A key can use a lifetime window or a UTC hourly, daily, weekly, or monthly reset cadence; request and token limits share that window.

The proxy enforces limits across the OpenAI-, Anthropic-, and Gemini-compatible API surfaces, including the direct OpenAI Realtime routes. The `/v1`, `/openai/v1`, and `/backend-api/codex` groups use the OpenAI HTTP `429` envelope, while `/v1beta` uses the Gemini envelope; all include rate-limit headers. Model discovery, token counting, and asynchronous video status or content routes do not consume quota. Requests count when admitted, while tokens are recorded from normalized response usage after completion.

Usage counters persist under the authentication directory at `state/usage-limits.json`. Persisted entries identify keys only by SHA-256 hash, survive restarts, and remain local to each proxy instance. Configuration hot reload applies limit changes immediately, preserves counters unless the reset cadence changes, and removes counters for deleted or unlimited keys.

The authenticated management API exposes current limited-key consumption through the canonical v8 routes `GET /v8/management/api-key-limits` and `POST /v8/management/api-key-limits/reset`; the existing `/v0/management/api-key-limits` routes remain compatible for older clients. A reset for a configured key that has no recorded usage succeeds as a no-op, and only an unconfigured key returns `404`.

`PATCH /v8/management/api-keys` creates, rotates, and re-limits one key in a single call using the configuration revision; `PUT` and `DELETE` use the same revisioned contract. The existing `/v0/management/api-keys` payloads remain available for compatibility. The v8 endpoint resolves the target from an in-range `index`, then from `old` or `match`, and otherwise appends a new entry, so a body that carries only `new` creates a key instead of failing. Its `limits` field distinguishes three states: omitted preserves the existing limits, an explicit `null` clears them, and an object replaces them as a whole. An object that resolves to no caps is stored as no limits, which keeps the key a bare string in `config.yaml`; a reset cadence without a cap counts as no caps, because a cadence alone enforces nothing and the usage tracker ignores it. An out-of-range index, a blank key on create, a `limits` value that is neither an object nor `null`, and an invalid cap or cadence return `400` with a descriptive message and leave the configuration unchanged. Key selection through `old` or `match` compares trimmed key strings. The v8 configuration document also returns an `ETag`, and writes accept an optional `If-Match` against the on-disk hash, adding stale-write protection without replacing the fork's revisioned key contract.

Key management now reports a deterministic configuration revision and each row's full SHA-256 key ID. Revisioned clients mutate by configuration index plus the expected revision, so duplicate raw keys remain independently editable and stale writes return `409`. Full-YAML writes from older panels that would flatten structured limits or unknown key fields are rejected rather than silently losing them. Authorized administrators can still retrieve raw keys, but management clients are expected to conceal them until an explicit per-row reveal; analytics and logs use only key IDs. Weak-key warnings identify short or predictable configured secrets by config index, label, and short key ID without logging their values.

The TUI API Keys tab accepts mixed bare and structured key representations and displays each limited key's request and token usage, reset cadence, and reset time, including keys that have configured limits but no recorded usage. Adding and editing a key use a four-field form covering the key, the request cap, the token cap in millions, and a reset-cadence selector; limits stay optional, and blank caps with a cadence of never clear the limits, while a cadence without a cap is rejected in the form. Rows carry their original position in the configured list, so editing or deleting a row targets that entry even when the list contains blank entries. The tab keeps keys masked and keeps the confirmed per-key usage reset action, which is now available for every key with configured limits.

**Implementation evidence:** `internal/config/{api_key_entry.go,config_load.go,config_v8.go,parse.go,sdk_config.go}`, `internal/usagelimit/`, `internal/api/middleware/usage_limit.go`, `internal/api/usage_limit.go`, `internal/api/{server.go,server_routes.go,server_reload.go,server_management.go,server_middleware.go}`, `internal/api/handlers/management/{api_key_limits.go,config_lists.go,config_v8.go,handler.go}`, `internal/access/config_access/provider.go`, `internal/tui/{client.go,keys_tab.go,i18n.go}`, `cmd/server/main.go`, `config.example.yaml`, and `docs/sdk-access.md`.

**Recorded validation:** focused config, usage-limit tracker, middleware, management API, server API, TUI, revision-conflict, duplicate-key, compatibility-guard, weak-key, and server-entrypoint tests pass, along with `go test ./...`, the focused race suite, and the required disposable `cmd/server` compile check. Earlier validation also includes a live management-API run against a temporary config covering create, rotate, limit edit, limit clear, rejected limit values, out-of-range index, usage reset, and the resulting `config.yaml`.

**Last updated:** 2026-10-19

### DL002 - Two-service web-console Docker deployment

`docker-compose-web.yml` runs CLIProxyAPI behind an Nginx service on port `8317`. Nginx serves the pinned Cli-Proxy-API-Management-Center artifact at `/` and proxies every other path to CLIProxyAPI, including streaming and WebSocket traffic. The public web port can be selected through `CLI_PROXY_WEB_PORT`; `CPA_WEB_NETWORK_SUBNET` selects the narrow bridge CIDR when the default overlaps another Docker network.

`Dockerfile.web` copies CPA's canonical Management Center artifact and manifest into a digest-pinned Nginx image. It does not rebuild the panel from an arbitrary submodule working tree or compare generated artifacts byte-for-byte. The canonical artifact is produced separately from a clean pinned CPAMC checkout, so the web image cannot drift when the source tree contains stale or partially applied panel changes. The deployment retains the existing CLIProxyAPI configuration, auth, log, plugin, and OAuth callback mounts and ports. An optional forwarding override trusts `X-Forwarded-Proto` only from an explicitly configured immediate TLS-proxy CIDR; the default configuration overwrites spoofed forwarding headers.

**Implementation evidence:** `docker-compose-web.yml`, `docker-compose-web-forwarded.yml`, `Dockerfile.web`, `nginx-web.conf`, `nginx-web-forwarded.conf.template`, and `docs/analytics-operations.md`.

**Recorded validation:** both Compose configurations render; both digest-pinned images build from the initialized submodule; and a live two-container run serves artifact SHA-256 `e6dcb11e8d76681d6ae678c5b48fcddf4e6fb0272fffbd441c3492d8945e1be6`, proxies health and authenticated analytics, preserves a durable viewer across restart, rejects spoofed forwarded HTTPS by default, and accepts verified forwarding only through the explicit override. A configurable bridge subnet was exercised after the default correctly reported a host-network overlap.

**Last updated:** 2026-08-31

### DL003 - Hermetic Codex Live media relay test

The Codex Live audio and data-channel bridge integration test creates every test peer on loopback. This keeps the test independent of host and container interface routing while leaving production ICE candidate selection unchanged.

**Implementation evidence:** `internal/client/codex/live/media_test.go`.

**Recorded validation:** `go test -count=3 -run TestPionMediaRelayBridgesAudioAndDataChannel ./internal/client/codex/live` passes on a runner where the unmodified test failed three consecutive times while every peer remained in the WebRTC `connecting` state.

**Last updated:** 2026-08-31

### DL004 - Pinned Type-Delta management client

CPA includes the Type-Delta CPAMC fork as a required submodule at `web/management-center`. The fork adds structured key management, the isolated Analytics workspace, complete visual analytics configuration, full `int64` storage precision, safe CRLF YAML normalization, Config-card spacing, routed icon tabs, URL-backed range and key filters, collision-safe key identities, consistent controls and Skeleton loading states, a persistent Analytics shell with in-page portal content, CPAUK-fidelity usage and management views, and the Claude OAuth capture safeguard control. The checked-out CPAMC pin `e2cb1d6` carries the upstream sync through `0da007e` (`merge(upstream): sync CPAMC through v1.26.5`) plus the restored legacy-backend login diagnostic.

The CPAMC checkout keeps Type-Delta as `origin` and the official repository as `upstream`. Its own `AGENTS.md` and `FORK.md` record the shared CPA, CPAUK, and CPAMC glossary, validation commands, current divergences, and append-only merge history.

**Implementation evidence:** `.gitmodules`, the `web/management-center` gitlink, and the CPAMC `AGENTS.md` and `FORK.md`.

**Recorded validation:** CPAMC verification, lint, TypeScript compilation, the Vite production build, and independent desktop/mobile browser journeys cover the Analytics routes, configuration contracts, viewer isolation, responsive layout, and runtime diagnostics. Detailed CPAMC validation remains in the submodule's `FORK.md`.

The analytics upgrade adds the animated diamond cost radar, shared p95/max/median timing radar and
scatter controls, token/cost/generation heatmap modes, cost component columns, and line-shaped
chart tooltip markers. Quick Stats includes accumulated processing time and explained comparisons.
The key catalog uses client-local lifetime activity dates, recent Active/Idle status, top-model tokens,
generation duration, requests, and header help. Visible analytics refresh every minute without
replacing charts or selections. Vite accepts all development hosts for the requested remote mock
workspace. Provider timings and historical generation remain unavailable where measurements are
absent; CPAMC does not estimate clock offsets from unrelated online UTC services.

Browser verification covers light/dark cost gradients, radar and heatmap modes, missing timing,
numeric alignment, comparison interactions, client-timezone dates, automatic refresh, and the
absence of page overflow or console errors.

The refinement shares Auth Files' segmented control with analytics and moves sortable headers into
the common Table component. Custom tooltip panels replace native title hints across CPAMC and
preserve dynamic content, accessible names, focus, and touch behavior. Key catalog header styling
is corrected. Quick Stats uses full-width trends and places Processing time before the two-column
Daily average card. Cost summaries sit beside the radar when space permits; paired cards share
height. All model-cost columns sort, latency metrics explain their measurements, radar axis delays
are more visible, and refresh spinners no longer dim existing content. Per-comparison assumptions
replace the standalone comparison section. The user's existing spacing changes are included.

Additional browser verification covers intermediate layouts, dynamic and touch tooltips, equal card
heights, model-cost sorting, latency animation, and the Claude OAuth safeguard control.

**Last updated:** 2026-10-19

### DL005 - Failure-isolated embedded CPA Usage Keeper

CPA carries optional CPAUK analytics behind the `internal/cpauk` package boundary. The port adapts observable behavior from upstream CPA Usage Keeper v1.15.0 at commit `696a4659ce1d5d6f2d2d0530e3205eb51fbce889` without importing its GORM, CGO SQLite, HTTP server, authentication, or process-lifecycle architecture. Analytics are disabled by default and have independent startup, health, circuit, maintenance, and shutdown states. Bounded, lossy observer delivery runs only after synchronous per-key accounting; storage errors, panics, queue saturation, and invalid analytics configuration cannot change proxy readiness or request success. The SDK usage manager keeps trusted accounting separate from generic observer delivery, with independent bounded lanes, taps, drain behavior, and access-provider/native-key metadata in each record.

The module sanitizes usage records before enqueue, persists full SHA-256 key identities in a separate pure-Go SQLite database, and retains a separate random identity key for credential pseudonyms and encrypted cursors. Checksummed migrations, quota reserves, verified backup and restore, corruption guards, hourly and daily rollups, monotonic retention checkpoints, resumable import and rollback, resumable raw-history repricing with an explicit retained-history cutoff, confirmed purge batches, repair jobs, and identity-epoch recovery are isolated from CPA's auth and limit state. Event detail is indexed; event APIs reject retained-away ranges rather than returning partial history. API schema v2 adds outcome and rate summaries, zero-filled activity and analysis series, full-range latency statistics with capped distributed scatter samples, once-per-event cost-component reconciliation, named range resolution, complete sanitized CSV/JSON exports, missing-price provenance, and credential-level provider/quota rows with request identity deduplication while retaining schema-v1 requests and fields. Current-period named ranges end at request time instead of including future buckets. Summary, time series, dimensions, event pages, pricing, provider quotas, key catalog, and token- or known-cost leaderboards share bounded multi-key query contracts and explicitly report unpriced tokens.

Pricing discovery uses models.dev with a durable six-hour cache. Usage activity and pricing-page reads request refreshes on demand; startup and idle time do not schedule catalog downloads. Concurrent demand shares a refresh, and failures retain the last successful catalog with a 15-minute retry delay. Manual model and alias overrides are stored separately and take precedence over discovered prices. Catalog matching uses explicit provider mappings and exact model IDs. CPAMC presents these as API-equivalent cost estimates, including for subscription-backed OAuth accounts; baseline token rates do not account for every context tier, service tier, or non-token charge. Historical events retain their stored cost until an explicit repricing job runs. Tests cover TTL expiry, concurrent refreshes, persistence, stale fallback, decimal parsing, and prices applied to newly ingested events. Live models.dev and browser checks cover catalog loading and override creation/removal.

The CPAUK intake adapter now persists the canonical input total from the SDK token breakdown. This keeps Anthropic and Claude cache-read buckets independent from uncached input when calculating cache-read rates and pricing, while preserving OpenAI-style input totals where cached tokens are already included. Existing event and rollup rows retain their stored token buckets; the current repricing operation updates price fields only, so repairing historical token totals requires an explicit migration with provider-scoped review.

CPAUK also persists an optional observed generation duration: the local monotonic interval between first and last substantive streaming token arrivals. Chat error envelopes, `[DONE]` markers, Claude signature-only deltas, and other metadata or terminal-only frames do not extend it. Native HTTP streams now capture generation timestamps in a bounded read-ahead observer before downstream forwarding; Codex WebSocket frames carry their socket-read timestamps through a byte-budgeted queue. Queue saturation invalidates generation timing instead of reporting a compressed interval, preserving a first-token observation already captured before saturation. Historical records and responses without observed token boundaries remain unavailable. This measures arrival timing at CPA, not exact provider-side execution. Analytics expose full-population p95, maximum, median and accumulated duration coverage, including ranges beyond 30 days; retained-away timing observations are reported as partial. E2E names the existing executor duration from attempt setup through completion; the local TTFT and token-arrival boundaries differ from client-observed wall time. This follows [OpenRouter’s total-latency decomposition](https://openrouter.ai/docs/guides/best-practices/latency-and-performance.md) into time to first token (network, queue and prefill) plus generation, without claiming identical measurement boundaries. CPAUK also stores locally observed dispatch-to-first-substantive-token and dispatch-to-response durations as nullable `first_token_latency_ms` and `provider_latency_ms`. HTTP headers and request-specific Codex/xAI WebSocket response frames establish the response endpoint. These measurements include network and provider wait time; they do not claim provider-reported acceptance timing. Native OpenAI-compatible, Claude, Gemini/Vertex, Antigravity, xAI, and Kimi streaming loops invoke their protocol-specific substantive-token observers alongside the existing Codex and plugin paths. Buffered nonstream parsing does not fabricate token arrival times. Historical and unsupported observations remain null. Analysis and processing-time totals expose measured values, coverage, and percentiles; retries restart the local timer. Existing TTFT retains its first-packet fallback, while first-token latency requires substantive token evidence. Provider `created_at` timestamps are never subtracted from CPA UTC: an unrelated online clock cannot calibrate a provider clock or remove timestamp quantization. Additive model cost components preserve reconciled stored totals, and key catalogs expose distinct requests, selected-range top models and observed generation totals alongside existing lifetime activity.

Versioned fixtures record events, token categories, deterministic credential identities, key display identities, ranges, pricing, limit windows, viewer isolation, maintenance envelopes, imports, reconciliation, and stable pagination. `test/perf` includes a production adapter covering HTTP, streaming, WebSocket, retries, blocked SQLite, and saturated queues. Its long median-of-five certification remains opt-in because it requires the documented dedicated-runner metadata and power controls.

A 2026-09-08 metric audit produced eleven corrections. The intake adapter now persists the canonical inclusive output total (Gemini candidate plus thought tokens) alongside the input total, so reasoning is priced. `TokenUsage.UnclassifiedTokens()` derives the remainder of the authoritative total that no bucket explains; a matched price rule reports that remainder as unpriced instead of claiming full coverage, and repricing inherits the rule. Analysis model rows, matrix cells, and activity buckets expose `unpriced_tokens`, and CPAMC uses it to qualify model efficiency as unknown or partial rather than ranking it as $0/M; token charts and the distribution add an explicit unclassified category so stacks sum to the total, and event speed uses the recorded generation interval, labels the TTFT fallback as an estimate, and shows unavailable when neither exists. Migration 005 adds `known_cost_nano` and `unpriced_tokens` to `daily_stats` so year activity keeps cost after retention; year activity deduplicates request identities across the raw/retained boundary; the public `success` filter maps to the `succeeded` column; retained credentials keep their auth type and actual last-observed timestamp instead of a bucket end; upstream v1.15 imports no longer force `exact` quality, so all-zero usage is inferred as `missing`; and the collector counts batches discarded at shutdown as abandoned so accepted equals written plus dropped plus abandoned. Existing stored event and rollup token buckets are unchanged; historical repair still requires the explicit provider-scoped migration noted above.

**Implementation evidence:** `internal/cpauk`, `internal/cpauk/collector/adapter.go`, `internal/cpauk/collector/sanitizer_test.go`, `internal/cpauk/testdata/upstream-v1.15.0`, `sdk/cliproxy/usage/{manager.go,delivery.go}`, and `test/perf`.

**Recorded validation:** the canonical Anthropic adapter regression and OpenAI compatibility tests pass, along with the existing CPAUK, schema-v2 contract, usage-delivery, API integration, deterministic load-adapter, retention/reopen, migration, backup/restore, import/rollback, repricing, DST and fractional-zone bucketing, latency, cost reconciliation, provider-quota, and failure-containment tests. The seeded UTC fixture resolves to 5,250 requests, 68,083,696 tokens, $295.62446097 known cost, 5,070 successes, and 180 failures. The required disposable server compile and ordinary full repository suite pass. The dedicated approximately 110-minute load certification remains an environment-specific release result and is not claimed by this checkout.

**Last updated:** 2026-10-19

### DL006 - Reproducible embedded CPAMC artifact

CPA builds CPAMC from the pinned `web/management-center` checkout with a digest-pinned Bun 1.3.14 container and embeds the resulting single-file application in the Go binary. The separate canonical builder removes host-dependent Rolldown output, keys the UI version to the pinned CPAMC commit, and uses a persistent package cache for offline rebuilds. Runtime images and release archives consume that canonical HTML and compatibility manifest directly; no product build compares generated artifacts byte-for-byte or compiles an arbitrary panel working tree. No product build clones or floats on a remote CPAMC branch.

Mutable and downloaded panels require an adjacent `management-artifact.json` whose Management API range accepts this CPA build and whose SHA-256 matches the exact HTML bytes. An invalid, missing, incompatible, or tampered mutable panel falls back to the immutable bundled artifact. The former undigested network fallback is removed, and updates default to the Type-Delta CPAMC fork.

**Implementation evidence:** `internal/managementasset/bundle.go`, `internal/managementasset/bundled`, `scripts/build-management-center.sh`, `scripts/verify-submodules.sh`, `Dockerfile.management`, both runtime Dockerfiles, `nginx-web.conf`, release workflows, `DEPENDENCY_NOTICES.md`, and `docs/management-center.md`.

**Recorded validation:** the canonical builder rebuilt the checked-in bundle from CPAMC `e2cb1d6cf7fdd996010d5cebe279e971a9ff222c`; the manifest records HTML SHA-256 `bdaad2c97c31701993456fd3ca34c8024da5fff2c5174110ddd86cc959bd24a9`. Earlier main-image, cached `--network=none`, Nginx image, mutable artifact, bundled fallback, workflow, and cross-build checks passed for the preceding implementation pin. CI remains authoritative for native CGO and plugin builds.

**Last updated:** 2026-10-19

### DL007 - Analytics management and shared-view control plane

CPA exposes capability-gated analytics management routes under the canonical authenticated `/v8/management/analytics/*` path (with the established v0 compatibility group retained) for health, summary, time series, activity, independently partial analysis, dimensions, events, indexed event detail, pricing, resumable repricing, credential-level provider quotas, key catalog, stable leaderboards, bounded complete CSV/JSON exports, backup, restore, import, rollback, purge, repair, maintenance jobs, and shared viewers. Named and explicit ranges resolve to echoed UTC bounds and an IANA zone. Key identity filters are accepted only in bounded JSON bodies; every analytics route rejects key IDs in URLs, and request logging masks accidental `key_id` and `key_ids` query parameters before recording them. Expensive admin reads and maintenance starts have independent bounded peer rate limits. The public analytics viewer remains at `/v0/analytics/viewer/*` and is not moved into the management group. Configuration saves and reloads use cloned runtime configuration and stale-snapshot protection, while the merged server publishes ETag, request-body, and Claude tool-search behavior without bypassing CPAUK lifecycle invalidation or viewer security.

Shared viewers are stored in a separate bounded, atomic JSON document containing only credential and session hashes plus fixed key scope. The file and its parent-directory rename are synchronized before success. An administrator receives a viewer credential once, can list durable non-secret viewer metadata after a restart, and can selectively revoke the viewer and all its sessions. Viewer exchange requires direct HTTPS or verified forwarded HTTPS from a configured immediate proxy, issues a short-lived `HttpOnly`, `Secure`, `SameSite=Strict` cookie, and removes credentials from request logging. Viewer routes have a fixed CORS allowlist, cannot accept a management credential, and cannot select another key ID. Configuration reload invalidates active sessions and reports restart-required trust changes without taking proxying down.

Plain HTTP development remains available only through the explicit loopback option. The protocol multiplexer no longer advertises an empty TLS connection state on ordinary buffered TCP connections, so same-origin loopback requests with ports are classified as HTTP. After protocol sniffing, TLS-backed HTTP retains the real connection state, including for clients that omit ALPN, while Redis RESP over the same no-ALPN TLS listener keeps its existing route.

CPA normalizes CRLF and lone-CR YAML input before comment-preserving writes. This prevents carriage returns retained in parsed comments from becoming extra blank lines when startup hashes a plaintext management secret or another nested scalar changes; the resulting file uses LF line endings.

**Implementation evidence:** `internal/api/analytics_*`, `internal/api/handlers/management/analytics*`, `internal/api/server*.go`, `internal/api/middleware/request_logging.go`, `internal/config/{analytics.go,config_yaml.go}`, `internal/util/provider.go`, and `config.example.yaml`.

**Recorded validation:** management/viewer authorization matrices, schema-v2 management handlers, named-range resolution, event filters and complete exports, stable pagination, pricing provenance and repricing, provider credential rows, production-mux loopback CORS, TLS with and without ALPN, trusted-proxy cases, logging, reload recovery, viewer persistence and revocation, URL identity rejection, throttling, maintenance API tests, and the CRLF nested-scalar regression pass. Raw CDP verifies the management journeys against a live seeded server.

**Last updated:** 2026-10-19

### DL008 - Fresh graceful-shutdown budget

The 30-second graceful-shutdown budget begins when `Service.Run` exits, not when the service starts. Long-running processes therefore give HTTP, analytics, usage observers, and other background components their intended bounded drain instead of receiving an already expired context.

**Implementation evidence:** `sdk/cliproxy/service_lifecycle.go` and `sdk/cliproxy/service_lifecycle_shutdown_test.go`.

**Recorded validation:** the focused regression waits beyond its short synthetic budget before invoking the deferred shutdown and confirms that the callback still receives a live context with a future deadline. Fresh-context CPAUK close tests pass repeatedly; live shutdown validation is part of the final container gate.

**Last updated:** 2026-08-31

### DL009 - Configured analytics storage time zone and exact retained reads

`analytics.storage-time-zone` selects the zone whose local hour and day boundaries retention uses when it builds rollups. It defaults to `UTC`, is validated as an IANA name, and an invalid value disables analytics only while the proxy keeps serving traffic. The store no longer adopts the first query's zone on a fresh database; the configured zone is recorded on first use and existing rollups are never rebucketed by a later configuration change.

Retained reads are exact or refused. A query whose range contains rollups but requests another zone is rejected for daily and hourly grains alike rather than relabelling an indivisible stored bucket, so minute-level events inside one stored hour cannot be moved into the wrong fractional-offset bucket. The store's partial error carries the stored zone, the requested zone, and the bucket width, and the management API surfaces that reason in the `analytics_invalid_query` envelope while keeping both partial sentinels matchable with `errors.Is`; internal error text is still withheld.

Named ranges require top-level `schema_version: 2`, and CSV and JSON exports emit the same flat set of sanitized event fields. Both are recorded in the v2 contract document.

**Implementation evidence:** `internal/cpauk/config.go`, `internal/config/analytics.go`, `internal/api/analytics_options.go`, `internal/cpauk/store/{config.go,operations.go,retained_query.go,store.go}`, `internal/api/handlers/management/analytics.go`, `config.example.yaml`, `docs/analytics-operations.md`, and `docs/analytics-api-contract-v2.md`.

**Recorded validation:** `go test ./internal/cpauk/... ./internal/config/... ./internal/api/... -count=1` passes, including the retained-only acceptance test that retains a day-boundary seed with minute-level offsets, replays hourly buckets in the storage zone against the pre-retention raw aggregation, and asserts a reasoned partial for the same range in `Asia/Kolkata`, plus the handler test that checks the zone pair and grain reach the error envelope. `gofmt -l .` is clean for the touched files and the disposable `cmd/server` compile check passes.

**Last updated:** 2026-09-03

### DL010 - Named retention and viewer expiry in the analytics contract

Three analytics responses stopped being generic.

Raw-event reads whose range starts before the retention cutoff now return a typed `store.RetainedRangeError` carrying the cutoff instead of the bare `ErrRetainedRangePartial` sentinel. The management API maps it to a 400 `analytics_invalid_query` whose message names the RFC3339 cutoff, and the frozen error envelope repeats it in an additive `retention_cutoff` field so a client can narrow the range without parsing English. Both partial sentinels stay matchable with `errors.Is` and internal error text is still withheld.

Viewer capabilities now report the shared view (link) expiry and the browser session expiry as distinct fields. `view_expires_at` is the creator's chosen link lifetime, `session_expires_at` is the 30-minute session, and `expires_at` remains an alias of the session expiry so older viewer clients keep working. The viewer page previously rendered the session expiry under the creator-facing "expires after 7 days" promise.

A store that refuses to open because its persisted `retention_time_zone` metadata disagrees with `analytics.storage-time-zone` now returns a typed `store.ZoneMismatchError`. The service publishes both zone names in health as `category: storage_time_zone`, `field: storage-time-zone`, a message naming each zone, and an additive `zone_mismatch: {stored, configured}` object, so Maintenance can show the operator why analytics is unavailable.

The v2 contract document additionally records the `{keys, meta}` catalog wrapper, the providers/quotas wrappers and their `durable` flag, pricing `sync_state`/`updated_at`, the full `Capabilities` and `Health` field sets, the viewer expiry fields, and the retention error details. On the client, `ViewerEventPage.total_count` became optional to match Go, and `AnalyticsQuotasResponse` gained `durable`.

**Implementation evidence:** `internal/cpauk/store/{config.go,query.go,operations.go}`, `internal/cpauk/model/result.go`, `internal/cpauk/{health.go,service.go}`, `internal/api/handlers/management/{analytics.go,analytics_viewers.go}`, `internal/api/analytics_viewer_routes.go`, `docs/analytics-api-contract-v2.md`, `docs/analytics-operations.md`, and in CPAMC `src/features/analytics/ViewerPage.tsx`, `src/features/analytics/views/viewer/viewerApi.ts`, `src/types/analytics.ts`.

**Recorded validation:** `go build -o test-output ./cmd/server` succeeds and `go test ./internal/cpauk/... ./internal/api/... -count=1` passes, including `TestRetainedEventErrorCarriesTheRetentionCutoff` and `TestStorageZoneMismatchNamesBothZones` (store), `TestStorageZoneMismatchSurfacesBothZonesInHealth` (service health), `TestAnalyticsEventsOverRetainedRangeNamesCutoff` (400 message and `retention_cutoff` envelope field), and `TestViewerScopeSeparatesViewAndSessionExpiry` (the session expiry precedes the view expiry and both serialize). `gofmt -l .` reports only the two pre-existing unformatted executor test files. In CPAMC, `bunx tsc --noEmit` is clean for the owned files and `bun test tests/analyticsViewerExpiry.test.ts` passes four cases covering the two dated sentences, the legacy `expires_at` fallback, and the omitted link sentence.

**Last updated:** 2026-09-03

### DL011 - Viewer cross-origin allowlist

The viewer API (`/v0/analytics/viewer/*`) was same-origin only, which 404s a
shared-view link whenever CPAMC is served from a different origin than CPA
itself (a dev server, or the fork's own documented Nginx deployment).
`analytics.viewer.allowed-origins` adds an explicit, validated allowlist of
extra browser origins permitted to call the viewer API cross-origin, in
addition to same-origin requests.

Each entry is an absolute origin (scheme + host[:port], no
path/query/fragment/userinfo), matched case-insensitively with default ports
normalized. `https` is always accepted; `http` is accepted only for a loopback
host (`127.0.0.1` / `localhost` / `[::1]`) and only when
`analytics.viewer.allow-loopback-http` is also `true`. Up to 32 entries are
allowed; duplicates (after normalization) or any other invalid entry disables
analytics only, matching the existing `viewer.trusted-proxy-cidrs` behavior.
Changing `allowed-origins` requires a CPA restart, like the other
`analytics.viewer` fields.

`applyViewerSameOriginPolicy` now falls back to the allowlist when a request's
`Origin` is not same-origin: an allowed cross-origin request gets
`Access-Control-Allow-Origin` echoing the request `Origin`,
`Access-Control-Allow-Credentials: true`, and `Vary: Origin`, the same headers
same-origin requests already received; anything else still gets a bare 403.
The session-exchange handler reads a gin context flag the middleware sets for
an allowed cross-origin request and issues the viewer cookie with
`SameSite=None; Secure` instead of the same-origin default
`SameSite=Strict; Secure`, since browsers require `SameSite=None` for a cookie
to be sent back on a cross-origin request. `Secure` is always set, so
cross-origin viewers need HTTPS except the already-documented loopback-http
development case. The shared CORS middleware also exposes `ETag`, allowing
cross-origin v8 management clients to complete the GET-then-`If-Match`
configuration write flow without weakening viewer origin checks.

**Implementation evidence:** `internal/config/analytics.go` (`AllowedOrigins`,
`NormalizeOrigin`, `IsLoopbackHostname`), `internal/api/analytics_viewer_routes.go`
(`AnalyticsViewerSecurityOptions.AllowedOrigins`, cookie `SameSite` selection),
`internal/api/server_middleware.go` (`applyViewerSameOriginPolicy` allowlist
branch, `analyticsViewerCrossOriginContextKey`), `internal/api/server.go` (both
`AnalyticsViewerSecurityOptions` construction sites), `internal/api/server_reload.go`
(`markAnalyticsViewerRestartRequired` equality check), `config.example.yaml`,
`docs/analytics-operations.md`, and `docs/analytics-api-contract-v2.md`.

**Recorded validation:** `go test ./internal/config/... ./internal/api/... -count=1`
passes, including `TestAnalyticsConfigAcceptsValidAllowedOrigins` and
`TestNormalizeOrigin` (config), `TestAnalyticsConfigFailuresAreIsolated`'s new
allowed-origins cases (path/query rejected, non-loopback `http` rejected,
loopback `http` rejected without `allow-loopback-http`, normalized duplicates
rejected), `TestApplyViewerSameOriginPolicyAllowsConfiguredCrossOrigin` /
`...RejectsUnknownOrigin` / `...SameOriginUnchanged` (middleware), and
`TestViewerSessionCookieSameSiteMatchesOriginDecision` (cookie attribute).
`gofmt -l` is clean on the touched files and `go build -o test-output
./cmd/server` succeeds. Verified live against the seeded stack: `curl -s -i -X
OPTIONS -H 'Origin: http://127.0.0.1:15173' ... /v0/analytics/viewer/session`
returns the allow headers while `Origin: http://evil.example` gets 403.

**Last updated:** 2026-10-19

### DL012 - Calendar range presets and retained one-year activity

Analytics v2 named ranges accept `prev_week`, `prev_month`, `this_year`, and
`prev_year`. The previous-period presets resolve to complete local calendar
periods, while `this_year` ends at request time. Calendar presets reject `n`,
`start`, and `end`. Activity `window: "year"` returns exactly 365 adjacent
local-day buckets and aligns long rolling requests to complete local days so
retained daily statistics remain queryable.

Schema migration 2 creates a per-storage-zone-day and per-key `daily_stats`
table, backfills it once from existing hourly or daily rollups, and rebuilds it
after retention. The year activity path reads hot days only from `events` and
retained days only from `daily_stats`; both halves honor `key_ids`. Retained
rows preserve request, succeeded, and failed counts, all six token categories
(`input`, `output`, `reasoning`, `cached`, `cache_read`, and `cache_creation`),
and `total_tokens`. They do not preserve known cost or unpriced-token totals.

**Implementation evidence:** `internal/cpauk/aggregate/range.go`,
`internal/cpauk/model/query.go`, `internal/cpauk/store/activity.go`,
`internal/cpauk/store/activity_test.go`, `internal/cpauk/store/daily_stats.go`,
`internal/cpauk/store/daily_stats_test.go`,
`internal/cpauk/store/migrations/002_daily_stats.sql`,
`internal/api/handlers/management/analytics.go`,
`docs/analytics-api-contract-v2.md`, `docs/analytics-operations.md`, and the R6 seeder in
`/tmp/cpa-critique/seed/main.go`.

**Recorded validation:** the calendar range tests cover
`America/St_Johns` and `Asia/Kolkata`; storage tests verify retention output,
migration backfill, DST-aware day bounds, 365 gap-free activity buckets,
cross-zone rejection, source independence, and `key_ids` across retained and
hot days.

**Last updated:** 2026-09-04

### DL013 - Human-readable API key labels

Configured inbound API keys may carry an optional `label` alongside their
stable SHA-256 `key_id`. Labels preserve their exact UTF-8 text, including
whitespace and case, and every non-empty label must be unique within the
configuration. An empty label clears the metadata; unlabeled keys continue to
use the short key ID in management views. Labels do not change key identity,
limits, authorization, or CPAUK history, and label-only edits apply through
configuration hot reload without restarting the proxy.

The management API accepts labels when creating, rotating, or editing a key;
omitting the field preserves the current label and sending an empty string
clears it. Config-index and revision checks remain the authority for writes, so
a stale label edit is rejected without changing another key. Secret-free
`key-identities` and analytics key catalogs expose the configured label for UI
joins while raw keys remain concealed. Duplicate labels are rejected during
configuration loading and all full or partial key mutations. The TUI and
CPAMC create and edit forms support labels, display them in place of short IDs,
and retain stable IDs for usage operations. The TUI label field uses plain
text by default; Ctrl+J switches to a JSON string for entering newlines, tabs,
and other control characters. Existing labels containing controls open in
that mode automatically.

CPAMC's Usage Distribution and Key × Model Heatmap axes show the configured
key label, falling back to the short ID. Their tooltips retain both label and
short ID. Usage Distribution names the token colors in a legend and reports
each category's count in its tooltip. Heatmap model headers truncate with an
ellipsis instead of disappearing when columns are narrow. These frontend
refinements are tracked under DL030 in `web/management-center/FORK.md`.
Analysis card metrics also reuse the dashboard count-up animation, with exact
final formatting and reduced-motion support. Latency tiles have larger values,
tighter spacing, and annotations aligned beside their labels. These changes
are tracked under CPAMC DL026.
CPAMC DL027 rejects invalid capabilities responses at the API boundary so an
HTML dev-server fallback cannot crash analytics while reading its support flag.
CPAMC DL028 extends the same count-up animation to Overview KPIs, daily averages,
and activity summary values through the shared analytics metric component.
CPAMC DL029 makes the Usage Distribution legend filter token categories with
accessible toggles and distinguishes visible token sums from full row totals.

**Implementation evidence:** `internal/config/api_key_entry.go`,
`internal/api/handlers/management/config_lists.go`,
`internal/api/handlers/management/analytics_pricing.go`,
`internal/watcher/diff/config_diff.go`, `internal/tui/{client.go,keys_tab.go}`,
`config.example.yaml`, and the CPAMC `src/features/config/components/blocks/ApiKeysCardEditor.tsx`,
`src/services/api/apiKeys.ts`, `src/types/apiKeys.ts`, and
`src/utils/keyIdentity.ts`.

**Recorded validation:** focused config, management, watcher, and TUI tests
pass, including exact Unicode round-trips, duplicate-label rejection,
create/edit/clear behavior, stable-ID usage resets, and redacted watcher
messages. CPAMC contract tests cover exact label comparison, identity joins,
structured-entry preservation, and create/edit payloads. No analytics schema
or history migration is required because labels are overlaid from current
configuration. An isolated live server verified create/rename/clear, exact
Unicode and control characters, stable ID/authentication/limits, unknown-field
preservation, file hot reload, and rejection of duplicate-label reloads while
retaining the previous runtime configuration. CPAMC passed its 651-test
verification pipeline, followed by final focused tests, lint, TypeScript, and
production build checks. Real Chrome desktop and mobile create/edit/clear
flows also verified short-ID fallback, blank-row targeting, key concealment,
and metadata preservation. The bundled panel was rebuilt from CPAMC commit
`c4ef0c26b22bbb7a0c630ef00db0cb8fc68c9391`.

**Last updated:** 2026-09-06

### DL014 - Shared management quota refresh protection

CPA protects the known CPAMC quota and account-information requests forwarded
through `POST /v0/management/api-call`. Concurrent identical requests for a
credential share one upstream call, and completed results are reused for three
minutes across management clients. This protection runs before OAuth token
resolution so repeated refreshes also share credential acquisition work.

An upstream HTTP 429 blocks further quota requests for the same credential and
provider until `Retry-After` expires. Both delta-seconds and HTTP-date values are
supported; missing or invalid values use a three-minute backoff. The backoff
covers sibling quota endpoints and provider fallback hosts, including 429s
received during token refresh or before an interrupted response body. Other
credentials remain independent. A 429 uses its retry deadline instead of the ordinary
three-minute result lifetime.

Cached provider Date headers advance by cache residence time so CPAMC retains
the provider clock offset used by quota countdowns.

The existing management response envelope is preserved. Generic API calls and
quota-reset mutations are not cached. The known xAI paid-account health probe is
covered, while ordinary chat completions are not. Successful Codex reset-credit
consumption invalidates cached quota results without lifting provider backoff.
An explicit `force_refresh` on a recognized quota read drops that request's
cached result before fetching upstream, while keeping any active 429 backoff.
CPAMC uses this for Codex usage when an administrator presses "Refresh all
credentials" so a provider-side automatic reset can be observed immediately.
State is held in memory per CPA management handler, shared by its clients, and
resets when CPA restarts; separate CPA processes do not share it.

**Implementation evidence:** `internal/api/handlers/management/api_tools.go`,
`internal/api/handlers/management/api_tools_quota_cache.go`, and
`internal/api/handlers/management/api_tools_quota_cache_test.go`.

**Recorded validation:** `go test ./...`,
`go test -race ./internal/api/handlers/management -count=1`, and the server build
pass. Handler tests use registered credentials and a local HTTPS provider to
verify call counts, concurrent clients, exact expiry, Retry-After parsing and
fallback, provider backoff, mutation isolation, interrupted responses, and
preserved countdown clock offset. Bypassing the guard through a test-only Go
overlay makes the one-minute expiry test fail on excess provider calls.

The Codex refresh regression exercises `force_refresh` against a local provider
response and confirms the cached one-minute result is bypassed without removing
active 429 backoff.

**Last updated:** 2026-09-24

Local timing follow-up validation: the full Go suite and required server build pass, including
real HTTP executor and reused-WebSocket regressions. Independent QA generated an OpenAI-compatible
SSE request through a fresh isolated CPA instance and confirmed collector persistence and API
aggregation: first-token latency 381 ms, provider latency 121 ms, unchanged TTFT 221 ms, and
generation 80 ms. Mixed historical/new coverage remained partial. CPAMC displayed these actual
API values at desktop and mobile sizes. No deployment or existing dev process was restarted.

Routing follow-up: CPA stores nullable `routing_time_ms` from receipt to the request's first
provider dispatch. A request-scoped observation shared across execution contexts and credential
attempts keeps later retries from being counted as routing. `upstream_sent_at` now follows the
current dispatch for HTTP and WebSockets and matches the local latency origin. Migration v8
adds routing duration without inventing values for historical events. TTFT measurement is unchanged.
The full Go suite and server build pass; independent end-to-end verification through a fresh
local streaming proxy confirms routing and provider-phase values in desktop/mobile CPAMC.

### DL015 - Per-hop event diagnostics and correct endpoint classes

Analytics events now describe every leg of Client -> CPA -> Provider -> CPA ->
Client so the CPAMC event detail sheet can show where a request stopped and
what each side said. Each event carries the downstream method and path, the
arrival time, the provider method and query-free URL, the send time, the
provider status for successes as well as failures, the raw provider usage node
(bounded to 40 KiB, emitted as JSON when valid), the raw provider error body
(bounded, marked when truncated), and the status, error text, and completion
time CPA returned to the client. The CPA -> Client leg is delivered through a
process-wide `usage.SetProxyResponseObserver` hook fired by the handler cancel
function and rides the same bounded FIFO collector queue as events, so the
patch is applied in the same transaction after its insert and is dropped, never
blocking, when the queue is saturated. The first completion for a proxy request
wins.

Two recording bugs are fixed. Handlers build their execution context on
`context.Background()`, which discarded the proxy request ID and endpoint class
assigned by the request middleware; `GetContextWithCancel` now carries those
values (plus arrival time and request line) across, and the sanitizer accepts
the middleware's class set verbatim, so `endpoint_class` is no longer `unknown`
for every event. The upstream HTTP status is captured by the reporter's tracked
round tripper, so successful attempts store `200` instead of `null`.

Requests that never reach a provider because auth selection finds no usable
credential (every candidate cooling down, none configured) previously left no
analytics trace at all, even though the client received a 429 or 503. The
handlers now publish a zero-token failure record with executor type
`auth-selection`; the sanitizer classifies it as `auth_unavailable`, leaves the
provider status and error body empty, and the proxy response patch supplies
CPA's own error body, so the detail sheet marks the failure on the CPA node
before any provider hop.

`MaxEventBytes` is 96 KiB, allowing 40 KiB generation diagnostics plus escaped
error bodies and event metadata. The conservative collector queue ceiling is
768 MiB so the default 8192-event capacity still fits; payload memory is allocated
as events arrive. Migration 006 adds the nullable
columns; older rows read back as `null` and the client renders them as not
recorded.

**Implementation evidence:** `sdk/cliproxy/usage/{manager.go,delivery.go,proxy_response.go}`,
`sdk/api/handlers/{handlers.go,handlers_auth_unavailable_usage.go}`,
`sdk/cliproxy/auth/conductor_selection.go`, `internal/api/request_id.go`,
`internal/api/server.go`, `internal/runtime/executor/helps/usage_helpers.go`,
`internal/cpauk/model/{event.go,schema.go}`,
`internal/cpauk/collector/{adapter.go,sanitizer.go,collector.go,writer.go}`,
`internal/cpauk/store/{migrations/006_hop_details.sql,write.go,query.go}`,
`internal/cpauk/service.go`, and `docs/analytics-api-contract-v2.md`.

**Recorded validation:** `go build ./...`, `go vet`, and
`go test ./internal/cpauk/... ./sdk/... ./internal/api/... ./internal/runtime/...
./internal/usagecontext/... ./internal/logging/... ./test/...` pass, including
new tests for context inheritance, proxy response publication, round-tripper
capture, raw usage capture, sanitizer bounds, patch queue ordering, and
first-write-wins patching. A live CPA against a local stub provider recorded
`endpoint_class=chat_completions`, `upstream_status_code=200`, the provider
URL, the raw usage JSON, and `proxy_status_code=200` for a success, and
`upstream_status_code=429`, the provider error body, `proxy_status_code=429`,
and CPA's error for a failure. A follow-up request while that credential was
cooling down recorded an `auth-selection` event with `error_class=auth_unavailable`,
no provider status, and the `model_cooldown` body as `proxy_error`.

CPAMC event details now end the timeline at CPA and display its returned status and response
timestamp there, without inferring client delivery. Responsive fact grids hide the first row
divider to avoid doubling the section heading rule.

Usage records also carry the client access-provider identity and whether the request used CPA's
native key provider. These fields are additive to the existing request IDs, endpoint classes, hop
timings, statuses, and bounded context snapshots; the fork's delivery manager remains the only
generic observer implementation.

Stream usage parsers (`ParseOpenAIStreamUsage`, `ParseClaudeStreamUsage`,
`ParseInteractionsStreamUsage`, `ParseGeminiStreamUsage`, `ParseCodexUsage`,
`ParseAntigravityStreamUsage`) now store the sanitized final generation chunk as
`Detail.RawUsage` instead of only the carved usage node, so the management center's
"Raw generation data" drawer shows the provider's end-of-generation telemetry verbatim
(usage, model id/slug, stop reason, timing). Response content, tool calls, prompts, and
instructions are recursively stripped by `sanitizeGenerationChunk` before storage, and
empty containers are pruned so a content-only chunk degrades to the bare usage node.
Non-stream parsers keep storing just the usage node. Generation diagnostics are
bounded to 40 KiB throughout parsing, SDK delivery, and collection; error bodies
retain their separate 4 KiB bound. The `upstream_usage_raw` column is unchanged.
Per-message usage attribution and low-value request echoes are removed before
storage. Recognized all-zero tool counters are omitted; nonzero or unknown tool
telemetry is retained. Oversized JSON falls back to complete fields with aggregate
usage prioritized and an `_truncated` marker, rather than a broken JSON fragment.
Generic observer snapshots accommodate the larger payload while retaining their
shared 64 MiB byte budget.

The authenticated event-detail response resolves the hashed credential identity
against currently loaded credentials and supplies the backing filename for CPAMC.
Only the filename is exposed, without its host directory. Historical entries with
no matching live credential keep the existing ID fallback; stored events and
exports retain their privacy-preserving identities.
Validation: full Go tests and server build pass. Independent review covers
40 KiB overflow, aggregate-counter priority, zero/nonzero tool telemetry,
a 39 KiB SQLite round-trip, and authenticated credential filename resolution.
Replay of a saved Codex completion preserves all aggregate token counts.

CSV and JSON event exports include every recorded event field, including the
nullable local timing observations, hop timestamps, sanitized raw generation
diagnostics, and errors. Existing flat token columns remain available alongside
the canonical token breakdown. CSV encodes structured values as JSON. Completeness
regressions compare the export fields against the event schema so additive
diagnostics cannot silently disappear from exports.
Validation: HTTP export tests retain an exact 40 KiB payload in JSON and CSV,
including quoting, timestamps, nulls, and measured zero values. The full Go suite
and server build pass; independent review also verifies legacy truncated raw text.

CPAMC audit corrections align raw diagnostics with the `upstream_usage_raw` API field and
prefer arrival-to-response timing for the total, falling back to attempt latency for older events.
Validation includes 691 frontend tests, lint, production build, desktop/mobile CDP fixture checks,
and the CPA server compile check.

**Last updated:** 2026-10-19

### DL016 - Catalog pricing, usage probes, and display labels for API-key credentials

Requests served by OpenAI-compatible providers carry the synthetic provider
key `openai-compatible-<name>`, which the models.dev import never produced
rules for, so their cost was always unpriced. Each `openai-compatibility` entry
may now set `pricing-catalog` to a models.dev provider id (for example
`zai-coding-plan`). CPA derives `{provider key, catalog id}` bindings from the
configuration, and the CPAUK fetcher emits catalog rules under the compat key
for every bound provider in addition to the static vendor table. An entry
without a binding falls back to the compat name with the
`openai-compatible-` prefix removed when that id exists in models.dev, so
providers named after their models.dev id (`openrouter`, `deepseek`, `zai`)
price without configuration. The bindings digest is persisted with the catalog
and included in the fetched catalog, so changing a binding through hot reload
or the management API refetches on the next demand-driven pricing read, even
when the change lands during an in-flight fetch. Every successful fetch also
persists the full models.dev provider list, served secret-free by
`GET /v0/management/analytics/pricing/catalog-providers` from the stored
catalog without contacting models.dev; CPAMC uses it to populate a searchable
pricing-catalog dropdown in the provider form. Migration 009 adds the
`pricing_catalog_bindings` and `pricing_catalog_providers` tables.

Entries may also set `usage-probe: zai`. Credentials carrying that attribute
are probed through the management api-call path with the plain API key against
`https://api.z.ai/api/monitor/usage/quota/limit`, which reports the Z.ai coding
plan five-hour and weekly credit windows. Results ride the existing one-minute
quota result cache, run concurrently with a bound of eight, and fill
`quota.windows` on analytics credential rows while keeping the single-meter
fields populated from the weekly window. CPAMC gains a Z.ai quota tab that
renders one meter per window for auth files with the probe attribute.

Every API-key credential type (`claude-api-key`, `codex-api-key`,
`gemini-api-key`, `interactions-api-key`, `xai-api-key`, `vertex-api-key`, and
`api-key-entries` under `openai-compatibility`) accepts an optional `label`,
unique within its list, and auth JSON files honour a top-level `label` that
can be set through `PATCH /v0/management/auth-files/fields`. Labels never feed
credential identity, so relabeling keeps statistics and analytics history.
Analytics credential rows, auth-file entries, and event detail expose
`display_name` / `credential_label` resolved as label, then file name, then a
non-generic auth label, then the provider with a masked key; the hashed
credential id remains the stable key. CPAMC shows the display name in the
Providers, Quotas, Events, and Auth Files views and offers a rename action on
the auth-file detail sheet.

Provider selector fields, labels, reserves, and usage-probe settings remain part of the merged
credential serialization, including Vertex and Interactions API-key entries; upstream additions
do not flatten or omit the fork's provider-specific fields.

**Implementation evidence:** `internal/config/{vertex_compat.go,credential_labels.go}`,
`internal/api/handlers/management/{config_lists.go,auth_files_fields.go,usage_probe.go}`,
`internal/cpauk/store/pricing_discovery.go`, and the provider configuration serializers.

Validation: `go build`, `go vet`, and `go test ./...` pass, including
deterministic tests for binding refetch after an in-flight reconfigure, the
Z.ai payload parser, and display-name precedence. A local CPA with a real Z.ai
coding-plan key priced a `glm-4.7` request from the bound catalog, refetched
the catalog after the binding changed through the management API, and served
live five-hour and weekly windows. The bundled panel was rebuilt from CPAMC
commit `a2eaf19e53f96b321dc51d25d6d7316c9222ecef` (CPAMC DL043).

**Last updated:** 2026-10-19

### DL017 - Temporary upstream failures stay in model lists

HTTP 429 and 5xx responses still cool down request routing, but they no longer
remove the affected model from registry-backed client model lists. A
credential-scoped 429 also keeps sibling models listed when it propagates
credential quota state. Disabled credentials remain hidden and the temporary
failure normalization does not apply to terminal errors such as 401, 403, or
404. Registry publication keeps `transient` as a list-preserving reason while
the merged `credential_quota` accounting correction avoids counting the same
quota state twice.

**Implementation evidence:** `sdk/cliproxy/auth/conductor_models.go`,
`internal/registry/model_registry.go`, and
`sdk/cliproxy/auth/conductor_model_list_visibility_test.go`.

**Recorded validation:** focused regression tests cover credential-scoped 429
siblings plus HTTP 500, 503, and 599, while also verifying that routing remains
blocked during each cooldown and disabled credentials remain hidden.
`go test ./... -count=1` and the disposable server build check pass.

When routing finds no ready credential, the selection warning now reports
credentials blocked by transient upstream failures in a separate "error
backoff" summary next to the quota cooldown summary, so mixed outage windows
are no longer misread as missing failover.

**Last updated:** 2026-10-19

### DL018 - OpenAI-compatible providers receive downstream request headers

OpenAI-compatible executors now forward downstream client request headers to
the provider instead of rebuilding each upstream request from a small fixed
set. This preserves protocol and vendor session headers such as `Session-Id`,
`X-Session-Id`, `X-Session-Affinity`, and `x-opencode-session` for harnesses
that rely on them. Chat, image, streaming, and non-streaming generated requests
share the same header preparation path.

Transport and body-framing headers, hop-by-hop headers and their
`Connection`-scoped fields, `Host`, `Accept-Encoding`, and downstream CPA
credentials are omitted. The selected provider credential replaces
`Authorization`, the downstream `User-Agent` is retained with the existing
fallback, and configured custom headers are applied last so they retain
precedence.

**Implementation evidence:** `internal/runtime/executor/openai_compat_executor.go`
and `internal/runtime/executor/openai_compat_executor_headers_test.go`.

**Recorded validation:** focused execute/stream regressions preserve the Codex
and OpenCode session headers, multiple arbitrary header values, and the client
User-Agent while replacing credentials and dropping transport or connection
headers. The full `internal/runtime/executor` package, `go test ./...`, and the
disposable server build pass.

**Last updated:** 2026-09-16

### DL019 - Credential-labelled distributions, quota window gating, and Analysis chart metrics

The Analysis credential distribution plotted the hashed credential id while every
other dimension already showed something a person could read. Credential
dimension rows now carry admin-only `credential_label` and `credential_filename`
fields, resolved through the same loaded-credential lookup that event details
use. The panel prefers the label, then the file name, then a compact id for
credentials that are no longer configured. Viewer routes keep the previous
behaviour and never receive either field.

Top Models gained a tokens / price / generation segmented control. Tokens and
price stay stacked bars; generation switches to a line of average per-bucket
generation time, with `null` gaps where a bucket recorded no timing sample and
a sample count beside each ranked model. `TimeseriesPoint` and `AnalysisModel`
carry `generation_time_ms` and `generation_sample_count`, and the SQLite store
aggregates the raw per-event timings into model and bucket totals so the average
is weighted by samples instead of averaging bucket averages.

Quota windows on one credential are gates, not independent buckets: whichever
window runs out first decides when the credential works again. A new
`windowGating` helper marks a window that still has headroom as unusable when a
sibling that also governs the same requests is spent, and `QuotaMeter` keeps the
remaining width while desaturating the fill into candy stripes, so the level
stays legible and the row reads as temporarily unusable rather than empty.
Codex groups the standard allowance, the code-review allowance, and each
additional model allowance (Codex Spark) into independent families that gate
only themselves. Claude treats the five-hour and seven-day windows as the
family blockers, so exhausting either greys the model-scoped allowances such as
Fable while spending Fable leaves the base windows alone. Z.AI credit windows
gate each other. Monthly second windows follow the same rule as weekly ones.
The stripe fill was added to both quota hosts and the blocked-window hint to all
four locales.

Smaller analytics changes in the same pass: Request Health now reaches amber
below a 90% success rate (red below 50%, orange below 70%, amber below 90%,
green above it), the Latency Diagnostics radar uses a linear millisecond axis
instead of log10 so a vertex sits at the share of the slowest timing it actually
represents, and the Active key badge breathes by modulating its colour, with a
`prefers-reduced-motion` opt-out.

**Implementation evidence:** `internal/cpauk/model/result.go`,
`internal/cpauk/store/query.go`, `internal/cpauk/store/analysis.go`,
`internal/api/handlers/management/credential_display.go`,
`internal/api/handlers/management/analytics.go`,
`internal/api/handlers/management/analytics_events.go`, and the CPAMC
`src/features/quota/windowGating.ts`, `src/features/quota/providers/*/data.ts`,
`src/features/analytics/views/analysis/*`, and `src/features/quota/QuotaMeter`.

**Recorded validation:** `go test ./...`, `go build`, and the disposable server
build pass, alongside 743 CPAMC tests, type-check, lint, and the production
bundle build. A dev CPA served the rebuilt panel against a copy of the real
analytics database: the credential dimension returned `credential_label` for the
loadable Z.ai credential and the chart rendered that label instead of the hash;
Top Models ranked by all three metrics with weighted averages (2 sec and 901 ms
over 30 samples each); the radar placed a 2.2 sec generation vertex on the outer
ring with 874 ms, 614 ms, and 457 ms proportionally inside it; the Z.ai card
striped its 100% five-hour bar because the weekly window was exhausted; and
desktop and 390 px mobile captures showed no horizontal overflow.

The bundled panel is rebuilt from the current CPAMC pin; see the latest
divergence entry for its recorded commit and artifact digest.

**Last updated:** 2026-09-16

### DL020 - Credential-scoped throughput estimation

The event throughput column only replaced a recorded generation duration when it was missing or
zero, so compressed observations of a millisecond or a few milliseconds survived as physically
impossible rates. CPA now treats a recorded generation interval as unusable when
`(provider_latency_ms + wait) * 0.08 > generation_time_ms`, where the wait is
`first_token_latency_ms - provider_latency_ms`, and publishes
`output tokens / (provider_latency_ms + wait + generation_time_ms - median provider latency)`
instead. The estimate expresses the whole measured request minus the credential's normal
acknowledgement overhead. This replaces the earlier rule that no TPS threshold rejected fast
generation.

The baseline is the median `provider_latency_ms` for the **same credential** over the twenty-four
hours ending at the query range end, because one credential backs one upstream account and a
baseline mixed across credentials would skew the result. The store computes it with one windowed
query per event read and applies it to both the events list and the single-event detail. A
credential with no observation in that window, a missing or non-positive provider acknowledgement,
or a non-positive span leaves the rate unpublished rather than guessed; the wait and generation
intervals count as zero when they were not recorded, and an event with no recorded first-token
latency keeps its measured rate because nothing suggests compression.

The derivation moved out of CPAMC so the table, the detail chart, the export path, and any other
client agree on one value. Events now carry response-only `tokens_per_second` and
`speed_estimated`; intake and storage still persist neither. CPAMC renders the published rate and
marks an estimate with its compact `EST` badge, and shows `Unavailable` when a server publishes no
rate.

Plugin executor streams no longer flush a partial usage line at 64 KiB. Long usage-bearing
completion lines are assembled until a complete line or valid JSON payload is available, so
telemetry is not discarded; the generic usage delivery manager remains bounded separately.

**Implementation evidence:** `internal/cpauk/model/event.go`,
`internal/cpauk/model/throughput_test.go`, `internal/cpauk/store/timing.go`,
`internal/cpauk/store/query.go`, `internal/cpauk/store/throughput_test.go`,
`internal/pluginhost/adapters_executors.go`, and the CPAMC
`src/features/analytics/views/events/eventDiagnostics.ts` and `src/types/analytics.ts`.

**Recorded validation:** focused model tests cover the observed branch, the threshold boundary, the
compressed-generation estimate, the zero-valued inputs, and every unavailable input; store tests
cover the credential-scoped median, the trailing-day window, and the detail lookup. `go test
./...`, the required disposable server build, and the CPAMC suite, type-check, lint, and production
bundle pass. A temporary CPA served a fixture copied from the real analytics database: the events
API published the expected `1000` estimated tokens per second for a compressed 10 ms generation,
and the bundled panel and event detail rendered `1,000 tokens/s` with the `EST`/`Estimated` label
at desktop and 390 px widths without overflow.

The bundled panel was rebuilt from CPAMC commit
`8fabf52c5a223352c3cd16eb9f91ae680190e947` with artifact digest
`ce3e9e4883d157dd60873bbbe247e264300d9832d59c60ad0dcbbeb60b87c5ad`.

**Last updated:** 2026-10-19

### DL021 - Provider selectors and OpenCode Go usage tracking

`pricing-catalog` and `usage-probe` now apply to every API-key provider section:
`claude-api-key`, `codex-api-key`, `xai-api-key`, `gemini-api-key`,
`interactions-api-key`, and `vertex-api-key`, in addition to the existing
`openai-compatibility` selectors. Configuration normalization, management
PUT/PATCH validation, auth synthesis, auth-index responses, hot-reload diffs,
and analytics catalog bindings all cover the expanded provider set. New catalog
bindings are emitted only when the selector is configured, while an explicit
binding suppresses the static models.dev mapping for the same CPA provider key.

The management usage-probe registry adds `opencode-go` alongside `zai`. An
OpenCode Go credential is probed at `https://opencode.ai/zen/go/v1/usage`; the
rolling, weekly, and monthly windows are exposed as provider quota windows,
with weekly preferred for the headline values when present. The request uses
the existing quota cache and API-call execution path.

The CPAMC OpenCode Go card renders its tier as `Plan` with a bold value and
reports every window as a remaining percentage, matching the Claude, Codex, and
Z.ai cards; the `opencode_go_quota.percent_used` string is retired.

**Last updated:** 2026-09-17

### DL022 - Usage refresh synchronizes routing cooldowns

Successful Z.ai and OpenCode Go quota refreshes now drive the same routing
cooldown state as upstream execution results. If a refreshed usage report shows
an exhausted window, CPA replaces the cached credential cooldown with the
earliest future reset time reported by that window; this can shorten an older,
longer provider `Retry-After` cooldown. If the report shows all parsed windows
healthy, CPA clears stale quota cooldowns so a manually reset credential can
rejoin rotation immediately. A report is ignored when an exhausted window lacks
a future reset time, when the report cannot be parsed, or when cooling is
disabled for the credential.

Claude usage refreshes now use the same provider-aware report path. Shared
five-hour and seven-day windows clear or set credential-wide cooldowns, while
the Fable weekly allowance updates only Fable models; Opus and Sonnet fields
remain display data because they draw from the shared weekly pool. An exhausted
Fable allowance therefore does not hide otherwise healthy models.
Claude, Z.ai, and OpenCode Go quota responses use the same cache-aware
reconciliation path as Codex. A successful fresh provider response updates
routing cooldowns; cached replay leaves the previous routing state unchanged.

Codex reset-credit consumption clears standard-model runtime and registry
cooldowns immediately after a successful upstream response. A fresh Codex usage
read clears stale cooldowns for models whose reported allowance has recovered;
it preserves a separate exhausted Spark allowance and does not impose a
credential-wide block from one exhausted model family. Cached healthy usage
responses cannot clear a newer quota error, and incomplete reports leave
routing state untouched. The manual reset and the Quota page's "Refresh all
credentials" action make recovered models selectable without waiting for the
old cooldown deadline.

The behavior is centralized in the auth manager and runs after successful
management quota API calls and analytics usage probes, so the existing
"Refresh quota" flow needs no separate reset action.

When quota observations are merged into auth state, the latest observation timestamp and signals
are preserved together with the fork's credential-quota cooldown fields, so refreshes cannot erase
newer routing state.

**Implementation evidence:** `sdk/cliproxy/auth/{conductor_cooldown.go,conductor_lifecycle.go}`,
`internal/api/handlers/management/usage_probe.go`,
`internal/api/handlers/management/api_tools.go`, and the CPAMC quota adapters
under `web/management-center/src/features/quota/providers/`.

**Recorded validation:** focused auth-manager tests cover exhausted-overwrite,
healthy-clear, past-reset no-op, and credential-wide cooldown preservation;
management tests cover the Z.ai/OpenCode Go window decision, Claude scoped
Fable recovery, Codex manual reset, mixed-family recovery, cached usage replay,
cache-expiry recovery, and upstream usage sync. CPAMC quota tests cover normal
cache-path refresh wiring. The focused Go tests, CPAMC type-check, lint, and
production build pass.

**Last updated:** 2026-10-19

### DL023 - Retry rounds cover transport and status-less failures

Credential retry rounds (`request-retry`) previously started only for HTTP
statuses 403/408/429/500/502/503/504. A request that died at the transport
layer (connection reset, DNS failure, mid-stream disconnect such as
`http2: response body closed`) returned no HTTP status, so the retry loop
exited after a single pass through the credential pool regardless of
configuration.

Status-less failures are now retry-round eligible: after the usual exclusions
(caller cancellation, request-scoped and invalid-request faults), the retry
gate treats a missing HTTP status as a transport-level failure and allows
additional rounds subject to the same `request-retry`, `max-retry-credentials`,
and `max-retry-interval` limits as status-based retries. Credential cooldown
state recorded from a status-less failure also stays retry-round eligible, and
typed transport errors (`*url.Error`, `net.Error`, EOF family) are classified
as connection-lifecycle failures so a network blip does not cool the
credential. Classification is type-based via `errors.Is`/`errors.As`; the
remaining message matching applies only to legacy results that carry no typed
error information.

**Implementation evidence:** `sdk/cliproxy/auth/conductor_selection.go`
(`isRetryRoundEligibleError`, `credentialRetryRoundStateEligible`) and
`sdk/cliproxy/auth/conductor_cooldown.go` (`isTransportFailure`,
connection-lifecycle classification).

**Recorded validation:** new auth-manager tests cover status-less retry rounds,
caller cancellation exclusion, status-less cooldown waits bounded by
`max-retry-interval`, and transport failures skipping credential cooldown.
`go test ./...` and the disposable `cmd/server` build check pass.

Follow-up (2026-10-14): an untyped `http2: response body closed` result still
fell through to the generic failure branch and cooled the credential and model
for the one-minute transient cooldown. That message is now classified as a
connection-lifecycle failure, so it no longer cools the credential;
`TestManager_MarkResult_PreHTTPTransportFailureDoesNotCooldown` covers the
wrapped error and the bare message.

**Last updated:** 2026-10-14

### DL024 - Plaintext collaboration metadata for optimized Codex calls

The optimized Codex multi-agent request path removes encrypted message arguments
so compatible upstream models can read collaboration tasks. Response restoration
now adds `encrypted_function_args: []` to restored `spawn_agent`, `send_message`,
and `followup_task` function calls when the upstream did not provide metadata.
Existing nonempty encryption metadata is preserved, and unrelated function calls
are unchanged. This lets Codex select plaintext inter-agent delivery before it
constructs an encrypted child message.

Optimization eligibility uses the request payload rather than the HTTP
`User-Agent`. CPA recognizes the exact encrypted `collaboration.spawn_agent`
tool schema, plaintext or encrypted `agent_message` input, the boundary-prepared
request marker, or its reserved optimized namespace. Same-named top-level
functions and functions in other namespaces remain untouched. This supports
Codex app-server embedders whose client name becomes a custom `User-Agent`,
including T3 Code, without broadening rewrites to unrelated tool definitions.

**Implementation evidence:** `internal/client/codex/optimize-multi-agent-v2/optimize_multi_agent_v2.go`,
`internal/client/codex/optimize-multi-agent-v2/optimize_multi_agent_v2_test.go`,
and `internal/runtime/executor/codex_executor_spawn_agent_test.go`.

**Recorded validation:** focused Responses-boundary, optimization, and executor
tests pass for custom client identities, GPT and DeepSeek-compatible parent model
names, plaintext child task delivery, unrelated same-named tools, and
preservation of existing encrypted-function metadata.

**Last updated:** 2026-09-24

### DL025 - Captured Claude Code OAuth request safeguard

`claude-header-defaults.oauth-safeguard` is an opt-in guard for direct
`api.anthropic.com` requests made with Claude OAuth credentials. CPA uses a
Claude OAuth credential from its own auth store to install the official Claude
Code npm package in a private directory and run a small request through a local
HTTPS interception proxy. The proxy relays the request to Anthropic. A successful
response promotes the new executable and its captured profile together. The
profile stores the CLI version, static software headers, and observed request
field names, without credentials or prompt values. The updater checks npm when
due at startup and every 24 hours afterward. A failed candidate leaves
the last validated reference active; a missing or invalid active reference
blocks guarded requests. Failed background captures retry after an hour. CPA
retains the active version and one rollback version, then removes older private
packages and the npm cache. `--claude-capture-update` forces an immediate check.

The reference CLI receives a clean environment with a private home and XDG
directories, including `CLAUDE_CONFIG_DIR`. npm uses a private cache and ignores
the user's npm configuration. CPA invokes the installed executable by its private
absolute path, never the host's `claude` executable or login state. Environment
redirection keeps Claude Code's normal config and data writes private; it does not
prevent a future CLI release from deliberately opening an absolute host path.
Docker deployments also separate the reference from host Claude paths because
those paths are not mounted in the container.

The runtime Docker image includes Node.js and npm. Docker Compose persists the
private reference in a dedicated volume without extra container privileges.

When enabled, CPA requires a current capture for its private Claude Code
version. It checks the native Claude Messages client format and captured static
headers before preparing the OAuth credential, then checks the final Anthropic
destination, bearer authentication, OAuth beta, request shape, and session
identity before any inference request reaches upstream. The incoming User-Agent
must use the `claude-cli/X.X.X` shape, but its version may differ from the
captured release. Native entrypoints include `cli`, `sdk`, `sdk-cli`, `sdk-ts`,
`sdk-py`, `claude-vscode`, and `claude-desktop`. Package, runtime, OS, and
architecture headers are not compared to the capture. The captured profile
does not replace configured `Claude Header Defaults`;
configured package and runtime versions supply the outgoing values. Optional
fields and beta values vary across legitimate Claude Code requests, so one
capture does not define an exhaustive request shape. This guard compares
observable request properties; a client that copies them can impersonate Claude
Code. TLS fingerprints are outside its scope. Rejections return a request-scoped
403 with the reason and log the same non-secret reason.

**Implementation evidence:** `internal/claudecapture/`,
`internal/runtime/executor/claude_oauth_safeguard.go`, Claude executor paths,
`cmd/server/main.go`, config, and the CPAMC toggle.

**Recorded validation:** focused capture/config/executor tests, `go test ./...`,
and the disposable server build pass. The private capture tests verify the
24-hour check boundary, a read-only active reference, a hidden host home, OAuth
delivery by file descriptor, rejection of a mismatched credential before relay,
compressed and JSON Messages responses, and old-version pruning. A prior real
installed Claude Code OAuth client completed a Messages request through an
isolated CPA server; a real
Codex Responses client received 403 from the guard before upstream. The
unrelated `TestSyncUsageProbeCooldown` fixture now uses a future timestamp so
the full suite remains valid after its former fixed date passes.
Executor regression tests confirm that `claude-cli/X.X.X (external, sdk)`,
`sdk-ts`, `sdk-py`, and `claude-desktop` reach the upstream transport with their
own User-Agent when the guard is enabled, while matching requests without
`X-App: cli` are rejected before transport. A local fake-endpoint capture of
T3 Code's installed Agent SDK observed `sdk-ts` with `X-App: cli`; a local
Claude Code process with the Desktop entrypoint observed `claude-desktop` with
the same `X-App` value. Neither capture sent credentials upstream. A separate
loopback CPA instance with a private OAuth credential copy and a newly captured
Claude Code 2.1.281 reference then completed live Anthropic Messages requests
from the installed T3 Agent SDK and Claude Code launched with the Desktop
entrypoint. Both returned `OK`; the temporary instance and credential copy were
removed afterward.

**Last updated:** 2026-09-24

### DL026 - Image generation tool injection covers metadata-only responses-lite requests

The Codex executor injects the built-in `image_generation` tool into eligible
Codex upstream requests that carry the responses-lite marker only in
`client_metadata.ws_request_header_x_openai_internal_codex_responses_lite`.
Codex CLI uses that form over the websocket transport, and its handshake has no
lite header. The previous exclusion skipped every responses-lite request, so
websocket agents that did not declare their own image tool got no image
capability, even with `disable-image-generation: false`.

Requests that carry the `X-OpenAI-Internal-Codex-Responses-Lite: true` header
still skip injection. CPA forwards that header upstream, and upstream then
rejects any built-in tool with `400 unsupported_value` ("only supports function
tools, custom tools, and client-executed tool search"). Codex CLI 0.162.1 sends
the header on HTTP turns for models whose metadata has `use_responses_lite`
(for example `gpt-6.1-sol`). An earlier version of this entry injected the tool
for header requests too, and every HTTP turn from those clients failed.

Clients that declare their own `image_gen` namespace tool are still detected and
skip injection, `spark` models and free-plan credentials still skip injection,
and `/responses/compact` requests remain tool-free.

**Implementation evidence:** `internal/runtime/executor/codex_executor_request.go`,
`internal/runtime/executor/codex_executor_execute.go`,
`internal/runtime/executor/codex_executor_stream.go`,
`internal/runtime/executor/codex_websockets_execute.go`,
`internal/runtime/executor/codex_websockets_stream.go`,
`internal/runtime/executor/codex_executor_imagegen_test.go`, and
`internal/runtime/executor/codex_websockets_executor_test.go`.

**Recorded validation:** focused executor tests cover header skip, metadata
injection, existing-tool detection, free-plan and spark skips, compact requests
without the tool, and responses-lite `parallel_tool_calls` normalization. The
header-skip tests fail when the guard is removed. `go test ./internal/runtime/executor`
and `go vet ./internal/runtime/executor` pass. Live runs on 2026-10-13 used Codex
CLI 0.162.1 through a local CPA with a ChatGPT Pro Lite credential. Over HTTP,
the request carried the lite header upstream with no injected tool, and it
completed (the previous build returned the 400 above). Over websocket, the
request carried metadata only with `image_generation` injected, and it also
completed.

**Last updated:** 2026-10-13

### DL027 - Quota report persistence releases the manager lock during store I/O

Targeted provider quota reports now use the manager's lock-safe persistence helper. The helper
persists an auth snapshot after releasing `m.mu`, then reacquires the lock and merges store-owned
changes, avoiding a self-deadlock when the report updates a model state while the manager write
lock is held.

**Implementation evidence:** `sdk/cliproxy/auth/conductor_cooldown.go` and
`sdk/cliproxy/auth/conductor_persistence.go`.

**Recorded validation:** the targeted quota-report regression reproduces the former `RLock`
deadlock before the fix and passes afterward; the full `sdk/cliproxy/auth` package, its race
checked targeted tests, and the server build pass.

**Last updated:** 2026-10-07

### DL028 - Greedy soonest recovery credential routing

Routing supports the `greedy-soonest-recovery` strategy (also accepted as `gsr`).
It assigns exponentially larger shares to eligible credentials with earlier quota
recovery timestamps, while normal priority and availability filtering still apply.
The default 36-hour half-life keeps later windows in rotation. Session affinity
wraps this selector like the other built-in strategies, preserving sticky sessions
while applying recovery-aware selection for new and failed bindings. Quota-only
affinity rebinding tracks transiently unavailable credential IDs computed from the
same candidate pool and timestamp used by GSR, so quota recovery and sticky-session
decisions remain consistent.
GSR also applies a provider-neutral allowance tier multiplier. Codex plan metadata
and explicit `gsr_tier` overrides are supported; unknown tiers remain neutral and
raw quota totals are never compared across providers.
The recovery window is provider-specific: Claude Fable uses the Fable weekly
window, other Claude models use the normal weekly window, Codex and GLM use the
weekly window, and OpenCode Go uses the monthly window. A missing cached reset is
refreshed synchronously through the shared management quota cache; failed refreshes
exclude that credential, and an empty eligible set returns an explicit routing error.
Management routing reads and writes normalize the aliases to the canonical strategy.

**Implementation evidence:** `internal/config/config_types.go`,
`sdk/cliproxy/service_config.go`, `sdk/cliproxy/auth/`, `sdk/cliproxy/auth/scheduler.go`,
`internal/api/handlers/management/config_basic.go`,
`internal/api/handlers/management/usage_probe.go`, `internal/api/server.go`, and
`config.example.yaml`.

**Recorded validation:** focused routing normalization and service selector tests pass,
including the canonical name, aliases, and session-affinity wrapping.

**Last updated:** 2026-10-19

### DL029 - CPAMC quota summary redesign

The bundled CPAMC quota page adds provider summaries while preserving the existing header,
refresh controls, provider tabs, search, sorting, and credential list. Summary percentages use the
largest available tier, Claude summaries prioritize the lower-remaining weekly pool (Fable wins
ties), and OpenCode Go uses its monthly recovery window. Summary and detailed quota rows expose
credential, remaining percentage, reset timing, and plan details through the shared tooltip path.
Unavailable allowance fills retain the existing candy-wrap treatment.
The bundled client is updated to CPAMC pin `e2cb1d6`.

**Implementation evidence:** the CPAMC gitlink and
`internal/managementasset/bundled/management.html`.

**Recorded validation:** focused quota tests, TypeScript, lint, production build, canonical Docker
management bundle build, and the required server build pass. The full CPAMC suite has one
pre-existing legacy-backend probe failure on the local base.

**Last updated:** 2026-10-19

### DL030 - Kiro OAuth, Claude-compatible routing, and quota support

CPA now treats Kiro/Amazon Q as a built-in OAuth provider. The native CLI login follows Kiro's official `app.kiro.dev/signin` portal, accepts the `awsidc` callback, starts AWS OIDC device authorization for the selected organization, polls for tokens, and persists both native camelCase and CPA snake_case credential shapes. Desktop/social refresh and AWS OIDC refresh are supported, with rotated refresh tokens and profile ARNs retained. The CLI also exposes `--kiro-login`.

Kiro credentials register a native executor that translates OpenAI Responses and Claude Messages requests into Kiro `conversationState` payloads, discovers missing Identity Center profile ARNs through `ListAvailableProfiles`, and sanitizes Codex JSON schemas. It sends AWS event-stream requests to `runtime.<region>.kiro.dev` for profiled accounts and the legacy `q.<region>.amazonaws.com` endpoint for profile-less Identity Center/Builder credentials, restores Claude-compatible streaming and non-streaming responses, and normalizes Claude model aliases such as `claude-sonnet-4-5-20250929` to Kiro's dotted IDs. Kiro does not return ordinary input/output token counts, so CPA estimates input tokens from the translated Claude request and output tokens from generated text, reasoning, and tool data while retaining explicit Kiro counts when a runtime variant supplies them. The stream adapter emits each Kiro event's `data:` record through CPA's Claude-to-OpenAI translators, preserves translator state across a complete stream, and emits terminal Responses events with the final response ID and output. Kiro participates in Claude model aliasing, thinking normalization, routing, and load balancing.

CPAMC adds Kiro to the OAuth page, provider branding, auth-file grouping, and quota views. The quota adapter reads Kiro `getUsageLimits` data through the management API and renders included and free-trial request windows, subscription details, overages, and reset times. Profile ARNs are exposed as sanitized metadata only; credential files are never downloaded into the browser.

The CPAMC Kiro quota body uses the redesigned quota row layout and contributes its plan and request windows to the quota summary strip. A later follow-up (DL033) adds the Kiro GSR recovery probe; Kiro credentials without an available monthly reset timestamp are still excluded under `greedy-soonest-recovery`.

**Implementation evidence:** `internal/auth/kiro/`, `sdk/auth/kiro.go`, `internal/api/handlers/management/auth_files_kiro_oauth.go`, `internal/runtime/executor/kiro_{executor,translator}.go`, `sdk/cliproxy/service_{auth,executors,models}.go`, `web/management-center/src/{pages/OAuthPage.tsx,features/quota/providers/kiro,services/api/kiroQuota.ts}`, and the bundled management assets.

**Recorded validation:** Kiro auth and translator tests, focused executor/auth/service tests, the usage-estimation regression test, and the required `go build -o test-output ./cmd/server` pass. CPAMC type-check, tests (apart from the pre-existing legacy-backend diagnostic), ESLint, Vite build, and Dockerized `scripts/build-management-center.sh` pass. The full Go suite reaches all packages; its only failure is the pre-existing `TestV0RepeatedSavesKeepLegacyClientAlias` compatibility test.

**Last updated:** 2026-10-12

### DL031 - Single-pass CPAUK model timeseries aggregation

CPAUK analytics now maintain exact compact 15-minute materialized aggregates for overall, model, provider, and key common-query paths, plus lossless request identities for exact retry deduplication. Batch writes coalesce aggregate deltas, and retention, imports, repricing, and purge rebuild only affected buckets. UTC ranges aligned to 15-minute boundaries and wider bucket widths read compact aggregates; other time zones and partial boundaries retain raw/rollup processing for calendar correctness. Analysis model-by-time queries use one raw-event scan to populate all model buckets, and timing, summary, activity, and dimension queries avoid duplicate scans. The prior high-cardinality materialization was removed because it created nearly one aggregate row per event. The retained-history path remains rollup-aware and keeps the existing hourly/daily correctness rules.

**Implementation evidence:** `internal/cpauk/store/{analysis.go,activity.go,query.go,retained_query.go,timing.go,write.go,materialized_15m.go,materialized_read.go,migrations/010_materialized_15m.sql}`, and `internal/cpauk/aggregate/range.go`.

**Recorded validation:** `go test ./internal/cpauk/...` and the required server build pass. On a deterministic 100,000-request, 90-day fixture, compact ingestion fell from 31.92s to 13.83s, database size from 444MB to 405MB, 30-day 15-minute timeseries from 183ms to 63ms, 30-day activity from 251ms to 55ms, and 90-day summary from 546ms to 550ms. Compact short-range dimension queries intentionally use the raw path because identity scans are slower than raw SQLite aggregation at that scale. Benchmark harness and comparison scripts live under `test/perf/`.

**Last updated:** 2026-10-07

### DL032 - Bounded historical analytics result cache

CPAUK keeps a bounded process-local LRU cache for complete historical Summary, Timeseries, and Analysis results. Cache keys include the normalized query selection, identity epoch, durable 15-minute bucket generations, and pricing provenance for Analysis. Writes, imports, repricing, retention, purge, restore, and identity-epoch changes advance affected generations or clear the cache, so late historical changes cannot return stale results. Cache entries are defensively cloned and skipped for live ranges or partial Analysis responses.

**Implementation evidence:** `internal/cpauk/store/history_cache.go`, `internal/cpauk/store/history_generation.go`, and the CPAUK query, write, retention, repricing, purge, restore, and epoch paths.

**Recorded validation:** `go test ./internal/cpauk/...` and the required server build pass. On a 100,000-request fixture, a 90-day cached Summary fell from about 535ms cold to 5ms on a hit; an unrelated tail write kept the historical hit at about 5ms, while a late historical write forced a fresh read at about 563ms.

**Last updated:** 2026-10-12

### DL033 - Kiro GSR recovery, plan tier, Retry-After, and Identity Center login

Kiro participates in greedy-soonest-recovery. The routing refresh probes `codewhisperer.<region>.amazonaws.com/getUsageLimits` with the credential region and optional profile ARN through the shared management quota cache, then persists the included breakdown's monthly `nextDateReset` as `monthly_reset_at` and the `subscriptionTitle` as `kiro_plan_type`. Kiro stays in the passive quota-observation set, so these probe signals survive Kiro responses that carry no quota headers. The plan title maps to GSR allowance tiers (Free neutral, Pro, Pro+ Premium, Power Max), and Kiro upstream errors carry a standard `Retry-After` delay to the cooldown policy.

The CPAMC Kiro login card offers AWS Builder ID or IAM Identity Center. Identity Center sends the organization start URL and region to the existing `start_url`/`region` backend parameters and remembers them in browser storage; the Kiro callback hint shows its real `http://127.0.0.1/oauth/callback` redirect.

**Implementation evidence:** `internal/api/handlers/management/usage_probe.go`, `sdk/cliproxy/auth/{conductor_selection,gsr_tier}.go`, `internal/runtime/executor/kiro_executor.go`, the CPAMC gitlink, and the bundled management assets.

**Recorded validation:** focused Go and CPAMC tests pass. A live Identity Center login through the CPAMC callback flow produced a working credential; under `greedy-soonest-recovery` a real Claude request routed through it, and the probe stored the account's `KIRO POWER` plan and monthly reset.

**Last updated:** 2026-10-12

### DL034 - Kiro executor usage reporting

Kiro now uses the shared executor usage reporter for both non-streaming and streaming requests. The completed event accumulator supplies its estimated input and output token counts to the usage record, which feeds analytics and per-key limits. Non-2xx and transport failures are tracked as failures, while streaming publishes only after the terminal SSE chunks have been emitted. The reporter's one-shot publish guard makes the terminal success or failure outcome the only record for the attempt.

**Implementation evidence:** `internal/runtime/executor/kiro_executor.go` and `internal/runtime/executor/kiro_executor_usage_test.go`.

The bundled CPAMC labels credit-billed Kiro allowances as credits with fractional amounts, shows the row percentage unrounded with the same formatter as the quota summary and timeline, and gives the Kiro timeline lane its monthly period so the current window draws a bar.

Kiro usage is priced with Amazon Bedrock rates from the models.dev `amazon-bedrock` catalog when no other catalog rule covers the model. Anthropic Bedrock IDs map to CPA Claude IDs by dropping the `anthropic.` prefix and `-vN[:M]` suffix. The `global.` cross-region price wins over the bare regional ID, and regional prefixes such as `us.` or `eu.` are ignored. An explicit `kiro` catalog binding takes precedence. Existing events keep their stored cost until a reprice job runs (`internal/cpauk/store/pricing_discovery.go`, `TestModelsDevFeedPricesKiroWithBedrockFallback`).

**Recorded validation:** Kiro usage tests cover non-streaming and streaming success, a single stream record, and 429 failures. Live requests through an Identity Center credential produced one analytics event each (Claude and OpenAI-format, streaming and non-streaming), and the quota page showed matching credit percentages in the summary, row, and monthly timeline bar.

**Last updated:** 2026-10-12

### DL035 - Kiro streamed tool-input reconstruction

Kiro `toolUseEvent` frames stream the tool input as JSON string fragments and terminate with a `stop` frame that may omit `input`. The executor now aggregates fragments by `toolUseId`, then emits Claude `tool_use` blocks with an empty start object followed by an `input_json_delta`, which preserves arguments in Claude Code. Payload rules run once after Kiro translation and immediately before transport for both buffered and streaming requests. Malformed or non-object provider values are preserved as strings rather than emitting `null`, which keeps Claude Code's tool-input contract valid for both response modes.

Kiro reasoning events do not carry Anthropic-compatible signatures, so CPA retains their text for usage estimation but suppresses unsigned Claude thinking blocks. Text and tool blocks have explicit start, delta, and stop lifecycles. The event-stream parser validates frame bounds, AWS prelude and message CRCs, rejects null or malformed event payloads, and propagates exception frames instead of treating them as successful completion. All named tools are preserved; the executor does not impose the earlier synthetic 16-tool cap.

Text-less turns are sent to Kiro with empty `content` rather than an `(empty placeholder)` filler. Kiro accepts empty content for tool-use and tool-result turns, empty user turns, prefill, and synthetic leading or alternation turns. The filler was imitated by the model in agent loops, which echoed `(empty placeholder)` as visible assistant text before silent tool calls.

**Implementation evidence:** `internal/runtime/executor/kiro_executor.go`, `internal/runtime/executor/kiro_translator.go`, `internal/runtime/executor/kiro_executor_usage_test.go`, `internal/runtime/executor/kiro_stream_test.go`, and `internal/runtime/executor/kiro_translator_test.go`.

**Recorded validation:** the complete executor package tests and disposable server build pass. Regression tests cover fragmented JSON tool inputs, terminal tool events with no input, 16/17/106 tool inventories, final payload overrides and filters, unsigned reasoning suppression, block lifecycles, CRC and malformed-frame rejection, exception/EOF handling, and empty tool-only assistant turns. Live Kiro requests accepted every empty-turn shape, and a tool loop that previously echoed the filler in 4 of 6 runs produced no text in 12 streaming and non-streaming runs. A live Kiro-backed Claude Code run through a CPA binary built from this worktree exercised `qa/2048.html` through Chrome CDP with no browser console or runtime errors.

**Last updated:** 2026-10-15

### DL036 - GSR request fallback and quota-provider identity

Greedy-soonest-recovery now keeps the complete eligible credential pool when a
quota refresh fails or a recovery timestamp is missing, expired, malformed, or
not anchored to an observation time. That request falls back to the selector's
persistent round-robin cursor and emits a warning with the request ID, model,
provider, reason, and eligible, usable, missing, and refresh counts. Normal
model support, disabled and cooldown state, priority, credential policy, auth
kind, pins, retry exclusions, and session affinity still apply. An established
session binding is not refreshed merely because its quota observation is absent;
credentials removed while a refresh is in flight are not restored. The normal
GSR weights resume as soon as every credential in the active choice pool has a
usable recovery timestamp.

Quota refresh now resolves the provider from the configured `usage_probe` and
normalized credential aliases before selecting an endpoint, parser, or persisted
signal. This keeps z.ai and OpenCode Go credentials on their own usage services,
including OpenCode credentials served through the Codex adapter. Mixed-provider
routing reads each credential's provider-specific recovery signal, and OpenCode
Go uses its monthly window even for GLM model names. The same fallback applies to
the scheduler path and normal, streaming, and token-count selection. Upstream
plugin quota-provider IDs remain additive and distinct from the fork's
request-level quota-provider identity mapping.

**Implementation evidence:** `sdk/cliproxy/auth/{conductor_selection.go,selector.go,scheduler.go,gsr_tier.go,quota_recovery_provider.go}`, `internal/api/handlers/management/usage_probe.go`, and their routing and provider tests.

**Recorded validation:** full `sdk/cliproxy/auth` tests and race tests pass; focused quota-provider and GSR management tests pass; the server builds; a local HTTP fixture served six non-streaming and six streaming requests with a 3:3 round-robin split and emitted fallback warnings, while session affinity stayed on one credential across both modes. The full repository suite passes except the unrelated pre-existing `TestV0RepeatedSavesKeepLegacyClientAlias` failure.

**Last updated:** 2026-10-19

### DL037 - Kiro static model definitions for OAuth exclusions

`GET /v8/management/routing/model-definitions/kiro` returned `400 unknown channel`, so the CPAMC OAuth excluded-models editor showed Kiro as unsupported even though `oauth-excluded-models.kiro` filters Kiro's runtime models. The static channel lookup now serves the Claude catalog for `kiro`, the same list Kiro credentials register at runtime.

Credentials that only carry a `usage-probe` (for example Z.AI or OpenCode Go under `openai-compatibility`) remain unsupported there by design: they are API-key credentials, which `oauth-excluded-models` does not apply to. The bundled panel (CPAMC `5647a51`) therefore leaves them out of the OAuth excluded-models and model-alias pickers, and also carries the plan-weighted quota summary update.

**Implementation evidence:** `internal/registry/model_definitions.go`, `internal/registry/model_definitions_test.go`, the CPAMC gitlink, and the bundled management assets.

**Recorded validation:** a CPA built from this checkout returned the 18 Claude models for `kiro`, and still returned `400` for `openai-compatible-zai`; through CDP, both OAuth editors listed only OAuth providers and Kiro loaded the Claude catalog.

**Last updated:** 2026-10-14

### DL038 - Per-credential quota reserves

A credential can hold back part of any quota bar. `quota_reserve` (auth files) or `quota-reserve` (provider API-key entries, on the provider object for `openai-compatibility`) maps a bar ID, the same ID CPAMC renders (for example `five-hour`, `weekly`, `seven-day-fable`, `zai-week-1`, `opencode-go-monthly`, `kiro-included-credit-0`), to the minimum remaining percent (0-99). Once CPA observes a bar at or below its reserve, routing withholds that credential until the bar resets. Client keys listed by ID in `quota_reserve_bypass_keys` / `quota-reserve-bypass-keys` (SHA-256 key IDs from `GET /v8/management/api-keys` `key-identities`, never raw keys) can still use the reserve.

Checks are lazy: after a request result is recorded, a background check reads the credential's usage through the shared management quota cache, using the same URLs and headers as CPAMC's quota adapters, so a recent quota-page read costs no provider call. Each credential is checked at most once per five minutes, and concurrent or retried requests collapse into one in-flight check. Management quota reads also apply reserves. Whether a reserve holds is derived from the last observed bars against the current reserves, so editing or removing a reserve takes effect immediately. A bar whose reset passes stops counting; a held bar with no known reset stays held until a later check sees it above the reserve. Supported providers are Claude, Codex, Z.ai, OpenCode Go, and Kiro.

Requests carry the client key ID in execution metadata (`client_key_id`, config-key principals only). While any reserve holds a credential, selection uses the legacy candidate loops instead of the scheduler fast path, because the scheduler index cannot apply per-request bypass. The credentials listing exposes `quota_reserve`, `quota_reserve_bypass_keys`, and the active `quota_reserve_state`. Config validation and `PATCH /v8/management/credentials/fields` reject out-of-range reserves and bypass entries that are not key IDs.

The bundled panel (CPAMC `a0d3fa2`) adds the quota reserve editor to the auth-file sheet and the Z.ai/OpenCode Go provider form, a draggable handle per bar, a searchable bypass-key picker, candy stripes in the bar's own colour on the part of the fill inside the reserve (on quota rows and provider summary cards), reserve details in row and segment tooltips, and a Reserve reached badge.

**Implementation evidence:** `sdk/cliproxy/auth/quota_reserve.go`, `sdk/cliproxy/auth/{conductor_selection.go,conductor_cooldown.go}`, `sdk/api/handlers/handlers.go`, `internal/api/handlers/management/{quota_reserve_probe.go,auth_files_fields.go,api_tools.go,auth_files.go}`, `internal/config/{config_types.go,vertex_compat.go,credential_labels.go}`, `internal/watcher/synthesizer/{config.go,file.go}`, their tests, the CPAMC gitlink, and the bundled management assets.

**Recorded validation:** quota-reserve unit tests cover evaluation, reset expiry, throttling, reserve edits against a stored observation, bar IDs for all five providers, and real selection with and without a bypass key. A local CPA with the owner's live Z.ai credential (weekly 19% remaining) and a stub fallback provider served the Z.ai credential at a 10% reserve, switched to the fallback after the first request once the reserve was raised to 30%, kept serving the bypass key from Z.ai, and returned to Z.ai as soon as the reserve was lowered again through the v8 config API. Through CDP, that CPA serving the CPAMC build showed the striped bar, its tooltip, and the badge; dragging the handle and saving the provider form persisted the new reserve with the bypass key intact. The full repository suite passes except the pre-existing `TestV0RepeatedSavesKeepLegacyClientAlias` failure.

**Last updated:** 2026-10-15

### DL039 - Streaming keep-alives before the first upstream chunk

`streaming.keepalive-seconds` previously started only after the first upstream chunk, because the SSE handlers hold the response until then to return fast failures with their real status. While credential selection, cooldown waits, retries, or the upstream's time to first token were pending, the client received nothing, and proxies in front of CPA cut those requests: nginx's 60s `proxy_read_timeout` (504) and Cloudflare's 125s origin timeout (524). Cloudflare forwards `103 Early Hints` but does not reset its timer on it, so an interim status is not an alternative.

The Claude Messages, OpenAI Chat Completions, legacy Completions, OpenAI Responses, and Gemini SSE stream handlers now run stream setup in the background through the pre-first-token keepalive path. Nothing is written until one keep-alive interval passes without a first chunk or error, so fast failures still return their JSON error and HTTP status. After that the handler commits HTTP 200 with SSE headers and writes `: keep-alive` every interval until the first outcome. Once committed, the first chunk and later chunks go through the handler's existing forwarder, and a late failure is delivered as that protocol's in-stream error event (`event: error` for Claude, a `response.failed` or `error` event for Responses). Upstream response headers cannot reach the client after an early commit. `send-keepalive-before-ft` is tri-state: when unset, a positive `keepalive-seconds` preserves the fork behavior and enables pre-first-token keepalives; `true` enables them; `false` disables them. Gemini `alt` (non-SSE) output remains unchanged.

**Implementation evidence:** `internal/config/sdk_config.go`, `sdk/api/handlers/handlers.go`, `sdk/api/handlers/stream_first_chunk.go`, `sdk/api/handlers/claude/code_handlers.go`, `sdk/api/handlers/openai/{openai_handlers.go,openai_responses_handlers.go}`, `sdk/api/handlers/gemini/gemini_handlers.go`, `sdk/api/handlers/stream_first_chunk_test.go`, and `sdk/api/handlers/claude/code_handlers_keepalive_test.go`.

**Recorded validation:** race tests cover keep-alives while stream setup is pending and while the first chunk is pending, fast success and fast errors staying uncommitted with their status, a late error redelivered for in-stream reporting, the disabled interval, and client disconnect; the Claude handler tests check keep-alives before the payload, a late 503 as one in-stream `event: error`, and a fast 503 as JSON. Against a local upstream that stays silent for 30s, Claude Messages and Responses streams sent `: keep-alive` every 5s from 5s on, and Claude Code and Codex both completed. Through a Cloudflare-proxied test hostname with a 150s silent upstream and a 15s interval, Claude Messages and Responses streams, a non-streaming Messages request, and a real Claude Code run all completed at 150s, past the 125s origin timeout. The full repository suite passes except the pre-existing `TestV0RepeatedSavesKeepLegacyClientAlias` failure.

**Last updated:** 2026-10-19

### DL040 - Home-mode Claude OAuth safeguard compatibility

The local Claude Code OAuth capture safeguard is disabled for Home-mode executor
requests. Home mode intentionally does not run the local reference updater or own
the provider credential files, so enforcing a filesystem capture there could leave
every direct Claude OAuth request rejected with `403` while the control plane is
otherwise healthy. Standalone CPA instances retain the fail-closed safeguard and
its background updater.

Safeguard rejection messages now include the active reference Claude Code version,
or explicitly say that the reference version is unavailable when rejection occurs
before a profile can be loaded. This makes stale or missing-reference failures
diagnosable without exposing credentials.

**Implementation evidence:** `internal/runtime/executor/claude_oauth_safeguard.go`
and `internal/runtime/executor/claude_oauth_safeguard_test.go`.

**Recorded validation:** focused Home-mode bypass and reference-version rejection
tests pass, and the complete `internal/runtime/executor` package test suite passes.

**Last updated:** 2026-10-15

### DL041 - Short Codex overload cooldown

Codex `server_is_overloaded` responses use a 15-second credential/model cooldown
when no upstream retry hint is provided. Other transient upstream errors retain
the existing configurable cooldown, and an explicit retry hint still takes
precedence. This lets a credential re-enter rotation quickly during a temporary
OpenAI capacity spike without changing the broader transient-error policy.

**Implementation evidence:** `sdk/cliproxy/auth/conductor_cooldown.go` and
`sdk/cliproxy/auth/conductor_overrides_test.go`.

**Recorded validation:** focused transient and Codex overload cooldown tests pass;
the overload regression observes a 10-20 second deadline around the 15-second
default while the existing generic transient test continues to observe about 60
seconds.

**Last updated:** 2026-10-15

### DL042 - Local-time compact analytics reads

CPAUK timeseries queries retain the requested IANA timezone for calendar bucket semantics and response metadata while reading UTC 15-minute materialized storage. Non-UTC queries regroup compact aggregate rows through the existing timezone bucketer and deduplicate request identities across 15-minute bins, preserving local-day, fixed-width, and DST bucket correctness. Queries with unaligned boundaries or retained-history overlap still use the existing exact paths. Calendar boundary resolution now handles zones that skip local midnight instead of producing zero-length day buckets.

**Implementation evidence:** `internal/cpauk/store/{materialized_read.go,query.go,materialized_15m_test.go}`, `internal/cpauk/aggregate/{range.go,aggregate_test.go}`.

**Recorded validation:** fixed-offset, unaligned rolling-range, New York DST, and skipped-midnight Havana/Azores compact-versus-raw and bucket-sequence parity tests pass; a 100k-request fixture comparison reports Asia/Bangkok 1-hour timeseries falling from 479.272 ms to 207.698 ms median (56.7% faster), with 2,160 points and exact raw-path parity. The hybrid unaligned-range check measures 217.700 ms after keeping only the edge minutes on the raw path. `go test ./internal/cpauk/...` and `go build -o cli-proxy-api ./cmd/server && unlink cli-proxy-api` pass. A full `go test ./...` run remains affected by the unrelated existing `internal/api/handlers/management/TestV0RepeatedSavesKeepLegacyClientAlias` failure.

**Last updated:** 2026-10-16

### DL043 - Shared one-minute analytics responses and conditional reads

First-page explicit and custom range cache keys floor both endpoints to UTC minutes. Seconds and fractional seconds no longer split server snapshots or CPAMC validators. Computation and `meta.range` retain the first request's exact bounds. Cursor continuation keys remain exact so signature validation and pagination stay tied to that snapshot. Adjacent minutes, timezones, and filters remain distinct, and cache entries still expire 60 seconds after computation.

Minute-identity regressions reproduce the old misses and cover all seven operations, GET revalidation and POST reuse, TTL expiry, sub-minute ranges, offset equivalence, and exact SQLite cursor continuation. A browser check changes explicit custom bounds within the same minute and observes `304`; crossing an endpoint minute observes `200`.

The v8 management analytics query operations share successful responses for 60 seconds after computation, bounded to 128 entries and 16 MiB. Cache identity includes the operation, original named or explicit range, timezone, normalized filters, key scope, bucket width, pagination, and sort. Concurrent matching requests share one calculation. Rolling presets reuse the cached response's resolved range during that minute; ordinary intake intentionally permits this bounded staleness. Configuration, pricing, service, and maintenance changes invalidate snapshots, and unavailable services cannot return cached results.

Authenticated `GET /v8/management/analytics/query` carries base64url JSON in `X-Analytics-Query`, keeping filters and full key IDs out of URLs. Responses include ETags; a matching `If-None-Match` returns an empty `304`. Existing POST requests share the cache and continue returning `200` bodies. Errors are not cached, and legacy v0 and public viewer routes retain their prior behavior.

CPAMC retains validators and response bodies in bounded, connection-scoped memory, reuses bodies after a `304`, and clears them on connection changes or sign-out. Large queries use POST, and older servers without the GET route retain POST compatibility. Completing a manual page refresh restarts the automatic refresh interval at 60 seconds; hidden-tab, concurrent-refresh, and Retry-After guards remain in place.

**Implementation evidence:** `internal/api/handlers/management/analytics_response_cache.go`, the v8 route and CORS registration, `internal/cpauk/service.go`, their response-cache tests, and the CPAMC analytics API and refresh coordinator.

**Recorded validation:** deterministic cache expiry, concurrent readers, query isolation, conditional GET, authentication, lifecycle invalidation, client session isolation, and manual-refresh scheduling tests pass, including focused Go race tests. A real CPAMC browser session against CPAUK confirmed initial `200` responses, manual refresh through `304`, suppression of the old automatic deadline, and fresh responses about 60 seconds after manual completion. Six concurrent 90-day Analysis requests against a 100k-request fixture completed in about 7.06 seconds; cached conditional reads took 2–3 ms in the local fixture server. The canonical management bundle was rebuilt from CPAMC `6110c3f`. The full-suite legacy management-config compatibility and CPAMC legacy-login diagnostic failures remain outside this change.

**Last updated:** 2026-10-16

### DL044 - Preserve unavailable provider metrics

CPAUK now calculates cache-read rates from the exact-token subset of a range, so estimated Kiro
requests do not hide cache metrics reported by other providers. CPAMC keeps estimated
input/output/total token counts visible while showing cache read, cache creation, cached, and
reasoning metrics as unavailable only when no usable exact subset exists. Exact providers retain
legitimate zero values. The same rule applies to overview sparklines, event cache-rate columns,
and event token details. Timeseries buckets carry the exact-subset cache rate too, so a mixed
bucket still renders a graph point instead of dropping the whole bucket. Regression coverage covers
raw, compact, and retained mixed-quality timeseries plus the CPAMC overview model; the CPAMC
submodule was rebuilt through the canonical management-center build.

**Last updated:** 2026-10-16

### DL045 - Reliable Claude Code reference capture

The private Claude Code reference updater repairs a missing activation pointer from a complete
installed version directory before attempting another network update. Older installations whose
version directory is directly under `claude-reference` are migrated into the current
`versions/<version>` layout. Reference update failures retry after five minutes.

Capture failures retain at most 16 KiB of probe output, redact the OAuth token, and preserve
bounded-memory behavior. Reference refresh tries every eligible direct Claude OAuth credential in
refresh order until one capture succeeds, while attempting identical access tokens only once.
Manual refresh and the background updater use the same fallback behavior.

**Implementation evidence:** `internal/claudecapture/{update.go,capture.go,private_test.go,capture_test.go}`,
`sdk/cliproxy/service_claude_reference.go`, and
`sdk/cliproxy/service_claude_reference_test.go`.

**Recorded validation:** pointer recovery, legacy-layout migration, bounded capture diagnostics,
ordered credential fallback, focused capture/executor/service suites, and the disposable server
build pass.

**Last updated:** 2026-10-16

### DL046 - Streaming telemetry and same-format Claude streams

CPA assembles multiline SSE `data:` fields before classifying token and usage events, keeps provider, TTFT, and first-token latency metrics non-decreasing, and forces `stream: true` for Claude streaming requests even when the client already uses Claude's native format. This preserves usage telemetry and stream framing without changing the selected response protocol.

**Implementation evidence:** `internal/runtime/executor/helps/generation_body.go`, `internal/runtime/executor/helps/usage_helpers.go`, and `internal/runtime/executor/claude_executor_stream.go`.

**Recorded validation:** focused executor, usage, stream-observation, and live OpenAI/Claude streaming checks pass.

**Last updated:** 2026-10-19

### DL047 - Claude safeguard capture failures fall back across providers

When the direct Claude OAuth safeguard cannot load a current Claude Code
capture, the selected Claude credential is now treated as unavailable for that
attempt. Mixed-provider routing can continue to a Kiro credential serving the
same Claude model, whose native runtime does not use the Claude OAuth
safeguard. Genuine safeguard rejections of the caller request remain
request-scoped and still stop processing without cooling credentials.

**Implementation evidence:** `internal/runtime/executor/claude_oauth_safeguard.go`,
`sdk/cliproxy/auth/conductor_cooldown.go`, and
`sdk/cliproxy/auth/conductor_claude_safeguard_routing_test.go`.

**Recorded validation:** focused safeguard tests and the mixed Claude/Kiro
routing regression pass. Production management inspection showed repeated
Claude reference-capture failures caused by the upstream organization policy,
while successful Claude-model requests were routed to Kiro.

### DL048 - Key catalog reports the top-cost model

CPAUK key catalogs add `top_cost_model` and `top_cost_model_usd` alongside the existing token-ranked
`top_model` / `top_model_tokens`. They come from the same per-key, per-model scan over raw events
and retained rollups, so the cost leader carries no extra query. Ties keep the token order. A key
with no known cost leaves `top_cost_model` null. CPAMC uses the field so the Keys catalog's Top
model column follows the selected Rank by metric (CPAMC DL063). The bundled client is updated to
CPAMC pin `1f2b4b0`.

**Implementation evidence:** `internal/cpauk/store/timing.go`, `internal/cpauk/store/key_catalog.go`,
`internal/cpauk/model/result.go`, and `TestKeyCatalogReportsTopCostModelSeparately`.

**Recorded validation:** `go test ./internal/cpauk/...` passes; a live local CPA with a priced
mock upstream returned `cheap-bulk` as the token leader and `pricey-small` as the cost leader for
the same key.

**Last updated:** 2026-10-22

### DL049 - Model leaderboard and privacy-preserving session digests

CPAUK accepts the v2 `model_leaderboard` analytics operation and returns four fixed awards in
one response: distinct sessions used, request reliability, P70 published throughput, and P95
generated tokens per successful attempt. Every award is global across keys, providers, and
credentials, contains at most six contiguous ranks, and returns an empty row array when no model
qualifies. Reliability requires at least 200 upstream attempts; speed requires a P70 of at least
80 tokens per second and excludes attempts without a published rate.

The collector hashes the trimmed upstream session identifier into a nullable `session_digest`
field and never persists the raw value. Migration 12 adds the column to existing databases while
leaving historic rows null. Throughput reuses the event read-time calculation and per-credential
median acknowledgement baseline. The canonical `normalized-v1` output total already includes
reasoning for every provider accounting style, so the talkative award adds reasoning only for
legacy or imported schemas that store separate output buckets. When a valid token breakdown
cannot supply its output total and the intake adapter falls back to the provider's raw output,
the event is stamped `raw-fallback-v1`; those attempts are excluded from most_talkative because
the raw output's relationship to reasoning is provider-dependent. The same events still count
toward the other awards.

**Implementation evidence:** `internal/cpauk/{collector,model,store}`, `internal/cpauk/service.go`,
and `internal/api/handlers/management/analytics.go`.

**Recorded validation:** sanitizer, query-contract, fresh-migration, v11-upgrade, empty-store,
partial-podium, reliability-floor, speed-floor, missing-TPS, tie-break, six-row cap, distinct
provider session, inclusive-reasoning, and raw-output-fallback tests pass. The disposable
`cmd/server` build passes.
The requested CPAUK, API, and usage suites are green except for the previously recorded
`TestV0RepeatedSavesKeepLegacyClientAlias` config compatibility failure; the management package
passes when that unrelated test is skipped.

**Last updated:** 2026-10-23

## Merge History

This is an append-only historical decision record. It provides context for integrations but never, by itself, establishes an ongoing fork divergence; use the current Divergence Log for that determination.

### 2026-08-31 - Merge upstream `main` at `81e1b537`

Merged upstream `main` into the fork without rebasing or rewriting the six published fork commits. The merge resolved upstream's server and configuration file splits by moving DL001 hooks into the new route, reload, management, middleware, and config-load files instead of restoring the pre-refactor monoliths. Direct OpenAI Realtime routes receive the limiter after authentication. DL002 files remain unchanged from `53866c01`.

The integration resolved content conflicts in `internal/api/server.go`, `internal/config/config.go`, and `internal/config/parse.go`. It kept upstream's refactored `server.go` and `config.go`, then reapplied the fork behavior in the split files listed under DL001.

### 2026-09-09 - Merge upstream `main` at `7fac6b15`

Integrated upstream's 132 commits since `81e1b537` with a no-fast-forward merge. The merge retains CPAUK analytics, per-key limits, management quota protection, hop diagnostics, and the CPAMC pin. Upstream's newer session hierarchy and stream metadata, Antigravity connection-pool handling, plugin-store release cache and GitHub rate-limit protection, health-probe logging, model metadata, Codex delegation compatibility, and provider protocol fixes are included.

The content conflicts were limited to `AGENTS.md`, the management handler, server reload and tests, Redis usage-queue imports, and the usage record context declarations. Fork analytics and usage-delivery fields remain alongside upstream session and stream fields; upstream's replacement plugin-release cache supersedes the earlier fork-local cache fields. Both the fork's `usagecontext` installation and upstream session normalization remain active in the Redis queue. The CPAMC gitlink includes its corresponding upstream synchronization to `ed5f1c4`, preserving the fork analytics interface and both event diagnostic corrections.

Validation: `go build -o test-output ./cmd/server && unlink test-output`, `go test ./...`, and `go vet ./...` pass. The full Go suite passes again after the observer metadata correction. Independent review reproduces bounded session metadata within the 16 KiB snapshot limit and preserved streaming context. CPAMC passes 708 tests, lint, TypeScript, production build, and isolated desktop/mobile Chrome CDP checks against the existing development server.

Follow-up correction: the bounded generic observer snapshot now accounts for
`Record.SessionID` and `Record.ParentSessionID`, bounds the corresponding
`ClientRequestMetadata` fields in the detached context, and preserves the
stream flag through observer delivery. Focused usage and usage-context
regressions cover the byte limits and stream propagation.

### 2026-10-07 - Merge upstream `main` at `57bde351`

Integrated upstream's changes after `7fac6b15` through `57bde35179ecbdca176ca8923d39cc18805774f` in a two-parent merge commit, keeping the fork's analytics, per-key limits, CPAMC integration, and runtime safeguards. The merge target is the current upstream tip verified by the orchestrator.

The v8 management tree is the canonical CPAMC surface. Upstream's v8 configuration layout and operational routes remain intact, while the fork's capabilities, complete analytics management group, revisioned `/api-keys`, and `/api-key-limits` routes are registered under `/v8/management`. Existing v0 management compatibility remains available without adding new legacy routes, and the public shared analytics viewer stays under `/v0/analytics/viewer`. Root `access.api-keys` entries retain labels and limits, root `analytics` settings remain in the v8 document, and `upstream.claude.header-defaults.oauth-safeguard` survives v8 reads and writes.

The four conflict resolutions keep both feature sets: the upstream v8 config template is combined with the fork's CPAMC URL, structured key and analytics examples, and Claude safeguard example; Codex executor tests retain the fork's plaintext collaboration coverage alongside upstream selected-home-model and compatibility cases; usage-helper tests retain both accounting behaviors; and Claude detection uses the upstream 2.1.280 baseline while restoring the configured-baseline acceptance half and the fork's entrypoint/helper coverage. Overlapping runtime fixes follow upstream. No conflict required dropping a user-facing feature. One adjacent stale multi-agent test fixture was updated after the merge exposed that upstream now requires a `spawn_agent` encrypted signature before removing collaboration encryption; the test now supplies that signature and still covers orphan delegation without credential selection.

Validation on 2026-10-07:

- `gofmt -w internal/runtime/executor/codex_executor_spawn_agent_test.go internal/runtime/executor/helps/claude_client_detection_test.go internal/runtime/executor/helps/usage_helpers_test.go` passed.
- The focused route/config tests passed: v8 example and fork-field round trips, v8 management access control, analytics management, revisioned API-key limits and labels, shared upstream settings, and the executor/helper packages.
- `go test ./sdk/api/handlers/openai` passed after the stale fixture correction.
- `go build -o test-output ./cmd/server && unlink test-output` passed.
- `go test ./...` was run to completion. It first reported the stale OpenAI fixture (corrected above) and then timed out in the pre-existing `sdk/cliproxy/auth` test `TestManager_ApplyProviderQuotaReportForModels_PreservesCredentialQuota`, blocked in `Manager.persist` on its read lock. A standalone `go test ./sdk/cliproxy/auth -run '^TestManager_ApplyProviderQuotaReportForModels_PreservesCredentialQuota$' -count=1 -timeout=45s` reproduces the timeout, so no unrelated auth implementation was changed.

### 2026-10-19 - Merge upstream `main` at `2422c6b0`

Integrated upstream's plugin-host and model-registration fencing, auth exclusion and snapshot fixes, Claude thinking, tool-search, stream, and error handling, stream truncation detection, TLS and request-body safeguards, provider updates for Vertex, Interactions, Grok, Gemini, Antigravity, Meta, and Kimi, and usage metadata improvements. The merge also includes upstream ETag/If-Match configuration concurrency, session-affinity and cooldown protections, plugin quota-provider identity, and long-line plugin telemetry handling.

The merge keeps the user's decisions: upstream's OAuth gallery and Kiro, plugin, Vertex, and cancel support; upstream configuration rows with the fork's configuration blocks; and the fork's keys and quota layout with upstream data. Keepalive uses upstream's `send-keepalive-before-ft` implementation with fork-compatible behavior when the flag is unset. The orchestrator combines GSR with session affinity, keeps both Claude stream observers, accepts upstream plugin line-limit behavior, removes CPAMC `testModel`, and drops CPAMC's configuration Quota section.

Notable resolutions retain structured API-key semantics with ETag optimistic concurrency, CPAUK viewer and usage-manager behavior, and the CPAMC pin and rebuilt bundle at `0da007e`. The fork-side nginx timeout commit `bae6e16f` remains the parent of the final merge. Multiline SSE assembly, non-decreasing latency metrics, and `stream: true` enforcement for same-format Claude streams remain in the merged runtime.

Validation: `go build` and `go vet` pass. `go test ./...` passes except the pre-existing `TestV0RepeatedSavesKeepLegacyClientAlias`, which also fails on main. In the race suite, `TestClaudeExecutorSharedCredentialMetadataMixedAccess` is a pre-existing flaky race that reproduces about 1 in 200 runs on fork main `bae6e16f` and on this merge, and `TestRequestLoggingMiddleware_StreamingPreservesFullUUIDForLogger` is also pre-existing. Live smoke against the Z.ai coding plan and Kiro covers OpenAI and Claude formats in streaming and non-streaming modes. CPAMC was checked through Chrome CDP at 1440px and 390px. An analytics database migration on a copy of a real database left all 50 events readable. A known pre-existing hot-reload quota issue remains: after an openai-compatibility key hot reload, quota fetch returns HTTP 501 `no quota provider available for credential` until restart, identically on main.
