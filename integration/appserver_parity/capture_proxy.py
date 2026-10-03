#!/usr/bin/env python3
"""Private, process-scoped CONNECT capture for Codex request parity experiments.

Requires mitmproxy==12.2.3. TLS is terminated and re-originated: this observes
application semantics, not the original upstream TLS/HTTP2 fingerprint.
"""
from __future__ import annotations

import argparse
import base64
import hashlib
import importlib.metadata
import json
import os
from pathlib import Path
import socket
import stat
import struct
import subprocess
import sys
import time
import uuid
import zlib

VERSION = "12.2.3"
LIMIT = 64 << 20


def sha(raw):
    return hashlib.sha256(raw).hexdigest()


def private_dir(path, synthetic=False):
    path = Path(path).absolute()
    path.mkdir(mode=0o700, parents=True, exist_ok=True)
    info = path.lstat()
    if not stat.S_ISDIR(info.st_mode) or path.is_symlink():
        raise ValueError("capture_directory_invalid")
    if os.name == "posix":
        if stat.S_IMODE(info.st_mode) != 0o700 or info.st_uid != os.geteuid():
            raise ValueError("capture_directory_requires_0700_and_current_owner")
    elif not synthetic:
        raise ValueError("real_capture_requires_posix_private_directory")
    return path


def private_write(path, data):
    flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_NOFOLLOW", 0) | getattr(os, "O_BINARY", 0)
    with os.fdopen(os.open(path, flags, 0o600), "wb") as handle:
        handle.write(data)


def encode(value):
    return json.dumps(value, sort_keys=True, separators=(",", ":")).encode()


def allowed(host, patterns):
    host = host.lower().rstrip(".")
    return any(host == pattern or (pattern.startswith("*.") and host.endswith(pattern[1:]) and host != pattern[2:])
               for pattern in patterns)


class FrameIndex:
    """Index actual observed WebSocket frames, including control/continuations.

    Payload stays in the private byte stream; the public record has no payload.
    Offsets count decrypted WS bytes on exactly one side/direction of the MITM.
    """
    def __init__(self):
        self.buffer = bytearray()
        self.offset = 0
        self.sequence = 0

    def feed(self, data):
        self.buffer.extend(data)
        rows = []
        while len(self.buffer) >= 2:
            first, second = self.buffer[:2]
            size, start = second & 127, 2
            if size in (126, 127):
                width = 2 if size == 126 else 8
                if len(self.buffer) < start + width:
                    break
                size = int.from_bytes(self.buffer[start:start + width], "big")
                start += width
            if size > LIMIT:
                raise ValueError("websocket_frame_capture_limit")
            start += 4 if second & 128 else 0
            end = start + size
            if len(self.buffer) < end:
                break
            raw = bytes(self.buffer[:end])
            del self.buffer[:end]
            self.sequence += 1
            rows.append({"_raw": raw, "_payload_start": start, "sequence": self.sequence, "offset": self.offset, "length": end,
                         "payload_length": size, "opcode": first & 15, "fin": bool(first & 128),
                         "rsv1": bool(first & 64), "masked": bool(second & 128), "sha256": sha(raw)})
            self.offset += end
        return rows


class MessageIndex:
    """Passive RFC6455/permessage-deflate reassembly, with no control mutation."""
    def __init__(self, no_context_takeover=False):
        self.no_context_takeover = no_context_takeover
        self.decompressor = None
        self.opcode = None
        self.compressed = False
        self.buffer = bytearray()

    def feed(self, frame):
        raw, start = frame["_raw"], frame["_payload_start"]
        payload = raw[start:]
        if frame["masked"]:
            mask = raw[start - 4:start]
            payload = bytes(value ^ mask[i % 4] for i, value in enumerate(payload))
        opcode = frame["opcode"]
        if opcode >= 8:
            return None
        if opcode in (1, 2):
            if self.opcode is not None:
                raise ValueError("websocket_message_overlap")
            self.opcode, self.compressed = opcode, frame["rsv1"]
            if self.compressed and self.decompressor is None:
                self.decompressor = zlib.decompressobj(-15)
        elif opcode != 0 or self.opcode is None:
            raise ValueError("websocket_continuation_invalid")
        if self.compressed:
            payload = self.decompressor.decompress(payload, LIMIT + 1 - len(self.buffer))
        self.buffer.extend(payload)
        if frame["fin"] and self.compressed:
            self.buffer.extend(self.decompressor.decompress(b"\x00\x00\xff\xff", LIMIT + 1 - len(self.buffer)))
        if len(self.buffer) > LIMIT:
            raise ValueError("websocket_message_capture_limit")
        if not frame["fin"]:
            return None
        result = (self.opcode, bytes(self.buffer))
        self.opcode = None
        self.buffer.clear()
        if self.compressed and self.no_context_takeover:
            self.decompressor = None
        return result


class CaptureAddon:
    def __init__(self, config):
        if importlib.metadata.version("mitmproxy") != VERSION:
            raise ValueError("unsupported_mitmproxy_version")
        self.config = config
        self.root = private_dir(config["directory"], config.get("synthetic", False))
        self.raw = private_dir(self.root / "private", config.get("synthetic", False))
        self.streams, self.frames, self.messages = {}, {}, {}
        self.manifest = self.root / "manifest.jsonl"
        self.emit({"event": "capture_scope", "schema": 1, "mitmproxy": VERSION,
                   "tls_fingerprint_preserved": False, "http2_wire_preserved": False,
                   "response_entity_streaming": True, "ws_raw_frames_both_sides": True,
                   "ws_frame_forwarding": "raw_passthrough_after_handshake",
                   "all_process_egress_proven": False, "allowed_hosts": config["allowed_hosts"]})
        self._install_frame_hook()

    def emit(self, record):
        phase_file = self.config.get("phase_file")
        if phase_file:
            try:
                phase = json.loads(Path(phase_file).read_text())
                # The orchestrator writes bounded synthetic labels, never user text.
                record["phase"] = {key: value for key, value in phase.items() if key in ("route", "case")
                                   and isinstance(value, str) and len(value) <= 96
                                   and all(c.isalnum() or c in "_-./" for c in value)}
            except (OSError, ValueError):
                record["phase_unavailable"] = True
        flags = os.O_WRONLY | os.O_CREAT | os.O_APPEND | getattr(os, "O_NOFOLLOW", 0) | getattr(os, "O_BINARY", 0)
        with os.fdopen(os.open(self.manifest, flags, 0o600), "ab") as handle:
            handle.write(encode(dict(record, monotonic_ns=time.monotonic_ns())) + b"\n")

    def blob(self, label, raw):
        filename = uuid.uuid4().hex + "-" + label + ".bin"
        private_write(self.raw / filename, raw)
        return {"file": "private/" + filename, "length": len(raw), "sha256": sha(raw)}

    def headers(self, fields):
        return [{"name": name.decode("ascii", "replace").lower(), "length": len(value), "sha256": sha(value)}
                for name, value in fields]

    def secret_head(self, flow, phase):
        message = flow.request if phase == "request" else flow.response
        head = {"url": flow.request.url, "http_version": message.http_version,
                "headers": [[base64.b64encode(k).decode(), base64.b64encode(v).decode()]
                            for k, v in message.headers.fields],
                "client_sni": flow.client_conn.sni, "server_sni": flow.server_conn.sni}
        return self.blob(phase + "-head", encode(head))

    def stream(self, flow_id, label, data):
        key = (flow_id, label)
        if key not in self.streams:
            path = self.raw / (flow_id + "-" + label + ".bin")
            flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_NOFOLLOW", 0) | getattr(os, "O_BINARY", 0)
            self.streams[key] = [os.fdopen(os.open(path, flags, 0o600), "wb"), hashlib.sha256(), 0]
        handle, digest, size = self.streams[key]
        handle.write(data)
        handle.flush()
        digest.update(data)
        self.streams[key][2] += len(data)

    def finish_stream(self, flow_id, label, complete):
        item = self.streams.pop((flow_id, label), None)
        if item:
            handle, digest, size = item
            handle.close()
            self.emit({"event": "stream_end", "flow_id": flow_id, "stream": label,
                       "length": size, "sha256": digest.hexdigest(), "complete": complete,
                       "file": "private/" + flow_id + "-" + label + ".bin"})

    def _install_frame_hook(self):
        from mitmproxy.proxy import commands, events
        from mitmproxy.proxy.layers.websocket import WebsocketLayer, WebsocketEndHook
        original = WebsocketLayer.relay_messages
        owner = self

        def observed(layer, event):
            if isinstance(event, events.DataReceived):
                from_client = event.connection == layer.context.client
                incoming = "client_to_proxy" if from_client else "upstream_to_proxy"
                outgoing = "proxy_to_upstream" if from_client else "proxy_to_client"
                target = layer.context.server if from_client else layer.context.client
                extension = layer.flow.response.headers.get("sec-websocket-extensions", "")
                takeover_key = "client_no_context_takeover" if from_client else "server_no_context_takeover"
                owner.messages.setdefault((layer.flow.id, incoming), MessageIndex(takeover_key in extension))
                owner.ws_bytes(layer.flow.id, incoming, event.data, decode=True)
                owner.ws_bytes(layer.flow.id, outgoing, event.data)
                yield commands.SendData(target, event.data)
            elif isinstance(event, events.ConnectionClosed):
                layer.flow.websocket.timestamp_end = time.time()
                target = layer.context.server if event.connection == layer.context.client else layer.context.client
                yield commands.CloseConnection(target)
                yield WebsocketEndHook(layer.flow)
                layer._handle_event = layer.done
            else:
                raise ValueError("unexpected_websocket_injection_during_capture")

        WebsocketLayer.relay_messages = observed
        self.original_ws_hook = original

    def ws_bytes(self, flow_id, side, data, decode=False):
        self.stream(flow_id, "ws-" + side, data)
        index = self.frames.setdefault((flow_id, side), FrameIndex())
        for frame in index.feed(data):
            self.emit(dict({key: value for key, value in frame.items() if not key.startswith("_")},
                           event="websocket_frame", flow_id=flow_id, side=side))
            if decode:
                message = self.messages[(flow_id, side)].feed(frame)
                if message:
                    opcode, content = message
                    self.emit({"event": "websocket_message", "flow_id": flow_id, "from_client": side == "client_to_proxy",
                               "opcode": opcode, "body": self.blob("ws-message", content)})

    def http_connect(self, flow):
        from mitmproxy import http
        host = flow.request.host
        permit = allowed(host, self.config["allowed_hosts"])
        self.emit({"event": "connect", "host": host, "port": flow.request.port, "allowed": permit})
        if not permit:
            flow.response = http.Response.make(403, b"capture_target_not_allowed", {"Content-Type": "text/plain"})

    def server_connect(self, data):
        host, port = data.server.address
        permit = allowed(host, self.config["allowed_hosts"])
        self.emit({"event": "upstream_connect", "host": host, "port": port, "allowed": permit})
        if not permit:
            data.server.error = "capture_target_not_allowed"
            return
        # Only synthetic mode can map an unchanged authority/SNI onto loopback.
        routes = self.config.get("synthetic_routes", {})
        if self.config.get("synthetic") and host in routes:
            address, port = routes[host]
            if address not in ("127.0.0.1", "::1"):
                raise ValueError("synthetic_route_must_be_loopback")
            data.server.address = (address, port)

    def tls_established_server(self, data):
        self.emit({"event": "upstream_tls", "sni": data.conn.sni,
                   "alpn": (data.conn.alpn or b"").decode("ascii", "replace")})

    def request(self, flow):
        from mitmproxy import http
        if not allowed(flow.request.host, self.config["allowed_hosts"]):
            flow.response = http.Response.make(403, b"capture_target_not_allowed")
            return
        body = flow.request.raw_content or b""
        raw_info = self.blob("request", body)
        decoded = body
        encoding = flow.request.headers.get("content-encoding", "").lower()
        decode_status = "identity"
        if encoding == "zstd":
            import zstandard
            try:
                with zstandard.ZstdDecompressor().stream_reader(body) as reader:
                    decoded = reader.read(LIMIT + 1)
                if len(decoded) > LIMIT:
                    raise ValueError("decoded_limit")
                decode_status = "zstd_decoded"
            except Exception:
                decoded, decode_status = b"", "zstd_decode_failed"
        elif encoding:
            decode_status = "encoding_not_decoded"
        private_head = self.secret_head(flow, "request")
        self.emit({"event": "request", "flow_id": flow.id, "method": flow.request.method,
                   "host": flow.request.host, "path_sha256": sha(flow.request.path.encode()),
                   "http_version": flow.request.http_version, "headers": self.headers(flow.request.headers.fields),
                   "private_head": private_head, "body": raw_info, "decoded_body": self.blob("request-decoded", decoded),
                   "decode_status": decode_status})

    def responseheaders(self, flow):
        private_head = self.secret_head(flow, "response")
        self.emit({"event": "response_headers", "flow_id": flow.id, "status": flow.response.status_code,
                   "private_head": private_head, "headers": self.headers(flow.response.headers.fields), "http_version": flow.response.http_version})
        if flow.response.status_code == 101:
            return

        def chunks(data):
            self.stream(flow.id, "response-entity", data)
            self.emit({"event": "response_chunk", "flow_id": flow.id, "length": len(data), "sha256": sha(data)})
            return data

        flow.response.stream = chunks

    def response(self, flow):
        self.finish_stream(flow.id, "response-entity", True)

    def websocket_message(self, flow):
        message = flow.websocket.messages[-1]
        self.emit({"event": "websocket_message", "flow_id": flow.id, "from_client": message.from_client,
                   "opcode": int(message.type), "body": self.blob("ws-message", message.content)})

    def websocket_end(self, flow):
        self.emit({"event": "websocket_end", "flow_id": flow.id, "close_code": flow.websocket.close_code})
        for key in list(self.streams):
            if key[0] == flow.id:
                self.finish_stream(*key, complete=not bool(self.frames.get((key[0], key[1][3:]), FrameIndex()).buffer))

    def error(self, flow):
        self.emit({"event": "flow_error", "flow_id": flow.id, "code": "mitmproxy_flow_error"})
        self.finish_stream(flow.id, "response-entity", False)

    def running(self):
        from mitmproxy import ctx
        private_write(self.root / "ready.json", encode({"ready": True, "port": ctx.options.listen_port}))

    def done(self):
        for key in list(self.streams):
            self.finish_stream(*key, complete=False)
        self.emit({"event": "capture_stopped"})


class CaptureProxy:
    """Runner used by real-account orchestration; credentials never enter argv."""
    def __init__(self, directory, mitmdump, allowed_hosts, synthetic=False, synthetic_routes=None, upstream_ca=None, phase_file=None):
        self.root = private_dir(directory, synthetic)
        self.synthetic = synthetic
        self.config = {"directory": str(self.root), "allowed_hosts": list(allowed_hosts), "synthetic": synthetic,
                       "synthetic_routes": synthetic_routes or {}}
        if phase_file:
            self.config["phase_file"] = str(Path(phase_file).absolute())
        if not allowed_hosts or (synthetic_routes and not synthetic):
            raise ValueError("invalid_capture_allowlist_or_route")
        private_write(self.root / "config.json", encode(self.config))
        with socket.socket() as listener:
            listener.bind(("127.0.0.1", 0))
            self.port = listener.getsockname()[1]
        self.ca_dir = private_dir(self.root / "ca", synthetic)
        self.process = None
        self.command = [str(mitmdump), "-q", "--listen-host", "127.0.0.1", "--listen-port", str(self.port),
                        "--set", "confdir=" + str(self.ca_dir), "--set", "connection_strategy=lazy",
                        "--set", "ssl_insecure=false", "--set", "termlog_verbosity=error", "--set", "flow_detail=0",
                        "-s", str(Path(__file__).resolve())]
        if upstream_ca:
            self.command += ["--set", "ssl_verify_upstream_trusted_ca=" + str(Path(upstream_ca).resolve())]

    def start(self):
        environment = {k: v for k, v in os.environ.items() if k.upper() not in
                       ("HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY", "SSL_CERT_FILE", "SSL_CERT_DIR", "CODEX_CA_CERTIFICATE")}
        environment["CODEX_PARITY_CAPTURE_CONFIG"] = str(self.root / "config.json")
        self.process = subprocess.Popen(self.command, env=environment, stdin=subprocess.DEVNULL,
                                        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        deadline = time.monotonic() + 30
        while time.monotonic() < deadline:
            if self.process.poll() is not None:
                raise RuntimeError("capture_proxy_start_failed")
            if (self.root / "ready.json").exists():
                return self
            time.sleep(.05)
        self.close()
        raise TimeoutError("capture_proxy_start_timeout")

    @property
    def proxy_url(self):
        return "http://127.0.0.1:" + str(self.port)

    @property
    def ca_file(self):
        return self.ca_dir / "mitmproxy-ca-cert.pem"

    def client_environment(self, base):
        result = dict(base)
        for key in list(result):
            if key.upper() in ("HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY", "SSL_CERT_FILE", "SSL_CERT_DIR", "CODEX_CA_CERTIFICATE"):
                del result[key]
        result.update(HTTPS_PROXY=self.proxy_url, HTTP_PROXY=self.proxy_url, ALL_PROXY=self.proxy_url,
                      NO_PROXY="", CODEX_CA_CERTIFICATE=str(self.ca_file))
        return result

    def close(self):
        if self.process and self.process.poll() is None:
            self.process.terminate()
            try:
                self.process.wait(timeout=8)
            except subprocess.TimeoutExpired:
                self.process.kill()
                self.process.wait(timeout=5)

    def __enter__(self):
        return self.start()

    def __exit__(self, *_):
        self.close()


if os.environ.get("CODEX_PARITY_CAPTURE_CONFIG"):
    addons = [CaptureAddon(json.loads(Path(os.environ["CODEX_PARITY_CAPTURE_CONFIG"]).read_text()))]


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--directory", required=True)
    parser.add_argument("--mitmdump", required=True)
    parser.add_argument("--allow-host", action="append", required=True)
    args = parser.parse_args()
    with CaptureProxy(args.directory, args.mitmdump, args.allow_host) as proxy:
        print(json.dumps({"proxy": proxy.proxy_url, "ca_file": str(proxy.ca_file), "manifest": str(proxy.root / "manifest.jsonl")}))
        try:
            while True:
                time.sleep(1)
        except KeyboardInterrupt:
            pass
