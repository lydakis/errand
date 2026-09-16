"""Validate and render the full, uninstrumented native comparison reports."""
from pathlib import Path
import json
import math
from collections import Counter
import statistics
import sys

from evidence import require, verify_archive


def row(label, pairs, metric, rounds):
    require(set(pairs) == {(b,r) for b in ('baseline','candidate') for r in range(rounds)}, "set(pairs) == {(b,r) for b in ('baseline','candidate') for r in range(rounds)}")
    require(all(math.isfinite(metric(s)) and metric(s) > 0 for s in pairs.values()), f'invalid metric: {label}')
    ratios = [metric(pairs['candidate',r])/metric(pairs['baseline',r]) for r in range(rounds)]
    medians = [statistics.median(metric(pairs[b,r]) for r in range(rounds))/1e6
               for b in ('baseline','candidate')]
    return (f'| {label} | {medians[0]:.3f} | {medians[1]:.3f} | '
            f'{statistics.median(ratios):.3f} | {min(ratios):.3f}–{max(ratios):.3f} | '
            f'{sum(x<1 for x in ratios)}/{rounds} |')


def table_header():
    return ['| Case | Baseline ms | Candidate ms | Paired ratio | Pair range | Wins |',
            '|---|---:|---:|---:|---:|---:|']


def render(path):
    report = json.loads(path.read_text())
    require(report['status']=='complete' and not report['smoke'], "report['status']=='complete' and not report['smoke']")
    verify_archive(path, report)
    commands, samples = report['commands'], report['samples']
    expected_samples = 72 if report.get('focused') else 1368
    require(len(commands)==168 and len(samples)==expected_samples, 'len(commands)==168 and len(samples)==expected_samples')
    require(len({(s['case'],s['build'],s['round']) for s in commands})==168, "len({(s['case'],s['build'],s['round']) for s in commands})==168")
    require(len({(s['fixture'],s['scenario'],s['mode'],s['build'],s['round']) for s in samples})==expected_samples, "len({(s['fixture'],s['scenario'],s['mode'],s['build'],s['round']) for s in samples})==expected_samples")
    phase_names = {'Selection', 'Load', 'Scan', 'Hash', 'Index', 'Verify', 'Save'}
    for sample in samples:
        phases = sample['Result']['Phases']
        require(set(phases) == phase_names, 'preparation phase identity differs')
        for key, value in {**phases, 'Wire': sample['Wire'], 'Total': sample['Total']}.items():
            require(isinstance(value, (int, float)) and math.isfinite(value) and value >= 0,
                    f'invalid duration for {key}')
    for history in (() if report.get('focused') else ('repeated', 'dispersed')):
        joint, positions = Counter(), Counter()
        for number in range(36):
            group = [s for s in samples if s['scenario'].startswith(history+'-') and s['round']==number]
            depths = list(dict.fromkeys(s['scenario'] for s in group))
            require(len(depths)==3, 'len(depths)==3')
            modes = list(dict.fromkeys(s['mode'] for s in group if s['scenario']==depths[0]))
            joint[tuple(depths),tuple(modes)] += 1
            for s in group:
                require(s['mode_position']==modes.index(s['mode']), "s['mode_position']==modes.index(s['mode'])")
                positions[s['scenario'],s['mode'],depths.index(s['scenario']),s['mode_position'],s['build'],s['position']] += 1
        require(len(joint)==36 and set(joint.values())=={1}, 'len(joint)==36 and set(joint.values())=={1}')
        require(len(positions)==324 and set(positions.values())=={2}, 'len(positions)==324 and set(positions.values())=={2}')
    scope = ('Original predicate candidate: command matrix and dense histories; sparse history is not repeated.'
             if report.get('focused') else 'Initial candidate: command matrix, crossed sparse histories and dense histories.')
    lines = [f'## {path.parent.name.upper()}', '', scope, '', '### Updates and complete commands', '', *table_header()]
    for case in sorted({s['case'] for s in commands}):
        pairs = {(s['build'],s['round']):s for s in commands if s['case']==case}
        label = case + (' (flat)' if case.startswith('update-1000-') else ' (indexed)' if case.startswith('update-10000-') else '')
        lines.append(row(label,pairs,lambda s:s['metrics']['ns/op'],6))
    lines += ['', '### Complete preparation, including wire hashing', '', *table_header()]
    for scenario,mode in sorted({(s['scenario'],s['mode']) for s in samples}):
        pairs = {(s['build'],s['round']):s for s in samples if s['scenario']==scenario and s['mode']==mode}
        rounds = 6 if scenario.startswith('dense-') else 36
        for r in range(rounds):
            b,c = pairs['baseline',r], pairs['candidate',r]
            require(b['Hash']==c['Hash'], "b['Hash']==c['Hash']")
            for key in ('Hashed','Reused','Written','ReplacedBase','JournalRecordsLoaded',
                        'CacheStatus','CacheError','CheckpointBytes'):
                require(b['Result'][key]==c['Result'][key], (scenario,mode,r,key))
        lines.append(row(f'{scenario} / {mode}',pairs,lambda s:s['Total'],rounds))
    lines += ['', '### Dense preparation phases', '',
              'Same samples and receipt checks as the complete preparation table. Phase savings are not a substitute for total cost.', '', *table_header()]
    for scenario, mode in sorted({(s['scenario'], s['mode']) for s in samples if s['scenario'].startswith('dense-')}):
        pairs = {(s['build'],s['round']):s for s in samples if s['scenario']==scenario and s['mode']==mode}
        for phase in ('Load', 'Index'):
            lines.append(row(f'{scenario} / {mode} / {phase}', pairs, lambda s: s['Result']['Phases'][phase], 6))
    return '\n'.join(lines)


if __name__=='__main__':
    print('# Shared hierarchy validation: native comparisons\n')
    print('Ratios are medians of matched candidate/baseline pairs, not ratios of the displayed medians. Lower is better. Ranges show observed pair extrema, not confidence intervals.\n')
    print('Update cases are 100-entry batches: 1K flat inventories and 10K indexed trees. Command benchmarks use loopback HTTP and exclude fixture setup; they do not measure cross-host delivery or shell startup. Journal history cases use 10K files; dense cases use 50K.\n')
    for name in sys.argv[1:]:
        print(render(Path(name)))
        print()
