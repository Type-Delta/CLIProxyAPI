#!/usr/bin/env python3
"""Compare two CPAUK compact benchmark JSON artifacts.

Usage:
    compare_cpauk_compact_benchmark.py baseline-results.json compact-results.json

The script only reads the artifacts emitted by the Go harness, so it can be
used after a redesign without changing either implementation. It prints a
small Markdown report with storage, ingestion, and per-operation latency
deltas. Positive latency percentages mean the second run is slower.
"""

from __future__ import annotations

import json
import sys
from pathlib import Path


def load(path: str) -> dict:
    with Path(path).open(encoding="utf-8") as stream:
        value = json.load(stream)
    if value.get("schema") != 1:
        raise ValueError(f"{path}: unsupported benchmark schema {value.get('schema')!r}")
    return value


def pct(before: float, after: float) -> str:
    if before == 0:
        return "n/a"
    return f"{(after - before) * 100 / before:+.1f}%"


def ns(value: float) -> str:
    if value == 0:
        return "-"
    if value < 1_000:
        return f"{value:.0f} ns"
    if value < 1_000_000:
        return f"{value / 1_000:.2f} µs"
    if value < 1_000_000_000:
        return f"{value / 1_000_000:.2f} ms"
    return f"{value / 1_000_000_000:.2f} s"


def main(argv: list[str]) -> int:
    if len(argv) != 3:
        print(f"usage: {argv[0]} baseline-results.json compact-results.json", file=sys.stderr)
        return 2
    baseline = load(argv[1])
    compact = load(argv[2])
    before_runs = {run["requests"]: run for run in baseline.get("runs", [])}
    after_runs = {run["requests"]: run for run in compact.get("runs", [])}
    print("# CPAUK compact benchmark comparison")
    print()
    print(f"Baseline revision: `{baseline.get('git_revision', '')}`")
    print(f"Compact revision: `{compact.get('git_revision', '')}`")
    print()
    print("| Requests | Metric | Baseline | Compact | Delta |")
    print("| ---: | --- | ---: | ---: | ---: |")
    for requests in sorted(set(before_runs) & set(after_runs)):
        before = before_runs[requests]
        after = after_runs[requests]
        for metric, label in (("ingest_ns", "ingest"), ("database_bytes", "database bytes")):
            left = before.get(metric, 0)
            right = after.get(metric, 0)
            delta = pct(left, right) if metric.endswith("ns") or metric.endswith("bytes") else "n/a"
            left_text = ns(left) if metric == "ingest_ns" else f"{left:,}"
            right_text = ns(right) if metric == "ingest_ns" else f"{right:,}"
            print(f"| {requests} | {label} | {left_text} | {right_text} | {delta} |")
        before_rows = before.get("rows", {})
        after_rows = after.get("rows", {})
        for table in sorted(set(before_rows) | set(after_rows)):
            left, right = before_rows.get(table, 0), after_rows.get(table, 0)
            if left == right:
                continue
            print(f"| {requests} | rows `{table}` | {left:,} | {right:,} | {right - left:+,} |")
        before_queries = {(item["window"], item["name"]): item for item in before.get("queries", [])}
        after_queries = {(item["window"], item["name"]): item for item in after.get("queries", [])}
        for key in sorted(set(before_queries) & set(after_queries)):
            left, right = before_queries[key], after_queries[key]
            print(f"| {requests} | warm `{key[0]}/{key[1]}` | {ns(left.get('warm_median_ns', 0))} | {ns(right.get('warm_median_ns', 0))} | {pct(left.get('warm_median_ns', 0), right.get('warm_median_ns', 0))} |")
            print(f"| {requests} | cold query `{key[0]}/{key[1]}` | {ns(left.get('cold_median_ns', 0))} | {ns(right.get('cold_median_ns', 0))} | {pct(left.get('cold_median_ns', 0), right.get('cold_median_ns', 0))} |")
            print(f"| {requests} | cold reopen `{key[0]}/{key[1]}` | {ns(left.get('cold_open_median_ns', 0))} | {ns(right.get('cold_open_median_ns', 0))} | {pct(left.get('cold_open_median_ns', 0), right.get('cold_open_median_ns', 0))} |")
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv))
