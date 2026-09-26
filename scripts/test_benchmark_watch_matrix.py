import json
from pathlib import Path
import tempfile
import unittest

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


if __name__ == "__main__":
    unittest.main()
