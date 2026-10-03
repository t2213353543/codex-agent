"""Controlled HTTP/2 settings and TLS resumption observations, without payload logs."""
from __future__ import annotations

import argparse
import hashlib
import importlib.metadata
import json
import os
from pathlib import Path
import socket
import ssl
import struct
import subprocess
import tempfile
import threading
import time

import h2.config
import h2.connection
import h2.events
from hyperframe.frame import Frame, SettingsFrame, WindowUpdateFrame, PriorityFrame, HeadersFrame

import loopback


FINAL = "CONTROLLED_FINGERPRINT_COMPLETE"


def sha(path):
    return hashlib.sha256(Path(path).read_bytes()).hexdigest()


def certificate(folder):
    root, key, leaf, leafkey, csr = [folder / name for name in ("root.pem", "root.key", "leaf.pem", "leaf.key", "leaf.csr")]
    ext = folder / "leaf.ext"
    ext.write_text("basicConstraints=critical,CA:FALSE\nkeyUsage=critical,digitalSignature,keyEncipherment\nextendedKeyUsage=serverAuth\nsubjectAltName=IP:127.0.0.1,DNS:localhost\n")
    commands = [
        ["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "2", "-subj", "/CN=Controlled TLS Root", "-addext", "basicConstraints=critical,CA:TRUE", "-keyout", str(key), "-out", str(root)],
        ["openssl", "req", "-new", "-newkey", "rsa:2048", "-nodes", "-subj", "/CN=localhost", "-keyout", str(leafkey), "-out", str(csr)],
        ["openssl", "x509", "-req", "-in", str(csr), "-CA", str(root), "-CAkey", str(key), "-CAcreateserial", "-days", "2", "-extfile", str(ext), "-out", str(leaf)],
    ]
    for command in commands:
        if subprocess.run(command, capture_output=True).returncode:
            raise RuntimeError("certificate_fixture_failed")
    hashname = subprocess.check_output(["openssl", "x509", "-in", str(root), "-noout", "-hash"], text=True).strip()
    trust = folder / "native-trust"
    trust.mkdir()
    (trust / (hashname + ".0")).write_bytes(root.read_bytes())
    return root, leaf, leafkey, trust


def hello_metadata(data):
    # OpenSSL supplies a complete decoded handshake message; only extension
    # identifiers are retained, never randoms/session IDs/key shares/PSK values.
    view = memoryview(data)
    if len(view) < 39 or view[0] != 1:
        return {"parse_error": "not_client_hello"}
    position = 4 + 2 + 32
    position += 1 + view[position]
    length = int.from_bytes(view[position:position + 2], "big")
    position += 2 + length
    position += 1 + view[position]
    length = int.from_bytes(view[position:position + 2], "big")
    position += 2
    end = position + length
    ids = []
    while position + 4 <= end <= len(view):
        kind, size = struct.unpack("!HH", view[position:position + 4])
        ids.append(kind)
        position += 4 + size
    return {"extension_ids": ids, "pre_shared_key_extension_present": 41 in ids,
            "psk_key_exchange_modes_present": 45 in ids, "session_ticket_extension_present": 35 in ids}


class FrameObserver:
    def __init__(self, record):
        self.record = record
        self.buffer = bytearray()
        self.preface = False

    def feed(self, data):
        self.buffer.extend(data)
        if not self.preface:
            if len(self.buffer) < 24:
                return
            if bytes(self.buffer[:24]) != b"PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n":
                raise RuntimeError("invalid_h2_preface")
            del self.buffer[:24]
            self.preface = True
        while len(self.buffer) >= 9:
            frame, length = Frame.parse_frame_header(memoryview(bytes(self.buffer[:9])))
            if len(self.buffer) < 9 + length:
                return
            frame.parse_body(memoryview(bytes(self.buffer[9:9 + length])))
            del self.buffer[:9 + length]
            if isinstance(frame, SettingsFrame) and "ACK" not in frame.flags:
                self.record["settings_frames"].append([[int(key), value] for key, value in frame.settings.items()])
            elif isinstance(frame, WindowUpdateFrame):
                self.record["window_updates"].append({"stream_id": frame.stream_id, "increment": frame.window_increment})
            elif isinstance(frame, PriorityFrame) or isinstance(frame, HeadersFrame) and "PRIORITY" in frame.flags:
                self.record["priorities"].append({"stream_id": frame.stream_id, "depends_on": frame.depends_on,
                                                  "weight": frame.stream_weight, "exclusive": frame.exclusive})


def tool_name(tools, wanted, namespace=""):
    for item in tools:
        if item.get("type") == "namespace":
            found = tool_name(item.get("tools", []), wanted, item.get("name", ""))
            if found:
                return found
        if item.get("name") == wanted:
            return wanted, namespace
    return None


class Server:
    def __init__(self, leaf, key, role, requests=4, port=0):
        self.role = role
        self.expected = requests
        self.records = []
        self.requests = []
        self.errors = []
        self.stopped = threading.Event()
        self.context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        self.context.load_cert_chain(leaf, key)
        self.context.minimum_version = ssl.TLSVersion.TLSv1_2
        self.context.set_alpn_protocols(["h2", "http/1.1"])
        self.context.num_tickets = 2
        self.pending_hello = {}
        self.tickets_sent = {}
        self.context._msg_callback = self.tls_message
        self.socket = socket.socket()
        self.socket.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        self.socket.bind(("127.0.0.1", port))
        self.socket.listen(16)
        self.socket.settimeout(0.2)
        self.origin = f"https://127.0.0.1:{self.socket.getsockname()[1]}"
        self.thread = threading.Thread(target=self.serve, daemon=True)
        self.thread.start()

    def tls_message(self, connection, direction, version, content_type, message_type, data):
        if direction == "read" and int(content_type) == 22 and int(message_type) == 1:
            self.pending_hello[id(connection)] = hello_metadata(data)
        if direction == "write" and int(content_type) == 22 and int(message_type) == 4:
            self.tickets_sent[id(connection)] = self.tickets_sent.get(id(connection), 0) + 1

    def response(self, headers, body):
        payload = json.loads(body)
        count = len(self.requests) + 1
        tool_outputs = sum(item.get("type") == "function_call_output" for item in payload.get("input", []) if isinstance(item, dict))
        self.requests.append({"request_number": count, "header_names_in_order": [key.lower() for key, _ in headers],
                              "tool_output_items": tool_outputs, "body_bytes": len(body)})
        if self.role == "official" and count < self.expected:
            selected = tool_name(payload.get("tools", []), "update_plan")
            if selected:
                name, namespace = selected
                arguments = {"plan": [{"step": "Observe local transport", "status": "completed"}]}
            else:
                selected = tool_name(payload.get("tools", []), "exec_command") or tool_name(payload.get("tools", []), "shell_command")
                if not selected:
                    raise RuntimeError("fixture_tool_missing")
                name, namespace = selected
                arguments = {"cmd" if name == "exec_command" else "command": "echo CONTROLLED_FINGERPRINT_TOOL", "login": False}
            item = {"type": "function_call", "id": f"fixture-item-{count}", "call_id": f"fixture-call-{count}",
                    "name": name, "arguments": json.dumps(arguments)}
            if namespace:
                item["namespace"] = namespace
        else:
            item = {"type": "message", "id": "fixture-final", "role": "assistant", "content": [{"type": "output_text", "text": FINAL}]}
        events = [{"type": "response.created", "response": {"id": f"fixture-{count}"}},
                  {"type": "response.output_item.done", "output_index": 0, "item": item},
                  {"type": "response.completed", "response": {"id": f"fixture-{count}", "status": "completed", "output": [item],
                    "usage": {"input_tokens": 20, "output_tokens": 1, "total_tokens": 21}}}]
        return b"".join(("event: " + event["type"] + "\ndata: " + json.dumps(event) + "\n\n").encode() for event in events)

    def serve_h1(self, stream, record):
        buffer = bytearray()
        while not buffer.endswith(b"\r\n\r\n"):
            chunk = stream.recv(1)
            if not chunk:
                raise EOFError()
            buffer.extend(chunk)
            if len(buffer) > 65536:
                raise RuntimeError("request_header_limit")
        lines = bytes(buffer).decode("latin-1").split("\r\n")
        headers = [tuple(line.split(":", 1)) for line in lines[1:] if ":" in line]
        length = int(next((value for key, value in headers if key.lower() == "content-length"), "0"))
        if length > 32 << 20:
            raise RuntimeError("request_body_limit")
        body = bytearray()
        while len(body) < length:
            chunk = stream.recv(length - len(body))
            if not chunk:
                raise EOFError()
            body.extend(chunk)
        result = self.response(headers, body)
        stream.sendall(b"HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nContent-Length: " + str(len(result)).encode() + b"\r\nConnection: close\r\n\r\n" + result)
        record["forced_new_connection"] = "HTTP/1.1 Connection: close"

    def serve_h2(self, stream, record):
        connection = h2.connection.H2Connection(config=h2.config.H2Configuration(client_side=False, header_encoding="utf-8"))
        connection.initiate_connection()
        stream.sendall(connection.data_to_send())
        observer = FrameObserver(record)
        bodies, headers = {}, {}
        while True:
            data = stream.recv(65536)
            if not data:
                return
            observer.feed(data)
            for event in connection.receive_data(data):
                if isinstance(event, h2.events.RequestReceived):
                    headers[event.stream_id] = event.headers
                    bodies[event.stream_id] = bytearray()
                elif isinstance(event, h2.events.DataReceived):
                    bodies[event.stream_id].extend(event.data)
                    if len(bodies[event.stream_id]) > 32 << 20:
                        raise RuntimeError("request_body_limit")
                    connection.acknowledge_received_data(event.flow_controlled_length, event.stream_id)
                elif isinstance(event, h2.events.StreamEnded):
                    result = self.response(headers[event.stream_id], bodies[event.stream_id])
                    connection.send_headers(event.stream_id, [(":status", "200"), ("content-type", "text/event-stream"), ("content-length", str(len(result)))])
                    connection.send_data(event.stream_id, result, end_stream=True)
                    connection.close_connection(error_code=0, last_stream_id=event.stream_id)
                    stream.sendall(connection.data_to_send())
                    record["forced_new_connection"] = "HTTP/2 GOAWAY after completed stream"
                    return
            stream.sendall(connection.data_to_send())

    def serve(self):
        while not self.stopped.is_set():
            try:
                raw, _ = self.socket.accept()
            except TimeoutError:
                continue
            except OSError:
                return
            record = {"connection": len(self.records) + 1, "settings_frames": [], "window_updates": [], "priorities": []}
            try:
                raw.settimeout(15)
                with self.context.wrap_socket(raw, server_side=True) as stream:
                    record.update(tls_version=stream.version(), alpn=stream.selected_alpn_protocol(), session_reused=stream.session_reused,
                                  client_hello=self.pending_hello.pop(id(stream), {}))
                    self.records.append(record)
                    if stream.selected_alpn_protocol() == "h2":
                        self.serve_h2(stream, record)
                    else:
                        self.serve_h1(stream, record)
                    record["server_new_session_tickets_sent"] = self.tickets_sent.pop(id(stream), 0)
                    time.sleep(0.05)
            except Exception as error:
                self.errors.append(type(error).__name__)
                raw.close()

    def close(self):
        self.stopped.set()
        self.socket.close()
        self.thread.join(timeout=18)
        if self.thread.is_alive():
            raise RuntimeError("server_did_not_stop")


def run_client(role, mode, binary, server, folder, root, trust):
    environment = {"SSL_CERT_FILE": str(root)} if mode == "custom_ca" else {"SSL_CERT_DIR": str(trust)}
    if role.startswith("helper"):
        loopback.BINARY = binary
        private = folder / "ipc"
        private.mkdir(mode=0o700)
        lifecycle = "pooled" if role == "helper_pooled_control" else "cli_request"
        helper = loopback.Helper(server.origin, unix_socket=str(private / "s"), environment=environment, headers_timeout_ms=10000, idle_timeout_ms=10000,
                                http_client_lifecycle=lifecycle)
        try:
            if helper.ready.get("http_client_lifecycle") != lifecycle:
                raise RuntimeError("helper_lifecycle_negotiation_failed")
            for _ in range(4):
                head, body, error = helper.collect("/v1/responses", b'{"input":[],"tools":[]}', headers=[("content-type", "application/json"), ("accept", "text/event-stream"), ("authorization", "Bearer synthetic")])
                if error or head["status"] != 200 or FINAL.encode() not in body:
                    raise RuntimeError("helper_response_failed")
        finally:
            helper.close()
        return {"completed": True, "same_process_connections": True, "http_client_lifecycle": lifecycle}
    home, workspace = folder / "home", folder / "workspace"
    home.mkdir()
    workspace.mkdir()
    (home / "config.toml").write_text(f'''model = "gpt-5.4"
model_provider = "fingerprint_fixture"
approval_policy = "never"
sandbox_mode = "read-only"
web_search = "disabled"
[model_providers.fingerprint_fixture]
name = "Controlled transport fixture"
base_url = "{server.origin}/v1"
env_key = "CODEX_FINGERPRINT_FIXTURE_KEY"
wire_api = "responses"
supports_websockets = false
request_max_retries = 0
stream_max_retries = 0
stream_idle_timeout_ms = 15000
''')
    env = loopback.Helper.environment(environment)
    env.update(CODEX_HOME=str(home), CODEX_FINGERPRINT_FIXTURE_KEY="synthetic", NO_PROXY="*", no_proxy="*", NO_COLOR="1")
    result = subprocess.run([str(binary), "exec", "--skip-git-repo-check", "--ignore-rules", "--json", "Complete the local observation using the requested plan tool."], cwd=workspace, env=env, capture_output=True, timeout=75)
    return {"completed": result.returncode == 0 and FINAL.encode() in result.stdout, "exit_code": result.returncode,
            "same_process_connections": True, "actual_tool_output_seen": any(item["tool_output_items"] for item in server.requests)}


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--cli", required=True, type=Path)
    parser.add_argument("--helper", required=True, type=Path)
    parser.add_argument("--report", required=True, type=Path)
    args = parser.parse_args()
    cases = []
    with tempfile.TemporaryDirectory(prefix="native-h2-tls-") as temporary:
        folder = Path(temporary)
        root, leaf, key, trust = certificate(folder)
        shared_port = 0
        for mode in ("default_native", "custom_ca"):
            for role, binary in (("official", args.cli), ("helper", args.helper), ("helper_pooled_control", args.helper)):
                destination = folder / (mode + "-" + role)
                destination.mkdir()
                server = Server(leaf, key, role, port=shared_port)
                shared_port = server.socket.getsockname()[1]
                try:
                    result = run_client(role, mode, binary.resolve(), server, destination, root, trust)
                except Exception as error:
                    result = {"completed": False, "failure_class": type(error).__name__}
                finally:
                    server.close()
                cases.append({"mode": mode, "client": role, "result": result, "connections": server.records,
                              "requests": server.requests, "server_error_classes": server.errors,
                              "openssl_session_stats": server.context.session_stats()})
    comparisons = []
    for mode in ("default_native", "custom_ca"):
        official, helper, pooled = [case for case in cases if case["mode"] == mode]
        first = lambda case, key: case["connections"][0].get(key) if case["connections"] else None
        h2_observed = first(official, "alpn") == "h2" and first(helper, "alpn") == "h2"
        official_reuse = [connection["session_reused"] for connection in official["connections"]]
        helper_reuse = [connection["session_reused"] for connection in helper["connections"]]
        comparisons.append({"mode": mode, "http2_observed_both": h2_observed,
                            "initial_settings_match": first(official, "settings_frames") == first(helper, "settings_frames") if h2_observed else None,
                            "initial_window_updates_match": first(official, "window_updates") == first(helper, "window_updates") if h2_observed else None,
                            "priorities_match": first(official, "priorities") == first(helper, "priorities") if h2_observed else None,
                            "session_reuse_sequence_matches": official_reuse == helper_reuse,
                            "official_reused_sessions": sum(bool(connection["session_reused"]) for connection in official["connections"]),
                            "helper_reused_sessions": sum(bool(connection["session_reused"]) for connection in helper["connections"])})
        comparisons[-1]["pooled_control_reused_sessions"] = sum(bool(connection["session_reused"]) for connection in pooled["connections"])
    report = {"schema": "controlled-native-h2-resumption-v1", "cli_sha256": sha(args.cli), "helper_sha256": sha(args.helper),
              "dependencies": {name: importlib.metadata.version(name) for name in ("h2", "hyperframe", "hpack")},
              "python_openssl": ssl.OPENSSL_VERSION, "production_access": False, "live_model_inference": False,
              "same_loopback_origin_all_cases": True,
              "cases": cases, "comparisons": comparisons,
              "passed": all(case["result"].get("completed") and len(case["requests"]) == 4 and len(case["connections"]) >= 4 and not case["server_error_classes"] for case in cases),
              "observed_resumption_parity": all(item["session_reuse_sequence_matches"] for item in comparisons),
              "limits": ["Same loopback IP target/certificate and process-local client pool; no production SNI or device geography claim.",
                         "Default native trust uses temporary SSL_CERT_DIR only; custom CA uses SSL_CERT_FILE and selects rustls.",
                         "passed means all bounded observations completed; it does not claim client/helper resumption or fingerprint parity.",
                         "Session resumption results describe server-observed behavior under forced connection closure, not a guarantee for all servers.",
                         "HTTP/2 core parsing uses pinned h2/hyperframe; request bodies and header values are never persisted."]}
    args.report.write_text(json.dumps(report, indent=2) + "\n")
    print(json.dumps({"passed": report["passed"], "comparisons": comparisons,
                      "case_results": [{"mode": case["mode"], "client": case["client"], "result": case["result"], "connections": len(case["connections"]), "requests": len(case["requests"]), "server_errors": case["server_error_classes"]} for case in cases]}), flush=True)
    raise SystemExit(0 if report["passed"] else 1)


if __name__ == "__main__":
    main()
