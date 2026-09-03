# Analytics operations

CPA analytics is optional and disabled by default. Its database, identity key,
viewer hashes, and maintenance artifacts live below `auth-dir/state/analytics`
unless `analytics.path` selects another location. Keep that directory on a
volume with enough space for the configured database limit, reserve, and one
full verified backup.

## Enable and disable

Set `analytics.enabled: true` and restart CPA for the first enablement. An empty
path resolves to `auth-dir/state/analytics/analytics.db`. CPA remains ready if
analytics cannot initialize; inspect `/v0/management/analytics/health` for the
redacted failure category.

Disabling analytics detaches intake and performs a bounded drain. It retains the
database and identity key. Re-enable it to resume the same history. Path and
queue-capacity changes require a CPA restart.

## Storage time zone

`analytics.storage-time-zone` selects the zone whose local hour and day
boundaries retention uses when it builds rollups. It defaults to `UTC` and must
be a valid IANA name; an invalid value disables analytics only and leaves the
proxy serving traffic. Set it before the first retention run: the zone is
recorded in the analytics database on first use, and existing rollups are never
rebucketed by a later configuration change.

Raw events answer any query zone. Once a range has been retained, a query in a
different zone cannot be rebucketed exactly, so CPA returns a `partial` result —
`analytics_invalid_query` naming the stored zone, the requested zone, and the
bucket width — instead of relabelling rollup buckets. Choose the zone your
operators read reports in, or keep the default and query in `UTC`.

If the persisted `retention_time_zone` metadata disagrees with the configured
zone and any rollups already exist, the store refuses to open. Analytics then
reports `state: circuit_open` with `category: storage_time_zone`, `field:
storage-time-zone`, and a health `zone_mismatch` object naming both zones:

```json
{ "zone_mismatch": { "stored": "UTC", "configured": "Asia/Kolkata" } }
```

Restore the stored zone in the configuration, or reset analytics storage, to
bring analytics back.

Raw-event queries (Events and single-event lookups) whose range starts before
the retention cutoff also return `analytics_invalid_query`. That message names
the RFC3339 cutoff and the error envelope repeats it in `retention_cutoff`, so
the operator can narrow the range to the raw window.

## Shared-view HTTPS

Viewer credentials can be exchanged only over direct TLS or forwarded HTTPS
from a trusted immediate proxy. The ordinary web Compose file deliberately
replaces inbound forwarding headers and therefore does not treat its public
HTTP port as secure.

When a TLS load balancer connects to the bundled Nginx container over HTTP, use
the forwarding override and set the narrow CIDR of that immediate balancer:

```bash
CPA_WEB_TLS_PROXY_CIDR=10.20.30.0/24 \
  docker compose -f docker-compose-web.yml -f docker-compose-web-forwarded.yml up -d
```

Keep `analytics.viewer.trusted-proxy-cidrs` restricted to the bundled
`CPA_WEB_NETWORK_SUBNET` bridge (the default is `172.30.0.0/24`). Override that
subnet when it overlaps another Docker network, and put the same narrow CIDR in
`analytics.viewer.trusted-proxy-cidrs`. Nginx accepts `X-Forwarded-Proto: https`
only from the CIDR supplied above and sends its verified result to CPA. Never
use a public or catch-all CIDR. Direct clients and untrusted peers cannot
upgrade plain HTTP by spoofing the header.

## Cross-origin viewer clients

The viewer API is same-origin only by default: the browser's `Origin` must
match the request's own scheme and host. `analytics.viewer.allowed-origins`
extends that with an explicit list of additional browser origins allowed to
call `/v0/analytics/viewer/*` — for example a CPAMC dev server or a viewer app
served from a different host than CPA itself. Each entry is an absolute
origin (scheme + host[:port], no path/query/fragment/userinfo), matched
case-insensitively with default ports normalized. `https` origins are always
accepted; `http` is accepted only for a loopback host (`127.0.0.1`,
`localhost`, `[::1]`) and only when `allow-loopback-http` is also `true`. Up
to 32 entries are allowed; an invalid or duplicate entry disables analytics
only, the same as any other analytics configuration error.

An allowed cross-origin request receives `Access-Control-Allow-Origin` echoing
the request's `Origin`, `Access-Control-Allow-Credentials: true`, and
`Vary: Origin`; anything else still gets a 403. The session cookie set for a
cross-origin exchange uses `SameSite=None` (still `Secure`) instead of the
same-origin default `SameSite=Strict`, because browsers require `SameSite=None`
for a cookie to be sent on a cross-origin request. `Secure` is always set, so a
cross-origin viewer needs HTTPS — the one exception is the documented
loopback-http development case, where Chrome and other major browsers accept
`Secure` cookies over `http://127.0.0.1` and `http://localhost`. Changing
`allowed-origins` requires a CPA restart, the same as the other
`analytics.viewer` fields.

Shared-view links embed the CPA API origin so recipients do not need stored
management configuration. For split-origin deployments, CPA must list the
CPAMC origin in `analytics.viewer.allowed-origins`. The trust model is narrow:
a crafted link can only send its own credential to its own server.

## Backup

Start a backup through the authenticated Analytics Maintenance page or the
management API. A successful job creates three protected files: the SQLite
backup, its identity key, and a strict checksum manifest. Treat the identity key
as secret material. A database backup without its matching identity key is not
restorable.

Never copy a live database file directly. CPA uses SQLite's online backup API,
then verifies integrity and checksums before reporting success.

## Restore and repair

Disable new analytics intake or use the restore maintenance job. Restore first
verifies the manifest, database, identity-key fingerprint, schema version, and
available temporary space. It stages the replacement and retains the former
database and key with a rollback suffix.

Use integrity check, WAL checkpoint, or reindex for a structurally readable
database. Do not delete, rename, or replace a corrupt database automatically.
If the identity key is lost, restore a verified backup. The explicit new-identity
epoch operation archives the old database and starts empty; it does not rewrite
historical identities.

## Retention, purge, and import

Retention builds and verifies rollups before deleting covered raw attempts.
Purge-by-key-ID requires a preview, explicit confirmation, and a verified backup.
Deleting or rotating a configured API key never deletes analytics history.

CPAUK import is an explicit authenticated maintenance job. Run a dry run first.
The importer reads the source database read-only, transforms allowlisted fields,
hashes keys, writes bounded transactions, and records a resumable checkpoint and
batch ID. Use rollback-by-batch-ID if reconciliation fails.

## Reprice stored events

Updating the pricing catalog affects new events immediately. To apply the
catalog to existing raw history, first preview the exact range:

```json
POST /v0/management/analytics/pricing/reprice
{
  "range": {
    "preset": "custom",
    "start": "2026-08-01T00:00:00Z",
    "end": "2026-08-08T00:00:00Z",
    "time_zone": "UTC"
  },
  "dry_run": true
}
```

The response is an accepted maintenance job. Poll its job URL and inspect
`matched`, `updated`, `checkpoint`, `completed`, `effective_start`,
`retained_cutoff`, and `history_complete` in the terminal result.
Repeat without `dry_run` to commit. Repricing runs in bounded transactions and
records its last committed checkpoint in the analytics database. If a job is
canceled or interrupted, submit the same resolved range with `resume: true`;
changing the range starts a distinct checkpoint lineage. A checkpoint is also
bound to the pricing catalog used for its committed chunks, so update the
catalog only after the job completes; a resume against a different catalog is
rejected and must restart from the beginning. Dry runs never advance the
durable checkpoint.

The job reprices every raw event still stored in the selected range. When the
range also contains retained rollups, it completes the raw portion and returns
`history_complete: false`, the `retained_cutoff`, and the earliest
`effective_start` from which the result is guaranteed complete. Retained totals
keep their stored prices. Rollups do not retain requested aliases or individual
event token distributions, so CPA cannot reproduce alias matching or
once-per-event rounding exactly from a rollup. Restoring a pre-retention backup
and repricing before retention runs is the only exact way to change that older
history.

## Query series and rounding

Activity and analysis series return every bucket that intersects the requested
range. Buckets without matching events contain zero values. This keeps gaps
visible and gives callers a stable sequence for charts.
The `today`, `this_week`, and `this_month` named ranges end at request time;
`yesterday` remains the complete previous local calendar day.

Latency p95 and maximum values use every matching raw event in ranges of up to
30 days. The scatter payload remains capped at 1,000 points and samples across
the full ordered range. Provider credential request counts deduplicate proxy
request IDs across raw events, retained request identities, and auth-type
groups.

Cost breakdown components reconcile to each event's stored known cost. CPA
prices each component, then assigns any nano-USD rounding residual in this
fixed order: uncached input, cache read, cache creation, output. Negative
residuals are removed in the same order without making a component negative.

## Rollback

To roll back CPA, first set `analytics.enabled: false`, restart, and deploy the
previous binary. Do not downgrade or remove the analytics database; older CPA
binaries ignore it.

To roll back CPAMC, disable the panel updater and install the prior matching
`management.html` and `management-artifact.json`, or deploy a CPA build that
bundles the prior CPAMC gitlink. If no valid mutable pair remains, remove only
those two named files through recoverable deployment tooling; CPA falls back to
its bundled artifact.
