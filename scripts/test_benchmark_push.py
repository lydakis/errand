import json
import os
from pathlib import Path
import sys
import tempfile
import time
import unittest
from concurrent.futures import ThreadPoolExecutor

import benchmark_push


class BenchmarkPushTests(unittest.TestCase):
    def command(self, source, timeout=10):
        return benchmark_push.run(sys.executable, Path.cwd(), os.environ,
                                  timeout, "-c", source)

    def test_command_failure_preserves_bounded_output_and_exit(self):
        with self.assertRaises(benchmark_push.CommandFailure) as caught:
            self.command("import sys; print('x' * 10000); "
                         "print('diagnostic', file=sys.stderr); sys.exit(7)")
        details = caught.exception.details
        self.assertEqual(details["exit_code"], 7)
        self.assertFalse(details["timed_out"])
        self.assertEqual(len(details["stdout"]["tail"]), 8192)
        self.assertTrue(details["stdout"]["truncated"])
        self.assertEqual(details["stderr"]["tail"], "diagnostic\n")
        self.assertGreater(details["seconds"], 0)
        self.assertEqual(details["command"][:2], [sys.executable, "-c"])
        self.assertNotIn("env", details)

    def test_timeout_preserves_partial_output(self):
        with self.assertRaises(benchmark_push.CommandFailure) as caught:
            self.command("import time; print('started', flush=True); time.sleep(30)", timeout=1)
        details = caught.exception.details
        self.assertTrue(details["timed_out"])
        self.assertIsNone(details["exit_code"])
        self.assertEqual(details["stdout"]["tail"], "started\n")
        self.assertGreaterEqual(details["seconds"], 1)

    def test_invalid_json_preserves_diagnostics(self):
        with self.assertRaises(benchmark_push.CommandFailure) as caught:
            self.command("print('invalid receipt')")
        self.assertEqual(caught.exception.details["exit_code"], 0)
        self.assertEqual(caught.exception.details["stdout"]["tail"], "invalid receipt\n")
        receipt, elapsed = self.command('print(\'{"ok": true}\')')
        self.assertEqual(receipt, {"ok": True})
        self.assertGreater(elapsed, 0)

    def test_failed_round_saves_successful_sibling_and_failure_identity(self):
        report = dict(complete=False, samples=[], rounds=[])

        def fail():
            self.command("import sys; print('bad workspace', file=sys.stderr); sys.exit(3)")

        with tempfile.TemporaryDirectory() as directory, ThreadPoolExecutor(2) as pool:
            output = Path(directory)
            futures = [("failed", pool.submit(fail)),
                       ("successful", pool.submit(dict, workspace="successful", seconds=.1))]
            with self.assertRaisesRegex(RuntimeError, "push round failed"):
                benchmark_push.finish_round(futures, report, output, 2, "edit", time.monotonic())
            saved = json.loads((output / "report.json").read_text())
        self.assertFalse(saved["complete"])
        self.assertFalse(saved["rounds"][0]["complete"])
        self.assertEqual(saved["samples"], [{"workspace": "successful", "seconds": .1}])
        failure = saved["failures"][0]
        self.assertEqual((failure["workspace"], failure["sample"], failure["case"]),
                         ("failed", 2, "edit"))
        self.assertEqual(failure["exit_code"], 3)
        self.assertEqual(failure["stderr"]["tail"], "bad workspace\n")


if __name__ == "__main__":
    unittest.main()
