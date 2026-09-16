"""Inspect retained fetch iterations and burst phases without new measurements."""
import json
from pathlib import Path
import statistics
import sys
import tarfile

from evidence import require, verify_archive


def timed_fetch_rows(raw, iterations):
    # Go calibrates at N=1 before the requested fixed count. Each timed run
    # starts at sample zero; never mix calibration with the final N iterations.
    rows = []
    for line in raw.splitlines():
        if line.startswith('EVALUATION '):
            row = json.loads(line.removeprefix('EVALUATION '))
            if row.get('mode') != 'fetch': continue
            if row['sample'] == 0: rows = []
            require(row['sample'] == len(rows), 'fetch iteration sequence')
            require(row['seconds'] > 0, 'fetch duration')
            rows.append(row)
    require(len(rows) == iterations, 'fetch iteration count')
    return rows


def render(path):
    r = json.loads(path.read_text())
    require(r['status'] == 'complete', 'incomplete campaign')
    verify_archive(path, r)
    lines = [f'## {path.parent.name}', '',
             'All timed fetch iterations are retained. Calibration is excluded; no timed outliers are removed.', '',
             '| Case | Build | Round | Iteration milliseconds |', '|---|---|---:|---|']
    with tarfile.open(path.parent/'raw-logs.tar.gz') as tar:
        for s in r.get('commands', r.get('samples', [])):
            if not s['case'].startswith('fetch-'): continue
            prefix = 'command-' if 'commands' in r else ''
            member = f"{prefix}{s['case']}-{s['round']}-{s['build']}.txt"
            raw = tar.extractfile(member).read().decode()
            rows = timed_fetch_rows(raw, s['iterations'])
            require(all(row['persistent'] == (s['case']=='fetch-true') for row in rows), 'fetch mode')
            values = ', '.join(f"{row['seconds']*1000:.1f}" for row in rows)
            lines.append(f"| {s['case']} | {s['build']} | {s['round']} | {values} |")
    if 'commands' in r:
        xs = [s for s in r['commands'] if s['case']=='watch-burst']
        pairs = {(s['build'],s['round']):s['metrics'] for s in xs}
        require(len(xs) == len(pairs) == 12, 'burst sample count')
        lines += ['', '| Burst metric | Baseline median | Candidate median | Median paired ratio |', '|---|---:|---:|---:|']
        for metric in ('ns/op','settle-ms/op','cpu-ms/op','client-ms/op','stage-ms/op','apply-ms/op','resamples/op'):
            a,b = [statistics.median(pairs[build,n][metric] for n in range(6)) for build in ('baseline','candidate')]
            ratio = statistics.median(pairs['candidate',n][metric]/pairs['baseline',n][metric] for n in range(6))
            lines.append(f'| {metric} | {a:.3f} | {b:.3f} | {ratio:.3f} |')
    return '\n'.join(lines)


if __name__ == '__main__':
    print('# Retained caller diagnostics\n')
    for name in sys.argv[1:]:
        print(render(Path(name)))
        print()
