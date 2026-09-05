"""Verify a published stable release and prepare a Homebrew Contents API update."""

import argparse
import base64
import hashlib
import json
from pathlib import Path
import re
import subprocess
import sys
import tempfile

from homebrew_formula import render_formula


STABLE_VERSION = r"(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)"


def prepare_update(tag: str, metadata: Path, assets: Path, current: Path,
                   *, generator: Path | None = None):
    match = re.fullmatch("v" + STABLE_VERSION, tag)
    if not match:
        raise ValueError("expected a stable tag such as v0.1.0")
    version = tag[1:]
    release = json.loads(metadata.read_text())
    if (release.get("tag_name") != tag or release.get("draft") is not False
            or release.get("prerelease") is not False or not release.get("published_at")):
        raise ValueError("release must match the tag and be published, stable, and not a draft")

    checksums = {}
    for line in (assets / "checksums.txt").read_text().splitlines():
        entry = re.fullmatch(r"([0-9a-f]{64}) [ *](\S+)", line)
        if not entry or entry[2] in checksums:
            raise ValueError("malformed or duplicate checksum entry")
        checksums[entry[2]] = entry[1]
    archive = assets / f"errand_{version}_source.tar.gz"
    formula_path = assets / "errand.rb"
    for path in [archive, formula_path]:
        if checksums.get(path.name) != hashlib.sha256(path.read_bytes()).hexdigest():
            raise ValueError(f"checksum mismatch or missing checksum for {path.name}")
    formula = render_formula(version, archive).encode()
    expected_release_formula = formula
    if generator is not None:
        with tempfile.TemporaryDirectory() as directory:
            expected = Path(directory) / "errand.rb"
            subprocess.run([sys.executable, str(generator), version,
                            str(archive), str(expected)], check=True)
            expected_release_formula = expected.read_bytes()
    if formula_path.read_bytes() != expected_release_formula:
        raise ValueError("release formula does not match the generated formula for this source")

    request = {
        "message": f"Update errand to {version}",
        "branch": "main",
        "content": base64.b64encode(formula).decode(),
    }
    if current.exists():
        previous = current.read_bytes()
        versions = re.findall(
            r'^  url "https://github\.com/lydakis/errand/releases/download/v('
            + STABLE_VERSION + r')/errand_\1_source\.tar\.gz"$',
            previous.decode(), re.MULTILINE)
        if len(versions) != 1:
            raise ValueError("current formula must have one stable version")
        previous_version = tuple(map(int, versions[0][1:]))
        next_version = tuple(map(int, match.groups()))
        if previous_version > next_version or previous == formula:
            return None
        if previous_version == next_version:
            raise ValueError("refusing to replace a different formula with the same version")
        # GitHub requires the previous Git blob ID to guard against concurrent edits.
        request["sha"] = hashlib.sha1(
            b"blob " + str(len(previous)).encode() + b"\0" + previous
        ).hexdigest()
    current.parent.mkdir(parents=True, exist_ok=True)
    current.write_bytes(formula)
    return request


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("tag")
    parser.add_argument("metadata", type=Path)
    parser.add_argument("assets", type=Path)
    parser.add_argument("current", type=Path)
    parser.add_argument("request", type=Path)
    parser.add_argument("--generator", type=Path,
                        help="release tag's generator, used to verify immutable assets")
    args = parser.parse_args()
    args.request.unlink(missing_ok=True)
    try:
        request = prepare_update(args.tag, args.metadata, args.assets, args.current,
                                 generator=args.generator)
        if request is not None:
            args.request.write_text(json.dumps(request))
        else:
            print("Tap already contains this version or a newer release; no update needed.")
    except (ValueError, OSError, subprocess.CalledProcessError) as error:
        parser.error(str(error))


if __name__ == "__main__":
    main()
