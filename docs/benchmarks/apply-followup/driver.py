#!/usr/bin/env python3
"""Compare immutable apply follow-up variants, one complete operation per sample."""
import argparse
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import platform
import statistics
import tarfile

from benchmark_snapshot_integration import benchmark_campaign, filesystem, parse_sample, run


def extract(source, dest):
    identity = json.loads((source/'identity.json').read_text())
    digest = hashlib.sha256()
    with tarfile.open(source/'inputs.tar.gz') as archive:
        seen = set()
        for member in archive:
            name = PurePosixPath(member.name)
            if not member.isfile() or name.is_absolute() or '..' in name.parts or member.name in seen:
                raise ValueError('Unexpected or duplicate archive member')
            seen.add(member.name)
            raw = archive.extractfile(member).read()
            digest.update(member.name.encode()+b'\0'+raw+b'\0')
        if digest.hexdigest() != identity['input_sha256']:
            raise ValueError('Frozen source identity mismatch')
        for member in archive.getmembers():
            target = dest/member.name
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_bytes(archive.extractfile(member).read())
    return identity


def summary(report):
    modes = report['modes']
    lines = ['# Apply follow-up', '', 'Individual operation medians in milliseconds. '
             'A/B run the identical reference binary; modes rotate within each round.', '',
             '| Case | '+' | '.join(modes)+' |', '|---|'+'---:|'*len(modes)]
    for case in report['cases']:
        values = [statistics.median(s['metrics']['ns/op'] for s in report['samples']
                                    if s['case'] == case and s['mode'] == mode)/1e6 for mode in modes]
        lines.append('| '+case+' | '+' | '.join(f'{v:.3f}' for v in values)+' |')
    return '\n'.join(lines)+'\n'


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--output', type=Path, required=True)
    parser.add_argument('--inputs', type=Path, default=Path('docs/benchmarks/apply-followup/inputs'))
    parser.add_argument('--variants', nargs='+', default=['baseline', 'scratch', 'parents'])
    parser.add_argument('--small-rounds', type=int, default=20)
    parser.add_argument('--batch-rounds', type=int, default=4)
    parser.add_argument('--no-watch', action='store_true')
    parser.add_argument('--smoke', action='store_true')
    parser.add_argument('--cases', nargs='+', help='Restrict to named workloads')
    args = parser.parse_args()
    if min(args.small_rounds, args.batch_rounds) < 1 or len(set(args.variants)) != len(args.variants):
        parser.error('Positive round counts and distinct variants required')
    source, out = Path.cwd(), args.output.resolve()
    out.mkdir(parents=True, exist_ok=False)
    env = dict(os.environ, GOMAXPROCS='2', CGO_ENABLED='0')
    for key in tuple(env):
        if key.startswith('GIT_'):
            del env[key]
    env.update(GIT_CONFIG_NOSYSTEM='1', GIT_CONFIG_GLOBAL=os.devnull)
    modes = ['reference-a', 'reference-b']+args.variants[1:]
    cases = {n: ('changes', '^BenchmarkApplyWorkloads$/^'+n+'$') for n in
             (['tiny', 'parents8'] if args.smoke else ['tiny', 'large', 'flat128', 'parents8', 'parents128', 'creation', 'deletion', 'new-parent', 'restricted'])}
    if not args.no_watch and not args.smoke:
        cases['watch-small'] = ('cli', '^BenchmarkWatchWorkloads$/^small$')
    if args.cases:
        if not set(args.cases) <= set(cases):
            parser.error('Unknown workload')
        cases = {n: cases[n] for n in args.cases}
    report = dict(platform=platform.platform(), cases=cases, modes=modes, inputs={},
                  samples=[], commands=[], gomaxprocs=2,
                  case_rounds={n: args.small_rounds if n in ('tiny', 'large', 'watch-small') else args.batch_rounds for n in cases})
    with benchmark_campaign(out, report) as scratch:
        report['go'] = run(['go', 'version'], source, env, out/'go-version.txt').strip()
        report['filesystem'] = filesystem(scratch, env, out, 'fixture-filesystem')
        binaries = {}
        for variant in args.variants:
            root = scratch/variant
            report['inputs'][variant] = extract(args.inputs.resolve()/variant, root)
            for kind in dict.fromkeys(kind for kind, _ in cases.values()):
                binary = scratch/f'{variant}-{kind}.test'
                command = ['go', 'test', '-c', '-o', str(binary), './internal/changes' if kind == 'changes' else './cmd/errand']
                report['commands'].append(command)
                run(command, root, env, out/f'build-{variant}-{kind}.txt')
                binaries[variant, kind] = binary
        fixture = scratch/'fixtures'
        fixture.mkdir()
        env['TMPDIR'] = str(fixture)
        for number in range(max(report['case_rounds'].values())):
            names = list(cases)
            names = names[number % len(names):]+names[:number % len(names)]
            for case in names:
                if number >= report['case_rounds'][case]:
                    continue
                kind, pattern = cases[case]
                order = modes[number % len(modes):]+modes[:number % len(modes)]
                for position, mode in enumerate(order):
                    variant = args.variants[0] if mode.startswith('reference-') else mode
                    command = [str(binaries[variant, kind]), '-test.run=^$', '-test.bench='+pattern,
                               '-test.benchtime=1x', '-test.count=1']
                    log = out/f'{number:02d}-{case}-{mode}.txt'
                    sample = parse_sample(run(command, scratch/variant, env, log, timeout=600), number)
                    if sample['iterations'] != 1:
                        raise ValueError('Expected one complete operation')
                    if kind == 'changes':
                        expected_group = case == 'flat128' or case in ('parents8', 'parents128') and variant in ('parents', 'exchange')
                        expected_roots = 1 if case in ('tiny', 'large') else 128
                        if sample['metrics'].get('grouped/op') != int(expected_group) or sample['metrics'].get('roots/op') != expected_roots:
                            raise ValueError('Unexpected eligibility or fixture shape')
                    sample.update(case=case, mode=mode, position=position, log=log.name,
                                  log_sha256=hashlib.sha256(log.read_bytes()).hexdigest())
                    report['samples'].append(sample)
                    print(f'round={number} case={case} mode={mode} ms={sample["metrics"]["ns/op"]/1e6:.2f}', flush=True)
                    (out/'report.json').write_text(json.dumps(report, indent=2)+'\n')
        (out/'summary.md').write_text(summary(report))


if __name__ == '__main__':
    main()
