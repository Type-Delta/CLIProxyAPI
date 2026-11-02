# Scoped management keys

`management.secret-key`, `MANAGEMENT_PASSWORD`, and a local management password retain
full access. Additional keys live in the v8 configuration:

```yaml
management:
  keys:
    - id: dashboard
      name: Dashboard
      secret-key: replace-with-a-long-random-secret
      scope: read-only
```

Every entry requires a unique nonempty ID, a nonempty name and secret, and exactly
`full-access` or `read-only`. CPA hashes plaintext secrets with bcrypt before storing
them. Invalid scopes fail configuration validation and never authenticate. The remote
management access setting applies to every key. A deployment with only additional keys
still exposes management routes. Removing a key or changing its secret revokes it after
the configuration update.

Authenticated `GET /v8/management/session` returns `{"scope":"read-only"}` or
`{"scope":"full-access"}`. Full-access keys can manage additional credentials:

| Method | Path under `/v8/management` | Request | Response |
| --- | --- | --- | --- |
| GET | `/management-keys` | | `{"keys":[{"id":"…","name":"…","scope":"read-only"}]}` |
| POST | `/management-keys` | `{"name":"Dashboard","key":"…","scope":"read-only"}` | `{id,name,scope}` |
| PATCH | `/management-keys/:id` | Optional `name`, `key`, `scope` | `{id,name,scope}` |
| DELETE | `/management-keys/:id` | | `{"status":"ok"}` |

CPA generates IDs for new keys. Key values and hashes never appear in key metadata or
ordinary JSON configuration reads. Full-access users can still download raw config YAML.
Configuration writes preserve unrelated persisted settings and surrounding comments.

Read-only access uses an explicit v8 route allowlist. It includes session and capability
metadata, credential summaries, model definitions, storage observations, analytics reads,
structured `POST /analytics/query`, in-memory CSV/JSON event exports, and restricted
`POST /telemetry/query`. The telemetry
SQL connection uses the existing ClickHouse account with constant read-only settings,
SELECT access limited to the configured database, and no external-source privileges.
Pricing GETs do not initiate catalog refreshes for read-only sessions.

Read-only keys cannot use legacy v0 management routes, arbitrary plugin management routes,
config paths, credential downloads, OAuth initiation, key CRUD, mutations, or the
non-HTTP management authentication used by the queue protocol. Plugin quota fetches are
observational and allowed; quota resets remain full-access operations.

`POST /requests/api-call` accepts read-only quota observations only for recognized
provider endpoints and matching credentials. Validation restricts methods, URLs, query
parameters, request bodies, and headers before resolving credentials. Caller-selected
proxies, host overrides, redirects, arbitrary token substitution, quota-reset operations,
and paid inference probes are forbidden. Responses exclude upstream headers and usable
credential fields. Unknown provider probes remain denied. Meta's existing quota operation
mints a key and requires credential-file access; it is unavailable to read-only sessions.

Dashboard, Logs Viewer, and Management Center Info are available to read-only sessions.
`GET /overview` returns an explicitly selected summary, never raw configuration:

```json
{"config":{"debug":false,"loggingToFile":true,"requestLog":false,"requestRetry":3,"routingStrategy":"round-robin","proxyUrl":"http://proxy.example:8080","quotaExceeded":{"switchProject":true,"switchPreviewModel":true}},"counts":{"apiKeys":2,"providerKeys":1},"providerKeyCounts":{"gemini":0,"interactions":0,"codex":1,"meta":0,"xai":0,"claude":0,"vertex":0,"openai":0}}
```

Provider counts count configured key entries, including each OpenAI-compatible key entry.
The proxy summary contains only scheme and host, with optional port; userinfo, paths,
queries, and fragments are omitted. Invalid proxy URLs appear as an empty string.
`GET /config` remains denied. Read-only sessions may also use `GET /server/latest-version`
for the version check and the following observational routes:

- `GET /observability/logs`
- `GET /observability/logs/errors`
- `GET /observability/logs/errors/:name`
- `GET /observability/logs/requests/:id`
- `GET /observability/usage/api-keys`

Read-only application log reads remove credential assignments, cookies, URL userinfo and
queries, and known configured/runtime secrets. Request-log downloads retain request
metadata and response status, omitting all headers, bodies, upstream payloads and WebSocket
timelines because arbitrary payload fields can contain credentials. Download filenames and
path validation are unchanged. Full-access log responses are unchanged.

Read-only API-key usage preserves provider groups, totals, and recent-request buckets,
but replaces each `baseUrl|apiKey` identity with `key-1`, `key-2`, and so on within its
provider group. These numbers are observational labels, not persistent credential IDs;
clients must not parse URLs or keys from them. Full-access identities remain unchanged.
Deleting logs, toggling log settings, updating the server, refreshing models, and all other
privileged operations remain denied.
