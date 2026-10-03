#!/usr/bin/env python3
"""One native GET usage sample; no inference, refresh, reserve, reset or retry.

Callers may send a model request only when eligibility.allowed is exactly True.
This observes the sampling interval, not the backend state at a later POST.
Credentials and raw response bodies stay in memory. JWT parsing checks local
identity consistency only; it does not verify a signature or infer entitlement.
"""
from __future__ import annotations

import base64
from datetime import datetime, timezone
import hashlib
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

from quality_native_ipc_capture import _ready_line, file_digest, read_exact, write_all
from quality_official_cli_probe import AUTH_CLAIM, jwt_claims

SCHEMA = "codex-quality-usage-v1"
ENDPOINT = "/backend-api/wham/usage"
MAX_BODY = 1 << 20
MAX_FRAME = 65536
REASONS = frozenset(("rate_limit_reached", "workspace_owner_credits_depleted",
    "workspace_member_credits_depleted", "workspace_owner_usage_limit_reached",
    "workspace_member_usage_limit_reached", "unknown"))
PLANS = frozenset(("guest", "free", "go", "plus", "pro", "prolite", "free_workspace",
    "team", "self_serve_business_prolite", "self_serve_business_usage_based", "business",
    "ent26", "enterprise_cbp_automation", "enterprise_cbp_usage_based", "education",
    "quorum", "k12", "enterprise", "edu", "edu_plus", "edu_pro", "unknown"))


class UsageProbeError(Exception):
    """Fixed labels only, never upstream text or credential values."""


def sha(raw):
    return hashlib.sha256(raw).hexdigest()


def utc_now():
    return datetime.now(timezone.utc).isoformat()


def strict_json(raw):
    def pairs(items):
        result = {}
        for key, value in items:
            if key in result:
                raise UsageProbeError("duplicate_json_member")
            result[key] = value
        return result
    def invalid(_):
        raise UsageProbeError("nonfinite_json")
    try:
        result = json.loads(raw.decode("utf-8"), object_pairs_hook=pairs, parse_constant=invalid)
        pending = [(result, 0)]
        nodes = 0
        while pending:
            node, depth = pending.pop()
            nodes += 1
            if depth > 64 or nodes > 65536:
                raise UsageProbeError("json_structure_limit")
            if isinstance(node, float) and not math.isfinite(node):
                raise UsageProbeError("nonfinite_json")
            if isinstance(node, str):
                node.encode("utf-8")
            if isinstance(node, dict):
                for name, value in node.items():
                    name.encode("utf-8")
                    pending.append((value, depth + 1))
            elif isinstance(node, list):
                pending.extend((value, depth + 1) for value in node)
        return result
    except (ValueError, UnicodeError, RecursionError) as error:
        raise UsageProbeError("invalid_json") from error


def timed_io(stream, deadline, operation, value):
    remaining = deadline - time.monotonic()
    if remaining <= 0:
        raise TimeoutError()
    stream.settimeout(remaining)
    return getattr(stream, operation)(value)


def remaining_time(deadline, cap):
    remaining = deadline - time.monotonic()
    if remaining <= 0:
        raise TimeoutError()
    return min(cap, remaining)


def optional_bool(value):
    return value if type(value) is bool else None


def numeric(value, integer=False):
    if type(value) not in ((int,) if integer else (int, float)):
        return None
    if isinstance(value, float) and not math.isfinite(value):
        return None
    return value if -(1 << 63) <= value < (1 << 63) else None


def rfc_time(value):
    if not isinstance(value, str) or not re.fullmatch(
            r"\d{4}-\d\d-\d\dT(?:[01]\d|2[0-3]):[0-5]\d:[0-5]\d(?:\.\d{1,9})?(?:Z|[+-](?:[01]\d|2[0-3]):[0-5]\d)", value):
        return None
    try:
        datetime.fromisoformat(value.replace("Z", "+00:00"))
    except ValueError:
        return None
    return value


def safe_rate(value):
    if not isinstance(value, dict):
        return None
    result = {key: optional_bool(value.get(key)) for key in ("allowed", "limit_reached")}
    for key in ("primary_window", "secondary_window"):
        window = value.get(key)
        result[key] = ({name: numeric(window.get(name), name != "used_percent") for name in
            ("used_percent", "limit_window_seconds", "reset_after_seconds", "reset_at")}
            if isinstance(window, dict) else None)
    return result


def safe_amount(value):
    if isinstance(value, str) and len(value) <= 128 and re.fullmatch(
            r"-?(?:0|[1-9]\d*)(?:\.\d+)?(?:[eE][+-]?\d+)?", value):
        return value
    return None


def identity_field(payload, name, expected):
    if name not in payload:
        return {"state": "not_observed"}
    value = payload[name]
    if value is None:
        return {"state": "not_observed", "present": True}
    if not isinstance(value, str) or not value or len(value) > 512:
        return {"state": "invalid"}
    return {"state": "not_comparable" if expected is None else "matched" if value == expected else "mismatch",
            "sha256": sha(value.encode())}


def summarize_usage(payload, workspace, expected_user=None, expected_model="gpt-5.6-sol"):
    if not isinstance(payload, dict):
        raise UsageProbeError("usage_not_object")
    if not isinstance(expected_model, str) or not re.fullmatch(r"[A-Za-z0-9._:/-]{1,256}", expected_model):
        raise UsageProbeError("invalid_expected_model")
    identities = {name: identity_field(payload, name, expected) for name, expected in (
        ("account_id", workspace), ("workspace_id", workspace),
        ("user_id", expected_user), ("chatgpt_user_id", expected_user))}
    plan = payload.get("plan_type")
    result = {"plan_type": plan if isinstance(plan, str) and plan in PLANS else "unknown",
              "rate_limit": safe_rate(payload.get("rate_limit")), "identity": identities,
              "model_usage": {}, "expected_model": expected_model,
              "expected_model_state": "not_observed"}
    reason = payload.get("rate_limit_reached_type")
    result["rate_limit_reached_type"] = (reason.get("type") if isinstance(reason, dict)
        and isinstance(reason.get("type"), str) and reason.get("type") in REASONS else "unknown") if reason is not None else None
    credits = payload.get("credits")
    if isinstance(credits, dict):
        result["credits"] = {key: optional_bool(credits.get(key)) for key in
                             ("has_credits", "unlimited", "overage_limit_reached")}
        result["credits"]["balance"] = safe_amount(credits.get("balance"))
    reset = payload.get("rate_limit_reset_credits")
    if isinstance(reset, dict):
        result["rate_limit_reset_credits"] = {key: numeric(reset.get(key), True) for key in
                                               ("available_count", "applicable_available_count")}
    spend = payload.get("spend_control")
    if isinstance(spend, dict):
        result["spend_control"] = {"reached": optional_bool(spend.get("reached"))}
        limit = spend.get("individual_limit")
        if isinstance(limit, dict):
            result["spend_control"]["individual_limit"] = {
                key: safe_amount(limit.get(key)) for key in ("limit", "used", "remaining")}
            result["spend_control"]["individual_limit"].update({key: numeric(limit.get(key), key.startswith("reset_"))
                for key in ("used_percent", "remaining_percent", "reset_after_seconds", "reset_at")})
    models = payload.get("model_usage")
    if isinstance(models, dict):
        if len(models) > 512:
            raise UsageProbeError("model_usage_limit")
        for name, value in models.items():
            label = name if name == expected_model or re.fullmatch(r"(?:gpt-|codex-|o[1-9])[A-Za-z0-9._-]{1,100}", name) else "sha256:" + sha(name.encode())
            if isinstance(value, dict):
                result["model_usage"][label] = {"available": optional_bool(value.get("available")),
                    "credits_would_enable": optional_bool(value.get("credits_would_enable")),
                    "available_at": rfc_time(value.get("available_at"))}
            else:
                result["model_usage"][label] = None
        if expected_model in models:
            value = models[expected_model]
            availability = optional_bool(value.get("available")) if isinstance(value, dict) else None
            result["expected_model_state"] = "available" if availability is True else "unavailable" if availability is False else "unknown"
    elif models is not None:
        result["expected_model_state"] = "unknown"
    # Presence of another model (e.g. astra) never supplies a sol decision.
    states = {entry["state"] for entry in identities.values()}
    rate = result["rate_limit"]
    if "mismatch" in states:
        eligibility = (False, "response_identity_mismatch")
    elif "invalid" in states or "not_comparable" in states:
        eligibility = (None, "response_identity_unknown")
    elif rate is not None and (rate["allowed"] is False or rate["limit_reached"] is True):
        eligibility = (False, "ordinary_usage_denied")
    elif isinstance(spend, dict) and spend.get("reached") is True:
        eligibility = (False, "spend_limit_reached")
    elif result["expected_model_state"] == "unavailable":
        eligibility = (False, "requested_model_unavailable")
    elif result["rate_limit_reached_type"] in REASONS - {"unknown"}:
        eligibility = (False, "backend_limit_reason")
    elif rate is None or rate["allowed"] is None or rate["limit_reached"] is None:
        eligibility = (None, "ordinary_usage_unknown")
    elif spend is not None and (not isinstance(spend, dict) or type(spend.get("reached")) is not bool):
        eligibility = (None, "spend_state_unknown")
    elif result["rate_limit_reached_type"] == "unknown":
        eligibility = (None, "backend_limit_reason_unknown")
    elif result["expected_model_state"] == "unknown":
        eligibility = (None, "requested_model_unknown")
    else:
        eligibility = (True, "ordinary_usage_allowed_no_observed_model_denial")
    result["eligibility"] = {"allowed": eligibility[0], "reason": eligibility[1]}
    return result


def validate_identity(token, workspace, timeout):
    if not isinstance(token, str) or len(token) > 65536 or not re.fullmatch(r"[A-Za-z0-9_.-]+", token):
        raise UsageProbeError("credential_invalid")
    if not isinstance(workspace, str) or not re.fullmatch(r"[A-Za-z0-9_-]{1,160}", workspace):
        raise UsageProbeError("credential_invalid")
    try:
        claims = jwt_claims(token)
        auth = claims.get(AUTH_CLAIM)
        expiry = claims.get("exp")
        if not isinstance(auth, dict) or auth.get("chatgpt_account_id") != workspace:
            raise ValueError()
        if type(expiry) not in (int, float) or not math.isfinite(expiry) or expiry <= time.time() + timeout + 300:
            raise ValueError()
        user = auth.get("chatgpt_user_id")
        if user is not None and (not isinstance(user, str) or not user or len(user) > 512):
            raise ValueError()
        return user
    except Exception as error:
        raise UsageProbeError("credential_identity_or_expiry_invalid") from error


def validate_application_identity(identity):
    # Values must come from the caller's recorded official CLI capture. Syntax
    # validation cannot establish provenance or infer a host/terminal identity.
    if identity is None:
        raise UsageProbeError("application_identity_not_supplied")
    limits = {"user_agent": 1024, "originator": 128, "version": 64}
    if not isinstance(identity, dict) or set(identity) != set(limits):
        raise UsageProbeError("application_identity_invalid")
    for name, limit in limits.items():
        value = identity[name]
        if not isinstance(value, str) or not 0 < len(value) <= limit or value != value.strip() or any(ord(ch) < 32 or ord(ch) > 126 for ch in value):
            raise UsageProbeError("application_identity_invalid")
    return dict(identity)


def usage_head(token, workspace, ipc_token, remaining_ms, application_identity):
    identity = validate_application_identity(application_identity)
    headers = (("Authorization", "Bearer " + token), ("ChatGPT-Account-Id", workspace),
        ("User-Agent", identity["user_agent"]), ("Originator", identity["originator"]),
        ("Version", identity["version"]), ("Accept", "application/json"))
    return {"protocol": 1, "ipc_token": ipc_token, "method": "GET", "path": ENDPOINT, "body_length": 0,
            "headers": [[name, base64.b64encode(value.encode()).decode()] for name, value in headers],
            "total_timeout_ms": remaining_ms}


def read_response(stream, deadline):
    reader = lambda size: timed_io(stream, deadline, "recv", size)
    response = None
    body = bytearray()
    for _ in range(8192):
        kind, size = struct.unpack("!BI", read_exact(reader, 5))
        if size > MAX_FRAME:
            raise UsageProbeError("frame_limit")
        raw = read_exact(reader, size)
        if kind == 1:
            if response is not None:
                raise UsageProbeError("duplicate_response_headers")
            response = strict_json(raw)
            if not isinstance(response, dict) or type(response.get("status")) is not int or not 100 <= response["status"] <= 599:
                raise UsageProbeError("invalid_response_headers")
            if response.get("version") not in ("HTTP/1.0", "HTTP/1.1", "HTTP/2.0", "HTTP/2", "HTTP/3.0", "HTTP/3"):
                raise UsageProbeError("invalid_response_headers")
            headers = response.get("headers")
            if not isinstance(headers, list) or len(headers) > 256:
                raise UsageProbeError("invalid_response_headers")
            for pair in headers:
                if not isinstance(pair, list) or len(pair) != 2 or not all(isinstance(v, str) for v in pair):
                    raise UsageProbeError("invalid_response_headers")
                if not re.fullmatch(r"[!#$%&'*+.^_`|~0-9A-Za-z-]{1,256}", pair[0]):
                    raise UsageProbeError("invalid_response_headers")
                try:
                    value = base64.b64decode(pair[1], validate=True)
                except ValueError as error:
                    raise UsageProbeError("invalid_response_headers") from error
                if len(value) > 16384 or b"\r" in value or b"\n" in value or b"\0" in value:
                    raise UsageProbeError("invalid_response_headers")
        elif kind == 2:
            if response is None:
                raise UsageProbeError("body_before_headers")
            body.extend(raw)
            if len(body) > MAX_BODY:
                raise UsageProbeError("response_limit")
        elif kind == 3:
            if size or response is None:
                raise UsageProbeError("invalid_response_end")
            return response, bytes(body)
        elif kind == 4:
            raise UsageProbeError("native_transport_error")
        else:
            raise UsageProbeError("invalid_frame_kind")
    raise UsageProbeError("frame_count_limit")


def native_usage(helper: Path, expected_helper_sha, token, workspace, timeout=35,
                 upstream="https://chatgpt.com", expected_model="gpt-5.6-sol", application_identity=None):
    started = time.monotonic()
    report = {"schema": SCHEMA, "method": "GET", "endpoint": ENDPOINT, "model_calls": 0,
        "attempts": 0, "native_dispatch_count": 0, "automatic_retries": False,
        "started_at": utc_now(), "protocol_success": False, "upstream_wire_capture": False,
        "environment_policy": "isolated_PATH_LANG_no_proxy_or_CA_overrides",
        "eligibility": {"allowed": None, "reason": "probe_not_completed"}}
    child = stream = None
    path = None
    directory = None
    try:
        if type(timeout) not in (int, float) or not math.isfinite(timeout) or not 0 < timeout <= 35:
            raise UsageProbeError("invalid_timeout")
        deadline = started + timeout
        identity = validate_application_identity(application_identity)
        user = validate_identity(token, workspace, timeout)
        if not isinstance(expected_model, str) or not re.fullmatch(r"[A-Za-z0-9._:/-]{1,256}", expected_model):
            raise UsageProbeError("invalid_expected_model")
        endpoint = urlsplit(upstream)
        if upstream != "https://chatgpt.com" and not (endpoint.scheme == "http" and endpoint.hostname in ("127.0.0.1", "::1")
                and endpoint.port and not endpoint.username and not endpoint.password and endpoint.path in ("", "/")
                and not endpoint.query and not endpoint.fragment):
            raise UsageProbeError("upstream_not_official_or_loopback")
        if os.name != "posix" or not hasattr(socket, "AF_UNIX"):
            raise UsageProbeError("linux_posix_required")
        if not isinstance(expected_helper_sha, str) or not re.fullmatch(r"[a-f0-9]{64}", expected_helper_sha) or file_digest(helper) != expected_helper_sha:
            raise UsageProbeError("helper_digest_mismatch")
        report.update(helper_sha256=expected_helper_sha, token_sha256=sha(token.encode()),
                      workspace_sha256=sha(workspace.encode()), expected_user_sha256=sha(user.encode()) if user else None,
                      expected_model=expected_model, timeout_seconds=timeout,
                      application_identity_sha256=sha(json.dumps(identity, sort_keys=True, separators=(",", ":")).encode()))
        directory = tempfile.TemporaryDirectory(prefix="qusage-")
        os.chmod(directory.name, 0o700)
        path = Path(directory.name) / "s"
        ipc = secrets.token_hex(32)
        config = {"protocol": 1, "unix_socket": str(path), "upstream": upstream, "ipc_token": ipc,
                  "headers_timeout_ms": min(20000, max(1, int(remaining_time(deadline, timeout) * 1000))),
                  "idle_timeout_ms": 0, "http_client_lifecycle": "cli_request"}
        child = subprocess.Popen([str(helper)], stdin=subprocess.PIPE, stdout=subprocess.PIPE,
            stderr=subprocess.DEVNULL, close_fds=True, bufsize=0, env={"PATH": "/usr/bin:/bin", "LANG": "C.UTF-8"})
        raw = json.dumps(config, separators=(",", ":")).encode()
        write_all(child.stdin.write, struct.pack("!I", len(raw)) + raw)
        ready = strict_json(_ready_line(child.stdout, remaining_time(deadline, 8)))
        if not isinstance(ready, dict) or type(ready.get("protocol")) is not int or ready.get("protocol") != 1 or ready.get("transport") != "unix" or ready.get("unix_socket") != str(path):
            raise UsageProbeError("helper_readiness_mismatch")
        runtime = file_digest(Path("/proc") / str(child.pid) / "exe")
        report["runtime_executable_sha256"] = runtime
        if runtime != expected_helper_sha:
            raise UsageProbeError("helper_runtime_mismatch")
        stream = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        stream.settimeout(remaining_time(deadline, 8))
        stream.connect(str(path))
        remaining_ms = int(remaining_time(deadline, timeout) * 1000)
        if remaining_ms < 1:
            raise TimeoutError()
        head = usage_head(token, workspace, ipc, remaining_ms, identity)
        report["request_header_hashes"] = [{"name": name.lower(), "value_sha256": sha(base64.b64decode(value))}
                                            for name, value in head["headers"]]
        raw = json.dumps(head, separators=(",", ":")).encode()
        report.update(attempts=1, ipc_dispatch_attempted=True, dispatch_started_at=utc_now())
        write_all(lambda data: timed_io(stream, deadline, "send", data), struct.pack("!I", len(raw)) + raw)
        report["native_dispatch_count"] = 1
        response, body = read_response(stream, deadline)
        report.update(http_status=response["status"], http_version=response["version"],
            body_bytes=len(body), body_sha256=sha(body), response_received_at=utc_now())
        if response["status"] != 200:
            raise UsageProbeError("usage_http_error")
        payload = strict_json(body)
        summary = summarize_usage(payload, workspace, user, expected_model)
        report.update(protocol_success=True, usage=summary, eligibility=dict(summary["eligibility"]))
    except Exception as error:
        label = (str(error) if isinstance(error, UsageProbeError) else "timeout" if isinstance(error, (TimeoutError, socket.timeout)) else "probe_error")
        report.update(error_class=label, eligibility={"allowed": None, "reason": label})
    finally:
        if stream is not None:
            stream.close()
        if child is not None:
            try:
                child.stdin.close()
                child.wait(timeout=2)
                report["helper_forced_kill"] = False
            except (subprocess.TimeoutExpired, OSError):
                report["helper_forced_kill"] = True
                try:
                    child.kill()
                    child.wait(timeout=2)
                except (subprocess.TimeoutExpired, OSError):
                    pass
            report.update(helper_exit_code=child.poll(), helper_reaped=child.poll() is not None)
            child.stdout.close()
            if report.get("helper_exit_code") != 0 or report.get("helper_forced_kill"):
                report["eligibility"] = {"allowed": None, "reason": "helper_cleanup_failed"}
        if directory is not None:
            try:
                directory.cleanup()
            except OSError:
                report["eligibility"] = {"allowed": None, "reason": "socket_cleanup_failed"}
        report["socket_removed"] = not path.exists() if path is not None else None
        report["finished_at"] = utc_now()
        report["elapsed_ms"] = round((time.monotonic() - started) * 1000)
        report["cleanup_budget_seconds"] = 4
    return report
