import json
from contextlib import nullcontext
import os
from pathlib import Path
import signal
import subprocess
import sys
import tempfile
import time
import unittest
from unittest.mock import patch

from benchmark_snapshot_integration import benchmark_scratch, main
import profile_transfer_latency


class BenchmarkScratchTests(unittest.TestCase):
    def test_both_drivers_report_campaign_and_cleanup_outcomes(self):
        for driver in ("benchmark", "profile"):
            for primary, cleanup in ((RuntimeError("campaign failed"), OSError("cleanup failed")),
                                     (KeyboardInterrupt("interrupted"), OSError("cleanup failed")),
                                     (None, OSError("cleanup failed")), (None, None)):
                with self.subTest(driver=driver, primary=primary, cleanup=cleanup), tempfile.TemporaryDirectory() as directory:
                    output = Path(directory) / "results"
                    original_handler = signal.getsignal(signal.SIGTERM)
                    if driver == "benchmark":
                        entry = main
                        argv = ["benchmark", "--baseline", directory, "--output", str(output), "--revision", "test"]
                        body = patch("benchmark_snapshot_integration.run_campaign", side_effect=primary)
                    else:
                        entry = profile_transfer_latency.main
                        # Zero cases let the successful body reach cleanup without Go.
                        argv = ["profile", "--output", str(output)]
                        body = patch("profile_transfer_latency.run", side_effect=primary)
                    cleanup_fault = patch("benchmark_snapshot_integration.shutil.rmtree", side_effect=cleanup) if cleanup else nullcontext()
                    with patch("sys.argv", argv), body, cleanup_fault, \
                         patch("profile_transfer_latency.CASES", {}), \
                         patch("profile_transfer_latency.source_digest", return_value="test"), \
                         patch("profile_transfer_latency.filesystem", return_value="test"):
                        expected = primary if primary is not None else cleanup
                        if expected is None:
                            entry()
                        else:
                            with self.assertRaises(type(expected)) as caught:
                                entry()
                            self.assertIs(caught.exception, expected)
                    self.assertEqual(signal.getsignal(signal.SIGTERM), original_handler)
                    report = json.loads((output / "report.json").read_text())
                    if expected is None:
                        self.assertEqual(report["status"], "complete")
                        self.assertNotIn("failure", report)
                        self.assertFalse(list(output.glob(".scratch-*")))
                    else:
                        self.assertEqual(report["status"], "failed")
                        self.assertEqual(report["failure"]["error"], str(expected))
                        self.assertEqual(report["cleanup_failure"]["error"], str(cleanup))

    def test_profile_preserves_failed_case_and_continues(self):
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory) / "results"

            def execute(command, root, env, log, **kwargs):
                if log.name == "build.txt":
                    return ""
                if "push" in log.name:
                    log.write_text("partial benchmark output")
                    raise RuntimeError("benchmark failed")
                return "BenchmarkFetchBodies/large-2 1 1000 ns/op\n"

            with patch("sys.argv", ["profile", "--output", str(output)]), \
                 patch("profile_transfer_latency.run", side_effect=execute), \
                 patch("profile_transfer_latency.source_digest", return_value="test"), \
                 patch("profile_transfer_latency.filesystem", return_value="test"):
                with self.assertRaises(SystemExit) as caught:
                    profile_transfer_latency.main()
                self.assertEqual(caught.exception.code, 1)
            report = json.loads((output / "report.json").read_text())
            self.assertEqual(report["status"], "failed")
            self.assertEqual(len(report["observations"]), 2)
            self.assertEqual(report["observations"][0]["error"], "benchmark failed")
            self.assertIn("sample", report["observations"][1])
            self.assertFalse(list(output.glob(".scratch-*")))

    def test_sigterm_stops_child_cleans_scratch_and_records_failure(self):
        for driver in ("benchmark", "profile"):
            with self.subTest(driver=driver), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                output, pidfile = root / "results", root / "child.pid"
                child = ("import os,pathlib,time; print('started',flush=True); "
                         f"p=pathlib.Path({str(pidfile)!r}); "
                         "p.with_suffix('.tmp').write_text(str(os.getpid())); "
                         "p.with_suffix('.tmp').rename(p); time.sleep(60)")
                script = (
                    "import pathlib,sys\nfrom unittest.mock import patch\n"
                    f"sys.path.insert(0,{str(Path(__file__).resolve().parent)!r})\n"
                    "import benchmark_snapshot_integration as benchmark\n"
                    "import profile_transfer_latency as profile\n"
                    "original_run=benchmark.run\n"
                    "def wait_child(command,root,env,log,**kwargs):\n"
                    f"    return original_run([sys.executable,'-c',{child!r}],root,env,log,timeout=30)\n"
                    "def campaign(args,roots,cases,output,env,report,scratch):\n"
                    "    report['active']={'case':'probe'}\n"
                    "    wait_child([],scratch,env,output/'run.txt')\n"
                    f"sys.argv=['probe','--output',{str(output)!r}]\n"
                    f"if {driver!r}=='benchmark':\n"
                    f"    sys.argv += ['--baseline',{directory!r},'--revision','test']\n"
                    "    with patch.object(benchmark,'run_campaign',campaign): benchmark.main()\n"
                    "else:\n"
                    "    with patch.object(profile,'run',wait_child), "
                    "patch.object(profile,'filesystem',return_value='test'), "
                    "patch.object(profile,'source_digest',return_value='test'): profile.main()\n"
                )
                process = subprocess.Popen([sys.executable, "-c", script], start_new_session=True)
                child_pid = None
                try:
                    deadline = time.monotonic() + 5
                    while not pidfile.exists() and time.monotonic() < deadline:
                        time.sleep(0.01)
                    self.assertTrue(pidfile.exists(), "child did not start")
                    child_pid = int(pidfile.read_text())
                    os.kill(process.pid, signal.SIGTERM)
                    self.assertEqual(process.wait(timeout=5), 128 + signal.SIGTERM)
                    with self.assertRaises(ProcessLookupError):
                        os.kill(child_pid, 0)
                    self.assertFalse(list(output.glob(".scratch-*")))
                    report = json.loads((output / "report.json").read_text())
                    self.assertEqual(report["status"], "failed")
                    self.assertEqual(report["failure"]["type"], "SystemExit")
                    log = output / ("run.txt" if driver == "benchmark" else "build.txt")
                    self.assertIn("started", log.read_text())
                finally:
                    if process.poll() is None:
                        os.killpg(process.pid, signal.SIGKILL)
                        process.wait()
                    if child_pid is not None:
                        try:
                            os.killpg(child_pid, signal.SIGKILL)
                        except ProcessLookupError:
                            pass

    def test_campaign_failure_retains_status_and_log(self):
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory) / "results"

            def fail(args, roots, cases, output, env, report, scratch):
                (scratch / "benchmark.test").write_text("binary")
                (output / "run.txt").write_text("partial output")
                report["active"] = dict(variant="baseline", round=0, case="push")
                raise RuntimeError("injected campaign failure")

            argv = ["benchmark", "--baseline", directory, "--output", str(output), "--revision", "test"]
            with patch("sys.argv", argv), patch("benchmark_snapshot_integration.run_campaign", fail):
                with self.assertRaisesRegex(RuntimeError, "injected campaign failure"):
                    main()
            report = json.loads((output / "report.json").read_text())
            self.assertEqual(report["status"], "failed")
            self.assertEqual(report["active"]["case"], "push")
            self.assertEqual((output / "run.txt").read_text(), "partial output")
            self.assertEqual(sorted(p.name for p in output.iterdir()), ["report.json", "run.txt"])

    def test_failure_removes_scratch_but_preserves_diagnostics(self):
        for failure in (RuntimeError, KeyboardInterrupt):
            with self.subTest(failure=failure), tempfile.TemporaryDirectory() as directory:
                output = Path(directory)
                log = output / "run.txt"
                log.write_text("partial output")
                outside = output / "retained"
                outside.mkdir()
                (outside / "report.json").write_text("{}")
                with self.assertRaises(failure):
                    with benchmark_scratch(output) as scratch:
                        locked = scratch / "restricted"
                        locked.mkdir()
                        (locked / "body").write_text("fixture")
                        (scratch / "external").symlink_to(outside, target_is_directory=True)
                        (scratch / "benchmark.test").write_text("binary")
                        os.chmod(locked, 0)
                        raise failure("injected failure")
                self.assertFalse(scratch.exists())
                self.assertEqual(log.read_text(), "partial output")
                self.assertEqual((outside / "report.json").read_text(), "{}")


if __name__ == "__main__":
    unittest.main()
