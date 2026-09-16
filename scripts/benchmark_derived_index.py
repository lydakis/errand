#!/usr/bin/env python3
"""Fresh-process comparison of frozen observations, restored indexes and journals."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import platform
import statistics
import tarfile

from benchmark_checkpoint_builder import candidate_digest
from benchmark_snapshot_integration import benchmark_campaign, benchmark_order, filesystem, run, source_digest
from snapshot_provenance import harness_inputs

CONTROL_ARCHIVE = "6cbd8f06a4906819f267c85943003efa93db473c38a94debca7924e68943a219"
VARIANTS = ("checkpoint", "derived", "journal")


def validate_sample(sample, files, edits, scenario, variant):
    result = sample["Result"]
    miss = scenario == "miss"
    recovered = scenario == "recovery" and variant != "checkpoint"
    expected = dict(Hashed=files if miss else edits, Reused=0 if miss else files-edits,
                    Written=miss or edits != 0 or recovered, CacheError="",
                    CacheStatus="missing" if miss else "recovered" if recovered else "hit")
    if any(result.get(k) != v for k, v in expected.items()):
        raise RuntimeError(f"Expected {expected}; got {result}")
    # Ordinary journal edits may also reach a byte/record limit in longer runs.
    if variant == "derived" or variant == "journal" and scenario in ("miss", "unchanged", "compaction", "recovery"):
        replaced = recovered or edits != 0 and (variant == "derived" or scenario == "compaction")
        if result.get("ReplacedBase") != replaced:
            raise RuntimeError(f"Expected ReplacedBase={replaced}; got {result}")
        if scenario == "compaction" and variant == "journal" and result.get("JournalRecordsLoaded") != 32:
            raise RuntimeError("Timed sample did not replay the full journal before replacement")


def summarize(samples):
    rows = []
    for fixture, scenario in sorted({(s["fixture"], s["scenario"]) for s in samples}):
        selected = [s for s in samples if (s["fixture"], s["scenario"]) == (fixture, scenario)]
        baseline = {s["round"]: s for s in selected if s["variant"] == "checkpoint"}
        for variant in VARIANTS:
            group = [s for s in selected if s["variant"] == variant]
            ratios = [s["Total"]/baseline[s["round"]]["Total"] for s in group]
            rows.append(dict(fixture=fixture, scenario=scenario, variant=variant,
                             median_ms=statistics.median(s["Total"] for s in group)/1e6,
                             paired_ratio=statistics.median(ratios),
                             ratio_range=[min(ratios), max(ratios)], wins=sum(r < 1 for r in ratios),
                             cache_bytes=statistics.median(s["cache_bytes"] for s in group),
                             written_bytes=statistics.median(s["Result"]["CheckpointBytes"] if s["Result"]["Written"] else 0 for s in group),
                             phases_ms={p: statistics.median(s["Result"]["Phases"][p] for s in group)/1e6
                                        for p in group[0]["Result"]["Phases"]}))
    return rows


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
                  control_commit="aff735ed86409164cb5ef6827a28e198aa058d7e", control_archive_sha256=CONTROL_ARCHIVE,
                  platform=platform.platform(), machine=platform.machine(), rounds=args.rounds,
                  smoke=args.smoke, samples=[])
    with benchmark_campaign(out, report) as scratch:
        env["TMPDIR"] = str(scratch)
        report["filesystem"] = filesystem(scratch, env, out, "filesystem")
        report["toolchain"] = json.loads(run(["go", "env", "-json", "GOVERSION", "GOOS", "GOARCH", "GOFLAGS", "CGO_ENABLED"], source, env, out/"toolchain.json"))
        report["mount"] = run(["findmnt", "-T", str(scratch), "-n", "-o", "FSTYPE,OPTIONS"]
                              if platform.system() == "Linux" else ["mount"], source, env, out/"mount.txt")
        archive = source/"docs/benchmarks/derived-index/control-inputs.tar.gz"
        if hashlib.sha256(archive.read_bytes()).hexdigest() != CONTROL_ARCHIVE:
            raise RuntimeError("Control archive changed")
        baseline = scratch/"control"
        baseline.mkdir()
        with tarfile.open(archive) as tar:
            tar.extractall(baseline, filter="data")
        report["control_source_sha256"] = source_digest(baseline)
        binaries = {}
        for name, root in (("control", baseline), ("candidate", source)):
            binary = scratch/name/"probe" if name == "control" else scratch/"probe"
            run(["go", "build", "-o", str(binary), "./experiments/snapshotcheckpoint/cmd"], root, env, out/f"build-{name}.txt")
            binaries[name] = binary
        report["binaries"] = {name: hashlib.sha256(p.read_bytes()).hexdigest() for name, p in binaries.items()}
        # Keep validation out of timing and serialize it to avoid competing jobs.
        packages = ["./internal/manifest", "./internal/snapshot", "./experiments/snapshotcheckpoint/..."]
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
            def invoke(v, log):
                binary = binaries["control" if v == "checkpoint" else "candidate"]
                return json.loads(run([str(binary), "-root", str(fixture), "-cache", str(caches[v]), "-mode", v], source, env, log, timeout=300))
            def mutate(tag, edits):
                for i in range(edits):
                    paths[i].write_bytes((f"{tag}-{i}".encode()+b"z"*size)[:size])
            for scenario, edits in (("miss", 0), ("unchanged", 0), ("edit", 1), ("batch", min(1000,count)), ("compaction", 1), ("recovery", 0)):
                for v in VARIANTS:
                    # Isolate journal history between scenarios.
                    (caches[v]/("checkpoint" if v == "checkpoint" else "index")).unlink(missing_ok=True)
                    invoke(v, out/f"seed-{label}-{scenario}-{v}.json")
                for number in range(args.rounds):
                    if scenario == "miss":
                        for v in VARIANTS:
                            (caches[v]/("checkpoint" if v == "checkpoint" else "index")).unlink()
                    if scenario == "compaction":
                        (caches["journal"]/"index").unlink()
                        invoke("journal", out/"journal-base.json")
                        for step in range(32):
                            mutate(f"fill-{number}-{step}", 1)
                            fill = invoke("journal", out/"journal-fill.json")
                            if not fill["Result"]["Written"] or fill["Result"]["ReplacedBase"]:
                                raise RuntimeError("Unexpected journal history during setup")
                        for v in VARIANTS[:2]:
                            invoke(v, out/"control-catchup.json")
                    if scenario == "recovery":
                        for v in VARIANTS[1:]:
                            with (caches[v]/"index").open("ab") as f:
                                f.write(b"interrupted transaction")
                    mutate(f"{scenario}-{number}", edits)
                    run(["sync"], source, env, out/"sync.txt")
                    hashes = []
                    for position, v in enumerate(benchmark_order(VARIANTS, number), 1):
                        report["active"] = dict(fixture=label, scenario=scenario, round=number, position=position, variant=v)
                        sample = invoke(v, out/f"{label}-{scenario}-{number}-{v}.json")
                        validate_sample(sample, count+int(not git), edits, scenario, v)
                        sample.update(report["active"])
                        cache_file = caches[v]/("checkpoint" if v == "checkpoint" else "index")
                        sample["cache_bytes"] = cache_file.stat().st_size
                        hashes.append(sample["Hash"])
                        report["samples"].append(sample)
                    if len(set(hashes)) != 1:
                        raise RuntimeError("Snapshot roots differ")
                    report.pop("active")
                    (out/"report.json").write_text(json.dumps(report, indent=2)+"\n")
                print(f"{label}: {scenario} complete", flush=True)
        report["summary"] = summarize(report["samples"])
        report["final_source_sha256"] = candidate_digest(source, out)
        report["final_harness_inputs"] = harness_inputs(source/"scripts")
        if report["final_source_sha256"] != report["source_sha256"] or report["final_harness_inputs"] != report["harness_inputs"]:
            raise RuntimeError("Measurement inputs changed")


if __name__ == "__main__":
    main()
