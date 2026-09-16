"""Render crossed follow-ups, including negative controls and caller phases."""
from collections import Counter
import itertools
import json
import math
from pathlib import Path
import statistics
import sys

from evidence import require, verify_archive


def render(path):
    r = json.loads(path.read_text())
    require(r['status'] == 'complete', 'campaign did not complete')
    verify_archive(path, r)
    samples, builds, rounds = r['samples'], r['builds'], r['rounds']
    require(len(builds) == 3 and len(set(builds)) == 3 and rounds >= 6 and rounds % 6 == 0, 'crossed design')
    require(len(samples) == len(r['cases'])*3*rounds, 'sample count')
    require({s['case'] for s in samples} == set(r['cases']), 'case identity')
    scope = {'alternatives': 'Reference: original hierarchy-preserving candidate. Entry-validation includes preallocation in this campaign.',
             'boundaries': 'Reference: original hierarchy-preserving candidate. Entry-validation does not include preallocation in this campaign.',
             'callers': 'Reference: pinned published baseline. Baseline-a and baseline-b execute the same binary; candidate is the final source recorded in this report.'}
    require(r['mode'] in scope, 'campaign mode')
    lines = [f'## {path.parent.name}', '', scope[r['mode']], '',
             '| Case / metric | Comparison | Paired ratio | Pair range | Wins |',
             '|---|---|---:|---:|---:|']
    for case in r['cases']:
        group = [s for s in samples if s['case'] == case]
        pairs = {(s['build'],s['round']):s for s in group}
        require(len(group) == len(pairs) == 3*rounds, 'duplicate or missing sample')
        require(set(pairs) == set(itertools.product(builds, range(rounds))), 'sample identity')
        orders = Counter(tuple(s['build'] for s in sorted(
            (s for s in group if s['round']==n), key=lambda s:s['position'])) for n in range(rounds))
        require(set(orders) == set(itertools.permutations(builds)) and set(orders.values()) == {rounds//6}, 'build order')
        metrics = ['ns/op']
        if case.startswith('update-'): metrics += ['B/op', 'allocs/op']
        if case == 'watch-burst': metrics += ['settle-ms/op','cpu-ms/op','client-ms/op','stage-ms/op','apply-ms/op','resamples/op']
        for metric in metrics:
            for denominator,numerator in itertools.combinations(builds, 2):
                values = [pairs[b,n]['metrics'][metric] for b in (denominator,numerator) for n in range(rounds)]
                require(all(math.isfinite(v) and v > 0 for v in values), f'invalid {metric}')
                ratios = [pairs[numerator,n]['metrics'][metric]/pairs[denominator,n]['metrics'][metric] for n in range(rounds)]
                lines.append(f'| {case} / {metric} | {numerator}/{denominator} | {statistics.median(ratios):.3f} | '
                             f'{min(ratios):.3f}–{max(ratios):.3f} | {sum(v<1 for v in ratios)}/{rounds} |')
    return '\n'.join(lines)


if __name__ == '__main__':
    print('# Hierarchy follow-up comparisons\n')
    print('Lower is better. Ratios use matched rounds; ranges are observed extrema, not confidence intervals. All six build orders are equally represented. Baseline-a and baseline-b execute the same binary.\n')
    for name in sys.argv[1:]:
        print(render(Path(name)))
        print()
