# CPA agent SQL

Use the management key supplied by the user as `Authorization: Bearer <key>` or `X-Management-Key: <key>`. Paths are relative to your CPA base URL. Never guess keys. Read [management](management.md) for access policy and config edits.

## Availability and workflow

SQL requires ClickHouse server mode, configured with `clickhouse.dsn`, and `clickhouse.agent-query.enabled`, default true. Home mode disables local management. Without a DSN, `/analytics/health` reports category `clickhouse_not_configured`; telemetry health, schema and query routes are absent and return 404. Disabled SQL returns `{"error":"agent SQL queries are disabled"}`. Storage can work while restricted-account provisioning fails, in which case SQL returns 503 with an error string. Do not assume a working proxy has telemetry history.

1. GET `/v8/management/analytics/health` for collection state and retention. GET `/v8/management/telemetry/health` for `backlog_files`, `oldest_age_seconds`, `last_error`, `paused_reason`, `last_success` and `legacy_import`. The telemetry health, schema and query routes exist only with a DSN; a 200 describes ingestion, not necessarily SQL readiness. Capture is asynchronous; backlog and imports can make results incomplete.
2. GET `/v8/management/telemetry/schema`. It returns `database`, `tables` with names, comments and column types/comments, plus named `examples`. Use this live schema for full column lists and deployed capabilities. Its examples include session 403s, monthly skill occurrences, usage by model, recent failures and cost by provider. Adapt them to the requested time bounds and deduplication rules below.
3. POST `/v8/management/telemetry/query` with bounded, parameterized SQL. Start with a narrow time range and inspect truncation before expanding.

```sh
CPA_BASE=https://cpa.example.com
curl -sS "$CPA_BASE/v8/management/analytics/health" -H "Authorization: Bearer $CPA_MANAGEMENT_KEY"
curl -sS "$CPA_BASE/v8/management/telemetry/health" -H "Authorization: Bearer $CPA_MANAGEMENT_KEY"
curl -sS "$CPA_BASE/v8/management/telemetry/schema" -H "Authorization: Bearer $CPA_MANAGEMENT_KEY"
curl -sS -X POST "$CPA_BASE/v8/management/telemetry/query" \
  -H "Authorization: Bearer $CPA_MANAGEMENT_KEY" -H 'Content-Type: application/json' \
  --data '{"sql":"SELECT request_id, started_at, path, model, status FROM requests FINAL WHERE session_id = {session:String} ORDER BY started_at DESC LIMIT 100","params":{"session":"claude:xb12345"},"max_rows":100}'
```

## Query contract and limits

Request JSON is `{"sql":"SELECT ... {name:Type} ...","params":{"name":"value"},"max_rows":100}`. `sql` is required and nonempty. `params` is optional and supports scalar, null, array and map JSON values with ClickHouse types such as `{session:String}`, `{n:UInt64}`, `{x:Nullable(String)}`, `{a:Array(String)}`. Bind values rather than interpolating untrusted input. `max_rows` must be nonnegative; omitted or zero uses the configured maximum, and larger values cannot raise it. There is no cursor; narrow bounds or use explicit keyset predicates to continue.

A successful 200 response has `query_id`, `columns:[{"name":"...","type":"..."}]`, `rows:[[...]]`, `row_count`, `truncated`, `elapsed_ms`, `rows_read`, `bytes_read`. Column order matches row positions. Integers outside JavaScript's safe integer range are strings; timestamps are RFC3339; arrays and maps remain JSON values. `truncated:true` can also mean the row count exactly reached the cap; it is not proof another matching row exists. Never present a capped result as a complete total.

| Failure | Response |
| --- | --- |
| Malformed JSON, empty SQL, negative max_rows | 400 `{"error":"invalid query request"}` |
| Invalid SQL, type/permission/limit error | 400 `{"error":"ClickHouse or validation message","query_id":"..."}` |
| Disabled, unconfigured, disconnected or provisioning failure | 503 `{"error":"..."}` without query_id |
| Authentication / policy / unavailable management | 401 / 403 / 404 as described in management docs |

Queries use a separate read-only ClickHouse account with SELECT on the configured database, never the writer account. Writes, DDL and relaxing settings are forbidden. External table functions such as `url`, `file`, `remote`, `s3`, `mysql`, `postgresql`, `executable`, `input` and `cluster` are unavailable. System table access is restricted; use the schema endpoint. Defaults are 30 seconds execution, 10,000 result rows, 64 MiB result bytes and 2 GiB memory. The account also caps reads at 100 million rows and 10 GiB. Server configuration can lower or raise configured limits; query settings cannot relax fixed constraints. Disconnecting cancels the query. These SQL execution limits are enforced by ClickHouse.

## Tables and joins

| Table | Grain and relationships |
| --- | --- |
| `requests` | One captured client request per `request_id`. Includes method/path/model/stream, final `status`, client `session_id`, masked headers and captured response. `proxy_request_id` joins analytics. |
| `attempts` | Upstream attempts keyed by `request_id`, one-based `attempt_index`. Provider, `auth_id`, label, request and response bodies/status; a client request can have many attempts. |
| `request_contexts` | Ordered transcript references per request, view and attempt. `attempt_index=0` is the client; positive values are upstream attempts. Parallel `entry_ids` and `content_ids` preserve positions. |
| `transcript_entries` | Distinct semantic occurrences keyed by `entry_id`, with role, tool names/call IDs, `content_id`, `occurred_at`, `first_attempt_index`. `session_id` is where first observed, not every session that reused it. |
| `transcript_content` | Content-addressed raw JSON and searchable `text`, keyed by `content_id`. Join entries for occurrence search. Rebuild exact request elements using context `content_ids`, not entry content IDs. |
| `analytics_events` | Versioned upstream usage attempts keyed by attempt identity. `proxy_request_id` groups retries. Includes tokens, cost, endpoint class, session display ID/digest, credential pseudonym and nullable `routing_trace_json`. |
| `analytics_outcomes` | Client completion patches. Apply only to listed `attempt_ids`, ordered by `sequence`, joined by `proxy_request_id`. Event detail/query API applies these overlays for you; raw event client status may predate them. |
| `analytics_rollups`, `analytics_request_rollups`, `analytics_retention_heads` | Retained aggregate generations and publication heads. Use the structured analytics API for totals across retention; summing all physical generations double-counts data. |
| `analytics_quota_history` | Append-only quota observations; current cooldown is runtime state, queried through credentials and dry run. |
| `cpa_jobs` | Durable one-time import jobs and migration state. Inspect `state`, `detail`, `updated_at` for incomplete imports. |
| `analytics_metadata`, `analytics_control`, `analytics_control_heads` | Internal analytics metadata and revisioned control state; use management APIs to interpret or modify it. |

## Avoid wrong answers

- Most telemetry tables use ReplacingMergeTree. Read `FINAL` before filtering or aggregating current rows. `analytics_events` needs `FINAL` plus `is_deleted=0`; tombstones and replay receipts are not usage. Count distinct `request_id` for client requests, distinct `attempt_id` for analytics attempts, and distinct `(session_id, entry_id)` through client-context membership for per-session transcript occurrences. Counting entry IDs alone can merge identical histories across sessions.
- A joined request repeats once per upstream attempt. Group or count distinct request IDs to answer how many client requests failed. Client status and upstream status differ; a retried 403 can end in a client 200.
- Use `request_contexts.attempt_index=0` for client-view membership. Do not count translated upstream contexts again. `transcript_entries.first_attempt_index` describes the first observation, not all later memberships. Repeated history and retries reuse entries. Exclude `occurred_at IS NULL` when counting known occurrences; `first_observed_at` is capture time, not execution time.
- Skill text matches are evidence of mentions, not an exact executed-skill counter. Inspect `tool_names`, role, item type and raw content to identify the client's actual invocation shape before narrowing the predicate. A mixed tool-result/user message can count again; regrouped calls can split occurrences. Entry content keeps the first observed variant. For later changed bytes inspect context content IDs.
- Session columns store canonical source-qualified IDs, not necessarily the bare ID the user quotes. Resolve the stored values first as described below. Transcript entry session IDs describe first observation; for request membership join context entry IDs instead.
- Timestamps in telemetry are UTC DateTime64; analytics `*_ns` are signed Unix nanoseconds, converted with `fromUnixTimestamp64Nano(..., 'UTC')`. Calendar periods such as "this month" follow the user's time zone: use it when known, otherwise say you assumed UTC. Convert local calendar starts with `toDateTime(<date>, <zone>)`, because comparing a UTC timestamp with a bare `Date` treats that date as UTC midnight. Use explicit inclusive start and exclusive end bounds. Do not use local server time implicitly.
- `known_cost_nano` is integer nano-USD: divide by 1,000,000,000 for USD. Null cost means unknown, not free. Keep unpriced tokens separate. Cancellation can carry `succeeded=1`; inspect `outcome`.
- Capture depends on `observability.logs.request-log`. With full capture off, failures are still forced; commercial mode suppresses upstream attempt capture. Retention defaults to 30 days for requests, attempts and contexts. Transcript GC follows surviving references, so old occurrences can disappear. Analytics has its own retention, and raw routing traces expire with raw events. TTL and GC are asynchronous. Report missing coverage rather than claiming zero use.

## Session identity

CPA's session extractor preserves opaque client IDs and qualifies them by source. These telemetry IDs are not the UUID normalization used elsewhere for affinity. Values are case-sensitive; do not strip prefixes or agent suffixes when querying.

| Stored form | Source and meaning |
| --- | --- |
| `claude:<id>` | Claude session identity from client metadata/headers. |
| `claude:<id>:agent:<agent-id>` | A Claude subagent sharing the base session ID; the main session lacks this suffix. |
| `codex:<id>` | Codex session/thread identity; a child can have its own ID. A named subagent sharing the parent ID can use `codex:<id>:agent:<name>`. |
| `header:<id>` | Generic `X-Session-ID` header. |
| `session:<id>` or `session:<id>:agent:<agent-id>` | Generic payload session ID, optionally with an agent ID. |
| `agy:`, `affinity:`, `slot:`, `task:`, `conv:`, `thread:`, `clientreq:`, `geminicache:`, `pck:`, `user:`, `execution:` followed by the ID | Other extracted sources, including Antigravity, session-affinity/slot/task headers, conversation/thread IDs, Gemini cache and prompt-cache keys. |

Synthetic/LCP identities can already be qualified, derived or unprefixed. IDs longer than 256 bytes are bounded with a prefix plus `#` and a SHA256 suffix. Older imported logs can have an empty session ID because the original log lacked session metadata. A bare-ID lookup cannot recover a hashed/bounded value or find a child whose distinct ID is unknown.

| Column | Representation |
| --- | --- |
| `requests.session_id` | Canonical source-qualified ID from the captured client request when known, otherwise empty String. |
| `request_contexts.session_id` | The same captured canonical ID for client and upstream views of that request. |
| `transcript_entries.session_id` | Canonical ID on the request where the occurrence was first observed. Shared or replayed entries do not acquire every later session's ID. Use context membership for a session's transcript. |
| `analytics_events.session_id` | Nullable, sanitized and bounded display copy of the canonical usage-record ID, including its source/agent prefix/suffix. Invalid or credential-like values may be omitted. Different extraction/capture paths and legacy rows can leave it different from request telemetry; prefer the `proxy_request_id` join. |
| `analytics_events.session_digest` | Nullable lowercase SHA256 hex of the trimmed usage-record session ID before display sanitization/bounding. Use the digest for exact distinct-session counts, not for displaying or matching a bare ID. It is not the raw ID or the credential HMAC. |

## Session xb12345 intermittently received 403

First resolve the user's bare ID to canonical values within the incident window. Stored IDs carry a namespace prefix such as `claude:` or `codex:`, and subagents append `:agent:<name>`. This literal comparison handles every CPA namespace and `:agent:` descendants without SQL wildcard interpretation or matching `xb123450` or an unrelated ID containing `xb12345`. Inspect returned values and choose the intended client/main/subagent scope. If the user supplied a complete canonical ID, query that exact value. Separate child session IDs require their own lookup.

The test-params comment declares runnable example values; send its JSON as query `params`. Set inclusive `start` and exclusive `end` to the incident window in UTC.

```sql
-- test-params: {"s":"xb12345","start":"2026-10-01 00:00:00","end":"2026-11-01 00:00:00"}
SELECT session_id, count() AS captured_requests
FROM requests FINAL
WHERE started_at >= {start:DateTime64(3, 'UTC')}
  AND started_at < {end:DateTime64(3, 'UTC')}
  AND (session_id = {s:String}
       OR arrayExists(prefix ->
            session_id = concat(prefix, {s:String})
            OR startsWith(session_id, concat(prefix, {s:String}, ':agent:')),
            ['claude:', 'codex:', 'header:', 'session:', 'agy:', 'affinity:',
             'slot:', 'task:', 'conv:', 'thread:', 'clientreq:', 'geminicache:',
             'pck:', 'user:', 'execution:', 'lcp:v1:', 'lcp:', 'derived:']))
GROUP BY session_id
ORDER BY session_id
LIMIT 100
```

Copy the selected returned IDs into `ids` below; the sample includes a main session and one subagent. Group all selected session requests by path, model, requested streaming and client status. Compare failures with successes before attributing a cause. Count each client request once even when retries create multiple attempts. `r.stream` describes requested streaming, including a streaming request rejected with a JSON 403. Rows ingested before this correction can retain the old response-derived value; inspect their envelope to verify.

```sql
-- test-params: {"ids":["claude:xb12345","claude:xb12345:agent:worker"],"start":"2026-10-01 00:00:00","end":"2026-11-01 00:00:00"}
SELECT r.path, r.model, r.stream, r.status,
       uniqExact(r.request_id) AS requests,
       groupUniqArray(a.provider) AS providers
FROM requests AS r FINAL
LEFT JOIN attempts AS a FINAL ON r.request_id = a.request_id
WHERE has({ids:Array(String)}, r.session_id)
  AND r.started_at >= {start:DateTime64(3, 'UTC')}
  AND r.started_at < {end:DateTime64(3, 'UTC')}
GROUP BY r.path, r.model, r.stream, r.status
ORDER BY r.path, r.model, r.stream, r.status
LIMIT 100
```

Then inspect individual attempts, including upstream bodies, to distinguish permission errors, model restrictions and retry recovery. Join `requests.envelope_content_id` to `transcript_content.content_id` and inspect `raw` for other client characteristics such as tools and model parameters. The envelope preserves non-transcript fields and replaces conversation arrays with reference markers. Rebuild conversation elements using ordered `request_contexts.content_ids`; if no envelope could be parsed, inspect `requests.request_body_raw`. Masked headers and captured bodies can contain private user content; report only what the investigation needs.

```sql
-- test-params: {"ids":["claude:xb12345","claude:xb12345:agent:worker"],"start":"2026-10-01 00:00:00","end":"2026-11-01 00:00:00"}
SELECT r.request_id, r.started_at, r.path, r.model, r.stream,
       r.status AS client_status, a.attempt_index, a.provider, a.auth_label,
       a.status AS upstream_status, a.response_body, c.raw AS client_envelope
FROM requests AS r FINAL
LEFT JOIN attempts AS a FINAL ON r.request_id = a.request_id
LEFT JOIN transcript_content AS c FINAL ON r.envelope_content_id = c.content_id
WHERE has({ids:Array(String)}, r.session_id)
  AND r.started_at >= {start:DateTime64(3, 'UTC')}
  AND r.started_at < {end:DateTime64(3, 'UTC')}
  AND (r.status = 403 OR a.status = 403)
ORDER BY r.started_at DESC, a.attempt_index
LIMIT 100
```

## How many times did I use AAA this month?

The broad query below counts text matches, not skill usage. Inspect matching entries, narrow to the client's actual invocation shape, and rerun before reporting a usage number. It uses the same per-session logic as the live schema's "Skill occurrences this month" example. Entry identity hashes the client key, protocol and content chain, so byte-identical opening prompts in two sessions can share one `entry_id`. Its stored session is the first observer. `uniqExact(entry_id)` alone undercounts across sessions.

Count each matching entry once per session through client contexts. The query filters text-matching entries before grouping contexts, but keeps contexts from before the month to distinguish a new occurrence from replayed history in the same session. `first_seen` is the first retained client-context membership for that session and entry. Change `zone` to the user's IANA time zone. After narrowing to confirmed invocation shapes, sum the returned `occurrences` for the selected sessions to report observed uses.

```sql
-- test-params: {"skill":"AAA","zone":"UTC"}
WITH matching_entries AS (
  SELECT e.entry_id
  FROM transcript_entries AS e FINAL
  INNER JOIN transcript_content AS c FINAL ON c.content_id = e.content_id
  WHERE e.occurred_at IS NOT NULL
    AND positionCaseInsensitive(c.text, {skill:String}) > 0
)
SELECT s.session_id, count() AS occurrences
FROM (
  SELECT rc.session_id, entry_id, min(rc.started_at) AS first_seen
  FROM request_contexts AS rc FINAL
  ARRAY JOIN rc.entry_ids AS entry_id
  WHERE rc.attempt_index = 0
    AND entry_id IN (SELECT entry_id FROM matching_entries)
  GROUP BY rc.session_id, entry_id
) AS s
WHERE s.first_seen >= toDateTime(toStartOfMonth(now({zone:String})), {zone:String})
  AND s.first_seen < now64(3)
GROUP BY s.session_id
ORDER BY s.session_id
```

This is still a text-match count. Inspect role, `tool_names`, item type and raw content, then narrow the predicate to the actual invocation shape, such as tool calls that read `AAA/SKILL.md`, when the user asks for executed skill usage. Mentions, skill definitions and generic prose are not necessarily invocations.

Sessionless requests share the empty `session_id` and can collapse together. A fork or history replay under a new session ID counts again. Entries first captured as replayed history have no `occurred_at` and count in no session, so uses before capture started or during a capture gap are missing. Retention can remove earlier contexts and shift `first_seen`; missing capture can undercount. Describe results as observed per-session occurrences when the requested month predates capture or retention.

## Why was request X routed to credential Y?

For a request-log ID use this mapping. If X is already a `proxy_request_id`, query analytics directly with that value. Request-log IDs and analytics request IDs need not match.

```sql
-- test-params: {"request":"REQUEST_LOG_ID"}
SELECT r.request_id, r.proxy_request_id, e.attempt_id,
       e.provider, e.credential_id, e.routing_trace_json
FROM requests AS r FINAL
INNER JOIN analytics_events AS e FINAL ON r.proxy_request_id = e.proxy_request_id
WHERE r.request_id = {request:String} AND e.is_deleted = 0
ORDER BY e.requested_at_ns
LIMIT 100
```

The mapping already returns `routing_trace_json` if only the trace is needed. GET `/v8/management/analytics/events/ATTEMPT_ID?start=START&end=END&time_zone=UTC` resolves trace labels and credential filename; supply RFC3339 bounds covering the attempt. SQL `routing_trace_json` is a nullable JSON String; candidate identifiers are credential pseudonyms. A null trace is missing evidence, not evidence that no rule matched. For current cooldown start with GET `/v8/management/routing/credentials` and its `cooldowns`; GET `/v8/management/credentials` adds file-credential detail, and POST `/v8/management/routing/dry-run` explains model-specific eligibility as shown in management docs. Historical SQL cannot establish current cooldown state.
