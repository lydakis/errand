from pathlib import Path
import json
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

import profile_materialization_paths as profile


class MaterializationDiagnosticsTests(unittest.TestCase):
    def test_input_mismatch_retains_failed_comparison_report(self):
        script = Path(__file__).with_name("benchmark_materialization.py")
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            for variant in ("baseline", "candidate"):
                (root / variant).mkdir()
                (root / variant / "fixture_test.go").write_text(variant)
            output = root / "results"
            result = subprocess.run(
                [sys.executable, str(script), "--baseline", str(root / "baseline"),
                 "--candidate", str(root / "candidate"), "--output", str(output)],
                capture_output=True, text=True, timeout=10)
            self.assertNotEqual(result.returncode, 0)
            report = json.loads((output / "report.json").read_text())
            self.assertEqual(report["status"], "failed")
            self.assertIn("fixture_test.go", report["failure"]["error"])
            self.assertFalse(list(output.glob(".scratch-*")))

    def test_empty_measurement_fails_campaign_and_retains_log(self):
        with tempfile.TemporaryDirectory() as directory:
            root, output = Path(directory) / "source", Path(directory) / "results"
            original = root / "internal/changes/materialize_paths.go"
            original.parent.mkdir(parents=True)
            original.write_bytes((Path(__file__).resolve().parents[1] /
                                  "internal/changes/materialize_paths.go").read_bytes())
            (root / "go.mod").write_text("module test\n")
            (root / "go.sum").touch()

            def execute(command, cwd, env, log, **kwargs):
                if command[:2] == ["go", "test"]:
                    Path(command[command.index("-o") + 1]).write_bytes(b"fake executable")
                raw = "{}" if command[:2] == ["go", "env"] else "PASS\n"
                log.write_text(raw)
                return raw

            with patch("sys.argv", ["profile", "--source", str(root), "--output", str(output)]), \
                 patch.object(profile, "filesystem", return_value="test"), \
                 patch.object(profile, "run", side_effect=execute):
                with self.assertRaisesRegex(RuntimeError, "Expected one benchmark sample"):
                    profile.main()
            report = json.loads((output / "report.json").read_text())
            self.assertEqual(report["status"], "failed")
            self.assertEqual(report["active"], "32-deep")
            self.assertIn("benchmark_snapshot_integration.py", report["harness_inputs"])
            self.assertEqual((output / "32-deep.txt").read_text(), "PASS\n")
            self.assertFalse(list(output.glob(".scratch-*")))

    def test_output_cannot_overlap_source_even_through_symlink(self):
        scripts = Path(__file__).resolve().parent
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory) / "source"
            root.mkdir()
            other = Path(directory) / "other-source"
            other.mkdir()
            alias = Path(directory) / "alias"
            alias.symlink_to(root, target_is_directory=True)
            configurations = (
                ("profile_materialization_paths.py", ["--source", str(root)]),
                ("benchmark_materialization.py", ["--baseline", str(root), "--candidate", str(other)]),
                ("benchmark_materialization.py", ["--baseline", str(other), "--candidate", str(root)]),
            )
            for driver, inputs in configurations:
                for output in (root, root / "results", alias / "results"):
                    with self.subTest(driver=driver, inputs=inputs, output=output):
                        result = subprocess.run(
                            [sys.executable, str(scripts / driver), *inputs, "--output", str(output)],
                            capture_output=True, text=True, timeout=10)
                        self.assertEqual(result.returncode, 2)
                        self.assertIn("output must be outside", result.stderr)
                        self.assertEqual(list(root.iterdir()), [])

    def test_complete_diagnostic_output_is_required(self):
        counter = "PARENT_COUNTS source={} direct=18 hits=200 opens=40 fallback=32\n"
        source, destination = counter.format("true"), counter.format("false")
        # Counter output can interrupt Go's benchmark-name/sample line.
        prefix = "BenchmarkCaptureWorkspaceBase/32-deep-2\n"
        sample = "       3 213307430 ns/op 11318789 B/op 28731 allocs/op\nPASS\n"
        raw = prefix + (source + destination) * 3 + sample
        result = profile.parse_diagnostic(raw, "32-deep", 3)
        self.assertEqual(result["sample"]["iterations"], 3)
        self.assertEqual(len(result["counters"]), 6)
        self.assertEqual(result["counters"][0]["opens"], 40)
        self.assertIs(result["counters"][0]["source"], True)
        invalid = {
            "no benchmark": "PASS\n",
            "wrong benchmark": raw.replace("32-deep", "8-deep-wide"),
            "wrong iterations": raw.replace("       3 ", "       1 "),
            "no counters": prefix + sample,
            "missing adapter": raw.replace(destination, source),
            "missing capture": prefix + (source + destination) * 2 + sample,
            "malformed counter": raw.replace("opens=40", "opens=bad", 1),
            "duplicate sample": raw + prefix + sample,
        }
        for label, output in invalid.items():
            with self.subTest(label=label), self.assertRaises(RuntimeError):
                profile.parse_diagnostic(output, "32-deep", 3)


if __name__ == "__main__":
    unittest.main()
