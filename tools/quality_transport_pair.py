"""Offline 0.155.0 full-first-turn HTTP/Lite -> WebSocket projection.

No networking or credential loading. The HTTP bytes are never rewritten. The
WS frame preserves every existing value's original JSON bytes except the outer
client_metadata object, whose existing entries are themselves byte-preserved.
Call immediately before dispatch with the real request_start_ms. This is a
source-derived controlled transport pair, not a claim of equal HTTP/WS bytes.
"""
from __future__ import annotations

import base64
import hashlib
import json
import re

SCHEMA = "codex-quality-transport-pair-v1"
MAX_BODY = 32 << 20
PATH = "/backend-api/codex/responses"
WS_BETA = "responses_websockets=2026-02-06"
LITE_HEADER = "x-openai-internal-codex-responses-lite"
LITE_METADATA = "ws_request_header_x_openai_internal_codex_responses_lite"
TIMING_METADATA = "x-codex-ws-stream-request-start-ms"
HTTP_FIELDS = frozenset(("model", "instructions", "input", "tools", "tool_choice",
    "parallel_tool_calls", "reasoning", "store", "stream", "stream_options", "include",
    "service_tier", "prompt_cache_key", "text", "client_metadata", "access_programs"))
REQUIRED_FIELDS = frozenset(("model", "input", "tool_choice", "parallel_tool_calls",
    "reasoning", "store", "stream", "include", "prompt_cache_key", "client_metadata"))
PRESERVED_HEADERS = frozenset(("user-agent", "originator", "version", "session-id", "thread-id",
    "x-client-request-id", "x-codex-installation-id", "x-codex-window-id", "x-codex-turn-metadata",
    "x-codex-parent-thread-id", "x-codex-beta-features", "x-codex-routing-hint",
    "x-openai-subagent", "x-openai-memgen-request", "x-openai-internal-codex-residency",
    "x-oai-attestation"))
REMOVED_HEADERS = frozenset(("accept", "content-type", "content-length", "content-encoding",
    "accept-encoding", "host"))
TRACE_FIELDS = {"traceparent": "ws_request_header_traceparent", "tracestate": "ws_request_header_tracestate"}


class TransportPairError(Exception):
    """Fixed error labels only; no caller-controlled data in diagnostics."""


class _Number(str):
    pass


def _sha(value):
    return hashlib.sha256(value).hexdigest()


def _unique(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise TransportPairError("duplicate_json_field")
        result[key] = value
    return result


def _invalid_number(value):
    raise TransportPairError("nonfinite_json")


_DECODER = json.JSONDecoder(object_pairs_hook=_unique, parse_int=_Number, parse_float=_Number,
                            parse_constant=_invalid_number)


def _canonical(value):
    if isinstance(value, _Number):
        return value.encode("ascii")
    if isinstance(value, dict):
        return b"{" + b",".join(_canonical(key) + b":" + _canonical(value[key]) for key in sorted(value)) + b"}"
    if isinstance(value, list):
        return b"[" + b",".join(_canonical(item) for item in value) + b"]"
    return json.dumps(value, ensure_ascii=False, allow_nan=False, separators=(",", ":")).encode()


def _object(raw):
    """Parse once for validation and locate untouched UTF-8 JSON value spans."""
    try:
        if not isinstance(raw, bytes) or not 0 < len(raw) <= MAX_BODY:
            raise TransportPairError("invalid_body_size")
        text = raw.decode("utf-8")
        value = _DECODER.decode(text)
        if not isinstance(value, dict):
            raise TransportPairError("body_must_be_object")
        position = len(text) - len(text.lstrip()) + 1
        fields = {}
        while True:
            while text[position].isspace():
                position += 1
            if text[position] == "}":
                break
            name, position = _DECODER.raw_decode(text, position)
            while text[position].isspace():
                position += 1
            if text[position] != ":":
                raise TransportPairError("invalid_json")
            position += 1
            while text[position].isspace():
                position += 1
            start = position
            _, position = _DECODER.raw_decode(text, position)
            fields[name] = (len(text[:start].encode()), len(text[:position].encode()))
            while text[position].isspace():
                position += 1
            if text[position] == ",":
                position += 1
                continue
            break
        return value, fields
    except (ValueError, UnicodeError, IndexError, RecursionError):
        raise TransportPairError("invalid_json") from None


def _headers(captured):
    if not isinstance(captured, dict) or captured.get("method") != "POST" or captured.get("path") != PATH:
        raise TransportPairError("invalid_http_capture_route")
    if captured.get("request_compression") not in ("none", "zstd"):
        raise TransportPairError("invalid_http_compression")
    rows = captured.get("headers")
    if not isinstance(rows, list) or len(rows) > 120:
        raise TransportPairError("invalid_headers")
    decoded = {}
    for row in rows:
        if not isinstance(row, list) or len(row) != 2 or not all(isinstance(x, str) for x in row):
            raise TransportPairError("invalid_headers")
        name, encoded = row
        key = name.lower()
        if not re.fullmatch(r"[!#$%&'*+.^_`|~0-9A-Za-z-]{1,256}", name) or key in decoded:
            raise TransportPairError("invalid_or_duplicate_header")
        if key not in PRESERVED_HEADERS | REMOVED_HEADERS | {LITE_HEADER, "openai-beta"} | TRACE_FIELDS.keys():
            raise TransportPairError("unsupported_header")
        try:
            value = base64.b64decode(encoded, validate=True)
        except ValueError:
            raise TransportPairError("invalid_header_base64") from None
        if len(value) > 65536 or any(x < 32 and x != 9 or x == 127 for x in value):
            raise TransportPairError("invalid_header_value")
        if re.search(rb"(?i)bearer\s|\beyJ[A-Za-z0-9_-]*\.[A-Za-z0-9_-]+\.|\bsk-[A-Za-z0-9_-]{12,}", value):
            raise TransportPairError("credential_in_non_auth_header")
        decoded[key] = value
    return rows, decoded


def make_websocket_pair(http_body, captured, *, request_start_ms, include_timing_metrics=False):
    """Return (WS bytes, non-auth native GET head, sanitized field evidence).

    Only fresh full first-turn Responses Lite captures are supported. No history,
    question, tool, instruction, cache or existing metadata value is regenerated.
    include_timing_metrics must match the captured CLI RuntimeMetrics feature;
    its presence cannot be inferred from an HTTP request alone.
    """
    if type(request_start_ms) is not int or not 1 <= request_start_ms <= 9007199254740991:
        raise TransportPairError("invalid_request_start_ms")
    if type(include_timing_metrics) is not bool:
        raise TransportPairError("invalid_timing_metrics_flag")
    if not isinstance(captured, dict):
        raise TransportPairError("invalid_http_capture_route")
    body, spans = _object(http_body)
    if set(body) - HTTP_FIELDS or not REQUIRED_FIELDS <= body.keys():
        raise TransportPairError("non_native_or_incomplete_http_fields")
    if captured.get("body_sha256") != _sha(http_body) or captured.get("body_bytes") != len(http_body):
        raise TransportPairError("http_capture_digest_mismatch")
    if body.get("stream") is not True or body.get("store") is not False or body.get("parallel_tool_calls") is not False:
        raise TransportPairError("not_stateless_lite_stream")
    if type(body.get("model")) is not str or not body["model"] or type(body.get("tool_choice")) is not str or body["tool_choice"] != "auto":
        raise TransportPairError("invalid_native_model_controls")
    if "instructions" in body or "tools" in body:
        # Native Lite has empty instructions/None tools and serde omits both.
        # A Go-normalized capture that reinserted them is a different boundary.
        raise TransportPairError("not_native_lite_serialization")
    if not isinstance(body.get("reasoning"), dict) or body["reasoning"].get("context") != "all_turns":
        raise TransportPairError("missing_lite_reasoning_context")
    if (not isinstance(body.get("input"), list) or not body["input"]
            or not isinstance(body["input"][0], dict) or body["input"][0].get("type") != "additional_tools"):
        raise TransportPairError("missing_full_lite_input")
    if body.get("include") != ["reasoning.encrypted_content"]:
        raise TransportPairError("non_native_include")
    metadata = body.get("client_metadata")
    if not isinstance(metadata, dict) or not metadata or any(type(v) is not str for v in metadata.values()):
        raise TransportPairError("invalid_client_metadata")
    if not all(type(metadata.get(k)) is str and metadata[k] for k in ("session_id", "thread_id", "turn_id")):
        raise TransportPairError("missing_session_metadata")
    if type(body.get("prompt_cache_key")) is not str or body["prompt_cache_key"] != metadata["session_id"]:
        raise TransportPairError("nonstandard_first_turn_cache_key")
    if set(metadata) & {LITE_METADATA, TIMING_METADATA, "x-codex-turn-state", *TRACE_FIELDS.values()}:
        raise TransportPairError("capture_already_has_ws_or_turn_state")
    rows, headers = _headers(captured)
    if headers.get(LITE_HEADER) != b"true" or headers.get("accept") != b"text/event-stream" or headers.get("content-type") != b"application/json":
        raise TransportPairError("missing_native_lite_http_headers")
    for header, field in (("session-id", "session_id"), ("thread-id", "thread_id"), ("x-client-request-id", "thread_id")):
        if headers.get(header) != metadata[field].encode():
            raise TransportPairError("header_metadata_identity_mismatch")
    expected_routing = "model=" + body["model"]
    if "service_tier" in body:
        if type(body["service_tier"]) is not str:
            raise TransportPairError("invalid_service_tier")
        expected_routing += ";tier=" + body["service_tier"]
    if headers.get("x-codex-routing-hint") != expected_routing.encode():
        raise TransportPairError("routing_hint_mismatch")
    additions = {LITE_METADATA: "true", TIMING_METADATA: str(request_start_ms)}
    for name, target in TRACE_FIELDS.items():
        if name in headers:
            try:
                additions[target] = headers[name].decode("ascii")
            except UnicodeError:
                raise TransportPairError("invalid_trace_header") from None
    start, end = spans["client_metadata"]
    raw_metadata = http_body[start:end]
    new_metadata = raw_metadata[:-1] + b"," + b",".join(_canonical(k) + b":" + _canonical(v) for k, v in additions.items()) + b"}"
    ws_body = http_body[:start] + new_metadata + http_body[end:]
    opening = ws_body.index(b"{")
    ws_body = ws_body[:opening + 1] + b'"type":"response.create",' + ws_body[opening + 1:]
    ws_rows = []
    header_projection = []
    for name, encoded in rows:
        key = name.lower()
        action = "preserved" if key in PRESERVED_HEADERS else "projected_to_metadata" if key in TRACE_FIELDS or key == LITE_HEADER else "removed_http_transport"
        if key == "openai-beta":
            action = "replaced_ws_beta"
        if key in PRESERVED_HEADERS:
            ws_rows.append([name, encoded])
        header_projection.append({"name": key, "action": action, "value_sha256": _sha(headers[key])})
    ws_rows.append(["OpenAI-Beta", base64.b64encode(WS_BETA.encode()).decode()])
    if include_timing_metrics:
        ws_rows.append(["x-responsesapi-include-timing-metrics", base64.b64encode(b"true").decode()])
    ws_head = {"method": "GET", "path": PATH, "headers": ws_rows}
    ws, ws_spans = _object(ws_body)
    fields = {}
    for name, value in body.items():
        original_raw = http_body[slice(*spans[name])]
        projected_raw = ws_body[slice(*ws_spans[name])]
        equal = original_raw == projected_raw
        if name != "client_metadata" and not equal:
            raise TransportPairError("unexpected_field_mutation")
        fields[name] = {"http_sha256": _sha(_canonical(value)), "ws_sha256": _sha(_canonical(ws[name])),
                        "http_raw_sha256": _sha(original_raw), "ws_raw_sha256": _sha(projected_raw),
                        "raw_value_unchanged": equal}
    _, old_meta_spans = _object(raw_metadata)
    _, new_meta_spans = _object(new_metadata)
    metadata_fields = {}
    for name in metadata:
        old_raw = raw_metadata[slice(*old_meta_spans[name])]
        new_raw = new_metadata[slice(*new_meta_spans[name])]
        if old_raw != new_raw:
            raise TransportPairError("unexpected_metadata_mutation")
        metadata_fields["key_sha256:" + _sha(name.encode())] = {"raw_value_sha256": _sha(old_raw), "raw_value_unchanged": True}
    report = {"schema": SCHEMA, "model_calls": 0, "source_cli_version": "0.155.0", "scope": "full_first_turn_responses_lite",
        "http_body_sha256": _sha(http_body), "ws_body_sha256": _sha(ws_body), "full_body_bytes_equal": False,
        "original_fields": fields, "original_client_metadata_fields": metadata_fields,
        "added_top_level_fields": ["type"], "added_client_metadata_fields": {k: _sha(_canonical(v)) for k,v in additions.items()},
        "headers": header_projection, "ws_non_auth_headers_sha256": _sha(_canonical(ws_rows)),
        "http_request_compression": captured["request_compression"], "ws_compression": "native_connector_permessage_deflate_negotiation",
        "ws_timing_metrics_requested": include_timing_metrics, "first_turn_has_no_turn_state": True,
        "equivalence": "original_field_values_preserved_except_documented_ws_metadata_not_identical_transport_or_state"}
    return ws_body, ws_head, report
