"""Integrity checks remain active under python -O and PYTHONOPTIMIZE."""
import hashlib


def require(condition, message):
    if not condition:
        raise RuntimeError(f'Invalid hierarchy evidence: {message}')


def verify_archive(path, report):
    require(hashlib.sha256((path.parent/'candidate-inputs.tar.gz').read_bytes()).hexdigest()
            == report['candidate_archive_sha256'], 'candidate archive digest differs')
