#!/usr/bin/env python3
"""Bounded native controls and isolated hierarchy-update alternatives."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import platform
import shutil
import tarfile
import time

from benchmark_checkpoint_builder import candidate_digest
from benchmark_hierarchy_validation import command_cases
from benchmark_snapshot_integration import benchmark_campaign, benchmark_order, filesystem, parse_sample, run
from hierarchy_inputs import BASELINE, require_production_diff, verify_baseline
from snapshot_provenance import comparison_inputs, harness_inputs, require_matching_inputs


def alternatives(candidate, scratch, entry_only=False):
    roots = {'candidate': candidate}
    for name in ('preallocated', 'entry-validation'):
        root = scratch/name
        shutil.copytree(candidate, root)
        p = root/'internal/manifest/snapshot.go'
        source = p.read_text()
        old = '\tvar replacements proto.Manifest\n'
        if source.count(old) != 1:
            raise RuntimeError('Unexpected replacement allocation')
        if name == 'preallocated' or not entry_only:
            source = source.replace(old, '\treplacements := proto.Manifest{Entries: make([]proto.ManifestEntry, 0, len(ordered))}\n')
        if name == 'entry-validation':
            old = '\tif err := archive.ValidateSortedContext(ctx, replacements); err != nil {\n\t\treturn nil, err\n\t}\n'
            if source.count(old) != 1:
                raise RuntimeError('Unexpected replacement validation')
            source = source.replace(old, '''\tfor _, e := range replacements.Entries {
\t\tif err := ctx.Err(); err != nil {
\t\t\treturn nil, err
\t\t}
\t\tif err := archive.ValidateEntry(e); err != nil {
\t\t\treturn nil, err
\t\t}
\t}
''')
            archive = root/'internal/archive/archive.go'
            archive.write_text(archive.read_text()+'''
// ValidateEntry checks one path and its metadata, including symlink containment.
// It does not establish ordering, uniqueness, or relationships between entries.
func ValidateEntry(e proto.ManifestEntry) error {
    if err := checkRelPath(e.Path); err != nil { return err }
    if err := validateEntry(e); err != nil { return err }
    if e.Type == proto.EntrySymlink { return checkSymlinkTarget(e.Path, e.Target) }
    return nil
}
''')
        p.write_text(source)
        roots[name] = root
    return roots


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--output', type=Path, required=True)
    parser.add_argument('--mode', choices=('alternatives', 'boundaries', 'callers'), required=True)
    parser.add_argument('--rounds', type=int, default=12)
    args = parser.parse_args()
    if args.rounds < 6 or args.rounds % 6:
        parser.error('rounds must be a positive multiple of six')
    source, out = Path.cwd(), args.output.resolve()
    out.mkdir(parents=True, exist_ok=False)
    env = dict(os.environ, GOMAXPROCS='2', CGO_ENABLED='0')
    for key in tuple(env):
        if key.startswith('GIT_'): del env[key]
    env.update(GIT_CONFIG_NOSYSTEM='1', GIT_CONFIG_GLOBAL=os.devnull)
    report = dict(baseline=BASELINE, mode=args.mode, rounds=args.rounds, platform=platform.platform(),
                  source_sha256=candidate_digest(source, out), harness_inputs=harness_inputs(source/'scripts'),
                  samples=[], toolchains={}, binaries={}, production_diff={})
    archive = source/'docs/benchmarks/hierarchy-validation/baseline-inputs.tar.gz'
    report['baseline_archive_sha256'] = verify_baseline(archive)
    # Freeze sources before generating variants; never copy scratch back into itself.
    with tarfile.open(out/'candidate-inputs.tar.gz', 'w:gz') as tar:
        for p in sorted([*source.rglob('*.go'), source/'go.mod', source/'go.sum', *(source/'scripts').glob('*.py')]):
            if not p.is_relative_to(out): tar.add(p, arcname=str(p.relative_to(source)), recursive=False)
    report['candidate_archive_sha256'] = hashlib.sha256((out/'candidate-inputs.tar.gz').read_bytes()).hexdigest()
    with benchmark_campaign(out, report) as scratch:
        fixture = scratch/'fixtures'
        fixture.mkdir()
        env['TMPDIR'] = str(fixture)
        report['filesystem'] = filesystem(fixture, env, out, 'filesystem')
        expected = 'apfs' if platform.system() == 'Darwin' else 'btrfs'
        if report['filesystem'].lower() != expected: raise RuntimeError('Native filesystem required')
        report['mount'] = run(['mount'] if expected == 'apfs' else ['findmnt','-T',str(fixture),'-n','-o','FSTYPE,OPTIONS'], source, env, out/'mount.txt')
        baseline, candidate = scratch/'baseline', scratch/'candidate'
        baseline.mkdir(); candidate.mkdir()
        with tarfile.open(archive) as tar: tar.extractall(baseline, filter='data')
        with tarfile.open(out/'candidate-inputs.tar.gz') as tar: tar.extractall(candidate, filter='data')
        added = Path('internal/manifest/hierarchy_update_test.go')
        shutil.copyfile(candidate/added, baseline/added)
        allowed = ['internal/manifest/snapshot.go']
        if args.mode == 'callers':
            allowed.append('internal/archive/archive.go')
        report['production_diff']['candidate'] = require_production_diff(baseline, candidate, allowed)
        all_cases = command_cases()
        if args.mode in ('alternatives', 'boundaries'):
            roots = alternatives(candidate, scratch, entry_only=args.mode == 'boundaries')
            cases = {k:v for k,v in all_cases.items() if k.startswith('update-')}
            if args.mode == 'boundaries':
                for count in (1000, 10000):
                    for shape in ('single', 'delete-only', 'delete-heavy'):
                        cases[f'update-{count}-{shape}'] = ('./internal/manifest', f'^BenchmarkHierarchyUpdateBoundaries$/^{count}$/^{shape}$', '300ms')
        else:
            roots = dict(baseline=baseline, candidate=candidate)
            names = ('update-1000-dispersed', 'update-1000-mixed', 'watch', 'watch-burst', 'fetch-false', 'fetch-true')
            cases = {k:all_cases[k] for k in names}
        report['cases'] = cases
        require_matching_inputs({k:comparison_inputs(v) for k,v in roots.items()}, [])
        packages = ['./internal/manifest', './internal/archive', './internal/snapshot', './internal/changes', './internal/client', './internal/daemon', './cmd/errand', './experiments/snapshotcheckpoint/...']
        if args.mode == 'boundaries':
            # Only a benchmark was added. Recheck the affected boundary suite;
            # the broad variant validation is retained in the alternatives run.
            packages = ['./internal/manifest', './internal/archive', './internal/snapshot', './internal/changes', './experiments/snapshotcheckpoint/...']
        binaries = {}
        for name, root in roots.items():
            if name not in ('baseline', 'candidate'):
                changes = ['internal/manifest/snapshot.go']
                if name == 'entry-validation': changes.append('internal/archive/archive.go')
                report['production_diff'][name] = require_production_diff(candidate, root, changes)
            # Exact variant patches plus the candidate archive reconstruct all inputs.
            run(['gofmt','-w','internal/manifest/snapshot.go','internal/archive/archive.go'], root, env, out/f'{name}-gofmt.txt')
            if name not in ('baseline', 'candidate'):
                report['production_diff'][name] = require_production_diff(candidate, root, changes)
            report['toolchains'][name] = json.loads(run(['go','env','-json','GOVERSION','GOOS','GOARCH','GOFLAGS','CGO_ENABLED','GOTOOLCHAIN','GOEXPERIMENT'], root, env, out/f'{name}-toolchain.json'))
            run(['go','test','-p=1','-timeout=10m',*packages], root, env, out/f'{name}-tests.txt')
            run(['go','test','-race','-p=1','-timeout=10m','./internal/manifest','./internal/archive','./internal/snapshot','./internal/changes','./experiments/snapshotcheckpoint/...'], root, dict(env,CGO_ENABLED='1'), out/f'{name}-race.txt')
            run(['go','vet',*packages], root, env, out/f'{name}-vet.txt')
            for package in sorted({v[0] for v in cases.values()}):
                key = package.replace('/','-').strip('.')
                binary = scratch/f'{name}-{key}.test'
                run(['go','test','-c','-o',str(binary),package], root, env, out/f'{name}-{key}-build.txt')
                binaries[name,package] = binary
                report['binaries'][f'{name}:{package}'] = hashlib.sha256(binary.read_bytes()).hexdigest()
            print(f'{name}: tests, race, vet and build passed', flush=True)
        if len({json.dumps(v, sort_keys=True) for v in report['toolchains'].values()}) != 1:
            raise RuntimeError('Toolchains differ')
        labels = list(roots) if args.mode != 'callers' else ['baseline-a','baseline-b','candidate']
        report['builds'] = labels
        for number in range(args.rounds):
            for case,(package,pattern,budget) in cases.items():
                for position,label in enumerate(benchmark_order(labels, number)):
                    name = 'baseline' if label.startswith('baseline-') else label
                    started, load = time.monotonic(), os.getloadavg()
                    raw = run([str(binaries[name,package]),'-test.run=^$',f'-test.bench={pattern}',f'-test.benchtime={budget}','-test.benchmem','-test.timeout=10m'],
                              roots[name], env, out/f'{case}-{number}-{label}.txt', timeout=300)
                    sample = parse_sample(raw, number)
                    sample.update(case=case, build=label, position=position, elapsed=time.monotonic()-started, load_before=load, load_after=os.getloadavg())
                    report['samples'].append(sample)
            (out/'report.json').write_text(json.dumps(report,indent=2)+'\n')
            print(f'{args.mode}: round {number+1}/{args.rounds}', flush=True)
        if candidate_digest(source,out) != report['source_sha256'] or harness_inputs(source/'scripts') != report['harness_inputs']:
            raise RuntimeError('Inputs changed during campaign')


if __name__ == '__main__':
    main()
