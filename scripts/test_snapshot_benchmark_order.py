import itertools
import unittest

from benchmark_snapshot_integration import benchmark_order


class SnapshotBenchmarkOrderTests(unittest.TestCase):
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
