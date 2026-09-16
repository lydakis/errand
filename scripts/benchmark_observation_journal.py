#!/usr/bin/env python3
"""Compare frozen/current observation replacement with an observation-only journal."""
import argparse
import struct
import hashlib
import json
import os
from pathlib import Path
import platform
import tarfile

from benchmark_checkpoint_builder import candidate_digest
from benchmark_derived_index import summarize
from benchmark_snapshot_integration import benchmark_campaign, benchmark_order, filesystem, run, source_digest
from snapshot_provenance import harness_inputs

CONTROL_COMMIT = "9d9fe5ab02a54b8e6ce48a2542a5441741019bfe"
CONTROL_ARCHIVE = "428f18dfa11841cb0acdee10b97ce959306d8fcc90b57b1b451d1f0b9dffd98a"
VARIANTS = ("checkpoint", "replacement", "observation-journal")
JOURNAL_LIMIT = 8 << 20
FILE_LIMIT = 64 << 20
RECORD_LIMIT = 32
MAGIC = b"ERRAND-OBS-JOURNAL-1\n"


def journal_layout(path):
    """Read framing outside timers, independently of the probe's receipts."""
    size = path.stat().st_size
    ends = []
    with path.open("rb") as stream:
        if stream.read(len(MAGIC)) != MAGIC:
            raise RuntimeError("Unexpected observation journal format")
        while stream.tell() < size:
            length = stream.read(8)
            if len(length) != 8:
                raise RuntimeError("Incomplete journal frame")
            stream.seek(struct.unpack("<Q", length)[0] + 64, 1)
            if stream.tell() > size:
                raise RuntimeError("Incomplete journal payload")
            ends.append(stream.tell())
    if not ends:
        raise RuntimeError("Missing journal base")
    return dict(records=len(ends)-1, base_bytes=ends[0], cache_bytes=size,
                journal_bytes=size-ends[0])


def append_budget(edits):
    # An explicit workload ceiling, checked after every ordinary append. Reset
    # untimed before either byte limit becomes ambiguous; dedicated cases time
    # byte compaction. This is not an estimate used by the storage implementation.
    return 4096 + 2048 * edits


def needs_reseed(before, edits):
    return (before["records"] < RECORD_LIMIT and
            (before["journal_bytes"] + append_budget(edits) > JOURNAL_LIMIT or
             before["cache_bytes"] + append_budget(edits) > FILE_LIMIT))


def validate_sample(sample, files, edits, scenario, variant, before=None, after=None):
    result = sample["Result"]
    journal = variant == "observation-journal"
    miss, recovery = scenario == "miss", journal and scenario == "recovery"
    replaced = journal and scenario in ("records", "bytes", "recovery")
    expected = dict(Hashed=files if miss else edits, Reused=0 if miss else files-edits,
                    Written=miss or edits != 0 or recovery, CacheError="",
                    CacheStatus="missing" if miss else "recovered" if recovery else "hit",
                    ReplacedBase=replaced)
    if journal and scenario in ("edit", "batch"):
        if before is None or after is None or needs_reseed(before, edits):
            raise RuntimeError("Ordinary sample needs bounded, independently recorded history")
        records = before["records"]
        if not 0 <= records <= RECORD_LIMIT:
            raise RuntimeError("Unexpected journal depth")
        expected["JournalRecordsLoaded"] = records
        expected["ReplacedBase"] = records == RECORD_LIMIT
        written = result.get("CheckpointBytes", 0)
        if records == RECORD_LIMIT:
            valid = after["records"] == 0 and written == after["cache_bytes"]
        else:
            valid = (after["records"] == records + 1 and
                     after["base_bytes"] == before["base_bytes"] and
                     0 < written <= append_budget(edits) and
                     written == after["cache_bytes"] - before["cache_bytes"] and
                     after["journal_bytes"] <= JOURNAL_LIMIT and
                     after["cache_bytes"] <= FILE_LIMIT)
        if not valid:
            raise RuntimeError("Publication receipt does not match journal growth or compaction")
    if journal and scenario in ("records", "bytes", "recovery"):
        expected["JournalRecordsLoaded"] = {"records": 32, "bytes": 1, "recovery": 4}[scenario]
    if any(result.get(k) != v for k, v in expected.items()):
        raise RuntimeError(f"Expected {expected}; got {result}")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--rounds", type=int, default=6)
    parser.add_argument("--smoke", action="store_true")
    args = parser.parse_args()
    if args.rounds < 6 or args.rounds % 6:
        parser.error("rounds must be a positive multiple of six for balanced order")
    source, out = Path.cwd(), args.output.resolve()
    out.mkdir(parents=True, exist_ok=False)
    env = dict(os.environ, GOMAXPROCS="2", CGO_ENABLED="0")
    for key in tuple(env):
        if key.startswith("GIT_"):
            del env[key]
    env.update(GIT_CONFIG_NOSYSTEM="1", GIT_CONFIG_GLOBAL=os.devnull)
    report = dict(scope="Fresh-process preparation plus wire hash; warm OS caches; no delivery claim",
                  source_sha256=candidate_digest(source, out), harness_inputs=harness_inputs(source/"scripts"),
                  control_commit=CONTROL_COMMIT, control_archive_sha256=CONTROL_ARCHIVE,
                  platform=platform.platform(), machine=platform.machine(), python=platform.python_version(),
                  rounds=args.rounds, gomaxprocs=2, smoke=args.smoke, samples=[])
    with benchmark_campaign(out, report) as scratch:
        env["TMPDIR"] = str(scratch)
        report["filesystem"] = filesystem(scratch, env, out, "filesystem")
        expected_fs = "apfs" if platform.system() == "Darwin" else "btrfs"
        if report["filesystem"].lower() != expected_fs:
            raise RuntimeError(f"Expected native {expected_fs}, got {report['filesystem']}")
        report["mount"] = run(["findmnt", "-T", str(scratch), "-n", "-o", "FSTYPE,OPTIONS"]
                              if platform.system() == "Linux" else ["mount"], source, env, out/"mount.txt")
        archive = source/"docs/benchmarks/observation-journal/control-inputs.tar.gz"
        if hashlib.sha256(archive.read_bytes()).hexdigest() != CONTROL_ARCHIVE:
            raise RuntimeError("Control archive changed")
        baseline = scratch/"control"
        baseline.mkdir()
        with tarfile.open(archive) as tar:
            tar.extractall(baseline, filter="data")
        report["control_source_sha256"] = source_digest(baseline)
        binaries = {}
        report["toolchains"] = {}
        for name, root in (("control", baseline), ("candidate", source)):
            report["toolchains"][name] = json.loads(run(["go", "env", "-json", "GOVERSION", "GOOS", "GOARCH", "GOFLAGS", "CGO_ENABLED", "GOTOOLCHAIN", "GOEXPERIMENT"], root, env, out/f"toolchain-{name}.json"))
            binary = scratch/f"{name}-probe"
            run(["go", "build", "-o", str(binary), "./experiments/snapshotcheckpoint/cmd"], root, env, out/f"build-{name}.txt")
            binaries[name] = binary
        if report["toolchains"]["control"] != report["toolchains"]["candidate"]:
            raise RuntimeError("Control and candidate toolchains differ")
        report["binaries"] = {name: hashlib.sha256(p.read_bytes()).hexdigest() for name, p in binaries.items()}
        # Validation is serialized before timing; no competing tests or profiler.
        packages = ["./internal/manifest", "./internal/snapshot", "./experiments/snapshotcheckpoint/..."]
        run(["python3", "-m", "unittest", "discover", "-s", "scripts"], source, env, out/"python-tests.txt")
        run(["go", "test", "-p=1", *packages], source, env, out/"tests.txt")
        run(["go", "vet", *packages], source, env, out/"vet.txt")
        run(["go", "test", "-race", "-p=1", *packages], source, dict(env, CGO_ENABLED="1"), out/"race.txt")
        fixtures = [(1000, 32, False), (10000, 4096, False), (50000, 128, False), (10000, 4096, True)]
        if args.smoke:
            fixtures = [(100, 32, False)]
        for count, size, git in fixtures:
            label = f"{count}-{size}-{'git' if git else 'ignore'}"
            fixture = scratch/label
            fixture.mkdir()
            if not git:
                (fixture/".errandignore").write_text("")
            paths = []
            for i in range(count):
                p = fixture/f"dir-{i//100:04d}"/f"file-{i:05d}"
                p.parent.mkdir(exist_ok=True)
                p.write_bytes((f"body-{i}".encode()+b"x"*size)[:size])
                paths.append(p)
            if git:
                run(["git", "init", "-q"], fixture, env, out/f"init-{label}.txt")
                run(["git", "add", "."], fixture, env, out/f"add-{label}.txt")
            caches = {v: scratch/f"cache-{label}-{v}" for v in VARIANTS}
            def cache_file(v):
                return caches[v]/("observations" if v == "observation-journal" else "checkpoint")
            def invoke(v, log):
                binary = binaries["control" if v == "checkpoint" else "candidate"]
                mode = "observation-journal" if v == "observation-journal" else "checkpoint"
                return json.loads(run([str(binary), "-root", str(fixture), "-cache", str(caches[v]), "-mode", mode], source, env, log, timeout=300))
            def mutate(tag, edits):
                for i in range(edits):
                    paths[i].write_bytes((f"{tag}-{i}".encode()+b"z"*size)[:size])
            scenarios = [("miss", 0), ("unchanged", 0), ("edit", 1), ("batch", min(1000,count)), ("records", 1), ("recovery", 0)]
            if count == 50000:
                scenarios.append(("bytes", 30000))
            for scenario, edits in scenarios:
                for v in VARIANTS:
                    cache_file(v).unlink(missing_ok=True)
                    invoke(v, out/f"seed-{label}-{scenario}-{v}.json")
                for number in range(args.rounds):
                    setup = {}
                    if scenario == "miss":
                        for v in VARIANTS:
                            cache_file(v).unlink()
                    if scenario in ("records", "bytes", "recovery"):
                        v = "observation-journal"
                        cache_file(v).unlink()
                        invoke(v, out/f"base-{label}-{scenario}-{number}.json")
                        base_bytes = cache_file(v).stat().st_size
                        steps, fill_edits = {"records": (32, 1), "bytes": (1, 30000), "recovery": (4, 1)}[scenario]
                        for step in range(steps):
                            mutate(f"fill-{number}-{step}", fill_edits)
                            fill = invoke(v, out/f"fill-{label}-{scenario}-{number}-{step}.json")
                            result = fill["Result"]
                            if result["CacheError"] or result["CacheStatus"] != "hit" or not result["Written"] or result["ReplacedBase"] or result["JournalRecordsLoaded"] != step:
                                raise RuntimeError(f"Unexpected journal history during setup: {result}")
                        setup = dict(base_bytes=base_bytes, journal_bytes=cache_file(v).stat().st_size-base_bytes,
                                     records=steps, last_append_bytes=result["CheckpointBytes"])
                        if scenario == "bytes" and not JOURNAL_LIMIT/2 < setup["journal_bytes"] < JOURNAL_LIMIT:
                            raise RuntimeError(f"Fixture does not straddle journal byte limit: {setup}")
                        for v in VARIANTS[:2]:
                            invoke(v, out/f"catchup-{label}-{scenario}-{number}-{v}.json")
                    if scenario == "recovery":
                        with cache_file("observation-journal").open("ab") as f:
                            f.write(b"interrupted transaction")
                    before = None
                    if scenario in ("edit", "batch"):
                        v = "observation-journal"
                        before = journal_layout(cache_file(v))
                        if needs_reseed(before, edits):
                            cache_file(v).unlink()
                            invoke(v, out/f"reseed-{label}-{scenario}-{number}.json")
                            before = journal_layout(cache_file(v))
                            setup["byte_budget_reseed"] = True
                        setup["prior_history"] = before
                    mutate(f"{scenario}-{number}", edits)
                    run(["sync"], source, env, out/"sync.txt")
                    hashes = []
                    for position, v in enumerate(benchmark_order(VARIANTS, number), 1):
                        report["active"] = dict(fixture=label, scenario=scenario, round=number, position=position, variant=v, edits=edits)
                        sample = invoke(v, out/f"{label}-{scenario}-{number}-{v}.json")
                        after = journal_layout(cache_file(v)) if v == "observation-journal" and before else None
                        validate_sample(sample, count+int(not git), edits, scenario, v, before, after)
                        sample.update(report["active"], setup=setup, cache_bytes=cache_file(v).stat().st_size)
                        hashes.append(sample["Hash"])
                        report["samples"].append(sample)
                    if len(set(hashes)) != 1:
                        raise RuntimeError("Snapshot roots differ")
                    report.pop("active")
                    (out/"report.json").write_text(json.dumps(report, indent=2)+"\n")
                print(f"{label}: {scenario} complete", flush=True)
        report["summary"] = summarize(report["samples"], VARIANTS)
        report["replacement_summary"] = summarize(report["samples"], VARIANTS, baseline_variant="replacement")
        report["final_source_sha256"] = candidate_digest(source, out)
        report["final_harness_inputs"] = harness_inputs(source/"scripts")
        if report["final_source_sha256"] != report["source_sha256"] or report["final_harness_inputs"] != report["harness_inputs"]:
            raise RuntimeError("Measurement inputs changed")


if __name__ == "__main__":
    main()
