#!/usr/bin/env python3
"""Compare frozen directory-materialization implementations on a native filesystem."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import platform
import time

from benchmark_snapshot_integration import (
    benchmark_campaign, benchmark_cases, benchmark_order, filesystem,
    parse_sample, run, source_digest,
)
from snapshot_provenance import comparison_inputs, harness_inputs, require_matching_inputs


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--baseline", type=Path, required=True)
    parser.add_argument("--candidate", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--rounds", type=int, default=7)
    parser.add_argument("--iterations", type=int, default=7)
    parser.add_argument("--capture-only", action="store_true")
    args = parser.parse_args()
    if args.rounds < 1 or args.iterations < 1:
        parser.error("rounds and iterations must be positive")
    roots = {"baseline": args.baseline.resolve(), "candidate": args.candidate.resolve()}
    out = args.output.resolve()
    if any(out.is_relative_to(root) for root in roots.values()):
        parser.error("output must be outside both source trees")
    out.mkdir(parents=True, exist_ok=False)
    cases = benchmark_cases("initialization")
    if args.capture_only:
        cases = {name: case for name, case in cases.items() if name.startswith("capture-")}
    cases = {name: (package, pattern, f"{args.iterations}x" if name.startswith("capture-") else duration)
             for name, (package, pattern, duration) in cases.items()}
    env = dict(os.environ, GOMAXPROCS="2", CGO_ENABLED="0")
    scope = "Native capture only. " if args.capture_only else "Native capture and loopback commands. "
    report = dict(platform=platform.platform(), machine=platform.machine(), gomaxprocs=2,
                  rounds=args.rounds, cases=cases,
                  versions={}, orders=[], toolchains={},
                  scope=scope + "Capture retains outputs until sample end; "
                        "fixture writes are flushed before timing. sync runs between processes; "
                        "this does not force cold caches or exclude unrelated host I/O.",
                  python=platform.python_version())
    harness_root = Path(__file__).resolve().parent
    with benchmark_campaign(out, report) as scratch:
        inputs = {name: comparison_inputs(root) for name, root in roots.items()}
        report["comparison_inputs"] = inputs
        require_matching_inputs(inputs, [])
        report["harness_inputs"] = harness_inputs(harness_root)
        env["TMPDIR"] = str(scratch)
        report["fixture_filesystem"] = filesystem(scratch, env, out, "filesystem")
        mount_command = (["findmnt", "-T", str(scratch), "-n", "-o", "FSTYPE,OPTIONS"]
                         if platform.system() == "Linux" else ["mount"])
        report["fixture_mount"] = run(mount_command, scratch, env, out / "fixture-mount.txt").strip()
        for name, root in roots.items():
            directory = out / name
            directory.mkdir()
            report["versions"][name] = dict(source_sha256=source_digest(root),
                production_sha256=source_digest(root, production=True), samples=[], binaries={})
            report["toolchains"][name] = json.loads(run(
                ["go", "env", "-json", "GOVERSION", "GOOS", "GOARCH", "CGO_ENABLED", "GOFLAGS", "GOTOOLCHAIN", "GOEXPERIMENT"],
                root, env, directory / "toolchain.json"))
            # An experimental scheduling change is screened against the complete
            # changes suite and its races before timing. Full repository checks
            # are a separate adoption gate, not implied by this screen.
            run(["go", "test", "-timeout=10m", "./internal/changes"], root, env, directory / "tests.txt")
            run(["go", "test", "-race", "-timeout=10m", "./internal/changes"], root,
                dict(env, CGO_ENABLED="1"), directory / "race.txt")
            run(["go", "vet", "./internal/changes"], root, env, directory / "vet.txt")
            for package in sorted({case[0] for case in cases.values()}):
                key = package.removeprefix("./").replace("/", "-")
                binary = scratch / f"{name}-{key}.test"
                run(["go", "test", "-c", "-o", str(binary), package], root, env, directory / f"build-{key}.txt")
                report["versions"][name]["binaries"][key] = hashlib.sha256(binary.read_bytes()).hexdigest()
            print(f"{name}: changes tests, race and vet passed", flush=True)
        if report["toolchains"]["baseline"] != report["toolchains"]["candidate"]:
            raise RuntimeError("toolchains differ")
        for number in range(args.rounds):
            order = benchmark_order(roots, number)
            report["orders"].append(order)
            for case, (package, pattern, duration) in cases.items():
                key = package.removeprefix("./").replace("/", "-")
                for name in order:
                    prefix = out / name / f"{number}-{case}"
                    report["active"] = dict(round=number, case=case, variant=name)
                    (out / "report.json").write_text(json.dumps(report, indent=2) + "\n")
                    run(["sync"], roots[name], env, prefix.with_suffix(".sync.txt"))
                    started, load = time.monotonic(), os.getloadavg()
                    raw = run([str(scratch / f"{name}-{key}.test"), "-test.run=^$",
                               f"-test.bench={pattern}", f"-test.benchtime={duration}", "-test.benchmem"],
                              roots[name], env, prefix.with_suffix(".txt"), timeout=300)
                    sample = parse_sample(raw, number)
                    sample.update(case=case, elapsed=time.monotonic()-started,
                                  load_before=load, load_after=os.getloadavg())
                    report["versions"][name]["samples"].append(sample)
                    report.pop("active", None)
                    (out / "report.json").write_text(json.dumps(report, indent=2) + "\n")
                print(f"Pair {number+1}/{args.rounds}: {case}", flush=True)
        # Profiles are separate from timing and retain both variants. Mutex delay
        # is aggregate blocked time, not a wall-time component to add to fsync.
        for name, root in roots.items():
            binary = scratch / f"{name}-internal-changes.test"
            for shape in ("32-deep", "8-deep-wide"):
                prefix = out / name / f"profile-{shape}"
                mutex, trace = scratch / f"{name}-{shape}.mutex", scratch / f"{name}-{shape}.trace"
                run([str(binary), "-test.run=^$", f"-test.bench=^BenchmarkCaptureWorkspaceBase$/^{shape}$",
                     "-test.benchtime=3x", f"-test.mutexprofile={mutex}", "-test.mutexprofilefraction=1",
                     f"-test.trace={trace}"], root, env, prefix.with_suffix(".txt"), timeout=300)
                run(["go", "tool", "pprof", "-top", "-cum", str(binary), str(mutex)],
                    root, env, prefix.with_suffix(".mutex.txt"))
                syscall = scratch / f"{name}-{shape}.syscall"
                run(["go", "tool", "trace", "-pprof=syscall", str(trace)], root, env, syscall, read_output=False)
                run(["go", "tool", "pprof", "-top", "-cum", str(binary), str(syscall)],
                    root, env, prefix.with_suffix(".syscall.txt"))
                # Preserve compact raw profiles for attribution beyond top tables.
                prefix.with_suffix(".mutex").write_bytes(mutex.read_bytes())
                prefix.with_suffix(".syscall").write_bytes(syscall.read_bytes())
        for name, root in roots.items():
            if source_digest(root) != report["versions"][name]["source_sha256"] or comparison_inputs(root) != inputs[name]:
                raise RuntimeError(f"{name} source changed")
        if harness_inputs(harness_root) != report["harness_inputs"]:
            raise RuntimeError("harness changed")


if __name__ == "__main__":
    main()
