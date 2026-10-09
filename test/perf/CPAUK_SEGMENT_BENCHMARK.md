# CPAUK cache and sealed-segment benchmark

This opt-in benchmark measures historical result cache cold and hit calls, a
current tail write followed by the same historical query, a late historical
write invalidation, and an unsealed/sealed summary differential. It reports
deterministic output counts, database bytes, and nanosecond timings in JSON.

Seed a fresh 100,000-request fixture first:

```bash
CPAUK_BENCH_DIR=/tmp/cpauk-segment-benchmark/fixture \
CPAUK_BENCH_REQUESTS=100000 CPAUK_BENCH_REPETITIONS=3 \
CPAUK_BENCH_LABEL=fixture CPAUK_BENCH_REBUILD=1 \
test/perf/run_cpauk_compact_benchmark.sh
```

Run the scenarios serially. The wrapper copies the source fixture to a
labelled run directory so tail and late writes do not alter the seed used for
another comparison:

```bash
CPAUK_BENCH_DIR=/tmp/cpauk-segment-benchmark/fixture/requests-100000 \
CPAUK_SEGMENT_BENCH_DIR=/tmp/cpauk-segment-benchmark \
CPAUK_BENCH_LABEL=run-a test/perf/run_cpauk_segment_benchmark.sh
```

The wrapper refuses to overwrite an existing labelled run directory; choose a
new `CPAUK_BENCH_LABEL` for each run. The JSON `scenarios` labels identify `cache_cold`,
`tail_write_history`, `late_write_history`, `unsealed_summary`, and
`sealed_summary`. `duration_ns` measures each query call. The fixture remains
deterministic (seed `20261007`, fixed end `2026-10-07T00:00:00Z`); copy it to a
new directory before each run. Database bytes and row counts provide the
storage and ingestion footprint. Sealing is recorded as unavailable when the
store API is absent, allowing the same benchmark file to run during staged
rollouts.
