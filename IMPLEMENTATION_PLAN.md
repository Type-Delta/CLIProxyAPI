# CPA control plane implementation handoff

Status: implementation handoff. Execution starts with Gate 0. Do not fan out feature work until the baseline and contract gates pass.

Date: 2026-08-31

Baseline:

- CPA local `main`: `aeea06fd`. This merge is not yet pushed to `origin/main`.
- CPA upstream base recorded in `FORK.md`: `81e1b5374f99c212f196f34956eeed964a46b8fa`.
- CPAMC fork remote: `https://github.com/Type-Delta/Cli-Proxy-API-Management-Center`.
- CPAMC fork inspected at: `Type-Delta/Cli-Proxy-API-Management-Center@d249ff0`. This is not the final implementation pin. At plan time it is two commits behind official upstream `e0ee7123`.
- CPAUK reference: `Willxup/cpa-usage-keeper@696a4659ce1d5d6f2d2d0530e3205eb51fbce889`, release `v1.15.0`.
- Earlier architecture brief: `docs/plans/cpa-control-plane-integration.html`.

This file is the implementation authority. When it differs from the earlier HTML brief, use this file.

## 1. Outcome

CPA will embed a CPAUK-derived analytics module in the existing process. It will consume CPA's normalized usage records without a loopback API or child process. The module must act like an optional sidecar from an operational point of view. If its queue, worker, migrations, database, pricing code, query code, or shutdown path fails, CPA must continue proxying requests and enforcing limits.

CPAMC will remain recognizably upstream. Existing pages and navigation should receive only the changes needed for structured API keys and per-key limits. CPAUK-equivalent screens will live in new routes under one new `Analytics` navigation group, including per-key drill-down and a key leaderboard ranked by tokens or API cost.

The management UI will keep its current ability to retrieve and manage raw API keys. Raw keys are secrets, but hiding them from an authorized administrator would remove a useful CPAMC feature. The UI will conceal them by default and reveal them only after an explicit action. Analytics storage uses SHA-256 key IDs, and authorized admin analytics responses and exports may expose them. URLs, logs, and non-admin or viewer APIs reveal neither raw keys nor key digests; viewer scope stays implicit.

## 2. Terms

- CPA means this Type-Delta CLIProxyAPI fork, including the Go proxy, Management API, and release packaging.
- CPAUK means the embedded analytics module ported from CPA Usage Keeper. Use "upstream CPAUK" for the standalone reference repository.
- CPAMC means the Type-Delta fork of Cli-Proxy-API-Management-Center that CPA ships as its web management client.
- Raw key means the configured inbound API-key secret.
- Key ID means the 64-character lowercase SHA-256 digest of the trimmed raw key.
- Short key ID means the shortest unique prefix used only for display. It starts at 12 hexadecimal characters.

The same definitions belong in each project's `AGENTS.md`. CPA has been updated as part of preparing this handoff. CPAMC receives the glossary when its fork-convention work lands.

## 3. Non-negotiable decisions

### 3.1 CPAUK is an in-process module with an independent failure state

Create a self-contained application module at `internal/cpauk/`. It remains inside CPA's root Go module and compiles into the CPA binary. Do not add a second `go.mod`, launch the upstream CPAUK executable, start a child process, or call the module over loopback HTTP.

A nested Go module would require separate tags and release ordering, and parent module archives can omit nested modules. That packaging cost does not provide process-level failure isolation. The required isolation comes from explicit interfaces, bounded queues, panic recovery, a no-op implementation, circuit breaking, route-level error handling, and a shutdown deadline.

The module owns its sanitizer adapter, storage schema, migrations, aggregation, pricing, retention, import, backup, health, and query behavior. CPA owns adapter registration and invocation, lifecycle composition, authentication, HTTP routes, configuration, and release packaging.

### 3.2 Analytics can never become a CPA readiness dependency

There is no `required` mode. CPA starts and reports healthy when analytics is disabled, initializing, unavailable, or degraded. Analytics health appears only in its capability and health APIs. Do not add it to CPA liveness or readiness failure conditions.

The following functions must continue to work during every analytics failure mode:

- Proxy authentication and provider routing.
- Streaming and WebSocket traffic.
- Request and token limit admission.
- Token-limit counter updates.
- Existing Management API routes.
- Existing CPAMC pages.
- Graceful CPA shutdown within its existing outer deadline.

### 3.3 CPAMC keeps its upstream layout

Do not redesign the dashboard shell, replace existing pages, or mix analytics widgets into unrelated pages. Add one `Analytics` navigation group and lazy-loaded routes beneath it. The existing API Keys page may gain limit controls, current consumption, key IDs, and concealed reveal controls because those functions belong to key management.

### 3.4 Raw keys remain available to authorized administrators

`GET /v0/management/api-keys` keeps returning the configured raw keys under the existing Management API authentication. CPAMC must preserve edit, copy, rotate, and delete behavior.

Raw values use password-style controls and stay concealed until the administrator selects Reveal. Revealing one row must not reveal every row. Copying a concealed key is an explicit secret action and should not require rendering the secret as normal text. The UI must clear revealed state after navigation, row mutation, logout, or loss of the authenticated session.

Analytics endpoints never return raw keys. Viewer endpoints never return raw keys. Raw keys never enter the CPAUK database.

### 3.5 CPAMC follows the CPA fork convention

The CPAMC fork will have an `upstream` remote, a current-state `FORK.md`, an append-only merge history, stable divergence IDs, a fork-aware `AGENTS.md`, and recorded validation for every surviving fork change. Published fork commits are merged forward and are not rebased during routine upstream syncs.

### 3.6 Known baseline hazards

- CPA has no `.gitmodules` file or `web/management-center` checkout yet.
- CPAMC currently types API keys as `string[]`. Its API client and config normalizer coerce structured entries with `String(...)`, which can produce `"[object Object]"` and erase limits on a later save.
- `Dockerfile.web` clones a mutable upstream branch and uses a Bun version that differs from the CPAMC manifest.
- CPA release workflows do not initialize submodules or include a pinned `management.html` in binary archives.
- The current panel updater can replace a compatible fork artifact with an unchecked upstream latest build.
- The shared usage manager ignores its buffer argument, dispatches plugins serially, has an unbounded slice, and does not wait for drain completion.
- Existing Redis usage payloads can include raw keys and client metadata. The destructive `/usage-queue` cannot feed durable analytics.
- CPA currently permits broad CORS. Viewer authentication requires a separate same-origin policy.

## 4. Target architecture

### 4.1 Gate 0: establish reproducible baselines

Complete these steps before creating feature branches:

1. Push CPA merge commit `aeea06fd` to Type-Delta `origin/main`.
2. Confirm a clean clone of that pushed SHA passes `go test ./...` and the disposable server build.
3. Add `https://github.com/Type-Delta/Cli-Proxy-API-Management-Center` as the CPAMC submodule at `web/management-center` using the current Type-Delta head. Commit the bootstrap `.gitmodules` and gitlink change so the working location is reproducible before CPAMC maintenance begins.
4. Inside `web/management-center`, add the official repository as `upstream`, merge through upstream commit `e0ee7123` or the newer agreed head, add the fork-convention files, run `bun run verify`, and push the synchronized CPAMC commit to Type-Delta.
5. Update the CPA gitlink to the pushed synchronized CPAMC commit and record both the initial and final CPAMC SHAs.
6. Record the pushed CPA fork SHA, CPA upstream SHA, pushed CPAMC fork SHA, CPAMC upstream SHA, and CPAUK reference SHA in `UPSTREAM.md` or another checked-in machine-readable manifest.
7. Confirm the implementation team has permission to push branches, tags, and releases in both Type-Delta repositories.
8. Record baseline Go tests, CPAMC verification, both image builds, and every current release-target build.

Any unpushed base, failing clean-clone build, or unknown release authority is a stop condition.

```text
request handlers and executors
            |
            v
  normalized usage.Record
            |
            +---- synchronous, trusted accounting ----> usage-limit tracker
            |
            +---- non-blocking CPAUK sanitizer tap ----> sanitized Event v1
            |                                               |
            |                                     bounded CPAUK queue
            |                                               |
            |                                     batch writer and circuit
            |                                               |
            |                                       SQLite and rollups
            |
            +---- independent bounded observer lanes ----> Redis and plugins
                                                            |
                                                            v
                                                  isolated observer workers

                                  SQLite query interfaces
                                                |
                          management and scoped-viewer query interfaces
                                                |
                                      CPAMC Analytics routes
```

The request path ends after limit accounting, a bounded sanitizer copy, and non-blocking enqueues. It performs no SQL, JSON serialization, pricing lookup, database probe, or network call. The CPAUK tap hashes the raw key and copies only Event v1 fields before asynchronous work starts. Raw keys and mutable headers never enter the CPAUK queue.

## 5. Code boundaries

Create this module tree:

```text
internal/cpauk/
    README.md
    UPSTREAM.md
    LICENSE.upstream
    service.go
    config.go
    state.go
    capabilities.go
    health.go
    errors.go

    model/
        event.go
        identity.go
        query.go
        result.go
        schema.go

    collector/
        adapter.go
        sanitizer.go
        collector.go
        writer.go
        circuit.go

    store/
        store.go
        errors.go
        sqlite/
            sqlite.go
            options.go
            writer.go
            queries.go
            integrity.go
            migrate.go
            backup.go
            migrations/

    aggregate/
        ranges.go
        rollup.go
        latency.go
        pricing.go

    maintenance/
        jobs.go
        backup.go
        restore.go
        purge.go
        repair.go

    importer/
        cpauk.go
        checkpoint.go
        sanitize.go

    testdata/
        upstream-v1.15.0/
```

Add CPA integration files outside the module:

```text
internal/api/analytics_options.go
internal/api/analytics_viewer_routes.go
internal/api/handlers/management/analytics.go
internal/api/handlers/management/analytics_events.go
internal/api/handlers/management/analytics_health.go
internal/api/handlers/management/analytics_maintenance.go
internal/api/handlers/management/analytics_pricing.go
internal/api/handlers/management/analytics_viewers.go
internal/api/handlers/management/capabilities.go
internal/config/analytics.go
```

Dependency rules:

- `internal/cpauk` must not import `internal/api`, Management API handlers, `internal/usagelimit`, `internal/managementasset`, TUI code, or CPAMC code.
- API handlers depend on CPAUK reader and maintenance interfaces. They do not depend on SQLite types.
- The collector adapter is the only CPAUK code allowed to accept a raw `usage.Record`. The usage manager invokes this small trusted tap after limit accounting. It hashes the key, copies allowlisted fields, and attempts one non-blocking enqueue before returning.
- CPAUK does not read CPA configuration files. CPA validates configuration and passes a typed `cpauk.Config` to the module.
- CPAMC talks only to versioned HTTP contracts. It does not infer Go config shapes or inspect the SQLite file.
- A dependency test or import-boundary lint must reject forbidden imports.

The module facade should expose interfaces close to the following shape:

```go
type Service interface {
    Observer() usage.Plugin
    Reader() Reader
    Maintenance() Maintenance
    Capabilities() Capabilities
    Health() Health
    Reconfigure(Config) ReconfigureResult
    Close(context.Context) error
}

type Reader interface {
    Summary(context.Context, SummaryQuery) (Summary, error)
    Timeseries(context.Context, TimeseriesQuery) (Timeseries, error)
    Dimensions(context.Context, DimensionQuery) (DimensionPage, error)
    Events(context.Context, EventQuery) (EventPage, error)
}
```

Provide disabled and unavailable implementations. Callers should receive typed `ErrDisabled` or `ErrUnavailable`, not nil services.

## 6. Failure-containment contract

The CPAUK state machine is `disabled`, `starting`, `ready`, `degraded`, `circuit_open`, and `stopping`.

| Failure | Required CPA behavior | Analytics behavior | CPAMC behavior |
| --- | --- | --- | --- |
| Invalid analytics config | Start CPA with analytics unavailable | Record validation error in health | Analytics group shows unavailable; other pages work |
| SQLite path cannot be created | Start CPA | Stay unavailable and allow retry after config change or restart | Show storage error category without a filesystem secret |
| Integrity or migration failure | Start CPA; do not mutate the source further | Keep circuit open until explicit repair or restart | Queries return `503 analytics_unavailable` |
| SQLite busy or temporary I/O error | Proxy requests never wait for it | Retry with backoff; drop events when queues fill | Show stale data and degraded metadata when reads still work |
| Disk full | Continue proxy and limit enforcement | Open circuit after the configured threshold | Show last successful write and dropped count |
| Collector or worker panic | Recover at the module boundary | Mark degraded, stop the failed worker, and attempt bounded restart | Route-level error state only |
| Query panic or malformed result | Gin recovery must contain the route | Return a stable `500 analytics_internal` envelope | Analytics route error boundary catches rendering failures |
| Queue saturation | Never block request completion | Drop newest observer event and increment counters | Health discloses loss and queue capacity |
| Shutdown flush stalls | Finish CPA shutdown by the outer deadline | Abandon remaining analytics events after five seconds | No effect |
| Analytics JavaScript chunk fails | No server effect | No effect | Existing dashboard routes remain usable |

Implementation requirements:

- Honor the buffer passed to `sdk/cliproxy/usage.NewManager`. The current implementation ignores it and grows an unbounded slice.
- Split trusted inline accounting and the CPAUK sanitizer tap from asynchronous generic observers. Token-limit accounting runs first. The tap may only hash, copy bounded scalars, and attempt a non-blocking enqueue.
- Redis usage output and external usage plugins use independent bounded asynchronous delivery lanes.
- A slow or failed observer cannot delay another observer or the CPAUK collector. Each named observer gets independent bounded delivery or equivalent isolation.
- Observer enqueue drops newest on overflow. It never blocks the request path.
- The CPAUK collector queue contains sanitized events only.
- Initial defaults are 4,096 generic observer records divided across independent lanes, 8,192 sanitized CPAUK events, batches of 256, a 250 ms flush interval, and a five-second shutdown drain. Generic asynchronous snapshots have a frozen 16 KiB maximum after bounded-field truncation, and CPAUK Event v1 has a 4 KiB maximum. Enforce a 64 MiB global generic-observer byte budget and a 32 MiB CPAUK queue budget. Observer registration that cannot fit returns a visible error rather than creating an unbounded lane.
- Expose queue bounds, depths, dropped counts, and defaults through capabilities and health.
- Recover panics at both the usage-plugin call and CPAUK worker boundaries. Never recover a panic and pretend the event was persisted.
- A CPAUK worker panic detaches that generation's intake, records a redacted panic category, and restarts after 1 second, then 5 seconds, then 30 seconds. Allow at most three restart attempts in a rolling five-minute window. A fourth panic leaves intake detached and state `circuit_open` until an explicit admin retry, a valid reconfiguration, or CPA restart. Expose restart count, window, last panic category, and last panic time. Panic-loop tests must prove no CPU spin, goroutine leak, or stale-generation enqueue.
- Use an explicit unregister function so a stopped service does not remain installed in the global usage manager.

The SQLite writer opens its circuit after five consecutive batch failures or one permanent failure. Transient retries begin after 30 seconds and back off to five minutes. Corruption, an unsupported schema, or a migration checksum mismatch remains open until an administrator repairs the database or restarts after correction. Do not silently rename, replace, or delete a corrupt database.

### 6.1 Lifecycle and reconfiguration protocol

Normal shutdown uses this order:

1. Stop accepting HTTP, streaming, WebSocket, and executor work, then wait for active publishers within CPA's outer shutdown deadline.
2. Atomically close CPAUK intake so a late publish drops and increments a counter instead of entering a stopped generation.
3. Unregister the CPAUK tap after active publisher calls have returned.
4. Close and drain generic observer lanes independently.
5. Drain the already-sanitized CPAUK queue into the writer, commit the final batch, and close SQLite within the remaining five-second CPAUK budget.
6. If the CPAUK deadline expires, record the abandoned count and continue CPA shutdown.

Disable or restart-required reconfiguration while CPA remains live uses a generation swap:

1. Build and validate a replacement disabled or ready service without exposing it.
2. Atomically detach the old intake and install the replacement tap or no-op tap.
3. Mark callbacks with a generation number so a late callback cannot enqueue into the replacement service by mistake.
4. Drain and close the old service within five seconds.
5. Publish the new health snapshot only after the swap completes. If replacement startup fails, retain the previous valid service for a rejected hot reload or install the unavailable service for initial startup.

Add race and order tests for publish during detach, unregister during callback, repeated enable and disable, repeated SDK start and stop, shutdown under load, and expired drain deadlines. Shared lifecycle and registration call sites belong to the integration owner. The CPAUK module owner implements generation-aware intake and close behavior behind the frozen interface.

## 7. Configuration and storage

Do not repurpose `usage-statistics-enabled`. It currently controls the existing destructive usage queue and may have external consumers.

Add a separate block:

```yaml
analytics:
  enabled: false
  path: ""
  queue-capacity: 8192
  batch-size: 256
  flush-interval: 250ms
  hot-retention-days: 90
  circuit-failure-threshold: 5
  max-storage-bytes: 5368709120
  min-free-bytes: 536870912
  privacy:
    store-credential-id: true
```

Rules:

- Existing installations start with analytics disabled until the administrator enables it.
- Disabling analytics stops new intake, performs a deadline-bounded drain, retains the database, and makes query routes return `analytics_disabled`. Re-enabling resumes the existing database after integrity and schema checks.
- An empty path resolves below the auth directory at `state/analytics/analytics.db`.
- Create the directory with `0700` permissions and the database with `0600` permissions where the platform supports them.
- Use a pure-Go SQLite driver. Validate the chosen version on every release target before adding it to `go.mod`.
- Configure WAL, foreign keys, `synchronous=NORMAL`, and a finite busy timeout of two seconds.
- Treat `max-storage-bytes` as a hard budget for the database, WAL, shared-memory file, and CPAUK temporary files. Set SQLite `max_page_count` from the database share of that budget. Before and between bounded batches, also require `min-free-bytes` to remain on the containing filesystem. Open the circuit with `storage_quota` before either bound is crossed so auth files and limit snapshots retain working space.
- A backup, restore, import, or migration job must prove enough temporary and reserved space before it starts. Refuse the job rather than consuming the reserve. A separately mounted quota-managed analytics volume may use different explicit values but cannot disable both limits.
- Path, driver, and queue-capacity changes require restart in the first release.
- Enabled state, retention, pricing rules, and intake may hot reload.
- Failed hot reload leaves the last valid analytics configuration active and reports `restart_required` where applicable.
- Decode the `analytics` YAML node separately from the rest of CPA configuration. YAML syntax errors remain fatal as they are today, but a wrong analytics node kind, invalid field type, unknown field, or semantic analytics error disables only analytics. Preserve the rest of the valid CPA config and expose a redacted field/category error through analytics health.
- Apply the same isolated decode on hot reload. An invalid replacement keeps the previous valid analytics service and configuration active while health reports the rejected update.
- Initial storage is local to one CPA instance. Capabilities must return `storage_scope: instance` and `shared_enforcement: false`.
- Postgres analytics storage and atomic multi-replica limit enforcement are later work. Do not imply that the first release aggregates a cluster.

Initial retention policy:

- Keep raw attempt events for 90 days.
- Keep hourly rollups for 400 days.
- Keep daily rollups until the administrator applies a shorter retention policy.
- Build and verify a rollup transaction before deleting the covered raw rows.
- Run deletion in bounded batches and expose the last completed cutoff in health.
- Version 1 has no automatic cold archive. Backup and explicit export are the archival paths.
- Deleting or rotating a configured API key does not silently erase history. Add an explicit admin-only purge-by-key-ID operation with preview, confirmation, batch ID, and backup requirement.

The CPAUK schema starts at CPA schema version 1. Do not reproduce every historical upstream CPAUK migration. Each CPA migration has a monotonically increasing version, embedded SQL, a SHA-256 checksum, a transaction, and a row in `schema_migrations`. Back up the database before an upgrade. Test upgrades from every released CPA analytics schema.

Store cost as fixed-point integer units or an exact decimal representation. Do not use binary floating-point for persisted currency. Missing prices remain unknown and must not become zero-cost usage.

## 8. Event and identity contracts

One `usage.Record` represents one upstream attempt. A single client request can produce several attempts because of retries or secondary calls. Add a collision-resistant `ProxyRequestID` to `usage.Record`, assign it once in request middleware, and pass it through every attempt.

Generate `attempt_id` as a 128-bit random identifier when the adapter sanitizes an event. If a direct SDK call has no proxy request ID, generate a synthetic 128-bit request ID for that record and mark `request_id_quality: synthetic`. Never group all missing IDs into one request.

Store timestamps in UTC. Every query range is start-inclusive and end-exclusive. Calendar weeks start Monday. Calendar ranges require an IANA time-zone name and use its daylight-saving rules. Rolling ranges use elapsed time rather than calendar boundaries. Freeze exact percentile method, currency precision, cache-token charging, retry charging, and rounding in fixtures during WP0 before aggregation code starts.

Analytics reports these separately:

- `proxy_requests`: distinct proxy request IDs.
- `upstream_attempts`: persisted event rows.
- Tokens and cost: totals for every recorded attempt, including failed attempts that report usage.

Event v1 may store only allowlisted fields:

- Schema version and attempt ID.
- Proxy request ID.
- Full key ID.
- Requested timestamp in UTC.
- Provider, executor type, model, requested alias, endpoint class, auth type, and a pseudonymous credential ID.
- Success or failure, upstream status code, and a bounded error class.
- Latency, time to first token, service tiers, and generation flag.
- Input, output, reasoning, cached, cache-read, cache-creation, and total tokens with accounting schema and quality.

Event v1 must not store raw API keys, access tokens, response headers, request headers, arbitrary failure bodies, IP addresses, forwarded addresses, user agents, or unbounded source strings.

Raw `usage.Record.AuthID` and `AuthIndex` are also forbidden. They can contain relative paths, filenames derived from email addresses, provider API keys, or other personal metadata. Generate a dedicated 256-bit analytics identity key at `state/analytics/identity.key` with `0600` permissions. Never use this identity key for authentication or encryption.

Credential identity v1 is exact: normalize provider with lowercase plus surrounding-space trim. If trimmed `AuthIndex` is nonempty, canonical identity bytes are `auth_index`, NUL, and the case-sensitive trimmed value. Otherwise, if trimmed `AuthID` is nonempty, use `auth_id`, NUL, and the case-sensitive trimmed value. If both are empty, store a null credential ID and count the event under Unknown; do not make one synthetic credential. Derive `credential_id = HMAC-SHA256(identity_key, "credential-id-v1" || NUL || normalized_provider || NUL || canonical_identity)` and record algorithm version `hmac-sha256-v1`.

Create `identity.key` only when creating a new analytics database. Store its non-secret fingerprint and identity epoch in schema metadata. If a database exists and the key is missing, unreadable, or has the wrong fingerprint, mark analytics unavailable until a verified restore. The only alternative is an explicit `start_new_identity_epoch` maintenance job that archives the old database and creates a new empty database plus key. Never silently regenerate a key beside existing data.

Backups must include the identity key through a separate protected manifest entry. Losing or rotating it starts new credential identities; it must never rewrite old rows. The admin UI may join a credential ID to a current friendly label in memory, but raw auth IDs, paths, emails, and provider keys never enter analytics storage, exports, URLs, or viewer responses.

Contract fixtures must specify type, null behavior, and byte limit for every stored field. Unless a CPAUK parity case requires a lower bound, cap provider, model, alias, endpoint class, auth type, service-tier, and error-class strings at 256 UTF-8 bytes after trimming. Credential IDs and key IDs have fixed algorithm-defined lengths. Reject invalid timestamps and negative token or latency values. Truncate only fields whose fixture explicitly permits truncation, and count every rejection or truncation in health.

### 8.1 Key IDs

Use one canonical function:

```go
func KeyID(raw string) string {
    sum := sha256.Sum256([]byte(strings.TrimSpace(raw)))
    return hex.EncodeToString(sum[:])
}
```

The full 64-character digest is the database key, join key, admin API identifier, and admin export identifier. It is not an authentication credential. Do not place it in a URL.

SHA-256 IDs are safe identifiers only when raw keys have sufficient entropy. Keep existing weak keys compatible, warn administrators when a newly entered key is short or predictable, and make generated keys high entropy. Never authorize a request from possession of a full or short key ID. Expose full and short key IDs only to Management API administrators, who can already retrieve the raw keys.

The UI uses a Git-like display algorithm:

1. Start with the first 12 hexadecimal characters.
2. Compare distinct full IDs in the current result set or configured-key list. Identical full IDs are one identity for prefix selection.
3. Lengthen only colliding prefixes, two characters at a time, until each is unique.
4. Keep the full digest in row data and provide an explicit Copy full ID action.
5. If two configured entries contain the same trimmed raw key, they share one full and short analytics identity. Show the config index to distinguish rows, but explain that usage is aggregated.
6. Prevent creation of new duplicate trimmed keys. Existing duplicates produce a warning and do not make CPA startup fail.
7. If two different raw keys ever produce the same full SHA-256 digest, mark both as `identity_conflict` and require rotation. Do not invent a second identity from secret fragments.

Key rotation creates a new key ID. Historical events remain attached to the old ID. A later audited alias feature may group IDs for presentation, but it must not rewrite history in the first release.

### 8.2 Raw-key management

- Keep `GET /v0/management/api-keys` raw and admin-only.
- Add `Cache-Control: no-store` and a restrictive `Referrer-Policy` to raw-key responses and the management page.
- Continue accepting string entries and structured entries with limits.
- Extend the limit read and reset contracts with full `key_id`. Keep a compatibility period for existing raw-key reset requests, restricted to management authentication.
- Preserve the existing raw `key` field in the v0 admin-only limit response during that compatibility period. Add `key_id`; do not silently remove a field used by the current TUI or older panels. New admin analytics DTOs are hash-only, and viewer DTOs omit both raw keys and key IDs.
- New CPAMC code joins the raw configuration entry to usage and limit data by `key_id` in memory.
- Admin analytics responses use `key_id` and optional administrator-defined labels, never the raw key.
- Viewer tokens are separate secrets stored as hashes. Do not use a key ID as a viewer token. Viewer DTOs omit full and short key IDs because a digest of a weak key would allow offline guessing. The viewer scope is implicit and may display an administrator-defined non-secret label.

## 9. HTTP contracts

Freeze JSON fixtures before backend and frontend implementation begin. All timestamps are RFC 3339 UTC. Range queries accept an IANA time zone and return the effective zone and boundaries. Lists use cursor pagination with hard range, bucket, row, and export limits.

Admin routes use the existing Management API middleware:

```text
GET    /v0/management/capabilities
GET    /v0/management/analytics/health
GET    /v0/management/analytics/summary
GET    /v0/management/analytics/timeseries
GET    /v0/management/analytics/dimensions
GET    /v0/management/analytics/events
GET    /v0/management/analytics/events/:attempt_id
GET    /v0/management/analytics/pricing
PUT    /v0/management/analytics/pricing
GET    /v0/management/analytics/providers
GET    /v0/management/analytics/quotas
GET    /v0/management/analytics/keys
GET    /v0/management/analytics/leaderboard
POST   /v0/management/analytics/query
POST   /v0/management/analytics/exports
POST   /v0/management/analytics/backups
POST   /v0/management/analytics/backups/:id/restore
POST   /v0/management/analytics/imports/cpauk
POST   /v0/management/analytics/imports/:batch_id/rollback
POST   /v0/management/analytics/purges/key
POST   /v0/management/analytics/repairs
GET    /v0/management/analytics/jobs/:job_id
DELETE /v0/management/analytics/jobs/:job_id
POST   /v0/management/analytics/viewers
DELETE /v0/management/analytics/viewers/:id
```

Scoped viewer routes live outside the management group and use separate authentication, throttling, CORS, and CSRF decisions:

```text
POST /v0/analytics/viewer/session
GET /v0/analytics/viewer/capabilities
GET /v0/analytics/viewer/summary
GET /v0/analytics/viewer/timeseries
GET /v0/analytics/viewer/events
```

Viewer middleware fixes the key ID internally from the viewer credential. It must ignore any client-supplied key filter and reject attempts to access another key. Viewer responses and URLs omit the key ID.

Viewer authentication contract:

- An administrator creates a random 256-bit viewer credential with one key ID, allowed views, expiration, and optional label.
- Store only a hash of the viewer credential. Return the raw credential once at creation.
- Shared CPAMC links put the raw credential in the URL fragment, never the path or query string. CPAMC reads the fragment and immediately removes it from the current history entry with `history.replaceState` before rendering, logging, or starting the exchange. It exchanges the in-memory value through a rate-limited `POST /v0/analytics/viewer/session` request and clears that value in a `finally` block on success or failure.
- The exchange sets an `HttpOnly`, `Secure`, `SameSite=Strict` session cookie with a short lifetime. Determine HTTPS from `Request.TLS` or from forwarded-proto headers only when the immediate peer belongs to configured trusted-proxy CIDRs. Include the bundled Nginx address range in its deployment configuration. Reject viewer-session creation when HTTPS cannot be established, except in an explicit loopback-only development mode.
- Viewer routes are same-origin by default and must not inherit CPA's wildcard CORS policy. Any future cross-origin allowlist requires a separate security review.
- All `/v0/analytics/viewer` routes bypass request-header, request-body, response-header, and error-body capture in CPA request logging. This includes the session exchange token, `Cookie`, and `Set-Cookie`. A separate security event may record route, status, latency, and a non-secret session audit ID, but never captured HTTP content.
- Revocation takes effect without CPA restart. Config reload, viewer deletion, and expiration invalidate sessions.
- CPAMC keeps the raw viewer credential only long enough to exchange or copy it. Do not write it to browser storage, logs, error reports, or analytics.

Capability state distinguishes `supported`, `enabled`, `available`, and `degraded`. It also reports API schema versions, event schema version, storage driver, storage scope, queue loss, last successful write, key-ID algorithm, structured-key support, and whether limit enforcement is shared.

GET query routes are unscoped convenience reads. They accept only non-secret filters and return `Cache-Control: private, no-store`. Any key-scoped admin request uses the JSON body of `POST /v0/management/analytics/query`; server access logs never record request bodies. Raw keys and key IDs are forbidden in paths and query strings.

The query body v1 has a required `operation` enum of `summary`, `timeseries`, `dimensions`, `events`, or `leaderboard`; required start, end, and IANA time zone; optional full `key_ids`; operation-specific allowlisted filters; and optional cursor, page size, bucket width, dimension, and leaderboard `sort_by`. Reject unknown operations, fields, or filters. Return the same typed result fixture as the matching unscoped GET plus common metadata. CPAMC must use this body route whenever one or more key filters are active.

`GET /v0/management/analytics/keys` returns the admin-only key identity catalog. It includes configured, rotated, deleted, and historical key IDs; collision-safe display IDs; optional administrator labels; current or historical status; first and last activity; token totals; priced cost; and unpriced token totals. It never returns a raw key.

Per-key analytics uses the same summary, time-series, dimension, and event contracts as overall analytics, filtered by one or more full key IDs in the POST body. Every aggregate and rollup must retain the key-ID dimension so filtering never requires scanning or guessing from masked secrets.

The leaderboard ranks key usage within the selected range. `sort_by: tokens` orders by total tokens. `sort_by: cost` orders by known API cost and reports unpriced tokens separately; unknown prices never count as zero. Results include rank, full and short key ID for administrators, optional label, request and attempt counts, token categories, known cost, unpriced tokens, and percent of the selected total. Use descending metric value, then full key ID ascending as the stable tie-breaker.

Status behavior:

- Disabled analytics returns `404` with code `analytics_disabled` from analytics query routes. Capabilities still returns `200`.
- Failed initialization returns `503` with code `analytics_unavailable`.
- A write circuit that remains readable returns `200` with `meta.degraded`, dropped counts, and `last_successful_write_at`.
- Invalid or excessive queries return `400`.
- Oversized exports return `413`.
- Throttled viewer or expensive requests return `429`.
- Unexpected route failures return `500` with code `analytics_internal` and no internal error text.
- Exported CSV cells that begin with spreadsheet formula markers are escaped. Export responses use `Cache-Control: no-store` and bounded, audited filenames.

Health and capability handlers read atomic snapshots. They must not perform a database write or wait on the writer.

Backup, restore, import, rollback, purge, and repair run as bounded maintenance jobs with frozen create, status, progress, result, cancel, and error fixtures. Mutating jobs detach CPAUK intake, drain the sanitized queue, acquire the module's exclusive maintenance lock, create and verify a backup, perform the operation, run an integrity check, reopen the writer, and attach a new intake generation. CPA keeps proxying throughout. Analytics reads return `503 analytics_maintenance` unless the operation has a safe read-only snapshot. CPA shutdown may cancel a job only at a transaction boundary and must never wait past the outer deadline.

`repairs` supports integrity check, WAL checkpoint, and reindex on a structurally readable database. Corruption or lost-identity-key recovery uses a verified backup restore or the explicit `start_new_identity_epoch` operation, which archives the old database and key material before creating an empty database and a new key. It never edits unknown corrupt pages in place or appends a new identity epoch to old rollups.

## 10. CPAMC product plan

### 10.1 Navigation and route isolation

Keep the current shell, dashboard, settings, authentication-file pages, logs, and configuration pages in place. Add one top-level `Analytics` navigation group with these child tabs:

- Overview: totals, cost, token categories, recent activity, and time series.
- Analysis: model, provider, credential, key, endpoint, latency, cache, failure, and service-tier breakdowns.
- Keys: searchable key usage, multi-key filtering, and a per-key drill-down with summary, time series, token categories, cost, models, providers, latency, failures, and events. Keep the selected full key IDs in memory and POST bodies, never in the route or query string.
- Leaderboard: rank keys by total tokens or known API cost for the selected range. Show unpriced tokens beside cost rankings so incomplete pricing is visible.
- Events: searchable attempt history, diagnostics, and bounded export.
- Pricing: effective prices, source, sync state, overrides, and missing-price warnings.
- Providers and quotas: credential health, provider availability, quota state, and reset information.
- Shared views: key-scoped viewer creation, revocation, and access links.
- Maintenance: database health, loss counters, migrations, retention, backup, and CPAUK import.

Each child route is lazy loaded behind an Analytics-specific error boundary. A failed chart, query, or JavaScript chunk must not crash the application shell or another CPAMC page. Capability discovery controls whether the group is enabled. Disabled or unavailable analytics shows a useful local state rather than redirecting or breaking navigation.

Do not place analytics cards on the existing default dashboard in the first release. That keeps upstream merges narrow and prevents analytics availability from affecting the normal landing page.

### 10.2 API Keys page

Preserve its current role and general layout. Add:

- String and structured-entry decoding.
- Per-key request cap, token cap, reset cadence, current consumption, next reset, and confirmed counter reset.
- Short key ID as the preferred row label. Show the full ID through a copy action and detail text.
- Config index when duplicate legacy keys need row-level mutation.
- Password-style raw-key display.
- Per-row Reveal, Hide, Copy raw key, Edit, Rotate, and Delete actions.
- A session-local reveal state that clears on navigation, mutation, logout, or auth loss.
- Explicit warnings that rotation creates a new analytics identity and does not move history.

Do not replace raw-key retrieval with a masked backend response. CPAMC needs the raw value for its existing administrative workflow. Do not place a raw key in the URL, page title, browser storage, analytics event, error report, or console log.

### 10.3 Compatibility rollout

Ship in this order:

1. CPAMC learns to parse both `string[]` and structured key objects without mutating unknown fields.
2. CPA exposes capability fixtures, key IDs, and structured limit contracts.
3. CPAMC enables limit editing only when the capability says writes are safe.
4. CPA adds analytics read APIs.
5. CPAMC enables the Analytics group route by route.

An old CPAMC build may keep reading raw keys. It must not flatten structured entries on write. If the backend cannot prove that the client supports structured entries, reject the unsafe mutation with a clear compatibility error.

For the existing v0 API, detect the dangerous case directly: if configured entries contain limits or unknown structured fields and a mutation would flatten or discard them, return `409 structured_api_keys_required`. Apply this guard to every Management API write that can change `api-keys`, including bulk `/api-keys` updates and `PUT /config.yaml`. Compare the current and candidate YAML nodes before typed decoding so unknown fields cannot disappear unnoticed. A new contract-version field or header may authorize an intentional clear only after WP0 freezes its fixture. Keep index-based PATCH behavior for safe row edits and add a configuration revision or `ETag` to reject stale concurrent writes. The TUI must retain its current key and limit behavior throughout this rollout.

## 11. CPAUK parity scope

Port user outcomes and calculations, not the upstream service layout or login system.

Required parity:

- Today, yesterday, rolling, calendar, and custom ranges.
- Time-zone boundary handling and daylight-saving transitions.
- Request and attempt counts.
- Input, output, cached, cache-read, cache-creation, reasoning, and total tokens.
- Model, provider, credential, key, endpoint, failure, latency, and service-tier dimensions.
- Pricing rules, overrides, sync provenance, and missing-price behavior.
- Request history and bounded exports.
- Credential and provider health.
- Quota state and reset times where providers expose them.
- Key-scoped viewers.
- Per-key analytics and multi-key filtering across summary, time series, dimensions, and events.
- A key leaderboard ranked by total tokens or known API cost, with stable ties and explicit unpriced usage. Model and credential rankings remain available under Analysis.
- Backups, migrations, retention, maintenance, and an optional transforming import.

Pin CPAUK `v1.15.0` commit `696a4659ce1d5d6f2d2d0530e3205eb51fbce889` in `internal/cpauk/UPSTREAM.md`. Record every adapted source file, formula, fixture, and intentional difference. Keep its MIT license notice. Check in small deterministic fixtures under `internal/cpauk/testdata/upstream-v1.15.0/`.

Do not add CPAUK as a required Git submodule. Its useful Go packages are under its own `internal/` tree and cannot be imported cleanly. An optional test-only submodule is allowed only if an executable parity harness consumes it in CI. A documentation-only submodule is not allowed.

## 12. Import and data migration

The CPAUK importer is optional and never runs during normal CPA startup. It runs only as an explicit authenticated maintenance job.

Requirements:

- Open the source CPAUK database read-only.
- Support dry run before any destination write.
- Hash raw source keys before insertion and discard the raw value.
- Drop personal metadata and arbitrary error text.
- Map source fields into Event v1 through the same sanitizer used by live ingestion.
- Detect overlapping source ranges and duplicate events.
- Use chunk transactions and a durable checkpoint.
- Report rows read, transformed, inserted, skipped, rejected, and reconciled.
- Resume safely after interruption.
- Roll back the current chunk on failure.
- Back up the CPA analytics database before importing.
- Never replace or copy the source database wholesale.

## 13. Fork and submodule workflow

### 13.1 Bootstrap the submodule, then update the CPAMC fork

Create the working checkout first:

1. From CPA, add `https://github.com/Type-Delta/Cli-Proxy-API-Management-Center` at `web/management-center`.
2. Commit the initial `.gitmodules` and gitlink change before CPAMC maintenance starts.
3. Work inside `web/management-center`. Keep `origin` set to `https://github.com/Type-Delta/Cli-Proxy-API-Management-Center` and add the official CPAMC repository as `upstream`.
4. Add `AGENTS.md` with repository commands, architecture, conventions, fork/upstream URLs, and the CPA, CPAUK, and CPAMC glossary.
5. Add `FORK.md` with a current-state divergence log. Use stable IDs such as `DL001`. Record implementation evidence, validation, the upstream base, and last-updated date.
6. Add an append-only merge-history section.
7. Merge upstream into published fork history. Do not rebase or force push for routine syncs.
8. Run CPAMC validation and push the synchronized commit to the Type-Delta fork.
9. Return to CPA and update the gitlink to that pushed commit.
10. Record the exact upstream comparison command and ahead/behind count after every sync. Update `FORK.md` whenever a surviving fork behavior changes.

`.gitmodules` records the submodule path and fetch URL. CPA's gitlink entry records the exact CPAMC commit. Updating CPAMC therefore ends with a parent-repository gitlink update; no branch name or floating ref determines a build.

Expected initial CPAMC divergence entries:

- Structured API-key and limit management.
- Concealed raw-key reveal plus hash-first identity display.
- Analytics navigation group, per-key analytics, leaderboard, and routes.
- CPA capability detection and versioned API client contracts.

### 13.2 Package the synchronized CPAMC submodule

Use `web/management-center` as the only required product submodule. Build the exact gitlink commit from that checkout in local development, CI, Docker, and releases.

Rules:

- Commit CPAMC changes in its repository first, then update the CPA gitlink in a separate CPA commit.
- Never leave uncommitted CPAMC changes hidden behind a gitlink update.
- CI checks `git submodule status --recursive` and rejects uninitialized or dirty submodules.
- Clean-clone instructions use `git clone --recurse-submodules` and `git submodule update --init --recursive`.
- Bare CPA `/management.html` and the Nginx web image use the same pinned CPAMC build.
- Build one canonical `management.html`, record its SHA-256 digest, and copy those exact bytes into binary archives, the main image, and the Nginx web image.
- Builds work without network access after dependencies and submodules are present.
- Preserve `disable-auto-update-panel`. A bundled artifact is immutable for that CPA release. When updates are enabled, the updater may install only a Type-Delta CPAMC release whose compatibility manifest accepts the running CPA Management API contract. It must verify the release digest before replacement.
- Remove the undigested fallback from the forked panel path. If an operator explicitly chooses an arbitrary repository or URL, mark that as an insecure override and never enable it by default.
- Use the Bun version and frozen lockfile declared by the pinned CPAMC commit. At plan time CPAMC declares Bun `1.3.14`; the current CPA Dockerfile's Bun `1.4.0` is not authoritative.
- Initialize submodules recursively in pull-request, release, and Docker workflows.
- Include the CPAMC and upstream CPAUK license notices in archives and images.
- Record the CPAMC commit in CPA release metadata and `FORK.md`.
- Write `management-artifact.json` beside the active HTML with CPAMC commit, compatible Management API range, build timestamp, and SHA-256 digest. On startup, use the mutable static-path artifact only when this manifest is valid, its digest matches, and it is compatible. Otherwise use the bundled release artifact and report the rejected override.

## 14. Work packages

Each package below has one owner. Shared-file changes go through the integration owner.

### WP0: Freeze contracts and provenance

Owner: architecture and integration owner.

Files:

- `IMPLEMENTATION_PLAN.md`
- `AGENTS.md`
- `internal/cpauk/UPSTREAM.md`
- `internal/cpauk/LICENSE.upstream`
- `internal/cpauk/model/*`
- JSON fixtures for capabilities, keys, limits, analytics queries, errors, and viewer scope.

Tasks:

- Freeze Event v1, key-ID v1, correlation IDs, query bounds, error envelopes, state values, and privacy allowlists.
- Capture CPAUK range, token, pricing, latency, per-key filtering, and leaderboard fixtures.
- Decide and record cost precision and rounding.
- Record which upstream CPAUK behavior is copied, adapted, or rejected.
- Check in `test/perf/analytics_load_test.go` and run it with `go test -run TestAnalyticsLoadProfile -count=5 ./test/perf`. Use one recorded dedicated CI runner class and record CPU, RAM, OS, Go version, and power mode with every result.
- The fixed profile uses an in-process deterministic upstream, 30 seconds of warm-up, five minutes of measurement, 64 concurrent clients, and 1,000 completed proxy requests per second. Traffic is 60 percent JSON responses, 25 percent SSE streams, 10 percent WebSockets, and 5 percent failed requests with one retry. Each run produces at least 300,000 completed client requests.
- Run the same commit with analytics disabled, healthy at default queue settings, SQLite blocked, and both queues saturated. Healthy analytics may add less than 1 ms to p99 client latency against the disabled median-of-five baseline. Blocked or saturated analytics must not add request-path waiting.
- After forced garbage collection at saturation, live heap may be at most 160 MiB above the disabled baseline. Goroutines may grow only by one worker per registered generic lane plus eight CPAUK and lifecycle workers, must not have a positive slope during the final two minutes, and must return within two goroutines of the expected disabled baseline after shutdown.

Exit:

- Backend and frontend can implement against fixtures without rereading upstream CPAUK.
- A secret scan of fixtures finds no live or raw credentials.

### WP1: Add the CPAMC submodule and synchronize the fork

Owner: integration owner. The CPAMC fork maintainer has exclusive write ownership inside the submodule and reports the pushed commit back to the integration owner.

Files and repositories:

- CPA `.gitmodules` and `web/management-center` gitlink.
- Type-Delta CPAMC checkout at `web/management-center`.
- CPAMC `AGENTS.md`, `FORK.md`, CI, and validation files.

Tasks:

- Add the Type-Delta CPAMC fork as the submodule first and commit that reproducible bootstrap location.
- Inside the submodule, add the remote convention, `AGENTS.md`, `FORK.md`, glossary, sync procedure, validation commands, and initial divergence records.
- Sync to the chosen official upstream base before feature work, then push the CPAMC commit.
- Update CPA's gitlink to the pushed synchronized commit.
- Add CI checks that match the fork's package manager and build commands.

Exit:

- A clean CPA checkout initializes the Type-Delta submodule, and a fresh maintainer can identify origin, upstream, surviving divergences, and the safe sync procedure from repository files alone.
- The final CPA gitlink resolves to the validated CPAMC commit pushed to the Type-Delta fork.

### WP2: Package the synchronized CPAMC submodule

Owner: integration owner for CPA shared files.

Files:

- `Dockerfile`
- `Dockerfile.web`
- `docker-compose-web.yml`
- `nginx-web.conf`
- release workflows
- `internal/managementasset/updater.go`
- `FORK.md`

Tasks:

- Consume the exact synchronized CPAMC gitlink produced by WP1.
- Replace remote-source UI builds with builds from the checkout.
- Make bare CPA and Nginx serve the same artifact.
- Include that artifact in binary release archives and the main CPA image, including tests for `WRITABLE_PATH` and config-relative static paths.
- Preserve offline use and enforce the compatible, digested panel-update policy from section 13.
- Document clean clone, update, release, and rollback commands.

Exit:

- Clean clones and release archives build reproducibly.
- Both deployment paths serve the pinned UI without fetching source at runtime.

### WP3: Build the isolated CPAUK module shell

Owner: CPAUK module owner.

Files: `internal/cpauk/**` except frozen fixture contracts.

Tasks:

- Implement disabled, starting, ready, degraded, circuit-open, and stopping states.
- Add disabled and unavailable service implementations.
- Add config validation, health snapshots, capability snapshots, and typed errors.
- Add import-boundary tests.
- Add panic containment and bounded close behavior before SQLite code.

Exit:

- Fault injection proves module construction, startup, panic, and close failures cannot fail CPA startup or shutdown.

### WP4: Bound usage delivery and add correlation identity

Owner: usage infrastructure owner.

Files:

- `sdk/cliproxy/usage/manager.go`
- `sdk/cliproxy/usage/manager_*_test.go`
- `internal/api/usage_limit.go`
- request-ID middleware and context helpers
- minimal registration call sites assigned by the integration owner

Tasks:

- Honor queue capacity.
- Separate synchronous trusted accounting, the non-blocking CPAUK sanitizer tap, and asynchronous generic observers.
- Isolate named observers and add unregister support.
- Define overflow counters and deadline-aware close.
- Copy mutable usage values before generic asynchronous delivery. The CPAUK tap sanitizes before its asynchronous queue.
- Add a 128-bit proxy request correlation ID carried through retries and usage records.
- Preserve token-limit update ordering.
- Support repeated SDK service start and shutdown without reusing a permanently closed singleton.

Exit:

- Slow and panicking generic observers do not delay limits, CPAUK ingestion, or proxy requests. A panicking CPAUK sanitizer is recovered and counted as a dropped event.
- Saturation has a fixed memory bound and observable loss.
- Existing plugin and Redis usage behavior remains compatible except for documented bounded-loss behavior.

### WP5: Implement sanitized ingestion, SQLite, and rollups

Owner: CPAUK module owner.

Files: `internal/cpauk/collector/**`, `store/**`, `aggregate/**`, and test fixtures.

Tasks:

- Implement the allowlist sanitizer and key hashing.
- Add non-blocking collector enqueue, batch writes, circuit breaker, and sampled state-transition logging.
- Add pure-Go SQLite, schema v1, checksummed migrations, integrity checks, pre-migration backup, retention, and restore.
- Expose store operations needed by maintenance jobs: consistent backup, verified restore into a temporary path followed by atomic replacement, integrity check, checkpoint, reindex, purge-by-key-ID, and imported-batch rollback.
- Add range, rollup, latency, token, and fixed-point pricing calculations.
- Retain full key ID in raw events and every rollup grain needed for summary, time-series, dimension, event, and cost filtering. Add indexes that make single-key and multi-key range queries bounded.
- Implement one leaderboard aggregation path for tokens and known cost. Reuse it for the API and CPAMC instead of recalculating ranks in the browser.
- Track proxy requests separately from upstream attempts.

Exit:

- Restarts, lock contention, read-only paths, disk-full simulation, corruption, interrupted migrations, retention, backup, restore, and deadline drains pass.
- Database inspection finds no raw key, header, body, IP, or user-agent material.
- CPAUK parity fixtures pass.

### WP6: Add configuration, lifecycle, capabilities, and APIs

Owner: Management API owner for new files. Integration owner handles shared constructors and route tables.

Files:

- `internal/config/analytics.go`
- new analytics handler files
- new viewer route and middleware files
- shared server and handler wiring through the integration owner
- `config.example.yaml`

Tasks:

- Add the separate `analytics` config block and reload rules.
- Wire the service through CPA lifecycle without making it required.
- Add admin capabilities, health, key catalog, per-key query, leaderboard, pricing, maintenance, export, import, and viewer-management routes.
- Add scoped viewer middleware and route bounds.
- Have the integration owner branch the global CORS middleware for `/v0/analytics/viewer` before its current early OPTIONS handling. Viewer actual requests and preflights use the frozen same-origin policy rather than wildcard CORS.
- Map typed module errors to stable HTTP responses.

Exit:

- Disabled, initializing, ready, degraded, and unavailable fixtures pass.
- All analytics database failures leave proxy and existing Management API integration tests green.
- Admin and viewer authorization matrices pass.
- Single-key and multi-key query fixtures reconcile with overall totals, and leaderboard fixtures pass for token and cost sorting.

### WP7: Complete key limits and identity contracts

Owner: API-key and limit owner.

Files:

- `internal/config/api_key_entry.go`
- `internal/usagelimit/**`
- `internal/api/handlers/management/api_key_limits.go`
- `internal/api/handlers/management/config_lists.go`
- new `internal/api/handlers/management/api_key_mutation_guard.go`
- focused tests and contract fixtures

Tasks:

- Add full key IDs to structured key and limit reads.
- Add reset-by-key-ID while keeping a documented admin-only compatibility period for raw-key resets.
- Resolve key IDs against raw configuration in memory.
- Warn on existing duplicate trimmed keys and reject new duplicates without failing startup.
- Preserve string and structured serialization behavior.
- Make unsafe old-client bulk writes fail with `409 structured_api_keys_required` rather than flatten limits. Add revision checking for new-client mutations.
- Apply the same guard to full-document Management API writes. The integration owner adds the small call site in `config_basic.go`; the API-key owner owns the shared guard and fixtures.
- Preserve current TUI behavior and extend its client only where additive fields require it.

Exit:

- Create, edit, rotate, limit, clear, inspect, and reset work through stable fixtures.
- Analytics and viewer responses contain no raw key.
- The raw Management API key endpoint still returns raw values to an authorized administrator.

### WP8: Add CPAMC compatibility and key controls

Owner: CPAMC owner. Work only in the CPAMC repository.

Primary files:

- `src/types/config.ts`
- `src/services/api/transformers.ts`
- `src/services/api/apiKeys.ts`
- `src/services/api/client.ts`
- `src/features/config/components/blocks/ApiKeysCardEditor.tsx`
- focused Bun tests and all supported locale files

Tasks:

- Add capability discovery and generated or typed API contracts.
- Parse string and structured key entries.
- Preserve unknown fields during mutation.
- Add limit editing and consumption display to the existing API Keys page.
- Add the short-ID algorithm, full-ID copy, concealed per-row reveal, raw copy, and session cleanup.
- Add route and component tests for old and new CPA fixtures.

Exit:

- The existing dashboard remains visually and functionally stable.
- Raw keys are retrievable but never initially visible.
- Old CPA versions remain readable, and unsupported writes are blocked safely.

### WP9: Add the CPAMC Analytics group

Owner: CPAMC owner. Work only in the CPAMC repository.

Primary files:

- `src/components/layout/MainLayout.tsx`
- `src/router/MainRoutes.tsx`
- new `src/features/analytics/**`
- new typed analytics API client and tests

Tasks:

- Add the Analytics group and lazy child routes listed in section 10.
- Implement Overview and Analysis first, then Keys and Leaderboard, followed by Events, Pricing, Providers and quotas, Shared views, and Maintenance.
- Make every applicable analytics view filterable by one or more keys through POST-body `key_ids`. The Keys drill-down must reuse these contracts rather than create a second calculation path.
- Let administrators switch the Leaderboard between total tokens and known API cost. Show time range, pricing completeness, unpriced tokens, stable rank ties, and links into the in-memory per-key drill-down.
- Use accessible tables, keyboard navigation, responsive charts, explicit loading states, and truthful missing-price or stale-data states.
- Add a route-level error boundary and capability gates.
- Avoid edits to unrelated pages and shared styling unless a new analytics component needs a reusable primitive.
- Update every locale supported by CPAMC, including English, Simplified Chinese, Traditional Chinese, and Russian at the inspected baseline.

Exit:

- A failed analytics endpoint or component does not affect another route.
- Overall totals reconcile with key-filtered views, and the Keys and Leaderboard pages use backend results rather than browser-side aggregation.
- Desktop and mobile CDP checks find no body overflow, unreachable controls, broken focus order, or missing error states.
- CPAUK parity journeys pass against pinned fixtures.

### WP10: Import, operations, and release hardening

Owner: CPAUK module owner for importer. Integration owner for packaging and final release.

Files:

- `internal/cpauk/importer/**`
- `internal/cpauk/maintenance/**`
- packaging and release files through the integration owner

Tasks:

- Add dry-run, checkpointed CPAUK import.
- Implement the maintenance job controller, exclusive lock, generation detach and reattach, progress records, cancellation at transaction boundaries, purge-by-key-ID, restore, repair, and imported-batch rollback.
- Add backup, restore, retention, migration, and repair runbooks.
- Add health counters and sampled structured logs.
- Use SQLite's backup API or a stopped and checkpointed database. Never copy a live database while guessing the state of its WAL files.
- Give every import a batch ID and support restoring or removing that batch through a documented recovery operation.
- Audit exports for spreadsheet formula injection and audit reverse-proxy handling for spoofed forwarded headers.
- Run load, race, upgrade, rollback, Docker-volume, offline-asset, and release-target tests.
- Fix release aggregation so matrix jobs do not overwrite a shared checksum file. Keep releases in draft form until every asset passes, then publish once from a final aggregation job.
- Produce checksums, source provenance, dependency and license notices, and SBOMs for binaries, the panel, and images. Pin container base images by digest where reproducible builds are claimed.
- Update CPA and CPAMC `FORK.md` files with surviving behavior and recorded validation.

Exit:

- The release gate in section 18 is green.

### WP11: Independent security and release QA

Owner: a reviewer who did not implement CPAUK, Management API, usage delivery, or CPAMC changes.

Files: QA reports and new regression tests only. The reviewer does not own implementation files.

Tasks:

- Audit the persisted-field inventory, API response inventory, browser storage, URLs, logs, exports, and screenshots for credentials and personal metadata.
- Verify every row in the admin and viewer authorization matrix, including CORS preflight and cross-group denial.
- Reproduce failure containment, queue bounds, proxy latency, backup restore, imported-batch rollback, binary rollback, and CPAMC gitlink rollback.
- Reproduce clean-clone builds from the pushed CPA and CPAMC SHAs.

Exit:

- The reviewer signs off on the security inventory, compatibility fixtures, and rollback rehearsal. Material findings return to the owning work package and require another independent review.

## 15. Dependency waves

```text
Wave 0: WP0 contract and fixture freeze
              |
      +-------+-------+
      |               |
Wave 1: WP1          WP3
      |               |
Wave 2: WP2       WP4 then WP5
      |               |
      +-------+-------+
              |
Wave 3: WP6 and WP7
              |
Wave 4: WP8, then WP9 route by route
              |
Wave 5: WP10, then independent WP11 QA
```

Do not start CPAMC analytics pages before their API fixtures freeze. Do not start SQLite calculations before parity fixtures freeze. After WP1's initial bootstrap commit, do not advance the CPA gitlink again until the matching CPAMC commits are pushed and their checks pass.

## 16. Shared-file ownership

One integration owner has sole write access to shared CPA files during implementation:

- `go.mod` and `go.sum`.
- `cmd/server/main.go`.
- `sdk/cliproxy/service_lifecycle.go`.
- `internal/api/server.go` and shared server option files.
- `internal/api/server_management.go`.
- `internal/api/server_middleware.go` for viewer-path CORS branching.
- `internal/api/middleware/request_logging.go` for viewer-path capture exclusion.
- `internal/api/handlers/management/handler.go`.
- `internal/api/handlers/management/config_basic.go` for the full-YAML guard call site.
- `.gitmodules`, Dockerfiles, Compose files, Nginx config, and release workflows.
- CPA `AGENTS.md`, `FORK.md`, and this handoff.
- The CPAMC gitlink.

The usage infrastructure owner has sole write access to `sdk/cliproxy/usage/**` and `internal/api/usage_limit.go`. The integration owner supplies required registration hooks but does not edit those files during WP4.

The Management API owner has sole write access to the new `analytics*.go` and `capabilities.go` handler files. The API-key and limit owner has sole write access to the existing key and limit files named in WP7. The integration owner alone edits the shared route table and handler constructor.

Other owners add new files or edit only the paths assigned in their work package. CPAMC work happens in the CPAMC repository, commits there first, and changes the CPA gitlink only through the integration owner. The independent QA owner writes regression tests and reports but does not repair implementation files.

Every handoff between owners includes commit SHA, files changed, tests run, known failures, fixture version, and any required shared-file hook. Do not pass an uncommitted shared worktree between agents.

## 17. Test plan

### 17.1 Go unit and integration tests

- Bounded usage manager, observer isolation, accounting order, overflow, unregister, and deadline close.
- Sanitizer rejection of raw keys, headers, bodies, IP data, user agents, and unbounded strings.
- Credential pseudonymization fixtures for AuthIndex preference, AuthID fallback, missing identity, case stability, email-derived filenames, relative paths, API-key-derived identities, identity-key backup, fingerprint mismatch, lost-key behavior, and explicit new-epoch recovery.
- Collector batching, loss counters, circuit transitions, panic recovery, and shutdown.
- SQLite clean create, each upgrade path, checksum mismatch, read-only path, busy lock, disk-full simulation, corruption, backup, restore, and interrupted write.
- Database, WAL, temporary-file, maximum-size, and reserved-free-space tests proving analytics opens its circuit before auth or limit persistence loses its configured reserve.
- Range and time-zone edges, daylight-saving transitions, all token categories, missing prices, overrides, retries, failures, per-key and multi-key filters, leaderboard ordering and ties, latency, and cost rounding.
- Backend reconciliation fixtures prove one-key and multi-key summary, time-series, dimension, event, token, and cost results equal the matching slice of overall data for active, rotated, deleted, and historical keys.
- Backend leaderboard fixtures cover token and cost ordering, stable tie-breaking, cursor pagination, missing prices, unpriced-token disclosure, and range changes.
- API status mapping, cursors, query limits, degraded metadata, secret absence, and capability versions.
- Raw-key CRUD, structured entries, old string entries, duplicate warnings, key rotation, key-ID reset, and compatibility reset.
- Old-panel full-YAML writes that would flatten limits or unknown structured fields return `409`; explicit revisioned new-client writes preserve or intentionally clear them.
- Isolated analytics config decoding for wrong YAML node kinds, invalid types, unknown fields, semantic errors, initial startup, and rejected hot reload.
- Admin and viewer authorization. A viewer cannot choose another key ID.
- Viewer session tests for direct TLS, HTTPS through a configured trusted proxy, spoofed `X-Forwarded-Proto` from an untrusted peer, and rejected plain-HTTP production requests.
- Viewer-link tests prove the credential disappears from the address bar and back/forward history before exchange and remains absent after both success and failure.
- Viewer actual-request and OPTIONS response-header tests proving the global wildcard CORS path cannot override viewer policy.
- Request-logging tests with normal and error-only logging enabled proving viewer exchange bodies, `Cookie`, `Set-Cookie`, and error bodies never reach request logs.
- Low-entropy raw-key fixtures proving viewer responses, URLs, and browser state reveal no full or short key ID that can validate guesses.
- Proxy availability while every CPAUK failure is injected.
- Restart, retention, import dry run, import resume, reconciliation, and rollback.
- Publish, detach, unregister, generation swap, job lock, and shutdown order under the race detector.

Run the race detector on usage delivery, collector, circuit, store, reload, viewer auth, and shutdown packages.

### 17.2 CPAMC tests

- Contract fixtures for old CPA, limits-capable CPA, analytics-disabled CPA, healthy analytics, degraded analytics, unavailable analytics, and future unknown fields.
- Raw keys concealed on initial render.
- Reveal and copy are per row. Reveal state clears at the required boundaries.
- Short IDs lengthen only when distinct full IDs share a prefix. Identical full IDs retain one short ID. Full IDs drive analytics joins, admin queries, and limit resets.
- Duplicate legacy key fixtures prove identical full digests keep one short ID, rows mutate by config index plus revision, and analytics remains aggregated under one identity.
- Per-key fixtures prove summary, time series, dimensions, events, token totals, and costs match the corresponding slice of the overall dataset for active, rotated, deleted, and historical keys.
- Leaderboard fixtures cover token and cost ordering, equal-value tie-breaking, pagination stability, missing prices, unpriced-token disclosure, and range changes.
- Existing dashboard routes and snapshots remain stable.
- Analytics route error boundaries contain component and fetch failures.
- Charts have accessible summaries and tables. Keyboard and responsive behavior pass.

### 17.3 System and release tests

- Saturate both queues with a slow and failed SQLite store. Measure bounded heap and proxy p99 overhead.
- Exercise HTTP streaming, WebSockets, retries, provider failures, and responses without token data.
- Start CPA with an invalid analytics path, corrupt database, failed migration, and unsupported schema.
- Build every current target with the pure-Go SQLite dependency: Darwin amd64 and arm64; Windows amd64 and arm64; Linux glibc amd64 and arm64; Linux no-plugin amd64 and arm64; FreeBSD amd64; and FreeBSD arm64 no-plugin.
- Build both Docker images from initialized submodules.
- Start Docker with a persistent analytics volume, restart, upgrade, restore, and roll back.
- Roll back an already deployed panel in both `WRITABLE_PATH` and config-relative static layouts, verify manifest and digest precedence, and prove a disabled updater does not reinstall the newer artifact.
- Serve the same CPAMC commit through bare `/management.html` and Nginx.
- Disconnect network access after dependencies are cached and verify the UI build and runtime.
- Validate desktop and mobile through an isolated Chrome CDP context at the globally configured endpoint.

Required repository commands after Go changes:

```bash
gofmt -w .
go test ./...
go test -race ./sdk/cliproxy/usage/... ./internal/cpauk/... ./internal/api/...
go build -o test-output ./cmd/server && unlink test-output
```

Use the CPAMC fork's own documented package-manager, lint, test, and production-build commands. Record them in its `AGENTS.md` rather than guessing in CPA.

## 18. Gates

### Contract gate

- Event v1, key ID, API fixtures, privacy allowlist, query bounds, cost precision, and CPAUK parity fixtures are committed.
- CPAMC and CPA tests consume the same fixture version.

### Failure-containment gate

- Both usage queues have enforced bounds.
- Token limits update before lossy observer delivery.
- Panics and storage failures leave proxy responses successful.
- No request goroutine performs SQL or waits for CPAUK.
- Analytics never changes CPA readiness.

### Storage gate

- Migrations, checksum rejection, backup, restore, retention, corruption handling, and deadline drain pass.
- Database and exports contain no raw keys or disallowed metadata.
- Pure-Go SQLite introduces no new CGO requirement. Every target builds with its release workflow's configured CGO mode, including every `CGO_ENABLED=0` no-plugin target.

### Identity and security gate

- Raw keys remain available only through authorized management flows and stay concealed in CPAMC until explicit reveal.
- Analytics uses full key IDs internally and collision-safe short IDs for display.
- Viewer credentials cannot access management routes or another key's data.
- URLs, logs, browser storage, exports, and error reports contain no raw keys.

### Parity gate

- Summary, ranges, tokens, costs, dimensions, latency, retries, diagnostics, quotas, viewers, per-key filtering, leaderboard results, and maintenance match the pinned CPAUK fixtures or have a documented intentional difference.
- Proxy requests and upstream attempts remain distinct.

### UI and fork-maintenance gate

- Existing CPAMC pages remain stable.
- All analytics pages live under the Analytics group.
- The Analytics group includes Keys and Leaderboard pages with single-key, multi-key, token-rank, and cost-rank fixtures.
- Both forks record upstream base, divergence evidence, validation, and merge history.
- CPA pins a clean CPAMC commit and both deployment paths serve it.

### Release gate

- Full Go, focused race, CPAMC, Docker, upgrade, rollback, and CDP test suites pass.
- Healthy analytics adds less than 1 ms p99 in-process overhead at the agreed load.
- Failed or saturated analytics does not add request-path waiting and stays within configured memory bounds.
- Operators can disable analytics and restart CPA without reverting the binary or losing proxy availability.

## 19. Rollout and rollback

Release order:

1. Release a CPAMC build that safely reads both key representations and gates new writes by capability.
2. Release CPA with bounded usage delivery, key IDs, capabilities, limits contracts, and the disabled CPAUK module shell.
3. Enable storage and core analytics APIs behind `analytics.enabled`.
4. Enable CPAMC Overview and Analysis.
5. Add the remaining parity tabs after their API and fixture gates pass.
6. Add import and maintenance operations last.

Rollback rules:

- Setting `analytics.enabled: false` stops intake and returns the module to disabled state after a bounded drain. It does not delete the database.
- A failed analytics migration leaves the pre-migration backup and original database intact.
- Roll back CPAMC by moving the CPA gitlink to the prior known-good commit. Backend capability checks keep older UI builds usable.
- For a deployed panel rollback, first disable the updater. Atomically install the prior `management.html` and matching `management-artifact.json` into the active `WRITABLE_PATH` or config-relative static directory, verify the digest, then restart CPA. If no valid mutable artifact remains, remove only the named mutable panel files through recoverable deployment tooling so startup falls back to the bundled artifact. Do not assume a source gitlink change updates an already deployed static file.
- Roll back CPA without touching the analytics database. Older binaries ignore the separate files and continue proxying.
- Never make automatic destructive schema downgrade part of binary rollback.
- Document manual restore and repair steps before the first schema-changing release.

## 20. Definition of done

The project is complete when all statements below are true:

- An administrator can create, inspect, rotate, limit, clear, reset, reveal, and copy API keys from CPAMC.
- Raw keys are concealed initially but remain retrievable through the authorized management workflow.
- CPAMC prefers collision-safe short key IDs and uses full key IDs for analytics joins, limit reads and resets, admin queries, and exports. Config row edit, rotate, and delete use config index plus revision so duplicate legacy rows remain addressable.
- CPA records durable request, attempt, token, price, latency, diagnostic, provider, credential, quota, viewer, per-key, and leaderboard data with the agreed CPAUK parity.
- Administrators can filter every applicable analytics view by one or more keys and inspect a single key without exposing its digest in the URL.
- The Leaderboard ranks keys by total tokens or known API cost, uses stable ties, and shows unpriced usage instead of treating it as zero cost.
- Every analytics screen is under the separate Analytics navigation group.
- A broken CPAUK queue, worker, database, migration, calculation, API, or UI route cannot stop CPA or the rest of CPAMC.
- CPA startup, proxy readiness, request latency, limits, streaming, WebSockets, and shutdown satisfy the failure-containment tests.
- CPA and CPAMC follow the same documented fork-sync convention.
- CPA pins CPAMC as a clean required submodule. CPAUK remains a pinned source and fixture reference, not a runtime submodule.
- `AGENTS.md` in both projects defines CPA, CPAUK, and CPAMC.
- All gates in section 18 pass and both `FORK.md` files record the shipped divergence and validation.
