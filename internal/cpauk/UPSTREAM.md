# CPAUK upstream provenance

CPA Usage Keeper in CPA is adapted from `Willxup/cpa-usage-keeper` release `v1.15.0`, commit `696a4659ce1d5d6f2d2d0530e3205eb51fbce889`. The pinned commit date is 2026-08-30. Its MIT license is preserved byte-for-byte in `LICENSE.upstream`.

CPAUK remains a package boundary inside CPA's root Go module. CPA does not import the upstream executable or its `internal` packages, run it as a child process, or use it as a runtime submodule.

## Adapted behavior

CPA adapts these upstream sources and formulas:

- Event and rollup grains from `internal/entities/usage_event.go`, `usage_event_archive.go`, `usage_overview_hourly_stat.go`, `usage_overview_daily_stat.go`, `usage_activity_stat.go`, `usage_latency_stat.go`, and the corresponding `internal/{overview,activity,latency}/aggregate.go` files.
- Calendar and rolling range behavior from `internal/timeutil/usage_query_range.go`, `usage_range.go`, and `storage.go`. CPA makes every range start-inclusive and end-exclusive, requires an explicit IANA time zone for calendar queries, and starts weeks on Monday.
- Token parity vectors from `internal/service/tokenprocessor/`. CPA consumes CPA's already-normalized `usage.Record` fields and does not reapply provider normalization.
- Cost calculation and exact-field rule matching from `internal/helper/usage_cost.go` and `internal/pricing/`. CPA resolves an exact model before its exact alias, subtracts cache-read and cache-creation tokens from uncached input without going below zero, and applies all matching rule multipliers.
- The one-percent relative-error latency sketch from `internal/latency/sketch.go`. CPA records its format version and derives deterministic sampling priority from attempt-ID bytes instead of an upstream numeric row ID.
- Event cursor ordering from `internal/repository/usage.go`. CPA uses a stable timestamp and attempt-ID tuple, queries full key hashes directly, and supports bounded multi-key filters.
- Backup, archive, and maintenance safety rules from `internal/backup/`, `internal/repository/usage_event_archive.go`, and `internal/repository/storage_vacuum.go`.

## Intentional differences

CPA rejects the upstream runtime layout and data practices that conflict with the control-plane contract:

- Upstream GORM and `mattn/go-sqlite3` are replaced with a pure-Go SQLite driver so CPA keeps its `CGO_ENABLED=0` targets.
- Raw API keys become lowercase SHA-256 key IDs before enqueue. Client IPs, forwarded addresses, user agents, raw headers and bodies, filenames, raw auth indexes, and arbitrary source metadata never enter CPAUK storage.
- CPA stores a random attempt ID and a distinct proxy request ID. Attempt counts are event rows; proxy request counts are distinct proxy request IDs.
- Upstream `float64` currency becomes signed 64-bit nano-USD. CPA calculates an event's cost with exact integer arithmetic and rounds half away from zero once, after all multipliers. Aggregates sum the rounded event values. JSON represents currency as a decimal string with up to nine fractional digits.
- Missing price with billable tokens is unknown. CPA reports its tokens separately as unpriced usage and never treats it as zero-cost traffic.
- Upstream's single raw-key filter becomes deduplicated full-hash multi-key filtering. Leaderboards rank an arbitrary selected range by tokens or known cost, then by full key ID ascending, with cursor state that includes both values.
- Upstream's fixed `Asia/Shanghai` ranking periods and proprietary overall score are not ported.
- CPA migrations use embedded, checksummed, transactional SQL. CPA adds verified backup, restore, repair, purge-by-key-ID, resumable import, rollback, and bounded maintenance jobs because upstream does not supply them.

## Gate 0 baselines

- CPA pushed baseline: `dae4267c70c835d323b00bfd9b2baaeb8386e92e`, including initial CPAMC gitlink commit `da22fc05737f933b4e7685794822ab3efe08d923` and synchronized gitlink commit `dae4267c70c835d323b00bfd9b2baaeb8386e92e`.
- CPA upstream base: `81e1b5374f99c212f196f34956eeed964a46b8fa`.
- CPAMC initial fork head: `d249ff008e0bc2803deb23fb3e2c62418a1e8d17`.
- CPAMC pushed synchronized head: `1f77aaeb126c44e69ff51ccbcac6b2d5ebde9ee3`.
- CPAMC upstream base: `e0ee7123dfb5aa89a14ff73ac5a5c3bf4db658e0`.
- Before sync, `git rev-list --left-right --count origin/main...upstream/main` in CPAMC returned `0 2`. After the push it returned `2 0`.
- Exact Bun 1.3.14 verification passed 424 tests, ESLint, TypeScript compilation, and the Vite production build.
- The Type-Delta account had `ADMIN` permission on both forks. Both repositories had no branch protection or repository rulesets at the time of the check.
- A clean detached CPA clone at `78678adcab805a0fe12c5b10d2b586cb9c177dd6` passed `go test ./...` and `go build -o test-output ./cmd/server && unlink test-output`. The later submodule commits do not change Go sources.
- The current main CPA image built as `sha256:8d161f53a62a2e0b5de3b38f3f58fec76bf1926d49960932da0b4c6dd2c0cb82`. The current web image built as `sha256:0580216b55729b5e591c1fe3559580af322a97de49d19721213cb19999aebbce`.
- Go 1.26.4 builds passed for native Linux amd64, Windows amd64 and arm64, Linux amd64 and arm64 with `CGO_ENABLED=0`, FreeBSD arm64 with `CGO_ENABLED=0`, and a native Linux amd64 CGO compile.
- This Linux host lacks Apple and FreeBSD CGO sysroots and an arm64 CGO cross-compiler. Its glibc is newer than the release baseline. Darwin CGO, FreeBSD amd64 CGO, Linux arm64 CGO, and the Linux glibc 2.17 packaging check remain assigned to their existing platform-specific CI runners.
