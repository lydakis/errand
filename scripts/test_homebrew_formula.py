import hashlib
from pathlib import Path
import tempfile
import unittest

from homebrew_formula import render_formula


class FormulaTest(unittest.TestCase):
    def test_formula_pins_source_and_embeds_release_version(self):
        with tempfile.TemporaryDirectory() as directory:
            archive = Path(directory) / "errand_0.1.0_source.tar.gz"
            archive.write_bytes(b"release source")
            formula = render_formula("0.1.0", archive)
        self.assertIn(hashlib.sha256(b"release source").hexdigest(), formula)
        self.assertIn("/releases/download/v0.1.0/errand_0.1.0_source.tar.gz", formula)
        self.assertNotIn('  version "', formula)
        self.assertIn('depends_on "go" => :build', formula)
        self.assertIn('-X main.version=#{version}', formula)
        self.assertIn('"./cmd/errand"', formula)

    def test_rejects_invalid_versions_and_wrong_archive(self):
        for version in ["v0.1.0", "0.1", "../0.1.0", '0.1.0\"', "01.2.3"]:
            with self.subTest(version=version), self.assertRaises(ValueError):
                render_formula(version, Path("missing.tar.gz"))
        with self.assertRaises(ValueError):
            render_formula("0.1.0", Path("errand_0.2.0_source.tar.gz"))

    def test_prerelease_uses_its_own_tag(self):
        with tempfile.TemporaryDirectory() as directory:
            archive = Path(directory) / "errand_0.1.0-rc.1_source.tar.gz"
            archive.write_bytes(b"release candidate")
            formula = render_formula("0.1.0-rc.1", archive)
        self.assertIn("/releases/download/v0.1.0-rc.1/", formula)


if __name__ == "__main__":
    unittest.main()
