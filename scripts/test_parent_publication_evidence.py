import hashlib
import io
import json
from pathlib import Path
import tarfile
import tempfile
import unittest
from unittest.mock import patch

from verify_parent_publication import verify_inputs


class ParentPublicationEvidenceTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.before = {
            'internal/changes/apply_group.go': b'original production',
            'internal/changes/apply_group_parents_test.go': b'original tests',
            'internal/changes/helpers_test.go': b'benchmark helper',
            'scripts/benchmark_apply_followup.py': b'common driver',
            'go.mod': b'module example', 'go.sum': b'',
        }
        self.after = {**self.before,
                      'internal/changes/apply_group.go': b'candidate production',
                      'internal/changes/apply_group_parents_test.go': b'candidate tests'}
        self.commit = {n: v for n, v in self.before.items()
                       if n.endswith('.go') or n in ('go.mod', 'go.sum')}

    def verify(self):
        for variant, files in (('baseline', self.before), ('candidate', self.after)):
            directory = self.root/'inputs'/variant
            directory.mkdir(parents=True, exist_ok=True)
            digest = hashlib.sha256()
            with tarfile.open(directory/'inputs.tar.gz', 'w:gz') as archive:
                for name, raw in sorted(files.items()):
                    info = tarfile.TarInfo(name)
                    info.size = len(raw)
                    archive.addfile(info, io.BytesIO(raw))
                    digest.update(name.encode()+b'\0'+raw+b'\0')
            (directory/'identity.json').write_text(json.dumps({'input_sha256': digest.hexdigest()}))
        (self.root/'review-source.json').write_text(json.dumps({
            'frozen_input_sha256': digest.hexdigest(), 'changes': {}}))
        checkout = self.root/'checkout'
        for name, raw in self.after.items():
            file = checkout/name
            file.parent.mkdir(parents=True, exist_ok=True)
            file.write_bytes(raw)
        with patch('verify_parent_publication.git_go_inputs', return_value=self.commit):
            verify_inputs(self.root, checkout)

    def test_accepts_only_declared_production_and_test_differences(self):
        self.verify()

    def test_rejects_wrong_baseline_even_with_matching_archive_hashes(self):
        self.before['internal/changes/helpers_test.go'] = b'wrong baseline'
        self.after['internal/changes/helpers_test.go'] = b'wrong baseline'
        with self.assertRaisesRegex(ValueError, 'Baseline differs from'):
            self.verify()

    def test_rejects_missing_baseline_input(self):
        del self.before['internal/changes/helpers_test.go']
        del self.after['internal/changes/helpers_test.go']
        with self.assertRaisesRegex(ValueError, 'Baseline differs from'):
            self.verify()

    def test_rejects_changed_helper_or_driver(self):
        for name in ('internal/changes/helpers_test.go', 'scripts/benchmark_apply_followup.py'):
            with self.subTest(name=name):
                original = self.after[name]
                self.after[name] = b'changed workload'
                with self.assertRaisesRegex(ValueError, 'Unexpected archive differences'):
                    self.verify()
                self.after[name] = original


if __name__ == '__main__':
    unittest.main()
