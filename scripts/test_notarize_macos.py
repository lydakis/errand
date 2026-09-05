import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

from notarize_macos import notarize


class NotarizeTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.binary = self.root / "errand"
        self.binary.write_bytes(b"unsigned fixture")
        self.events = []
        self.status = "Accepted"
        self.raw_response = None
        self.failure = None
        self.environment = {
            "APPLE_DEVELOPER_ID_APPLICATION": "Developer ID Application: Test",
            "ERRAND_SIGNING_KEYCHAIN": str(self.root / "signing.keychain-db"),
            "ERRAND_NOTARY_KEY": str(self.root / "notary.p8"),
            "APP_STORE_CONNECT_KEY_ID": "TESTKEY123",
            "APP_STORE_CONNECT_ISSUER_ID": "00000000-0000-0000-0000-000000000001",
        }

    def run_command(self, command, **kwargs):
        self.events.append(command[0])
        if command[0] == self.failure:
            raise subprocess.CalledProcessError(1, command)
        if command[0] == "codesign" and "--force" in command:
            self.binary.write_bytes(b"signed fixture")
        if command[0] == "ditto":
            self.assertEqual(Path(command[-2]).read_bytes(), b"signed fixture")
        return subprocess.CompletedProcess(command, 0, self.raw_response or json.dumps({
            "status": self.status, "id": "00000000-0000-0000-0000-000000000002",
        }), "")

    def execute(self, target="darwin_arm64", snapshot="false"):
        with patch.dict(os.environ, self.environment, clear=True):
            with patch("notarize_macos.subprocess.run", side_effect=self.run_command):
                notarize(self.binary, target, snapshot)

    def test_only_accepted_signed_binary_can_complete(self):
        self.execute()
        self.assertEqual(self.binary.read_bytes(), b"signed fixture")
        self.assertEqual(self.events, ["codesign", "codesign", "ditto", "xcrun"])

    def test_pending_rejected_and_missing_status_fail(self):
        for status in ["In Progress", "Invalid", "Rejected", None, ""]:
            with self.subTest(status=status):
                self.status = status
                with self.assertRaisesRegex(ValueError, "not accepted"):
                    self.execute()

    def test_tool_failures_prevent_success(self):
        for tool in ["codesign", "ditto", "xcrun"]:
            with self.subTest(tool=tool):
                self.failure = tool
                with self.assertRaises(subprocess.CalledProcessError):
                    self.execute()

    def test_malformed_responses_fail(self):
        for response in ["not JSON", "[]", "{}"]:
            with self.subTest(response=response):
                self.raw_response = response
                with self.assertRaises(ValueError):
                    self.execute()

    def test_snapshots_and_linux_need_no_credentials_or_tools(self):
        self.environment = {}
        for target, snapshot in [("darwin_arm64", "true"), ("linux_amd64_v1", "false")]:
            with self.subTest(target=target):
                self.execute(target, snapshot)
        self.assertEqual(self.events, [])
        self.assertEqual(self.binary.read_bytes(), b"unsigned fixture")

    def test_missing_credentials_fail_before_signing(self):
        for name in list(self.environment):
            with self.subTest(name=name):
                value = self.environment.pop(name)
                with self.assertRaisesRegex(ValueError, name):
                    self.execute()
                self.environment[name] = value
        self.assertEqual(self.events, [])


if __name__ == "__main__":
    unittest.main()
