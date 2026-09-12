import os
from pathlib import Path
import re
import subprocess
import tempfile
import textwrap
import unittest


WORKFLOWS = Path(__file__).resolve().parents[1] / ".github" / "workflows"


def workflow_step(name):
    """Run the actual workflow's shell gate without invoking release tooling."""
    workflow = (WORKFLOWS / "release.yml").read_text()
    match = re.search(
        rf"^      - name: {re.escape(name)}\n(.*?)(?=^      - |\Z)",
        workflow, re.MULTILINE | re.DOTALL,
    )
    if match is None:
        raise AssertionError(f"missing release gate: {name}")
    run = re.search(r"^        run: \|\n((?:^          .*\n|^\n)+)",
                    match[1], re.MULTILINE)
    if run is None:
        raise AssertionError(f"missing shell script for {name}")
    return textwrap.dedent(run[1])


class ReleaseWorkflowTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.output = self.root / "outputs"
        self.git("init", "-b", "main")
        self.git("config", "user.name", "Release test")
        self.git("config", "user.email", "release-test@example.invalid")
        self.git("commit", "--allow-empty", "-m", "Approved release")
        self.approved = self.git("rev-parse", "HEAD")
        self.git("tag", "v1.0.0")
        self.git("tag", "-a", "v1.0.0-rc.1", "-m", "Annotated release")
        self.git("commit", "--allow-empty", "-m", "Newer approved work")
        self.main = self.git("rev-parse", "HEAD")
        self.git("tag", "v1.1.0")
        self.git("update-ref", "refs/remotes/origin/main", self.main)
        self.git("switch", "-c", "unmerged")
        self.git("commit", "--allow-empty", "-m", "Unreviewed release")
        self.unmerged = self.git("rev-parse", "HEAD")
        self.git("tag", "v2.0.0")
        self.git("switch", "main")

    def git(self, *args):
        return subprocess.run(
            ["git", "-c", "core.hooksPath=/dev/null", "-c", "commit.gpgsign=false",
             "-c", "tag.gpgsign=false", "-C", str(self.root), *args],
            check=True, capture_output=True, text=True,
        ).stdout.strip()

    def gate(self, tag, *, commit=None, remote=False):
        env = {**os.environ, "RELEASE_TAG": tag,
               "GITHUB_OUTPUT": str(self.output)}
        if commit is None:
            script = workflow_step("Validate release tag")
            script += workflow_step("Resolve approved release")
        else:
            env["RELEASE_COMMIT"] = commit
            script = workflow_step("Verify release tag before publishing" if remote
                                   else "Verify release checkout")
        return subprocess.run(
            ["bash", "-e", "-u", "-o", "pipefail", "-c", script],
            cwd=self.root, env=env, capture_output=True, text=True,
        )

    def test_approved_lightweight_annotated_and_older_tags(self):
        for tag, commit in [("v1.0.0", self.approved),
                            ("v1.0.0-rc.1", self.approved),
                            ("v1.1.0", self.main)]:
            with self.subTest(tag=tag):
                self.output.unlink(missing_ok=True)
                result = self.gate(tag)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(self.output.read_text(),
                                 f"tag={tag}\ncommit={commit}\n")

    def test_unmerged_or_missing_tag_cannot_publish_a_commit(self):
        for tag in ["v2.0.0", "v9.9.9"]:
            with self.subTest(tag=tag):
                result = self.gate(tag)
                self.assertNotEqual(result.returncode, 0)
                self.assertFalse(self.output.exists())

    def test_merged_release_commit_is_approved(self):
        self.git("merge", "--no-ff", "unmerged", "-m", "Reviewed merge")
        self.git("update-ref", "refs/remotes/origin/main", "HEAD")
        result = self.gate("v2.0.0")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.output.read_text(),
                         f"tag=v2.0.0\ncommit={self.unmerged}\n")

    def test_branch_with_same_name_cannot_replace_release_tag(self):
        self.git("branch", "v1.0.0", self.unmerged)
        result = self.gate("v1.0.0")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.output.read_text(),
                         f"tag=v1.0.0\ncommit={self.approved}\n")

    def test_malformed_or_executable_input_is_rejected(self):
        for tag in ["main", self.approved, "refs/tags/v1.0.0", "../v1.0.0",
                    "v1.0.0-..", "v1.0.0-$(touch injected)",
                    "v1.0.0;touch injected", "v1.0.0\ncommit=untrusted"]:
            with self.subTest(tag=tag):
                result = self.gate(tag)
                self.assertNotEqual(result.returncode, 0)
                self.assertFalse(self.output.exists())
                self.assertFalse((self.root / "injected").exists())

    def test_tag_must_point_to_a_commit_and_main_must_exist(self):
        tree = self.git("rev-parse", "HEAD^{tree}")
        self.git("tag", "v3.0.0", tree)
        self.assertNotEqual(self.gate("v3.0.0").returncode, 0)
        self.git("update-ref", "-d", "refs/remotes/origin/main")
        self.assertNotEqual(self.gate("v1.0.0").returncode, 0)
        self.assertFalse(self.output.exists())

    def test_checkout_verifies_commit_and_selected_tag(self):
        for tag in ["v1.0.0", "v1.0.0-rc.1"]:
            with self.subTest(tag=tag):
                self.git("checkout", "--detach", self.approved)
                result = self.gate(tag, commit=self.approved)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.git("checkout", "--detach", self.main)
                self.assertNotEqual(self.gate(tag, commit=self.approved).returncode, 0)

    def test_tag_moved_after_resolution_blocks_packaging(self):
        self.assertEqual(self.gate("v1.0.0").returncode, 0)
        self.git("checkout", "--detach", self.approved)
        for commit in [self.main, self.unmerged]:
            with self.subTest(commit=commit):
                self.git("tag", "-f", "v1.0.0", commit)
                result = self.gate("v1.0.0", commit=self.approved)
                self.assertNotEqual(result.returncode, 0)
        self.git("tag", "-d", "v1.0.0")
        self.assertNotEqual(self.gate("v1.0.0", commit=self.approved).returncode, 0)

    def test_remote_lightweight_and_annotated_tags_must_still_match(self):
        remote = self.root / "remote.git"
        self.git("clone", "--bare", str(self.root), str(remote))
        self.git("remote", "add", "origin", str(remote))
        for tag in ["v1.0.0", "v1.0.0-rc.1"]:
            with self.subTest(tag=tag):
                result = self.gate(tag, commit=self.approved, remote=True)
                self.assertEqual(result.returncode, 0, result.stderr)
                # Keep the checkout's tag unchanged, as during a long build.
                self.git("--git-dir", str(remote), "update-ref",
                         f"refs/tags/{tag}", self.main)
                result = self.gate(tag, commit=self.approved, remote=True)
                self.assertNotEqual(result.returncode, 0)
                self.git("--git-dir", str(remote), "update-ref", "-d",
                         f"refs/tags/{tag}")
                result = self.gate(tag, commit=self.approved, remote=True)
                self.assertNotEqual(result.returncode, 0)


if __name__ == "__main__":
    unittest.main()
