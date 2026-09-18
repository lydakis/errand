"""Derived decision tables and exact production-source provenance for P1b."""
import hashlib
import json
from pathlib import Path
import statistics
import tarfile


CAMPAIGNS = ('apfs', 'btrfs', 'apfs-exchange', 'btrfs-exchange',
             'btrfs-historical-small', 'btrfs-flat-repeat')


def paired(report, case, candidate, reference):
    values = {}
    for mode in (candidate, reference):
        samples = [s for s in report['samples'] if s['case'] == case and s['mode'] == mode]
        values[mode] = {s['round']: s['metrics']['ns/op'] for s in samples}
        if len(values[mode]) != len(samples):
            raise ValueError('Duplicate paired round')
    a, b = values[candidate], values[reference]
    if not a or a.keys() != b.keys() or any(v <= 0 for v in (*a.values(), *b.values())):
        raise ValueError('Missing paired round or nonpositive duration')
    ratios = [a[r] / b[r] for r in a]
    return 100 * (1 - statistics.median(ratios)), sum(v < 1 for v in ratios), len(ratios)


def reductions(report, case, mode):
    stats = [paired(report, case, mode, ref) for ref in ('reference-a', 'reference-b')]
    return (' / '.join(f'{reduction:.1f}%' for reduction, _, _ in stats),
            ' / '.join(f'{wins}/{count}' for _, wins, count in stats))


def median_ms(report, case, mode):
    return statistics.median(s['metrics']['ns/op'] for s in report['samples']
                             if s['case'] == case and s['mode'] == mode) / 1e6


def decision_tables(base):
    reports = {name: json.loads((base/name/'report.json').read_text()) for name in CAMPAIGNS}
    tables = {}
    for key, suffix, candidate, cases in (
        ('main', '', 'parents', ('flat128', 'parents8', 'parents128', 'tiny', 'large', 'watch-small')),
        ('exchange', '-exchange', 'exchange', ('flat128', 'parents8', 'parents128')),
    ):
        lines = ['| Host / workload | Reference A / B | '+('Scratch only | ' if key == 'main' else '')+
                 'Candidate | Paired reduction vs A / B | Faster pairs vs A / B |',
                 '|---|---:|'+('---:|' if key == 'main' else '')+'---:|---:|---:|']
        for host in ('apfs', 'btrfs'):
            report = reports[host+suffix]
            for case in cases:
                medians = [median_ms(report, case, m) for m in ('reference-a', 'reference-b')]
                reduction, wins = reductions(report, case, candidate)
                scratch = f'{median_ms(report, case, "scratch"):.1f} ms | ' if key == 'main' else ''
                lines.append(f'| {host.upper()} / {case} | {medians[0]:.1f} / {medians[1]:.1f} ms | '
                             f'{scratch}{median_ms(report, case, candidate):.1f} ms | {reduction} | {wins} |')
        tables[key] = '\n'.join(lines)
    for key, campaign in (('historical', 'btrfs-historical-small'), ('repeat', 'btrfs-flat-repeat')):
        report = reports[campaign]
        lines = ['| Workload / variant | Reference A / B | Candidate | Paired reduction vs A / B | Faster pairs vs A / B |',
                 '|---|---:|---:|---:|---:|']
        for case in report['cases']:
            for mode in report['modes'][2:]:
                a, b, c = [median_ms(report, case, m) for m in ('reference-a', 'reference-b', mode)]
                reduction, wins = reductions(report, case, mode)
                lines.append(f'| {case} / {mode} | {a:.2f} / {b:.2f} ms | {c:.2f} ms | {reduction} | {wins} |')
        tables[key] = '\n'.join(lines)
    return tables


def verify_decision_tables(document, tables):
    for name, table in tables.items():
        start, end = f'<!-- apply-{name}:start -->', f'<!-- apply-{name}:end -->'
        if document.count(start) != 1 or document.count(end) != 1:
            raise ValueError(f'Missing or duplicate {name} table markers')
        if document.split(start)[1].split(end)[0] != '\n'+table+'\n':
            raise ValueError(f'Derived {name} decision table differs')


def production_path(name):
    return (name.endswith('.go') and not name.endswith('_test.go')) or name in ('go.mod', 'go.sum')


def archive_production(source):
    identity = json.loads((source/'identity.json').read_text())
    digest, files = hashlib.sha256(), {}
    with tarfile.open(source/'inputs.tar.gz') as archive:
        for member in archive:
            raw = archive.extractfile(member).read()
            digest.update(member.name.encode()+b'\0'+raw+b'\0')
            if production_path(member.name):
                files[member.name] = hashlib.sha256(raw).hexdigest()
    if digest.hexdigest() != identity['input_sha256']:
        raise ValueError('Source archive differs')
    return identity['input_sha256'], files


def checkout_production(root):
    paths = [*root.rglob('*.go'), root/'go.mod', root/'go.sum']
    return {p.relative_to(root).as_posix(): hashlib.sha256(p.read_bytes()).hexdigest()
            for p in paths if production_path(p.relative_to(root).as_posix())}


def verify_source_transition(frozen, current, changes):
    expected = dict(frozen)
    for name, change in changes.items():
        if frozen.get(name) != change['before'] or change['before'] == change['after']:
            raise ValueError(f'Invalid source transition: {name}')
        if change['after'] is None:
            expected.pop(name)
        else:
            expected[name] = change['after']
    differences = sorted(name for name in expected.keys() | current.keys()
                         if expected.get(name) != current.get(name))
    if differences:
        raise ValueError(f'Production differs from recorded review source: {differences}')


def verify_review_source(root, base):
    record = json.loads((base/'review-source.json').read_text())
    identity, frozen = archive_production(base/'inputs/parents')
    if identity != record['frozen_input_sha256']:
        raise ValueError('Review source uses a different benchmark input')
    verify_source_transition(frozen, checkout_production(root), record['changes'])
    print(f'Production source matches frozen parents plus {len(record["changes"])} recorded review edits')
