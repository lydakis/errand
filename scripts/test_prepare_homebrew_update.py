import base64
import hashlib
import json
from pathlib import Path
import tempfile
import unittest

from homebrew_formula import render_formula
from prepare_homebrew_update import prepare_update


class HomebrewUpdateTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.assets = self.root / "assets"
        self.assets.mkdir()
        self.current = self.root / "tap" / "Formula" / "errand.rb"
        self.metadata = self.root / "release.json"
        self.release = {
            "tag_name": "v0.2.0", "draft": False, "prerelease": False,
            "published_at": "2026-09-05T00:00:00Z",
        }
        self.archive = self.assets / "errand_0.2.0_source.tar.gz"
        self.archive.write_bytes(b"release source")
        self.formula = render_formula("0.2.0", self.archive).encode()
        (self.assets / "errand.rb").write_bytes(self.formula)
        self.write_checksums()

    def write_checksums(self):
        (self.assets / "checksums.txt").write_text("".join(
            f"{hashlib.sha256(path.read_bytes()).hexdigest()}  {path.name}\n"
            for path in [self.archive, self.assets / "errand.rb"]
        ))

    def prepare(self, tag="v0.2.0"):
        self.metadata.write_text(json.dumps(self.release))
        return prepare_update(tag, self.metadata, self.assets, self.current)

    def test_first_publication_and_upgrade_request(self):
        request = self.prepare()
        self.assertEqual(base64.b64decode(request["content"]), self.formula)
        self.assertEqual(request["branch"], "main")
        self.assertNotIn("sha", request)
        self.assertEqual(self.current.read_bytes(), self.formula)

        old = self.formula.replace(b'0.2.0', b'0.1.0')
        self.current.write_bytes(old)
        request = self.prepare()
        self.assertEqual(request["sha"], hashlib.sha1(
            b"blob " + str(len(old)).encode() + b"\0" + old
        ).hexdigest())
        self.assertEqual(self.current.read_bytes(), self.formula)

    def test_rerun_and_older_release_do_not_write(self):
        self.prepare()
        self.assertIsNone(self.prepare())
        newer = self.formula.replace(b'0.2.0', b'0.10.0')
        self.current.write_bytes(newer)
        self.assertIsNone(self.prepare())
        self.assertEqual(self.current.read_bytes(), newer)

    def test_same_version_cannot_replace_different_formula(self):
        self.prepare()
        changed = self.formula + b"# manual adjustment\n"
        self.current.write_bytes(changed)
        with self.assertRaisesRegex(ValueError, "same version"):
            self.prepare()
        self.assertEqual(self.current.read_bytes(), changed)

    def test_tagged_generator_verifies_asset_before_packaging_correction(self):
        legacy = self.formula.replace(b'  sha256 ', b'  version "0.2.0"\n  sha256 ')
        (self.assets / "errand.rb").write_bytes(legacy)
        self.write_checksums()
        self.metadata.write_text(json.dumps(self.release))
        generator = self.root / "tagged_generator.py"
        generator.write_text(
            "import pathlib, sys\n"
            f"pathlib.Path(sys.argv[3]).write_bytes({legacy!r})\n"
        )
        request = prepare_update("v0.2.0", self.metadata, self.assets, self.current,
                                 generator=generator)
        self.assertEqual(base64.b64decode(request["content"]), self.formula)
        self.assertEqual((self.assets / "errand.rb").read_bytes(), legacy)
        self.assertIsNone(prepare_update("v0.2.0", self.metadata, self.assets,
                                        self.current, generator=generator))
        (self.assets / "errand.rb").write_bytes(legacy + b"# unexpected\n")
        self.write_checksums()
        with self.assertRaisesRegex(ValueError, "generated formula"):
            prepare_update("v0.2.0", self.metadata, self.assets, self.current,
                           generator=generator)

    def test_requires_matching_published_stable_release(self):
        for field, value in [
            ("draft", True), ("prerelease", True), ("published_at", None),
            ("tag_name", "v0.3.0"), ("draft", None),
        ]:
            with self.subTest(field=field):
                original = self.release[field]
                self.release[field] = value
                with self.assertRaises(ValueError):
                    self.prepare()
                self.release[field] = original
        for tag in ["v0.2.0-rc.1", "v01.2.0", "../v0.2.0", "0.2.0"]:
            with self.subTest(tag=tag), self.assertRaises(ValueError):
                self.prepare(tag)
        self.assertFalse(self.current.exists())

    def test_checks_both_assets_and_exact_formula(self):
        for path in [self.archive, self.assets / "errand.rb"]:
            with self.subTest(path=path.name):
                original = path.read_bytes()
                path.write_bytes(original + b"changed")
                with self.assertRaisesRegex(ValueError, "checksum"):
                    self.prepare()
                path.write_bytes(original)
        (self.assets / "errand.rb").write_bytes(self.formula + b"# unexpected\n")
        self.write_checksums()
        with self.assertRaisesRegex(ValueError, "generated formula"):
            self.prepare()
        self.assertFalse(self.current.exists())

    def test_rejects_missing_or_duplicate_checksum(self):
        checksums = self.assets / "checksums.txt"
        original = checksums.read_text()
        for content in ["", original + original]:
            with self.subTest(content=content):
                checksums.write_text(content)
                with self.assertRaises(ValueError):
                    self.prepare()


if __name__ == "__main__":
    unittest.main()
