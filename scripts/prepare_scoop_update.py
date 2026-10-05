"""Verify a published stable release and prepare a Scoop bucket Contents API update."""

import argparse
import base64
import hashlib
import json
from pathlib import Path
import re
import zipfile


STABLE_VERSION = r"(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)"
ARCHITECTURES = {"64bit": "amd64", "arm64": "arm64"}


def render_manifest(version: str, hashes: dict[str, str]) -> str:
    manifest = {
        "version": version,
        "description": "Personal job runner for machines you own (Windows runner)",
        "homepage": "https://github.com/lydakis/errand",
        "license": "MIT",
        "notes": "Run `errand setup` to start the runner, and again after each update.",
        "architecture": {
            scoop: {
                "url": f"https://github.com/lydakis/errand/releases/download/v{version}/"
                       f"errand_{version}_windows_{goarch}.zip",
                "hash": hashes[goarch],
            }
            for scoop, goarch in ARCHITECTURES.items()
        },
        "bin": "errand.exe",
    }
    return json.dumps(manifest, indent=4) + "\n"


def prepare_update(tag: str, metadata: Path, assets: Path, current: Path):
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
    hashes = {}
    for goarch in ARCHITECTURES.values():
        archive = assets / f"errand_{version}_windows_{goarch}.zip"
        digest = hashlib.sha256(archive.read_bytes()).hexdigest()
        if checksums.get(archive.name) != digest:
            raise ValueError(f"checksum mismatch or missing checksum for {archive.name}")
        with zipfile.ZipFile(archive) as contents:
            if "errand.exe" not in contents.namelist():
                raise ValueError(f"{archive.name} has no errand.exe at its root")
        hashes[goarch] = digest
    manifest = render_manifest(version, hashes).encode()

    request = {
        "message": f"Update errand to {version}",
        "branch": "main",
        "content": base64.b64encode(manifest).decode(),
    }
    if current.exists():
        previous = current.read_bytes()
        previous_version = json.loads(previous).get("version")
        previous_match = re.fullmatch(STABLE_VERSION, str(previous_version))
        if not previous_match:
            raise ValueError("current manifest must have one stable version")
        if tuple(map(int, previous_match.groups())) > tuple(map(int, match.groups())) or previous == manifest:
            return None
        if previous_version == version:
            raise ValueError("refusing to replace a different manifest with the same version")
        # GitHub requires the previous Git blob ID to guard against concurrent edits.
        request["sha"] = hashlib.sha1(
            b"blob " + str(len(previous)).encode() + b"\0" + previous
        ).hexdigest()
    current.parent.mkdir(parents=True, exist_ok=True)
    current.write_bytes(manifest)
    return request


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("tag")
    parser.add_argument("metadata", type=Path)
    parser.add_argument("assets", type=Path)
    parser.add_argument("current", type=Path)
    parser.add_argument("request", type=Path)
    args = parser.parse_args()
    args.request.unlink(missing_ok=True)
    try:
        request = prepare_update(args.tag, args.metadata, args.assets, args.current)
        if request is not None:
            args.request.write_text(json.dumps(request))
        else:
            print("Bucket already contains this version or a newer release; no update needed.")
    except (ValueError, OSError, zipfile.BadZipFile) as error:
        parser.error(str(error))


if __name__ == "__main__":
    main()
