import itertools
import os
from pathlib import Path
import signal
import subprocess
import sys
import tempfile
import time
import unittest

from benchmark_snapshot_integration import benchmark_order, parse_sample, run


class SnapshotBenchmarkOrderTests(unittest.TestCase):
    def test_parse_logged_and_split_benchmark_output(self):
        for gap in (" ", "\nEVALUATION {}\n"):
            sample = parse_sample(f"BenchmarkFetch/body-2{gap}3 120 ns/op 4 B/op\nPASS\n", 2)
            self.assertEqual(sample["iterations"], 3)
            self.assertEqual(sample["metrics"]["ns/op"], 120)
        with self.assertRaises(RuntimeError):
            parse_sample("BenchmarkA-2 1 120 ns/op\nBenchmarkB-2 1 130 ns/op", 0)

    def test_outer_timeout_retains_partial_output(self):
        with tempfile.TemporaryDirectory() as directory:
            log = Path(directory) / "run.txt"
            with self.assertRaisesRegex(RuntimeError, "exceeded"):
                run([sys.executable, "-c", "import time; print('started', flush=True); time.sleep(60)"],
                    directory, os.environ, log, timeout=0.5)
            self.assertIn("started", log.read_text())

    def test_interrupt_stops_detached_child_and_preserves_log(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            pidfile, log = root / "child.pid", root / "run.txt"
            child = (f"import os, pathlib, time; print('started', flush=True); "
                     f"p = pathlib.Path({str(pidfile)!r}); "
                     "p.with_suffix('.tmp').write_text(str(os.getpid())); "
                     "p.with_suffix('.tmp').rename(p); time.sleep(60)")
            driver = (
                f"import os, pathlib, sys; sys.path.insert(0, {str(Path(__file__).resolve().parent)!r})\n"
                "from benchmark_snapshot_integration import run\n"
                "try:\n"
                f"    run([sys.executable, '-c', {child!r}], {directory!r}, os.environ, pathlib.Path({str(log)!r}))\n"
                "except KeyboardInterrupt:\n"
                "    sys.exit(130)\n"
            )
            process = subprocess.Popen([sys.executable, "-c", driver], start_new_session=True)
            child_pid = None
            try:
                deadline = time.monotonic() + 5
                while not pidfile.exists() and time.monotonic() < deadline:
                    time.sleep(0.01)
                self.assertTrue(pidfile.exists(), "child did not start")
                child_pid = int(pidfile.read_text())
                os.killpg(process.pid, signal.SIGINT)
                self.assertEqual(process.wait(timeout=5), 130)
                with self.assertRaises(ProcessLookupError):
                    os.kill(child_pid, 0)
                self.assertIn("started", log.read_text())
            finally:
                # Also clean up when exercising the pre-fix implementation.
                if process.poll() is None:
                    os.killpg(process.pid, signal.SIGKILL)
                    process.wait()
                if child_pid is not None:
                    try:
                        os.kill(child_pid, signal.SIGKILL)
                    except ProcessLookupError:
                        pass

    def test_two_variants_alternate_across_seven_rounds(self):
        variants = ["baseline", "candidate"]
        orders = [benchmark_order(variants, number) for number in range(7)]
        self.assertEqual(orders, [
            ["baseline", "candidate"], ["candidate", "baseline"],
            ["baseline", "candidate"], ["candidate", "baseline"],
            ["baseline", "candidate"], ["candidate", "baseline"],
            ["baseline", "candidate"],
        ])
        self.assertEqual(variants, ["baseline", "candidate"])

    def test_three_variants_cover_every_order_before_repeating(self):
        variants = ["baseline", "candidate", "flat-control"]
        orders = [tuple(benchmark_order(variants, number)) for number in range(7)]
        self.assertEqual(set(orders[:6]), set(itertools.permutations(variants)))
        self.assertEqual(orders[6], orders[0])
        self.assertEqual(variants, ["baseline", "candidate", "flat-control"])


if __name__ == "__main__":
    unittest.main()
