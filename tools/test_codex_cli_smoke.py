"""Acceptance guard regressions; no gateway, credentials, or fake CLI needed."""

import contextlib
import io
import json
import os
import tempfile
from types import SimpleNamespace
from pathlib import Path
import unittest
from unittest.mock import patch

import codex_cli_smoke as smoke


THREAD = "0199a213-81c0-7800-8aa1-bbab2a035a53"


def process_result(extra=(), reply="memory-test", thread=THREAD):
    events = [{"type": "thread.started", "thread_id": thread}, {"type": "turn.started"}]
    events.extend(extra)
    events.extend([
        {"type": "item.completed", "item": {"type": "agent_message", "text": reply}},
        {"type": "turn.completed", "usage": {"input_tokens": 10, "output_tokens": 3}},
    ])
    return {"stdout": "\n".join(json.dumps(event) for event in events), "stderr": "",
            "returncode": 0, "timed_out": False, "elapsed_seconds": 0.1}


class AcceptanceTests(unittest.TestCase):
    def test_isolated_windows_home_enables_sandbox_without_relaxing_policy(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "config.toml"
            with patch.object(smoke.os, "name", "nt"):
                smoke.write_config(path, SimpleNamespace(model="test", base_url="http://127.0.0.1:1/v1", timeout=30))
            text = path.read_text()
        self.assertIn('sandbox_mode = "read-only"', text)
        self.assertIn('approval_policy = "never"', text)
        self.assertIn('[windows]\nsandbox = "unelevated"', text)
        self.assertNotIn('danger-full-access', text)

    def test_memory_resume_requires_matching_history_and_thread(self):
        report, thread = smoke.check_turn(process_result(), "memory-test", expected_thread=THREAD)
        self.assertTrue(report["passed"])
        self.assertEqual(thread, THREAD)
        report, _ = smoke.check_turn(process_result(reply="wrong"), "memory-test", expected_thread=THREAD)
        self.assertFalse(report["passed"])
        report, _ = smoke.check_turn(process_result(), "memory-test", expected_thread="different")
        self.assertFalse(report["passed"])

    def test_error_event_fails_even_with_completed_turn_and_correct_answer(self):
        report, _ = smoke.check_turn(process_result([{"type": "error", "message": "stream failed"}]), "memory-test")
        self.assertFalse(report["passed"])
        self.assertFalse(report["assertions"]["no_error_events"])

    def test_tool_claim_without_command_output_cannot_pass(self):
        report, _ = smoke.check_turn(process_result(reply="shell-test"), "shell-test", tool_marker="shell-test")
        self.assertFalse(report["passed"])
        command = {"type": "item.completed", "item": {"type": "command_execution", "status": "completed",
                   "exit_code": 0, "aggregated_output": "shell-test\n"}}
        report, _ = smoke.check_turn(process_result([command], reply="shell-test"), "shell-test", tool_marker="shell-test")
        self.assertTrue(report["passed"])
        report, _ = smoke.check_turn(process_result([command]), "memory-test")
        self.assertFalse(report["passed"], "memory checks must not permit reading history with shell")

    def test_non_json_stdout_cannot_pass(self):
        result = process_result()
        result["stdout"] += "\nnot-json"
        report, _ = smoke.check_turn(result, "memory-test")
        self.assertFalse(report["passed"])
        self.assertIn("invalid_jsonl", report["failure"])

    def test_missing_credentials_do_not_launch_cli(self):
        stream = io.StringIO()
        with patch.dict(os.environ, {}, clear=True), patch.object(smoke, "run_process") as run:
            with contextlib.redirect_stdout(stream):
                status = smoke.main(["--base-url", "http://127.0.0.1:8080/v1", "--model", "test-model"])
        self.assertEqual(status, 1)
        self.assertIn("missing_api_key", json.loads(stream.getvalue())["failure"])
        run.assert_not_called()

    def test_environment_isolates_provider_secrets(self):
        with patch.dict(os.environ, {"OPENAI_API_KEY": "old-secret", "CODEX_HOME": "real-home",
                                     "CODEX_THREAD_ID": "parent-session", "PATH": "executable-path"}, clear=True):
            child = smoke.isolated_environment(Path("temporary-home"), "chosen-key")
            self.assertEqual(os.environ["CODEX_HOME"], "real-home")
        self.assertNotIn("OPENAI_API_KEY", child)
        self.assertNotIn("CODEX_THREAD_ID", child)
        self.assertEqual(child[smoke.KEY_ENV], "chosen-key")
        self.assertEqual(child["CODEX_HOME"], "temporary-home")

    def test_secret_redaction_preserves_json(self):
        secret = 'key-with-"-and-backslash-\\'
        cleaned = smoke.render_report({"message": "error " + secret + " Bearer other-token sk-1234567890"}, secret)
        self.assertNotIn(secret, cleaned)
        self.assertNotIn("other-token", cleaned)
        self.assertNotIn("sk-1234567890", cleaned)
        json.loads(cleaned)
        self.assertEqual(json.loads(smoke.render_report({"message": 'literal " value'}, '"')),
                         {"message": "literal [REDACTED] value"})


if __name__ == "__main__":
    unittest.main()
