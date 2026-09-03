# CLIProxyAPI Fork

This fork keeps per-API-key request and token usage limits, failure-isolated CPAUK analytics, a pinned CPAMC client, and reproducible web-console packaging that are not supplied by upstream.

## Divergence Log

This is a current-state record only. Each entry describes a surviving difference between `HEAD` and the current upstream base, `81e1b5374f99c212f196f34956eeed964a46b8fa`. The 2026-08-31 integration merged that upstream commit without rewriting the six published fork commits `2037ab99`, `04cfb113`, `5758371b`, `b67c5e31`, `45a589fb`, and `53866c01`. Their former shared base was `a14dfc779f43aed588e68b31fb34ab5ced700851`.

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

CPA includes the Type-Delta CPAMC fork as a required submodule at `web/management-center`. The initial gitlink pinned `d249ff008e0bc2803deb23fb3e2c62418a1e8d17`; the current gitlink pins CPAMC commit `fa25a73973306dfc9c8eb7daee70a5b05d00f04b`, which merges official CPAMC through `e0ee7123dfb5aa89a14ff73ac5a5c3bf4db658e0` and adds structured key management, the isolated Analytics workspace, complete visual analytics configuration, full `int64` storage precision, safe CRLF YAML normalization, Config-card spacing, routed icon tabs, URL-backed range and key filters, collision-safe key identities, consistent controls and Skeleton loading states, a persistent Analytics shell with in-page portal content, and the CPAUK-fidelity usage and management views.

The CPAMC checkout keeps Type-Delta as `origin` and the official repository as `upstream`. Its own `AGENTS.md` and `FORK.md` record the shared CPA, CPAUK, and CPAMC glossary, validation commands, current divergences, and append-only merge history.

**Implementation evidence:** `.gitmodules`, the `web/management-center` gitlink, and CPAMC commits `c1a2044`, `1f77aae`, `0cb4790`, `a7a342a`, `69e7ab6`, `8e67a42`, `9aeef9e`, `864f394`, `ad545f4`, `7088800`, and `fa25a73`.

**Recorded validation:** Bun verification passes 495 tests, ESLint, TypeScript compilation, and the Vite production build. Independent raw-CDP desktop and mobile journeys covered all eight Analytics routes in light and dark themes, including deep-link restore, URL state, rapid route recovery, viewer credential scrubbing, short-hash identity, responsive overflow, control naming, and runtime diagnostics. A seven-day `Asia/Kolkata` Analysis run verified both 168-bucket SVG charts at the start, midpoint, and end with exact 40 px interaction targets and effectively zero target-to-mark drift. Earlier clean-clone validation confirmed both remotes follow the documented convention.

**Last updated:** 2026-09-02

### DL005 - Failure-isolated embedded CPA Usage Keeper

CPA carries optional CPAUK analytics behind the `internal/cpauk` package boundary. The port adapts observable behavior from upstream CPA Usage Keeper v1.15.0 at commit `696a4659ce1d5d6f2d2d0530e3205eb51fbce889` without importing its GORM, CGO SQLite, HTTP server, authentication, or process-lifecycle architecture. Analytics are disabled by default and have independent startup, health, circuit, maintenance, and shutdown states. Bounded, lossy observer delivery runs only after synchronous per-key accounting; storage errors, panics, queue saturation, and invalid analytics configuration cannot change proxy readiness or request success.

The module sanitizes usage records before enqueue, persists full SHA-256 key identities in a separate pure-Go SQLite database, and retains a separate random identity key for credential pseudonyms and encrypted cursors. Checksummed migrations, quota reserves, verified backup and restore, corruption guards, hourly and daily rollups, monotonic retention checkpoints, resumable import and rollback, resumable raw-history repricing with an explicit retained-history cutoff, confirmed purge batches, repair jobs, and identity-epoch recovery are isolated from CPA's auth and limit state. Event detail is indexed; event APIs reject retained-away ranges rather than returning partial history. API schema v2 adds outcome and rate summaries, zero-filled activity and analysis series, full-range latency statistics with capped distributed scatter samples, once-per-event cost-component reconciliation, named range resolution, complete sanitized CSV/JSON exports, missing-price provenance, and credential-level provider/quota rows with request identity deduplication while retaining schema-v1 requests and fields. Current-period named ranges end at request time instead of including future buckets. Summary, time series, dimensions, event pages, pricing, provider quotas, key catalog, and token- or known-cost leaderboards share bounded multi-key query contracts and explicitly report unpriced tokens.

Versioned fixtures record events, token categories, deterministic credential identities, key display identities, ranges, pricing, limit windows, viewer isolation, maintenance envelopes, imports, reconciliation, and stable pagination. `test/perf` includes a production adapter covering HTTP, streaming, WebSocket, retries, blocked SQLite, and saturated queues. Its long median-of-five certification remains opt-in because it requires the documented dedicated-runner metadata and power controls.

**Implementation evidence:** `internal/cpauk`, `internal/cpauk/testdata/upstream-v1.15.0`, `sdk/cliproxy/usage/manager.go`, and `test/perf`.

**Recorded validation:** all CPAUK, schema-v2 contract, usage-delivery, API integration, deterministic load-adapter, retention/reopen, migration, backup/restore, import/rollback, repricing, DST and fractional-zone bucketing, latency, cost reconciliation, provider-quota, and failure-containment tests pass. The seeded UTC fixture resolves to 5,250 requests, 68,083,696 tokens, $295.62446097 known cost, 5,070 successes, and 180 failures. The required disposable server compile and ordinary full repository suite pass. The dedicated approximately 110-minute load certification remains an environment-specific release result and is not claimed by this checkout.

**Last updated:** 2026-09-03

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

## Merge History

This is an append-only historical decision record. It provides context for integrations but never, by itself, establishes an ongoing fork divergence; use the current Divergence Log for that determination.

### 2026-08-31 - Merge upstream `main` at `81e1b537`

Merged upstream `main` into the fork without rebasing or rewriting the six published fork commits. The merge resolved upstream's server and configuration file splits by moving DL001 hooks into the new route, reload, management, middleware, and config-load files instead of restoring the pre-refactor monoliths. Direct OpenAI Realtime routes receive the limiter after authentication. DL002 files remain unchanged from `53866c01`.

The integration resolved content conflicts in `internal/api/server.go`, `internal/config/config.go`, and `internal/config/parse.go`. It kept upstream's refactored `server.go` and `config.go`, then reapplied the fork behavior in the split files listed under DL001.
