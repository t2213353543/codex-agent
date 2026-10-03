#!/usr/bin/env python3
"""Native Codex capture driver; Python 3.9+, private server artifacts only.

Uses the same APP_SERVER_PARITY_* / BPS_LIVE_* environment as the gateway
capture test. It never retrieves credentials or changes production settings.
Run once per isolated route. Only fixed labels, counts and hashes reach stdout.
"""
import argparse
import collections
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import queue
import re
import signal
import subprocess
import sys
import threading
import time
import traceback
from urllib.parse import urlsplit


INSTRUCTIONS = "Complete only the supplied request. Use tools only when explicitly requested."
TEXT_PROMPT = "Remember the marker PARITY_MARKER. Reply exactly SAVED without tools."
RESUME_PROMPT = "Reply exactly the remembered marker without tools."
TOOL_PROMPT = "Call lookup_marker once, then reply only with its returned text."
SEARCH_PROMPT = "Search OpenAI official documentation for the Codex CLI installation command. Give the command and one source URL. Do not run shell commands."
TOOL = {"type": "function", "name": "lookup_marker", "description": "Retrieve the marker.",
        "inputSchema": {"type": "object", "properties": {}, "additionalProperties": False}}


class DriverError(Exception):
    """Messages are fixed internal codes, never upstream text."""


def encode(value):
    return json.dumps(value, ensure_ascii=False, sort_keys=True).encode("utf-8")


def sha(value):
    return hashlib.sha256(value.encode() if isinstance(value, str) else value).hexdigest()


def write_private(path, value):
    raw = value if isinstance(value, bytes) else encode(value)
    temporary = path.with_name(path.name + ".tmp")
    flags = os.O_WRONLY | os.O_CREAT | os.O_TRUNC | getattr(os, "O_NOFOLLOW", 0)
    with os.fdopen(os.open(str(temporary), flags, 0o600), "wb") as handle:
        handle.write(raw)
    os.replace(str(temporary), str(path))


def private_open(path):
    return os.fdopen(os.open(str(path), os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600), "wb")


def counter_safe(counter):
    return {key: value for key, value in counter.items()
            if isinstance(key, str) and re.fullmatch(r"[A-Za-z0-9_./-]{1,96}", key)}


def stop(process):
    if process.poll() is None:
        try:
            if os.name == "posix":
                os.killpg(process.pid, signal.SIGTERM)
            else:
                process.terminate()
            process.wait(timeout=5)
        except (OSError, subprocess.TimeoutExpired):
            if os.name == "posix":
                try:
                    os.killpg(process.pid, signal.SIGKILL)
                except ProcessLookupError:
                    pass
            else:
                process.kill()
            process.wait(timeout=5)


class RPC:
    def __init__(self, driver):
        self.driver, self.number = driver, 0
        self.pending, self.messages = [], queue.Queue()
        self.journal = private_open(driver.private / "app-rpc.jsonl")
        self.stderr = private_open(driver.private / "app-stderr.log")
        self.lock = threading.Lock()
        self.process = subprocess.Popen([str(driver.binary), "app-server"], cwd=str(driver.work),
            env=driver.environment(), stdin=subprocess.PIPE, stdout=subprocess.PIPE,
            stderr=self.stderr, start_new_session=os.name == "posix")
        self.reader = threading.Thread(target=self.read_loop, daemon=True)
        self.reader.start()

    def log(self, direction, message):
        with self.lock:
            self.journal.write(encode({"direction": direction, "message": message}) + b"\n")
            self.journal.flush()

    def read_loop(self):
        try:
            for line in self.process.stdout:
                try:
                    message = json.loads(line)
                except (ValueError, UnicodeError):
                    self.messages.put(DriverError("invalid_rpc_json"))
                    return
                self.log("receive", message)
                self.messages.put(message)
        finally:
            self.messages.put(None)

    def send(self, message):
        self.log("send", message)
        self.process.stdin.write(encode(message) + b"\n")
        self.process.stdin.flush()

    def receive(self, deadline):
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise DriverError("rpc_deadline")
        try:
            message = self.messages.get(timeout=remaining)
        except queue.Empty:
            raise DriverError("rpc_deadline") from None
        if message is None:
            raise DriverError("rpc_eof")
        if isinstance(message, Exception):
            raise message
        if "id" in message and "method" in message:
            params = message.get("params", {})
            if message["method"] == "item/tool/call" and params.get("tool") == "lookup_marker":
                if params.get("arguments") not in ({}, "{}"):
                    raise DriverError("unexpected_tool_arguments")
                self.driver.tool_calls += 1
                self.driver.phase("function-result")
                self.send({"id": message["id"], "result": {
                    "contentItems": [{"type": "inputText", "text": "PARITY_TOOL_MARKER"}], "success": True}})
            else:
                self.send({"id": message["id"], "error": {
                    "code": -32000, "message": "Unsupported isolated capture request"}})
                raise DriverError("unexpected_server_request")
        return message

    def call(self, method, params, timeout=90):
        self.number += 1
        number = self.number
        self.send({"id": number, "method": method, "params": params})
        deadline = time.monotonic() + timeout
        while True:
            message = self.receive(deadline)
            if message.get("id") == number and "method" not in message:
                if "error" in message:
                    raise DriverError("rpc_error")
                return message.get("result", {})
            self.pending.append(message)

    def next_event(self, deadline):
        return self.pending.pop(0) if self.pending else self.receive(deadline)

    def close(self):
        if self.process.stdin:
            self.process.stdin.close()
        try:
            self.process.wait(timeout=5)
        except subprocess.TimeoutExpired:
            stop(self.process)
        self.reader.join(timeout=5)
        self.journal.close()
        self.stderr.close()


class Driver:
    def __init__(self, args):
        self.args, self.route = args, args.route
        self.output, self.work, self.binary = args.output.resolve(), args.work_root.resolve(), args.codex.resolve()
        self.home, self.private = self.work / "native-codex-home", self.output / "private"
        for path in (self.output, self.work, self.home, self.private):
            path.mkdir(mode=0o700, parents=True, exist_ok=True)
            if os.name == "posix":
                path.chmod(0o700)
        fixture = json.loads(args.accounts_file.read_text(encoding="utf-8"))
        accounts = fixture.get("accounts", [])
        if not accounts or not accounts[0].get("access_token") or not accounts[0].get("account_id"):
            raise DriverError("invalid_private_account_fixture")
        self.account = accounts[0]
        self.rows, self.tool_calls = [], 0
        self.instructions = self.work / "base-instructions.txt"
        write_private(self.instructions, INSTRUCTIONS.encode())
        self.configure()

    def environment(self):
        env = {key: value for key, value in os.environ.items()
               if key in ("PATH", "LANG", "LC_ALL", "TZ", "SYSTEMROOT", "WINDIR", "PATHEXT")}
        env.update(CODEX_HOME=str(self.home), NO_COLOR="1", RUST_LOG="off",
                   CODEX_CA_CERTIFICATE=str(self.args.ca), SSL_CERT_FILE=str(self.args.ca),
                   REQUESTS_CA_BUNDLE=str(self.args.ca), CURL_CA_BUNDLE=str(self.args.ca),
                   CODEX_REFRESH_TOKEN_URL_OVERRIDE="http://127.0.0.1:9/refresh-disabled",
                   NO_PROXY="127.0.0.1,localhost,::1", no_proxy="127.0.0.1,localhost,::1")
        for key in ("HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"):
            env[key] = self.args.proxy
        return env

    def configure(self, web=False, mcp=False):
        settings = {
            "model": self.args.model, "model_provider": "openai", "model_reasoning_effort": self.args.effort,
            "approval_policy": "never", "sandbox_mode": "read-only", "web_search": "live" if web else "disabled",
            "cli_auth_credentials_store": "file", "project_doc_max_bytes": 0,
            "model_instructions_file": str(self.instructions), "history.persistence": "none",
            "analytics.enabled": False, "feedback.enabled": False, "skills.include_instructions": False,
            "skills.bundled.enabled": False, "features.skip_host_skill_discovery": True,
            "tools.update_plan.enabled": False, "tools.experimental_request_user_input.enabled": False,
        }
        for feature in ("shell_tool", "view_image", "js_repl", "code_mode", "apps", "multi_agent",
                        "multi_agent_v2", "image_generation", "hooks", "plugins", "remote_plugin"):
            settings["features." + feature] = False
        lines = [key + " = " + json.dumps(value, ensure_ascii=False) for key, value in settings.items()]
        if mcp:
            server = self.work / "lookup_marker_fixture.py"
            write_private(server, Path(__file__).read_bytes())
            lines.extend(["", "[mcp_servers.parity_fixture]", "command = " + json.dumps(sys.executable),
                          "args = " + json.dumps([str(server), "--mcp-tool-server", str(self.output / "phase.json"), self.route]),
                          "startup_timeout_sec = 20", "tool_timeout_sec = 20",
                          "[mcp_servers.parity_fixture.tools.lookup_marker]", 'approval_mode = "approve"'])
        write_private(self.home / "config.toml", ("\n".join(lines) + "\n").encode())

    def phase(self, case):
        write_private(self.output / "phase.json", {"route": self.route, "case": case})

    def record(self, case, started, **fields):
        row = {"case": case, "route": self.route, "elapsed_ms": round((time.monotonic() - started) * 1000),
               "model": self.args.model, "effort": self.args.effort, "instructions_sha256": sha(INSTRUCTIONS)}
        row.update(fields)
        self.rows.append(row)
        write_private(self.output / "driver-summary.json", self.rows)
        print(json.dumps(row, sort_keys=True), flush=True)
        return row

    def thread_params(self, web=False, dynamic=False):
        params = {"model": self.args.model, "modelProvider": "openai", "cwd": str(self.work),
                  "approvalPolicy": "never", "sandbox": "read-only", "ephemeral": False,
                  "experimentalRawEvents": True, "baseInstructions": INSTRUCTIONS,
                  "config": {"web_search": "live" if web else "disabled", "model_reasoning_effort": self.args.effort},
                  "dynamicTools": [TOOL] if dynamic else []}
        return params

    def app_turn(self, rpc, thread, case, prompt):
        self.phase(case)
        started = time.monotonic()
        before = self.tool_calls
        kickoff = rpc.call("turn/start", {"threadId": thread, "input": [{"type": "text", "text": prompt}],
                                         "model": self.args.model, "effort": self.args.effort})
        turn_id = kickoff["turn"]["id"]
        methods, types, texts = collections.Counter(), collections.Counter(), []
        terminal = None
        deadline = time.monotonic() + self.args.timeout
        while terminal is None:
            event = rpc.next_event(deadline)
            method, params = event.get("method", ""), event.get("params", {})
            if params.get("threadId") not in (None, thread) or params.get("turnId") not in (None, turn_id):
                continue
            methods[method] += 1
            if method == "item/completed":
                item = params.get("item", {})
                types[item.get("type", "unknown")] += 1
                if item.get("type") == "agentMessage" and item.get("phase") in (None, "final_answer"):
                    texts.append(item.get("text", ""))
            if method == "turn/completed" and params.get("turn", {}).get("id") == turn_id:
                terminal = params["turn"]
        answer = "\n".join(texts)
        fields = {"status": "completed" if terminal.get("status") == "completed" else "failed",
                  "prompt_sha256": sha(prompt), "answer_sha256": sha(answer), "answer_bytes": len(answer.encode()),
                  "thread_sha256": sha(thread), "event_counts": counter_safe(methods), "item_types": counter_safe(types),
                  "dynamic_tool_calls": self.tool_calls - before,
                  "terminal_error": terminal.get("error") is not None}
        if case == "text":
            fields["expected_marker_match"] = answer.strip() == "SAVED"
        elif case in ("resume", "compact-resume"):
            fields["expected_marker_match"] = answer.strip() == "PARITY_MARKER"
        elif case == "function-call":
            fields["expected_marker_match"] = answer.strip() == "PARITY_TOOL_MARKER"
        self.record(case, started, **fields)
        if fields["status"] != "completed" or fields["terminal_error"]:
            raise DriverError("app_turn_failed")
        if case == "function-call":
            self.record("function-result", started, status="completed", mechanism="dynamic_tool_rpc",
                        dynamic_tool_calls=self.tool_calls - before, answer_sha256=sha(answer))
            if self.tool_calls - before != 1:
                raise DriverError("required_dynamic_tool_not_called_once")
        if fields.get("expected_marker_match") is False:
            raise DriverError("marker_mismatch")
        if case == "web-search" and not any("web" in key.lower() for key in types):
            raise DriverError("web_search_not_observed")

    def app(self):
        self.phase("initialize")
        rpc = RPC(self)
        try:
            started = time.monotonic()
            rpc.call("initialize", {"clientInfo": {"name": "codex-parity-native", "version": "1.0.0"},
                                    "capabilities": {"experimentalApi": True}})
            rpc.send({"method": "initialized", "params": {}})
            self.phase("login")
            rpc.call("account/login/start", {"type": "chatgptAuthTokens", "accessToken": self.account["access_token"],
                                           "chatgptAccountId": self.account["account_id"]})
            self.record("initialize-login", started, status="completed", auth_mode="chatgptAuthTokens")
            self.phase("models")
            started = time.monotonic()
            models = rpc.call("model/list", {"limit": 100})
            self.record("models", started, status="completed", model_count=len(models.get("data", [])))
            self.phase("thread-start")
            result = rpc.call("thread/start", self.thread_params())
            thread = result["thread"]["id"]
            if result.get("model") != self.args.model or result.get("reasoningEffort") != self.args.effort:
                raise DriverError("thread_model_effort_mismatch")
            self.app_turn(rpc, thread, "text", TEXT_PROMPT)
            self.phase("resume")
            resumed = rpc.call("thread/resume", {"threadId": thread})
            if resumed.get("thread", {}).get("id") != thread:
                raise DriverError("thread_resume_identity_mismatch")
            self.app_turn(rpc, thread, "resume", RESUME_PROMPT)
            if self.args.pilot:
                return
            self.phase("compact")
            started = time.monotonic()
            rpc.call("thread/compact/start", {"threadId": thread})
            deadline = time.monotonic() + self.args.timeout
            compacted, completed = False, False
            while not completed:
                event = rpc.next_event(deadline)
                params = event.get("params", {})
                if params.get("threadId") not in (None, thread):
                    continue
                compacted |= event.get("method") == "thread/compacted"
                item = params.get("item", {})
                compacted |= event.get("method") == "item/completed" and item.get("type") == "contextCompaction"
                if event.get("method") == "turn/completed":
                    if params.get("turn", {}).get("status") != "completed":
                        raise DriverError("compact_turn_failed")
                    completed = True
            self.record("compact", started, status="completed", mechanism="thread_compact_start", compact_event=compacted)
            if not compacted:
                raise DriverError("compact_event_not_observed")
            self.app_turn(rpc, thread, "compact-resume", RESUME_PROMPT)
            self.phase("function-call")
            tool_thread = rpc.call("thread/start", self.thread_params(dynamic=True))["thread"]["id"]
            self.app_turn(rpc, tool_thread, "function-call", TOOL_PROMPT)
            self.phase("web-search")
            web_thread = rpc.call("thread/start", self.thread_params(web=True))["thread"]["id"]
            self.app_turn(rpc, web_thread, "web-search", SEARCH_PROMPT)
        finally:
            rpc.close()

    def exec_turn(self, case, prompt, thread=None, compact=False):
        self.phase(case)
        command = [str(self.binary), "exec", "--json", "--skip-git-repo-check", "-m", self.args.model,
                   "-c", "model_reasoning_effort=" + json.dumps(self.args.effort)]
        if compact:
            command.extend(["-c", "model_auto_compact_token_limit=1"])
        command.extend(["resume", thread, "-"] if thread else ["-"])
        started = time.monotonic()
        outpath, errpath = self.private / (case + "-events.jsonl"), self.private / (case + "-stderr.log")
        with private_open(outpath) as output, private_open(errpath) as errors:
            process = subprocess.Popen(command, cwd=str(self.work), env=self.environment(), stdin=subprocess.PIPE,
                                       stdout=output, stderr=errors, start_new_session=os.name == "posix")
            try:
                process.communicate(prompt.encode(), timeout=self.args.timeout)
            except subprocess.TimeoutExpired:
                stop(process)
                raise DriverError("exec_deadline") from None
            finally:
                stop(process)
        kinds, types, texts, found_thread = collections.Counter(), collections.Counter(), [], None
        for raw in outpath.read_bytes().splitlines():
            try:
                event = json.loads(raw)
            except (ValueError, UnicodeError):
                raise DriverError("invalid_exec_json") from None
            kind = event.get("type", "unknown")
            kinds[kind] += 1
            if kind == "thread.started":
                found_thread = event.get("thread_id")
            if kind == "item.completed":
                item = event.get("item", {})
                types[item.get("type", "unknown")] += 1
                if item.get("type") == "agent_message":
                    texts.append(item.get("text", ""))
        answer = "\n".join(texts)
        passed = process.returncode == 0 and kinds["turn.completed"] >= 1 and not kinds["turn.failed"]
        fields = {"status": "completed" if passed else "failed", "exit_code": process.returncode,
                  "prompt_sha256": sha(prompt), "answer_sha256": sha(answer), "answer_bytes": len(answer.encode()),
                  "event_counts": counter_safe(kinds), "item_types": counter_safe(types),
                  "thread_sha256": sha(found_thread) if found_thread else None}
        expected = {"text": "SAVED", "resume": "PARITY_MARKER", "compact": "PARITY_MARKER",
                    "compact-resume": "PARITY_MARKER", "function-call": "PARITY_TOOL_MARKER"}.get(case)
        if expected:
            fields["expected_marker_match"] = answer.strip() == expected
        if compact:
            fields.update(mechanism="exec_resume_auto_compact_limit_1", compact_wire_verification_required=True)
        self.record(case, started, **fields)
        if not passed or not found_thread:
            raise DriverError("exec_turn_failed")
        if thread and found_thread != thread:
            raise DriverError("exec_resume_identity_mismatch")
        if expected and answer.strip() != expected:
            raise DriverError("marker_mismatch")
        if case == "function-call":
            calls = types["mcp_tool_call"]
            self.record("function-result", started, status="completed" if calls == 1 else "failed",
                        mechanism="local_stdio_mcp_fixture", mcp_tool_calls=calls, answer_sha256=sha(answer))
            if calls != 1:
                raise DriverError("required_mcp_tool_not_called_once")
        if case == "web-search" and not any("web" in key.lower() for key in types):
            raise DriverError("web_search_not_observed")
        return found_thread

    def cli(self):
        write_private(self.home / "auth.json", {"auth_mode": "chatgpt", "OPENAI_API_KEY": None,
            "tokens": {"id_token": self.account.get("id_token") or self.account["access_token"],
                       "access_token": self.account["access_token"], "refresh_token": "", "account_id": self.account["account_id"]},
            "last_refresh": datetime.now(timezone.utc).isoformat()})
        thread = self.exec_turn("text", TEXT_PROMPT)
        self.exec_turn("resume", RESUME_PROMPT, thread=thread)
        if self.args.pilot:
            return
        self.exec_turn("compact", RESUME_PROMPT, thread=thread, compact=True)
        self.exec_turn("compact-resume", RESUME_PROMPT, thread=thread)
        self.configure(mcp=True)
        self.exec_turn("function-call", TOOL_PROMPT)
        self.configure(web=True)
        self.exec_turn("web-search", SEARCH_PROMPT)


def mcp_fixture(phase_path, route):
    """A workspace-only MCP fixture; does not execute commands or access a network."""
    for raw in sys.stdin.buffer:
        message = json.loads(raw)
        if "id" not in message:
            continue
        method = message.get("method")
        response = {"jsonrpc": "2.0", "id": message["id"]}
        if method == "initialize":
            response["result"] = {"protocolVersion": message.get("params", {}).get("protocolVersion", "2024-11-05"),
                                  "capabilities": {"tools": {}}, "serverInfo": {"name": "parity_fixture", "version": "1.0.0"}}
        elif method == "tools/list":
            tool = {key: value for key, value in TOOL.items() if key != "type"}
            tool["annotations"] = {"readOnlyHint": True, "destructiveHint": False,
                                   "idempotentHint": True, "openWorldHint": False}
            response["result"] = {"tools": [tool]}
        elif method == "tools/call" and message.get("params", {}).get("name") == "lookup_marker":
            write_private(Path(phase_path), {"route": route, "case": "function-result"})
            response["result"] = {"content": [{"type": "text", "text": "PARITY_TOOL_MARKER"}], "isError": False}
        elif method == "ping":
            response["result"] = {}
        else:
            response["error"] = {"code": -32601, "message": "Method not found"}
        sys.stdout.buffer.write(encode(response) + b"\n")
        sys.stdout.buffer.flush()


def arguments():
    parser = argparse.ArgumentParser(description=__doc__)
    for flag, variable in (("accounts-file", "BPS_LIVE_ACCOUNTS_FILE"), ("codex", "BPS_LIVE_CODEX_BINARY"),
                           ("work-root", "BPS_LIVE_WORK_ROOT"), ("output", "APP_SERVER_PARITY_OUTPUT"),
                           ("ca", "APP_SERVER_PARITY_CA")):
        parser.add_argument("--" + flag, type=Path, default=os.environ.get(variable), required=not os.environ.get(variable))
    parser.add_argument("--route", choices=("native_app_server", "native_codex_exec", "native_codex_cli"),
                        default=os.environ.get("APP_SERVER_PARITY_ROUTE"), required=not os.environ.get("APP_SERVER_PARITY_ROUTE"))
    parser.add_argument("--proxy", default=os.environ.get("APP_SERVER_PARITY_PROXY"), required=not os.environ.get("APP_SERVER_PARITY_PROXY"))
    parser.add_argument("--model", default=os.environ.get("APP_SERVER_PARITY_MODEL", "gpt-6-astra"))
    parser.add_argument("--effort", default=os.environ.get("APP_SERVER_PARITY_EFFORT", "xhigh"))
    parser.add_argument("--pilot", action="store_true", default=os.environ.get("APP_SERVER_PARITY_PILOT") == "1")
    parser.add_argument("--timeout", type=int, default=240)
    args = parser.parse_args()
    proxy = urlsplit(args.proxy)
    if proxy.scheme != "http" or proxy.hostname != "127.0.0.1" or proxy.username or proxy.password or not proxy.port:
        parser.error("proxy must be the dedicated loopback HTTP listener")
    if not args.ca.is_file() or not args.codex.is_file() or args.timeout <= 0:
        parser.error("CA, Codex binary and positive timeout are required")
    return args


def main():
    if len(sys.argv) == 4 and sys.argv[1] == "--mcp-tool-server":
        mcp_fixture(sys.argv[2], sys.argv[3])
        return 0
    args = arguments()
    def interrupted(signum, frame):
        raise DriverError("driver_interrupted")
    for signum in (signal.SIGINT, signal.SIGTERM):
        signal.signal(signum, interrupted)
    driver = None
    try:
        driver = Driver(args)
        if args.route == "native_app_server":
            driver.app()
        else:
            driver.cli()
        return 0
    except Exception as error:
        code = str(error) if isinstance(error, DriverError) else "driver_exception"
        if driver is not None:
            write_private(driver.private / "driver-error.log", traceback.format_exc().encode())
            driver.record("driver-failure", time.monotonic(), status="failed", error_code=code)
        else:
            print(json.dumps({"status": "failed", "error_code": code}), flush=True)
        return 1
    finally:
        if driver is not None:
            for path in driver.home.rglob("auth.json"):
                path.unlink()


if __name__ == "__main__":
    raise SystemExit(main())
