# CLIProxyAPI Fork

<!-- In-synced fork: maintained with the `syncfork` skill. -->

This fork keeps per-API-key request and token usage limits, failure-isolated CPAUK analytics, an optional ClickHouse server mode with searchable request transcripts and agent SQL, a pinned CPAMC client, and reproducible web-console packaging that are not supplied by upstream.

## Divergence Log

Each behavior where this fork differs from upstream has one entry here. If you change code in this repository, check if this log needs an update.

The rules:

- Update the entry that owns the behavior you changed, or add an entry with the next unused ID. One entry covers one behavior, not one commit.
- Describe the current behavior, why the fork needs it, and how it composes with upstream's related behavior. When the behavior changes, rewrite the entry. Do not append a changelog; git and the Merge History hold history.
- Under **Implementation evidence**, list the key source files and tests. Under **Recorded validation**, list the checks that proved the current behavior.
- Set **Last updated** to today's date on every entry you touch.
- Never reuse an ID. Retire an entry only with the user's explicit approval: delete it and name the retired ID in the next Merge History entry.
- When an ID conflicts from merges, renumber the IDs as needed.
- A change that does not alter how the fork differs from upstream (formatting, or an edit to this file) needs no entry.

### DL001 — Per-API-key request and token usage limits

Inbound client API keys may be configured as structured entries with optional request and token caps while bare-string keys remain compatible and unlimited. A key can use a lifetime window or a UTC hourly, daily, weekly, or monthly reset cadence; request and token limits share that window.

The proxy enforces limits across the OpenAI-, Anthropic-, and Gemini-compatible API surfaces, including the direct OpenAI Realtime routes. The `/v1`, `/openai/v1`, and `/backend-api/codex` groups use the OpenAI HTTP `429` envelope, while `/v1beta` uses the Gemini envelope; all include rate-limit headers. Model discovery, including Gemini model-detail GETs, token counting, and asynchronous video status or content routes do not consume quota. Gemini `countTokens` is exempt; `generateContent` and `streamGenerateContent` remain charged. Requests count when admitted, while tokens are recorded from normalized response usage after completion.

Usage counters persist under the authentication directory at `state/usage-limits.json`. Persisted entries identify keys only by SHA-256 hash, survive restarts, and remain local to each proxy instance. Configuration hot reload applies limit changes immediately, preserves counters unless the reset cadence changes, and removes counters for deleted or unlimited keys.

The authenticated management API exposes current limited-key consumption through the canonical v8 routes `GET /v8/management/api-key-limits` and `POST /v8/management/api-key-limits/reset`; the existing `/v0/management/api-key-limits` routes remain compatible for older clients. A reset for a configured key that has no recorded usage succeeds as a no-op, and only an unconfigured key returns `404`.

`PATCH /v8/management/api-keys` creates, rotates, and re-limits one key in a single call using the configuration revision; `PUT` and `DELETE` use the same revisioned contract. The existing `/v0/management/api-keys` payloads remain available for compatibility. The v8 endpoint resolves the target from an in-range `index`, then from `old` or `match`, and otherwise appends a new entry, so a body that carries only `new` creates a key instead of failing. Its `limits` field distinguishes three states: omitted preserves the existing limits, an explicit `null` clears them, and an object replaces them as a whole. An object that resolves to no caps is stored as no limits, which keeps the key a bare string in `config.yaml`; a reset cadence without a cap counts as no caps, because a cadence alone enforces nothing and the usage tracker ignores it. An out-of-range index, a blank key on create, a `limits` value that is neither an object nor `null`, and an invalid cap or cadence return `400` with a descriptive message and leave the configuration unchanged. Key selection through `old` or `match` compares trimmed key strings. The v8 configuration document also returns an `ETag`, and writes accept an optional `If-Match` against the on-disk hash, adding stale-write protection without replacing the fork's revisioned key contract.

Key management now reports a deterministic configuration revision and each row's full SHA-256 key ID. Revisioned clients mutate by configuration index plus the expected revision, so duplicate raw keys remain independently editable and stale writes return `409`. Full-YAML writes from older panels that would flatten structured limits or unknown key fields are rejected rather than silently losing them. Authorized administrators can still retrieve raw keys, but management clients are expected to conceal them until an explicit per-row reveal; analytics and logs use only key IDs. Weak-key warnings identify short or predictable configured secrets by config index, label, and short key ID without logging their values.

The TUI API Keys tab accepts mixed bare and structured key representations and displays each limited key's request and token usage, reset cadence, and reset time, including keys that have configured limits but no recorded usage. Adding and editing a key use a four-field form covering the key, the request cap, the token cap in millions, and a reset-cadence selector; limits stay optional, and blank caps with a cadence of never clear the limits, while a cadence without a cap is rejected in the form. Rows carry their original position in the configured list, so editing or deleting a row targets that entry even when the list contains blank entries. The tab keeps keys masked and keeps the confirmed per-key usage reset action, which is now available for every key with configured limits.

Credential listing, direct parsing, mirrored-store scans, and watcher scans and events exclude `<auth-dir>/state` and reserved `kv.db`, `kv.db.*`, and `kv.db-*` files. Upstream's default local KV database and its sidecars therefore cannot become credentials or routing candidates, even if their contents resemble credential JSON.

The TUI uses canonical v8 transport with the fork's revision/index key contract, rather than replacing the list as strings. An open form captures its row index and revision, so a background refresh cannot retarget an edit or deletion. Labels, structured limits and unknown limit fields survive key rotation and label edits. `internal/tui/client_revision_merge_test.go` exercises these journeys through actual Gin handlers over HTTP. The new authenticated tunnel adapts entries through `APIKeyStrings()` and remains outside inference quota charging.

The `/muse-code` group shares request admission: model/config discovery GETs are exempt, while search and browser actions consume a request. Relay actions carry the native client-key ID for reserve bypass and do not invent token usage.

**Implementation evidence:** `internal/api/server_routes.go`. `internal/config/{api_key_entry.go,config_load.go,config_v8.go,parse.go,sdk_config.go}`, `internal/usagelimit/`, `internal/api/middleware/usage_limit.go`, `internal/api/usage_limit.go`, `internal/api/{server.go,server_routes.go,server_reload.go,server_management.go,server_middleware.go}`, `internal/api/handlers/management/{api_key_limits.go,config_lists.go,config_v8.go,handler.go}`, `internal/access/config_access/provider.go`, `internal/tui/{client.go,keys_tab.go,i18n.go}`, `cmd/server/main.go`, `config.example.yaml`, and `docs/sdk-access.md`. `internal/api/middleware/usage_limit_test.go` covers Gemini discovery, token counting, and generation; `sdk/auth/{filestore.go,filestore_test.go}` cover `FileTokenStore.List`, `TestFileTokenStoreListSkipsForkStateDirectory`, and `TestFileTokenStoreReadSkipsKVState`. `sdk/cliproxy/auth/store.go` defines the shared exclusion used by `internal/store/{gitstore.go,objectstore.go}` and `internal/watcher/{events.go,clients.go,synthesizer/file.go}`; `internal/watcher/synthesizer/runtime_state_test.go` covers scans and direct synthesis.

**Recorded validation:** On 2026-11-18, `internal/api/server_routes.go` attaches admission to inference, Realtime and new Muse actions; discovery stays exempt, trusted token accounting and state/KV exclusions remain; usage-limit, structured-key/TUI HTTP, runtime-state and Muse limit tests pass.

**Last updated:** 2026-11-18

### DL002 — Two-service web-console Docker deployment

`docker-compose-web.yml` runs CLIProxyAPI behind an Nginx service on port `8317`. Nginx serves the pinned Cli-Proxy-API-Management-Center artifact at `/` and proxies every other path to CLIProxyAPI, including streaming and WebSocket traffic. The public web port can be selected through `CLI_PROXY_WEB_PORT`; `CPA_WEB_NETWORK_SUBNET` selects the narrow bridge CIDR when the default overlaps another Docker network.

`Dockerfile.web` compiles the panel source supplied in the Docker build context with pinned Bun 1.4.3, generates a compatibility manifest using the supplied `CPAMC_COMMIT`, and copies both into a digest-pinned Nginx image. `Dockerfile` uses the same source-build contract for the embedded panel. CI verifies clean submodule pins before building; local Docker builds do not enforce the canonical builder's clean-tree check, so the manifest commit alone does not prove that the source context was clean. The deployment retains the existing CLIProxyAPI configuration, auth, log, plugin, and OAuth callback mounts and ports. An optional forwarding override trusts `X-Forwarded-Proto` only from an explicitly configured immediate TLS-proxy CIDR; the default configuration overwrites spoofed forwarding headers.

Both Compose services pass the exported `CPAMC_COMMIT` to their Docker builds, matching the pinned-panel build workflow.

The fork's Docker publishing workflow builds CPA and the Nginx panel in four parallel jobs, one per image on native amd64 and arm64 runners, for main pushes, version tags, and manual runs. It uses the configured Docker Hub repository and publishes matching full-commit image tags after all four jobs succeed. The CPA Dockerfile compiles with BuildKit cache mounts for the Go module and build caches, and compiles packages before the panel lands so Go and Bun build in parallel. CI saves the Go build cache as one tarball in the Actions cache at most once per hour and feeds it back through the optional `go-cache-seed` build context, which local builds leave empty. The Actions layer cache keeps only final-image layers. `docker-compose-web-published.yml` requires explicit image references and removes both build definitions for production deployments. Production pulls both images before recreating services, retaining the existing storage and configuration. `docker-compose.clickhouse-web.yml` connects the optional private ClickHouse service to the web stack's network without exposing database ports.

**Implementation evidence:** `.github/workflows/docker-image.yml`, `Dockerfile`, `docker-compose-web.yml`, `docker-compose-web-published.yml`, `docker-compose.clickhouse-web.yml`, `docker-compose-web-forwarded.yml`, `Dockerfile.web`, `nginx-web.conf`, `nginx-web-forwarded.conf.template`, `docs/analytics-operations.md`, and `docs/management-center.md`.

**Recorded validation:** Read-only Compose assertions confirm that both source builds receive `CPAMC_COMMIT` and the published-image override removes both build definitions. Shell syntax checks pass for the canonical builder and submodule verifier. Dockerfile inspection confirms Bun 1.4.3 source builds and generated manifests. Fresh image builds and deployed-panel checks remain pending.

**Last updated:** 2026-11-18

### DL003 — Hermetic Codex Live media relay test

The shared Codex Live media fixture includes loopback candidates and filters out non-loopback addresses for client and upstream peers. The audio and data-channel bridge test also restricts its relay API and asserts that client and relay offers contain candidates and only loopback addresses. This keeps tests independent of host and container interface routing while leaving production ICE candidate selection unchanged.

**Implementation evidence:** `internal/client/codex/live/media_test.go`. `newTestWebRTCAPI`, `completeOffer`, and `TestPionMediaRelayBridgesAudioAndDataChannel` share the fixture.

**Recorded validation:** `go test -count=3 -run TestPionMediaRelayBridgesAudioAndDataChannel ./internal/client/codex/live` passes on a runner where the unmodified test failed three consecutive times while every peer remained in the WebRTC `connecting` state. The updated bridge passes with `-count=3`, and the full live-media package passes with `-count=1`. Restoring default peer settings through a Go overlay fails the new offer assertion.

**Last updated:** 2026-10-31

### DL004 — Pinned Type-Delta management client

CPA includes the Type-Delta CPAMC fork as a required submodule at `web/management-center`. The fork adds structured key management, the isolated Analytics workspace, complete visual analytics configuration, full `int64` storage precision, safe CRLF YAML normalization, Config-card spacing, routed icon tabs, URL-backed range and key filters, collision-safe key identities, consistent controls and Skeleton loading states, a persistent Analytics shell with in-page portal content, CPAUK-fidelity usage and management views, and the Claude OAuth capture safeguard control. The submodule gitlink records the client revision included by each CPA build.

The CPAMC checkout keeps Type-Delta as `origin` and the official repository as `upstream`. Its own `AGENTS.md` and `FORK.md` record the shared CPA, CPAUK, and CPAMC glossary, validation commands, current divergences, and append-only merge history.

Usage's Top Models card uses 100% stacked areas for Tokens and Price, with actual amounts in
tooltips, and overlapping, unstacked duration lines for Output window. Model colors and ranking
highlighting persist across modes; the chart and ranking fit narrow screens after viewport changes.
All three Top Models modes use straight segments with smoothing explicitly disabled.

Event detail sheets finish one close animation across parent re-renders and delayed URL updates.
The client prefers `/v8/management/analytics/attempts/:attempt_id`, an equivalent detail route
whose path avoids privacy-list matches against `/analytics/event`. Older servers retain a
missing-route fallback to the original event URL. CPAMC DL080 records the reproduction and checks.

The coordinated panel sync adopts InfoTips, Auth Files display modes, the Model Rules workspace, combined Plugins views and replacement editors while retaining fork quota rows, scoped sessions and Kiro/plugin OAuth. The 2026-11-18 Merge History entry records the pin and its paired-browser validation.

**Implementation evidence:** `.gitmodules`. `.gitmodules`, the `web/management-center` gitlink, and the CPAMC `AGENTS.md` and `FORK.md`.

**Recorded validation:** CPAMC verification, lint, TypeScript compilation, the Vite production build, and independent desktop/mobile browser journeys cover the Analytics routes, configuration contracts, viewer isolation, responsive layout, and runtime diagnostics. Top Models verification includes 2,138 passing frontend tests, all three modes with synthetic analytics responses on desktop/mobile, the canonical Docker panel build, and a compiled-server smoke check serving the bundled `/management.html` with HTTP 200. Detailed CPAMC validation remains in the submodule's `FORK.md`.

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

Usage cost comparisons split the 48,000 USD to 1 million USD range into Ford Mustang GT,
Porsche 911 GT3 RS, and Lamborghini Aventador SVJ bands, using illustrative benchmarks of
48,000, 250,000, and 520,000 USD. Explanations in all six locales disclose the model years
and US MSRP basis, including the Aventador's historical launch price.
The 1,000 USD to 48,000 USD range uses iPhone 17 Pro 256GB, Apple Vision Pro,
Rolex Submariner, NVIDIA RTX PRO 6000 Blackwell, and Tesla Model 3 RWD comparisons at
1,000, 4,000, 10,000, 17,000, and 37,000 USD, with dated price assumptions in all six locales.
The 200 USD to 1,000 USD band uses LEGO Back to the Future DeLorean sets at a rounded
200 USD per set, replacing months of an AI subscription.
The 1 million USD to 25 million USD range uses Gravity Jet Suits at 500,000 USD each,
Princess F55 yachts at 2.5 million USD each, and an illustrative 15 million USD F1-car build
estimate. The F1 tooltip includes a partial historical component-cost breakdown, with
research, development, and season operations excluded.
Super Bowl airtime covers 25 million to 30 million USD. The following bands compare
AH-64E Apache helicopters from 30 million to 40 million USD, MQ-9A Reaper drones from
40 million to 60 million USD, and F/A-18E/F Super Hornets from 60 million to 70 million USD.
Their benchmarks are 35 million, 50 million, and 67 million USD, with fiscal-year prices
and the Reaper's upper-end sensor configuration disclosed in all six locales.
Processing-time comparisons end the work-week band at 88 Earth days, then compare Mercury
years until 210 Earth days and one-way Mars journeys until one Julian Earth year. The
benchmarks use 88 and 210 Earth days, with favorable-launch-window assumptions and the
Perseverance cruise reference explained in all six locales.

Comparison-refresh validation: CPAMC passes all 2,126 tests, ESLint, TypeScript, and its
production build. Desktop/mobile Chrome CDP checks cover the revised bands and all six F1
translations, including tooltip placement and Escape dismissal. The canonical embedded
panel builds, `go test ./...` and the disposable server compile pass, and a live loopback
CPA serves the exact embedded HTML bytes.

Keys displays Last activity before First activity in the desktop table and mobile cards.
Client verification and desktop/mobile browser checks cover the order, matching lifetime
timestamps, and both activity sorts.
The canonical bundle, management-asset tests, server compile, and live embedded-HTML smoke
check pass for the updated client.

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

Usage cost comparisons split the 48,000 USD to 1 million USD range into Ford Mustang GT,
Porsche 911 GT3 RS, and Lamborghini Aventador SVJ bands, using illustrative benchmarks of
48,000, 250,000, and 520,000 USD. Explanations in all six locales disclose the model years
and US MSRP basis, including the Aventador's historical launch price.
The 1,000 USD to 48,000 USD range uses iPhone 17 Pro 256GB, Apple Vision Pro,
Rolex Submariner, NVIDIA RTX PRO 6000 Blackwell, and Tesla Model 3 RWD comparisons at
1,000, 4,000, 10,000, 17,000, and 37,000 USD, with dated price assumptions in all six locales.
The 200 USD to 1,000 USD band uses LEGO Back to the Future DeLorean sets at a rounded
200 USD per set, replacing months of an AI subscription.
The 1 million USD to 25 million USD range uses Gravity Jet Suits at 500,000 USD each,
Princess F55 yachts at 2.5 million USD each, and an illustrative 15 million USD F1-car build
estimate. The F1 tooltip includes a partial historical component-cost breakdown, with
research, development, and season operations excluded.
Super Bowl airtime covers 25 million to 30 million USD. The following bands compare
AH-64E Apache helicopters from 30 million to 40 million USD, MQ-9A Reaper drones from
40 million to 60 million USD, and F/A-18E/F Super Hornets from 60 million to 70 million USD.
Their benchmarks are 35 million, 50 million, and 67 million USD, with fiscal-year prices
and the Reaper's upper-end sensor configuration disclosed in all six locales.
Processing-time comparisons end the work-week band at 88 Earth days, then compare Mercury
years until 210 Earth days and one-way Mars journeys until one Julian Earth year. The
benchmarks use 88 and 210 Earth days, with favorable-launch-window assumptions and the
Perseverance cruise reference explained in all six locales.

Comparison-refresh validation: CPAMC passes all 2,126 tests, ESLint, TypeScript, and its
production build. Desktop/mobile Chrome CDP checks cover the revised bands and all six F1
translations, including tooltip placement and Escape dismissal. The canonical embedded
panel builds, `go test ./...` and the disposable server compile pass, and a live loopback
CPA serves the exact embedded HTML bytes.

Keys displays Last activity before First activity in the desktop table and mobile cards.
Client verification and desktop/mobile browser checks cover the order, matching lifetime
timestamps, and both activity sorts.
The canonical bundle, management-asset tests, server compile, and live embedded-HTML smoke
check pass for the updated client.

**Last updated:** 2026-11-18

### DL005 — Failure-isolated embedded CPA Usage Keeper

CPA carries optional CPAUK analytics behind the `internal/cpauk` package boundary. The port adapts observable behavior from upstream CPA Usage Keeper v1.15.0 at commit `696a4659ce1d5d6f2d2d0530e3205eb51fbce889` without importing its GORM, CGO SQLite, HTTP server, authentication, or process-lifecycle architecture. Analytics are disabled by default and have independent startup, health, circuit, maintenance, and shutdown states. Bounded, lossy observer delivery runs only after synchronous per-key accounting; storage errors, panics, queue saturation, and invalid analytics configuration cannot change proxy readiness or request success. The SDK usage manager keeps trusted accounting separate from generic observer delivery, with independent bounded lanes, taps, drain behavior, and access-provider/native-key metadata in each record.

Runtime analytics require ClickHouse server mode (DL060). The original pure-Go SQLite store described below is frozen as the one-time import source and the differential test oracle for the ClickHouse store, which preserves the same contracts. Its migration numbers in this and later entries describe the legacy import format.

The module sanitizes usage records before enqueue, persists full SHA-256 key identities in its analytics database, and retains a separate random identity key for credential pseudonyms and encrypted cursors. Checksummed migrations, quota reserves, verified backup and restore, corruption guards, hourly and daily rollups, monotonic retention checkpoints, resumable import and rollback, resumable raw-history repricing with an explicit retained-history cutoff, confirmed purge batches, repair jobs, and identity-epoch recovery are isolated from CPA's auth and limit state. Event detail is indexed; event APIs reject retained-away ranges rather than returning partial history. API schema v2 adds outcome and rate summaries, zero-filled activity and analysis series, full-range latency statistics with capped distributed scatter samples, once-per-event cost-component reconciliation, named range resolution, complete sanitized CSV/JSON exports, missing-price provenance, and credential-level provider/quota rows with request identity deduplication while retaining schema-v1 requests and fields. Current-period named ranges end at request time instead of including future buckets. Summary, time series, dimensions, event pages, pricing, provider quotas, key catalog, and token- or known-cost leaderboards share bounded multi-key query contracts and explicitly report unpriced tokens.

Pricing discovery uses models.dev with a durable six-hour cache. Usage activity and pricing-page reads request refreshes on demand; startup and idle time do not schedule catalog downloads. Concurrent demand shares a refresh, and failures retain the last successful catalog with a 15-minute retry delay. Manual model and alias overrides are stored separately and take precedence over discovered prices. Catalog matching uses explicit provider mappings and exact model IDs. CPAMC presents these as API-equivalent cost estimates, including for subscription-backed OAuth accounts; context-dependent rates and non-token charges remain outside these estimates. Historical events retain their stored cost until an explicit repricing job runs. Tests cover TTL expiry, concurrent refreshes, persistence, stale fallback, decimal parsing, and prices applied to newly ingested events. Live models.dev and browser checks cover catalog loading and override creation/removal.

Service-tier token pricing reads models.dev's experimental v2 catalog, including per-model service-tier multipliers, with the v1 catalog and its explicit mode prices as a fallback. Import normalizes scalar and component-specific multipliers into complete input, output, and cache rates; conditions requiring other request metadata are skipped. The provider's returned tier takes precedence over the requested tier, including a returned default tier. Unknown tiers and models without tier prices retain their baseline rates. Manual overrides remain authoritative and can include `service_tier_prices` through the management pricing API. Migration 15 preserves existing rules, adds durable tier prices, and expires the old catalog for its next demand-driven refresh. Stored event costs change only through explicit repricing. Analytics cost components include tiers in their event reads and pricing cache keys. Regression tests cover both feed formats, cache TTL decoding without source mutation, tier precedence, overrides, migration, restart, ingestion, and repricing; a live-catalog smoke check verifies distinct default, priority, and flex costs persisted through the production writer.

The CPAUK intake adapter now persists the canonical input total from the SDK token breakdown. This keeps Anthropic and Claude cache-read buckets independent from uncached input when calculating cache-read rates and pricing, while preserving OpenAI-style input totals where cached tokens are already included. Existing event and rollup rows retain their stored token buckets; the current repricing operation updates price fields only, so repairing historical token totals requires an explicit migration with provider-scoped review.

CPAUK also persists an optional observed generation duration: the local monotonic interval between first and last substantive streaming token arrivals. Chat error envelopes, `[DONE]` markers, Claude signature-only deltas, and other metadata or terminal-only frames do not extend it. Native HTTP streams now capture generation timestamps in a bounded read-ahead observer before downstream forwarding; Codex WebSocket frames carry their socket-read timestamps through a byte-budgeted queue. Queue saturation invalidates generation timing instead of reporting a compressed interval, preserving a first-token observation already captured before saturation. Historical records and responses without observed token boundaries remain unavailable. This measures arrival timing at CPA, not exact provider-side execution. Analytics expose full-population p95, maximum, median and accumulated duration coverage, including ranges beyond 30 days; retained-away timing observations are reported as partial. E2E names the existing executor duration from attempt setup through completion; the local TTFT and token-arrival boundaries differ from client-observed wall time. This follows [OpenRouter’s total-latency decomposition](https://openrouter.ai/docs/guides/best-practices/latency-and-performance.md) into time to first token (network, queue and prefill) plus generation, without claiming identical measurement boundaries. CPAUK also stores locally observed dispatch-to-first-substantive-token and dispatch-to-response durations as nullable `first_token_latency_ms` and `provider_latency_ms`. HTTP headers and request-specific Codex/xAI WebSocket response frames establish the response endpoint. These measurements include network and provider wait time; they do not claim provider-reported acceptance timing. Native OpenAI-compatible, Claude, Gemini/Vertex, Antigravity, xAI, and Kimi streaming loops invoke their protocol-specific substantive-token observers alongside the existing Codex and plugin paths. Buffered nonstream parsing does not fabricate token arrival times. Historical and unsupported observations remain null. Analysis and processing-time totals expose measured values, coverage, and percentiles; retries restart the local timer. Existing TTFT retains its first-packet fallback, while first-token latency requires substantive token evidence. Provider `created_at` timestamps are never subtracted from CPA UTC: an unrelated online clock cannot calibrate a provider clock or remove timestamp quantization. Additive model cost components preserve reconciled stored totals, and key catalogs expose distinct requests, selected-range top models and observed generation totals alongside existing lifetime activity.

Versioned fixtures record events, token categories, deterministic credential identities, key display identities, ranges, pricing, limit windows, viewer isolation, maintenance envelopes, imports, reconciliation, and stable pagination. `test/perf` includes a production adapter covering HTTP, streaming, WebSocket, retries, blocked SQLite, and saturated queues. Its long median-of-five certification remains opt-in because it requires the documented dedicated-runner metadata and power controls.

A 2026-09-08 metric audit produced eleven corrections. The intake adapter now persists the canonical inclusive output total (Gemini candidate plus thought tokens) alongside the input total, so reasoning is priced. `TokenUsage.UnclassifiedTokens()` derives the remainder of the authoritative total that no bucket explains; a matched price rule reports that remainder as unpriced instead of claiming full coverage, and repricing inherits the rule. Analysis model rows, matrix cells, and activity buckets expose `unpriced_tokens`, and CPAMC uses it to qualify model efficiency as unknown or partial rather than ranking it as $0/M; token charts and the distribution add an explicit unclassified category so stacks sum to the total, and event speed follows the qualified arrival-window and separate effective-rate contract in DL052. Migration 005 adds `known_cost_nano` and `unpriced_tokens` to `daily_stats` so year activity keeps cost after retention; year activity deduplicates request identities across the raw/retained boundary; the public `success` filter maps to the `succeeded` column; retained credentials keep their auth type and actual last-observed timestamp instead of a bucket end; upstream v1.15 imports no longer force `exact` quality, so all-zero usage is inferred as `missing`; and the collector counts batches discarded at shutdown as abandoned so accepted equals written plus dropped plus abandoned. Existing stored event and rollup token buckets are unchanged; historical repair still requires the explicit provider-scoped migration noted above.

Trusted accounting and bounded observer lanes remain independent of plugin observers. Executor test registrations unregister during cleanup so repeated fixture setup cannot exhaust those lanes.

**Implementation evidence:** `sdk/cliproxy/usage/delivery.go`. `internal/cpauk`, `internal/cpauk/collector/adapter.go`, `internal/cpauk/collector/sanitizer_test.go`, `internal/cpauk/testdata/upstream-v1.15.0`, `sdk/cliproxy/usage/{manager.go,delivery.go}`, and `test/perf`.

**Recorded validation:** On 2026-11-18, `sdk/cliproxy/usage/delivery.go` still runs trusted accounting before sanitizer taps and bounded observer lanes; CPAUK implementation/fixtures are unchanged. Delivery ordering, saturation, panic, sanitization and real ClickHouse suites pass.

**Last updated:** 2026-11-18

### DL006 — Reproducible embedded CPAMC artifact

CPA builds CPAMC from the pinned `web/management-center` checkout with a digest-pinned Bun 1.4.3 container (the version CPAMC's `packageManager` declares) and embeds the resulting single-file application in the Go binary. The separate canonical builder removes host-dependent Rolldown output, keys the UI version to the pinned CPAMC commit, and uses a persistent package cache for offline rebuilds. Release archives use the canonical HTML and compatibility manifest. Runtime Dockerfiles instead compile panel source from their build context with Bun 1.4.3 and generate a manifest using the nonempty `CPAMC_COMMIT` build argument. CI separately verifies clean submodule pins; local Docker builds do not perform the canonical builder's clean-tree validation. No product build compares generated artifacts byte-for-byte. No product build clones or floats on a remote CPAMC branch. DL050 later stops committing that bundle: panel-shipping builds now produce it from the pinned submodule, and the submodule is a hard build dependency.

Mutable and downloaded panels require an adjacent `management-artifact.json` whose Management API range accepts this CPA build and whose SHA-256 matches the exact HTML bytes. An invalid, missing, incompatible, or tampered mutable panel falls back to the immutable bundled artifact. The former undigested network fallback is removed, and updates default to the Type-Delta CPAMC fork.

**Implementation evidence:** `internal/managementasset/bundle.go`. `internal/managementasset/bundle.go`, `internal/managementasset/bundled`, `scripts/build-management-center.sh`, `scripts/verify-submodules.sh`, `Dockerfile.management`, both runtime Dockerfiles, `nginx-web.conf`, release workflows, `DEPENDENCY_NOTICES.md`, and `docs/management-center.md`.

**Recorded validation:** Read-only Compose assertions confirm that both source builds receive `CPAMC_COMMIT` and the published-image override removes both build definitions. Shell syntax checks pass for the canonical builder and submodule verifier. Dockerfile inspection confirms Bun 1.4.3 source builds and generated manifests. Management asset tests pass with `TestBundledArtifactIsValid` explicitly excluded because the canonical bundle had not yet been rebuilt. Fresh image builds and deployed-panel checks remain pending.

**Last updated:** 2026-11-18

### DL007 — Analytics management and shared-view control plane

CPA exposes capability-gated analytics management routes under the canonical authenticated `/v8/management/analytics/*` path (with the established v0 compatibility group retained) for health, summary, time series, activity, independently partial analysis, dimensions, events, indexed event detail, pricing, resumable repricing, credential-level provider quotas, key catalog, stable leaderboards, bounded complete CSV/JSON exports, backup, restore, import, rollback, purge, repair, maintenance jobs, and shared viewers. Named and explicit ranges resolve to echoed UTC bounds and an IANA zone. Key identity filters are accepted only in bounded JSON bodies; every analytics route rejects key IDs in URLs, and request logging masks accidental `key_id` and `key_ids` query parameters before recording them. Expensive admin reads and maintenance starts have independent bounded peer rate limits. The public analytics viewer remains at `/v0/analytics/viewer/*` and is not moved into the management group. Configuration saves and reloads use cloned runtime configuration and stale-snapshot protection, while the merged server publishes ETag, request-body, and Claude tool-search behavior without bypassing CPAUK lifecycle invalidation or viewer security.

Shared viewers are stored in a separate bounded, atomic JSON document containing only credential and session hashes plus fixed key scope. The file and its parent-directory rename are synchronized before success. An administrator receives a viewer credential once, can list durable non-secret viewer metadata after a restart, and can selectively revoke the viewer and all its sessions. Viewer exchange requires direct HTTPS or verified forwarded HTTPS from a configured immediate proxy, issues a short-lived `HttpOnly`, `Secure`, `SameSite=Strict` cookie, and removes credentials from request logging. Viewer routes have a fixed CORS allowlist, cannot accept a management credential, and cannot select another key ID. Configuration reload invalidates active sessions and reports restart-required trust changes without taking proxying down.

Plain HTTP development remains available only through the explicit loopback option. The protocol multiplexer no longer advertises an empty TLS connection state on ordinary buffered TCP connections, so same-origin loopback requests with ports are classified as HTTP. After protocol sniffing, TLS-backed HTTP retains the real connection state, including for clients that omit ALPN, while Redis RESP over the same no-ALPN TLS listener keeps its existing route.

CPA normalizes CRLF and lone-CR YAML input before comment-preserving writes. This prevents carriage returns retained in parsed comments from becoming extra blank lines when startup hashes a plaintext management secret or another nested scalar changes; the resulting file uses LF line endings.

The root `analytics` block is valid in both the legacy and v8 configuration layouts, so it never marks a file as v8. A v0 management save that writes the default analytics block keeps the legacy layout on the next save instead of migrating the whole file.

The caller-owned interceptor host and cached legacy-secret verification compose with the analytics service, request taps and viewer lifecycle. Reload still invalidates sessions when trust settings change.

**Implementation evidence:** `internal/api/server_management_v8.go`. `internal/api/analytics_*`, `internal/api/handlers/management/analytics*`, `internal/api/server*.go`, `internal/api/middleware/request_logging.go`, `internal/config/{analytics.go,config_yaml.go,config_v8.go}`, `internal/util/provider.go`, and `config.example.yaml`.

**Recorded validation:** On 2026-11-18, `internal/api/server_management_v8.go` retains authenticated, URL-key-rejecting analytics routes; server construction retains service/tap/viewer lifecycle and reload invalidation. Management/viewer isolation, persistence and config reload tests pass.

**Last updated:** 2026-11-18

### DL008 — Fresh graceful-shutdown budget

The 30-second graceful-shutdown budget begins when `Service.Run` exits, not when the service starts. Long-running processes therefore give HTTP, analytics, usage observers, and other background components their intended bounded drain instead of receiving an already expired context.

The regression uses `testing/synctest` to expire a synthetic startup budget before `Run` exits. It proves that the actual deferred shutdown still waits for a usage observer released 29 virtual seconds after exit, without wall-clock sleeps or production test hooks. The independent listener-close test remains covered under its accurate name.

Shutdown drains the fork's bounded usage delivery before closing upstream's local KV store and retiring publisher-scoped connection pools. Retirement fences stale pool snapshots and closes idle connections without waiting for active requests. The fresh drain budget does not impose a deadline on stalled bbolt close I/O.

The upstream optional HTTP shutdown drain composes with the fresh SDK shutdown budget and honors a tighter parent deadline. Owned hijacked WebSockets are reclaimed through their raw transport, including no-ALPN TLS wrappers, before capture finalization. Closure does not introduce post-upgrade read/write deadlines or wait for TLS close_notify.

**Implementation evidence:** `sdk/cliproxy/{service_lifecycle.go,service_stop_test.go,kv_store.go,kv_store_test.go,proxy_pool_publisher_test.go}`, including `TestServiceRun_ShutdownContextFreshAtExit` and `TestServiceRun_ClosesListenerAtExit`.

**Recorded validation:** `TestServiceRun_ShutdownContextFreshAtExit` passes ten consecutive runs. Both mutation overlays fail: an already expired final shutdown deadline, and a 30-second shutdown context created before the deferred function is installed. The full SDK service package passes with `-count=1`; the full build, disposable server compile, and touched-package vet pass.

**Last updated:** 2026-11-13


### DL009 — Configured analytics storage time zone and exact retained reads

`analytics.storage-time-zone` selects the zone whose local hour and day boundaries retention uses when it builds rollups. It defaults to `UTC`, is validated as an IANA name, and an invalid value disables analytics only while the proxy keeps serving traffic. The store no longer adopts the first query's zone on a fresh database; the configured zone is recorded on first use and existing rollups are never rebucketed by a later configuration change.

Retained reads are exact or refused. A query whose range contains rollups but requests another zone is rejected for daily and hourly grains alike rather than relabelling an indivisible stored bucket, so minute-level events inside one stored hour cannot be moved into the wrong fractional-offset bucket. The store's partial error carries the stored zone, the requested zone, and the bucket width, and the management API surfaces that reason in the `analytics_invalid_query` envelope while keeping both partial sentinels matchable with `errors.Is`; internal error text is still withheld.

Named ranges require top-level `schema_version: 2`, and CSV and JSON exports emit the same flat set of sanitized event fields. Both are recorded in the v2 contract document.

**Implementation evidence:** `internal/cpauk/config.go`, `internal/config/analytics.go`, `internal/api/analytics_options.go`, `internal/cpauk/store/{config.go,operations.go,retained_query.go,store.go}`, `internal/api/handlers/management/analytics.go`, `config.example.yaml`, `docs/analytics-operations.md`, and `docs/analytics-api-contract-v2.md`.

**Recorded validation:** `go test ./internal/cpauk/... ./internal/config/... ./internal/api/... -count=1` passes, including the retained-only acceptance test that retains a day-boundary seed with minute-level offsets, replays hourly buckets in the storage zone against the pre-retention raw aggregation, and asserts a reasoned partial for the same range in `Asia/Kolkata`, plus the handler test that checks the zone pair and grain reach the error envelope. `gofmt -l .` is clean for the touched files and the disposable `cmd/server` compile check passes.

**Last updated:** 2026-09-03

### DL010 — Named retention and viewer expiry in the analytics contract

Three analytics responses stopped being generic.

Raw-event reads whose range starts before the retention cutoff now return a typed `store.RetainedRangeError` carrying the cutoff instead of the bare `ErrRetainedRangePartial` sentinel. The management API maps it to a 400 `analytics_invalid_query` whose message names the RFC3339 cutoff, and the frozen error envelope repeats it in an additive `retention_cutoff` field so a client can narrow the range without parsing English. Both partial sentinels stay matchable with `errors.Is` and internal error text is still withheld.

Viewer capabilities now report the shared view (link) expiry and the browser session expiry as distinct fields. `view_expires_at` is the creator's chosen link lifetime, `session_expires_at` is the 30-minute session, and `expires_at` remains an alias of the session expiry so older viewer clients keep working. The viewer page previously rendered the session expiry under the creator-facing "expires after 7 days" promise.

A store that refuses to open because its persisted `retention_time_zone` metadata disagrees with `analytics.storage-time-zone` now returns a typed `store.ZoneMismatchError`. The service publishes both zone names in health as `category: storage_time_zone`, `field: storage-time-zone`, a message naming each zone, and an additive `zone_mismatch: {stored, configured}` object, so Maintenance can show the operator why analytics is unavailable.

The v2 contract document additionally records the `{keys, meta}` catalog wrapper, the providers/quotas wrappers and their `durable` flag, pricing `sync_state`/`updated_at`, the full `Capabilities` and `Health` field sets, the viewer expiry fields, and the retention error details. On the client, `ViewerEventPage.total_count` became optional to match Go, and `AnalyticsQuotasResponse` gained `durable`.

**Implementation evidence:** `internal/cpauk/store/{config.go,query.go,operations.go}`, `internal/cpauk/model/result.go`, `internal/cpauk/{health.go,service.go}`, `internal/api/handlers/management/{analytics.go,analytics_viewers.go}`, `internal/api/analytics_viewer_routes.go`, `docs/analytics-api-contract-v2.md`, `docs/analytics-operations.md`, and in CPAMC `src/features/analytics/ViewerPage.tsx`, `src/features/analytics/views/viewer/viewerApi.ts`, `src/types/analytics.ts`.

**Recorded validation:** `go build -o test-output ./cmd/server` succeeds and `go test ./internal/cpauk/... ./internal/api/... -count=1` passes, including `TestRetainedEventErrorCarriesTheRetentionCutoff` and `TestStorageZoneMismatchNamesBothZones` (store), `TestStorageZoneMismatchSurfacesBothZonesInHealth` (service health), `TestAnalyticsEventsOverRetainedRangeNamesCutoff` (400 message and `retention_cutoff` envelope field), and `TestViewerScopeSeparatesViewAndSessionExpiry` (the session expiry precedes the view expiry and both serialize). `gofmt -l .` reports only the two pre-existing unformatted executor test files. In CPAMC, `bunx tsc --noEmit` is clean for the owned files and `bun test tests/analyticsViewerExpiry.test.ts` passes four cases covering the two dated sentences, the legacy `expires_at` fallback, and the omitted link sentence.

**Last updated:** 2026-09-03

### DL011 — Viewer cross-origin allowlist

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

Both server and viewer registration propagate the configured allowed origins. Cookie selection and reload invalidation retain the same trust boundary alongside upstream management authentication caching.

**Implementation evidence:** `internal/api/server.go`. `internal/config/analytics.go` (`AllowedOrigins`,
`NormalizeOrigin`, `IsLoopbackHostname`), `internal/api/analytics_viewer_routes.go`
(`AnalyticsViewerSecurityOptions.AllowedOrigins`, cookie `SameSite` selection),
`internal/api/server_middleware.go` (`applyViewerSameOriginPolicy` allowlist
branch, `analyticsViewerCrossOriginContextKey`), `internal/api/server.go` (both
`AnalyticsViewerSecurityOptions` construction sites), `internal/api/server_reload.go`
(`markAnalyticsViewerRestartRequired` equality check), `config.example.yaml`,
`docs/analytics-operations.md`, and `docs/analytics-api-contract-v2.md`.

**Recorded validation:** On 2026-11-18, `internal/api/server.go` and the viewer registration both pass AllowedOrigins; middleware and SameSite cookie selection remain wired, and reload marks trust changes. Origin, forwarded HTTPS, cookie and session invalidation tests pass.

**Last updated:** 2026-11-18

### DL012 — Calendar range presets and retained one-year activity

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
and `total_tokens`, plus `known_cost_nano` and `unpriced_tokens`. SQLite migration 005 adds and backfills the cost fields. ClickHouse is the runtime backend; SQLite remains the legacy import and differential-test backend, and both preserve retained activity costs.

**Implementation evidence:** `internal/cpauk/aggregate/range.go`,
`internal/cpauk/model/query.go`, `internal/cpauk/store/activity.go`,
`internal/cpauk/store/activity_test.go`, `internal/cpauk/store/daily_stats.go`,
`internal/cpauk/store/daily_stats_test.go`,
`internal/cpauk/store/migrations/{002_daily_stats.sql,005_daily_stats_cost.sql}`, `internal/cpauk/chstore/activity.go`,
`internal/api/handlers/management/analytics.go`,
`docs/analytics-api-contract-v2.md` and `docs/analytics-operations.md`. `TestYearActivityPreservesCostAfterRetention` and `TestMigrationFiveBackfillsDailyStatsCost` cover retained cost and its backfill.

**Recorded validation:** the calendar range tests cover
`America/St_Johns` and `Asia/Kolkata`; storage tests verify retention output,
migration backfill, DST-aware day bounds, 365 gap-free activity buckets,
cross-zone rejection, source independence, and `key_ids` across retained and
hot days.

**Last updated:** 2026-11-07

### DL013 — Human-readable API key labels

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

The watcher parser carries labels through its expanded return contract; structured TUI writes retain SHA-256 key identity, limits, original indexes and revisions.

**Implementation evidence:** `internal/config/api_key_entry.go`. `internal/config/api_key_entry.go`,
`internal/api/handlers/management/config_lists.go`,
`internal/api/handlers/management/analytics_pricing.go`,
`internal/watcher/diff/config_diff.go`, `internal/tui/{client.go,keys_tab.go}`,
`config.example.yaml`, and the CPAMC `src/features/config/components/blocks/ApiKeysCardEditor.tsx`,
`src/services/api/apiKeys.ts`, `src/types/apiKeys.ts`, and
`src/utils/keyIdentity.ts`.

**Recorded validation:** On 2026-11-18, `internal/config/api_key_entry.go` preserves exact inbound-key labels and SHA-256 identity; management, analytics catalog, reload diff and TUI paths still consume them. Label validation and revisioned TUI HTTP journeys pass.

**Last updated:** 2026-11-18

### DL014 — Shared management quota refresh protection

CPA protects the known CPAMC quota and account-information requests forwarded
through `POST /v0/management/api-call`. Concurrent identical requests for a
credential share one upstream call, and completed results are reused for three
minutes across management clients. This protection runs before OAuth token
resolution so repeated refreshes also share credential acquisition work.

An upstream HTTP 429 blocks further quota requests for the same credential and
provider until `Retry-After` expires. Both delta-seconds and HTTP-date values are
supported; missing, invalid, zero, or elapsed values use a three-minute backoff.
The backoff covers sibling quota endpoints and provider fallback hosts, including 429s
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

Claude reset-grant display reads at `/api/oauth/usage?cedar_ember=1&skip_spend=1`
share the same cache and credential backoff as ordinary usage reads. CPAMC forces
fresh grant and organization reads before spending a reset grant; these reads
bypass successful cached results while respecting active backoff. Successful
Claude reset requests invalidate the credential's cached quota and grant results,
preserving any active backoff. The retained-snapshot notice states the last
successful check time; a separate error strip describes a failed refresh.

Refresh-protection follow-up validation: the full Go suite, focused race tests,
server build, and canonical panel build pass. A browser against the updated
backend and live Claude quota endpoint confirms that repeated usage, profile,
and grant display reads reuse all three upstream response IDs. Simulated 429,
loading, and recovery states retain the last successful snapshot and display
separate status and error text on desktop and mobile.

State is held in memory per CPA management handler, shared by its clients, and
resets when CPA restarts; separate CPA processes do not share it.

Quota cache and reconciliation remain attached to the combined management handler, including fresh spending preflights and reset invalidation.

**Implementation evidence:** `internal/api/handlers/management/api_tools.go`. `internal/api/handlers/management/api_tools.go`,
`internal/api/handlers/management/api_tools_quota_cache.go`, and
`internal/api/handlers/management/api_tools_quota_cache_test.go`.

**Recorded validation:** On 2026-11-18, `internal/api/handlers/management/api_tools.go` selects quota cache/backoff before token resolution; three-minute coalescing, force-refresh, advanced Date and reset invalidation remain. Quota cache/concurrency/backoff tests pass.

The Codex refresh regression exercises `force_refresh` against a local provider
response and confirms the cached three-minute result is bypassed without removing
active 429 backoff.

**Last updated:** 2026-11-18

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

### DL015 — Per-hop event diagnostics and correct endpoint classes

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

Streaming telemetry preserves the committed HTTP status when a later API error has a different classified status. Gemini and Antigravity buffering retain sanitized final generation diagnostics alongside normalized usage. `internal/api/middleware/request_stream_telemetry_test.go` and `internal/runtime/executor/helps/usage_helpers_test.go` cover HTTP-200/later-400 capture and sanitized Gemini generation chunks.

Legacy request-log import accepts both unlabeled responses and upstream response sections labeled `Headers:` and `Body:` without changing body bytes or SSE framing. Local read/transport diagnostics belong to the request and do not become fabricated upstream attempt errors.

**Implementation evidence:** `sdk/api/handlers/handlers.go`. `sdk/cliproxy/usage/{manager.go,delivery.go,proxy_response.go}`,
`sdk/api/handlers/{handlers.go,handlers_auth_unavailable_usage.go}`,
`sdk/cliproxy/auth/conductor_selection.go`, `internal/api/request_id.go`,
`internal/api/server.go`, `internal/runtime/executor/helps/usage_helpers.go`,
`internal/cpauk/model/{event.go,schema.go}`,
`internal/cpauk/collector/{adapter.go,sanitizer.go,collector.go,writer.go}`,
`internal/cpauk/store/{migrations/006_hop_details.sql,write.go,query.go}`,
`internal/cpauk/service.go`, and `docs/analytics-api-contract-v2.md`.

**Recorded validation:** On 2026-11-18, `sdk/api/handlers/handlers.go` still carries hop identity and publishes downstream outcomes; tracked transport and auth-selection failure reporting remain. New response-log layout is handled by legacy import; committed-status, sanitized diagnostics and capture/import tests pass.

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

**Last updated:** 2026-11-18

### DL016 — Catalog pricing, usage probes, and display labels for API-key credentials

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
plan five-hour and weekly credit windows. Results ride the existing three-minute
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

Upstream connection-pool overrides coexist with labels, selectors, probes, and reserves through provider serialization, synthesis, reload, and auth-index reconstruction. Pool settings participate in credential identity and deduplication; label text does not. The v8 `oauth_provider` metadata identifies the login provider separately from display labels and usage probes. `internal/watcher/synthesizer/config.go` and `internal/api/handlers/management/config_auth_index.go` cover the combined identity inputs.

**Implementation evidence:** `internal/config/{vertex_compat.go,credential_labels.go}`,
`internal/api/handlers/management/{config_lists.go,auth_files_fields.go,usage_probe.go}`,
`internal/cpauk/store/pricing_discovery.go`, and the provider configuration serializers.

**Recorded validation:** `go build`, `go vet`, and `go test ./...` pass, including
deterministic tests for binding refetch after an in-flight reconfigure, the
Z.ai payload parser, and display-name precedence. A local CPA with a real Z.ai
coding-plan key priced a `glm-4.7` request from the bound catalog, refetched
the catalog after the binding changed through the management API, and served
live five-hour and weekly windows. The bundled panel was rebuilt from CPAMC
commit `a2eaf19e53f96b321dc51d25d6d7316c9222ecef` (CPAMC DL043).

**Last updated:** 2026-11-07

### DL017 — Temporary upstream failures stay in model lists

HTTP 429 and 5xx responses still cool down request routing, but they no longer
remove the affected model from registry-backed client model lists. A
credential-scoped 429 also keeps sibling models listed when it propagates
credential quota state. Disabled credentials remain hidden and the temporary
failure normalization does not apply to terminal errors such as 401, 403, or
404. Registry publication keeps `transient` as a list-preserving reason while
the merged `credential_quota` accounting correction avoids counting the same
quota state twice.

Transient quota and service failures retain model visibility while blocking selection. Terminal Antigravity TOS deactivation remains authoritative; healthy shared or model-scoped quota reports cannot revive that credential.

**Implementation evidence:** `internal/registry/model_registry.go`. `sdk/cliproxy/auth/conductor_models.go`,
`internal/registry/model_registry.go`, and
`sdk/cliproxy/auth/conductor_model_list_visibility_test.go`.

**Recorded validation:** On 2026-11-18, `internal/registry/model_registry.go` treats credential quota as quota and preserves transient listings while conductor still blocks routing. Existing model-list 429/5xx/disabled tests and new terminal-TOS recovery protection tests pass.

When routing finds no ready credential, the selection warning now reports
credentials blocked by transient upstream failures in a separate "error
backoff" summary next to the quota cooldown summary, so mixed outage windows
are no longer misread as missing failover.

**Last updated:** 2026-11-18

### DL018 — OpenAI-compatible providers receive downstream request headers

OpenAI-compatible executors now forward downstream client request headers to
the provider instead of rebuilding each upstream request from a small fixed
set. This preserves protocol and vendor session headers such as `Session-Id`,
`X-Session-Id`, `X-Session-Affinity`, and `x-opencode-session` for harnesses
that rely on them. Chat, image, streaming, and non-streaming generated requests
share the same header preparation path.

Transport and body-framing headers, hop-by-hop headers and their
`Connection`-scoped fields, `Host`, `Accept-Encoding`, and downstream CPA
`Authorization`, `X-Api-Key`, `Api-Key`, and `X-Goog-Api-Key` credentials are omitted. The selected provider credential replaces
`Authorization`, the downstream `User-Agent` is retained with the existing
fallback, and configured custom headers are applied last so they retain
precedence.

Automatic forwarding also omits `Forwarded`, `X-Forwarded-*`, `X-Real-IP`,
`True-Client-IP`, `X-Client-IP`, `Cf-*`, and `Cdn-Loop`. These describe the
client-to-CPA proxy chain and can expose client addresses to unrelated providers.
An explicit configured custom header may still opt into forwarding one of them.

**Implementation evidence:** `internal/runtime/executor/openai_compat_executor.go`
and `internal/runtime/executor/openai_compat_executor_headers_test.go`.

**Recorded validation:** The 2026-11-13 audit reproduced `X-Goog-Api-Key` leakage in actual Execute and ExecuteStream HTTP requests and in the no-provider-key helper. The extended regressions pass after adding it to the existing exclusion list. Focused execute/stream regressions preserve the Codex
and OpenCode session headers, multiple arbitrary header values, and the client
User-Agent while replacing credentials and dropping transport or connection
headers. The full `internal/runtime/executor` package, `go test ./...`, and the
disposable server build pass.

**Last updated:** 2026-11-16

### DL019 — Credential-labelled distributions, quota window gating, and Analysis chart metrics

The Analysis credential distribution plotted the hashed credential id while every
other dimension already showed something a person could read. Credential
dimension rows now carry admin-only `credential_label` and `credential_filename`
fields, resolved through the same loaded-credential lookup that event details
use. The panel prefers the label, then the file name, then a compact id for
credentials that are no longer configured. Viewer routes keep the previous
behaviour and never receive either field.

Top Models has a tokens / price / generation segmented control. Tokens and
price use 100% stacked area charts with actual amounts in tooltips. Generation
uses independent, unstacked lines of average per-bucket generation time. All
three modes use straight segments without smoothing. Generation has `null` gaps
where a bucket recorded no timing sample and a sample count beside each ranked model. `TimeseriesPoint` and `AnalysisModel`
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

**Last updated:** 2026-11-07

### DL020 — Backend throughput reporting

Event throughput is calculated by the backend so the events table, detail view, exports, and
other clients agree. The original credential-median subtraction estimator and 8% timing rule
were replaced by DL052 after they produced unstable rates from missing generation observations.
The backend now qualifies a matching local arrival window and separately exposes effective
output over the whole attempt. Historical rows without the required observation evidence do
not receive an invented arrival rate. The compatibility fields `tokens_per_second` and
`speed_estimated` mirror the qualified arrival value and its token-count quality; clients use
the explicit `throughput` object to distinguish the method and unavailable reason.

Plugin executor streams no longer flush a partial usage line at 64 KiB. Long usage-bearing
completion lines are assembled until a complete line or valid JSON payload is available, so
telemetry is not discarded; the generic usage delivery manager remains bounded separately.

**Implementation evidence:** `internal/pluginhost/adapters_executors.go`. `internal/cpauk/model/event.go`,
`internal/cpauk/model/throughput_test.go`, `internal/cpauk/store/timing.go`,
`internal/cpauk/store/query.go`, `internal/cpauk/store/throughput_test.go`,
`internal/pluginhost/adapters_executors.go`, and the CPAMC
`src/features/analytics/views/events/eventDiagnostics.ts` and `src/types/analytics.ts`.

**Recorded validation:** On 2026-11-18, `internal/pluginhost/adapters_executors.go` still assembles complete usage lines with no 64-KiB flush; the large-SSE-line test verifies 7,030 tokens. Qualified arrival/effective throughput tests pass through model and store packages.

**Last updated:** 2026-11-18

### DL021 — Provider selectors and OpenCode Go usage tracking

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

Selectors and labels survive upstream per-provider and per-key connection-pool overrides. Pool inheritance affects synthesis and credential IDs without replacing catalog or probe attributes; hot reload compares both sets of fields. The independent `oauth_provider` metadata does not change selector resolution.

**Implementation evidence:** `internal/watcher/synthesizer/config.go`. `internal/config/{config_types.go,credential_labels.go,vertex_compat.go}`, `internal/api/handlers/management/{config_lists.go,config_auth_index.go,usage_probe.go,api_tools_quota_cache.go}`, `internal/api/analytics_options.go`, `internal/cpauk/store/pricing_discovery.go`, `internal/watcher/{diff/config_diff.go,synthesizer/config.go}`, and `config.example.yaml`; tests in `internal/api/handlers/management/{config_provider_selectors_test.go,usage_probe_test.go,quota_reserve_probe_test.go}`, `internal/config/credential_labels_test.go`, `internal/watcher/diff/config_diff_test.go`, `internal/watcher/synthesizer/config_test.go`, and `internal/api/analytics_options_test.go`.

**Recorded validation:** On 2026-11-18, `internal/watcher/synthesizer/config.go` still propagates selectors/reserves/labels beside connection pools; parsing, v8 index and analytics bindings retain all provider families. ProviderSelectorManagement, synthesis, config diff and probe tests pass.

**Last updated:** 2026-11-18

### DL022 — Usage refresh synchronizes routing cooldowns

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
Fresh quota responses reconcile routing at the shared upstream fetch boundary,
covering management reads, analytics and history collection, routing-recovery
probes, and reserve checks. Cached replay leaves routing state unchanged,
including analytics replay after a newer quota refusal. Claude reset-grant reads
also reconcile any usage windows included in the response.

Successful Claude reset grants and Codex reset-credit consumption reconcile
refilled pools before returning, publish updated runtime and registry
availability, and invalidate reserve observations only for refilled windows.
Shared quota blocks are released when grant scope or fresh usage proves that
their allowance has recovered.
Claude recovery follows the selected grant's declared windows, preserving quota
and reserve windows that were not refilled. A `not_limited` result can release
stale quota state without invalidating reserve observations or advancing the
reset epoch; refused or unknown reset results do not release routing state.
Codex preserves independent Spark and code-review limits.
Grant scope evidence expires with the quota response cache. Partial or unknown
Claude grants perform a fresh usage reconciliation without bypassing provider
backoff; failed reads retain unproven holds. Replayed claims do not blindly
release newer cooldowns or reserve observations. Duplicate-claim protection uses
the stable credential identity and covers the panel's retry window independently
of quota result cache expiry.
Quota observations started before a reset are rejected by a runtime reset epoch,
including their reserve-bar publication, so a late response cannot reinstate a
pre-reset cooldown. Provider reset recovery preserves disabled models and
independent non-quota failures.
Fresh Codex reports reconcile standard, additional-model and code-review pools
separately, so a healthy Spark allowance can recover even while the standard
pool is exhausted. Claude's Fable-only reports create model state when needed
and preserve other models' independent cooldowns. Healthy shared Claude windows
do not erase an unreported Fable limit.
Scoped quota reports preserve a separate persisted layer over independent model
failures. Both that layer and the shared credential layer retain the longer
active deadline; quota recovery removes only the applicable layer. Model-pool
classification resolves configured aliases to upstream identities and updates
the cooldown keys used by routing, including alias-only registrations.
Restore merges and live results use the same layer ordering. Disabling ordinary
cooling retains explicit force deadlines beneath either quota layer, and an
earlier success cannot clear an active inner layer after its shared wrapper
expires. Quota recovery preserves independent transient capacity limits.
Registry availability is published for resolved aliases as well as direct IDs.
Recovery loads previously unseen persisted state before writing a snapshot.
Delayed restores apply the same selective recovery transforms, retaining
unselected pools, independent failures and credentials awaiting registration;
explicit removal still deletes their state. A read failure prevents an
incomplete snapshot from replacing persisted cooldowns.
Independent auth-level service and capacity failures retain their own deadlines
alongside quota exhaustion. Newly written snapshots preserve that provenance;
older mixed snapshots cannot reconstruct a shorter deadline that was never
recorded. Delayed reads are fenced against removal and re-registration while
legitimate late registration can still hydrate its persisted state.
An auth record remains live through its longest independent deadline, including
when a shorter shared quota has expired. Restore also accepts older records
whose independent state outlives the outer quota deadline.

Kiro, Kimi on both official domains, xAI billing observations, Antigravity quota
pools, plugin quota providers and declarative probes also reconcile usable
quota evidence. Kiro treats included and trial credits as alternative pools;
an xAI paid health probe recovers its model allowance without clearing other
models' independent limits.
Antigravity keeps Gemini and Claude/GPT pools separate. Plugin fetches and
successful resets reconcile at the host boundary, including SDK callers.
Normalized plugin and declarative reports have no model identity, so they only
recover stale quota when every reported bucket is positive; they cannot infer
credential-wide exhaustion from a model-family pool. A local bookkeeping error
after a confirmed upstream reset is logged without reporting the spent reset as
a provider failure.
Incomplete observations remain inconclusive, and Z.ai preserves omitted numeric
fields instead of treating missing remaining quota as zero. Disabled credentials
and terminal authentication failures remain protected.

Reserve holds are derived from current settings and the latest observation.
Fresh quota reads retain reserve bars even before a reserve is configured, and
removing a reserve preserves those observations for later edits. Canonical v8
configuration saves publish reserve and bypass edits to existing credentials
before returning success, without waiting for the asynchronous full reload.
Reserve and bypass edits do not lift provider quota-read rate-limit backoff.
Claude family reserves are checked against the requested upstream model after
alias resolution. A Fable-only reserve therefore withholds Fable without
blocking Opus or Sonnet, including when no model execution history exists;
shared five-hour and weekly reserves still apply to every model. Routing traces
and dry runs use the same model-aware eligibility check.
Partial reads merge bars by ID, preserving unreported windows until their reset
or explicit invalidation. CPAMC relies on the confirmed upstream reset response
and no longer follows a successful Claude claim with a blanket cooldown reset.

The behavior is centralized in the auth manager and runs after successful
management quota API calls and analytics usage probes, so the existing
"Refresh quota" flow needs no separate reset action.

When quota observations are merged into auth state, the latest observation timestamp and signals
are preserved together with the fork's credential-quota cooldown fields, so refreshes cannot erase
newer routing state.

Credential retry, quota-force, independent-force and non-quota deadlines compose with shared and model-scoped provider quota layers. Persistence includes the scoped independent layer and all deadline equality checks, so recovery or restart cannot erase a shorter independent service failure. Terminal TOS deactivation survives quota recovery; the pool-wide protective pause remains opt-in with a zero default.

**Implementation evidence:** `sdk/cliproxy/auth/conductor_cooldown.go`. `sdk/cliproxy/auth/{conductor_cooldown.go,conductor_lifecycle.go,cooldown_state.go,quota_observation.go,provider_quota_models.go,quota_reserve.go}`,
`internal/api/handlers/management/{api_tools.go,usage_probe.go,quota_reconcile.go,quota_reserve_probe.go,plugin_quota.go,config_v8.go,config_auth_index.go}`,
`internal/pluginhost/quota_provider.go`, and the CPAMC quota adapters under
`web/management-center/src/features/quota/providers/`.

**Recorded validation:** On 2026-11-18, `sdk/cliproxy/auth/conductor_cooldown.go` now compares every new independent deadline as well as scoped layers; save/restore and reset epochs preserve provenance. Quota reconciliation, scoped-family/reset, delayed-restore, independent-cause and terminal-TOS tests pass.

On 2026-11-17, regressions reproduced blocked credentials after upstream manual
resets, fresh quota reads bypassing reconciliation, and reserve saves waiting
for asynchronous reload. The reset happy path now sends its next request through
the real Claude and Codex executors against local upstream fixtures. Additional
selection tests cover Fable reserves with 99% five-hour and 21% weekly quota
remaining, aliases, empty model history, unchanged sibling cooldowns, and
immediate routing after reserve edits. The focused race checks, bundled panel
build and disposable server compile pass.
The integrated full Go suite and CPAMC's pinned-Bun verification also pass,
including tests, lint, TypeScript and the production build. Later regressions
cover alias-only registrations, persisted scoped quota with independent 503s,
both shared/scoped nesting orders, late responses after reset, partial reserve
observations, and the panel reset flow without a blanket cooldown clear.
Browser verification covers login, quota refresh, grant selection, confirmation
and updated quota cards at desktop and mobile sizes with synthetic management
responses, exactly one claim and no blanket reset. A disposable Docker container
serves the panel and authenticated APIs with a matching packaged panel digest.

**Last updated:** 2026-11-18

### DL023 — Retry rounds cover transport and status-less failures

Both the request retry gate and recorded credential cooldown gate accept status-less failures, including `http2: response body closed` and unclassified disconnects. Additional rounds obey the configured `request-retry`, `max-retry-credentials`, and `max-retry-interval` limits, alongside retries for HTTP statuses 403/408/429/500/502/503/504. Caller cancellation, request-scoped and invalid-request faults, permanent certificate/TLS verification errors, and standard TLS or uTLS record-header failures remain excluded at both gates.

Typed transport errors, including `*url.Error`, `net.Error`, and the EOF family, are classified as connection-lifecycle failures so a network blip does not cool the credential. Classification uses `errors.Is`/`errors.As`; message matching applies only to legacy results without typed error information. The untyped `http2: response body closed` message also skips credential and model cooldown.

Transport and status-less failures keep their retry-round eligibility beside credential retry deadlines and bounded continuity. Cancellation and permanent TLS failures remain ineligible for transport retry.

**Implementation evidence:** `sdk/cliproxy/auth/conductor_selection.go`. `sdk/cliproxy/auth/conductor_selection.go` (`isRequestRetryRoundError`, `credentialRetryRoundStateEligible`), `sdk/cliproxy/auth/conductor_cooldown.go`, and `sdk/cliproxy/auth/conductor_transport_retry_test.go`, including `TestTLSRecordHeaderErrorsDoNotRetry` and `TestManagerOversizedUTLSRecordDoesNotRetry`.

**Recorded validation:** On 2026-11-18, `sdk/cliproxy/auth/conductor_selection.go` retains transport/status-less retry eligibility and permanent TLS/cancellation exclusions beside continuity. Existing retry-round, no-cooldown transport and real malformed-uTLS-handshake regressions pass.

**Last updated:** 2026-11-18


### DL024 — Plaintext collaboration metadata for optimized Codex calls

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

### DL025 — Captured Claude Code OAuth request safeguard

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
blocks guarded requests. Failed background captures retry after five minutes. CPA
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
`sdk-py`, `claude-vscode`, and `claude-desktop`.
`upstream.claude.header-defaults.oauth-safeguard-client-identities` accepts a
list of additional entrypoint names, such as `["partner-app", "another-app"]`.
Names match the identity in `claude-cli/X.X.X (external, <identity>)` without
case sensitivity and extend the built-in list only while the direct OAuth
safeguard is active. All other header, beta, format, and session checks remain
required. Stabilized header cache entries distinguish each custom entrypoint
so one identity cannot reuse another identity's cached User-Agent. CPAMC can
add, edit, and remove multiple identities in Claude Header Defaults. Package,
runtime, OS, and architecture headers are not compared to the capture. The
captured profile does not replace configured `Claude Header Defaults`;
configured package and runtime versions supply the outgoing values. Optional
fields and beta values vary across legitimate Claude Code requests, so one
capture does not define an exhaustive request shape. This guard compares
observable request properties; a client that copies them can impersonate Claude
Code. TLS fingerprints are outside its scope. Rejections return a request-scoped
403 with the reason and log the same non-secret reason.

Signature Stabilization (`upstream.claude.header-defaults.signature-stabilization`)
keeps one coherent client/software tuple and a distinct device ID per credential.
The old `stabilize-device-profile` key remains an input alias; canonical values,
including explicit `false`, win. The legacy Go field name remains source-compatible.
A valid persisted device ID stays fixed. New profiles seed from the credential's
existing stored device pool; an ID is allocated only when it is missing or
collides with a distinct credential. Exact copies of the same account and stored
device are aliases of one credential identity, independent of file names, and
share learned software state. Startup preflights known credential records and
migrates their existing filename-scoped profiles before opening the listener,
so an alias cannot initialize shared state below a known learned tuple.
OFF-to-ON reloads run the same preflight before publishing the enabled config. Requests
also reconcile later-seen original records without downgrading canonical state. This preserves the existing device across
renames and stabilization toggles, without inventing software versions.
Existing metadata device IDs are
normalized before payload rules; selected account and root session values remain
intact. Token-count requests retain their metadata-free native shape.
A confirmed native request establishes the client identity and
software versions while the configured platform stays pinned. Later observed
software tuples may upgrade the same client only when every version component
is nondecreasing; another entrypoint or an older client cannot replace it.
Request-specific User-Agent fields such as `workload/cron` remain per-request.
Profiles have no expiration in local/Home KV; disabling KV limits persistence
to the process lifetime. Concurrent upgrades use compare-and-swap, and each
request shares one resolved profile snapshot between billing and headers.
Confirmed native API-key requests share this snapshot too. Existing native CCH
signatures are recomputed on the final first-party Messages body after adaptation,
without injecting billing into requests that did not supply it.
When an unlearned fallback remains active, coherent configuration upgrades are
accepted without downgrades or changing its device; persisted custom fallback
values remain readable until a native client establishes a valid tuple.
Existing billing version and entrypoint are normalized before user payload
rules and CCH signing. A changed billing version also recomputes its prompt-
dependent three-character fingerprint. A read-only final check examines billing
fields independently of order and rejects payload-rule overrides
that contradict the stabilized billing version or entrypoint instead of
overwriting configured values. Token-count requests use the same profile without
receiving billing attribution. Explicit `cloak_mode: never` keeps native
passthrough semantics. With stabilization disabled, confirmed native callers
retain valid User-Agent, SDK, runtime, OS, and architecture values, including
older software and newer release lines.

Confirmed native subagent requests retain their root session UUID on the wire.
The per-agent routing key remains internal, so `routing.session-affinity-subagents`
continues to control parent-credential inheritance independently. Native requests
do not receive the thinking-prefix `drop_block` adaptation unless credential
metadata establishes that CPA selected a different account. Explicit caller
thinking settings remain authoritative. Native caller feature betas are retained;
required credential/body betas still apply. Body betas are lifted to headers
only after payload rules, consistently for Messages and token counting; confirmed
native managed feature betas receive the same treatment as header equivalents. When
original native account metadata proves a credential switch, predecessor request
and message links are cleared before payload rules, while same-account and
unknown-provenance links are preserved.

Upstream `upstream.claude.header-defaults.native-entrypoints` independently extends general native-client detection, including bypass paths where the safeguard is inactive. Safeguard-only identities apply only to direct guarded OAuth. Both lists still require strong request signals; stabilized profiles are credential-scoped across client identities. `claudeOAuthDetectionConfig` preserves the general list when disabling safeguard-only identities. Session-date pins use upstream local or Home KV with zero TTL and remain separate from executable captures. With KV active, `session-date-pin-days` is ignored; disabling local KV restores the standalone memory policy. `internal/runtime/executor/helps/{claude_client_detection.go,claude_client_detection_test.go,claude_device_profile_test.go,claude_diagnostics.go}` and `sdk/cliproxy/service_claude_reference.go` cover detection, pinning, and retry cadence.

**Stabilization and native-forwarding validation:** the full Go suite and server
build pass, as do focused race tests covering local/Home profile CAS, concurrent
upgrades, the request snapshot, native sessions, betas, and continuity. A real
Claude Code 2.1.322 process completed a turn through an isolated CPA HTTP server
and an intercepted Anthropic transport with stabilization both off and on.
The runtime Docker image also passes a private-network smoke test: the profile
upgrades coherently, survives a container restart, rejects an older replay, and
forwards the caller tuple after stabilization is disabled.
The captured outbound requests preserve the native software/platform when off,
and use coherent normalized billing and a pinned platform when on. This fixture
validates the client/proxy path. A separate live request through the final
Signature Stabilization executor reached Anthropic and returned `OK.` after the
provider quota reset, without refreshing credentials or changing production. Regression tests
also cover all three executor paths, signed-body integrity, payload overrides and
filters, subagent affinity on/off, and workload/background native identities.
Alias migration regressions cover account-field aliases, copy-first startup, late
original reconciliation, and OFF-to-ON configuration reload before publication.
Independent final council review approved the requested scope at 9.5/10 and 9/10.

Cloaked Claude cross-model refusal fallbacks follow upstream's explicit opt-in `upstream.claude.server-side-fallback`, default false. Explicit caller and payload fallbacks remain authoritative. Opus 5.5 to Opus 4.8 and Fable 5.1 to Opus 5 are injected only when opted in before user payload rules; later model rewrites do not rerun built-ins. Actual serving models and fallback transitions remain visible in usage and translated responses. The measured fallback client tuple is CLI 2.1.321, SDK 0.128.0 and Node v26.12.1; the fork's coherent identity, alias migration, monotonic profile and CAS design remain intact. `TestClaudeFallbackPayloadBarrierMergedPaths` checks 13 cases through Execute and ExecuteStream, including model rewrites, explicit overrides, filtered fallbacks, final beta headers and one rule evaluation.

Claude uses native `output_config.format` for structured output. Identity, billing preparation and prompt-cache cleanup run before final payload rules on execute, stream and token-count paths; only beta-header extraction, signing, serialization and read-only validation follow. Explicit payload-configured prompt-cache fields survive.

**Implementation evidence:** `internal/runtime/executor/claude_executor_execute.go`. `internal/claudecapture/`,
`internal/runtime/executor/claude_oauth_safeguard.go`, Claude executor paths,
`internal/runtime/executor/helps/claude_device_profile.go`,
`internal/runtime/executor/{claude_executor_stabilized_billing_test.go,claude_stabilization_integration_test.go,claude_native_fidelity_test.go}`,
`cmd/server/main.go`, config, and the CPAMC toggle.

**Recorded validation:** On 2026-11-18, `internal/runtime/executor/claude_executor_execute.go` prepares identity/billing and cleanup before final rules; only transport beta lifting/signing/validation follow. Safeguard, native fidelity, coherent snapshot/CAS, billing, schema and fallback barrier tests pass across execute/stream/count paths.

**Custom identity validation:** On the final code, `go test ./...` and
`go build -o test-output ./cmd/server && unlink test-output` pass. The canonical
`ALLOW_DIRTY_PANEL=1 ./scripts/build-management-center.sh` Docker build passes.
CPAMC's `bun run verify` passes all 2,979 tests, lint, TypeScript, and the
production build; a Docker rerun with pinned Bun 1.4.3 also passes all 2,979 tests.
Chrome CDP verified the embedded CPA panel by editing, adding, and deleting list
rows, saving through the review dialog, reloading, and checking that the v8
config API returned the same list. Inspected screenshots at desktop 1440px and
mobile 390px show no horizontal overflow. The independent finish review found
no material issues and returned SHIP. Executor acceptance tests use a simulated
Anthropic transport; no real partner inference was attempted.

Stabilized-billing regressions verify per-request versions, confirmed native
entrypoints and Agent SDK suffixes, case-insensitive client prefixes, pinned
software/platform fields, malformed-billing fallbacks, and unchanged body bytes.
Loopback HTTP checks exercise execute, streaming, and upstream token counting
with original, overridden, and filtered billing blocks; Messages checks also
validate credential identity and CCH. Live Anthropic requests passed the outgoing
header and CCH assertions, but successful inference could not be verified:
one existing credential returned revoked-token 401 and another returned account
rate-limit 429. Neither credential was refreshed or changed. The full Go suite,
canonical panel Docker build, and disposable server build pass.

**Last updated:** 2026-11-18

### DL026 — Hosted images on standard Responses and metadata-only Lite requests

Eligible native Codex Responses requests receive an automatic `image_generation` tool on standard Responses routes without reasoning, including catalog-Lite models. The fork also preserves hosted images when the only true Lite hint is `client_metadata.ws_request_header_x_openai_internal_codex_responses_lite`, accepting boolean `true` or the string `"true"`, including requests with reasoning.

An effective explicit `X-OpenAI-Internal-Codex-Responses-Lite: true` header takes precedence after client, credential and model-header resolution. Otherwise a true metadata marker permits the fork exception; without it, reasoning and model-derived Lite restrictions apply. False hints do not bypass those restrictions. HTTP and WebSocket execute/stream paths share injection, configured-payload policy and read-only final validation. Forced image tools on an incompatible Lite route produce a request-scoped 400 directing the caller to the Images API; optional tools retain upstream policy.

Existing caller tools and tool choices, `image_gen` namespace tools, Spark/free credentials, disabled image generation and compact requests retain their exclusions. API-key, SIWC, passthrough and standalone Images paths retain their own contracts. The user-approved Codex automatic-image exception in AGENTS.md applies only to requests carrying an automatically injected image tool. Rules evaluate once against a private prepared projection, so conditions and queries may observe the automatic tool even if compatibility cleanup removes it before sending. Conditional effects and encoded values are retained, cleanup runs before those operations commit once, and final filters preserve an explicit empty tools array. Configured values remain authoritative and no business mutation follows that commit. Caller-supplied image tools and other executors keep the general ordering contract.

Ordered literal/RE2 replacements read and write the same private effects document as defaults and overrides, before final filters. They join the retained operation plan, including role and explicit-path targets, so automatic-image cleanup precedes the single final commit without reevaluating rules or restoring filtered tools.

**Implementation evidence:** `internal/runtime/executor/helps/payload_helpers.go`. `internal/runtime/executor/helps/{codex_hosted_images.go,codex_hosted_images_test.go,payload_helpers.go,payload_finalizer.go,payload_rule_plan.go,payload_image_plan_test.go}`, `internal/runtime/executor/codex_executor_request.go`, `internal/runtime/executor/codex_executor_{execute,stream}.go`, `internal/runtime/executor/codex_websockets_{execute,stream}.go`, `internal/runtime/executor/codex_executor_imagegen_test.go`, and `internal/runtime/executor/payload_barrier_test.go`.

**Recorded validation:** On 2026-11-18, `internal/runtime/executor/helps/payload_helpers.go` evaluates automatic-image effects privately, retracts only an unclaimed automatic tool, then commits retained operations once. Replacement is inside that plan; HTTP/WS image, Lite precedence, empty-tools and one-evaluation tests pass.

**Last updated:** 2026-11-18


### DL027 — Quota report persistence releases the manager lock during store I/O

Targeted provider quota reports now use the manager's lock-safe persistence helper. The helper
persists an auth snapshot after releasing `m.mu`, then reacquires the lock and merges store-owned
changes, avoiding a self-deadlock when the report updates a model state while the manager write
lock is held.

Unlocked store I/O and concurrency-aware state merging include upstream credential retry, force and non-quota deadlines as well as the fork scoped independent layer.

**Implementation evidence:** `sdk/cliproxy/auth/conductor_persistence.go`. `sdk/cliproxy/auth/conductor_cooldown.go` and
`sdk/cliproxy/auth/conductor_persistence.go`.

**Recorded validation:** On 2026-11-18, `sdk/cliproxy/auth/conductor_persistence.go` still unlocks around Store.Save and merges after reacquisition, now including upstream deadline fields. The targeted quota-report persistence regression and concurrent-save/state tests pass.

**Last updated:** 2026-11-18

### DL028 — Greedy soonest recovery credential routing

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
(Pro 500 elite, Pro 200 max, Pro 100 premium, matching their 50x/20x/10x Plus
allowances; Claude Max and Ultra both map to max) and explicit `gsr_tier` overrides are supported; unknown tiers remain neutral and
raw quota totals are never compared across providers. The credential scheduling
weight is added after recovery decay and the allowance multiplier in both the
selector and scheduler. Omitted weights add 1; non-positive or invalid weights
add nothing and preserve GSR eligibility. The existing request-level round-robin
fallback remains unweighted. CPAMC auth-file hints, card tooltips, and provider
forms describe both strategies in all six locales.
The recovery window is provider-specific: Claude Fable uses the Fable weekly
window, other Claude models use the normal weekly window, Codex and GLM use the
weekly window, and OpenCode Go uses the monthly window. A missing cached reset is
refreshed synchronously through the shared management quota cache; incomplete
recovery data uses the request-level round-robin fallback described in DL036.
Codex available `codex_rate_limits` credits and eligible Claude `cedar_ember`
grants that clear `seven_day` can shorten the effective recovery deadline to
their earliest future expiry. For example, a credit expiring in two days gives
an account with a six-day weekly reset the same recovery weight as a two-day
weekly reset. The provider's reported weekly reset remains unchanged.
Manual reset observations refresh on demand every three minutes even when a
weekly reset is already cached, and survive ordinary response-header snapshots
and cooldown transitions. Spent, expired, paused, ineligible, or not-yet-started
credits do not contribute an expiry. Failed optional reads preserve the last
successful reset observation, or use the normal weekly window when none exists.
HTTP errors of any status, transport failures, and invalid optional responses
cannot erase a known credit or grant expiry. Claude grants that require reaching
a limit still count while the account is healthy. Independent Fable, Spark, and
code-review allowances do not inherit an expiry from a reset of the shared allowance.
Management routing reads and writes normalize the aliases to the canonical strategy.

Plugin candidate exclusions apply before GSR refresh and delegated builtin selection. Continuity uses the same reserve-aware eligibility and ready-pool rule fall-through as ordinary selection without replacing GSR recovery weights or fallback cursors.

**Implementation evidence:** `internal/api/server.go`. `internal/config/config_types.go`,
`sdk/cliproxy/service_config.go`, `sdk/cliproxy/auth/`, `sdk/cliproxy/auth/scheduler.go`,
`internal/api/handlers/management/config_basic.go`,
`internal/api/handlers/management/{usage_probe,gsr_manual_reset}.go`, `internal/api/server.go`, and
`config.example.yaml`.

**Recorded validation:** On 2026-11-18, `internal/api/server.go` still installs the quota refresher; strategy parsing, service selector, recovery decay, tier multiplier and scheduler remain connected. GSR weighting/fallback/affinity and new continuity/live/dry-run parity tests pass.

**Last updated:** 2026-11-18

### DL029 — CPAMC quota summary redesign

Claude cards resolve Max 5x and Ultra 10x relative to Pro. Summary capacity uses
allowance multipliers rather than subscription prices and weights remaining
quota by those shares. One Max and one Ultra contribute 150% of an Ultra's
capacity. Summary and card labels share translation keys. The `iguana_necktie`
dollar-credit pool uses the localized Cloud session credits label and tooltip.

Provider summaries preserve refresh controls, provider tabs, search, sorting,
and the credential list. Summary percentages use the largest available tier.
Claude prioritizes the lower-remaining weekly pool, with Fable winning ties;
OpenCode Go uses its monthly recovery window. Shared tooltips expose credential,
remaining percentage, reset timing, and plan details. Unavailable allowance fills
keep the candy-wrap treatment. The client is pinned at `eea93b6`.

**Implementation evidence:** the `web/management-center` gitlink, its
`tests/{quotaCardTier,quotaSummary,claudePlanType,claudeUltraQuota,claudeFableQuota}.test.ts`,
and the generated management bundle.

**Recorded validation:** the audit's focused quota and Claude tier tests pass,
and the bundled manifest matches the pinned submodule. The canonical bundle
build and desktop/mobile light/dark Quota checks against the merged CPA pass.

**Last updated:** 2026-11-07


### DL030 — Kiro OAuth, Claude-compatible routing, and quota support

CPA now treats Kiro/Amazon Q as a built-in OAuth provider. The native CLI login follows Kiro's official `app.kiro.dev/signin` portal, accepts the `awsidc` callback, starts AWS OIDC device authorization for the selected organization, polls for tokens, and persists both native camelCase and CPA snake_case credential shapes. Desktop/social refresh and AWS OIDC refresh are supported, with rotated refresh tokens and profile ARNs retained. The CLI also exposes `--kiro-login`.

Kiro credentials register a native executor that translates OpenAI Responses and Claude Messages requests into Kiro `conversationState` payloads, discovers missing Identity Center profile ARNs through `ListAvailableProfiles`, and sanitizes Codex JSON schemas. It sends AWS event-stream requests to `runtime.<region>.kiro.dev` for profiled accounts and the legacy `q.<region>.amazonaws.com` endpoint for profile-less Identity Center/Builder credentials, restores Claude-compatible streaming and non-streaming responses, and normalizes Claude model aliases such as `claude-sonnet-4-5-20250929` to Kiro's dotted IDs. Kiro does not return ordinary input/output token counts, so CPA estimates input tokens from the translated Claude request and output tokens from generated text, reasoning, and tool data while retaining explicit Kiro counts when a runtime variant supplies them. The stream adapter emits each Kiro event's `data:` record through CPA's Claude-to-OpenAI translators, preserves translator state across a complete stream, and emits terminal Responses events with the final response ID and output. Kiro participates in Claude model aliasing, thinking normalization, routing, and load balancing.

CPAMC adds Kiro to the OAuth page, provider branding, auth-file grouping, and quota views. The quota adapter reads Kiro `getUsageLimits` data through the management API and renders included and free-trial request windows, subscription details, overages, and reset times. Profile ARNs are exposed as sanitized metadata only; credential files are never downloaded into the browser.

Kiro upstream requests are recorded like every other provider's: request logs and the ClickHouse `attempts` table receive the URL, masked headers, final Kiro payload, credential identity, status, error body, and stream errors. Because the binary AWS event-stream framing is unreadable, the response body stores each decoded event as SSE-style `event:`/`data:` text.

The CPAMC Kiro quota body uses the redesigned quota row layout and contributes its plan and request windows to the quota summary strip. DL033 supplies the Kiro GSR recovery probe. Missing or unusable recovery data keeps eligible credentials in DL036's round-robin fallback.

Kiro explicitly pins Responses system/developer items to the top-level Claude system field before translation. This adapts upstream's in-place placement default to Kiro's builder, so late instructions remain system content rather than joining the active user message. Buffered execution, streaming, and local token counting share `kiroRequestPayload`; `TestKiroResponsesLateSystemItemsRemainSeparate` exercises the buffered and streaming wire payloads; counting uses the same conversion helper.

Shared Claude translators produce native structured-output schemas, but Kiro folds `output_config.format` JSON schemas into system prompt guidance before wire translation and local token estimation. Chat, Responses and native Messages inputs share that preparation in both stream modes. Ordinary and forced Kiro registration use the executor-wrapper helper. Shared media translation does not expand Kiro adapter media support.

**Implementation evidence:** `internal/runtime/executor/kiro_executor.go`. `internal/auth/kiro/`, `sdk/auth/kiro.go`, `internal/api/handlers/management/auth_files_kiro_oauth.go`, `internal/runtime/executor/kiro_{executor,translator}.go`, `sdk/cliproxy/service_{auth,executors,models}.go`, `internal/runtime/executor/kiro_system_items_test.go`, `web/management-center/src/{features/oauth,features/quota/providers/kiro,services/api/kiroQuota.ts}`, and the bundled management assets.

**Recorded validation:** On 2026-11-18, `internal/runtime/executor/kiro_executor.go` restores schema prompt guidance before Kiro translation; shared Responses top-level system conversion remains, and service registration wraps Kiro executors. OAuth/auth, real wire schema/system-item and wrapper-registration tests pass.

**Last updated:** 2026-11-18

### DL031 — Single-pass CPAUK model timeseries aggregation

CPAUK analytics now maintain exact compact 15-minute materialized aggregates for overall, model, provider, and key common-query paths, plus lossless request identities for exact retry deduplication. Batch writes coalesce aggregate deltas, and retention, imports, repricing, and purge rebuild only affected buckets. Aligned ranges read compact UTC aggregates and regroup them into requested local-time buckets, including DST transitions. Unaligned edges use raw/rollup processing for calendar correctness, as detailed in DL042. ClickHouse is the runtime backend; the SQLite implementation remains the legacy import and differential-test backend. Analysis model-by-time queries use one raw-event scan to populate all model buckets, and timing, summary, activity, and dimension queries avoid duplicate scans. The prior high-cardinality materialization was removed because it created nearly one aggregate row per event. The retained-history path remains rollup-aware and keeps the existing hourly/daily correctness rules.

**Implementation evidence:** `internal/cpauk/store/{analysis.go,activity.go,query.go,retained_query.go,timing.go,write.go,materialized_15m.go,materialized_read.go,migrations/010_materialized_15m.sql,migrations/011_compact_materialized_15m.sql}`, and `internal/cpauk/aggregate/range.go`.

**Recorded validation:** `go test ./internal/cpauk/...` and the required server build pass. On a deterministic 100,000-request, 90-day fixture, compact ingestion fell from 31.92s to 13.83s, database size from 444MB to 405MB, 30-day 15-minute timeseries from 183ms to 63ms, 30-day activity from 251ms to 55ms, and 90-day summary from 546ms to 550ms. Compact short-range dimension queries intentionally use the raw path because identity scans are slower than raw SQLite aggregation at that scale. Benchmark harness and comparison scripts live under `test/perf/`.

**Last updated:** 2026-11-07

### DL032 — Bounded historical analytics result cache

Both CPAUK backends cache complete historical Summary, Timeseries, Activity, and
Analysis results in a shared process-local LRU. The cache holds at most 128 entries
and 32 MiB of encoded response and key bytes, and defensively clones values at
ownership boundaries. Keys include the normalized selection, named range/window,
identity epoch, and mutation generation. SQLite retains its durable 15-minute
bucket-generation digest and Analysis pricing-provenance digest. ClickHouse uses
a bounded journal of at most 256 mutation ranges, coalescing writes in the same
UTC 15-minute interval; journal overflow clears the cache and starts a new
generation. An unrelated tail write can reuse closed historical results.

Native and imported events, repricing, and response patches invalidate affected
ClickHouse ranges using event time. Pricing/provenance publication, catalog-binding
changes, retention, purge, import rollback, restore, identity rotation, legacy
publication, recovered late generations, and metadata changes clear cached
results. A miss publishes only if the store's mutation revision is unchanged.
ClickHouse caches only ranges ending at or before the current local calendar day;
today, future ranges, partial Analysis responses, active legacy imports, and known
pending late publications bypass caching. Reopening or restoring pending late
plans preserves that bypass. Time-zone changes require restart and therefore start
with an empty cache.

ClickHouse event, outcome, control, metadata, retained-publication, and restore
writes await visibility before invalidation and unlocking. Submitted writes detach
caller cancellation and disable asynchronous INSERT; synchronous maintenance keeps
its existing mutation-visibility contract. Errors after a partial mutation also
invalidate results. The range-based HTTP response cache in DL043 remains independent.

**Implementation evidence:** `internal/cpauk/backend/history_cache.go`,
`internal/cpauk/store/{history_cache.go,history_generation.go}`, and
`internal/cpauk/chstore/{history_cache.go,aggregates.go,analysis.go,activity.go,write.go,control.go,legacy.go,retention*.go,operations.go,backup.go,restore_journal.go,epoch.go}`.

**Recorded validation:** real ClickHouse tests count native-protocol SELECTs. Warm
Summary, Timeseries, Analysis, and historical Activity reads issue zero SELECTs and
match uncached results. Disabling the cache through a Go overlay makes the same
regression test fail with 4, 3, 11, and 6 SELECTs respectively. Tests cover entry
and byte eviction, defensive ownership, each mutation class, late recovery,
concurrent publication rejection, partial Analysis bypass, duplicate replay,
legacy-import yields, and asynchronous-insert settings. `go build ./...` and
`go vet ./internal/cpauk/...` pass.
`go test -count=1 ./internal/cpauk/... ./internal/api/...` passes against the running
ClickHouse Docker test container.

**Last updated:** 2026-11-02


### DL033 — Kiro GSR recovery, plan tier, Retry-After, and Identity Center login

Kiro participates in greedy-soonest-recovery. The routing refresh probes `codewhisperer.<region>.amazonaws.com/getUsageLimits` with the credential region and optional profile ARN through the shared management quota cache, then persists the included breakdown's monthly `nextDateReset` as `monthly_reset_at` and the `subscriptionTitle` as `kiro_plan_type`. Kiro stays in the passive quota-observation set, so these probe signals survive Kiro responses that carry no quota headers. The plan title maps to GSR allowance tiers (Free neutral, Pro, Pro+ Premium, Power Max), and Kiro upstream errors carry a standard `Retry-After` delay to the cooldown policy.

The CPAMC Kiro login card offers AWS Builder ID or IAM Identity Center. Identity Center sends the organization start URL and region to the existing `start_url`/`region` backend parameters and remembers them in browser storage; the Kiro callback hint shows its real `http://127.0.0.1/oauth/callback` redirect.

Passive Kiro quota observations, plan tiers and Retry-After hints remain available to the combined selection/cooldown pipeline; they do not require an active refresh for each selection.

**Implementation evidence:** `internal/api/handlers/management/usage_probe.go`. `internal/api/handlers/management/usage_probe.go`, `sdk/cliproxy/auth/{conductor_selection,gsr_tier}.go`, `internal/runtime/executor/kiro_executor.go`, the CPAMC gitlink, and the bundled management assets.

**Recorded validation:** On 2026-11-18, `internal/api/handlers/management/usage_probe.go` retains regional/profile Kiro quota reads, monthly reset and plan signals; passive observations, allowance mapping and executor Retry-After remain. Kiro signal/tier, quota parsing, login and retry tests pass; panel login UI excluded.

**Last updated:** 2026-11-18

### DL034 — Kiro executor usage reporting

Kiro now uses the shared executor usage reporter for both non-streaming and streaming requests. The completed event accumulator supplies its estimated input and output token counts to the usage record, which feeds analytics and per-key limits. Non-2xx and transport failures are tracked as failures, while streaming publishes only after the terminal SSE chunks have been emitted. The reporter's one-shot publish guard makes the terminal success or failure outcome the only record for the attempt.

Completed usage uses Kiro's `contextUsagePercentage` and the selected wire model's
`tokenLimits.maxInputTokens` from `ListAvailableModels`. The catalog is scoped to
the credential, region and effective profile, follows pagination, and refreshes
after three hours. Failed refreshes retain the last successful catalog and retry
after one minute. Missing limits or invalid percentages preserve local estimates;
there is no hardcoded context-window fallback. Catalog requests respect credential
proxy routing and do not replace inference timing or upstream diagnostics.

The percentage estimates occupied context for the completed exchange. CPA subtracts
locally estimated output, including reasoning and tool inputs, to derive estimated
input. Explicit upstream counters take precedence, including zero. Counts remain
marked estimated because context occupancy includes provider overhead and does not
establish exact billable input/output. Streaming content is forwarded immediately;
the final `message_delta` corrects input and output usage for Claude clients and the
OpenAI translators. The local token-count endpoint uses the same Claude-request
estimator as inference instead of dividing serialized JSON bytes by four. Context
lookup reads the final wire model and profile after payload rules without changing
the configured business payload.

Schema prompt guidance participates in the same prepared Kiro representation used by inference and local estimation. Usage remains once-only, with explicit reported zero distinct from missing usage and model/profile context derived from the final wire request.

**Implementation evidence:** `internal/runtime/executor/kiro_executor.go`. `internal/runtime/executor/kiro_executor.go`,
`internal/runtime/executor/kiro_executor_usage_test.go`,
`internal/runtime/executor/kiro_stream_test.go`, and
`internal/runtime/executor/helps/kiro_models.go` with its unit tests.

The bundled CPAMC labels credit-billed Kiro allowances as credits with fractional amounts, shows the row percentage unrounded with the same formatter as the quota summary and timeline, and gives the Kiro timeline lane its monthly period so the current window draws a bar.

Kiro attempts retain the last valid `meteringEvent` credit snapshot as an exact,
bounded decimal quantity, including reported zero. Credits survive SDK delivery
independently of optional raw diagnostics and remain available on failures after
metering arrives. They are separate from estimated token counts and do not imply
cache-read or cache-write token measurements.

CPAUK prices metered Kiro attempts as reported credits multiplied by the configured
`kiro_usd_per_credit` rate, defaulting to USD 0.02 and treating every credit as an
included credit. Arithmetic is decimal and rounds once per attempt to nano-USD;
the measured credits already include the model multiplier. Explicit manual
model/alias overrides remain authoritative, including unknown-price overrides.
Automatic credit pricing precedes catalog token pricing. Missing metering retains
the existing token-price fallback or unpriced result. This conversion estimates
usage cost and does not reconstruct a subscription invoice.

The analytics database preserves reported credits and the applied rate alongside
the existing monetary cost and pricing source. Changing the rate affects subsequent
attempts; explicit repricing recomputes retained events from their stored credits.
Legacy pricing writes that omit the setting preserve its current value, and catalog
refreshes do not reset it. Migration 016 leaves older credit measurements null.
Events without metering cannot be retroactively credit-priced, and compacted history
keeps its existing repricing limits. JSON/CSV exports preserve the new metering and
rate fields.

CPAMC exposes the conversion rate in Pricing and the reported quantity, applied
rate and source in request details. Credit-based amounts remain part of normal
monetary totals and cost-per-token ratios, while the cost chart and model table
display only the existing token categories. The undisplayed amount is not assigned
to input/output/cache categories. Component coverage can be incomplete even when
the full monetary total is known. The API retains the separate amount for exact
accounting. Token volume and throughput retain their independent estimates. All
six locales describe the pricing basis and included-credit assumption.

Kiro requests without metering can use Amazon Bedrock rates from the models.dev
`amazon-bedrock` catalog when no other catalog rule covers the model. Anthropic
Bedrock IDs map to CPA Claude IDs by dropping the `anthropic.` prefix and `-vN[:M]`
suffix. The `global.` cross-region price wins over the bare regional ID, and regional
prefixes such as `us.` or `eu.` are ignored. An explicit `kiro` catalog binding takes
precedence within catalog pricing. Existing events keep their stored cost until a
reprice job runs (`internal/cpauk/store/pricing_discovery.go`,
`TestModelsDevFeedPricesKiroWithBedrockFallback`).

**Recorded validation:** On 2026-11-18, `internal/runtime/executor/kiro_executor.go` retains shared once-only usage and final-wire model/profile context lookup; schema guidance is included in the shared estimator representation. Reported-zero, estimated usage, streamed correction, credits/pricing and model-catalog tests pass.

Context-accounting regressions cover all three public protocols in both response
modes, final-stream corrections, catalog pagination and account/profile isolation,
concurrent refreshes, stale-on-error retention, missing models, invalid percentages,
explicit zero counters, and payload-configured wire model/profile selection. The
full Go suite, focused Kiro race tests, executor vet checks, and server build pass.
Live Kiro requests pass through Claude Messages, Chat Completions and Responses in
both modes with matching corrected usage. Native Claude Code retains the corrected
input count; an explicit `/compact` reduces a 140,258-token conversation to 884
retained tokens and the resumed session completes successfully.
The canonical Docker image also completes live buffered and streaming Kiro requests
with matching corrected usage.

Credit-pricing checks cover precise and bounded metering, explicit zero, failed
responses after metering, manual and unknown-price precedence, migration 015-to-016,
restart/catalog-refresh/legacy-write preservation, repricing and checkpoint changes,
mixed token/credit components, import validation, export parity, and backup restore.
Regressions also prevent stale live overrides after restoring an empty price book
and distinguish applied credit pricing from a caller-selected source label. The full
Go suite, focused race tests, vet, server build and canonical Docker build pass.
Both a native server and the Docker image complete all six Claude Messages, Chat
Completions and Responses modes with metering, stored cost, summaries, analysis and
JSON/CSV exports agreeing. A rate change survives restart and reprices retained
events. CPAMC passes 2,136 tests, lint, TypeScript and production build; desktop and
mobile CDP checks cover rate editing, event details and the token-category breakdown,
including hidden-credit totals and non-overlapping radar labels.

**Last updated:** 2026-11-18

### DL035 — Kiro streamed tool-input reconstruction

Kiro `toolUseEvent` frames stream the tool input as JSON string fragments and terminate with a `stop` frame that may omit `input`. The executor now aggregates fragments by `toolUseId`, then emits Claude `tool_use` blocks with an empty start object followed by an `input_json_delta`, which preserves arguments in Claude Code. Payload rules run once after Kiro translation and immediately before transport for both buffered and streaming requests. Malformed or non-object provider values are preserved as strings rather than emitting `null`, which keeps Claude Code's tool-input contract valid for both response modes.

Kiro reasoning events do not carry Anthropic-compatible signatures, so CPA retains their text for usage estimation but suppresses unsigned Claude thinking blocks. Text and tool blocks have explicit start, delta, and stop lifecycles. The event-stream parser validates frame bounds, AWS prelude and message CRCs, rejects null or malformed event payloads, and propagates exception frames instead of treating them as successful completion. All named tools are preserved; the executor does not impose the earlier synthetic 16-tool cap.

Text-less turns are sent to Kiro with empty `content` rather than an `(empty placeholder)` filler. Kiro accepts empty content for tool-use and tool-result turns, empty user turns, prefill, and synthetic leading or alternation turns. The filler was imitated by the model in agent loops, which echoed `(empty placeholder)` as visible assistant text before silent tool calls.

Kiro `payload.replace` targets explicit final-wire JSON paths only. System/developer/user role targets do not apply to the Kiro protocol. Execute and stream finalize the wire payload once, preserving authoritative filters and reconstructed tool arguments.

**Implementation evidence:** `internal/runtime/executor/kiro_stream_test.go`. `internal/runtime/executor/kiro_executor.go`, `internal/runtime/executor/kiro_translator.go`, `internal/runtime/executor/kiro_executor_usage_test.go`, `internal/runtime/executor/kiro_stream_test.go`, and `internal/runtime/executor/kiro_translator_test.go`.

**Recorded validation:** On 2026-11-18, `internal/runtime/executor/kiro_stream_test.go` retains fragmented tool-input reconstruction, valid block lifecycles and bounded CRC-checked AWS event parsing. Final-wire replacement runs once; role targets deliberately do not apply. Empty-turn, all-tool and stream regressions pass.

**Last updated:** 2026-11-18

### DL036 — GSR request fallback and quota-provider identity

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

Quota-fetch HTTP 429s and transport failures keep otherwise eligible credentials
in the routing pool. A refresh operation's own cancellation or deadline error
counts as a failed observation and uses the same fallback; selection stops only
when the client request's context is canceled or expires. Optional reset-credit
and grant failures preserve the last successful routing observation regardless
of HTTP status.

Quota refresh now resolves the provider from the configured `usage_probe` and
normalized credential aliases before selecting an endpoint, parser, or persisted
signal. This keeps z.ai and OpenCode Go credentials on their own usage services,
including OpenCode credentials served through the Codex adapter. Mixed-provider
routing reads each credential's provider-specific recovery signal, and OpenCode
Go uses its monthly window even for GLM model names. The same fallback applies to
the scheduler path and normal, streaming, and token-count selection. Upstream
plugin quota-provider IDs remain additive and distinct from the fork's
request-level quota-provider identity mapping.

Plugin exclusions constrain the pool before quota refresh and the full eligible RR fallback. Quota-provider identity remains separate from the adapter/model name, and the fallback cursor persists across requests.

**Implementation evidence:** `sdk/cliproxy/auth/quota_recovery_provider.go`. `sdk/cliproxy/auth/{conductor_selection.go,selector.go,scheduler.go,gsr_tier.go,quota_recovery_provider.go}`, `internal/api/handlers/management/usage_probe.go`, and their routing and provider tests.

**Recorded validation:** On 2026-11-18, `sdk/cliproxy/auth/quota_recovery_provider.go` keeps quota identity separate from adapter/model names; plugin exclusions are applied before refresh and RR fallback. Full eligible-pool, persistent cursor, mixed-provider, failed-probe and canceled-probe tests pass.

Follow-up regressions exercise real Claude and Codex HTTPS quota lookups returning
429 or disconnecting, plus optional reset lookups returning malformed HTTP 200
responses, 302, 400, 401, 403, 404, 408, 429, 500, or 503. They verify credential
selection and preservation of the saved reset expiry; successful empty lookups
still clear spent credits. Auth tests cover independent transport cancellations
and deadlines in single-provider and mixed-provider routing, and client cancellation.
The full Go suite, focused auth and management race tests, canonical panel build,
server build, and production Docker build pass. The Docker image served 12 normal
and 12 streaming chat requests with a 6:6 credential split in each mode while the
quota endpoint returned 429, then repeated that result with quota connection
failures. Each run made only two cached quota calls and completed all 24 requests.

**Last updated:** 2026-11-18

### DL037 — Kiro static model definitions for OAuth exclusions

`GET /v8/management/routing/model-definitions/kiro` returned `400 unknown channel`, so the CPAMC OAuth excluded-models editor showed Kiro as unsupported even though `oauth-excluded-models.kiro` filters Kiro's runtime models. The static channel lookup now serves the Claude catalog for `kiro`, the same list Kiro credentials register at runtime.

Credentials that only carry a `usage-probe` (for example Z.AI or OpenCode Go under `openai-compatibility`) remain unsupported there by design: they are API-key credentials, which `oauth-excluded-models` does not apply to. The bundled panel (CPAMC `5647a51`) therefore leaves them out of the OAuth excluded-models and model-alias pickers, and also carries the plan-weighted quota summary update.

**Implementation evidence:** `internal/registry/model_definitions.go`, `internal/registry/model_definitions_test.go`, the CPAMC gitlink, and the bundled management assets.

**Recorded validation:** a CPA built from this checkout returned the 18 Claude models for `kiro`, and still returned `400` for `openai-compatible-zai`; through CDP, both OAuth editors listed only OAuth providers and Kiro loaded the Claude catalog.

**Last updated:** 2026-10-14

### DL038 — Per-credential quota reserves

A credential can hold back part of any quota bar. `quota_reserve` (auth files) or `quota-reserve` (provider API-key entries, on the provider object for `openai-compatibility`) maps a bar ID, the same ID CPAMC renders (for example `five-hour`, `weekly`, `seven-day-fable`, `zai-week-1`, `opencode-go-monthly`, `kiro-included-credit-0`), to the minimum remaining percent (0-99). Once CPA observes a bar at or below its reserve, routing withholds that credential until the bar resets. Client keys listed by ID in `quota_reserve_bypass_keys` / `quota-reserve-bypass-keys` (SHA-256 key IDs from `GET /v8/management/api-keys` `key-identities`, never raw keys) can still use the reserve.

Checks are lazy: after a request result is recorded, a background check reads the credential's usage through the shared management quota cache, using the same URLs and headers as CPAMC's quota adapters, so a recent quota-page read costs no provider call. Each credential is checked at most once per five minutes, and concurrent or retried requests collapse into one in-flight check. Management quota reads also apply reserves. Whether a reserve holds is derived from the last observed bars against the current reserves, so editing or removing a reserve takes effect immediately. A bar whose reset passes stops counting; a held bar with no known reset stays held until a later check sees it above the reserve. Supported providers are Claude, Codex, Z.ai, OpenCode Go, and Kiro.

Requests carry the client key ID in execution metadata (`client_key_id`, config-key principals only). While any reserve holds a credential, selection uses the legacy candidate loops instead of the scheduler fast path, because the scheduler index cannot apply per-request bypass. The credentials listing exposes `quota_reserve`, `quota_reserve_bypass_keys`, and the active `quota_reserve_state`. Config validation and `PATCH /v8/management/credentials/fields` reject out-of-range reserves and bypass entries that are not key IDs.

The bundled panel (CPAMC `a0d3fa2`) adds the quota reserve editor to the auth-file sheet and the Z.ai/OpenCode Go provider form, a draggable handle per bar, a searchable bypass-key picker, candy stripes in the bar's own colour on the part of the fill inside the reserve (on quota rows and provider summary cards), reserve details in row and segment tooltips, and a Reserve reached badge.

Codex alpha-search selection carries the same config-client key hash as ordinary execution, so quota-reserve bypass keys can use held credentials.

Codex continuity applies reserves and bypass keys before candidate construction. Muse relay actions preserve native config-key identity for bypass eligibility. Terminal pool-cooldown reporting also uses `m.eligibilityForRequest`, so reserve-held credentials cannot suppress Retry-After for the usable pool.

**Implementation evidence:** `sdk/cliproxy/auth/conductor_pool_cooldown_test.go`. `sdk/cliproxy/auth/conductor_selection.go`. `sdk/cliproxy/auth/quota_reserve.go`, `sdk/cliproxy/auth/{conductor_selection.go,conductor_cooldown.go}`, `sdk/api/handlers/handlers.go`, `internal/api/handlers/management/{quota_reserve_probe.go,auth_files_fields.go,api_tools.go,auth_files.go}`, `internal/config/{config_types.go,vertex_compat.go,credential_labels.go}`, `internal/watcher/synthesizer/{config.go,file.go}`, their tests, the CPAMC gitlink, and the bundled management assets. `internal/api/server_routes.go` reuses `RequestClientKeyID` from `sdk/api/handlers/handlers.go`; `internal/api/server_codex_search_reserve_test.go` exercises authenticated HTTP through the real Codex executor and reserve selection.

**Recorded validation:** `sdk/cliproxy/auth/conductor_pool_cooldown_test.go`, `TestPoolCooldownTerminalErrorIgnoresReserveHeldCredential`, covers reserve-aware terminal pool reporting in `earliestPoolCooldownForModelAt`.  On 2026-11-18, `sdk/cliproxy/auth/conductor_selection.go` retains model-aware reserve/bypass eligibility and scheduler bypass while held; client identity reaches Codex search and Muse. Reserve edit/reset/partial-bar, alpha-search HTTP, Muse HTTP and continuity eligibility tests pass.

**Last updated:** 2026-11-18

### DL039 — Streaming keep-alives before the first upstream chunk

`streaming.keepalive-seconds` previously started only after the first upstream chunk, because the SSE handlers hold the response until then to return fast failures with their real status. While credential selection, cooldown waits, retries, or the upstream's time to first token were pending, the client received nothing, and proxies in front of CPA cut those requests: nginx's 60s `proxy_read_timeout` (504) and Cloudflare's 125s origin timeout (524). Cloudflare forwards `103 Early Hints` but does not reset its timer on it, so an interim status is not an alternative.

The Claude Messages, OpenAI Chat Completions, legacy Completions, OpenAI Responses, and Gemini SSE stream handlers now run stream setup in the background through the pre-first-token keepalive path. Nothing is written until one keep-alive interval passes without a first chunk or error, so fast failures still return their JSON error and HTTP status. After that the handler commits HTTP 200 with SSE headers and writes `: keep-alive` every interval until the first outcome. Once committed, the first chunk and later chunks go through the handler's existing forwarder, and a late failure is delivered as that protocol's in-stream error event (`event: error` for Claude, a `response.failed` or `error` event for Responses). Upstream response headers cannot reach the client after an early commit. `send-keepalive-before-ft` is tri-state: when unset, a positive `keepalive-seconds` preserves the fork behavior and enables pre-first-token keepalives; `true` enables them; `false` disables them. Gemini `alt` (non-SSE) output remains unchanged.

Opt-in Codex continuity may emit keepalives during an active quota wait even when ordinary pre-first-token keepalives are disabled. Wait-only ticks run only when the ordinary ticker is absent, preserving tri-state behavior without double keepalives.

**Implementation evidence:** `sdk/api/handlers/stream_forwarder.go`. `internal/config/sdk_config.go` decodes flag presence; `sdk/api/handlers/{handlers.go,stream_forwarder.go}` implement `ExecuteStreamWithPreFTKeepAlive`, used by `sdk/api/handlers/claude/code_handlers.go`, `sdk/api/handlers/openai/{openai_handlers.go,openai_responses_handlers.go}`, and `sdk/api/handlers/gemini/gemini_handlers.go`. Current-path coverage includes `sdk/api/handlers/handlers_stream_bootstrap_test.go`, `sdk/api/handlers/claude/code_handlers_keepalive_test.go`, and `sdk/api/handlers/openai/openai_responses_handlers_stream_error_test.go`. `stream_first_chunk.go` remains unused by production handlers.

**Recorded validation:** On 2026-11-18, `sdk/api/handlers/stream_forwarder.go` retains tri-state pre-first-token SSE handling in Claude/Chat/Completions/Responses/Gemini; continuity adds the active-continuity-wait exception. Bootstrap, late-error and continuity HTTP/WebSocket tests remain and pass.

**Last updated:** 2026-11-18

### DL040 — Home-mode Claude OAuth safeguard compatibility

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

### DL041 — Short Codex overload cooldown

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

### DL042 — Local-time compact analytics reads

CPAUK timeseries queries retain the requested IANA timezone for calendar bucket semantics and response metadata while reading UTC 15-minute materialized storage. Non-UTC queries regroup compact aggregate rows through the existing timezone bucketer and deduplicate request identities across 15-minute bins, preserving local-day, fixed-width, and DST bucket correctness. Queries with unaligned boundaries or retained-history overlap still use the existing exact paths. Calendar boundary resolution now handles zones that skip local midnight instead of producing zero-length day buckets.

**Implementation evidence:** `internal/cpauk/store/{materialized_read.go,query.go,materialized_15m_test.go}`, `internal/cpauk/aggregate/{range.go,aggregate_test.go}`.

**Recorded validation:** fixed-offset, unaligned rolling-range, New York DST, and skipped-midnight Havana/Azores compact-versus-raw and bucket-sequence parity tests pass; a 100k-request fixture comparison reports Asia/Bangkok 1-hour timeseries falling from 479.272 ms to 207.698 ms median (56.7% faster), with 2,160 points and exact raw-path parity. The hybrid unaligned-range check measures 217.700 ms after keeping only the edge minutes on the raw path. `go test ./internal/cpauk/...` and `go build -o cli-proxy-api ./cmd/server && unlink cli-proxy-api` pass. A full `go test ./...` run remains affected by the unrelated existing `internal/api/handlers/management/TestV0RepeatedSavesKeepLegacyClientAlias` failure.

**Last updated:** 2026-10-16

### DL043 — Shared range-based analytics responses and conditional reads

First-page explicit and custom range cache keys floor both endpoints to UTC minutes. Seconds and fractional seconds no longer split server snapshots or CPAMC validators. Computation and `meta.range` retain the first request's exact bounds. Cursor continuation keys remain exact so signature validation and pagination stay tied to that snapshot. Adjacent minutes, timezones, and filters remain distinct, and TTL tiers identify separate entries even when minute-rounded bounds match.

The v8 management analytics query operations share successful responses after computation, bounded to 128 entries and 16 MiB. TTL follows the exact resolved range before minute grouping: up to 6 hours uses 1 minute, up to 24 hours uses 3 minutes, up to 7 days uses 15 minutes, up to 30 days uses 30 minutes, and longer ranges use 1 hour. Cache identity includes the operation, original named or explicit range, timezone, normalized filters, key scope, bucket width, pagination, and sort. Concurrent matching requests share one calculation. Rolling presets reuse the cached response's resolved range during that cache interval; ordinary intake intentionally permits this bounded staleness. Configuration, pricing, service, and maintenance changes invalidate snapshots, and unavailable services cannot return cached results.

Authenticated `GET /v8/management/analytics/query` carries base64url JSON in `X-Analytics-Query`, keeping filters and full key IDs out of URLs. Responses include ETags; a matching `If-None-Match` returns an empty `304`. Existing POST requests share the cache and continue returning `200` bodies. Errors are not cached, and legacy v0 and public viewer routes retain their prior behavior.

CPAMC retains validators and response bodies in bounded, connection-scoped memory, reuses bodies after a `304`, and clears them on connection changes or sign-out. Large queries use POST, and older servers without the GET route retain POST compatibility. Completing a manual page refresh restarts the automatic refresh interval at 60 seconds; hidden-tab, concurrent-refresh, and Retry-After guards remain in place.

**Implementation evidence:** `internal/api/handlers/management/analytics_response_cache.go`, the v8 route and CORS registration, `internal/cpauk/service.go`, their response-cache tests, and the CPAMC analytics API and refresh coordinator.

**Recorded validation:** deterministic GET revalidation and POST reuse tests cover explicit, custom, and rolling ranges, exact inclusive TTL thresholds, nanosecond-over-threshold ranges, expiry after calculation, and isolation between TTL tiers with the same minute-rounded bounds. `go test ./internal/api/handlers/management ./internal/api ./internal/agentdocs -count=1` passes against an isolated Docker ClickHouse fixture, including authenticated HTTP cache reads and exact cursor continuation. `go test -race ./internal/api/handlers/management -run '^TestAnalyticsResponseCache' -count=1` passes. The canonical CPAMC bundle build and disposable `cmd/server` compile pass.

**Last updated:** 2026-11-02

### DL044 — Preserve unavailable provider metrics

CPAUK now calculates cache-read rates from the exact-token subset of a range, so estimated Kiro
requests do not hide cache metrics reported by other providers. CPAMC keeps estimated
input/output/total token counts visible while showing cache read, cache creation, cached, and
reasoning metrics as unavailable only when no usable exact subset exists. Exact providers retain
legitimate zero values. The same rule applies to overview sparklines, event cache-rate columns,
and event token details. Timeseries buckets carry the exact-subset cache rate too, so a mixed
bucket still renders a graph point instead of dropping the whole bucket. Regression coverage covers
raw, compact, and retained mixed-quality timeseries plus the CPAMC overview model; the CPAMC
submodule was rebuilt through the canonical management-center build.

**Implementation evidence:** `internal/cpauk/store/{query.go,retained_query.go,materialized_read.go}` and `internal/cpauk/model/result.go`, with tests in `internal/cpauk/store/query_v2_test.go`.

**Recorded validation:** On 2026-10-30, `TestV2SummaryExcludesEstimatedRowsFromCacheRate`, `TestV2TimeseriesRetainsExactSubsetCacheRateInMixedBucket`, and `TestOptionalCacheReadRateIsUnavailableForEstimatedTokens` pass.

**Last updated:** 2026-10-30

### DL045 — Reliable Claude Code reference capture

The private Claude Code reference updater repairs a missing activation pointer from a complete
installed version directory before attempting another network update. Older installations whose
version directory is directly under `claude-reference` are migrated into the current
`versions/<version>` layout. Reference update failures retry after five minutes.

Capture failures retain at most 16 KiB of probe output, redact the OAuth token, and preserve
bounded-memory behavior. Reference refresh tries every eligible direct Claude OAuth credential in
refresh order until one capture succeeds, while attempting identical access tokens only once.
Manual refresh and the background updater use the same fallback behavior.

`docker-compose.yml` and `docker-compose-web.yml` keep the reference root on the `claude-reference`
named volume. Without it, every container recreation discards the capture and the OAuth safeguard
rejects Claude OAuth requests with a local 503 until the updater reinstalls and captures again;
callers with other eligible providers then see that provider's error instead. DL076 records that
refusal as a failed analytics attempt.

**Implementation evidence:** `internal/claudecapture/{update.go,capture.go,private_test.go,capture_test.go}`,
`sdk/cliproxy/service_claude_reference.go`,
`sdk/cliproxy/service_claude_reference_test.go`, `docker-compose.yml`, and `docker-compose-web.yml`.

**Recorded validation:** pointer recovery, legacy-layout migration, bounded capture diagnostics,
ordered credential fallback, focused capture/executor/service suites, and the disposable server
build pass. With the web stack, a Claude Code request sent right after `--force-recreate` completed
about 2 s after container start; recreating from the stack without the volume reproduced the
missing-`active.json` safeguard error, and the same request took 70 s.

**Last updated:** 2026-11-17

### DL046 — Streaming telemetry and same-format Claude streams

CPA assembles multiline SSE `data:` fields before classifying token and usage events, keeps provider, TTFT, and first-token latency metrics non-decreasing, and forces `stream: true` for Claude streaming requests even when the client already uses Claude's native format. This preserves usage telemetry and stream framing without changing the selected response protocol.

Claude usage and serving-model observers read valid source events before Thread buffering. Terminal withholding cannot lose observed usage on a reset or missing message_stop. Buffered and streaming failures retain partial tokens exactly once. Source-reader timing precedes buffering and ends at a complete message_stop, so a delayed terminal event does not compress or extend the arrival window.

Source-reader token observation precedes Thread terminal buffering and Antigravity zero-output buffering. Native Claude forces streaming before final payload rules and retains once-only partial usage on failures.

**Implementation evidence:** `internal/runtime/executor/claude_executor_stream.go`. `internal/runtime/executor/helps/generation_body.go`, `internal/runtime/executor/helps/usage_helpers.go`, and `internal/runtime/executor/claude_executor_stream.go`.

**Recorded validation:** On 2026-11-18, `internal/runtime/executor/claude_executor_stream.go` still observes native Claude source reads before Thread buffering, forces streaming before final rules, assembles SSE data, and publishes partial usage once. Generation/usage helpers survive; executor and handler regressions pass.

**Last updated:** 2026-11-18

### DL047 — Claude safeguard capture failures fall back across providers

When the direct Claude OAuth safeguard cannot load a current Claude Code
capture, the selected Claude credential is now treated as unavailable for that
attempt. Mixed-provider routing can continue to a Kiro credential serving the
same Claude model, whose native runtime does not use the Claude OAuth
safeguard. Genuine safeguard rejections of the caller request remain
request-scoped and still stop processing without cooling credentials.

A local capture-reference-unavailable 503 remains eligible for mixed Claude/Kiro fallback. Recognized request-scoped safety-policy 400 responses stop cross-credential/provider retries, including Kiro, without cooling the rejecting credential.

**Implementation evidence:** `sdk/cliproxy/auth/conductor_claude_safeguard_routing_test.go`. `internal/runtime/executor/claude_oauth_safeguard.go`,
`sdk/cliproxy/auth/conductor_cooldown.go`, and
`sdk/cliproxy/auth/conductor_claude_safeguard_routing_test.go`.

**Recorded validation:** On 2026-11-18, `sdk/cliproxy/auth/conductor_claude_safeguard_routing_test.go` still asserts actual Claude-then-Kiro attempts for local 503 across execute/count/stream and Claude-only for request rejection. Safeguard classification survives U6 policy stops; normal and race runs pass.

**Last updated:** 2026-11-18

### DL048 — Key catalog reports the top-cost model

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

### DL049 — Model leaderboard and privacy-preserving session digests

CPAUK accepts the v2 `model_leaderboard` analytics operation and returns four fixed awards in
one response: distinct sessions used, request reliability, P70 qualified arrival throughput, and P95
generated tokens per successful attempt. Every award is global across keys, providers, and
credentials, contains at most six contiguous ranks, and returns an empty row array when no model
qualifies. Reliability requires at least 200 upstream attempts; speed requires a P70 of at least
80 tokens per second and includes only successful attempts with qualified arrival evidence.

CPAMC renders these four fixed award headers and descriptions before the ranking query finishes,
with neutral skeletons replacing the ranking decorations until data arrives, and reserved podium
and runners-up space for empty or partial results. The range chip reserves its localized dates,
pending mascot images show the family lab logo. Loaded mascot images use block layout to
fit the same inner frame without inline baseline overflow; rank medals remain unclipped.
Speed Maniac states its P70 qualification
floor of 80 tok/s in every supported locale.
The existing four-award contract supplies the card order and count without an additional API.
The shared skeleton uses distinct theme-derived gradient colors, including White and Light
themes whose hover and tertiary backgrounds are identical, and respects reduced-motion settings.

The collector hashes the trimmed upstream session identifier into a nullable `session_digest`
field for distinct-session ranking. Migration 12 leaves historical rows null. Separately,
DL057 stores a bounded session display identifier for authenticated event diagnostics; the digest
is not a promise that event storage contains no session identifier. Throughput reuses the qualified event arrival-window calculation from DL052. The canonical `normalized-v1` output total already includes
reasoning for every provider accounting style, so the talkative award adds reasoning only for
legacy or imported schemas that store separate output buckets. When a valid token breakdown
cannot supply its output total and the intake adapter falls back to the provider's raw output,
the event is stamped `raw-fallback-v1`; those attempts are excluded from most_talkative because
the raw output's relationship to reasoning is provider-dependent. The same events still count
toward the other awards.

**Implementation evidence:** `internal/cpauk/{collector,model,store}`, `internal/cpauk/service.go`,
and `internal/api/handlers/management/analytics.go`.

**Recorded validation:** On 2026-11-13, the analytics/storage/config audit passes all 18 tested
packages against real isolated ClickHouse databases. Sanitizer and leaderboard regressions cover
serialized display IDs and digests, qualification floors, distinct provider sessions, six-row ranks,
inclusive reasoning and raw-fallback exclusions. Management handlers and focused API checks pass.

**Last updated:** 2026-11-13

### DL050 — Management panel bundle is build output; mascots ship as separate assets

`internal/managementasset/bundled/` is no longer committed: only `.gitkeep` is tracked and
`scripts/build-management-center.sh` regenerates `management.html`, the manifest, and
`management-assets/` from the pinned `web/management-center` submodule. The embed uses
`all:bundled`, so a fresh clone still compiles with plain `go build`. That build reports
`ErrBundleMissing` instead of an internal error: `/management.html` answers 503 with a short
page naming the build script (logged once at warn) while a valid mutable panel and the panel
auto-updater keep working exactly as before.

The leaderboard mascots moved out of `management.html` into optional WebP files under CPAMC
`public/management-assets/mascots/`. CPA serves them through `GET|HEAD /management-assets/*` from
the embedded `management-assets/` subtree only, under the same home/disabled gates as the panel,
without a management key and without the example-API-key safe-mode gate. Request paths must be
clean relative names, so `..`, encoded traversal, directories, and unknown files are 404 and
nothing is listed. Responses take their content type from the extension and
`Cache-Control: public, max-age=86400`; a missing asset makes the panel fall back to its logo.

Build plumbing follows the layout: the script copies `management-assets/` and only requires a
clean submodule by default (`ALLOW_DIRTY_PANEL=1` for local builds), `Dockerfile.management`
exports the assets, and both runtime Dockerfiles rebuild the panel from the pinned submodule in a
Bun stage and generate the manifest inside the build from `CPAMC_COMMIT` and `BUILD_DATE` because
`.dockerignore` excludes `.git`. The release workflow builds the bundle once on Linux and every
matrix target, including the macOS and Windows runners without Docker, downloads it before
`go build`; archives ship the HTML, manifest, and assets together.

Runtime Docker images compile the panel source supplied in their build context with Bun 1.4.3 and generate the compatibility manifest from the supplied `CPAMC_COMMIT`; CI verifies clean submodule pins before those builds. The separate canonical builder enforces its own clean-tree contract.

**Implementation evidence:** `internal/api/server_management.go`. `internal/managementasset/{bundle.go,bundle_test.go}`,
`internal/api/{server_management.go,server_routes.go,server_middleware.go,server_management_assets_test.go}`,
`scripts/build-management-center.sh`, `Dockerfile`, `Dockerfile.web`, `Dockerfile.management`,
`.github/workflows/{release.yaml,pr-test-build.yml,docker-image.yml}`, `nginx-web.conf`,
`docs/management-center.md`, and `docs/analytics-operations.md`.

**Recorded validation:** On 2026-11-18, `internal/api/server_management.go` retains missing-bundle 503 and gated embedded GET/HEAD assets. Canonical builder, Docker/release asset flow and gitignore remain; asset/traversal/safe-mode/missing-bundle tests pass. Bundle revision validation is excluded as requested.

**Last updated:** 2026-11-18

### DL051 — Freeze closed days in overview activity

Both CPAUK backends split overview activity at the current local calendar-day
boundary in the configured analytics query zone. The bounded historical result
cache reuses the closed-day prefix while today's buckets are recalculated, so new
usage appears immediately. Late events and response patches use event time to
invalidate closed history; unrelated tail writes and today's response patches leave
it reusable. A selected range ending earlier today remains live in ClickHouse.

The shared LRU is bounded to 128 entries and 32 MiB of encoded response and key
bytes. SQLite keeps its durable range generations and authoritative `daily_stats`
retention path. ClickHouse uses a bounded mutation journal for keys and reads published
raw/retained accounting and exact request identities on cache misses. Retention,
pricing, purge, rollback, restore, epoch changes, legacy publication, and late
recovery invalidate the ClickHouse cache before readers can reuse results. Active
imports and known pending late publications remain uncached. Calendar boundaries
use the query's IANA zone, including skipped midnights, DST, and fractional offsets;
today and future days are never frozen.

**Implementation evidence:** `internal/cpauk/backend/history_cache.go`,
`internal/cpauk/store/{activity.go,history_cache.go,activity_test.go}`, and
`internal/cpauk/chstore/{activity.go,history_cache.go,activity_cache_test.go,history_cache_test.go,SCHEMA.md}`.

**Recorded validation:** controllable-clock tests on real ClickHouse cover UTC,
America/St_Johns, America/Santiago, Australia/Lord_Howe, and Asia/Kathmandu. Repeated
closed-prefix reads issue zero SELECTs, a mixed historical/today read still queries
today, tail events and patches preserve the prefix, late events force a miss,
advancing the local date freezes the newly completed day, and cached results match
uncached accounting. Shared-cache eviction and defensive-cloning tests pass.
`go build ./...` and `go vet ./internal/cpauk/...` pass.
`go test -count=1 ./internal/cpauk/... ./internal/api/...` passes against the running
ClickHouse Docker test container.

**Last updated:** 2026-10-31


### DL052 — Qualified arrival rates and timing evidence

TPS no longer subtracts a historical response-latency median or compares generation duration
with a fraction of pre-generation waiting time. Arrival throughput uses a bounded cumulative
output collector at the upstream reader, excluding the entire first substantive read batch
from both the counted token window and its elapsed interval. The collector tokenizes the first
batch prefix and final content at publication rather than summing independently tokenized
fragments. Token estimates are marked separately from observed timing. Hidden reasoning usage
is never added to a visible-delta numerator.

A public arrival rate requires at least three distinct substantive reads, a 100 ms span, and
16 tokens in the matched window. These are minimum sampling requirements, not speed ceilings;
well-observed rates above 1,000 TPS remain supported. Unsupported, malformed, oversized,
backpressured, and insufficient observations retain explicit unavailable reasons. The bounded
collector stores counts and timing only, never response text. Effective output rate uses the
whole attempt duration and consistent normalized output usage, and remains a separate metric.

Kiro's Amazon binary EventStream is observed before translation on both streaming and
nonstreaming client paths. Codex and xAI WebSocket observations retain source-read timestamps
through delivery queues. Nullable `output_timing` evidence records protocol/source, monotonic
arrival offsets, distinct batch count, aligned token count and quality, coverage, substantive
byte totals and batch sizes, and the longest interarrival gap. Migration 013 also preserves
client stream mode and recorded reasoning preference. The preference is a request hint, not
proof of the final upstream setting after payload overrides. Legacy rows keep these fields
unknown; importing or restoring data does not manufacture evidence or persist derived rates.

Event list/detail, JSON/CSV exports, the public viewer, and the management panel use the same
rate contract. The speed leaderboard includes successful qualified arrival observations only,
with qualified samples and successful-attempt coverage. It does not rank whole-attempt rates,
failed attempts, or predictions as observed arrival speeds. Existing raw first-to-last timing
aggregates remain available and are labelled as arrival spans.

The initial telemetry draft deferred prediction until observed history was available. DL053
adds guarded estimates for eligible unmeasured attempts. Historical rows without this evidence
remain unavailable; external benchmarks never clamp measurements.

Upstream direct-plugin TTFT uses first-packet fallback and protocol-aware events. It does not provide source-reader arrival evidence, so plugin delivery chunks alone cannot qualify for arrival throughput. `internal/pluginhost` TTFT regressions cover that boundary alongside the executor source-reader tests.

Substantive source observations occur before Antigravity zero-output buffering, preserving qualified first-batch exclusion, generation timing and bounded arrival evidence.

**Implementation evidence:** `internal/runtime/executor/helps/output_observation.go`. `sdk/cliproxy/usage/output_timing.go`,
`internal/runtime/executor/helps/output_observation.go`,
`internal/runtime/executor/helps/kiro_generation_body.go`,
`internal/cpauk/model/output_timing.go`,
`internal/cpauk/store/migrations/013_output_timing.sql`, and the CPAMC analytics consumers.

**Recorded validation:** On 2026-11-18, `internal/runtime/executor/helps/output_observation.go` and Kiro/source-reader observers retain matched first-batch exclusion and bounded arrival evidence before new Antigravity buffering. Model qualification, accounting/export/viewer and observer tests remain and pass; timing contract files are unchanged.

**Last updated:** 2026-11-18

### DL053 — Guarded historical arrival-rate estimates

Unmeasured successful attempts can receive a separate `throughput_estimate` from recent
comparable observations. The estimate never replaces `throughput`, its compatibility aliases,
or the whole-attempt effective rate. Event lists, details, JSON/CSV exports and public viewers
share the contract. The panel marks the fallback as Estimated, shows the historical P10/P90
range and sample/validation evidence, and never derives a timing bar from a predicted rate.
Measured-speed rankings continue to exclude predictions. The compact `EST` label identifies
only historical predictions. Observed arrival rates and whole-attempt effective rates do not
receive that badge merely because their token counts
are approximate; details and tooltips describe local token counting separately.

History is isolated by client key, provider, model, executor, credential, protocol, reader
source, coverage, request mode, reasoning preference and service tiers. Unknown preferences
match only unknown preferences. Only native successful observations with at least one second,
64 matching tokens and ten distinct arrivals can train the predictor. Prediction values are
never labels. History is limited to the latest 200 unique qualifying requests in 24 hours,
completed strictly before the target started; the newest observation must be within two hours.
No model whitelist or TPS ceiling is applied.

The latest 20 observations validate a frozen median and empirical P10/P90 range fitted on at
least 50 earlier observations. Training must also have completed before the earliest validation
request started, preventing overlapping requests from leaking later results into validation.
Median absolute percentage error must be at most 25%, its 90th percentile at most 60%, and
historical-range coverage at least 75%. These are historical admission checks, not guarantees
about future accuracy or confidence probabilities. Sparse, stale, unstable or ineligible
history produces an explicit unavailable result. Source-reported reasoning preference remains
a hint, not proof of the final applied setting.

Migration 014 adds a partial expression index for strict cohort and completion-range lookups.
Queries read only label evidence, stop after 200 qualifying unique requests, and do not depend
on display filters or pagination. Optional history-read failures suppress the estimate without
failing the accounting read. Imported derived predictions are discarded and imported rows do
not train the estimator. Client-key isolation also protects shared-viewer history metadata.

**Implementation evidence:** `internal/cpauk/model/throughput_estimate.go`,
`internal/cpauk/store/throughput_estimate.go`,
`internal/cpauk/store/migrations/014_arrival_estimate_index.sql`, shared analytics exports/viewers,
and the CPAMC event table and detail diagnostics.

**Recorded validation:** The full Go suite, canonical embedded-panel build and server build pass.
CPAMC passes 2,120 tests, lint, type checking and its production build. Regression tests cover
future and overlapping completions, sparse/drifted history, client-key isolation, imports,
filter/pagination invariance, malformed optional timing, migration 013-to-014 and indexed
lookup plans. A 10,000-row legacy-noise benchmark retained roughly 1 ms detail lookups.
Independent review also swept 5,593 captured events across 12 pages and reproduced all 23
unmeasured-event estimates; API/export/viewer and observation-preservation checks pass.

Actual Go replay on 5,433 captured attempts admitted 430 of 4,090 measured successful targets
for out-of-sample evaluation, with 15.1% median error and 61.8% P90 error. A separate later set
admitted 12 measured targets with 7.2% median error; another later set admitted none, correctly
leaving accuracy unclaimed. The 23 unmeasured estimates have no accuracy ground truth. These
selective, session-correlated results do not establish general calibration; Sol retained large
tail errors. No model whitelist or gate thresholds were tuned to hide those limitations.

**Last updated:** 2026-10-25

### DL054 — Kiro model ID normalization

Kiro wire IDs use dotted minor versions, including `claude-fable-5.1`. The
normalizer now includes Fable, removes CPA reasoning suffixes before conversion,
and strips Claude context and release-date suffixes. A dated major-only model
such as `claude-opus-5-20260724` resolves to `claude-opus-5` instead of treating
the date as a minor version. Both current and historical user messages receive
the normalized ID before payload rules run.

Kiro registration uses its credential-scoped live catalog, described in DL072.
The normalizer translates the public Claude names back to their Kiro wire IDs.

**Implementation evidence:** `internal/runtime/executor/kiro_translator.go`,
`internal/runtime/executor/kiro_translator_test.go`, and
`internal/runtime/executor/kiro_executor_models_test.go`.

**Recorded validation:** the full executor package tests, executor `go vet`, and
server build pass. Regressions exercise wire IDs in both executor modes with
Claude Messages, Chat Completions, and Responses input, and retain the existing
final-payload ordering check. A disposable CPA server built from this checkout
completed 13 live Kiro requests: Fable 5.1 and Sonnet 5 with a reasoning suffix
through all three public protocols, streaming and buffered, plus Opus 5.5 with
a reasoning suffix. Every response contained the requested text and streaming
responses reached their terminal event.

**Last updated:** 2026-11-12

### DL055 — Local storage usage in Config Panel

The authenticated `GET /v8/management/storage` endpoint reports local CPA log and data
allocation, other occupied space, usable capacity, and available bytes for each backing
filesystem. Usable capacity excludes filesystem-reserved blocks. CPA data covers the auth
and state directories, the active analytics database and sidecars, local storage mirrors,
plugins, configuration, and mutable management assets. Backups inside these managed locations
are included; remote storage and backups elsewhere are excluded. The endpoint accepts no scan
paths from clients and returns file locations and sizes without reading file contents.

Accounting deduplicates overlapping directories and hard links, counts allocated storage
rather than sparse-file logical size, resolves configured symlinks without following nested
symlink targets, and reports partial scans. A custom analytics database does not claim its
entire parent directory. Its active path is measured when a new configured path awaits restart.
Missing directories still expose the capacity of their existing filesystem. Scans are coalesced
and cached for 15 seconds, with invalidation when measured locations change.

In ClickHouse server mode (DL060) the response adds a `clickhouse` object with reachability,
the database, and one breakdown per ClickHouse disk: CPA analytics, request logs and transcripts,
other CPA tables, other databases' active parts, and remaining disk usage. Only the configured
CPA database is attributed to CPA. Racing disk and part readings are capped and marked partial.
Local volumes remain reported when ClickHouse is unreachable.

CPAMC adds a theme-aware stacked bar and a numeric legend to Collection and storage, with
one breakdown per filesystem, refresh, a measurement timestamp, and expandable measured
locations. All six locales include loading, unavailable, unsupported, and partial-result copy.

When local KV is enabled outside Home mode, data allocation includes the effective KV database and its actual dot/dash sidecars, even outside other scan roots. Each file is measured separately without claiming its shared parent; overlap and hard-link deduplication still apply. Disabled/Home mode omits this explicit local KV location, and path or mode changes invalidate the cached location set. `TestAuditStorageIncludesCustomKVDatabase` and `TestStorageUsageKVFilesAndModeChanges` in `storage_test.go` cover external paths, sidecars, mode changes, and cache invalidation.

The management handler retains both the storage meter and ClickHouse handle alongside new upstream fields; the v8 storage route keeps allocated-space, sidecar and unavailable-backend accounting.

**Implementation evidence:** `internal/api/handlers/management/storage.go`. `internal/storageusage/` (including `clickhouse.go`),
`internal/api/handlers/management/{storage.go,storage_test.go,handler.go}`,
`internal/api/{server_management_v8.go,server_management_v8_test.go}`,
`internal/cpauk/service.go`, and the CPAMC storage API and Config Panel components.

**Recorded validation:** On 2026-11-18, `internal/api/handlers/management/storage.go` remains wired at v8 /storage with preserved Handler meter/ClickHouse fields. Allocated-space, custom DB/KV sidecars, cache invalidation and unreachable-ClickHouse tests remain unchanged and pass.

**Last updated:** 2026-11-18

### DL056 — Per-model cost series by service tier

The v2 analytics query accepts a `model_cost_series` operation for one model under the current
range, time zone, key, and provider selection. It returns zero-padded buckets grouped by
provider and effective service tier. Each tier carries token components, USD cost components,
effective per-1M rates, and input/output multipliers. Recorded pricing snapshots (DL057) take
precedence; legacy events without a snapshot are reconstructed from one pricing result per
event, and unavailable breakdowns mark the response partial. The scan honors query
cancellation. CPAMC loads this series on demand for its model cost modal.

**Implementation evidence:** `internal/cpauk/model/model_cost_series.go`,
`internal/cpauk/model_cost_series.go`, `internal/cpauk/store/model_cost_series.go`, the
operation validation in `internal/cpauk/model/query.go`, and v8 dispatch in
`internal/api/handlers/management/analytics.go`.

**Recorded validation:** store and v8 handler tests cover tier grouping, components, filters, time zones,
recorded versus reconstructed pricing, canceled rows, and cancellation during a long scan.
Reconstructing 100,001 legacy rows dropped from 7.06 s to 2.20 s after removing repeated
pricing work.

**Last updated:** 2026-10-28

### DL057 — Recorded event pricing and client session IDs

Migration 018 adds a `pricing_breakdown` snapshot and `session_id` to events. The pricing
engine records the selected input, output, cache read, and cache write rates, token counts,
component costs, the service tier, and the tier's input/output ratios to base rates. Component
costs use cumulative nano-USD rounding, so they sum exactly to the stored total. Kiro events keep
their provider-reported credits and applied USD-per-credit rate without token-rate components.
Maintenance repricing updates snapshot and total together. Native imports keep a recorded
snapshot after validating its arithmetic, rule, and source; ordinary intake ignores supplied
pricing evidence, and missing or invalid snapshots are repriced. Historical rows keep a null
snapshot.

Session IDs are captured from the usage record or the client request metadata, bounded, and
stored with a source prefix so namespaces cannot collide. Reads return the raw ID and its source
separately, and identities derived by CPA (`derived`, `ctx`, `lcp`) are reported as not sent by
the client. The events query accepts an exact Session ID filter on raw client IDs through
`events_session_id_idx`. Public viewer responses do not include session or pricing fields.

Intake chooses the returned service tier first, including the detail-level returned tier, then the effective outbound tier, then the client preference for pricing. The opt-in `upstream.codex.assume-requested-service-tier` option makes a nonempty effective outbound tier take precedence for Codex-provider usage, because Codex can return `default` even when the outbound request specifies `priority`. This is a pricing assumption, not confirmation of upstream scheduling. It defaults to false, supports hot reload for subsequent intake, and leaves other providers and records without an outbound tier unchanged. Raw returned tiers remain in the upstream usage evidence, and the client preference remains stored separately. Persisting the selected tier in `service_tier_used` keeps DL056 model cost grouping and recorded pricing snapshots consistent. Existing recorded events are not changed. CPAMC exposes the option as a toggle in the Config Panel's Codex settings.

Upstream cached image/audio counts remain diagnostic subcounts of cache-read tokens. The fork's `Detail.Clone` deep-copies their pointers before observer delivery; pricing still uses the existing four token components without charging the subcounts again. `sdk/cliproxy/usage/{detail.go,detail_clone_test.go}` cover value preservation and mutation isolation.

**Implementation evidence:** `internal/cpauk/store/migrations/018_event_pricing_session.sql`,
`internal/cpauk/aggregate/{pricing.go,pricing_snapshot.go}`,
`internal/cpauk/model/{pricing_breakdown.go,session_identity.go}`,
`internal/cpauk/collector/adapter.go`, `internal/cpauk/store/{write.go,reprice.go,query.go}`,
and `internal/api/handlers/management/analytics_events.go`. The Codex tier option is mapped in `internal/config/config_v8.go` and passed through `internal/api/analytics_options.go` and `internal/cpauk/{config.go,service.go}`. `internal/cpauk/collector/adapter_test.go` covers `Adapter.HandleUsage` through sanitization and pricing, returned versus outbound precedence, client-only fallback, the opt-in Codex override, other-provider isolation, and raw-tier preservation. `internal/cpauk/service_test.go` covers switching the policy on and off through hot reload.

**Recorded validation:** regression tests cover token and Kiro snapshots, repricing consistency, native
import after a pricebook change, invalid snapshots, session capture, and raw-ID filtering across
sources. A copy of an existing database downgraded to v16 migrated through 019 with rows and cost
totals intact. The synthetic auto-to-priority override prices 100 input and 100 output tokens at 400,000 nano-USD; restoring the old adapter through a Go overlay fails the regression. With the Codex assumption off, an authoritative returned default prices the same usage at 200,000 nano-USD; with it on, outbound priority prices at 400,000 nano-USD while raw evidence retains default. Removing the opt-in selection through a Go overlay fails that regression. Config, API, CPAUK service, and collector tests and vet pass, including policy hot reload. The server compiles and starts on localhost with the option enabled. Live `gpt-6-luna` calls on 2026-10-31 that sent `priority` upstream returned `default` (non-stream) and `auto` then `default` (stream); none of 10,848 production Codex events requested as priority or flex over the prior seven days returned either tier.

**Last updated:** 2026-11-07

### DL058 — Event outcomes, failure hops, and client cancellation

Migration 019 adds `outcome` (`succeeded`, `failed`, `canceled`), `failure_hop`, and
`cancellation_source` to events, exposed on list, detail, and export responses with an exact
outcome filter. Provider errors delivered inside HTTP-200 streams, incomplete upstream streams,
and peer HTTP/2 errors fail on the provider-to-CPA hop. A local rejection without upstream
dispatch fails on the CPA-to-provider hop and no longer records a synthetic upstream status.
Observed transport status is stored separately from the terminal failure.

A request is canceled by the client only when its own request context closes first. The request
bridge and reporter keep the first cancellation cause: internal cleanup, Home release and drain,
retry siblings, and WebSocket cleanup carry an internal cause and fail on a CPA hop, and an
observed provider error outranks a later disconnect. Stream executors retain usage observed
before a read error, send interruption, or cancellation. Canceled events keep legacy
`succeeded=true` and count as successes in aggregates, but are excluded from latency, generation
time, throughput, and model speed cohorts.

Historical rows are classified at read time by one shared classifier: failure hops are repaired,
and attempts whose own error is `context canceled` read as canceled with an unknown source.
Stored aggregates are not rebuilt. A read-only audit of 10,507 failed production attempts found
9,324 wrong hops; replaying them through the classifier leaves none and reads 207 as canceled.

Vertex API-key, service-account, and native interactions streams, xAI Responses streams, and AI Studio relay streams retain observed usage when an upstream read/reset error ends the stream. Failure publication uses the existing stream usage buffer.

Direct native Codex Images streams finish the usage observer and publish cancellation when a downstream send is interrupted. Typed client cancellation records `canceled/client` while retaining observed usage; the reporter publishes once, so deferred success finalization cannot replace that outcome.

Claude Thread streaming and buffered SSE failures preserve observed input/output usage before terminal validation or source errors. Actual executor regressions retain 5 input, 7 output and 12 total tokens as one failed attempt for both Claude and OpenAI targets. The existing once-only reporter and client-cancellation precedence remain authoritative.

Terminal Codex WebSocket HTTP errors remain owned by the active stream rather than racing the disconnect subscriber. Antigravity local body-close errors caused by cancellation normalize to that cancellation, preserving completed success and genuine upstream failure precedence. Partial usage and observed upstream status remain once-only.

**Implementation evidence:** `internal/runtime/executor/helps/usage_helpers.go`. `internal/cpauk/store/migrations/019_event_outcomes.sql`,
`internal/cpauk/model/event_outcome.go`, `internal/cpauk/collector/{adapter.go,sanitizer.go}`,
`sdk/cliproxy/usage/{manager.go,client_context.go}`, `sdk/api/handlers/handlers.go`,
`internal/runtime/executor/helps/usage_helpers.go`, and the Claude, Codex, and
OpenAI-compatible stream executors. `internal/runtime/executor/{gemini_vertex_executor.go,aistudio_executor.go,xai_executor_stream.go,stream_partial_usage_test.go}` cover these stream paths. `internal/runtime/executor/{codex_openai_images.go,codex_openai_images_cancellation_test.go}` and `TestCodexDirectImageClientCancellationRetainsObservedUsage` cover interrupted direct image delivery.

**Recorded validation:** On 2026-11-18, `internal/runtime/executor/helps/usage_helpers.go` retains once-only failures, original cancellation cause, partial detail and observed upstream status. Native image, Claude Thread, Vertex/AI Studio/xAI partial-usage and collector outcome tests remain; executor/analytics suites and focused races pass.

**Last updated:** 2026-11-18

### DL059 — Quota window history and hourly collection

Per-credential quota window samples record the provider, CPAUK identity HMAC, window, remaining
percent, reset time, plan, capacity, source, and a server-stamped observation time. ClickHouse
stores them in the append-only `analytics_quota_history` table (DL060). Legacy SQLite migration
017 is imported once.
`POST /v8/management/analytics/quota-history` accepts samples from CPAMC, resolves each auth
index to its HMAC, and rejects credentials whose quota provider differs from the submitted one.
`GET` returns samples for a provider and range. Writes keep one sample per credential and window
every five minutes, prune samples older than 90 days, and cap each provider at 100,000 rows.

An hourly collector reads quota for Claude, Codex, OpenCode Go, Z.ai, and Kiro credentials
through their base usage endpoints only, without manual-reset or grant reads. It skips disabled
credentials, spaces reads one second apart, reuses the shared management quota cache (with
order-independent query parameters), does nothing when analytics is disabled, and stops with
the server. Other providers are recorded only from CPAMC refreshes.

CPAMC charts these samples on each provider's Quota tab: a live figure with projected pool-empty, next-reset and recent burn stats, a recorded line over a gradient fill, a dashed projection with labeled refills, and a Now marker (CPAMC DL071).

**Implementation evidence:** `internal/cpauk/chstore/{migrations/0001_analytics.sql,quota_history.go}`,
`internal/cpauk/store/migrations/017_quota_history.sql`,
`internal/cpauk/{quota_history.go,store/quota_history.go}`,
`internal/api/handlers/management/{quota_history.go,quota_history_collector.go,api_tools_quota_cache.go,usage_probe.go}`,
and `internal/api/quota_history_collector.go`.

**Recorded validation:** tests cover validation, provider mismatch, throttling, retention, cache reuse with
a single provider call, Kiro query-order reuse, disabled analytics and credentials, and shutdown.
Live collection against real provider credentials has not been exercised.

**Last updated:** 2026-11-03

### DL060 — ClickHouse server mode: analytics, request capture, and agent SQL

Sanitized routing decisions from DL061 live in nullable String
`analytics_events.routing_trace_json`, visible through agent SQL and column comments.
The column is folded into the initial, undeployed ClickHouse schema.

ClickHouse separates CPA as a local proxy from CPA as a server. Without `clickhouse.dsn`,
CPA keeps upstream's file request and error logs, and analytics health reports
`clickhouse_not_configured` while proxying is unaffected. With a DSN, CPAUK analytics and
request capture use the configured database (`clickhouse.database`, default `cpa`) through
one shared, lazily connected pool, and request logs are no longer written as files. Home mode
keeps upstream file request logging even with a DSN. DSN and database changes take effect after
restart. The log directory is resolved once to an absolute path at startup, in plain mode too, so
a later working-directory change no longer moves it.

Schema comes from checksummed, embedded migrations per component (`core`, `analytics`,
`telemetry`), each starting from a single `0001` file; analytics adds `0002` for physical
storage efficiency, while telemetry adds `0002` comments plus `0003` lookup indexes and
`0004` storage codecs (DL065). Databases created by earlier pre-release migration sequences must be recreated. A `cpa_jobs` journal records one-time jobs. A job row is written
before the first schema statement, so an interrupted creation still runs its import.

**Requested streaming correction, 2026-10-29.** `requests.stream` now derives from
client body `stream`, Gemini streaming action/`alt=sse`, or downstream WebSocket
transport/timeline. A streaming request rejected with a JSON 403 remains marked
as requested streaming. The ingester keeps response framing separate for
transcript parsing, leaving ordinary file-log behavior unchanged. Legacy text-log
imports use the same intent detection when their request data is available.
Existing ingested rows are not rewritten. Real ClickHouse regression tests are
`TestRequestsStreamUsesClientIntent` and
`TestCapturedRequestStreamSurvivesJSON403`, covering failed, non-streaming and
successful streaming capture, protocol variants and legacy file import. Evidence
is in `internal/telemetry/ingest/{ingest.go,request_stream_test.go}`,
`internal/telemetry/record/record.go` and
`internal/api/middleware/request_stream_telemetry_test.go`.

**Request capture.** Each captured request, with every upstream attempt, is written as an
fsynced, zstd-compressed JSON record under `<logs>/telemetry-outbox/` and atomically renamed. A
batching ingester loads it into `requests`, `attempts`, `request_contexts`, `transcript_entries`
and `transcript_content`. `observability.logs.request-log` selects full capture; failed requests
are force-captured when it is off. `server.commercial-mode` suppresses upstream attempt capture,
as it does for file logs. Outbox records count toward
`observability.logs.logs-max-total-size-mb`. During a long outage the oldest are evicted with a
warning that gives the files and bytes lost. Ingestion is replay-idempotent and pauses during
ClickHouse outages or low disk. It quarantines a record to `telemetry-outbox/failed/` only for
decode failures, or for classified deterministic data errors after three attempts.

HTTP header diagnostics are available in CPAMC's event detail sheet through
`GET /v8/management/analytics/attempts/:attempt_id/headers`, with the event's range and
one of the four hop names. The full-access-only endpoint reads header columns without
loading bodies or transcripts and returns `no-store`. All retained HTTP dispatches for
the proxy request are identified separately because telemetry dispatches and usage
attempts are not one-to-one. WebSocket exchanges are omitted. Header names and repeated
values are retained with existing credential/cookie masking. Successful correlated HTTP
requests retain `detail=headers` records when full request logging is off; their client
and upstream bodies, WebSocket timelines, and free-form errors are removed before publication.
Existing full/error capture and commercial-mode upstream restrictions remain in effect.
Historical or expired captures return no header blocks. Each CPAMC accordion loads on
expansion, prefetches on hover or focus, and clears cached data across connection changes.

Validation: the full Go suite passes 128 packages; focused API, middleware, logging,
telemetry, schema-migration, and agent-documentation suites pass after the final changes.
The disposable server build and focused `go vet` pass. CPAMC passes 3,169 tests, lint,
TypeScript, and its single-file build. Chrome checks at 1440×1050 and 390×844 verify lazy
reads, hover/focus prefetch, keyboard expansion, repeated values, and no overflow. A
Docker image built with the pinned CPAMC commit serves the panel and proxies buffered
and SSE HTTP requests through a test upstream and real ClickHouse. All four header sets
are readable with request logging off; retained client and upstream body bytes are zero.
The panel omits the masking note.

Transcripts are parsed from client and upstream bodies and stored once. Content is addressed by
hash and entries chain by semantic ID. Elements whose provider tool-call IDs pass a uniqueness
guard are anchored independently of earlier history. Resent history and retries therefore add no
rows. Each request context keeps its ordered entry and content IDs, so request logs are rebuilt
byte-exact. The existing request-log and error-log management endpoints serve these rebuilds in
the legacy text format. `clickhouse.telemetry-retention-days` (default 30) bounds request
retention. Transcript rows are garbage-collected once no retained request references them.

**Agent SQL.** `POST /v8/management/telemetry/query` and `GET /v8/management/telemetry/schema`
let management-key holders run parameterized read-only SQL over telemetry and analytics. The
schema response carries table and column comments and example queries, such as a session's
failing request types and monthly skill-usage counts. Queries run under a separately provisioned
ClickHouse account with:

- a password HMAC-derived from the DSN password; with no DSN password it is random and held in
  memory, so sharing the database across CPA processes is unsupported;
- a read-only settings profile;
- configurable execution, row, byte and memory limits;
- privilege-denied external table functions.

Disconnecting cancels the query, and CPA never falls back to its write-capable account.
`GET /v8/management/telemetry/health` reports the outbox backlog, ingestion errors, and the
request-log import. `clickhouse.agent-query.enabled: false` disables both query endpoints.

Without a ClickHouse DSN, telemetry schema and query routes are absent, matching telemetry health. Registration uses the active startup handle, so adding or removing a DSN requires restart. Embedded agent route documentation describes this condition; CPAMC has no consumers of the schema or query routes.

**Analytics on ClickHouse.** `internal/cpauk/chstore` serves the existing v2 analytics contract
and maintenance API, verified against the frozen SQLite store by differential reader suites:

- versioned ReplacingMergeTree tables read with `FINAL`, events partitioned by UTC month;
- first-write-wins outcomes, atomic control-generation snapshots, and append-only quota history;
- retention through persisted bucket bounds and whole-partition drops;
- signed Int64 nano-USD with overflow-checked sums;
- logical backup and restore with a crash journal.

`analytics.max-storage-bytes` and `min-free-bytes` apply to the ClickHouse database and its disk.
Only one CPA process may own an analytics database for writes.

**One-time migration.** On the first creation of each component, CPA imports existing data in the
background:

- **Analytics:** copies the SQLite database at `analytics.path` with `VACUUM INTO`, migrates only
  the copy, and adopts the `identity.key` beside it.
- **Request logs:** parses legacy request and error log files on a best-effort basis, with
  compression-independent checkpoints.

Both imports resume after interruption, leave source files unchanged, and report state and counts
through health and CPAMC. Neither reruns after completion. Retention and destructive maintenance
wait while the analytics import is unfinished. An unreadable source fails the job with its reason.
The `abandon_legacy_import` repair can then give up the remaining rows explicitly, with a stored
reason.

`docker-compose.clickhouse.yml` layers a private ClickHouse 26.8 LTS service over the base
Compose file without publishing its ports. Server shutdown closes CPA-owned WebSocket connections
before finalizing request capture, so open sockets no longer hold shutdown until its deadline.
Setup, the API, limits and troubleshooting are in `docs/clickhouse.md`.

Request capture records every upstream dispatch, including the plaintext retry after an upstream rejects a zstd-compressed Codex body: one logical request keeps both the 415 and the 200 attempt.

**Known limits:**

- Read performance was benchmarked with `internal/cpauk/chstore/c5_bench_test.go` on a
  5-million-event fixture. At that baseline, unfiltered 365-day Analysis was the slowest
  reader and every read scanned the outcomes table. DL065 reworks those query paths; results
  still depend on hardware.
- Transcript identity has documented merge and split edge cases.
- Legacy text-log parsing is best effort.
- Readable reasoning persistence was not observed live, because the test models returned no
  reasoning text.
- Power loss was not fault-injected.

Telemetry preserves committed HTTP status across later classified stream errors. `TelemetryStreamingWriter.WriteAPIResponseErrors` is intentionally a no-op because middleware already records those errors in `req.Errors` for the final record. Upstream response-writer initialization and finalization therefore compose with the fork's capture without duplicating terminal errors. `internal/api/middleware/request_stream_telemetry_test.go` covers an HTTP-200 stream followed by a classified 400.

Upstream bounded deferred response capture when full request logging is off composes with per-dispatch telemetry and no-DSN file logs. Unrouted 404 requests do not create dumps. Owned WebSocket transports close before capture finalization, and read-only log projection discards upstream request/response sections. A live local ClickHouse-mode server imports the real analytics snapshot, serves inference, captures a request and exposes it through the restricted SQL endpoint; plain mode writes file logs and omits telemetry routes.

Legacy-log ingestion accepts the old response layout and upstream labeled headers/body while preserving SSE bytes. Local request diagnostics remain separate from upstream attempts in ClickHouse capture; plain file-log and no-DSN modes retain their contracts.

**Implementation evidence:** `internal/telemetry/legacylog/legacylog.go`. `internal/clickhouse/`, `internal/config/clickhouse.go`,
`internal/telemetry/{record,outbox,ingest,transcript,store,legacylog}`,
`internal/logging/{telemetry_*.go,global_logger.go,log_dir_cleaner.go}`,
`internal/runtime/executor/helps/logging_helpers.go`, `internal/runtime/executor/codex_executor_request.go` (compression retry capture, `codex_executor_compression_test.go`),
`internal/api/{clickhouse.go,telemetry.go,hijacked_connections.go,analytics_options.go,server_options.go}`,
`internal/api/handlers/management/{telemetry_query.go,telemetry_query_examples.json,logs.go,clickhouse.go}`,
`internal/cpauk/{backend,chstore}`, `internal/cpauk/service.go`, `docker-compose.clickhouse.yml`,
`config.example.yaml`, and `docs/clickhouse.md`. `internal/api/{server_management_v8.go,server_telemetry_routes_test.go,server_agentdocs_test.go,telemetry_disconnect_test.go,telemetry_smoke_test.go}` and `internal/agentdocs/{management.md,sql.md,llms.txt}` cover conditional registration and the documented route inventory.

**Recorded validation:** On 2026-11-18, `internal/telemetry/legacylog/legacylog.go` now accepts upstream's labeled response layout without fabricating upstream errors. DSN/file-log mode, SQL authorization, startup handles, capture/outbox/import/shutdown and compression-retry wiring remain; real ClickHouse and HTTP fixture suites pass.

- Claude Code and Codex CLI sessions through CPA produce one transcript entry per executed tool
  call and result. The shipped monthly skill query counts exactly the real content loads.
- An upgrade from the previous fork build imports SQLite analytics and request logs once and
  leaves the sources unchanged.
- CPA recovers from a ClickHouse restart without its own restart, with no lost or duplicated
  rows.
- An unreadable legacy source fails with its reason and can be abandoned. CPAMC shows each
  state.
- Without a DSN, a real model request, file request logs, upstream cancellation after a client
  disconnect, and shutdown with an open WebSocket behave like the previous build.

Before the route fix, authenticated schema/query requests returned 503 in plain mode. Mode tests now verify all three telemetry routes return 404 without a DSN, real ClickHouse schema/query/health return 200, and DSN reloads preserve startup route availability. The configured agent-doc inventory parity test passes. Disconnect fixtures install their database handle before route registration; the conversation fixture expects plain-mode 404s, and fixtures wait for ingestion startup before cleanup.

**Last updated:** 2026-11-18

### DL061 — Rule-based routing, routing traces, and routing dry run

DL070 extends these traces with strategy factors, read-only weighted predictions, and affinity binding provenance.

`routing.rules[]` adds ordered custom rules: a condition tree (`all`, `any`, `not`) over the
requested model, client API-key label, request header, and detected client type, plus `route-to`
credential IDs (`Auth.ID`). Selection follows a fixed precedence stack, weakest first: custom
rules (a lower matching rule wins), routing strategy (priority tier, then the configured picker),
session affinity (an eligible binding wins), and the routing gate (disabled, cooldown, quota,
quota reserve, unsupported model, retry round, tried in this round, weight, pins and policies). Rules narrow the
gate-passed pool across all priority tiers after the affinity check; a rule whose targets are all
blocked falls through to the next matching rule above it, and no applicable rule keeps the full
pool. Requests a rule narrows take the legacy selection path, so the indexed scheduler never picks
outside the narrowed pool. Rules never override pins, credential policies, the Antigravity
paid-credits fallback, plugin-handled picks, or Home dispatch; in Home mode rules are ignored with
a warning. Rules-only config changes do not recreate the selector, so affinity caches survive.

When CPAUK collects analytics, every selection records a versioned routing trace (candidates with
gate reasons, matched rules with rendered conditions and status, strategy tier, affinity state,
pick, and decider). A credential withheld only by its quota reserve reports gate `reserve` with
the reserve's reset time instead of the generic `policy`. Active cooldown and credential-quota
gates take precedence over reserves, so a simultaneous reserve does not hide the cooldown reason
or replace its deadline. This applies to initial capture and quota-refresh revalidation, including
dry runs. Previously stored events keep their original trace. This changes diagnostics only;
credential eligibility and reserve enforcement are unchanged. The trace travels on the usage record, is stored with the attempt's event in
ClickHouse `analytics_events.routing_trace_json` as nullable String, with candidate IDs replaced
by credential pseudonyms. `GET /v8/management/analytics/events/:attempt_id` returns the trace
with labels resolved from loaded credentials. `GET /v8/management/routing/credentials` lists routable
credentials without secrets and includes the v8 group name for all API-key families and the non-secret
auth lifecycle `status`, never status-message details.
Non-compat key-level name overrides take precedence; labels are unchanged. Names survive v8 saves
and legacy migration at group level. `POST /v8/management/routing/dry-run` simulates a
request (optionally with draft rules) without moving strategy, affinity, or cooldown state. CPAMC DL073 adds the Config →
Routing tab and the event "Routing decision" section. Detail and exports include the stored trace;
ordinary event lists omit it. Without ClickHouse, analytics and trace capture are unavailable,
while routing rules and dry runs remain usable.

Dry runs accept optional `source_format`; `openai-image` selects the native Images API credential track independently of the model name or client User-Agent. Omitted input preserves text-route behavior. Invalid formats return HTTP 400 with `path: "source_format"`. Live selection, trace gates, GSR refresh, and dry-run affinity checks use the same request-aware SIWC eligibility. Native image calls use the outer OAuth token even when the nested SIWC token has a different expiry. `sdk/cliproxy/auth/routing_siwc_merge_test.go` covers buffered/streaming generations and edits, both expiry directions, rules, and affinity; `internal/api/handlers/management/routing_test.go` covers format validation. Embedded routing docs describe the input.

Codex continuity candidates use reserve-aware eligibility, pins, exclusions, zero-weight gates and retry limits. Existing eligible affinity wins before rule narrowing; rules use ready-pool fall-through, or the recoverable quota pool when none is ready, and mixed-provider targets cannot become invalid Codex candidates.

**Implementation evidence:** `sdk/cliproxy/auth/conductor_continuity.go`. `internal/config/{config_types.go,routing_rules.go}`,
`sdk/cliproxy/routing/{contract.go,engine.go}`,
`sdk/cliproxy/auth/{routing_selection.go,routing_trace.go,routing_dry_run.go,conductor_selection.go,selector.go,quota_reserve.go}`,
`sdk/api/handlers`, `sdk/cliproxy/usage`, `internal/runtime/executor/helps/usage_helpers.go`,
`internal/cpauk/{collector,model,chstore}`,
`internal/api/handlers/management/{routing.go,analytics*.go}`, and `config.example.yaml`.

**Recorded validation:** On 2026-11-18, `sdk/cliproxy/auth/conductor_continuity.go` applies request eligibility, reserves/bypass, pins/exclusions, affinity and ready-pool rule fall-through to continuity. Live/dry-run rules, trace persistence/sanitization and mixed-provider parity tests remain; auth/routing/management plus focused races pass.

After the ClickHouse merge, real ClickHouse tests cover scalar/event JSON trace persistence,
detail/list/export projections, outcome patches, repricing, backup/restore, epoch archives,
and interrupted late-retention publication. The targeted suite passed except the known
`TestResponsesLateInterruptDoesNotPoisonNextCreate` flake. The full suite found an existing
CPAMC bundle/source revision mismatch and an intermittent
`TestXAIWebsockets_KeepalivePingDuringUpload_WithSession` failure, which passed in isolation.
`go build ./...`, the server compile check, `go vet ./...`, and formatting checks passed.

Cooldown/reserve precedence regression tests first reproduced `reserve` hiding active cooldowns
and credential-quota deadlines in both initial capture and revalidation. They now pass, including
reserve-only and policy-precedence cases. Validation on 2026-11-18 passed
`go test ./sdk/cliproxy/auth ./sdk/cliproxy/routing ./internal/api/handlers/management -count=1`,
the server compile check, gofmt, and `git diff --check`.

Validation after applying the status fix to current `main` also passed the routing and management
suites and the server compile check. The Windows auth suite reports two existing cleanup failures,
`TestSessionCacheKVAliasesProvenanceAndInvalidation` and `TestSessionCacheKVExpiryAndEviction`,
because `kv.db` remains open. Both reproduce with the status fix removed. The auth suite passes
with only those two tests skipped, including the cooldown/reserve regression cases.

**Last updated:** 2026-11-18

### DL062 — Embedded agent documentation and API discovery

CPA serves public `/llms.txt`, `/llms/management.md`, `/llms/sql.md` and
`/llms/model-availability.md` from the binary whenever the existing
local-management availability gate permits v8.
Home mode and disabled management return 404. Disabling the panel does not
disable API documentation. GET and HEAD work without a management key, including
in template-key safe mode. Root JSON adds `agent_docs`; root and v8 responses,
including authentication failures, advertise the covering llms.txt with a Link
header. No legacy management endpoints change.

The index follows the llms.txt convention. Its Commonly used section links to a
focused model-availability guide with a curl dry-run recipe, request context,
candidate gates and prediction limits. The guide distinguishes local eligibility
from verified upstream access; the repository's AGENTS.md links to it too.
Companion pages describe every v8 route, configuration writes, current cooldown and historical routing checks,
and restricted SQL with session-error and monthly skill-occurrence recipes.
The SQL guide explains capture coverage, retention, replacing merges, client
versus upstream transcript views, occurrence limits, time zones and cost units.
Live schema comments remain the source for full column lists. Session recipes
resolve bare IDs to canonical source/agent-qualified IDs before exact queries,
using seeded near-match and literal-wildcard regression cases. The guide maps
session columns and digests and shows envelope inspection for tools and model
parameters. The local-calendar month boundary uses explicit time-zone conversion.

**Cold-agent follow-up, 2026-10-29.** Unknown management paths now return a JSON
404 with `agent_docs` when local management is available. Legacy unknown paths
retain authentication and plugin dispatch before the hint; v8 hints can be
public. Availability failures retain empty 404s. No existing legacy endpoint is
changed. Routing credentials add local `cooldowns` for every credential,
including config API keys, and a shared `observed_at`; Home mode reports null.
The panel's field-selecting routing decoder tolerates the additive fields.

Monthly skill SQL now counts distinct client-context entry membership per
session, since identical prompts in different sessions share entry hashes.
Both embedded docs and live schema examples use the same text-prefiltered
query with an explicit time-zone month boundary. Tests include shared entries,
repeated history, old memberships and translated attempts. The guide distinguishes
text matches from executed skill calls, documents event-detail range parameters,
and lists the accepted static model channels.

**Final QA corrections, 2026-10-29.** Management docs distinguish scalar outcome/result
enums from list filters, mark required analytics selectors and values, document
file-logging prerequisites, and identify summary request/attempt counters. The
session comparison recipe includes successful and failed requests together, and
AAA guidance requires invocation-shape narrowing before reporting usage.
Capabilities report ClickHouse in both the service and unavailable-handler
fallback; the panel accepts the driver as a string without driver-specific logic.
Routing dry runs reject missing or blank models with 400 and `path: "model"`.
Telemetry schema comments direct per-session counts through client request
contexts. Migration `0002` updates those comments with `ALTER TABLE`, leaving the
checksummed `0001` unchanged so existing server-mode databases keep starting.

The embedded maintained-route inventory includes the observational in-flight count GET and documents Muse discovery exemptions and action admission.

**Implementation evidence:** `internal/api/server_agentdocs.go`. `internal/agentdocs/{docs.go,llms.txt,management.md,sql.md,model-availability.md}`,
`internal/api/server_agentdocs.go`, the shared availability predicate in
`internal/api/server_management.go`, and discovery/route wiring in
`internal/api/{server.go,server_routes.go,server_middleware.go}`,
`internal/api/handlers/management/{routing.go,capabilities.go,telemetry_query_examples.json}`,
`internal/cpauk/capabilities.go`, `internal/telemetry/store/migrations/0002_transcript_count_comments.sql`,
and `internal/telemetry/ingest/fixes_test.go`.

**Recorded validation:** On 2026-11-18, `internal/api/server_agentdocs.go` still advertises/serves embedded GET/HEAD docs behind the shared availability gate. Inventory adds U8's in-flight read and Muse policy; route parity, safe-mode/auth-failure hints and real restricted-SQL examples pass.

**Last updated:** 2026-11-18

### DL063 — Restarts never serve an empty OAuth model catalog

Upstream opens the HTTP listener before the file watcher loads the auth directory, so stored OAuth
credentials have no registered models for a short window after every start. Requests in that window
fail with `400 unknown provider for model`, and `/v1/models` returns 200 without those providers'
models; Codex clients cache that list and their GPT models disappear from the picker. `Service.Run`
now registers models for the auths loaded from the store before the listener opens. The watcher's
initial load still re-registers them afterwards.

The window was seconds long here because the reload callback that precedes model registration ran
CPAUK's analytics reconfiguration and API-key lifecycle sync inline. Those calls wait on the
ClickHouse store lock and run synchronous `ALTER` mutations, so a slow or busy ClickHouse stalled the
whole auth load. Startup and reload analytics sync now run as a FIFO background chain on the server
with unchanged calls and order, and a reload no longer waits for ClickHouse.

Upstream Antigravity discovery now lives in `sdk/auth/antigravity_models.go`. Service wrappers retain persisted-hint seeding, cached publication, asynchronous refresh, and stale-result fencing. Known persisted catalogs publish before refresh; a never-discovered catalog still follows upstream asynchronous discovery.

Kiro registration passes through the executor-wrapper helper on ordinary and forced startup/refresh paths while keeping discovered catalogs ready before listening.

**Implementation evidence:** `sdk/cliproxy/service_lifecycle.go`. `sdk/cliproxy/service_lifecycle.go`, `internal/api/server.go`,
`internal/api/server_reload.go`, `internal/api/analytics_options.go`,
`sdk/cliproxy/service_run_startup_models_test.go`, and
`internal/api/analytics_reload_async_test.go`.

**Recorded validation:** On 2026-11-18, `sdk/cliproxy/service_lifecycle.go` still publishes loaded-auth catalogs before OnBeforeStart/listening; analytics reload remains FIFO/asynchronous. Startup-model, blocked-analytics reload and wrapped Kiro registration regressions remain and pass.

**Last updated:** 2026-11-18

### DL064 — Example modules build against the v8 tree

The fork's plugin examples use `/v8` module names, require `CLIProxyAPI/v8 v8.0.0`, and replace that dependency with the repository root. Each builds against the checkout it ships in rather than downloading a published SDK. The example generator writes the same module path. Upstream supplies matching v8 paths in two examples; the remaining examples still need the fork correction. The `claude-web-search-router` and `codex-service-tier` module graphs combine upstream's moego dependency with local root and moego replacements.

The nested module graphs retain local v8 replacements and reconcile moego dependencies and sums required by migrated SDK imports.

**Implementation evidence:** `examples/plugin/claude-web-search-router/go/go.mod`. `examples/plugin/*/go/go.mod` and the reconciled `claude-web-search-router` and `codex-service-tier` `go.sum` files, `examples/realtime-openai-go/go.mod`, `examples/plugin/scripts/generate_examples.py`, and `examples/plugin/README.md`.

**Recorded validation:** On 2026-11-18, `examples/plugin/claude-web-search-router/go/go.mod` and the other nested module graphs retain local v8 replacements and reconciled moego dependencies/sums; generator/docs still agree. All 22 plugin modules plus realtime compile with go test -mod=readonly -run '^$' ./....

**Last updated:** 2026-11-18

### DL065 — ClickHouse query, ingest, and storage efficiency

The first ClickHouse implementation moved many full analytics rows into Go and issued
repeated discovery queries. The fork now keeps bounded report work in SQL. Key catalog,
provider credentials, missing-pricing, and Analysis cost aggregation run set-based queries;
Analysis costs use exact Int256 rational arithmetic with the same current-price validation,
per-event rounding and reconciliation, credit semantics, errors, and Go fallback. Shared
reads memoize retention cutoffs within one read-lock scope, bound retained validation and
publication lookups, prune event pages before response overlays, restrict outcome overlays
to selected requests, compute model awards in one pass, and send all throughput-estimate
targets through one external-table join. The maximum-window missing-pricing read starts at
the containing storage-calendar day, so retained buckets no longer make it fail. Management `/keys` uses the analytics response
cache with its exact range and cursor. Public report and request-log contracts are
intentionally unchanged; some paths scan more ClickHouse rows so that substantially less
data reaches Go.

Ingest and maintenance avoid repeated work without changing replay or recovery semantics.
Analytics outcome eligibility reads only scalar columns and excludes attempts that already
have patches. Reprice reuses one matched count within a run and recounts at its first and
final chunk. Late-retention recovery checks durable plans first; when no plans exist, it
prunes healthy raw history by the published cutoff while retaining recovery for unsupported
old pending rows. Telemetry ingestion drains bounded request batches, probes shared
transcript identities set-based, and preserves per-file rejection, retry, and continuation
behavior. Upstream continuation lookups join only the requests that own the matching
attempts. Request detail tries its exact ID first and falls back only for suffix and proxy
IDs. CPA defaults native ClickHouse transport to LZ4 unless the DSN sets `compress`.

Migration `analytics/0002_event_storage.sql` converts repeated event dimensions to
LowCardinality strings, adds Delta/ZSTD codecs to event time and ZSTD codecs to diagnostic
JSON mirrors, and materializes a proxy-request bloom index synchronously. Telemetry
migration `0003_lookup_indexes.sql` adds request and response bloom indexes; migration
`0004_storage_codecs.sql` adds ZSTD and delta codecs, with larger ZSTD blocks for transcript
content. Stored values and query results do not change. Codec-only changes apply to newly
written and merged parts; older parts that no longer merge keep their previous compression
until retention removes them or an operator rewrites them. The LowCardinality conversion
rewrites those analytics columns during upgrade. Ordinary analytics reindex requests background-sized merges instead of forcing
`FINAL`, and schema-1 analytics archives restore into schema 2. Deferred follow-ups include
hot-window rollups, alternate text indexes, exact percentile histograms, lightweight routine
deletes, report fusion, an error-log paging contract change, and further transcript
storage factoring and deduplication.

**Implementation evidence:** `internal/api/handlers/management/{analytics_pricing.go,analytics_response_cache.go}`,
`internal/clickhouse/clickhouse.go`, `internal/cpauk/{backend/types.go,service.go}`,
`internal/cpauk/chstore/{activity.go,aggregates.go,analysis.go,backup.go,dimensions.go,key_catalog.go,model_analysis.go,model_cost_series.go,model_leaderboard.go,operations.go,pricing.go,provider_quota.go,query.go,read_selection.go,reprice.go,retention_late.go,throughput_estimate.go,write.go,migrations/0002_event_storage.sql}`,
`internal/cpauk/maintenance/operations.go`,
`internal/telemetry/{ingest/ingest.go,store/read.go,store/migrations/0003_lookup_indexes.sql,store/migrations/0004_storage_codecs.sql}`,
and the `s1_*`, `s2_*`, `s3_*`, `s4_*`, and `s5_*` regression suites.

**Recorded validation:** real ClickHouse tests compare reports and request rebuilding with
frozen pre-change readers. They cover maximum-page cohorts, cached `/keys` ranges and
lifecycle invalidation, patch eligibility across batches, migration and retry, reprice
resume, bounded late recovery, ordinary reindex, schema-1 restore, batched telemetry replay
and failure, exact-detail lookup, and LZ4/DSN compression selection. Representative tests
are `TestS1ReportParity`, `TestS2MixedReadParity`,
`TestS3MigrationExistingDataAndRetry`, `TestS3LateRecoveryAfterReopen`,
`TestS5AnalysisParity`, `TestS4BatchReplayAndLookup`, `TestS4CompressionOptions`,
`TestS1KeyCatalogResponseCache`, and `TestC3QuotaHistoryFreshSchemaAndRetrySafeImport`.
`gofmt`, `go vet`, and the required disposable server compile check also pass.

**Last updated:** 2026-10-30

### DL066 — Scoped management keys

**Last updated:** 2026-11-18

Additional `management.keys` credentials have explicit `full-access` or `read-only`
scopes. Legacy, environment, and local passwords retain full access. Startup, config
uploads, comment-preserving persistence, and reloads validate metadata and hash plaintext
keys. The v8 session endpoint reports the authenticated scope; full-access key CRUD
returns only IDs, names, and scopes. Ordinary JSON config reads redact scoped-key hashes.

Read-only management uses an explicit v8 allowlist for analytics and quota observation,
including event exports that only read data and stream an in-memory CSV/JSON response.
Dashboard, Logs Viewer, and Management Center Info remain accessible through a safe
`GET /v8/management/overview` summary, the latest-version check, and allowlisted log and
API-key usage reads. Overview exposes selected settings and counts, with proxy scheme/host
only. Read-only usage replaces raw URL/key identities with numbered labels. Application
logs remove credentials and known secrets; request downloads omit all headers and payloads
while retaining request metadata and response status. Full-access log/usage behavior and
filename/path validation remain unchanged. Full config, log deletion/settings writes,
updates, and model refreshes remain denied.
It rejects legacy routes, config and credential downloads, mutations, plugin management
operations, and non-HTTP management authentication. Quota proxy requests require validated
provider-specific read operations, prohibit redirect/proxy/header/body escapes, and redact
credential-bearing response fields. Observational plugin quota fetches remain available;
quota resets and paid inference probes require full access. Pricing GETs do not schedule
catalog refreshes for read-only sessions. Read-only plugin requests must match the
credential's server-selected quota plugin, and plugin errors never return provider details.
See `docs/management-keys.md` for the contract. Numeric token accounting fields survive
quota sanitization only for recognized count names; token credential fields and
string-valued lookalikes remain stripped. Read-only panel quota reads use official Kimi
endpoints without credential downloads and sanitized Antigravity project metadata.
Plugin quota routing uses the credential's server-selected plugin ID without privileged
discovery. Meta refresh is unavailable to read-only sessions because its existing quota
flow mints a key.

Codex manual-reset expiry observations accept CPAMC's fixed `OpenAI-Beta: codex-1`
and `Originator: Codex Desktop` headers only on the exact reset-credit GET endpoint.
Account header and subscription-query validation prefer the ID-token account exposed by
credential listings, then use the existing metadata/attribute aliases and legacy `account_id`
fallback. Stale stored aliases cannot override the account CPAMC receives. Reset-credit
consumption and all existing request restrictions remain denied
to read-only sessions. Actual-transport regressions replay usage, subscription, and reset
details with production-shaped credentials and reject account/query/header escapes.
Listing-to-request regressions also cover conflicting snake-case and camel-case aliases
in both metadata and attributes, while preserving server-side quota-reserve selection.
Desktop and mobile browser checks exercise the actual panel and CPA through a local TLS
quota fixture: expiry details render without warnings and no reset mutation is sent.
The Docker runtime accepts all three quota reads and rejects reset consumption and stale
account headers. The full Go suite and pinned Bun Codex quota tests pass.

Read-only scoped keys may GET `/v8/management/observability/requests/in-flight`; mutations remain denied. The bounded legacy bcrypt cache does not cache scoped authorization or revocation, which are resolved on every request.

**Implementation evidence:** `internal/api/handlers/management/management_keys.go`. `internal/config/management_keys.go`,
`internal/api/handlers/management/management_keys.go`,
`internal/api/handlers/management/management_quota_read.go`, v8 route registration,
startup/reload availability checks, scoped-key and quota security regression tests.

**Recorded validation:** `go test ./...` passes with an isolated ClickHouse instance,
including restricted SQL reads, rejected SQL writes and external-source access, key
lifecycle, scope enforcement, redaction, and quota transport regressions. The canonical
Docker panel build and disposable server compile pass. Live API checks cover create,
rename, scope changes, rotation, revocation, restricted reads, and a successful read-only
`SELECT`. Chrome checks against the bundled panel cover key management, read-only login,
restricted-route redirects, and absence of config requests at desktop and mobile widths.
The Docker runtime smoke test covers scoped login, key CRUD, denied mutations, and bundled
panel delivery. The panel's pinned Bun 1.4.3 verification passes 2,637 tests, lint,
TypeScript, and its production build. Session races are covered through the real Axios
interceptors as well as deferred config completions; plugin regressions cover credential
selection and secret-bearing errors for both GET and POST. Additional tests cover
read-only overview counts, opaque usage identities, plugin YAML secrets, log section
spoofing, and denied logging mutations. Desktop and mobile browser checks cover the
Dashboard, Logs Viewer, and Management Center Info with no privileged preloads.

### DL067 — Config YAML syntax errors report their line

**Last updated:** 2026-11-18

`PUT /v8/management/config.yaml` and the other v8 config writes return the YAML parser's message
as `message` when the request body is not valid YAML, alongside the unchanged
`"error": "invalid_body"` code. Upstream returned only the code, so neither the management panel
nor a script editing the config could see what was wrong or where. gopkg.in/yaml.v3 v3.0.1 can't
be trusted for the line: depending on the problem it counts from zero, omits the line on line 1,
or reports the start of the enclosing block. Its line is never past the problem, though, so the
fork takes the shortest line prefix of the body that fails with the same message, starting at
yaml.v3's line. Bodies over 1 MiB skip the search and keep yaml.v3's message. Lines in later
validation errors are covered by DL069. Proposed upstream on the
`fix/v8-config-error-messages` branch; drop this entry if upstream accepts it.

Config authentication-cache invalidation composes with line-aware syntax errors before writes; invalid YAML still leaves the configuration unchanged.

**Implementation evidence:** `internal/config/yaml_syntax_error.go`. `DescribeYAMLSyntaxError` in `internal/config/yaml_syntax_error.go`
and the `invalid_body` response in `internal/api/handlers/management/config_v8.go`.

**Recorded validation:** On 2026-11-03, `yaml_syntax_error_test.go` covers dedents after
comments, sequence entries in a mapping, unclosed flow collections and quotes (including after
valid multi-line ones), one-line bodies, tabs, and CR, CRLF, NEL, LS, and PS line breaks.
`config_v8_parse_error_test.go` covers the handler response, and the config file stays
unchanged. A real server built from fork `main` reported the line for an unclosed quote and for
a one-line body. An earlier version used a table of yaml.v3 parser messages and reported the
enclosing block's line for several of them.

### DL068 — Unknown v8 config fields are named at their own line

**Last updated:** 2026-11-03

`ValidateV8Config` decodes a flattened legacy copy of the v8 document strictly. Flattening moves
every known v8 field out of its section, so a typo such as `server.hots` stays behind under
`server`. Upstream then reported `line 1: field server not found in type config.legacyConfig`,
which names the parent section, the line in the internal copy, and an internal Go type. The
fork maps each strict-decode error back to the node in the `ValidateV8Config` input and keeps it
a `*yaml.TypeError`. Below a leftover v8 section it reports every unknown key
(`line 6: field hots not found`), and elsewhere it reports the key at its input line. Keys that
flattening or API-key expansion generated have no position, so their value's line is used. The
flattened tree keeps input positions because its nodes are copies of the parsed input. Proposed
upstream with DL067; drop this entry if upstream accepts it.

**Implementation evidence:** `internal/config/yaml_error_lines.go` (`describeV8DecodeError`) and
the `ValidateV8Config` decode call in `internal/config/config_v8.go`.

**Recorded validation:** On 2026-11-03, `yaml_error_lines_test.go` covers a typo beside a known
sibling, in a nested section, under `routing.retry`, in an expanded API-key entry, and in a
section decoded as one struct (`routing`), plus every unknown key in one section. Run against
the previous fork version, the `routing.retry` and API-key cases reported the parent section or
no line. A real server built from fork `main` reported `line 9 (clickhouse.dns-typo)` for a typo
in the fork-only `clickhouse` section.

### DL069 — Config validation errors point at the submitted field

**Last updated:** 2026-11-18

The v8 config handler validates its re-encoded document, so upstream's `invalid_config` line
references ignored the body's comments, blank lines, and indentation. A type error on line 11 of
an uploaded `config.yaml` was reported on line 6. The handler maps each `line N` back to the field
and names it: `line 11 (server.port): cannot unmarshal !!str 'notaport' into int`. A
whole-document PUT re-parses the body only when there is an error to describe and reports the
body's line and spelling, so a historical key name keeps the user's path. PATCH and sub-path writes merge into nodes from the
stored file, so they report only the path (`server.port: ...`). Values copied through an anchor
or merge key also get only the path, because their line is the anchor's, while the anchored
original keeps its line; they are found from
the submitted body's aliases, so this holds when normalization removes the anchor's own field. Without a column, a
collection nested in one of the same kind on one line is named one level too deep. Documents
over 1 MiB keep yaml.v3's message, as for DL067. Proposed upstream with DL067; drop this entry if upstream accepts it.

Whole-document PUT diagnostics use the original submitted bytes, while PATCH diagnostics retain path-only context alongside management authentication-cache invalidation.

**Implementation evidence:** `internal/api/handlers/management/config_v8.go`. `DescribeYAMLErrorLines` in `internal/config/yaml_error_lines.go`
and its use after `ParseConfigBytes` and `ValidateV8Config` in
`internal/api/handlers/management/config_v8.go`.

**Recorded validation:** On 2026-11-03, `config_v8_validation_error_test.go` covers type and
unknown-field errors in whole-document PUTs and PATCHes, `routing.retry`, API-key entries,
historical spellings, a mapping where a list belongs, block-valued unknown fields, aliases
(including one whose anchor normalization removes), merge keys, and user values containing `line N:`. Any case expecting no line fails if one
appears. Run against the previous fork version, eight of these cases failed. HTTP statuses, error
codes, and the bytes written for valid saves are unchanged.

### DL070 — Routing strategy explanations and affinity provenance

**Panel controls, 2026-11-06.** CPAMC Routing reuses the Usage tab selector. Its shared Dry run
and Rules model field searches the routing catalog and supports aliases, prefixed IDs, and
custom entry. See CPAMC DL079 for implementation and verification details. The canonical panel
build, CPA compile check, and desktop/mobile browser dry runs against a local CPA server pass.

Routing traces explain the built-in strategy's preferred credential independently of
session affinity. `strategy_step` records the applied algorithm, GSR's missing-data
round-robin fallback, previous cursor pick, per-candidate scheduling shares, effective
weights, and accounting credits before selection. GSR includes cached recovery deadlines
and their source, delay, decay, base weight, allowance tier and source, tier multiplier,
and configured weight. Legacy selectors and single/mixed-provider scheduler picks
share the same GSR factor calculation. Live explanations consume the picker's prepared
deadlines and weights, so expiry during tracing cannot change the reported algorithm
or shares. Explanation maps are allocated only when a routing capture is present.
Existing exported timestamp and tier APIs remain compatible. GSR keeps its existing truncation and minimum-one base weight; the contract
comment now describes that arithmetic rather than rounding.

Dry runs copy live credits and cursors and predict ten selections for RR, WRR, fill-first,
and GSR. WRR clones the selector's exact or dynamic accounting pool, including pending and
settled reservations, member epochs and generations, collision-chain identities and active
subsets, then runs production reservation and settlement on that private copy. It creates no
live pool and cannot change live credits, cursors, membership or quota observations.
WRR predictions use the same `mixed` accounting namespace as public Execute/Stream/Count
execution with or without affinity. The one-provider affinity cache remains provider-scoped,
independently of accounting. Direct SelectAuth callers retain their separate provider-scoped
WRR pools and are outside this dry-run prediction contract. Kept affinity traces retain the
strategy's wanted pick
separately from the bound credential. Unavailable `never` bindings do not predict replacements.
Dry runs use cached quota observations only, report GSR's fallback when observations
are missing, and show the strategy's wanted credential even when affinity keeps another.
When missing cached recovery data would need refreshing, `strategy_step.fallback` is
`missing_recovery_data_refresh_pending` and `pick_predicted` is false. Weights and
wanted selections remain cached-data estimates; an existing kept affinity binding can
still determine a reliable pick. The optional dry-run input `downstream_websocket`
defaults to false and applies live WebSocket priority and ready-view preferences.
Dry-run scheduler WebSocket preference includes both Codex and xAI before priority
selection. GSR fallback simulations read the fallback selector's provider/model cursor,
including per-entry OpenAI-compatible provider names and `mixed`, and round-robin rows
report zero smooth credits. Regressions alternate dry runs and live selections four
times with missing recovery data and check both predicted picks and previous picks in
live traces, with and without affinity; scheduler fallback coverage checks the ready
view's cursor as well.

Usage-probe observations for z.ai and OpenCode Go survive execution results, including
when another adapter serves the credential. Previously `MarkResult` treated an
OpenAI-compatible response as an unsupported passive snapshot and cleared the persisted
probe signals. Alternating live picks could therefore erase and refresh one entry's
weekly reset between dry runs. These probe-owned observations now bypass passive
adapter-header replacement. A real quota HTTP transport/parser fixture verifies two
entries with the same key retain their weekly reset across four dry-run/Execute cycles
without further refreshes. The cache already isolates entries by credential identity,
and routing refreshes already persist observations; neither needs a cache or persistence
redesign.

The affinity cache records the original binding time and proxy request ID; TTL refreshes,
held failovers, and alias merges preserve them, while a rebind records the new request.
LCP bindings also retain this provenance; forks inherit the parent's creation time
and request ID. With analytics and credential identity storage enabled, only the
bound candidate in the management dry-run response includes optional `analytics_id`,
computed by CPAUK's credential identity service using the same provider, auth index,
and identity key as persisted traces. The sanitizer always strips this response-only
field. The trace's affinity step exposes the binding's creation time, age, and request ID when it references a previous credential.

Persistence preserves candidate indexes and binding request IDs. Oversized traces lose
predicted selections and per-weight GSR factors before candidate details. ClickHouse
continues storing raw trace JSON with no schema migration; detail label resolution keeps
the new fields. Event queries accept the correlation-ID-validated `proxy_request_id`
filter. The v8 GET interface uses repeated `proxy_request_id=<id>` query parameters,
matching CPAMC's existing flattened filter encoding; POST queries use
`filters: {"proxy_request_id": ["<id>"]}`.

Dry runs accept optional `source_format`; `openai-image` selects the native Images API credential track independently of the model name or client User-Agent. Omitted input preserves text-route behavior. Invalid formats return HTTP 400 with `path: "source_format"`. Live selection, trace gates, GSR refresh, and dry-run affinity checks use the same request-aware SIWC eligibility. Native image calls use the outer OAuth token even when the nested SIWC token has a different expiry. `sdk/cliproxy/auth/routing_siwc_merge_test.go` covers buffered/streaming generations and edits, both expiry directions, rules, and affinity; `internal/api/handlers/management/routing_test.go` covers format validation. Embedded routing docs describe the input.

Dry-run simulations retain private selector credits, cursors and affinity provenance. Credential retry deadlines join live eligibility gates, and continuity parity checks preserve routing explanations without advancing selector state.

**Implementation evidence:** `sdk/cliproxy/auth/routing_dry_run.go`. `sdk/cliproxy/auth/{routing_strategy.go,routing_trace.go,routing_dry_run.go,selector.go,scheduler.go,gsr_tier.go,session_cache.go}`,
`sdk/cliproxy/routing/contract.go`, `internal/cpauk/model/{routing_trace.go,query.go}`,
`internal/cpauk/chstore/read_selection.go`, and
`internal/api/handlers/management/{analytics.go,routing.go}`.
QA regressions in `sdk/cliproxy/auth/routing_explanation_regression_test.go` cover
expiry after factor preparation, quota refresh uncertainty, inherited LCP provenance,
WebSocket priority and view cursor parity, and the pre-explanation allocation baseline.
`sdk/cliproxy/auth/routing_weighted_merge_test.go` covers exact/dynamic pool parity, pending
reservations, changed weights and epochs, collisions, sticky saturation, public execute/stream/count,
Thread ownership gates and unavailable `never` bindings.
`sdk/cliproxy/auth/routing_dry_run_parity_test.go` adds GSR fallback cursor/credit and
probe-observation regressions; `internal/api/handlers/management/usage_probe_gsr_identity_test.go`
checks shared-key probe caching, persistence, and dry-run/live execution parity.
Validation also exposed a quota-refresh cancellation race: the launch loop now stops
launching on cancellation and joins started refresh workers before reading their
shared preparation result in `conductor_selection.go`.
Management telemetry and model sanitizer tests cover response-only analytics IDs and
the WebSocket JSON input. Regression tests cover shared GSR math
and sources, captured strategy steps, single/mixed scheduler parity, read-only dry runs
against the next manager selection, no quota refresh during simulation, kept/held/rebound
affinity provenance, alias merges, truncation order, query validation, real ClickHouse
request filtering and trace round trips, and management detail resolution.

**Recorded validation:** On 2026-11-18, `sdk/cliproxy/auth/routing_dry_run.go` still privately simulates live selector credits/cursors, factors and affinity provenance. New credentialRetryAfter exclusion composes with trace gates. Weighted/GSR/SIWC/explanation/read-only/provenance tests remain and auth suite passes.

**Last updated:** 2026-11-18

### DL071 — ClickHouse transcript and system-log storage reductions

A production audit found that most ClickHouse growth came from data stored more than once.
Kiro upstream bodies had no transcript format, so each attempt kept its full history and
base64 images in `attempts.request_body_raw`; 84% of those bytes were images resent every
turn. In envelopes, 97% of the bytes were the `tools` array, and only about 93 distinct
arrays appeared per day. Stock ClickHouse system logs also had no TTL.

- `internal/telemetry/transcript` adds the `kiro` format. `conversationState.history`
  items and `currentMessage` become elements; the user and assistant roles, tool use IDs and
  tool names are extracted. History items that do not change deduplicate across turns.
- The request `tools` array (top level, Gemini `request.tools`, or Kiro's
  `currentMessage.userInputMessage.userInputMessageContext.tools`) is replaced with a
  `$cpa_tools_v1` marker and stored as its own content item. It appears as the final
  `request_contexts.content_ids` position with an empty entry ID, so it creates no
  transcript entry, and context-based garbage collection already covers it. Non-compact
  arrays record their separators, so rebuilt bodies stay byte-exact. Envelopes stored
  before this change keep tools inline and still rebuild unchanged.
- Migration 0005 sets `response_body` and `websocket_timeline` on `requests` and
  `attempts` to ZSTD(6) with 8 MiB compression blocks, the same as `transcript_content`.
  It changes codecs only and applies on merge; point lookups on those tables cost about
  10 ms more. Migration 0006 updates schema comments.
- `deploy/clickhouse/config.d/cpa-system-logs.xml`, mounted by
  `docker-compose.clickhouse.yml`, gives the system logs a 7-day TTL, sets `text_log` to
  `warning`, and removes `trace_log`. `docs/clickhouse.md` explains how existing
  deployments reclaim space.

**Recorded validation:** 15 and 12 consecutive real production Claude requests from two
Kiro sessions were run through the real Kiro payload builder. Every body rebuilt
byte-exact in both the client and Kiro views. Stored bytes fell 14.4× (text-only) and 9.4×
(image-heavy) over those windows, and envelopes fell from 541 KB to 3.1 KB. On one real
125-response session, response bodies compressed 1.97× smaller with the 0005 settings.
Element tool IDs and names are deduplicated in one pass, so a Kiro message with 64,000
distinct tool results now parses in 1 s instead of 10 s. An end-to-end run of the built server
against ClickHouse 26.8 and an OpenAI-compatible stub applied migrations 0005 and 0006,
stored each turn's 21 KB tools array once per format, and closed every context with an
empty-entry tools item. Transcript, ingest, store, agentdocs, ClickHouse, and management
tests pass against real ClickHouse.

**Last updated:** 2026-11-07

### DL072 — Credential-scoped Kiro model discovery

Kiro credentials register only models returned by their authenticated
`ListAvailableModels` endpoint. Registration includes the credential's region
and profile, follows pagination, and preserves advertised names and token limits.
Claude dotted minor versions become CPA's existing dashed public names; other
model IDs, including `auto`, remain unchanged. Known static metadata enriches
discovered entries without granting access to models missing from Kiro's list.
Every entry retains Claude-compatible translation through the Kiro executor.

Successful catalogs are cached for three hours. A minute scan refreshes due
accounts through the existing bounded registration workers and publication
fences. Failed refreshes retain the last successful catalog and retry after a
minute; failed first discovery advertises no inferred models. Empty catalogs
remove prior registrations, and incomplete paginated responses never replace a
successful snapshot. Cache identities separate credentials, registration epochs,
profiles, regions, and configured proxy routes. Model exclusions, OAuth aliases,
settings, prefixes, scheduler updates, and credential-removal fences still apply.
The Kiro management model-definition list also uses discovered registrations.

Case-only Kiro aliases retain discovered metadata, and credential-local identity matches suppress global alias fallback. Provider-scoped lookup can enrich metadata but cannot advertise or grant access to an undiscovered Kiro model; registration uses the executor wrapper.

**Implementation evidence:** `internal/runtime/executor/helps/kiro_models.go`. `internal/runtime/executor/helps/kiro_models.go`,
`sdk/cliproxy/kiro_models.go`, `sdk/cliproxy/service_models.go`,
`internal/registry/model_definitions.go`, and their Kiro regression tests.

**Recorded validation:** On 2026-11-18, `internal/runtime/executor/helps/kiro_models.go` retains authenticated, paginated credential-scoped membership, cache/fences and metadata enrichment. Case-only/local aliases preserve case-only mappings and credential-local identity precedence. Discovery/failure/empty/removal/profile/proxy and wrapper tests remain and pass, including focused races.

**Last updated:** 2026-11-18

### DL073 — Reject forced hosted web search on Chat Completions routes

Responses-to-Chat Completions translation rejects an explicit hosted
`web_search` or `web_search_*` `tool_choice` with a request-scoped HTTP 400
naming the tool and directing the caller to a provider with native Responses
search support. Previously that choice was passed through with no matching tool.

Optional hosted search declarations (top-level, namespace, Responses Lite
`additional_tools`, and loaded tool-search declarations) are still dropped
silently, because Codex CLI declares `web_search` on every request by default.
Rejecting declarations broke every Codex request routed to a Chat Completions
provider such as DeepSeek. Caller-defined functions named `web_search` continue
to translate normally. Native Responses routes keep their existing hosted-tool
behavior. The forced-choice error propagates through streaming, non-streaming
and token-count translation before an upstream request is sent.

Shared media translation preserves the fork request-scoped 400 for forced hosted search, dropped hosted declarations and caller-owned function tools; refused requests make no upstream call.

**Implementation evidence:** `internal/translator/openai/openai/responses/hosted_web_search_test.go`.
`internal/translator/openai/openai/responses/{openai_openai-responses_tools.go,openai_openai-responses_request.go,hosted_web_search_test.go}`
and `internal/runtime/executor/openai_compat_executor_hosted_tools_test.go`.

**Recorded validation:** On 2026-11-18, `internal/translator/openai/openai/responses/hosted_web_search_test.go` still verifies request-scoped forced-search 400, dropped declarations and caller-owned functions after upstream media translation changes. Actual execute/stream/count fixture proves zero upstream calls; both suites pass.

**Last updated:** 2026-11-18

### DL074 — Opt-in instruction role wrapping for OpenAI-compatible providers

Each `api-keys.openai-compatibility` provider can set `instruction-role-wrapper: true`
when its Chat Completions upstream rejects developer messages. The default is false.
When developer messages are present, CPA adds an explicit system/developer/user
precedence policy and wraps each system/developer text instruction as a labeled
system message at its original position. The JSON record retains the original role,
position, and text content, including text-part arrays. Other message fields and
conversation/tool order are preserved. Requests without developer messages are unchanged.

The wrapper expresses priority through text; it does not provide native role
isolation. It adds input tokens and changes prompt serialization and cache reuse.
Non-text instruction parts are rejected instead of being dropped. Native
`/responses/compact` and image payloads remain unchanged. Wrapping runs before
user payload rules in streaming and nonstream requests and before final token
counting, so final overrides and filters remain authoritative. Home credential
options can carry the same flag. CPAMC exposes a provider behavior checkbox with
its default off and preserves explicit false and untouched absence on save.

Instruction wrapping runs before final payload rules and ordered replacement. Explicit user rules and filters remain authoritative for both compatible request protocols and token estimates.

**Implementation evidence:** `internal/runtime/executor/helps/openai_instruction_roles.go`. `internal/config/config_types.go`,
`internal/runtime/executor/helps/openai_instruction_roles.go`,
`internal/runtime/executor/openai_compat_executor.go`,
`internal/watcher/diff/openai_compat.go`, and `config.example.yaml`.
Executor regressions cover both client formats, streaming, opt-in behavior,
message/tool order, text arrays, payload-rule ordering, token counts, and Home
credential options. Config regressions cover v8 validation and save/reload.

**Recorded validation:** On 2026-11-18, `internal/runtime/executor/helps/openai_instruction_roles.go` remains opt-in via parsed provider/Home config, runs before final rules/replacement, and rejects nontext instructions. Both protocols/modes, order, token overhead, authoritative final rules and save/reload tests remain and pass.

**Last updated:** 2026-11-18

### DL075 — Session affinity survives restarts through the existing KV store

When session affinity and the local KV store are enabled, explicit session bindings
and history-prefix affinity groups persist in the configured bbolt database.
No additional path or persistence timer is required. Disabling local KV retains
memory-only behavior; Home continues to own its own routing state. SDK callers
constructing selectors directly can opt in with `SessionAffinityConfig.Persistent`.

Each cache operation commits its changed groups and deletions atomically before
returning. Recovery retains aliases, original binding provenance, parent/fork and
compaction metadata, selection order, eviction order, and access generations used
to reject delayed failures. Expired bindings stay expired, shorter configured TTLs
cap restored lifetimes, and normal credential eligibility and failover policy still
apply. Restores are bounded by entry and byte limits. A replacement cache fences
the previous cache's durable writes, preventing late operations from overwriting
newer routing state after a reload. Storage errors are logged without session or
credential values, and routing continues with its in-memory state.

This implements the restart-continuity goal of upstream
[PR #6775](https://github.com/router-for-me/CLIProxyAPI/pull/6775) using the existing
KV store instead of its separate JSON snapshot file and periodic/shutdown flushes.
It prevents restart-driven account reassignment; it does not make encrypted
reasoning portable across organizations or prevent intentional failover.

The config-applied hook follows executor registration; standalone KV-backed affinity persistence retains restore bounds, provenance, TTL and writer fencing, and remains disabled in Home mode.

**Implementation evidence:** `sdk/cliproxy/auth/session_cache_persistence.go`. `internal/kvstore/persistent_map.go`,
`sdk/cliproxy/auth/session_cache_persistence.go`,
`sdk/cliproxy/session/lcp_persistence.go`, and `sdk/cliproxy/service_config.go`.
Regressions reopen the KV database without stopping the selector, preserve both
explicit and history-based bindings, cover routing reload and disabled KV, and
exercise alias/provenance restoration, expiry, eviction, compaction, generation
fencing, durable invalidation, transaction rollback, and superseded writers.

**Recorded validation:** On 2026-11-18, `sdk/cliproxy/auth/session_cache_persistence.go` and LCP/KV adapters retain atomic operations, restore bounds, aliases/provenance/TTL and writer fencing. Service config still enables local persistence only outside Home; new config-applied hook follows registration. Restart/reopen/reload/KV tests pass.

**Last updated:** 2026-11-18

### DL076 — Locally refused credential attempts appear in analytics

When CPA refuses a credential before sending anything upstream, it still publishes a failed
analytics attempt (the refusal's status, `cpa_to_provider` failure hop, no upstream status)
so the event history shows that credential beside the fallback that follows. Previously the
conductor moved to the next credential silently, and a request could surface only the last
provider's error.

Covered refusals: a missing Claude Code reference under the OAuth safeguard; Codex SIWC/OAuth
token checks on HTTP, compaction, image and WebSocket execution (direct image routes record the
image model, not the backing text model); an unavailable KV store during the Antigravity cooldown
read; Meta credential resolution and request preparation; and xAI request preparation on HTTP,
compaction and WebSocket streaming. The Antigravity short-cooldown skip stays unrecorded because it is a
deliberate fast switch. When request auth preparation fails (for example a token refresh), the
conductor hands the failure to executors implementing `PrepareFailureReporter` (Claude,
Antigravity, Meta) for non-streaming, streaming and Home execution; token counting records no
attempt.

Executor preparation failures still report a local credential attempt through `PrepareFailureReporter` for execute, stream and Home paths, with original model identity and no invented upstream status. Token counts remain excluded.

**Implementation evidence:** `internal/runtime/executor/helps/usage_helpers.go`. `internal/runtime/executor/helps/usage_helpers.go`
(`PublishAttemptFailure`), the Claude, Codex, Antigravity, Meta and xAI execute/stream/WebSocket files,
`sdk/cliproxy/auth/{conductor.go,conductor_execution.go,conductor_home_execution.go}`,
`internal/runtime/executor/attempt_failure_usage_test.go`, and
`sdk/cliproxy/auth/conductor_prepare_failure_report_test.go`.

**Recorded validation:** On 2026-11-18, `internal/runtime/executor/helps/usage_helpers.go` remains called for all documented local refusals; execute/stream/Home conductor preparation failures still use PrepareFailureReporter, counts do not. Exactly-once/source-hop/no-upstream-status and model identity regressions remain and pass normally and with race detection.

**Last updated:** 2026-11-18

### DL077 — Live proxy request monitoring

The process keeps an in-memory view of admitted proxy requests and their upstream attempts.
`GET /v8/management/live-requests` exposes their derived states, provider acknowledgement time (`ack_at`: the provider's first answer to the attempt, HTTP
response headers or a WebSocket frame, kept across retries within the attempt; analytics provider
latency is measured from the same signal but follows the latest dispatch), output timing
and byte counts,
reported token counts, credential display labels, short API-key identifiers, and an optional
eight-character SHA-256 digest of the client session ID (never the ID itself; the latest non-empty
attempt session wins) with the same digest of its parent session when the request metadata names one
(a subagent's main session), to read-only management keys. Completed requests remain for 30 seconds, with
at most 200 retained and at most 16 attempts shown per request; request-level output byte and token
totals still count every attempt, so long websocket sessions are not undercounted. Model discovery is
excluded. Raw API keys and request content are not stored in this registry. A stream that fails
after its response headers ends as failed, and usage reported after the request closed still lands
on its attempt. CPAMC's Live Handle page draws this feed as a request map. The view supplements
upstream execution and usage reporting without changing routing or the legacy management routes.

`GET /v8/management/live-requests/stream` has the same read-only access and sends server-sent events
(`data: <json>` frames): a full snapshot (`"full": true`) on connection, then every second the
requests whose state changed, `progress` tuples `[id, request output bytes, attempt index, attempt
output bytes, last output time]` for requests that only streamed more output, and `removed` IDs. The
per-second frame doubles as a heartbeat carrying `server_time`. Private revisions drive the delta: any
change advances a request's revision, and every change other than output growth after the first
output also advances its shape, so steady streaming costs a tuple instead of the whole request. A
shared broadcaster per registry takes one snapshot and encodes one frame per tick for all
subscribers, caches the tick's full snapshot for new subscribers, closes subscribers whose bounded
queues fill so they reconnect for a fresh full, and stops its ticker when idle. Totals, rates, and
any feed are left to clients. A stream checks its caller again when it opens, against settings read
before that check, and again whenever management access settings change (keys, remote access, or
the secret); it closes before its next frame if the key no longer has access, or once the Management API stops
serving (Home mode, or no management secret). These re-checks do not
count as failed logins, so revoked streams cannot get their address banned. Credential labels are
read from the auth manager when each frame is built, so a credential renamed during a long request
shows its new name, and the delta resends a request whose decorated key or credential label changed.

**Implementation evidence:** `internal/livehandle/` (`registry.go`, `stream.go`),
`internal/api/server_middleware.go`, `internal/api/server_management_v8.go`,
`internal/api/handlers/management/{live_requests.go,management_keys.go}`,
`sdk/cliproxy/auth/{conductor_execution.go,types.go}`, and
`internal/runtime/executor/helps/{usage_helpers.go,output_observation.go}`. Registry, middleware,
handler, and reporter tests cover lifecycle, late usage, post-header stream failure, request totals
beyond the attempt window, retention, privacy, and the read-only routes. Deterministic race-enabled
stream tests cover session and parent digests, acknowledgements kept at the first answer, and revisions, one snapshot and one shared frame per tick, cached
full-to-delta consistency, additions, changes, progress-only growth, removals, label updates,
slow-subscriber eviction, and idle shutdown and restart. A route test revokes the viewer's key
mid-stream and expects the stream to close without another frame; handler tests cover a key revoked
between middleware and handler, re-checks that leave the ban counter alone, and current credential
labels; a stream test resends a renamed credential that carries no new revision. A route test turns the
Management API off mid-stream and expects the stream to close, and a race-enabled test decorates
while API keys are edited.

**Recorded validation:** focused registry, stream, reporter, conductor, API, and management handler
tests pass with `-race`, and the progress test fails if output growth advances the shape. With the
panel bundle built, the full suite passes apart from two load-sensitive tests
(`TestPricingDiscoveryThroughManagementAPI`, `TestC4bBulkRetentionDropsMonthsAndKeepsReceipts`)
that pass when their packages are rerun alone; the race-enabled API suite also reports races in the
logger hot-reload and targeted OAuth tests outside this feature. Against a local CPA with
OpenAI-compatible, Codex API-key, and Claude API-key credentials, streamed requests moved through
waiting, streaming, and finished with exact token counts and matching request-level totals; routing
failures (503 with no attempt) and upstream 400s ended as failed; the payload carried no raw keys;
and a read-only management key could read the route. With real streaming traffic on two
header-identified client sessions, the stream answered `text/event-stream` with
`X-Accel-Buffering: no`, delivered frames 995–1004 ms apart, sent 31 of 42 frames as progress-only,
and carried session digests that CPAMC drew as session nodes.

**Last updated:** 2026-11-18

## Sync Checklist

Project-specific checks every upstream sync runs, in addition to the full test suite, lint, typecheck, builds, and a smoke test of upstream and fork features.

- Sync the CPAMC submodule (`web/management-center`) with its own upstream first, following its FORK.md, and push it. Then advance the gitlink and rebuild the bundle with `./scripts/build-management-center.sh`; `TestBundledArtifactIsValid` needs that bundle.
- Apply new CPAUK SQLite and ClickHouse migrations to a copy of a real analytics database. Confirm a rerun is a no-op and existing events, costs, and quota history stay readable.
- Run once with `clickhouse.dsn` against a real ClickHouse (analytics, request capture, agent SQL) and once without it. Without a DSN, CPA must behave like upstream: file request logs, no telemetry routes.
- If auth is set up, send live OpenAI- and Claude-format requests, streaming and non-streaming, through at least one OAuth provider and one API-key provider. Use the cheapest model that exercises each path.
- Build the Docker image and the `docker-compose-web.yml` stack, and confirm the panel and proxy are served.
- Check CPAMC against the merged CPA in a browser at desktop (1440px) and mobile (390px) widths.
- If a change touches CLIProxyAPIHome integration (`internal/home/`), check whether the CLIProxyAPIHome repository needs a matching update.
- Live-test DL026 metadata-only Lite image injection whenever upstream changes hosted-image policy.
- When upstream changes a shared translator default, check fork-only providers that reuse it, including Kiro.

## Merge History

This is an append-only decision record, newest first, with one entry per upstream sync. It explains past integrations but does not by itself establish a current divergence; use the Divergence Log for that. Sync with upstream only when the user explicitly asks. Do not edit past entries.

### 2026-11-18 - Merge upstream `main` at `a5e8f69e5`

Target: `upstream/main` at `a5e8f69e56309e1ef6d6872e667da81fdfbce2ce`.
Parents: fork `9b5fe9d1cd40fb9434b4cb586e314fc1292c0dd8` and upstream `a5e8f69e56309e1ef6d6872e667da81fdfbce2ce`. Merged 76 upstream commits since the previous upstream base `1aaf56bc6a0fc6fce19049ba7ae8554c882304a6`, without rebasing or squashing. The panel pin advances from CPAMC `8b7056547848670b72d85977c34bbcd825a986b1` to `7e1e738499f4fe9b65555c5127dae62502572289`, its two-parent sync through upstream `e5eb4fe63ad8a2a20d13022e13e6dc4d386ac07b`, 26 incoming commits.

User decisions: accept terminal Antigravity TOS deactivation with pool protection disabled by default. Use native Claude structured output while retaining Kiro schema-in-prompt fallback. Integrate ordered payload replacements into the retained single-evaluation image plan; Kiro supports final-wire paths only. Adopt opt-in Codex continuity within fork routing, reserves, affinity and retry gates. Adopt credential-bound OAuth plugin catalogs, request-scoped policy-error stops, case-only/local alias precedence, native Gemini thinking levels and strict provider lookup. Charge Muse search/browser admission, exempt discovery and propagate native client-key identity for reserve bypass. Allow the observational in-flight GET for read-only scoped management keys. No divergence was retired.

Other decisions: retain every fork quota, analytics, routing, identity and packaging contract alongside upstream GPT-6.2/Gemini 4 metadata, caller-owned interceptor wrappers, zero-output handling, media translation, sensitive-pattern replacement, retry hints and management authentication caching. Scope and revocation checks remain per request. Shared media support does not imply additional Kiro adapter capabilities. Docker images compile build-context panel source with Bun 1.4.3; CI verifies clean pins separately from the canonical builder.

Integration repairs: compose credential retry/force/non-quota deadlines with persisted scoped independent failures and terminal TOS state. Move Claude cleanup before final rules and retain replacement effects through automatic-image cleanup and final filters. Wrap ordinary and forced Kiro registration. Preserve local diagnostics while importing old and labeled response logs. Release bounded observer registrations in new executor fixtures; reconcile nested moego module dependencies and scheduler/key/trace fixtures. Keep active Codex streams responsible for terminal WebSocket HTTP errors and normalize cancellation-caused Antigravity body-close errors. Make terminal pool Retry-After reserve-aware with `m.eligibilityForRequest` and `TestPoolCooldownTerminalErrorIgnoresReserveHeldCredential`.

Validation:

- The implementation handoff records 128 passing test packages with only `TestBundledArtifactIsValid` failing on the stale bundle, plus passing vet, formatting, the disposable server build and all 23 nested example builds. Independent audit A passes `go test -p 2 ./... -count=1 -skip '^TestBundledArtifactIsValid$'` and `go vet ./...` successfully. Audits cover all 76 divergences without a verified production regression or deleted fork test file.
- HTTP, WebSocket and AWS event-stream fixtures cover schema fallback, admission, wrappers, payload ordering, local failures and read-only in-flight access. Focused routing, cooldown, executor, handler and management race suites pass. Repaired terminal-429 stress passes 100 repetitions; Antigravity completion passes 1,000 repetitions. Risk review confirms continuity eligibility, restart/recovery deadline composition and the final payload barrier; its reserve-aware pool-cooldown finding is fixed with a named regression.
- Real isolated ClickHouse suites exercise storage, capture, import, recovery and pricing. Read-only Compose assertions verify both build arguments and published-image overrides; builder/verifier shell syntax checks pass. CPAMC post-repair verification records 3,539 passing tests with lint and build clean.
- Final `go test -p 2 ./... -count=1` on the merged tree with the rebuilt canonical bundle passes all 129 tested packages, including `TestBundledArtifactIsValid`; `go vet ./...`, `gofmt` and the disposable server build pass. The pre-merge baseline passed 127 packages.
- Paid live checks on a plain-mode merged binary pass Chat Completions and Claude Messages, buffered and streaming, through Claude OAuth, Codex OAuth and an OpenAI-compatible API-key provider, plus Codex Responses; the API-key provider returns the expected text in all four modes. Per-key admission then rejection passes. A literal `payload.replace` rule reaches the upstream body. Full and read-only scoped management keys both read in-flight counts while the read-only key's config write returns 403. Plain mode writes file request logs and serves no telemetry routes; analytics health reports `disabled`, as before the merge.
- A ClickHouse-mode server against a disposable real ClickHouse applies its 10 migrations, restarts as a no-op, imports a legacy SQLite analytics copy with event, token, cost and quota-history totals preserved, captures a live inference into analytics and request capture, and exposes it through restricted SQL. No CPAUK migration is added by this sync.
- Both runtime images build, and the disposable web Compose stack serves the panel and proxy and passes a live inference. Integrated Chrome QA of the canonical panel against the merged binary covers every main route at 1440px and 390px in light and dark themes without page errors, including read-only sessions and a config round trip that preserves key labels, limits and comments.
- Temporary servers, containers, volumes, images, copied credentials and key-bearing configs were removed after verification. Existing user services were left untouched.

Limits:

- The Kiro accounts available were disabled, so Kiro's paid happy path was not certified; Kiro schema fallback, final-wire replacements and alias handling are covered by fixtures. Firefox, WebKit and native macOS/Windows behavior were not checked. No live Home cluster is certified; the incoming Home changes require no new mandatory wire method, and earlier scheduling gaps remain outside this sync.
- Audit B's broad run did not establish a clean full suite from its retained output; its isolated executor `-count=1` run passed. The pre-existing `TestAntigravityAuthHasCreditsRequiredHomeBalanceUsesKV` leaks an SDK credits hint under `-count>1` and remains unfixed. An unchanged ClickHouse query-ID collision reproduced on the pre-merge tree and passes in isolation on the merge. Neither is claimed repaired.

### 2026-11-13 - Merge upstream `main` at `1aaf56bc6`

Target: `upstream/main` at `1aaf56bc6a0fc6fce19049ba7ae8554c882304a6`.
Parents: fork `8bab456cfdd97ced1ccabba71e6083de8d2a9573` and upstream target. Merged 101 upstream commits since the previous `base` merge `e603442162035ae7d5a15efeb7b613f085cc29ac`, without rebasing or squashing. The first parent preserves the existing local Signature Stabilization commits. The panel pin advances to CPAMC `3fad6d14a2d0269ca83681973480b8637a43bcd1`, its pushed two-parent sync through upstream `7a662031df9810f7b4168583529dfdd34a8779a8`, 22 incoming commits.

User decisions: adopt upstream's default-off Claude server-side refusal fallback, preserving opt-in and explicit caller/payload fallbacks. Adopt standard-Responses image-tool injection without reasoning on catalog-Lite models while retaining the fork's metadata-only Lite exception, including reasoning-capable requests and explicit effective Lite=true header precedence. Approve the narrow Codex automatic-image evaluation-order exception in AGENTS.md: rule conditions may observe the prepared automatic tool even if cleanup later removes it before the single commit; configured values and final filters remain authoritative. No divergence was retired.

Other decisions: keep every logged fork behavior alongside upstream's Thread ownership, served-model diagnostics, local compaction, WRR reservation accounting, affinity never-mode, explicit forced cooldowns, refresh/reload changes, tunnel service and opt-in HTTP shutdown drain. GSR remains a distinct recovery-weighted algorithm; its tiers, additive weights, reserves, routing rules and fallback cursor remain intact. Structured TUI keys retain labels, limits, original indexes and revisioned v8 writes. Temporary 429/5xx failures keep models listed. The newer coherent Claude identity/CAS design remains on the updated client tuple.

Integration repairs: copy the actual WRR pool state for pure ten-pick simulations and separate public mixed-provider accounting from provider-scoped affinity cache keys, preserving progress and pending reservations across affinity reloads. Fix xAI WebSocket prediction preference. Preserve partial Claude usage and source timing before Thread terminal buffering. Deep-copy and bound HomeSelection in fork delivery without changing trusted accounting. Reclaim owned TLS/WebSocket raw transports without adding post-connect deadlines. Exclude downstream X-Goog-Api-Key credentials from compatible-provider forwarding after a failing HTTP regression. Synchronize tunnel test registration through an actual control response rather than treating a completed upgrade as a committed peer. Update the panel credit-row flex ordering while preserving the fork layout and locale union. The payload-ordering repair retains evaluated operations, performs automatic cleanup before their final commit, and preserves final empty-tools filters; fresh review confirms its mechanics and the user explicitly approved the documented evaluation-order exception.

Validation:

- The pre-merge full Go baseline passed 127 packages. Final `go test -p 2 ./... -count=1` passes all 128 tested packages with real isolated ClickHouse integration tests. `go vet ./...`, `gofmt`, the required disposable server build and actionlint pass. All 23 nested example modules build. Focused auth, service reload, usage, identity, Thread, payload and tunnel race suites pass. The full tunnel suite passes ten consecutive race runs after readiness synchronization.
- An initial concurrent root run tripped an unchanged 20 ms cache-control timing assertion. Twenty isolated merged runs, 100 isolated pre-merge runs and the merged full executor package passed; the final full suite passes without changing that assertion. A subsequent run exposed two more tunnel registration-test races, repaired in the shared helper and covered by the final suite.
- The real SQLite online snapshot migrates schema 9 to 19 with all 6,004 original events, fields, token totals and costs preserved. Reopening is a logical no-op, quick_check is ok and foreign-key checks are clean. A separate source snapshot imports into real ClickHouse; rerun changes neither totals nor physical event count and leaves the source file unchanged. No new CPAUK migration file was introduced.
- Paid live Codex and Claude OAuth checks pass Chat, Messages and Responses in buffered and streaming modes. The API-key provider passes Chat and Messages in both modes. Per-key admission/rejection passes. A final merged-binary metadata-only Lite WebSocket request with low reasoning produces a valid 1254 by 1254 PNG, confirming automatic image injection. Actual TUI interaction renders structured keys, saves a label edit through v8 and preserves its request cap.
- A plain-mode local server writes file request logs and has no telemetry route. A ClickHouse-mode server imports the real snapshot, serves inference, captures a request and exposes it through restricted SQL. Both images build with pinned toolchains. The disposable Compose stack serves the exact canonical panel digest, management and analytics through Nginx, executes a real inference, captures it, and supports authenticated WebSocket tunnel discovery. Its fresh ClickHouse applies ZSTD(6), 8 MiB blocks and seven-day system-log TTLs; trace_log is absent.
- CPAMC passes 3,163 tests, lint, TypeScript and production build with pinned Bun 1.4.3. Fresh panel QA covers all 80 entries and verifies the 4,043-key union in all seven locales. Forty-four integrated desktop/mobile and light/dark route checks have no page errors or horizontal overflow; quota reads, credit-dialog opening and read-only restrictions pass. Separate backend audits cover all 72 entries; final independent repair review confirms credential-header filtering, tunnel readiness synchronization and the retained-plan mechanics. Its sole contract objection was resolved by the explicit Codex-only exception approved above. No upstream account setting was changed.
- Temporary servers, Compose containers/volume/network, copied credentials and identity/database copies were removed after verification. Existing user services were left untouched.

Limits: the selected Kiro account returns monthly-quota exhaustion, so its paid cross-protocol happy path was not certified; shared translator/executor regressions pass. The historical database predates quota-history rows, so populated historical quota preservation is covered by synthetic real-ClickHouse tests rather than that real snapshot. Native macOS/Windows behavior, legacy animation details and native-speaker translation review were not rerun. CLIProxyAPIHome main `f31b149c18c50a84fdb9f6ba5194a45754ad0749` supports PONG liveness, core cloak metadata and CAS transport, but needs matching reused-turn scheduling compensation and kind-aware scheduling work; no live Home cluster is certified or changed here.

### 2026-11-07 - Merge upstream `main` at `c5251b57`

Target: upstream `main` at `c5251b57b3fa5dcbc689f514f4aa166daacb4481`.
Parents: fork `main` `000bcabaec32a75971b757f9c8ada018cbc89dba` and upstream
`c5251b57b3fa5dcbc689f514f4aa166daacb4481`; 132 upstream commits since `base`
`b1df7638ff3082b1a9b2e0dbbad4f7d240f506a3`. The CPAMC gitlink moves from
`a4ec1ed` to `eea93b6`, the CPAMC fork's own sync with its upstream `953fb24`
with 10 commits, recorded in `web/management-center/FORK.md`.

Integrated upstream's moego JSON/Base64 migration, default local bbolt KV,
permanent Claude session-date pins, general native-entrypoint configuration,
Claude OAuth state and scope changes, per-key connection pools, OAuth-provider
metadata, uncertain Codex refresh protection, native Images API SIWC bypass and
GPT Image 2.6 support, Responses-Lite hosted-image policy, remote compaction,
ToolSearch and custom-tool normalization, terminal-response preservation,
availability-neutral unsupported token counts, plugin lifecycle and TTFT fixes,
and multi-part Gemini/Antigravity reasoning and text preservation.

User decisions: keep DL026's metadata-only Lite image exception on upstream's
shared policy helper. A pre-merge live websocket call with Codex OAuth Pro-lite
and `gpt-6.1-sol` returned 200 and a valid 1254×1254 PNG with the injected tool.
An explicit Lite header takes precedence; catalog-Lite detection applies when
no true metadata marker exists. Accept upstream's default `<auth-dir>/kv.db`
and permanent Claude date pins; `session-date-pin-days` is ignored with KV
active, and standalone deployments can opt out with `server.kv-store.disabled`.
Accept `org:create_api_key` for new Claude OAuth logins; refresh scope is unchanged.

Other decisions: all 52 content conflicts keep both feature sets. Preserve
bounded usage delivery, synchronous per-key accounting, timing and diagnostic
capture, GSR, reserves, routing rules, retry rounds, and asynchronous analytics
reload. General native entrypoints and safeguard-only identities keep separate
scopes and cache identities. ClickHouse/SQLite dependencies coexist with moego
and bbolt. No divergence was retired. The pre-existing direct Codex image-stream
cancellation gap in DL058 is reserved for a separate follow-up commit.

Integration repairs: port executor observers, usage capture, per-dispatch request
capture, the Claude safeguard, collaboration optimizer, and Kiro thinking to
`ojson.Doc`. Restore sanitized Gemini/Antigravity generation diagnostics through
buffering. Preserve labels/selectors beside connection pools, bounded usage
queue-slot reclamation, and deep cloning of cached image/audio usage pointers.
Fence KV files and sidecars from credential scanning, parsing, and watcher events.
Close KV and retire pools after the bounded shutdown drain. Make SIWC selection,
trace gates, GSR affinity refresh, and dry runs request-aware; add validated
`source_format: "openai-image"` for native image simulation and update embedded
docs. Include external local KV files in storage accounting. Pin Kiro's late
Responses system/developer items to top-level system content across execute,
stream, and count paths. Strengthen DL047's regression to attempt Claude before
Kiro and prove request-scoped rejection stops fallback. Preserve actual stream
HTTP status and add telemetry's no-op `WriteAPIResponseErrors`. Reconcile nested
example modules, remove an unused `sync.Once` copy that failed vet, and join
websocket reauthorization and HTTP interrupt test goroutines before assertions
or cleanup. All 98 v8 management routes already have agent-docs inventory entries.

Validation at merge time: `go build ./...`, `go vet ./...`, and the disposable
`cmd/server` build pass. `go test ./... -count=1`, with Docker available for the
ClickHouse test container, passes 126 packages; the only failure is the upstream
flake `TestCodexExecutorRefreshSelectiveAndDual` described below. Five
consecutive `sdk/cliproxy/auth` runs pass. Each audit repair has a regression test
that failed before its fix and passes after it. The canonical CPAMC bundle for
`eea93b6` builds and the bundled-artifact test passes.

The pre-merge baseline had 124 passing packages and failures in
`TestC4bBulkRetentionDropsMonthsAndKeepsReceipts`, a ClickHouse query-ID collision
that passed after merging, and the management bundle test, whose baseline
worktree lacked the submodule. Before the audit repairs, the merged suite had
126 passing packages and only `TestCodexExecutorRefreshSelectiveAndDual` failing.
That test and its code match upstream; it fails 9/20 runs on pure upstream and
14/20 on the merge, establishing an upstream flake. Reported build, vet,
real-ClickHouse, routing, credential-fence, storage, and nested-module checks
pass. All 22 plugin examples and the realtime example build offline.

Live E2E checks used a temporary loopback CPA and credential copies with refresh
tokens stripped, removed afterward. Codex OAuth `gpt-6-luna`, Claude OAuth
`claude-haiku-4-5`, and Z.ai OpenAI-compatible `glm-4.5-air` pass OpenAI, Claude,
and Responses requests with streaming and non-streaming. The attempted
`glm-4.7-flash` received upstream 429 code 1305. Per-key limits, no-DSN file logs
and telemetry-route 404s, disposable ClickHouse capture/SQL/restart, local KV
creation and credential exclusion, and date-pin persistence across restart pass.
The Docker image and `docker-compose-web.yml` stack serve the panel and proxy
with HTTP 200. CPAMC browser checks at 1440px and 390px in light and dark themes
cover Quota, Auth Files, Providers, OAuth, Routing, Config, Analytics, Events,
and Usage without horizontal overflow. Console 404s for unset OAuth model aliases
and excluded models are the previously triaged upstream default behavior.

Not verified: SQLite-to-ClickHouse migration on a real database, since this merge
adds no CPAUK migrations; corresponding CLIProxyAPIHome repository changes for
upstream `internal/home` updates. DL026 was rechecked live on the merge commit:
the metadata-only Lite websocket request on `gpt-6.1-sol` carried the injected
tool and returned a valid PNG, and the header-Lite HTTP request carried no image
tool and returned 200.

Follow-up commits, oldest first:

- `a534d7c1d`: record canceled direct Codex image streams as client cancellations while retaining observed usage (DL058). This gap predates the merge.

### 2026-10-31 - Merge upstream `main` at `dbb70621`

Target: upstream `main` at `dbb706215` (v8.0.48). Parents: fork `main` `63cf82582` and upstream `dbb706215`; 230 upstream commits since `base` (`20e098bf`). The CPAMC gitlink moves to `b380bea`, the CPAMC fork's own sync with its upstream `4e4aeff` (41 commits, recorded in its FORK.md).

Integrated upstream's structured per-provider usage capture (Anthropic, OpenAI/Codex, xAI, Gemini, Vertex, AI Studio, Antigravity), Codex websocket early-429 credential retry and downstream keep-open on upstream disconnect, Codex request compression with plaintext fallback, response compression encoder reuse and bounded zstd request decoding, home-mode websocket restriction, plugin-host multi-provider registrars and body-delivery opt-out, cooldown-state placement next to relative auth directories, incomplete-stream cooldown exemptions, AI Studio SSE frame reassembly, discovery TXT schema, signature and translator fixes, and Claude client-detection baseline updates.

User decisions: Codex plan badges follow upstream (platinum only for Pro 500; Pro 200 and Pro 100 gold). Plan-allowance updates (Codex Pro 100/200/500 = 10x/20x/50x Plus; Claude Max 20x renamed Ultra at 10x Pro weekly, Max 5x renamed Max) land as separate commits after the merge, in CPAMC and in the GSR tier mapping. The DL003 test helper is fixed, the DL008 regression test is restored, and DL057 pricing uses the effective outbound tier. Divergences that the audit found already broken before this merge (DL001, DL002, DL003, DL008, DL023, DL038, DL057, DL058, the no-DSN telemetry routes of DL060) are fixed in separate follow-up commits, and the DL032/DL051 caches are ported to the ClickHouse backend.

Other decisions: all 33 content conflicts keep both sides. Usage-manager and executor usage helpers combine upstream's structured usage with the fork's bounded delivery, timing, outcome, diagnostic, and routing-trace metadata. Claude client detection keeps the fork's configured-baseline acceptance and safeguard rules on upstream's baseline. Conductor selection keeps GSR, reserves, retry rounds, and routing rules with upstream's scheduler snapshots. No divergence was retired.

Integration repairs: Codex compression retries now capture each upstream dispatch (DL060 regression found in the audit, fixed with buffered and streaming tests). Runtime and panel Dockerfiles move to digest-pinned Bun 1.4.3, which the merged CPAMC requires. Structured usage cloning and identity propagation, executor test observer cleanup, capture fixtures, a zstd single-segment fixture, seven new agent-docs route inventory entries, and a Home reconnect test allowance were adapted.

Validation at merge time: `gofmt`, `go build`, and `go vet ./...` pass. `go test ./... -count=1` with the ClickHouse test container passes except the pre-existing flaky `TestCodexWebsockets_KeepalivePingDuringUpload_Sessionless` (30/5,000 failures before the merge, 20/5,000 after). The pre-merge baseline also failed `TestClaudeExecutorManagerRetriesSilentTLSHandshakeThroughConnectProxy` and `TestInterruptExecutionSessionRequiresActiveRead`. The canonical CPAMC bundle builds and the bundled-artifact tests pass. Three independent audits covered all 65 divergence entries. CPAMC passes 2,576 tests, lint, TypeScript, build, and desktop/mobile browser checks.

Follow-up commits, oldest first:

- `5b5dad023`: exempt Gemini discovery and token counting from request quota (DL001).
- `095b94479`: update GSR tiers for Codex Pro 500 and Claude Ultra (DL028).
- `03ee9fe3c`: pass the panel commit to both web Compose builds (DL002).
- `dafc68e2f`: restore status-less request retry rounds (DL023).
- `8305759c8`: propagate reserve-bypass identity to Codex alpha search (DL038).
- `45176a9d4`: restrict the shared Codex Live media test fixture to loopback (DL003).
- `1eb30486c`: restore the fresh deferred-shutdown budget regression (DL008).
- `9d05f2a0a`: price events using the effective outbound tier (DL057).
- `02c5740e5`: retain observed usage on stream failures (DL058).
- `3e8df25f0`: omit telemetry SQL routes without ClickHouse (DL060).
- `d1d61b328`: port historical result and closed-day activity caches to ClickHouse (DL032/DL051).
- `874f418fe`: build Docker images with Go 1.26.10 to match `go.mod`.
- `79cd2d4b8`: advance the CPAMC gitlink to `fa7f4f2`.
- `84dbdeb12`: exclude uTLS record-header failures from retry rounds, from QA (DL023).
- `bfb9c514d`: exclude `<auth-dir>/state` from the credential store, from E2E checks. This DL001 gap predates the merge.

Validation of the final tree:

- The disposable `cmd/server` build, `go build ./...`, and `go vet ./...` pass. Uncached executor, SDK, agent-docs, CPAUK, and API suites pass; API tests were rerun after telemetry fixture updates. Real Docker ClickHouse tests cover schema/query, disconnect cancellation, conversation capture, historical caches, and DL056 model cost series. Touched Go files are `gofmt`-clean.
- `go test ./... -count=1` with the ClickHouse test container running: 125 packages pass. `internal/runtime/executor` crashed once with `concurrent map read and map write` in `TestClaudeExecutorSharedCredentialMetadataMixedAccess` and passed on rerun. That test fails under `-race` on pre-merge `63cf82582` too, because `isClaudeSetupToken` reads `auth.Metadata` without the lock (same code in upstream `dbb70621`). The race predates this merge and is left as is.
- Live E2E checks use a temporary loopback-only CPA with copied production Codex and Claude OAuth credentials. Refresh tokens were stripped and the copies deleted afterwards. Codex OAuth (Pro 100), Claude OAuth (Ultra), and a Z.ai OpenAI-compatible key pass OpenAI and Claude formats, stream and non-stream, and Responses. Per-key usage limits pass. Without a DSN, telemetry routes return 404 and file logs are written. ClickHouse capture, SQL, and restart pass.
- The SQLite-to-ClickHouse analytics migration is partially verified: 50 events were imported. The rerun was not verified, and the source had no quota history.
- Both Docker images build with Go 1.26.10 and Bun 1.4.3. In the Compose stack, the panel, `/management.html`, proxy, and management API return 200 through Nginx on loopback.
- Headless Chrome over CDP on desktop and mobile shows Max/Ultra quota labels and Ultra summary weighting. Auth Files, Routing, Config, Analytics, Events, and Usage render.
- Triage confirms `/v8/management/config/oauth/model-alias` and `/excluded-models` return 404 when unset. This is upstream behavior handled by CPAMC's `getConfigValue` default, not a regression; no change was made.

Not verified: the SQLite migration rerun, rule-based routing UI, and Pro 100 badge color through automation. Corresponding CLIProxyAPIHome repository updates for upstream's `internal/home` changes were not assessed. The machine-translated Turkish CPAMC strings have not had native-speaker review.

### 2026-10-19 - Merge upstream `main` at `2422c6b0`

Integrated upstream's plugin-host and model-registration fencing, auth exclusion and snapshot fixes, Claude thinking, tool-search, stream, and error handling, stream truncation detection, TLS and request-body safeguards, provider updates for Vertex, Interactions, Grok, Gemini, Antigravity, Meta, and Kimi, and usage metadata improvements. The merge also includes upstream ETag/If-Match configuration concurrency, session-affinity and cooldown protections, plugin quota-provider identity, and long-line plugin telemetry handling.

The merge keeps the user's decisions: upstream's OAuth gallery and Kiro, plugin, Vertex, and cancel support; upstream configuration rows with the fork's configuration blocks; and the fork's keys and quota layout with upstream data. Keepalive uses upstream's `send-keepalive-before-ft` implementation with fork-compatible behavior when the flag is unset. The orchestrator combines GSR with session affinity, keeps both Claude stream observers, accepts upstream plugin line-limit behavior, removes CPAMC `testModel`, and drops CPAMC's configuration Quota section.

Notable resolutions retain structured API-key semantics with ETag optimistic concurrency, CPAUK viewer and usage-manager behavior, and the CPAMC pin and rebuilt bundle at `0da007e`. The fork-side nginx timeout commit `bae6e16f` remains the parent of the final merge. Multiline SSE assembly, non-decreasing latency metrics, and `stream: true` enforcement for same-format Claude streams remain in the merged runtime.

Validation: `go build` and `go vet` pass. `go test ./...` passes except the pre-existing `TestV0RepeatedSavesKeepLegacyClientAlias`, which also fails on main. In the race suite, `TestClaudeExecutorSharedCredentialMetadataMixedAccess` is a pre-existing flaky race that reproduces about 1 in 200 runs on fork main `bae6e16f` and on this merge, and `TestRequestLoggingMiddleware_StreamingPreservesFullUUIDForLogger` is also pre-existing. Live smoke against the Z.ai coding plan and Kiro covers OpenAI and Claude formats in streaming and non-streaming modes. CPAMC was checked through Chrome CDP at 1440px and 390px. An analytics database migration on a copy of a real database left all 50 events readable. A known pre-existing hot-reload quota issue remains: after an openai-compatibility key hot reload, quota fetch returns HTTP 501 `no quota provider available for credential` until restart, identically on main.

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

### 2026-09-09 - Merge upstream `main` at `7fac6b15`

Integrated upstream's 132 commits since `81e1b537` with a no-fast-forward merge. The merge retains CPAUK analytics, per-key limits, management quota protection, hop diagnostics, and the CPAMC pin. Upstream's newer session hierarchy and stream metadata, Antigravity connection-pool handling, plugin-store release cache and GitHub rate-limit protection, health-probe logging, model metadata, Codex delegation compatibility, and provider protocol fixes are included.

The content conflicts were limited to `AGENTS.md`, the management handler, server reload and tests, Redis usage-queue imports, and the usage record context declarations. Fork analytics and usage-delivery fields remain alongside upstream session and stream fields; upstream's replacement plugin-release cache supersedes the earlier fork-local cache fields. Both the fork's `usagecontext` installation and upstream session normalization remain active in the Redis queue. The CPAMC gitlink includes its corresponding upstream synchronization to `ed5f1c4`, preserving the fork analytics interface and both event diagnostic corrections.

Validation: `go build -o test-output ./cmd/server && unlink test-output`, `go test ./...`, and `go vet ./...` pass. The full Go suite passes again after the observer metadata correction. Independent review reproduces bounded session metadata within the 16 KiB snapshot limit and preserved streaming context. CPAMC passes 708 tests, lint, TypeScript, production build, and isolated desktop/mobile Chrome CDP checks against the existing development server.

Follow-up correction: the bounded generic observer snapshot now accounts for
`Record.SessionID` and `Record.ParentSessionID`, bounds the corresponding
`ClientRequestMetadata` fields in the detached context, and preserves the
stream flag through observer delivery. Focused usage and usage-context
regressions cover the byte limits and stream propagation.

### 2026-08-31 - Merge upstream `main` at `81e1b537`

Merged upstream `main` into the fork without rebasing or rewriting the six published fork commits. The merge resolved upstream's server and configuration file splits by moving DL001 hooks into the new route, reload, management, middleware, and config-load files instead of restoring the pre-refactor monoliths. Direct OpenAI Realtime routes receive the limiter after authentication. DL002 files remain unchanged from `53866c01`.

The integration resolved content conflicts in `internal/api/server.go`, `internal/config/config.go`, and `internal/config/parse.go`. It kept upstream's refactored `server.go` and `config.go`, then reapplied the fork behavior in the split files listed under DL001.
