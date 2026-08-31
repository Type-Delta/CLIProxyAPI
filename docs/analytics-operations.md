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

## Rollback

To roll back CPA, first set `analytics.enabled: false`, restart, and deploy the
previous binary. Do not downgrade or remove the analytics database; older CPA
binaries ignore it.

To roll back CPAMC, disable the panel updater and install the prior matching
`management.html` and `management-artifact.json`, or deploy a CPA build that
bundles the prior CPAMC gitlink. If no valid mutable pair remains, remove only
those two named files through recoverable deployment tooling; CPA falls back to
its bundled artifact.
