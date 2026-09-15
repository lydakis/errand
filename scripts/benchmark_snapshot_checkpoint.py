#!/usr/bin/env python3
"""Alternate fresh-process preparation with and without advisory checkpoints."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import platform
import time

from benchmark_snapshot_integration import benchmark_campaign, filesystem, run, source_digest
from snapshot_provenance import harness_inputs


def validate_result(result, count, scenario, mode):
    hashed = count + 1 if mode == "cold" or scenario == "miss" else int(scenario == "edit")
    expected = dict(Hashed=hashed, Reused=count + 1 - hashed,
                    Written=mode == "checkpoint" and scenario != "unchanged",
                    CacheStatus="" if mode == "cold" else ("missing" if scenario == "miss" else "hit"),
                    CacheError="")
    if any(result.get(key) != value for key, value in expected.items()):
        raise RuntimeError(f"Wrong {mode}/{scenario} behavior: expected {expected}, got {result}")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--rounds", type=int, default=6)
    args = parser.parse_args()
    if args.rounds < 2 or args.rounds % 2:
        parser.error("rounds must be positive, even, and at least two")
    source, out = Path.cwd(), args.output.resolve()
    out.mkdir(parents=True, exist_ok=False)
    env = dict(os.environ, GOMAXPROCS="2", CGO_ENABLED="0")
    harness = Path(__file__).resolve().parent
    report = dict(scope="Fresh-process source preparation and wire identity only; warm filesystem caches. "
                        "No transfer, materialization, remote negotiation or browser readiness claim.",
                  platform=platform.platform(), machine=platform.machine(), gomaxprocs=2,
                  rounds=args.rounds, samples=[], source_sha256=source_digest(source),
                  harness_inputs=harness_inputs(harness))
    with benchmark_campaign(out, report) as scratch:
        env["TMPDIR"] = str(scratch)
        report["fixture_filesystem"] = filesystem(scratch, env, out, "filesystem")
        mount = (["findmnt", "-T", str(scratch), "-n", "-o", "FSTYPE,OPTIONS"]
                 if platform.system() == "Linux" else ["mount"])
        report["fixture_mount"] = run(mount, source, env, out / "mount.txt")
        report["toolchain"] = json.loads(run(
            ["go", "env", "-json", "GOVERSION", "GOOS", "GOARCH", "CGO_ENABLED", "GOFLAGS", "GOTOOLCHAIN", "GOEXPERIMENT"],
            source, env, out / "toolchain.json"))
        binary = scratch / "checkpoint-probe"
        run(["python3", "-m", "unittest", "discover", "-s", "scripts", "-p", "test_snapshot_checkpoint.py"],
            source, env, out / "harness-tests.txt")
        run(["go", "test", "./experiments/snapshotcheckpoint"], source, env, out / "tests.txt")
        run(["go", "test", "-race", "./experiments/snapshotcheckpoint"], source,
            dict(env, CGO_ENABLED="1"), out / "race.txt")
        run(["go", "vet", "./experiments/snapshotcheckpoint/..."], source, env, out / "vet.txt")
        run(["go", "build", "-o", str(binary), "./experiments/snapshotcheckpoint/cmd"], source, env, out / "build.txt")
        report["binary_sha256"] = hashlib.sha256(binary.read_bytes()).hexdigest()
        for count, size in ((1000, 32), (10000, 4096), (1000, 65536)):
            fixture, cache = scratch / f"source-{count}-{size}", scratch / f"cache-{count}-{size}"
            fixture.mkdir()
            (fixture / ".errandignore").write_text("")
            for i in range(count):
                file = fixture / f"dir-{i // 100:04d}" / f"file-{i:05d}"
                file.parent.mkdir(exist_ok=True)
                file.write_bytes((f"body-{i:05d}".encode() + b"x" * size)[:size])
            command = [str(binary), "-root", str(fixture), "-cache", str(cache)]
            for scenario in ("miss", "unchanged", "edit"):
                run(command + ["-mode", "checkpoint"], source, env, out / f"seed-{count}-{size}-{scenario}.json")
                for pair in range(args.rounds):
                    if scenario == "miss":
                        (cache / "checkpoint").unlink(missing_ok=True)
                    if scenario == "edit":
                        (fixture / "dir-0000/file-00000").write_bytes((f"edit-{pair}".encode() + b"z" * size)[:size])
                    run(["sync"], source, env, out / "sync.txt")
                    order = ("cold", "checkpoint") if pair % 2 == 0 else ("checkpoint", "cold")
                    hashes = []
                    for position, mode in enumerate(order, 1):
                        report["active"] = dict(count=count, size=size, scenario=scenario, pair=pair, mode=mode,
                                                position=position)
                        started = time.monotonic()
                        raw = run(command + ["-mode", mode], source, env,
                                  out / f"{count}-{size}-{scenario}-{pair}-{mode}.json", timeout=300)
                        sample = json.loads(raw)
                        validate_result(sample["Result"], count, scenario, mode)
                        hashes.append(sample["Hash"])
                        sample.update(report["active"], elapsed=time.monotonic()-started)
                        report["samples"].append(sample)
                    if hashes[0] != hashes[1]:
                        raise RuntimeError("cold and checkpoint snapshots differ")
                    report.pop("active")
                    (out / "report.json").write_text(json.dumps(report, indent=2) + "\n")
                print(f"{count} files, {size} bytes: {scenario} complete", flush=True)
        if source_digest(source) != report["source_sha256"] or harness_inputs(harness) != report["harness_inputs"]:
            raise RuntimeError("source or harness changed during measurement")


if __name__ == "__main__":
    main()
