#!/usr/bin/env python3
"""Recompute paired journal comparisons from retained samples, without rerunning them."""
import argparse
import hashlib
import json
from pathlib import Path

from benchmark_derived_index import summarize
from benchmark_observation_journal import VARIANTS


def analyze(report):
    samples = report["samples"]
    summaries = {baseline: summarize(samples, VARIANTS, baseline_variant=baseline)
                 for baseline in ("checkpoint", "replacement")}
    histories = []
    for fixture, scenario in sorted({(s["fixture"], s["scenario"]) for s in samples
                                     if s["scenario"] in ("edit", "batch")}):
        group = sorted((s for s in samples if (s["fixture"], s["scenario"], s["variant"]) ==
                        (fixture, scenario, "observation-journal")), key=lambda s: s["round"])
        histories.append(dict(fixture=fixture, scenario=scenario,
                              rounds=[s["round"] for s in group],
                              loaded_records=[s["Result"]["JournalRecordsLoaded"] for s in group],
                              positions=[s["position"] for s in group]))
    return dict(summaries=summaries, ordinary_histories=histories)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("report", type=Path)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    raw = args.report.read_bytes()
    result = analyze(json.loads(raw))
    result["report_sha256"] = hashlib.sha256(raw).hexdigest()
    directory = Path(__file__).resolve().parent
    result["analysis_inputs"] = {name: hashlib.sha256((directory/name).read_bytes()).hexdigest()
                                 for name in ("summarize_observation_journal.py", "benchmark_derived_index.py",
                                              "benchmark_observation_journal.py")}
    # Never overwrite a report or an earlier analysis artifact.
    with args.output.open("x") as output:
        output.write(json.dumps(result, indent=2)+"\n")


if __name__ == "__main__":
    main()
