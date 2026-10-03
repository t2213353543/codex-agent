"""Additional HTTP IPC/TLS regression; loopback fixtures and synthetic data only.

Run: python3 tests/http_boundaries.py --binary target/debug/codex-native-transport
"""
from __future__ import annotations

import argparse
import json
from pathlib import Path
import shutil
import socket
import ssl
import struct
import subprocess
import tempfile
import threading
import time
import unittest

import loopback


class HttpBoundaryTests(unittest.TestCase):
    def setUp(self):
        self.server = loopback.Fixture()
        self.helper = loopback.Helper(self.server.origin)

    def tearDown(self):
        self.helper.close()
        self.server.close()

    def envelope(self, **changes):
        head = dict(protocol=1, ipc_token=self.helper.token, method="POST",
                    path="/echo", headers=[], body_length=0)
        head.update(changes)
        return head

    def test_signed_and_non_decimal_content_length_never_reach_upstream(self):
        for value in ("+0", "-0", "0,0", "0 ", " 0", "184467440737095516160"):
            with self.subTest(value=value):
                self.assertEqual(self.helper.collect("/echo", headers=[("Content-Length", value)])[2],
                                 "content_length_invalid")
        self.assertEqual(self.server.requests, [])
        self.assertEqual(self.helper.collect("/echo", b"raw", headers=[("Content-Length", "003")])[1:],
                         (b"raw", None))

    def test_total_budget_expires_during_incomplete_ipc_upload(self):
        started = time.monotonic()
        with self.helper.raw_request(self.envelope(body_length=1024, total_timeout_ms=120), b"partial") as stream:
            stream.settimeout(1)
            self.assertEqual(loopback.read_frame(stream), (loopback.ERROR, b"total_timeout"))
            self.assertEqual(stream.recv(1), b"")
        self.assertLess(time.monotonic() - started, 0.8)
        self.assertEqual(self.server.requests, [])

    def test_upload_time_is_not_refunded_when_waiting_for_headers(self):
        started = time.monotonic()
        with self.helper.raw_request(self.envelope(path="/headers_wait?upload-budget", body_length=3,
                                                   total_timeout_ms=600)) as stream:
            time.sleep(0.35)
            stream.sendall(b"raw")
            self.assertTrue(self.server.event("started", "/headers_wait?upload-budget").wait(0.2))
            self.assertEqual(loopback.read_frame(stream), (loopback.ERROR, b"total_timeout"))
        self.assertLess(time.monotonic() - started, 0.85)
        self.assertTrue(self.server.event("closed", "/headers_wait?upload-budget").wait(0.5))

    def test_truncated_body_is_never_submitted_upstream(self):
        with self.helper.raw_request(self.envelope(body_length=1024), b"partial") as stream:
            stream.shutdown(socket.SHUT_WR)
            self.assertEqual(loopback.read_frame(stream), (loopback.ERROR, b"ipc_read"))
        self.assertEqual(self.server.requests, [])

    def test_extra_body_byte_is_cancellation_before_exchange(self):
        with self.helper.raw_request(self.envelope(body_length=3), b"raw\x03") as stream:
            self.assertEqual(stream.recv(1), b"")
        self.assertEqual(self.server.requests, [])


class TlsBoundaryTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        openssl = shutil.which("openssl")
        if not openssl:
            raise RuntimeError("openssl is required for synthetic TLS regression")
        cls.temp = tempfile.TemporaryDirectory(prefix="native-http-boundaries-")
        cls.addClassCleanup(cls.temp.cleanup)
        cls.folder = Path(cls.temp.name)
        cls.root = cls.folder / "root.pem"
        root_key = cls.folder / "root.key"
        cls.leaf = cls.folder / "leaf.pem"
        cls.key = cls.folder / "leaf.key"
        csr = cls.folder / "leaf.csr"
        extensions = cls.folder / "leaf.ext"
        extensions.write_text("basicConstraints=critical,CA:FALSE\nkeyUsage=critical,digitalSignature,keyEncipherment\n"
                              "extendedKeyUsage=serverAuth\nsubjectAltName=DNS:localhost\n")
        commands = [
            [openssl, "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-sha256", "-days", "2",
             "-subj", "/CN=Synthetic HTTP Boundary Root", "-addext", "basicConstraints=critical,CA:TRUE",
             "-addext", "keyUsage=critical,keyCertSign,cRLSign", "-keyout", str(root_key), "-out", str(cls.root)],
            [openssl, "req", "-new", "-newkey", "rsa:2048", "-nodes", "-sha256", "-subj", "/CN=localhost",
             "-keyout", str(cls.key), "-out", str(csr)],
            [openssl, "x509", "-req", "-in", str(csr), "-CA", str(cls.root), "-CAkey", str(root_key),
             "-CAcreateserial", "-days", "2", "-sha256", "-extfile", str(extensions), "-out", str(cls.leaf)],
        ]
        for command in commands:
            result = subprocess.run(command, capture_output=True, timeout=15)
            if result.returncode:
                raise RuntimeError("synthetic TLS certificate generation failed")

    def context(self, protocols):
        context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        context.load_cert_chain(self.leaf, self.key)
        context.set_alpn_protocols(protocols)
        return context

    def test_dns_sni_and_hostname_validation_with_custom_ca(self):
        observed_sni = []
        context = self.context(["http/1.1"])
        context.set_servername_callback(lambda _socket, name, _context: observed_sni.append(name))
        server = loopback.Fixture(tls_context=context)
        self.addCleanup(server.close)
        trusted = loopback.Helper(f"https://localhost:{server.server_port}",
                                 environment={"CODEX_CA_CERTIFICATE": str(self.root)})
        try:
            head, body, error = trusted.collect("/echo", b"dns-verified")
            self.assertEqual((head["version"], body, error), ("HTTP/1.1", b"dns-verified", None))
            self.assertEqual(observed_sni, ["localhost"])
            self.assertEqual(server.requests[-1]["alpn"], "http/1.1")
        finally:
            trusted.close()
        mismatch = loopback.Helper(server.origin, environment={"CODEX_CA_CERTIFICATE": str(self.root)})
        try:
            self.assertEqual(mismatch.collect("/echo", b"wrong-host"), (None, b"", "upstream_connect"))
            self.assertEqual(len(server.requests), 1)
            self.assertEqual(observed_sni[-1], None, "IP literals must not become DNS SNI")
        finally:
            mismatch.close()

    def test_custom_ca_precedence_and_empty_override_fallback(self):
        server = loopback.Fixture(tls_context=self.context(["http/1.1"]))
        self.addCleanup(server.close)
        for environment in (
            {"CODEX_CA_CERTIFICATE": str(self.root), "SSL_CERT_FILE": str(self.folder / "missing.pem")},
            {"CODEX_CA_CERTIFICATE": "", "SSL_CERT_FILE": str(self.root)},
        ):
            helper = loopback.Helper(f"https://localhost:{server.server_port}", environment=environment)
            try:
                self.assertEqual(helper.collect("/echo", b"ca-priority")[1:], (b"ca-priority", None))
            finally:
                helper.close()

    def test_rustls_alpn_negotiates_and_exchanges_real_http2_frames(self):
        context = self.context(["h2", "http/1.1"])
        observed = {}
        errors = []
        listener = socket.socket()
        listener.settimeout(3)
        listener.bind(("127.0.0.1", 0))
        listener.listen(1)
        self.addCleanup(listener.close)

        def frame(kind, flags, stream, payload=b""):
            return len(payload).to_bytes(3, "big") + bytes([kind, flags]) + struct.pack("!I", stream) + payload

        def serve():
            try:
                with listener.accept()[0] as connection, context.wrap_socket(connection, server_side=True) as stream:
                    stream.settimeout(3)
                    observed["alpn"] = stream.selected_alpn_protocol()
                    observed["preface"] = loopback.read_exact(stream, 24)
                    stream.sendall(frame(4, 0, 0))  # Server SETTINGS.
                    while True:
                        header = loopback.read_exact(stream, 9)
                        length = int.from_bytes(header[:3], "big")
                        kind, flags = header[3:5]
                        stream_id = int.from_bytes(header[5:], "big") & 0x7fffffff
                        if length > 1024 * 1024:
                            raise AssertionError("unexpected synthetic HTTP/2 frame size")
                        loopback.read_exact(stream, length)
                        if kind == 4 and flags == 0:
                            stream.sendall(frame(4, 1, 0))  # SETTINGS ACK.
                        if kind in (0, 1) and stream_id and flags & 1:
                            # HPACK static entry 8 is ':status: 200'.
                            stream.sendall(frame(1, 5, stream_id, b"\x88"))
                            break
            except Exception as error:
                errors.append(type(error).__name__)

        worker = threading.Thread(target=serve, daemon=True)
        worker.start()
        helper = loopback.Helper(f"https://localhost:{listener.getsockname()[1]}",
                                 environment={"CODEX_CA_CERTIFICATE": str(self.root)})
        try:
            head, body, error = helper.collect("/responses", b"synthetic-http2")
            self.assertEqual(error, None)
            self.assertIsNotNone(head)
            self.assertEqual((head["status"], head["version"], body), (200, "HTTP/2", b""))
        finally:
            helper.close()
            worker.join(timeout=4)
        self.assertFalse(worker.is_alive())
        self.assertEqual(errors, [])
        self.assertEqual(observed, {"alpn": "h2", "preface": b"PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"})


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--binary", required=True, type=Path)
    args, unittest_args = parser.parse_known_args()
    loopback.BINARY = args.binary.resolve(strict=True)
    unittest.main(argv=[__file__, *unittest_args], verbosity=2)
