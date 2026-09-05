import tempfile
from pathlib import Path
import unittest

from benchmark import parse_transfer, summarize, write_fixture


class BenchmarkTest(unittest.TestCase):
    def test_transfer_classification_requires_observed_cache_behavior(self):
        snapshot = "errand: snapshot contains 2 files, 16 bytes\n"
        cold = snapshot + "errand: shipping 2 of 2 files (16 bytes; the rest is cached on the runner)\n"
        cached = snapshot + "errand: shipping 0 of 2 files (0 bytes; the rest is cached on the runner)\n"
        self.assertEqual(parse_transfer(cold, "cold")["shipped_bytes"], 16)
        self.assertEqual(parse_transfer(cached, "cached")["shipped_bytes"], 0)
        self.assertEqual(parse_transfer("", "no-snapshot")["snapshot_bytes"], 0)
        for text, mode in [(snapshot, "cached"), (cold, "cached"), (cached, "cold"), (cold, "no-snapshot"),
                           (cached + "errand: runner evicted negotiated blobs; re-shipping the full snapshot", "cached")]:
            with self.subTest(mode=mode, text=text), self.assertRaises(ValueError):
                parse_transfer(text, mode)

    def test_summary_excludes_warmups_and_failures(self):
        samples = [
            {"peer": "cabal", "scenario": "cached", "valid": True, "wall_ms": 100},
            {"peer": "cabal", "scenario": "cached", "valid": True, "wall_ms": 300},
            {"peer": "cabal", "scenario": "warmup", "valid": True, "wall_ms": 900},
            {"peer": "cabal", "scenario": "cached", "valid": False, "wall_ms": 2000},
        ]
        rows = summarize(samples)
        self.assertEqual(len(rows), 1)
        self.assertEqual(rows[0]["samples"], 2)
        self.assertEqual(rows[0]["median_ms"], 200)
        self.assertEqual((rows[0]["min_ms"], rows[0]["max_ms"]), (100, 300))

    def test_fixture_is_reproducible_unique_per_sample_and_exact_size(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            write_fixture(root, "seed-a", 3, 17)
            before = {p.name: p.read_bytes() for p in root.iterdir()}
            self.assertEqual(sum(map(len, before.values())), 51)
            self.assertEqual(len(set(before.values())), 3)
            write_fixture(root, "seed-a", 3, 17)
            self.assertEqual(before, {p.name: p.read_bytes() for p in root.iterdir()})
            write_fixture(root, "seed-b", 3, 17)
            self.assertTrue(all(p.read_bytes() != before[p.name] for p in root.iterdir()))


if __name__ == "__main__":
    unittest.main()
