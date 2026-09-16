#!/usr/bin/env python3
"""Summarize paired contract checks without pooling revisions or attempts."""
import json
import statistics
import sys
from pathlib import Path


def summarize(samples, case_key, pair_key, value):
    result = {}
    for case in sorted({s[case_key] for s in samples}):
        rows = [s for s in samples if s[case_key] == case]
        baseline = {s[pair_key]: value(s) for s in rows if s['variant'] == 'baseline'}
        candidate = {s[pair_key]: value(s) for s in rows if s['variant'] == 'candidate'}
        if baseline.keys() != candidate.keys():
            raise ValueError('unpaired samples')
        ratios = [candidate[p]/baseline[p] for p in sorted(baseline)]
        result[case] = dict(baseline_ms=statistics.median(baseline.values())/1e6,
                            candidate_ms=statistics.median(candidate.values())/1e6,
                            paired_ratio=statistics.median(ratios),
                            range=[min(ratios), max(ratios)],
                            wins=sum(r < 1 for r in ratios), pairs=len(ratios))
    return result


if __name__ == '__main__':
    output = {}
    for filename in sys.argv[1:]:
        report = json.loads(Path(filename).read_text())
        if report['status'] != 'complete':
            raise ValueError('only complete reports may be summarized')
        commands = report['samples']
        output[report['filesystem']] = dict(
            report=filename, source_sha256=report['source_sha256'],
            commands=summarize(commands, 'case', 'round', lambda s: s['metrics']['ns/op']),
            # Preserve both orders when checking sensitivity to the initial period.
            later_commands=summarize([s for s in commands if s['round'] >= 2], 'case', 'round', lambda s: s['metrics']['ns/op']),
            checkpoints=summarize(report.get('checkpoint_samples', []), 'scenario', 'pair', lambda s: s['Total']))
    print(json.dumps(output, indent=2))
