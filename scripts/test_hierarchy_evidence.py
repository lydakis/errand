import json
import importlib.util
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

from hierarchy_inputs import require_production_diff, verify_baseline


class EvidenceContracts(unittest.TestCase):
    def test_fetch_diagnostics_keep_timed_outliers_but_not_calibration(self):
        evidence = Path(__file__).resolve().parents[1]/'docs/benchmarks/hierarchy-validation'
        sys.path.insert(0, str(evidence))
        try:
            spec = importlib.util.spec_from_file_location('hierarchy_callers', evidence/'summarize_callers.py')
            module = importlib.util.module_from_spec(spec)
            spec.loader.exec_module(module)
        finally:
            sys.path.pop(0)
        def line(sample, seconds):
            return 'EVALUATION '+json.dumps(dict(mode='fetch', sample=sample, seconds=seconds))
        raw = '\n'.join([line(0, .01), line(0, .1), line(1, .7), line(2, .09)])
        rows = module.timed_fetch_rows(raw, 3)
        self.assertEqual([r['seconds'] for r in rows], [.1, .7, .09])
        with self.assertRaisesRegex(RuntimeError, 'count'):
            module.timed_fetch_rows(raw, 4)
        with self.assertRaisesRegex(RuntimeError, 'sequence'):
            module.timed_fetch_rows(raw+'\n'+line(4, .1), 4)

    def test_source_gate_rejects_unrelated_changes_and_wrong_baseline(self):
        with tempfile.TemporaryDirectory() as tmp:
            a, b = Path(tmp)/'a', Path(tmp)/'b'
            for root in (a, b):
                (root/'internal/manifest').mkdir(parents=True)
                (root/'internal/manifest/snapshot.go').write_text('baseline')
                (root/'other.go').write_text('same')
            (b/'internal/manifest/snapshot.go').write_text('candidate')
            self.assertEqual(set(require_production_diff(a, b)), {'internal/manifest/snapshot.go'})
            (b/'other.go').write_text('unrelated')
            with self.assertRaisesRegex(RuntimeError, 'other.go'):
                require_production_diff(a, b)
            archive = Path(tmp)/'baseline.tar.gz'
            archive.write_bytes(b'wrong archive')
            with self.assertRaisesRegex(RuntimeError, 'baseline archive'):
                verify_baseline(archive)

    def test_renderers_reject_invalid_reports_under_optimized_python(self):
        evidence = Path(__file__).resolve().parents[1]/'docs/benchmarks/hierarchy-validation'
        for renderer, campaign in (('summarize.py', 'apfs-final'), ('summarize_controls.py', 'btrfs-controls')):
            original = json.loads((evidence/campaign/'report.json').read_text())
            for defect in ('status', 'duplicate', 'receipt', 'phase', 'archive'):
                if renderer == 'summarize_controls.py' and defect in ('receipt', 'phase'):
                    continue
                report = json.loads(json.dumps(original))
                if defect == 'status':
                    report['status'] = 'failed'
                elif defect == 'duplicate':
                    key = 'commands' if renderer == 'summarize.py' else 'samples'
                    report[key][1] = report[key][0]
                elif defect == 'receipt':
                    report['samples'][0]['Hash'] = 'wrong root'
                elif defect == 'phase':
                    report['samples'][0]['Result']['Phases']['Load'] = -1
                else:
                    report['candidate_archive_sha256'] = 'wrong archive'
                with self.subTest(renderer=renderer, defect=defect), tempfile.TemporaryDirectory() as tmp:
                    root = Path(tmp)
                    (root/'candidate-inputs.tar.gz').symlink_to(evidence/campaign/'candidate-inputs.tar.gz')
                    (root/'report.json').write_text(json.dumps(report))
                    result = subprocess.run([sys.executable, '-O', str(evidence/renderer), str(root/'report.json')],
                                            capture_output=True, text=True)
                    self.assertNotEqual(result.returncode, 0, result.stdout)
                    self.assertIn('RuntimeError', result.stderr)


if __name__ == '__main__':
    unittest.main()
