#!/usr/bin/env python3
"""Short native-host diagnostics, including work outside the benchmark timer."""

import argparse
import json
import os
from pathlib import Path
import platform
import time

from benchmark_snapshot_integration import benchmark_campaign, filesystem, parse_sample, run, source_digest


CASES = {"push": "^BenchmarkPushPhases$", "fetch-large": "^BenchmarkFetchBodies$/^large$"}


def phase_events(raw):
    return [json.loads(line.split("BENCH_PHASE ", 1)[1]) for line in raw.splitlines()
            if "BENCH_PHASE " in line]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--case", choices=CASES, action="append")
    parser.add_argument("--rounds", type=int, default=1)
    parser.add_argument("--timeout", type=float, default=90)
    parser.add_argument("--trace", action="store_true", help="Collect diagnostic syscall/CPU profiles; timings include profiling overhead")
    args = parser.parse_args()
    if args.rounds < 1 or args.timeout <= 0:
        parser.error("rounds and timeout must be positive")
    root, output = Path.cwd(), args.output.resolve()
    output.mkdir(parents=True, exist_ok=False)
    env = dict(os.environ, GOMAXPROCS="2", CGO_ENABLED="0")
    report = dict(platform=platform.platform(), source_sha256=source_digest(root),
                  production_sha256=source_digest(root, production=True),
                  traced=args.trace, observations=[], status="running",
                  scope="Diagnostic native-host loopback runs, one operation per process; not a paired performance comparison")
    with benchmark_campaign(output, report) as scratch:
        fixtures = scratch / "fixtures"
        fixtures.mkdir()
        env["TMPDIR"] = str(fixtures)
        report["filesystem"] = filesystem(fixtures, env, output, "filesystem")
        binary = scratch / "errand.test"
        run(["go", "test", "-c", "-o", str(binary), "./cmd/errand"], root, env, output / "build.txt")
        for number in range(args.rounds):
            for case in args.case or CASES:
                prefix = f"{number}-{case}"
                log = output / f"{prefix}.txt"
                trace, cpu = scratch / f"{prefix}.trace", scratch / f"{prefix}.cpu"
                command = [str(binary), "-test.run=^$", f"-test.bench={CASES[case]}", "-test.benchtime=1x"]
                if args.trace:
                    command += [f"-test.trace={trace}", f"-test.cpuprofile={cpu}"]
                row = dict(case=case, round=number, load_before=os.getloadavg())
                started = time.monotonic()
                try:
                    raw = run(command, root, env, log, timeout=args.timeout)
                    row["sample"] = parse_sample(raw, number)
                except RuntimeError as exc:
                    row["error"] = str(exc)
                    raw = log.read_text()
                row.update(elapsed=time.monotonic()-started, load_after=os.getloadavg(), phases=phase_events(raw))
                report["observations"].append(row)
                (output / "report.json").write_text(json.dumps(report, indent=2) + "\n")
                print(f"{prefix}: {row['elapsed']:.2f}s, {'failed' if 'error' in row else 'completed'}", flush=True)
                if args.trace and "error" not in row:
                    syscall = scratch / f"{prefix}.syscall"
                    run(["go", "tool", "trace", "-pprof=syscall", str(trace)], root, env, syscall, timeout=30, read_output=False)
                    for kind, profile in (("syscall", syscall), ("cpu", cpu)):
                        run(["go", "tool", "pprof", "-top", "-cum", str(binary), str(profile)], root, env, output / f"{prefix}-{kind}.txt", timeout=30)
        if source_digest(root) != report["source_sha256"]:
            raise RuntimeError("source changed during diagnostic run")
        if any("error" in row for row in report["observations"]):
            raise SystemExit(1)


if __name__ == "__main__":
    main()
