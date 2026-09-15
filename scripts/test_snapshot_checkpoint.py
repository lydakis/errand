import unittest

from benchmark_snapshot_checkpoint import validate_result


class CheckpointCampaignContracts(unittest.TestCase):
    def test_cold_control_must_hash_every_file_without_reuse_or_writes(self):
        cold = dict(Hashed=11, Reused=0, Written=False, CacheStatus="", CacheError="")
        for scenario in ("miss", "unchanged", "edit"):
            validate_result(cold, 10, scenario, "cold")
        for field, value in (("Hashed", 0), ("Reused", 1), ("Written", True), ("CacheStatus", "hit")):
            with self.subTest(field=field), self.assertRaises(RuntimeError):
                validate_result(dict(cold, **{field: value}), 10, "unchanged", "cold")

    def test_checkpoint_requires_the_intended_cache_path(self):
        for scenario, hashed, reused, written, status in (
            ("miss", 11, 0, True, "missing"), ("unchanged", 0, 11, False, "hit"), ("edit", 1, 10, True, "hit")
        ):
            result = dict(Hashed=hashed, Reused=reused, Written=written, CacheStatus=status, CacheError="")
            validate_result(result, 10, scenario, "checkpoint")
            with self.subTest(scenario=scenario), self.assertRaises(RuntimeError):
                validate_result(dict(result, CacheError="busy writer"), 10, scenario, "checkpoint")


if __name__ == "__main__":
    unittest.main()
