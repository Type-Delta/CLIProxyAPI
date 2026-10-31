# CPA management API

Paths are relative to your CPA base URL. Ask the user for their management key; never guess it. Send `Authorization: Bearer <key>` or `X-Management-Key: <key>`. Client inference keys are separate. Use `/v8/management`; `/v0/management` is deprecated.

## Access and errors

Docs are public whenever local management is available, even if remote API calls are forbidden. Remote calls require `management.allow-remote`; an environment management secret can override that policy. Home mode disables local management and docs. No configured management secret or local password also disables them.

| Status | Typical body or meaning |
| --- | --- |
| 401 | `{"error":"missing management key"}` or `{"error":"invalid management key"}`. Obtain the correct key from the user. |
| 403 | `{"error":"remote management disabled"}`, management key not set, or an IP ban after five failed attempts, normally 30 minutes. Stop guessing. |
| 404 | Management unavailable, unknown route, or resource not found. Availability failures have an empty body. Unknown management routes on an available server return `{"error":"unknown management route; see /llms.txt for the management API","agent_docs":"/llms.txt"}` after applicable v0 authentication/plugin handling; missing resources often return `{"error":"..."}`. |
| 400 / 422 | Invalid input or config validation failure; inspect `error` and any `path`. |
| 409 | Conflicting or missing revision for revisioned client-key writes. Read again before editing. |
| 503 | Optional subsystem unavailable. Analytics errors use `{"error":{"code":"...","message":"..."}}`; SQL uses an error string. |

OAuth callbacks are the exception to management-key authentication. They validate a pending login state. Do not treat callback acceptance as completed credential exchange.

## Conventions

Use JSON bodies and `Content-Type: application/json`, except YAML replacement, file uploads and downloads. Config JSON accepts the target value directly, without a `value` envelope. GET reads persisted configuration in the v8 tree without adding omitted defaults or migrating the file. PUT replaces its target; PATCH merges objects and replaces arrays and scalars; DELETE removes a field. Mapping paths cannot index arrays: replace the list. Successful writes save the latest layout; reads and rejected writes leave the file unchanged. Legacy field names are rejected; historical v8 aliases remain accepted. Per-key `null` inherits the corresponding group setting. Home-owned revision fields cannot be edited. JSON config reads redact TURN credentials; YAML reads include them.

For `/api-keys`, send `X-CPA-API-Key-Contract: 1` to use structured entries and require revision checks. Read `config_revision` or ETag first; send the revision in the JSON body or `If-Match`. DELETE always requires a revision and an `index`. Stale or missing required revisions return 409.

Tables list literal Gin route patterns. Replace `:id`, `:name`, `:channel`, `:attempt_id`, `:job_id` and `:batch_id` with URL-encoded values. `*path` covers any nested mapping path, for example `routing/retry/request-retry`.

Credentials use optional positive `page` and `page_size`, defaulting to page 1 and size 50 when pagination is requested. With neither, the list is unpaginated. Responses include `files`, `observed_at`, and, when paginated, `total`, `has_more`, `page`, `page_size`. Filters `name` and `auth_index` select exact identities. Log pagination uses `cursor` and `limit`, not page numbers.

Analytics GET query routes require RFC3339 `start`, `end`, and an IANA `time_zone`. In addition to the range, `timeseries` requires `bucket_width`, `dimensions` requires `dimension`, and `leaderboard` requires `sort_by`. Pagination and filters are operation-specific; do not send fields an operation does not support. POST `/analytics/query` accepts `schema_version`, `operation`, `start`/`end`/`time_zone` or a v2 `range`, `key_ids`, `filters`, and pagination fields. A calendar range such as `{"preset":"this_month","time_zone":"UTC"}` requires `schema_version:2`. Responses echo exact resolved bounds in `meta.range`; preserve those bounds and filters with the next cursor. Filter names include `provider`, `model`, `credential_id`, `endpoint_class`, `auth_type`, `service_tier`, `success`, `error_class`, `status_code`, `token_quality`. Events also accept `session_id`, `generated`, `outcome`, `result` and `source`. Availability varies by operation and retained data. Most string filters accept arrays of strings, with a scalar string also accepted. Event `outcome` instead requires one scalar enum: `succeeded`, `failed`, or `canceled`; `result` requires `success` or `failure`. Boolean filters require JSON booleans. `status_code` accepts an integer or integer array of HTTP statuses from 100 to 599.

Keep full key IDs and credential IDs in POST bodies, never URL parameters. For conditional reads use GET `/analytics/query` with unpadded base64url UTF-8 query JSON in `X-Analytics-Query`, no URL parameters or GET body. Decoded limit is 64 KiB. Send returned `ETag` as `If-None-Match`; a 304 needs the previously saved body. POST always returns a body. Matching successful reads can reuse a 60-second snapshot. Non-cursor bounds share snapshots at UTC-minute precision; cursor continuations retain exact returned bounds. Analytics amounts and decimal rates are strings; inspect partial/retention markers before drawing conclusions.

## Configuration and client keys

| Method and path | Purpose | Key parameters |
| --- | --- | --- |
| `GET /v8/management/config` | Read persisted v8 config | No params |
| `PUT /v8/management/config` | Replace complete JSON config | JSON configuration document |
| `PATCH /v8/management/config` | Merge JSON config | JSON object; arrays and scalars replace |
| `GET /v8/management/config.yaml` | Read complete YAML config | No params; includes secrets |
| `PUT /v8/management/config.yaml` | Replace complete YAML config | YAML body |
| `GET /v8/management/api-keys` | Read revisioned client keys and key identities | Returns `config_revision`, `key-identities`, ETag |
| `PUT /v8/management/api-keys` | Replace client keys | JSON `items`, `config_revision` or `If-Match` |
| `PATCH /v8/management/api-keys` | Edit one client key | JSON `index` or `match`/`old`; `new`/`value`, `label`, `limits`, `config_revision` or `If-Match` |
| `DELETE /v8/management/api-keys` | Delete client key by index | Query `index`; current revision in `If-Match` |
| `GET /v8/management/api-key-limits` | Read current client-key request/token limits | No params |
| `POST /v8/management/api-key-limits/reset` | Reset client-key usage counters | JSON `key` or `key_id` |
| `GET /v8/management/config/*path` | Read config section or field | `*path` mapping path |
| `PUT /v8/management/config/*path` | Replace config section or field | JSON target value; `*path` mapping path |
| `PATCH /v8/management/config/*path` | Merge config section or replace field | JSON target value; `*path` mapping path |
| `DELETE /v8/management/config/*path` | Remove config field | `*path` mapping path; no body |

## Server and upstream tools

| Method and path | Purpose | Key parameters |
| --- | --- | --- |
| `GET /v8/management/capabilities` | Discover supported management and analytics capabilities | No params |
| `GET /v8/management/storage` | Read storage size breakdown | No params |
| `GET /v8/management/server/latest-version` | Read latest release information | No params |
| `POST /v8/management/requests/api-call` | Make a credential-authenticated upstream HTTP call | JSON `method`, `url`; optional `auth_index`, `header`, `data`, `proxy_url`, `force_refresh`; `$TOKEN$` in header values substitutes upstream token |

## Telemetry

| Method and path | Purpose | Key parameters |
| --- | --- | --- |
| `POST /v8/management/telemetry/query` | Execute restricted agent SQL | JSON `sql`, optional `params`, `max_rows`; see [SQL](sql.md) |
| `GET /v8/management/telemetry/schema` | Read live tables, column types, comments and example SQL | No params |
| `GET /v8/management/telemetry/health` | Read capture ingestion backlog and legacy import state | No params; registered only with ClickHouse DSN |

## Analytics, usage and quota

| Method and path | Purpose | Key parameters |
| --- | --- | --- |
| `GET /v8/management/analytics/health` | Read analytics state, queue, retention and failure category | No params; `clickhouse_not_configured` without server mode |
| `GET /v8/management/analytics/summary` | Read usage, success and cost totals | Analytics GET range and filters |
| `GET /v8/management/analytics/timeseries` | Read bucketed usage | Analytics GET range; required `bucket_width`: `1m`, `5m`, `15m`, `30m`, `1h`, `1d`, `1w`; filters |
| `GET /v8/management/analytics/dimensions` | Read values for a dimension | Analytics GET range; required `dimension`: `provider`, `model`, `credential`, `key`, `endpoint`, `failure`, `latency`, `cache`, `service_tier`, `source`; filters |
| `GET /v8/management/analytics/events` | Page through sanitized upstream attempts | Analytics GET range, filters, `page_size`, `cursor`; list omits routing trace |
| `GET /v8/management/analytics/events/:attempt_id` | Read one attempt including routing trace and credential filename | Path `attempt_id`; required RFC3339 `start`, `end`, IANA `time_zone`; no pagination; trace may be null |
| `GET /v8/management/analytics/pricing` | Read effective pricing, overrides and missing prices | No params |
| `GET /v8/management/analytics/pricing/catalog-providers` | List pricing catalog providers | No params |
| `PUT /v8/management/analytics/pricing` | Replace manual pricing overrides | JSON `rules` or `overrides`; optional `currency_unit`, `rounding`, `kiro_usd_per_credit` |
| `POST /v8/management/analytics/pricing/reprice` | Start repricing surviving raw events | JSON `range` or `start`, `end`, `time_zone`; `dry_run`, `resume` |
| `GET /v8/management/analytics/providers` | Read provider credential availability and activity | Optional analytics GET range and filters |
| `GET /v8/management/analytics/quotas` | Read observed credential quota state | Optional analytics GET range and filters |
| `GET /v8/management/analytics/quota-history` | Read quota samples and credential identity mapping | Query `provider`, RFC3339 `start`, `end`, at most 90 days |
| `POST /v8/management/analytics/quota-history` | Record quota observations | JSON `provider`, `samples` up to 100: `auth_index`, `window_id`, `remaining_percent`; optional `reset_at`, `period_hours`, `plan_id`, `capacity` |
| `GET /v8/management/analytics/keys` | Read key labels and range/lifetime activity | Analytics GET range; catalog cursor in `X-Analytics-Cursor`, not URL; key identity filters use POST query instead |
| `GET /v8/management/analytics/leaderboard` | Rank usage or cost | Analytics GET range, filters; required `sort_by`: `tokens` or `cost` |
| `POST /v8/management/analytics/query` | Run a structured analytics query | JSON common query; operations `summary`, `timeseries`, `dimensions`, `events`, `leaderboard`, `activity`, `analysis`, `model_cost_series`, `model_leaderboard` |
| `GET /v8/management/analytics/query` | Conditionally read structured analytics | Header `X-Analytics-Query`; optional `If-None-Match`; no URL params |

## Analytics maintenance and sharing

| Method and path | Purpose | Key parameters |
| --- | --- | --- |
| `POST /v8/management/analytics/exports` | Download sanitized event CSV or JSON | JSON `query` with operation `events`, `format`: `csv` or `json`, optional `max_rows`; no cursor |
| `POST /v8/management/analytics/backups` | Start analytics backup job | JSON `path`, a server-side destination |
| `POST /v8/management/analytics/backups/:id/restore` | Start analytics restore job | Path `id`; JSON `path`, `manifest` |
| `POST /v8/management/analytics/imports/cpauk` | Import standalone CPAUK data | JSON `path`, `dry_run`, `resume`, `batch_id`, `chunk_size` up to 10000; `backup_path` required for real import |
| `POST /v8/management/analytics/imports/:batch_id/rollback` | Start rollback of an import batch | Path `batch_id` |
| `POST /v8/management/analytics/purges/key` | Preview or execute key erasure | JSON `key_id`, `preview`; execution needs `confirmed:true`, preview `batch_id`, `backup_path` |
| `POST /v8/management/analytics/repairs` | Start repair or retry intake | JSON `kind`: `integrity_check`, `checkpoint`, `reindex`, `retry_intake`, `abandon_legacy_import` with `reason`, or `start_new_identity_epoch` with `confirmed`, `archive_path` |
| `GET /v8/management/analytics/jobs/:job_id` | Read maintenance job state | Path `job_id` |
| `DELETE /v8/management/analytics/jobs/:job_id` | Cancel maintenance job | Path `job_id`; 204 on success |
| `POST /v8/management/analytics/viewers` | Create scoped viewer credential | JSON `key_id`, `allowed_views`, `expires_at`, optional `label`; credential returned once |
| `GET /v8/management/analytics/viewers` | List viewer metadata | No params |
| `DELETE /v8/management/analytics/viewers/:id` | Revoke viewer | Path `id` |

## Routing

| Method and path | Purpose | Key parameters |
| --- | --- | --- |
| `POST /v8/management/routing/cooldown/reset` | Clear one credential cooldown/quota routing state | JSON `auth_index`; does not reset provider billing quota |
| `GET /v8/management/routing/model-definitions/:channel` | Read static provider model definitions | Path `channel`: `claude`, `kiro`, `gemini`, `gemini-interactions`, `vertex`, `aistudio`, `codex`, `kimi`, `kimi-ai`, `kimi.ai`, `kimi.com`, `antigravity`, `xai`, `x-ai`, `grok`, `devin`, `meta`, `muse`; case-insensitive. No static channel exists for `openai-compatibility` or arbitrary config API-key provider names; these built-in channels are not per-key catalogs. |
| `POST /v8/management/routing/model-definitions/refresh` | Refresh model catalogs immediately | No body; returns catalog status, sources and `changed_providers`; invalid sources return 400, refresh failures return 502 |
| `POST /v8/management/models/refresh` | Refresh model catalogs immediately | Alias of `/routing/model-definitions/refresh` |
| `GET /v8/management/routing/credentials` | List secret-free routable credentials | Returns `credentials` with `cooldowns`, `observed_at`, `client_types`; covers config API keys and OAuth; IDs are Auth.ID, not analytics HMAC IDs |
| `POST /v8/management/routing/dry-run` | Simulate routing without changing runtime state | JSON required nonblank `model`, otherwise 400 with `path:"model"`; optional `api_key_label`, `client_type`, `headers`, `session_id`, draft `rules`; returns `trace`, `warnings` |

## Observability

| Method and path | Purpose | Key parameters |
| --- | --- | --- |
| `GET /v8/management/observability/logs` | Read application logs incrementally | Query `after` Unix seconds, `limit`, `cursor`; use returned `next-cursor`. Requires file logging enabled, otherwise 400 `{"error":"logging to file disabled"}`. Captured request logs use separate endpoints. |
| `DELETE /v8/management/observability/logs` | Clear application log files | No params |
| `GET /v8/management/observability/logs/errors` | List captured request error logs | No params |
| `GET /v8/management/observability/logs/errors/:name` | Download one error log | Path `name` |
| `GET /v8/management/observability/logs/requests/:id` | Read or reconstruct captured request and attempts | Path `id` is request-log identifier |
| `GET /v8/management/observability/usage/api-keys` | Read in-memory per-key usage | No params |
| `GET /v8/management/observability/usage/queue` | Pop queued usage records | Query `count`, default 1; consuming read |

## Credentials and OAuth

| Method and path | Purpose | Key parameters |
| --- | --- | --- |
| `GET /v8/management/credentials` | List credentials and live cooldown state | Query `name`, `auth_index`, `page`, `page_size`; returns `files` |
| `POST /v8/management/credentials` | Upload credential JSON files | Multipart files, or raw JSON with query `name`; batches report per-file failures |
| `DELETE /v8/management/credentials` | Delete credential files | Query repeated `name`, or JSON `name`/`names`/array; `all=true` deletes all files |
| `GET /v8/management/credentials/models` | List models for credential | Query `name` |
| `GET /v8/management/credentials/download` | Download credential JSON, including secrets | Query `name` ending in `.json` |
| `PATCH /v8/management/credentials/status` | Enable or disable a credential | JSON `name`, `disabled`; optional `auth_index` |
| `PATCH /v8/management/credentials/fields` | Edit credential settings | JSON `name` plus supported fields such as `label`, `prefix`, `priority`, `weight`, `proxy_url`, `request_retry` |
| `POST /v8/management/credentials/refresh` | Refresh one or all credential tokens | JSON or query `name`, `auth_index`, `all`; `all=true` selects all |
| `POST /v8/management/credentials/verify` | Verify a credential with a minimal inference request | JSON `name`, optional `model`; uses an isolated session and a minimal 1-token prompt |
| `GET /v8/management/credentials/siwc/status` | Read local SIWC credential status | Alias of `/oauth/siwc/status`; query `name` or `auth_index`; no upstream requests |
| `POST /v8/management/oauth/import` | Import Vertex service account | Query `provider=vertex`; multipart `file` |
| `GET /v8/management/oauth/auth-url` | Start built-in or plugin OAuth | Required query `provider`; optional provider-specific login params; returns `url`, `state` |
| `POST /v8/management/oauth/auth-url` | Start built-in or plugin OAuth | JSON provider and login options; supports targeted recovery using `name` or `auth_index` and optional `verify` |
| `POST /v8/management/oauth/siwc/bind` | Bind SIWC to an existing Codex OAuth credential | JSON `name`; standalone mode and local file-backed credentials required |
| `GET /v8/management/oauth/siwc/status` | Read local SIWC credential status | Query `name` or `auth_index`; returns `status`: `off`, `valid`, or `invalid`; no refresh or inference request |
| `GET /v8/management/oauth/status` | Poll OAuth exchange and persistence | Query `state`; `status`: `wait`, `ok`, `error` |
| `DELETE /v8/management/oauth/session` | Cancel pending OAuth session | Query `state` |
| `GET /v8/management/oauth/callback` | Submit OAuth callback without management key | Query `state`, `code` or `error`, optional `provider` |
| `POST /v8/management/oauth/callback` | Submit OAuth callback without management key | JSON `state`, `code` or `error`, optional `provider`; alternatively `redirect_url` |

## Plugins

| Method and path | Purpose | Key parameters |
| --- | --- | --- |
| `GET /v8/management/plugins` | List installed plugins | No params |
| `DELETE /v8/management/plugins/:id` | Delete installed plugin | Path `id` |
| `GET /v8/management/plugins/store` | Read plugin catalog and source errors | No params |
| `POST /v8/management/plugins/store/:id/install` | Install or update plugin | Path `id`; optional query `source`, `version`, JSON `version` |
| `GET /v8/management/plugins/:id/quota` | Read cached plugin credential quota | Path `id`; query `auth_index` |
| `POST /v8/management/plugins/:id/quota` | Fetch plugin credential quota | Path `id`; JSON `auth_index` |
| `DELETE /v8/management/plugins/:id/quota` | Reset plugin quota cache/state | Path `id`; query `auth_index` |

## Task recipes

Set the base URL and use the key supplied by the user. Do not include secrets in reports.

```sh
CPA_BASE=https://cpa.example.com
curl -sS "$CPA_BASE/v8/management/capabilities" \
  -H "Authorization: Bearer $CPA_MANAGEMENT_KEY"
```

### Set routing request-retry to 1

Read the current value, write the scalar, then read again to verify persistence.

```sh
curl -sS "$CPA_BASE/v8/management/config/routing/retry/request-retry" \
  -H "Authorization: Bearer $CPA_MANAGEMENT_KEY"
curl -sS -X PUT "$CPA_BASE/v8/management/config/routing/retry/request-retry" \
  -H "Authorization: Bearer $CPA_MANAGEMENT_KEY" -H 'Content-Type: application/json' --data '1'
curl -sS "$CPA_BASE/v8/management/config/routing/retry/request-retry" \
  -H "Authorization: Bearer $CPA_MANAGEMENT_KEY"
```

### Which credentials are in cooldown now?

Start with the routing catalogue. It includes every credential, including config API keys with no auth file.

```sh
curl -sS "$CPA_BASE/v8/management/routing/credentials" \
  -H "Authorization: Bearer $CPA_MANAGEMENT_KEY"
```

Inspect every `credentials[].cooldowns` using the response's `observed_at`. Each active timer has `scope`, optional `model_key`, `reason`, `retry_at`, `remaining_seconds`, and optional `http_status`/`backoff_level`. An empty array means no active local cooldown timer, not that the credential is usable: disabled credentials, auth failures and other gates can still block it. Do not answer "none in cooldown" based on an empty file list.

For file-backed credentials, `/credentials` adds quota observations and status detail:

```sh
curl -sS "$CPA_BASE/v8/management/credentials?page=1&page_size=100" \
  -H "Authorization: Bearer $CPA_MANAGEMENT_KEY"
```

Follow `has_more` with increasing pages. Inspect `files[].unavailable`, `disabled`, `status_message`, `next_retry_after`, `quota`, `model_quotas` and `quota_reserve_state`. This file-oriented list can omit config API-key credentials. A dry run with the affected model explains candidate gates in the current state. Do not reset state merely to investigate.

```sh
curl -sS -X POST "$CPA_BASE/v8/management/routing/dry-run" \
  -H "Authorization: Bearer $CPA_MANAGEMENT_KEY" -H 'Content-Type: application/json' \
  --data '{"model":"MODEL_FROM_REQUEST","session_id":"codex:SESSION_ID_FROM_REQUEST","client_type":"codex"}'
```

Use the exact canonical session ID, including its source prefix and any `:agent:` suffix; resolve a bare user ID with the [SQL session recipe](sql.md#session-xb12345-intermittently-received-403). Optional `api_key_label` must match a configured client-key label. Add actual request `headers` to reproduce header rules. Draft `rules` replace saved rules for this simulation only. Inspect `trace.candidates`, rules, affinity, pick and decider. Routing gates outrank affinity, which outranks the strategy, which outranks custom rules. Rules cannot override disabled, cooldown, quota or model gates.

### Why was request X routed to credential Y?

Use [SQL](sql.md) to map a request-log ID to its `proxy_request_id` and `attempt_id` values, or query events by the known session and range. The SQL mapping already returns `routing_trace_json` if only the trace is needed. To resolve labels and credential filename, fetch event detail with a range covering the attempt:

```sh
curl -sS --get "$CPA_BASE/v8/management/analytics/events/ATTEMPT_ID" \
  -H "Authorization: Bearer $CPA_MANAGEMENT_KEY" \
  --data-urlencode "start=$CPA_START" --data-urlencode "end=$CPA_END" \
  --data-urlencode "time_zone=$CPA_TIME_ZONE"
```

Set `CPA_START` and `CPA_END` to RFC3339 timestamps and `CPA_TIME_ZONE` to the user's IANA zone, for example `UTC`. Event detail without a range returns 400 `analytics_invalid_query`. Event detail returns `routing_trace` with loaded credential labels and nullable `credential_filename`. Read the historical gates, matched/applied rules, affinity, strategy and pick. A dry run describes current state, so it cannot prove a historical decision. Trace capture needs analytics and ClickHouse; null traces and expired events cannot be reconstructed from current state. Routing IDs, `auth_index` and analytics HMAC credential IDs are different identities; use returned mappings and labels instead of comparing them directly.

### Investigate session 403s or monthly AAA skill usage

Start with [agent SQL](sql.md), health and live schema. Session 403s need client status and every upstream attempt, since retries can hide a 403 behind a final 200. Skill counts need distinct per-session client-context membership, not global entry IDs or repeated history. Without ClickHouse, request logs can help with individual captured failures, but there is no complete SQL history.

### Start credential login

```sh
curl -sS "$CPA_BASE/v8/management/oauth/auth-url?provider=codex" \
  -H "Authorization: Bearer $CPA_MANAGEMENT_KEY"
curl -sS "$CPA_BASE/v8/management/oauth/status?state=STATE_FROM_LOGIN" \
  -H "Authorization: Bearer $CPA_MANAGEMENT_KEY"
```

Open the returned `url` for the user to authorize. Providers include `claude`, `codex`, `antigravity`, `kimi`, `kimi-ai`, `xai`, `devin`, `meta`, and registered plugin IDs. Poll the returned state until `ok` or `error`; `wait` remains pending.

### Read this month's usage

```sh
curl -sS -X POST "$CPA_BASE/v8/management/analytics/query" \
  -H "Authorization: Bearer $CPA_MANAGEMENT_KEY" -H 'Content-Type: application/json' \
  --data '{"schema_version":2,"operation":"summary","range":{"preset":"this_month","time_zone":"UTC"}}'
```

Use the user's intended time zone. Read `proxy_requests` for analytics client-request counts and `upstream_attempts` for attempt counts; retries can make the latter larger. Use captured-request SQL for request-level investigation, checking capture and retention coverage before comparing datasets.
