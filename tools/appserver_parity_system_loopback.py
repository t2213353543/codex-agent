#!/usr/bin/env python3
"""Capture pinned official cost writers and authentication gates offline.

Only synthetic auth and loopback responses are used. The official 150 second
cost poll timer is preserved; both auth cases run concurrently in one fresh
network namespace that has only loopback interfaces/routes.
"""
import argparse
import base64
from concurrent.futures import ThreadPoolExecutor
from datetime import datetime, timezone
import hashlib
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import queue
import signal
import subprocess
import sys
import threading
import time
from urllib.parse import urlsplit

PIN = "660e159a49e823ac8e5986cb238f73158ce4b957d40d9292f8de90862644b501"
BINARY = "/opt/codex-agent-integration/checks/quality-20260924/official-bundle/bin/codex"
CASES = ["api-key", "chatgpt"]
PRINT_LOCK = threading.Lock()


def emit(value):
    with PRINT_LOCK:
        print(json.dumps(value, ensure_ascii=False), flush=True)


def write(path, value):
    raw = value if isinstance(value, bytes) else json.dumps(value, ensure_ascii=False, indent=2).encode()
    path.write_bytes(raw)
    if os.name == "posix":
        path.chmod(0o600)


def synthetic_auth():
    encode = lambda value: base64.urlsafe_b64encode(json.dumps(value).encode()).decode().rstrip("=")
    identity = encode({"alg": "none", "typ": "JWT"}) + "." + encode({
        "email": "system-fixture@example.invalid", "https://api.openai.com/auth": {
            "chatgpt_plan_type": "pro", "chatgpt_user_id": "system-fixture-user",
            "chatgpt_account_id": "system-fixture-account"}}) + ".c3ludGhldGlj"
    return {"auth_mode": "chatgpt", "tokens": {"access_token": "synthetic-system-chatgpt-token", "id_token": identity,
            "refresh_token": "", "account_id": "system-fixture-account"}, "last_refresh": datetime.now(timezone.utc).isoformat()}


class Fixture:
    def __init__(self, output, case):
        self.output, self.case = output, case
        self.lock = threading.Lock()
        self.rows = []
        self.started = time.monotonic()
        self.thread_id = None
        self.turn_id = None
        self.response_id = "resp-system-fixture-" + case
        self.cost_polled = threading.Event()
        self.cost_metric = threading.Event()
        owner = self

        class Handler(BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"
            def log_message(self, *_):
                pass
            def body(self):
                if self.headers.get("Transfer-Encoding", "").lower() == "chunked":
                    chunks = []
                    while True:
                        size = int(self.rfile.readline().split(b";", 1)[0], 16)
                        if size == 0:
                            self.rfile.readline()
                            return b"".join(chunks)
                        chunks.append(self.rfile.read(size))
                        self.rfile.read(2)
                return self.rfile.read(int(self.headers.get("Content-Length", 0)))
            def reply(self, code, value, content_type="application/json"):
                raw = value if isinstance(value, bytes) else json.dumps(value, separators=(",", ":")).encode()
                self.send_response(code)
                self.send_header("Content-Type", content_type)
                self.send_header("Content-Length", str(len(raw)))
                self.send_header("Connection", "close")
                self.end_headers()
                self.wfile.write(raw)
                self.close_connection = True
                return raw
            def record(self, request, response, status):
                owner.record(self, request, response, status)
            def do_CONNECT(self):
                raw = self.reply(403, {"error": "offline-fixture-blocks-nonlocal-connect"})
                self.record(b"", raw, 403)
            def do_GET(self):
                path = urlsplit(self.path).path
                status = 200
                if self.headers.get("Upgrade", "").lower() == "websocket":
                    status, result = 426, {"error": "fixture-uses-http"}
                elif path.endswith("/models"):
                    result = {"models": []}
                elif path.endswith("/wham/usage"):
                    result = {"plan_type": "pro", "rate_limit": {"allowed": True, "limit_reached": False,
                        "primary_window": {"used_percent": 7, "limit_window_seconds": 300,
                        "reset_after_seconds": 60, "reset_at": 2000000000}}}
                elif path.endswith("/settings/user"):
                    result = {"commit_attribution_enabled": True}
                elif path.endswith("/plugins/featured"):
                    result = []
                elif "/ps/plugins/" in path:
                    result = {"plugins": [], "has_more": False}
                else:
                    result = {}
                raw = self.reply(status, result)
                self.record(b"", raw, status)
            def do_POST(self):
                request = self.body()
                path = urlsplit(self.path).path
                try:
                    value = json.loads(request)
                except (ValueError, UnicodeDecodeError):
                    value = None
                if path.endswith("/analytics/codex/turn-costs"):
                    result = {"turns": [{"turn_id": turn, "status": "priced", "total_usd": "0.125001", "event_count": 1,
                        "responses": [{"response_id": owner.response_id, "total_usd": "0.125001"}],
                        "model": "mock-model", "speed": "standard", "reasoning_effort": "high"} for turn in value["turn_ids"]]}
                    if owner.turn_id in value["turn_ids"]:
                        owner.cost_polled.set()
                    raw = self.reply(200, result)
                elif path.endswith("/usage/thread-estimates/query"):
                    result = {"threads": [{"thread_id": thread["thread_id"], "turns": [{"turn_id": turn,
                        "model": "mock-model", "estimated_usage_usd_micros": 125001,
                        "settled_response_ids": [owner.response_id]} for turn in thread["turn_ids"]]} for thread in value["threads"]]}
                    if any(owner.thread_id == thread["thread_id"] and owner.turn_id in thread["turn_ids"] for thread in value["threads"]):
                        owner.cost_polled.set()
                    raw = self.reply(200, result)
                elif path.endswith("/responses"):
                    item = {"id": "msg-system", "type": "message", "role": "assistant", "content": [{"type": "output_text", "text": "SYSTEM_FIXTURE_DONE"}]}
                    events = [{"type": "response.created", "response": {"id": owner.response_id}},
                        {"type": "response.output_item.done", "output_index": 0, "item": item},
                        {"type": "response.completed", "response": {"id": owner.response_id, "model": "mock-model", "status": "completed", "output": [],
                         "usage": {"input_tokens": 3, "output_tokens": 1, "total_tokens": 4}}}]
                    response = b"".join(b"data: " + json.dumps(event).encode() + b"\n\n" for event in events)
                    raw = self.reply(200, response, "text/event-stream")
                else:
                    if b"codex.turn.cost_microusd" in request or b"codex.turn_cost" in request:
                        owner.cost_metric.set()
                    raw = self.reply(200, {})
                self.record(request, raw, 200)

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.worker = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.worker.start()
    @property
    def url(self):
        return "http://127.0.0.1:" + str(self.server.server_port)
    def record(self, handler, request, response, status):
        def decode(raw):
            try: return json.loads(raw)
            except (ValueError, UnicodeDecodeError): return None
        retain = any(path in handler.path for path in ("turn-costs", "thread-estimates", "settings/user", "plugins/featured", "/wham/usage"))
        cost_metric = b"codex.turn.cost_microusd" in request or b"codex.turn_cost" in request
        retained_request, retained_response = (request, response) if retain or cost_metric else (b"", b"")
        row = {"method": handler.command, "path": handler.path, "headers": dict(handler.headers),
            "body": decode(retained_request), "body_base64": base64.b64encode(retained_request).decode(),
            "body_sha256": hashlib.sha256(request).hexdigest(), "response_body": decode(response),
            "response_base64": base64.b64encode(retained_response).decode(), "status": status,
            "body_bytes":len(request), "body_retained": retain or cost_metric,
            "elapsed_ms": round((time.monotonic() - self.started) * 1000)}
        with self.lock:
            self.rows.append(row)
            write(self.output / "requests.json", self.rows)
        if "turn-costs" in handler.path or "thread-estimates" in handler.path:
            emit({"event": "official_cost_request", "case": self.case, "path": handler.path,
                  "actual_turn_matched": self.cost_polled.is_set(), "elapsed_ms": row["elapsed_ms"]})
    def close(self):
        self.server.shutdown()
        self.server.server_close()
        self.worker.join(timeout=2)


def run_case(binary, root, case, probe_only=False):
    output = root / case
    output.mkdir(mode=0o700)
    home, work = output / "home", output / "work"
    home.mkdir(mode=0o700); work.mkdir(mode=0o700)
    fixture = Fixture(output, case)
    provider = "fixture" if case == "api-key" else "openai"
    config = [
        'model = "mock-model"', 'model_provider = ' + json.dumps(provider),
        'approval_policy = "never"', 'sandbox_mode = "read-only"', 'web_search = "disabled"',
        'cli_auth_credentials_store = "file"', 'project_doc_max_bytes = 0',
        'chatgpt_base_url = ' + json.dumps(fixture.url + "/backend-api"),
        'openai_base_url = ' + json.dumps(fixture.url + "/v1"),
        'analytics.enabled = true', 'feedback.enabled = false', 'skills.include_instructions = false',
        'skills.bundled.enabled = false', 'features.skip_host_skill_discovery = true',
        'features.plugins = true', 'features.remote_plugin = true', 'features.apps = false',
        'features.shell_tool = false', 'features.multi_agent = false', 'features.multi_agent_v2 = false',
        'features.hooks = false', 'features.enable_request_compression = false', 'features.runtime_metrics = true',
        'features.responses_websockets = false', 'features.responses_websockets_v2 = false',
        '[otel]', 'exporter = "none"',
        'metrics_exporter = { otlp-http = { endpoint = ' + json.dumps(fixture.url + "/otel/metrics") + ', protocol = "json" } }',
    ]
    if case == "api-key":
        config += ['[model_providers.fixture]', 'name = "Fixture"', 'base_url = ' + json.dumps(fixture.url + "/v1"),
            'wire_api = "responses"', 'env_key = "SYSTEM_FIXTURE_API_KEY"', 'requires_openai_auth = false',
            'supports_websockets = false', 'request_max_retries = 0', 'stream_max_retries = 0']
        write(home / "auth.json", {"auth_mode": "apikey", "OPENAI_API_KEY": "sk-synthetic-system-api-key"})
    else:
        write(home / "auth.json", synthetic_auth())
    write(home / "config.toml", "\n".join(config).encode())
    env = {k: v for k, v in os.environ.items() if k in ("PATH", "LANG", "LC_ALL", "TZ")}
    env.update(CODEX_HOME=str(home), SYSTEM_FIXTURE_API_KEY="sk-synthetic-system-api-key",
        HTTP_PROXY=fixture.url, HTTPS_PROXY=fixture.url, ALL_PROXY=fixture.url, NO_PROXY="127.0.0.1,localhost,::1",
        CODEX_REFRESH_TOKEN_URL_OVERRIDE=fixture.url + "/disabled-refresh", RUST_LOG="off", OTEL_METRIC_EXPORT_INTERVAL="1000")
    process = None; status = "failed"; failure = None; rpc = {}; methods = []
    try:
        with (output / "stderr.log").open("wb") as stderr:
            process = subprocess.Popen([str(binary), "app-server"], cwd=work, env=env, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                stderr=stderr, start_new_session=True)
            messages = queue.Queue()
            def reader():
                with (output / "events.jsonl").open("wb") as log:
                    for line in process.stdout:
                        log.write(line); log.flush()
                        try: messages.put(json.loads(line))
                        except ValueError: pass
                messages.put(None)
            threading.Thread(target=reader, daemon=True).start()
            next_id = 0; pending = []
            def send(value):
                process.stdin.write(json.dumps(value).encode() + b"\n"); process.stdin.flush()
            def receive(timeout=20):
                value = messages.get(timeout=timeout)
                if value is None: raise RuntimeError("runtime_eof")
                if "method" in value:
                    methods.append(value["method"])
                    if "id" in value: send({"id": value["id"], "error": {"code": -32000, "message": "offline fixture client request unsupported"}})
                return value
            def call(method, params, allow_error=False):
                nonlocal next_id
                next_id += 1; send({"id": next_id, "method": method, "params": params})
                while True:
                    value = receive()
                    if value.get("id") == next_id and "method" not in value:
                        rpc[method] = value
                        if "error" in value and not allow_error: raise RuntimeError("rpc:" + method + ":" + json.dumps(value["error"]))
                        return value.get("result")
                    pending.append(value)
            call("initialize", {"clientInfo": {"name": "system-loopback-fixture", "version": "1"}, "capabilities": {"experimentalApi": True}})
            send({"method": "initialized", "params": {}})
            call("account/rateLimits/read", {"excludeResetCreditDetails": True}, allow_error=True)
            call("plugin/list", {"cwds": [str(work)], "forceRefetch": True}, allow_error=True)
            thread = call("thread/start", {"cwd": str(work), "model": "mock-model", "modelProvider": provider,
                "approvalPolicy": "never", "sandbox": "read-only", "ephemeral": True})
            fixture.thread_id = thread["thread"]["id"]
            turn = call("turn/start", {"threadId": fixture.thread_id, "input": [{"type": "text", "text": "Reply SYSTEM_FIXTURE_DONE. Use no tools."}]})
            fixture.turn_id = turn["turn"]["id"]
            deadline = time.monotonic() + 30
            while time.monotonic() < deadline:
                item = pending.pop(0) if pending else receive()
                if item.get("method") == "turn/completed":
                    status = item.get("params", {}).get("turn", {}).get("status", "failed")
                    break
            emit({"event": "official_turn_finished", "case": case, "status": status,
                  "rate_limits_rpc_error": "error" in rpc["account/rateLimits/read"]})
            if status != "completed": raise RuntimeError("synthetic_turn_not_completed")
            if not probe_only:
                deadline = fixture.started + 205
                while time.monotonic() < deadline and not (fixture.cost_polled.is_set() and fixture.cost_metric.is_set()):
                    try: receive(timeout=1)
                    except queue.Empty: pass
                    if process.poll() is not None: raise RuntimeError("runtime_exited_before_poll")
            write(output / "rpc.json", rpc)
    except Exception as error:
        status, failure = "failed", type(error).__name__ + ": " + str(error)
    finally:
        if process is not None and process.poll() is None:
            process.stdin.close()
            try: process.wait(timeout=4)
            except subprocess.TimeoutExpired:
                os.killpg(process.pid, signal.SIGKILL); process.wait(timeout=3)
        fixture.close()
    cost_rows = [r for r in fixture.rows if "turn-costs" in r["path"] or "thread-estimates" in r["path"]]
    target_rows = [r for r in fixture.rows if any(s in r["path"] for s in ("settings/user", "plugins/featured", "/wham/usage"))]
    summary = {"case": case, "status": status, "error": failure, "thread_id": fixture.thread_id, "turn_id": fixture.turn_id,
        "response_id": fixture.response_id, "official_cost_request_count": len(cost_rows), "actual_turn_cost_query": fixture.cost_polled.is_set(),
        "cost_metric_observed": fixture.cost_metric.is_set(), "rate_limits_rpc": rpc.get("account/rateLimits/read"),
        "gate_requests": [{"method": row["method"], "path": row["path"], "authorization_present": any(k.lower() == "authorization" for k in row["headers"])} for row in target_rows],
        "methods": sorted(set(methods)), "blocked_connects": sum(r["method"] == "CONNECT" for r in fixture.rows),
        "real_credentials": False, "real_model": False, "elapsed_ms": round((time.monotonic() - fixture.started) * 1000)}
    summary["passed"] = status == "completed" and len(cost_rows) > 0 and (probe_only or fixture.cost_polled.is_set() and fixture.cost_metric.is_set())
    write(output / "summary.json", summary)
    return summary


def execute(args):
    if not args.isolated_net or os.name != "posix" or os.readlink("/proc/self/ns/net") == os.readlink("/proc/1/ns/net"):
        raise RuntimeError("fresh_isolated_network_namespace_required")
    subprocess.run(["ip", "link", "set", "lo", "up"], check=True)
    links = json.loads(subprocess.check_output(["ip", "-j", "link"]))
    routes = json.loads(subprocess.check_output(["ip", "-j", "route", "show", "table", "all"]))
    if any(x["ifname"] != "lo" for x in links) or any(x.get("dev") != "lo" for x in routes): raise RuntimeError("non_loopback_network_present")
    binary = args.codex.resolve()
    if hashlib.sha256(binary.read_bytes()).hexdigest() != PIN: raise RuntimeError("official_binary_pin_mismatch")
    args.output.mkdir(mode=0o700, parents=True, exist_ok=False)
    with ThreadPoolExecutor(max_workers=2) as pool:
        jobs = [pool.submit(run_case, binary, args.output, case, args.probe_only) for case in args.case]
        while not all(job.done() for job in jobs):
            # Remote heartbeat keeps the specific active process observable.
            emit({"event": "capture_running", "pid": os.getpid(), "unfinished_cases": sum(not j.done() for j in jobs)})
            threading.Event().wait(20)
        outcomes = [job.result() for job in jobs]
    result = {"schema": "codex-system-loopback-v1", "official_binary_sha256": PIN, "official_version": subprocess.check_output([str(binary), "--version"], text=True).strip(),
        "isolated_netns": os.readlink("/proc/self/ns/net"), "host_netns": os.readlink("/proc/1/ns/net"),
        "interfaces": links, "routes": routes, "only_loopback_available": True, "probe_only": args.probe_only, "outcomes": outcomes}
    write(args.output / "summary.json", result)
    emit({"event": "capture_finished", "passed": all(r["passed"] for r in outcomes), "outcomes": outcomes})
    return 0 if all(r["passed"] for r in outcomes) else 1


def export_oracle(folder):
    summary = json.loads((folder / "summary.json").read_text(encoding="utf-8"))
    run = json.loads((folder / "run.json").read_text(encoding="utf-8"))
    if summary["probe_only"] or any(not case["passed"] for case in summary["outcomes"]):
        raise RuntimeError("full_actual_turn_capture_required")
    cases = []
    for case in summary["outcomes"]:
        source = folder / case["case"] / "requests.json"
        records = json.loads(source.read_text(encoding="utf-8"))
        costs, gates, metric_points, metadata = [], [], [], []
        def visit(value):
            if isinstance(value, dict):
                if value.get("name") == "codex.turn.cost_microusd":
                    for point in value.get("sum", {}).get("dataPoints", []):
                        attributes = {x["key"]: x["value"].get("stringValue") for x in point.get("attributes", [])}
                        if attributes.get("turn.id") == case["turn_id"] and attributes.get("conversation.id") == case["thread_id"]:
                            metric_points.append({"asInt": point.get("asInt"), "attributes": attributes})
                for item in value.values(): visit(item)
            elif isinstance(value, list):
                for item in value: visit(item)
        for row in records:
            if "turn-costs" in row["path"] or "thread-estimates" in row["path"]:
                raw = base64.b64decode(row["body_base64"])
                if hashlib.sha256(raw).hexdigest() != row["body_sha256"]: raise RuntimeError("capture_body_sha256_mismatch")
                costs.append({"method": row["method"], "path": row["path"], "headers": row["headers"],
                    "body": row["body"], "body_utf8": raw.decode(), "body_sha256": row["body_sha256"],
                    "response": row["response_body"], "elapsed_ms": row["elapsed_ms"]})
            if any(path in row["path"] for path in ("settings/user", "plugins/featured")) or urlsplit(row["path"]).path.endswith("/wham/usage"):
                gates.append({"method": row["method"], "path": row["path"], "headers": row["headers"], "response": row["response_body"]})
            if row["path"].endswith("/responses") and row["method"] == "POST":
                header = next((v for k, v in row["headers"].items() if k.lower() == "x-codex-turn-metadata"), None)
                if header:
                    fields = json.loads(header)
                    metadata.append({k: fields[k] for k in ("thread_id", "turn_id", "session_id")})
            if row["path"].endswith("/otel/metrics"): visit(row["body"])
        if not any(point["asInt"] in (125001, "125001") for point in metric_points):
            raise RuntimeError("actual_cost_metric_amount_and_turn_unverified")
        if not any(x["thread_id"] == case["thread_id"] and x["turn_id"] == case["turn_id"] for x in metadata):
            raise RuntimeError("model_request_ids_do_not_match_cost_turn")
        cases.append({"case": case["case"], "thread_id": case["thread_id"], "turn_id": case["turn_id"],
            "response_id": case["response_id"], "source_sha256": hashlib.sha256(source.read_bytes()).hexdigest(),
            "cost_requests": costs, "gate_requests": gates, "model_request_ids": metadata,
            "actual_cost_metric_points": metric_points, "rate_limits_rpc": case["rate_limits_rpc"]})
    oracle = {"schema": "codex-system-cost-oracle-v1", "capture_id": folder.name,
        "official_binary_sha256": summary["official_binary_sha256"], "script_sha256": run["script_sha256"],
        "only_loopback_available": summary["only_loopback_available"], "synthetic_credentials_only": True,
        "responses_are_synthetic_backend_fixtures": True, "official_timer_seconds": 150, "cases": cases}
    write(folder / "writer-oracle.json", oracle)
    return oracle


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--remote", action="store_true")
    parser.add_argument("--isolated-net", action="store_true")
    parser.add_argument("--probe-only", action="store_true")
    parser.add_argument("--codex", type=Path, default=Path(BINARY))
    parser.add_argument("--output", type=Path)
    parser.add_argument("--export-run", type=Path)
    parser.add_argument("--case", action="append", choices=CASES)
    args = parser.parse_args(); args.case = args.case or CASES
    if args.export_run:
        oracle = export_oracle(args.export_run)
        emit({"oracle": str(args.export_run / "writer-oracle.json"), "cases": len(oracle["cases"]), "verified": True})
        return 0
    if not args.remote: return execute(args)
    from integration_env import ROOT, connect
    import shlex
    stamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%S.%fZ")
    digest = hashlib.sha256(Path(__file__).read_bytes()).hexdigest()
    remote_script = "/opt/codex-agent-integration/appserver-parity/artifacts/system-" + digest + ".py"
    remote_output = "/opt/codex-agent-integration/appserver-parity/system/" + stamp
    local = ROOT / ".local/appserver-parity/system" / stamp
    local.mkdir(parents=True)
    write(local / "run.json", {"remote_output": remote_output, "script_sha256": digest})
    client = connect()
    try:
        with client.open_sftp() as sftp:
            sftp.put(__file__, remote_script); sftp.chmod(remote_script, 0o600)
        command = ["unshare", "--net", "--", "python3", "-B", remote_script, "--isolated-net", "--output", remote_output]
        for case in args.case: command += ["--case", case]
        if args.probe_only: command += ["--probe-only"]
        _, out, err = client.exec_command(shlex.join(command), timeout=270)
        with (local / "events.jsonl").open("wb") as events:
            for line in out:
                events.write(line.encode()); events.flush()
                print(line.rstrip(), flush=True)
                try: event=json.loads(line)
                except ValueError: continue
                if event.get("event")=="capture_finished": break
        code = out.channel.recv_exit_status()
        errors = err.read()
        with client.open_sftp() as sftp:
            try: sftp.get(remote_output + "/summary.json", str(local / "summary.json"))
            except FileNotFoundError: pass
            for case in args.case:
                folder = local / case; folder.mkdir()
                for name in ("requests.json", "summary.json", "rpc.json", "stderr.log"):
                    try: sftp.get(remote_output + "/" + case + "/" + name, str(folder / name))
                    except FileNotFoundError: pass
        write(local / "run.json", {"remote_output": remote_output, "script_sha256": digest, "exit_code": code, "stderr_bytes": len(errors)})
        if code == 0 and not args.probe_only: export_oracle(local)
        emit({"local": str(local), "remote": remote_output, "exit_code": code, "stderr_bytes": len(errors)})
        return code
    finally:
        client.close()


if __name__ == "__main__": raise SystemExit(main())
