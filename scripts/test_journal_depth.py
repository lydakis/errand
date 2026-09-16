import collections
import unittest

from journal_depth_cases import VARIANTS, fixed_cases, check_receipt, cycle_receipt
from benchmark_snapshot_integration import benchmark_order
from summarize_journal_depth import phase_rows


class JournalDepthContracts(unittest.TestCase):
    def test_phase_accounting_preserves_pairs_and_cycle_wire(self):
        samples=[]
        for number,(base,wire,current,current_wire) in enumerate(((100,10,50,5),(20,10,30,10))):
            for variant,total,w in (('checkpoint',base,wire),('observation-journal',current,current_wire)):
                samples.append(dict(fixture='fixture',scenario='cycle',round=number,variant=variant,
                                    Total=total*1_000_000,Wire=w*1_000_000,
                                    Result={'Phases':{'Load':5_000_000}}))
        row=next(r for r in phase_rows({'samples':samples}) if r['variant']=='observation-journal')
        self.assertEqual(row['preparation_ratio'],1.25)
        self.assertEqual(row['medians_ms'],dict(Load=5,Wire=7.5,Other=27.5,Total=40))
        with self.assertRaises(ValueError):
            phase_rows({'samples':samples+[samples[0]]})

    def test_depth_and_workload_each_receive_every_execution_order(self):
        orders = collections.defaultdict(set)
        depth_orders = set()
        for number in range(6):
            depth_orders.add(tuple(d for d,w in fixed_cases(number) if w == 'unchanged'))
            for depth, workload in fixed_cases(number):
                orders[depth, workload].add(tuple(benchmark_order(VARIANTS, number)))
        self.assertEqual(set(orders), {(d,w) for d in (0,8,31) for w in ('unchanged','edit','batch')})
        self.assertTrue(all(len(v)==6 for v in orders.values()))
        self.assertEqual(len(depth_orders),6)

    def test_fixed_history_rejects_fallback_wrong_depth_and_wrong_hash_work(self):
        result = dict(CacheStatus='hit', CacheError='', Written=True, Hashed=1, Reused=99,
                      JournalRecordsLoaded=31, ReplacedBase=False, CheckpointBytes=1000)
        check_receipt({'Result':result}, 100, 1, 'observation-journal', 31, False)
        for field,value in [('CacheStatus','missing'),('CacheError','busy'),('Written',False),
                            ('Hashed',100),('JournalRecordsLoaded',30),('ReplacedBase',True)]:
            with self.subTest(field=field),self.assertRaises(RuntimeError):
                check_receipt({'Result':dict(result,**{field:value})},100,1,'observation-journal',31,False)
        check_receipt({'Result':dict(result,JournalRecordsLoaded=0,ReplacedBase=True)},
                      100,1,'observation-replacement',0,True)

    def test_cycle_includes_all_appends_and_the_terminal_compaction(self):
        samples=[]
        for step in range(33):
            samples.append(dict(Total=10, Wire=1, cache_bytes=100,
                                Result=dict(Phases={'Load':2},CheckpointBytes=100 if step==32 else 1,
                                            Written=True,ReplacedBase=step==32,JournalRecordsLoaded=step)))
        total=cycle_receipt(samples,'observation-journal')
        self.assertEqual(total['Total'],330)
        self.assertEqual(total['Result']['CheckpointBytes'],132)
        for invalid in (samples[:-1],samples+[samples[-1]],samples[:10]+samples[11:]+[samples[10]]):
            with self.assertRaises(RuntimeError):
                cycle_receipt(invalid,'observation-journal')


if __name__=='__main__':
    unittest.main()
