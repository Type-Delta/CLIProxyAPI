# Analytics Management API v2

This document freezes the additive contract used by CPA and CPAMC analytics.
CPA advertises support by including `2` in `api_schema_versions`. Version 1
requests and fields retain their existing meaning. Full API-key and credential
identities remain POST-body-only and never appear in URLs; CPAMC may persist a
collision-safe short key reference in its client-side hash URL.

## Query ranges

Query bodies may keep the version 1 `start`, `end`, and `time_zone` fields or
use `range`:

```json
{
  "preset": "today|yesterday|last_n_hours|last_n_days|this_week|this_month|custom",
  "n": 7,
  "start": "2026-08-01T00:00:00Z",
  "end": "2026-08-08T00:00:00Z",
  "time_zone": "Asia/Kolkata"
}
```

`n` is required only for rolling presets. `start` and `end` are required only
for `custom`. Every response echoes the resolved UTC bounds and IANA zone in
`meta.range`. `today`, `this_week`, and `this_month` end at request time;
`yesterday` is the complete previous local calendar day.

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
and ordered, gap-free `buckets`. Each bucket contains `start`, `end`, request
success/failure counts, known cost, and the six token categories plus
`total_tokens`.

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
filter body without a cursor and emit every stored, sanitized event field.
They never export raw keys, request bodies, headers, IP addresses, or other
forbidden event fields. Event pages include filter-wide `total_count`; cursors
remain bound to the exact resolved range and filter selection.

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
