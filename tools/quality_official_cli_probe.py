#!/usr/bin/env python3
"""Run an isolated, server-side official Codex 0.155.0 quality reference.

Credentials never enter argv, inherited environment, retained logs, or reports.
The prompt is sent unchanged on stdin; this tool neither supplies nor grades an
expected answer. See docs/quality-20260924/04-cli-reference.md for limitations.
"""
from __future__ import annotations

import argparse
import base64
from collections import Counter
from datetime import datetime, timezone
import hashlib
import json
import math
import os
from pathlib import Path
import re
import signal
import stat
import subprocess
import sys
import tempfile
import threading
import time

VERSION = "0.155.0"
MAX_OUTPUT = 2 * 1024 * 1024
MAX_INPUT = 64 * 1024
AUTH_CLAIM = "https://api.openai.com/auth"
REFRESH_DISABLED = "http://127.0.0.1:9/refresh-disabled"
EVENT_TYPES = {"thread.started", "turn.started", "turn.completed", "turn.failed",
               "item.started", "item.updated", "item.completed", "error"}
PASSIVE_ITEMS = {"agent_message", "reasoning", "error"}
TOOL_ITEMS = {"command_execution", "file_change", "mcp_tool_call", "collab_tool_call",
              "web_search", "todo_list"}
USAGE_KEYS = {"input_tokens", "cached_input_tokens", "cache_write_input_tokens",
              "output_tokens", "reasoning_output_tokens"}


class ProbeFailure(Exception):
    """Only fixed, safe error codes may be raised through this boundary."""


class Parser(argparse.ArgumentParser):
    def error(self, message):
        raise ProbeFailure("invalid_arguments")


def parse_args(argv=None):
    parser = Parser(description=__doc__)
    parser.add_argument("--credential-file", required=True, type=Path,
                        help="private JSON: access_token, account_id, optional exp and id_token")
    parser.add_argument("--cli", required=True, type=Path, help="absolute official native CLI path")
    parser.add_argument("--prompt-file", required=True, type=Path)
    parser.add_argument("--model", required=True)
    parser.add_argument("--effort", choices=("minimal", "low", "medium", "high", "xhigh"), default="medium")
    parser.add_argument("--websocket", choices=("default", "true", "false"), default="default",
                        help="default/true preserves built-in provider and its HTTP fallback; false uses a custom HTTP reference")
    parser.add_argument("--output-dir", required=True, type=Path)
    parser.add_argument("--timeout", type=float, default=240)
    return parser.parse_args(argv)


def read_small(path, limit=MAX_INPUT):
    try:
        with path.open("rb") as handle:
            data = handle.read(limit + 1)
        if len(data) > limit:
            raise ProbeFailure("input_too_large")
        return data.decode("utf-8-sig")
    except (OSError, UnicodeError) as exc:
        raise ProbeFailure("input_read_failed") from exc


def jwt_claims(token):
    if not isinstance(token, str) or len(token) > MAX_INPUT or re.search(r"\s", token):
        raise ProbeFailure("invalid_token")
    pieces = token.split(".")
    if len(pieces) != 3 or not all(pieces):
        raise ProbeFailure("invalid_token")
    try:
        encoded = pieces[1]
        claims = json.loads(base64.b64decode(encoded + "=" * (-len(encoded) % 4), altchars=b"-_", validate=True),
                            object_pairs_hook=unique_json_object)
    except (ValueError, UnicodeError) as exc:
        raise ProbeFailure("invalid_token_claims") from exc
    if not isinstance(claims, dict):
        raise ProbeFailure("invalid_token_claims")
    return claims


def load_credential(path, timeout, now=None):
    # Validate the opened descriptor, not a path followed again after stat.
    flags = os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0) | getattr(os, "O_NONBLOCK", 0)
    try:
        with os.fdopen(os.open(path, flags), "rb") as handle:
            details = os.fstat(handle.fileno())
            if not stat.S_ISREG(details.st_mode):
                raise ProbeFailure("credential_file_must_be_regular")
            if os.name == "posix" and (details.st_mode & 0o077 or details.st_uid != os.geteuid()):
                raise ProbeFailure("credential_file_must_be_private_0600")
            raw = handle.read(MAX_INPUT + 1)
        if len(raw) > MAX_INPUT:
            raise ProbeFailure("input_too_large")
        credential = json.loads(raw.decode("utf-8-sig"), object_pairs_hook=unique_json_object)
    except OSError as exc:
        raise ProbeFailure("credential_read_failed") from exc
    except (ValueError, TypeError) as exc:
        raise ProbeFailure("invalid_credential_json") from exc
    if not isinstance(credential, dict):
        raise ProbeFailure("invalid_credential_json")
    # Explicitly reject RT-bearing inputs, including copied auth.json containers.
    if "refresh_token" in credential or "tokens" in credential:
        raise ProbeFailure("credential_must_not_contain_refresh_token_or_auth_container")
    if set(credential) - {"access_token", "account_id", "exp", "id_token"}:
        raise ProbeFailure("credential_has_unknown_fields")
    access_token = credential.get("access_token")
    claims = jwt_claims(access_token)
    account_id = credential.get("account_id")
    if not isinstance(account_id, str) or not re.fullmatch(r"[A-Za-z0-9_-]{1,160}", account_id):
        raise ProbeFailure("invalid_account_id")
    auth_claim = claims.get(AUTH_CLAIM)
    if not isinstance(auth_claim, dict) or auth_claim.get("chatgpt_account_id") != account_id:
        raise ProbeFailure("access_token_account_mismatch")
    now = time.time() if now is None else now
    expiration = claims.get("exp")
    if isinstance(expiration, bool) or not isinstance(expiration, (int, float)) or not math.isfinite(expiration):
        raise ProbeFailure("missing_access_token_expiration")
    if "exp" in credential and credential["exp"] != expiration:
        raise ProbeFailure("credential_expiration_mismatch")
    if expiration <= now + 300 + timeout:
        raise ProbeFailure("access_token_too_close_to_refresh_window")
    id_token = credential.get("id_token", access_token)
    identity_claims = jwt_claims(id_token)
    identity_auth = identity_claims.get(AUTH_CLAIM)
    if not isinstance(identity_auth, dict) or identity_auth.get("chatgpt_account_id") != account_id:
        raise ProbeFailure("identity_token_account_mismatch")
    if "id_token" in credential:
        # Workspace membership is shared by multiple users. Match the stable
        # user claims consumed by the pinned CLI, never email or JWT sub whose
        # semantics may differ between access and identity tokens.
        users = []
        for auth_claims in (auth_claim, identity_auth):
            user = auth_claims.get("chatgpt_user_id")
            if user is None:
                user = auth_claims.get("user_id")
            if (not isinstance(user, str) or not 1 <= len(user) <= 512
                    or any(char.isspace() or ord(char) < 32 or ord(char) == 127 for char in user)):
                raise ProbeFailure("identity_token_user_not_comparable")
            users.append(user)
        if users[0] != users[1]:
            raise ProbeFailure("identity_token_user_mismatch")
    auth = {"auth_mode": "chatgpt", "OPENAI_API_KEY": None,
            "tokens": {"id_token": id_token, "access_token": access_token,
                       "refresh_token": "", "account_id": account_id},
            "last_refresh": datetime.fromtimestamp(now, timezone.utc).isoformat()}
    # Never manufacture claims or a signature. The official external-AT path
    # itself parses the same unmodified access token into IdTokenInfo.
    metadata = {"auth_mode": "chatgpt", "refresh_token_supplied": False,
                "identity_source": "provided_id_token" if "id_token" in credential else "access_token_local_claims",
                "token_valid_for_probe_window": True}
    secrets = [access_token, id_token, account_id]
    for source in (claims, identity_claims):
        profile = source.get("https://api.openai.com/profile")
        for value in (source.get("email"), profile.get("email") if isinstance(profile, dict) else None):
            if isinstance(value, str) and value:
                secrets.append(value)
    return auth, metadata, secrets


def unique_json_object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ProbeFailure("duplicate_json_field")
        result[key] = value
    return result


def isolated_environment(temp_home):
    # No proxy, API key, provider override, session routing, cloud credential,
    # custom certificate, RUST_LOG, or user CODEX_* setting is inherited.
    allowed = {"PATH", "LANG", "LC_ALL", "LC_CTYPE", "TZ"}
    env = {key: value for key, value in os.environ.items() if key in allowed}
    env.update({"CODEX_HOME": str(temp_home), "NO_COLOR": "1", "RUST_LOG": "off",
                "CODEX_REFRESH_TOKEN_URL_OVERRIDE": REFRESH_DISABLED,
                "NO_PROXY": "127.0.0.1,localhost,::1", "no_proxy": "127.0.0.1,localhost,::1"})
    return env


def config_text(args):
    quote = lambda value: json.dumps(value, ensure_ascii=False)
    provider = "official_http_reference" if args.websocket == "false" else "openai"
    text = f'''model = {quote(args.model)}
model_provider = {quote(provider)}
model_reasoning_effort = {quote(args.effort)}
suppress_unstable_features_warning = true
approval_policy = "never"
sandbox_mode = "read-only"
web_search = "disabled"
cli_auth_credentials_store = "file"
mcp_oauth_credentials_store = "file"
project_doc_max_bytes = 0
developer_instructions = "Solve only the supplied problem. Do not call any tools. Return your answer directly."

[history]
persistence = "none"
[analytics]
enabled = false
[feedback]
enabled = false
[skills]
include_instructions = false
[skills.bundled]
enabled = false
[tools.update_plan]
enabled = false
[tools.experimental_request_user_input]
enabled = false
[features]
shell_tool = false
view_image = false
sleep_tool = false
js_repl = false
code_mode = false
apps = false
enable_mcp_apps = false
multi_agent = false
multi_agent_v2 = false
multi_agent_mode = false
enable_fanout = false
image_generation = false
hooks = false
plugin_hooks = false
plugins = false
remote_plugin = false
skill_search = false
tool_search = false
tool_suggest = false
request_permissions_tool = false
skip_host_skill_discovery = true
token_budget = false
current_time_reminder = false
'''
    if args.websocket == "false":
        # Built-in openai cannot be overridden in 0.155.0. A different provider
        # ID is required. Preserve its name, auth, endpoint and version header.
        text += f'''
[model_providers.official_http_reference]
name = "OpenAI"
base_url = "https://chatgpt.com/backend-api/codex"
wire_api = "responses"
requires_openai_auth = true
supports_websockets = false
supports_standalone_web_search = true
request_max_retries = 0
stream_max_retries = 0
[model_providers.official_http_reference.http_headers]
version = "{VERSION}"
'''
    return text


def tool_item(event):
    if not isinstance(event, dict) or not str(event.get("type", "")).startswith("item."):
        return False
    item = event.get("item")
    return isinstance(item, dict) and item.get("type") not in PASSIVE_ITEMS


def disqualifying_event(event, phase):
    """Stop an already-invalid reference, without inspecting answer content."""
    if not isinstance(event, dict):
        return False
    if event.get("type") in {"error", "turn.failed"}:
        return True
    item = event.get("item")
    if isinstance(item, dict) and item.get("type") == "error":
        message = item.get("message")
        rerouted = isinstance(message, str) and "model rerouted:" in message.lower()
        return phase != "startup" or rerouted
    return False


def stop_process(process):
    try:
        os.killpg(process.pid, signal.SIGKILL)
    except ProcessLookupError:
        pass


def run_process(command, prompt, cwd, env, timeout):
    started = time.monotonic()
    try:
        process = subprocess.Popen(command, cwd=cwd, env=env, stdin=subprocess.PIPE,
                                   stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                                   start_new_session=True)
    except OSError as exc:
        raise ProbeFailure("cli_launch_failed") from exc
    chunks = {"stdout": [], "stderr": []}
    count = {"stdout": 0, "stderr": 0}
    output_limit = threading.Event()
    tool_seen = threading.Event()
    reader_failed = threading.Event()
    reference_error = threading.Event()

    def read_stream(name, stream):
        phase = "startup"
        try:
            while True:
                data = stream.readline(MAX_INPUT + 1)
                if not data:
                    break
                count[name] += len(data)
                if len(data) > MAX_INPUT or count[name] > MAX_OUTPUT:
                    output_limit.set()
                    break
                chunks[name].append(data)
                if name == "stdout":
                    try:
                        event = json.loads(data)
                        if isinstance(event, dict) and event.get("type") == "turn.started":
                            phase = "turn"
                        if tool_item(event):
                            tool_seen.set()
                        if disqualifying_event(event, phase):
                            reference_error.set()
                        if isinstance(event, dict) and event.get("type") in {"turn.completed", "turn.failed"}:
                            phase = "after_turn"
                    except (ValueError, UnicodeError):
                        pass
        except (OSError, ValueError):
            reader_failed.set()
        finally:
            stream.close()

    def feed():
        try:
            if prompt:
                process.stdin.write(prompt.encode("utf-8"))
            process.stdin.close()
        except (BrokenPipeError, OSError, ValueError):
            pass

    readers = [threading.Thread(target=read_stream, args=(name, getattr(process, name)), daemon=True)
               for name in chunks]
    feeder = threading.Thread(target=feed, daemon=True)
    for thread in [*readers, feeder]:
        thread.start()
    timed_out = False
    try:
        while process.poll() is None:
            timed_out = time.monotonic() - started >= timeout
            if timed_out or tool_seen.is_set() or output_limit.is_set() or reader_failed.is_set() or reference_error.is_set():
                stop_process(process)
                break
            time.sleep(0.05)
        process.wait(timeout=5)
    except BaseException:
        stop_process(process)
        process.wait(timeout=5)
        raise
    finally:
        for thread in [*readers, feeder]:
            thread.join(timeout=1)
    if any(thread.is_alive() for thread in readers):
        reader_failed.set()
        stop_process(process)
    return {"returncode": process.returncode, "timed_out": timed_out,
            "stopped_on_reference_error": reference_error.is_set(),
            "tool_observed": tool_seen.is_set(), "output_limit": output_limit.is_set(),
            "reader_failed": reader_failed.is_set(), "elapsed_seconds": round(time.monotonic() - started, 3),
            **{name: b"".join(parts).decode("utf-8", errors="replace") for name, parts in chunks.items()}}


def redact(text, secrets):
    for secret in sorted(set(secrets), key=len, reverse=True):
        text = text.replace(secret, "[REDACTED]")
    text = re.sub(r"\b[A-Za-z0-9_-]{12,}\.[A-Za-z0-9_-]{12,}\.[A-Za-z0-9_-]{8,}\b", "[REDACTED_JWT]", text)
    text = re.sub(r"(?i)\bBearer\s+\S+", "Bearer [REDACTED]", text)
    return text


def diagnostic_indicators(text):
    text = text.lower()
    indicators = {name: any(word in text for word in words) for name, words in {
        "rate_limit": ("rate limit", "usage limit", "usage_limit"),
        "authentication": ("unauthorized", "refresh-disabled", "refresh token", "not logged in"),
        "model_unavailable": ("model_not_found", "not supported", "model is not"),
        "model_rerouted": ("model rerouted:",),
        "transport": ("connection closed", "connection reset", "websocket error", "tls error", "falling back from websockets"),
        "forbidden": ("403 forbidden",),
        "reconnecting": ("reconnecting...",),
    }.items()}
    indicators["rate_limit"] |= bool(re.search(r"\b429\b", text))
    indicators["authentication"] |= bool(re.search(r"\b401\b", text))
    indicators["model_capacity"] = text.strip() == "selected model is at capacity. please try a different model."
    return indicators


def safe_error_details(message, secrets):
    """Keep one short diagnostic line, never a multiline context or JSON body."""
    if not isinstance(message, str):
        return {"classification": "unavailable", "message_excerpt": None}
    lower = message.lower()
    indicators = diagnostic_indicators(message)
    classification = next((key for key in ("model_rerouted", "model_capacity", "rate_limit", "authentication", "model_unavailable", "forbidden", "transport", "reconnecting")
                           if indicators[key]), None)
    if classification is None:
        classification = ("deprecation" if "deprecat" in lower else
                          "configuration" if any(word in lower for word in ("config", "feature", "experimental")) else
                          "model_catalog" if any(word in lower for word in ("model catalog", "models list", "refresh models")) else
                          "other")
    message = redact(message, secrets)
    # Token-looking values, addresses, URLs, and local paths are never retained.
    message = re.sub(r"(?i)(?:https?|wss?)://[^\s\"'<>]+", "[REDACTED_URL]", message)
    message = re.sub(r"\b[A-Za-z0-9.!#$%&'*+/=?^_`{|}~-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}\b", "[REDACTED_EMAIL]", message)
    message = re.sub(r"(?i)\b(?:access_token|refresh_token|id_token|api_key|authorization)\b\s*[:=]\s*[^\s,;]+",
                     "[REDACTED_CREDENTIAL]", message)
    message = re.sub(r"(?i)\bsk-[A-Za-z0-9_-]+\b|\b[A-Za-z0-9_-]{40,}\b", "[REDACTED_TOKEN]", message)
    message = re.sub(r"(?:[A-Za-z]:[\\/]|\\\\)[^\s\"'<>]+|(?<![A-Za-z0-9_])/(?:[^\s\"'<>]+)",
                     "[REDACTED_PATH]", message)
    message = re.sub(r"\b[0-9a-fA-F]{8}(?:-[0-9a-fA-F]{4}){3}-[0-9a-fA-F]{12}\b", "[REDACTED_ID]", message)
    # Stop before any request/context/body material rather than truncating it.
    message = re.split(r"(?i)\r|\n|<\s*(?:system|developer|environment)|(?:request|response)\s+body\s*[:=]|\bpayload\s*[:=]|\b(?:system|developer)\s+(?:context|instructions|prompt)\s*[:=]|\{\s*\"", message, maxsplit=1)[0]
    message = " ".join(message.split())[:240]
    details = {"classification": classification, "message_excerpt": message or "[details omitted]"}
    if classification == "model_rerouted":
        match = re.search(r"model rerouted: ([A-Za-z0-9][A-Za-z0-9._-]{0,127}) -> ([A-Za-z0-9][A-Za-z0-9._-]{0,127}) \(([A-Za-z0-9_]{1,80})\)", message)
        if match:
            details["model_reroute"] = {"from_model": match[1], "to_model": match[2], "reason": match[3]}
    return details


def summarize(result, secrets):
    safe_events = []
    invalid = 0
    types = Counter()
    final_messages = []
    usage = {}
    errors = 0
    startup_warnings = 0
    critical_errors = 0
    tool_calls = 0
    completed = 0
    phase = "startup"
    diagnostic_text = {"startup": [], "turn": [], "after_turn": [], "stderr_unphased": [result["stderr"]]}
    for line in result["stdout"].splitlines():
        if not line.strip():
            continue
        try:
            event = json.loads(line)
        except ValueError:
            invalid += 1
            continue
        if not isinstance(event, dict) or event.get("type") not in EVENT_TYPES:
            invalid += 1
            continue
        kind = event["type"]
        types[kind] += 1
        if kind == "turn.started":
            phase = "turn"
        safe = {"type": kind, "phase": phase}
        if kind in {"error", "turn.failed"}:
            errors += 1
            critical_errors += 1
            source = event.get("error") if kind == "turn.failed" else event
            message = source.get("message") if isinstance(source, dict) else None
            safe["error"] = safe_error_details(message, secrets)
            if isinstance(message, str):
                diagnostic_text[phase].append(message)
        if kind == "turn.completed":
            completed += 1
            source = event.get("usage", {})
            if isinstance(source, dict):
                usage = {key: value for key, value in source.items()
                         if key in USAGE_KEYS and type(value) is int and value >= 0}
                safe["usage"] = usage
        item = event.get("item")
        if isinstance(item, dict):
            item_type = item.get("type")
            safe["item_type"] = item_type if item_type in PASSIVE_ITEMS | TOOL_ITEMS else "unknown"
            if item_type == "error":
                errors += 1
                message = item.get("message")
                safe["error"] = safe_error_details(message, secrets)
                if isinstance(message, str):
                    diagnostic_text[phase].append(message)
                # Official config/deprecation warnings use item.completed/error.
                # Top-level errors and in-turn warnings remain disqualifying.
                if phase == "startup" and kind == "item.completed" and safe["error"]["classification"] != "model_rerouted":
                    startup_warnings += 1
                else:
                    critical_errors += 1
            if tool_item(event):
                tool_calls += 1
            if kind == "item.completed" and item_type == "agent_message" and isinstance(item.get("text"), str):
                safe["text"] = redact(item["text"], secrets)
                final_messages.append(safe["text"])
        safe_events.append(safe)
        if kind in {"turn.completed", "turn.failed"}:
            phase = "after_turn"
    successful = (result["returncode"] == 0 and completed == 1 and bool(final_messages)
                  and not critical_errors and not invalid and not tool_calls
                  and not any(result[key] for key in ("timed_out", "tool_observed", "output_limit", "reader_failed")))
    # Stderr has no reliable phase. Do not infer a turn 429 from startup noise or
    # numbers that happen to appear in the model's generated answer.
    indicators_by_phase = {name: diagnostic_indicators("\n".join(parts)) for name, parts in diagnostic_text.items()}
    indicators = {key: any(value[key] for value in indicators_by_phase.values())
                  for key in diagnostic_indicators("")}
    # A reroute written only to stderr still invalidates the requested-model reference.
    successful = successful and not indicators["model_rerouted"] and not result.get("stopped_on_reference_error", False)
    status = ("completed_with_startup_warnings" if startup_warnings else "completed") if successful else "invalid_reference"
    return {"status": status, "reference_clean": status == "completed",
            "generated_answer_available": bool(final_messages), "turn_completed": completed == 1,
            "returncode": result["returncode"], "elapsed_seconds": result["elapsed_seconds"],
            "timed_out": result["timed_out"], "output_limit": result["output_limit"],
            "reader_failed": result["reader_failed"], "invalid_jsonl_lines": invalid,
            "stopped_on_reference_error": result.get("stopped_on_reference_error", False),
            "model_reroute_observed": indicators["model_rerouted"],
            "model_reroutes": [event["error"]["model_reroute"] for event in safe_events
                               if "model_reroute" in event.get("error", {})],
            "event_counts": dict(types), "error_events": errors,
            "startup_warning_events": startup_warnings, "critical_error_events": critical_errors,
            "tool_item_events": tool_calls, "tool_observed": result["tool_observed"] or bool(tool_calls),
            "final_output": final_messages[-1] if final_messages else None,
            "usage": usage, "diagnostic_indicators": indicators,
            "diagnostic_indicators_by_phase": indicators_by_phase}, safe_events


def summarize_prior_safe_evidence(report, events):
    """Reassess retained schema-1 event order without inventing lost messages."""
    reconstructed = []
    for event in events:
        if not isinstance(event, dict):
            raise ProbeFailure("invalid_prior_safe_event")
        safe = {"type": event.get("type")}
        if "item_type" in event:
            safe["item"] = {"type": event["item_type"]}
            if "text" in event:
                safe["item"]["text"] = event["text"]
        if "usage" in event:
            safe["usage"] = event["usage"]
        reconstructed.append(safe)
    result = {key: report[key] for key in ("returncode", "timed_out", "tool_observed", "output_limit",
                                         "reader_failed", "elapsed_seconds")}
    result.update({"stdout": "\n".join(json.dumps(event) for event in reconstructed), "stderr": ""})
    summary, phased_events = summarize(result, [])
    if summary["final_output"] != report.get("final_output") or summary["usage"] != report.get("usage"):
        raise ProbeFailure("prior_safe_evidence_does_not_match_report")
    if summary["status"] == "completed_with_startup_warnings":
        summary["status"] = "completed_with_unresolved_startup_events"
    # Reconstructed diagnostics are empty because schema 1 discarded messages.
    # Do not publish their false values as evidence that errors did not occur.
    summary.pop("diagnostic_indicators", None)
    summary.pop("diagnostic_indicators_by_phase", None)
    summary.update({
        "analysis": "summary_from_prior_safe_evidence",
        "source_schema_version": report.get("schema_version"),
        "source_status": report.get("status"),
        "new_live_request": False,
        "startup_message_text_available": False,
        "startup_message_classification": "unknown_original_text_not_retained",
        "model_reroute_excluded": False,
        "original_error_or_stderr_rate_limit_evidence": "unknown_not_retained",
        "legacy_rate_limit_indicator": report.get("diagnostic_indicators", {}).get("rate_limit"),
        "legacy_usage_429_collision": "429" in json.dumps(report.get("usage", {})),
        "legacy_diagnostic_limitation": "schema_1_scanned_all_stdout_including_usage_numbers",
        "event_phases": [{"type": event["type"], "phase": event["phase"],
                          **({"item_type": event["item_type"]} if "item_type" in event else {})}
                         for event in phased_events],
        **{key: report.get(key) for key in ("cli_version", "cli_sha256", "config_sha256", "prompt_sha256",
                                          "model_requested", "effort_requested", "transport_requested",
                                          "model_returned", "transport_observed", "started_at_utc")},
    })
    return summary


def write_private(path, text):
    with path.open("x", encoding="utf-8", newline="\n") as handle:
        os.chmod(path, 0o600)
        handle.write(text)


def executable_sha256(path):
    """Streaming digest compatible with the production server's Python 3.9."""
    digest = hashlib.sha256()
    with path.open("rb") as executable:
        for chunk in iter(lambda: executable.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def validate_cli_bundle(cli):
    """Mirror the official package resource-first helper lookup; never use PATH."""
    cli = cli.resolve(strict=True)
    if cli.parent.name != "bin":
        raise ProbeFailure("cli_requires_complete_official_package_layout")
    package = cli.parent.parent
    manifest_path = package / "codex-package.json"
    try:
        manifest = json.loads(read_small(manifest_path), object_pairs_hook=unique_json_object)
    except (ValueError, TypeError) as exc:
        raise ProbeFailure("invalid_cli_package_manifest") from exc
    if not isinstance(manifest, dict) or manifest.get("version") != VERSION:
        raise ProbeFailure("cli_package_version_must_be_0_155_0")
    if manifest.get("entrypoint") != "bin/" + cli.name or manifest.get("layoutVersion") != 1:
        raise ProbeFailure("unsupported_cli_package_layout")
    host_name = "codex-code-mode-host.exe" if cli.name.endswith(".exe") else "codex-code-mode-host"
    candidates = [package / "codex-resources" / host_name, cli.parent / host_name]
    host = next((candidate for candidate in candidates if candidate.is_file()), None)
    if host is None:
        raise ProbeFailure("official_code_mode_host_missing_install_complete_package")
    try:
        host.resolve(strict=True).relative_to(package)
    except ValueError as exc:
        raise ProbeFailure("code_mode_host_outside_official_package") from exc
    if not os.access(cli, os.X_OK) or not os.access(host, os.X_OK):
        raise ProbeFailure("official_cli_and_host_must_be_executable")
    return {"package_version": manifest["version"], "code_mode_host_present": True,
            "code_mode_host_location": host.relative_to(package).as_posix(),
            "code_mode_host_sha256": executable_sha256(host),
            "package_manifest_sha256": executable_sha256(manifest_path)}


def prepare_output_directory(path):
    path.mkdir(mode=0o700, parents=True, exist_ok=True)
    details = path.lstat()
    if not stat.S_ISDIR(details.st_mode):
        raise ProbeFailure("output_directory_must_not_be_symlink")
    if os.name == "posix" and (details.st_mode & 0o077 or details.st_uid != os.geteuid()):
        raise ProbeFailure("output_directory_must_be_private_0700")
    for name in ("report.json", "events.safe.jsonl"):
        if os.path.lexists(path / name):
            raise ProbeFailure("output_already_exists_use_new_directory")


def catalog_observation(home, model):
    """Describe only the CLI-written cache, never persist prompts or auth identity."""
    path = home / "models_cache.json"
    if not path.is_file():
        return {"cache_file_present": False, "requested_model_present": None}
    try:
        catalog = json.loads(read_small(path, MAX_OUTPUT), object_pairs_hook=unique_json_object)
        if not isinstance(catalog, dict) or not isinstance(catalog.get("models"), list):
            raise ValueError("invalid catalog")
        matches = [entry for entry in catalog["models"] if isinstance(entry, dict) and entry.get("slug") == model]
        tool_mode = matches[0].get("tool_mode") if len(matches) == 1 else None
        if tool_mode not in {None, "direct", "code_mode", "code_mode_only"}:
            tool_mode = "unknown"
        return {"cache_file_present": True, "cache_parse_valid": True,
                "client_version_matches": catalog.get("client_version") == VERSION,
                "identity_present": isinstance(catalog.get("identity"), str) and bool(catalog["identity"]),
                "requested_model_present": bool(matches), "requested_model_duplicate": len(matches) > 1,
                "requested_model_tool_mode": tool_mode}
    except (ProbeFailure, ValueError, TypeError):
        return {"cache_file_present": True, "cache_parse_valid": False, "requested_model_present": None}


def execute(args):
    if os.name != "posix":
        raise ProbeFailure("server_only_requires_posix")
    if not math.isfinite(args.timeout) or not 30 <= args.timeout <= 240:
        raise ProbeFailure("timeout_must_be_30_to_240_seconds")
    if not args.cli.is_absolute() or not args.cli.is_file():
        raise ProbeFailure("cli_must_be_absolute_native_executable")
    if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._-]{0,127}", args.model):
        raise ProbeFailure("invalid_model")
    bundle = validate_cli_bundle(args.cli)
    system_config_present = any(Path(path).exists() for path in
                               ("/etc/codex/config.toml", "/etc/codex/requirements.toml", "/etc/codex/managed_config.toml"))
    if system_config_present:
        raise ProbeFailure("system_codex_configuration_would_contaminate_reference")
    prompt = read_small(args.prompt_file)
    if not prompt.strip():
        raise ProbeFailure("empty_prompt")
    auth, auth_metadata, secrets = load_credential(args.credential_file, args.timeout)
    started_at_utc = datetime.now(timezone.utc).isoformat()
    # Complete local preflight before making a paid/live request.
    cli_sha256 = executable_sha256(args.cli)
    prepare_output_directory(args.output_dir)
    report_path = args.output_dir / "report.json"
    events_path = args.output_dir / "events.safe.jsonl"
    with tempfile.TemporaryDirectory(prefix="codex-official-quality-") as temporary:
        root = Path(temporary)
        temp_home = root / "codex-home"
        temp_home.mkdir(mode=0o700)
        workdir = root / "work"
        workdir.mkdir(mode=0o700)
        env = isolated_environment(temp_home)
        version_result = run_process([str(args.cli), "--version"], None, workdir, env, 15)
        version = version_result["stdout"].strip()
        if version_result["returncode"] != 0 or version != f"codex-cli {VERSION}":
            raise ProbeFailure("official_cli_version_must_be_0_155_0")
        write_private(temp_home / "auth.json", json.dumps(auth))
        config = config_text(args)
        write_private(temp_home / "config.toml", config)
        command = [str(args.cli), "exec", "--json", "--ephemeral", "--ignore-rules",
                   "--strict-config", "--skip-git-repo-check", "--color", "never",
                   "--model", args.model, "-"]
        result = run_process(command, prompt, workdir, env, args.timeout)
        report, events = summarize(result, secrets)
        report.update({"schema_version": 3, "cli_version": VERSION,
                       "cli_bundle": bundle,
                       "retry_policy": "official_builtin_with_probe_stop_on_visible_error" if args.websocket != "false" else "custom_http_no_retries",
                       "retry_visibility": "first_release_websocket_retry_may_be_hidden",
                       "model_catalog_initial_state": "fresh_isolated_home_no_injected_cache",
                       "model_catalog_override": False,
                       "model_catalog_observed": catalog_observation(temp_home, args.model),
                       "tool_policy": "disabled_configurable_tools_and_abort_on_observed_tool_item",
                       "tool_surface_zero_verified": False,
                       "model_requested": args.model, "effort_requested": args.effort,
                       "model_returned": None, "transport_observed": None,
                       "transport_requested": "http_custom_provider" if args.websocket == "false" else "official_default_ws_with_fallback",
                       "provider_id": "official_http_reference" if args.websocket == "false" else "openai",
                       "auth": auth_metadata,
                       "prompt_sha256": hashlib.sha256(prompt.encode()).hexdigest(),
                       "config_sha256": hashlib.sha256(config.encode()).hexdigest(),
                       "cli_sha256": cli_sha256,
                       "prompt_modified": False, "expected_answer_supplied": False,
                       "refresh_endpoint": "loopback_disabled", "proxy_environment_inherited": False,
                       "session_persisted": False, "raw_events_persisted": False,
                       "system_config_present": system_config_present,
                       "started_at_utc": started_at_utc})
    # The temporary auth/home/logs are deleted before any result is published.
    write_private(events_path, "".join(json.dumps(event, ensure_ascii=False) + "\n" for event in events))
    write_private(report_path, json.dumps(report, indent=2, ensure_ascii=False) + "\n")
    return report


def main(argv=None):
    try:
        report = execute(parse_args(argv))
    except ProbeFailure as exc:
        print(json.dumps({"status": "probe_error", "error": str(exc)}))
        return 2
    except (OSError, ValueError, TypeError, subprocess.SubprocessError):
        print(json.dumps({"status": "probe_error", "error": "probe_runtime_failed"}))
        return 2
    print(json.dumps(report, ensure_ascii=False))
    return 0 if report["status"] in {"completed", "completed_with_startup_warnings"} else 1


if __name__ == "__main__":
    sys.exit(main())
