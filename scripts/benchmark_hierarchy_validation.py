#!/usr/bin/env python3
"""Matched shared-validation comparison, production callers and crossed journal histories."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import platform
import shutil
import tarfile

from benchmark_checkpoint_builder import candidate_digest
from benchmark_snapshot_integration import benchmark_campaign, benchmark_order, filesystem, parse_sample, run
from journal_depth_cases import Fixture, VARIANTS, check_receipt, journal_layout
from snapshot_provenance import harness_inputs, comparison_inputs, require_matching_inputs
from hierarchy_inputs import BASELINE, verify_baseline, require_production_diff


def crossed_orders(number):
    # All six depth orders crossed with all six mode orders in 36 rounds.
    return (benchmark_order((0, 8, 31), number // 6),
            benchmark_order(VARIANTS, number % 6))


# Keep the matrix explicit: initialization is expected to be neutral, while
# retained updates and mixed batches exercise different validation paths.
def command_cases():
    cases = {}
    for count in (1000, 10000):
        for shape in ('repeated', 'dispersed', 'mixed'):
            cases[f'update-{count}-{shape}'] = ('./internal/manifest', f'^BenchmarkHierarchyUpdate$/^{count}$/^{shape}$', '300ms')
    cases.update({
        'push': ('./cmd/errand', '^BenchmarkPushPhases$', '5x'),
        'watch': ('./cmd/errand', '^BenchmarkWatchPhases$', '5x'),
        'watch-structural': ('./cmd/errand', '^BenchmarkWatchWorkloads$/^structural$', '5x'),
        'watch-burst': ('./cmd/errand', '^BenchmarkWatchBurst$', '5x'),
    })
    for kind in ('workspace-create', 'ephemeral-job'):
        cases[kind] = ('./cmd/errand', f'^BenchmarkWorkspaceCreationAndSubmission$/^{kind}$', '5x')
    for persistent in ('false', 'true'):
        cases[f'fetch-{persistent}'] = ('./cmd/errand', f'^BenchmarkFetchCompletion$/^persistent={persistent}$', '5x')
    return cases


class HistoryFixture(Fixture):
    dispersed = False

    def mutate(self, edits):
        if not self.dispersed:
            return super().mutate(edits)
        self.serial += 1
        for i in range(edits):
            index = (self.serial * 6181 + i * 47) % self.count
            self.paths[index].write_bytes((f'edit-{self.serial}-{index}'.encode()+b'z'*self.size)[:self.size])


def measure_pair(f, binaries, scenario, number, edits, depth, modes, replace=False):
    f.mutate(edits)
    samples = []
    for mode_position, mode in enumerate(modes):
        for position, build in enumerate(benchmark_order(binaries, number + mode_position)):
            # Both processes see identical source bytes and identical baseline
            # cache bytes. Restore and drain each copy outside the timer.
            shutil.copyfile(f.templates[mode], f.cache(mode))
            f.drain(f'{scenario}-{number}-{mode}-{build}')
            f.binary = binaries[build]
            sample = f.invoke(mode, f'{scenario}-{number}-{build}')
            check_receipt(sample, f.files, edits, mode, depth,
                          edits > 0 and (mode == VARIANTS[1] or mode == VARIANTS[2] and replace))
            if mode == VARIANTS[2]:
                layout = journal_layout(f.cache(mode))
                expected = 0 if replace else depth + int(edits > 0)
                if layout['records'] != expected:
                    raise RuntimeError('Unexpected journal publication')
            sample.update(fixture=f.label, scenario=scenario, round=number, mode=mode,
                          build=build, position=position, mode_position=mode_position, edits=edits)
            samples.append(sample)
    if len({s['Hash'] for s in samples}) != 1:
        raise RuntimeError('Matched preparation roots differ')
    return samples


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--output', type=Path, required=True)
    scope = parser.add_mutually_exclusive_group()
    scope.add_argument('--smoke', action='store_true')
    scope.add_argument('--focused', action='store_true', help='final caller/dense-history check; omit sparse history matrix')
    args = parser.parse_args()
    source, out = Path.cwd(), args.output.resolve()
    out.mkdir(parents=True, exist_ok=False)
    env = dict(os.environ, GOMAXPROCS='2', CGO_ENABLED='0')
    for key in tuple(env):
        if key.startswith('GIT_'): del env[key]
    env.update(GIT_CONFIG_NOSYSTEM='1', GIT_CONFIG_GLOBAL=os.devnull)
    report = dict(baseline=BASELINE, smoke=args.smoke, focused=args.focused, platform=platform.platform(),
                  python=platform.python_version(), source_sha256=candidate_digest(source, out),
                  harness_inputs=harness_inputs(source/'scripts'), commands=[], samples=[])
    archive = source/'docs/benchmarks/hierarchy-validation/baseline-inputs.tar.gz'
    report['baseline_archive_sha256'] = verify_baseline(archive)
    with tarfile.open(out/'candidate-inputs.tar.gz', 'w:gz') as tar:
        paths = [*source.rglob('*.go'), source/'go.mod', source/'go.sum', *(source/'scripts').glob('*.py')]
        for path in sorted(p for p in paths if not p.is_relative_to(out)):
            tar.add(path, arcname=str(path.relative_to(source)), recursive=False)
    report['candidate_archive_sha256'] = hashlib.sha256((out/'candidate-inputs.tar.gz').read_bytes()).hexdigest()
    with benchmark_campaign(out, report) as scratch:
        env['TMPDIR'] = str(scratch)
        report['filesystem'] = filesystem(scratch, env, out, 'filesystem')
        expected = 'apfs' if platform.system() == 'Darwin' else 'btrfs'
        if report['filesystem'].lower() != expected: raise RuntimeError('Native filesystem required')
        report['mount'] = run(['findmnt','-T',str(scratch),'-n','-o','FSTYPE,OPTIONS']
                              if platform.system() == 'Linux' else ['mount'], source, env, out/'mount.txt')
        baseline = scratch/'baseline'
        baseline.mkdir()
        with tarfile.open(archive) as tar:
            tar.extractall(baseline, filter='data')
        # Run the same new behavioral tests and benchmark in both builds.
        added = Path('internal/manifest/hierarchy_update_test.go')
        shutil.copyfile(source/added, baseline/added)
        roots = dict(baseline=baseline, candidate=source)
        cases = command_cases()
        report['cases'] = cases
        report['toolchains'], report['binaries'] = {}, {}
        binaries, probes = {}, {}
        # Candidate inventory excludes extracted baseline in scratch/output.
        candidate_inputs = {p:h for p,h in comparison_inputs(source).items()
                            if not (source/p).is_relative_to(out)}
        require_matching_inputs(dict(baseline=comparison_inputs(baseline), candidate=candidate_inputs), [])
        report['comparison_inputs'] = candidate_inputs
        report['production_diff'] = require_production_diff(baseline, source,
            ('internal/manifest/snapshot.go', 'internal/archive/archive.go'), exclude=(out,))
        packages = ['./internal/manifest', './internal/snapshot', './internal/changes',
                    './internal/archive', './internal/client', './internal/daemon',
                    './cmd/errand', './experiments/snapshotcheckpoint/...']
        run(['python3','-m','unittest','discover','-s','scripts'], source, env, out/'python-tests.txt')
        for build, root in roots.items():
            report['toolchains'][build] = json.loads(run(['go','env','-json','GOVERSION','GOOS','GOARCH','GOFLAGS','CGO_ENABLED','GOTOOLCHAIN','GOEXPERIMENT'],root,env,out/f'{build}-toolchain.json'))
            run(['go','test','-p=1','-timeout=10m',*packages],root,env,out/f'{build}-tests.txt')
            run(['go','test','-race','-p=1','-timeout=10m',*packages],root,dict(env,CGO_ENABLED='1'),out/f'{build}-race.txt')
            run(['go','vet',*packages],root,env,out/f'{build}-vet.txt')
            for package in sorted({c[0] for c in cases.values()}):
                key = package.removeprefix('./').replace('/','-')
                binary = scratch/f'{build}-{key}.test'
                run(['go','test','-c','-o',str(binary),package],root,env,out/f'{build}-{key}-build.txt')
                binaries[build,package] = binary
                report['binaries'][f'{build}-{key}'] = hashlib.sha256(binary.read_bytes()).hexdigest()
            probes[build] = scratch/f'{build}-probe'
            run(['go','build','-o',str(probes[build]),'./experiments/snapshotcheckpoint/cmd'],root,env,out/f'{build}-probe-build.txt')
            report['binaries'][f'{build}-probe'] = hashlib.sha256(probes[build].read_bytes()).hexdigest()
            print(f'{build}: native tests, race and vet passed', flush=True)
        if report['toolchains']['baseline'] != report['toolchains']['candidate']:
            raise RuntimeError('Toolchains differ')
        for number in range(2 if args.smoke else 6):
            for case,(package,pattern,benchtime) in cases.items():
                for build in benchmark_order(roots,number):
                    raw = run([str(binaries[build,package]),'-test.run=^$',f'-test.bench={pattern}',
                               f'-test.benchtime={"1x" if args.smoke else benchtime}', '-test.benchmem','-test.timeout=10m'],
                              roots[build],env,out/f'command-{case}-{number}-{build}.txt',timeout=300)
                    receipt = parse_sample(raw,number)
                    report['commands'].append(dict(receipt,case=case,build=build))
            print(f'command pairs {number+1} complete',flush=True)
            (out/'report.json').write_text(json.dumps(report,indent=2)+'\n')
        if not args.focused:
            f = HistoryFixture(source,scratch,out,env,probes['baseline'],100 if args.smoke else 10000,128,False)
        for history in (() if args.focused else ('repeated','dispersed')):
            f.dispersed = history == 'dispersed'
            for number in range(1 if args.smoke else 36):
                depths,modes = crossed_orders(number)
                for depth in depths:
                    f.binary = probes['baseline']
                    f.prepare(depth,f'{history}-{number}-d{depth}')
                    report['samples'].extend(measure_pair(f,probes,f'{history}-d{depth}',number,1,depth,modes))
                print(f'{history}: crossed round {number+1} complete',flush=True)
                (out/'report.json').write_text(json.dumps(report,indent=2)+'\n')
        if not args.smoke:
            f = HistoryFixture(source,scratch,out,env,probes['baseline'],50000,128,False)
            for number in range(6):
                f.binary = probes['baseline']
                f.prepare(2,f'dense-{number}',near_bytes=True)
                for scenario,edits,replaced in (('dense-append',1,False),('dense-compact',30000,True)):
                    report['samples'].extend(measure_pair(f,probes,scenario,number,edits,2,benchmark_order(VARIANTS,number),replaced))
                print(f'dense round {number+1} complete',flush=True)
        if report['source_sha256'] != candidate_digest(source,out) or report['harness_inputs'] != harness_inputs(source/'scripts'):
            raise RuntimeError('Inputs changed during comparison')


if __name__ == '__main__':
    main()
