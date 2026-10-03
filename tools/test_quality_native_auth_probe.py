import base64
from contextlib import redirect_stderr, redirect_stdout
import hashlib
import io
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import time
import unittest
from unittest.mock import patch

import quality_native_auth_probe as probe


def fake_token(**changes):
    claims = {"exp": time.time() + 3600, "email": "synthetic-private@example.test",
              probe.AUTH_CLAIM: {"chatgpt_account_id": "synthetic-workspace", "chatgpt_user_id": "synthetic-user", "chatgpt_plan_type": "plus"}}
    claims.update(changes)
    encoded = base64.urlsafe_b64encode(json.dumps(claims).encode()).rstrip(b"=").decode()
    return "synthetic." + encoded + ".signature"


def mock_child(mode, role, record_path):
    if role == "version":
        print("codex-cli 0.154.0" if mode == "wrong_version" else "codex-cli 0.155.0", flush=True)
        return
    def emit(value):
        print(json.dumps(value), flush=True)
    email = "synthetic-private@example.test"
    plan = "self_serve_business_prolite" if mode == "business_plan" else "plus"
    def remote_status(status="disabled", **extra):
        return {"status": status, "serverName": "synthetic-private-server", "installationId": "synthetic-private-installation",
                "environmentId": None, **extra}
    if mode == "spawn_stall":
        descendant = subprocess.Popen([sys.executable, "-c", "import time; time.sleep(90)"],
                                     creationflags=subprocess.CREATE_NO_WINDOW if os.name == "nt" else 0)
        Path(record_path + ".descendant").write_text(str(descendant.pid))
    for line in sys.stdin:
        message = json.loads(line)
        with open(record_path, "a", encoding="utf-8") as record:
            record.write(json.dumps({"method": message.get("method"), "params": message.get("params"),
                "environment": {key: os.environ.get(key) for key in ("OPENAI_API_KEY", "HTTPS_PROXY", "CODEX_HOME")}}) + "\n")
        method = message.get("method")
        if mode in ("stall", "spawn_stall"):
            time.sleep(90)
        if method == "initialize":
            if mode == "bad_json":
                print('{"id":1,"id":1,"result":{}}', flush=True)
                continue
            if mode == "stdout_flood":
                print("x" * (probe.MAX_LINE + 1), flush=True)
                time.sleep(90)
            if mode == "stderr_flood":
                sys.stderr.write("x" * (probe.MAX_OUTPUT + 4096))
                sys.stderr.flush()
                time.sleep(90)
            emit({"id": message["id"], "result": {"codexHome": os.environ["CODEX_HOME"] if mode != "wrong_home" else "/unexpected/home",
                "userAgent": "codex_native_auth_probe/0.155.0 (Synthetic OS; x64) unknown (probe; 1)",
                "platformFamily": "windows" if os.name == "nt" else "unix", "platformOs": "windows" if os.name == "nt" else "linux"}})
            if mode.startswith("startup_"):
                emit({"method": "configWarning", "params": {"summary": "Codex could not find bubblewrap on PATH. Install bubblewrap with your OS package manager. "
                    "See the sandbox prerequisites: https://developers.openai.com/codex/concepts/sandboxing#prerequisites. "
                    "Codex will use the bundled bubblewrap in the meantime.", "details": None}})
                status = remote_status()
                if mode == "startup_bad_remote":
                    status["installationId"] = 7
                elif mode == "startup_extra_remote":
                    status["task"] = "must-not-run"
                elif mode == "startup_unknown_remote_status":
                    status["status"] = "unknown-private-value"
                if mode == "startup_remote_request":
                    emit({"id": 99, "method": "remoteControl/status/changed", "params": status})
                else:
                    emit({"method": "remoteControl/status/changed", "params": status})
                if mode == "startup_remote_flood":
                    for _ in range(8):
                        emit({"method": "remoteControl/status/changed", "params": status})
        elif method == "account/login/start":
            params = message["params"]
            if mode.startswith("startup_"):
                emit({"method": "skills/changed", "params": {"unexpected": params["accessToken"]} if mode == "startup_bad_skills" else {}})
                if mode == "startup_transitions":
                    for status in ("connecting", "connected", "errored", "disabled"):
                        emit({"method": "remoteControl/status/changed", "params": remote_status(status, environmentId="synthetic-private-environment")})
                if mode in ("startup_thread", "startup_turn", "startup_item"):
                    emit({"method": mode.removeprefix("startup_") + "/started", "params": {"private": params["accessToken"]}})
            if mode.startswith("missing_bwrap"):
                warning = {"summary": "Codex could not find bubblewrap on PATH. Install bubblewrap with your OS package manager. "
                    "See the sandbox prerequisites: https://developers.openai.com/codex/concepts/sandboxing#prerequisites. "
                    "Codex will use the bundled bubblewrap in the meantime.", "details": None}
                if mode == "missing_bwrap_null_fields":
                    warning.update(path=None, range=None)
                elif mode == "missing_bwrap_details":
                    warning["details"] = params["accessToken"]
                elif mode == "missing_bwrap_path":
                    warning["path"] = params["chatgptAccountId"]
                elif mode == "missing_bwrap_extra":
                    warning["unexpected"] = "private-extra-value"
                elif mode == "missing_bwrap_modified":
                    warning["summary"] += " extra text"
                emit({"method": "configWarning", "params": warning})
                if mode == "missing_bwrap_duplicate":
                    emit({"method": "configWarning", "params": warning})
            if mode in ("config_warning", "namespace_warning", "unknown_notification", "mcp_notification"):
                notification_method = "configWarning"
                diagnostic = {"summary": params["accessToken"], "details": email, "path": params["chatgptAccountId"]}
                if mode == "namespace_warning":
                    diagnostic = {"summary": "Codex's Linux sandbox uses bubblewrap and needs access to create user namespaces.", "details": None}
                elif mode == "unknown_notification":
                    notification_method = "unknown/" + params["accessToken"]
                elif mode == "mcp_notification":
                    notification_method = "mcpServer/startupStatus/updated"
                emit({"method": notification_method, "params": diagnostic})
                time.sleep(90)
            if mode == "login_error":
                emit({"id": message["id"], "error": {"code": -1, "message": params["accessToken"] + " " + email}})
                continue
            if mode == "server_request":
                emit({"id": 88, "method": "item/tool/call", "params": {"secret": params["accessToken"]}})
                time.sleep(90)
            if mode == "refresh_request":
                emit({"id": 89, "method": "account/chatgptAuthTokens/refresh", "params": {"reason": "unauthorized"}})
                time.sleep(90)
            if mode == "echo_stderr":
                print(params["accessToken"] + " " + params["chatgptAccountId"] + " " + email, file=sys.stderr, flush=True)
            if mode == "persist_auth":
                Path(os.environ["CODEX_HOME"], "auth.json").write_text(json.dumps(params))
            emit({"id": message["id"], "result": {"type": "chatgptAuthTokens"}})
            if mode != "late_notifications":
                emit({"method": "account/login/completed", "params": {"loginId": None, "success": True, "error": None}})
                emit({"method": "account/updated", "params": {"authMode": "chatgpt" if mode == "wrong_auth_mode" else "chatgptAuthTokens", "planType": plan}})
        elif method == "account/read":
            emit({"id": message["id"], "result": {"account": {"type": "chatgpt", "email": "other-private@example.test" if mode == "wrong_email" else email,
                "planType": "pro" if mode == "wrong_plan" else plan}, "requiresOpenaiAuth": True}})
            if mode == "late_notifications":
                emit({"method": "account/login/completed", "params": {"loginId": None, "success": True, "error": None}})
                emit({"method": "account/updated", "params": {"authMode": "chatgptAuthTokens", "planType": "plus"}})
            time.sleep(90)


class Harness:
    def __init__(self, directory, mode="ok"):
        self.directory, self.mode = Path(directory), mode
        self.cli = self.directory / "registered-cli"
        self.cli.write_bytes(b"registered synthetic executable fixture")
        self.token = fake_token()
        self.credential = {"access_token": self.token, "account_id": "synthetic-workspace"}
        self.pins = {"cli_sha256": probe.sha(self.cli.read_bytes()), "token_sha256": probe.sha(self.token),
                     "account_sha256": probe.sha("synthetic-workspace"), "user_sha256": probe.sha("synthetic-user")}
        self.record = self.directory / "protocol.jsonl"
        self.children, self.commands, self.roots = [], [], []

    def child(self, command, root, environment, deadline):
        self.commands.append(command)
        self.roots.append(root)
        role = "version" if command[-1] == "--version" else "app-server"
        mock_command = [sys.executable, str(Path(__file__).resolve()), "--mock-child", self.mode, role, str(self.record)]
        child = probe._Child(mock_command, root, environment, deadline)
        self.children.append(child)
        return child

    def run(self, **overrides):
        kwargs = {"cli": self.cli, "pins": self.pins, "raw_credential": json.dumps(self.credential).encode(),
                  "deadline": time.monotonic() + 8, "child_factory": self.child}
        kwargs.update(overrides)
        return probe.run_probe(**kwargs)

    def assert_clean(self, test, result):
        test.assertTrue(result["cleanup_completed"], result)
        test.assertTrue(all(child.process.poll() is not None for child in self.children))
        test.assertTrue(all(all(child.cleanup_evidence().values()) for child in self.children))
        test.assertTrue(all(not root.exists() for root in self.roots))
        for secret in (self.token, "synthetic-workspace", "synthetic-user", "synthetic-private@example.test", "other-private@example.test"):
            test.assertNotIn(secret, json.dumps(result))
        for secret in ("synthetic-private-server", "synthetic-private-installation", "synthetic-private-environment", "unknown-private-value"):
            test.assertNotIn(secret, json.dumps(result))
        test.assertEqual(result["model_calls"], 0)
        test.assertEqual(result["turns"], 0)


class NativeAuthProbeTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)

    def harness(self, mode="ok"):
        return Harness(self.temporary.name, mode)

    def test_real_mock_child_zero_turn_protocol_and_secret_boundaries(self):
        harness = self.harness()
        with patch.dict(os.environ, {"OPENAI_API_KEY": "must-not-inherit", "HTTPS_PROXY": "must-not-inherit"}):
            result = harness.run()
        self.assertEqual(result["status"], "completed", result)
        harness.assert_clean(self, result)
        records = [json.loads(line) for line in harness.record.read_text().splitlines()]
        self.assertEqual([record["method"] for record in records], ["initialize", "initialized", "account/login/start", "account/read"])
        self.assertEqual(records[2]["params"], {"type": "chatgptAuthTokens", "accessToken": harness.token,
            "chatgptAccountId": "synthetic-workspace", "chatgptPlanType": None})
        self.assertEqual(records[3]["params"], {"refreshToken": False})
        self.assertTrue(all(record["environment"]["OPENAI_API_KEY"] is None and record["environment"]["HTTPS_PROXY"] is None for record in records))
        self.assertTrue(all(harness.token not in json.dumps(command) for command in harness.commands))
        self.assertEqual(result["account_read"]["auth_mode"], "chatgptAuthTokens")
        self.assertEqual(result["account_read"]["remote_token_validity"], "not_checked")

    def test_notifications_may_arrive_after_account_response(self):
        harness = self.harness("late_notifications")
        result = harness.run()
        self.assertEqual(result["status"], "completed", result)
        harness.assert_clean(self, result)

    def test_unexpected_notification_diagnostics_are_hashed_and_still_reject(self):
        for mode, known in (("config_warning", "configWarning"), ("namespace_warning", "configWarning"),
                            ("unknown_notification", None), ("mcp_notification", "mcpServer/startupStatus/updated")):
            with self.subTest(mode=mode):
                harness = self.harness(mode)
                result = harness.run()
                self.assertEqual(result["error"], "unexpected_notification")
                diagnostic = result["unexpected_notification"]
                self.assertEqual(diagnostic["known_method"], known)
                self.assertEqual(diagnostic["waiting_response_id"], 2)
                method = known or "unknown/" + harness.token
                self.assertEqual(diagnostic["method_sha256"], probe.sha(method))
                self.assertRegex(diagnostic["params_sha256"], r"^[0-9a-f]{64}$")
                if mode == "namespace_warning":
                    self.assertEqual(diagnostic["source_warning"], "bubblewrap_user_namespace_unavailable")
                if mode == "config_warning":
                    self.assertEqual(diagnostic["summary_sha256"], probe.sha(harness.token))
                    self.assertIsNone(diagnostic["source_warning"])
                self.assertNotIn("account/read", result["client_methods"])
                harness.assert_clean(self, result)

    def test_observed_exact_missing_bubblewrap_warning_allows_only_zero_turn_auth(self):
        for mode in ("missing_bwrap", "missing_bwrap_null_fields"):
            with self.subTest(mode=mode):
                harness = self.harness(mode)
                result = harness.run()
                self.assertEqual(result["status"], "completed_with_known_zero_turn_warning", result)
                self.assertEqual(result["account_read"]["auth_mode"], "chatgptAuthTokens")
                self.assertEqual(len(result["startup_warnings"]), 1)
                warning = result["startup_warnings"][0]
                self.assertEqual(warning["source_warning"], "missing_system_bubblewrap")
                self.assertEqual(warning["summary_sha256"], "0423500be79523998b914735593b9d7a2a3bde53601c0f60bd51ae9bac1a6ba1")
                self.assertEqual(warning["scope"], "zero_turn_local_auth_only")
                self.assertFalse(result["model_reference_clean"])
                self.assertTrue(result["zero_turn_only"])
                self.assertNotIn("unexpected_notification", result)
                harness.assert_clean(self, result)

    def test_missing_bubblewrap_warning_mutations_and_duplicates_still_reject(self):
        for mode in ("missing_bwrap_details", "missing_bwrap_path", "missing_bwrap_extra", "missing_bwrap_modified", "missing_bwrap_duplicate"):
            with self.subTest(mode=mode):
                harness = self.harness(mode)
                result = harness.run()
                self.assertEqual(result["status"], "rejected", result)
                self.assertEqual(result["error"], "unexpected_notification")
                harness.assert_clean(self, result)

    def test_complete_official_startup_login_sequence_and_passive_changes(self):
        for mode in ("startup_complete", "startup_transitions"):
            with self.subTest(mode=mode):
                harness = self.harness(mode)
                result = harness.run()
                self.assertEqual(result["status"], "completed_with_known_zero_turn_warning", result)
                self.assertEqual(result["client_methods"], ["initialize", "initialized", "account/login/start", "account/read"])
                self.assertEqual(result["skills_changed_notifications"], 1)
                self.assertEqual([row["status"] for row in result["remote_control_status"]],
                    ["disabled"] if mode == "startup_complete" else ["disabled", "connecting", "connected", "errored", "disabled"])
                self.assertEqual(result["remote_control_status"][0]["installation_id_sha256"], probe.sha("synthetic-private-installation"))
                self.assertEqual(result["account_read"]["remote_token_validity"], "not_checked")
                harness.assert_clean(self, result)

    def test_startup_passive_notifications_do_not_enable_tasks_or_bad_schemas(self):
        expected = {"startup_bad_remote": "invalid_remote_control_notification", "startup_extra_remote": "invalid_remote_control_notification",
            "startup_unknown_remote_status": "invalid_remote_control_notification", "startup_remote_flood": "invalid_remote_control_notification",
            "startup_remote_request": "server_request_rejected", "startup_bad_skills": "invalid_skills_notification",
            "startup_thread": "unexpected_notification", "startup_turn": "unexpected_notification", "startup_item": "unexpected_notification"}
        for mode, error in expected.items():
            with self.subTest(mode=mode):
                harness = self.harness(mode)
                result = harness.run()
                self.assertEqual(result["status"], "rejected", result)
                self.assertEqual(result["error"], error, result)
                self.assertEqual(result["model_calls"], 0)
                harness.assert_clean(self, result)

    def test_business_prolite_plan_is_preserved_as_official_schema_specifies(self):
        harness = self.harness("business_plan")
        harness.token = fake_token(**{probe.AUTH_CLAIM: {"chatgpt_account_id": "synthetic-workspace", "chatgpt_user_id": "synthetic-user",
            "chatgpt_plan_type": "self_serve_business_prolite"}})
        harness.credential["access_token"] = harness.token
        harness.pins["token_sha256"] = probe.sha(harness.token)
        result = harness.run()
        self.assertEqual(result["status"], "completed", result)
        self.assertEqual(result["account_read"]["plan_type"], "self_serve_business_prolite")
        harness.assert_clean(self, result)

    def test_all_pins_fail_before_child_launch(self):
        for key in ("cli_sha256", "token_sha256", "account_sha256", "user_sha256"):
            with self.subTest(key=key):
                harness = self.harness()
                harness.pins[key] = "0" * 64
                result = harness.run()
                self.assertEqual(result["status"], "rejected")
                self.assertEqual(harness.children, [])
                harness.assert_clean(self, result)

    def test_tokens_must_be_stdin_payload_and_no_id_or_refresh_token(self):
        for key in ("id_token", "refresh_token", "tokens", "plan_type"):
            harness = self.harness()
            harness.credential[key] = "private-extra-value"
            result = harness.run()
            self.assertEqual(result["error"], "invalid_credential_fields")
            self.assertEqual(harness.children, [])

    def test_expired_jwt_rejected_without_child(self):
        harness = self.harness()
        harness.credential["access_token"] = fake_token(exp=time.time() - 1)
        result = harness.run()
        self.assertEqual(result["error"], "access_token_expiration_insufficient")
        self.assertEqual(harness.children, [])

    def test_server_tool_and_refresh_requests_abort_without_execution(self):
        for mode in ("server_request", "refresh_request"):
            with self.subTest(mode=mode):
                harness = self.harness(mode)
                result = harness.run()
                self.assertEqual(result["error"], "server_request_rejected", result)
                self.assertEqual(result["server_requests_rejected"], 1)
                self.assertNotIn("account/read", result["client_methods"])
                harness.assert_clean(self, result)

    def test_sensitive_rpc_and_stderr_are_never_reported_raw(self):
        for mode in ("login_error", "echo_stderr"):
            harness = self.harness(mode)
            result = harness.run()
            self.assertEqual(result["status"], "rejected" if mode == "login_error" else "completed", result)
            harness.assert_clean(self, result)
            if mode == "login_error":
                self.assertEqual(result["error"], "rpc_error")
                self.assertRegex(result["rpc_error_sha256"], r"^[0-9a-f]{64}$")
            else:
                self.assertGreater(result["child_output"]["stderr"]["bytes"], 0)

    def test_mismatched_runtime_and_account_states_fail_closed(self):
        expected = {"wrong_version": "cli_version_mismatch", "wrong_home": "isolated_home_mismatch",
            "wrong_auth_mode": "auth_state_mismatch", "wrong_email": "account_email_mismatch", "wrong_plan": "account_read_mismatch",
            "bad_json": "duplicate_json_field", "persist_auth": "unexpected_persisted_auth"}
        for mode, error in expected.items():
            with self.subTest(mode=mode):
                harness = self.harness(mode)
                result = harness.run()
                self.assertEqual(result["error"], error, result)
                self.assertEqual(result["status"], "rejected")
                harness.assert_clean(self, result)

    def test_output_limits_abort_and_cleanup(self):
        for mode in ("stdout_flood", "stderr_flood"):
            with self.subTest(mode=mode):
                harness = self.harness(mode)
                result = harness.run()
                self.assertEqual(result["error"], "child_output_limit", result)
                harness.assert_clean(self, result)

    def test_absolute_deadline_kills_process_tree(self):
        harness = self.harness("spawn_stall")
        started = time.monotonic()
        result = harness.run(deadline=started + 2)
        self.assertEqual(result["error"], "deadline_exceeded", result)
        self.assertLess(time.monotonic() - started, 3)
        harness.assert_clean(self, result)
        descendant_pid = int(Path(str(harness.record) + ".descendant").read_text())
        if os.name == "nt":
            import ctypes
            kernel = ctypes.WinDLL("kernel32", use_last_error=True)
            kernel.OpenProcess.restype = ctypes.c_void_p
            handle = kernel.OpenProcess(0x1000, False, descendant_pid)
            if handle:
                code = ctypes.c_ulong()
                kernel.GetExitCodeProcess.argtypes = [ctypes.c_void_p, ctypes.c_void_p]
                kernel.GetExitCodeProcess(handle, ctypes.byref(code))
                kernel.CloseHandle.argtypes = [ctypes.c_void_p]
                kernel.CloseHandle(handle)
                self.assertNotEqual(code.value, 259, "descendant remained running")
        else:
            proc_stat = Path("/proc") / str(descendant_pid) / "stat"
            self.assertTrue(not proc_stat.exists() or proc_stat.read_text().split()[2] == "Z", "descendant remained running")

    def test_exception_and_interrupt_always_cleanup(self):
        for error in (RuntimeError("private-error-token"), KeyboardInterrupt()):
            harness = self.harness()
            with patch.object(probe, "auth_handshake", side_effect=error):
                result = harness.run()
            self.assertEqual(result["status"], "rejected")
            self.assertNotIn("private-error-token", json.dumps(result))
            harness.assert_clean(self, result)

    def test_unknown_arguments_never_echo_secrets_and_turn_unavailable(self):
        for argument in ("--run-turn", "--access-token=private-error-token"):
            output, error = io.StringIO(), io.StringIO()
            with redirect_stdout(output), redirect_stderr(error):
                code = probe.main([argument])
            self.assertEqual(code, 1)
            self.assertEqual(json.loads(output.getvalue())["error"], "invalid_arguments")
            self.assertNotIn("private-error-token", output.getvalue() + error.getvalue())

    def test_stdin_timeout_exits_without_buffered_reader_shutdown_deadlock(self):
        command = [sys.executable, str(Path(probe.__file__).resolve()), "--cli", str(Path(sys.executable).resolve()), "--timeout", "2"]
        for field in ("cli", "token", "account", "user"):
            command += ["--expected-" + field + "-sha256", "0" * 64]
        child = subprocess.Popen(command, stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
            creationflags=subprocess.CREATE_NO_WINDOW if os.name == "nt" else 0)
        try:
            child.wait(timeout=4)
            output, error = child.communicate(timeout=1)
        finally:
            if child.poll() is None:
                child.kill()
                child.communicate(timeout=1)
        self.assertEqual(json.loads(output)["error"], "stdin_deadline_exceeded")
        self.assertEqual(error, b"")

    def test_malformed_input_deadlines_and_duplicate_keys(self):
        harness = self.harness()
        for raw in (b'{"access_token":"one","access_token":"two"}', b"NaN", b"x" * (probe.MAX_INPUT + 1), b"[]"):
            result = harness.run(raw_credential=raw)
            self.assertEqual(result["status"], "rejected")
            self.assertEqual(harness.children, [])
        for deadline in (float("inf"), float("nan"), time.monotonic() - 1):
            result = harness.run(deadline=deadline)
            self.assertEqual(result["error"], "invalid_deadline")

    def test_launch_failure_removes_isolated_workspace(self):
        harness = self.harness()
        roots = []
        def fail(command, root, environment, deadline):
            roots.append(root)
            raise OSError("private-error-token")
        result = harness.run(child_factory=fail)
        self.assertEqual(result["error"], "probe_failed")
        self.assertTrue(result["cleanup_completed"])
        self.assertTrue(all(not root.exists() for root in roots))
        self.assertNotIn("private-error-token", json.dumps(result))

    def test_system_configuration_rejects_before_any_child(self):
        harness = self.harness()
        with patch.object(probe, "reject_system_config", side_effect=probe.ProbeFailure("system_codex_configuration_present")):
            result = harness.run()
        self.assertEqual(result["error"], "system_codex_configuration_present")
        self.assertEqual(harness.children, [])

    def test_version_cleanup_failure_cannot_be_hidden_by_later_success(self):
        harness = self.harness()
        def factory(command, root, environment, deadline):
            child = harness.child(command, root, environment, deadline)
            close = child.close
            def fail_close():
                close()
                child._fail("process_cleanup_timeout")
            child.close = fail_close
            return child
        result = harness.run(child_factory=factory)
        self.assertEqual(result["status"], "rejected")
        self.assertEqual(len(harness.children), 1)
        self.assertEqual(result["client_methods"], [])
        harness.assert_clean(self, result)

    def test_sigterm_handler_is_restored_after_argument_failure(self):
        import signal
        before = signal.getsignal(signal.SIGTERM)
        with redirect_stdout(io.StringIO()):
            probe.main(["--unsupported"])
        self.assertEqual(signal.getsignal(signal.SIGTERM), before)


if __name__ == "__main__":
    if len(sys.argv) > 1 and sys.argv[1] == "--mock-child":
        mock_child(*sys.argv[2:])
    else:
        unittest.main()
