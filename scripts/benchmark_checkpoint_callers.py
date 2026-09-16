#!/usr/bin/env python3
"""Compare real command paths with the frozen checkpoint baseline on one host."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import platform
import tarfile

from benchmark_checkpoint_builder import ARCHIVE, BASELINE, candidate_digest
from benchmark_snapshot_integration import benchmark_campaign, filesystem, parse_sample, run, source_digest
from snapshot_provenance import comparison_inputs, harness_inputs, require_matching_inputs


ALLOWED_TEST_DIFFERENCES = {
    "experiments/snapshotcheckpoint/checkpoint_test.go",
    "experiments/snapshotcheckpoint/codec_test.go",
    "experiments/snapshotcheckpoint/revalidation_test.go",
    "experiments/snapshotcheckpoint/strategies_test.go",
    "experiments/snapshotcheckpoint/admission_test.go",
    "internal/snapshot/observations_test.go",
    "internal/snapshot/symlink_observation_test.go",
}


def inputs(root, output):
    return {name: digest for name, digest in comparison_inputs(root).items()
            if not (root/name).is_relative_to(output)}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--rounds", type=int, default=8)
    args = parser.parse_args()
    if args.rounds < 8 or args.rounds % 2:
        parser.error("use at least eight rounds and an even count for balanced pairs")
    source, out = Path.cwd(), args.output.resolve()
    out.mkdir(parents=True, exist_ok=False)
    env = dict(os.environ, GOMAXPROCS="2", CGO_ENABLED="0")
    for key in tuple(env):
        if key.startswith("GIT_"):
            del env[key]
    report = dict(scope="Real command paths over loopback HTTP on the native filesystem; no cross-host network claim",
                  samples=[], rounds=args.rounds, source_sha256=candidate_digest(source, out),
                  harness_inputs=harness_inputs(source/"scripts"), baseline_sha256=BASELINE,
                  platform=platform.platform(), gomaxprocs=2)
    with benchmark_campaign(out, report) as scratch:
        fixtures = scratch/"fixtures"
        fixtures.mkdir()
        env["TMPDIR"] = str(fixtures)
        report["filesystem"] = filesystem(fixtures, env, out, "filesystem")
        report["mount"] = run(["findmnt", "-T", str(fixtures), "-n", "-o", "FSTYPE,OPTIONS"]
                              if platform.system() == "Linux" else ["mount"], source, env, out/"mount.txt")
        report["toolchain"] = json.loads(run(["go", "env", "-json", "GOVERSION", "GOOS", "GOARCH", "GOFLAGS", "CGO_ENABLED", "GOTOOLCHAIN", "GOEXPERIMENT"], source, env, out/"toolchain.json"))
        archive = source/"docs/benchmarks/snapshot-checkpoint/review-final-inputs.tar.gz"
        if hashlib.sha256(archive.read_bytes()).hexdigest() != ARCHIVE:
            raise RuntimeError("baseline archive identity differs")
        baseline = scratch/"baseline"
        baseline.mkdir()
        with tarfile.open(archive) as tar:
            tar.extractall(baseline, filter="data")
        if source_digest(baseline) != BASELINE:
            raise RuntimeError("baseline source identity differs")
        roots = {"baseline": baseline, "candidate": source}
        report["comparison_inputs"] = {name: inputs(root, out) if name == "candidate" else comparison_inputs(root)
                                       for name, root in roots.items()}
        report["allowed_input_differences"] = require_matching_inputs(report["comparison_inputs"], ALLOWED_TEST_DIFFERENCES)
        binaries = {}
        for name, root in roots.items():
            binary = scratch/f"{name}.test"
            run(["go", "test", "-c", "-o", str(binary), "./cmd/errand"], root, env, out/f"build-{name}.txt")
            binaries[name] = binary
        report["binaries"] = {name: hashlib.sha256(p.read_bytes()).hexdigest() for name, p in binaries.items()}
        packages = ["./cmd/errand", "./internal/client", "./internal/daemon"]
        run(["go", "test", "-timeout=10m", *packages], source, env, out/"tests.txt")
        run(["go", "vet", *packages], source, env, out/"vet.txt")
        run(["go", "test", "-race", "-timeout=10m", *packages], source, dict(env, CGO_ENABLED="1"), out/"race.txt")
        cases = {
            "workspace-create": "^BenchmarkWorkspaceCreationAndSubmission$/^workspace-create$",
            "ephemeral-job": "^BenchmarkWorkspaceCreationAndSubmission$/^ephemeral-job$",
            "push": "^BenchmarkPushPhases$",
            "watch": "^BenchmarkWatchPhases$",
            "fetch-ephemeral": "^BenchmarkFetchCompletion$/^persistent=false$",
            "fetch-persistent": "^BenchmarkFetchCompletion$/^persistent=true$",
        }
        for pair in range(args.rounds):
            for case, pattern in cases.items():
                run(["sync"], source, env, out/"sync.txt")
                for position, name in enumerate(("baseline", "candidate") if pair % 2 == 0 else ("candidate", "baseline"), 1):
                    report["active"] = dict(pair=pair, case=case, variant=name, position=position)
                    raw = run([str(binaries[name]), "-test.run=^$", "-test.bench="+pattern,
                               "-test.benchtime=3x", "-test.benchmem", "-test.timeout=5m"],
                              roots[name], env, out/f"{pair}-{case}-{name}.txt", timeout=300)
                    sample = parse_sample(raw, pair)
                    sample.update(report["active"])
                    report["samples"].append(sample)
                    report.pop("active")
                    (out/"report.json").write_text(json.dumps(report, indent=2)+"\n")
                print(f"pair {pair+1}/{args.rounds}: {case}", flush=True)
        # Same-binary fallback control isolates disabled observation overhead.
        fallback = scratch/"snapshot.test"
        run(["go", "test", "-c", "-o", str(fallback), "./internal/snapshot"], source, env, out/"build-fallback.txt")
        report["fallback_binary"] = hashlib.sha256(fallback.read_bytes()).hexdigest()
        report["fallback_samples"] = []
        for pair in range(args.rounds):
            run(["sync"], source, env, out/"sync.txt")
            for name in (("ordinary", "disabled") if pair % 2 == 0 else ("disabled", "ordinary")):
                raw = run([str(fallback), "-test.run=^$", f"-test.bench=^BenchmarkObservationBypass$/^{name}$",
                           "-test.benchtime=3x", "-test.benchmem"], source, env, out/f"fallback-{pair}-{name}.txt", timeout=300)
                sample = parse_sample(raw, pair)
                sample.update(pair=pair, variant=name)
                report["fallback_samples"].append(sample)
        if source_digest(baseline) != BASELINE:
            raise RuntimeError("baseline changed during measurement")
        report["final_source_sha256"] = candidate_digest(source, out)
        report["final_harness_inputs"] = harness_inputs(source/"scripts")
        if report["final_source_sha256"] != report["source_sha256"] or report["final_harness_inputs"] != report["harness_inputs"]:
            raise RuntimeError("candidate inputs changed during measurement")


if __name__ == "__main__":
    main()
