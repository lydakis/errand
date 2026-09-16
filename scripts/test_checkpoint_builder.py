import collections
import unittest
import tempfile
from pathlib import Path
from benchmark_checkpoint_builder import variant_order, validate_sample, candidate_digest


class BuilderCampaignContracts(unittest.TestCase):
    def test_generated_baseline_is_excluded_but_candidate_edits_are_detected(self):
        with tempfile.TemporaryDirectory() as tmp:
            root=Path(tmp)
            for name in ('go.mod','go.sum','candidate.go'):
                (root/name).write_text('original')
            output=root/'output'
            output.mkdir()
            before=candidate_digest(root,output)
            (output/'baseline.go').write_text('frozen')
            self.assertEqual(candidate_digest(root,output),before)
            (root/'candidate.go').write_text('changed')
            self.assertNotEqual(candidate_digest(root,output),before)

    def test_every_variant_occupies_every_position_twice(self):
        counts=collections.Counter((name,position) for pair in range(8)
                                   for position,name in enumerate(variant_order('abcd',pair)))
        self.assertEqual(len(counts),16)
        self.assertEqual(set(counts.values()),{2})

    def test_batch_reuse_and_failed_publication_are_checked(self):
        sample={'Result':dict(Hashed=1000,Reused=9001,Written=True,CacheStatus='hit',CacheError='')}
        validate_sample(sample,10001,1000,False,False)
        for field,value in [('Hashed',1),('Reused',10000),('Written',False),('CacheStatus','missing'),('CacheError','busy')]:
            with self.subTest(field=field),self.assertRaises(RuntimeError):
                validate_sample({'Result':dict(sample['Result'],**{field:value})},10001,1000,False,False)


if __name__=='__main__':
    unittest.main()
