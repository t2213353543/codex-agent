"""Synthetic-only error-layer regressions; imports never execute inventory.main."""
import importlib.util
from contextlib import redirect_stdout
import io
import json
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import Mock, patch


SPEC = importlib.util.spec_from_file_location("inventory_classifier_under_test", Path(__file__).with_name("inventory_remote.py"))
INVENTORY = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(INVENTORY)


class InventoryClassifierTests(unittest.TestCase):
    def test_http_status_alone_never_establishes_failure_origin(self):
        for status in (0, 200, 403, 407, 429, 502, 503):
            with self.subTest(status=status):
                value = INVENTORY.classify("", status)
                self.assertTrue(value.startswith("unclassified_"), value)
                self.assertNotIn("risk", value)

    def test_local_admission_has_known_gateway_error_text(self):
        for message in ("all accounts in the key's groups are busy", "session account is busy"):
            with self.subTest(message=message):
                self.assertEqual(INVENTORY.classify(message, 429), "local_admission_busy")
        self.assertEqual(INVENTORY.classify("no enabled accounts", 503), "local_no_enabled_account")
        self.assertEqual(INVENTORY.classify("outbound proxy configuration changed or is unavailable", 503), "local_route_unavailable")

    def test_http_200_stream_failure_is_not_treated_as_success(self):
        failure = json.dumps({"type": "response.failed", "response": {"status": "failed", "error": {"code": "rate_limit_exceeded", "message": "synthetic failure"}}})
        self.assertEqual(INVENTORY.classify(failure, 200), "upstream_rate_limit")
        self.assertNotEqual(INVENTORY.classify("rejected", 200), "ok")

    def test_explicit_provider_quota_and_rate_codes(self):
        for code, want in (("rate_limit_exceeded", "upstream_rate_limit"), ("usage_limit_reached", "upstream_quota"), ("insufficient_quota", "upstream_quota")):
            with self.subTest(code=code):
                payload = json.dumps({"error": {"code": code, "message": "synthetic"}})
                self.assertEqual(INVENTORY.classify(payload, 429), want)

    def test_explicit_proxy_403_407_429_are_not_provider_attribution(self):
        for status, message, expected in ((403, "HTTP proxy returned 403 Forbidden", "proxy_rejected"), (407, "HTTP proxy returned 407 Proxy Authentication Required", "proxy_auth_required"), (429, "HTTP proxy returned 429 Too Many Requests", "proxy_rate_limit")):
            with self.subTest(status=status):
                value = INVENTORY.classify(message, status)
                self.assertEqual(value, expected)

    def test_generic_server_error_is_not_proof_of_overload(self):
        payload = json.dumps({"error": {"code": "server_error", "message": "synthetic internal processing failure"}})
        self.assertNotEqual(INVENTORY.classify(payload, 503), "upstream_overload")

    def test_compatibility_error_does_not_become_authentication_failure(self):
        self.assertEqual(INVENTORY.classify("Unsupported parameter: authentication", 400), "compatibility")

    def test_explicit_cancellation_not_swallowed_by_transport_words(self):
        for message in ("context canceled", "upstream connection failed: context canceled"):
            with self.subTest(message=message):
                self.assertEqual(INVENTORY.classify(message, 502), "client_cancelled")

    def test_structured_code_has_priority_over_incidental_message(self):
        for message in ("retry after authentication flow", "unauthorized retry pattern encountered", "upstream connection failed"):
            with self.subTest(message=message):
                payload = json.dumps({"error": {"code": "rate_limit_exceeded", "message": message}})
                self.assertEqual(INVENTORY.classify(payload, 429), "upstream_rate_limit")

    def test_upstream_401_is_authentication_without_guessing_token_cause(self):
        self.assertEqual(INVENTORY.classify("expired", 401, source="upstream", phase="http_status"), "auth_failure")
        self.assertEqual(INVENTORY.classify("opaque", 401, source="upstream", phase="http_status"), "auth_failure")
        self.assertTrue(INVENTORY.classify("", 401).startswith("unclassified_"))

    def test_generic_proxy_error_envelope_does_not_mean_outbound_proxy(self):
        payload = json.dumps({"error": {"type": "proxy_error", "message": "HTTP SSE is disabled"}})
        value = INVENTORY.classify(payload, 403, source="gateway", phase="settings")
        self.assertFalse(value.startswith("proxy_"), "Generic gateway proxy_error envelope is not evidence of outbound-proxy rejection")

    def test_observed_upstream_event_and_http_phase_preserve_failure_layer(self):
        self.assertEqual(INVENTORY.classify("rejected", 200, source="upstream", phase="response_event"), "upstream_stream_error")
        self.assertEqual(INVENTORY.classify("unavailable", 503, source="upstream", phase="http_status"), "upstream_http_error")
        self.assertEqual(INVENTORY.classify("server_error", 502, source="upstream", phase="http_status"), "upstream_http_error")

    def test_explicit_gateway_proxy_phase_distinguishes_status_layer(self):
        for status, expected in ((403, "proxy_rejected"), (407, "proxy_auth_required"), (429, "proxy_rate_limit")):
            with self.subTest(status=status):
                self.assertEqual(INVENTORY.classify("synthetic rejection", status, source="gateway", phase="proxy_route"), expected)

    def test_unknown_metadata_and_bare_status_do_not_manufacture_origin(self):
        value = INVENTORY.classify("opaque", 503, source="unrecognized-synthetic", phase="unrecognized-phase")
        self.assertTrue(value.startswith("unclassified_"), value)
        self.assertNotIn("risk", value)

    def test_main_uses_record_outcome_not_http_200_for_success(self):
        # Exercise actual aggregation rather than claiming classify() sees status.
        rows = [
            {"created_at": "2026-09-18T00:00:01Z", "status": "ok", "code": "200", "error": "server_error", "latency_ms": 10, "has_proxy": False,
             "source": "", "phase": "", "error_source": "", "error_phase": ""},
            {"created_at": "2026-09-18T00:00:00Z", "status": "error", "code": "200", "error": "rejected", "latency_ms": 10, "has_proxy": False,
             "source": "upstream", "phase": "response_event", "error_source": "upstream", "error_phase": "response_event"},
        ]
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            proc = root / "proc" / "123"
            proc.mkdir(parents=True)
            (proc / "cmdline").write_bytes(b"/opt/codex-agent/synthetic\0--web\0/opt/codex-agent/synthetic-web\0")
            (proc / "environ").write_bytes(b"PG_MAX_CONNS=2\0")
            (proc / "exe").write_bytes(b"synthetic executable")
            def scoped_path(value):
                return root / "proc" if str(value) == "/proc" else Path(value)
            response = Mock(status=200)
            response.__enter__ = Mock(return_value=response)
            response.__exit__ = Mock(return_value=False)
            opener = Mock()
            opener.open.return_value = response
            commands = lambda args, **kwargs: "123" if "--value" in args else "MainPID=123\nActiveState=active\n"
            fake_sql = lambda query: rows if "FROM request_logs" in query else []
            output = io.StringIO()
            with patch.object(INVENTORY, "Path", side_effect=scoped_path), patch.object(INVENTORY, "command", side_effect=commands), \
                 patch.object(INVENTORY, "sql", side_effect=fake_sql), patch.object(INVENTORY, "snapshot", return_value={"at":0}), \
                 patch.object(INVENTORY.urllib.request, "build_opener", return_value=opener), patch.object(sys, "argv", ["inventory_remote.py"]), redirect_stdout(output):
                INVENTORY.main()
            report = json.loads(output.getvalue())
            classified = {group["status"]:group["category"] for group in report["logs"]["groups"]}
            self.assertEqual(classified["ok"], "ok")
            self.assertEqual(classified["error"], "upstream_stream_error")
            self.assertFalse(report["logs"]["raw_error_text_exported"])


if __name__ == "__main__":
    unittest.main()
