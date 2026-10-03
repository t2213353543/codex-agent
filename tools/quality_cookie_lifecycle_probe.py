"""One HTTP POST under an explicitly bounded native-helper lifecycle.

cold: fresh P -> POST. same_helper: fresh P -> GET models -> POST.
separate_helper: fresh G -> GET models -> clean G -> fresh P -> POST.
Each transaction uses a new IPC socket; only same_helper retains the process
and its native process-local cookie store. This is a process/GET intervention,
not proof that cookies are its only causal difference. No retry or refresh.
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

from quality_exact_request_probe import (ALLOWED_HEADERS, CAPTURE_HEAD_SCHEMA, ExactProbeError,
    MAX_BODY, MAX_HEAD, SSECollector, exact_head, file_digest, sha, summarize_body,
    summarize_head, read_exact, write_all, _ready_line, deadline_io, private_json, _value_summary)
from quality_exact_websocket_probe import _json, _pairs, _b64, _origin
from quality_catalog_compare import catalog_issues

SCHEMA = "codex-quality-cookie-lifecycle-v1"
MODELS_PATH = "/backend-api/codex/models?client_version=0.155.0"
MAX_METADATA = 2 << 20
MAX_ERROR_BODY = 64 << 10
COOKIE_NAMES = frozenset(("__cf_bm", "__cflb", "__cfruid", "__cfseq", "__cfwaitingroom", "__oailb",
    "_cfuvid", "cf_clearance", "cf_ob_info", "cf_use_ob"))
COOKIE_ATTRS = frozenset(("domain", "path", "expires", "max-age", "secure", "httponly", "samesite", "partitioned", "priority"))
ERROR_CODES = frozenset(("rate_limit_exceeded", "rate_limit_reached", "usage_limit_reached", "insufficient_quota",
    "invalid_api_key", "invalid_request_error", "authentication_error", "permission_denied", "access_denied",
    "invalid_grant", "server_error", "internal_error", "upstream_error", "overloaded", "model_not_found",
    "error", "account_deactivated", "tokens", "requests"))
NATIVE_CODES = frozenset(("headers_timeout", "idle_timeout", "total_timeout", "body_limit", "response_limit",
    "upstream_transport", "upstream_timeout", "upstream_connect", "upstream_body", "transport", "cancelled", "ipc_write", "ipc_read", "request_invalid",
    "request_head_invalid", "unauthorized", "proxy_unsupported", "tls_or_client_config", "connect", "timeout",
    "decode", "request", "body", "upstream", "tls", "request_limit", "request_compression_failed"))


def _remaining(deadline, cap=None):
    remaining = deadline - time.monotonic()
    if remaining <= 0:
        raise TimeoutError()
    return min(remaining, cap) if cap is not None else remaining


def _cookie(raw):
    """Inspect bounded Set-Cookie syntax without retaining values or raw IDs."""
    result = {"header_sha256": sha(raw), "header_bytes": len(raw), "parse_complete": False}
    try:
        pieces = raw.split(b";")
        name, value = pieces[0].strip().split(b"=", 1)
        decoded = name.decode("ascii")
        if not re.fullmatch(r"[!#$%&'*+.^_`|~0-9A-Za-z-]{1,256}", decoded):
            return result
        allowed = decoded in COOKIE_NAMES or decoded.startswith("cf_chl_")
        result.update(name=decoded if decoded in COOKIE_NAMES else "cf_chl_*" if decoded.startswith("cf_chl_") else "other",
                      name_sha256=sha(name), allowed_infrastructure_name=allowed,
                      value_sha256=sha(value), value_bytes=len(value))
        attributes = []
        for piece in pieces[1:33]:
            key, separator, item = piece.strip().partition(b"=")
            key_lower = key.lower()
            label = key_lower.decode("ascii", errors="replace")
            entry = {"name": label if label in COOKIE_ATTRS else "other", "name_sha256": sha(key_lower),
                     "value_present": bool(separator)}
            if separator:
                item = item.strip()
                entry.update(value_sha256=sha(item), value_bytes=len(item))
                if label in ("domain", "path"):
                    result[label + "_sha256"] = sha(item)
                elif label == "max-age" and re.fullmatch(rb"-?[0-9]{1,18}", item):
                    entry["seconds"] = int(item)
                elif label == "samesite" and item.lower() in (b"strict", b"lax", b"none"):
                    entry["value"] = item.lower().decode()
                elif label == "priority" and item.lower() in (b"low", b"medium", b"high"):
                    entry["value"] = item.lower().decode()
            attributes.append(entry)
        result.update(attributes=attributes, attribute_count=len(pieces)-1,
                      parse_complete=len(pieces) <= 33)
    except (ValueError, UnicodeError):
        pass
    return result


def safe_response_headers(headers):
    hashes = _pairs(headers)
    cookies, retry = [], []
    for name, encoded in headers:
        raw = _b64(encoded)
        if name.lower() == "set-cookie":
            if len(cookies) >= 64:
                raise ExactProbeError("set_cookie_count_limit")
            cookies.append(_cookie(raw))
        elif name.lower() == "retry-after":
            entry = {"value_sha256": sha(raw), "value_bytes": len(raw), "kind": "other"}
            if re.fullmatch(rb"[0-9]{1,10}", raw.strip()):
                entry.update(kind="delta_seconds", seconds=int(raw.strip()))
            retry.append(entry)
    return {"response_header_hashes": hashes, "set_cookie": cookies, "retry_after": retry,
            "cookie_scope_note": "response_syntax_only; storage_and_next_wire_cookie_not_observed"}


def safe_error_body(raw):
    result = {"body_sha256": sha(raw), "body_bytes": len(raw), "json_valid": False}
    try:
        value = _json(raw)
    except ExactProbeError:
        return result
    result["json_valid"] = True
    result["root_type"] = type(value).__name__
    if not isinstance(value, dict):
        return result
    result["top_level_fields"] = [{"name": key if key in ("error", "type", "code", "message", "status", "status_code") else "other",
                                   "name_sha256": sha(key.encode()), "value_type": type(item).__name__}
                                  for key, item in list(value.items())[:32]]
    candidate = value.get("error", value)
    if isinstance(candidate, dict):
        detail = {}
        for key in ("type", "code"):
            item = candidate.get(key)
            if isinstance(item, str):
                detail[key + "_sha256"] = sha(item.encode())
                detail[key] = item if item in ERROR_CODES else "other"
        for key in ("status", "status_code"):
            item = candidate.get(key, value.get(key))
            if type(item) is int and 100 <= item <= 599:
                detail[key] = item
        if "message" in candidate:
            encoded = json.dumps(candidate["message"], ensure_ascii=False, separators=(",", ":")).encode()
            detail.update(message_sha256=sha(encoded), message_json_bytes=len(encoded))
        result["error"] = detail
    return result


def _validate_capture(body, captured, application_identity):
    if not isinstance(body, bytes) or not 0 < len(body) <= MAX_BODY:
        raise ExactProbeError("invalid_request_body")
    value = _json(body)
    if (not isinstance(value, dict) or value.get("stream") is not True or value.get("store") is not False
            or not isinstance(value.get("input"), list) or not isinstance(value.get("model"), str) or not value["model"]
            or "previous_response_id" in value):
        raise ExactProbeError("requires_exact_first_turn_http_capture")
    if (not isinstance(captured, dict) or set(captured) != {"schema", "method", "path", "headers", "request_compression", "body_sha256", "body_bytes"}
            or captured.get("schema") != CAPTURE_HEAD_SCHEMA or captured.get("method") != "POST"
            or captured.get("path") != "/backend-api/codex/responses" or captured.get("request_compression") not in ("none", "zstd")
            or captured.get("body_sha256") != sha(body) or type(captured.get("body_bytes")) is not int or captured["body_bytes"] != len(body)):
        raise ExactProbeError("capture_contract_or_hash_mismatch")
    _pairs(captured["headers"], request=True)
    decoded = {name.lower(): _b64(encoded) for name, encoded in captured["headers"]}
    if decoded.get("accept") != b"text/event-stream" or decoded.get("content-type") != b"application/json":
        raise ExactProbeError("capture_missing_http_content_headers")
    if not decoded.get("user-agent") or not decoded.get("originator"):
        raise ExactProbeError("capture_missing_application_identity")
    actual = {name: decoded[header].decode("utf-8") for name, header in (
        ("user_agent", "user-agent"), ("originator", "originator"), ("version", "version")) if header in decoded}
    if application_identity is not None:
        if not isinstance(application_identity, dict) or set(application_identity) - {"user_agent", "originator", "version"}:
            raise ExactProbeError("invalid_application_identity")
        if application_identity != actual:
            raise ExactProbeError("application_identity_must_match_capture")
    return actual


def _models_headers(captured, supplied):
    identity_names = {"user-agent", "originator", "version", "x-openai-internal-codex-residency"}
    if supplied is None:
        # Official ModelsClient extra_headers is empty and body is None.
        # No explicit Accept or Content-Type is invented from the POST.
        return [list(pair) for pair in captured["headers"] if pair[0].lower() in identity_names]
    _pairs(supplied, request=True)
    if any(pair[0].lower() not in identity_names | {"accept"} for pair in supplied):
        raise ExactProbeError("models_headers_contain_non_catalog_fields")
    old = {name.lower(): _b64(value) for name, value in captured["headers"]}
    new = {name.lower(): _b64(value) for name, value in supplied}
    if any(new.get(name) != old.get(name) for name in identity_names):
        raise ExactProbeError("models_application_identity_mismatch")
    return [list(pair) for pair in supplied]


def _models_head(headers, token, workspace, ipc_token, remaining_ms):
    headers = [list(pair) for pair in headers]
    headers.extend([["Authorization", base64.b64encode(("Bearer " + token).encode()).decode()],
                    ["ChatGPT-Account-Id", base64.b64encode(workspace.encode()).decode()]])
    return {"protocol": 1, "ipc_token": ipc_token, "method": "GET", "path": MODELS_PATH,
            "headers": headers, "body_length": 0, "total_timeout_ms": remaining_ms}


def _invalidate(report, category, code):
    if report.get("response_completion_observed") and "observed_completion" not in report:
        report["observed_completion"] = {key: report.get(key) for key in ("answer", "final_output", "generated_answer_available",
            "answer_extraction", "response_id_sha256", "response_usage", "response_controls", "final_output_source", "final_output_conflict")}
    report.update(protocol_success=False, eligible_for_scoring=False, answer=None, final_output="",
                  generated_answer_available=False, failure_category=category, error_class=code)


def _collector_report(collector, status):
    result = collector.report(status)
    # SSE error envelopes share the same fixed-code disclosure policy as
    # non-200 HTTP error bodies. Arbitrary identifiers remain hashed.
    error = result.get("response_error", {})
    for key in ("code", "type"):
        value = error.get(key)
        if isinstance(value, str):
            error[key + "_sha256"] = sha(value.encode())
            error[key] = value if value in ERROR_CODES else "other"
    return result


class _Helper:
    def __init__(self, helper, digest, upstream, operation_end, end, record):
        self.helper, self.digest, self.upstream = helper, digest, upstream
        self.operation_end, self.end, self.record = operation_end, end, record
        self.process = self.directory = None
        self.path = None
        self.ipc_token = secrets.token_hex(32)
        self.closed = False

    def start(self):
        _remaining(self.operation_end)
        self.directory = tempfile.TemporaryDirectory(prefix="qcookie-")
        os.chmod(self.directory.name, 0o700)
        self.path = str(Path(self.directory.name) / "s")
        config = {"protocol": 1, "unix_socket": self.path, "upstream": self.upstream, "ipc_token": self.ipc_token,
                  "headers_timeout_ms": min(45000, max(1, int(_remaining(self.operation_end) * 1000))),
                  "idle_timeout_ms": 0, "http_client_lifecycle": "cli_request"}
        self.process = subprocess.Popen([str(self.helper)], stdin=subprocess.PIPE, stdout=subprocess.PIPE,
            stderr=subprocess.DEVNULL, close_fds=True, bufsize=0,
            env={"PATH": os.environ.get("PATH", "/usr/bin:/bin"), "LANG": "C.UTF-8"})
        self.record.update(pid=self.process.pid, process_started=True)
        encoded = json.dumps(config, separators=(",", ":")).encode()
        write_all(self.process.stdin.write, struct.pack("!I", len(encoded)) + encoded)
        ready = _json(_ready_line(self.process.stdout, _remaining(self.operation_end, 8)))
        if (not isinstance(ready, dict) or type(ready.get("protocol")) is not int or ready["protocol"] != 1
                or ready.get("transport") != "unix" or ready.get("unix_socket") != self.path
                or ready.get("http_client_lifecycle") != "cli_request" or not isinstance(ready.get("capabilities"), list)):
            raise ExactProbeError("helper_readiness_mismatch")
        runtime = file_digest(Path("/proc") / str(self.process.pid) / "exe")
        self.record["runtime_executable_sha256"] = runtime
        if runtime != self.digest:
            raise ExactProbeError("helper_runtime_mismatch")
        self.capabilities = ready["capabilities"]
        self.record.update(readiness_verified=True, lifecycle="cli_request")
        return self

    def connect(self, phase):
        _remaining(self.operation_end)
        if self.closed or self.process.poll() is not None:
            raise ExactProbeError("helper_not_alive_before_transaction")
        phase.update(helper_scope_id=self.record["scope_id"], helper_pid=self.process.pid,
                     runtime_executable_sha256=self.digest, helper_alive_before_dispatch=True,
                     ipc_connection_id=secrets.token_hex(16))
        stream = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        try:
            stream.settimeout(_remaining(self.operation_end))
            stream.connect(self.path)
        except Exception:
            stream.close()
            raise
        return stream

    def close(self):
        if self.closed:
            return self.record.get("cleanup_ok", False)
        self.closed = True
        okay = True
        if self.process is not None:
            try:
                self.process.stdin.close()
                self.process.wait(timeout=_remaining(self.end, 2))
                self.record["forced_kill"] = False
            except (OSError, ValueError, TimeoutError, subprocess.TimeoutExpired):
                self.record["forced_kill"] = True
                okay = False
                try:
                    self.process.kill()
                    self.process.wait(timeout=_remaining(self.end, 2))
                except (OSError, ValueError, TimeoutError, subprocess.TimeoutExpired):
                    pass
            try:
                self.process.stdout.close()
            except (OSError, ValueError):
                okay = False
            self.record.update(returncode=self.process.poll(), reaped=self.process.poll() is not None)
            okay = okay and self.record["reaped"] and self.record["returncode"] == 0
        if self.directory is not None:
            try:
                self.directory.cleanup()
            except OSError:
                okay = False
        self.record.update(local_socket_absent=not Path(self.path).exists() if self.path else None, cleanup_ok=okay)
        return okay


def _catalog(raw, expected_model):
    value = _json(raw)
    issues = catalog_issues(value)
    if issues:
        raise ExactProbeError("models_response_schema_invalid")
    matches = [index for index, item in enumerate(value["models"]) if item["slug"] == expected_model]
    if len(matches) != 1:
        raise ExactProbeError("models_expected_model_not_unique_or_absent")
    exact = private_json(raw, exact_numbers=True)
    model = exact["models"][matches[0]]
    important = ("slug", "supported_reasoning_levels", "default_reasoning_level", "base_instructions", "model_messages",
                 "tool_mode", "use_responses_lite", "context_window", "max_context_window", "auto_compact_token_limit",
                 "supported_in_api", "visibility", "truncation_policy", "experimental_supported_tools")
    return {"catalog_count": len(value["models"]), "expected_model_count": 1,
            "expected_model_sha256": sha(expected_model.encode()), "catalog_schema_issues": [],
            "models_canonical_summary": _value_summary(exact["models"]), "expected_model_summary": _value_summary(model),
            "expected_model_fields": {name: dict(_value_summary(model[name]), present=True) if name in model else {"present": False}
                                      for name in important}}


def _read_response(stream, deadline, phase, collector=None, expected_model=None):
    recv = lambda count: deadline_io(stream, deadline, "recv", count)
    status, body, count = None, bytearray(), 0
    while count < 65536:
        count += 1
        kind, size = struct.unpack("!BI", read_exact(recv, 5))
        if size > MAX_HEAD or kind not in (1, 2, 3, 4) or kind == 3 and size:
            raise ExactProbeError("invalid_native_frame")
        raw = read_exact(recv, size)
        if kind == 1:
            value = _json(raw)
            if (status is not None or not isinstance(value, dict) or type(value.get("status")) is not int
                    or not 200 <= value["status"] <= 599 or value.get("version") not in ("HTTP/1.0", "HTTP/1.1", "HTTP/2.0", "HTTP/2", "HTTP/3.0")):
                raise ExactProbeError("invalid_native_response_headers")
            status = value["status"]
            phase.update(http_status=status, http_version=value["version"], **safe_response_headers(value.get("headers")))
        elif kind == 2:
            if status is None:
                raise ExactProbeError("body_before_native_headers")
            if collector is not None and status == 200:
                collector.feed(raw)
                if collector.terminal is not None:
                    phase.update(response_stop="terminal_event", ipc_end_observed=False)
                    return
            else:
                body.extend(raw)
                if len(body) > (MAX_METADATA if status == 200 and collector is None else MAX_ERROR_BODY):
                    raise ExactProbeError("response_body_limit")
        elif kind == 3:
            if status is None:
                raise ExactProbeError("end_before_native_headers")
            phase.update(ipc_end_observed=True, response_stop="ipc_end", body_sha256=sha(body), body_bytes=len(body))
            if collector is not None and status == 200:
                collector.feed(b"", eof=True)
            elif status == 200:
                summary = _catalog(bytes(body), expected_model)
                phase.update(protocol_success=True, metadata_model_calls=0, **summary)
            else:
                phase.update(error_body=safe_error_body(bytes(body)), protocol_success=False)
            return
        else:
            # HTTP IPC ERROR is a fixed ASCII code, unlike WS's JSON metadata.
            try:
                code = raw.decode("ascii")
            except UnicodeError:
                code = None
            phase["native_error"] = {"code": code if code in NATIVE_CODES else "other",
                                     "payload_sha256": sha(raw)}
            raise ExactProbeError("native_transport_error")
    raise ExactProbeError("native_frame_count_limit")


def _transaction(helper, head, body, phase, collector=None, expected_model=None):
    started = time.monotonic()
    stream = None
    try:
        stream = helper.connect(phase)
        head["total_timeout_ms"] = max(1, int(_remaining(helper.operation_end) * 1000))
        head_summary, sensitive = summarize_head(head)
        phase.update(request_header_summary=head_summary, request_body_sha256=sha(body), request_body_bytes=len(body))
        if collector is not None:
            phase["request_body_summary"] = summarize_body(body, sensitive)
        encoded = json.dumps(head, separators=(",", ":")).encode()
        if len(encoded) > MAX_HEAD:
            raise ExactProbeError("ipc_head_limit")
        phase.update(ipc_dispatch_attempted=True, ipc_bytes_accepted=0)
        def accepted(part):
            phase["ipc_bytes_accepted"] += len(part)
        write_all(lambda raw: deadline_io(stream, helper.operation_end, "send", raw), struct.pack("!I", len(encoded)) + encoded + body, accepted)
        phase.update(native_dispatch_count=1, local_ipc_write_complete=True, actual_ipc_body_sha256=sha(body))
        _read_response(stream, helper.operation_end, phase, collector, expected_model)
    except Exception as error:
        phase.update(protocol_success=False, error_class=error.args[0] if isinstance(error, ExactProbeError) else "timeout" if isinstance(error, TimeoutError) else "probe_error")
        raise
    finally:
        if stream is not None:
            try:
                stream.close()
                phase["ipc_socket_closed"] = True
            except (OSError, ValueError):
                phase["ipc_socket_closed"] = False
        phase["duration_ms"] = round((time.monotonic()-started)*1000)


def native_lifecycle_request(helper, expected_helper_sha, body, captured, token, workspace, hidden=(), timeout=240,
                             *, mode="cold", deadline=None, application_identity=None, models_headers=None, upstream="https://chatgpt.com"):
    """Execute at most one POST, with an optional one-GET lifecycle control.

    application_identity, if supplied, must exactly match captured UA/originator/
    optional version. None derives these from the capture. It never substitutes
    the usage endpoint's potentially different CLI application identity.
    """
    started = time.monotonic()
    report = {"schema": SCHEMA, "transport": "http", "mode": mode if mode in ("cold", "same_helper", "separate_helper") else "invalid",
              "protocol_success": False, "eligible_for_scoring": False, "answer": None, "final_output": "",
              "generated_answer_available": False, "response_completion_observed": False,
              "automatic_retries": False, "automatic_refresh": False, "http_fallback": False,
              "attempts": 0, "model_post_attempts": 0, "native_dispatch_count": 0,
              "metadata_get_count": 0, "metadata_get": {"requested": mode != "cold", "native_dispatch_count": 0},
              "inference": {"native_dispatch_count": 0}, "helpers": [], "upstream_wire_capture": False,
              "cookie_intervention_scope": "prior_GET_and_process_lifecycle_not_cookie_only",
              "environment_policy": "isolated_PATH_LANG_no_proxy_or_CA_overrides"}
    helpers, collector = [], None
    end = started + 240
    try:
        if (mode not in ("cold", "same_helper", "separate_helper") or type(timeout) not in (int, float)
                or not math.isfinite(timeout) or not 1 <= timeout <= 240
                or deadline is not None and (type(deadline) not in (int, float) or not math.isfinite(deadline))):
            raise ExactProbeError("invalid_mode_or_deadline")
        end = min(started+timeout, deadline) if deadline is not None else started+timeout
        if end-started < 1:
            raise ExactProbeError("deadline_before_start")
        operation_end = end-min(8, (end-started)/4)
        if os.name != "posix" or not hasattr(socket, "AF_UNIX"):
            raise ExactProbeError("linux_posix_required")
        helper = Path(helper)
        if not isinstance(expected_helper_sha, str) or not re.fullmatch(r"[a-f0-9]{64}", expected_helper_sha) or file_digest(helper) != expected_helper_sha:
            raise ExactProbeError("helper_digest_mismatch")
        _origin(upstream)
        identity = _validate_capture(body, captured, application_identity)
        expected_model = _json(body)["model"]
        captured = json.loads(json.dumps(captured))
        get_headers = _models_headers(captured, models_headers)
        if (not isinstance(token, str) or not token or not token.isascii() or len(token) > 64 << 10
                or any(ord(char) <= 32 or ord(char) == 127 for char in token)
                or not isinstance(workspace, str) or not re.fullmatch(r"[A-Za-z0-9_-]{1,160}", workspace)):
            raise ExactProbeError("invalid_credential_input")
        hidden = tuple(dict.fromkeys([token, workspace]+[value for value in hidden if isinstance(value, str) and value]))
        collector = SSECollector(hidden)
        report.update(helper_sha256=expected_helper_sha, token_sha256=sha(token.encode()), workspace_sha256=sha(workspace.encode()),
                      request_body_sha256=sha(body), request_body_bytes=len(body),
                      application_identity_sha256=sha(json.dumps(identity,sort_keys=True,separators=(",", ":")).encode()),
                      captured_head_sha256=sha(json.dumps(captured,separators=(",", ":")).encode()))
        def launch(role):
            record = {"role": role, "scope_id": secrets.token_hex(16), "process_started": False}
            report["helpers"].append(record)
            child = _Helper(helper, expected_helper_sha, upstream, operation_end, end, record)
            helpers.append(child)
            return child.start()
        current = launch("post" if mode == "cold" else "models_then_post" if mode == "same_helper" else "models")
        if mode != "cold":
            stage = report["metadata_get"]
            stage["header_source"] = "captured_models_get" if models_headers is not None else "source_default_identity_only_no_explicit_accept"
            _transaction(current, _models_head(get_headers, token, workspace, current.ipc_token, 1), b"", stage, expected_model=expected_model)
            if stage.get("protocol_success") is not True or stage.get("ipc_end_observed") is not True:
                raise ExactProbeError("models_get_not_successful_complete")
            if current.process.poll() is not None:
                raise ExactProbeError("helper_died_after_metadata_get")
            stage["helper_alive_after_complete_get"] = True
            if mode == "separate_helper":
                if not current.close():
                    raise ExactProbeError("metadata_helper_cleanup_failed")
                current = launch("post")
        if captured["request_compression"] == "zstd" and "request_zstd_v1" not in current.capabilities:
            raise ExactProbeError("helper_compression_unavailable")
        phase = report["inference"]
        post_head = exact_head(captured, body, token, workspace, current.ipc_token, 1)
        _transaction(current, post_head, body, phase, collector)
        report.update(_collector_report(collector, phase.get("http_status")))
        report.update(response_completion_observed=report["protocol_success"],
                      eligible_for_scoring=report["protocol_success"] and report["generated_answer_available"])
    except Exception as error:
        if collector is not None:
            try:
                observed = _collector_report(collector, report["inference"].get("http_status"))
                report.update(observed, response_completion_observed=observed["protocol_success"])
            except Exception:
                pass
        _invalidate(report, "timeout" if isinstance(error, (TimeoutError, socket.timeout)) else "probe_error",
                    error.args[0] if isinstance(error, ExactProbeError) else "unexpected_probe_failure")
    finally:
        cleanup_ok = True
        for child in reversed(helpers):
            cleanup_ok = child.close() and cleanup_ok
        get, post = report["metadata_get"], report["inference"]
        report.update(metadata_get_count=get.get("native_dispatch_count", 0), native_dispatch_count=post.get("native_dispatch_count", 0),
                      model_post_attempts=int(bool(post.get("ipc_dispatch_attempted"))), attempts=int(bool(post.get("ipc_dispatch_attempted"))),
                      helper_reaped=bool(helpers) and all(item.get("reaped") is True for item in report["helpers"]),
                      helper_forced_kill=any(item.get("forced_kill") is True for item in report["helpers"]),
                      local_socket_absent=bool(helpers) and all(item.get("local_socket_absent") is True for item in report["helpers"]))
        for key in ("request_header_summary", "request_body_summary", "actual_ipc_body_sha256", "local_ipc_write_complete", "ipc_dispatch_attempted", "ipc_bytes_accepted", "http_version", "response_header_hashes", "set_cookie", "retry_after", "error_body"):
            if key in post:
                report[key] = post[key]
        report["same_helper_reuse_verified"] = bool(mode == "same_helper" and get.get("helper_scope_id") and
            get.get("helper_scope_id") == post.get("helper_scope_id") and get.get("helper_pid") == post.get("helper_pid") and
            get.get("ipc_connection_id") != post.get("ipc_connection_id") and get.get("helper_alive_after_complete_get") is True)
        report["separate_helper_verified"] = bool(mode == "separate_helper" and get.get("helper_scope_id") and post.get("helper_scope_id") and
            get["helper_scope_id"] != post["helper_scope_id"] and report["helpers"][0].get("cleanup_ok") is True)
        if not cleanup_ok:
            _invalidate(report, "helper_cleanup_failed", "helper_cleanup_failed")
        report["duration_ms"] = round((time.monotonic()-started)*1000)
    return report
