import collections
import unittest

from benchmark_hierarchy_validation import crossed_orders, HistoryFixture
from journal_depth_cases import VARIANTS


class HierarchyComparisonContracts(unittest.TestCase):
    def test_depth_and_mode_orders_are_fully_crossed(self):
        joint = collections.Counter()
        positions = collections.Counter()
        for number in range(36):
            depths, modes = crossed_orders(number)
            joint[tuple(depths), tuple(modes)] += 1
            for dp, depth in enumerate(depths):
                for mp, mode in enumerate(modes):
                    positions[depth, mode, dp, mp] += 1
        self.assertEqual(len(joint), 36)
        self.assertEqual(set(joint.values()), {1})
        self.assertEqual(len(positions), 3*3*3*3)
        self.assertEqual(set(positions.values()), {4})

    def test_dispersed_edits_change_distinct_paths_across_history(self):
        class Path:
            def __init__(self): self.writes = []
            def write_bytes(self, value): self.writes.append(value)
        f = object.__new__(HistoryFixture)
        f.count, f.size, f.serial, f.dispersed = 10000, 128, 0, True
        f.paths = [Path() for _ in range(f.count)]
        for _ in range(31): f.mutate(1)
        self.assertEqual(sum(bool(p.writes) for p in f.paths), 31)
        self.assertGreater(len({i//100 for i,p in enumerate(f.paths) if p.writes}), 20)
        f.paths = [Path() for _ in range(f.count)]
        f.mutate(1000)
        self.assertEqual(sum(bool(p.writes) for p in f.paths), 1000)


if __name__ == '__main__':
    unittest.main()
