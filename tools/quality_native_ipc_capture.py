#!/usr/bin/env python3
"""Opt-in, test-only observation of the Go -> native-helper Unix IPC boundary.

The real helper still owns HTTP, TLS, compression, cookies and upstream sockets.
Only its local Unix socket name is changed. Request and response bytes are relayed
without reserialization. No request bodies, bearer tokens or raw IPC heads are
written to disk. This is not an upstream wire/TLS capture or Go accountScope hook.
"""
from __future__ import annotations

from quality_nested_session_evidence import summarize_nested_session_evidence

import argparse
import base64
import hashlib
import json
import os
from pathlib import Path
import queue
import re
import signal
import socket
import stat
import struct
import subprocess
import sys
import threading
import time
import uuid

MAX_HEAD = 64 << 10
MAX_BODY = 32 << 20
CAPTURE_SCHEMA = 4
SUMMARY_FIELD_LIMIT = 512
SUMMARY_ITEM_LIMIT = 8192
CA_KEYS = ("SSL_CERT_FILE", "SSL_CERT_DIR", "CODEX_CA_CERTIFICATE")
PLAIN_HEADERS = frozenset(("accept", "content-type", "user-agent", "originator",
                           "version", "openai-beta", "x-codex-routing-hint"))
HASH_HEADERS = frozenset(("session-id", "conversation-id", "x-codex-turn-metadata",
                          "x-codex-parent-thread-id", "x-client-request-id"))
BODY_FIELD_NAMES = frozenset(("model", "instructions", "input", "tools", "tool_choice",
    "parallel_tool_calls", "reasoning", "store", "stream", "stream_options", "include",
    "service_tier", "prompt_cache_key", "prompt_cache_options", "text", "client_metadata",
    "access_programs", "previous_response_id", "generate", "type", "stream_id",
    "metadata", "max_output_tokens", "temperature", "top_p", "truncation", "user"))
CLIENT_METADATA_NAMES = frozenset(("session_id", "thread_id", "turn_id", "parent_thread_id",
    "traceparent", "tracestate", "x-codex-turn-state", "x-codex-turn-metadata",
    "x-codex-beta-features", "x-codex-ws-stream-request-start-ms",
    "x-codex-responses-lite", "x-openai-internal-codex-responses-lite",
    "x-codex-parent-thread-id", "x-codex-session-id", "x-codex-thread-id",
    "x-codex-turn-id", "x-codex-window-id", "x-codex-request-start-time",
    "x-codex-client-request-id", "x-codex-traceparent", "x-codex-tracestate",
    "x-codex-request-header-traceparent", "guardian_credits_requested"))
INPUT_ITEM_TYPES = frozenset(("message", "reasoning", "compaction", "compaction_summary",
    "compaction_trigger", "function_call", "function_call_output", "custom_tool_call",
    "custom_tool_call_output", "local_shell_call", "local_shell_call_output",
    "tool_search_call", "tool_search_output", "additional_tools", "agent_message",
    "web_search_call", "image_generation_call", "computer_call", "computer_call_output",
    "file_search_call", "mcp_call", "mcp_list_tools", "mcp_approval_request", "mcp_approval_response"))
INPUT_ITEM_FIELD_NAMES = frozenset(("id", "type", "role", "content", "tools", "body", "input", "output",
    "arguments", "encrypted_content", "encrypted_function_args", "summary", "phase", "status", "name",
    "namespace", "call_id", "author", "recipient", "action", "execution", "format", "metadata",
    "internal_chat_message_metadata_passthrough"))
ITEM_METADATA_NAMES = frozenset(("turn_id", "create_time", "content_item_kinds", "cell_id",
                                "executed_tool_calls", "tool_calls_complete"))
STABLE_CONTROL_FIELDS = frozenset(("model", "instructions", "tools", "tool_choice",
    "parallel_tool_calls", "reasoning", "store", "stream", "stream_options", "include",
    "service_tier", "text", "access_programs"))


def digest(raw: bytes) -> str:
    return hashlib.sha256(raw).hexdigest()


def file_digest(path: Path) -> str:
    value = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1 << 20), b""):
            value.update(chunk)
    return value.hexdigest()


def write_all(writer, raw: bytes, observed=None) -> None:
    """Handle partial writes; observe only bytes the destination accepted."""
    position = 0
    while position < len(raw):
        part = raw[position:]
        sent = writer(part)
        if not isinstance(sent, int) or sent <= 0 or sent > len(part):
            raise OSError("invalid_write_count")
        if observed is not None:
            observed(part[:sent])
        position += sent


def read_exact(reader, count: int) -> bytes:
    result = bytearray()
    while len(result) < count:
        part = reader(count - len(result))
        if not part:
            raise EOFError("truncated_ipc")
        result.extend(part)
    return bytes(result)


def private_directory(path: Path) -> Path:
    path.mkdir(mode=0o700, parents=True, exist_ok=True)
    info = path.lstat()
    if not stat.S_ISDIR(info.st_mode):
        raise ValueError("capture_directory_not_directory")
    if os.name == "posix" and (info.st_mode & 0o077 or info.st_uid != os.geteuid()):
        raise ValueError("capture_directory_not_private")
    return path.resolve()


class EventSink:
    """One exclusive 0600 file per wrapper instance; serialized thread writes."""
    def __init__(self, directory: Path, instance: str):
        self.lock = threading.Lock()
        self.invalid_path = directory / (instance + ".invalid")
        self.failed = False
        self.failure_reason = None
        flags = os.O_CREAT | os.O_EXCL | os.O_WRONLY | getattr(os, "O_NOFOLLOW", 0) | getattr(os, "O_BINARY", 0)
        self.fd = os.open(str(directory / (instance + ".jsonl")), flags, 0o600)

    def emit(self, event: dict) -> None:
        encoded = (json.dumps(event, sort_keys=True, separators=(",", ":")) + "\n").encode()
        with self.lock:
            if self.failed:
                raise OSError("capture_disabled")
            try:
                write_all(lambda part: os.write(self.fd, part), encoded)
            except Exception:
                self._invalidate_locked("event_sink_write_failed")
                raise

    def _invalidate_locked(self, reason: str) -> None:
        if self.failed:
            return
        self.failed = True
        self.failure_reason = reason
        # The disk may be full/unavailable, so this marker is best effort. A
        # missing normal helper_exit plus wrapper exit 1 is also invalidation.
        marker = (json.dumps({"schema": CAPTURE_SCHEMA, "capture_valid": False,
                              "observation_error": reason}) + "\n").encode()
        fd = None
        try:
            flags = os.O_CREAT | os.O_EXCL | os.O_WRONLY | getattr(os, "O_NOFOLLOW", 0) | getattr(os, "O_BINARY", 0)
            fd = os.open(str(self.invalid_path), flags, 0o600)
            write_all(lambda part: os.write(fd, part), marker)
        except Exception:
            pass
        finally:
            if fd is not None:
                try:
                    os.close(fd)
                except OSError:
                    pass

    def invalidate(self, reason="observation_callback_failed") -> None:
        with self.lock:
            self._invalidate_locked(reason)

    def try_emit(self, event: dict) -> bool:
        try:
            self.emit(event)
            return True
        except Exception:
            self.invalidate()
            return False

    def close(self) -> None:
        with self.lock:
            if self.fd is not None:
                os.close(self.fd)
                self.fd = None


def _safe_text(raw: bytes, secrets: list[bytes], limit=256) -> str | None:
    if not raw or len(raw) > limit or any(secret and secret in raw for secret in secrets):
        return None
    try:
        text = raw.decode("ascii")
    except UnicodeDecodeError:
        return None
    if any(ord(ch) < 32 or ord(ch) > 126 for ch in text):
        return None
    return text


def summarize_head(head: dict) -> tuple[dict, list[bytes]]:
    decoded = []
    for pair in head.get("headers", []):
        if not isinstance(pair, list) or len(pair) != 2 or not all(isinstance(item, str) for item in pair):
            raise ValueError("invalid_header_pair")
        name, value = pair
        decoded.append((name.lower(), base64.b64decode(value, validate=True)))
    secrets = [value for name, value in decoded if name in ("authorization", "chatgpt-account-id", "openai-organization",
                                                          "openai-project", "cookie", "proxy-authorization", "x-api-key", "api-key")]
    for name, value in decoded:
        if name == "authorization" and value.startswith(b"Bearer "):
            secrets.append(value[7:])
    result = {"authorization": [], "workspace_header_sha256": [], "headers": {},
              "all_headers": [], "header_summary_complete": True,
              "header_name_normalization": "ASCII_lowercase_for_comparison_original_name_hash_retained"}
    for (name, value), original_pair in zip(decoded, head.get("headers", [])):
        safe_name = (name if re.fullmatch(r"[!#$%&'*+.^_`|~0-9a-z-]{1,256}", name)
                     and _safe_text(name.encode(), [secret.lower() for secret in secrets]) else "sha256:" + digest(name.encode()))
        result["all_headers"].append({"name": safe_name, "name_sha256": digest(name.encode()),
                                      "original_name_sha256": digest(original_pair[0].encode()),
                                      "value_sha256": digest(value), "value_bytes": len(value)})
        if name == "authorization":
            entry = {"header_value_sha256": digest(value)}
            if value.startswith(b"Bearer "):
                entry["scheme"] = "Bearer"
                entry["token_sha256"] = digest(value[7:])
            result["authorization"].append(entry)
        elif name == "chatgpt-account-id":
            result["workspace_header_sha256"].append(digest(value))
        elif name == "openai-organization":
            result.setdefault("organization_header_sha256", []).append(digest(value))
        elif name in PLAIN_HEADERS or name in HASH_HEADERS:
            entry = {"value_sha256": digest(value)}
            text = _safe_text(value, secrets)
            if name in PLAIN_HEADERS and text is not None:
                entry["value"] = text
            if name == "x-client-request-id" and text is not None and re.fullmatch(r"(?:quality-[A-Za-z0-9_-]{1,190}|[0-9a-fA-F-]{36})", text):
                entry["value"] = text
            result["headers"].setdefault(name, []).append(entry)
        if name == "x-codex-turn-metadata":
            evidence = summarize_nested_session_evidence(value, number_factory=ExactJSONNumber, value_summary=_value_summary)
            if "turn_metadata_summary" in result:
                # Do not choose one among multiple scalar identities.
                result["turn_metadata_summary"] = {"schema": "codex-nested-session-evidence-v1", "present": True,
                    "complete": False, "parse_status": "invalid", "error": "duplicate_turn_metadata_header", "fields": {}}
            else:
                result["turn_metadata_summary"] = evidence
    result["header_names"] = sorted({entry["name"] for entry in result["all_headers"]})
    result["header_value_count"] = len(decoded)
    for key in ("method", "operation", "request_compression"):
        value = head.get(key)
        if isinstance(value, str) and re.fullmatch(r"[A-Za-z0-9_-]{1,40}", value) and _safe_text(value.encode(), secrets):
            result[key] = value
    path = head.get("path")
    if isinstance(path, str):
        result["path_sha256"] = digest(path.encode())
        if path in ("/responses", "/backend-api/codex/responses", "/models", "/backend-api/codex/models"):
            result["endpoint"] = path
    result["go_account_scope"] = {"observed": False, "reason": "not_present_in_native_ipc"}
    return result, secrets


class ExactJSONNumber(str):
    """An original JSON number lexeme, never converted through float/int."""


class BodySummaryError(ValueError):
    pass


def _unique_json_object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise BodySummaryError("duplicate_json_member")
        result[key] = value
    return result


def _nonfinite_json_number(_):
    raise BodySummaryError("nonfinite_json_number")


def _canonical_json_bytes(value):
    # Sorting object keys and normalizing string escapes are report-only.
    # Arrays, null/presence and every numeric lexeme remain distinguishable.
    if isinstance(value, ExactJSONNumber):
        return value.encode("ascii")
    if isinstance(value, dict):
        return b"{" + b",".join(_canonical_json_bytes(key) + b":" + _canonical_json_bytes(value[key])
                                  for key in sorted(value)) + b"}"
    if isinstance(value, list):
        return b"[" + b",".join(_canonical_json_bytes(item) for item in value) + b"]"
    return json.dumps(value, ensure_ascii=False, separators=(",", ":"), allow_nan=False).encode("utf-8")


def _value_summary(value):
    encoded = _canonical_json_bytes(value)
    kind = ("number" if isinstance(value, ExactJSONNumber) else "null" if value is None
            else "boolean" if isinstance(value, bool) else "string" if isinstance(value, str)
            else "array" if isinstance(value, list) else "object")
    return {"content_sha256": digest(encoded), "canonical_json_bytes": len(encoded), "json_type": kind}


def _summary_name(name, known, secrets):
    return name if name in known and _safe_text(name.encode(), secrets) else "sha256:" + digest(name.encode())


def _object_field_summaries(value, known, secrets):
    fields = {}
    for key in sorted(value)[:SUMMARY_FIELD_LIMIT]:
        name = _summary_name(key, known, secrets)
        fields[name] = dict(_value_summary(value[key]), name_sha256=digest(key.encode()))
    return {"fields": fields, "field_names": sorted(fields), "field_count": len(value),
            "fields_captured": len(fields), "complete": len(fields) == len(value)}


def summarize_body(raw: bytes, secrets: list[bytes]) -> dict:
    base = {"body_summary_schema": CAPTURE_SCHEMA, "body_summary_complete": False,
            "body_summary_basis": "canonical_json_sorted_object_keys_normalized_string_escapes_exact_number_lexemes",
            "body_field_byte_lengths": "canonical_JSON_UTF8_bytes_not_original_wire_spans",
            "body_duplicate_member_policy": "reject_any_duplicate_at_any_depth",
            "body_wire_bytes": len(raw), "body_wire_sha256": digest(raw)}
    if len(raw) > MAX_BODY:
        return dict(base, body_json_object=False, body_summary_error="body_limit")
    try:
        body = json.loads(raw.decode("utf-8"), object_pairs_hook=_unique_json_object,
                          parse_int=ExactJSONNumber, parse_float=ExactJSONNumber,
                          parse_constant=_nonfinite_json_number)
        if not isinstance(body, dict):
            return dict(base, body_json_object=False, body_summary_error="not_json_object")
        return _summarize_body_object(body, base, secrets)
    except BodySummaryError as error:
        return dict(base, body_json_object=False, body_summary_error=str(error))
    except UnicodeError:
        return dict(base, body_json_object=False, body_summary_error="invalid_unicode")
    except (ValueError, RecursionError):
        return dict(base, body_json_object=False, body_summary_error="invalid_or_too_deep_json")


def _summarize_body_object(body, base, secrets):
    result = dict(base, body_json_object=True, body_summary_complete=True,
                  body_field_count=len(body), body_fields={}, body_summary_error=None)
    for key in sorted(body)[:SUMMARY_FIELD_LIMIT]:
        name = _summary_name(key, BODY_FIELD_NAMES, secrets)
        result["body_fields"][name] = dict(_value_summary(body[key]), name_sha256=digest(key.encode()))
    result["body_field_names"] = sorted(result["body_fields"])
    result["body_fields_captured"] = len(result["body_fields"])
    if len(body) > SUMMARY_FIELD_LIMIT:
        result.update(body_summary_complete=False, body_summary_error="summary_field_limit")
    result["stable_controls"] = dict(_value_summary({key: body[key] for key in sorted(STABLE_CONTROL_FIELDS) if key in body}),
                                     fields=[key for key in sorted(STABLE_CONTROL_FIELDS) if key in body],
                                     interpretation="control_comparison_only_not_complete_request_equivalence")
    if "input" in body:
        source = body["input"]
        result["input_summary"] = dict(_value_summary(source), item_count=len(source) if isinstance(source, list) else None)
        if isinstance(source, list):
            items, counts = [], {}
            for index, item in enumerate(source[:SUMMARY_ITEM_LIMIT]):
                entry = dict(_value_summary(item), index=index)
                kind = item.get("type") if isinstance(item, dict) else None
                if isinstance(item, dict) and "type" not in item:
                    label = "missing"
                elif type(kind) is str and kind in INPUT_ITEM_TYPES:
                    label = kind
                else:
                    label = "unknown"
                    if isinstance(item, dict) and "type" in item:
                        entry["item_type_sha256"] = _value_summary(kind)["content_sha256"]
                entry["item_type"] = label
                if isinstance(item, dict):
                    entry["field_summary"] = _object_field_summaries(item, INPUT_ITEM_FIELD_NAMES, secrets)
                    if not entry["field_summary"]["complete"]:
                        result.update(body_summary_complete=False, body_summary_error="summary_item_field_limit")
                    metadata = item.get("internal_chat_message_metadata_passthrough")
                    if isinstance(metadata, dict):
                        entry["internal_metadata_summary"] = _object_field_summaries(metadata, ITEM_METADATA_NAMES, secrets)
                        if not entry["internal_metadata_summary"]["complete"]:
                            result.update(body_summary_complete=False, body_summary_error="summary_item_metadata_limit")
                counts[label] = counts.get(label, 0) + 1
                items.append(entry)
            result["input_summary"].update(items=items, item_type_counts=counts, items_captured=len(items),
                                            item_type_count_scope="captured_items_only",
                                            complete=len(items) == len(source))
            if len(items) != len(source):
                result.update(body_summary_complete=False, body_summary_error="summary_item_limit")
    if isinstance(body.get("client_metadata"), dict):
        metadata = body["client_metadata"]
        keys = {}
        for key in sorted(metadata)[:SUMMARY_FIELD_LIMIT]:
            keys[_summary_name(key, CLIENT_METADATA_NAMES, secrets)] = dict(_value_summary(metadata[key]), name_sha256=digest(key.encode()))
        result["client_metadata_summary"] = {"key_count": len(metadata), "keys": keys,
                                              "key_names": sorted(keys), "complete": len(keys) == len(metadata)}
        if len(keys) != len(metadata):
            result.update(body_summary_complete=False, body_summary_error="summary_metadata_limit")
        if "x-codex-turn-metadata" in metadata:
            encoded = metadata["x-codex-turn-metadata"]
            raw_metadata = encoded.encode("utf-8") if type(encoded) is str else False
            result["client_metadata_turn_metadata_summary"] = summarize_nested_session_evidence(
                raw_metadata, number_factory=ExactJSONNumber, value_summary=_value_summary)
    model = body.get("model")
    if type(model) is str and re.fullmatch(r"[A-Za-z0-9_.:/-]{1,128}", model) and _safe_text(model.encode(), secrets):
        result["model"] = model
    reasoning = body.get("reasoning")
    if isinstance(reasoning, dict) and reasoning.get("effort") in ("none", "minimal", "low", "medium", "high", "xhigh"):
        result["reasoning_effort"] = reasoning["effort"]
    tier = body.get("service_tier")
    result["service_tier_present"] = "service_tier" in body
    if tier in ("auto", "default", "flex", "priority", "scale"):
        result["service_tier"] = tier
    for key in ("stream", "store"):
        if isinstance(body.get(key), bool):
            result[key] = body[key]
    return result


class RequestObserver:
    """Inspect a copy of accepted bytes, without changing forwarding decisions."""
    def __init__(self, emit, metadata: dict, on_failure=None):
        self.emit = emit
        self.metadata = dict(metadata, schema=CAPTURE_SCHEMA)
        self.on_failure = on_failure
        self.evidence_valid = False
        self.disabled = False
        self.prefix = bytearray()
        self.head = bytearray()
        self.body = bytearray()
        self.head_length = None
        self.body_length = None
        self.head_summary = {}
        self.secrets = []
        self.forwarded = 0
        self.ipc_hash = hashlib.sha256()
        self.body_hash = hashlib.sha256()
        self.done = False
        self.error = None

    def feed(self, raw: bytes) -> None:
        if self.done:
            return
        try:
            self._feed(raw)
        except Exception:
            self.disable()

    def disable(self) -> None:
        if self.disabled:
            return
        self.disabled = True
        self.done = True
        self.evidence_valid = False
        self.error = "observation_callback_failed"
        self.body.clear()
        self.head.clear()
        self.secrets.clear()
        if self.on_failure is not None:
            try:
                self.on_failure()
            except Exception:
                pass

    def _feed(self, raw: bytes) -> None:
        if self.done:
            return
        self.forwarded += len(raw)
        position = 0
        if len(self.prefix) < 4:
            size = min(4 - len(self.prefix), len(raw))
            self.prefix.extend(raw[:size])
            self.ipc_hash.update(raw[:size])
            position += size
            if len(self.prefix) == 4:
                self.head_length = struct.unpack("!I", self.prefix)[0]
                if not 0 < self.head_length <= MAX_HEAD:
                    self.error = "head_length_invalid"
                    self.finish("observation_rejected")
                    return
        if self.head_length is not None and len(self.head) < self.head_length:
            size = min(self.head_length - len(self.head), len(raw) - position)
            self.head.extend(raw[position:position + size])
            self.ipc_hash.update(raw[position:position + size])
            position += size
            if len(self.head) == self.head_length:
                try:
                    head = json.loads(self.head)
                    if not isinstance(head, dict):
                        raise ValueError("head_not_object")
                    length = head.get("body_length")
                    if isinstance(length, bool) or not isinstance(length, int) or not 0 <= length <= MAX_BODY:
                        raise ValueError("body_length_invalid")
                    self.body_length = length
                    self.head_summary, self.secrets = summarize_head(head)
                except (ValueError, TypeError, UnicodeError, RecursionError):
                    self.error = "head_observation_invalid"
                    self.finish("observation_rejected")
                    return
        if self.body_length is not None:
            size = min(self.body_length - len(self.body), len(raw) - position)
            part = raw[position:position + size]
            self.body.extend(part)
            self.body_hash.update(part)
            self.ipc_hash.update(part)
            if len(self.body) == self.body_length:
                self.finish("request_forwarded")

    def finish(self, reason: str) -> None:
        if self.done:
            return
        try:
            self._finish(reason)
        except Exception:
            self.disable()

    def _finish(self, reason: str) -> None:
        if self.done:
            return
        self.done = True
        complete = self.error is None and self.body_length is not None and len(self.body) == self.body_length
        event = dict(self.metadata, event="native_ipc_request", finish_reason=reason,
                     request_forwarded_complete=complete, observation_error=self.error,
                     body_bytes_forwarded=len(self.body), body_length_declared=self.body_length,
                     actual_ipc_body_sha256=self.body_hash.hexdigest(),
                     actual_ipc_request_sha256=self.ipc_hash.hexdigest(),
                     ipc_head_complete=self.head_length is not None and len(self.head) == self.head_length,
                     ipc_head_forwarded_sha256=digest(self.head), **self.head_summary)
        event["body_digest_scope"] = "complete_pre_rust_compression_body" if complete else "partial_body_only"
        operation = self.head_summary.get("operation", "http")
        event["observation_kind"] = "http_request" if operation == "http" else "handshake_only_no_message_capture"
        if complete and operation == "http":
            event.update(summarize_body(bytes(self.body), self.secrets))
        self.emit(event)
        self.evidence_valid = complete
        # Do not keep a completed payload or credential-bearing head in memory.
        self.body.clear()
        self.head.clear()
        self.secrets.clear()


def _shutdown(stream: socket.socket, direction: int) -> None:
    try:
        stream.shutdown(direction)
    except OSError:
        pass


def _unlink_owned_socket(path: Path, identity) -> None:
    if identity is None:
        return
    try:
        current = path.lstat()
        if current.st_ino == identity.st_ino and current.st_dev == identity.st_dev and stat.S_ISSOCK(current.st_mode):
            path.unlink()
    except FileNotFoundError:
        pass


def relay_pair(client: socket.socket, backend: socket.socket, observer: RequestObserver) -> None:
    """Full duplex; EOF/close cancels the native request, never EOF on body end."""
    def upstream():
        try:
            while True:
                raw = client.recv(65536)
                if not raw:
                    observer.finish("client_eof")
                    break
                write_all(backend.send, raw, observer.feed)
        except OSError:
            observer.finish("request_io_error")
        finally:
            _shutdown(backend, socket.SHUT_WR)

    request_thread = threading.Thread(target=upstream, daemon=True)
    request_thread.start()
    try:
        while True:
            raw = backend.recv(65536)
            if not raw:
                break
            write_all(client.send, raw)
    except OSError:
        pass
    finally:
        _shutdown(client, socket.SHUT_RDWR)
        _shutdown(backend, socket.SHUT_RDWR)
        request_thread.join(timeout=2)
        client.close()
        backend.close()


def _ready_line(stream, timeout=9) -> bytes:
    results = queue.Queue()
    threading.Thread(target=lambda: results.put(stream.readline(MAX_HEAD + 1)), daemon=True).start()
    try:
        line = results.get(timeout=timeout)
    except queue.Empty:
        raise ValueError("helper_readiness_timeout") from None
    if len(line) > MAX_HEAD or not line.endswith(b"\n"):
        raise ValueError("helper_readiness_invalid")
    return line


def relay(helper: Path, expected: str, output: Path) -> int:
    if os.name != "posix" or not hasattr(socket, "AF_UNIX"):
        raise ValueError("relay_requires_posix_unix_socket")
    helper = helper.resolve(strict=True)
    if file_digest(helper) != expected:
        raise ValueError("helper_digest_mismatch")
    output = private_directory(output)
    stdin_read = lambda count: os.read(sys.stdin.fileno(), count)
    startup_length = struct.unpack("!I", read_exact(stdin_read, 4))[0]
    if not 0 < startup_length <= MAX_HEAD:
        raise ValueError("startup_length_invalid")
    original = json.loads(read_exact(stdin_read, startup_length))
    if not isinstance(original, dict) or original.get("protocol") != 1 or "listen" in original:
        raise ValueError("unix_native_startup_required")
    external = Path(original["unix_socket"])
    if not external.is_absolute() or external.name != "s" or external.exists():
        raise ValueError("unexpected_gateway_socket")
    parent_info = external.parent.stat()
    if parent_info.st_uid != os.geteuid() or parent_info.st_mode & 0o077:
        raise ValueError("gateway_socket_directory_not_private")
    internal = external.with_name("r")
    if internal.exists():
        raise ValueError("internal_socket_exists")
    startup = dict(original, unix_socket=str(internal))
    raw_startup = json.dumps(startup, separators=(",", ":")).encode()
    instance = uuid.uuid4().hex
    sink = EventSink(output, instance)
    child = None
    listener = None
    socket_identity = None
    internal_identity = None
    metadata = {"schema": CAPTURE_SCHEMA, "instance": instance, "wrapper_pid": os.getpid()}
    forced_kill = False
    stop = threading.Event()
    clients = set()
    clients_lock = threading.Lock()
    workers = []
    for signum in (signal.SIGTERM, signal.SIGINT):
        signal.signal(signum, lambda *_: stop.set())
    try:
        # Inherit exactly the wrapper environment. Do not add/remove CA or proxy
        # variables, inject headers, alter the HTTP lifecycle, or set RUST_LOG.
        child = subprocess.Popen([str(helper)], stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                 stderr=subprocess.DEVNULL, close_fds=True, bufsize=0)
        write_all(child.stdin.write, struct.pack("!I", len(raw_startup)) + raw_startup)
        ready = json.loads(_ready_line(child.stdout))
        if ready.get("protocol") != 1 or ready.get("transport") != "unix" or ready.get("unix_socket") != str(internal):
            raise ValueError("helper_readiness_mismatch")
        internal_identity = internal.lstat()
        if not stat.S_ISSOCK(internal_identity.st_mode) or internal_identity.st_mode & 0o077:
            raise ValueError("helper_socket_not_private")
        listener = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        listener.bind(str(external))
        os.chmod(external, 0o600)
        socket_identity = external.stat()
        listener.listen(128)
        listener.settimeout(0.2)
        runtime_digest = None
        try:
            runtime_digest = file_digest(Path("/proc") / str(child.pid) / "exe")
        except OSError:
            pass
        metadata = {"schema": CAPTURE_SCHEMA, "instance": instance, "wrapper_pid": os.getpid(),
                    "helper_pid": child.pid, "configured_helper_sha256": expected,
                    "runtime_executable_sha256": runtime_digest,
                    "runtime_executable_matches_helper": runtime_digest == expected,
                    "scope_provenance": "per_wrapper_helper_process_not_go_account_scope"}
        sink.try_emit(dict(metadata, event="native_ipc_helper_start",
                       launcher_tool_sha256=file_digest(Path(__file__)),
                       startup_original_sha256=digest(json.dumps(original, sort_keys=True, separators=(",", ":")).encode()),
                       upstream_configuration_sha256=digest(str(original.get("upstream", "")).encode()),
                       proxy_configuration_sha256=digest(str(original.get("proxy", "")).encode()),
                       http_client_lifecycle=original.get("http_client_lifecycle"),
                       startup_change=["unix_socket"],
                       ca_environment={key: {"present": key in os.environ,
                                             "value_sha256": digest(os.environ[key].encode()) if key in os.environ else None}
                                       for key in CA_KEYS}))
        ready["unix_socket"] = str(external)
        write_all(sys.stdout.buffer.write, (json.dumps(ready, separators=(",", ":")) + "\n").encode())
        sys.stdout.buffer.flush()

        def stdin_lifetime():
            try:
                while True:
                    raw = stdin_read(65536)
                    if not raw:
                        break
                    write_all(child.stdin.write, raw)
            except (OSError, ValueError):
                pass
            finally:
                try:
                    child.stdin.close()
                except (OSError, ValueError):
                    pass
                stop.set()

        def stdout_discard():
            try:
                while child.stdout.read(65536):
                    pass
            except (OSError, ValueError):
                pass

        threading.Thread(target=stdin_lifetime, daemon=True).start()
        threading.Thread(target=stdout_discard, daemon=True).start()

        def handle(client):
            backend = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
            with clients_lock:
                clients.add(backend)
            observer = RequestObserver(sink.emit, dict(metadata, connection=uuid.uuid4().hex), sink.invalidate)
            try:
                backend.connect(str(internal))
                relay_pair(client, backend, observer)
            except OSError:
                observer.finish("backend_connect_error")
                client.close()
                backend.close()
            finally:
                with clients_lock:
                    clients.discard(client)
                    clients.discard(backend)

        while not stop.is_set() and child.poll() is None:
            try:
                client, _ = listener.accept()
            except socket.timeout:
                continue
            with clients_lock:
                clients.add(client)
            worker = threading.Thread(target=handle, args=(client,), daemon=True)
            workers.append(worker)
            worker.start()
    finally:
        stop.set()
        if listener is not None:
            listener.close()
        with clients_lock:
            for client in clients:
                _shutdown(client, socket.SHUT_RDWR)
        for worker in workers:
            worker.join(timeout=2)
        if child is not None:
            try:
                child.stdin.close()
            except (OSError, ValueError):
                pass
            try:
                child.wait(timeout=2)
            except subprocess.TimeoutExpired:
                forced_kill = True
                child.kill()
                child.wait(timeout=2)
            child.stdout.close()
        _unlink_owned_socket(external, socket_identity)
        _unlink_owned_socket(internal, internal_identity)
        sink.try_emit(dict(metadata, event="native_ipc_helper_exit", observation_healthy=True,
                       helper_returncode=child.returncode if child is not None else None,
                       helper_reaped=child is not None and child.poll() is not None,
                       helper_forced_kill=forced_kill,
                       external_socket_absent=not external.exists(),
                       internal_socket_absent=not internal.exists()))
        sink.close()
    return 0 if child.returncode == 0 and not sink.failed else 1


def install_launcher(helper: Path, launcher: Path, output: Path, interpreter: str, expected=None) -> dict:
    helper = helper.resolve(strict=True)
    actual = file_digest(helper)
    if expected is not None and actual != expected:
        raise ValueError("helper_digest_mismatch")
    if not Path(interpreter).is_absolute() or any(ch.isspace() for ch in interpreter):
        raise ValueError("absolute_python_without_spaces_required")
    output = private_directory(output)
    tool = Path(__file__).resolve()
    dependency = tool.with_name("quality_nested_session_evidence.py")
    arguments = [str(tool), "relay", "--helper", str(helper), "--expected-helper-sha256", actual, "--output", str(output)]
    content = ("#!" + interpreter + "\nimport hashlib, runpy, sys\n"
               + "from pathlib import Path\n"
               + "p = " + repr(str(tool)) + "\n"
               + "if hashlib.sha256(Path(p).read_bytes()).hexdigest() != " + repr(file_digest(tool)) + ":\n"
               + "    raise SystemExit('capture_tool_digest_mismatch')\n"
               + "dependency = " + repr(str(dependency)) + "\n"
               + "if hashlib.sha256(Path(dependency).read_bytes()).hexdigest() != " + repr(file_digest(dependency)) + ":\n"
               + "    raise SystemExit('capture_dependency_digest_mismatch')\n"
               + "sys.path.insert(0, str(Path(p).parent))\n"
               + "sys.argv = " + repr(arguments) + "\n"
               + "runpy.run_path(p, run_name='__main__')\n").encode()
    flags = os.O_CREAT | os.O_EXCL | os.O_WRONLY | getattr(os, "O_NOFOLLOW", 0) | getattr(os, "O_BINARY", 0)
    fd = os.open(str(launcher), flags, 0o700)
    try:
        write_all(lambda part: os.write(fd, part), content)
    finally:
        os.close(fd)
    return {"launcher": str(launcher.resolve()), "launcher_sha256": digest(content),
            "helper_sha256": actual, "output": str(output),
            "capture_boundary": "Go_to_real_Rust_helper_IPC_before_helper_compression_and_TLS"}


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    install = commands.add_parser("install")
    install.add_argument("--launcher", type=Path, required=True)
    install.add_argument("--python", default=sys.executable)
    for sub in (install, commands.add_parser("relay")):
        sub.add_argument("--helper", type=Path, required=True)
        sub.add_argument("--expected-helper-sha256", required=sub is not install)
        sub.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    try:
        if args.command == "install":
            print(json.dumps(install_launcher(args.helper, args.launcher, args.output, args.python, args.expected_helper_sha256), sort_keys=True))
            return 0
        return relay(args.helper, args.expected_helper_sha256, args.output)
    except (OSError, ValueError, EOFError, KeyError, TypeError, RecursionError):
        # Never print exception text: OS/JSON failures can carry raw input.
        print("native_ipc_capture_failed", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
