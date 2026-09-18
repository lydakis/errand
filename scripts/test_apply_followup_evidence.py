import unittest

from apply_followup_evidence import paired, verify_decision_tables, verify_source_transition


class ApplyFollowupEvidenceTests(unittest.TestCase):
    def test_pairing_uses_rounds_not_order_or_ratio_of_medians(self):
        report = {'samples': [
            {'case': 'tiny', 'mode': mode, 'round': r, 'metrics': {'ns/op': n}}
            for mode, r, n in [('a', 0, 100), ('a', 1, 1000), ('b', 1, 2000), ('b', 0, 10)]
        ]}
        self.assertEqual(paired(report, 'tiny', 'a', 'b'), (-425, 1, 2))
        report['samples'].pop()
        with self.assertRaisesRegex(ValueError, 'Missing paired round'):
            paired(report, 'tiny', 'a', 'b')

    def test_decision_table_drift_is_rejected(self):
        document = '<!-- apply-main:start -->\n50.0% | 3/4\n<!-- apply-main:end -->'
        verify_decision_tables(document, {'main': '50.0% | 3/4'})
        for incorrect in ('51.0% | 3/4', '50.0% | 4/4'):
            with self.assertRaisesRegex(ValueError, 'decision table differs'):
                verify_decision_tables(document, {'main': incorrect})

    def test_review_source_requires_exact_changes_and_full_inventory(self):
        frozen = {'a.go': 'old', 'b.go': 'same'}
        current = {'a.go': 'new', 'b.go': 'same'}
        changes = {'a.go': {'before': 'old', 'after': 'new'}}
        verify_source_transition(frozen, current, changes)
        for invalid in ({**current, 'a.go': 'drift'}, {'a.go': 'new'}, {**current, 'added.go': 'extra'}):
            with self.assertRaisesRegex(ValueError, 'Production differs'):
                verify_source_transition(frozen, invalid, changes)
        with self.assertRaisesRegex(ValueError, 'Invalid source transition'):
            verify_source_transition(frozen, current, {'a.go': {'before': 'wrong', 'after': 'new'}})


if __name__ == '__main__':
    unittest.main()
