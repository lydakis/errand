"""Check and summarize the focused comparisons, keeping each campaign separate."""
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
    require(r['status'] == 'complete', "r['status'] == 'complete'")
    verify_archive(path, r)
    samples = r['samples']
    builds = sorted({s['build'] for s in samples})
    rounds = r['rounds']
    require(len(builds) == 3 and rounds == 12, 'len(builds) == 3 and rounds == 12')
    require(len(samples) == len(r['cases'])*len(builds)*rounds, "len(samples) == len(r['cases'])*len(builds)*rounds")
    lines = [f'## {path.parent.name}', '',
             '| Case | Comparison | Median paired ratio | Pair range | Wins |',
             '|---|---|---:|---:|---:|']
    for case in sorted(r['cases']):
        group = [s for s in samples if s['case'] == case]
        pairs = {(s['build'], s['round']): s for s in group}
        require(len(pairs) == 3*rounds, 'len(pairs) == 3*rounds')
        orders = Counter(tuple(s['build'] for s in sorted(
            (s for s in group if s['round'] == number), key=lambda s: s['position']))
            for number in range(rounds))
        require(set(orders) == set(itertools.permutations(builds)), 'set(orders) == set(itertools.permutations(builds))')
        require(set(orders.values()) == {2}, 'set(orders.values()) == {2}')
        for denominator, numerator in itertools.combinations(builds, 2):
            require(all(math.isfinite(s['metrics']['ns/op']) and s['metrics']['ns/op'] > 0 for s in group), 'invalid timing')
            ratios = [pairs[numerator,n]['metrics']['ns/op']/pairs[denominator,n]['metrics']['ns/op']
                      for n in range(rounds)]
            lines.append(f'| {case} | {numerator}/{denominator} | {statistics.median(ratios):.3f} | '
                         f'{min(ratios):.3f}–{max(ratios):.3f} | {sum(x<1 for x in ratios)}/{rounds} |')
    return '\n'.join(lines)


if __name__ == '__main__':
    print('# Focused controls\n')
    print('Lower is faster. Ranges are observed extrema, not confidence intervals. '
          'Each case uses all six build orders twice. Baseline-a and baseline-b '
          'execute the same binary. Read each report for its timing budget.\n')
    for name in sys.argv[1:]:
        print(render(Path(name)))
        print()
