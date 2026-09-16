#!/usr/bin/env python3
"""Paired native checks for shared reuse and verified checkpoint publication."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import platform
import tarfile

from benchmark_checkpoint_builder import candidate_digest, validate_sample
from benchmark_snapshot_integration import benchmark_campaign, filesystem, parse_sample, run, source_digest
from snapshot_provenance import comparison_inputs, harness_inputs, require_matching_inputs

ALLOWED_TEST_DIFFERENCES = {
    # Concurrent, test-only fixture permission correction; no benchmark uses it.
    'internal/changes/source_search_test.go',
    'internal/snapshot/observations_test.go',
    'internal/snapshot/symlink_observation_test.go',
    'internal/snapshot/watch_prepare_test.go',
    'internal/snapshot/observation_publication_test.go',
    'internal/snapshot/reuse_evidence_test.go',
    'experiments/snapshotcheckpoint/checkpoint_test.go',
    'experiments/snapshotcheckpoint/codec_test.go',
    'experiments/snapshotcheckpoint/admission_test.go',
    'experiments/snapshotcheckpoint/store_fixture_test.go',
    'experiments/snapshotcheckpoint/publication_test.go',
}


def comparison_versions(roots, output):
    return {name: {path: digest for path, digest in comparison_inputs(root).items()
                   if name != 'candidate' or not (root/path).is_relative_to(output)}
            for name, root in roots.items()}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--output', type=Path, required=True)
    parser.add_argument('--rounds', type=int, default=8)
    args = parser.parse_args()
    if args.rounds < 8 or args.rounds % 2:
        parser.error('use an even count of at least eight paired rounds')
    source, out = Path.cwd(), args.output.resolve()
    out.mkdir(parents=True, exist_ok=False)
    env = {k: v for k, v in os.environ.items() if not k.startswith('GIT_')}
    env.update(GOMAXPROCS='2', CGO_ENABLED='0')
    evidence = source/'docs/benchmarks/snapshot-contracts'
    frozen = json.loads((evidence/'baseline-inputs.json').read_text())
    report = dict(samples=[], checkpoint_samples=[], baseline=frozen,
                  source_sha256=candidate_digest(source, out),
                  harness_inputs=harness_inputs(source/'scripts'), platform=platform.platform(),
                  rounds=args.rounds, scope='Native filesystem, loopback command paths; warm caches, no network delivery claim')
    with benchmark_campaign(out, report) as scratch:
        fixtures = scratch/'fixtures'
        fixtures.mkdir()
        env['TMPDIR'] = str(fixtures)
        report['filesystem'] = filesystem(fixtures, env, out, 'filesystem')
        report['toolchain'] = json.loads(run(['go', 'env', '-json', 'GOVERSION', 'GOOS', 'GOARCH', 'GOFLAGS', 'GOTOOLCHAIN', 'GOEXPERIMENT'], source, env, out/'toolchain.json'))
        archive = evidence/'baseline-inputs.tar.gz'
        if hashlib.sha256(archive.read_bytes()).hexdigest() != frozen['archive_sha256']:
            raise RuntimeError('baseline archive differs')
        baseline = scratch/'baseline'
        baseline.mkdir()
        with tarfile.open(archive) as tar:
            tar.extractall(baseline, filter='data')
        if source_digest(baseline) != frozen['source_sha256']:
            raise RuntimeError('baseline source differs')
        roots = dict(baseline=baseline, candidate=source)
        inputs = comparison_versions(roots, out)
        report['comparison_inputs'] = inputs
        report['allowed_input_differences'] = require_matching_inputs(inputs, ALLOWED_TEST_DIFFERENCES)
        binaries = {}
        for name, root in roots.items():
            for kind, package in [('snapshot', './internal/snapshot'), ('commands', './cmd/errand'), ('checkpoint', './experiments/snapshotcheckpoint/cmd')]:
                binary = scratch/f'{name}-{kind}'
                cmd = ['go', 'build'] if kind == 'checkpoint' else ['go', 'test', '-c']
                run(cmd+['-o', str(binary), package], root, env, out/f'build-{name}-{kind}.txt')
                binaries[name, kind] = binary
        report['binaries'] = {f'{name}-{kind}': hashlib.sha256(path.read_bytes()).hexdigest() for (name, kind), path in binaries.items()}
        packages = ['./internal/snapshot', './internal/manifest', './experiments/snapshotcheckpoint/...']
        run(['go', 'test', '-race', *packages], source, dict(env, CGO_ENABLED='1'), out/'race.txt')
        cases = {
            'first-edit': ('snapshot', '^BenchmarkWatchPreparation$/^10000$/^first-edit$'),
            'retained-edit': ('snapshot', '^BenchmarkWatchPreparation$/^10000$/^retained-edit$'),
            'reconcile': ('snapshot', '^BenchmarkWatchPreparation$/^10000$/^reconcile$'),
            'workspace-create': ('commands', '^BenchmarkWorkspaceCreationAndSubmission$/^workspace-create$'),
            'ephemeral-job': ('commands', '^BenchmarkWorkspaceCreationAndSubmission$/^ephemeral-job$'),
            'push': ('commands', '^BenchmarkPushPhases$'),
            'watch': ('commands', '^BenchmarkWatchPhases$'),
            'fetch-ephemeral': ('commands', '^BenchmarkFetchCompletion$/^persistent=false$'),
            'fetch-persistent': ('commands', '^BenchmarkFetchCompletion$/^persistent=true$'),
        }
        for pair in range(args.rounds):
            for case, (kind, pattern) in cases.items():
                run(['sync'], source, env, out/'sync.txt')
                for position, name in enumerate(('baseline', 'candidate') if pair % 2 == 0 else ('candidate', 'baseline'), 1):
                    raw = run([str(binaries[name, kind]), '-test.run=^$', '-test.bench='+pattern, '-test.benchtime=3x', '-test.benchmem'], roots[name], env, out/f'{pair}-{case}-{name}.txt', timeout=300)
                    sample = parse_sample(raw, pair)
                    sample.update(case=case, variant=name, position=position)
                    report['samples'].append(sample)
                (out/'report.json').write_text(json.dumps(report, indent=2)+'\n')
            print(f'command/preparation round {pair+1}/{args.rounds}', flush=True)
        fixture = fixtures/'checkpoint'
        fixture.mkdir()
        (fixture/'.errandignore').write_text('')
        for i in range(50000):
            path = fixture/f'dir-{i//100:04d}'/f'file-{i:05d}'
            path.parent.mkdir(exist_ok=True)
            path.write_bytes(b'x'*128)
        caches = {name: scratch/f'cache-{name}' for name in roots}
        def invoke(name, mode, log):
            return json.loads(run([str(binaries[name, 'checkpoint']), '-root', str(fixture), '-cache', str(caches[name]), '-mode', mode], roots[name], env, log, timeout=300))
        for scenario in ['miss', 'unchanged', 'edit', 'current']:
            for name in roots:
                invoke(name, 'checkpoint', out/f'seed-{scenario}-{name}.json')
            for pair in range(args.rounds):
                if scenario == 'miss':
                    for cache in caches.values():
                        (cache/'checkpoint').unlink(missing_ok=True)
                if scenario == 'edit':
                    (fixture/'dir-0000/file-00000').write_bytes((f'edit-{pair}'.encode()+b'x'*128)[:128])
                run(['sync'], source, env, out/'sync.txt')
                hashes = []
                for position, name in enumerate(('baseline', 'candidate') if pair % 2 == 0 else ('candidate', 'baseline'), 1):
                    sample = invoke(name, 'current' if scenario == 'current' else 'checkpoint', out/f'{scenario}-{pair}-{name}.json')
                    validate_sample(sample, 50001, int(scenario == 'edit'), scenario == 'miss', False)
                    hashes.append(sample['Hash'])
                    sample.update(scenario=scenario, pair=pair, variant=name, position=position)
                    report['checkpoint_samples'].append(sample)
                if len(set(hashes)) != 1:
                    raise RuntimeError('snapshot roots differ')
            print(f'checkpoint {scenario} complete', flush=True)
        report['final_source_sha256'] = candidate_digest(source, out)
        report['final_harness_inputs'] = harness_inputs(source/'scripts')
        if report['final_source_sha256'] != report['source_sha256'] or report['final_harness_inputs'] != report['harness_inputs'] or source_digest(baseline) != frozen['source_sha256']:
            raise RuntimeError('measurement inputs changed')


if __name__ == '__main__':
    main()
