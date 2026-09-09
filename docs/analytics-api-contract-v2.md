# Analytics Management API v2

This document freezes the additive contract used by CPA and CPAMC analytics.
CPA advertises support by including `2` in `api_schema_versions`. Version 1
requests and fields retain their existing meaning. Full API-key and credential
identities remain POST-body-only and never appear in URLs; CPAMC may persist a
collision-safe short key reference in its client-side hash URL.

## Query ranges

Query bodies may keep the version 1 `start`, `end`, and `time_zone` fields or
use `range`. Named ranges require top-level `schema_version: 2`:

```json
{
  "preset": "today|yesterday|last_n_hours|last_n_days|this_week|prev_week|this_month|prev_month|this_year|prev_year|custom",
  "n": 7,
  "start": "2026-08-01T00:00:00Z",
  "end": "2026-08-08T00:00:00Z",
  "time_zone": "Asia/Kolkata"
}
```

`n` is required only for rolling presets. `start` and `end` are required only
for `custom`; calendar presets reject `n`, `start`, and `end`. Every response
echoes the resolved UTC bounds and IANA zone in `meta.range`. `today`,
`this_week`, `this_month`, and `this_year` end at request time. `yesterday`,
`prev_week`, `prev_month`, and `prev_year` are complete previous local
calendar periods. Calendar boundaries use the requested IANA time zone.

Retention uses the configured `analytics.storage-time-zone`. If a range contains
retained rows and requests another time zone, the API returns
`analytics_invalid_query` with a reason instead of rebucketing an indivisible
stored row.

## Summary

`POST /v0/management/analytics/query` with `operation: "summary"` adds:

- `succeeded`, `failed`, and nullable `success_rate` from upstream attempts
- `requests_per_minute`, `tokens_per_minute`, and nullable
  `cache_read_rate`, where cache rate is cache-read tokens divided by input
  tokens
- fractional `range_days`
- `avg_requests_per_day`, `avg_tokens_per_day`, and
  `avg_known_cost_usd_per_day`
- `price_coverage_complete`

Decimal rates and costs use JSON strings so clients do not lose precision.

## Activity

`operation: "activity"` accepts the common range and `key_ids`, plus `window`
of `day`, `week`, `month`, or `year`. The response contains `grain`, `zone`,
and ordered, gap-free `buckets`. A `year` window uses exactly 365 local-day
(`1d`) buckets. It reads hot days from `events` and retained days from the
per-day, per-key `daily_stats` snapshot compiled by retention; it never reads
rollups directly. `key_ids` applies to both sources. Other filters cannot be
applied to retained snapshots and are rejected when the selected range
contains them. Each bucket contains `start`, `end`, request success/failure
counts, known cost, and the six token categories plus `total_tokens`.
Retained snapshots preserve all listed counts and token fields, but not known
cost, so their contribution to `known_cost_usd` is zero.

## Analysis

`operation: "analysis"` returns independently nullable sections. Each section
has its own `meta.partial` marker so one failed or unsupported calculation does
not hide the others:

- `series_by_category`: the six token categories, requests, and known cost by
  time bucket
- `model_by_time`: top models and per-model totals by bucket
- `latency`: capped samples with timestamp, TTFT, latency, model, and result;
  full-range p95 and maximum values; full sample count and sampling marker; or an
  `unsupported_reason` for ranges over 30 days
- `cost_components`: uncached input, cache read, cache creation, output, and a
  blended cost per million tokens
- `key_model_matrix`: key and model axes with request, token, cost, and token
  category values per cell

## Keys

`GET /v0/management/analytics/keys` fills the configured key label and adds
range-local `first_activity_at` and `last_activity_at`, lifetime
`lifetime_first_activity_at` and `lifetime_last_activity_at`, and
`unpriced_tokens`.

## Events and exports

Events accept `result: "success"|"failure"`, `error_class`, and `source`
filters in addition to existing filters. CSV and JSON exports accept the same
filter body without a cursor and emit the same set of stored, sanitized
event fields.
They never export raw keys, request bodies, headers, IP addresses, or other
forbidden event fields. Event pages include filter-wide `total_count`; cursors
remain bound to the exact resolved range and filter selection.

CSV and JSON exports preserve all recorded event fields, including routing, strict
first-token and provider latency, request/response timestamps, statuses, sanitized
raw generation diagnostics, and recorded error bodies. Existing flat token columns
remain compatible; the canonical token breakdown is also retained. JSON preserves
nulls and structured raw values; CSV uses empty cells for missing values and JSON
inside quoted cells for structured values. Export does not add another raw-payload
truncation limit. Already truncated historical data cannot be recovered.

The authenticated single-event detail response additionally includes nullable
`credential_filename`, resolved against currently loaded credentials. It contains
only the backing filename, without a host directory. Missing or deleted credentials
return null. Stored events, exports, and shared viewer responses retain hashed IDs.

### Hop diagnostics (schema v1, additive)

Every event carries nullable per-hop fields describing
Client -> CPA -> Provider -> CPA -> Client. Events recorded before these fields
existed return `null` for all of them; clients must treat that as "not
recorded", never as zero.

| Field | Meaning |
| --- | --- |
| `client_method`, `client_path` | Downstream request line. The path never includes a query string. |
| `received_at` | When CPA accepted the downstream request (RFC 3339 UTC). |
| `upstream_method`, `upstream_url` | Provider request line. The URL never includes a query string or user info. |
| `upstream_sent_at` | Current provider dispatch, using the same origin as local latency measurements. It updates on retries. |
| `routing_time_ms` | CPA receipt to the request's first provider dispatch, measured once and shared across retries. Null for historical records without that observation. |
| `first_token_latency_ms` | Local monotonic duration from dispatch to the first substantive output observed. A populated Codex terminal output can establish this endpoint without establishing generation duration. Null when output was not observed before timing became unreliable; never falls back to a heartbeat or first packet. |
| `provider_latency_ms` | Local monotonic duration from dispatch to HTTP response headers or the first application response frame for that WebSocket request. Includes network and provider waiting time, not a provider-reported acceptance timestamp. |
| `upstream_status_code` | Provider HTTP status. Now recorded for successful attempts as well as failures. |
| `upstream_usage_raw` | Sanitized provider generation telemetry or usage node, at most 40 KiB. Per-message attribution, request echoes, and recognized all-zero tool usage are omitted. Oversized JSON preserves complete fields with aggregate usage prioritized and `_truncated: true`; legacy non-JSON text remains a JSON string. |
| `upstream_error_body` | Provider error body for failed attempts, at most 4 KiB, suffixed `...[truncated]` when cut. |
| `proxy_status_code`, `proxy_error`, `responded_at` | What CPA returned to the client. Applied to every attempt of the proxy request once the handler finishes; the first completion wins. |

Requests refused before any provider request because no credential was
available are recorded with `executor_type: "auth-selection"`,
`error_class: "auth_unavailable"`, null provider fields, and CPA's client-facing
error in `proxy_error`.

`endpoint_class` uses the proxy middleware's bounded class set verbatim
(`chat_completions`, `responses`, `messages`, `embeddings`, `images`, `audio`,
`videos`, `moderations`, `realtime`, `live`, `search`, `gemini_generate`,
`gemini_stream_generate`, `models`, `other`) and falls back to `unknown` only
for records that carry none of them.

## Pricing and repricing

Pricing GET returns per-rule `source` and `updated_at`, plus `missing` entries
with model, provider, first-seen time, request count, and unpriced tokens.
Pricing PUT validates `currency_unit` and `rounding` when supplied.

`POST /v0/management/analytics/pricing/reprice` accepts a range and `dry_run`.
It starts a resumable maintenance job that recalculates every surviving raw
event in the selected range. The terminal result includes `effective_start`,
`retained_cutoff`, and `history_complete`. Retained rollups keep their stored
prices because they do not contain the requested alias and per-event rounding
inputs needed for exact repricing. Status and cancellation use the existing
job endpoints.

## Providers and quotas

Provider results add credential rows with `credential_id`, `provider`,
`auth_type`, `status`, request/failure counts, last error class/time, optional
quota `limit`, `used`, `remaining`, and `resets_at`, plus `observed_at`.
Credential IDs are privacy-preserving hashes, never raw auth identifiers.

## Response wrappers

Several management endpoints wrap their payload rather than returning the store
DTO directly:

- `GET /v0/management/analytics/keys` returns `{"meta": ResponseMeta, "keys":
  AnalyticsKey[]}`. `keys` may be `null` when the range holds no keys.
- `GET /v0/management/analytics/providers` returns `{"providers": [...],
  "storage_scope": "instance", "durable": true}`. `durable` is present and
  `true` only when the rows come from durable analytics storage; it is absent
  when they are derived from the in-memory auth manager.
- `GET /v0/management/analytics/quotas` returns `{"quotas": [...],
  "shared_enforcement": bool, "durable": true}` with the same `durable`
  semantics.

## Pricing state

The pricing GET envelope carries `sync_state` (the remote price-book sync
state) and a nullable `updated_at` (the durable manual snapshot's last update)
beside `currency_unit`, `rounding`, `rules`, and `missing`. The additive
`catalog` and `overrides` arrays expose the discovered models.dev inputs and
management rules separately; `rules` is the effective display set after
manual model and alias overrides shadow discovered rows. A catalog rule also
includes an exact `match.provider`.

`catalog_source`, `catalog_updated_at`, and `catalog_expires_at` identify the
last-good remote catalog. CPA refreshes models.dev lazily on demand with a
six-hour TTL. A management pricing PUT replaces only the manual override set,
so a refresh never deletes operator entries. A failed refresh retains the
last-good catalog and suppresses another attempt briefly; it does not erase
known prices. `sync_state` is `not_configured`, `refreshing`, `ready`,
`stale`, or `unavailable`. A failed first download reports `unavailable`;
a failed refresh with a saved catalog reports `stale`. Catalog rates are baseline input/output and cache estimates from
models.dev; long-context, service-tier, and modality-specific billing tiers
are not represented by the current per-million-token rule format.

## Capabilities and health

`GET /v0/management/analytics/capabilities` reports `api_schema_versions`,
`event_schema_version`, `supported`, `enabled`, `available`, `degraded`,
`state`, `storage_driver`, `storage_scope`, `key_id_algorithm`,
`structured_keys`, `shared_enforcement`, `management_query_v1`, `viewer_v1`,
`queue`, and `last_successful_write_at`.

`GET /v0/management/analytics/health` reports `state`, optional `category`,
`field`, and `message`, the `queue` snapshot, `last_successful_write_at`,
`last_panic_category`, `last_panic_at`, `restart_count`,
`restart_window_seconds`, `rejected_events`, `truncated_fields`,
`abandoned_events`, an optional `retention_cutoff`, and an optional
`zone_mismatch` object:

```json
{"zone_mismatch": {"stored": "UTC", "configured": "Asia/Kolkata"}}
```

`zone_mismatch` appears when the analytics store refuses to open because its
persisted `retention_time_zone` metadata disagrees with
`analytics.storage-time-zone`. `category` is then `storage_time_zone` and
`field` is `storage-time-zone`.

## Viewer expiry

`GET /v0/analytics/viewer/capabilities` returns two distinct expiries:

- `view_expires_at` — when the shared link itself stops working, chosen by the
  creator (up to 90 days).
- `session_expires_at` — when this browser session lapses (30 minutes, or the
  view expiry when that is sooner). Reopening the link starts a new session.
- `expires_at` — a compatibility alias of `session_expires_at`.

`ViewerEventPage` intentionally omits `total_count`; only the management event
page carries it.

### Cross-origin session exchange

`POST /v0/analytics/viewer/session` and the rest of `/v0/analytics/viewer/*`
are same-origin only unless the request's `Origin` matches an entry in
`analytics.viewer.allowed-origins` (see `docs/analytics-operations.md`). A
same-origin exchange sets the session cookie with `SameSite=Strict`; an
allowed cross-origin exchange sets `SameSite=None; Secure` instead, so a
cross-origin client must call with `credentials: 'include'` and must be served
over HTTPS (loopback HTTP is exempt when `allow-loopback-http` is enabled).
Any other origin gets HTTP 403 with no CORS headers.

## Retention errors

A raw-event read (`operation: "events"`, or a single event lookup) whose range
starts before the retention cutoff returns HTTP 400
`analytics_invalid_query`. The message names the RFC3339 cutoff and the error
envelope repeats it in a machine-readable field:

```json
{"error": {"code": "analytics_invalid_query",
           "message": "Events older than the retention cutoff 2026-08-05T06:00:00Z were compacted into rollups; narrow the range.",
           "retention_cutoff": "2026-08-05T06:00:00Z"}}
```

A retained read requested in a different time zone than the storage zone
returns the same code with both zone names and the bucket width in the message.

### Local timing observations

The `latency` and `provider_latency` timing aggregates use `first_token_latency_ms` and
`provider_latency_ms`, with sources `observed_dispatch_to_first_token` and
`observed_dispatch_to_response`. Analysis percentiles and processing-time totals/counts include
only observed values. Zero is a valid measurement; historical or unsupported observations stay
null and reduce coverage. Each retry restarts its local dispatch timer. These intervals use CPA's
monotonic clock and never subtract timestamps supplied by another machine.

Existing `latency_ms` remains E2E attempt duration. Existing TTFT behavior remains unchanged,
including its first-packet fallback; the new first-token latency requires a substantive token.

Native HTTP generation observers read decoded upstream data independently of downstream forwarding. Codex WebSocket observations use socket-read timestamps. Bounded queue saturation invalidates the attempt's generation duration while retaining first-output latency already observed; timing cannot be reconstructed after backpressure stops upstream reads. No fixed duration or TPS threshold changes measured values. Codex nonstream requests can have measured generation duration because their upstream response is SSE. Terminal-only output cannot supply a generation interval. CPAMC labels its existing duration/TTFT throughput fallback `EST`; this does not replace missing generation duration with a fabricated measurement.
