"""Fail-closed source identity for the bounded hierarchy comparisons."""
import difflib
import hashlib

from snapshot_provenance import file_inventory, require_matching_inputs

BASELINE = 'dd9fb846bd600eeb4e0e984926bd8e4da54c7328'
BASELINE_ARCHIVE_SHA256 = 'da592f9fca320f016ab211d9879d595d43d5c306036e8937defc7de8f4422bcb'


def verify_baseline(path):
    digest = hashlib.sha256(path.read_bytes()).hexdigest()
    if digest != BASELINE_ARCHIVE_SHA256:
        raise RuntimeError(f'Unexpected baseline archive: {digest}; expected {BASELINE_ARCHIVE_SHA256}')
    return digest


def production_inputs(root, exclude=()):
    paths = [p for p in root.rglob('*')
             if p.suffix in ('.go', '.s', '.c', '.h', '.syso')
             and not p.name.endswith('_test.go') and 'testdata' not in p.parts
             and not any(p.is_relative_to(e) for e in exclude)]
    return file_inventory(root, [*paths, root/'go.mod', root/'go.sum'])


def require_production_diff(baseline, candidate, allowed=('internal/manifest/snapshot.go',), exclude=()):
    inventories = dict(baseline=production_inputs(baseline), candidate=production_inputs(candidate, exclude))
    changed = require_matching_inputs(inventories, allowed)
    return {p: dict(baseline=inventories['baseline'].get(p), candidate=inventories['candidate'].get(p),
                    patch=''.join(difflib.unified_diff(
                        (baseline/p).read_text().splitlines(keepends=True) if (baseline/p).exists() else [],
                        (candidate/p).read_text().splitlines(keepends=True) if (candidate/p).exists() else [],
                        fromfile='baseline/'+p, tofile='candidate/'+p))) for p in changed}
