"""Mock-only regression checks; never invoke real Clash, SSH or network sockets."""
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import Mock, patch


SPEC = importlib.util.spec_from_file_location("mocked_integration_env", Path(__file__).with_name("integration_env.py"))
CONNECTOR = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(CONNECTOR)


def completed(code=0, body=None):
    return subprocess.CompletedProcess([], code, json.dumps(body).encode() if body is not None else b"", b"")


class ClashConnectorTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = patch.object(CONNECTOR, "ROOT", Path(self.directory.name))
        self.root.start(); self.addCleanup(self.root.stop)
        self.environment = patch.dict(os.environ, {"CODEX_AGENT_CLASH_REGION": "hk", "CODEX_AGENT_SSH_PROXY": "http://127.0.0.1:7897"}, clear=True)
        self.environment.start(); self.addCleanup(self.environment.stop)
        self.fake_client = Mock()
        self.direct = patch.object(CONNECTOR, "_connect", return_value=self.fake_client)
        self.connect = self.direct.start(); self.addCleanup(self.direct.stop)
        self.child = patch.object(CONNECTOR.subprocess, "run")
        self.run = self.child.start(); self.addCleanup(self.child.stop)
        self.lock_patch = patch("msvcrt.locking")
        self.locking = self.lock_patch.start(); self.addCleanup(self.lock_patch.stop)
        self.state = {"group": "synthetic-selector", "previous": "synthetic-original", "selected": "synthetic-hk"}

    def assert_unlocked(self):
        import msvcrt
        self.assertEqual(self.locking.call_args.args[1:], (msvcrt.LK_UNLCK, 1))

    def test_default_does_not_select_or_lock(self):
        os.environ.pop("CODEX_AGENT_CLASH_REGION")
        self.assertIs(CONNECTOR.connect(), self.fake_client)
        self.run.assert_not_called(); self.locking.assert_not_called()

    def test_invalid_region_rejected_without_changes(self):
        for region in ("us", "HK", "../hk", "hk\n"):
            with self.subTest(region=region):
                os.environ["CODEX_AGENT_CLASH_REGION"] = region
                with self.assertRaises(ValueError): CONNECTOR.connect()
        self.run.assert_not_called(); self.connect.assert_not_called(); self.locking.assert_not_called()

    def test_invalid_proxy_rejected_without_changes(self):
        for proxy in ("http://127.0.0.1:17897", "http://user:pass@127.0.0.1:7897", "http://example.invalid:7897", "socks5://127.0.0.1:7897", "http://127.0.0.1:7897/path"):
            with self.subTest(proxy=proxy):
                os.environ["CODEX_AGENT_SSH_PROXY"] = proxy
                with self.assertRaises(ValueError): CONNECTOR.connect()
        self.run.assert_not_called(); self.connect.assert_not_called(); self.locking.assert_not_called()

    def test_authorized_regions_restore_after_success(self):
        for region in ("hk", "jp", "sg"):
            with self.subTest(region=region):
                os.environ["CODEX_AGENT_CLASH_REGION"] = region
                self.run.side_effect = [completed(body=self.state), completed(body={"restored": True})]
                self.assertIs(CONNECTOR.connect(), self.fake_client)
                self.assertEqual(self.run.call_args_list[-2].args[0][-2:], ["select", region])
                self.assertEqual(self.run.call_args.args[0][-2], "restore")
                self.assertEqual(json.loads(self.run.call_args.args[0][-1]), self.state)
                self.assert_unlocked()
        self.fake_client.close.assert_not_called()

    def test_failed_ssh_restores_and_unlocks(self):
        self.run.side_effect = [completed(body=self.state), completed(body={"restored": True})]
        self.connect.side_effect = OSError("synthetic SSH failure")
        with self.assertRaisesRegex(OSError, "synthetic SSH failure"): CONNECTOR.connect()
        self.assertEqual(self.run.call_args.args[0][-2], "restore")
        self.assert_unlocked()

    def test_failed_select_never_connects_and_unlocks(self):
        self.run.return_value = completed(code=1)
        with self.assertRaisesRegex(RuntimeError, "could not select"): CONNECTOR.connect()
        self.connect.assert_not_called(); self.assert_unlocked()

    def test_select_timeout_never_connects_and_unlocks(self):
        self.run.side_effect = subprocess.TimeoutExpired("mock-node", 15)
        with self.assertRaises(subprocess.TimeoutExpired): CONNECTOR.connect()
        self.connect.assert_not_called(); self.assert_unlocked()

    def test_restore_nonzero_closes_client_and_unlocks(self):
        self.run.side_effect = [completed(body=self.state), completed(code=1)]
        with self.assertRaisesRegex(RuntimeError, "could not be restored"): CONNECTOR.connect()
        self.fake_client.close.assert_called_once(); self.assert_unlocked()

    def test_restore_timeout_closes_client_and_unlocks(self):
        self.run.side_effect = [completed(body=self.state), subprocess.TimeoutExpired("mock-node", 15)]
        with self.assertRaises((subprocess.TimeoutExpired, RuntimeError)): CONNECTOR.connect()
        self.fake_client.close.assert_called_once(); self.assert_unlocked()

    def test_restore_spawn_error_closes_client_and_unlocks(self):
        self.run.side_effect = [completed(body=self.state), OSError("synthetic node unavailable")]
        with self.assertRaises((OSError, RuntimeError)): CONNECTOR.connect()
        self.fake_client.close.assert_called_once(); self.assert_unlocked()

    def test_lock_contention_then_success(self):
        self.locking.side_effect = [OSError("busy"), None, None]
        self.run.side_effect = [completed(body=self.state), completed(body={"restored": True})]
        with patch.object(CONNECTOR.time, "sleep") as sleep:
            self.assertIs(CONNECTOR.connect(), self.fake_client)
        sleep.assert_called_once_with(.2); self.assert_unlocked()

    def test_lock_timeout_does_not_unlock_an_unowned_lock(self):
        self.locking.side_effect = OSError("busy")
        with patch.object(CONNECTOR.time, "monotonic", side_effect=[0, 91]):
            with self.assertRaisesRegex(RuntimeError, "lock timed out"): CONNECTOR.connect()
        self.assertEqual(self.locking.call_count, 1)
        self.run.assert_not_called(); self.connect.assert_not_called()

    def test_malformed_select_result_never_connects_and_unlocks(self):
        self.run.return_value = subprocess.CompletedProcess([], 0, b"not-json", b"")
        with self.assertRaises((ValueError, RuntimeError)): CONNECTOR.connect()
        self.connect.assert_not_called(); self.assert_unlocked()


if __name__ == "__main__":
    unittest.main()
