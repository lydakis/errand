"""Print every case from one complete baseline/candidate report, without filtering."""
import json
from pathlib import Path
from statistics import median
import sys

for name in sys.argv[1:]:
    report = json.loads(Path(name).read_text())
    if report['status'] != 'complete':
        raise SystemExit(f'{name}: incomplete report')
    variants = report['versions']
    samples = {v: {(s['case'], s['round']): s['metrics']['ns/op'] / 1e6
                   for s in variants[v]['samples']} for v in ('baseline', 'candidate')}
    expected = {(case, n) for case in report['cases'] for n in range(report['rounds'])}
    if any(set(values) != expected or len(variants[v]['samples']) != len(expected)
           for v, values in samples.items()):
        raise SystemExit(f'{name}: missing or duplicate case/round coverage')
    print(f'\n{name}: {report["fixture_filesystem"]}, {report["platform"]}')
    print('| Case | Baseline ms | Candidate ms | Paired ratio | Faster pairs | Screen |')
    print('|---|---:|---:|---:|---:|---|')
    for case in report['cases']:
        baseline = [samples['baseline'][case, n] for n in range(report['rounds'])]
        candidate = [samples['candidate'][case, n] for n in range(report['rounds'])]
        ratios = [c / b for b, c in zip(baseline, candidate)]
        faster = sum(c < b for b, c in zip(baseline, candidate))
        slower = sum(c > b for b, c in zip(baseline, candidate))
        screen = 'investigate' if median(ratios) > 1.05 and slower >= 4 else ''
        print(f'| {case} | {median(baseline):.3f} | {median(candidate):.3f} | '
              f'{median(ratios):.3f} | {faster}/{report["rounds"]} | {screen} |')
