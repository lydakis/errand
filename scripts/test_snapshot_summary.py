import unittest

from summarize_snapshot_benchmarks import compare, median_interval


class SnapshotSummaryTests(unittest.TestCase):
    def test_seven_pairs_do_not_claim_narrow_precision(self):
        interval = median_interval([0.8, 0.9, 0.91, 0.92, 0.93, 0.94, 1.1])
        self.assertEqual(interval["bounds"], [0.8, 1.1])
        self.assertEqual(interval["coverage"], 0.984375)
        self.assertIsNone(median_interval([0.9] * 5))

    def test_pairs_match_rounds_instead_of_input_order(self):
        left = [{"round": i, "metrics": {"ns/op": 100 * (i + 1)}} for i in range(7)]
        right = [{"round": i, "metrics": {"ns/op": 80 * (i + 1)}} for i in reversed(range(7))]
        row = compare(left, right)
        self.assertEqual(row["paired_ratios"], [0.8] * 7)
        self.assertTrue(row["watch_target_met"])
        self.assertEqual(row["classification"], "faster")
        with self.assertRaises(ValueError):
            compare(left, right[:-1])

    def test_large_median_does_not_hide_uncertain_regression(self):
        left = [{"round": i, "metrics": {"ns/op": 100}} for i in range(7)]
        right = [{"round": i, "metrics": {"ns/op": t}} for i, t in enumerate([90, 120, 121, 122, 123, 124, 125])]
        row = compare(left, right)
        self.assertFalse(row["regression_above_five_percent"])
        self.assertEqual(row["classification"], "inconclusive")


if __name__ == "__main__":
    unittest.main()
