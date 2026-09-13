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


CASES = {
    "changes": ("./internal/changes", "^BenchmarkPreparedMetadata$", "150ms", 8),
    "retained-small": ("./internal/changes", "^BenchmarkRetainedTransferMetadata$/^(1000|10000)$/", "100x", 4),
    "retained-large": ("./internal/changes", "^BenchmarkRetainedTransferMetadata$/^100000$/", "20x", 2),
    "snapshot": ("./internal/snapshot", "^BenchmarkWatchPreparation$", "10x", 6),
    "commands": ("./cmd/errand", "^Benchmark(PushPhases|WatchPhases|WorkspaceCreationAndSubmission|FetchCompletion)$", "3x", 6),
}


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
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--revision", required=True)
    parser.add_argument("--rounds", type=int, default=5)
    parser.add_argument("--scope", choices=("full", "retained"), default="full",
                        help="Use retained for metadata/preparation and complete watch cycles only")
    args = parser.parse_args()
    if args.rounds < 1:
        parser.error("rounds must be positive")
    cases = dict(CASES)
    if args.scope == "retained":
        cases["commands"] = ("./cmd/errand", "^BenchmarkWatchPhases$", "5x", 1)
    output = args.output.resolve()
    output.mkdir(parents=True, exist_ok=False)
    env = dict(os.environ, GOMAXPROCS="2", CGO_ENABLED="0")
    roots = {"baseline": args.baseline.resolve(), "candidate": Path.cwd()}
    report = dict(revision=args.revision, platform=platform.platform(), machine=platform.machine(),
                  gomaxprocs=2, rounds=args.rounds, versions={}, orders=[], cases=cases,
                  scope="Native-host metadata/preparation and loopback HTTP command paths; no cross-host network latency.",
                  harness_sha256=hashlib.sha256(Path(__file__).read_bytes()).hexdigest())
    report["go"] = run(["go", "version"], Path.cwd(), env, output / "go.txt").strip()
    report["filesystem"] = filesystem(Path.cwd(), env, output, "filesystem")
    run(["go", "test", "-timeout=10m", "./..."], Path.cwd(), env, output / "tests.txt")
    run(["go", "vet", "./..."], Path.cwd(), env, output / "vet.txt")
    run(["go", "test", "-race", "-timeout=10m", "./internal/manifest", "./internal/snapshot", "./internal/changes", "./internal/archive", "./internal/client"],
        Path.cwd(), dict(env, CGO_ENABLED="1"), output / "race.txt")
    print("Candidate tests, vet and race checks passed", flush=True)
    for name, root in roots.items():
        directory = output / name
        directory.mkdir()
        files = sorted([*root.rglob("*.go"), root / "go.mod", root / "go.sum"])
        digest = hashlib.sha256()
        for path in files:
            digest.update(str(path.relative_to(root)).encode() + b"\0" + path.read_bytes() + b"\0")
        version = dict(source_sha256=digest.hexdigest(), source_files=[str(p.relative_to(root)) for p in files], binaries={}, samples=[])
        report["versions"][name] = version
        for key, (package, _, _, _) in cases.items():
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
        order = list(roots) if number % 2 == 0 else list(reversed(roots))
        report["orders"].append(order)
        for name in order:
            for key, (_, pattern, benchtime, expected) in cases.items():
                directory = output / name
                # Verbose mode prints complete result lines after benchmark
                # output, even when a benchmark emits EVALUATION records.
                raw = run([str(directory / f"{key}.test"), "-test.v", "-test.run=^$", f"-test.bench={pattern}",
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
                if len(samples) != expected:
                    raise RuntimeError(f"Expected {expected} {key} samples, got {len(samples)}")
                report["versions"][name]["samples"].extend(samples)
            (output / "report.json").write_text(json.dumps(report, indent=2) + "\n")
            print(f"Round {number+1}/{args.rounds}: {name} complete", flush=True)
    for name in roots:
        for key in cases:
            (output / name / f"{key}.test").unlink()


if __name__ == "__main__":
    main()
