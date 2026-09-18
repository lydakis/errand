import collections
from pathlib import Path
import re
import shutil
import tempfile
import unittest

from benchmark_apply_journal import instrument, replace_once, require_reference_source, summarize, verify_sample
from benchmark_snapshot_integration import benchmark_order


class ApplyJournalEvidenceTests(unittest.TestCase):
    def test_baseline_driver_refuses_grouped_checkout(self):
        with self.assertRaisesRegex(ValueError, 'Baseline-only driver'):
            require_reference_source(Path(__file__).resolve().parents[1])

    def test_instrumentation_preserves_direct_sync_calls_in_order(self):
        source = Path(__file__).resolve().parents[1] / 'internal/changes'
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            target = root / 'internal/changes'
            target.parent.mkdir(parents=True)
            shutil.copytree(source, target)
            sites = instrument(root)
            observed = []
            for file in sorted(source.glob('*.go')):
                candidate = (target/file.name).read_text()
                if file.name.endswith('_test.go'):
                    self.assertEqual(candidate, file.read_text())
                    continue
                original = re.findall(r'\b(\w+)\.Sync\(\)', file.read_text())
                calls = re.findall(r'applyJournalSync\((\w+), "([^"]+)"\)', candidate)
                self.assertEqual([receiver for receiver, _ in calls], original)
                self.assertNotRegex(candidate, r'\b\w+\.Sync\(\)')
                observed.extend(site for _, site in calls)
            self.assertEqual(observed, sites)
            self.assertTrue(sites)
            # A changed/already-instrumented source must fail rather than silently
            # claiming the expected attribution boundary was measured.
            with self.assertRaises(ValueError):
                instrument(root)

    def test_missing_or_duplicate_probe_anchor_fails(self):
        for source in ('missing', 'anchor anchor'):
            with self.assertRaises(ValueError):
                replace_once(source, 'anchor', 'replacement')

    def test_each_mode_occupies_each_position_twice(self):
        counts = collections.Counter()
        orders = []
        for number in range(6):
            order = benchmark_order(('plain-a', 'plain-b', 'attribution'), number)
            orders.append(tuple(order))
            counts.update((name, position) for position, name in enumerate(order))
        self.assertEqual(len(set(orders)), 6)
        self.assertEqual(set(counts.values()), {2})

    def test_rejects_averages_and_incomplete_attribution(self):
        plain = {'iterations': 1, 'metrics': {'changed-bytes/op': 1 << 20, 'ns/op': 10}}
        verify_sample(plain, 'plain-a', 8)
        for bad in ({**plain, 'iterations': 3}, plain):
            with self.assertRaises(ValueError):
                verify_sample(bad, 'attribution', 8)
        profiled = {'iterations': 1, 'metrics': {
            **plain['metrics'], 'journal/encode-count/op': 18,
            'journal/validate-count/op': 18, 'barrier/example-count/op': 110,
        }}
        verify_sample(profiled, 'attribution', 8)
        with self.assertRaises(ValueError):
            verify_sample(profiled, 'plain-b', 8)

    def test_summary_does_not_add_nested_spans(self):
        metrics = {'ns/op': 1000000, 'journal/encode-ns/op': 10000,
                   'journal/validate-ns/op': 10000, 'journal/encode-bytes/op': 20,
                   'barrier/example-ns/op': 500000, 'barrier/example-count/op': 10,
                   'journal/publication-ns/op': 750000, 'apply/core-ns/op': 900000}
        report = {'roots': [1], 'samples': [
            {'roots': 1, 'mode': mode, 'metrics': metrics}
            for mode in ('plain-a', 'plain-b', 'attribution')]}
        self.assertIn('| 1 | 1.00 | 1.00 | 1.00 | 0.50 | 0.02 | 20.00 | 10.00 |', summarize(report))


if __name__ == '__main__':
    unittest.main()
