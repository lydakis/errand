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
import signal
import subprocess
import time


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
    if scope == "staging":
        for shape in ("small", "batch", "large"):
            add(f"stage-{shape}", "./internal/changes", f"^BenchmarkTransferStageBodies$/^{shape}$", "3x")
        for shape in ("batch", "large"):
            add(f"fetch-{shape}", "./cmd/errand", f"^BenchmarkFetchBodies$/^{shape}$", "3x")
        add("fetch-small", "./cmd/errand", "^BenchmarkFetchCompletion$/^persistent=true$", "3x")
        add("watch", "./cmd/errand", "^BenchmarkWatchPhases$", "3x")
        add("push", "./cmd/errand", "^BenchmarkPushPhases$", "3x")
        return cases
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
    if scope in ("full", "receiver"):
        add("push", "./cmd/errand", "^BenchmarkPushPhases$", "5x")
        for kind in ("workspace-create", "ephemeral-job"):
            add(kind, "./cmd/errand", f"^BenchmarkWorkspaceCreationAndSubmission$/^{kind}$", "3x")
        for persistent in ("false", "true"):
            add(f"fetch-{persistent}", "./cmd/errand", f"^BenchmarkFetchCompletion$/^persistent={persistent}$", "3x")
    if scope == "receiver":
        cases = {key: value for key, value in cases.items() if value[0] == "./cmd/errand"}
        for count in (1000, 10000):
            cases[f"checkpoint-{count}"] = ("./internal/changes", f"^BenchmarkCheckpointRead$/^{count}$", "500ms")
    return cases


def run(command, root, env, log, timeout=1200):
    # Go's test timeout does not bound benchmarks. Bound the entire process
    # group, including commands launched by a benchmark, and retain partial logs.
    with log.open("w") as output:
        process = subprocess.Popen(command, cwd=root, env=env, stdout=output,
                                   stderr=subprocess.STDOUT, text=True, start_new_session=True)
        try:
            process.wait(timeout=timeout)
        except BaseException as exc:
            # Ctrl-C interrupts this driver, not the child's separate session.
            # Stop and reap that group before propagating any interrupted wait.
            try:
                os.killpg(process.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
            process.wait()
            if isinstance(exc, subprocess.TimeoutExpired):
                raise RuntimeError(f"{command} exceeded {timeout}s; see {log}") from exc
            raise
    if process.returncode:
        raise RuntimeError(f"{command} exited {process.returncode}; see {log}")
    return log.read_text()


def source_digest(root, production=False, benchmarks=False):
    files = sorted([*root.rglob("*.go"), root / "go.mod", root / "go.sum"])
    digest = hashlib.sha256()
    for path in files:
        if production and path.name.endswith("_test.go"):
            continue
        if benchmarks and not path.name.endswith("_benchmark_test.go"):
            continue
        digest.update(str(path.relative_to(root)).encode() + b"\0" + path.read_bytes() + b"\0")
    return digest.hexdigest()


def parse_sample(raw, number):
    samples, pending = [], None
    for line in raw.splitlines():
        head = re.match(r"^(Benchmark\S+)-\d+\s*(.*)$", line)
        if head:
            pending, line = head[1], head[2]
        match = re.match(r"^\s*(\d+)\s+(.+)$", line)
        if pending and match:
            metrics = match[2].split()
            if len(metrics) % 2 or "ns/op" not in metrics:
                continue
            samples.append(dict(name=pending, iterations=int(match[1]), round=number,
                                metrics={metrics[i+1]: float(metrics[i]) for i in range(0, len(metrics), 2)}))
            pending = None
    if len(samples) != 1:
        raise RuntimeError(f"Expected one benchmark sample, got {len(samples)}")
    return samples[0]


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
    parser.add_argument("--scope", choices=("full", "retained", "metadata", "receiver", "staging"), default="full",
                        help="Use receiver for complete commands and checkpoint reads, retained for metadata/preparation and watch")
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
    fixtures = output / "fixtures"
    fixtures.mkdir()
    env = dict(env, TMPDIR=str(fixtures))
    for name, root in roots.items():
        directory = output / name
        directory.mkdir()
        # Validate each frozen tree, including the control, on this host.
        run(["go", "test", "-timeout=10m", "./..."], root, env, directory / "tests.txt")
        run(["go", "vet", "./..."], root, env, directory / "vet.txt")
        run(["go", "test", "-race", "-timeout=10m", "./internal/manifest", "./internal/snapshot",
             "./internal/changes", "./internal/archive", "./internal/client", "./internal/daemon", "./cmd/errand"],
            root, dict(env, CGO_ENABLED="1"), directory / "race.txt")
        print(f"{name}: tests, vet and race passed", flush=True)
        version = dict(source_sha256=source_digest(root), production_sha256=source_digest(root, production=True),
                       benchmark_sha256=source_digest(root, benchmarks=True), binaries={}, samples=[])
        report["versions"][name] = version
        for package in sorted({case[0] for case in cases.values()}):
            key = package.removeprefix("./").replace("/", "-")
            binary = directory / f"{key}.test"
            run(["go", "test", "-c", "-o", str(binary), package], root, env, directory / f"build-{key}.txt")
            version["binaries"][key] = hashlib.sha256(binary.read_bytes()).hexdigest()
    if args.scope == "staging" and len({v["benchmark_sha256"] for v in report["versions"].values()}) != 1:
        raise RuntimeError("Staging comparisons require identical benchmark sources")
    # Go normally puts fixtures in /tmp, which may be tmpfs even when the
    # runner's workspaces use Btrfs. Place fixtures under the requested output
    # directory and measure its actual filesystem, which may differ from cwd.
    report["fixture_root"] = str(fixtures)
    report["fixture_filesystem"] = filesystem(fixtures, env, output, "fixture-filesystem")
    if platform.system() == "Linux":
        report["fixture_mount"] = run(["findmnt", "-T", str(fixtures), "-n", "-o", "FSTYPE,OPTIONS"], fixtures, env, output / "fixture-mount.txt").strip()
    else:
        report["fixture_mount"] = run(["mount"], fixtures, env, output / "fixture-mount.txt")
    (output / "report.json").write_text(json.dumps(report, indent=2) + "\n")
    for number in range(args.rounds):
        order = benchmark_order(roots, number)
        report["orders"].append(order)
        for key, (package, pattern, benchtime) in cases.items():
            binary_key = package.removeprefix("./").replace("/", "-")
            for name in order:
                directory = output / name
                started, load_before = time.monotonic(), os.getloadavg()
                raw = run([str(directory / f"{binary_key}.test"), "-test.v", "-test.run=^$", f"-test.bench={pattern}",
                           f"-test.benchtime={benchtime}", "-test.benchmem", "-test.timeout=10m"],
                          roots[name], env, directory / f"{number}-{key}.txt", timeout=300)
                sample = parse_sample(raw, number)
                sample.update(case=key, elapsed=time.monotonic()-started, load_before=load_before, load_after=os.getloadavg())
                report["versions"][name]["samples"].append(sample)
                (output / "report.json").write_text(json.dumps(report, indent=2) + "\n")
            print(f"Pair {number+1}/{args.rounds}: {key}", flush=True)
    for name in roots:
        if source_digest(roots[name]) != report["versions"][name]["source_sha256"]:
            raise RuntimeError(f"{name} source changed during measurement")
        for key in report["versions"][name]["binaries"]:
            (output / name / f"{key}.test").unlink()


if __name__ == "__main__":
    main()
