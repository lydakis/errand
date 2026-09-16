"""Compare watch retry changes with a committed baseline on the current volume.

Run from the repository root. Both binaries use the current benchmark tests.
Only the six production files changed by this slice are replaced in the control.
"""

import argparse
import json
import os
import pathlib
import subprocess
import tempfile
import time


parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("baseline")
parser.add_argument("--rounds", type=int, default=4)
parser.add_argument("--iterations", type=int, default=3)
parser.add_argument("--output", default="watch-benchmark-results.json")
args = parser.parse_args()
root = pathlib.Path.cwd()
paths = [
    "internal/archive/archive.go",
    "internal/changes/transfer_snapshot.go",
    "internal/client/push_watch.go",
    "internal/snapshot/builder.go",
    "internal/snapshot/snapshot.go",
    "internal/snapshot/watch_prepare.go",
]
report = {"baseline": args.baseline, "runs": []}
with tempfile.TemporaryDirectory(prefix=".watch-bench-", dir=root) as tmp:
    directory = pathlib.Path(tmp)
    env = dict(os.environ, TMPDIR=tmp)
    replacements = {}
    for i, path in enumerate(paths):
        target = directory / f"{i}.go"
        target.write_bytes(subprocess.check_output(["git", "show", f"{args.baseline}:{path}"]))
        replacements[str(root / path)] = str(target)
    overlay = directory / "overlay.json"
    overlay.write_text(json.dumps({"Replace": replacements}))
    for variant in ["baseline", "candidate"]:
        command = ["go", "test", "-c", "-o", str(directory / variant)]
        if variant == "baseline":
            command += ["-overlay", str(overlay)]
        subprocess.run(command + ["./cmd/errand"], env=env, check=True)
    for pair in range(args.rounds):
        order = ["baseline", "candidate"] if pair % 2 == 0 else ["candidate", "baseline"]
        for variant in order:
            started = time.monotonic()
            result = subprocess.run(
                [str(directory / variant), "-test.run=^$",
                 "-test.bench=^Benchmark(PushPhases|WatchPhases|WatchBurst)$",
                 f"-test.benchtime={args.iterations}x", "-test.benchmem", "-test.timeout=5m"],
                env=env, capture_output=True, text=True,
            )
            row = {"pair": pair, "variant": variant, "exit": result.returncode,
                   "wall_seconds": time.monotonic() - started,
                   "stdout": result.stdout, "stderr": result.stderr}
            report["runs"].append(row)
            pathlib.Path(args.output).write_text(json.dumps(report, indent=2) + "\n")
            print(json.dumps({k: v for k, v in row.items() if k not in ["stdout", "stderr"]}), flush=True)
            print("\n".join(line for line in result.stdout.splitlines() if "ns/op" in line), flush=True)
            result.check_returncode()
