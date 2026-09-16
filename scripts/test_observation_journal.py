import struct
import tempfile
from pathlib import Path
import unittest

from benchmark_derived_index import summarize
from benchmark_observation_journal import (
    FILE_LIMIT, JOURNAL_LIMIT, MAGIC, VARIANTS, append_budget, journal_layout,
    needs_reseed, validate_sample,
)
from summarize_observation_journal import analyze


def layout(records, journal_bytes=1000, base_bytes=8000):
    return dict(records=records, base_bytes=base_bytes, journal_bytes=journal_bytes,
                cache_bytes=base_bytes+journal_bytes)


class JournalCampaignReceipts(unittest.TestCase):
    def test_ordinary_edit_requires_actual_history_and_bounded_publication(self):
        for scenario in ("edit", "batch"):
            before, after = layout(1), layout(2, 2000)
            result = dict(Hashed=1, Reused=99, Written=True, CacheError="", CacheStatus="hit",
                          ReplacedBase=False, JournalRecordsLoaded=1, CheckpointBytes=1000)
            validate_sample({"Result": result}, 100, 1, scenario, "observation-journal", before, after)
            for key, value in (("ReplacedBase", True), ("JournalRecordsLoaded", 0),
                               ("CheckpointBytes", 8000000), ("CheckpointBytes", 999)):
                with self.subTest(scenario=scenario, key=key, value=value), self.assertRaises(RuntimeError):
                    validate_sample({"Result": dict(result, **{key: value})}, 100, 1,
                                    scenario, "observation-journal", before, after)
            with self.assertRaises(RuntimeError):
                validate_sample({"Result": result}, 100, 1, scenario, "observation-journal",
                                before, layout(1, 2000))
            with self.assertRaises(RuntimeError):
                validate_sample({"Result": result}, 100, 1, scenario, "observation-journal")

    def test_long_campaign_requires_record_compaction_and_exact_replacement_bytes(self):
        before, after = layout(32, 32000), layout(0, 0)
        result = dict(Hashed=1, Reused=99, Written=True, CacheError="", CacheStatus="hit",
                      ReplacedBase=True, JournalRecordsLoaded=32, CheckpointBytes=8000)
        validate_sample({"Result": result}, 100, 1, "edit", "observation-journal", before, after)
        for key, value in (("ReplacedBase", False), ("CheckpointBytes", 7999),
                           ("JournalRecordsLoaded", 31)):
            with self.subTest(key=key), self.assertRaises(RuntimeError):
                validate_sample({"Result": dict(result, **{key: value})}, 100, 1, "edit",
                                "observation-journal", before, after)
        with self.assertRaises(RuntimeError):
            validate_sample({"Result": result}, 100, 1, "edit", "replacement")

    def test_ordinary_history_reseeds_before_either_byte_budget_becomes_ambiguous(self):
        budget = append_budget(1000)
        for before in (layout(1, JOURNAL_LIMIT-budget),
                       layout(1, 1000, FILE_LIMIT-budget-1000)):
            self.assertFalse(needs_reseed(before, 1000))
            larger = dict(before, journal_bytes=before["journal_bytes"]+1,
                          cache_bytes=before["cache_bytes"]+1)
            self.assertTrue(needs_reseed(larger, 1000))
            # The independently known record limit still times real compaction.
            self.assertFalse(needs_reseed(dict(larger, records=32), 1000))
            with self.assertRaises(RuntimeError):
                validate_sample({"Result": {}}, 10000, 1000, "batch",
                                "observation-journal", larger, layout(0, 0))

    def test_layout_counts_real_frames_and_rejects_incomplete_files(self):
        def frame(body):
            return struct.pack("<Q", len(body)) + bytes(32) + body + bytes(32)
        base, delta = frame(b"base"), frame(b"edit")
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory)/"observations"
            path.write_bytes(MAGIC+base+delta)
            self.assertEqual(journal_layout(path), layout(1, len(delta), len(MAGIC)+len(base)))
            for data in (MAGIC, MAGIC+base+b"short", MAGIC+base+delta[:-1]):
                path.write_bytes(data)
                with self.assertRaises(RuntimeError):
                    journal_layout(path)

    def test_compaction_and_history_recovery_cannot_silently_fall_back(self):
        for scenario, edits, records in (("records", 1, 32), ("bytes", 60, 1), ("recovery", 0, 4)):
            result = dict(Hashed=edits, Reused=100-edits, Written=True, CacheError="",
                          CacheStatus="recovered" if scenario == "recovery" else "hit",
                          ReplacedBase=True, JournalRecordsLoaded=records)
            validate_sample({"Result": result}, 100, edits, scenario, "observation-journal")
            for key, value in (("ReplacedBase", False), ("JournalRecordsLoaded", 0),
                               ("Hashed", 100), ("CacheStatus", "missing"), ("CacheError", "skipped")):
                with self.subTest(scenario=scenario, key=key), self.assertRaises(RuntimeError):
                    validate_sample({"Result": dict(result, **{key: value})},
                                    100, edits, scenario, "observation-journal")


class JournalPairedSummaries(unittest.TestCase):
    def samples(self):
        samples = []
        for variant, totals in zip(VARIANTS, ((10, 100, 1000), (100, 10, 1000), (20, 20, 1000))):
            for number, total in enumerate(totals):
                samples.append(dict(fixture="fixture", scenario="edit", variant=variant,
                                    round=number, position=1, Total=total, cache_bytes=100,
                                    Result=dict(CheckpointBytes=10, Written=True, Phases={"Load": 1},
                                                JournalRecordsLoaded=number)))
        return samples

    def test_both_controls_use_matching_round_ratios_and_retain_history(self):
        result = analyze({"samples": self.samples()})
        for baseline in ("checkpoint", "replacement"):
            row = result["summaries"][baseline][-1]
            self.assertEqual(row["baseline"], baseline)
            self.assertEqual(row["paired_ratio"], 1)  # Not 20/100, the ratio of medians.
            self.assertEqual(row["ratio_range"], [0.2, 2])
            self.assertEqual(row["wins"], 1)
        self.assertEqual(result["ordinary_histories"][0]["loaded_records"], [0, 1, 2])
        # A different frozen control must not change the same-binary comparison.
        samples = self.samples()
        samples[0]["Total"] = 100
        self.assertNotEqual(summarize(samples, VARIANTS)[-1]["paired_ratio"],
                            summarize(samples, VARIANTS, "replacement")[-1]["paired_ratio"])

    def test_missing_and_duplicate_pairs_fail_instead_of_biasing_ratios(self):
        samples = self.samples()
        for invalid in (samples[1:], samples+[samples[0]]):
            with self.assertRaises(ValueError):
                analyze({"samples": invalid})


if __name__ == "__main__":
    unittest.main()
