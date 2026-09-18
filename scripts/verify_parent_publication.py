#!/usr/bin/env python3
"""Verify the bounded parent-publication comparison against its frozen sources."""
import argparse
import io
import json
from pathlib import Path
import subprocess
import tarfile

from apply_followup_evidence import archive_production, checkout_production, median_ms, reductions, verify_source_transition
from verify_apply_followup import verify

BASELINE_COMMIT = 'fd3ce973ed6610911461c72a446fbdb83f9af54e'
ARCHIVE_CHANGES = {'internal/changes/apply_group.go',
                   'internal/changes/apply_group_parents_test.go'}


def comparison(base):
    lines = ['Durations are individual-operation medians. Reductions are medians of same-round ratios, '
             'not ratios of the displayed medians. Faster pairs count individual rounds.', '',
             '| Host / workload | Baseline A / B | Candidate | Paired reduction A / B | Faster pairs A / B |',
             '|---|---:|---:|---:|---:|']
    for host in ('apfs', 'btrfs'):
        report = json.loads((base/host/'report.json').read_text())
        for case in ('tiny', 'flat128', 'parents8', 'parents128'):
            a, b, c = [median_ms(report, case, mode) for mode in ('reference-a', 'reference-b', 'candidate')]
            reduction, wins = reductions(report, case, 'candidate')
            lines.append(f'| {host.upper()} / {case} | {a:.2f} / {b:.2f} ms | {c:.2f} ms | {reduction} | {wins} |')
    return '\n'.join(lines)+'\n'


def go_input(name):
    return name.endswith('.go') or name in ('go.mod', 'go.sum')


def git_go_inputs(checkout):
    names = subprocess.check_output(
        ['git', '-C', str(checkout), 'ls-tree', '-r', '--name-only', '-z', BASELINE_COMMIT]
    ).decode().rstrip('\0').split('\0')
    names = [name for name in names if go_input(name)]
    raw = subprocess.check_output(
        ['git', '-C', str(checkout), 'archive', BASELINE_COMMIT, '--', *names])
    with tarfile.open(fileobj=io.BytesIO(raw)) as archive:
        return {m.name: archive.extractfile(m).read() for m in archive if m.isfile()}


def verify_inputs(base, checkout):
    sources, identities = {}, {}
    for variant in ('baseline', 'candidate'):
        source = base/'inputs'/variant
        identities[variant] = archive_production(source)  # Validate the frozen archive digest.
        with tarfile.open(source/'inputs.tar.gz') as archive:
            members = archive.getmembers()
            if any(not m.isfile() for m in members) or len({m.name for m in members}) != len(members):
                raise ValueError('Unexpected or duplicate archive member')
            sources[variant] = {m.name: archive.extractfile(m).read() for m in members}
    before, after = sources['baseline'], sources['candidate']
    if {n: raw for n, raw in before.items() if go_input(n)} != git_go_inputs(checkout):
        raise ValueError(f'Baseline differs from {BASELINE_COMMIT}')
    changed = {name for name in before.keys() | after.keys() if before.get(name) != after.get(name)}
    if changed != ARCHIVE_CHANGES:
        raise ValueError(f'Unexpected archive differences: {sorted(changed)}')
    record = json.loads((base/'review-source.json').read_text())
    if record['frozen_input_sha256'] != identities['candidate'][0]:
        raise ValueError('Review source uses a different benchmark input')
    verify_source_transition(identities['candidate'][1], checkout_production(checkout), record['changes'])


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--base', type=Path, default=Path('docs/benchmarks/parent-publication'))
    parser.add_argument('--checkout', type=Path, default=Path.cwd())
    args = parser.parse_args()
    for host in ('apfs', 'btrfs'):
        verify(args.base/host, args.base/'inputs')
    verify_inputs(args.base, args.checkout)
    if (args.base/'comparison.md').read_text() != comparison(args.base):
        raise ValueError('Paired comparison differs')
    print('Verified paired reductions, win counts, Git baseline, all archive differences and recorded review source')


if __name__ == '__main__':
    main()
