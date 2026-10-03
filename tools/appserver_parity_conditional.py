#!/usr/bin/env python3
"""Capture official conditional writers against a synthetic loopback provider.

No real account is loaded. All model and image responses are local fixtures.
Use --remote to run the pinned binary on the existing private integration host.
"""
import argparse
import base64
from datetime import datetime, timezone
import hashlib
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import queue
import signal
import struct
import subprocess
import sys
import threading
import time

PIN = "660e159a49e823ac8e5986cb238f73158ce4b957d40d9292f8de90862644b501"
SERVER_BINARY = "/opt/codex-agent-integration/checks/quality-20260924/official-bundle/bin/codex"
PNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR4nGP4z8DwHwAFAAH/iZk9HQAAAABJRU5ErkJggg=="


def write(path, value):
    raw = value if isinstance(value, bytes) else json.dumps(value, ensure_ascii=False, indent=2).encode()
    path.write_bytes(raw)
    if os.name == "posix":
        path.chmod(0o600)


def synthetic_auth():
    token = lambda value: base64.urlsafe_b64encode(json.dumps(value).encode()).decode().rstrip("=")
    identity = token({"alg": "none", "typ": "JWT"}) + "." + token({"email": "fixture@example.invalid", "https://api.openai.com/auth": {
        "chatgpt_plan_type": "pro", "chatgpt_user_id": "fixture-user", "chatgpt_account_id": "fixture-account"}}) + ".c3ludGhldGlj"
    return {"auth_mode": "chatgpt", "tokens": {"access_token": "synthetic-access-not-a-real-token", "id_token": identity,
            "refresh_token": "", "account_id": "fixture-account"}, "last_refresh": datetime.now(timezone.utc).isoformat()}


def sse(items, response_id):
    events = [{"type": "response.created", "response": {"id": response_id}}]
    events.extend({"type": "response.output_item.done", "output_index": index, "item": item} for index, item in enumerate(items))
    events.append({"type": "response.completed", "response": {"id": response_id, "status": "completed", "output": [],
        "usage": {"input_tokens": 1, "output_tokens": 1, "total_tokens": 2}}})
    return b"".join(b"data: " + json.dumps(event).encode() + b"\n\n" for event in events)


class Fixture:
    def __init__(self, output, case):
        self.output, self.case = output, case
        self.requests, self.calls, self.lock = [], 0, threading.Lock()
        owner = self
        class Handler(BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"
            def log_message(self, *args):
                pass
            def reply(self, status, value, content_type="application/json"):
                raw = value if isinstance(value, bytes) else json.dumps(value).encode()
                self.send_response(status)
                self.send_header("Content-Type", content_type)
                self.send_header("Content-Length", str(len(raw)))
                self.send_header("Connection", "close")
                self.end_headers()
                self.wfile.write(raw)
                self.close_connection = True
            def do_CONNECT(self):
                owner.record(self, b"", blocked=True)
                self.reply(502, {"error": "remote_connections_disabled_by_fixture"})
            def do_GET(self):
                owner.record(self, b"")
                if self.headers.get("Upgrade", "").lower() == "websocket":
                    accept = base64.b64encode(hashlib.sha1((self.headers["Sec-WebSocket-Key"] + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11").encode()).digest()).decode()
                    self.send_response(101)
                    self.send_header("Upgrade", "websocket")
                    self.send_header("Connection", "Upgrade")
                    self.send_header("Sec-WebSocket-Accept", accept)
                    self.end_headers()
                    self.wfile.flush()
                    self.connection.settimeout(10)
                    try:
                        while True:
                            head = self.rfile.read(2)
                            if len(head) != 2:
                                break
                            opcode, size = head[0] & 15, head[1] & 127
                            if size in (126, 127):
                                size = int.from_bytes(self.rfile.read(2 if size == 126 else 8), "big")
                            mask = self.rfile.read(4) if head[1] & 128 else b""
                            payload = self.rfile.read(size)
                            if mask:
                                payload = bytes(x ^ mask[index % 4] for index, x in enumerate(payload))
                            if opcode == 8:
                                break
                            if opcode != 1:
                                continue
                            owner.record(self, payload, websocket=True)
                            message = {"type": "message", "id": "guardian-fixture-message", "role": "assistant", "content": [{"type": "output_text", "text": "low"}]}
                            for line in sse([message], "guardian-fixture-response").splitlines():
                                if not line.startswith(b"data: "):
                                    continue
                                data = line[6:]
                                header = bytes([129, len(data)]) if len(data) < 126 else bytes([129, 126]) + struct.pack("!H", len(data))
                                self.wfile.write(header + data)
                                self.wfile.flush()
                    except (OSError, TimeoutError):
                        pass
                    self.close_connection = True
                    return
                if self.path.split("?", 1)[0].endswith("/models"):
                    self.reply(200, {"models": []})
                else:
                    self.reply(200, {})
            def do_POST(self):
                raw = self.rfile.read(int(self.headers.get("Content-Length", 0)))
                owner.record(self, raw)
                if self.path.endswith("/images/generations") or self.path.endswith("/images/edits"):
                    self.reply(200, {"created": 1, "background": "opaque", "data": [{"b64_json": PNG, "generation_id": "fixture-image"}]})
                    return
                if self.path.endswith("/guardian"):
                    assessment = {"risk_level": "low", "user_authorization": "high", "outcome": "allow", "rationale": "Synthetic read-only fixture."}
                    self.reply(200, sse([{"type": "message", "role": "assistant", "id": "guardian-assessment", "content": [{"type": "output_text", "text": json.dumps(assessment)}]}], "guardian-review-fixture"), "text/event-stream")
                    return
                if self.path.endswith("/responses"):
                    body = json.loads(raw)
                    if (body.get("client_metadata") or {}).get("x-openai-subagent") == "guardian":
                        assessment = {"risk_level": "low", "user_authorization": "high", "outcome": "allow", "rationale": "Synthetic read-only fixture."}
                        self.reply(200, sse([{"type": "message", "role": "assistant", "id": "guardian-assessment", "content": [{"type": "output_text", "text": json.dumps(assessment)}]}], "guardian-review-fixture"), "text/event-stream")
                        return
                    owner.calls += 1
                    if owner.calls == 1:
                        args = {"prompt": "SYNTHETIC_PROVIDER_IMAGE_MARKER"}
                        if owner.case == "images-edit":
                            args["referenced_image_paths"] = [str(owner.output / "input.png")]
                        items = [{"type": "function_call", "id": "fc-fixture", "call_id": "call-fixture", "namespace": "image_gen",
                                  "name": "imagegen", "arguments": json.dumps(args)}]
                        if owner.case == "guardian-classifier":
                            items = [{"type": "function_call", "id": "fc-fixture", "call_id": "call-fixture", "namespace": "mcp__fixture",
                                      "name": "inspect_marker", "arguments": "{}"}]
                    else:
                        items = [{"type": "message", "id": "msg-fixture", "role": "assistant", "content": [{"type": "output_text", "text": "FIXTURE_DONE"}]}]
                    self.reply(200, sse(items, "resp-fixture-" + str(owner.calls)), "text/event-stream")
                    return
                self.reply(200, {})
        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()

    @property
    def url(self):
        return "http://127.0.0.1:" + str(self.server.server_port)

    def record(self, handler, raw, blocked=False, websocket=False):
        try:
            body = json.loads(raw) if raw else None
        except ValueError:
            body = None
        row = {"method": handler.command, "path": handler.path, "headers": dict(handler.headers),
               "body": body, "body_bytes": len(raw), "body_sha256": hashlib.sha256(raw).hexdigest(), "blocked": blocked,
               "websocket_message": websocket}
        with self.lock:
            self.requests.append(row)
            write(self.output / "requests.json", self.requests)

    def close(self):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=2)


def run_case(binary, root, route, case):
    output = root / (route + "-" + case)
    output.mkdir(mode=0o700)
    work, home = output / "work", output / "home"
    work.mkdir(mode=0o700)
    home.mkdir(mode=0o700)
    write(output / "input.png", base64.b64decode(PNG))
    fixture = Fixture(output, case)
    config = '\n'.join([
        'model = "mock-model"', 'model_provider = "openai-fixture"', 'approval_policy = "never"',
        'sandbox_mode = "danger-full-access"', 'web_search = "disabled"', 'cli_auth_credentials_store = "file"',
        'chatgpt_base_url = ' + json.dumps(fixture.url), 'project_doc_max_bytes = 0',
        'analytics.enabled = false', 'feedback.enabled = false', 'skills.include_instructions = false',
        'skills.bundled.enabled = false', 'features.skip_host_skill_discovery = true',
        'features.plugins = false', 'features.remote_plugin = false', 'features.apps = false',
        'features.shell_tool = false', 'features.multi_agent = false', 'features.multi_agent_v2 = false',
        'features.hooks = false', 'features.image_generation = true', 'features.enable_request_compression = false',
        '[model_providers.openai-fixture]', 'name = "OpenAI"', 'base_url = ' + json.dumps(fixture.url + "/api/codex"),
        'wire_api = "responses"', 'requires_openai_auth = true', 'supports_websockets = false',
        'request_max_retries = 0', 'stream_max_retries = 0', 'stream_idle_timeout_ms = 10000',
    ])
    if case == "guardian-classifier":
        config = config.replace('approval_policy = "never"', 'approval_policy = "on-request"\napprovals_reviewer = "auto_review"')
        config = config.replace('sandbox_mode = "danger-full-access"', 'sandbox_mode = "read-only"')
        config = config.replace(fixture.url + "/api/codex", fixture.url + "/backend-api/codex")
        config += '\n[features.guardianv2]\nenabled = true\nfree_guardian = true\n[features.guardianv2.review_scope]\ncomputer_use_only = false\n'
        config += '\n[mcp_servers.fixture]\ncommand = ' + json.dumps(sys.executable) + '\nargs = ' + json.dumps([str(Path(__file__).resolve()), '--mcp-fixture']) + '\ndefault_tools_approval_mode = "prompt"\n'
    write(home / "config.toml", config.encode())
    write(home / "auth.json", synthetic_auth())
    env = {k: v for k, v in os.environ.items() if k in ["PATH", "LANG", "LC_ALL", "TZ"]}
    env.update(CODEX_HOME=str(home), HTTP_PROXY=fixture.url, HTTPS_PROXY=fixture.url, ALL_PROXY=fixture.url,
               NO_PROXY="127.0.0.1,localhost,::1", CODEX_REFRESH_TOKEN_URL_OVERRIDE=fixture.url + "/refresh-disabled", RUST_LOG="off")
    process = None
    started = time.monotonic()
    status, error = "failed", None
    try:
        command = [str(binary), "app-server"] if route == "app" else [str(binary), "exec", "--json", "--skip-git-repo-check", "-"]
        with (output / "stderr.log").open("wb") as stderr:
            process = subprocess.Popen(command, cwd=work, env=env, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                       stderr=stderr, start_new_session=os.name == "posix")
            if route == "exec":
                out, _ = process.communicate(b"Generate the synthetic fixture image.", timeout=45)
                write(output / "events.jsonl", out)
                status = "completed" if process.returncode == 0 else "failed"
            else:
                messages = queue.Queue()
                def reader():
                    with (output / "events.jsonl").open("wb") as log:
                        for line in process.stdout:
                            log.write(line)
                            log.flush()
                            messages.put(json.loads(line))
                        messages.put(None)
                thread = threading.Thread(target=reader, daemon=True)
                thread.start()
                counter, pending = 0, []
                def send(value):
                    process.stdin.write(json.dumps(value).encode() + b"\n")
                    process.stdin.flush()
                def receive():
                    item = messages.get(timeout=30)
                    if item is None:
                        raise ValueError("runtime_eof")
                    if "method" in item and "id" in item:
                        send({"id": item["id"], "error": {"code": -32000, "message": "fixture does not support client requests"}})
                    return item
                def call(method, params):
                    nonlocal counter
                    counter += 1
                    send({"id": counter, "method": method, "params": params})
                    while True:
                        value = receive()
                        if value.get("id") == counter and "method" not in value:
                            if "error" in value:
                                raise ValueError("rpc_" + method.replace("/", "_") + "_failed")
                            return value["result"]
                        pending.append(value)
                call("initialize", {"clientInfo": {"name": "conditional-fixture", "version": "1.0"}, "capabilities": {"experimentalApi": True}})
                send({"method": "initialized", "params": {}})
                params = {"cwd": str(work), "model": "mock-model", "modelProvider": "openai-fixture",
                    "approvalPolicy": "never", "sandbox": "danger-full-access", "ephemeral": True}
                if case == "guardian-classifier":
                    params.update(approvalPolicy="on-request", approvalsReviewer="auto_review", sandbox="read-only")
                tid = call("thread/start", params)["thread"]["id"]
                call("turn/start", {"threadId": tid, "input": [{"type": "text", "text": "Generate the synthetic fixture image."}]})
                deadline = time.monotonic() + 45
                while time.monotonic() < deadline:
                    event = pending.pop(0) if pending else receive()
                    if event.get("method") == "turn/completed":
                        status = event.get("params", {}).get("turn", {}).get("status", "failed")
                        break
    except Exception as failure:
        error = type(failure).__name__ + ": " + str(failure)
    finally:
        if process is not None and process.poll() is None:
            process.stdin.close()
            try:
                process.wait(timeout=3)
            except subprocess.TimeoutExpired:
                if os.name == "posix":
                    os.killpg(process.pid, signal.SIGKILL)
                else:
                    process.kill()
                process.wait(timeout=3)
        fixture.close()
        (home / "auth.json").unlink(missing_ok=True)
    path = "/api/codex/images/edits" if case == "images-edit" else "/api/codex/images/generations"
    if case == "guardian-classifier":
        path = "/backend-api/codex/guardian-classifier"
    matching = [row for row in fixture.requests if row["path"] == path]
    result = {"route": route, "case": case, "status": status, "error": error, "captured": bool(matching),
              "request_count": len(fixture.requests), "target_count": len(matching),
              "target_http_count": sum(not row.get("websocket_message") for row in matching),
              "target_ws_message_count": sum(bool(row.get("websocket_message")) for row in matching),
              "remote_connects_blocked": sum(row["blocked"] for row in fixture.requests),
              "elapsed_ms": round((time.monotonic() - started) * 1000), "scope": "official_binary_to_local_synthetic_fixture",
              "real_upstream": False, "real_credentials_loaded": False}
    write(output / "summary.json", result)
    return result


def execute(args):
    if args.isolated_net:
        if os.name != "posix" or os.readlink("/proc/self/ns/net") == os.readlink("/proc/1/ns/net"):
            raise ValueError("fresh_network_namespace_required")
        subprocess.run(["ip", "link", "set", "lo", "up"], check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    binary = args.codex.resolve()
    digest = hashlib.sha256(binary.read_bytes()).hexdigest()
    if digest != PIN:
        raise ValueError("official_binary_pin_mismatch")
    args.output.mkdir(mode=0o700, parents=True, exist_ok=False)
    outcomes = [run_case(binary, args.output, route, case) for route in args.route for case in args.case]
    result = {"schema": "codex-conditional-loopback-v1", "official_binary_sha256": digest,
              "official_version": subprocess.check_output([str(binary), "--version"], text=True).strip(),
              "network_namespace": "isolated" if args.isolated_net else "not_enforced", "outcomes": outcomes}
    write(args.output / "summary.json", result)
    print(json.dumps(result), flush=True)
    return 0 if all(item["captured"] for item in outcomes) else 1


def main():
    if sys.argv[1:] == ["--mcp-fixture"]:
        for raw in sys.stdin.buffer:
            message = json.loads(raw)
            if "id" not in message:
                continue
            method = message.get("method")
            value = {}
            if method == "initialize":
                value = {"protocolVersion": message.get("params", {}).get("protocolVersion", "2024-11-05"), "capabilities": {"tools": {}}, "serverInfo": {"name": "fixture", "version": "1"}}
            elif method == "tools/list":
                value = {"tools": [{"name": "inspect_marker", "description": "Inspect synthetic marker", "inputSchema": {"type": "object", "properties": {}}}]}
            elif method == "tools/call":
                value = {"content": [{"type": "text", "text": "SYNTHETIC_TOOL_MARKER"}], "isError": False}
            print(json.dumps({"jsonrpc": "2.0", "id": message["id"], "result": value}), flush=True)
        return 0
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--remote", action="store_true")
    parser.add_argument("--isolated-net", action="store_true", help=argparse.SUPPRESS)
    parser.add_argument("--codex", type=Path, default=Path(SERVER_BINARY))
    parser.add_argument("--output", type=Path)
    parser.add_argument("--route", action="append", choices=["app", "exec"])
    parser.add_argument("--case", action="append", choices=["images-generation", "images-edit", "guardian-classifier"])
    args = parser.parse_args()
    args.route = args.route or ["app", "exec"]
    args.case = args.case or ["images-generation", "images-edit"]
    if not args.remote:
        if args.output is None:
            parser.error("--output is required")
        return execute(args)
    from integration_env import ROOT, connect
    import shlex
    stamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%S.%fZ")
    digest = hashlib.sha256(Path(__file__).read_bytes()).hexdigest()
    remote_script = "/opt/codex-agent-integration/appserver-parity/artifacts/conditional-" + digest + ".py"
    remote_output = "/opt/codex-agent-integration/appserver-parity/conditional/" + stamp
    destination = ROOT / ".local/appserver-parity/conditional" / stamp
    destination.mkdir(parents=True)
    client = connect()
    try:
        with client.open_sftp() as sftp:
            sftp.put(__file__, remote_script)
            sftp.chmod(remote_script, 0o600)
        command = ["unshare", "--net", "--", "python3", "-B", remote_script, "--isolated-net", "--output", remote_output]
        for route in args.route:
            command += ["--route", route]
        for case in args.case:
            command += ["--case", case]
        _, stdout, stderr = client.exec_command(shlex.join(command), timeout=240)
        raw = stdout.read()
        code = stdout.channel.recv_exit_status()
        error = stderr.read()
        write(destination / "summary.json", json.loads(raw) if raw else {"failed": True, "stderr_bytes": len(error)})
        write(destination / "run.json", {"remote_output": remote_output, "script_sha256": digest, "exit_code": code})
        print(raw.decode(), flush=True)
        print(json.dumps({"local": str(destination), "remote": remote_output, "exit_code": code, "stderr_bytes": len(error)}))
        return code
    finally:
        client.close()


if __name__ == "__main__":
    raise SystemExit(main())
