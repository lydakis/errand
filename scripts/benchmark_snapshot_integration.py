#!/usr/bin/env python3
"""Alternate frozen baseline/candidate paths on the same native host."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import platform
import plistlib
import re
import subprocess


def benchmark_order(variants, number):
    names = list(variants)
    offset = number % len(names)
    order = names[offset:] + names[:offset]
    # Rotation alone alternates two variants; reversal would cancel it out.
    # With three variants, reversal covers all six permutations.
    if len(names) > 2 and number % 2:
        order.reverse()
    return order


def benchmark_cases(scope):
    # One leaf workload per process makes corresponding variants adjacent.
    cases = {}
    def add(name, package, pattern, benchtime):
        cases[name] = (package, pattern, benchtime)
    for count in (1000, 10000):
        for nested in ("false", "true"):
            for phase in ("prepare", "expand"):
                add(f"metadata-{count}-{nested}-{phase}", "./internal/changes",
                    f"^BenchmarkPreparedMetadata$/^{count}$/^nested={nested}$/^{phase}$", "500ms")
    for count in (1000, 10000, 100000):
        for edits in (1, 100):
            add(f"retained-{count}-{edits}", "./internal/changes",
                f"^BenchmarkRetainedTransferMetadata$/^{count}$/^edit{edits}$", "500ms")
    if scope == "metadata":
        return cases
    for count in (1000, 10000):
        for phase in ("first-edit", "retained-edit", "reconcile"):
            add(f"preparation-{count}-{phase}", "./internal/snapshot",
                f"^BenchmarkWatchPreparation$/^{count}$/^{phase}$", "10x")
    add("watch", "./cmd/errand", "^BenchmarkWatchPhases$", "10x")
    for scenario in ("small", "git-atomic", "nested-atomic", "structural"):
        add(f"watch-{scenario}", "./cmd/errand", f"^BenchmarkWatchWorkloads$/^{scenario}$", "5x")
    if scope == "full":
        add("push", "./cmd/errand", "^BenchmarkPushPhases$", "5x")
        for kind in ("workspace-create", "ephemeral-job"):
            add(kind, "./cmd/errand", f"^BenchmarkWorkspaceCreationAndSubmission$/^{kind}$", "3x")
        for persistent in ("false", "true"):
            add(f"fetch-{persistent}", "./cmd/errand", f"^BenchmarkFetchCompletion$/^persistent={persistent}$", "3x")
    return cases


def run(command, root, env, log):
    result = subprocess.run(command, cwd=root, env=env, stdout=subprocess.PIPE,
                            stderr=subprocess.STDOUT, text=True)
    log.write_text(result.stdout)
    if result.returncode:
        raise RuntimeError(f"{command} exited {result.returncode}; see {log}")
    return result.stdout


def filesystem(root, env, output, name):
    if platform.system() == "Darwin":
        disk = run(["df", "-P", "."], root, env, output / f"{name}-disk.txt").splitlines()[-1].split()[0]
        info = run(["diskutil", "info", "-plist", disk], root, env, output / f"{name}.txt")
        return plistlib.loads(info.encode())["FilesystemType"]
    return run(["stat", "-f", "-c", "%T", "."], root, env, output / f"{name}.txt").strip()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--baseline", type=Path, required=True)
    parser.add_argument("--flat-control", type=Path, help="Optional matched flat engine with the same caller optimizations")
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--revision", required=True)
    parser.add_argument("--rounds", type=int, default=7)
    parser.add_argument("--scope", choices=("full", "retained", "metadata"), default="full",
                        help="Use retained for metadata/preparation and complete watch cycles only")
    parser.add_argument("--gomaxprocs", type=int, default=2)
    args = parser.parse_args()
    if args.rounds < 1 or args.gomaxprocs < 1:
        parser.error("rounds and gomaxprocs must be positive")
    cases = benchmark_cases(args.scope)
    output = args.output.resolve()
    output.mkdir(parents=True, exist_ok=False)
    env = dict(os.environ, GOMAXPROCS=str(args.gomaxprocs), CGO_ENABLED="0")
    roots = {"baseline": args.baseline.resolve(), "candidate": Path.cwd()}
    if args.flat_control:
        roots["flat-control"] = args.flat_control.resolve()
    report = dict(revision=args.revision, platform=platform.platform(), machine=platform.machine(),
                  gomaxprocs=args.gomaxprocs, rounds=args.rounds, versions={}, orders=[], cases=cases,
                  scope="Native-host metadata/preparation and loopback HTTP command paths; no cross-host network latency.",
                  harness_sha256=hashlib.sha256(Path(__file__).read_bytes()).hexdigest())
    report["go"] = run(["go", "version"], Path.cwd(), env, output / "go.txt").strip()
    report["filesystem"] = filesystem(Path.cwd(), env, output, "filesystem")
    for name, root in roots.items():
        directory = output / name
        directory.mkdir()
        # Validate each frozen tree, including the control, on this host.
        run(["go", "test", "-timeout=10m", "./..."], root, env, directory / "tests.txt")
        run(["go", "vet", "./..."], root, env, directory / "vet.txt")
        run(["go", "test", "-race", "-timeout=10m", "./internal/manifest", "./internal/snapshot",
             "./internal/changes", "./internal/archive", "./internal/client", "./cmd/errand"],
            root, dict(env, CGO_ENABLED="1"), directory / "race.txt")
        print(f"{name}: tests, vet and race passed", flush=True)
        files = sorted([*root.rglob("*.go"), root / "go.mod", root / "go.sum"])
        digest = hashlib.sha256()
        for path in files:
            digest.update(str(path.relative_to(root)).encode() + b"\0" + path.read_bytes() + b"\0")
        version = dict(source_sha256=digest.hexdigest(), source_files=[str(p.relative_to(root)) for p in files], binaries={}, samples=[])
        report["versions"][name] = version
        for package in sorted({case[0] for case in cases.values()}):
            key = package.removeprefix("./").replace("/", "-")
            binary = directory / f"{key}.test"
            run(["go", "test", "-c", "-o", str(binary), package], root, env, directory / f"build-{key}.txt")
            version["binaries"][key] = hashlib.sha256(binary.read_bytes()).hexdigest()
    # Go normally puts fixtures in /tmp, which may be tmpfs even when the
    # runner's workspaces use Btrfs. Place fixtures under the requested output
    # directory and measure its actual filesystem, which may differ from cwd.
    fixtures = output / "fixtures"
    fixtures.mkdir()
    env = dict(env, TMPDIR=str(fixtures))
    report["fixture_root"] = str(fixtures)
    report["fixture_filesystem"] = filesystem(fixtures, env, output, "fixture-filesystem")
    for number in range(args.rounds):
        order = benchmark_order(roots, number)
        report["orders"].append(order)
        for key, (package, pattern, benchtime) in cases.items():
            binary_key = package.removeprefix("./").replace("/", "-")
            for name in order:
                directory = output / name
                raw = run([str(directory / f"{binary_key}.test"), "-test.v", "-test.run=^$", f"-test.bench={pattern}",
                           f"-test.benchtime={benchtime}", "-test.benchmem", "-test.timeout=10m"],
                          roots[name], env, directory / f"{number}-{key}.txt")
                samples = []
                for line in raw.splitlines():
                    match = re.match(r"^(Benchmark\S+)-\d+\s+(\d+)\s+(.+)$", line)
                    if not match:
                        continue
                    metrics = match[3].split()
                    if len(metrics) % 2:
                        raise RuntimeError(f"Malformed benchmark line: {line}")
                    samples.append(dict(name=match[1], iterations=int(match[2]), round=number,
                                        metrics={metrics[i+1]: float(metrics[i]) for i in range(0, len(metrics), 2)}))
                if len(samples) != 1:
                    raise RuntimeError(f"Expected one {key} sample, got {len(samples)}")
                report["versions"][name]["samples"].extend(samples)
                (output / "report.json").write_text(json.dumps(report, indent=2) + "\n")
            print(f"Pair {number+1}/{args.rounds}: {key}", flush=True)
    for name in roots:
        for key in report["versions"][name]["binaries"]:
            (output / name / f"{key}.test").unlink()


if __name__ == "__main__":
    main()
