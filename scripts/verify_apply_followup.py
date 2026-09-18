#!/usr/bin/env python3
"""Recheck complete reports against raw operation logs and frozen source identities."""
import argparse
import hashlib
import json
from pathlib import Path
import tarfile

from benchmark_apply_followup import summary
from benchmark_snapshot_integration import parse_sample
from apply_followup_evidence import CAMPAIGNS, decision_tables, verify_decision_tables, verify_review_source


def verify(directory, inputs):
    report = json.loads((directory/'report.json').read_text())
    if report['status'] != 'complete' or report.get('cleanup_failure'):
        raise ValueError('Incomplete campaign or failed cleanup')
    for variant, identity in report['inputs'].items():
        source = inputs/variant
        if json.loads((source/'identity.json').read_text()) != identity:
            raise ValueError('Source identity record differs')
        digest = hashlib.sha256()
        with tarfile.open(source/'inputs.tar.gz') as archive:
            for member in archive:
                digest.update(member.name.encode()+b'\0'+archive.extractfile(member).read()+b'\0')
        if digest.hexdigest() != identity['input_sha256']:
            raise ValueError('Source archive differs')
    expected = {(case, mode, number) for case, rounds in report['case_rounds'].items()
                for mode in report['modes'] for number in range(rounds)}
    seen = set()
    for sample in report['samples']:
        key = sample['case'], sample['mode'], sample['round']
        if key not in expected or key in seen:
            raise ValueError('Unexpected or duplicate operation')
        seen.add(key)
        raw = (directory/sample['log']).read_bytes()
        if hashlib.sha256(raw).hexdigest() != sample['log_sha256']:
            raise ValueError('Raw log hash mismatch')
        parsed = parse_sample(raw.decode(), sample['round'])
        if any(parsed[k] != sample[k] for k in parsed) or parsed['iterations'] != 1:
            raise ValueError('Raw operation differs from report')
        modes = report['modes']
        rotated = modes[sample['round'] % len(modes):]+modes[:sample['round'] % len(modes)]
        if rotated[sample['position']] != sample['mode']:
            raise ValueError('Unexpected execution position')
    if seen != expected:
        raise ValueError('Missing operation samples')
    if (directory/'summary.md').read_text() != summary(report):
        raise ValueError('Regenerated summary differs')
    print(f'{directory}: verified {len(seen)} individual observations and frozen inputs')


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('reports', nargs='+', type=Path)
    parser.add_argument('--inputs', type=Path, default=Path('docs/benchmarks/apply-followup/inputs'))
    parser.add_argument('--checkout', type=Path, default=Path.cwd(), help='Checkout whose production source must match the recorded review edits')
    args = parser.parse_args()
    base = args.inputs.parent
    # Decision tables consume all six campaigns; verify every source report too.
    for directory in dict.fromkeys([*(base/name for name in CAMPAIGNS), *args.reports]):
        verify(directory, args.inputs)
    verify_decision_tables((base.parent.parent/'APPLY_FOLLOWUP.md').read_text(), decision_tables(base))
    verify_review_source(args.checkout, base)
