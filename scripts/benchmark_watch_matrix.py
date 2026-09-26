#!/usr/bin/env python3
"""Run the P2 realistic-watch matrix and summarize it.

Each round runs every size x case once, alternating forward and reverse order.
Competitors (rsync, optional Mutagen) run in the first round only. Summaries
report medians across rounds plus watch preparation modes and fallback reasons.
With --baseline, every size x case runs both binaries back to back, alternating
which goes first from round to round, and the summary adds paired ratios.
"""

import argparse
import collections
import itertools
import json
from pathlib import Path
import statistics
import subprocess
import sys
import time

CASES = {
    "explicit-inplace-edit": [],
    "explicit-atomic-edit": ["--save-mode", "atomic"],
    "explicit-inplace-create": ["--change", "create"],
    "explicit-inplace-delete": ["--change", "delete"],
    "git-untracked-inplace-edit": ["--selection", "git"],
    "git-tracked-inplace-edit": ["--selection", "git", "--git-tracking", "all"],
    "git-mixed-inplace-edit": ["--selection", "git", "--git-tracking", "mixed"],
    "git-tracked-atomic-edit": ["--selection", "git", "--git-tracking", "all", "--save-mode", "atomic"],
    "git-tracked-inplace-create": ["--selection", "git", "--git-tracking", "all", "--change", "create"],
    "git-tracked-inplace-delete": ["--selection", "git", "--git-tracking", "all", "--change", "delete"],
}


def report_complete(target):
    try:
        return json.loads((target / "report.json").read_text()).get("complete") is True
    except (OSError, ValueError):
        return False


def run_matrix(args):
    args.output.mkdir(parents=True, exist_ok=True)
    matrix = list(itertools.product(args.sizes, args.cases))
    script = Path(__file__).with_name("benchmark_watch.py")
    variants = [("", args.binary)] if not args.baseline else [("@baseline", args.baseline), ("@candidate", args.binary)]
    # Mutagen runs once per size x case, beside the candidate when paired.
    competitor = variants[-1][0]
    cells = list(enumerate(matrix))
    for r in range(args.rounds):
        for index, (files, name) in cells if r % 2 == 0 else cells[::-1]:
            # Order by the cell's fixed index, not its position in this round's
            # traversal: reversing an even-sized matrix preserves position
            # parity, so each cell would run its variants in one order always.
            order = variants if (r + index) % 2 == 0 else variants[::-1]
            for label, binary in order:
                target = args.output / f"r{r}-{files}-{name}{label}"
                if report_complete(target):
                    continue
                if target.exists():
                    # Rerun a failed cell. Keep the attempt for diagnosis, outside
                    # the directories summarize reads.
                    aside = args.output / "incomplete"
                    aside.mkdir(exist_ok=True)
                    target.rename(aside / f"{target.name}.{time.time_ns()}")
                cmd = [sys.executable, str(script), "--binary", binary, "--files", str(files),
                       "--samples", str(args.samples), "--idle-seconds", "2", "--skip-once", "--trace",
                       "--pause-seconds", str(args.pause_seconds), "--output", str(target), *CASES[name]]
                if args.mutagen and r == 0 and label == competitor:
                    cmd += ["--mutagen", args.mutagen]
                started = time.monotonic()
                proc = subprocess.run(cmd, capture_output=True, text=True)
                print(json.dumps(dict(round=r, files=files, case=name + label, code=proc.returncode,
                                      seconds=round(time.monotonic()-started, 1),
                                      error=proc.stderr[-400:] if proc.returncode else "")), flush=True)


def summarize(root):
    groups = collections.defaultdict(lambda: dict(watch=[], receipt=[], mutagen=[], rsync=[], full=[], incremental=[],
                                                  reasons=collections.Counter(), incomplete=0))
    rounds = collections.defaultdict(dict)  # (files, case) -> {(round, variant): median}
    for path in sorted(root.glob("r*-*/report.json")):
        round_label, files, case = path.parent.name.split("-", 2)
        base, _, variant = case.partition("@")
        report = json.loads(path.read_text())
        g = groups[(int(files), case)]
        if not report.get("complete"):
            g["incomplete"] += 1
            continue
        watch = [s["delivery_seconds"]*1000 for s in report["samples"] if s["mode"] == "watch"]
        if variant and watch:
            rounds[(int(files), base)][(round_label, variant)] = statistics.median(watch)
        for s in report["samples"]:
            if s["mode"] == "watch":
                g["watch"].append(s["delivery_seconds"]*1000)
                g["receipt"].append(s["seconds"]*1000)
            elif s["mode"] == "mutagen":
                g["mutagen"].append(s["delivery_seconds"]*1000)
            elif s["mode"] == "rsync-checksum":
                g["rsync"].append(s["seconds"]*1000)
        for p in report.get("watch_trace", {}).get("preparations", []):
            if p["reason"] != "first":
                g["reasons"][p["reason"]] += 1
                g[p["mode"]].append(p["elapsed_ms"])

    def median(values):
        return f"{statistics.median(values):.0f}" if values else "–"

    lines = ["| Files | Case | Errand visible, median (range) | Errand receipt | Mutagen visible | rsync -c | "
             "Prep full / incremental | Prep reasons | n |", "|---:|---|---:|---:|---:|---:|---:|---|---:|"]
    for (files, case), g in sorted(groups.items()):
        spread = f"{min(g['watch']):.0f}–{max(g['watch']):.0f}" if g["watch"] else "–"
        reasons = ", ".join(f"{k} {v}" for k, v in g["reasons"].most_common())
        incomplete = f" ({g['incomplete']} incomplete)" if g["incomplete"] else ""
        lines.append(f"| {files:,} | {case} | {median(g['watch'])} ({spread}) | {median(g['receipt'])} | "
                     f"{median(g['mutagen'])} | {median(g['rsync'])} | {median(g['full'])} / {median(g['incremental'])} | "
                     f"{reasons} | {len(g['watch'])}{incomplete} |")
    if rounds:
        lines += ["", "| Files | Case | Baseline median | Candidate median | Paired ratio, median (range) | Candidate faster |",
                  "|---:|---|---:|---:|---:|---:|"]
        for (files, case), values in sorted(rounds.items()):
            labels = sorted({r for r, _ in values})
            pairs = [(values[(r, "baseline")], values[(r, "candidate")]) for r in labels
                     if (r, "baseline") in values and (r, "candidate") in values]
            if not pairs:
                continue
            ratios = [c / b for b, c in pairs]
            lines.append(f"| {files:,} | {case} | {statistics.median(b for b, _ in pairs):.0f} | "
                         f"{statistics.median(c for _, c in pairs):.0f} | {statistics.median(ratios):.3f} "
                         f"({min(ratios):.3f}–{max(ratios):.3f}) | {sum(r < 1 for r in ratios)}/{len(ratios)} |")
    return "\n".join(lines)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", required=True, type=Path)
    parser.add_argument("--binary", help="Errand binary; omit with --summarize-only")
    parser.add_argument("--mutagen", help="optional Mutagen binary")
    parser.add_argument("--baseline", help="baseline Errand binary for a paired comparison")
    parser.add_argument("--rounds", type=int, default=2)
    parser.add_argument("--samples", type=int, default=7)
    parser.add_argument("--pause-seconds", type=float, default=0, help="idle interval before each measured watch save")
    parser.add_argument("--sizes", type=int, nargs="+", default=[1000, 10000])
    parser.add_argument("--cases", nargs="+", choices=sorted(CASES), default=list(CASES))
    parser.add_argument("--summarize-only", action="store_true")
    args = parser.parse_args()
    if not args.summarize_only:
        if not args.binary:
            parser.error("--binary is required unless --summarize-only")
        args.binary = str(Path(args.binary).resolve())
        if args.baseline:
            args.baseline = str(Path(args.baseline).resolve())
        run_matrix(args)
    summary = summarize(args.output)
    (args.output / "summary.md").write_text(summary + "\n")
    print(summary)


if __name__ == "__main__":
    main()
