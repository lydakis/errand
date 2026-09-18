from pathlib import Path
import tempfile
import unittest

from benchmark_grouped_apply import extract_baseline, summary, verify


class GroupedApplyEvidenceTests(unittest.TestCase):
    def test_ordinary_samples_reject_instrumentation(self):
        for case in ('apply-8', 'watch'):
            for key in ('journal/encode-count/op', 'barrier/example-count/op', 'member/fsync-count/op'):
                with self.subTest(case=case, key=key), self.assertRaises(ValueError):
                    verify({'iterations': 1, 'metrics': {'changed-bytes/op': 1 << 20, key: 1}}, case, 'candidate')

    def test_frozen_baseline_identity(self):
        source = Path(__file__).resolve().parents[1]
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            extract_baseline(source/'docs/benchmarks/apply-journal-scaling/apfs/inputs.tar.gz', root)
            self.assertIn('2*roots+2', (root/'internal/changes/apply_journal_benchmark_test.go').read_text())
            self.assertNotIn('applyItemGrouped', (root/'internal/changes/journal.go').read_text())

    def test_publication_and_sync_gates_reject_missing_boundaries(self):
        for darwin in (False, True):
            metrics = {'changed-bytes/op': 1 << 20, 'journal/encode-count/op': 3,
                       'journal/validate-count/op': 3, 'journal/publication-count/op': 3,
                       'barrier/example-count/op': 34 if darwin else 74}
            if darwin:
                metrics['member/fsync-count/op'] = 40
            sample = {'iterations': 1, 'metrics': metrics}
            verify(sample, 'apply-8', 'attribution')
            for key in ('journal/publication-count/op', 'barrier/example-count/op'):
                broken = {**sample, 'metrics': {**metrics, key: metrics[key]-1}}
                with self.assertRaises(ValueError):
                    verify(broken, 'apply-8', 'attribution')

    def test_ratios_pair_with_the_same_round(self):
        report = {'cases': ['apply-8'], 'samples': [
            {'case': 'apply-8', 'round': number, 'mode': mode, 'metrics': {'ns/op': ns}}
            for number, values in enumerate(((100, 200, 50), (1000, 2000, 500)))
            for mode, ns in zip(('baseline-a', 'baseline-b', 'candidate'), values)]}
        self.assertIn('| 0.500 | 0.250 |', summary(report))


if __name__ == '__main__':
    unittest.main()
