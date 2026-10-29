# ClickHouse storage and agent queries

Agent documentation is served by CPA at `/llms.txt`.

CPA uses ClickHouse when `clickhouse.dsn` is configured. Analytics and captured requests, upstream attempts and transcripts are stored in the configured database. `observability.logs.request-log` selects full request capture; forced error capture still records errors when full capture is off. With no DSN, CPA retains its original file request logs and analytics is unavailable.

Sanitized routing decisions are available in `analytics_events.routing_trace_json`
and through event detail and exports. Ordinary event lists omit traces. The
nullable String contains versioned `routing.Trace` JSON and is NULL when not
captured or for legacy-imported events. Traces follow raw-event retention.
Without ClickHouse, routing rules and dry runs still work, but trace capture is inactive.

## Setup

For Docker Compose, set a strong `CLICKHOUSE_PASSWORD` in your environment and start the application with both Compose files:

```sh
docker compose -f docker-compose.yml -f docker-compose.clickhouse.yml up -d
```

Set CPA's `clickhouse.dsn` to the native-protocol address of the Compose ClickHouse service, using the same credentials and database configured for that service. Configure `clickhouse.database` and restart CPA after changing the DSN or database. The Compose overlay keeps ClickHouse on the private Compose network and does not publish its ports.

For a separately managed server, create a CPA account with permission to create the database and tables, manage the schema, and use SQL access management to create and alter users, settings profiles and grants. CPA provisions a separate account for agent queries. If SQL access management is unavailable, storage can still operate but the agent SQL endpoints return 503. Keep the ClickHouse server private and use secure network transport where appropriate.

## Legacy import and retention

On the first migration of each component, CPA creates a one-time import job. The analytics job imports the legacy SQLite analytics database at `analytics.path`. The request-log job imports legacy request and error log files from the log directory on a best-effort basis. The jobs run in the background, resume after interruption and retry infrastructure failures on a later start. Unparseable request logs and persistently rejected request records are counted as skipped. A completed job does not run again. Source files are left untouched. Request-log checkpoints use the header timestamp and compression-independent filename; `.log` to `.log.gz` rotation does not create a new request identity. Timestamp read failures pause import rather than checkpointing past an unreadable file.

Inspect job state and details with the agent SQL endpoint:

```sql
SELECT name, state, detail, updated_at FROM cpa_jobs FINAL ORDER BY name
```

`clickhouse.telemetry-retention-days` controls retention of requests, attempts and request contexts. Garbage collection runs at startup and daily. Transcript entries and content are garbage-collected after they are no longer referenced by retained requests. Analytics retention follows the analytics configuration. Database TTL and garbage collection run asynchronously, so removal is not immediate.

## Agent SQL API

Both endpoints require the normal management key. Send it as `Authorization: Bearer <management-key>`.

- `GET /v8/management/telemetry/schema` returns the database's table and column names, ClickHouse types and comments, along with example queries.
- `POST /v8/management/telemetry/query` accepts `sql`, optional `params`, and optional `max_rows`.

For example:

```json
{"sql":"SELECT request_id, started_at, status FROM requests WHERE session_id = {session:String} ORDER BY started_at DESC", "params":{"session":"claude:example-session"}, "max_rows":100}
```

Parameters use ClickHouse's `{name:Type}` syntax. The response contains `query_id`, `columns`, `rows`, `row_count`, `truncated`, `elapsed_ms`, `rows_read` and `bytes_read`. Integers outside JavaScript's safe integer range are strings; timestamps use RFC3339. Arrays and maps are JSON values. A `truncated` result may have more matching rows; narrow the query to inspect them. Disconnecting the HTTP client cancels the query.

For monthly skill counts, use the [per-session client-context recipe](../internal/agentdocs/sql.md#how-many-times-did-i-use-aaa-this-month), also served at `/llms/sql.md`. Identical prompts in different sessions can share an entry ID, so counting distinct entries alone undercounts. The recipe deduplicates repeated history within each session, excludes upstream contexts, and uses the user's time zone. Narrow text matches to the client's actual skill invocation shape before claiming an executed-skill count.

Transcript occurrence IDs use the v2 scheme. Elements carrying sufficiently unique provider tool-call IDs anchor to the actor, protocol, role, use/result kind and sorted call IDs, independently of earlier history. Parent and depth describe their first observation. Text-only elements retain parent-chain identity, which resynchronizes after a call anchor. Short or predictable IDs such as `call_0`, `functions.Read:0` and numeric counters stay chain-scoped. The guard requires at least 20 alphanumeric suffix characters, at least eight distinct characters, letters and digits, and at least 64 bits of estimated character entropy after known prefixes. Unknown ID schemes can conservatively remain chain-scoped.

Claude string content and a single text block normalize identically. Empty semantic elements create no transcript entry: their request-context entry ID is an empty string, while the parallel content ID still preserves their exact bytes. Always use `request_contexts.content_ids` to rebuild a request. Codex replay metadata is excluded from semantic identity. Unknown protocol content remains conservatively preserved.

Call anchoring has these limits:

- The same call ID and use/result kind merge even when the payload changes. The entry keeps its first observed content; the usual entry-to-content occurrence query searches that first version. Later raw variants remain in `transcript_content` and the request's exact `content_ids` for inspection and reconstruction.
- A Claude tool-result message that also carries user text or other non-result content stays chain-scoped. This preserves a searchable entry for appended content instead of hiding it behind the earlier plain-result anchor. It represents the mixed message as a whole, so a query matching repeated result text can count that message as another occurrence. It is not a separate text-only entry or a pure executed-call counter.
- A grouped two-call message and a resend containing only one of those calls have different sorted call-ID sets and therefore different anchors. Regrouping calls can split occurrences.
- CPA's OpenAI/Gemini translator can synthesize deterministic `call_` IDs with 24 lowercase hexadecimal characters from call kind, message/part position, tool name and payload. They omit session identity and previously passed the uniqueness guard, merging identical translated calls across sessions in an upstream view. The guard now treats this exact shape as weak and uses chain identity instead. This also conservatively declines to anchor genuine provider IDs with that shape. Identical histories under the same actor/protocol can still share chain entries across sessions; the scheme does not make sessions independent namespaces. The generated-ID issue does not affect the client-view counting query above.

Entries with unknown occurrence time have a NULL `occurred_at`. The schema endpoint includes a session 403 investigation and analytics examples.

## Limits and security

Setting `clickhouse.agent-query.enabled` to `false` disables both endpoints. The `max-execution-time`, `max-result-rows`, `max-result-bytes`, and `max-memory-bytes` settings bound each query. CPA also sets limits on rows and bytes read. `max_rows` can reduce the response row limit. Queries run under a dedicated account with a password HMAC-derived from the DSN password and database name and kept in CPA memory, a read-only settings profile and SELECT access to the CPA database. External-source table functions, including `s3()`, `url()`, `file()`, `remote()` and `cluster()`, are privilege-denied. Built-in generators such as `numbers()` remain available within the enforced query limits. The account cannot change those limits. If the DSN has no password, CPA generates a random agent password in memory instead; sharing that database across multiple CPA processes is then unsupported. CPA never falls back to its write-capable account for agent SQL.

The management key grants access to stored prompts, tool inputs and outputs, request and response details, and analytics. Treat it as sensitive. ClickHouse should not be exposed to untrusted clients.

## Troubleshooting

A 503 can mean ClickHouse is not configured, agent queries are disabled, the server is unavailable, or CPA cannot provision the restricted user. Check the response and CPA logs, then verify connectivity and SQL access-management permissions. If schema or query results are empty, check the configured database and import jobs. Queries rejected for time, memory or read limits need a narrower filter or an administrator-adjusted configuration. Large result sets can be truncated; use tighter filters and ordering to inspect them in pieces.

### Failed legacy analytics import

If `GET /v8/management/analytics/health` reports `legacy_import.state = failed`, fix the reported source, quota or database problem before retrying the import. An unfinished import does not itself stop normal collection, but destructive maintenance remains guarded to protect the unfinished source.

Preserve the original `identity.key` before attempting recovery. If SQLite confirms that the source is corrupt or not a database and `identity.key` is missing, CPA creates a fresh identity key and epoch so the failed import can be inspected and abandoned. The legacy pseudonyms then cannot be joined to the new identity if the old source is repaired later. Recover the original key and repair the source before starting CPA if you need that continuity.

Busy, locked, I/O and other identity-read errors do not authorize a fresh key. Startup fails without replacing the identity and can retry after the source becomes readable. Analytics health identifies these failures with `legacy_identity_busy`, `legacy_identity_locked`, `legacy_identity_io` or `legacy_identity_read`; the wrapped SQLite error remains available for diagnosis.

If the historical import cannot be completed and you deliberately accept leaving its remaining rows unimported, use the normal management key to submit `POST /v8/management/analytics/repairs` with this JSON body:

```json
{
  "kind": "abandon_legacy_import",
  "reason": "The historical source cannot be recovered; its remaining rows will not be imported."
}
```

The nonempty reason is required and stored in the durable job journal. Poll `GET /v8/management/analytics/jobs/{job_id}` using the returned maintenance job ID until the job succeeds. Analytics health then reports `legacy_import.state = abandoned` with the recorded reason.

Abandoned is not successful completion. Remaining source rows will not be retried, including after restart, and the original source files are retained. This deliberate state allows retention, purge, restore and identity recovery to proceed through the existing storage guard. A running import cannot be abandoned; wait for its worker to exit or resolve the running operation first.

### Request capture and ingestion

Only decode failures and classified deterministic data errors can move outbox files to `telemetry-outbox/failed/` or cause legacy records to be skipped. Deterministic ingestion errors receive three attempts before that action; malformed local capture files can be rejected on their first decode attempt. Each failed attempt and permanent action logs its attempt count. Connection failures, capacity/memory/readonly errors, timeouts and unknown errors remain retryable, regardless of whether a later ping succeeds. An idle ingester checks database health every 30 seconds without updating its last successful-ingestion timestamp or querying disk capacity.

Rejected outbox files are moved to `telemetry-outbox/failed/` so later requests can continue ingesting. Inspect the warning and preserve or repair those files before manually replaying them. The log-directory budget may evict pending published outbox records, with a warning containing the number of files and bytes lost. Startup removes abandoned `.capture-*.tmp` files older than one hour. Error-log listings report stored uncompressed payload bytes; downloaded text logs add section framing, so their byte length can differ.
