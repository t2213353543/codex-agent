#!/usr/bin/env python3
"""Capture the pinned official History/Notes executor against offline loopback."""
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
import subprocess
import threading
import time
from urllib.parse import urlsplit

PIN = "660e159a49e823ac8e5986cb238f73158ce4b957d40d9292f8de90862644b501"
BINARY = "/opt/codex-agent-integration/checks/quality-20260924/official-bundle/bin/codex"
CALLS = [
    ("history", "list_windows", {}),
    ("history", "list_items", {"limit": 3, "recent_first": False}),
    ("history", "read_item", {"window_id": "fixture-window", "item_id": "fixture-item", "offset_chars": 0, "limit_chars": 32}),
    ("history", "search_contents", {"query": "enc_synthetic_history_query", "limit": 2}),
    ("notes", "list_files_by_prefix", {"prefix": "fixture", "max_results": 3}),
    ("notes", "read_file", {"path": "fixture.md", "start_line": 1, "stop_line": 2}),
    ("notes", "search_contents", {"query": "enc_synthetic_notes_query", "max_files": 2}),
    ("notes", "append_to_file", {"path": "fixture.md", "text": "enc_synthetic_append"}),
    ("notes", "write_file", {"path": "fixture.md", "text": "enc_synthetic_write"}),
]


def write(path, value):
    raw = value if isinstance(value, bytes) else json.dumps(value, ensure_ascii=False, indent=2).encode()
    path.write_bytes(raw)
    path.chmod(0o600)


def token():
    def part(value): return base64.urlsafe_b64encode(json.dumps(value).encode()).decode().rstrip("=")
    return part({"alg": "none", "typ": "JWT"})+"."+part({"sub": "synthetic-user", "exp": 4102444800,
        "https://api.openai.com/auth": {"chatgpt_account_id": "synthetic-account", "chatgpt_user_id": "synthetic-user", "chatgpt_plan_type": "pro"}})+".synthetic"


class Fixture:
    def __init__(self, output, model):
        self.output, self.model = output, model
        self.records, self.sequence = [], 0
        self.lock = threading.Lock()
        owner = self

        class Handler(BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"
            def log_message(self, *_): pass
            def reply(self, status, value, content_type="application/json"):
                raw = value if isinstance(value, bytes) else json.dumps(value).encode()
                self.send_response(status)
                self.send_header("Content-Type", content_type)
                self.send_header("Content-Length", str(len(raw)))
                self.end_headers()
                self.wfile.write(raw)
                self.wfile.flush()
            def record(self, raw):
                headers = {key: ("<synthetic-auth-redacted>" if key.lower() in ("authorization", "cookie") else value) for key, value in self.headers.items()}
                try: body = json.loads(raw) if raw else None
                except ValueError: body = None
                with owner.lock:
                    owner.records.append({"method": self.command, "path": self.path, "headers": headers,
                        "body": body, "body_base64": base64.b64encode(raw).decode(), "body_sha256": hashlib.sha256(raw).hexdigest()})
                    write(owner.output/"requests.json", owner.records)
                return body
            def do_CONNECT(self):
                self.record(b"")
                self.reply(403, {"error": "offline_fixture_rejects_connect"})
            def do_GET(self):
                self.record(b"")
                self.reply(200, {"models": [owner.model]} if urlsplit(self.path).path.endswith("/models") else {})
            def do_POST(self):
                raw = self.rfile.read(int(self.headers.get("Content-Length", 0)))
                body = self.record(raw)
                path = urlsplit(self.path).path
                if "/alpha/notes/v2/thread_hint" in path:
                    self.reply(200, {"text": "SYNTHETIC_THREAD_HINT"})
                elif "/alpha/history/v2/" in path or "/alpha/notes/v2/" in path:
                    self.reply(200, {"encrypted_output": "enc_synthetic_result", "text": "SYNTHETIC_TOOL_RESULT"})
                elif path.endswith("/responses"):
                    with owner.lock:
                        sequence = owner.sequence
                        if body.get("generate", True): owner.sequence += 1
                    identity = "fixture-response-"+str(sequence)
                    events = [{"type": "response.created", "response": {"id": identity}}]
                    if body.get("generate", True) and sequence < len(CALLS):
                        namespace, name, arguments = CALLS[sequence]
                        events.append({"type": "response.output_item.done", "item": {"type": "function_call", "id": "fc_"+str(sequence),
                            "call_id": "fixture-call-"+str(sequence), "namespace": namespace, "name": name, "arguments": json.dumps(arguments)}})
                    elif body.get("generate", True):
                        events.append({"type": "response.output_item.done", "item": {"type": "message", "id": "fixture-final", "role": "assistant",
                            "content": [{"type": "output_text", "text": "HISTORY_CAPTURE_DONE"}]}})
                    events.append({"type": "response.completed", "response": {"id": identity, "status": "completed", "output": [],
                        "usage": {"input_tokens": 10, "output_tokens": 1, "total_tokens": 11}}})
                    self.reply(200, b"".join(b"data: "+json.dumps(event).encode()+b"\n\n" for event in events), "text/event-stream")
                else: self.reply(200, {})
        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()

    @property
    def url(self): return "http://127.0.0.1:"+str(self.server.server_port)
    def close(self):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=2)


def execute(args):
    if not args.isolated_net or os.name != "posix" or os.readlink("/proc/self/ns/net") == os.readlink("/proc/1/ns/net"):
        raise RuntimeError("fresh_offline_network_namespace_required")
    subprocess.run(["ip", "link", "set", "lo", "up"], check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    links = json.loads(subprocess.check_output(["ip", "-j", "link", "show"]))
    routes = json.loads(subprocess.check_output(["ip", "-j", "route", "show", "table", "all"]))
    if any(link["ifname"] != "lo" for link in links) or any(route.get("dev") != "lo" for route in routes):
        raise RuntimeError("offline_namespace_has_non_loopback_network")
    binary = args.codex.resolve()
    if hashlib.sha256(binary.read_bytes()).hexdigest() != PIN: raise RuntimeError("official_binary_pin_mismatch")
    args.output.mkdir(mode=0o700, parents=True, exist_ok=False)
    home, work = args.output/"home", args.output/"work"
    home.mkdir(mode=0o700); work.mkdir(mode=0o700)
    model = json.loads(args.catalog.read_text())["models"][0]
    fixture = Fixture(args.output, model)
    config = [
        'model = "'+model["slug"]+'"', 'model_provider = "fixture"', 'approval_policy = "never"', 'sandbox_mode = "read-only"',
        'web_search = "disabled"', 'cli_auth_credentials_store = "file"', 'project_doc_max_bytes = 0',
        'chatgpt_base_url = '+json.dumps(fixture.url), 'model_catalog_json = '+json.dumps(str(args.catalog)),
        'analytics.enabled = false', 'feedback.enabled = false', 'skills.include_instructions = false', 'skills.bundled.enabled = false',
        'features.skip_host_skill_discovery = true', 'features.plugins = false', 'features.remote_plugin = false',
        'features.apps = false', 'features.shell_tool = false', 'features.multi_agent = false', 'features.multi_agent_v2 = false',
        'features.hooks = false', 'features.enable_request_compression = false',
        '[features.token_budget]', 'enabled = true', 'use_history_notes_extension = true',
        '[model_providers.fixture]', 'name = "OpenAI"', 'base_url = '+json.dumps(fixture.url+"/backend-api/codex"),
        'wire_api = "responses"', 'requires_openai_auth = true', 'supports_websockets = false',
    ]
    write(home/"config.toml", "\n".join(config).encode())
    env = {k: v for k,v in os.environ.items() if k in ("PATH", "LANG", "LC_ALL", "TZ")}
    env.update(CODEX_HOME=str(home), HTTP_PROXY=fixture.url, HTTPS_PROXY=fixture.url, ALL_PROXY=fixture.url, NO_PROXY="127.0.0.1,localhost,::1", RUST_LOG="off")
    process = None; error = None; done = False; methods = []; thread = None
    try:
        with (args.output/"stderr.log").open("wb") as stderr:
            process = subprocess.Popen([str(binary), "app-server"], cwd=work, env=env, stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=stderr, start_new_session=True)
            messages = queue.Queue()
            def reader():
                with (args.output/"events.jsonl").open("wb") as log:
                    for line in process.stdout:
                        log.write(line); log.flush()
                        messages.put(json.loads(line))
                messages.put(None)
            threading.Thread(target=reader, daemon=True).start()
            next_id = 0
            def send(value): process.stdin.write(json.dumps(value).encode()+b"\n"); process.stdin.flush()
            def receive():
                item = messages.get(timeout=35)
                if item is None: raise RuntimeError("runtime_eof")
                if "method" in item:
                    methods.append(item["method"])
                    if "id" in item: send({"id": item["id"], "error": {"code": -32000, "message": "fixture rejects unexpected delegated call"}})
                return item
            def call(method, params):
                nonlocal next_id
                next_id += 1
                send({"id": next_id, "method": method, "params": params})
                while True:
                    value = receive()
                    if value.get("id") == next_id and "method" not in value:
                        if "error" in value: raise RuntimeError("rpc_"+method+":"+json.dumps(value["error"]))
                        return value["result"]
            call("initialize", {"clientInfo": {"name": "history-loopback-fixture", "version": "1"}, "capabilities": {"experimentalApi": True}})
            send({"method": "initialized", "params": {}})
            call("account/login/start", {"type": "chatgptAuthTokens", "accessToken": token(), "chatgptAccountId": "synthetic-account", "chatgptPlanType": "pro"})
            thread = call("thread/start", {"cwd": str(work), "model": model["slug"], "modelProvider": "fixture", "approvalPolicy": "never", "sandbox": "read-only", "ephemeral": True})["thread"]["id"]
            call("turn/start", {"threadId": thread, "input": [{"type": "text", "text": "Exercise the synthetic history and notes fixtures."}]})
            deadline = time.monotonic()+100
            while time.monotonic() < deadline:
                value = receive()
                if value.get("method") == "turn/completed":
                    done = value.get("params", {}).get("turn", {}).get("status") == "completed"
                    break
    except Exception as exc: error = type(exc).__name__+": "+str(exc)
    finally:
        if process is not None and process.poll() is None:
            process.stdin.close()
            try: process.wait(timeout=3)
            except subprocess.TimeoutExpired: os.killpg(process.pid, signal.SIGKILL); process.wait(timeout=3)
        fixture.close()
    history = [r for r in fixture.records if "/alpha/history/v2/" in r["path"] or "/alpha/notes/v2/" in r["path"]]
    expected = {"/backend-api/codex/alpha/"+space+"/v2/"+name for space,name,_ in CALLS} | {"/backend-api/codex/alpha/notes/v2/thread_hint"}
    observed = {urlsplit(r["path"]).path for r in history}
    headers_ok = all(any(k.lower()=="x-openai-tool-output-truncation-policy" for k in r["headers"]) for r in history)
    context_ok = bool(thread) and all(isinstance(r["body"],dict) and r["body"].get("context",{}).get("session_id") == thread and r["body"].get("context",{}).get("current_agent_name") == "/root" for r in history) if history else False
    result = {"schema": "codex-history-loopback-v1", "status": "passed" if done and expected == observed and headers_ok and context_ok else "failed", "error": error,
        "official_binary_sha256": PIN, "only_loopback_available": True, "network_namespace": os.readlink("/proc/self/ns/net"), "interfaces": links, "routes": routes,
        "real_account": False, "real_model": False, "real_history_or_notes_written": False, "tool_operations": len(history), "observed_paths": sorted(observed), "missing_paths": sorted(expected-observed),
        "truncation_headers_observed": headers_ok, "session_agent_context_observed": context_ok, "turn_completed": done, "notification_methods": sorted(set(methods))}
    write(args.output/"summary.json", result)
    print(json.dumps(result), flush=True)
    return 0 if result["status"] == "passed" else 1


def export_oracle(local, output):
    summary = json.loads((local/"summary.json").read_text(encoding="utf-8"))
    provenance = json.loads((local/"run.json").read_text(encoding="utf-8"))
    raw_records = (local/"requests.json").read_bytes()
    records = json.loads(raw_records)
    if summary.get("status") != "passed" or not summary.get("only_loopback_available"):
        raise RuntimeError("cannot_export_unverified_history_capture")
    operations = []
    for row in records:
        if "/alpha/history/v2/" not in row["path"] and "/alpha/notes/v2/" not in row["path"]: continue
        raw = base64.b64decode(row["body_base64"])
        if hashlib.sha256(raw).hexdigest() != row["body_sha256"] or json.loads(raw) != row["body"]:
            raise RuntimeError("captured_body_hash_mismatch")
        headers = {k.lower(): v for k,v in row["headers"].items() if k.lower() in
                   ("x-openai-encrypted-tool-arguments", "x-openai-tool-output-truncation-policy")}
        encrypted = row["path"].endswith(("/search_contents", "/append_to_file", "/write_file"))
        if (headers.get("x-openai-encrypted-tool-arguments") == "true") != encrypted:
            raise RuntimeError("unexpected_encrypted_argument_marker")
        operations.append({"method": row["method"], "path": row["path"], "headers": headers,
                           "body_utf8": raw.decode(), "body_sha256": row["body_sha256"]})
    request = next(row for row in records if row["path"].endswith("/responses"))["body"]
    tools = [{"namespace": namespace["name"], "name": tool["name"], "strict": tool.get("strict"), "parameters": tool.get("parameters")}
             for namespace in request["tools"] if namespace.get("name") in ("history", "notes")
             for tool in namespace["tools"]]
    if {(v["namespace"],v["name"]) for v in tools} != {(space,name) for space,name,_ in CALLS}:
        raise RuntimeError("captured_history_tool_inventory_incomplete")
    result = {"schema": "codex-history-official-writer-oracle-v1", "capture_id": local.name,
              "official_binary_sha256": summary["official_binary_sha256"], "source_requests_sha256": hashlib.sha256(raw_records).hexdigest(),
              "capture_script_sha256": provenance["script_sha256"], "only_loopback_available": True,
              "real_account": False, "real_model": False, "real_history_or_notes_written": False,
              "body_bytes_normalized": False, "operations": operations, "tool_schemas": tools,
              "normalizations": ["control header names lowercased", "non-control headers omitted", "tool descriptions omitted"]}
    write(output, result)
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--remote", action="store_true")
    parser.add_argument("--isolated-net", action="store_true")
    parser.add_argument("--codex", type=Path, default=Path(BINARY))
    parser.add_argument("--catalog", type=Path)
    parser.add_argument("--output", type=Path)
    parser.add_argument("--export-run", type=Path)
    args = parser.parse_args()
    if args.export_run:
        target=args.output or args.export_run/"writer-oracle.json"
        result=export_oracle(args.export_run,target)
        print(json.dumps({"oracle":str(target),"operations":len(result["operations"]),"tool_schemas":len(result["tool_schemas"])}))
        return 0
    if not args.remote: return execute(args)
    from integration_env import ROOT, connect
    import shlex
    stamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%S.%fZ")
    script_sha = hashlib.sha256(Path(__file__).read_bytes()).hexdigest()
    artifact = "/opt/codex-agent-integration/appserver-parity/artifacts/history-"+script_sha
    output = "/opt/codex-agent-integration/appserver-parity/history/"+stamp
    local = ROOT/".local/appserver-parity/history"/stamp
    local.mkdir(parents=True)
    catalog_source = ROOT/".local/codex-source-0.155.0/codex-rs/models-manager/models.json"
    models = json.loads(catalog_source.read_text(encoding="utf-8"))["models"]
    model = next(m for m in models if m["slug"] == "gpt-5.5")
    write(local/"models.json", {"models": [model]})
    client = connect()
    try:
        with client.open_sftp() as sftp:
            sftp.put(__file__, artifact+".py"); sftp.chmod(artifact+".py", 0o600)
            sftp.put(str(local/"models.json"), artifact+"-models.json"); sftp.chmod(artifact+"-models.json", 0o600)
        command = ["unshare", "--net", "--", "python3", "-B", artifact+".py", "--isolated-net", "--catalog", artifact+"-models.json", "--output", output]
        _, stdout, stderr = client.exec_command(shlex.join(command), timeout=200)
        raw = stdout.read(); code = stdout.channel.recv_exit_status(); error = stderr.read()
        with client.open_sftp() as sftp:
            for name in ("requests.json", "summary.json", "events.jsonl", "stderr.log"):
                try: sftp.get(output+"/"+name, str(local/name))
                except FileNotFoundError: pass
        write(local/"run.json", {"remote_output": output, "script_sha256": script_sha, "exit_code": code, "stderr_bytes": len(error), "catalog_source_sha256": hashlib.sha256(catalog_source.read_bytes()).hexdigest()})
        result = json.loads(raw) if raw else {"status": "failed", "stderr_bytes": len(error)}
        if code == 0: export_oracle(local, local/"writer-oracle.json")
        print(json.dumps({"local": str(local), "result": result}), flush=True)
        return code
    finally: client.close()


if __name__ == "__main__": raise SystemExit(main())
