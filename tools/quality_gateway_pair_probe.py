"""One bounded HTTP request to a private loopback gateway fixture, never public TLS.

Caller owns fixture binary/account pins and lifecycle. The original decoded CLI
entity is sent unchanged, without zstd on this local ingress hop. Native upstream
compression is owned by the gateway and must be observed independently. No retry,
redirect, proxy discovery, credential file, subprocess, or command-line entrypoint.
"""
from __future__ import annotations

import base64
import http.client
import json
import math
import re
import socket
import threading
import time
from urllib.parse import urlsplit

from quality_cookie_lifecycle_probe import (_validate_capture, safe_error_body, safe_response_headers,
                                          _collector_report, _invalidate)
from quality_exact_request_probe import ExactProbeError, MAX_HEAD, SSECollector, sha, summarize_body
from quality_transport_pair import PRESERVED_HEADERS, LITE_HEADER

SCHEMA = "codex-quality-gateway-pair-v1"
LOCAL_PATH = "/v1/responses"
MAX_ERROR_BODY = 64 << 10
MAX_READ = 64 << 10
REQUEST_HEADERS = PRESERVED_HEADERS - {"x-oai-attestation"} | {
    "accept", "content-type", "openai-beta", LITE_HEADER, "traceparent", "tracestate",
    "x-responsesapi-include-timing-metrics",
}


def _remaining(deadline):
    remaining = deadline - time.monotonic()
    if remaining <= 0:
        raise TimeoutError()
    return remaining


def _target(endpoint):
    if not isinstance(endpoint, str) or len(endpoint) > 256:
        raise ExactProbeError("invalid_loopback_endpoint")
    # Exact spelling avoids host aliases, alternate IPv4 encodings, userinfo,
    # percent-escaped paths, fragments, queries and parser normalization gaps.
    match = re.fullmatch(r"http://127\.0\.0\.1:([1-9][0-9]{0,4})/v1/responses", endpoint)
    if match is None or not 1 <= int(match[1]) <= 65535:
        raise ExactProbeError("invalid_loopback_endpoint")
    parsed = urlsplit(endpoint)
    return parsed.hostname, parsed.port


def gateway_pair_request(endpoint, gateway_key, body, captured, hidden=(), timeout=240,
                         *, deadline=None, application_identity=None):
    """Return a sanitized receipt; endpoint/key are private in-memory arguments.

    `native_upstream_observed` stays False: this process only observes localhost.
    A completed answer remains available if cleanup subsequently fails, while
    protocol_success/eligible_for_scoring become false.
    """
    started = time.monotonic()
    report = {"schema": SCHEMA, "transport": "loopback_http_to_gateway", "protocol_success": False,
        "eligible_for_scoring": False, "generated_answer_available": False, "answer": None, "final_output": "",
        "response_completion_observed": False, "attempts": 0, "model_post_attempts": 0,
        "automatic_retries": False, "automatic_refresh": False, "redirects_followed": 0,
        "proxy_discovery": False, "python_tls": False, "native_upstream_observed": False,
        "ingress_request_compression": "identity", "upstream_request_compression": "not_observed",
        "cleanup_completed": False, "capture_body_unchanged": False}
    connection = response = timer = None
    io_socket = None
    expired = threading.Event()
    collector = None
    failure = None
    status = None
    try:
        if type(timeout) not in (int, float) or not math.isfinite(timeout) or not 1 <= timeout <= 240:
            raise ExactProbeError("invalid_timeout")
        if deadline is not None and (type(deadline) not in (int, float) or not math.isfinite(deadline)):
            raise ExactProbeError("invalid_deadline")
        end = min(started + timeout, deadline) if deadline is not None else started + timeout
        _remaining(end)
        host, port = _target(endpoint)
        if (not isinstance(gateway_key, str) or not 1 <= len(gateway_key) <= 512
                or not gateway_key.isascii() or any(ord(c) <= 32 or ord(c) == 127 for c in gateway_key)):
            raise ExactProbeError("invalid_private_gateway_key")
        identity = _validate_capture(body, captured, application_identity)
        rows = []
        for name, encoded in captured["headers"]:
            if name.lower() not in REQUEST_HEADERS:
                raise ExactProbeError("unreviewed_capture_header")
            rows.append((name, base64.b64decode(encoded, validate=True)))
        secrets = tuple(dict.fromkeys([gateway_key] + [v for v in hidden if isinstance(v, str) and v]))
        collector = SSECollector(secrets)
        report.update(request_body_sha256=sha(body), request_body_bytes=len(body),
            source_request_compression=captured["request_compression"],
            captured_head_sha256=sha(json.dumps(captured, separators=(",", ":")).encode()),
            request_body_summary=summarize_body(body, [v.encode() for v in secrets]),
            application_identity_sha256=sha(json.dumps(identity, sort_keys=True, separators=(",", ":")).encode()),
            request_header_hashes=[{"name": name.lower(), "value_sha256": sha(value)} for name, value in rows],
            fixture_identity_binding="caller_must_join_hash_pinned_server_and_account_receipts")
        connection = http.client.HTTPConnection(host, port, timeout=_remaining(end))
        connection.connect()
        io_socket = connection.sock

        def expire():
            expired.set()
            try:
                io_socket.shutdown(socket.SHUT_RDWR)
            except OSError:
                pass
            try:
                io_socket.close()
            except OSError:
                pass

        # getresponse() may read many header lines; per-read socket timeouts
        # alone would let a trickling peer reset the effective total deadline.
        timer = threading.Timer(_remaining(end), expire)
        timer.daemon = True
        timer.start()
        connection.putrequest("POST", LOCAL_PATH, skip_accept_encoding=True)
        for name, value in rows:
            connection.putheader(name, value)
        connection.putheader("Authorization", "Bearer " + gateway_key)
        connection.putheader("Content-Length", str(len(body)))
        report.update(attempts=1, model_post_attempts=1, dispatch_attempted=True)
        io_socket.settimeout(_remaining(end))
        connection.endheaders(body)
        report.update(local_http_write_complete=True, capture_body_unchanged=True,
                      actual_ingress_body_sha256=sha(body))
        io_socket.settimeout(_remaining(end))
        response = connection.getresponse()
        status = response.status
        report.update(http_status=status, http_version="HTTP/1.1" if response.version == 11 else "HTTP/1.0" if response.version == 10 else "unknown")
        headers = [[name, base64.b64encode(value.encode("latin-1")).decode()] for name, value in response.getheaders()]
        if sum(len(n) + len(v) for n, v in response.getheaders()) > MAX_HEAD:
            raise ExactProbeError("response_header_limit")
        report.update(safe_response_headers(headers))
        encodings = response.headers.get_all("Content-Encoding", [])
        if encodings and encodings != ["identity"]:
            raise ExactProbeError("unsupported_gateway_response_encoding")
        media = response.getheader("Content-Type", "").split(";", 1)[0].strip().lower()
        stream = status == 200 and media == "text/event-stream"
        error_body = bytearray()
        while True:
            io_socket.settimeout(_remaining(end))
            chunk = response.read1(MAX_READ)
            if expired.is_set():
                raise TimeoutError()
            if not chunk:
                report["response_eof_observed"] = True
                if stream:
                    collector.feed(b"", eof=True)
                else:
                    report["error_body"] = safe_error_body(bytes(error_body))
                break
            if stream:
                collector.feed(chunk)
                if collector.terminal is not None:
                    report.update(response_stop="terminal_event", response_eof_observed=False)
                    break
            else:
                error_body.extend(chunk)
                if len(error_body) > MAX_ERROR_BODY:
                    report["error_body"] = {"body_bytes_at_least": len(error_body), "prefix_sha256": sha(error_body[:MAX_ERROR_BODY]), "truncated": True}
                    raise ExactProbeError("error_body_limit")
        if not stream:
            failure = "unexpected_response_content_type" if status == 200 else "http_error"
    except Exception as error:
        failure = "deadline_exceeded" if expired.is_set() or isinstance(error, (TimeoutError, socket.timeout)) else str(error) if isinstance(error, ExactProbeError) else "local_http_failure"
    finally:
        if timer is not None:
            timer.cancel()
            timer.join(timeout=1)
        clean = True
        for item in (response, connection):
            if item is not None:
                try:
                    item.close()
                except Exception:
                    clean = False
        report["cleanup_completed"] = clean
        if collector is not None:
            try:
                outcome = _collector_report(collector, status)
                report.update(outcome)
                report["response_completion_observed"] = outcome["protocol_success"]
            except Exception:
                failure = failure or "response_evidence_invalid"
        if not clean:
            failure = failure or "local_http_cleanup_failed"
        if failure:
            _invalidate(report, "timeout" if failure == "deadline_exceeded" else "http_error" if failure == "http_error" else "probe_error", failure)
        else:
            report["eligible_for_scoring"] = report["protocol_success"] and report["generated_answer_available"]
        report["duration_ms"] = round((time.monotonic() - started) * 1000)
    return report
