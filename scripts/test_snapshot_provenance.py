from pathlib import Path
import tempfile
import unittest

from snapshot_provenance import comparison_inputs, require_matching_inputs, harness_inputs


class SnapshotProvenanceTests(unittest.TestCase):
    def test_helpers_fixtures_and_modules_are_comparison_inputs(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            for name in ('go.mod', 'go.sum', 'pkg/ordinary_test.go',
                         'pkg/body_benchmark_test.go', 'pkg/testdata/body.bin', 'pkg/code.go'):
                file = root / name
                file.parent.mkdir(parents=True, exist_ok=True)
                file.write_text(name)
            baseline = comparison_inputs(root)
            self.assertNotIn('pkg/code.go', baseline)
            for name in baseline:
                file = root / name
                original = file.read_bytes()
                file.write_bytes(original + b'changed')
                with self.assertRaisesRegex(RuntimeError, 'differ'):
                    require_matching_inputs({'baseline': baseline, 'candidate': comparison_inputs(root)}, [])
                file.write_bytes(original)
            (root / 'pkg/helper_test.go').write_text('new helper')
            with self.assertRaisesRegex(RuntimeError, 'helper_test.go'):
                require_matching_inputs({'baseline': baseline, 'candidate': comparison_inputs(root)}, [])

    def test_exceptions_are_exact_recorded_and_must_match_a_difference(self):
        versions = {'baseline': {'a_test.go': 'old'}, 'candidate': {'a_test.go': 'new'}}
        self.assertEqual(require_matching_inputs(versions, ['a_test.go']), ['a_test.go'])
        for allowed in (['*_test.go'], ['unused_test.go'], ['a_test.go', 'unused_test.go']):
            with self.assertRaises(RuntimeError):
                require_matching_inputs(versions, allowed)

    def test_harness_includes_support_modules(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / 'driver.py').write_text('import helper')
            (root / 'helper.py').write_text('first')
            first = harness_inputs(root)
            (root / 'helper.py').write_text('second')
            self.assertNotEqual(first, harness_inputs(root))


if __name__ == '__main__':
    unittest.main()
