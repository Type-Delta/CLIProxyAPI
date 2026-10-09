#!/usr/bin/env bash
set -euo pipefail

# Run the opt-in CPAUK storage benchmark and leave JSON, Markdown, and the
# complete `go test` log together. The generated dataset is retained so a
# follow-up implementation can be measured with CPAUK_BENCH_REBUILD=0.
repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
output_dir=${CPAUK_BENCH_DIR:-${TMPDIR:-/tmp}/cpauk-compact-benchmark}
requests=${CPAUK_BENCH_REQUESTS:-100000}
repetitions=${CPAUK_BENCH_REPETITIONS:-3}
label=${CPAUK_BENCH_LABEL:-baseline}
rebuild=${CPAUK_BENCH_REBUILD:-1}
operations=${CPAUK_BENCH_OPERATIONS:-}
windows=${CPAUK_BENCH_WINDOWS:-}

mkdir -p "$output_dir"
output_json=${CPAUK_BENCH_OUTPUT:-$output_dir/${label}-results.json}
output_report=${CPAUK_BENCH_REPORT:-$output_dir/${label}-report.md}
run_log=${CPAUK_BENCH_LOG:-$output_dir/${label}-run.log}

cd "$repo_root"
command_description="CPAUK_COMPACT_BENCHMARK=1 CPAUK_BENCH_REQUESTS=$requests CPAUK_BENCH_REPETITIONS=$repetitions CPAUK_BENCH_LABEL=$label CPAUK_BENCH_REBUILD=$rebuild CPAUK_BENCH_OPERATIONS=$operations CPAUK_BENCH_WINDOWS=$windows test/perf/run_cpauk_compact_benchmark.sh"
{
	echo "# CPAUK compact benchmark"
	echo "# started: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
	echo "# repo: $repo_root"
	echo "# output: $output_dir"
	echo "# command: $command_description"
	echo
	CPAUK_COMPACT_BENCHMARK=1 \
	CPAUK_BENCH_DIR="$output_dir" \
	CPAUK_BENCH_OUTPUT="$output_json" \
	CPAUK_BENCH_REPORT="$output_report" \
	CPAUK_BENCH_REQUESTS="$requests" \
	CPAUK_BENCH_REPETITIONS="$repetitions" \
	CPAUK_BENCH_LABEL="$label" \
	CPAUK_BENCH_REBUILD="$rebuild" \
	CPAUK_BENCH_COMMAND="$command_description" \
	go test -run '^TestCPAUKCompactBenchmark$' -count=1 ./internal/cpauk/store
} 2>&1 | tee "$run_log"

echo "JSON:   $output_json"
echo "Report: $output_report"
echo "Log:    $run_log"
