"""Real loopback regression for the built HTTP-only native transport.

Uses only Python's standard library and synthetic bodies/credentials. Does not
call the CLI, production gateway, OAuth endpoint or a public model endpoint.
"""
from __future__ import annotations

import argparse
import base64
import gzip
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import queue
import secrets
import shutil
import socket
import ssl
import stat
import struct
import subprocess
import sys
import tempfile
import threading
import time
import unittest
from urllib.parse import urlsplit, parse_qs


HEAD, DATA, END, ERROR = 1, 2, 3, 4
BINARY = None


def encode_headers(headers):
    return [(name, base64.b64encode(value if isinstance(value, bytes) else value.encode()).decode()) for name, value in headers]


def decode_headers(head):
    return [(name, base64.b64decode(value)) for name, value in head["headers"]]


def read_exact(stream, count):
    out = bytearray()
    while len(out) < count:
        data = stream.recv(count - len(out))
        if not data:
            raise EOFError("IPC closed")
        out.extend(data)
    return bytes(out)


def read_frame(stream):
    kind, length = struct.unpack("!BI", read_exact(stream, 5))
    if length > 65536:
        raise AssertionError("unbounded IPC frame")
    return kind, read_exact(stream, length)


class Helper:
    @staticmethod
    def environment(overrides=None):
        env = dict(os.environ)
        for name in ("CODEX_CA_CERTIFICATE", "SSL_CERT_FILE", "SSL_CERT_DIR", "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "all_proxy", "no_proxy", "RUST_LOG"):
            env.pop(name, None)
        env.update(overrides or {})
        return env

    def __init__(self, upstream, **overrides):
        self.token = secrets.token_hex(32)
        env = self.environment(overrides.pop("environment", {}))
        config = dict(protocol=1, listen="127.0.0.1:0", upstream=upstream, ipc_token=self.token, headers_timeout_ms=1000, idle_timeout_ms=150, http_client_lifecycle="pooled")
        config.update(overrides)
        if config.get("http_client_lifecycle") is None:
            config.pop("http_client_lifecycle", None)
        if config.get("unix_socket"):
            config.pop("listen", None)
        self.process = subprocess.Popen([str(BINARY)], stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE, env=env)
        encoded = json.dumps(config).encode()
        self.process.stdin.write(struct.pack("!I", len(encoded)) + encoded)
        self.process.stdin.flush()
        received = queue.Queue()
        threading.Thread(target=lambda: received.put(self.process.stdout.readline()), daemon=True).start()
        try:
            line = received.get(timeout=20)
            ready = json.loads(line)
            self.ready = ready
            self.transport = ready["transport"]
            if self.transport == "unix":
                self.address = ready["unix_socket"]
            else:
                host, port = ready["listen"].rsplit(":", 1)
                self.address = (host, int(port))
            assert ready["protocol"] == 1 and ready["cli_source"] == "0.155.0"
        except Exception:
            self.process.kill()
            self.process.communicate(timeout=3)
            raise AssertionError("helper did not report readiness") from None

    def request(self, path, body=b"", headers=(), method="POST", token=None, total_timeout_ms=None, request_compression=None):
        head = dict(protocol=1, ipc_token=token if token is not None else self.token, method=method, path=path, headers=encode_headers(headers), body_length=len(body))
        if total_timeout_ms is not None:
            head["total_timeout_ms"] = total_timeout_ms
        if request_compression is not None:
            head["request_compression"] = request_compression
        return self.raw_request(head, body)

    def raw_request(self, head, body=b""):
        stream = self.connect()
        stream.settimeout(4)
        encoded = json.dumps(head).encode()
        stream.sendall(struct.pack("!I", len(encoded)) + encoded + body)
        return stream

    def connect(self):
        if self.transport == "unix":
            stream = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
            stream.settimeout(3)
            stream.connect(self.address)
            return stream
        return socket.create_connection(self.address, timeout=3)

    def collect(self, *args, **kwargs):
        with self.request(*args, **kwargs) as stream:
            head, body, error = None, bytearray(), None
            while True:
                kind, data = read_frame(stream)
                if kind == HEAD:
                    head = json.loads(data)
                elif kind == DATA:
                    body.extend(data)
                elif kind == ERROR:
                    error = data.decode()
                    break
                elif kind == END:
                    break
                else:
                    raise AssertionError("unknown response frame")
            return head, bytes(body), error

    def close(self):
        if self.process.poll() is None:
            self.process.stdin.close()
            self.process.stdin = None
        _, errors = self.process.communicate(timeout=4)
        assert self.process.returncode == 0, "helper shutdown failed"
        assert errors == b"", "helper emitted diagnostics during normal requests"


class Fixture(ThreadingHTTPServer):
    daemon_threads = True
    request_queue_size = 256  # Keep the synthetic upstream from throttling 96-request bursts.

    def __init__(self, tls_context=None):
        super().__init__(("127.0.0.1", 0), Handler)
        self.tls = tls_context is not None
        if tls_context:
            self.socket = tls_context.wrap_socket(self.socket, server_side=True)
        self.connections = 0
        self.requests = []
        self.started = {}
        self.closed = {}
        self.lock = threading.Lock()
        self.thread = threading.Thread(target=self.serve_forever, daemon=True)
        self.thread.start()

    def get_request(self):
        stream, address = super().get_request()
        with self.lock:
            self.connections += 1
        return stream, address

    def handle_error(self, request, client_address):
        pass

    @property
    def origin(self):
        scheme = "https" if self.tls else "http"
        return f"{scheme}://127.0.0.1:{self.server_port}"

    def event(self, kind, path):
        with self.lock:
            return getattr(self, kind).setdefault(path, threading.Event())

    def close(self):
        self.shutdown()
        self.server_close()
        self.thread.join(timeout=2)


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *_):
        pass

    def chunk(self, data):
        self.wfile.write(f"{len(data):x}\r\n".encode() + data + b"\r\n")
        self.wfile.flush()

    def do_GET(self):
        self.do_POST()

    def do_POST(self):
        target = urlsplit(self.path)
        path = target.path
        body = self.rfile.read(int(self.headers.get("content-length", "0")))
        with self.server.lock:
            self.server.requests.append({"path": self.path, "body": body, "headers": list(self.headers.raw_items()), "method": self.command,
                "tls": self.connection.version() if isinstance(self.connection, ssl.SSLSocket) else None,
                "alpn": self.connection.selected_alpn_protocol() if isinstance(self.connection, ssl.SSLSocket) else None})
        self.server.event("started", self.path).set()
        if path == "/headers_wait":
            self.connection.settimeout(4)
            try:
                self.connection.recv(1)
            except (OSError, TimeoutError):
                pass
            self.server.event("closed", self.path).set()
            self.close_connection = True
            return
        if path == "/disconnect":
            self.close_connection = True
            return
        if path == "/upgrade":
            self.send_response(101)
            self.send_header("Connection", "upgrade")
            self.send_header("Upgrade", "synthetic")
            self.end_headers()
            self.close_connection = True
            return
        if path in ("/sse", "/idle", "/stream_wait", "/large"):
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.send_header("Transfer-Encoding", "chunked")
            self.end_headers()
            try:
                if path == "/large":
                    block = b"x" * 16384
                    for _ in range(2048):
                        self.chunk(block)
                elif path == "/stream_wait":
                    self.chunk(b"data: start\n\n")
                    self.connection.settimeout(4)
                    self.connection.recv(1)
                    self.close_connection = True
                    return
                else:
                    for i in range(4):
                        self.chunk(f"data: {i}\n\n".encode())
                        time.sleep(0.06 if path == "/sse" else 0.4)
                self.wfile.write(b"0\r\n\r\n")
                self.wfile.flush()
            except (OSError, TimeoutError):
                self.close_connection = True
            finally:
                self.server.event("closed", self.path).set()
            return
        status = int(parse_qs(target.query).get("status", ["200"])[0])
        if path == "/redirect":
            status = 307
        self.send_response(status)
        self.send_header("Content-Length", str(len(body)))
        self.send_header("X-Repeat", "one")
        self.send_header("X-Repeat", "two")
        self.send_header("Set-Cookie", "a=1")
        self.send_header("Set-Cookie", "b=2")
        self.send_header("X-Raw", "\xff")
        if path == "/redirect":
            self.send_header("Location", "/must-not-follow")
        if path == "/gzip":
            self.send_header("Content-Encoding", "gzip")
        self.end_headers()
        self.wfile.write(body)
        self.wfile.flush()


class TransportTests(unittest.TestCase):
    def setUp(self):
        self.server = Fixture()
        self.helper = Helper(self.server.origin)

    def tearDown(self):
        self.helper.close()
        self.server.close()

    def test_raw_body_duplicate_headers_and_connection_reuse(self):
        payload = bytes(range(256)) + b' { "tools": [ ], "input": "synthetic" } \n'
        count = self.server.connections
        for _ in range(3):
            head, body, error = self.helper.collect("/echo", payload, headers=[("X-Repeat", "A"), ("X-Repeat", "B"), ("X-Raw", b"\x80\xff"), ("Authorization", "Bearer fixture-token")])
            self.assertEqual((head["status"], body, error), (200, payload, None))
            headers = decode_headers(head)
            self.assertEqual([v for n, v in headers if n == "x-repeat"], [b"one", b"two"])
            self.assertEqual([v for n, v in headers if n == "set-cookie"], [b"a=1", b"b=2"])
            self.assertIn(("x-raw", b"\xff"), headers)
        self.assertEqual(self.server.connections - count, 1)
        request = self.server.requests[-1]
        self.assertEqual(request["body"], payload)
        self.assertEqual([v for n, v in request["headers"] if n.lower() == "x-repeat"], ["A", "B"])
        self.assertEqual([v.encode("latin-1") for n, v in request["headers"] if n.lower() == "x-raw"], [b"\x80\xff"])
        self.assertFalse(any(n.lower() in ("accept-encoding", "cookie", "ipc-token", "version", "originator", "user-agent") for n, _ in request["headers"]))

    def test_default_cli_request_lifecycle_does_not_share_http_clients(self):
        for selected in (None, "cli_request", "pooled"):
            helper = Helper(self.server.origin, http_client_lifecycle=selected)
            try:
                self.assertEqual(helper.ready["http_client_lifecycle"], selected or "cli_request")
                before = self.server.connections
                for _ in range(4):
                    self.assertEqual(helper.collect("/echo", b"lifecycle")[1], b"lifecycle")
                self.assertEqual(self.server.connections - before, 1 if selected == "pooled" else 4)
            finally:
                helper.close()

    def test_all_statuses_pass_through_without_retry(self):
        for status in (401, 403, 429, 500, 502, 503):
            path = f"/echo?status={status}"
            head, body, error = self.helper.collect(path, b"synthetic-error")
            self.assertEqual((head["status"], body, error), (status, b"synthetic-error", None))
            self.assertEqual(sum(r["path"] == path for r in self.server.requests), 1)
        head, body, error = self.helper.collect("/redirect", b"redirect-body")
        self.assertEqual((head["status"], body, error), (307, b"redirect-body", None))
        self.assertFalse(any(r["path"] == "/must-not-follow" for r in self.server.requests))
        _, _, error = self.helper.collect("/disconnect", b"must-not-replay")
        self.assertIsNotNone(error)
        self.assertEqual(sum(r["path"] == "/disconnect" for r in self.server.requests), 1)
        head, body, error = self.helper.collect("/upgrade")
        self.assertEqual((head["status"], body, error), (101, b"", "upgrade_unsupported"))

    def test_encoded_bytes_are_not_decoded_or_recompressed(self):
        payload = gzip.compress(b"synthetic compressed response")
        head, body, error = self.helper.collect("/gzip", payload, headers=[("Content-Encoding", "gzip")])
        self.assertEqual((body, error), (payload, None))
        self.assertIn(("content-encoding", b"gzip"), decode_headers(head))
        self.assertEqual(self.server.requests[-1]["body"], payload)
        # Encoding labels do not make the adapter parse or transform body bytes.
        payload = b"synthetic-prepared-zstd-bytes\x00\xff"
        _, body, error = self.helper.collect("/echo", payload, headers=[("Content-Encoding", "zstd")])
        self.assertEqual((body, error), (payload, None))

    def test_official_zstd_request_mode_preserves_prepared_json_wire_contract(self):
        self.assertIn("request_zstd_v1", self.helper.ready["capabilities"])
        payload = b'{"z": 1e+03,"input":[],"tools":[],"a":"\\u0061"}'
        outputs = []
        for _ in range(2):
            head, body, error = self.helper.collect("/echo", payload, headers=[("Content-Type", "application/json"), ("Content-Length", str(len(payload)))], request_compression="zstd")
            self.assertEqual((head["status"], error), (200, None))
            self.assertTrue(body.startswith(b"\x28\xb5\x2f\xfd"))
            outgoing = self.server.requests[-1]
            self.assertEqual(outgoing["body"], body)
            self.assertEqual([value for name, value in outgoing["headers"] if name.lower() == "content-encoding"], ["zstd"])
            self.assertEqual([value for name, value in outgoing["headers"] if name.lower() == "content-length"], [str(len(body))])
            outputs.append(body)
        self.assertEqual(outputs[0], outputs[1])
        self.assertEqual(self.helper.collect("/echo", payload)[1], payload)
        before = len(self.server.requests)
        for body, headers, mode, expected in (
            (b"{}", [("Content-Encoding", "zstd")], "zstd", "request_compression_encoding_conflict"),
            (b"not-json", [], "zstd", "request_compression_json"),
            (b" {}\n", [], "zstd", "request_compression_json_noncanonical"),
            (b"{}", [], "gzip", "request_json"),
        ):
            self.assertEqual(self.helper.collect("/echo", body, headers=headers, request_compression=mode)[2], expected)
        request = dict(protocol=1, ipc_token=self.helper.token, method="GET", path="/echo", headers=[], body_length=0,
                       operation="websocket_message_v1", request_compression="zstd")
        with self.helper.raw_request(request) as stream:
            self.assertEqual(read_frame(stream), (ERROR, b"request_compression_operation"))
        self.assertEqual(len(self.server.requests), before)

    def test_sse_flush_and_active_stream_exceeds_idle_budget(self):
        started = time.monotonic()
        with self.helper.request("/sse") as stream:
            self.assertEqual(read_frame(stream)[0], HEAD)
            kind, first = read_frame(stream)
            self.assertEqual((kind, first), (DATA, b"data: 0\n\n"))
            self.assertLess(time.monotonic() - started, 0.2)
            body = first
            while True:
                kind, data = read_frame(stream)
                if kind == END:
                    break
                self.assertEqual(kind, DATA)
                body += data
        self.assertEqual(body, b"data: 0\n\ndata: 1\n\ndata: 2\n\ndata: 3\n\n")
        self.assertGreater(time.monotonic() - started, 0.2)

    def test_idle_and_total_deadlines(self):
        head, body, error = self.helper.collect("/idle")
        self.assertEqual((head["status"], body, error), (200, b"data: 0\n\n", "idle_timeout"))
        _, _, error = self.helper.collect("/sse", total_timeout_ms=100)
        self.assertEqual(error, "total_timeout")

    def test_headers_deadline_cancels_without_retry(self):
        path = "/headers_wait?deadline=1"
        head, body, error = self.helper.collect(path)
        self.assertEqual((head, body, error), (None, b"", "headers_timeout"))
        self.assertTrue(self.server.event("closed", path).wait(1.5))
        self.assertEqual(sum(r["path"] == path for r in self.server.requests), 1)
        self.assertEqual(self.helper.collect("/echo", b"after-deadline")[1], b"after-deadline")

    def test_cancel_and_eof_before_headers_and_during_stream(self):
        for path, cancel_byte in (("/headers_wait?a=1", True), ("/headers_wait?a=2", False), ("/stream_wait?a=1", True), ("/stream_wait?a=2", False)):
            stream = self.helper.request(path)
            self.assertTrue(self.server.event("started", path).wait(2))
            if path.startswith("/stream_wait"):
                self.assertEqual(read_frame(stream)[0], HEAD)
                self.assertEqual(read_frame(stream)[0], DATA)
            began = time.monotonic()
            if cancel_byte:
                stream.sendall(b"\x03")
                self.assertEqual(stream.recv(1), b"")
            stream.close()
            self.assertTrue(self.server.event("closed", path).wait(1.5))
            self.assertLess(time.monotonic() - began, 1.5)
        self.assertEqual(self.helper.collect("/echo", b"still-ready")[1], b"still-ready")

    def test_slow_consumer_does_not_start_idle_clock_or_block_other_requests(self):
        with self.helper.request("/large") as stream:
            self.assertEqual(read_frame(stream)[0], HEAD)
            # Wait longer than the 150 ms upstream idle budget while refusing
            # output. This is application backpressure, not network idle.
            time.sleep(0.4)
            began = time.monotonic()
            self.assertEqual(self.helper.collect("/echo", b"parallel")[1], b"parallel")
            self.assertLess(time.monotonic() - began, 1)
            count = 0
            while True:
                kind, data = read_frame(stream)
                if kind == END:
                    break
                self.assertEqual(kind, DATA)
                count += len(data)
            self.assertEqual(count, 32 * 1024 * 1024)

    def test_cancel_interrupts_backpressured_writer(self):
        path = "/large?cancel=1"
        stream = self.helper.request(path)
        stream.setsockopt(socket.SOL_SOCKET, socket.SO_RCVBUF, 4096)
        self.assertEqual(read_frame(stream)[0], HEAD)
        time.sleep(0.2)
        stream.sendall(b"\x03")
        self.assertTrue(self.server.event("closed", path).wait(1.5))
        stream.close()
        self.assertEqual(self.helper.collect("/echo", b"after-cancel")[1], b"after-cancel")

    def test_parent_eof_cancels_waiting_and_backpressured_requests(self):
        helper = Helper(self.server.origin)
        waiting = helper.request("/headers_wait?parent=1")
        blocked = helper.request("/large?parent=1")
        blocked.setsockopt(socket.SOL_SOCKET, socket.SO_RCVBUF, 4096)
        try:
            self.assertTrue(self.server.event("started", "/headers_wait?parent=1").wait(2))
            self.assertEqual(read_frame(blocked)[0], HEAD)
            time.sleep(0.2)
            helper.close()
            self.assertTrue(self.server.event("closed", "/headers_wait?parent=1").wait(1.5))
            self.assertTrue(self.server.event("closed", "/large?parent=1").wait(1.5))
            self.assertEqual(waiting.recv(1), b"")
        finally:
            waiting.close()
            blocked.close()
            if helper.process.poll() is None:
                helper.close()

    def test_total_timeout_stops_backpressured_writer_without_false_completion(self):
        stream = self.helper.request("/large?deadline=1", total_timeout_ms=100)
        try:
            self.assertEqual(read_frame(stream)[0], HEAD)
            time.sleep(0.3)
            self.assertTrue(self.server.event("closed", "/large?deadline=1").wait(1.5))
            # Buffered complete DATA frames may precede a boundary ERROR or an
            # incomplete frame followed by EOF. END must never claim success.
            while True:
                try:
                    kind, data = read_frame(stream)
                except EOFError:
                    break
                self.assertNotEqual(kind, END)
                if kind == ERROR:
                    self.assertEqual(data, b"total_timeout")
                    break
                self.assertEqual(kind, DATA)
                self.assertTrue(data and data == b"x" * len(data))
        finally:
            stream.close()

    def test_auth_path_header_and_size_rejection_do_not_contact_upstream(self):
        before = len(self.server.requests)
        rejected = [({"token": "0" * 64}, "ipc_auth"), ({"headers": [("Content-Length", "123")]}, "content_length_invalid"), ({"headers": [("Content-Length", "0"), ("Content-Length", "0")]}, "content_length_invalid"), ({"method": "CONNECT"}, "method_unsupported"), ({"total_timeout_ms": 0}, "timeout_invalid"), ({"total_timeout_ms": 86400001}, "timeout_invalid")]
        rejected.extend(({"path": path}, "path_invalid") for path in ("//outside.invalid/x", "http://outside.invalid/x", "/echo#fragment", "/a\\b", "/echo\n", "/space here", "/a/../echo", "/a/%2e%2e/echo", "/\u00ff"))
        rejected.extend(({"headers": [(name, "synthetic")]}, "header_hop_by_hop") for name in ("Host", "Connection", "Transfer-Encoding", "Upgrade", "Trailer", "TE", "Proxy-Authorization", "Proxy-Authenticate", "Proxy-Connection", "Keep-Alive"))
        for kwargs, expected in rejected:
            values = dict(path="/echo")
            values.update(kwargs)
            self.assertEqual(self.helper.collect(**values)[2], expected)
        with socket.create_connection(self.helper.address, timeout=3) as stream:
            stream.sendall(struct.pack("!I", 65537))
            self.assertEqual(read_frame(stream), (ERROR, b"request_limit"))
        normal = dict(protocol=1, ipc_token=self.helper.token, method="POST", path="/echo", headers=[], body_length=0)
        for patch, expected in (({"body_length": 33554433}, b"request_limit"), ({"headers": encode_headers([("x-repeat", "A")] * 129)}, b"request_limit"), ({"headers": [["x-raw", "not base64!"]]}, b"header_invalid"), ({"headers": encode_headers([("x-raw", b"\r\nsynthetic")])}, b"header_invalid"), ({"extra": True}, b"request_json")):
            with self.helper.raw_request(dict(normal, **patch)) as stream:
                self.assertEqual(read_frame(stream), (ERROR, expected))
        with self.helper.raw_request(dict(normal, body_length=16), b"short") as stream:
            stream.shutdown(socket.SHUT_WR)
            self.assertEqual(read_frame(stream), (ERROR, b"ipc_read"))
        self.assertEqual(len(self.server.requests), before)

    def test_gateway_body_limit_accepts_32_mib_and_rejects_next_byte(self):
        self.assertEqual(self.helper.ready["max_body"], 32 * 1024 * 1024)
        payload = b"x" * (32 * 1024 * 1024)
        head, body, error = self.helper.collect("/echo?body-limit=1", payload)
        self.assertEqual((head["status"], error), (200, None))
        self.assertEqual(body, payload)
        before = len(self.server.requests)
        request = dict(protocol=1, ipc_token=self.helper.token, method="POST", path="/echo", headers=[], body_length=len(payload) + 1)
        with self.helper.raw_request(request) as stream:
            self.assertEqual(read_frame(stream), (ERROR, b"request_limit"))
        self.assertEqual(len(self.server.requests), before)

    def test_zero_helper_timeouts_allow_parent_cancellation(self):
        helper = Helper(self.server.origin, headers_timeout_ms=0, idle_timeout_ms=0)
        try:
            head, body, error = helper.collect("/idle?zero-timeout=1")
            self.assertEqual((head["status"], error), (200, None))
            self.assertIn(b"data: 3", body)
            with helper.request("/headers_wait?zero-timeout=1") as stream:
                self.assertTrue(self.server.event("started", "/headers_wait?zero-timeout=1").wait(2))
                time.sleep(1.1)
                stream.sendall(b"\x03")
                self.assertEqual(stream.recv(1), b"")
            self.assertTrue(self.server.event("closed", "/headers_wait?zero-timeout=1").wait(1.5))
        finally:
            helper.close()

    def test_prepared_path_and_query_bytes_are_not_rewritten(self):
        path = "/echo/%2F?first=one&second=%2f&duplicate=a&duplicate=b&empty="
        self.assertEqual(self.helper.collect(path, b"exact-target")[1], b"exact-target")
        self.assertEqual(self.server.requests[-1]["path"], path)

    def test_bad_configuration_has_fixed_diagnostics_and_no_secret_output(self):
        token = secrets.token_hex(32)
        normal = dict(protocol=1, listen="127.0.0.1:0", upstream=self.server.origin, ipc_token=token)
        for patch, environment, error in (
            ({"listen": "0.0.0.0:0"}, {}, "config_invalid"),
            ({"upstream": "http://fixture-user:fixture-secret@127.0.0.1/"}, {}, "upstream_invalid"),
            ({"upstream": self.server.origin + "/nested"}, {}, "upstream_invalid"),
            ({"proxy": "ftp://fixture-user:fixture-secret@127.0.0.1:1"}, {}, "proxy_unsupported"),
            ({"proxy": "http://fixture-user:fixture-secret@127.0.0.1:1/not-a-proxy-origin"}, {}, "proxy_invalid"),
            ({}, {"CODEX_CA_CERTIFICATE": str(Path(tempfile.gettempdir()) / ("missing-ca-" + token)), "RUST_LOG": "trace"}, "tls_or_client_config"),
        ):
            encoded = json.dumps(dict(normal, **patch)).encode()
            result = subprocess.run([str(BINARY)], input=struct.pack("!I", len(encoded)) + encoded, capture_output=True, env=Helper.environment(environment), timeout=10)
            self.assertEqual(result.returncode, 1)
            self.assertEqual(result.stdout, b"")
            self.assertEqual(result.stderr, ("native-transport: " + error + "\n").encode())

    def test_explicit_http_proxy_is_used_and_environment_proxy_is_ignored(self):
        # The fixture acts as an HTTP forward proxy and sees absolute-form.
        proxy = Helper("http://synthetic.invalid", proxy=self.server.origin)
        try:
            head, body, error = proxy.collect("/echo", b"proxied")
            self.assertEqual((head["status"], body, error), (200, b"proxied", None))
            self.assertEqual(self.server.requests[-1]["path"], "http://synthetic.invalid/echo")
        finally:
            proxy.close()
        direct = Helper(self.server.origin, environment={"HTTP_PROXY": "http://127.0.0.1:1", "ALL_PROXY": "http://127.0.0.1:1"})
        try:
            self.assertEqual(direct.collect("/echo", b"direct")[1], b"direct")
        finally:
            direct.close()
        # Reserve a loopback port without listening so refusal is deterministic.
        with socket.socket() as unavailable:
            unavailable.bind(("127.0.0.1", 0))
            failed = Helper(self.server.origin, proxy=f"http://127.0.0.1:{unavailable.getsockname()[1]}")
            before = len(self.server.requests)
            try:
                head, body, error = failed.collect("/echo", b"must-not-go-direct")
                self.assertEqual((head, body, error), (None, b"", "upstream_connect"))
                self.assertEqual(len(self.server.requests), before)
            finally:
                failed.close()

    def test_real_tls_custom_ca_and_linux_native_trust(self):
        openssl = shutil.which("openssl")
        self.assertIsNotNone(openssl, "openssl executable is required for synthetic TLS certificate creation")
        with tempfile.TemporaryDirectory(prefix="native-transport-tls-") as folder:
            folder = Path(folder)
            cert, ca_key = folder / "root.pem", folder / "root.key"
            leaf, key, csr = folder / "server.pem", folder / "server.key", folder / "server.csr"
            extensions = folder / "server.ext"
            extensions.write_text("basicConstraints=critical,CA:FALSE\nkeyUsage=critical,digitalSignature,keyEncipherment\nextendedKeyUsage=serverAuth\nsubjectAltName=DNS:localhost,IP:127.0.0.1\n")
            commands = [
                [openssl, "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-sha256", "-days", "2", "-subj", "/CN=Synthetic Loopback Root", "-addext", "basicConstraints=critical,CA:TRUE", "-addext", "keyUsage=critical,keyCertSign,cRLSign", "-keyout", str(ca_key), "-out", str(cert)],
                [openssl, "req", "-new", "-newkey", "rsa:2048", "-nodes", "-sha256", "-subj", "/CN=localhost", "-keyout", str(key), "-out", str(csr)],
                [openssl, "x509", "-req", "-in", str(csr), "-CA", str(cert), "-CAkey", str(ca_key), "-CAcreateserial", "-days", "2", "-sha256", "-extfile", str(extensions), "-out", str(leaf)],
            ]
            for command in commands:
                result = subprocess.run(command, capture_output=True)
                self.assertEqual(result.returncode, 0, "synthetic certificate generation failed")
            context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
            context.load_cert_chain(leaf, key)
            context.set_alpn_protocols(["http/1.1"])
            server = Fixture(tls_context=context)
            try:
                # Trust must be checked. There is no insecure certificate flag.
                untrusted = Helper(server.origin)
                try:
                    head, _, error = untrusted.collect("/echo", b"not-trusted")
                    self.assertIsNone(head)
                    self.assertEqual(error, "upstream_connect")
                    self.assertEqual(len(server.requests), 0)
                finally:
                    untrusted.close()
                # This environment variable deliberately selects official rustls.
                custom = Helper(server.origin, environment={"SSL_CERT_FILE": str(cert)})
                try:
                    head, body, error = custom.collect("/echo", b"rustls-verified")
                    self.assertIsNone(error, "custom-CA TLS handshake failed")
                    self.assertIsNotNone(head)
                    self.assertEqual((head["status"], body, error), (200, b"rustls-verified", None))
                    self.assertIn(server.requests[-1]["tls"], ("TLSv1.2", "TLSv1.3"))
                    self.assertEqual(server.requests[-1]["alpn"], "http/1.1")
                finally:
                    custom.close()
                # On Linux, OpenSSL accepts SSL_CERT_DIR without triggering
                # Codex's SSL_CERT_FILE custom-CA switch. Never install system CA.
                if sys.platform.startswith("linux"):
                    cert_hash = subprocess.run([openssl, "x509", "-in", str(cert), "-noout", "-hash"], capture_output=True, check=True).stdout.decode().strip()
                    self.assertRegex(cert_hash, r"^[0-9a-f]{8}$")
                    shutil.copyfile(cert, folder / (cert_hash + ".0"))
                    native = Helper(server.origin, environment={"SSL_CERT_DIR": str(folder)})
                    try:
                        head, body, error = native.collect("/echo", b"native-verified")
                        self.assertIsNone(error, "native TLS handshake failed")
                        self.assertIsNotNone(head)
                        self.assertEqual((head["status"], body, error), (200, b"native-verified", None))
                        self.assertIn(server.requests[-1]["tls"], ("TLSv1.2", "TLSv1.3"))
                        self.assertIsNone(server.requests[-1]["alpn"], "native-tls-alpn must not be silently added")
                    finally:
                        native.close()
            finally:
                server.close()


@unittest.skipUnless(hasattr(socket, "AF_UNIX") and os.name == "posix", "Unix socket ownership requires Unix")
class UnixTransportTests(unittest.TestCase):
    def setUp(self):
        self.folder = tempfile.TemporaryDirectory(prefix="native-ipc-")
        self.directory = Path(self.folder.name).resolve()
        self.directory.chmod(0o700)
        self.path = self.directory / "helper.sock"
        self.server = Fixture()

    def tearDown(self):
        self.server.close()
        self.folder.cleanup()

    def failed_config(self, path, expected):
        config = dict(protocol=1, unix_socket=str(path), upstream=self.server.origin, ipc_token=secrets.token_hex(32))
        encoded = json.dumps(config).encode()
        result = subprocess.run([str(BINARY)], input=struct.pack("!I", len(encoded)) + encoded, capture_output=True, env=Helper.environment(), timeout=10)
        self.assertEqual(result.returncode, 1)
        self.assertEqual(result.stdout, b"")
        self.assertEqual(result.stderr, ("native-transport: " + expected + "\n").encode())

    def test_private_unix_socket_permissions_reuse_cancel_and_cleanup(self):
        helper = Helper(self.server.origin, unix_socket=str(self.path))
        try:
            metadata = self.path.lstat()
            self.assertTrue(stat.S_ISSOCK(metadata.st_mode))
            self.assertEqual(stat.S_IMODE(metadata.st_mode), 0o600)
            self.assertEqual(metadata.st_uid, os.geteuid())
            self.assertEqual(helper.ready["transport"], "unix")
            self.assertNotIn("listen", helper.ready)
            for _ in range(3):
                self.assertEqual(helper.collect("/echo", b"uds-pool")[1], b"uds-pool")
            self.assertEqual(self.server.connections, 1)
            with helper.request("/stream_wait?unix=1") as stream:
                self.assertEqual(read_frame(stream)[0], HEAD)
                self.assertEqual(read_frame(stream)[0], DATA)
                stream.sendall(b"\x03")
                self.assertEqual(stream.recv(1), b"")
            self.assertTrue(self.server.event("closed", "/stream_wait?unix=1").wait(1.5))
        finally:
            helper.close()
        self.assertFalse(self.path.exists())

    def test_unix_parent_must_be_private_real_directory(self):
        self.directory.chmod(0o755)
        self.failed_config(self.path, "unix_parent_invalid")
        self.directory.chmod(0o700)
        real = self.directory / "real"
        real.mkdir(mode=0o700)
        link = self.directory / "link"
        link.symlink_to(real, target_is_directory=True)
        self.failed_config(link / "helper.sock", "unix_parent_invalid")
        self.failed_config(Path("relative.sock"), "unix_path_invalid")

    def test_existing_file_socket_and_symlink_are_never_removed(self):
        self.path.write_text("unrelated-file")
        self.failed_config(self.path, "unix_path_exists")
        self.assertEqual(self.path.read_text(), "unrelated-file")
        self.path.unlink()
        unrelated = self.directory / "unrelated"
        unrelated.write_text("unrelated-target")
        self.path.symlink_to(unrelated)
        self.failed_config(self.path, "unix_path_exists")
        self.assertTrue(self.path.is_symlink())
        self.assertEqual(unrelated.read_text(), "unrelated-target")
        self.path.unlink()
        with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as owned:
            owned.bind(str(self.path))
            identity = self.path.stat().st_ino
            self.failed_config(self.path, "unix_path_exists")
            self.assertEqual(self.path.stat().st_ino, identity)

    def test_cleanup_does_not_remove_replaced_socket_or_symlink(self):
        for replacement in ("socket", "symlink"):
            helper = Helper(self.server.origin, unix_socket=str(self.path))
            self.path.unlink()
            owned = None
            try:
                if replacement == "socket":
                    owned = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
                    owned.bind(str(self.path))
                else:
                    target = self.directory / "unrelated-target"
                    target.write_text("preserved")
                    self.path.symlink_to(target)
                identity = self.path.lstat().st_ino
                helper.close()
                self.assertEqual(self.path.lstat().st_ino, identity)
            finally:
                if owned:
                    owned.close()
                if helper.process.poll() is None:
                    helper.close()
                self.path.unlink(missing_ok=True)

    def test_parent_eof_cancels_unix_request_and_removes_own_socket(self):
        helper = Helper(self.server.origin, unix_socket=str(self.path), headers_timeout_ms=0)
        stream = helper.request("/headers_wait?unix-parent=1")
        try:
            self.assertTrue(self.server.event("started", "/headers_wait?unix-parent=1").wait(2))
            helper.close()
            self.assertEqual(stream.recv(1), b"")
            self.assertTrue(self.server.event("closed", "/headers_wait?unix-parent=1").wait(1.5))
            self.assertFalse(self.path.exists())
        finally:
            stream.close()
            if helper.process.poll() is None:
                helper.close()

    def test_full_capacity_close_then_immediate_refill_never_resets(self):
        report = self.directory / "refill-report.json"
        result = subprocess.run([sys.executable, str(Path(__file__).with_name("refill_probe.py")),
                                 "--binary", str(BINARY), "--report", str(report), "--loops", "10"],
                                capture_output=True, timeout=45)
        self.assertEqual(result.returncode, 0, "full-capacity cancel/refill rejected replacement IPC")
        observed = json.loads(report.read_text())
        self.assertTrue(observed["passed"])
        self.assertEqual(len(observed["batches"]), 11)
        self.assertEqual(observed["concurrency"], 96)
        self.assertTrue(all(batch["heads"] == 96 and not batch["error_classes"] for batch in observed["batches"]))


def main():
    global BINARY
    parser = argparse.ArgumentParser()
    parser.add_argument("--binary", required=True, type=Path)
    parser.add_argument("--report", type=Path)
    args = parser.parse_args()
    BINARY = args.binary.resolve(strict=True)
    suite = unittest.TestSuite([unittest.defaultTestLoader.loadTestsFromTestCase(test)
                              for test in (TransportTests, UnixTransportTests)])
    started = time.monotonic()
    result = unittest.TextTestRunner(verbosity=2).run(suite)
    if args.report:
        args.report.write_text(json.dumps({"scope": "synthetic loopback HTTP IPC", "live_accounts": False, "tests": result.testsRun, "failures": len(result.failures), "errors": len(result.errors), "skipped": len(result.skipped), "passed": result.wasSuccessful(), "seconds": round(time.monotonic() - started, 3)}, indent=2) + "\n")
    raise SystemExit(0 if result.wasSuccessful() else 1)


if __name__ == "__main__":
    main()
