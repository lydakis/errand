#!/usr/bin/env python3
"""Reproduce the isolated snapshot metadata comparison on a trusted runner."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import platform
import plistlib
import re
import subprocess
import time


SAMPLE = re.compile(r"^(BenchmarkIndex/\S+)-\d+\s+(\d+)\s+([\d.]+) ns/op\s+(\d+) B/op\s+(\d+) allocs/op$", re.MULTILINE)


def run(command, log, env, root):
    started = time.monotonic()
    result = subprocess.run(command, cwd=root, env=env, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
    log.write_text(result.stdout)
    if result.returncode:
        raise RuntimeError(f"{command[0]} failed ({result.returncode}); see {log}")
    return result.stdout, time.monotonic() - started


def prepare(root, output, env, revision):
    output.mkdir(parents=True, exist_ok=False)
    source = hashlib.sha256()
    files = sorted({Path("go.mod"), Path("go.sum"),
                    *(p.relative_to(root) for p in (root / "experiments/snapshotindex").glob("*.go")),
                    *(p.relative_to(root) for p in (root / "internal/proto").glob("*.go"))})
    for path in files:
        source.update(str(path).encode() + b"\0" + (root / path).read_bytes() + b"\0")
    version, _ = run(["go", "version"], output / "go-version.txt", env, root)
    binary = output / "snapshotindex.test"
    _, build_seconds = run(["go", "test", "-c", "-o", str(binary), "./experiments/snapshotindex"], output / "build.txt", env, root)
    tests, test_seconds = run([str(binary), "-test.v"], output / "tests.txt", env, root)
    try:
        if platform.system() == "Darwin":
            disks, _ = run(["df", "-P", "."], output / "disk.txt", env, root)
            device = disks.splitlines()[-1].split()[0]
            info, _ = run(["diskutil", "info", "-plist", device], output / "filesystem.txt", env, root)
            filesystem = plistlib.loads(info.encode()).get("FilesystemType", "unknown")
        else:
            filesystem, _ = run(["stat", "-f", "-c", "%T", "."], output / "filesystem.txt", env, root)
    except (OSError, RuntimeError, ValueError, IndexError) as error:
        # Filesystem identification is ancillary to this in-memory benchmark.
        filesystem = f"unavailable: {error}"
    return dict(parent_revision=revision, source_sha256=source.hexdigest(), source_files=[str(p) for p in files],
                platform=platform.platform(), machine=platform.machine(), filesystem=filesystem.strip(), go_version=version.strip(),
                gomaxprocs=2, cgo_enabled=False, build_seconds=build_seconds, test_seconds=test_seconds,
                binary_sha256=hashlib.sha256(binary.read_bytes()).hexdigest(), contract_test_output=tests,
                samples=[], benchmark_seconds=0,
                scope="In-memory metadata only. No filesystem inventory, transport, durable staging, or application latency.")


def measure(root, output, env, report, round_number, benchtime):
    command = [str(output / "snapshotindex.test"), "-test.run=^$", "-test.bench=^BenchmarkIndex$",
               "-test.count=1", f"-test.benchtime={benchtime}", "-test.benchmem"]
    raw, elapsed = run(command, output / f"bench-{round_number}.txt", env, root)
    samples = []
    for name, iterations, ns, allocated, allocations in SAMPLE.findall(raw):
        _, count, representation, operation = name.split("/")
        samples.append(dict(entries=int(count), representation=representation, operation=operation,
                            round=round_number, iterations=int(iterations), ns_per_op=float(ns),
                            bytes_per_op=int(allocated), allocations_per_op=int(allocations)))
    if len(samples) != 90:
        raise RuntimeError(f"Expected 90 benchmark samples; parsed {len(samples)}")
    report["samples"].extend(samples)
    report["benchmark_seconds"] += elapsed


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--revision", required=True, help="Parent revision; source digest includes uncommitted Go files")
    parser.add_argument("--count", type=int, default=5)
    parser.add_argument("--benchtime", default="150ms")
    parser.add_argument("--baseline-source", type=Path, help="Optional preserved module root for alternating before/after rounds")
    args = parser.parse_args()
    if args.count < 1:
        parser.error("--count must be positive")
    output = args.output.resolve()
    if output.exists():
        parser.error("--output must be a new directory")
    env = dict(os.environ, GOMAXPROCS="2", CGO_ENABLED="0")
    roots = {"candidate": Path.cwd()}
    if args.baseline_source:
        roots = {"baseline": args.baseline_source.resolve(), **roots}
    directories = {name: output / name if args.baseline_source else output for name in roots}
    reports = {name: prepare(root, directories[name], env, args.revision) for name, root in roots.items()}
    orders = []
    for round_number in range(args.count):
        order = list(roots)
        if round_number % 2:
            order.reverse()
        orders.append(order)
        for name in order:
            measure(roots[name], directories[name], env, reports[name], round_number, args.benchtime)
            print(f"Round {round_number + 1}/{args.count}: {name} completed", flush=True)
    for name, report in reports.items():
        report.update(count=args.count, benchtime=args.benchtime, round_orders=orders,
                      harness_sha256=hashlib.sha256(Path(__file__).read_bytes()).hexdigest())
        (directories[name] / "report.json").write_text(json.dumps(report, indent=2) + "\n")
        # Keep retained artifacts compact. Failures leave the binary for diagnosis.
        (directories[name] / "snapshotindex.test").unlink()
    if args.baseline_source:
        (output / "comparison.json").write_text(json.dumps({"reports": reports, "round_orders": orders}, indent=2) + "\n")
    print(f"Passed contracts; recorded {90 * args.count * len(roots)} samples in {output}", flush=True)


if __name__ == "__main__":
    main()
