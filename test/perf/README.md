# Analytics load profile

`analytics_load_test.go` freezes the WP0 load contract. The fast contract tests check the traffic mix, mode order, duration, request count, retry behavior, deterministic upstream, and five-run aggregation. The long test reports an explicit skip unless `CPA_ANALYTICS_LOAD_PROFILE=1` is set.

The required command is:

```bash
go test -run TestAnalyticsLoadProfile -count=5 ./test/perf
```

Set `CPA_ANALYTICS_LOAD_PROFILE=1` to run the full profile. `analytics_load_adapter_test.go` composes CPA's production usage manager, CPAUK service, collector, and SQLite store in process. It routes all four traffic classes to the deterministic upstream, reports attempts and queue metrics, and shuts the components down in lifecycle order. SQLite-blocked mode stalls the real collector writer. Both-queues-saturated mode also stalls a generic observer lane.

Set `CPA_PERF_RUNNER_CLASS` to the stable CI runner class. Each `ANALYTICS_LOAD_RUN` JSON line records that value, Go and OS details, CPU count and model, RAM, and the CPU power mode. The full profile refuses to run when any required metadata is missing. Linux hosts are detected through `/proc`, `/etc/os-release`, and sysfs. Other dedicated runners can set `CPA_PERF_OS`, `CPA_PERF_CPU_MODEL`, `CPA_PERF_RAM_BYTES`, and `CPA_PERF_POWER_MODE` explicitly. Keep the five run lines in one file, then set `CPA_ANALYTICS_LOAD_RESULTS_FILE` to that file and run the required command again. The recorded gate computes the disabled and healthy median-of-five p99 and checks the blocked and saturated queue-wait, dispatch-lag, heap, and goroutine limits.

## Observed runner metadata

Observed on 2026-08-31 in the current checkout:

| Field | Value |
| --- | --- |
| Runner class | `local-reference-i7-7700` |
| OS | Debian GNU/Linux 13.6, Linux 6.1.0-28-amd64 |
| Go | go1.27.0 linux/amd64 |
| CPU | Intel Core i7-7700 at 3.60 GHz, 4 cores, 8 threads |
| RAM | 16,711,749,632 bytes, about 15.56 GiB |
| Power mode | `powersave` scaling governor |

This machine is reference metadata, not yet a certified dedicated CI runner. Record the chosen dedicated runner under one stable `CPA_PERF_RUNNER_CLASS` value before using results as release evidence. The full performance gate remains open until the production adapter exists and five runs pass on that dedicated runner.

## Full run

Run:

```bash
CPA_ANALYTICS_LOAD_PROFILE=1 \
CPA_PERF_RUNNER_CLASS=local-reference-i7-7700 \
go test -v -run TestAnalyticsLoadProfile -count=5 ./test/perf | tee analytics-load-runs.log
```

Feed those five records back through the gate with `CPA_ANALYTICS_LOAD_RESULTS_FILE=analytics-load-runs.log go test -run TestAnalyticsLoadProfileRecordedGate -count=1 ./test/perf`.

One test invocation runs disabled, healthy, SQLite-blocked, and both-queues-saturated modes. Each mode uses 30 seconds of warmup followed by five minutes at 1,000 completed client requests per second across 64 clients. Every mode completes 300,000 measured client requests with a 60 percent JSON, 25 percent SSE, 10 percent WebSocket, and 5 percent fail-then-retry mix.

The result captures client p99, scheduler lag, live heap after forced GC, goroutine maximum and final two-minute slope, goroutines after shutdown, shutdown duration, queue depth and loss, and request-path queue waiting. The gate requires at least 1,000 completed measured requests per second and at least 300,000 requests. It rejects queue waiting, blocked or saturated dispatch lag more than 1 ms above disabled, a healthy median p99 increase of 1 ms or more, blocked or saturated live-heap growth above 160 MiB, excess workers, a positive final goroutine slope, or failure to return within two goroutines of the disabled shutdown baseline.
