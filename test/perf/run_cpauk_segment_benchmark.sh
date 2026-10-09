#!/usr/bin/env bash
set -euo pipefail

# Run the cache and sealed-segment scenarios against an existing compact fixture.
repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
output_dir=${CPAUK_SEGMENT_BENCH_DIR:-${TMPDIR:-/tmp}/cpauk-segment-benchmark}
fixture_dir=${CPAUK_BENCH_DIR:-$output_dir/requests-100000}
label=${CPAUK_BENCH_LABEL:-segment}
output=${CPAUK_SEGMENT_BENCH_OUTPUT:-$output_dir/${label}-segment-results.json}
log=${CPAUK_SEGMENT_BENCH_LOG:-$output_dir/${label}-segment-run.log}
run_fixture=$output_dir/fixture-$label

if [[ ! -f "$fixture_dir/analytics.db" ]]; then
  echo "missing fixture: $fixture_dir/analytics.db" >&2
  echo "seed one with test/perf/run_cpauk_compact_benchmark.sh first" >&2
  exit 1
fi
mkdir -p "$output_dir"
if [[ -e "$run_fixture" ]]; then
  echo "run fixture already exists: $run_fixture (choose a new label)" >&2
  exit 1
fi
cp -a "$fixture_dir" "$run_fixture"
cd "$repo_root"
{
  echo "# CPAUK segment benchmark"
  echo "# started: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "# source fixture: $fixture_dir"
  echo "# run fixture: $run_fixture"
  CPAUK_SEGMENT_BENCHMARK=1 CPAUK_BENCH_DIR="$run_fixture" CPAUK_BENCH_LABEL="$label" CPAUK_SEGMENT_BENCH_OUTPUT="$output" \
    go test -run '^TestCPAUKSegmentBenchmark$' -count=1 ./internal/cpauk/store
} 2>&1 | tee "$log"
echo "JSON: $output"
echo "Log: $log"
