#!/usr/bin/env python3
"""Summarize adjacent snapshot comparisons without hiding noisy samples."""

import argparse
import json
import math
from pathlib import Path
import statistics


def median_interval(values):
    """Distribution-free median interval, assuming independent paired ratios.

    Choose the tightest order-statistic interval with at least 95% coverage.
    Seven pairs permit only [min, max], with 98.4375% coverage. Insufficient
    sample counts remain unclassified rather than fabricating precision.
    """
    values = sorted(values)
    n = len(values)
    chosen = None
    for k in range(1, (n + 1) // 2 + 1):
        coverage = 1 - 2 * sum(math.comb(n, j) for j in range(k)) / 2**n
        if coverage >= 0.95:
            chosen = {"bounds": [values[k - 1], values[n - k]], "coverage": coverage}
    return chosen


def compare(control, candidate):
    left = {row["round"]: row["metrics"]["ns/op"] for row in control}
    right = {row["round"]: row["metrics"]["ns/op"] for row in candidate}
    if len(left) != len(control) or len(right) != len(candidate) or left.keys() != right.keys() or not left:
        raise ValueError("comparison requires unique matching rounds")
    if any(not math.isfinite(v) or v <= 0 for v in [*left.values(), *right.values()]):
        raise ValueError("timings must be positive and finite")
    ratios = [right[r] / left[r] for r in sorted(left)]
    interval = median_interval(ratios)
    bounds = interval["bounds"] if interval else None
    return {
        "control_ms": statistics.median(left.values()) / 1e6,
        "candidate_ms": statistics.median(right.values()) / 1e6,
        "paired_ratios": ratios,
        "median_ratio": statistics.median(ratios),
        "median_interval": interval,
        "wins": sum(r < 1 for r in ratios),
        "classification": "faster" if bounds and bounds[1] < 1 else "slower" if bounds and bounds[0] > 1 else "inconclusive",
        "watch_target_met": bool(bounds and bounds[1] < 1 and statistics.median(ratios) <= 0.9),
        "regression_above_five_percent": bool(bounds and bounds[0] > 1.05),
    }


def summarize(report):
    samples = {variant: data["samples"] for variant, data in report["versions"].items()}
    names = {variant: {row["name"] for row in rows} for variant, rows in samples.items()}
    if any(value != names["candidate"] for value in names.values()):
        raise ValueError("variants have different workload sets")
    result = {}
    for control in samples.keys() - {"candidate"}:
        result[control] = {}
        for name in sorted(names["candidate"]):
            result[control][name] = compare(
                [row for row in samples[control] if row["name"] == name],
                [row for row in samples["candidate"] if row["name"] == name],
            )
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--host", action="append", required=True, help="NAME=results directory")
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    combined = {}
    for host in args.host:
        name, directory = host.split("=", 1)
        root = Path(directory)
        report = json.loads((root / "report.json").read_text())
        rows = summarize(report)
        combined[name] = {"report": report, "comparisons": rows,
                          "logs": {str(p.relative_to(root)): p.read_text() for p in root.rglob("*.txt")}}
        for control, workloads in rows.items():
            for workload, row in workloads.items():
                print(f"{name} vs {control}: {workload}: {row['control_ms']:.3f} -> {row['candidate_ms']:.3f} ms; "
                      f"ratio {row['median_ratio']:.3f}, {row['classification']}, {row['wins']}/{len(row['paired_ratios'])} wins")
    args.output.write_text(json.dumps(combined, indent=2) + "\n")


if __name__ == "__main__":
    main()
