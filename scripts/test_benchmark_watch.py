import argparse
from pathlib import Path
import tempfile
import unittest

import benchmark_watch


class DeletionPreconditionTest(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        self.source = Path(directory.name) / "source"
        self.destination = Path(directory.name) / "destination"
        self.source.mkdir()
        self.destination.mkdir()
        self.args = argparse.Namespace(change="delete", save_mode="inplace")
        self.name = "victim-watch-0.txt"
        (self.source / self.name).write_text(benchmark_watch.victim_body("watch", 0))

    def apply(self):
        return benchmark_watch.apply_change(self.args, self.source, self.destination, 0, "watch")

    def test_absent_destination_victim_fails_before_unlink(self):
        with self.assertRaisesRegex(RuntimeError, "absent from destination"):
            self.apply()
        self.assertTrue((self.source / self.name).exists())

    def test_stale_destination_victim_fails_before_unlink(self):
        (self.destination / self.name).write_text("victim watch 1\n")
        with self.assertRaisesRegex(RuntimeError, "differs at destination"):
            self.apply()
        self.assertTrue((self.source / self.name).exists())

    def test_present_destination_victim_is_timed_until_removed(self):
        (self.destination / self.name).write_text("victim watch 0\n")
        started, check, name = self.apply()
        self.assertEqual(name, self.name)
        self.assertFalse((self.source / self.name).exists())
        (self.destination / self.name).unlink()
        self.assertGreaterEqual(check(), started)


if __name__ == "__main__":
    unittest.main()
