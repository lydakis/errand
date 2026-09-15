"""Explicit, reviewable identities for native snapshot comparisons."""

import hashlib
from pathlib import Path


def file_inventory(root, paths):
    return {str(path.relative_to(root)): hashlib.sha256(path.read_bytes()).hexdigest()
            for path in sorted(set(paths)) if path.is_file()}


def comparison_inputs(root):
    # Ordinary test files contain benchmark helpers too. Fixtures and module
    # definitions affect the workload even when no *_benchmark_test.go changes.
    files = [root / 'go.mod', root / 'go.sum', *root.rglob('*_test.go')]
    files.extend(path for path in root.rglob('*') if 'testdata' in path.relative_to(root).parts)
    return file_inventory(root, files)


def harness_inputs(directory):
    # Include support modules, not just the CLI entry point. This deliberately
    # records all local Python sources, including modules loaded through runpy.
    return file_inventory(directory, directory.rglob('*.py'))


def require_matching_inputs(versions, allowed):
    paths = set().union(*(files.keys() for files in versions.values()))
    differences = {path for path in paths if len({files.get(path) for files in versions.values()}) > 1}
    unexpected = differences - set(allowed)
    unused = set(allowed) - differences
    if unexpected or unused:
        raise RuntimeError(f'Comparison inputs differ: {sorted(unexpected)}; '
                           f'unused input-difference exceptions: {sorted(unused)}')
    return sorted(differences)
