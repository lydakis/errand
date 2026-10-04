import base64
import hashlib
import json
from pathlib import Path
import tempfile
import unittest
import zipfile

from prepare_scoop_update import prepare_update


class ScoopUpdateTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.assets = self.root / "assets"
        self.assets.mkdir()
        self.current = self.root / "bucket" / "bucket" / "errand.json"
        self.metadata = self.root / "release.json"
        self.release = {
            "tag_name": "v0.2.0", "draft": False, "prerelease": False,
            "published_at": "2026-09-05T00:00:00Z",
        }
        self.archives = []
        for goarch in ["amd64", "arm64"]:
            archive = self.assets / f"errand_0.2.0_windows_{goarch}.zip"
            with zipfile.ZipFile(archive, "w") as contents:
                contents.writestr("errand.exe", f"errand {goarch}")
                contents.writestr("LICENSE", "MIT")
            self.archives.append(archive)
        self.write_checksums()

    def write_checksums(self):
        (self.assets / "checksums.txt").write_text("".join(
            f"{hashlib.sha256(path.read_bytes()).hexdigest()}  {path.name}\n"
            for path in self.archives
        ))

    def prepare(self, tag="v0.2.0"):
        self.metadata.write_text(json.dumps(self.release))
        return prepare_update(tag, self.metadata, self.assets, self.current)

    def test_first_publication_and_upgrade_request(self):
        request = self.prepare()
        manifest = json.loads(base64.b64decode(request["content"]))
        self.assertEqual(manifest["version"], "0.2.0")
        self.assertEqual(manifest["bin"], "errand.exe")
        arm64 = manifest["architecture"]["arm64"]
        self.assertEqual(arm64["url"], "https://github.com/lydakis/errand/releases/download/"
                                       "v0.2.0/errand_0.2.0_windows_arm64.zip")
        self.assertEqual(arm64["hash"], hashlib.sha256(self.archives[1].read_bytes()).hexdigest())
        self.assertIn("64bit", manifest["architecture"])
        self.assertNotIn("sha", request)
        written = self.current.read_bytes()

        old = written.replace(b'"0.2.0"', b'"0.1.0"')
        self.current.write_bytes(old)
        request = self.prepare()
        self.assertEqual(request["sha"], hashlib.sha1(
            b"blob " + str(len(old)).encode() + b"\0" + old
        ).hexdigest())
        self.assertEqual(self.current.read_bytes(), written)

    def test_rerun_and_older_release_do_not_write(self):
        self.prepare()
        self.assertIsNone(self.prepare())
        newer = self.current.read_bytes().replace(b'"0.2.0"', b'"0.10.0"')
        self.current.write_bytes(newer)
        self.assertIsNone(self.prepare())
        self.assertEqual(self.current.read_bytes(), newer)

    def test_same_version_with_different_content_is_refused(self):
        self.prepare()
        self.current.write_bytes(self.current.read_bytes().replace(b"errand.exe", b"other.exe"))
        with self.assertRaisesRegex(ValueError, "same version"):
            self.prepare()

    def test_rejects_unpublished_prerelease_and_bad_assets(self):
        for change in [{"draft": True}, {"prerelease": True}, {"published_at": None}]:
            with self.subTest(change=change):
                self.release.update(change)
                with self.assertRaisesRegex(ValueError, "published"):
                    self.prepare()
                self.setUp()
        with self.assertRaisesRegex(ValueError, "stable tag"):
            self.prepare("v0.2.0-rc.1")
        self.archives[0].write_bytes(b"tampered")
        with self.assertRaisesRegex(ValueError, "checksum mismatch"):
            self.prepare()
        with zipfile.ZipFile(self.archives[0], "w") as contents:
            contents.writestr("bin/errand.exe", "nested")
        self.write_checksums()
        with self.assertRaisesRegex(ValueError, "no errand.exe"):
            self.prepare()
        self.assertFalse(self.current.exists())


if __name__ == "__main__":
    unittest.main()
