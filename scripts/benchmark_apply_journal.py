#!/usr/bin/env python3
"""Attribute the frozen P1 reference protocol; use the grouped driver for current code."""

import argparse
import difflib
import hashlib
import json
import os
from pathlib import Path
import platform
import re
import shutil
import statistics
import tarfile

from benchmark_snapshot_integration import (
    benchmark_campaign, benchmark_order, filesystem, parse_sample, run,
)


def replace_once(text, old, new):
    if text.count(old) != 1:
        raise ValueError(f"Instrumentation anchor is not unique: {old!r}")
    return text.replace(old, new, 1)


def instrument(source):
    """Temporary copy only. Keep every original Sync invocation and its order."""
    sites = []
    for file in sorted((source / 'internal/changes').glob('*.go')):
        if file.name.endswith('_test.go'):
            continue
        original = text = file.read_text()

        def wrap(match):
            line = original[:match.start()].count('\n') + 1
            site = f'{file.stem}:{line}:{match[1]}'
            sites.append(site)
            return f'applyJournalSync({match[1]}, "{site}")'

        text = re.sub(r'\b(\w+)\.Sync\(\)', wrap, text)
        if file.name == 'journal.go':
            anchor = 'func writeApplyJournalAtRoot(root *os.Root, journal applyJournal) error {'
            start = text.index(anchor)
            end = text.index('\nfunc ', start + len(anchor))
            body = text[start:end]
            body = replace_once(body, 'validateApplyJournal(journal)', 'applyJournalValidate(journal)')
            body = replace_once(body, 'json.MarshalIndent(journal, "", "  ")', 'applyJournalEncode(journal)')
            body = replace_once(body, anchor, anchor + '\n\tdefer applyJournalRecord("journal/publication", time.Now(), 0)')
            text = text[:start] + body + text[end:]
            text = replace_once(text, 'import (', 'import (\n\t"time"')
        if file.name == 'apply.go':
            start = text.index('func ApplyToWorkspace(')
            offset = text.index(') (ApplyResult, error) {', start) + len(') (ApplyResult, error) {')
            text = text[:offset] + '\n\tdefer applyJournalRecord("apply/core", time.Now(), 0)' + text[offset:]
            text = replace_once(text, 'import (', 'import (\n\t"time"')
        if file.name == 'staging_sync_darwin.go':
            text = replace_once(text, 'func syncStagedData(file *os.File) error {',
                                'func syncStagedData(file *os.File) error {\n\tdefer applyJournalRecord("member/fsync", time.Now(), 0)')
            text = replace_once(text, 'import (', 'import (\n\t"time"')
        if text != original:
            file.write_text(text)
    if not sites:
        raise ValueError('No synchronization sites instrumented')
    return sites


def freeze(source, dest, output):
    files = [source/'go.mod', source/'go.sum']
    for directory in ('internal', 'cmd', 'experiments'):
        files.extend((source/directory).rglob('*.go'))
    files.extend((source/'scripts').glob('*.py'))
    digest = hashlib.sha256()
    with tarfile.open(output/'inputs.tar.gz', 'w:gz') as archive:
        for file in sorted(files):
            relative = file.relative_to(source)
            raw = file.read_bytes()
            digest.update(str(relative).encode()+b'\0'+raw+b'\0')
            target = dest/relative
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_bytes(raw)
            archive.add(target, arcname=str(relative))
    return digest.hexdigest()


def verify_sample(sample, mode, roots):
    metrics = sample['metrics']
    if sample['iterations'] != 1 or metrics.get('changed-bytes/op') != 1 << 20:
        raise ValueError('Expected one operation with exactly 1 MiB changed')
    publications = metrics.get('journal/encode-count/op', 0)
    if mode == 'attribution':
        if publications != 2*roots+2:
            raise ValueError(f'Expected 2k+2 journal publications, got {publications}')
        if metrics.get('journal/validate-count/op') != publications:
            raise ValueError('Publication validation and encoding counts differ')
        if not any(key.startswith('barrier/') for key in metrics):
            raise ValueError('Missing synchronization observations')
    elif publications:
        raise ValueError('Ordinary timing binary unexpectedly instrumented')


def summarize(report):
    lines = ['# Apply-journal scaling', '',
             'One complete `TransferTarget.Apply` per sample, including durable receipt and cleanup. '
             'Fixed 1 MiB changed; flat, existing-parent file replacements. Setup and retry checks are outside timing.', '',
             '| Roots | Plain A median ms | Plain B median ms | Instrumented median ms | Sync ms | Validate + encode ms | Journal bytes | Sync calls |',
             '|---|---:|---:|---:|---:|---:|---:|---:|']
    for roots in report['roots']:
        samples = [s for s in report['samples'] if s['roots'] == roots]
        by_mode = {mode: [s['metrics'] for s in samples if s['mode'] == mode]
                   for mode in ('plain-a', 'plain-b', 'attribution')}
        def med(mode, fn):
            return statistics.median(fn(m) for m in by_mode[mode])
        sync_ns = lambda m: sum(v for k, v in m.items() if k.startswith('barrier/') and k.endswith('-ns/op'))
        sync_count = lambda m: sum(v for k, v in m.items() if k.startswith('barrier/') and k.endswith('-count/op'))
        journal_ns = lambda m: m['journal/validate-ns/op'] + m['journal/encode-ns/op']
        values = [med(mode, lambda m: m['ns/op'])/1e6 for mode in by_mode]
        values += [med('attribution', sync_ns)/1e6, med('attribution', journal_ns)/1e6,
                   med('attribution', lambda m: m['journal/encode-bytes/op']), med('attribution', sync_count)]
        lines.append('| '+str(roots)+' | '+' | '.join(f'{v:,.2f}' for v in values)+' |')
    lines += ['', 'Synchronization times are sums of measured `File.Sync` calls. '
              'Component spans overlap: journal publication includes its validation, encoding and barriers; '
              'core apply includes journal publication. Do not add inclusive spans together. '
              'Instrumentation adds clocks and accounting, so use plain samples for latency. '
              'A and B execute the identical binary as an execution-position control.', '']
    return '\n'.join(lines)


def require_reference_source(source):
    if (source/'internal/changes/apply_group.go').exists():
        raise ValueError('Baseline-only driver: extract docs/benchmarks/apply-journal-scaling/apfs/inputs.tar.gz '
                         'into an empty directory and run there, or use benchmark_grouped_apply.py for current code')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--output', type=Path, required=True)
    parser.add_argument('--revision', required=True, help='descriptive source revision; frozen input hash is authoritative')
    parser.add_argument('--rounds', type=int, default=6)
    parser.add_argument('--smoke', action='store_true')
    args = parser.parse_args()
    if args.rounds < 1:
        parser.error('--rounds must be positive')
    source, out = Path.cwd(), args.output.resolve()
    try:
        require_reference_source(source)
    except ValueError as error:
        parser.error(str(error))
    out.mkdir(parents=True, exist_ok=False)
    env = dict(os.environ, GOMAXPROCS='2', CGO_ENABLED='0')
    report = dict(revision=args.revision, platform=platform.platform(), roots=[1, 8] if args.smoke else [1, 8, 32, 128, 512],
                  rounds=args.rounds, samples=[], commands=[], gomaxprocs=2)
    with benchmark_campaign(out, report) as scratch:
        plain, profiled = scratch/'plain', scratch/'attribution'
        report['input_sha256'] = freeze(source, plain, out)
        report['archive_sha256'] = hashlib.sha256((out/'inputs.tar.gz').read_bytes()).hexdigest()
        shutil.copytree(plain, profiled)
        report['sync_sites'] = instrument(profiled)
        # diff exits 1 for expected differences; generate the retained patch directly.
        patch = []
        for file in sorted((plain/'internal/changes').glob('*.go')):
            rel = file.relative_to(plain)
            patch.extend(difflib.unified_diff(file.read_text().splitlines(True), (profiled/rel).read_text().splitlines(True),
                                             fromfile='a/'+str(rel), tofile='b/'+str(rel)))
        (out/'instrumentation.patch').write_text(''.join(patch))
        report['go'] = run(['go', 'version'], source, env, out/'go-version.txt').strip()
        report['filesystem'] = filesystem(scratch, env, out, 'fixture-filesystem')
        bins = {}
        for name, root in (('plain', plain), ('attribution', profiled)):
            binary = scratch/(name+'.test')
            command = ['go', 'test', '-c', '-o', str(binary), './internal/changes']
            report['commands'].append(command)
            run(command, root, env, out/f'build-{name}.txt')
            bins[name] = binary
        fixture = scratch/'fixtures'
        fixture.mkdir()
        env['TMPDIR'] = str(fixture)
        for number in range(args.rounds):
            for roots in benchmark_order(report['roots'], number):
                for position, mode in enumerate(benchmark_order(('plain-a', 'plain-b', 'attribution'), number)):
                    binary = bins['attribution' if mode == 'attribution' else 'plain']
                    command = [str(binary), '-test.run=^$', f'-test.bench=^BenchmarkApplyJournalScaling$/^{roots}$',
                               '-test.benchtime=1x', '-test.count=1']
                    log = out/f'{number:02d}-{roots}-{mode}.txt'
                    raw = run(command, plain, env, log, timeout=600)
                    sample = parse_sample(raw, number)
                    verify_sample(sample, mode, roots)
                    sample.update(roots=roots, mode=mode, position=position, log=log.name)
                    report['samples'].append(sample)
                    print(f'round={number} roots={roots} mode={mode} ms={sample["metrics"]["ns/op"]/1e6:.2f}', flush=True)
                    (out/'report.json').write_text(json.dumps(report, indent=2)+'\n')
        (out/'summary.md').write_text(summarize(report))


if __name__ == '__main__':
    main()
