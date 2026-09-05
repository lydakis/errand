"""Sign a release binary and require Apple's acceptance before archiving it."""

import argparse
import json
import os
from pathlib import Path
import subprocess
import tempfile


def notarize(binary: Path, target: str, snapshot: str):
    if snapshot not in {"true", "false"}:
        raise ValueError("snapshot must be true or false")
    if snapshot == "true" or not target.startswith("darwin_"):
        return
    required = [
        "APPLE_DEVELOPER_ID_APPLICATION", "ERRAND_SIGNING_KEYCHAIN",
        "ERRAND_NOTARY_KEY", "APP_STORE_CONNECT_KEY_ID", "APP_STORE_CONNECT_ISSUER_ID",
    ]
    for name in required:
        if not os.environ.get(name, "").strip():
            raise ValueError(f"missing required signing setting: {name}")

    subprocess.run([
        "codesign", "--force", "--options", "runtime", "--timestamp",
        "--sign", os.environ["APPLE_DEVELOPER_ID_APPLICATION"],
        "--keychain", os.environ["ERRAND_SIGNING_KEYCHAIN"], str(binary),
    ], check=True)
    subprocess.run(["codesign", "--verify", "--strict", str(binary)], check=True)
    with tempfile.TemporaryDirectory(prefix="errand-notary-") as directory:
        archive = Path(directory) / "errand.zip"
        subprocess.run([
            "ditto", "-c", "-k", "--keepParent", str(binary), str(archive),
        ], check=True)
        result = subprocess.run([
            "xcrun", "notarytool", "submit", str(archive),
            "--key", os.environ["ERRAND_NOTARY_KEY"],
            "--key-id", os.environ["APP_STORE_CONNECT_KEY_ID"],
            "--issuer", os.environ["APP_STORE_CONNECT_ISSUER_ID"],
            "--wait", "--timeout", "20m", "--output-format", "json",
        ], check=True, capture_output=True, text=True)
        response = json.loads(result.stdout)
        if not isinstance(response, dict) or response.get("status") != "Accepted":
            raise ValueError(f"notarization not accepted for {target}: {response}")
        print(f"Notarization accepted for {target}; submission {response.get('id', 'unknown')}")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("binary", type=Path)
    parser.add_argument("target")
    parser.add_argument("snapshot")
    args = parser.parse_args()
    try:
        notarize(args.binary, args.target, args.snapshot)
    except (ValueError, OSError, subprocess.CalledProcessError) as error:
        parser.exit(1, f"Notarization failed: {error}\n")


if __name__ == "__main__":
    main()
