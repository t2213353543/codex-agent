#!/usr/bin/env python3
"""Exercise a gateway with the installed Codex CLI, including persisted resume."""

from __future__ import annotations

import argparse
from collections import Counter
import hashlib
import json
import math
import os
from pathlib import Path
import re
import secrets
import shutil
import signal
import subprocess
import sys
import tempfile
import time
from urllib.parse import urlsplit
import uuid


KEY_ENV = "CODEX_SMOKE_PROVIDER_KEY"


class SmokeFailure(Exception):
    """An expected validation or acceptance failure, safe to report."""


class Parser(argparse.ArgumentParser):
    def error(self, message: str) -> None:
        # argparse's default error can echo a secret passed as an unknown argument.
        raise SmokeFailure("invalid_arguments: use --help for supported arguments")


def parse_args(argv: list[str] | None = None) -> argparse.Namespace:
    parser = Parser(description=__doc__)
    parser.add_argument("--base-url", default=os.getenv("CODEX_GATEWAY_URL"),
                        help="Responses API base URL, e.g. http://127.0.0.1:8080/v1; env CODEX_GATEWAY_URL")
    parser.add_argument("--model", default=os.getenv("CODEX_GATEWAY_MODEL"),
                        help="gateway model ID; env CODEX_GATEWAY_MODEL (required)")
    parser.add_argument("--api-key-env", default="CODEX_GATEWAY_API_KEY",
                        help="environment variable holding the gateway key (default: CODEX_GATEWAY_API_KEY)")
    parser.add_argument("--api-key", help="explicit gateway key; prefer --api-key-env to avoid shell history")
    parser.add_argument("--codex", default="codex", help="installed CLI executable or npm launcher")
    parser.add_argument("--timeout", type=float, default=120,
                        help="timeout in seconds for EACH turn (default: 120)")
    parser.add_argument("--with-tools", action="store_true",
                        help="also require a real shell command to read a random temporary marker")
    parser.add_argument("--output", type=Path, help="also write the redacted JSON summary to this file")
    return parser.parse_args(argv)


def validate_settings(args: argparse.Namespace) -> str:
    if not args.base_url:
        raise SmokeFailure("missing_base_url: set --base-url or CODEX_GATEWAY_URL")
    try:
        parts = urlsplit(args.base_url)
        valid = parts.scheme in ("http", "https") and bool(parts.hostname)
        _ = parts.port
    except ValueError:
        valid = False
        parts = None
    if not valid or parts is None:
        raise SmokeFailure("invalid_base_url: expected an absolute http(s) API base URL")
    if parts.username or parts.password or parts.query or parts.fragment:
        raise SmokeFailure("invalid_base_url: userinfo, query strings and fragments are not accepted")
    if parts.path.rstrip("/").endswith("/responses"):
        raise SmokeFailure("invalid_base_url: supply the API base, without the /responses suffix")
    if not args.model or not args.model.strip():
        raise SmokeFailure("missing_model: set --model or CODEX_GATEWAY_MODEL")
    if not math.isfinite(args.timeout) or args.timeout <= 0:
        raise SmokeFailure("invalid_timeout: must be a positive finite number")
    key = args.api_key if args.api_key is not None else os.getenv(args.api_key_env)
    if not key or not key.strip():
        raise SmokeFailure("missing_api_key: set the environment variable selected by --api-key-env")
    if "\n" in key or "\r" in key:
        raise SmokeFailure("invalid_api_key: newline characters are not accepted")
    return key


def isolated_environment(temp_home: Path, key: str) -> dict[str, str]:
    # Do not inherit app-server/session routing, cloud credentials, or provider overrides.
    allowed = {
        "PATH", "PATHEXT", "SYSTEMROOT", "WINDIR", "COMSPEC", "TEMP", "TMP", "TMPDIR",
        "HOME", "USERPROFILE", "HOMEDRIVE", "HOMEPATH", "APPDATA", "LOCALAPPDATA",
        "PROGRAMFILES", "PROGRAMFILES(X86)", "PROGRAMDATA", "LANG", "LC_ALL", "SHELL",
        "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY", "SSL_CERT_FILE", "SSL_CERT_DIR",
        "NODE_EXTRA_CA_CERTS",
    }
    environment = {name: value for name, value in os.environ.items() if name.upper() in allowed}
    environment.update({"CODEX_HOME": str(temp_home), KEY_ENV: key, "NO_COLOR": "1"})
    bypass = [value for name, value in environment.items() if name.upper() == "NO_PROXY"]
    bypass.extend(["localhost", "127.0.0.1", "::1"])
    environment["NO_PROXY"] = environment["no_proxy"] = ",".join(bypass)
    return environment


def write_config(path: Path, args: argparse.Namespace) -> None:
    # JSON strings are valid TOML basic strings for the URL/model values accepted here.
    quoted = lambda value: json.dumps(value, ensure_ascii=False)
    config = f'''model = {quoted(args.model)}
model_provider = "gateway_smoke"
approval_policy = "never"
sandbox_mode = "read-only"
web_search = "disabled"
cli_auth_credentials_store = "file"

[model_providers.gateway_smoke]
name = "Gateway CLI smoke"
base_url = {quoted(args.base_url.rstrip("/"))}
env_key = "{KEY_ENV}"
wire_api = "responses"
supports_websockets = false
request_max_retries = 0
stream_max_retries = 0
stream_idle_timeout_ms = {max(1, int(min(args.timeout, 30) * 1000))}

[shell_environment_policy]
inherit = "core"
ignore_default_excludes = false
experimental_use_profile = false
'''
    if os.name == "nt":
        # An isolated home has no Windows onboarding preference. Explicitly
        # enable the native restricted-token sandbox instead of leaving command
        # execution disabled. Keep the read-only filesystem and never-approval
        # policy above; this does not install privileged sandbox users.
        config += '\n[windows]\nsandbox = "unelevated"\n'
    path.write_text(config, encoding="utf-8")


def stop_process_tree(process: subprocess.Popen) -> None:
    if os.name == "nt":
        subprocess.run(["taskkill", "/PID", str(process.pid), "/T", "/F"],
                       stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                       timeout=10, check=False)
    else:
        try:
            os.killpg(process.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
    if process.poll() is None:
        process.kill()


def run_process(command: list[str], prompt: str | None, workdir: Path,
                environment: dict[str, str], timeout: float) -> dict:
    started = time.monotonic()
    try:
        process = subprocess.Popen(command, cwd=workdir, env=environment,
                                   stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                   stderr=subprocess.PIPE, text=True, encoding="utf-8",
                                   errors="replace", start_new_session=os.name != "nt")
    except OSError as exc:
        raise SmokeFailure(f"cli_launch_failed: {type(exc).__name__}") from exc
    timed_out = False
    try:
        stdout, stderr = process.communicate(prompt, timeout=timeout)
    except subprocess.TimeoutExpired:
        timed_out = True
        stop_process_tree(process)
        stdout, stderr = process.communicate(timeout=10)
    except BaseException:
        stop_process_tree(process)
        process.communicate(timeout=10)
        raise
    return {"returncode": process.returncode, "timed_out": timed_out,
            "elapsed_seconds": round(time.monotonic() - started, 3),
            "stdout": stdout, "stderr": stderr}


def parse_events(stdout: str) -> list[dict]:
    events = []
    for line in stdout.splitlines():
        if not line.strip():
            continue
        try:
            event = json.loads(line)
        except json.JSONDecodeError as exc:
            raise SmokeFailure("invalid_jsonl: CLI stdout contains a non-JSON line") from exc
        if not isinstance(event, dict) or not isinstance(event.get("type"), str):
            raise SmokeFailure("invalid_jsonl: each event must have a string type")
        events.append(event)
    if not events:
        raise SmokeFailure("empty_jsonl: CLI emitted no JSON events")
    return events


def check_turn(process_result: dict, expected_reply: str, expected_thread: str | None = None,
               tool_marker: str | None = None) -> tuple[dict, str | None]:
    summary = {name: process_result[name] for name in ("returncode", "timed_out", "elapsed_seconds")}
    summary.update({"passed": False, "assertions": {}, "stderr_tail": process_result["stderr"][-4000:]})
    assertions = summary["assertions"]
    assertions["process_exited_successfully"] = process_result["returncode"] == 0 and not process_result["timed_out"]
    try:
        events = parse_events(process_result["stdout"])
    except SmokeFailure as exc:
        summary["failure"] = str(exc)
        return summary, None
    summary["event_counts"] = dict(Counter(event["type"] for event in events))
    threads = [event.get("thread_id") for event in events if event["type"] == "thread.started"]
    thread_id = threads[0] if len(threads) == 1 and isinstance(threads[0], str) else None
    try:
        valid_thread = thread_id is not None and str(uuid.UUID(thread_id)) == thread_id.lower()
    except ValueError:
        valid_thread = False
    assertions["thread_started"] = valid_thread
    if expected_thread is not None:
        assertions["same_thread_resumed"] = thread_id == expected_thread
    assertions["turn_started"] = any(event["type"] == "turn.started" for event in events)
    assertions["turn_completed"] = any(event["type"] == "turn.completed" for event in events)
    assertions["no_error_events"] = not any(event["type"] in ("turn.failed", "error") for event in events)
    completed = [event.get("item", {}) for event in events if event["type"] == "item.completed"]
    summary["completed_item_types"] = dict(Counter(item.get("type", "unknown") for item in completed))
    summary["tool_diagnostics"] = [
        {key: item[key] for key in ("type", "status", "error") if key in item}
        for item in completed if item.get("type") not in ("agent_message", "reasoning")
    ]
    messages = [item.get("text", "") for item in completed if item.get("type") == "agent_message"]
    assertions["final_reply_matches"] = bool(messages) and messages[-1].strip() == expected_reply
    commands = [item for item in completed if item.get("type") == "command_execution"]
    summary["completed_command_count"] = len(commands)
    if tool_marker is not None:
        assertions["shell_read_marker"] = any(
            item.get("status") == "completed" and item.get("exit_code") == 0
            and tool_marker in item.get("aggregated_output", "") for item in commands)
    else:
        # Memory must come from session history, not reading the saved rollout with a tool.
        assertions["no_commands_for_memory_check"] = not commands
    summary["error_messages"] = [
        event.get("message") or event.get("error", {}).get("message", "")
        for event in events if event["type"] in ("error", "turn.failed")
    ]
    summary["passed"] = all(assertions.values())
    return summary, thread_id


def redact(text: str, key: str | None) -> str:
    if key:
        text = text.replace(key, "[REDACTED]")
    text = re.sub(r"(?i)(Bearer\s+)[A-Za-z0-9_.~+/=-]+", r"\1[REDACTED]", text)
    text = re.sub(r"\bsk-[A-Za-z0-9_-]{8,}\b", "[REDACTED]", text)
    return text


def render_report(report: dict, key: str | None) -> str:
    # Redact values before JSON serialization, including keys with quotes/backslashes.
    def clean(value):
        if isinstance(value, str):
            return redact(value, key)
        if isinstance(value, dict):
            return {name: clean(item) for name, item in value.items()}
        if isinstance(value, list):
            return [clean(item) for item in value]
        return value

    return json.dumps(clean(report), ensure_ascii=False, indent=2)


def prepare_readable_workspace(root: Path, workspace: Path) -> None:
    if os.name != "nt":
        return
    # Python's fresh temporary directory has an owner-only Windows ACL, which
    # blocks the native restricted token even under a read-only sandbox. Grant
    # read/traverse only on this disposable synthetic workspace. Do not inherit
    # the grant into the sibling CLI home (which holds conversation history).
    for target, grant in ((root, "*S-1-1-0:(RX)"),
                          (workspace, "*S-1-1-0:(OI)(CI)(RX)")):
        try:
            result = subprocess.run(["icacls.exe", str(target), "/grant", grant],
                                    capture_output=True, timeout=10, check=False)
        except (OSError, subprocess.TimeoutExpired) as exc:
            raise SmokeFailure("temporary_workspace_acl_failed") from exc
        if result.returncode != 0:
            raise SmokeFailure("temporary_workspace_acl_failed")


def run_smoke(args: argparse.Namespace, key: str, report: dict) -> None:
    executable = shutil.which(args.codex)
    if not executable:
        raise SmokeFailure("cli_not_found: install Codex CLI or provide --codex")
    report.update({"base_url": args.base_url.rstrip("/"), "model": args.model,
                   "with_tools": args.with_tools, "timeout_per_turn_seconds": args.timeout})
    with tempfile.TemporaryDirectory(prefix="codex-gateway-smoke-") as temporary:
        root = Path(temporary)
        temp_home, workdir = root / "codex-home", root / "workspace"
        temp_home.mkdir()
        workdir.mkdir()
        if args.with_tools:
            prepare_readable_workspace(root, workdir)
        environment = isolated_environment(temp_home, key)
        write_config(temp_home / "config.toml", args)
        version = run_process([executable, "--version"], None, workdir, environment, min(args.timeout, 15))
        if version["returncode"] != 0 or version["timed_out"]:
            raise SmokeFailure("cli_version_failed: cannot read the installed CLI version")
        report["cli_version"] = version["stdout"].strip()
        if not report["cli_version"].startswith("codex-cli "):
            raise SmokeFailure("unexpected_cli: --version did not identify codex-cli")
        flags = ["--json", "--skip-git-repo-check", "--ignore-rules"]
        marker = "memory_" + secrets.token_hex(16)
        initial_prompt = ("This is a gateway acceptance test. Remember this marker in conversation history: "
                          + marker + ". Do not use any tools or write any files. "
                          "Reply with exactly the marker, with no quotes or other text.")
        result = run_process([executable, "exec", *flags, "-"], initial_prompt,
                             workdir, environment, args.timeout)
        first, thread_id = check_turn(result, marker)
        report["checks"].append({"name": "initial_session", **first})
        if not first["passed"]:
            raise SmokeFailure("initial_session_failed: inspect checks[].assertions and stderr_tail")
        report["thread_id_sha256"] = hashlib.sha256(thread_id.encode()).hexdigest()
        resume_prompt = ("Return exactly the marker I asked you to remember in our previous turn. "
                         "Use only conversation history. Do not use tools or read files. "
                         "No quotes, explanation or other text.")
        result = run_process([executable, "exec", "resume", *flags, thread_id, "-"],
                             resume_prompt, workdir, environment, args.timeout)
        resumed, _ = check_turn(result, marker, expected_thread=thread_id)
        report["checks"].append({"name": "resume_memory", **resumed})
        if not resumed["passed"]:
            raise SmokeFailure("resume_memory_failed: inspect checks[].assertions and stderr_tail")
        if args.with_tools:
            tool_marker = "shell_" + secrets.token_hex(16)
            marker_file = workdir / "gateway-smoke-marker.txt"
            marker_file.write_text(tool_marker + "\n", encoding="utf-8")
            tool_prompt = ("Use your shell command tool to read gateway-smoke-marker.txt in the current "
                           "working directory. The file holds a random marker you have not been given. "
                           "Do not change any files, inspect other directories, or use the network. "
                           "Reply with exactly that file's marker, with no quotes or other text.")
            result = run_process([executable, "exec", "resume", *flags, thread_id, "-"],
                                 tool_prompt, workdir, environment, args.timeout)
            tool_check, _ = check_turn(result, tool_marker, expected_thread=thread_id, tool_marker=tool_marker)
            tool_check["assertions"]["marker_file_unchanged"] = marker_file.read_text(encoding="utf-8") == tool_marker + "\n"
            tool_check["passed"] = all(tool_check["assertions"].values())
            report["checks"].append({"name": "shell_tool", **tool_check})
            if not tool_check["passed"]:
                raise SmokeFailure("shell_tool_failed: inspect checks[].assertions and stderr_tail")
        report["passed"] = True


def main(argv: list[str] | None = None) -> int:
    report = {"schema_version": 1, "passed": False, "checks": []}
    args = None
    key = None
    started = time.monotonic()
    exit_code = 1
    try:
        args = parse_args(argv)
        key = args.api_key if args.api_key is not None else os.getenv(args.api_key_env)
        key = validate_settings(args)
        run_smoke(args, key, report)
        exit_code = 0
    except SmokeFailure as exc:
        report["failure"] = str(exc)
    except KeyboardInterrupt:
        report["failure"] = "interrupted"
        exit_code = 130
    except Exception as exc:
        # Keep unexpected exception details out of logs: a transport may include credentials.
        report["failure"] = f"unexpected_error: {type(exc).__name__}"
    report["elapsed_seconds"] = round(time.monotonic() - started, 3)
    rendered = render_report(report, key)
    if args is not None and args.output:
        try:
            args.output.write_text(rendered + "\n", encoding="utf-8")
        except OSError:
            report["passed"] = False
            report["failure"] = "report_write_failed: cannot write --output path"
            rendered = render_report(report, key)
            exit_code = 1
    print(rendered)
    return exit_code


if __name__ == "__main__":
    sys.exit(main())
