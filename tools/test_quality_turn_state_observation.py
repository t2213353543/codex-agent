"""Pure/socket tests plus opt-in real bf920 TLS fixture; never calls an upstream."""
import base64
import contextlib
import hashlib
import io
import json
import os
from pathlib import Path
import shutil
import socket
import ssl
import struct
import subprocess
import tempfile
import threading
import time
from types import SimpleNamespace
import unittest
from unittest import mock
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import quality_turn_state_observation as probe

TOKEN, WORKSPACE = "synthetic-access", "synthetic-workspace"
BODY = b'{"model":"fixture","stream":true,"store":false,"input":[{"role":"user","content":"synthetic fixture"}]}'
IDENTITY = {"user_agent": "codex_exec/0.155.0 (TestOS 1; x86_64) synthetic", "originator": "codex_exec", "version": "0.155.0"}
NATIVE_SHA = "bf920a0bd578afa5180d05d60c47e98349cd35490100067eae9a35553f8d4863"


def pair(value, name="X-Codex-Turn-State"):
    return [name, base64.b64encode(value).decode()]


def captured():
    return {"schema": probe.base.CAPTURE_HEAD_SCHEMA, "method": "POST", "path": "/backend-api/codex/responses",
            "body_sha256": probe.base.sha(BODY), "body_bytes": len(BODY), "request_compression": "none",
            "headers": [pair(value.encode(), name) for name, value in [
                ("User-Agent", IDENTITY["user_agent"]), ("Originator", IDENTITY["originator"]),
                ("Version", "0.155.0"), ("Accept", "text/event-stream"), ("Content-Type", "application/json")]]}


def frame(kind, raw=b""):
    return struct.pack("!BI", kind, len(raw)) + raw


def final_body():
    return b'data: ' + json.dumps({"type": "response.completed", "response": {
        "id": "synthetic-response", "status": "completed", "model": "fixture",
        "usage": {"input_tokens": 4, "output_tokens": 8}, "output": [{"type": "message", "role": "assistant",
        "content": [{"type": "output_text", "text": "Final answer: 21"}]}]}}).encode() + b'\n\n'


def wire(headers=None, status=200):
    head = {"status": status, "version": "HTTP/1.1", "headers": headers or []}
    body = final_body() if status == 200 else b'{"error":{"type":"invalid_request_error","message":"synthetic"}}'
    return frame(1, json.dumps(head).encode()) + frame(2, body) + frame(3)


class Stream:
    def __init__(self, raw, chunk=3):
        self.incoming, self.sent = io.BytesIO(raw), bytearray()
        self.chunk, self.closed = chunk, False
    def recv(self, count): return self.incoming.read(min(count, self.chunk))
    def send(self, raw):
        count = min(len(raw), self.chunk)
        self.sent.extend(raw[:count])
        return count
    def settimeout(self, value): pass
    def close(self): self.closed = True


class SummaryTests(unittest.TestCase):
    def test_absent_empty_and_duplicates_are_distinct(self):
        absent = probe.summarize_turn_state_headers([])
        self.assertEqual((absent["present"], absent["count"]), (False, 0))
        result = probe.summarize_turn_state_headers([pair(b""), pair(b"A" * 292, "x-codex-turn-state"), pair(b"B" * 312)])
        self.assertEqual((result["present"], result["count"]), (True, 3))
        self.assertEqual([v["value_bytes"] for v in result["values"]], [0, 292, 312])
        self.assertEqual([v["header_index"] for v in result["values"]], [0, 1, 2])
        self.assertEqual(result["values"][0]["value_sha256"], hashlib.sha256(b"").hexdigest())
        self.assertNotIn("quality", result)

    def test_utf8_ascii_and_invalid_utf8_do_not_interpret_state(self):
        result = probe.summarize_turn_state_headers([pair("汉é".encode()), pair(b"\xff\xfe"), pair(b"YWJj")])
        self.assertEqual([(v["value_bytes"], v["ascii_valid"], v["utf8_valid"], v["utf8_char_length"]) for v in result["values"]],
                         [(5, False, True, 2), (2, False, False, None), (4, True, True, 4)])
        self.assertEqual(result["values"][2]["value_sha256"], probe.base.sha(b"YWJj"))
        self.assertNotIn("YWJj", json.dumps(result))

    def test_all_inputs_bounded_and_errors_never_echo(self):
        bad = [None, tuple(), [pair(b"x")] * 33, [["bad\nname", ""]], [["X-Codex-Turn-State", "private!bad"]],
               [["X-Codex-Turn-State", "汉"]], [pair(b"x" * 16385)], [["X-Codex-Turn-State", False]], [pair(b"x"), [1, ""]]]
        for value in bad:
            with self.subTest(value_type=type(value).__name__):
                with self.assertRaises(ValueError) as error: probe.summarize_turn_state_headers(value)
                self.assertRegex(str(error.exception), r"^turn_state_[a-z0-9_]+$")
                self.assertNotIn("private", str(error.exception))
        self.assertEqual(probe.summarize_turn_state_headers([pair(b"x" * 16384)])["values"][0]["value_bytes"], 16384)
        self.assertEqual(probe.summarize_turn_state_headers([pair(b"")] * 32)["count"], 32)


class ObserverTests(unittest.TestCase):
    def test_fragmentation_exact_bytes_and_only_first_frame(self):
        raw = wire([pair(b"opaque-do-not-output")]) + frame(1, b"bad trailing duplicate")
        for chunk in (1, 2, 4, 5, 11, 65536):
            with self.subTest(chunk=chunk):
                source, observer = Stream(raw, chunk), probe._HeaderObserver()
                tapped = probe._ObservedSocket(source, observer)
                received = bytearray()
                while True:
                    part = tapped.recv(65536)
                    if not part: break
                    received.extend(part)
                self.assertEqual(bytes(received), raw)
                tapped.send(b"unchanged")
                self.assertEqual(source.sent, b"unchanged"[:chunk])
                tapped.close()
                result = observer.summary()
                self.assertTrue(result["complete"])
                self.assertEqual(result["count"], 1)
                self.assertEqual(observer._buffer, bytearray())
                self.assertNotIn("opaque-do-not-output", json.dumps(result))

    def test_invalid_and_truncated_frames_remain_unknown_not_absent(self):
        cases = [(b"", "native_headers_not_observed"), (b"\x01\x00", "native_header_frame_incomplete"),
                 (frame(2, b"body"), "first_frame_not_headers"),
                 (struct.pack("!BI", 1, probe.base.MAX_HEAD + 1), "native_header_frame_limit"),
                 (frame(1, b"bad private json"), "native_response_headers_invalid"),
                 (frame(1, json.dumps({"status": 200, "version": "HTTP/1.1", "headers": [pair(b"x")] * 33}).encode()), "turn_state_header_count_invalid")]
        for raw, error in cases:
            observer = probe._HeaderObserver()
            observer.feed(raw)
            result = observer.summary()
            self.assertFalse(result["complete"])
            self.assertIsNone(result["present"])
            self.assertEqual(result["error"], error)
            self.assertFalse(observer._buffer)

    def test_old_parser_still_rejects_duplicate_head(self):
        raw_head = frame(1, json.dumps({"status": 200, "version": "HTTP/1.1", "headers": [pair(b"first")]}).encode())
        observer = probe._HeaderObserver()
        stream = probe._ObservedSocket(Stream(raw_head + raw_head + frame(3)), observer)
        with self.assertRaises(probe.base.ExactProbeError):
            probe.base._read_response(stream, time.monotonic() + 5, {}, probe.base.SSECollector(()))
        self.assertTrue(observer.summary()["complete"])
        self.assertEqual(observer.summary()["count"], 1)


class RequestTests(unittest.TestCase):
    def call(self, raw, cleanup=True):
        stream = Stream(raw)
        original_helper, original_transaction = probe.route_probe._RouteHelper, probe.base._transaction
        class FixtureHelper(probe._StateHelper):
            def start(self):
                self.capabilities = []
                self.record.update(process_started=True, pid=12345, runtime_executable_sha256=self.digest)
                return self
            def connect(self, phase):
                phase.update(helper_scope_id=self.record["scope_id"], helper_pid=12345,
                             runtime_executable_sha256=self.digest, helper_alive_before_dispatch=True)
                return probe._ObservedSocket(stream, self._observer)
            def close(self):
                self.record.update(reaped=True, forced_kill=not cleanup, local_socket_absent=True)
                return cleanup
        fake_os = SimpleNamespace(name="posix", geteuid=lambda: 1, readlink=lambda _: "net:[synthetic]")
        with tempfile.TemporaryDirectory() as folder:
            helper = Path(folder) / "synthetic-helper"
            helper.write_bytes(b"not executed")
            fake_socket = SimpleNamespace(AF_UNIX=1, timeout=socket.timeout)
            with mock.patch.object(probe, "_StateHelper", FixtureHelper), mock.patch.object(probe, "os", fake_os), mock.patch.object(probe, "socket", fake_socket):
                report = probe.native_state_request(helper, probe.base.file_digest(helper), BODY, captured(), TOKEN, WORKSPACE,
                                                    application_identity=IDENTITY, timeout=10)
        self.assertIs(probe.route_probe._RouteHelper, original_helper)
        self.assertIs(probe.base._transaction, original_transaction)
        return report, stream

    def test_complete_single_post_exact_request_no_state_injection(self):
        report, stream = self.call(wire([pair(b"private-state-292")]))
        self.assertTrue(report["protocol_success"], report)
        self.assertTrue(report["turn_state_observation"]["complete"])
        self.assertEqual(report["native_dispatch_count"], 1)
        self.assertEqual(report["metadata_get_count"], 0)
        size = struct.unpack("!I", stream.sent[:4])[0]
        head = json.loads(stream.sent[4:4 + size])
        self.assertEqual(bytes(stream.sent[4 + size:]), BODY)
        self.assertEqual(head["headers"][:-2], captured()["headers"])
        self.assertNotIn("private-state-292", json.dumps(report))
        self.assertEqual(report["turn_state_observation"]["additional_requests"], 0)
        self.assertTrue(stream.closed)

    def test_HTTP_failure_and_cleanup_failure_keep_observed_header_evidence(self):
        report, _ = self.call(wire([pair(b"failed-state")], 429))
        self.assertFalse(report["protocol_success"])
        self.assertEqual(report["turn_state_observation"]["http_status"], 429)
        self.assertTrue(report["turn_state_observation"]["complete"])
        self.assertEqual(report["native_dispatch_count"], 1)
        report, _ = self.call(wire([pair(b"")]), cleanup=False)
        self.assertFalse(report["protocol_success"])
        self.assertEqual(report["observed_completion"]["answer"], 21)
        self.assertEqual(report["turn_state_observation"]["values"][0]["value_bytes"], 0)

    def test_observer_limit_does_not_change_stream_but_disqualifies_sample(self):
        report, stream = self.call(wire([pair(b"x")] * 33))
        self.assertFalse(report["protocol_success"])
        self.assertFalse(report["eligible_for_scoring"])
        self.assertEqual(report["observed_completion"]["answer"], 21)
        self.assertEqual(report["failure_category"], "turn_state_observation_failed")
        self.assertEqual(report["turn_state_observation"]["error"], "turn_state_header_count_invalid")
        self.assertEqual(report["native_dispatch_count"], 1)
        self.assertTrue(stream.closed)


@contextlib.contextmanager
def tls_fixture(headers, status):
    """A self-signed test CA/leaf; no public DNS or upstream connection."""
    openssl = shutil.which("openssl")
    if not openssl: raise unittest.SkipTest("openssl fixture dependency unavailable")
    with tempfile.TemporaryDirectory(prefix="state-tls-") as folder:
        root = Path(folder)
        key, cert, csr, cakey, ca, ext = [root / name for name in ("key", "cert", "csr", "cakey", "ca", "ext")]
        ext.write_text("basicConstraints=critical,CA:FALSE\nsubjectAltName=DNS:chatgpt.com\nkeyUsage=critical,digitalSignature,keyEncipherment\nextendedKeyUsage=serverAuth\n")
        commands = [["req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1", "-subj", "/CN=Local synthetic CA", "-addext", "basicConstraints=critical,CA:TRUE", "-keyout", str(cakey), "-out", str(ca)],
                    ["req", "-new", "-newkey", "rsa:2048", "-nodes", "-subj", "/CN=chatgpt.com", "-keyout", str(key), "-out", str(csr)],
                    ["x509", "-req", "-in", str(csr), "-CA", str(ca), "-CAkey", str(cakey), "-CAcreateserial", "-days", "1", "-extfile", str(ext), "-out", str(cert)]]
        for command in commands:
            subprocess.run([openssl] + command, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=15, check=True)
        seen, sni = [], []
        class Handler(BaseHTTPRequestHandler):
            def do_POST(self):
                raw = self.rfile.read(int(self.headers["Content-Length"]))
                seen.append({"body": raw, "headers": list(self.headers.items()), "path": self.path})
                body = final_body() if status == 200 else b'{"error":{"type":"invalid_request_error"}}'
                self.send_response(status)
                for name, value in headers: self.send_header(name, value.decode("latin-1"))
                self.send_header("Content-Type", "text/event-stream" if status == 200 else "application/json")
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)
            def log_message(self, *args): pass
        server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        context.load_cert_chain(str(cert), str(key))
        context.set_servername_callback(lambda sock, name, ctx: sni.append(name))
        server.socket = context.wrap_socket(server.socket, server_side=True)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try: yield server.server_address, ca, seen, sni
        finally: server.shutdown(); server.server_close(); thread.join(3)


@unittest.skipUnless(os.name == "posix", "actual native helper requires Linux")
class NativeTLSFixtureTests(unittest.TestCase):
    def test_bf920_actual_TLS_header_duplicates_failed_HTTP_and_exact_single_POST(self):
        name = os.environ.get("CODEX_NATIVE_HTTP_TEST_BIN")
        if not name or not Path(name).is_file(): self.skipTest("explicit bf920 binary required")
        helper = Path(name)
        self.assertEqual(probe.base.file_digest(helper), NATIVE_SHA)
        # Reuse only this pure loopback TCP relay. It never terminates TLS.
        from test_quality_route_lifecycle_probe import http_connect_proxy
        for status, rows in [(200, [("X-Codex-Turn-State", b"A" * 292), ("x-codex-turn-state", b"B" * 312), ("X-Codex-Turn-State", b"")]),
                             (429, [("X-Codex-Turn-State", "汉é".encode())]), (200, [])]:
            with self.subTest(status=status, count=len(rows)), tls_fixture(rows, status) as (target, ca, seen, sni), http_connect_proxy(target) as (proxy, connects):
                class TrustedFixtureHelper(probe._StateHelper):
                    def start(self):
                        # Test-only explicit CA injection; public start remains inherited
                        # and its PATH/LANG-only environment has not been patched.
                        self.directory = tempfile.TemporaryDirectory(prefix="state-ipc-")
                        os.chmod(self.directory.name, 0o700)
                        self.path = str(Path(self.directory.name) / "s")
                        config = {"protocol": 1, "unix_socket": self.path, "upstream": self.upstream, "ipc_token": self.ipc_token,
                                  "headers_timeout_ms": 5000, "idle_timeout_ms": 0, "http_client_lifecycle": "cli_request", "proxy": self.proxy_url}
                        self.process = subprocess.Popen([str(self.helper)], stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
                            close_fds=True, bufsize=0, env={"PATH": os.environ.get("PATH", "/usr/bin:/bin"), "LANG": "C.UTF-8", "CODEX_CA_CERTIFICATE": str(ca)})
                        self.record.update(process_started=True, pid=self.process.pid)
                        raw = json.dumps(config).encode()
                        probe.base.write_all(self.process.stdin.write, struct.pack("!I", len(raw)) + raw)
                        ready = probe.base._json(probe.base._ready_line(self.process.stdout, 5))
                        if ready.get("unix_socket") != self.path: raise AssertionError("fixture readiness mismatch")
                        actual = probe.base.file_digest(Path("/proc") / str(self.process.pid) / "exe")
                        if actual != self.digest: raise AssertionError("fixture executable mismatch")
                        self.record.update(runtime_executable_sha256=actual, readiness_verified=True)
                        self.capabilities = ready["capabilities"]
                        return self
                with mock.patch.object(probe, "_StateHelper", TrustedFixtureHelper):
                    report = probe.native_state_request(helper, NATIVE_SHA, BODY, captured(), TOKEN, WORKSPACE, timeout=20,
                        proxy_url="http://127.0.0.1:" + str(proxy[1]), application_identity=IDENTITY)
                self.assertEqual(report["protocol_success"], status == 200, report)
                observation = report["turn_state_observation"]
                self.assertTrue(observation["complete"], report)
                self.assertEqual(observation["count"], len(rows))
                self.assertEqual([v["value_sha256"] for v in observation["values"]], [probe.base.sha(value) for _, value in rows])
                self.assertEqual([v["value_bytes"] for v in observation["values"]], [len(value) for _, value in rows])
                self.assertEqual(len(seen), 1)
                self.assertEqual(len(connects), 1)
                self.assertEqual(sni, ["chatgpt.com"])
                self.assertEqual(seen[0]["body"], BODY)
                self.assertEqual(seen[0]["path"], "/backend-api/codex/responses")
                received = {key.lower(): value for key, value in seen[0]["headers"]}
                self.assertEqual(received["authorization"], "Bearer " + TOKEN)
                for key, encoded in captured()["headers"]:
                    self.assertEqual(received[key.lower()], base64.b64decode(encoded).decode())
                self.assertNotIn("x-codex-turn-state", received)
                self.assertEqual(report["native_dispatch_count"], 1)
                self.assertEqual(report["metadata_get_count"], 0)
                self.assertTrue(report["helper_reaped"] and report["local_socket_absent"])
                self.assertFalse(report["helper_forced_kill"])
                self.assertNotIn(TOKEN, json.dumps(report))


if __name__ == "__main__": unittest.main(verbosity=2)
