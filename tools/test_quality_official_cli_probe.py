"""Offline security/protocol boundaries for the server-side CLI reference."""
import base64
import contextlib
import io
import json
import os
from pathlib import Path
import sys
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch
try:
    import tomllib
except ModuleNotFoundError:
    tomllib = None

import quality_official_cli_probe as probe


NOW = 1800000000
ACCOUNT = "account-private-test"


def token(account=ACCOUNT, expiration=NOW + 3600, email="private@example.test",
          user="user-private-test", user_field="chatgpt_user_id", sub=None):
    claims = {"exp": expiration, "email": email,
              probe.AUTH_CLAIM: {"chatgpt_account_id": account, "chatgpt_plan_type": "plus"}}
    if user is not None:
        claims[probe.AUTH_CLAIM][user_field] = user
    if sub is not None:
        claims["sub"] = sub
    encode = lambda value: base64.urlsafe_b64encode(json.dumps(value).encode()).decode().rstrip("=")
    return encode({"alg": "RS256"}) + "." + encode(claims) + ".testSignatureOnly"


def process_result(events=None, **overrides):
    if events is None:
        events = [
            {"type": "thread.started", "thread_id": "private-thread"},
            {"type": "turn.started"},
            {"type": "item.completed", "item": {"type": "reasoning", "text": "private reasoning"}},
            {"type": "item.completed", "item": {"type": "agent_message", "text": "test-answer"}},
            {"type": "turn.completed", "usage": {"input_tokens": 10, "output_tokens": 4,
                                                   "reasoning_output_tokens": 2, "private": "secret"}},
        ]
    return {"returncode": 0, "timed_out": False, "tool_observed": False,
            "output_limit": False, "reader_failed": False, "elapsed_seconds": .1,
            "stderr": "private stderr", "stdout": "\n".join(json.dumps(e) for e in events), **overrides}


class CredentialTests(unittest.TestCase):
    def load(self, values):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "credential.json"
            path.write_text(json.dumps(values))
            os.chmod(path, 0o600)
            return probe.load_credential(path, 240, now=NOW)

    def test_unmodified_real_token_serves_local_id_claims(self):
        access = token()
        auth, meta, secrets = self.load({"access_token": access, "account_id": ACCOUNT})
        self.assertEqual(auth["tokens"]["access_token"], access)
        self.assertEqual(auth["tokens"]["id_token"], access)
        self.assertEqual(auth["tokens"]["refresh_token"], "")
        self.assertEqual(auth["auth_mode"], "chatgpt")
        self.assertEqual(meta["identity_source"], "access_token_local_claims")
        self.assertIn("private@example.test", secrets)

    def test_explicit_id_token_not_rewritten(self):
        identity = token(email="other@example.test")
        auth, meta, _ = self.load({"access_token": token(), "account_id": ACCOUNT, "id_token": identity})
        self.assertEqual(auth["tokens"]["id_token"], identity)
        self.assertEqual(meta["identity_source"], "provided_id_token")

    def test_explicit_identity_rejects_another_user_in_same_workspace(self):
        with self.assertRaisesRegex(probe.ProbeFailure, "identity_token_user_mismatch"):
            self.load({"access_token": token(user="user-a"), "account_id": ACCOUNT,
                       "id_token": token(user="user-b")})

    def test_explicit_identity_requires_comparable_user_claims(self):
        for access, identity in ((token(user=None), token()), (token(), token(user=None)),
                                 (token(user=None), token(user=None))):
            with self.subTest(access_missing=probe.jwt_claims(access)[probe.AUTH_CLAIM].get("chatgpt_user_id") is None), \
                    self.assertRaisesRegex(probe.ProbeFailure, "identity_token_user_not_comparable"):
                self.load({"access_token": access, "account_id": ACCOUNT, "id_token": identity})

    def test_explicit_identity_invalid_stable_user_cannot_match(self):
        for user in ("", " ", " user ", 1, True, [], {}, "user\n", "x" * 513):
            with self.subTest(user_type=type(user).__name__), \
                    self.assertRaisesRegex(probe.ProbeFailure, "identity_token_user_not_comparable"):
                self.load({"access_token": token(user=user), "account_id": ACCOUNT,
                           "id_token": token(user=user)})

    def test_explicit_identity_supports_official_user_id_fallback(self):
        identity = token(user="same-user", user_field="user_id", sub="different-id-subject")
        access = token(user="same-user", sub="different-at-subject")
        auth, _, _ = self.load({"access_token": access, "account_id": ACCOUNT, "id_token": identity})
        self.assertEqual(auth["tokens"]["id_token"], identity)

    def test_email_or_sub_cannot_substitute_for_missing_stable_user(self):
        access = token(user=None, sub="shared-subject")
        with self.assertRaisesRegex(probe.ProbeFailure, "identity_token_user_not_comparable"):
            self.load({"access_token": access, "account_id": ACCOUNT, "id_token": access})

    def test_access_token_only_path_does_not_require_an_id_token_user_claim(self):
        access = token(user=None)
        auth, meta, _ = self.load({"access_token": access, "account_id": ACCOUNT})
        self.assertEqual(auth["tokens"]["id_token"], access)
        self.assertEqual(meta["identity_source"], "access_token_local_claims")

    def test_refresh_tokens_and_auth_containers_rejected(self):
        for extra in ({"refresh_token": ""}, {"tokens": {}}):
            with self.subTest(extra=extra), self.assertRaisesRegex(probe.ProbeFailure, "must_not_contain"):
                self.load({"access_token": token(), "account_id": ACCOUNT, **extra})

    def test_near_expiry_rejected_even_when_not_expired(self):
        with self.assertRaisesRegex(probe.ProbeFailure, "refresh_window"):
            self.load({"access_token": token(expiration=NOW + 530), "account_id": ACCOUNT})

    def test_unknown_secret_fields_and_duplicate_fields_rejected(self):
        with self.assertRaisesRegex(probe.ProbeFailure, "unknown_fields"):
            self.load({"access_token": token(), "account_id": ACCOUNT, "session": {"refresh_token": "private"}})
        with self.assertRaisesRegex(probe.ProbeFailure, "duplicate_json_field"):
            json.loads('{"access_token":"first","access_token":"second"}', object_pairs_hook=probe.unique_json_object)

    @unittest.skipUnless(os.name == "posix", "POSIX ownership and no-follow credential boundary")
    def test_symlink_fifo_and_public_credentials_rejected(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            source = root / "credential.json"
            source.write_text(json.dumps({"access_token": token(), "account_id": ACCOUNT}))
            os.chmod(source, 0o600)
            link = root / "link"
            link.symlink_to(source)
            fifo = root / "fifo"
            os.mkfifo(fifo, 0o600)
            for path in (link, fifo):
                with self.assertRaises(probe.ProbeFailure):
                    probe.load_credential(path, 240, now=NOW)
            os.chmod(source, 0o644)
            with self.assertRaisesRegex(probe.ProbeFailure, "private_0600"):
                probe.load_credential(source, 240, now=NOW)

    def test_account_mismatch_and_claim_exp_mismatch_rejected(self):
        for values, error in [
            ({"access_token": token("different"), "account_id": ACCOUNT}, "account_mismatch"),
            ({"access_token": token(), "account_id": ACCOUNT, "exp": NOW + 4000}, "expiration_mismatch"),
            ({"access_token": token(), "account_id": ACCOUNT, "id_token": token("different")}, "account_mismatch"),
        ]:
            with self.subTest(error=error), self.assertRaisesRegex(probe.ProbeFailure, error):
                self.load(values)

    def test_invalid_jwt_does_not_echo_token(self):
        for value in ("super-secret-credential", "a.@@.b", "a.b.c.d", None):
            with self.subTest(value=value), self.assertRaises(probe.ProbeFailure) as error:
                probe.jwt_claims(value)
            self.assertNotIn("super-secret", str(error.exception))


class ConfigTests(unittest.TestCase):
    def test_env_excludes_proxy_auth_provider_and_process_context(self):
        with patch.dict(os.environ, {"PATH": "/usr/bin", "OPENAI_API_KEY": "private",
                                    "HTTPS_PROXY": "http://private", "HOME": "/private-user",
                                    "CODEX_HOME": "/private-codex", "RUST_LOG": "trace",
                                    "CODEX_API_ENDPOINT": "http://private"}, clear=True):
            env = probe.isolated_environment(Path("/isolated"))
        self.assertNotIn("OPENAI_API_KEY", env)
        self.assertNotIn("HTTPS_PROXY", env)
        self.assertNotIn("HOME", env)
        self.assertNotIn("CODEX_API_ENDPOINT", env)
        self.assertEqual(env["CODEX_HOME"], str(Path("/isolated")))
        self.assertEqual(env["RUST_LOG"], "off")
        self.assertEqual(env["CODEX_REFRESH_TOKEN_URL_OVERRIDE"], probe.REFRESH_DISABLED)

    def test_default_config_preserves_official_provider(self):
        text = probe.config_text(SimpleNamespace(model="gpt-5.6-sol", effort="xhigh", websocket="default"))
        self.assertIn('model_provider = "openai"', text)
        self.assertNotIn("[model_providers.", text)
        self.assertIn('approval_policy = "never"', text)
        self.assertNotIn("21", text)
        if tomllib is None:
            return
        data = tomllib.loads(text)
        self.assertEqual(data["model_provider"], "openai")
        self.assertNotIn("model_providers", data)
        self.assertEqual(data["sandbox_mode"], "read-only")
        self.assertEqual(data["approval_policy"], "never")
        self.assertTrue(data["suppress_unstable_features_warning"])
        self.assertNotIn("code_mode_host", data["features"])
        self.assertFalse(data["features"]["shell_tool"])
        self.assertNotIn("responses_websockets", data["features"])
        self.assertNotIn("21", data["developer_instructions"])

    def test_http_config_uses_separate_provider(self):
        text = probe.config_text(SimpleNamespace(model="gpt-5.6-sol", effort="medium", websocket="false"))
        self.assertIn('model_provider = "official_http_reference"', text)
        self.assertIn('supports_websockets = false', text)
        self.assertIn('requires_openai_auth = true', text)
        if tomllib is None:
            return
        data = tomllib.loads(text)
        provider = data["model_providers"][data["model_provider"]]
        self.assertFalse(provider["supports_websockets"])
        self.assertTrue(provider["requires_openai_auth"])
        self.assertEqual(provider["base_url"], "https://chatgpt.com/backend-api/codex")
        self.assertEqual(provider["http_headers"]["version"], "0.155.0")
        self.assertEqual(provider["request_max_retries"], 0)
        self.assertEqual(provider["stream_max_retries"], 0)


class EventTests(unittest.TestCase):
    def test_fail_fast_only_uses_error_events_not_answers(self):
        warning = {"type": "item.completed", "item": {"type": "error", "message": "config warning"}}
        self.assertFalse(probe.disqualifying_event(warning, "startup"))
        self.assertTrue(probe.disqualifying_event(warning, "turn"))
        for value in ("28", "29", "rate limit", "model rerouted: a -> b (Test)"):
            answer = {"type": "item.completed", "item": {"type": "agent_message", "text": value}}
            self.assertFalse(probe.disqualifying_event(answer, "turn"))
        self.assertTrue(probe.disqualifying_event({"type": "error", "message": "Reconnecting... 2/5 (403 Forbidden)"}, "turn"))

    def test_reroute_in_stderr_cannot_be_clean_and_jsonl_reroute_is_structured(self):
        report, _ = probe.summarize(process_result(stderr="model rerouted: model-a -> model-b (TestReason)"), [])
        self.assertEqual(report["status"], "invalid_reference")
        self.assertTrue(report["model_reroute_observed"])
        detail = probe.safe_error_details("model rerouted: model-a -> model-b (TestReason)", [])
        self.assertEqual(detail["model_reroute"], {"from_model": "model-a", "to_model": "model-b", "reason": "TestReason"})

    def test_forbidden_and_rate_limit_are_distinct(self):
        detail = probe.safe_error_details("Reconnecting... 2/5 (unexpected status 403 Forbidden)", [])
        self.assertEqual(detail["classification"], "forbidden")
        self.assertFalse(probe.diagnostic_indicators("unexpected status 403 Forbidden")["rate_limit"])
        self.assertTrue(probe.diagnostic_indicators("Falling back from WebSockets to HTTPS transport")["transport"])
    def test_report_retains_answer_usage_without_context_or_raw_errors(self):
        report, events = probe.summarize(process_result(), [])
        self.assertEqual(report["status"], "completed")
        self.assertEqual(report["final_output"], "test-answer")
        self.assertEqual(report["usage"]["reasoning_output_tokens"], 2)
        serialized = json.dumps([report, events])
        for secret in ("private reasoning", "private-thread", "private stderr", "secret"):
            self.assertNotIn(secret, serialized)

    def test_any_tool_or_unknown_item_invalidates_reference(self):
        for item_type in probe.TOOL_ITEMS | {"new_future_tool"}:
            with self.subTest(item_type=item_type):
                result = process_result()
                result["stdout"] += "\n" + json.dumps({"type": "item.started", "item": {"type": item_type, "arguments": "private"}})
                report, events = probe.summarize(result, [])
                self.assertEqual(report["status"], "invalid_reference")
                self.assertTrue(report["tool_observed"])
                self.assertNotIn("private", json.dumps(events))

    def test_parse_errors_limits_timeouts_and_stream_errors_cannot_pass(self):
        for extra in ({"stdout": "[]"}, {"stdout": "secret invalid json"}, {"timed_out": True},
                      {"output_limit": True}, {"reader_failed": True}, {"returncode": 1}):
            with self.subTest(extra=extra):
                report, _ = probe.summarize(process_result(**extra), [])
                self.assertEqual(report["status"], "invalid_reference")
        result = process_result()
        result["stdout"] += '\n{"type":"error","message":"429 private request"}'
        report, events = probe.summarize(result, ["private request"])
        self.assertEqual(report["status"], "invalid_reference")
        self.assertTrue(report["diagnostic_indicators"]["rate_limit"])
        self.assertNotIn("private request", json.dumps(events))

    def test_startup_warning_is_not_turn_failure(self):
        events = [
            {"type": "thread.started", "thread_id": "private"},
            {"type": "item.completed", "item": {"type": "error", "message": "experimental feature configuration warning"}},
            {"type": "item.completed", "item": {"type": "error", "message": "model catalog request failed: 429"}},
            {"type": "turn.started"},
            {"type": "item.completed", "item": {"type": "agent_message", "text": "generated answer"}},
            {"type": "turn.completed", "usage": {"output_tokens": 4}},
        ]
        report, safe = probe.summarize(process_result(events), [])
        self.assertEqual(report["status"], "completed_with_startup_warnings")
        self.assertFalse(report["reference_clean"])
        self.assertTrue(report["generated_answer_available"])
        self.assertTrue(report["turn_completed"])
        self.assertEqual(report["startup_warning_events"], 2)
        self.assertEqual(report["critical_error_events"], 0)
        self.assertEqual(safe[1]["phase"], "startup")
        self.assertEqual(safe[3]["phase"], "turn")
        self.assertTrue(report["diagnostic_indicators_by_phase"]["startup"]["rate_limit"])
        self.assertFalse(report["diagnostic_indicators_by_phase"]["turn"]["rate_limit"])
        # Exactly the same warning emitted during the turn stays conservative.
        events[1], events[3] = events[3], events[1]
        report, _ = probe.summarize(process_result(events), [])
        self.assertEqual(report["status"], "invalid_reference")

    def test_startup_top_level_errors_and_model_reroutes_still_invalid(self):
        for error in (
            {"type": "error", "message": "429"},
            {"type": "turn.failed", "error": {"message": "failure"}},
            {"type": "item.completed", "item": {"type": "error", "message": "model rerouted: a -> b (reason)"}},
        ):
            events = [error] + [json.loads(line) for line in process_result()["stdout"].splitlines()]
            report, _ = probe.summarize(process_result(events), [])
            self.assertEqual(report["status"], "invalid_reference")
            self.assertGreater(report["critical_error_events"], 0)
            self.assertTrue(report["generated_answer_available"])

    def test_short_diagnostics_redact_credentials_emails_paths_and_context(self):
        message = ("config warning private@example.test /private/auth.json C:\\private\\auth.json "
                   "Bearer credential-value https://example.test?token=private " + token() +
                   " payload={\"system\":\"must never retain this\"}")
        details = probe.safe_error_details(message, [])
        self.assertEqual(details["classification"], "configuration")
        self.assertLessEqual(len(details["message_excerpt"]), 240)
        for private in ("private@example.test", "/private", "C:\\private", "credential-value", "example.test", token(), "must never retain"):
            self.assertNotIn(private, details["message_excerpt"])
        details = probe.safe_error_details("warning\nprivate context", [])
        self.assertNotIn("private context", details["message_excerpt"])

    def test_answer_number_cannot_create_rate_limit_diagnostic(self):
        result = process_result()
        result["stdout"] = result["stdout"].replace("test-answer", "429")
        report, _ = probe.summarize(result, [])
        self.assertFalse(report["diagnostic_indicators"]["rate_limit"])

    def test_usage_429_is_not_rate_limit_and_status_has_numeric_boundary(self):
        result = process_result()
        events = [json.loads(line) for line in result["stdout"].splitlines()]
        events[-1]["usage"]["reasoning_output_tokens"] = 429
        report, _ = probe.summarize(process_result(events), [])
        self.assertEqual(report["usage"]["reasoning_output_tokens"], 429)
        self.assertFalse(report["diagnostic_indicators"]["rate_limit"])
        self.assertFalse(probe.diagnostic_indicators("HTTP 4290")["rate_limit"])
        self.assertTrue(probe.diagnostic_indicators("HTTP status 429")["rate_limit"])

    def test_prior_safe_replay_marks_lost_warning_details_unknown(self):
        result = process_result()
        report, safe = probe.summarize(result, [])
        prior_events = [{key: value for key, value in event.items() if key != "phase"} for event in safe]
        prior_events.insert(1, {"type": "item.completed", "item_type": "error"})
        report["usage"]["reasoning_output_tokens"] = 429
        prior_events[-1]["usage"]["reasoning_output_tokens"] = 429
        report["status"] = "invalid_reference"
        report["schema_version"] = 1
        report["diagnostic_indicators"]["rate_limit"] = True
        summary = probe.summarize_prior_safe_evidence(report, prior_events)
        self.assertEqual(summary["status"], "completed_with_unresolved_startup_events")
        self.assertFalse(summary["new_live_request"])
        self.assertFalse(summary["startup_message_text_available"])
        self.assertFalse(summary["model_reroute_excluded"])
        self.assertTrue(summary["legacy_usage_429_collision"])
        self.assertEqual(summary["original_error_or_stderr_rate_limit_evidence"], "unknown_not_retained")
        self.assertNotIn("diagnostic_indicators", summary)
        self.assertEqual(summary["final_output"], report["final_output"])
        # Reclassification must not mutate original evidence.
        self.assertEqual(report["status"], "invalid_reference")

    def test_prior_safe_replay_rejects_mismatched_report(self):
        report, safe = probe.summarize(process_result(), [])
        report["final_output"] = "different answer"
        with self.assertRaisesRegex(probe.ProbeFailure, "does_not_match"):
            probe.summarize_prior_safe_evidence(report, safe)

    def test_echoed_secret_is_redacted_from_answer(self):
        access = token()
        result = process_result([{"type": "item.completed", "item": {"type": "agent_message", "text": access + " " + ACCOUNT}}])
        report, events = probe.summarize(result, [access, ACCOUNT])
        self.assertNotIn(access, json.dumps([report, events]))
        self.assertNotIn(ACCOUNT, json.dumps([report, events]))


class ExecuteTests(unittest.TestCase):
    @unittest.skipUnless(os.name == "posix", "POSIX process-group termination")
    def test_process_stops_on_visible_retry_before_next_attempt(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            script = (
                "import json,time,pathlib\n"
                "print(json.dumps({'type':'turn.started'}),flush=True)\n"
                "print(json.dumps({'type':'error','message':'Reconnecting... 2/5 (403 Forbidden)'}),flush=True)\n"
                "time.sleep(2)\n"
                "pathlib.Path('unexpected-next-attempt').write_text('bad')\n"
            )
            result = probe.run_process([sys.executable, "-c", script], None, root, dict(os.environ), 5)
            self.assertTrue(result["stopped_on_reference_error"])
            self.assertLess(result["elapsed_seconds"], 1.5)
            self.assertFalse((root / "unexpected-next-attempt").exists())
            report, _ = probe.summarize(result, [])
            self.assertEqual(report["status"], "invalid_reference")
            self.assertTrue(report["diagnostic_indicators"]["forbidden"])

    @unittest.skipUnless(os.name == "posix", "POSIX private output permission boundary")
    def test_public_or_symlink_output_and_dangling_event_path_fail_preflight(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            public = root / "public"
            public.mkdir(mode=0o755)
            os.chmod(public, 0o755)
            with self.assertRaisesRegex(probe.ProbeFailure, "private_0700"):
                probe.prepare_output_directory(public)
            link = root / "linked"
            link.symlink_to(public, target_is_directory=True)
            with self.assertRaisesRegex(probe.ProbeFailure, "must_not_be_symlink"):
                probe.prepare_output_directory(link)
            private = root / "private"
            private.mkdir(mode=0o700)
            (private / "events.safe.jsonl").symlink_to(root / "missing")
            with self.assertRaisesRegex(probe.ProbeFailure, "output_already_exists"):
                probe.prepare_output_directory(private)

    def test_bundle_requires_manifest_and_host_and_prefers_resources(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "bin").mkdir()
            cli = root / "bin" / "codex"
            cli.write_bytes(b"fake-not-executed")
            os.chmod(cli, 0o755)
            (root / "codex-package.json").write_text(json.dumps({"version": probe.VERSION, "layoutVersion": 1, "entrypoint": "bin/codex"}))
            with self.assertRaisesRegex(probe.ProbeFailure, "host_missing"):
                probe.validate_cli_bundle(cli)
            legacy = root / "bin" / "codex-code-mode-host"
            legacy.write_bytes(b"legacy")
            os.chmod(legacy, 0o755)
            self.assertEqual(probe.validate_cli_bundle(cli)["code_mode_host_location"], "bin/codex-code-mode-host")
            (root / "codex-resources").mkdir()
            resource = root / "codex-resources" / "codex-code-mode-host"
            resource.write_bytes(b"resource")
            os.chmod(resource, 0o755)
            bundle = probe.validate_cli_bundle(cli)
            self.assertEqual(bundle["code_mode_host_location"], "codex-resources/codex-code-mode-host")
            self.assertEqual(bundle["code_mode_host_sha256"], probe.hashlib.sha256(b"resource").hexdigest())

    def test_catalog_observation_never_persists_identity_or_model_prompt(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            self.assertIsNone(probe.catalog_observation(root, "test-model")["requested_model_present"])
            (root / "models_cache.json").write_text(json.dumps({"client_version": probe.VERSION, "identity": "private-identity", "models": [
                {"slug": "test-model", "tool_mode": "code_mode_only", "base_instructions": "private-prompt"}]}))
            observed = probe.catalog_observation(root, "test-model")
            self.assertTrue(observed["requested_model_present"])
            self.assertTrue(observed["client_version_matches"])
            self.assertEqual(observed["requested_model_tool_mode"], "code_mode_only")
            self.assertNotIn("private", json.dumps(observed))

    def test_digest_works_without_python_311_file_digest(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "cli"
            payload = b"native-cli-payload" * 100000
            path.write_bytes(payload)
            with patch.object(probe.hashlib, "file_digest", side_effect=AssertionError("Python 3.9 has no file_digest"), create=True):
                self.assertEqual(probe.executable_sha256(path), probe.hashlib.sha256(payload).hexdigest())

    def test_mock_process_uses_stdin_and_deletes_home_before_publish(self):
        # Works on Windows without launching any CLI or reading live credentials.
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            cli = root / "codex"
            cli.write_bytes(b"test fake executable, never run")
            prompt = root / "prompt.txt"
            prompt.write_text("unaltered math prompt", encoding="utf-8")
            args = SimpleNamespace(timeout=240, cli=cli, model="gpt-5.6-sol", effort="medium",
                                   prompt_file=prompt, credential_file=root / "credentials.json",
                                   output_dir=root / "out", websocket="default")
            homes = []

            def run(command, stdin, cwd, env, timeout):
                homes.append(env["CODEX_HOME"])
                self.assertNotIn("private-access", json.dumps(command))
                self.assertNotIn("private-access", json.dumps(env))
                if command[-1] == "--version":
                    return process_result(stdout="codex-cli 0.155.0\n")
                self.assertEqual(stdin, "unaltered math prompt")
                self.assertIn("--ephemeral", command)
                self.assertIn("--strict-config", command)
                return process_result()

            auth = {"tokens": {"access_token": "private-access"}}
            # os.name is patched only after Path arguments were constructed.
            # Path() inside execute resolves platform dynamically, so pin it.
            real_path = type(root)
            with patch.object(probe.os, "name", "posix"), patch.object(probe, "Path", real_path), \
                    patch.object(probe, "validate_cli_bundle", return_value={"code_mode_host_present": True}), \
                    patch.object(probe, "prepare_output_directory", side_effect=lambda path: path.mkdir()), \
                    patch.object(probe, "load_credential", return_value=(auth, {}, ["private-access"])), \
                    patch.object(probe, "run_process", side_effect=run):
                report = probe.execute(args)
            self.assertEqual(report["status"], "completed")
            self.assertTrue(all(not real_path(home).exists() for home in homes))
            output = (args.output_dir / "report.json").read_text(encoding="utf-8")
            self.assertNotIn("private-access", output)
            self.assertFalse(report["prompt_modified"])
            self.assertIsNone(report["transport_observed"])

    def test_argument_error_does_not_echo_secret(self):
        output = io.StringIO()
        with contextlib.redirect_stdout(output):
            result = probe.main(["--secret-unrecognized-credential"])
        self.assertEqual(result, 2)
        self.assertNotIn("secret-unrecognized", output.getvalue())


if __name__ == "__main__":
    unittest.main()
