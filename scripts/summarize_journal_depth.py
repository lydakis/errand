#!/usr/bin/env python3
"""Render complete phase accounting from retained uninstrumented journal samples."""
import argparse
import json
from pathlib import Path
import statistics


def phase_rows(report):
    rows=[]
    samples=report['samples']
    keys=[(s['fixture'],s['scenario'],s['round'],s['variant']) for s in samples]
    if len(set(keys))!=len(keys):
        raise ValueError('Duplicate sample')
    by_key=dict(zip(keys,samples))
    for fixture,scenario,variant in sorted({(s['fixture'],s['scenario'],s['variant']) for s in samples}):
        group=[s for s in samples if (s['fixture'],s['scenario'],s['variant'])==(fixture,scenario,variant)]
        phases=list(group[0]['Result']['Phases'])
        medians={p:statistics.median(s['Result']['Phases'][p] for s in group)/1e6 for p in phases}
        medians['Wire']=statistics.median(s['Wire'] for s in group)/1e6
        medians['Other']=statistics.median(s['Total']-s['Wire']-sum(s['Result']['Phases'].values()) for s in group)/1e6
        medians['Total']=statistics.median(s['Total'] for s in group)/1e6
        ratios=[]
        for s in group:
            baseline=by_key[fixture,scenario,s['round'],'checkpoint']
            if baseline['Total']<=baseline['Wire'] or s['Total']<=s['Wire']:
                raise ValueError('Nonpositive preparation time')
            ratios.append((s['Total']-s['Wire'])/(baseline['Total']-baseline['Wire']))
        rows.append(dict(fixture=fixture,scenario=scenario,variant=variant,medians_ms=medians,
                         preparation_ratio=statistics.median(ratios)))
    return rows


def render(report):
    rows=phase_rows(report)
    phases=list(rows[0]['medians_ms']) if rows else []
    lines=['# Complete phase accounting','',
           'Derived from original uninstrumented samples; no benchmark rerun. R = checkpoint, F = framed replacement, J = journal.',
           'All values are medians in milliseconds. Cycle rows sum 33 preparations per sample.',
           'Sparse history and cycle rows repeatedly edit the same first file. Fixed-depth orders are conditionally balanced and coupled.',
           'Other is computed per sample as Total minus Wire minus recorded phases. Medians need not add up to the median Total.',
           'Prep/R is the median paired ratio of (Total - Wire); the adoption decision continues to use complete Total.', '',
           '| Fixture | Case | Mode | '+' | '.join(phases)+' | Prep/R |',
           '|---|---|---|'+'---:|'*(len(phases)+1)]
    names={'checkpoint':'R','observation-replacement':'F','observation-journal':'J'}
    for row in rows:
        lines.append(f"| {row['fixture']} | {row['scenario']} | {names[row['variant']]} | "+
                     ' | '.join(f'{row["medians_ms"][p]:.2f}' for p in phases)+
                     f" | {row['preparation_ratio']:.3f} |")
    return '\n'.join(lines)+'\n'


if __name__=='__main__':
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('report',type=Path)
    parser.add_argument('--output',type=Path,required=True)
    args=parser.parse_args()
    args.output.write_text(render(json.loads(args.report.read_text())))
