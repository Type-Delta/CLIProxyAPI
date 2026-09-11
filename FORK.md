# CLIProxyAPI Fork

This fork keeps per-API-key request and token usage limits, failure-isolated CPAUK analytics, a pinned CPAMC client, and reproducible web-console packaging that are not supplied by upstream.

## Divergence Log

This is a current-state record only. Each entry describes a surviving difference between `HEAD` and the current upstream base, `7fac6b15bcfe5ea55c18c9eaec8e5b7e6457d974`. The 2026-09-09 integration merged 132 upstream commits after the 2026-08-31 baseline without rewriting the fork history. The prior integration had merged upstream through `81e1b5374f99c212f196f34956eeed964a46b8fa` without rewriting the six published fork commits `2037ab99`, `04cfb113`, `5758371b`, `b67c5e31`, `45a589fb`, and `53866c01`. Their former shared base was `a14dfc779f43aed588e68b31fb34ab5ced700851`.

Gate 0 ended at pushed commit `dae4267c70c835d323b00bfd9b2baaeb8386e92e`, where the fork was 10 commits ahead and zero behind the recorded upstream base. The implementation commits after that baseline add the CPAUK package, control plane, CPAMC analytics workspace, runtime fixes, and reproducible release packaging described below. Release validation must confirm zero missing upstream commits, an exact `HEAD`/`origin/main` match, and a CPAMC gitlink that resolves from its pushed `origin/main`.

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

`docker-compose-web.yml` runs CLIProxyAPI behind an Nginx service on port `8317`. Nginx serves the pinned Cli-Proxy-API-Management-Center artifact at `/` and proxies every other path to CLIProxyAPI, including streaming and WebSocket traffic. The public web port can be selected through `CLI_PROXY_WEB_PORT`; `CPA_WEB_NETWORK_SUBNET` selects the narrow bridge CIDR when the default overlaps another Docker network.

`Dockerfile.web` builds the initialized Management Center submodule with digest-pinned Bun 1.3.14, keys the UI version to the recorded CPAMC commit, then copies the single-file output into a digest-pinned Nginx image. It does not compare the generated HTML with the bundled artifact, so Git line-ending conversion cannot reject an otherwise valid web-image build. The deployment retains the existing CLIProxyAPI configuration, auth, log, plugin, and OAuth callback mounts and ports. An optional forwarding override trusts `X-Forwarded-Proto` only from an explicitly configured immediate TLS-proxy CIDR; the default configuration overwrites spoofed forwarding headers.

**Implementation evidence:** `docker-compose-web.yml`, `docker-compose-web-forwarded.yml`, `Dockerfile.web`, `nginx-web.conf`, `nginx-web-forwarded.conf.template`, and `docs/analytics-operations.md`.

**Recorded validation:** both Compose configurations render; both digest-pinned images build from the initialized submodule; and a live two-container run serves artifact SHA-256 `e6dcb11e8d76681d6ae678c5b48fcddf4e6fb0272fffbd441c3492d8945e1be6`, proxies health and authenticated analytics, preserves a durable viewer across restart, rejects spoofed forwarded HTTPS by default, and accepts verified forwarding only through the explicit override. A configurable bridge subnet was exercised after the default correctly reported a host-network overlap.

**Last updated:** 2026-08-31

### DL003 - Hermetic Codex Live media relay test

The Codex Live audio and data-channel bridge integration test creates every test peer on loopback. This keeps the test independent of host and container interface routing while leaving production ICE candidate selection unchanged.

**Implementation evidence:** `internal/client/codex/live/media_test.go`.

**Recorded validation:** `go test -count=3 -run TestPionMediaRelayBridgesAudioAndDataChannel ./internal/client/codex/live` passes on a runner where the unmodified test failed three consecutive times while every peer remained in the WebRTC `connecting` state.

**Last updated:** 2026-08-31

### DL004 - Pinned Type-Delta management client

CPA includes the Type-Delta CPAMC fork as a required submodule at `web/management-center`. The initial gitlink pinned `d249ff008e0bc2803deb23fb3e2c62418a1e8d17`; the current gitlink pins CPAMC commit `068724f69e4a1cb45acdedf9f0bb667c25769cc1`, which merges official CPAMC through `e0ee7123dfb5aa89a14ff73ac5a5c3bf4db658e0` and adds structured key management, the isolated Analytics workspace, complete visual analytics configuration, full `int64` storage precision, safe CRLF YAML normalization, Config-card spacing, routed icon tabs, URL-backed range and key filters, collision-safe key identities, consistent controls and Skeleton loading states, a persistent Analytics shell with in-page portal content, and the CPAUK-fidelity usage and management views.

The CPAMC checkout keeps Type-Delta as `origin` and the official repository as `upstream`. Its own `AGENTS.md` and `FORK.md` record the shared CPA, CPAUK, and CPAMC glossary, validation commands, current divergences, and append-only merge history.

**Implementation evidence:** `.gitmodules`, the `web/management-center` gitlink, and CPAMC commits `c1a2044`, `1f77aae`, `0cb4790`, `a7a342a`, `69e7ab6`, `8e67a42`, `9aeef9e`, `864f394`, `ad545f4`, `7088800`, and `fa25a73`.

**Recorded validation:** Bun verification passes 495 tests, ESLint, TypeScript compilation, and the Vite production build. Independent raw-CDP desktop and mobile journeys covered all eight Analytics routes in light and dark themes, including deep-link restore, URL state, rapid route recovery, viewer credential scrubbing, short-hash identity, responsive overflow, control naming, and runtime diagnostics. A seven-day `Asia/Kolkata` Analysis run verified both 168-bucket SVG charts at the start, midpoint, and end with exact 40 px interaction targets and effectively zero target-to-mark drift. Earlier clean-clone validation confirmed both remotes follow the documented convention.

The analytics upgrade adds the animated diamond cost radar, shared p95/max/median timing radar and
scatter controls, token/cost/generation heatmap modes, cost component columns, and line-shaped
chart tooltip markers. Quick Stats includes accumulated processing time and explained comparisons.
The key catalog uses client-local lifetime activity dates, recent Active/Idle status, top-model tokens,
generation duration, requests, and header help. Visible analytics refresh every minute without
replacing charts or selections. Vite accepts all development hosts for the requested remote mock
workspace. Provider timings and historical generation remain unavailable where measurements are
absent; CPAMC does not estimate clock offsets from unrelated online UTC services.

Validation for this pin: `bun run verify` passed 673 tests, ESLint, TypeScript, and production build.
Isolated Chrome CDP at 1440px and 390px checked light/dark cost gradients, clockwise synchronized
numbers, radar/heatmap modes, missing timing, numeric header alignment, dashed/solid tooltip markers,
comparison hover and first-tap behavior, client timezone dates, and a full automatic refresh cycle.
The cycle retained chart instances and controls; no page overflow or console errors were observed.
A browser-only fixture verified complete timing axes without changing stored mock data.

The refinement shares Auth Files' segmented control with analytics and moves sortable headers into
the common Table component. Custom tooltip panels replace native title hints across CPAMC and
preserve dynamic content, accessible names, focus, and touch behavior. Key catalog header styling
is corrected. Quick Stats uses full-width trends and places Processing time before the two-column
Daily average card. Cost summaries sit beside the radar when space permits; paired cards share
height. All model-cost columns sort, latency metrics explain their measurements, radar axis delays
are more visible, and refresh spinners no longer dim existing content. Per-comparison assumptions
replace the standalone comparison section. The user's existing spacing changes are included.

Additional browser verification covered 1024-pixel layouts, dynamic and touch tooltips, equal card
heights, all added cost sorts, and empty-to-populated latency animation. The CPA compile check passes.

**Last updated:** 2026-09-08

### DL005 - Failure-isolated embedded CPA Usage Keeper

CPA carries optional CPAUK analytics behind the `internal/cpauk` package boundary. The port adapts observable behavior from upstream CPA Usage Keeper v1.15.0 at commit `696a4659ce1d5d6f2d2d0530e3205eb51fbce889` without importing its GORM, CGO SQLite, HTTP server, authentication, or process-lifecycle architecture. Analytics are disabled by default and have independent startup, health, circuit, maintenance, and shutdown states. Bounded, lossy observer delivery runs only after synchronous per-key accounting; storage errors, panics, queue saturation, and invalid analytics configuration cannot change proxy readiness or request success.

The module sanitizes usage records before enqueue, persists full SHA-256 key identities in a separate pure-Go SQLite database, and retains a separate random identity key for credential pseudonyms and encrypted cursors. Checksummed migrations, quota reserves, verified backup and restore, corruption guards, hourly and daily rollups, monotonic retention checkpoints, resumable import and rollback, resumable raw-history repricing with an explicit retained-history cutoff, confirmed purge batches, repair jobs, and identity-epoch recovery are isolated from CPA's auth and limit state. Event detail is indexed; event APIs reject retained-away ranges rather than returning partial history. API schema v2 adds outcome and rate summaries, zero-filled activity and analysis series, full-range latency statistics with capped distributed scatter samples, once-per-event cost-component reconciliation, named range resolution, complete sanitized CSV/JSON exports, missing-price provenance, and credential-level provider/quota rows with request identity deduplication while retaining schema-v1 requests and fields. Current-period named ranges end at request time instead of including future buckets. Summary, time series, dimensions, event pages, pricing, provider quotas, key catalog, and token- or known-cost leaderboards share bounded multi-key query contracts and explicitly report unpriced tokens.

Pricing discovery uses models.dev with a durable six-hour cache. Usage activity and pricing-page reads request refreshes on demand; startup and idle time do not schedule catalog downloads. Concurrent demand shares a refresh, and failures retain the last successful catalog with a 15-minute retry delay. Manual model and alias overrides are stored separately and take precedence over discovered prices. Catalog matching uses explicit provider mappings and exact model IDs. CPAMC presents these as API-equivalent cost estimates, including for subscription-backed OAuth accounts; baseline token rates do not account for every context tier, service tier, or non-token charge. Historical events retain their stored cost until an explicit repricing job runs. Tests cover TTL expiry, concurrent refreshes, persistence, stale fallback, decimal parsing, and prices applied to newly ingested events. Live models.dev and browser checks cover catalog loading and override creation/removal.

The CPAUK intake adapter now persists the canonical input total from the SDK token breakdown. This keeps Anthropic and Claude cache-read buckets independent from uncached input when calculating cache-read rates and pricing, while preserving OpenAI-style input totals where cached tokens are already included. Existing event and rollup rows retain their stored token buckets; the current repricing operation updates price fields only, so repairing historical token totals requires an explicit migration with provider-scoped review.

CPAUK also persists an optional observed generation duration: the local monotonic interval between first and last substantive streaming token arrivals. Chat error envelopes, `[DONE]` markers, Claude signature-only deltas, and other metadata or terminal-only frames do not extend it. Native HTTP streams now capture generation timestamps in a bounded read-ahead observer before downstream forwarding; Codex WebSocket frames carry their socket-read timestamps through a byte-budgeted queue. Queue saturation invalidates generation timing instead of reporting a compressed interval, preserving a first-token observation already captured before saturation. Historical records and responses without observed token boundaries remain unavailable. This measures arrival timing at CPA, not exact provider-side execution. Analytics expose full-population p95, maximum, median and accumulated duration coverage, including ranges beyond 30 days; retained-away timing observations are reported as partial. E2E names the existing executor duration from attempt setup through completion; the local TTFT and token-arrival boundaries differ from client-observed wall time. This follows [OpenRouter’s total-latency decomposition](https://openrouter.ai/docs/guides/best-practices/latency-and-performance.md) into time to first token (network, queue and prefill) plus generation, without claiming identical measurement boundaries. CPAUK also stores locally observed dispatch-to-first-substantive-token and dispatch-to-response durations as nullable `first_token_latency_ms` and `provider_latency_ms`. HTTP headers and request-specific Codex/xAI WebSocket response frames establish the response endpoint. These measurements include network and provider wait time; they do not claim provider-reported acceptance timing. Native OpenAI-compatible, Claude, Gemini/Vertex, Antigravity, xAI, and Kimi streaming loops invoke their protocol-specific substantive-token observers alongside the existing Codex and plugin paths. Buffered nonstream parsing does not fabricate token arrival times. Historical and unsupported observations remain null. Analysis and processing-time totals expose measured values, coverage, and percentiles; retries restart the local timer. Existing TTFT retains its first-packet fallback, while first-token latency requires substantive token evidence. Provider `created_at` timestamps are never subtracted from CPA UTC: an unrelated online clock cannot calibrate a provider clock or remove timestamp quantization. Additive model cost components preserve reconciled stored totals, and key catalogs expose distinct requests, selected-range top models and observed generation totals alongside existing lifetime activity.

Versioned fixtures record events, token categories, deterministic credential identities, key display identities, ranges, pricing, limit windows, viewer isolation, maintenance envelopes, imports, reconciliation, and stable pagination. `test/perf` includes a production adapter covering HTTP, streaming, WebSocket, retries, blocked SQLite, and saturated queues. Its long median-of-five certification remains opt-in because it requires the documented dedicated-runner metadata and power controls.

A 2026-09-08 metric audit produced eleven corrections. The intake adapter now persists the canonical inclusive output total (Gemini candidate plus thought tokens) alongside the input total, so reasoning is priced. `TokenUsage.UnclassifiedTokens()` derives the remainder of the authoritative total that no bucket explains; a matched price rule reports that remainder as unpriced instead of claiming full coverage, and repricing inherits the rule. Analysis model rows, matrix cells, and activity buckets expose `unpriced_tokens`, and CPAMC uses it to qualify model efficiency as unknown or partial rather than ranking it as $0/M; token charts and the distribution add an explicit unclassified category so stacks sum to the total, and event speed uses the recorded generation interval, labels the TTFT fallback as an estimate, and shows unavailable when neither exists. Migration 005 adds `known_cost_nano` and `unpriced_tokens` to `daily_stats` so year activity keeps cost after retention; year activity deduplicates request identities across the raw/retained boundary; the public `success` filter maps to the `succeeded` column; retained credentials keep their auth type and actual last-observed timestamp instead of a bucket end; upstream v1.15 imports no longer force `exact` quality, so all-zero usage is inferred as `missing`; and the collector counts batches discarded at shutdown as abandoned so accepted equals written plus dropped plus abandoned. Existing stored event and rollup token buckets are unchanged; historical repair still requires the explicit provider-scoped migration noted above.

**Implementation evidence:** `internal/cpauk`, `internal/cpauk/collector/adapter.go`, `internal/cpauk/collector/sanitizer_test.go`, `internal/cpauk/testdata/upstream-v1.15.0`, `sdk/cliproxy/usage/manager.go`, and `test/perf`.

**Recorded validation:** the canonical Anthropic adapter regression and OpenAI compatibility tests pass, along with the existing CPAUK, schema-v2 contract, usage-delivery, API integration, deterministic load-adapter, retention/reopen, migration, backup/restore, import/rollback, repricing, DST and fractional-zone bucketing, latency, cost reconciliation, provider-quota, and failure-containment tests. The seeded UTC fixture resolves to 5,250 requests, 68,083,696 tokens, $295.62446097 known cost, 5,070 successes, and 180 failures. The required disposable server compile and ordinary full repository suite pass. The dedicated approximately 110-minute load certification remains an environment-specific release result and is not claimed by this checkout.

**Last updated:** 2026-09-08

### DL006 - Reproducible embedded CPAMC artifact

CPA builds CPAMC from the pinned `web/management-center` checkout with a digest-pinned Bun 1.3.14 container and embeds the resulting single-file application in the Go binary. The separate canonical builder removes host-dependent Rolldown output, keys the UI version to the pinned CPAMC commit, and uses a persistent package cache for offline rebuilds. The main image verifies its panel build byte-for-byte against that canonical artifact. The Nginx web image builds the same pinned source without the byte comparison so checkout line endings do not block the image build. Release archives carry the canonical HTML, compatibility manifest, and CPAUK/CPAMC license notices. No product build clones or floats on a remote CPAMC branch.

Mutable and downloaded panels require an adjacent `management-artifact.json` whose Management API range accepts this CPA build and whose SHA-256 matches the exact HTML bytes. An invalid, missing, incompatible, or tampered mutable panel falls back to the immutable bundled artifact. The former undigested network fallback is removed, and updates default to the Type-Delta CPAMC fork.

**Implementation evidence:** `internal/managementasset/bundle.go`, `internal/managementasset/bundled`, `scripts/build-management-center.sh`, `scripts/verify-submodules.sh`, `Dockerfile.management`, both runtime Dockerfiles, `nginx-web.conf`, release workflows, `DEPENDENCY_NOTICES.md`, and `docs/management-center.md`.

**Recorded validation:** the exact Bun 1.3.14 canonical builder produces SHA-256 `f92e08023b2988fc09ec860900371c04bca4186e81a2e7fea5f1a8009dd2f38f` from CPAMC `fa25a73973306dfc9c8eb7daee70a5b05d00f04b`. Earlier main-image, cached `--network=none`, Nginx image, mutable artifact, bundled fallback, workflow, and cross-build checks passed for the preceding implementation pin. CI remains authoritative for native CGO and plugin builds.

**Last updated:** 2026-09-02

### DL007 - Analytics management and shared-view control plane

CPA exposes capability-gated analytics management routes for health, summary, time series, activity, independently partial analysis, dimensions, events, indexed event detail, pricing, resumable repricing, credential-level provider quotas, key catalog, stable leaderboards, bounded complete CSV/JSON exports, backup, restore, import, rollback, purge, repair, maintenance jobs, and shared viewers. Named and explicit ranges resolve to echoed UTC bounds and an IANA zone. Key identity filters are accepted only in bounded JSON bodies; every analytics route rejects key IDs in URLs, and request logging masks accidental `key_id` and `key_ids` query parameters before recording them. Expensive admin reads and maintenance starts have independent bounded peer rate limits.

Shared viewers are stored in a separate bounded, atomic JSON document containing only credential and session hashes plus fixed key scope. The file and its parent-directory rename are synchronized before success. An administrator receives a viewer credential once, can list durable non-secret viewer metadata after a restart, and can selectively revoke the viewer and all its sessions. Viewer exchange requires direct HTTPS or verified forwarded HTTPS from a configured immediate proxy, issues a short-lived `HttpOnly`, `Secure`, `SameSite=Strict` cookie, and removes credentials from request logging. Viewer routes have a fixed CORS allowlist, cannot accept a management credential, and cannot select another key ID. Configuration reload invalidates active sessions and reports restart-required trust changes without taking proxying down.

Plain HTTP development remains available only through the explicit loopback option. The protocol multiplexer no longer advertises an empty TLS connection state on ordinary buffered TCP connections, so same-origin loopback requests with ports are classified as HTTP. After protocol sniffing, TLS-backed HTTP retains the real connection state, including for clients that omit ALPN, while Redis RESP over the same no-ALPN TLS listener keeps its existing route.

CPA normalizes CRLF and lone-CR YAML input before comment-preserving writes. This prevents carriage returns retained in parsed comments from becoming extra blank lines when startup hashes a plaintext management secret or another nested scalar changes; the resulting file uses LF line endings.

**Implementation evidence:** `internal/api/analytics_*`, `internal/api/handlers/management/analytics*`, `internal/api/server*.go`, `internal/api/middleware/request_logging.go`, `internal/config/{analytics.go,config_yaml.go}`, `internal/util/provider.go`, and `config.example.yaml`.

**Recorded validation:** management/viewer authorization matrices, schema-v2 management handlers, named-range resolution, event filters and complete exports, stable pagination, pricing provenance and repricing, provider credential rows, production-mux loopback CORS, TLS with and without ALPN, trusted-proxy cases, logging, reload recovery, viewer persistence and revocation, URL identity rejection, throttling, maintenance API tests, and the CRLF nested-scalar regression pass. Raw CDP verifies the management journeys against a live seeded server.

**Last updated:** 2026-09-03

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
development case.

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

**Last updated:** 2026-09-03

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
credential share one upstream call, and completed results are reused for one
minute across management clients. This protection runs before OAuth token
resolution so repeated refreshes also share credential acquisition work.

An upstream HTTP 429 blocks further quota requests for the same credential and
provider until `Retry-After` expires. Both delta-seconds and HTTP-date values are
supported; missing or invalid values use a three-minute backoff. The backoff
covers sibling quota endpoints and provider fallback hosts, including 429s
received during token refresh or before an interrupted response body. Other
credentials remain independent. A 429 uses its retry deadline instead of the ordinary
one-minute result lifetime.

Cached provider Date headers advance by cache residence time so CPAMC retains
the provider clock offset used by quota countdowns.

The existing management response envelope is preserved. Generic API calls and
quota-reset mutations are not cached. The known xAI paid-account health probe is
covered, while ordinary chat completions are not. Successful Codex reset-credit
consumption invalidates cached quota results without lifting provider backoff.
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

**Last updated:** 2026-09-06

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

**Last updated:** 2026-09-09

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

Validation: `go build`, `go vet`, and `go test ./...` pass, including
deterministic tests for binding refetch after an in-flight reconfigure, the
Z.ai payload parser, and display-name precedence. A local CPA with a real Z.ai
coding-plan key priced a `glm-4.7` request from the bound catalog, refetched
the catalog after the binding changed through the management API, and served
live five-hour and weekly windows. The bundled panel was rebuilt from CPAMC
commit `b07189943449a9c0002dd153caf4e7dd0b231007` (CPAMC DL043).

**Last updated:** 2026-09-11

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

### Generation observation and downstream backpressure

Codex observes upstream SSE even for nonstreaming callers. Populated terminal output can establish first-visible-output latency but cannot invent a generation duration. Native HTTP streaming executors observe decoded upstream data independently of downstream forwarding with a bounded queue; Codex WebSockets preserve socket-read timestamps. Saturation marks generation duration unavailable for that attempt, preventing blocked downstream consumers from producing artificially high measured TPS. No duration or TPS cutoff rejects legitimate fast generation. Existing plugin callbacks remain limited to the arrival boundary exposed by the plugin. CPAMC shares the event throughput calculation between table and detail and marks the existing TTFT-based fallback `EST` in the table. Historical events are unchanged.
