import unittest

from benchmark_derived_index import validate_sample


class DerivedCampaignReceipts(unittest.TestCase):
    def test_timed_compaction_requires_replay_and_replacement(self):
        result = dict(Hashed=1, Reused=99, Written=True, CacheError="",
                      CacheStatus="hit", ReplacedBase=True, JournalRecordsLoaded=32)
        validate_sample({"Result": result}, 100, 1, "compaction", "journal")
        for key, value in (("ReplacedBase", False), ("JournalRecordsLoaded", 0)):
            with self.subTest(key=key), self.assertRaises(RuntimeError):
                validate_sample({"Result": dict(result, **{key: value})},
                                100, 1, "compaction", "journal")

    def test_ordinary_edits_may_reach_compaction_limits(self):
        result = dict(Hashed=1, Reused=99, Written=True, CacheError="",
                      CacheStatus="hit", ReplacedBase=True, JournalRecordsLoaded=32)
        # Longer campaigns can compact outside the dedicated compaction case.
        validate_sample({"Result": result}, 100, 1, "edit", "journal")
        validate_sample({"Result": result}, 100, 1, "edit", "derived")
        with self.assertRaises(RuntimeError):
            validate_sample({"Result": dict(result, ReplacedBase=False)},
                            100, 1, "edit", "derived")


if __name__ == "__main__":
    unittest.main()
