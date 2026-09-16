import json,statistics,collections,sys
from pathlib import Path
summary=[]
for host,filename in zip(('apfs','btrfs'),sys.argv[1:]):
 report=json.loads(Path(filename).read_text())
 groups=collections.defaultdict(dict)
 for s in report['samples']:
  groups[s['fixture'],s['scenario']].setdefault(s['variant'],[]).append(s)
 for (fixture,scenario),variants in groups.items():
  rows={}
  for name,samples in variants.items():
   values=[s['Total']/1e6 for s in samples]
   rows[name]=dict(n=len(values),median=statistics.median(values),min=min(values),max=max(values),phases={p:statistics.median(s['Result']['Phases'][p]/1e6 for s in samples) for p in samples[0]['Result']['Phases']},positions={str(pos):statistics.median(s['Total']/1e6 for s in samples if s['position']==pos) for pos in sorted(set(s['position'] for s in samples))})
  comparisons={}
  for before,after in ([('baseline','candidate')] if scenario=='cold-guard' else [('checkpoint','shared-update'),('checkpoint','shared-current'),('shared-update','shared-current')]):
   b={s['pair']:s['Total'] for s in variants[before]}
   ratios=[s['Total']/b[s['pair']] for s in variants[after]]
   comparisons[f'{before}->{after}']=dict(paired_median=statistics.median(ratios),wins=sum(x<1 for x in ratios),n=len(ratios))
  summary.append(dict(host=host,fixture=fixture,scenario=scenario,variants=rows,comparisons=comparisons))
print(json.dumps(summary,indent=2))
