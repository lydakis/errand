"""Measure no-op jobs on explicitly selected Errand peers using synthetic data."""

import argparse
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import platform
import random
import re
import shutil
import statistics
import subprocess
import sys
import tempfile
import time
import uuid


def write_fixture(root, seed, files, file_bytes):
    root.mkdir(parents=True, exist_ok=True)
    for index in range(files):
        content = hashlib.shake_256(f"{seed}:{index}".encode()).digest(file_bytes)
        (root / f"file-{index:05d}.bin").write_bytes(content)


def parse_transfer(stderr, scenario):
    if scenario == "no-snapshot":
        if "errand: snapshot contains" in stderr or "errand: shipping" in stderr:
            raise ValueError("no-snapshot sample unexpectedly prepared a snapshot")
        return {"snapshot_files": 0, "snapshot_bytes": 0, "shipped_files": 0, "shipped_bytes": 0}
    snapshot = re.search(r"^errand: snapshot contains (\d+) files, (\d+) bytes$", stderr, re.M)
    shipped = re.search(r"^errand: shipping (\d+) of (\d+) files \((\d+) bytes;", stderr, re.M)
    if not snapshot or not shipped or "re-shipping" in stderr or "negotiation failed" in stderr:
        raise ValueError("cache behavior was not verified; inspect stderr")
    files, size = map(int, snapshot.groups())
    sent, total, sent_bytes = map(int, shipped.groups())
    if files != total or files == 0:
        raise ValueError("unexpected snapshot accounting")
    if scenario == "cached" and (sent != 0 or sent_bytes != 0):
        raise ValueError("cached sample transferred file content")
    if scenario == "cold" and (sent != files or sent_bytes != size):
        raise ValueError("cold sample reused cached file content")
    return {"snapshot_files": files, "snapshot_bytes": size, "shipped_files": sent, "shipped_bytes": sent_bytes}


def summarize(samples):
    groups = {}
    for sample in samples:
        if sample.get("valid") and sample["scenario"] != "warmup":
            groups.setdefault((sample["peer"], sample["scenario"]), []).append(sample["wall_ms"])
    return [dict(peer=peer, scenario=scenario, samples=len(values),
                 median_ms=statistics.median(values), min_ms=min(values), max_ms=max(values))
            for (peer, scenario), values in sorted(groups.items())]


def run_process(argv, cwd, env, timeout):
    start = time.perf_counter_ns()
    try:
        result = subprocess.run(argv, cwd=cwd, env=env, capture_output=True, text=True, timeout=timeout)
        record = {"exit_code": result.returncode, "stdout": result.stdout, "stderr": result.stderr}
    except subprocess.TimeoutExpired as error:
        def text(value):
            return value.decode(errors="replace") if isinstance(value, bytes) else (value or "")
        record = {"exit_code": None, "error": "client timeout", "stdout": text(error.stdout), "stderr": text(error.stderr)}
    record["wall_ms"] = (time.perf_counter_ns() - start) / 1_000_000
    return record


def checked_json(binary, args, cwd, env):
    result = run_process([binary, *args], cwd, env, 30)
    if result["exit_code"] != 0:
        raise ValueError(f"{' '.join(args)} failed: {result['stderr']}")
    return json.loads(result["stdout"])


def measure_peer(binary, peer, scenario, index, root, env, timeout, output):
    flags = ["--no-snapshot"] if scenario == "no-snapshot" else ["--include-all", "--workspace-root", str(root)]
    record = run_process([binary, "--on", peer, "--no-apply", *flags, "--", "/usr/bin/true"], root, env, timeout)
    record.update(peer=peer, scenario=scenario, index=index, valid=False)
    stem = f"{index:04d}"
    (output / f"{stem}.stdout.txt").write_text(record.pop("stdout"))
    stderr = record.pop("stderr")
    (output / f"{stem}.stderr.txt").write_text(stderr)
    record["stderr_file"] = f"{stem}.stderr.txt"
    handle = re.search(r"^errand: job (\S+) \(", stderr, re.M)
    if handle:
        record["handle"] = handle.group(1)
    try:
        if record["exit_code"] != 0 or not handle:
            raise ValueError("job did not complete successfully or no handle was reported")
        status = checked_json(binary, ["status", "--json", record["handle"]], root, env)
        (output / f"{stem}.status.json").write_text(json.dumps(status, indent=2) + "\n")
        result = status.get("result") or {}
        if status.get("state") != "exited" or result.get("exit_code") != 0 or not all(
                result.get(key) for key in ("started", "cleanup_ok", "changes_ok", "logs_complete")) or result.get("transaction_error"):
            raise ValueError("terminal receipt does not confirm a successful transaction")
        record["process_ms"] = result.get("duration_ms", 0)
        record.update(parse_transfer(stderr, "cold" if scenario == "warmup" else scenario))
        record["valid"] = True
    except (ValueError, OSError) as error:
        record["error"] = str(error)
    return record


def positive(value):
    number = int(value)
    if number <= 0:
        raise argparse.ArgumentTypeError("must be positive")
    return number


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", default="errand")
    parser.add_argument("--on", action="append", required=True, help="configured peer; repeat for another peer")
    parser.add_argument("--samples", type=positive, default=5)
    parser.add_argument("--files", type=positive, default=128)
    parser.add_argument("--file-bytes", type=positive, default=65536)
    parser.add_argument("--timeout", type=positive, default=120, help="client timeout per job, seconds")
    parser.add_argument("--output", type=Path, required=True, help="new directory for raw evidence and report.json")
    args = parser.parse_args()
    binary = shutil.which(args.binary)
    if binary is None:
        parser.error(f"binary not found: {args.binary}")
    binary = str(Path(binary).resolve())
    if len(set(args.on)) != len(args.on):
        parser.error("duplicate peer selection")
    output = args.output.resolve()
    output.mkdir(parents=True, exist_ok=False)
    nonce = uuid.uuid4().hex
    report = {"schema_version": 1, "started_at": datetime.now(timezone.utc).isoformat(),
              "run_seed": nonce, "client_sha256": hashlib.sha256(Path(binary).read_bytes()).hexdigest(),
              "caller": {"system": platform.system(), "arch": platform.machine(), "os_release": platform.release()},
              "fixture": {"files": args.files, "file_bytes": args.file_bytes},
              "requested_samples_per_scenario": args.samples, "peers": {}, "samples": [], "complete": False}

    def save():
        report["summary"] = summarize(report["samples"])
        temporary = output / "report.json.tmp"
        temporary.write_text(json.dumps(report, indent=2) + "\n")
        temporary.replace(output / "report.json")

    try:
        with tempfile.TemporaryDirectory(prefix="errand-benchmark-") as directory:
            temp = Path(directory).resolve()
            # Isolate local receipt/apply state; retain user config for explicit peer routing.
            env = dict(os.environ, XDG_STATE_HOME=str(temp / "state"))
            version = run_process([binary, "version"], temp, env, 30)
            if version["exit_code"] != 0:
                raise ValueError("could not read client version")
            report["client_version"] = version["stdout"].strip()
            for _ in range(args.samples):
                result = run_process(["/usr/bin/true"], temp, env, args.timeout)
                report["samples"].append({"peer": "local", "scenario": "local-noop", "valid": result["exit_code"] == 0,
                                          "exit_code": result["exit_code"], "wall_ms": result["wall_ms"]})
                if result["exit_code"] != 0:
                    raise ValueError("local reference command failed")
            for peer in args.on:
                before = checked_json(binary, ["peers", "--json", "--on", peer], temp, env)
                report["peers"][peer] = {"before": before}
                info = before[0]["info"]
                if any(info.get(key, 0) for key in ("running_jobs", "starting_jobs", "staging_jobs", "queued_jobs")):
                    raise ValueError(f"{peer} has active jobs; benchmark an idle runner")
                cached = temp / "cached"
                cold = temp / "cold"
                empty = temp / "empty"
                empty.mkdir(exist_ok=True)
                write_fixture(cached, f"{nonce}:{peer}:cached", args.files, args.file_bytes)
                scenarios = ["warmup"]
                for trial in range(args.samples):
                    order = ["no-snapshot", "cold", "cached"]
                    random.Random(f"{nonce}:{peer}:{trial}").shuffle(order)
                    scenarios.extend(order)
                for scenario in scenarios:
                    index = len(report["samples"])
                    root = empty if scenario == "no-snapshot" else cached
                    if scenario == "cold":
                        root = cold
                        write_fixture(cold, f"{nonce}:{peer}:{index}", args.files, args.file_bytes)
                    record = measure_peer(binary, peer, scenario, index, root, env, args.timeout, output)
                    report["samples"].append(record)
                    save()
                    print(f"{peer} {scenario}: {record['wall_ms']:.1f} ms ({'ok' if record['valid'] else 'invalid'})", flush=True)
                    if not record["valid"]:
                        raise ValueError(f"invalid sample; see {output / record['stderr_file']}")
                report["peers"][peer]["after"] = checked_json(binary, ["peers", "--json", "--on", peer], temp, env)
            report["complete"] = True
    except (ValueError, OSError) as error:
        report["error"] = str(error)
        print(f"benchmark stopped: {error}", file=sys.stderr)
    finally:
        save()
    print(f"Report: {output / 'report.json'}")
    return 0 if report["complete"] else 1


if __name__ == "__main__":
    sys.exit(main())
