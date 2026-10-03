"""Offline usage gating and native IPC contract; real Rust GET is opt-in loopback."""
import base64
import copy
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import io
import json
import os
from pathlib import Path
import struct
import threading
import time
from types import SimpleNamespace
import unittest
from unittest import mock

import quality_usage_probe as probe

WORKSPACE = "private-workspace"
USER = "private-user"
HELPER_SHA = "a" * 64
APPLICATION_IDENTITY = {"user_agent": "synthetic-cli/0.155.0 (TestOS 1.0; testarch) test-terminal",
                        "originator": "synthetic-cli", "version": "0.155.0"}


def token(workspace=WORKSPACE, user=USER, expires=None):
    claims = {probe.AUTH_CLAIM: {"chatgpt_account_id": workspace}, "exp": expires or time.time() + 3600}
    if user is not None:
        claims[probe.AUTH_CLAIM]["chatgpt_user_id"] = user
    encoded = base64.urlsafe_b64encode(json.dumps(claims).encode()).decode().rstrip("=")
    return "header." + encoded + ".signature"


def payload():
    return {"plan_type": "self_serve_business_prolite", "account_id": WORKSPACE, "user_id": USER,
            "rate_limit": {"allowed": True, "limit_reached": False,
                           "primary_window": {"used_percent": 1, "reset_after_seconds": 600}},
            "spend_control": {"reached": False}, "rate_limit_reached_type": None}


def frame(kind, raw=b""):
    return struct.pack("!BI", kind, len(raw)) + raw


def wire(value=None, status=200):
    raw = json.dumps(value if value is not None else payload()).encode()
    return frame(1, json.dumps({"status": status, "version": "HTTP/2.0", "headers": []}).encode()) + frame(2, raw) + frame(3)


class FakeSocket:
    def __init__(self, response, chunk=7):
        self.incoming = io.BytesIO(response)
        self.sent = bytearray()
        self.chunk = chunk
        self.timeouts = []
        self.closed = False

    def settimeout(self, value):
        self.timeouts.append(value)

    def connect(self, path):
        self.path = path

    def send(self, value):
        size = min(len(value), self.chunk)
        self.sent.extend(value[:size])
        return size

    def recv(self, size):
        return self.incoming.read(min(size, self.chunk))

    def close(self):
        self.closed = True


class RetainedBuffer(io.BytesIO):
    def close(self):
        self.was_closed = True


class FakeChild:
    pid = 999

    def __init__(self, returncode=0, stuck=False):
        self.stdin = RetainedBuffer()
        self.stdout = io.BytesIO()
        self.returncode = returncode
        self.stuck = stuck
        self.killed = False

    def wait(self, timeout):
        if self.stuck and not self.killed:
            raise probe.subprocess.TimeoutExpired("synthetic", timeout)
        return self.returncode

    def kill(self):
        self.killed = True
        self.returncode = -9

    def poll(self):
        return self.returncode


class SummaryTests(unittest.TestCase):
    def summary(self, value=None, user=USER, model="gpt-5.6-sol"):
        return probe.summarize_usage(value if value is not None else payload(), WORKSPACE, user, model)

    def test_known_usage_allows_without_fabricating_missing_model_permission(self):
        out = self.summary()
        self.assertIs(out["eligibility"]["allowed"], True)
        self.assertEqual(out["expected_model_state"], "not_observed")
        self.assertEqual(out["identity"]["account_id"]["state"], "matched")
        self.assertEqual(out["identity"]["workspace_id"]["state"], "not_observed")

    def test_missing_null_and_wrong_type_core_do_not_become_zero_or_allowed(self):
        for rate in (None, {}, [], {"allowed": True}, {"allowed": None, "limit_reached": False},
                     {"allowed": 1, "limit_reached": False}, {"allowed": "true", "limit_reached": False}):
            with self.subTest(rate=rate):
                value = payload()
                value["rate_limit"] = rate
                self.assertIsNone(self.summary(value)["eligibility"]["allowed"])
        value = payload()
        value.pop("rate_limit")
        value["secondary_used_percent"] = 0
        self.assertIsNone(self.summary(value)["eligibility"]["allowed"])

    def test_explicit_limits_deny_even_with_other_available_signals(self):
        for change in ({"rate_limit": {"allowed": False, "limit_reached": False}},
                       {"rate_limit": {"allowed": True, "limit_reached": True}},
                       {"spend_control": {"reached": True}},
                       {"rate_limit_reached_type": {"type": "workspace_member_credits_depleted"}},
                       {"model_usage": {"gpt-5.6-sol": {"available": False, "credits_would_enable": True}}}):
            value = payload()
            value.update(change)
            self.assertIs(self.summary(value)["eligibility"]["allowed"], False)

    def test_other_model_never_supplies_requested_model_permission(self):
        value = payload()
        value["model_usage"] = {"gpt-6-astra": {"available": False, "credits_would_enable": True}}
        out = self.summary(value)
        self.assertIs(out["eligibility"]["allowed"], True)
        self.assertEqual(out["expected_model_state"], "not_observed")
        self.assertIs(self.summary(value, model="gpt-6-astra")["eligibility"]["allowed"], False)
        value["model_usage"]["gpt-5.6-sol"] = {"available": "future"}
        self.assertIsNone(self.summary(value)["eligibility"]["allowed"])

    def test_identity_mismatch_invalid_missing_and_uncomparable_are_distinct(self):
        for field in ("account_id", "workspace_id", "user_id", "chatgpt_user_id"):
            value = payload()
            value[field] = "foreign-private"
            self.assertIs(self.summary(value)["eligibility"]["allowed"], False)
            value[field] = []
            self.assertIsNone(self.summary(value)["eligibility"]["allowed"])
        value = payload()
        value.pop("account_id")
        value.pop("user_id")
        self.assertIs(self.summary(value)["eligibility"]["allowed"], True)
        self.assertIsNone(self.summary(user=None)["eligibility"]["allowed"])
        self.assertNotIn(WORKSPACE, json.dumps(self.summary()))
        self.assertNotIn(USER, json.dumps(self.summary()))

    def test_unknown_optional_shapes_do_not_raise_or_fabricate_availability(self):
        for key, malformed in (("spend_control", []), ("rate_limit_reached_type", {"type": []}),
                               ("model_usage", [])):
            value = payload()
            value[key] = malformed
            self.assertIsNone(self.summary(value)["eligibility"]["allowed"])
        value = payload()
        value["plan_type"] = {"secret": "private"}
        self.assertEqual(self.summary(value)["plan_type"], "unknown")

    def test_allowlist_exact_amounts_and_typed_counts_exclude_private_unknowns(self):
        value = payload()
        value.update({"email": "private-email", "private_token": "private-token",
            "credits": {"has_credits": False, "unlimited": False, "overage_limit_reached": False,
                        "balance": "9007199254740993", "private": "private-credit"},
            "rate_limit_reset_credits": {"available_count": 0, "applicable_available_count": 0},
            "spend_control": {"reached": False, "individual_limit": {"limit": "-1.5", "used": "1e+9", "remaining": "private-not-amount"}},
            "model_usage": {"private-key": {"available": False, "private": "private-text"}}})
        out = self.summary(value)
        self.assertEqual(out["credits"]["balance"], "9007199254740993")
        self.assertIs(out["credits"]["overage_limit_reached"], False)
        self.assertEqual(out["rate_limit_reset_credits"]["applicable_available_count"], 0)
        self.assertEqual(out["spend_control"]["individual_limit"]["used"], "1e+9")
        self.assertIsNone(out["spend_control"]["individual_limit"]["remaining"])
        for secret in ("private-email", "private-token", "private-credit", "private-key", "private-text", "private-not-amount"):
            self.assertNotIn(secret, json.dumps(out))

    def test_rfc3339_times_are_validated_and_do_not_make_permission_true(self):
        for valid in ("2030-01-02T03:04:05Z", "2030-01-02T03:04:05.123456789+08:00"):
            self.assertEqual(probe.rfc_time(valid), valid)
        for invalid in ("future", "2030-02-30T03:04:05Z", "2030-01-02T03:04:05+00:99", "2030-01-02", {}, None):
            self.assertIsNone(probe.rfc_time(invalid))
        value = payload()
        value["model_usage"] = {"gpt-5.6-sol": {"available": False, "available_at": "2000-01-02T03:04:05Z"}}
        self.assertIs(self.summary(value)["eligibility"]["allowed"], False)


class ProtocolTests(unittest.TestCase):
    def test_json_rejects_duplicate_nonfinite_invalid_utf8_and_unpaired_surrogate(self):
        for raw in (b'{"a":1,"a":2}', b'{"a":NaN}', b'{"a":1e9999}', b'{"a":"\xff"}',
                    b'{"a":"\\ud800"}', b'{"a":', b'[' * 70 + b']' * 70):
            with self.subTest(raw=raw), self.assertRaises(probe.UsageProbeError):
                probe.strict_json(raw)

    def test_get_headers_are_fixed_without_reserve_reset_or_body(self):
        head = probe.usage_head("private-token", WORKSPACE, "ipc", 1000, APPLICATION_IDENTITY)
        self.assertEqual((head["method"], head["path"], head["body_length"]), ("GET", probe.ENDPOINT, 0))
        names = {name.lower() for name, _ in head["headers"]}
        self.assertEqual(names, {"authorization", "chatgpt-account-id", "user-agent", "originator", "version", "accept"})
        self.assertNotIn("request_compression", head)
        self.assertNotIn("reserve", json.dumps(head).lower())
        self.assertNotIn("reset", head["path"])

    def test_fragmented_ipc_is_exact_and_truncated_or_invalid_order_is_rejected(self):
        original = wire()
        for chunk in (1, 7, 65536):
            response, body = probe.read_response(FakeSocket(original, chunk), time.monotonic() + 5)
            self.assertEqual(response["status"], 200)
            self.assertEqual(json.loads(body), payload())
        header = frame(1, b'{"status":200,"version":"HTTP/2.0","headers":[]}')
        for bad in (original[:-1], frame(2, b'{}'), frame(3), header + header,
                    header + frame(3, b'x'), frame(99), frame(4, b'private-token'),
                    struct.pack("!BI", 2, probe.MAX_FRAME + 1)):
            with self.subTest(length=len(bad)), self.assertRaises((probe.UsageProbeError, EOFError)):
                probe.read_response(FakeSocket(bad), time.monotonic() + 5)

    def test_bad_response_headers_and_body_limit(self):
        for h in ({"status": True, "version": "HTTP/2.0", "headers": []},
                  {"status": 200, "version": "private-version", "headers": []},
                  {"status": 200, "version": "HTTP/2.0", "headers": [["Set-Cookie", "bad base64"]]},
                  {"status": 200, "version": "HTTP/2.0", "headers": [["Bad\rName", ""]]}):
            with self.assertRaises(probe.UsageProbeError):
                probe.read_response(FakeSocket(frame(1, json.dumps(h).encode()) + frame(3)), time.monotonic() + 5)
        header = frame(1, b'{"status":200,"version":"HTTP/2.0","headers":[]}')
        oversized = header + frame(2, b'x' * probe.MAX_FRAME) * 17 + frame(3)
        with self.assertRaisesRegex(probe.UsageProbeError, 'response_limit'):
            probe.read_response(FakeSocket(oversized, 65536), time.monotonic() + 5)

    def test_deadline_recomputed_for_every_partial_send_and_receive(self):
        for operation in ("send", "recv"):
            stream = FakeSocket(b'xxxx', 1)
            with mock.patch.object(probe.time, "monotonic", side_effect=(100.1, 100.5, 101.1)):
                with self.assertRaises(TimeoutError):
                    if operation == "send":
                        probe.write_all(lambda raw: probe.timed_io(stream, 101, operation, raw), b'xxx')
                    else:
                        probe.read_exact(lambda count: probe.timed_io(stream, 101, operation, count), 3)
            self.assertEqual(len(stream.timeouts), 2)
            self.assertAlmostEqual(stream.timeouts[0], .9)
            self.assertAlmostEqual(stream.timeouts[1], .5)

    def test_credential_workspace_user_and_expiry_are_checked_without_refresh(self):
        self.assertEqual(probe.validate_identity(token(), WORKSPACE, 35), USER)
        for bad_token, workspace in ((token(workspace="other"), WORKSPACE),
                                     (token(expires=time.time() + 100), WORKSPACE),
                                     (token(), "bad\nworkspace"), ("not.a.jwt", WORKSPACE)):
            with self.assertRaises(probe.UsageProbeError):
                probe.validate_identity(bad_token, workspace, 35)


class NativeHarnessTests(unittest.TestCase):
    def call(self, response=None, child=None, ready_change=None, runtime_sha=HELPER_SHA):
        stream = FakeSocket(wire() if response is None else response)
        child = child or FakeChild()
        popen = mock.Mock(return_value=child)
        def ready(_stream, _timeout):
            encoded = child.stdin.getvalue()
            config = json.loads(encoded[4:])
            value = {"protocol": 1, "transport": "unix", "unix_socket": config["unix_socket"]}
            if ready_change:
                value.update(ready_change)
            return json.dumps(value).encode()
        isolated_os = SimpleNamespace(name="posix", chmod=os.chmod)
        with mock.patch.object(probe, "os", isolated_os), mock.patch.object(probe, "file_digest", side_effect=(HELPER_SHA, runtime_sha)), \
             mock.patch.object(probe.subprocess, "Popen", popen), mock.patch.object(probe, "_ready_line", side_effect=ready), \
             mock.patch.object(probe.socket, "AF_UNIX", 1, create=True), \
             mock.patch.object(probe.socket, "socket", return_value=stream):
            report = probe.native_usage(Path("synthetic-helper"), HELPER_SHA, token(), WORKSPACE,
                                        application_identity=APPLICATION_IDENTITY)
        return report, stream, child, popen

    def test_single_get_safe_report_clean_helper_and_no_ca_or_proxy_overrides(self):
        report, stream, child, popen = self.call()
        self.assertIs(report["eligibility"]["allowed"], True)
        self.assertTrue(report["protocol_success"])
        self.assertEqual(report["native_dispatch_count"], 1)
        self.assertTrue(report["helper_reaped"])
        self.assertFalse(report["helper_forced_kill"])
        self.assertTrue(report["socket_removed"])
        self.assertTrue(stream.closed)
        self.assertEqual(popen.call_args.kwargs["env"], {"PATH": "/usr/bin:/bin", "LANG": "C.UTF-8"})
        size = struct.unpack("!I", stream.sent[:4])[0]
        self.assertEqual(len(stream.sent), 4 + size)
        head = json.loads(stream.sent[4:])
        self.assertEqual(head["method"], "GET")
        self.assertEqual(head["body_length"], 0)
        headers = {name.lower(): base64.b64decode(value).decode() for name, value in head["headers"]}
        self.assertEqual(headers["user-agent"], APPLICATION_IDENTITY["user_agent"])
        self.assertEqual(headers["originator"], APPLICATION_IDENTITY["originator"])
        self.assertEqual(headers["version"], APPLICATION_IDENTITY["version"])
        self.assertNotIn(WORKSPACE, json.dumps(report))
        self.assertNotIn(USER, json.dumps(report))
        self.assertNotIn(token(), json.dumps(report))

    def test_http_errors_invalid_json_and_truncation_cannot_enable_dispatch(self):
        bad_json = frame(1, b'{"status":200,"version":"HTTP/2.0","headers":[]}') + frame(2, b'{"secret":"private"') + frame(3)
        for response in (wire(status=429), wire(status=302), bad_json, wire()[:-2]):
            report, *_ = self.call(response)
            self.assertIsNone(report["eligibility"]["allowed"])
            self.assertFalse(report["protocol_success"])
            self.assertTrue(report["helper_reaped"])
            self.assertEqual(report["attempts"], 1)
        report, *_ = self.call(wire(status=429))
        self.assertEqual(report["http_status"], 429)
        self.assertIn("body_sha256", report)

    def test_bad_runtime_or_readiness_never_dispatches(self):
        for change in ({"ready_change": {"protocol": 2}}, {"ready_change": {"protocol": True}}, {"runtime_sha": "b" * 64}):
            report, stream, child, _ = self.call(**change)
            self.assertEqual(report["attempts"], 0)
            self.assertEqual(bytes(stream.sent), b'')
            self.assertIsNone(report["eligibility"]["allowed"])
            self.assertTrue(report["helper_reaped"])

    def test_nonzero_exit_and_forced_cleanup_invalidates_otherwise_allowed_sample(self):
        for child in (FakeChild(returncode=1), FakeChild(stuck=True)):
            report, *_ = self.call(child=child)
            self.assertIsNone(report["eligibility"]["allowed"])
            self.assertEqual(report["eligibility"]["reason"], "helper_cleanup_failed")
            self.assertIs(report["usage"]["eligibility"]["allowed"], True)
            self.assertTrue(report["helper_reaped"])

    def test_invalid_origin_and_timeout_fail_before_process_creation(self):
        with mock.patch.object(probe.subprocess, "Popen") as popen:
            for kwargs in ({"upstream": "https://example.com"}, {"upstream": "http://chatgpt.com"},
                           {"upstream": "http://127.0.0.1:8/reset"}, {"timeout": 0}, {"timeout": 36}):
                report = probe.native_usage(Path("missing"), HELPER_SHA, token(), WORKSPACE,
                                            application_identity=APPLICATION_IDENTITY, **kwargs)
                self.assertIsNone(report["eligibility"]["allowed"])
            popen.assert_not_called()

    def test_missing_or_invalid_application_identity_never_starts_helper(self):
        with mock.patch.object(probe.subprocess, "Popen") as popen:
            for identity in (None, {}, {"user_agent": "fake"},
                    dict(APPLICATION_IDENTITY, user_agent="bad\nvalue"),
                    dict(APPLICATION_IDENTITY, originator=""),
                    dict(APPLICATION_IDENTITY, version="version\x00"),
                    dict(APPLICATION_IDENTITY, host="invented")):
                report = probe.native_usage(Path("missing"), HELPER_SHA, token(), WORKSPACE,
                                            application_identity=identity)
                self.assertIsNone(report["eligibility"]["allowed"])
                self.assertEqual(report["attempts"], 0)
                self.assertEqual(report["native_dispatch_count"], 0)
                self.assertIn(report["error_class"], ("application_identity_not_supplied", "application_identity_invalid"))
            popen.assert_not_called()


@unittest.skipUnless(os.name == "posix", "real Rust helper uses POSIX IPC")
class NativeLoopbackTests(unittest.TestCase):
    def test_real_native_get_no_reserve_or_reset_single_dispatch_and_cleanup(self):
        name = os.environ.get("CODEX_NATIVE_HTTP_TEST_BIN")
        if not name:
            self.skipTest("explicit real Rust helper required")
        received = []
        class Handler(BaseHTTPRequestHandler):
            def do_GET(self):
                received.append((self.command, self.path, dict(self.headers)))
                raw = json.dumps(payload()).encode()
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(raw)))
                self.end_headers()
                self.wfile.write(raw)
            def log_message(self, *_):
                pass
        server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        worker = threading.Thread(target=server.serve_forever, daemon=True)
        worker.start()
        try:
            helper = Path(name)
            report = probe.native_usage(helper, probe.file_digest(helper), token(), WORKSPACE,
                upstream="http://127.0.0.1:" + str(server.server_port), timeout=10,
                application_identity=APPLICATION_IDENTITY)
        finally:
            server.shutdown()
            server.server_close()
            worker.join(timeout=3)
        self.assertIs(report["eligibility"]["allowed"], True, report)
        self.assertEqual(len(received), 1)
        self.assertEqual(received[0][:2], ("GET", probe.ENDPOINT))
        headers = {key.lower(): value for key, value in received[0][2].items()}
        self.assertNotIn("x-openai-codex-luna-reserve", headers)
        self.assertEqual(headers["chatgpt-account-id"], WORKSPACE)
        self.assertTrue(report["helper_reaped"])
        self.assertEqual(report["helper_exit_code"], 0)
        self.assertFalse(report["helper_forced_kill"])
        self.assertTrue(report["socket_removed"])


if __name__ == "__main__":
    unittest.main()
