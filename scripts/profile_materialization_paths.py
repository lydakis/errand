#!/usr/bin/env python3
"""Count retained-parent decisions with a temporary Go overlay, outside timing runs."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import platform
import re

from benchmark_snapshot_integration import benchmark_campaign, filesystem, parse_sample, run, source_digest
from snapshot_provenance import harness_inputs


def instrument(source):
    replacements = [
        ('"sync"', '"sync"\n\t"sync/atomic"'),
        ('type materializationPaths struct {',
         'type materializationPaths struct {\n\tdirect, hits, opens, fallback atomic.Int64'),
        ('return p.root, name, nil, nil\n\t}',
         'p.direct.Add(1)\n\t\treturn p.root, name, nil, nil\n\t}'),
        ('if parent := p.parents[key]; parent != nil {\n\t\tparent.used',
         'if parent := p.parents[key]; parent != nil {\n\t\tp.hits.Add(1)\n\t\tparent.used'),
        ('if oldest == nil {', 'if oldest == nil {\n\t\t\tp.fallback.Add(1)'),
        ('root, err := base.OpenRoot(rel)', 'p.opens.Add(1)\n\troot, err := base.OpenRoot(rel)'),
        ('func (p *materializationPaths) close() error {',
         'func (p *materializationPaths) close() error {\n'
         '\tfmt.Fprintf(os.Stderr, "PARENT_COUNTS source=%t direct=%d hits=%d opens=%d fallback=%d\\n", '
         'p.verify, p.direct.Load(), p.hits.Load(), p.opens.Load(), p.fallback.Load())'),
    ]
    for before, after in replacements:
        if source.count(before) != 1:
            raise RuntimeError(f"overlay anchor must occur exactly once: {before}")
        source = source.replace(before, after)
    return source


def parse_diagnostic(raw, shape, iterations):
    sample = parse_sample(raw, 0)
    if sample["name"] != f"BenchmarkCaptureWorkspaceBase/{shape}" or sample["iterations"] != iterations:
        raise RuntimeError(f"Expected {iterations} captures of {shape}, got {sample}")
    counters = []
    for line in raw.splitlines():
        if not line.startswith("PARENT_COUNTS"):
            continue
        match = re.fullmatch(r"PARENT_COUNTS source=(true|false) direct=(\d+) hits=(\d+) opens=(\d+) fallback=(\d+)", line)
        if match is None:
            raise RuntimeError(f"Malformed diagnostic counter: {line}")
        counters.append(dict(source=match[1] == "true", **dict(zip(
            ("direct", "hits", "opens", "fallback"), map(int, match.groups()[1:])))))
    for source in (True, False):
        if sum(counter["source"] == source for counter in counters) != iterations:
            raise RuntimeError(f"Expected {iterations} counter records for source={source}")
    return dict(sample=sample, counters=counters)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--source", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    root, out = args.source.resolve(), args.output.resolve()
    if out.is_relative_to(root):
        parser.error("output must be outside the source tree")
    out.mkdir(parents=True, exist_ok=False)
    original = root / "internal/changes/materialize_paths.go"
    report = dict(scope="Diagnostic counts only; atomic instrumentation and logging alter timings.",
                  platform=platform.platform(), machine=platform.machine(),
                  python=platform.python_version(), gomaxprocs=2, diagnostics={})
    env = dict(os.environ, GOMAXPROCS="2", CGO_ENABLED="0")
    harness_root = Path(__file__).resolve().parent
    with benchmark_campaign(out, report) as scratch:
        report["source_sha256"] = source_digest(root)
        report["harness_inputs"] = harness_inputs(harness_root)
        instrumented = instrument(original.read_text())
        env["TMPDIR"] = str(scratch)
        report["fixture_filesystem"] = filesystem(scratch, env, out, "filesystem")
        mount_command = (["findmnt", "-T", str(scratch), "-n", "-o", "FSTYPE,OPTIONS"]
                         if platform.system() == "Linux" else ["mount"])
        report["fixture_mount"] = run(mount_command, scratch, env, out / "fixture-mount.txt").strip()
        report["toolchain"] = json.loads(run(
            ["go", "env", "-json", "GOVERSION", "GOOS", "GOARCH", "CGO_ENABLED", "GOFLAGS", "GOTOOLCHAIN", "GOEXPERIMENT"],
            root, env, out / "toolchain.json"))
        overlay_source = scratch / "materialize_paths.go"
        overlay_source.write_text(instrumented)
        run(["gofmt", "-w", str(overlay_source)], root, env, out / "gofmt.txt")
        (out / "instrumented.go.txt").write_bytes(overlay_source.read_bytes())
        overlay = scratch / "overlay.json"
        overlay.write_text(json.dumps({"Replace": {str(original): str(overlay_source)}}))
        binary = scratch / "changes.test"
        run(["go", "test", "-overlay", str(overlay), "-c", "-o", str(binary), "./internal/changes"],
            root, env, out / "build.txt")
        report["binary_sha256"] = hashlib.sha256(binary.read_bytes()).hexdigest()
        for shape in ("32-deep", "8-deep-wide"):
            report["active"] = shape
            raw = run([str(binary), "-test.run=^$", f"-test.bench=^BenchmarkCaptureWorkspaceBase$/^{shape}$",
                      "-test.benchtime=3x"], root, env, out / f"{shape}.txt", timeout=300)
            report["diagnostics"][shape] = parse_diagnostic(raw, shape, 3)
            report.pop("active")
        if source_digest(root) != report["source_sha256"]:
            raise RuntimeError("source changed")
        if harness_inputs(harness_root) != report["harness_inputs"]:
            raise RuntimeError("harness changed")


if __name__ == "__main__":
    main()
