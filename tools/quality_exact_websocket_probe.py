"""One exact first-turn Responses WebSocket request through a fresh native helper.

No HTTP-to-WS conversion, credential discovery, refresh, retry, or fallback.
Credentials, handshake bytes and application messages remain in memory. The
caller supplies captured non-auth headers as ordered [name, base64-value] pairs.
Only a hash-pinned helper may execute; production upstream is chatgpt.com and
loopback origins exist solely for synthetic tests. ``deadline`` includes cleanup.
"""
from __future__ import annotations

import base64
import json
import math
import os
from pathlib import Path
import re
import secrets
import socket
import struct
import subprocess
import tempfile
import time
from urllib.parse import urlsplit

from quality_exact_request_probe import (ALLOWED_HEADERS, ExactProbeError, MAX_BODY,
    MAX_HEAD, MAX_RESPONSE, SSECollector, deadline_io, file_digest, read_exact,
    safe_token, sha, summarize_body, summarize_head, write_all, _ready_line)
from quality_usage_probe import strict_json, UsageProbeError

SCHEMA = "codex-quality-exact-websocket-v1"
PATH = "/backend-api/codex/responses"
HEADER_NAME = re.compile(r"[!#$%&'*+.^_`|~0-9A-Za-z-]{1,256}\Z")
NATIVE_ERRORS = frozenset(("ws_handshake_timeout", "ws_write_timeout", "ws_ipc_write_timeout",
    "ws_ipc_read_timeout", "ws_close_timeout", "ws_message_limit", "ws_frame_limit", "ws_transport",
    "ws_connection_closed", "ws_unexpected_eof", "ws_ipc_read", "ws_ipc_write", "ws_output_incomplete",
    "ws_protocol", "ws_request_invalid", "ws_owned_header", "ws_before_handshake", "ws_command_invalid",
    "ws_sequence", "ws_text_utf8", "ws_message_kind", "ws_close_invalid", "ws_command_capacity",
    "ws_metadata", "ws_metadata_limit", "ws_raw_frame"))


def _json(raw):
    try:
        return strict_json(raw)
    except (UsageProbeError, TypeError) as error:
        raise ExactProbeError("invalid_json") from error


def _b64(value):
    if not isinstance(value, str):
        raise ExactProbeError("invalid_base64")
    try:
        return base64.b64decode(value, validate=True)
    except (ValueError, UnicodeError) as error:
        raise ExactProbeError("invalid_base64") from error


def _pairs(headers, request=False):
    if not isinstance(headers, list) or len(headers) > (120 if request else 512):
        raise ExactProbeError("invalid_headers")
    seen, total, hashes = set(), 0, []
    for pair in headers:
        if not isinstance(pair, list) or len(pair) != 2 or not isinstance(pair[0], str):
            raise ExactProbeError("invalid_headers")
        name, value = pair[0], _b64(pair[1])
        lower = name.lower()
        if not HEADER_NAME.fullmatch(name) or any(byte < 32 and byte != 9 or byte == 127 for byte in value):
            raise ExactProbeError("invalid_header_value")
        total += len(name) + len(value)
        if total > MAX_HEAD:
            raise ExactProbeError("header_limit")
        if request:
            if lower in seen or lower not in ALLOWED_HEADERS | {"x-oai-attestation"} and not lower.startswith("x-codex-"):
                raise ExactProbeError("header_not_allowlisted_or_repeated")
            if re.search(rb"(?i)bearer\s|\beyJ[A-Za-z0-9_-]*\.[A-Za-z0-9_-]+\.|\bsk-[A-Za-z0-9_-]{12,}", value):
                raise ExactProbeError("header_contains_credential")
            seen.add(lower)
        hashes.append({"name_sha256": sha(lower.encode()), "value_sha256": sha(value)})
    return hashes


def validate_request(body, captured):
    """Validate transport premises; never reserialize or invent request fields."""
    if not isinstance(body, bytes) or not 0 < len(body) <= MAX_BODY:
        raise ExactProbeError("invalid_request_body_size")
    value = _json(body)
    if (not isinstance(value, dict) or value.get("type") != "response.create"
            or not isinstance(value.get("model"), str) or not value["model"]
            or not isinstance(value.get("input"), list) or value.get("store") is not False
            or value.get("previous_response_id") not in (None, "")
            or "generate" in value and value["generate"] is not None and value["generate"] is not True
            or "stream" in value and value["stream"] is not True):
        raise ExactProbeError("requires_full_first_turn_response_create")
    if not isinstance(captured, dict) or set(captured) - {"schema", "method", "path", "headers"}:
        raise ExactProbeError("invalid_handshake_contract")
    if captured.get("method") != "GET" or captured.get("path") != PATH:
        raise ExactProbeError("invalid_handshake_contract")
    _pairs(captured.get("headers"), request=True)
    return value


def _origin(value):
    try:
        parsed = urlsplit(value)
        if (parsed.username or parsed.password or parsed.path not in ("", "/") or parsed.query or parsed.fragment
                or parsed.scheme not in ("http", "https") or parsed.port == 0):
            raise ValueError()
        if not (parsed.scheme == "https" and parsed.hostname == "chatgpt.com" and parsed.port in (None, 443)
                or parsed.scheme == "http" and parsed.hostname in ("127.0.0.1", "::1") and parsed.port):
            raise ValueError()
    except (ValueError, TypeError) as error:
        raise ExactProbeError("invalid_upstream_origin") from error


def _error_metadata(raw, accepted):
    value = _json(raw)
    if (not isinstance(value, dict) or value.get("accepted") is not accepted
            or value.get("phase") != ("message" if accepted else "handshake") or not isinstance(value.get("code"), str)):
        raise ExactProbeError("invalid_native_error")
    code = value["code"]
    return {"phase": value["phase"], "accepted": accepted,
            "code": code if code in NATIVE_ERRORS else "unknown_native_error", "code_sha256": sha(code.encode())}


def _frame(recv):
    kind, size = struct.unpack("!BI", read_exact(recv, 5))
    if kind not in (4, 65, 66, 67, 68, 69, 70):
        raise ExactProbeError("unknown_native_frame")
    if size > (min(MAX_BODY, MAX_RESPONSE) + 9 if kind == 66 else MAX_HEAD):
        raise ExactProbeError("native_response_frame_limit")
    if kind == 69 and size != 0 or kind == 70 and size != 8 or kind == 66 and size < 9:
        raise ExactProbeError("invalid_native_frame_length")
    return kind, read_exact(recv, size)


def _handshake(raw):
    value = _json(raw)
    if (not isinstance(value, dict) or type(value.get("status")) is not int or not 100 <= value["status"] <= 599
            or value.get("body_scope") not in ("handshake_tail", "none") or value.get("body_complete", False) is not None
            or type(value.get("tail_truncated")) is not bool):
        raise ExactProbeError("invalid_handshake_metadata")
    hashes, body = _pairs(value.get("headers")), _b64(value.get("body_base64"))
    if len(body) > 16 << 10 or (value["status"] == 101 or value["body_scope"] == "none") and (body or value["tail_truncated"]):
        raise ExactProbeError("invalid_handshake_body")
    return value, hashes, body


class WebSocketCollector:
    """Feed exact WS JSON events into the shared scorer, without fake SSE wire."""
    def __init__(self, hidden):
        self.sse = SSECollector(hidden)
        self.messages = 0
        self.total = 0

    def feed(self, raw):
        self.messages += 1
        self.total += len(raw)
        if self.total > MAX_RESPONSE or self.messages > 32768:
            raise ExactProbeError("response_limit")
        event = _json(raw)
        if not isinstance(event, dict):
            raise ExactProbeError("invalid_websocket_event")
        if event.get("stream_id") not in (None, ""):
            raise ExactProbeError("unexpected_response_lane")
        if self.sse.terminal is not None:
            raise ExactProbeError("message_after_terminal_before_ack")
        self.sse.total = self.total
        self.sse.data = [raw]
        self.sse.dispatch()
        if self.sse.terminal_event_data is not None:
            self.sse.terminal_event_data["basis"] = "exact_websocket_text_message_UTF8_bytes_excluding_IPC_and_RFC6455_framing"

    def report(self, handshake_status):
        # Shared collector uses 200 for an accepted HTTP stream. The external
        # report preserves the actual WS 101 handshake, never claims HTTP 200.
        result = self.sse.report(200 if handshake_status == 101 else handshake_status)
        result.update(http_status=handshake_status, handshake_status=handshake_status,
                      websocket_message_count=self.messages, response_bytes=self.total,
                      response_completion_observed=result["protocol_success"])
        return result


def _invalidate(report, category, code):
    # Transport receipt/cleanup failure does not erase a completed response
    # already received. Keep it explicitly separate from the scorable receipt.
    if report.get("response_completion_observed") and "observed_completion" not in report:
        report["observed_completion"] = {key: report.get(key) for key in (
            "answer", "answer_extraction", "final_output", "final_output_source", "final_output_conflict",
            "generated_answer_available", "response_id_sha256", "response_usage", "response_controls")}
    report.update(protocol_success=False, failure_category=category, error_class=code,
                  answer=None, generated_answer_available=False, final_output="", eligible_for_scoring=False)


def _after_handshake_body(body, captured, body_factory, hidden, report):
    """Allow the source-defined WS timing value to change, and nothing else."""
    from quality_control_runner import _safe
    from quality_transport_pair import TIMING_METADATA, _object
    report["body_factory_invocations"] = 1
    try:
        result = body_factory()
    except Exception as error:
        raise ExactProbeError("body_factory_failed") from error
    if not isinstance(result, tuple) or len(result) != 2 or not isinstance(result[1], dict):
        raise ExactProbeError("invalid_body_factory_result")
    updated, projection = result
    validate_request(updated, captured)
    try:
        def position(raw):
            _, fields = _object(raw)
            start, end = fields["client_metadata"]
            metadata, inner = _object(raw[start:end])
            first, last = inner[TIMING_METADATA]
            value = metadata[TIMING_METADATA]
            if type(value) is not str or not re.fullmatch(r"[0-9]{1,16}", value) or not 1 <= int(value) <= 9007199254740991:
                raise ValueError()
            return start + first, start + last, int(value)
        first, last, _ = position(body)
        new_first, new_last, stamp = position(updated)
        if body[:first] != updated[:new_first] or body[last:] != updated[new_last:]:
            raise ExactProbeError("body_factory_changed_non_timing_bytes")
        encoded = json.dumps(projection, ensure_ascii=False, allow_nan=False, separators=(",", ":")).encode()
        if len(encoded) > MAX_HEAD:
            raise ExactProbeError("body_factory_projection_limit")
        _json(encoded)
    except ExactProbeError:
        raise
    except Exception as error:
        raise ExactProbeError("invalid_body_factory_timing_or_projection") from error
    report.update(request_template_sha256=sha(body), request_body_sha256=sha(updated), request_body_bytes=len(updated),
                  request_start_ms=stamp, request_start_ms_scope="factory_after_verified_101_before_single_create",
                  body_factory_projection=_safe(projection, hidden), body_factory_projection_sha256=sha(encoded))
    return updated


def _exchange(stream, body, head, report, collector, deadline, *, body_factory=None, captured=None, hidden=()):
    send = lambda raw: deadline_io(stream, deadline, "send", raw)
    recv = lambda size: deadline_io(stream, deadline, "recv", size)
    encoded = json.dumps(head, separators=(",", ":")).encode()
    if len(encoded) > MAX_HEAD:
        raise ExactProbeError("ipc_head_limit")
    report.update(handshake_dispatch_attempted=True, handshake_ipc_bytes_accepted=0)
    def handshake_accepted(part):
        report["handshake_ipc_bytes_accepted"] += len(part)
    write_all(send, struct.pack("!I", len(encoded)) + encoded, handshake_accepted)
    report.update(handshake_dispatch_count=1, handshake_ipc_write_complete=True)
    kind, raw = _frame(recv)
    if kind == 68:
        report["native_error"] = _error_metadata(raw, False)
        raise ExactProbeError("native_handshake_error")
    if kind == 4:
        _json(raw)
        raise ExactProbeError("native_pre_handshake_error")
    if kind != 65:
        raise ExactProbeError("frame_before_handshake")
    handshake, hashes, tail = _handshake(raw)
    status = handshake["status"]
    report.update(http_status=status, handshake_status=status, http_version="HTTP/1.1",
                  response_header_hashes=hashes, handshake_tail_truncated=handshake["tail_truncated"],
                  handshake_body_complete=None, handshake_body_scope=handshake["body_scope"])
    if status != 101:
        report.update(handshake_error_body_sha256=sha(tail), handshake_error_body_bytes=len(tail))
        if tail:
            try:
                parsed = _json(tail)
                if isinstance(parsed, dict):
                    collector.sse.data = [json.dumps({"type": "error", "error": parsed.get("error", {})}).encode()]
                    collector.sse.dispatch()
                    report["response_error"] = collector.sse.error
            except ExactProbeError:
                pass
        kind, _ = _frame(recv)
        if kind != 69:
            raise ExactProbeError("rejected_handshake_missing_end")
        _invalidate(report, "handshake_rejected", "handshake_rejected")
        return
    if body_factory is not None:
        body = _after_handshake_body(body, captured, body_factory, hidden, report)
        _, sensitive = summarize_head(head)
        report["request_body_summary"] = summarize_body(body, sensitive)
    wire = struct.pack("!BIQB", 82, len(body) + 9, 1, 1) + body
    report.update(ipc_dispatch_attempted=True, ipc_bytes_accepted=0)
    def accepted(part):
        report["ipc_bytes_accepted"] += len(part)
    write_all(send, wire, accepted)
    report.update(native_dispatch_count=1, ws_send_count=1, local_ipc_write_complete=True,
                  actual_ipc_body_sha256=sha(body), actual_ipc_request_sha256=sha(wire))
    sequence, ack = 0, False
    while True:
        kind, raw = _frame(recv)
        if kind == 66:
            sequence += 1
            received, message_type = struct.unpack("!QB", raw[:9])
            if received != sequence or message_type != 1:
                raise ExactProbeError("invalid_message_sequence_or_kind")
            collector.feed(raw[9:])
            report["ws_received_sequence"] = sequence
        elif kind == 70:
            if ack or struct.unpack("!Q", raw)[0] != 1:
                raise ExactProbeError("invalid_or_duplicate_write_ack")
            ack = True
            report["ws_write_acknowledged"] = True
        elif kind == 68:
            report["native_error"] = _error_metadata(raw, True)
            raise ExactProbeError("native_message_error")
        elif kind == 67:
            value = _json(raw)
            if (not isinstance(value, dict) or not isinstance(value.get("reason"), str)
                    or len(value["reason"].encode()) > 123 or value.get("code") is not None
                    and (type(value["code"]) is not int or not (value["code"] in (1000, 1001, 1002, 1003, 1007, 1008, 1009, 1010, 1011, 1012, 1013, 1014) or 3000 <= value["code"] <= 4999))):
                raise ExactProbeError("invalid_native_close")
            report["ws_close"] = {"code": value.get("code"), "reason_sha256": sha(value["reason"].encode())}
            raise ExactProbeError("closed_before_completed_and_ack")
        elif kind == 69:
            raise ExactProbeError("end_before_completed_and_ack")
        else:
            raise ExactProbeError("unexpected_frame_after_handshake")
        if collector.sse.terminal is not None and ack:
            report.update(collector.report(status))
            report.update(eligible_for_scoring=report["protocol_success"] and report["generated_answer_available"],
                          receive_stop_reason="terminal_and_write_ack_observed",
                          peer_close_observed=False,
                          later_transport_events="not_observed_after_terminal_and_ack")
            return


def native_websocket_request(helper, expected_helper_sha, body, captured, token, workspace, hidden=(),
                             timeout=240, upstream="https://chatgpt.com", *, deadline=None, body_factory=None):
    """Return public evidence for one WS inference; stop once terminal + ACK arrive.

    ``native_dispatch_count`` counts complete response.create IPC writes; a partial
    write remains visible via ipc_dispatch_attempted/ipc_bytes_accepted and must
    never be retried. HTTP key names coexist with actual handshake status 101.
    Optional body_factory() -> (body_bytes, projection_report) runs once after
    verified 101. It must perform no I/O. Only the existing source-defined WS
    request-start timestamp's exact JSON value span may differ from the template.
    """
    started = time.monotonic()
    report = {"schema": SCHEMA, "transport": "websocket", "attempts": 0, "native_dispatch_count": 0,
              "handshake_dispatch_count": 0, "ws_send_count": 0, "ws_write_acknowledged": False,
              "automatic_retries": False, "automatic_refresh": False, "http_fallback": False,
              "protocol_success": False, "generated_answer_available": False, "answer": None,
              "final_output": "", "http_status": None, "handshake_status": None,
              "eligible_for_scoring": False, "response_completion_observed": False,
              "body_factory_invocations": 0, "request_start_ms_scope": "caller_supplied_exact_prehandshake_body",
              "upstream_wire_capture": False, "helper_sha256": expected_helper_sha if isinstance(expected_helper_sha, str) and re.fullmatch(r"[0-9a-f]{64}", expected_helper_sha) else None,
              "environment_policy": "isolated_PATH_LANG_no_proxy_or_CA_overrides"}
    process = stream = temporary = None
    collector = None
    path = None
    end = started + 240
    try:
        if (type(timeout) not in (int, float) or not math.isfinite(timeout) or not 1 <= timeout <= 240
                or deadline is not None and (type(deadline) not in (int, float) or not math.isfinite(deadline))):
            raise ExactProbeError("invalid_deadline")
        end = min(started + timeout, deadline) if deadline is not None else started + timeout
        if end - started < 1:
            raise ExactProbeError("deadline_before_start")
        operation_end = end - min(5, (end - started) / 4)
        if os.name != "posix" or not hasattr(socket, "AF_UNIX"):
            raise ExactProbeError("linux_posix_required")
        if not isinstance(expected_helper_sha, str) or not re.fullmatch(r"[0-9a-f]{64}", expected_helper_sha):
            raise ExactProbeError("invalid_helper_digest")
        helper = Path(helper)
        if file_digest(helper) != expected_helper_sha:
            raise ExactProbeError("helper_digest_mismatch")
        _origin(upstream)
        validate_request(body, captured)
        captured = json.loads(json.dumps(captured)) # Freeze caller-owned header data before invoking any callback.
        if body_factory is not None and not callable(body_factory):
            raise ExactProbeError("invalid_body_factory")
        if (not isinstance(token, str) or not token or len(token) > 64 << 10 or not token.isascii()
                or any(ord(c) <= 32 or ord(c) == 127 for c in token)
                or not isinstance(workspace, str) or not re.fullmatch(r"[A-Za-z0-9_-]{1,160}", workspace)):
            raise ExactProbeError("invalid_credential_input")
        hidden = tuple(dict.fromkeys([token, workspace] + [value for value in hidden if isinstance(value, str) and value]))
        collector = WebSocketCollector(hidden)
        report.update(request_body_sha256=sha(body), request_body_bytes=len(body),
                      token_sha256=sha(token.encode()), workspace_sha256=sha(workspace.encode()))
        temporary = tempfile.TemporaryDirectory(prefix="qexactws-")
        os.chmod(temporary.name, 0o700)
        path = str(Path(temporary.name) / "s")
        ipc_token = secrets.token_hex(32)
        startup = {"protocol": 1, "unix_socket": path, "upstream": upstream, "ipc_token": ipc_token,
                   "headers_timeout_ms": max(1, min(90000, int((operation_end - time.monotonic()) * 1000))),
                   "idle_timeout_ms": 0, "http_client_lifecycle": "cli_request"}
        process = subprocess.Popen([str(helper)], stdin=subprocess.PIPE, stdout=subprocess.PIPE,
            stderr=subprocess.DEVNULL, close_fds=True, bufsize=0,
            env={"PATH": os.environ.get("PATH", "/usr/bin:/bin"), "LANG": "C.UTF-8"})
        encoded = json.dumps(startup, separators=(",", ":")).encode()
        write_all(process.stdin.write, struct.pack("!I", len(encoded)) + encoded)
        remaining = operation_end - time.monotonic()
        if remaining <= 0:
            raise TimeoutError()
        ready = _json(_ready_line(process.stdout, min(9, remaining)))
        if (not isinstance(ready, dict) or type(ready.get("protocol")) is not int or ready["protocol"] != 1
                or ready.get("transport") != "unix" or ready.get("unix_socket") != path
                or ready.get("http_client_lifecycle") != "cli_request"
                or not isinstance(ready.get("capabilities"), list)
                or "websocket_message_v1" not in ready["capabilities"]):
            raise ExactProbeError("helper_readiness_mismatch")
        runtime = file_digest(Path("/proc") / str(process.pid) / "exe")
        report["runtime_executable_sha256"] = runtime
        if runtime != expected_helper_sha:
            raise ExactProbeError("helper_runtime_mismatch")
        headers = [list(pair) for pair in captured["headers"]]
        headers.extend([["Authorization", base64.b64encode(("Bearer " + token).encode()).decode()],
                        ["ChatGPT-Account-Id", base64.b64encode(workspace.encode()).decode()]])
        head = {"protocol": 1, "ipc_token": ipc_token, "operation": "websocket_message_v1", "method": "GET",
                "path": PATH, "headers": headers, "body_length": 0, "max_message_bytes": min(MAX_BODY, MAX_RESPONSE),
                "write_timeout_ms": max(1, min(60000, int((operation_end - time.monotonic()) * 1000)))}
        head_summary, sensitive = summarize_head(head)
        report.update(request_header_summary=head_summary, request_body_summary=summarize_body(body, sensitive),
                      handshake_nonauth_header_hashes=_pairs(captured["headers"], request=True))
        stream = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        remaining = operation_end - time.monotonic()
        if remaining <= 0:
            raise TimeoutError()
        stream.settimeout(remaining)
        stream.connect(path)
        report["attempts"] = 1
        _exchange(stream, body, head, report, collector, operation_end,
                  body_factory=body_factory, captured=captured, hidden=hidden)
    except Exception as error:
        if collector is not None:
            try:
                safe = collector.report(report.get("handshake_status"))
                # Preserve the handshake's typed error if no WS event existed.
                if report.get("response_error") and not safe.get("response_error"):
                    safe.pop("response_error", None)
                report.update(safe)
            except Exception:
                pass
        _invalidate(report, "timeout" if isinstance(error, (TimeoutError, socket.timeout)) else "probe_error",
                    error.args[0] if isinstance(error, ExactProbeError) else "unexpected_probe_failure")
    finally:
        cleanup_ok = True
        if stream is not None:
            try:
                stream.close()
            except OSError:
                cleanup_ok = False
        if process is not None:
            try:
                process.stdin.close()
                process.wait(timeout=max(0.01, min(2, end - time.monotonic())))
                report["helper_forced_kill"] = False
            except (OSError, subprocess.TimeoutExpired):
                report["helper_forced_kill"] = True
                cleanup_ok = False
                try:
                    process.kill()
                    process.wait(timeout=max(0.01, min(2, end - time.monotonic())))
                except (OSError, subprocess.TimeoutExpired):
                    pass
            try:
                process.stdout.close()
            except OSError:
                cleanup_ok = False
            report.update(helper_returncode=process.returncode, helper_reaped=process.poll() is not None)
            cleanup_ok = cleanup_ok and report["helper_reaped"] and process.returncode == 0
        if temporary is not None:
            try:
                temporary.cleanup()
            except OSError:
                cleanup_ok = False
        report["local_socket_absent"] = not Path(path).exists() if path else None
        if not cleanup_ok:
            _invalidate(report, "helper_cleanup_failed", "helper_cleanup_failed")
        report["duration_ms"] = round((time.monotonic() - started) * 1000)
    return report
