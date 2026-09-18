#!/usr/bin/env python3
"""Compare grouped apply with the frozen P1 baseline on one native filesystem."""

import argparse
import difflib
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import platform
import shutil
import statistics
import tarfile

from benchmark_apply_journal import freeze, instrument
from benchmark_snapshot_integration import benchmark_campaign, filesystem, parse_sample, run

BASELINE_INPUT = 'ea33e79dedfc7428bf53a011e225b2c568694b80af0b77a4536a6b8f2164367e'


def extract_baseline(archive, dest):
    digest = hashlib.sha256()
    with tarfile.open(archive) as t:
        for member in t:
            name = PurePosixPath(member.name)
            if not member.isfile() or name.is_absolute() or '..' in name.parts:
                raise ValueError('Unexpected baseline archive member')
            digest.update(member.name.encode()+b'\0'+t.extractfile(member).read()+b'\0')
        if digest.hexdigest() != BASELINE_INPUT:
            raise ValueError('Baseline inputs do not match the recorded P1 baseline')
        # Native runners also ship Python versions predating extraction filters.
        # Copy only the checked regular files after verifying the complete hash.
        for member in t.getmembers():
            target = dest/member.name
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_bytes(t.extractfile(member).read())


def verify(sample, case, mode):
    m = sample['metrics']
    if sample['iterations'] != 1:
        raise ValueError('Expected an individual operation')
    if mode != 'attribution' and any(k.startswith(('journal/', 'barrier/', 'member/')) for k in m):
        raise ValueError('Ordinary timing binary unexpectedly instrumented')
    if case.startswith('apply-'):
        roots = int(case.split('-')[1])
        if m.get('changed-bytes/op') != 1 << 20:
            raise ValueError('Fixture content size changed')
        if mode == 'attribution':
            expected = 4 if roots == 1 else 3
            for key in ('journal/encode-count/op', 'journal/validate-count/op', 'journal/publication-count/op'):
                if m.get(key) != expected:
                    raise ValueError(f'Unexpected grouped publication count: {key}={m.get(key)}')
            barriers = sum(v for k, v in m.items() if k.startswith('barrier/') and k.endswith('-count/op'))
            members = m.get('member/fsync-count/op', 0)
            expected_barriers = (26 if members else 27) if roots == 1 else (2*roots+18 if members else 7*roots+18)
            expected_members = 1 if roots == 1 else 5*roots
            if barriers != expected_barriers or members not in (0, expected_members):
                raise ValueError('Unexpected grouped synchronization counts')


def summary(report):
    lines = ['# Grouped apply comparison', '',
             'Individual complete-operation timings; A/B execute the same frozen baseline binary.', '',
             '| Case | Baseline A ms | Baseline B ms | Candidate ms | Median paired C/A | Median paired C/B |',
             '|---|---:|---:|---:|---:|---:|']
    for case in report['cases']:
        samples = [s for s in report['samples'] if s['case'] == case]
        by = {mode: {s['round']: s['metrics']['ns/op'] for s in samples if s['mode'] == mode}
              for mode in ('baseline-a', 'baseline-b', 'candidate')}
        med = [statistics.median(by[mode].values())/1e6 for mode in by]
        ratios = [statistics.median(by['candidate'][r]/by[mode][r] for r in by['candidate'])
                  for mode in ('baseline-a', 'baseline-b')]
        lines.append('| '+case+' | '+' | '.join(f'{x:.3f}' for x in med+ratios)+' |')
    return '\n'.join(lines)+'\n'


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--output', type=Path, required=True)
    parser.add_argument('--rounds', type=int, default=4)
    parser.add_argument('--smoke', action='store_true')
    args = parser.parse_args()
    if args.rounds < 1:
        parser.error('--rounds must be positive')
    source, out = Path.cwd(), args.output.resolve()
    out.mkdir(parents=True, exist_ok=False)
    env = dict(os.environ, GOMAXPROCS='2', CGO_ENABLED='0')
    # Isolate Git fixtures from inherited shell and user Git configuration.
    for key in tuple(env):
        if key.startswith('GIT_'):
            del env[key]
    env.update(GIT_CONFIG_NOSYSTEM='1', GIT_CONFIG_GLOBAL=os.devnull)
    cases = {f'apply-{n}': ('changes', f'^BenchmarkApplyJournalScaling$/^{n}$')
             for n in ([1, 8] if args.smoke else [1, 8, 32, 128, 512])}
    if not args.smoke:
        cases.update({
            'push': ('cli', '^BenchmarkPushPhases$'),
            'watch': ('cli', '^BenchmarkWatchPhases$'),
            'fetch-batch': ('cli', '^BenchmarkFetchBodies$/^batch$'),
            'workspace-create': ('cli', '^BenchmarkWorkspaceCreationAndSubmission$/^workspace-create$'),
            'ephemeral-job': ('cli', '^BenchmarkWorkspaceCreationAndSubmission$/^ephemeral-job$'),
        })
    report = dict(platform=platform.platform(), rounds=args.rounds, cases=cases,
                  baseline_input_sha256=BASELINE_INPUT, samples=[], commands=[], gomaxprocs=2)
    cli_rounds = args.rounds - args.rounds % 3 if args.rounds >= 3 else args.rounds
    report['case_rounds'] = {name: args.rounds if kind == 'changes' else cli_rounds
                             for name, (kind, _) in cases.items()}
    with benchmark_campaign(out, report) as scratch:
        baseline, candidate, profiled = (scratch/name for name in ('baseline', 'candidate', 'attribution'))
        archive = source/'docs/benchmarks/apply-journal-scaling/apfs/inputs.tar.gz'
        shutil.copyfile(archive, out/'baseline-inputs.tar.gz')
        extract_baseline(archive, baseline)
        report['input_sha256'] = freeze(source, candidate, out)
        report['archive_sha256'] = hashlib.sha256((out/'inputs.tar.gz').read_bytes()).hexdigest()
        shutil.copytree(candidate, profiled)
        report['sync_sites'] = instrument(profiled)
        for label, left, right in (('candidate', baseline, candidate), ('instrumentation', candidate, profiled)):
            patch = []
            names = sorted(set(p.relative_to(left) for p in (left/'internal/changes').glob('*.go')) |
                           set(p.relative_to(right) for p in (right/'internal/changes').glob('*.go')))
            for name in names:
                a, b = left/name, right/name
                patch.extend(difflib.unified_diff(a.read_text().splitlines(True) if a.exists() else [],
                                                 b.read_text().splitlines(True) if b.exists() else [],
                                                 fromfile='a/'+str(name), tofile='b/'+str(name)))
            (out/f'{label}.patch').write_text(''.join(patch))
        report['go'] = run(['go', 'version'], source, env, out/'go-version.txt').strip()
        report['filesystem'] = filesystem(scratch, env, out, 'fixture-filesystem')
        binaries = {}
        packages = {'changes': './internal/changes', 'cli': './cmd/errand'}
        for mode, root in (('baseline', baseline), ('candidate', candidate), ('attribution', profiled)):
            for kind in dict.fromkeys(kind for kind, _ in cases.values()):
                if mode == 'attribution' and kind != 'changes':
                    continue
                binary = scratch/f'{mode}-{kind}.test'
                command = ['go', 'test', '-c', '-o', str(binary), packages[kind]]
                report['commands'].append(command)
                run(command, root, env, out/f'build-{mode}-{kind}.txt')
                binaries[mode, kind] = binary
        fixture = scratch/'fixtures'
        fixture.mkdir()
        env['TMPDIR'] = str(fixture)
        for number in range(args.rounds):
            names = list(cases)
            names = names[number % len(names):]+names[:number % len(names)]
            for case in names:
                if number >= report['case_rounds'][case]:
                    continue
                kind, pattern = cases[case]
                modes = ['baseline-a', 'baseline-b', 'candidate']
                if kind == 'changes':
                    modes.append('attribution')
                order = modes[number % len(modes):]+modes[:number % len(modes)]
                for position, mode in enumerate(order):
                    build = 'baseline' if mode.startswith('baseline-') else mode
                    command = [str(binaries[build, kind]), '-test.run=^$', '-test.bench='+pattern,
                               '-test.benchtime=1x', '-test.count=1']
                    log = out/f'{number:02d}-{case}-{mode}.txt'
                    sample = parse_sample(run(command, candidate, env, log, timeout=600), number)
                    verify(sample, case, mode)
                    sample.update(case=case, mode=mode, position=position, log=log.name)
                    report['samples'].append(sample)
                    print(f'round={number} case={case} mode={mode} ms={sample["metrics"]["ns/op"]/1e6:.2f}', flush=True)
                    (out/'report.json').write_text(json.dumps(report, indent=2)+'\n')
        (out/'summary.md').write_text(summary(report))


if __name__ == '__main__':
    main()
