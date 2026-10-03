#!/usr/bin/env python3
"""Real loopback HTTPS/SSE/WSS self-test; only generated synthetic credentials."""
from __future__ import annotations

import argparse
import base64
from datetime import datetime, timedelta, timezone
import hashlib
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import socket
import ssl
import threading
import time
import uuid

from capture_proxy import CaptureProxy, FrameIndex, private_dir, private_write, sha


def certificates(folder):
    from cryptography import x509
    from cryptography.hazmat.primitives import hashes, serialization
    from cryptography.hazmat.primitives.asymmetric import rsa
    from cryptography.x509.oid import NameOID
    key = rsa.generate_private_key(public_exponent=65537, key_size=2048)
    name = x509.Name([x509.NameAttribute(NameOID.COMMON_NAME, "capture.invalid")])
    now = datetime.now(timezone.utc)
    cert = (x509.CertificateBuilder().subject_name(name).issuer_name(name).public_key(key.public_key())
            .serial_number(x509.random_serial_number()).not_valid_before(now - timedelta(days=1))
            .not_valid_after(now + timedelta(days=1)).add_extension(x509.BasicConstraints(ca=True, path_length=None), True)
            .add_extension(x509.SubjectAlternativeName([x509.DNSName("capture.invalid")]), False).sign(key, hashes.SHA256()))
    private_write(folder / "upstream.pem", cert.public_bytes(serialization.Encoding.PEM))
    private_write(folder / "upstream-key.pem", key.private_bytes(serialization.Encoding.PEM, serialization.PrivateFormat.PKCS8, serialization.NoEncryption()))
    return folder / "upstream.pem", folder / "upstream-key.pem"


def read_header(conn):
    raw = bytearray()
    while not raw.endswith(b"\r\n\r\n"):
        part = conn.recv(1)
        if not part or len(raw) > 65536:
            raise RuntimeError("synthetic_invalid_header")
        raw.extend(part)
    return bytes(raw)


def tunnel(proxy, host="capture.invalid", tls=True):
    conn = socket.create_connection(("127.0.0.1", proxy.port), timeout=10)
    conn.sendall((f"CONNECT {host}:443 HTTP/1.1\r\nHost: {host}:443\r\n\r\n").encode())
    response = read_header(conn)
    if not tls:
        return conn, response
    assert b"200" in response.split(b"\r\n")[0]
    context = ssl.create_default_context(cafile=str(proxy.ca_file))
    context.set_alpn_protocols(["http/1.1"])
    return context.wrap_socket(conn, server_hostname=host)


def run(directory, mitmdump):
    import wsproto
    from wsproto.events import CloseConnection, Ping, Pong, TextMessage
    from wsproto.extensions import PerMessageDeflate
    import zstandard

    directory = private_dir(directory, True)
    cert, key = certificates(directory)
    observations = {"sni": [], "hosts": [], "requests": [], "ws_messages": []}
    payload = b'{"model":"synthetic","input":"synthetic-request","stream":true}'
    response_bytes = b'data: {"type":"response.output_text.delta","delta":"synthetic"}\n\ndata: {"type":"response.completed"}\n\n'
    ws_text = "synthetic-fragmented-message-" * 12
    ws_reply = "synthetic-server-message-" * 15

    class Handler(BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.1"

        def log_message(self, *args):
            pass

        def do_POST(self):
            raw = self.rfile.read(int(self.headers["Content-Length"]))
            observations["requests"].append(raw)
            observations["hosts"].append(self.headers["Host"])
            assert self.headers["Authorization"] == "Bearer SYNTHETIC_CAPTURE_TOKEN"
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.send_header("Transfer-Encoding", "chunked")
            self.send_header("Set-Cookie", "synthetic-cookie=private-value")
            self.end_headers()
            for chunk in response_bytes.splitlines(keepends=True):
                self.wfile.write((hex(len(chunk))[2:] + "\r\n").encode() + chunk + b"\r\n")
                self.wfile.flush()
                time.sleep(.02)
            self.wfile.write(b"0\r\n\r\n")
            self.wfile.flush()

        def do_GET(self):
            observations["hosts"].append(self.headers["Host"])
            accept = base64.b64encode(hashlib.sha1((self.headers["Sec-WebSocket-Key"] + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11").encode()).digest()).decode()
            self.send_response(101)
            self.send_header("Upgrade", "websocket")
            self.send_header("Connection", "Upgrade")
            self.send_header("Sec-WebSocket-Accept", accept)
            self.send_header("Sec-WebSocket-Extensions", "permessage-deflate")
            self.end_headers()
            extension = PerMessageDeflate()
            extension.accept("permessage-deflate")
            server = wsproto.Connection(wsproto.ConnectionType.SERVER, [extension])
            frames = FrameIndex()
            message = ""
            while True:
                raw = self.connection.recv(65536)
                if not raw:
                    break
                # wsproto 1.3.2 resets deflate state for an interleaved Ping.
                # The fixture handles Ping directly so its decoder cannot mask
                # or introduce the proxy regression this case is checking.
                for frame in frames.feed(raw):
                    if frame["opcode"] == 9:
                        start = frame["_payload_start"]
                        data = frame["_raw"][start:]
                        mask = frame["_raw"][start - 4:start]
                        data = bytes(value ^ mask[i % 4] for i, value in enumerate(data))
                        self.connection.sendall(server.send(Pong(payload=data)))
                    else:
                        server.receive_data(frame["_raw"])
                for event in server.events():
                    if isinstance(event, TextMessage):
                        message += event.data
                        if event.message_finished:
                            observations["ws_messages"].append(message)
                            self.connection.sendall(server.send(TextMessage(data=ws_reply)))
                    elif isinstance(event, Ping):
                        self.connection.sendall(server.send(event.response()))
                    elif isinstance(event, CloseConnection):
                        self.connection.sendall(server.send(event.response()))
                        self.close_connection = True
                        return

    server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    context.load_cert_chain(cert, key)
    context.set_alpn_protocols(["http/1.1"])
    context.set_servername_callback(lambda conn, name, context: observations["sni"].append(name))
    server.socket = context.wrap_socket(server.socket, server_side=True)
    worker = threading.Thread(target=server.serve_forever, daemon=True)
    worker.start()
    capture_dir = directory / "capture"
    compressed = zstandard.ZstdCompressor(level=3).compress(payload)
    checks = {}
    try:
        with CaptureProxy(capture_dir, mitmdump, ["capture.invalid"], synthetic=True,
                          synthetic_routes={"capture.invalid": ["127.0.0.1", server.server_port]}, upstream_ca=cert) as proxy:
            env = proxy.client_environment({"PATH": os.environ.get("PATH", ""), "HTTPS_PROXY": "http://must-disappear.invalid", "NO_PROXY": "*"})
            checks["isolated_process_environment"] = env["HTTPS_PROXY"] == proxy.proxy_url and env["NO_PROXY"] == "" and env["CODEX_CA_CERTIFICATE"] == str(proxy.ca_file)
            with tunnel(proxy) as client:
                client.sendall(b"POST /backend-api/codex/responses HTTP/1.1\r\nHost: capture.invalid\r\nAuthorization: Bearer SYNTHETIC_CAPTURE_TOKEN\r\nContent-Encoding: zstd\r\nContent-Type: application/json\r\nContent-Length: " + str(len(compressed)).encode() + b"\r\nConnection: close\r\n\r\n" + compressed)
                header = read_header(client)
                received = bytearray()
                while True:
                    chunk = client.recv(4096)
                    if not chunk:
                        break
                    received.extend(chunk)
                checks["https_response_received"] = b"200" in header and b"synthetic" in received

            extension = PerMessageDeflate()
            extension.finalize("permessage-deflate")
            websocket = wsproto.Connection(wsproto.ConnectionType.CLIENT, [extension])
            with tunnel(proxy) as client:
                client.sendall(b"GET /backend-api/codex/responses HTTP/1.1\r\nHost: capture.invalid\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: c3ludGhldGljLXRlc3QhIQ==\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Extensions: permessage-deflate\r\n\r\n")
                assert b"101" in read_header(client)
                client.sendall(websocket.send(TextMessage(data=ws_text[:77], message_finished=False)))
                client.sendall(websocket.send(Ping(payload=b"synthetic-ping")))
                client.sendall(websocket.send(TextMessage(data=ws_text[77:], message_finished=True)))
                reply, pong = "", False
                while reply != ws_reply or not pong:
                    received_ws = client.recv(65536)
                    if not received_ws:
                        raise RuntimeError("synthetic_websocket_unexpected_eof")
                    websocket.receive_data(received_ws)
                    for event in websocket.events():
                        if isinstance(event, TextMessage):
                            reply += event.data
                        if isinstance(event, Pong):
                            pong = True
                checks["wss_messages_and_ping_pong"] = reply == ws_reply and pong
                client.sendall(websocket.send(CloseConnection(code=1000, reason="synthetic-end")))
                websocket.receive_data(client.recv(65536))
                list(websocket.events())
            blocked, reply = tunnel(proxy, "not-allowed.invalid", False)
            blocked.close()
            checks["unlisted_host_rejected"] = b"403" in reply
            time.sleep(.3)
    finally:
        server.shutdown()
        server.server_close()
        worker.join(timeout=3)

    manifest_bytes = (capture_dir / "manifest.jsonl").read_bytes()
    records = [json.loads(line) for line in manifest_bytes.splitlines()]
    request = next(x for x in records if x["event"] == "request" and x["method"] == "POST")
    checks["zstd_raw_hash"] = request["body"]["sha256"] == sha(compressed)
    checks["zstd_decoded_hash"] = request["decode_status"] == "zstd_decoded" and request["decoded_body"]["sha256"] == sha(payload)
    response = next(x for x in records if x["event"] == "stream_end" and x["stream"] == "response-entity")
    checks["response_entity_retained_exactly"] = response["sha256"] == sha(response_bytes) and response["complete"]
    frames = [x for x in records if x["event"] == "websocket_frame"]
    checks["all_four_ws_directions"] = {x["side"] for x in frames} == {"client_to_proxy", "proxy_to_upstream", "upstream_to_proxy", "proxy_to_client"}
    checks["ws_control_and_continuation_frames"] = {0, 1, 8, 9, 10}.issubset({x["opcode"] for x in frames})
    checks["ws_compressed_frames"] = any(x["rsv1"] for x in frames)
    complete_streams = {x["stream"]: x for x in records if x["event"] == "stream_end" and x["stream"].startswith("ws-")}
    checks["ws_frames_forwarded_byte_identically"] = all(complete_streams[one]["sha256"] == complete_streams[two]["sha256"]
        for one, two in (("ws-client_to_proxy", "ws-proxy_to_upstream"), ("ws-upstream_to_proxy", "ws-proxy_to_client")))
    client_frames = [x for x in frames if x["side"] == "client_to_proxy"]
    checks["ping_inside_compressed_fragmented_message"] = [x["opcode"] for x in client_frames[:3]] == [1, 9, 0]
    messages = [x for x in records if x["event"] == "websocket_message"]
    checks["ws_reassembled_hashes"] = {sha(ws_text.encode()), sha(ws_reply.encode())}.issubset({x["body"]["sha256"] for x in messages})
    checks["upstream_host_and_sni_preserved"] = set(observations["sni"]) == {"capture.invalid"} and set(observations["hosts"]) == {"capture.invalid"}
    checks["upstream_bytes_preserved"] = observations["requests"] == [compressed] and observations["ws_messages"] == [ws_text]
    checks["manifest_no_synthetic_secrets_or_bodies"] = all(value not in manifest_bytes for value in (b"SYNTHETIC_CAPTURE_TOKEN", b"private-value", b"synthetic-request", ws_text.encode(), ws_reply.encode()))
    if os.name == "posix":
        checks["private_directory_mode"] = (capture_dir.stat().st_mode & 0o777) == 0o700
        checks["private_files_mode"] = all((p.stat().st_mode & 0o777) == 0o600 for p in (capture_dir / "private").iterdir())
    result = {"schema": 1, "synthetic_only": True, "public_dns_used": False, "live_model_inference": False,
              "passed": all(checks.values()), "checks": checks, "websocket_frames": len(frames),
              "environment_coverage": {"manual_https_wss_client": "executed", "official_codex_exec": "not_executed",
                                       "official_app_server": "not_executed", "native_helper_explicit_proxy": "not_executed"}}
    private_write(directory / "selftest-summary.json", json.dumps(result, indent=2).encode())
    return result


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--directory", required=True)
    parser.add_argument("--mitmdump", required=True)
    args = parser.parse_args()
    print(json.dumps(run(Path(args.directory), args.mitmdump), indent=2))
