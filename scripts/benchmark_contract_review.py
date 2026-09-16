#!/usr/bin/env python3
"""Compare review fixes against the frozen shared-contract candidate."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import platform
import tarfile
import time

from benchmark_checkpoint_builder import candidate_digest
from benchmark_snapshot_integration import benchmark_campaign, filesystem, parse_sample, run, source_digest
from snapshot_provenance import comparison_inputs, harness_inputs, require_matching_inputs


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--output', type=Path, required=True)
    args = parser.parse_args()
    source, output = Path.cwd(), args.output.resolve()
    output.mkdir(parents=True, exist_ok=False)
    env = {k: v for k, v in os.environ.items() if not k.startswith('GIT_')}
    env.update(GOMAXPROCS='2', CGO_ENABLED='0')
    evidence = source/'docs/benchmarks/snapshot-contracts'
    frozen = json.loads((evidence/'candidate-inputs.json').read_text())
    report = dict(scope='Review cleanup versus frozen pre-review candidate; isolated comparison and warm watch preparation, not end-to-end delivery',
                  samples=[], platform=platform.platform(), baseline=frozen, developer_dir=env.get('DEVELOPER_DIR'),
                  source_sha256=candidate_digest(source, output), harness_inputs=harness_inputs(source/'scripts'))
    with benchmark_campaign(output, report) as scratch:
        fixtures = scratch/'fixtures'
        fixtures.mkdir()
        env['TMPDIR'] = str(fixtures)
        report['filesystem'] = filesystem(fixtures, env, output, 'filesystem')
        report['toolchain'] = json.loads(run(['go', 'env', '-json', 'GOVERSION', 'GOOS', 'GOARCH', 'GOFLAGS', 'GOTOOLCHAIN', 'GOEXPERIMENT'], source, env, output/'toolchain.json'))
        archive = evidence/'candidate-inputs.tar.gz'
        if hashlib.sha256(archive.read_bytes()).hexdigest() != frozen['archive_sha256']:
            raise RuntimeError('frozen archive differs')
        baseline = scratch/'baseline'
        baseline.mkdir()
        with tarfile.open(archive) as tar:
            tar.extractall(baseline, filter='data')
        if source_digest(baseline) != frozen['source_sha256']:
            raise RuntimeError('frozen source differs')
        # Both variants execute exactly the same new comparison benchmark.
        benchmark = Path('experiments/snapshotcheckpoint/differences_benchmark_test.go')
        (baseline/benchmark).write_bytes((source/benchmark).read_bytes())
        roots = dict(baseline=baseline, candidate=source)
        inputs = {name: {p: digest for p, digest in comparison_inputs(root).items()
                         if name != 'candidate' or not (root/p).is_relative_to(output)}
                  for name, root in roots.items()}
        report['comparison_inputs'] = inputs
        report['allowed_input_differences'] = require_matching_inputs(inputs, {
            'internal/snapshot/observation_publication_test.go',  # Unused-root regression.
            'experiments/snapshotcheckpoint/store_fixture_test.go',  # Raw encoder moved into tests.
        })
        report['effective_baseline_sha256'] = source_digest(baseline)
        run(['git', '--version'], source, env, output/'git.txt')
        run(['go', 'test', '-p=1', '-timeout=10m', './...'], source, env, output/'tests.txt')
        run(['go', 'vet', './...'], source, env, output/'vet.txt')
        run(['go', 'test', '-race', '-p=1', '-timeout=10m', './internal/snapshot', './experiments/snapshotcheckpoint/...', './internal/changes'],
            source, dict(env, CGO_ENABLED='1'), output/'race.txt')
        print('Tests, vet and focused race checks passed', flush=True)
        binaries = {}
        for name, root in roots.items():
            for kind, package in [('snapshot', './internal/snapshot'), ('checkpoint', './experiments/snapshotcheckpoint')]:
                binary = scratch/f'{name}-{kind}'
                run(['go', 'test', '-c', '-o', str(binary), package], root, env, output/f'build-{name}-{kind}.txt')
                binaries[name, kind] = binary
        report['binaries'] = {f'{name}-{kind}': hashlib.sha256(path.read_bytes()).hexdigest() for (name, kind), path in binaries.items()}
        cases = {
            'differences-unchanged': ('checkpoint', '^BenchmarkVerifiedDifferences$/^changed=false$', '200ms'),
            'differences-edit': ('checkpoint', '^BenchmarkVerifiedDifferences$/^changed=true$', '200ms'),
            'first-edit': ('snapshot', '^BenchmarkWatchPreparation$/^10000$/^first-edit$', '3x'),
            'retained-edit': ('snapshot', '^BenchmarkWatchPreparation$/^10000$/^retained-edit$', '3x'),
        }
        for pair in range(8):
            for case, (kind, pattern, benchtime) in cases.items():
                for position, name in enumerate(('baseline', 'candidate') if pair % 2 == 0 else ('candidate', 'baseline'), 1):
                    started = time.time()
                    raw = run([str(binaries[name, kind]), '-test.run=^$', f'-test.bench={pattern}', f'-test.benchtime={benchtime}', '-test.count=1', '-test.benchmem'],
                              roots[name], env, output/f'{case}-{pair}-{name}.txt')
                    sample = parse_sample(raw, pair)
                    sample.update(case=case, variant=name, position=position, started_unix=started, finished_unix=time.time())
                    report['samples'].append(sample)
            (output/'report.json').write_text(json.dumps(report, indent=2)+'\n')
            print(f'Paired round {pair+1}/8', flush=True)
        report['final_source_sha256'] = candidate_digest(source, output)
        report['final_harness_inputs'] = harness_inputs(source/'scripts')
        if report['source_sha256'] != report['final_source_sha256'] or report['harness_inputs'] != report['final_harness_inputs'] or source_digest(baseline) != report['effective_baseline_sha256']:
            raise RuntimeError('measurement inputs changed')


if __name__ == '__main__':
    main()
