import os
from pathlib import Path
import subprocess
import tempfile
import time
import tomllib
import unittest
from unittest import mock

from benchmark_loop import (Lines, Loop, NoOutput, burst_receipts, observer_command, peer_config, summarize,
                            write_fixture)


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

    def test_timeout_ends_children_that_hold_the_pipes(self):
        # Like an SSH transport outliving the client it serves.
        started = time.monotonic()
        with self.assertRaisesRegex(ValueError, "timed out"):
            self.loop.run(self.cwd, "-c", "sleep 30 & sleep 30")
        self.assertLess(time.monotonic() - started, 10)


class LinesTest(unittest.TestCase):
    def test_stop_kills_a_process_that_ignores_ctrl_c(self):
        with tempfile.TemporaryDirectory() as cwd:
            lines = Lines(["sh", "-c", "trap '' INT; echo up; sleep 30"], cwd, dict(os.environ))
            self.assertEqual(lines.next(10)[1], "up")
            self.assertIsNone(lines.stop(0.5))
            self.assertIsNotNone(lines.process.poll())
            self.assertEqual(lines.remaining(), [])


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


class FixtureTest(unittest.TestCase):
    def test_commit_ignores_the_users_git_configuration(self):
        with tempfile.TemporaryDirectory() as home:
            hooks = Path(home, "hooks")
            hooks.mkdir()
            (hooks / "pre-commit").write_text("#!/bin/sh\nexit 1\n")
            (hooks / "pre-commit").chmod(0o755)
            Path(home, "ignore").write_text("*.txt\n")
            # Signing with a program that always fails, as a locked key would.
            Path(home, ".gitconfig").write_text(f"[commit]\n\tgpgsign = true\n[gpg]\n\tprogram = false\n"
                                                f"[core]\n\texcludesFile = {home}/ignore\n\thooksPath = {hooks}\n")
            with mock.patch.dict(os.environ, HOME=home, XDG_CONFIG_HOME=home, GIT_CONFIG_NOSYSTEM="1"):
                write_fixture(Path(home, "repo"), 3, "n")
            tracked = subprocess.run(["git", "-C", str(Path(home, "repo")), "ls-files"],
                                     capture_output=True, text=True, check=True)
            self.assertEqual(len(tracked.stdout.splitlines()), 4)


class PeerConfigTest(unittest.TestCase):
    def test_keeps_only_the_selected_peers(self):
        with tempfile.TemporaryDirectory() as root:
            personal = Path(root, "config.toml")
            personal.write_text('apply_on_success = true\n[env]\npass = ["UNSET_FOR_TEST"]\n'
                                '[session]\nforward = ["8080"]\n[peers.cabal]\nurl = "http://cabal:7443"\n'
                                '[peers."mac mini"]\nssh = "mini"\nremote_command = "/opt/errand"\n')
            target = Path(root, "scratch", "errand", "config.toml")
            peer_config(personal, target, ["mac mini"])
            self.assertEqual(tomllib.loads(target.read_text()),
                             {"peers": {"mac mini": {"ssh": "mini", "remote_command": "/opt/errand"}}})
            with self.assertRaisesRegex(ValueError, "not configured"):
                peer_config(personal, Path(root, "other", "config.toml"), ["absent"])


class BurstTest(unittest.TestCase):
    @staticmethod
    def receipts(stamps):
        """Return receipts arriving at `stamps`, timed out the way the watch reader is."""
        stamps, now = list(stamps), [0.0]

        def receipt(timeout=600):
            if not stamps or stamps[0] - now[0] > timeout:
                raise NoOutput("quiet")
            now[0] = stamps.pop(0)
            return now[0], {}
        return receipt

    def test_waits_through_gaps_as_long_as_the_pushes(self):
        # Pushes of about 1.5 s: a fixed one-second silence would stop after the first.
        self.assertEqual(burst_receipts(self.receipts([1.5, 3.0, 4.6]), 0, 1), (4.6, 3))

    def test_stops_at_a_silence_longer_than_twice_the_slowest_push(self):
        self.assertEqual(burst_receipts(self.receipts([0.3, 0.6, 2.0]), 0, 1), (0.6, 2))


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
