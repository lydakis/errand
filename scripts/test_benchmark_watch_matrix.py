import argparse
import contextlib
import io
import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest import mock

import benchmark_watch_matrix
from benchmark_watch_matrix import summarize


def write_report(root, name, complete, watch_ms):
    target = root / name
    target.mkdir()
    samples = [dict(mode="watch", sample=i, seconds=ms/1000, delivery_seconds=ms/1000) for i, ms in enumerate(watch_ms)]
    (target / "report.json").write_text(json.dumps(dict(complete=complete, samples=samples)))


class WatchMatrixSummaryTest(unittest.TestCase):
    def test_incomplete_reports_are_excluded_from_paired_ratios(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            write_report(root, "r0-1000-explicit-inplace-edit@baseline", True, [100, 100, 100])
            write_report(root, "r0-1000-explicit-inplace-edit@candidate", True, [50, 50, 50])
            # A run that failed after collecting watch samples has a partial median.
            write_report(root, "r1-1000-explicit-inplace-edit@baseline", True, [100, 100, 100])
            write_report(root, "r1-1000-explicit-inplace-edit@candidate", False, [10])
            # A size whose only candidate run is incomplete has no valid pair.
            write_report(root, "r0-10000-explicit-inplace-edit@baseline", True, [300])
            write_report(root, "r0-10000-explicit-inplace-edit@candidate", False, [30])
            summary = summarize(root)
        per_case, _, paired = summary.partition("Paired ratio")
        self.assertIn("| 1,000 | explicit-inplace-edit@candidate | 50 (50–50) |", per_case)
        self.assertIn("| 3 (1 incomplete) |", per_case)
        paired_rows = [line for line in paired.splitlines() if line.startswith("| 1")]
        self.assertEqual(paired_rows, ["| 1,000 | explicit-inplace-edit | 100 | 50 | 0.500 (0.500–0.500) | 1/1 |"])


class WatchMatrixCommandTest(unittest.TestCase):
    def commands(self, baseline, setup=None, check=None):
        """Return each variant's benchmark_watch command, keyed by output name."""
        commands = {}
        def run(cmd, **_):
            commands[Path(cmd[cmd.index("--output") + 1]).name] = cmd
            return subprocess.CompletedProcess(cmd, 0, "", "")
        with tempfile.TemporaryDirectory() as directory:
            args = argparse.Namespace(output=Path(directory), sizes=[1000],
                                      cases=["explicit-inplace-edit", "explicit-inplace-delete"],
                                      binary="/candidate", baseline=baseline, mutagen="/mutagen",
                                      rounds=2, samples=1, pause_seconds=0)
            if setup:
                setup(args.output)
            with mock.patch.object(benchmark_watch_matrix.subprocess, "run", run), \
                    contextlib.redirect_stdout(io.StringIO()):
                benchmark_watch_matrix.run_matrix(args)
            if check:
                check(args.output)
        return commands

    def test_rerun_retries_incomplete_cells_and_keeps_complete_ones(self):
        def setup(root):
            write_report(root, "r0-1000-explicit-inplace-edit", True, [100])
            write_report(root, "r0-1000-explicit-inplace-delete", False, [100])
            # A run that died before writing any report.
            (root / "r1-1000-explicit-inplace-edit").mkdir()
        def check(root):
            aside = sorted(p.name.rsplit(".", 1)[0] for p in (root / "incomplete").iterdir())
            self.assertEqual(aside, ["r0-1000-explicit-inplace-delete", "r1-1000-explicit-inplace-edit"])
            self.assertFalse((root / "r0-1000-explicit-inplace-delete").exists())
        commands = self.commands(None, setup, check)
        self.assertEqual(sorted(commands), ["r0-1000-explicit-inplace-delete", "r1-1000-explicit-inplace-delete",
                                            "r1-1000-explicit-inplace-edit"])

    def mutagen(self, cmd):
        return cmd[cmd.index("--mutagen") + 1] if "--mutagen" in cmd else None

    def test_paired_campaign_runs_mutagen_beside_first_round_candidate(self):
        commands = self.commands("/baseline")
        self.assertEqual(len(commands), 8)
        for name, cmd in commands.items():
            candidate = name.endswith("@candidate")
            binary = cmd[cmd.index("--binary") + 1]
            self.assertEqual(binary, "/candidate" if candidate else "/baseline", name)
            want = "/mutagen" if candidate and name.startswith("r0-") else None
            self.assertEqual(self.mutagen(cmd), want, name)

    def test_unpaired_campaign_runs_mutagen_in_first_round(self):
        commands = self.commands(None)
        self.assertEqual(len(commands), 4)
        for name, cmd in commands.items():
            want = "/mutagen" if name.startswith("r0-") else None
            self.assertEqual(self.mutagen(cmd), want, name)


if __name__ == "__main__":
    unittest.main()
