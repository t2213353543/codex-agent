"""Offline single-field prompt_cache_key projection and dispatch pin check.

Only the original top-level JSON value span is replaced. Other request bytes,
including metadata/session IDs, and all header values/order are retained. The
returned capture head changes only body_sha256/body_bytes. Inputs stay in memory.

CLI 0.155.0 core/src/client.rs:497 accepts a String override; common.rs:278
serializes Option<String>. No CLI length/UUID validation was found. This tool's
1..256 printable ASCII replacement limit is an experiment bound, not a claim
about an upstream limit. Different cache/session values are permitted overrides.
"""
from __future__ import annotations

import base64
from copy import deepcopy
from decimal import Decimal, InvalidOperation
import hashlib
import json
import re

SCHEMA = "codex-quality-cache-key-projection-v1"
CAPTURE_SCHEMA = "codex-quality-captured-request-v1"
MAX_BODY = 32 << 20
MAX_HEAD = 64 << 10
MAX_FIELDS = 512
MAX_NODES = 262144
MAX_DEPTH = 64
MISSING = object()
KNOWN_FIELDS = frozenset(("model", "instructions", "input", "tools", "tool_choice", "parallel_tool_calls",
    "reasoning", "store", "stream", "stream_options", "include", "service_tier", "prompt_cache_key", "text",
    "client_metadata", "access_programs"))
HEADER_NAMES = frozenset(("accept", "content-type", "user-agent", "originator", "version", "openai-beta",
    "session-id", "thread-id", "session_id", "conversation_id", "x-request-id", "x-client-request-id", "x-oai-attestation",
    "x-openai-subagent", "x-openai-memgen-request", "x-openai-internal-codex-responses-lite",
    "x-openai-internal-codex-residency", "x-responsesapi-include-timing-metrics", "traceparent", "tracestate"))
HEAD_FIELDS = frozenset(("schema", "method", "path", "headers", "request_compression", "body_sha256", "body_bytes"))
HASH = re.compile(r"[0-9a-f]{64}\Z")


class CacheKeyProjectionError(Exception):
    """Only fixed labels; never include caller content or exception messages."""


class _Number(str):
    pass


def _sha(raw):
    return hashlib.sha256(raw).hexdigest()


def _unique(pairs):
    result = {}
    for name, value in pairs:
        if name in result:
            raise CacheKeyProjectionError("duplicate_json_member")
        result[name] = value
    return result


def _invalid(_):
    raise CacheKeyProjectionError("nonfinite_json")


DECODER = json.JSONDecoder(object_pairs_hook=_unique, parse_int=_Number, parse_float=_Number, parse_constant=_invalid)


def _canonical(value):
    if isinstance(value, _Number):
        return value.encode("ascii")
    if isinstance(value, dict):
        return b"{"+b",".join(_canonical(key)+b":"+_canonical(value[key]) for key in sorted(value))+b"}"
    if isinstance(value, list):
        return b"["+b",".join(_canonical(item) for item in value)+b"]"
    return json.dumps(value, ensure_ascii=False, allow_nan=False, separators=(",", ":")).encode()


def _object(raw):
    if type(raw) is not bytes or not 0 < len(raw) <= MAX_BODY:
        raise CacheKeyProjectionError("invalid_body_size")
    try:
        text = raw.decode("utf-8")
        value = DECODER.decode(text)
        if not isinstance(value, dict):
            raise CacheKeyProjectionError("body_must_be_object")
        if len(value) > MAX_FIELDS:
            raise CacheKeyProjectionError("top_level_field_limit")
        pending, nodes = [(value, 0)], 0
        while pending:
            item, depth = pending.pop()
            nodes += 1
            if depth > MAX_DEPTH or nodes > MAX_NODES:
                raise CacheKeyProjectionError("json_structure_limit")
            if isinstance(item, _Number):
                if len(item) > 128 or not Decimal(item).is_finite():
                    raise CacheKeyProjectionError("invalid_or_excessive_json_number")
            elif isinstance(item, str):
                item.encode("utf-8")
            elif isinstance(item, dict):
                for name, child in item.items():
                    name.encode("utf-8")
                    pending.append((child, depth+1))
            elif isinstance(item, list):
                pending.extend((child, depth+1) for child in item)
        position = len(text)-len(text.lstrip(" \t\r\n"))+1
        spans, encoded_cursor, char_cursor = {}, 0, 0
        def byte_position(index):
            nonlocal encoded_cursor, char_cursor
            encoded_cursor += len(text[char_cursor:index].encode())
            char_cursor = index
            return encoded_cursor
        while True:
            while text[position] in " \t\r\n":
                position += 1
            if text[position] == "}":
                break
            name, position = DECODER.raw_decode(text, position)
            while text[position] in " \t\r\n":
                position += 1
            if text[position] != ":":
                raise CacheKeyProjectionError("invalid_json")
            position += 1
            while text[position] in " \t\r\n":
                position += 1
            start = position
            _, position = DECODER.raw_decode(text, position)
            spans[name] = (byte_position(start), byte_position(position))
            while text[position] in " \t\r\n":
                position += 1
            if text[position] != ",":
                break
            position += 1
        return value, spans
    except CacheKeyProjectionError:
        raise
    except (ValueError, UnicodeError, IndexError, RecursionError, InvalidOperation) as error:
        raise CacheKeyProjectionError("invalid_json") from error


def _validate_head(body, captured):
    if (type(captured) is not dict or set(captured) != HEAD_FIELDS or captured.get("schema") != CAPTURE_SCHEMA
            or captured.get("method") != "POST" or captured.get("path") != "/backend-api/codex/responses"
            or captured.get("request_compression") not in ("none", "zstd")
            or captured.get("body_sha256") != _sha(body) or type(captured.get("body_bytes")) is not int
            or captured["body_bytes"] != len(body)):
        raise CacheKeyProjectionError("capture_contract_or_body_pin_mismatch")
    headers = captured.get("headers")
    if type(headers) is not list or len(headers) > 120:
        raise CacheKeyProjectionError("invalid_capture_headers")
    seen, decoded = set(), {}
    total = 0
    for pair in headers:
        if type(pair) is not list or len(pair) != 2 or any(type(value) is not str for value in pair):
            raise CacheKeyProjectionError("invalid_capture_headers")
        name, encoded = pair
        lower = name.lower()
        if (not re.fullmatch(r"[!#$%&'*+.^_`|~0-9A-Za-z-]{1,256}", name) or lower in seen
                or lower not in HEADER_NAMES and not lower.startswith("x-codex-")):
            raise CacheKeyProjectionError("invalid_or_repeated_capture_header")
        try:
            value = base64.b64decode(encoded, validate=True)
        except (ValueError, UnicodeError) as error:
            raise CacheKeyProjectionError("invalid_capture_header_encoding") from error
        total += len(name)+len(value)
        if total > MAX_HEAD or any(char < 32 and char != 9 or char == 127 for char in value):
            raise CacheKeyProjectionError("invalid_capture_header_value")
        if re.search(rb"(?i)bearer\s|\beyJ[A-Za-z0-9_-]*\.[A-Za-z0-9_-]+\.|\bsk-[A-Za-z0-9_-]{12,}", value):
            raise CacheKeyProjectionError("credential_in_capture_header")
        seen.add(lower)
        decoded[lower] = value
    if decoded.get("accept") != b"text/event-stream" or decoded.get("content-type") != b"application/json":
        raise CacheKeyProjectionError("capture_missing_http_content_headers")


def _validate(body, captured):
    value, spans = _object(body)
    if (value.get("stream") is not True or value.get("store") is not False or "previous_response_id" in value
            or type(value.get("input")) is not list or type(value.get("model")) is not str or not value["model"].strip()
            or type(value.get("prompt_cache_key")) is not str or not value["prompt_cache_key"]):
        raise CacheKeyProjectionError("requires_full_first_turn_with_string_cache_key")
    _validate_head(body, captured)
    return value, spans


def _key_evidence(value):
    encoded = value.encode()
    return {"utf8_sha256": _sha(encoded), "utf8_bytes": len(encoded), "json_type": "string"}


def verify_cache_key_pin(body, captured_head, expected_cache_key_sha256):
    """Dispatch preflight: pin SHA256(decoded prompt_cache_key UTF8), not JSON bytes.

    Body length/hash and the complete captured-head contract are also checked.
    Caller must invoke this before any helper, GET, or model dispatch. No key is
    derived from session metadata and no input is rewritten by this function.
    """
    value, _ = _validate(body, captured_head)
    if type(expected_cache_key_sha256) is not str or not HASH.fullmatch(expected_cache_key_sha256):
        raise CacheKeyProjectionError("invalid_expected_cache_key_pin")
    actual = _sha(value["prompt_cache_key"].encode())
    if actual != expected_cache_key_sha256:
        raise CacheKeyProjectionError("cache_key_pin_mismatch")
    return {"schema": SCHEMA, "expected_cache_key_sha256": expected_cache_key_sha256,
            "actual_cache_key_sha256": actual, "cache_key_pin_matched": True,
            "key_hash_basis": "SHA256(decoded_cache_key_UTF8)", "body_sha256": _sha(body), "body_bytes": len(body),
            "model_calls": 0, "network_calls": 0}


def project_cache_key(body, captured_head, new_key):
    """Return (projected_body, detached_updated_head, safe_change_evidence).

    Unknown body fields are opaque and retained. Escaped JSON member spellings
    resolve case-sensitively to their decoded names. Duplicate names, including
    escaped equivalents, are rejected at any depth before transformation.
    """
    value, spans = _validate(body, captured_head)
    if type(new_key) is not str or not 1 <= len(new_key) <= 256 or any(ord(char) < 32 or ord(char) > 126 for char in new_key):
        raise CacheKeyProjectionError("replacement_key_must_be_1_to_256_printable_ASCII")
    start, end = spans["prompt_cache_key"]
    encoded = json.dumps(new_key, ensure_ascii=False, separators=(",", ":")).encode()
    if len(body)-(end-start)+len(encoded) > MAX_BODY:
        raise CacheKeyProjectionError("projected_body_limit")
    projected = body[:start]+encoded+body[end:]
    updated_head = deepcopy(captured_head)
    updated_head.update(body_sha256=_sha(projected), body_bytes=len(projected))
    post, next_spans = _validate(projected, updated_head)
    if post["prompt_cache_key"] != new_key:
        raise CacheKeyProjectionError("projection_internal_key_mismatch")
    next_start, next_end = next_spans["prompt_cache_key"]
    if body[:start] != projected[:next_start] or body[end:] != projected[next_end:]:
        raise CacheKeyProjectionError("projection_crossed_value_boundary")
    metadata = value.get("client_metadata")
    session = metadata.get("session_id", MISSING) if isinstance(metadata, dict) else MISSING
    fields = []
    for name in sorted(value):
        original_span, next_span = spans[name], next_spans[name]
        raw_before, raw_after = body[slice(*original_span)], projected[slice(*next_span)]
        fields.append({"name": name if name in KNOWN_FIELDS else "name_sha256:"+_sha(name.encode()),
                       "name_sha256": _sha(name.encode()), "original_value_sha256": _sha(raw_before),
                       "projected_value_sha256": _sha(raw_after), "raw_value_unchanged": raw_before == raw_after,
                       "original_value_bytes": len(raw_before), "projected_value_bytes": len(raw_after)})
    stable_head = {key: captured_head[key] for key in captured_head if key not in ("body_sha256", "body_bytes")}
    evidence = {"schema": SCHEMA, "model_calls": 0, "network_calls": 0,
        "scope": "exact_top_level_prompt_cache_key_JSON_value_span_only",
        "original_body_sha256": _sha(body), "projected_body_sha256": _sha(projected),
        "original_body_bytes": len(body), "projected_body_bytes": len(projected), "body_bytes_changed": body != projected,
        "original_key": _key_evidence(value["prompt_cache_key"]), "projected_key": _key_evidence(new_key),
        "key_hash_basis": "SHA256(decoded_cache_key_UTF8); value_span_hashes_include_original_JSON_quotes_and_escapes",
        "original_cache_key_matches_metadata_session": type(session) is str and value["prompt_cache_key"] == session,
        "projected_cache_key_matches_metadata_session": type(session) is str and new_key == session,
        "metadata_session_comparable": type(session) is str,
        "prefix": {"original_sha256": _sha(body[:start]), "projected_sha256": _sha(projected[:next_start]), "bytes": start, "unchanged": True},
        "suffix": {"original_sha256": _sha(body[end:]), "projected_sha256": _sha(projected[next_end:]), "bytes": len(body)-end, "unchanged": True},
        "cache_value_span": {"original_start": start, "original_end": end, "projected_start": next_start, "projected_end": next_end},
        "fields": fields, "field_count": len(fields), "non_cache_fields_byte_identical": all(item["raw_value_unchanged"] for item in fields if item["name"] != "prompt_cache_key"),
        "head_change_fields": ["body_sha256", "body_bytes"], "non_body_head_fields_unchanged": True,
        "unchanged_head_fields_canonical_sha256": _sha(_canonical(stable_head)),
        "header_order_and_values_unchanged": True, "session_metadata_unchanged": True,
        "replacement_bound_scope": "experimental_tool_limit_not_upstream_or_CLI_maximum",
        "signature_or_provider_acceptance_verified": False}
    return projected, updated_head, evidence


__all__ = ["CacheKeyProjectionError", "project_cache_key", "verify_cache_key_pin"]
