import os
from pathlib import Path
import subprocess
import tempfile
import unittest

from benchmark_loop import Lines, Loop, observer_command, summarize


class LoopRunTest(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        self.cwd = Path(directory.name)
        self.loop = Loop("/bin/sh", dict(os.environ), 2)

    def test_first_output_is_timed_before_exit(self):
        row, stdout, _ = self.loop.run(self.cwd, "-c", "echo ready; sleep 0.2", marker="ready")
        self.assertEqual(stdout, "ready\n")
        self.assertLess(row["first_output_ms"] + 150, row["wall_ms"])

    def test_missing_marker_failure_and_timeout_invalidate_the_sample(self):
        for args, message in [(("-c", "echo not-ready"), "never reached stdout"),
                              (("-c", "echo oops >&2; exit 3"), "exited 3: oops"),
                              (("-c", "sleep 5"), "timed out")]:
            with self.subTest(args=args), self.assertRaisesRegex(ValueError, message):
                self.loop.run(self.cwd, *args, marker="ready")


class ObserverTest(unittest.TestCase):
    def test_remote_poller_finds_the_default_state_directory_and_reports_saves(self):
        with tempfile.TemporaryDirectory() as home:
            data = Path(home) / ".errand" / "workspaces" / "ws 1" / "data"
            data.mkdir(parents=True)
            command = observer_command(("cabal", "~/.errand"), "ws 1", 5)
            self.assertEqual(command[:3], ["ssh", "-T", "cabal"])
            # Run the remote command the way sshd would: through the user's shell.
            poller = Lines(["sh", "-c", command[-1]], home, dict(os.environ, HOME=home), stdin=subprocess.PIPE)
            try:
                self.assertEqual(poller.next(10)[1], "ready")
                poller.send("body 'quoted'")
                (data / "edit.txt").write_text("body 'quoted'\n")
                self.assertEqual(poller.next(10)[1], "seen")
            finally:
                poller.process.stdin.close()
                poller.process.wait(timeout=10)

    def test_missing_workspace_copy_is_reported(self):
        with tempfile.TemporaryDirectory() as root:
            poller = Lines(observer_command((None, root), "absent", 5), root, dict(os.environ), stdin=subprocess.PIPE)
            try:
                self.assertTrue(poller.next(10)[1].startswith("missing "))
            finally:
                poller.process.stdin.close()
                poller.process.wait(timeout=10)


class SummaryTest(unittest.TestCase):
    def test_groups_by_peer_size_and_case_with_tails(self):
        samples = [dict(peer="cabal", files=1000, case="job-warm", sample=i, wall_ms=float(i + 1),
                        client_cpu_ms=1.0, first_output_ms=0.5) for i in range(20)]
        samples.append(dict(peer="cabal", files=10000, case="job-warm", sample=0, wall_ms=50.0, client_cpu_ms=2.0))
        rows = {row["files"]: row for row in summarize(samples)}
        self.assertEqual(rows[1000]["samples"], 20)
        self.assertEqual(rows[1000]["wall_median_ms"], 10.5)
        self.assertEqual(rows[1000]["wall_p95_ms"], 19.0)
        self.assertEqual((rows[1000]["wall_min_ms"], rows[1000]["wall_max_ms"]), (1.0, 20.0))
        self.assertEqual(rows[1000]["first_output_median_ms"], 0.5)
        self.assertNotIn("first_output_median_ms", rows[10000])


if __name__ == "__main__":
    unittest.main()
