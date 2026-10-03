#!/usr/bin/env python3
"""One explicit server-only native IPC replay; never modifies captured JSON.

Inputs are private files. This tool creates one fresh real Rust helper, sends
one POST, performs no auth refresh/retry and retains only safe evidence. A
nonzero exit/result is not permission to retry the same cell automatically.
"""
from __future__ import annotations

import argparse
import base64
from collections import Counter
import hashlib
import json
import math
import os
from pathlib import Path
import re
import secrets
import socket
import stat
import struct
import subprocess
import tempfile
import time

from quality_native_ipc_capture import (MAX_BODY, MAX_HEAD, ExactJSONNumber, _ready_line,
    _value_summary, file_digest, read_exact, summarize_body, summarize_head, write_all)
from quality_official_cli_probe import AUTH_CLAIM, jwt_claims, unique_json_object, ProbeFailure
from quality_probe_report import extract_final, parse_answer, redact

SCHEMA = "codex-quality-exact-request-v1"
CAPTURE_HEAD_SCHEMA = "codex-quality-captured-request-v1"
MAX_RESPONSE = 8 << 20
MAX_FINAL = 256 << 10
OUTPUT_EVIDENCE_SCHEMA = "codex-quality-response-output-v1"
OUTPUT_ITEM_LIMIT = 128
OUTPUT_FIELD_LIMIT = 32
OUTPUT_ITEM_TYPES = frozenset(("message", "reasoning", "compaction", "compaction_summary",
    "function_call", "function_call_output", "custom_tool_call", "custom_tool_call_output",
    "local_shell_call", "local_shell_call_output", "web_search_call", "image_generation_call",
    "computer_call", "computer_call_output", "file_search_call", "mcp_call", "mcp_list_tools",
    "mcp_approval_request", "mcp_approval_response", "tool_search_call", "tool_search_output"))
OUTPUT_FIELD_NAMES = frozenset(("id", "type", "role", "status", "phase", "content", "summary",
    "encrypted_content", "call_id", "name", "arguments", "input", "output", "body", "action"))
ALLOWED_HEADERS = frozenset(("accept", "content-type", "user-agent", "originator", "version",
    "openai-beta", "session-id", "thread-id", "session_id", "conversation_id", "x-request-id",
    "x-client-request-id", "x-openai-subagent", "x-openai-memgen-request",
    "x-openai-internal-codex-responses-lite", "x-openai-internal-codex-residency",
    "x-responsesapi-include-timing-metrics", "traceparent", "tracestate"))


class ExactProbeError(Exception):
    """Fixed error labels only; never interpolate input or exception messages."""


def sha(raw):
    return hashlib.sha256(raw).hexdigest()


def private_read(path, limit):
    flags = os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0) | getattr(os, "O_NONBLOCK", 0) | getattr(os, "O_BINARY", 0)
    try:
        with os.fdopen(os.open(path, flags), "rb") as stream:
            info = os.fstat(stream.fileno())
            if not stat.S_ISREG(info.st_mode) or os.name == "posix" and (info.st_mode & 0o077 or info.st_uid != os.geteuid()):
                raise ExactProbeError("input_not_private_regular_file")
            raw = stream.read(limit + 1)
        if len(raw) > limit:
            raise ExactProbeError("input_limit")
        return raw
    except OSError as error:
        raise ExactProbeError("input_read_failed") from error


def private_json(raw, exact_numbers=False):
    def invalid(_):
        raise ExactProbeError("nonfinite_json")
    try:
        numbers = {"parse_int": ExactJSONNumber, "parse_float": ExactJSONNumber} if exact_numbers else {}
        return json.loads(raw.decode("utf-8"), object_pairs_hook=unique_json_object, parse_constant=invalid, **numbers)
    except (ValueError, UnicodeError, RecursionError, ProbeFailure) as error:
        raise ExactProbeError("invalid_json") from error


def load_capture(body_path, head_path, body_sha, head_sha):
    body, raw_head = private_read(body_path, MAX_BODY), private_read(head_path, MAX_HEAD)
    if sha(body) != body_sha or sha(raw_head) != head_sha:
        raise ExactProbeError("capture_digest_mismatch")
    head = private_json(raw_head)
    if not isinstance(head, dict) or set(head) != {"schema", "method", "path", "headers", "request_compression", "body_sha256", "body_bytes"}:
        raise ExactProbeError("capture_head_schema")
    if (head["schema"] != CAPTURE_HEAD_SCHEMA or head["method"] != "POST"
            or head["path"] != "/backend-api/codex/responses"
            or head["request_compression"] not in ("none", "zstd")
            or head["body_sha256"] != body_sha or head["body_bytes"] != len(body)):
        raise ExactProbeError("capture_contract_mismatch")
    summary = summarize_body(body, [])
    if not summary.get("body_summary_complete") or summary.get("stream") is not True or summary.get("store") is not False:
        raise ExactProbeError("capture_body_not_complete_stateless_stream")
    if "previous_response_id" in summary.get("body_fields", {}):
        # A server-owned previous response cannot be moved across accounts.
        raise ExactProbeError("capture_must_be_first_turn_full_history")
    headers = head["headers"]
    if not isinstance(headers, list) or len(headers) > 120:
        raise ExactProbeError("capture_headers_invalid")
    seen = set()
    for pair in headers:
        if not isinstance(pair, list) or len(pair) != 2 or not all(isinstance(x, str) for x in pair):
            raise ExactProbeError("capture_headers_invalid")
        name, encoded = pair
        lower = name.lower()
        if (not re.fullmatch(r"[!#$%&'*+.^_`|~0-9A-Za-z-]{1,256}", name)
                or lower not in ALLOWED_HEADERS and not lower.startswith("x-codex-")
                or lower in seen):
            raise ExactProbeError("capture_header_not_allowlisted_or_repeated")
        try:
            value = base64.b64decode(encoded, validate=True)
        except ValueError as error:
            raise ExactProbeError("capture_header_encoding") from error
        if len(value) > MAX_HEAD or any(byte < 32 and byte != 9 or byte == 127 for byte in value):
            raise ExactProbeError("capture_header_value")
        if re.search(rb"(?i)bearer\s|\beyJ[A-Za-z0-9_-]*\.[A-Za-z0-9_-]+\.|\bsk-[A-Za-z0-9_-]{12,}", value):
            raise ExactProbeError("capture_header_contains_credential")
        if lower == "accept" and value != b"text/event-stream":
            raise ExactProbeError("capture_requires_sse")
        if lower == "content-type" and value != b"application/json":
            raise ExactProbeError("capture_requires_json")
        seen.add(lower)
    if not {"accept", "content-type"} <= seen:
        raise ExactProbeError("capture_missing_content_headers")
    return body, head


def load_at_only(path, timeout):
    raw = private_read(path, 64 << 10)
    value = private_json(raw)
    if not isinstance(value, dict) or set(value) - {"access_token", "account_id", "exp"}:
        raise ExactProbeError("credential_must_be_at_only")
    try:
        token, workspace = value.get("access_token"), value.get("account_id")
        claims = jwt_claims(token)
    except ProbeFailure as error:
        raise ExactProbeError("credential_preflight_failed") from error
    auth_claim = claims.get(AUTH_CLAIM)
    expires = claims.get("exp")
    if (not isinstance(workspace, str) or not re.fullmatch(r"[A-Za-z0-9_-]{1,160}", workspace)
            or not isinstance(auth_claim, dict) or auth_claim.get("chatgpt_account_id") != workspace
            or isinstance(expires, bool) or not isinstance(expires, (int, float)) or not math.isfinite(expires)
            or expires <= time.time() + timeout + 300 or "exp" in value and value["exp"] != expires):
        raise ExactProbeError("credential_identity_or_expiry_invalid")
    hidden = [token, workspace]
    profile = claims.get("https://api.openai.com/profile")
    for item in (claims.get("email"), profile.get("email") if isinstance(profile, dict) else None):
        if isinstance(item, str) and item:
            hidden.append(item)
    return token, workspace, hidden


def exact_head(captured, body, token, workspace, ipc_token, remaining_ms):
    headers = [list(pair) for pair in captured["headers"]]
    headers.extend([["Authorization", base64.b64encode(("Bearer " + token).encode()).decode()],
                    ["ChatGPT-Account-Id", base64.b64encode(workspace.encode()).decode()]])
    head = {"protocol": 1, "ipc_token": ipc_token, "method": "POST", "path": captured["path"],
            "headers": headers, "body_length": len(body), "total_timeout_ms": remaining_ms}
    if captured["request_compression"] == "zstd":
        head["request_compression"] = "zstd"
    return head


def deadline_io(stream, deadline, operation, value):
    remaining = deadline - time.monotonic()
    if remaining <= 0:
        raise TimeoutError()
    stream.settimeout(remaining)
    return getattr(stream, operation)(value)


def safe_token(value, hidden):
    return (value if isinstance(value, str) and re.fullmatch(r"[A-Za-z0-9_.:/-]{1,128}", value)
            and not any(secret and secret in value for secret in hidden) else None)


def safe_usage(value, details=False):
    if not isinstance(value, dict):
        return {}
    allowed = {"input_tokens", "output_tokens", "total_tokens", "cached_tokens", "cache_write_tokens", "reasoning_tokens"}
    result = {key: number for key, number in value.items() if (key in allowed or details and re.fullmatch(r"[a-z][a-z0-9_]{0,63}", key)) and isinstance(number, (int, float)) and not isinstance(number, bool) and math.isfinite(number) and number >= 0}
    for key in ("input_tokens_details", "output_tokens_details"):
        if isinstance(value.get(key), dict):
            result[key] = safe_usage(value[key], True)
    return result


def response_controls(response, hidden):
    result = {}
    for key in ("model", "service_tier", "status"):
        value = safe_token(response.get(key), hidden)
        if value is not None:
            result[key] = value
    if "max_output_tokens" in response:
        value = response["max_output_tokens"]
        if value is None or isinstance(value, int) and not isinstance(value, bool) and value >= 0:
            result["max_output_tokens"] = value
    reasoning = response.get("reasoning")
    if isinstance(reasoning, dict):
        known, unknown = {}, {}
        for key, value in reasoning.items():
            if key in ("effort", "summary", "context", "mode") and (value is None or safe_token(value, hidden) is not None):
                known[key] = value
            elif key in ("budget", "budget_tokens", "max_tokens", "max_output_tokens") and isinstance(value, (int, float)) and not isinstance(value, bool) and math.isfinite(value):
                known[key] = value
            else:
                encoded = json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":")).encode()
                unknown["sha256:" + sha(key.encode())] = {"content_sha256": sha(encoded), "canonical_json_bytes": len(encoded)}
        result["reasoning"] = known
        result["reasoning_unknown_fields"] = unknown
        result["reasoning_field_count"] = len(reasoning)
    elif "reasoning" in response and reasoning is None:
        result["reasoning"] = None
    result["usage"] = safe_usage(response.get("usage"))
    return result


def output_item_evidence(item, hidden):
    """Hash a complete item but expand only bounded, fixed-name metadata.

    The exact-number tree is report-only. Neither a hash nor equal token counts
    establish equal hidden reasoning; ciphertext can change under encryption.
    """
    result = dict(_value_summary(item), present=True)
    if not isinstance(item, dict):
        return result
    kind = item.get("type")
    result["item_type"] = (kind if type(kind) is str and kind in OUTPUT_ITEM_TYPES and safe_token(kind, hidden)
                           else "missing" if "type" not in item else "unknown")
    if type(item.get("id")) is str:
        result["id_sha256"] = sha(item["id"].encode())
    fields = {}
    # Keep content/summary/ciphertext evidence before expanding unknown fields.
    keys = sorted(item, key=lambda key: (key not in OUTPUT_FIELD_NAMES, key))
    for key in keys[:OUTPUT_FIELD_LIMIT]:
        name = key if key in OUTPUT_FIELD_NAMES else "sha256:" + sha(key.encode())
        fields[name] = dict(_value_summary(item[key]), name_sha256=sha(key.encode()))
    result["field_summary"] = {"fields": fields, "field_count": len(item),
                               "fields_captured": len(fields), "complete": len(fields) == len(item)}
    return result


def terminal_output_evidence(response, hidden):
    if not isinstance(response, dict) or "output" not in response:
        return {"present": False, "complete": True}
    output = response["output"]
    result = dict(_value_summary(output), present=True, complete=True)
    if isinstance(output, list):
        items = [{"position": index, "item": output_item_evidence(item, hidden)}
                 for index, item in enumerate(output[:OUTPUT_ITEM_LIMIT])]
        result.update(item_count=len(output), items_captured=len(items), items=items,
                      complete=len(items) == len(output) and all(
                          entry["item"].get("field_summary", {}).get("complete", True) for entry in items))
    return result


class SSECollector:
    def __init__(self, hidden=()):
        self.hidden = hidden
        self.buffer = b""
        self.data = []
        self.total = 0
        self.started = False
        self.counts = Counter()
        self.done_messages = []
        self.terminal = None
        self.response = {}
        self.error = {}
        self.signals = []
        self.signals_dropped = 0
        self.response_id = None
        self.done_item_evidence = []
        self.done_item_count = 0
        self.terminal_output_evidence = {"present": False, "complete": True}
        self.terminal_event_data = None

    def feed(self, raw, eof=False):
        self.total += len(raw)
        if self.total > MAX_RESPONSE:
            raise ExactProbeError("response_limit")
        self.buffer += raw
        while self.buffer and self.terminal is None:
            ending = re.search(rb"\r\n|\r|\n", self.buffer)
            if ending is None:
                break
            if ending.group() == b"\r" and ending.end() == len(self.buffer) and not eof:
                break
            line, self.buffer = self.buffer[:ending.start()], self.buffer[ending.end():]
            if not self.started:
                self.started = True
                line = line.removeprefix(b"\xef\xbb\xbf")
            try:
                line.decode("utf-8")
            except UnicodeError as error:
                raise ExactProbeError("invalid_sse_utf8") from error
            if not line:
                self.dispatch()
            elif line == b"data":
                self.data.append(b"")
            elif line.startswith(b"data:"):
                value = line[5:]
                self.data.append(value[1:] if value.startswith(b" ") else value)
        if eof and self.terminal is None and (self.buffer or self.data):
            raise ExactProbeError("truncated_sse_event")

    def dispatch(self):
        if not self.data:
            return
        raw, self.data = b"\n".join(self.data), []
        if raw == b"[DONE]":
            return
        event = private_json(raw)
        if not isinstance(event, dict) or not isinstance(event.get("type"), str):
            raise ExactProbeError("invalid_sse_event")
        kind = event["type"]
        if not re.fullmatch(r"[a-z_.]{1,100}", kind):
            raise ExactProbeError("invalid_sse_event_type")
        self.counts[kind] += 1
        response = event.get("response")
        top_id = event.get("response_id")
        nested_id = response.get("id") if isinstance(response, dict) else None
        for identity in (top_id, nested_id):
            if identity is not None and (not isinstance(identity, str) or len(identity) > 1024):
                raise ExactProbeError("invalid_response_identity")
        if top_id and nested_id and top_id != nested_id:
            raise ExactProbeError("conflicting_response_identity")
        identity = nested_id or top_id
        if identity:
            if self.response_id is not None and self.response_id != identity:
                raise ExactProbeError("conflicting_response_identity")
            self.response_id = identity
        if kind == "response.output_item.done" or kind in ("response.completed", "response.incomplete", "response.failed", "error"):
            # Parsing once more here preserves numeric lexemes for hashes while
            # the existing scoring/control parser retains its original types.
            exact = private_json(raw, exact_numbers=True)
            try:
                if kind == "response.output_item.done":
                    self.done_item_count += 1
                    if len(self.done_item_evidence) < OUTPUT_ITEM_LIMIT:
                        entry = {"event_position": self.done_item_count - 1,
                                 "output_index_present": "output_index" in exact,
                                 "item": output_item_evidence(exact["item"], self.hidden) if "item" in exact else {"present": False}}
                        if "output_index" in exact:
                            index = exact["output_index"]
                            entry["output_index_summary"] = _value_summary(index)
                            if index is None:
                                entry["output_index"] = None
                            elif isinstance(index, ExactJSONNumber) and re.fullmatch(r"[0-9]{1,10}", index) and int(index) <= 2147483647:
                                entry["output_index"] = int(index)
                        self.done_item_evidence.append(entry)
                else:
                    self.terminal_output_evidence = terminal_output_evidence(exact.get("response"), self.hidden)
                    self.terminal_event_data = {"sha256": sha(raw), "bytes": len(raw),
                        "basis": "SSE_data_lines_joined_with_LF_excluding_framing_not_full_wire_bytes"}
            except (ValueError, UnicodeError, RecursionError) as error:
                raise ExactProbeError("invalid_output_evidence") from error
        if isinstance(response, dict):
            metadata = dict(response_controls(response, self.hidden), event_type=kind)
            if len(self.signals) < 32:
                self.signals.append(metadata)
            else:
                self.signals_dropped += 1
        if kind == "response.output_item.done" and isinstance(event.get("item"), dict):
            item = event["item"]
            if item.get("type") == "message" and item.get("role", "assistant") == "assistant":
                if len(self.done_messages) >= 128:
                    raise ExactProbeError("final_item_limit")
                self.done_messages.append({"type": kind, "item": item, "output_index": event.get("output_index")})
        if kind in ("response.completed", "response.incomplete", "response.failed", "error"):
            self.terminal = kind
            self.response = response if isinstance(response, dict) else {}
            candidate = event.get("error") or self.response.get("error") or (event if kind in ("error", "response.failed") else {})
            if isinstance(candidate, dict):
                for key in ("code", "type"):
                    value = safe_token(candidate.get(key), self.hidden)
                    if value is not None:
                        self.error[key] = value
                for key in ("status", "status_code"):
                    number = candidate.get(key, event.get(key))
                    if isinstance(number, int) and not isinstance(number, bool) and 100 <= number <= 599:
                        self.error[key] = number
                if isinstance(candidate.get("message"), str):
                    encoded = candidate["message"].encode("utf-8")
                    self.error.update(message_sha256=sha(encoded), message_bytes=len(encoded))

    def report(self, status):
        text, source, conflict = extract_final({"response": self.response}, self.done_messages)
        extracted = text.encode()
        if len(extracted) > MAX_FINAL:
            raise ExactProbeError("final_text_limit")
        for hidden in self.hidden:
            if hidden:
                text = text.replace(hidden, "[REDACTED]")
        text = redact(text)
        complete = (status == 200 and self.terminal == "response.completed" and self.response.get("status") == "completed"
                    and isinstance(self.response.get("id"), str) and bool(self.response["id"]) and not self.response.get("error"))
        answer, extraction = parse_answer(text) if complete and not conflict else (None, "not_completed_or_conflicting_output")
        output_evidence = {"schema": OUTPUT_EVIDENCE_SCHEMA,
            "summary_basis": "canonical_json_sorted_object_keys_normalized_string_escapes_exact_number_lexemes",
            "field_byte_lengths": "canonical_JSON_UTF8_bytes_not_original_wire_spans",
            "duplicate_member_policy": "reject_any_duplicate_at_any_depth",
            "item_limit_per_source": OUTPUT_ITEM_LIMIT, "field_limit_per_item": OUTPUT_FIELD_LIMIT,
            "done_items": {"event_count": self.done_item_count, "items_captured": len(self.done_item_evidence),
                "items": self.done_item_evidence, "complete": self.done_item_count == len(self.done_item_evidence) and all(
                    entry["item"].get("field_summary", {}).get("complete", True) for entry in self.done_item_evidence)},
            "terminal_output": self.terminal_output_evidence, "terminal_event_data": self.terminal_event_data,
            "final_text": {"basis": "extract_final_text_not_raw_wire_or_full_output_tree", "source": source,
                "extracted_utf8_sha256": sha(extracted), "extracted_utf8_bytes": len(extracted),
                "redacted_utf8_sha256": sha(text.encode()), "redacted_utf8_bytes": len(text.encode()),
                "redacted_text_is_reported": complete}}
        return {"http_status": status, "terminal_event": self.terminal, "response_status": self.response.get("status") if self.response.get("status") in ("completed", "failed", "incomplete", "cancelled") else None,
                "protocol_success": complete, "event_counts": dict(self.counts), "response_bytes": self.total,
                "generated_answer_available": complete and bool(text) and not conflict,
                "response_id_sha256": sha(self.response_id.encode()) if self.response_id else None,
                "final_output": text if complete else "", "final_output_source": source, "final_output_conflict": conflict,
                "answer": answer, "answer_extraction": extraction, "response_error": self.error,
                "response_usage": safe_usage(self.response.get("usage")), "response_controls": response_controls(self.response, self.hidden),
                "response_signals": self.signals, "response_signals_dropped": self.signals_dropped,
                "response_output_evidence": output_evidence,
                "failure_category": None if complete else "http_error" if status != 200 else "upstream_error" if self.terminal else "missing_terminal"}


def native_request(helper, expected_helper_sha, body, captured, token, workspace, hidden, timeout=240, upstream="https://chatgpt.com"):
    if os.name != "posix" or not hasattr(socket, "AF_UNIX"):
        raise ExactProbeError("linux_posix_required")
    if not 1 <= timeout <= 240 or file_digest(helper) != expected_helper_sha:
        raise ExactProbeError("helper_or_deadline_invalid")
    started = time.monotonic()
    deadline = started + timeout
    report = {"schema": SCHEMA, "attempts": 1, "native_dispatch_count": 0, "automatic_retries": False,
              "helper_sha256": expected_helper_sha, "request_body_sha256": sha(body), "request_body_bytes": len(body),
              "token_sha256": sha(token.encode()), "workspace_sha256": sha(workspace.encode()),
              "environment_policy": "isolated_PATH_LANG_no_proxy_or_CA_overrides", "upstream_wire_capture": False}
    process = None
    stream = None
    collector = SSECollector(hidden)
    status = None
    try:
        with tempfile.TemporaryDirectory(prefix="qexact-") as directory:
            os.chmod(directory, 0o700)
            path = str(Path(directory) / "s")
            ipc_token = secrets.token_hex(32)
            startup = {"protocol": 1, "unix_socket": path, "upstream": upstream, "ipc_token": ipc_token,
                       "headers_timeout_ms": min(90000, int(timeout * 1000)), "idle_timeout_ms": 0,
                       "http_client_lifecycle": "cli_request"}
            process = subprocess.Popen([str(helper)], stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                       stderr=subprocess.DEVNULL, close_fds=True, bufsize=0,
                                       env={"PATH": os.environ.get("PATH", "/usr/bin:/bin"), "LANG": "C.UTF-8"})
            encoded = json.dumps(startup, separators=(",", ":")).encode()
            write_all(process.stdin.write, struct.pack("!I", len(encoded)) + encoded)
            ready = private_json(_ready_line(process.stdout, min(9, timeout)))
            if ready.get("protocol") != 1 or ready.get("transport") != "unix" or ready.get("unix_socket") != path:
                raise ExactProbeError("helper_readiness_mismatch")
            runtime = file_digest(Path("/proc") / str(process.pid) / "exe")
            report["runtime_executable_sha256"] = runtime
            if runtime != expected_helper_sha:
                raise ExactProbeError("helper_runtime_mismatch")
            if captured["request_compression"] == "zstd" and "request_zstd_v1" not in ready.get("capabilities", []):
                raise ExactProbeError("helper_compression_unavailable")
            stream = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
            stream.settimeout(max(0.001, deadline - time.monotonic()))
            stream.connect(path)
            remaining = int((deadline - time.monotonic()) * 1000)
            if remaining <= 0:
                raise ExactProbeError("deadline_before_dispatch")
            head = exact_head(captured, body, token, workspace, ipc_token, remaining)
            head_summary, sensitive = summarize_head(head)
            report.update(request_header_summary=head_summary, request_body_summary=summarize_body(body, sensitive))
            encoded = json.dumps(head, separators=(",", ":")).encode()
            if len(encoded) > MAX_HEAD:
                raise ExactProbeError("ipc_head_limit")
            wire = struct.pack("!I", len(encoded)) + encoded + body
            report["ipc_dispatch_attempted"] = True
            report["ipc_bytes_accepted"] = 0
            def accepted(part):
                report["ipc_bytes_accepted"] += len(part)
            def timed_send(value):
                return deadline_io(stream, deadline, "send", value)
            def timed_recv(size):
                return deadline_io(stream, deadline, "recv", size)
            write_all(timed_send, wire, accepted)
            report.update(native_dispatch_count=1, local_ipc_write_complete=True,
                          actual_ipc_body_sha256=sha(body), actual_ipc_request_sha256=sha(wire))
            failure_body = bytearray()
            while True:
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    raise TimeoutError()
                stream.settimeout(remaining)
                prefix = read_exact(timed_recv, 5)
                kind, size = struct.unpack("!BI", prefix)
                if size > MAX_HEAD:
                    raise ExactProbeError("native_response_frame_limit")
                data = read_exact(timed_recv, size)
                if kind == 1:
                    if status is not None:
                        raise ExactProbeError("duplicate_native_headers")
                    response = private_json(data)
                    status = response.get("status")
                    if not isinstance(status, int) or not 200 <= status <= 599:
                        raise ExactProbeError("invalid_native_status")
                    report["http_version"] = response.get("version") if response.get("version") in ("HTTP/1.0", "HTTP/1.1", "HTTP/2.0", "HTTP/2", "HTTP/3.0") else None
                    report["response_header_hashes"] = [{"name_sha256": sha(name.lower().encode()), "value_sha256": sha(base64.b64decode(value, validate=True))} for name, value in response.get("headers", [])]
                elif kind == 2:
                    if status is None:
                        raise ExactProbeError("body_before_native_headers")
                    if status != 200:
                        if len(failure_body) + len(data) > 64 << 10:
                            raise ExactProbeError("http_error_body_limit")
                        failure_body.extend(data)
                    else:
                        collector.feed(data)
                        if collector.terminal is not None:
                            break
                elif kind == 3:
                    if size != 0:
                        raise ExactProbeError("invalid_native_end")
                    if status == 200:
                        collector.feed(b"", eof=True)
                    break
                elif kind == 4:
                    raise ExactProbeError("native_transport_error")
                else:
                    raise ExactProbeError("unknown_native_frame")
            report.update(collector.report(status))
            if failure_body:
                report["http_error_body_sha256"] = sha(failure_body)
                try:
                    collector.data = [json.dumps({"type": "error", "error": private_json(failure_body).get("error", {})}).encode()]
                    collector.dispatch()
                    report["response_error"] = collector.error
                except (ExactProbeError, AttributeError):
                    pass
    except Exception as error:
        try:
            report.update(collector.report(status))
        except ExactProbeError:
            pass
        report.update(http_status=status, protocol_success=False, failure_category="timeout" if isinstance(error, (TimeoutError, socket.timeout)) else "probe_error",
                      error_class=error.args[0] if isinstance(error, ExactProbeError) else type(error).__name__,
                      answer=None, generated_answer_available=False, final_output="")
    finally:
        if stream is not None:
            stream.close()
        if process is not None:
            process.stdin.close()
            try:
                process.wait(timeout=5)
                report["helper_forced_kill"] = False
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait(timeout=5)
                report["helper_forced_kill"] = True
            process.stdout.close()
            report.update(helper_returncode=process.returncode, helper_reaped=process.poll() is not None)
        report["local_socket_absent"] = not Path(path).exists() if process is not None else None
        report["duration_ms"] = round((time.monotonic() - started) * 1000)
    return report


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("body-file", "head-file", "credential-file", "helper", "output"):
        parser.add_argument("--" + name, type=Path, required=True)
    for name in ("expected-body-sha256", "expected-head-sha256", "expected-helper-sha256"):
        parser.add_argument("--" + name, required=True)
    parser.add_argument("--alias", required=True)
    parser.add_argument("--allow-live", action="store_true")
    parser.add_argument("--timeout", type=int, default=240)
    args = parser.parse_args(argv)
    fd = None
    try:
        if not args.allow_live or not re.fullmatch(r"[A-Za-z0-9_-]{1,40}", args.alias) or not 1 <= args.timeout <= 240:
            raise ExactProbeError("explicit_valid_single_probe_required")
        body, head = load_capture(args.body_file, args.head_file, args.expected_body_sha256, args.expected_head_sha256)
        token, workspace, hidden = load_at_only(args.credential_file, args.timeout)
        parent = args.output.parent
        info = parent.lstat()
        if not stat.S_ISDIR(info.st_mode) or os.name == "posix" and (info.st_mode & 0o077 or info.st_uid != os.geteuid()):
            raise ExactProbeError("output_parent_not_private")
        fd = os.open(args.output, os.O_CREAT | os.O_EXCL | os.O_WRONLY | getattr(os, "O_NOFOLLOW", 0), 0o600)
        report = native_request(args.helper, args.expected_helper_sha256, body, head, token, workspace, hidden, args.timeout)
        report.update(account_alias=args.alias, captured_head_sha256=args.expected_head_sha256,
                      tool_sha256=file_digest(Path(__file__)), source_body_and_headers_unchanged=True)
        write_all(lambda value: os.write(fd, value), (json.dumps(report, ensure_ascii=False, indent=2) + "\n").encode())
        print(json.dumps({"account_alias": args.alias, "http_status": report.get("http_status"), "protocol_success": report.get("protocol_success"), "failure_category": report.get("failure_category")}))
        return 0 if report.get("protocol_success") else 1
    except Exception as error:
        print(json.dumps({"error_class": error.args[0] if isinstance(error, ExactProbeError) else type(error).__name__, "automatic_retry": False}))
        return 1
    finally:
        if fd is not None:
            os.close(fd)


if __name__ == "__main__":
    raise SystemExit(main())
