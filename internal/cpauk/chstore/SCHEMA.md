# CPAUK ClickHouse schema contract

Component `analytics`. All tables use the configured database and
start with `analytics_`. Migration SQL is the authoritative column inventory,
including a COMMENT on every column and table. Money is signed Int64 nano-USD.

## File map

- `store.go`, `config.go`, `identity.go`, `budget.go`, `reconfigure.go`: lifecycle, identity, configuration and storage limits.
- `write.go`, `control.go`, `quota_history.go`: event writes, atomic control snapshots and quota observations.
- `read_selection.go`, `retained_query.go`, `query.go`, `aggregates.go`, `numbers.go`: shared SQL reads and accounting.
- `dimensions.go`, `activity.go`, `analysis.go`, `model_analysis.go`, `timing.go`, `throughput_estimate.go`, `model_leaderboard.go`, `model_cost_series.go`: analytics reports.
- `pricing.go`, `pricing_discovery.go`, `reprice.go`, `key_catalog.go`, `provider_quota.go`: pricing and account controls.
- `retention*.go`, `legacy*.go`, `imports.go`: retained publication, late rows, legacy migration and import checkpoints.
- `operations.go`, `epoch.go`, `backup.go`, `archive.go`, `restore_journal.go`: maintenance and recovery.
- `migrations/`: authoritative ClickHouse schema.

`0001_analytics.sql` creates every analytics table, including
`analytics_quota_history`; `0002_event_storage.sql` upgrades existing tables in place
without changing logical values or the control-snapshot contract. There is no intermediate
control-snapshot quota-history format or compatibility copy. The quota-history import helper preserves
source observation times and deduplicates repeated observations in its append-only
table. `MigrateWithJob` still records the pending `legacy-import:analytics` job before
initial schema creation. Databases made with earlier pre-release migrations must be
recreated, rather than upgraded through a compatibility path.

## Shared Go contract

`Config` and the public pricing, quota, retention, backup and reprice DTOs alias
`internal/cpauk/backend`. Service, handlers and the frozen SQLite import source
share these neutral types. Do not copy them into distinct named types.
`config.go` defines the ClickHouse aliases.

`Store` has `mu sync.RWMutex`, `db *clickhouse.DB`, `config Config`,
`identityKey []byte`, `identityEpoch string`, `currentSchema int`,
`retentionCutoff time.Time`. `mu` serializes local writers and maintenance.
The process owns the ClickHouse Handle. `Close(ctx)` only detaches this Store.
Only one CPA writer process may own an analytics database. ClickHouse provides
no unique constraint or compare-and-swap for cross-process writers.

Shared helpers:

- `s.table(name)` returns a quoted qualified identifier for a bare table name.
- `s.nextVersion()` returns a process-monotonic UInt64 version.
- `s.loadEvents(ctx, where string, args ...any) ([]model.Event,error)` reads
  logical current events, overlays first-wins outcomes and restores Event JSON.
  `where` is trusted SQL against event columns, empty means all. It acquires no
  mutex. Callers hold `mu.RLock` or `mu.Lock`. It excludes tombstones. Always
  apply filters on patched fields after this helper, not in its SQL predicate.
- `s.insertEvents(ctx, events, batchID, replace bool) (int64,error)` acquires no
  mutex. Validates and prices all events before sending a batch. `replace=false`
  keeps the first attempt ID, including duplicates within one batch. `true`
  writes new versions for reprice, preserving the supplied recorded price.
- `s.loadControl(ctx, namespace) (map[string]json.RawMessage,error)` and
  `s.replaceControl(ctx, namespace, values map[string]json.RawMessage) error`
  acquire no mutex. A replace stages all rows then publishes one head.
- `s.metadata(ctx,key) (string,bool,error)` and `s.setMetadata(ctx,key,value)`
  acquire no mutex and read/write current metadata.
- `s.checkQuota(ctx, additional int64) error` acquires no mutex.
- Test helpers: `openTestStore(t)`, `loadFixtureEvents(t)`, `fixturePriceBook()`.

Keep Store fields in `store.go`; shared refresh synchronization is managed by the pricing refresh state.

## Events and response outcomes

`analytics_events` is ReplacingMergeTree(version), ordered by
(requested_at_ns,attempt_id) and partitioned by the UTC YYYYMM of event time.
Partition zero is reserved for compact late-import replay receipts; those are
is_deleted=1 and never event facts. A bloom_filter(0.0001) skip index on attempt_id
supports global existence checks and detail lookups. Migration 0002 adds the same kind of
index on proxy_request_id and materializes it synchronously. It also converts repeated
dimensions to LowCardinality strings and adds codecs to event time and diagnostic JSON
mirrors; logical values stay unchanged. The writer checks incoming
IDs across all partitions with a safe attempt-ID PREWHERE and FINAL before
inserting, so conflicting replay dates still keep the original. Before applying patches,
the writer selects only eligible scalar columns and excludes attempt IDs already recorded
in outcomes. Replacement
writers, including reprice, must preserve requested_at_ns as well as attempt_id.
The time-leading key permits primary-key pruning for range queries. Detail lookup
resolves the ID through the bloom index, then reads its exact timestamp.

The table contains the full SQLite event column set, event_json String CODEC(ZSTD),
version and is_deleted. Timestamps remain signed Int64 UTC Unix nanoseconds.
The nullable String column `routing_trace_json` stores sanitized `routing.Trace`
JSON, including its version, candidates, applied rule, tier, affinity and pick.
It is NULL when not captured or for legacy-imported events. Detail and export
reads include the trace; ordinary event list projections omit it. Agent SQL can
read the column and its schema comment. Traces follow raw-event retention.
Scalar columns and JSON stay consistent. Never count physical versions; use FINAL
before filtering tombstones. Normal replay never reprices. Late rows additionally
carry retained_generation, retained_grain and retained_bucket_start_ns until their
retained snapshots publish. Compact receipts have an empty event payload and
preserve attempt/key/batch identity for replay and rollback. Their pending monthly
source rows are explicitly deleted after receipt publication.

`analytics_outcomes` is plain MergeTree ORDER BY (proxy_request_id, sequence).
Rows contain request ID, status, error, response nanoseconds and monotonic sequence.
The earliest sequence wins via argMin(tuple(status,error,responded_at_ns),sequence).
The tuple preserves empty errors. Apply an outcome only to an event whose own
proxy_status_code and responded_at_ns are both NULL, exactly as SQLite does.
Before inserting a patch, the writer finds existing eligible attempts and excludes
already patched attempts. A patch with no eligible attempts is discarded; later
attempts must not inherit it. For that reason outcomes also store `attempt_ids
Array(String)` and readers require membership. Repeated patches cannot rewrite
an attempt. New attempts can receive a later patch without changing old ones.
Invalid IDs/statuses are ignored and a zero response time becomes current UTC.
The events batch commits before the outcomes batch, matching the actual SQLite
implementation, which also uses separate transactions. Retrying after a failed
outcome insert is safe.

## Mutable control snapshots

`analytics_metadata`: ReplacingMergeTree(version), ORDER BY key. Columns key,
value, version, is_deleted. Read FINAL before filtering tombstones. Keys include
identity_fingerprint, identity_epoch, retention_time_zone, retention cutoffs and
reprice checkpoints. Fingerprint and epoch are initialized together in one batch.
An existing populated component without identity metadata is rejected.

`analytics_control`: ReplacingMergeTree(version), ORDER BY
(namespace,generation,key). Columns namespace, generation, key, value String JSON,
version, is_deleted. `analytics_control_heads`: ReplacingMergeTree(version),
ORDER BY namespace, columns namespace,generation,version. Stage all snapshot rows
in one native batch; only after Send succeeds insert a head. Readers resolve the
head once, then read exactly that generation with FINAL. An empty generation
publishes an empty collection. Interrupted staging leaves harmless unreachable
rows. After publishing the head, the mutation owner synchronously removes superseded generations while holding the writer lock. All readers hold the corresponding read lock.

Namespaces and JSON values use the existing Go DTO encoding:

- `pricing`: one `snapshot` value containing PricingSnapshot. One generation
  publishes manual rules, catalog, provenance, providers and credit conversion
  atomically. Store provider list under `providers` in the same generation.
- `provider_quota`: keys encode provider, credential and model, values are
  ProviderQuotaSnapshot. Replace-all uses a new generation.
- `key_lifecycle`: keys are full key SHA256 IDs, values preserve status and time.
- `import_checkpoints`: keys are batch IDs; values preserve source kind,
  fingerprint, offset, chunk, checkpoint digest, completion and all five counters.
- `retention`: keys are grain or operation IDs; values preserve resumable state and native bucket plans.
- `late_retention`: keys are pending generation IDs; values preserve exact bucket bounds and import ownership until replay receipts are durable.

A singleton generation avoids partially published multi-table replace operations.
JSON keeps evolving control fields without schema churn. SQL users can inspect
with JSONExtract; comments explain the namespaces. No asynchronous mutations for
control updates. All readers use helpers or the same head/FINAL discipline.

## Retained history

`analytics_rollups` and `analytics_request_rollups` preserve SQLite v1 facts,
plus version, is_deleted and generation. They are ReplacingMergeTree(version),
ordered by the complete SQLite primary key followed by generation. Always FINAL.
These are retained facts, not additive materialized views. analytics_retention_heads
publishes a generation per (grain,bucket_start_ns,import_batch_id). Both fact tables
select that same generation with c4bPublishedPredicate, including all direct
maintenance/control readers. Generation zero is visible only if that bucket/batch
has no head, preserving legacy imported facts. Superseded and unpublished snapshots
are excluded. Do not use SummingMergeTree or sum per-bucket request counters for
exact request totals.

Native retention promotes all eligible complete buckets of a grain in two staged
INSERT SELECT passes. Go plans canonical, non-overlapping source intervals using
aggregate.BucketBounds; an external table and ASOF JOIN assign sources without
per-bucket SQL or per-bucket mutations, including fractional DST transitions.
Only after all totals and exact request identities are durable do bucket heads and
then raw/hourly cutoff metadata advance. Queries select raw rows at/after the raw
floor, hourly facts between floors, and daily facts before the hourly floor.
Sources are physically cleaned once per run, after publication of both grains.
A set-based outcome mutation removes just the expired attempt references before
source deletion; empty outcome rows are then removed. Whole UTC event months below
the raw cutoff use DROP PARTITION; only the boundary month needs a bounded DELETE.
Receipt partition zero is preserved. Hourly sources use one deletion per retained
fact table. A crash leaves a complete old or new snapshot selected; retries finish
cleanup without adding a previously published source again.

Late-write plans persist the exact bucket bounds in `late_retention` control snapshots before source insertion. Recovery reuses those bounds; merges reuse already published intervals, including fractional DST overlaps. Native plans persist their complete source intervals in the retention journal.

A late imported or live row below the raw floor is published directly into the
appropriate hourly/daily bucket through the same generation protocol. Only its
own import-batch snapshot is merged; empty batch IDs identify native history.
Durable pending event markers and compact receipts make recovery/replay safe. If no
durable late plans exist, recovery prunes the scan below the published raw cutoff and
excludes receipt partition zero; unsupported old pending rows without plans still produce
their explicit recovery error. Rollback and purge remove all matching physical generations and receipts, while
preview counts and backups select only published snapshots. Reprice stays limited
to surviving raw rows and retains SQLite's partial-history reporting. Exact
requests combine DISTINCT IDs across raw and retained request identities.

SQLite 15-minute compact materializations, daily_stats, durable historical generation
counters and their indexes are dropped. ClickHouse scans the columnar facts directly
on cache misses. Both backends share the process-local historical result LRU in
`backend/history_cache.go`, bounded to 128 entries and 32 MiB of encoded response
and key bytes, with defensive JSON cloning at ownership boundaries. There are no
additional ClickHouse cache tables.

ClickHouse caches complete Summary, Timeseries, Activity and Analysis results only
when the selected end is at or before today's calendar boundary in the query's
configured IANA analytics zone. Today, future ranges, incomplete Analysis sections,
active legacy imports and known pending late publications bypass the cache. Activity
splits a range spanning today at that calendar boundary, reusing its closed-day
prefix while recalculating today. Calendar bucketing preserves DST transitions and
fractional offsets. Retained-range and year-activity validation still run on misses;
a mutation invalidates any previously validated result before readers can reuse it.

ClickHouse keys include the normalized selection plus window and named range,
identity epoch, and a process-local range mutation generation. A bounded journal
coalesces writes in the same UTC 15-minute interval and records at most 256 ranges;
overflow clears the LRU and starts a new generation. Native intake, imports,
repricing and response patches advance generations for their affected event times,
so unrelated tail writes preserve closed history. Pricing publication, catalog
binding changes, retention, purge, rollback, restore, identity changes, legacy
publication and recovered late generations clear the cache. Metadata publication
also clears it, covering time-zone and retention-boundary changes. Time-zone
reconfiguration requires restart, which starts with an empty cache.

All cache generation checks and mutation invalidation hold the Store lock. A miss
captures the revision before computing and publishes only if that revision remains
unchanged. Multi-statement mutations invalidate on errors too. Analytics event,
outcome, control, metadata and retention writes disable asynchronous INSERT and
await submitted writes without caller cancellation, then invalidate before releasing
the writer lock. Restore staging follows the same visibility rule. Synchronous
maintenance mutations retain their existing `mutations_sync=2` contract, so a cache
entry cannot survive a write that becomes visible after invalidation. No raw-event TTL, because deletion must follow durable
rollup publication. Purge and import rollback filter raw and both retained tables
and checkpoints; maintenance uses synchronous mutations or replacement tombstones and
must make read visibility consistent before returning.

## Identity, time and operations

The identity key remains 32 raw bytes, `identity.key` beside analytics.path unless
IdentityKeyPath overrides it, directory 0700 and file 0600 on creation. Readable
legacy identity metadata requires its original key; a missing key is an error,
never generate replacement pseudonyms for that identity. On initial ClickHouse
creation, legacy fingerprint/epoch metadata is read without changing the source.
Only confirmed SQLite corruption or a not-a-database result permits startup to
log why legacy metadata was not adopted and use a local key, creating one if
absent. Other identity-read failures reject startup without replacing identity.
The background import records an unreadable-source failure for confirmed corrupt
sources, leaving its deliberate abandonment operation available.
Legacy import reads matching metadata and preserves the
epoch. Normal startup verifies fingerprint before intake. New identity epochs
require detached intake and explicit maintenance, and affect analytics only.

All instants are UTC Int64 Unix ns. Buckets use the configured IANA storage zone
and Go aggregate.BucketBounds, retaining actual bucket end for DST and fractional
offset zones. Stored timezone cannot change while retained rows exist. Reads
that cut retained buckets or request an incompatible timezone keep the existing
partial-range error behavior.

Budget uses active analytics_* parts bytes in system.parts and the minimum free
space of system.disks. The result is cached briefly and estimated inserted bytes
are reserved locally between refreshes. MaxStorageBytes is a soft preflight limit,
not a physical server quota. MinFreeBytes reserves server disk, not local disk.
Control snapshots and quota-history appends also reserve their estimated write bytes. Retention journals remain writable over budget so cleanup can free space. Money totals aggregate as Int128 and use checked conversion to Int64; overflow returns an error before publication or source deletion. Errors retain the SQLite classified sentinels. Maintenance that frees space must
still run when writes are over budget.

The store owns export/restore with the existing manifest contract, translated to a
portable analytics-only archive. A ClickHouse BACKUP of the whole shared database
would wrongly include telemetry. Integrity checks use CHECK TABLE on analytics
facts; checkpoint/reindex must report meaningful CH behavior. Source SQLite
backup/restore procedures cannot be reused as ClickHouse storage operations.

Open uses MigrateWithJob for analytics, recording `legacy-import:analytics`
before the first statement of a from-zero migration. Existing components never
create this job. The shared Handle and unrelated components survive
Store.Close. All migrations and table/column comments are tested on real CH. Restore
accepts archives no newer than the live schema and compares logical table schemas with
LowCardinality wrappers ignored, so a schema-1 archive restores into schema 2. Reindex
issues ordinary `OPTIMIZE TABLE` statements; it does not force `FINAL`, which readers
already use for logical correctness.

## Implementation notes for dependent slices

`WriteImportBatch` is in `write.go`. `imports.go` implements checkpoint methods and the upstream importer extensions.
Test helper `openTestStore(t, optionalConfig...)` accepts a complete Config
override; `insertFixtureEvents(t,s)` writes and returns the v1.15 fixture events.
Test helpers accept testing.TB so benchmarks can reuse them.

Purge/rollback must remove outcome references for deleted attempts as well as
raw and retained facts. Otherwise re-importing the same attempt ID could inherit
a patch from its previous incarnation. Do this before making that ID writable
again, under the Store mutation lock. A purge can preserve other attempts in the
same outcome row by rewriting its attempt_ids, or synchronously delete the old
outcome row after recording surviving patches in replacement rows.

The monthly/time-leading event layout is intentional: time pruning and whole-month
retention avoid rewriting every raw part for each hour. Global identity is a writer
invariant backed by bloom-indexed existence checks, not a ClickHouse unique key.
There is still exactly one mutation-owning CPA process per analytics database.
Outcome rows stay ordered by proxy request/sequence because one patch can reference
attempts on both sides of the retention boundary; response time alone cannot safely
classify them for month dropping. Event JSON mirrors scalars to preserve lossless
Go detail reads. Imported raw rows must go through the event writer so both forms
and historical prices stay consistent.

Startup restores `pricing/snapshot` into config.PriceBook before intake. Writers must
persist the full effective Rules slice in that PricingSnapshot, not just overrides.
Core also restores retention metadata keys `retention_raw_cutoff_ns` and
`retention_raw_checkpoint_ns` as signed integer nanosecond text. The checkpoint
is inclusive and advances the raw floor by one nanosecond. Retention uses these
keys, and `retention_hourly_cutoff_ns` for the hourly publication boundary.

Destructive maintenance refuses pending, running and failed `legacy-import:analytics` jobs. Resumed imports preserve recorded prices and use the deduplicating late-row publication path. Legacy pricing seeds only an untouched namespace, preserving acknowledged online changes.

## Recovery and deliberate import abandonment

The control-head INSERT is the commit point for mutable snapshots. After it
succeeds, pricing updates the event writer's in-memory PriceBook even if the
caller's context is canceled before reclaiming obsolete generations. Reclamation
failures are logged separately and do not turn a committed update into an error.
Refreshing persisted key-lifecycle identities, including startup state and status
transitions, bypasses budget preflight. Newly introduced lifecycle identities are
charged and reserved as growth, so retaining old IDs cannot grow the collection
without quota checks. An already over-budget database can still refresh existing
state. Checkpoint removal during rollback also bypasses budget preflight. Rollback
invalidates its completion checkpoint before deleting facts, so an interrupted
cleanup cannot advertise a completed missing import.

A checked money overflow in late staging rolls back only the unacknowledged,
unpublished generation, including its pending source rows and plan. Published
baseline facts remain intact. This rejected write returns an overflow error;
subsequent intake and retention can proceed. Transient database errors retain
the normal durable pending-generation recovery path. Under the mutation lock,
recovery also removes journal entries with no pending source generation. This
reconciles interrupted rejection or receipt cleanup without touching published
history or discarding plans for sources that still need recovery.

`AbandonLegacyImport(ctx, reason)` is a deliberate escape for a pending or failed
legacy analytics import. It persists job state `abandoned`, a reason and an UTC
abandonment timestamp. It refuses a running worker and does not relabel a completed
import. Abandoned imports never resume automatically and lift the destructive
maintenance guard without claiming that import completed successfully.

Pending late rows without valid persisted bucket plans belong to an unsupported
pre-release database. The error explicitly states that pre-release databases must
be recreated; no compatibility recovery is attempted.

## Round 4: legacy identity read failures

Only SQLite primary result classes `SQLITE_CORRUPT` and `SQLITE_NOTADB`, including
their extended codes, allow the unadopted/fresh-identity initialization path.
Modernc reports the truncated-header fixture as `SQLITE_CORRUPT`. Busy, locked,
I/O, missing metadata and other read errors return from Open before creating a
key or recording a fingerprint/epoch. Context cancellation is propagated directly.
Non-corruption read failures retain their wrapped SQLite error and implement the
existing retryable startup classification interface. Safe health categories are
`legacy_identity_busy`, `legacy_identity_locked`, `legacy_identity_io` and
`legacy_identity_read`; they do not expose filesystem paths. Readable sources still
require their original identity key and adopt their recorded epoch. The exact
whole-corrupt source continues through Open, failed import and explicit abandon.
