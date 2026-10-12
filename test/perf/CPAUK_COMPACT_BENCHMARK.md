# CPAUK compact storage benchmark

This opt-in benchmark seeds deterministic events through the real `SQLiteStore.WriteBatch` intake path, then measures storage and query behavior. It is kept separate from the normal test suite and emits JSON plus Markdown so a compact aggregate implementation can be compared with the same fixture and command.

Run the committed baseline from an isolated checkout or worktree:

```bash
CPAUK_BENCH_REQUESTS=100000 \
CPAUK_BENCH_REPETITIONS=3 \
CPAUK_BENCH_LABEL=baseline \
CPAUK_BENCH_REBUILD=1 \
test/perf/run_cpauk_compact_benchmark.sh
```

The runner defaults to `/tmp/cpauk-compact-benchmark` and keeps the dataset, JSON artifact, Markdown report, and complete test log together. Use `CPAUK_BENCH_REQUESTS=500000` for the larger fixture. A comma-separated value runs both sizes. `CPAUK_BENCH_REBUILD=1` creates a fresh fixture; `0` reuses an existing fixture and its `seed-metadata.json`.

The fixture spans 90 UTC days. Every request has a deterministic model/key distribution and 9% retry attempts; failed attempts carry a 1 KiB error body and successful attempts carry diagnostic usage JSON. The report includes event, aggregate, and request identity row counts, SQLite database plus sidecar bytes, ingestion time, and warm/fresh-SQLite timings for summary, 15m/30m/1h timeseries, activity, analysis, costs, latency, models, dimensions, and leaderboard at 1h, 6h, 30d, and 90d.

The cold query closes and reopens SQLite before each sample, then measures only the query. The report also records the reopen duration and their sum. This is a fresh SQLite connection with the operating system page cache left intact, so it is useful for connection/cache behavior and should not be described as a guaranteed cold disk read.

Compare two JSON artifacts after a redesign:

```bash
test/perf/compare_cpauk_compact_benchmark.py \
  /tmp/cpauk-compact-benchmark/baseline-results.json \
  /tmp/cpauk-compact-benchmark/compact-results.json \
  > /tmp/cpauk-compact-benchmark/comparison.md
```

For a quick focused run while iterating, use `CPAUK_BENCH_OPERATIONS=summary,timeseries_15m` and `CPAUK_BENCH_WINDOWS=1h,90d`. The full command intentionally runs serially so one operation never overlaps another.
