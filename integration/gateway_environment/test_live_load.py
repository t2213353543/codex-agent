"""Loopback-only regression tests; never loads environment credentials."""
from contextlib import contextmanager
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
from pathlib import Path
import socket
import tempfile
import threading
import time
import unittest

import live_load as probe


class Server(ThreadingHTTPServer):
    request_queue_size = 256
    daemon_threads = True

    def handle_error(self, request, client_address):
        pass  # Expected disconnect in timeout/reset/terminal-early-close cases.


@contextmanager
def upstream(mode="ok"):
    state = {"calls": 0, "active": 0, "peak": 0, "credential_errors": 0}
    lock = threading.Lock()

    class Handler(BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.1"

        def log_message(self, *_):
            pass

        def do_POST(self):
            self.rfile.read(int(self.headers.get("Content-Length", "0")))
            with lock:
                state["calls"] += 1
                call = state["calls"]
                state["active"] += 1
                state["peak"] = max(state["peak"], state["active"])
                if self.headers.get("Authorization") != "Bearer synthetic-only-secret":
                    state["credential_errors"] += 1
            try:
                if mode == "timeout":
                    time.sleep(.3)
                if mode == "reset":
                    self.connection.shutdown(socket.SHUT_RDWR)
                    self.connection.close()
                    return
                if mode in ("local_429", "upstream_502", "unknown_503") and call > 1:
                    status = {"local_429": 429, "upstream_502": 502, "unknown_503": 503}[mode]
                    data = ({"error": {"type": "proxy_error", "message": "all accounts in the key's groups are busy"}}
                            if mode == "local_429" else {"error": {"type": "server_error", "message": "secret-account-not-recorded"}})
                    body = json.dumps(data).encode() if mode != "unknown_503" else b"temporary upstream failure"
                    self.send_response(status)
                    self.send_header("Content-Length", str(len(body)))
                    if mode == "local_429":
                        self.send_header("Retry-After", "1")
                    self.end_headers()
                    self.wfile.write(body)
                    return
                self.send_response(200)
                self.send_header("X-Request-Id", f"req_synthetic_{call:04}")
                self.send_header("Content-Type", "text/event-stream")
                self.send_header("Connection", "close")
                self.end_headers()
                self.wfile.write(b'data: {"type":"response.created","response":{"id":"do-not-store"}}\r\n\r\n')
                self.wfile.flush()
                time.sleep(.015)
                if mode == "missing_terminal":
                    self.wfile.write(b'data: {"type":"response.completed","response":{"status":"completed"}}')
                    return
                if mode == "failed":
                    self.wfile.write(b'data: {"type":"response.failed","response":{"status":"failed","error":{"message":"private-output-not-recorded"}}}\n\n')
                    return
                self.wfile.write(b'data: {"type":"response.output_text.delta","delta":"private-output-not-recorded"}\n\n')
                self.wfile.write(b'data: {"type":"response.completed","response":{"id":"resp_synthetic_001","status":"completed","usage":{"input_tokens":10,"output_tokens":1,"total_tokens":11,"input_tokens_details":{"cached_tokens":5}}}}\n\n')
            finally:
                with lock:
                    state["active"] -= 1

    server = Server(("127.0.0.1", 0), Handler)
    worker = threading.Thread(target=server.serve_forever, daemon=True)
    worker.start()
    try:
        yield "http://127.0.0.1:" + str(server.server_port), state
    finally:
        server.shutdown()
        server.server_close()
        worker.join(timeout=2)


def config(base, **changes):
    values = {"base_url": base, "model": "synthetic", "levels": [2, 3], "waves": 1,
              "timeout_seconds": 5, "cooldown_seconds": 0,
              "max_cooldown_seconds": 2, "stop_on_429": True}
    values.update(changes)
    return values


class LiveLoadTests(unittest.TestCase):
    def test_error_code_allowlist_and_safe_correlation(self):
        for code in ("token_revoked", "invalid_token", "rate_limit_exceeded", "usage_limit", "insufficient_quota", "overloaded", "server_error"):
            self.assertEqual(probe.safe_error_code({"error": {"code": code, "message": "private-output"}}), code)
            self.assertEqual(probe.safe_error_code({"response": {"error": {"code": code}}}), code)
        self.assertEqual(probe.safe_error_code({"error": {"code": "private@example.invalid"}}), "other")
        self.assertEqual(probe.safe_correlation_id("req_synthetic123", "request"), "req_synthetic123")
        self.assertEqual(probe.safe_correlation_id("resp_synthetic123", "response"), "resp_synthetic123")
        self.assertIsNone(probe.safe_correlation_id("private@example.invalid", "request"))
        self.assertIsNone(probe.safe_correlation_id("Bearer synthetic-secret", "response"))
        self.assertIsNone(probe.safe_correlation_id("resp_" + "x" * 128, "response"))

    def test_sse_chunk_boundaries_multiline_and_no_implicit_eof(self):
        parser = probe.SSEParser()
        self.assertEqual(parser.feed(b':comment\r\ndata: {"type":\r'), [])
        self.assertEqual(parser.feed(b'\ndata: "response.created"}\r\n\r'), [])
        self.assertEqual(parser.feed(b'\n'), [{"type": "response.created"}])
        self.assertEqual(parser.feed(b'data: {"type":"response.completed"}'), [])

    def test_all_four_levels_two_waves_synthetic(self):
        with upstream() as (base, state), tempfile.TemporaryDirectory() as temporary:
            report = probe.run_load(config(base, levels=[30, 50, 70, 100], waves=2), "synthetic-only-secret", Path(temporary) / "run")
            self.assertEqual(report["status"], "completed")
            self.assertEqual(report["summary"]["completed"], 500)
            self.assertEqual(state["calls"], 501)
            self.assertEqual(state["credential_errors"], 0)
            self.assertEqual(report["summary"]["observed_usage"]["input_tokens"], 5000)
            self.assertEqual(report["summary"]["ttft_samples"], 500)
            self.assertEqual(report["sanity"]["observed_request_id"], "req_synthetic_0001")
            self.assertEqual(report["sanity"]["observed_response_id"], "resp_synthetic_001")
            self.assertIsNone(report["summary"]["upstream_peak_inflight"])
            self.assertEqual([wave["level"] for wave in report["waves"]], [30,30,50,50,70,70,100,100])
            self.assertTrue(all(0 < wave["client_peak_inflight"] <= wave["level"] for wave in report["waves"]))
            serialized = json.dumps(report)
            for secret in ("synthetic-only-secret", "private-output-not-recorded", "do-not-store", base):
                self.assertNotIn(secret, serialized)
            self.assertEqual(report["auto_retries"], 0)

    def test_failed_sse_is_not_successful_http_200(self):
        with upstream("failed") as (base, _), tempfile.TemporaryDirectory() as temporary:
            report = probe.run_load(config(base), "synthetic-only-secret", Path(temporary) / "run")
            self.assertEqual(report["status"], "stopped_sanity_failed")
            self.assertEqual(report["sanity"]["http_status"], 200)
            self.assertEqual(report["sanity"]["outcome"], "terminal_failure")
            self.assertEqual(report["waves"], [])

    def test_429_cools_down_and_stops_without_next_level(self):
        with upstream("local_429") as (base, state), tempfile.TemporaryDirectory() as temporary:
            started = time.monotonic()
            report = probe.run_load(config(base), "synthetic-only-secret", Path(temporary) / "run")
            self.assertEqual(report["status"], "stopped_rate_limited")
            self.assertEqual(state["calls"], 3)
            self.assertEqual(len(report["waves"]), 1)
            self.assertEqual(report["summary"]["observed_gateway_local_rejections"], 2)
            self.assertGreaterEqual(time.monotonic() - started, 1)
            self.assertEqual(report["cooldowns"][0]["seconds"], 1)

    def test_retry_after_budget_stops_instead_of_shortening_server_delay(self):
        with upstream("local_429") as (base, state), tempfile.TemporaryDirectory() as temporary:
            report = probe.run_load(config(base, max_cooldown_seconds=.5), "synthetic-only-secret", Path(temporary) / "run")
            self.assertEqual(report["status"], "stopped_retry_after_exceeds_budget")
            self.assertEqual(state["calls"], 3)
            self.assertEqual(report["next_allowed_after_seconds"], 1)
            self.assertEqual(report["cooldowns"], [])

    def test_origins_are_inference_or_unknown_without_proof(self):
        for mode, origin in (("upstream_502", "upstream_payload_inferred"), ("unknown_503", "unknown")):
            with self.subTest(mode=mode), upstream(mode) as (base, state), tempfile.TemporaryDirectory() as temporary:
                report = probe.run_load(config(base, levels=[2]), "synthetic-only-secret", Path(temporary) / "run")
                self.assertEqual(report["status"], "completed_with_failures")
                self.assertEqual(report["summary"]["origins"], {origin: 2})
                self.assertEqual(state["calls"], 3)
                self.assertNotIn("secret-account-not-recorded", json.dumps(report))

    def test_eof_timeout_and_reset_are_distinct(self):
        for mode, outcome in (("missing_terminal", "missing_terminal_event"), ("timeout", "timeout"), ("reset", "transport_error")):
            with self.subTest(mode=mode), upstream(mode) as (base, state):
                settings = config(base, timeout_seconds=.08 if mode == "timeout" else 5)
                row = probe.request_once(settings, "synthetic-only-secret", "synthetic-id", probe.Inflight())
                self.assertFalse(row["success"])
                self.assertEqual(row["outcome"], outcome)
                self.assertEqual(state["calls"], 1)

    def test_metadata_only_accepts_explicit_digest_fields(self):
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / "metadata.json"
            path.write_text(json.dumps({"binary_sha256": "a" * 64}), encoding="utf-8")
            self.assertEqual(probe.load_metadata(path), {"binary_sha256": "a" * 64})
            path.write_text(json.dumps({"access_token": "do-not-copy"}), encoding="utf-8")
            with self.assertRaises(ValueError):
                probe.load_metadata(path)


if __name__ == "__main__":
    unittest.main()
