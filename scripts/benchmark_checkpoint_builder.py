#!/usr/bin/env python3
"""Interleave frozen checkpoint, shared builder, and current-index alternatives."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import platform
import tarfile

from benchmark_snapshot_integration import benchmark_campaign, filesystem, run, source_digest
from snapshot_provenance import harness_inputs

BASELINE = "094b5b02dce653e8125a3a5b933868cf0db70bea459b0fe3492276b188fb0b8d"
ARCHIVE = "d98b852b85c69912be47e311eaf6658d7d425a8e592b2dfb4338dc6a3b01208d"


def candidate_digest(root, output):
    # Output is a newly created directory. Its extracted baseline and generated
    # fixtures are not candidate inputs; every other Go/module file is guarded.
    digest = hashlib.sha256()
    paths = sorted([*root.rglob("*.go"), root/"go.mod", root/"go.sum"])
    for path in paths:
        if path.is_relative_to(output):
            continue
        digest.update(str(path.relative_to(root)).encode()+b"\0"+path.read_bytes()+b"\0")
    return digest.hexdigest()


def variant_order(variants, number):
    names = list(variants)
    offset = number % len(names)
    order = names[offset:] + names[:offset]
    return order if (number // len(names)) % 2 == 0 else order[::-1]


def validate_sample(sample, files, edits, miss, cold):
    result = sample["Result"]
    hashed = files if cold or miss else edits
    expected = dict(Hashed=hashed, Reused=files-hashed,
                    Written=not cold and (miss or edits != 0),
                    CacheStatus="" if cold else "missing" if miss else "hit", CacheError="")
    if any(result.get(k) != v for k, v in expected.items()):
        raise RuntimeError(f"Behavior mismatch: expected {expected}, got {result}")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--rounds", type=int, default=8)
    parser.add_argument("--smoke", action="store_true")
    args = parser.parse_args()
    if args.rounds < 8 or args.rounds % 8:
        parser.error("rounds must be a positive multiple of eight for balanced four-way order")
    source, out = Path.cwd(), args.output.resolve()
    out.mkdir(parents=True, exist_ok=False)
    env = dict(os.environ, GOMAXPROCS="2", CGO_ENABLED="0")
    # Prevent inherited Git routing/configuration from changing generated fixtures.
    for key in tuple(env):
        if key.startswith("GIT_"):
            del env[key]
    env.update(GIT_CONFIG_NOSYSTEM="1", GIT_CONFIG_GLOBAL=os.devnull)
    report = dict(scope="Fresh-process preparation plus wire hash, warm filesystem caches; no delivery claim",
                  source_sha256=candidate_digest(source,out), harness_inputs=harness_inputs(source/"scripts"), developer_dir=env.get("DEVELOPER_DIR"),
                  baseline_sha256=BASELINE, rounds=args.rounds, smoke=args.smoke,
                  platform=platform.platform(), machine=platform.machine(), gomaxprocs=2, samples=[])
    with benchmark_campaign(out, report) as scratch:
        env["TMPDIR"] = str(scratch)
        report["filesystem"] = filesystem(scratch, env, out, "filesystem")
        report["mount"] = run(["findmnt", "-T", str(scratch), "-n", "-o", "FSTYPE,OPTIONS"]
                              if platform.system() == "Linux" else ["mount"], source, env, out/"mount.txt")
        report["toolchain"] = json.loads(run(["go", "env", "-json", "GOVERSION", "GOOS", "GOARCH", "GOFLAGS", "CGO_ENABLED", "GOTOOLCHAIN", "GOEXPERIMENT"], source, env, out/"toolchain.json"))
        archive = source/"docs/benchmarks/snapshot-checkpoint/review-final-inputs.tar.gz"
        assert hashlib.sha256(archive.read_bytes()).hexdigest() == ARCHIVE
        baseline = scratch/"baseline"
        baseline.mkdir()
        with tarfile.open(archive) as tar:
            tar.extractall(baseline, filter="data")
        assert source_digest(baseline) == BASELINE
        binaries = {}
        for name, root in (("baseline", baseline), ("candidate", source)):
            binary = scratch/f"{name}-probe"
            run(["go", "build", "-o", str(binary), "./experiments/snapshotcheckpoint/cmd"], root, env, out/f"build-{name}.txt")
            binaries[name] = binary
        report["binaries"] = {name: hashlib.sha256(p.read_bytes()).hexdigest() for name,p in binaries.items()}
        run(["python3", "-m", "unittest", "discover", "-s", "scripts", "-p", "test_*checkpoint*.py"], source, env, out/"python-tests.txt")
        validation_env = {k:v for k,v in env.items() if k not in ("GIT_CONFIG_GLOBAL", "GIT_CONFIG_NOSYSTEM")}
        run(["go", "test", "./internal/snapshot", "./internal/manifest", "./experiments/snapshotcheckpoint/..."], source, validation_env, out/"tests.txt")
        run(["go", "test", "-race", "./internal/snapshot", "./internal/manifest", "./experiments/snapshotcheckpoint/..."], source, dict(validation_env, CGO_ENABLED="1"), out/"race.txt")
        run(["go", "vet", "./internal/snapshot", "./internal/manifest", "./experiments/snapshotcheckpoint/..."], source, env, out/"vet.txt")
        variants = {"cold": ("baseline", "cold"), "checkpoint": ("baseline", "checkpoint"),
                    "shared-update": ("candidate", "checkpoint"), "shared-current": ("candidate", "current")}
        fixtures = [(1000,32,False), (10000,4096,False), (1000,65536,False), (50000,128,False), (10000,4096,True)]
        if args.smoke:
            fixtures = [(1000,32,False), (1000,32,True)]
        for count, size, git in fixtures:
            label = f"{count}-{size}-{'git' if git else 'ignore'}"
            fixture = scratch/label
            fixture.mkdir()
            if not git:
                (fixture/".errandignore").write_text("")
            paths = []
            for i in range(count):
                file = fixture/f"dir-{i//100:04d}"/f"file-{i:05d}"
                file.parent.mkdir(exist_ok=True)
                file.write_bytes((f"body-{i:05d}".encode()+b"x"*size)[:size])
                paths.append(file)
            if git:
                run(["git", "init", "-q"], fixture, env, out/f"git-init-{label}.txt")
                run(["git", "add", "."], fixture, env, out/f"git-add-{label}.txt")
            files = count + int(not git)
            caches = {name: scratch/f"cache-{label}-{name}" for name in variants}
            def invoke(name, log):
                binary, mode = variants[name]
                return json.loads(run([str(binaries[binary]), "-root", str(fixture), "-cache", str(caches[name]), "-mode", mode], source, env, log, timeout=300))
            for scenario, edits in (("miss",0), ("unchanged",0), ("edit",1), ("batch",min(1000,count))):
                for name in variants:
                    if name != "cold":
                        invoke(name, out/f"seed-{label}-{scenario}-{name}.json")
                for pair in range(args.rounds):
                    if scenario == "miss":
                        for cache in caches.values():
                            (cache/"checkpoint").unlink(missing_ok=True)
                    for i in range(edits):
                        paths[i].write_bytes((f"{scenario}-{pair}-{i}".encode()+b"z"*size)[:size])
                    run(["sync"], source, env, out/"sync.txt")
                    hashes=[]
                    for position,name in enumerate(variant_order(variants,pair),1):
                        report["active"]=dict(fixture=label,count=count,size=size,git=git,scenario=scenario,edits=edits,pair=pair,position=position,variant=name)
                        sample=invoke(name,out/f"{label}-{scenario}-{pair}-{name}.json")
                        validate_sample(sample,files,edits,scenario=="miss",name=="cold")
                        hashes.append(sample["Hash"])
                        sample.update(report["active"])
                        report["samples"].append(sample)
                    if len(set(hashes)) != 1:
                        raise RuntimeError("snapshot roots differ")
                    report.pop("active")
                    (out/"report.json").write_text(json.dumps(report,indent=2)+"\n")
                print(f"{label}: {scenario} complete",flush=True)
            # Ordinary production builder guard, separate from checkpoint ranking.
            for pair in range(8):
                hashes=[]
                for position,name in enumerate(("baseline","candidate") if pair%2==0 else ("candidate","baseline"),1):
                    sample=json.loads(run([str(binaries[name]),"-root",str(fixture),"-mode","cold"],source,env,out/f"{label}-cold-guard-{pair}-{name}.json"))
                    validate_sample(sample,files,0,False,True)
                    hashes.append(sample["Hash"])
                    sample.update(fixture=label,count=count,size=size,git=git,scenario="cold-guard",pair=pair,position=position,variant=name)
                    report["samples"].append(sample)
                if len(set(hashes)) != 1:
                    raise RuntimeError("ordinary builder roots differ")
        report["final_source_sha256"] = candidate_digest(source,out)
        report["final_harness_inputs"] = harness_inputs(source/"scripts")
        if report["final_source_sha256"]!=report["source_sha256"] or report["final_harness_inputs"]!=report["harness_inputs"]:
            raise RuntimeError("measurement inputs changed")


if __name__ == "__main__":
    main()
