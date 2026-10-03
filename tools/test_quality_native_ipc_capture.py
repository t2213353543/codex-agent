"""Offline tests: synthetic IPC only, optionally a real helper on local HTTP."""
from __future__ import annotations

import base64
import hashlib
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import io
import json
import os
from pathlib import Path
import select
import socket
import stat
import struct
import subprocess
import sys
import tempfile
import threading
import time
import unittest
from unittest import mock

import quality_native_ipc_capture as capture


TOKEN = "synthetic-sensitive-bearer-never-log"
WORKSPACE = "synthetic-sensitive-workspace-never-log"
PROMPT = "synthetic sensitive body text, never persist"


def head_for(body, **overrides):
    headers = [("Authorization", "Bearer " + TOKEN), ("ChatGPT-Account-Id", WORKSPACE),
               ("X-Client-Request-Id", "quality-q4-1234-1"), ("Session-Id", "private-session"),
               ("X-Codex-Routing-Hint", "model=gpt-5.6-sol"), ("Version", "0.155.0"),
               ("X-Ignored-Secret", "not-allowed"), ("Accept", "text/event-stream"),
               ("Accept", "application/json")]
    value = dict(protocol=1, ipc_token="9" * 64, method="POST", path="/responses",
                 headers=[[key, base64.b64encode(value.encode()).decode()] for key, value in headers],
                 body_length=len(body), total_timeout_ms=20000)
    value.update(overrides)
    return value


def request(body=None, **overrides):
    if body is None:
        body = ('{ "model": "gpt-5.6-sol", "reasoning" : { "effort": "medium" },'
                '"stream":true,"store":false,"service_tier":"default","input":"' + PROMPT + '"}').encode()
    # Deliberately preserve whitespace/newlines and nonstandard key order.
    raw_head = json.dumps(head_for(body, **overrides), indent=2).encode()
    return struct.pack("!I", len(raw_head)) + raw_head + body, raw_head, body


def response_frame(kind, body=b""):
    return struct.pack("!BI", kind, len(body)) + body


def receive_frames(stream):
    result = bytearray()
    body = bytearray()
    while True:
        prefix = capture.read_exact(stream.recv, 5)
        kind, length = struct.unpack("!BI", prefix)
        data = capture.read_exact(stream.recv, length)
        result.extend(prefix + data)
        if kind == 2:
            body.extend(data)
        if kind in (3, 4):
            return bytes(result), bytes(body)


class WriteReadTests(unittest.TestCase):
    def test_short_writes_and_fragmented_reads(self):
        accepted, observed = bytearray(), bytearray()
        def writer(raw):
            accepted.extend(raw[:3])
            return min(3, len(raw))
        capture.write_all(writer, b"0123456789", observed.extend)
        self.assertEqual(bytes(accepted), b"0123456789")
        self.assertEqual(accepted, observed)
        source = io.BytesIO(accepted)
        self.assertEqual(capture.read_exact(lambda n: source.read(min(n, 2)), 10), accepted)

    def test_zero_negative_overreported_and_none_write_counts_fail(self):
        for count in (0, -1, 10, None):
            with self.subTest(count=count), self.assertRaises(OSError):
                capture.write_all(lambda _: count, b"abc")

    def test_observer_sees_only_successful_partial_writes(self):
        seen = []
        def writer(raw):
            if seen:
                raise OSError("fixture_destination_failed")
            return 2
        with self.assertRaises(OSError):
            capture.write_all(writer, b"abcdef", seen.append)
        self.assertEqual(seen, [b"ab"])

    def test_read_truncation(self):
        for length in (0, 1, 3):
            with self.subTest(length=length), self.assertRaises(EOFError):
                capture.read_exact(io.BytesIO(b"abcd"[:length]).read, 4)


class ObserverTests(unittest.TestCase):
    def observe(self, raw, chunk=1, finish="fixture_eof"):
        events = []
        observer = capture.RequestObserver(events.append, {"instance": "fixture"})
        for offset in range(0, len(raw), chunk):
            observer.feed(raw[offset:offset + chunk])
        observer.finish(finish)
        self.assertEqual(len(events), 1)
        return events[0], observer

    def test_actual_original_bytes_and_selected_fields(self):
        raw, raw_head, body = request()
        event, observer = self.observe(raw)
        self.assertTrue(event["request_forwarded_complete"])
        self.assertEqual(event["actual_ipc_body_sha256"], capture.digest(body))
        self.assertEqual(event["actual_ipc_request_sha256"], capture.digest(raw))
        self.assertEqual(event["ipc_head_forwarded_sha256"], capture.digest(raw_head))
        self.assertEqual(event["model"], "gpt-5.6-sol")
        self.assertEqual(event["reasoning_effort"], "medium")
        self.assertEqual(event["service_tier"], "default")
        self.assertEqual(event["authorization"][0]["token_sha256"], capture.digest(TOKEN.encode()))
        self.assertEqual(event["workspace_header_sha256"], [capture.digest(WORKSPACE.encode())])
        self.assertEqual(event["headers"]["x-client-request-id"][0]["value"], "quality-q4-1234-1")
        self.assertEqual(len(event["headers"]["accept"]), 2)
        self.assertFalse(event["go_account_scope"]["observed"])
        self.assertNotIn("value", event["headers"]["session-id"][0])
        self.assertEqual(observer.body, b"")
        self.assertEqual(observer.head, b"")
        persisted = json.dumps(event)
        for secret in (TOKEN, WORKSPACE, PROMPT, "private-session", "not-allowed", "9" * 64):
            self.assertNotIn(secret, persisted)

    def test_arbitrary_fragmentation_same_digest(self):
        raw, _, body = request()
        for chunk in (2, 3, 4, 7, 64, 65536):
            with self.subTest(chunk=chunk):
                event, _ = self.observe(raw, chunk)
                self.assertEqual(event["actual_ipc_request_sha256"], capture.digest(raw))
                self.assertEqual(event["actual_ipc_body_sha256"], capture.digest(body))

    def test_truncated_prefix_head_and_body_are_never_complete(self):
        raw, head, body = request()
        for end in (0, 1, 3, 4, 7, 4 + len(head) - 1, 4 + len(head), len(raw) - 3):
            with self.subTest(end=end):
                event, _ = self.observe(raw[:end], 11)
                self.assertFalse(event["request_forwarded_complete"])
                expected_body = body[:max(0, end - 4 - len(head))]
                self.assertEqual(event["body_bytes_forwarded"], len(expected_body))
                self.assertEqual(event["actual_ipc_body_sha256"], capture.digest(expected_body))
                self.assertEqual(event["actual_ipc_request_sha256"], capture.digest(raw[:end]))
                self.assertEqual(event["body_digest_scope"], "partial_body_only")

    def test_invalid_length_is_bounded(self):
        for size in (0, capture.MAX_HEAD + 1, 0xffffffff):
            with self.subTest(size=size):
                event, observer = self.observe(struct.pack("!I", size) + b"sensitive" * 10000, 1000)
                self.assertFalse(event["request_forwarded_complete"])
                self.assertEqual(event["observation_error"], "head_length_invalid")
                self.assertEqual(len(observer.head), 0)

    def test_invalid_head_body_length_and_non_json(self):
        for head in (b"{", b"[]", json.dumps({"body_length": True}).encode(),
                     json.dumps({"body_length": capture.MAX_BODY + 1}).encode(),
                     json.dumps({"body_length": 0, "headers": [["Authorization", "not:base64"]]}).encode()):
            event, _ = self.observe(struct.pack("!I", len(head)) + head, 5)
            self.assertFalse(event["request_forwarded_complete"])
            self.assertEqual(event["observation_error"], "head_observation_invalid")

    def test_empty_get_and_binary_body(self):
        for body in (b"", b"\xff\x00\x10"):
            event, _ = self.observe(request(body, method="GET")[0])
            self.assertTrue(event["request_forwarded_complete"])
            self.assertFalse(event["body_json_object"])
            self.assertEqual(event["actual_ipc_body_sha256"], capture.digest(body))

    def test_websocket_is_marked_handshake_only_and_payload_never_recorded(self):
        raw, _, _ = request(b"", operation="websocket_message_v1", method="GET")
        event, _ = self.observe(raw + b"sensitive websocket frame", 100000)
        self.assertEqual(event["observation_kind"], "handshake_only_no_message_capture")
        self.assertNotIn("body_json_object", event)
        self.assertEqual(event["actual_ipc_request_sha256"], capture.digest(raw))
        self.assertNotIn("sensitive websocket", json.dumps(event))

    def test_known_credentials_copied_to_allowlisted_fields_stay_redacted(self):
        body = json.dumps({"model": TOKEN}).encode()
        head = head_for(body)
        head["headers"].append(["User-Agent", base64.b64encode(("prefix " + TOKEN).encode()).decode()])
        raw_head = json.dumps(head).encode()
        event, _ = self.observe(struct.pack("!I", len(raw_head)) + raw_head + body)
        self.assertNotIn(TOKEN, json.dumps(event))
        self.assertNotIn("value", event["headers"]["user-agent"][0])
        self.assertNotIn("model", event)

    def test_emit_failure_disables_capture_without_escaping_callback(self):
        calls, invalidations = [], []
        def failing_emit(event):
            calls.append(event["event"])
            raise OSError("synthetic disk failure containing " + TOKEN)
        observer = capture.RequestObserver(failing_emit, {}, lambda: invalidations.append(True))
        observer.feed(request()[0])
        observer.feed(b"further bytes")
        observer.finish("client_eof")
        self.assertEqual(calls, ["native_ipc_request"])
        self.assertEqual(invalidations, [True])
        self.assertTrue(observer.disabled)
        self.assertFalse(observer.evidence_valid)
        self.assertEqual(observer.error, "observation_callback_failed")
        self.assertEqual(observer.body, b"")
        self.assertEqual(observer.head, b"")
        self.assertEqual(observer.secrets, [])


class ShortSocket:
    def __init__(self, stream, limit):
        self.stream = stream
        self.limit = limit

    def send(self, raw):
        return self.stream.send(raw[:self.limit])

    def recv(self, count):
        return self.stream.recv(min(count, self.limit))

    def __getattr__(self, name):
        return getattr(self.stream, name)


class FieldSummaryTests(unittest.TestCase):
    def summary(self, raw):
        if isinstance(raw, str):
            raw = raw.encode()
        return capture.summarize_body(raw, [TOKEN.encode(), WORKSPACE.encode()])

    def test_every_field_is_hashed_without_prompt_or_identity_values(self):
        raw = json.dumps({"model": "gpt-5.6-sol", "instructions": PROMPT, "tools": [{"name": PROMPT}],
                          "input": [{"type": "message", "role": "user", "content": [{"type": "input_text", "text": PROMPT}]},
                                    {"type": "reasoning", "encrypted_content": TOKEN},
                                    {"type": "private-kind-never-log", "output": WORKSPACE}],
                          "reasoning": {"effort": "medium"}, "include": ["reasoning.encrypted_content"],
                          "text": {"verbosity": "low"}, "client_metadata": {"session_id": WORKSPACE,
                              "thread_id": TOKEN, "private-key-never-log": PROMPT},
                          "private-field-never-log": TOKEN}, ensure_ascii=False).encode()
        result = self.summary(raw)
        self.assertTrue(result["body_summary_complete"])
        self.assertEqual(result["body_summary_schema"], 4)
        self.assertEqual(result["body_wire_sha256"], capture.digest(raw))
        self.assertEqual(result["body_field_count"], 9)
        self.assertEqual(result["body_fields_captured"], 9)
        self.assertEqual(result["body_fields"]["instructions"], {
            "content_sha256": capture.digest(json.dumps(PROMPT).encode()), "canonical_json_bytes": len(json.dumps(PROMPT).encode()),
            "json_type": "string", "name_sha256": capture.digest(b"instructions")})
        self.assertEqual(result["input_summary"]["item_type_counts"], {"message": 1, "reasoning": 1, "unknown": 1})
        self.assertEqual(result["input_summary"]["items_captured"], 3)
        self.assertEqual(result["client_metadata_summary"]["key_count"], 3)
        self.assertIn("session_id", result["client_metadata_summary"]["key_names"])
        self.assertIn("sha256:" + capture.digest(b"private-key-never-log"), result["client_metadata_summary"]["key_names"])
        persisted = json.dumps(result)
        for sensitive in (PROMPT, TOKEN, WORKSPACE, "private-kind-never-log", "private-key-never-log", "private-field-never-log"):
            self.assertNotIn(sensitive, persisted)

    def test_numbers_preserve_large_integer_negative_zero_and_exponent_lexemes(self):
        lexemes = ("9007199254740993", "9007199254740992", "-0", "0", "1e1000", "10e999", "1.2300e+009", "1230000000")
        observed = []
        for number in lexemes:
            with self.subTest(number=number):
                result = self.summary('{"reasoning":{"budget":' + number + '}}')
                self.assertTrue(result["body_summary_complete"])
                expected = ('{"budget":' + number + '}').encode()
                field = result["body_fields"]["reasoning"]
                self.assertEqual((field["content_sha256"], field["canonical_json_bytes"]), (capture.digest(expected), len(expected)))
                observed.append(field["content_sha256"])
        self.assertEqual(len(set(observed)), len(lexemes))
        self.assertNotEqual(self.summary('{"input":1}')["body_fields"]["input"]["content_sha256"],
                            self.summary('{"input":"1"}')["body_fields"]["input"]["content_sha256"])

    def test_report_only_normalization_keeps_full_raw_digest(self):
        a = self.summary(b'{ "instructions":"<x>&\\u4f60", "reasoning":{"summary":"auto","effort":"medium"} }')
        b = self.summary('{"reasoning":{"effort":"medium","summary":"auto"},"instructions":"\\u003cx\\u003e\\u0026你"}')
        self.assertNotEqual(a["body_wire_sha256"], b["body_wire_sha256"])
        self.assertEqual(a["body_fields"], b["body_fields"])
        self.assertEqual(a["stable_controls"], b["stable_controls"])

    def test_missing_null_empty_and_sequence_changes_are_distinct(self):
        absent, null, empty = (self.summary(raw) for raw in ('{}', '{"include":null}', '{"include":[]}'))
        self.assertNotIn("include", absent["body_fields"])
        self.assertEqual(null["body_fields"]["include"]["json_type"], "null")
        self.assertNotEqual(null["body_fields"]["include"]["content_sha256"], empty["body_fields"]["include"]["content_sha256"])
        a = self.summary('{"input":[{"type":"message","content":"one"},{"type":"message","content":"two"}]}')
        b = self.summary('{"input":[{"type":"message","content":"two"},{"type":"message","content":"one"}]}')
        self.assertNotEqual(a["input_summary"]["content_sha256"], b["input_summary"]["content_sha256"])
        self.assertEqual(a["input_summary"]["items"][0]["content_sha256"], b["input_summary"]["items"][1]["content_sha256"])

    def test_duplicate_members_reject_summary_at_any_depth_without_false_completeness(self):
        for raw in ('{"model":"a","model":"b"}', '{"tools":[{"name":"a","name":"b"}]}',
                    '{"client_metadata":{"thread_id":"a","thread_id":"b"}}',
                    '{"reasoning":{"effort":"high","\\u0065ffort":"low"}}'):
            with self.subTest(raw=raw):
                result = self.summary(raw)
                self.assertFalse(result["body_summary_complete"])
                self.assertEqual(result["body_summary_error"], "duplicate_json_member")
                self.assertNotIn("body_fields", result)
                self.assertEqual(result["body_wire_sha256"], capture.digest(raw.encode()))

    def test_nonfinite_invalid_utf8_and_lone_surrogate_do_not_produce_partial_hashes(self):
        for raw in (b'{"input":NaN}', b'{"input":Infinity}', b'{"input":-Infinity}',
                    b'{"input":"\xff"}', b'{"input":"\\ud800"}', b'[]', b'{', b'{"input":1} {}'):
            with self.subTest(raw=raw):
                result = self.summary(raw)
                self.assertFalse(result["body_summary_complete"])
                self.assertNotIn("body_fields", result)
                self.assertIn("body_summary_error", result)

    def test_stable_control_digest_does_not_claim_session_or_prompt_equivalence(self):
        a = self.summary('{"model":"gpt-5.6-sol","instructions":"same","input":"first","prompt_cache_key":"a","client_metadata":{"turn_id":"one"}}')
        b = self.summary('{"model":"gpt-5.6-sol","instructions":"same","input":"second","prompt_cache_key":"b","client_metadata":{"turn_id":"two"}}')
        self.assertEqual(a["stable_controls"], b["stable_controls"])
        self.assertNotEqual(a["body_fields"]["input"], b["body_fields"]["input"])
        self.assertNotEqual(a["body_fields"]["client_metadata"], b["body_fields"]["client_metadata"])
        self.assertEqual(a["stable_controls"]["fields"], ["instructions", "model"])

    def test_summary_limits_are_explicit_and_complete_field_hash_survives_item_limit(self):
        with mock.patch.object(capture, "SUMMARY_ITEM_LIMIT", 1):
            result = self.summary('{"input":[{"type":"message"},{"type":"reasoning"}]}')
        self.assertFalse(result["body_summary_complete"])
        self.assertEqual(result["body_summary_error"], "summary_item_limit")
        self.assertEqual(result["input_summary"]["item_count"], 2)
        self.assertEqual(result["input_summary"]["items_captured"], 1)
        self.assertEqual(result["body_fields"]["input"]["content_sha256"], capture.digest(b'[{"type":"message"},{"type":"reasoning"}]'))
        with mock.patch.object(capture, "SUMMARY_FIELD_LIMIT", 1):
            result = self.summary('{"model":"x","instructions":"y"}')
        self.assertFalse(result["body_summary_complete"])
        self.assertEqual(result["body_fields_captured"], 1)
        self.assertEqual(result["body_field_count"], 2)

    def test_all_headers_have_hashes_lengths_and_value_order_without_private_values(self):
        head = head_for(b'{}')
        for name, value in (("Thread-Id", TOKEN), ("X-Codex-Turn-State", WORKSPACE), ("X-Future-Routing", PROMPT),
                            ("Cookie", TOKEN), ("X-Future-Routing", "second-private-value")):
            head["headers"].append([name, base64.b64encode(value.encode()).decode()])
        result, _ = capture.summarize_head(head)
        self.assertTrue(result["header_summary_complete"])
        self.assertEqual(result["header_value_count"], len(head["headers"]))
        self.assertEqual(len(result["all_headers"]), len(head["headers"]))
        future = [item for item in result["all_headers"] if item["name"] == "x-future-routing"]
        self.assertEqual([item["value_sha256"] for item in future], [capture.digest(PROMPT.encode()), capture.digest(b"second-private-value")])
        self.assertEqual(future[0]["value_bytes"], len(PROMPT.encode()))
        for name in ("authorization", "chatgpt-account-id", "cookie", "thread-id", "x-codex-turn-state", "x-future-routing"):
            self.assertIn(name, result["header_names"])
        for sensitive in (TOKEN, WORKSPACE, PROMPT, "second-private-value"):
            self.assertNotIn(sensitive, json.dumps(result))

    def test_lite_item_content_and_identity_are_independently_comparable(self):
        def lite(item_id, turn_id, create_time, text=PROMPT, tool_limit=9007199254740993):
            return self.summary(json.dumps({"instructions": "", "input": [
                {"type": "additional_tools", "id": "at_" + item_id, "role": "developer",
                 "tools": [{"type": "function", "name": "inspect", "parameters": {"limit": tool_limit}}]},
                {"type": "message", "id": "msg_" + item_id, "role": "developer",
                 "content": [{"type": "input_text", "text": text}],
                 "internal_chat_message_metadata_passthrough": {
                     "turn_id": turn_id, "create_time": create_time, "content_item_kinds": ["developer_instructions"]}}]}))
        left = lite("left-private-id", "left-private-turn", 123.25)
        right = lite("right-private-id", "right-private-turn", 456.5)
        for a, b in zip(left["input_summary"]["items"], right["input_summary"]["items"]):
            self.assertNotEqual(a["content_sha256"], b["content_sha256"])
            af, bf = a["field_summary"], b["field_summary"]
            self.assertTrue(af["complete"] and bf["complete"])
            self.assertEqual(af["field_names"], bf["field_names"])
            self.assertNotEqual(af["fields"]["id"], bf["fields"]["id"])
            for field in ("type", "role", "tools" if a["item_type"] == "additional_tools" else "content"):
                self.assertEqual(af["fields"][field], bf["fields"][field])
        am = left["input_summary"]["items"][1]["internal_metadata_summary"]["fields"]
        bm = right["input_summary"]["items"][1]["internal_metadata_summary"]["fields"]
        self.assertNotEqual(am["turn_id"], bm["turn_id"])
        self.assertNotEqual(am["create_time"], bm["create_time"])
        self.assertEqual(am["content_item_kinds"], bm["content_item_kinds"])
        changed = lite("right-private-id", "right-private-turn", 456.5, "different-private-content", 9007199254740992)
        self.assertNotEqual(right["input_summary"]["items"][0]["field_summary"]["fields"]["tools"],
                            changed["input_summary"]["items"][0]["field_summary"]["fields"]["tools"])
        self.assertNotEqual(right["input_summary"]["items"][1]["field_summary"]["fields"]["content"],
                            changed["input_summary"]["items"][1]["field_summary"]["fields"]["content"])
        serialized = json.dumps([left, right, changed])
        for secret in (PROMPT, "left-private-id", "right-private-id", "left-private-turn", "right-private-turn", "different-private-content", "developer_instructions"):
            self.assertNotIn(secret, serialized)

    def test_item_fields_preserve_missing_null_and_unknown_metadata_without_leakage(self):
        result = self.summary(json.dumps({"input": [
            {"type": "message", "role": "user", "content": [], "private-field-key": PROMPT,
             "internal_chat_message_metadata_passthrough": {"turn_id": TOKEN, "private-meta-key": WORKSPACE}},
            {"type": "message", "id": None, "role": "user", "content": []},
            {"type": "message", "id": "", "role": "user", "content": []}]}))
        items = result["input_summary"]["items"]
        self.assertNotIn("id", items[0]["field_summary"]["fields"])
        self.assertEqual(items[1]["field_summary"]["fields"]["id"]["json_type"], "null")
        self.assertNotEqual(items[1]["field_summary"]["fields"]["id"], items[2]["field_summary"]["fields"]["id"])
        self.assertIn("sha256:" + capture.digest(b"private-field-key"), items[0]["field_summary"]["field_names"])
        self.assertIn("sha256:" + capture.digest(b"private-meta-key"), items[0]["internal_metadata_summary"]["field_names"])
        serialized = json.dumps(result)
        for secret in (TOKEN, WORKSPACE, PROMPT, "private-field-key", "private-meta-key"):
            self.assertNotIn(secret, serialized)

    def test_item_field_limits_mark_incomplete_without_dropping_full_item_hash(self):
        source = '{"input":[{"type":"message","id":"private-id","content":[]}]}'
        complete = self.summary(source)
        with mock.patch.object(capture, "SUMMARY_FIELD_LIMIT", 2):
            limited = self.summary(source)
        self.assertFalse(limited["body_summary_complete"])
        self.assertEqual(limited["body_summary_error"], "summary_item_field_limit")
        item = limited["input_summary"]["items"][0]
        self.assertFalse(item["field_summary"]["complete"])
        self.assertEqual(item["field_summary"]["field_count"], 3)
        self.assertEqual(item["field_summary"]["fields_captured"], 2)
        self.assertEqual(item["content_sha256"], complete["input_summary"]["items"][0]["content_sha256"])


class RelayTests(unittest.TestCase):
    def test_schema4_duplicate_rejection_does_not_change_relay_or_full_body_digest(self):
        body = b'{"input":"private-first","input":"private-second"}'
        raw, _, _ = request(body)
        event = self.exchange(raw, b"original-native-response", short=True)
        self.assertEqual(event["schema"], 4)
        self.assertTrue(event["request_forwarded_complete"])
        self.assertEqual(event["actual_ipc_body_sha256"], capture.digest(body))
        self.assertFalse(event["body_summary_complete"])
        self.assertEqual(event["body_summary_error"], "duplicate_json_member")
        self.assertNotIn("private-first", json.dumps(event))
        self.assertNotIn("private-second", json.dumps(event))

    def exchange(self, raw, response, short=False, observer_factory=None, expected_events=1,
                 check_no_early_eof=False):
        client, relay_client = socket.socketpair()
        relay_backend, server = socket.socketpair()
        for stream in (client, server):
            stream.settimeout(10)
        events, received, server_errors = [], [], []
        observer = (observer_factory(events.append) if observer_factory is not None
                    else capture.RequestObserver(events.append, {"connection": "fixture"}))
        worker = threading.Thread(target=capture.relay_pair,
                                  args=(ShortSocket(relay_client, 37) if short else relay_client,
                                        ShortSocket(relay_backend, 29) if short else relay_backend, observer))
        def serve():
            try:
                received.append(capture.read_exact(server.recv, len(raw)))
                if check_no_early_eof:
                    readable, _, _ = select.select([server], [], [], 0.05)
                    self.assertEqual(readable, [], "observer failure prematurely closed native request")
                # No client half-close is needed to receive the complete response.
                server.sendall(response)
            except BaseException as error:
                server_errors.append(error)
            finally:
                server.close()
        server_worker = threading.Thread(target=serve)
        worker.start()
        server_worker.start()
        try:
            client.sendall(raw)
            self.assertEqual(capture.read_exact(client.recv, len(response)), response)
            self.assertEqual(client.recv(1), b"")
        finally:
            client.close()
            worker.join(timeout=12)
            server_worker.join(timeout=12)
        self.assertFalse(worker.is_alive())
        self.assertFalse(server_worker.is_alive())
        self.assertEqual(server_errors, [])
        self.assertEqual(received, [raw])
        self.assertEqual(len(events), expected_events)
        return events[0] if expected_events else observer

    def test_large_bidirectional_fragmentation_preserves_every_byte(self):
        raw, _, body = request(b'{"input":"' + b"A" * 180000 + b'"}')
        response = bytes(range(256)) * 900
        event = self.exchange(raw, response, short=True)
        self.assertEqual(event["actual_ipc_request_sha256"], capture.digest(raw))
        self.assertEqual(event["actual_ipc_body_sha256"], capture.digest(body))

    def test_concurrent_connections_do_not_mix_bytes_or_credentials(self):
        failures = []
        def execute(index):
            try:
                raw, _, body = request(json.dumps({"model": "gpt-5.6-sol", "input": str(index) * 4096}).encode())
                event = self.exchange(raw, ("response-%d" % index).encode() * 5000, short=True)
                self.assertEqual(event["actual_ipc_body_sha256"], capture.digest(body))
            except BaseException as error:
                failures.append(error)
        workers = [threading.Thread(target=execute, args=(index,)) for index in range(12)]
        for worker in workers:
            worker.start()
        for worker in workers:
            worker.join(timeout=30)
        self.assertTrue(all(not worker.is_alive() for worker in workers))
        self.assertEqual(failures, [])

    def test_client_close_propagates_cancellation_and_records_truncation(self):
        raw, head, _ = request()
        client, relay_client = socket.socketpair()
        relay_backend, server = socket.socketpair()
        server.settimeout(3)
        events = []
        worker = threading.Thread(target=capture.relay_pair,
                                  args=(relay_client, relay_backend, capture.RequestObserver(events.append, {})))
        worker.start()
        sent = raw[:len(head) + 5]
        client.sendall(sent)
        client.close()
        self.assertEqual(capture.read_exact(server.recv, len(sent)), sent)
        self.assertEqual(server.recv(1), b"")
        server.close()
        worker.join(timeout=4)
        self.assertFalse(worker.is_alive())
        self.assertEqual(len(events), 1)
        self.assertFalse(events[0]["request_forwarded_complete"])
        self.assertEqual(events[0]["body_bytes_forwarded"], 1)

    def test_invalid_observation_does_not_rewrite_or_suppress_transport_bytes(self):
        invalid = struct.pack("!I", 3) + b"bad" + bytes(range(256))
        event = self.exchange(invalid, b"native-original-error", short=True)
        self.assertFalse(event["request_forwarded_complete"])
        self.assertEqual(event["observation_error"], "head_observation_invalid")

    def test_disk_error_on_complete_request_keeps_both_directions_and_no_early_eof(self):
        failures = []
        def observer_factory(_):
            def emit(_event):
                raise OSError("synthetic disk full")
            return capture.RequestObserver(emit, {}, lambda: failures.append(True))
        raw, _, _ = request(b'{"input":"' + b"B" * 120000 + b'"}')
        observer = self.exchange(raw, bytes(range(256)) * 700, short=True,
                                 observer_factory=observer_factory, expected_events=0,
                                 check_no_early_eof=True)
        self.assertEqual(failures, [True])
        self.assertTrue(observer.disabled)
        self.assertFalse(observer.evidence_valid)

    def test_observer_error_mid_request_cannot_truncate_remaining_transport(self):
        failures = []
        class FailingObserver(capture.RequestObserver):
            def _feed(self, raw):
                raise ValueError("synthetic observation parser failure")
        raw, _, _ = request(b"synthetic body" * 10000)
        observer = self.exchange(raw, b"unchanged response" * 6000, short=True,
                                 observer_factory=lambda emit: FailingObserver(emit, {}, lambda: failures.append(True)),
                                 expected_events=0, check_no_early_eof=True)
        self.assertTrue(observer.disabled)
        self.assertFalse(observer.evidence_valid)
        self.assertEqual(failures, [True])


class ArtifactTests(unittest.TestCase):
    def test_concurrent_records_are_complete_lines_and_private(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = capture.private_directory(Path(temporary) / "out")
            sink = capture.EventSink(directory, "fixture")
            workers = [threading.Thread(target=lambda n=n: [sink.emit({"worker": n, "n": i}) for i in range(40)]) for n in range(10)]
            for worker in workers:
                worker.start()
            for worker in workers:
                worker.join(timeout=5)
            sink.close()
            path = directory / "fixture.jsonl"
            rows = [json.loads(line) for line in path.read_text().splitlines()]
            self.assertEqual(len(rows), 400)
            self.assertEqual(len({(row["worker"], row["n"]) for row in rows}), 400)
            if os.name == "posix":
                self.assertEqual(stat.S_IMODE(path.stat().st_mode), 0o600)

    def test_install_embeds_paths_without_shell_and_pins_tool_and_helper(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            helper = directory / "helper with space"
            helper.write_bytes(b"fixture helper bytes")
            launcher = directory / "launcher"
            interpreter = sys.executable if os.name == "posix" else str(directory / "python")
            result = capture.install_launcher(helper, launcher, directory / "out", interpreter)
            content = launcher.read_text()
            compile(content, str(launcher), "exec")
            self.assertEqual(result["helper_sha256"], capture.digest(helper.read_bytes()))
            self.assertEqual(result["launcher_sha256"], capture.digest(launcher.read_bytes()))
            self.assertIn("capture_tool_digest_mismatch", content)
            self.assertIn("runpy.run_path", content)
            with self.assertRaises(FileExistsError):
                capture.install_launcher(helper, launcher, directory / "out", interpreter)
            with self.assertRaisesRegex(ValueError, "helper_digest_mismatch"):
                capture.install_launcher(helper, directory / "other", directory / "out", interpreter, "0" * 64)

    def test_partial_disk_write_permanently_invalidates_instance(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = capture.private_directory(Path(temporary) / "out")
            sink = capture.EventSink(directory, "fixture")
            real_write = os.write
            writes = []
            def failing_write(fd, raw):
                if fd != sink.fd:
                    return real_write(fd, raw)
                writes.append(len(raw))
                if len(writes) == 1:
                    return real_write(fd, raw[:7])
                raise OSError("synthetic disk failure containing " + TOKEN)
            with mock.patch.object(capture.os, "write", side_effect=failing_write):
                self.assertFalse(sink.try_emit({"event": "fixture"}))
                self.assertFalse(sink.try_emit({"event": "must_not_resume"}))
            self.assertEqual(len(writes), 2)
            self.assertTrue(sink.failed)
            self.assertEqual(sink.failure_reason, "event_sink_write_failed")
            marker = json.loads((directory / "fixture.invalid").read_text())
            self.assertFalse(marker["capture_valid"])
            self.assertNotIn(TOKEN, json.dumps(marker))
            self.assertEqual((directory / "fixture.jsonl").read_bytes(), b'{"event')
            sink.close()

    @unittest.skipUnless(os.name == "posix", "POSIX permission and symlink checks")
    def test_public_or_symlink_output_directory_rejected(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            public = directory / "public"
            public.mkdir(mode=0o755)
            with self.assertRaisesRegex(ValueError, "not_private"):
                capture.private_directory(public)
            private = directory / "private"
            private.mkdir(mode=0o700)
            link = directory / "link"
            link.symlink_to(private, target_is_directory=True)
            with self.assertRaisesRegex(ValueError, "not_directory"):
                capture.private_directory(link)


def fake_helper_main():
    """Synthetic native protocol peer. No network connections or real tokens."""
    reader = lambda n: os.read(sys.stdin.fileno(), n)
    length = struct.unpack("!I", capture.read_exact(reader, 4))[0]
    config = json.loads(capture.read_exact(reader, length))
    path = Path(config["unix_socket"])
    server = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    server.bind(str(path))
    os.chmod(path, 0o600)
    server.listen(128)
    server.settimeout(0.1)
    stop = threading.Event()
    def stdin_watch():
        while os.read(sys.stdin.fileno(), 65536):
            pass
        stop.set()
    threading.Thread(target=stdin_watch, daemon=True).start()
    ready = dict(protocol=1, transport="unix", unix_socket=str(path), max_body=capture.MAX_BODY,
                 max_active=0, http_client_lifecycle=config["http_client_lifecycle"],
                 capabilities=["request_zstd_v1", "websocket_message_v1"])
    sys.stdout.write(json.dumps(ready) + "\n")
    sys.stdout.flush()
    workers = []
    def serve(client):
        try:
            prefix = capture.read_exact(client.recv, 4)
            raw_head = capture.read_exact(client.recv, struct.unpack("!I", prefix)[0])
            head = json.loads(raw_head)
            body = capture.read_exact(client.recv, head["body_length"])
            startup_without_socket = dict(config)
            startup_without_socket.pop("unix_socket")
            payload = json.dumps({"request_wire_sha256": capture.digest(prefix + raw_head + body),
                                  "startup_without_socket": startup_without_socket,
                                  "ca_environment_sha256": {key: capture.digest(os.environ[key].encode()) if key in os.environ else None for key in capture.CA_KEYS}}).encode()
            response = (response_frame(1, b'{"status":200,"version":"HTTP/1.1","headers":[]}')
                        + response_frame(2, payload) + response_frame(3))
            capture.write_all(lambda raw: client.send(raw[:7]), response)
        finally:
            client.close()
    try:
        while not stop.is_set():
            try:
                client, _ = server.accept()
            except socket.timeout:
                continue
            worker = threading.Thread(target=serve, args=(client,))
            workers.append(worker)
            worker.start()
    finally:
        server.close()
        for worker in workers:
            worker.join(timeout=3)
        path.unlink()
    return 0


@unittest.skipUnless(os.name == "posix" and hasattr(socket, "AF_UNIX"), "Linux/POSIX launcher integration")
class LauncherIntegrationTests(unittest.TestCase):
    def make_fake(self, directory):
        helper = directory / "fake-native"
        helper.write_text("#!" + sys.executable + "\nimport runpy, sys\n"
                          + "sys.path.insert(0, " + repr(str(Path(__file__).resolve().parent)) + ")\n"
                          + "namespace = runpy.run_path(" + repr(str(Path(__file__).resolve())) + ")\n"
                          + "raise SystemExit(namespace['fake_helper_main']())\n")
        helper.chmod(0o700)
        return helper

    def start(self, directory, helper, environment=None, upstream="https://fixture.invalid",
              injected_capture_failure=False):
        socket_directory = directory / "socket"
        socket_directory.mkdir(mode=0o700)
        output = directory / "capture"
        launcher = directory / "launcher"
        capture.install_launcher(helper, launcher, output, sys.executable)
        if injected_capture_failure:
            # Fault injection exists only in this synthetic test launcher, not
            # in the capture tool or any environment-controlled production path.
            launcher.write_text("#!" + sys.executable + "\nimport sys\nfrom pathlib import Path\n"
                                + "sys.path.insert(0, " + repr(str(Path(__file__).resolve().parent)) + ")\n"
                                + "import quality_native_ipc_capture as c\noriginal = c.EventSink.emit\n"
                                + "def failing_emit(self, event):\n"
                                + "    if event.get('event') == 'native_ipc_request':\n"
                                + "        raise OSError('synthetic_capture_disk_failure')\n"
                                + "    return original(self, event)\n"
                                + "c.EventSink.emit = failing_emit\n"
                                + "raise SystemExit(c.relay(Path(" + repr(str(helper)) + "), "
                                + repr(capture.file_digest(helper)) + ", Path(" + repr(str(output)) + ")))\n")
        process = subprocess.Popen([str(launcher)], stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                   stderr=subprocess.PIPE, env=environment)
        config = dict(protocol=1, upstream=upstream, ipc_token="8" * 64, proxy=None,
                      headers_timeout_ms=3000, idle_timeout_ms=0, http_client_lifecycle="cli_request",
                      unix_socket=str(socket_directory / "s"))
        raw = json.dumps(config).encode()
        process.stdin.write(struct.pack("!I", len(raw)) + raw)
        process.stdin.flush()
        ready = json.loads(capture._ready_line(process.stdout))
        self.assertEqual(ready["unix_socket"], config["unix_socket"])
        self.assertEqual(stat.S_IMODE(Path(ready["unix_socket"]).stat().st_mode), 0o600)
        return process, config, output

    def stop(self, process, expected_returncode=0):
        process.stdin.close()
        process.wait(timeout=6)
        stderr = process.stderr.read()
        process.stdout.close()
        process.stderr.close()
        self.assertEqual(process.returncode, expected_returncode, stderr.decode(errors="replace"))
        self.assertEqual(stderr, b"")

    def test_startup_probe_eof_reaps_child_and_both_socket_names(self):
        with tempfile.TemporaryDirectory(prefix="qipc-") as temporary:
            directory = Path(temporary)
            process, config, output = self.start(directory, self.make_fake(directory))
            self.stop(process)
            self.assertEqual(list((directory / "socket").iterdir()), [])
            events = [json.loads(line) for path in output.glob("*.jsonl") for line in path.read_text().splitlines()]
            self.assertEqual([event["event"] for event in events], ["native_ipc_helper_start", "native_ipc_helper_exit"])
            self.assertTrue(events[-1]["helper_reaped"])
            self.assertFalse(events[-1]["helper_forced_kill"])
            with self.assertRaises(ProcessLookupError):
                os.kill(events[0]["helper_pid"], 0)

    def test_full_launcher_preserves_config_ca_environment_bytes_and_concurrency(self):
        with tempfile.TemporaryDirectory(prefix="qipc-") as temporary:
            directory = Path(temporary)
            environment = dict(os.environ, SSL_CERT_FILE="synthetic-ca-file", CODEX_CA_CERTIFICATE="synthetic-codex-ca")
            environment.pop("SSL_CERT_DIR", None)
            process, config, output = self.start(directory, self.make_fake(directory), environment)
            failures = []
            def execute(index):
                try:
                    with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as client:
                        client.settimeout(5)
                        client.connect(config["unix_socket"])
                        raw, _, _ = request(json.dumps({"model": "gpt-5.6-sol", "input": str(index)}).encode())
                        capture.write_all(lambda part: client.send(part[:3]), raw)
                        _, body = receive_frames(client)
                        response = json.loads(body)
                        self.assertEqual(response["request_wire_sha256"], capture.digest(raw))
                        expected = dict(config)
                        expected.pop("unix_socket")
                        self.assertEqual(response["startup_without_socket"], expected)
                        self.assertEqual(response["ca_environment_sha256"], {key: capture.digest(environment[key].encode()) if key in environment else None for key in capture.CA_KEYS})
                except BaseException as error:
                    failures.append(error)
            workers = [threading.Thread(target=execute, args=(index,)) for index in range(8)]
            for worker in workers:
                worker.start()
            for worker in workers:
                worker.join(timeout=7)
            self.stop(process)
            self.assertTrue(all(not worker.is_alive() for worker in workers))
            self.assertEqual(failures, [])
            self.assertEqual(list((directory / "socket").iterdir()), [])
            persisted = "".join(path.read_text() for path in output.glob("*.jsonl"))
            events = [json.loads(line) for line in persisted.splitlines()]
            self.assertEqual(len(events), 10)
            self.assertEqual(len({event["connection"] for event in events if event["event"] == "native_ipc_request"}), 8)
            self.assertFalse(events[0]["runtime_executable_matches_helper"])
            for secret in (TOKEN, WORKSPACE, "8" * 64, "9" * 64, "synthetic-ca-file", "synthetic-codex-ca"):
                self.assertNotIn(secret, persisted)

    def test_sigterm_reaps_helper_and_cleans_sockets_while_stdin_remains_open(self):
        with tempfile.TemporaryDirectory(prefix="qipc-") as temporary:
            directory = Path(temporary)
            process, _, output = self.start(directory, self.make_fake(directory))
            process.terminate()
            process.wait(timeout=6)
            self.stop(process)
            self.assertEqual(list((directory / "socket").iterdir()), [])
            events = [json.loads(line) for path in output.glob("*.jsonl") for line in path.read_text().splitlines()]
            self.assertEqual(events[-1]["event"], "native_ipc_helper_exit")
            self.assertTrue(events[-1]["helper_reaped"])
            self.assertFalse(events[-1]["helper_forced_kill"])
            with self.assertRaises(ProcessLookupError):
                os.kill(events[0]["helper_pid"], 0)

    def test_capture_failure_keeps_subsequent_requests_but_invalidates_session(self):
        with tempfile.TemporaryDirectory(prefix="qipc-") as temporary:
            directory = Path(temporary)
            process, config, output = self.start(directory, self.make_fake(directory), injected_capture_failure=True)
            for index in range(2):
                with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as client:
                    client.settimeout(5)
                    client.connect(config["unix_socket"])
                    raw, _, _ = request(json.dumps({"input": str(index) * 15000}).encode())
                    client.sendall(raw)
                    _, body = receive_frames(client)
                    self.assertEqual(json.loads(body)["request_wire_sha256"], capture.digest(raw))
            self.stop(process, expected_returncode=1)
            self.assertEqual(list((directory / "socket").iterdir()), [])
            events = [json.loads(line) for path in output.glob("*.jsonl") for line in path.read_text().splitlines()]
            self.assertEqual([event["event"] for event in events], ["native_ipc_helper_start"])
            markers = list(output.glob("*.invalid"))
            self.assertEqual(len(markers), 1)
            self.assertFalse(json.loads(markers[0].read_text())["capture_valid"])
            with self.assertRaises(ProcessLookupError):
                os.kill(events[0]["helper_pid"], 0)

    def test_real_rust_helper_local_http_body_and_executable_identity(self):
        helper_path = os.environ.get("CODEX_NATIVE_HTTP_TEST_BIN")
        if not helper_path or not Path(helper_path).is_file():
            self.skipTest("explicit real Rust CODEX_NATIVE_HTTP_TEST_BIN required")
        received = []
        class Handler(BaseHTTPRequestHandler):
            def do_POST(self):
                body = self.rfile.read(int(self.headers["Content-Length"]))
                received.append((self.path, body, self.headers.get("Authorization")))
                response = b'{"fixture":"local-only"}'
                self.send_response(200)
                self.send_header("Content-Length", str(len(response)))
                self.end_headers()
                self.wfile.write(response)
            def log_message(self, *_):
                pass
        server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        server_worker = threading.Thread(target=server.serve_forever)
        server_worker.start()
        try:
            with tempfile.TemporaryDirectory(prefix="qipc-") as temporary:
                directory = Path(temporary)
                process, config, output = self.start(directory, Path(helper_path), upstream="http://127.0.0.1:%d" % server.server_port)
                with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as client:
                    client.settimeout(5)
                    client.connect(config["unix_socket"])
                    raw, _, body = request(ipc_token=config["ipc_token"])
                    client.sendall(raw)
                    _, response_body = receive_frames(client)
                    self.assertEqual(response_body, b'{"fixture":"local-only"}')
                self.stop(process)
                events = [json.loads(line) for path in output.glob("*.jsonl") for line in path.read_text().splitlines()]
                self.assertEqual(received, [("/responses", body, "Bearer " + TOKEN)])
                self.assertTrue(events[0]["runtime_executable_matches_helper"])
                self.assertEqual(events[1]["actual_ipc_body_sha256"], capture.digest(body))
                self.assertTrue(events[1]["request_forwarded_complete"])
        finally:
            server.shutdown()
            server.server_close()
            server_worker.join(timeout=3)


if __name__ == "__main__":
    unittest.main()
