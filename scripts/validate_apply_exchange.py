#!/usr/bin/env python3
"""Run isolated exchange tests without enabling the candidate in the checkout."""
import argparse
import json
import os
from pathlib import Path

from benchmark_apply_followup import extract
from benchmark_snapshot_integration import benchmark_campaign, run


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--output', type=Path, required=True)
    args = parser.parse_args()
    out = args.output.resolve()
    out.mkdir(parents=True, exist_ok=False)
    report = {'commands': []}
    with benchmark_campaign(out, report) as scratch:
        root = scratch/'exchange'
        report['input'] = extract(Path('docs/benchmarks/apply-followup/inputs/exchange'), root)
        for name, command in (
            ('tests', ['go', 'test', './internal/changes', '-run', '^Test(Exchange|Merge|Transfer|Apply|CopyToRoot)', '-skip', '^TestApplySynchronization', '-count=1']),
            ('race', ['go', 'test', '-race', './internal/changes', '-run', '^TestExchange', '-count=1']),
        ):
            report['commands'].append(command)
            run(command, root, dict(os.environ), out/(name+'.txt'), timeout=600)
    print(json.dumps(report))


if __name__ == '__main__':
    main()
