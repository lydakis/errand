import tempfile
import unittest
from pathlib import Path
from benchmark_snapshot_contracts import comparison_versions
from snapshot_provenance import require_matching_inputs


class ComparisonInventoryTests(unittest.TestCase):
    def test_nested_baseline_is_preserved_without_polluting_candidate(self):
        with tempfile.TemporaryDirectory() as temporary:
            source = Path(temporary)
            output = source/'results'
            baseline = output/'scratch'/'baseline'
            baseline.mkdir(parents=True)
            for root in [source, baseline]:
                (root/'go.mod').write_text('module fixture\n')
                (root/'go.sum').write_text('')
                (root/'helper_test.go').write_text('fixture')
            roots = dict(baseline=baseline, candidate=source)
            versions = comparison_versions(roots, output)
            self.assertEqual(require_matching_inputs(versions, set()), [])
            self.assertIn('helper_test.go', versions['baseline'])
            (source/'helper_test.go').write_text('changed')
            with self.assertRaises(RuntimeError):
                require_matching_inputs(comparison_versions(roots, output), set())
