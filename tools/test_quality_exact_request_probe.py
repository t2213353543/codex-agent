"""Offline exact replay contracts; opt-in real Rust connects only to loopback."""
import base64
import copy
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import stat
import tempfile
import threading
import time
import unittest
from unittest import mock

import quality_exact_request_probe as probe


BODY = b'{ "model":"gpt-5.6-sol", "instructions":"private-instruction", "input":[{"type":"message","id":"msg_fixed","role":"user","content":[{"type":"input_text","text":"private prompt <>&"}]}], "reasoning":{"effort":"medium"}, "store":false, "stream":true }'
TOKEN = "synthetic-sensitive-at"
WORKSPACE = "synthetic-sensitive-workspace"


def head(body=BODY, compression="none"):
    return {"schema": probe.CAPTURE_HEAD_SCHEMA, "method": "POST", "path": "/backend-api/codex/responses",
            "headers": [[name, base64.b64encode(value.encode()).decode()] for name, value in (
                ("Accept", "text/event-stream"), ("Content-Type", "application/json"),
                ("User-Agent", "codex_exec/0.155.0 (Linux 6.8; x86_64) unknown"),
                ("Session-Id", "fixed-session-private"), ("Thread-Id", "fixed-thread-private"),
                ("X-Codex-Turn-State", "fixed-turn-state-private"), ("X-Codex-Routing-Hint", "model=gpt-5.6-sol"))],
            "request_compression": compression, "body_sha256": probe.sha(body), "body_bytes": len(body)}


def write_private(path, raw):
    path.write_bytes(raw)
    path.chmod(0o600)


class CaptureInputTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name)
        self.body = self.root / "request.body"
        self.head = self.root / "request-head.json"
        write_private(self.body, BODY)
        write_private(self.head, json.dumps(head()).encode())

    def tearDown(self):
        self.temp.cleanup()

    def load(self):
        return probe.load_capture(self.body, self.head, probe.sha(self.body.read_bytes()), probe.sha(self.head.read_bytes()))

    def test_exact_body_and_header_bytes_are_preserved_and_auth_is_only_replacement(self):
        raw, captured = self.load()
        before = copy.deepcopy(captured)
        first = probe.exact_head(captured, raw, TOKEN, WORKSPACE, "ipc-one", 230000)
        second = probe.exact_head(captured, raw, "second-at", "second-workspace", "ipc-two", 210000)
        self.assertEqual(raw, BODY)
        self.assertEqual(captured, before)
        self.assertEqual(first["headers"][:-2], second["headers"][:-2])
        self.assertEqual(first["headers"][:-2], captured["headers"])
        self.assertEqual(first["body_length"], len(BODY))
        for name in ("method", "path", "body_length"):
            self.assertEqual(first[name], second[name])
        self.assertEqual(first["headers"][-2][0], "Authorization")
        self.assertEqual(first["headers"][-1][0], "ChatGPT-Account-Id")

    def test_zstd_is_native_flag_and_never_python_recompression(self):
        write_private(self.head, json.dumps(head(compression="zstd")).encode())
        raw, captured = self.load()
        encoded = probe.exact_head(captured, raw, TOKEN, WORKSPACE, "ipc", 240000)
        self.assertEqual(encoded["request_compression"], "zstd")
        self.assertEqual(raw, BODY)
        self.assertNotIn("content-encoding", [name.lower() for name, _ in encoded["headers"]])

    def test_capture_pin_and_bearer_or_unsafe_headers_fail_before_dispatch(self):
        with self.assertRaisesRegex(probe.ExactProbeError, "digest_mismatch"):
            probe.load_capture(self.body, self.head, "0" * 64, probe.sha(self.head.read_bytes()))
        for name, value in (("Authorization", "Bearer secret"), ("Cookie", "secret"),
                            ("Host", "other-host"), ("X-Codex-Other", "Bearer embedded-secret"),
                            ("Content-Length", "99"), ("X-Oai-Attestation", "opaque-other-account")):
            with self.subTest(name=name):
                modified = head()
                modified["headers"].append([name, base64.b64encode(value.encode()).decode()])
                write_private(self.head, json.dumps(modified).encode())
                with self.assertRaises(probe.ExactProbeError):
                    self.load()

    def test_duplicate_members_and_previous_response_are_not_cross_account_replayable(self):
        for raw in (b'{"model":"a","model":"b","stream":true,"store":false}',
                    b'{"model":"a","previous_response_id":"other-owner","stream":true,"store":false}'):
            write_private(self.body, raw)
            write_private(self.head, json.dumps(head(raw)).encode())
            with self.assertRaises(probe.ExactProbeError):
                self.load()

    def test_at_only_credential_rejects_rt_id_token_and_wrong_identity(self):
        path = self.root / "at.json"
        for extra in ("refresh_token", "id_token", "tokens"):
            write_private(path, json.dumps({"access_token": "synthetic", "account_id": "workspace", extra: "must-not-pass"}).encode())
            with self.assertRaisesRegex(probe.ExactProbeError, "at_only"):
                probe.load_at_only(path, 240)
        expiration = int(time.time()) + 3600
        claims = {"exp": expiration, probe.AUTH_CLAIM: {"chatgpt_account_id": "workspace"}}
        token = "header." + base64.urlsafe_b64encode(json.dumps(claims).encode()).decode().rstrip("=") + ".signature"
        write_private(path, json.dumps({"access_token": token, "account_id": "workspace", "exp": expiration}).encode())
        self.assertEqual(probe.load_at_only(path, 240)[:2], (token, "workspace"))
        write_private(path, json.dumps({"access_token": token, "account_id": "other", "exp": expiration}).encode())
        with self.assertRaisesRegex(probe.ExactProbeError, "identity_or_expiry"):
            probe.load_at_only(path, 240)

    @unittest.skipUnless(os.name == "posix", "POSIX permission/symlink controls")
    def test_public_and_symlink_inputs_rejected(self):
        self.body.chmod(0o644)
        with self.assertRaises(probe.ExactProbeError):
            self.load()
        self.body.chmod(0o600)
        link = self.root / "link"
        link.symlink_to(self.body)
        with self.assertRaises(probe.ExactProbeError):
            probe.private_read(link, probe.MAX_BODY)


class SSETests(unittest.TestCase):
    def test_absolute_deadline_is_recomputed_for_every_short_read_and_write(self):
        class FragmentedSocket:
            def __init__(self):
                self.timeouts, self.calls = [], 0
            def settimeout(self, value):
                self.timeouts.append(value)
            def recv(self, _):
                self.calls += 1
                return b"x"
            def send(self, _):
                self.calls += 1
                return 1
        for operation in ("recv", "send"):
            stream = FragmentedSocket()
            with mock.patch.object(probe.time, "monotonic", side_effect=(100.1, 100.5, 101.1)):
                with self.assertRaises(TimeoutError):
                    if operation == "recv":
                        probe.read_exact(lambda count: probe.deadline_io(stream, 101, "recv", count), 3)
                    else:
                        probe.write_all(lambda raw: probe.deadline_io(stream, 101, "send", raw), b"xxx")
            self.assertEqual(stream.calls, 2)
            self.assertAlmostEqual(stream.timeouts[0], 0.9)
            self.assertAlmostEqual(stream.timeouts[1], 0.5)

    def collect(self, events, newline="\n", chunk=1, status=200):
        wire = ("\ufeff" + "".join("data: " + json.dumps(event, ensure_ascii=False) + newline * 2 for event in events)).encode()
        collector = probe.SSECollector([TOKEN, WORKSPACE])
        for offset in range(0, len(wire), chunk):
            collector.feed(wire[offset:offset + chunk])
        collector.feed(b"", eof=True)
        return collector.report(status)

    def test_fragmented_completed_sse_preserves_final_status_controls_and_usage(self):
        message = {"type": "message", "role": "assistant", "phase": "final_answer", "content": [{"type": "output_text", "text": "最终答案：29个"}]}
        events = [{"type": "response.created", "response": {"id": "r", "model": "gpt-5.6-sol", "service_tier": "auto", "reasoning": {"effort": "medium"}}},
                  {"type": "response.output_item.done", "output_index": 0, "item": message},
                  {"type": "response.completed", "response": {"id": "r", "status": "completed", "output": [], "service_tier": "default", "usage": {"input_tokens": 10, "output_tokens": 20, "output_tokens_details": {"reasoning_tokens": 15}, "private": TOKEN}}}]
        for newline in ("\n", "\r\n", "\r"):
            with self.subTest(newline=newline):
                report = self.collect(events, newline)
                self.assertTrue(report["protocol_success"])
                self.assertEqual(report["answer"], 29)
                self.assertEqual(report["response_error"], {})
                self.assertEqual(report["response_usage"]["output_tokens_details"]["reasoning_tokens"], 15)
                self.assertNotIn(TOKEN, json.dumps(report))

    def test_failed_200_or_incomplete_is_not_scored_as_wrong_answer(self):
        for kind in ("response.failed", "response.incomplete", "error"):
            with self.subTest(kind=kind):
                report = self.collect([{"type": kind, "status": 429, "error": {"code": "rate_limit_exceeded", "message": TOKEN}, "response": {"status": "failed"}}])
                self.assertFalse(report["protocol_success"])
                self.assertIsNone(report["answer"])
                self.assertEqual(report["response_error"]["code"], "rate_limit_exceeded")
                self.assertNotIn(TOKEN, json.dumps(report))

    def test_conflicting_final_sources_are_not_silently_preferred(self):
        def item(answer):
            return {"type": "message", "role": "assistant", "content": [{"type": "output_text", "text": "最终答案：%d个" % answer}]}
        report = self.collect([{"type": "response.output_item.done", "output_index": 0, "item": item(21)},
                               {"type": "response.completed", "response": {"status": "completed", "output": [item(29)]}}])
        self.assertTrue(report["final_output_conflict"])
        self.assertIsNone(report["answer"])

    def test_missing_truncated_invalid_json_and_invalid_utf8_are_not_completed(self):
        collector = probe.SSECollector()
        collector.feed(b'data: {"type":"response.created","response":{"id":"r"}}\n\n')
        self.assertFalse(collector.report(200)["protocol_success"])
        for raw in (b'data: {"type":"response.completed"}', b'data: {bad}\n\n', b'data: {"type":"error","message":"\xff"}\n\n'):
            with self.subTest(raw=raw), self.assertRaises(probe.ExactProbeError):
                probe.SSECollector().feed(raw, eof=True)

    def test_final_output_redacts_actual_credentials(self):
        report = self.collect([{"type": "response.completed", "response": {"id": "r", "status": "completed", "output": [
            {"type": "message", "role": "assistant", "content": [{"type": "output_text", "text": TOKEN + " " + WORKSPACE + "\n最终答案：21个"}]}]}}])
        self.assertNotIn(TOKEN, json.dumps(report))
        self.assertNotIn(WORKSPACE, json.dumps(report))
        self.assertEqual(report["answer"], 21)

    def test_reasoning_budget_and_every_output_token_detail_are_stage_observations(self):
        report = self.collect([
            {"type": "response.created", "response": {"model": "gpt-5.6-sol", "service_tier": "auto", "max_output_tokens": None,
                "reasoning": {"effort": "medium", "summary": None, "context": "all_turns", "mode": "standard", "budget_tokens": 512, "private_field": TOKEN}}},
            {"type": "response.completed", "response": {"id": "r", "status": "completed", "model": "gpt-5.6-sol", "service_tier": "default", "max_output_tokens": 4096,
                "reasoning": {"effort": "medium", "budget_tokens": 1024}, "usage": {"output_tokens": 600,
                    "output_tokens_details": {"reasoning_tokens": 516, "accepted_prediction_tokens": 20, "rejected_prediction_tokens": 4, "audio_tokens": 0}}, "output": []}}])
        self.assertEqual(report["response_signals"][0]["reasoning"]["budget_tokens"], 512)
        self.assertEqual(report["response_signals"][1]["reasoning"]["budget_tokens"], 1024)
        self.assertIsNone(report["response_signals"][0]["max_output_tokens"])
        self.assertEqual(report["response_controls"]["max_output_tokens"], 4096)
        self.assertEqual(len(report["response_usage"]["output_tokens_details"]), 4)
        self.assertEqual(report["response_usage"]["output_tokens_details"]["reasoning_tokens"], 516)
        self.assertNotIn(TOKEN, json.dumps(report))
        self.assertNotIn("private_field", json.dumps(report))

    def test_foreign_or_contradictory_response_cannot_supply_a_final_answer(self):
        base = {"type": "response.created", "response": {"id": "original"}}
        for foreign in (
                {"type": "response.output_item.done", "response_id": "foreign", "item": {"type": "message", "role": "assistant", "content": [{"type": "output_text", "text": "最终答案：21个"}]}},
                {"type": "response.completed", "response_id": "foreign", "response": {"id": "original", "status": "completed", "output": []}},
                {"type": "response.completed", "response": {"id": "foreign", "status": "completed", "output": []}}):
            with self.subTest(kind=foreign["type"]), self.assertRaisesRegex(probe.ExactProbeError, "conflicting_response_identity"):
                self.collect([base, foreign])


class OutputEvidenceTests(unittest.TestCase):
    collect = SSETests.collect

    def evidence(self, reasoning, answer=21):
        final = {"type": "message", "id": "private-message-id", "role": "assistant", "content": [
            {"type": "output_text", "text": "最终答案：%d个" % answer}]}
        return self.collect([
            {"type": "response.created", "response": {"id": "private-response-id"}},
            {"type": "response.output_item.done", "response_id": "private-response-id", "output_index": 0, "item": reasoning},
            {"type": "response.completed", "response": {"id": "private-response-id", "status": "completed", "output": [reasoning, final],
                "usage": {"output_tokens_details": {"reasoning_tokens": 516}}}},
        ], chunk=4096)

    def test_same_reasoning_different_final_and_same_tokens_different_reasoning_are_distinguished(self):
        reasoning = {"type": "reasoning", "id": "private-reasoning-id", "summary": [{"type": "summary_text", "text": "private reasoning A"}],
                     "content": [{"type": "reasoning_text", "text": "private nested thought"}], "encrypted_content": "private-cipher-A"}
        first, second = self.evidence(reasoning, 21), self.evidence(reasoning, 29)
        altered = copy.deepcopy(reasoning)
        altered["summary"][0]["text"] = "private reasoning B"
        third = self.evidence(altered, 29)
        first_output, second_output, third_output = [report["response_output_evidence"] for report in (first, second, third)]
        for source in ("done_items", "terminal_output"):
            self.assertEqual(first_output[source]["items"][0]["item"]["content_sha256"], second_output[source]["items"][0]["item"]["content_sha256"])
            self.assertNotEqual(second_output[source]["items"][0]["item"]["content_sha256"], third_output[source]["items"][0]["item"]["content_sha256"])
        self.assertEqual(first["response_usage"], third["response_usage"])
        self.assertNotEqual(first_output["terminal_output"]["content_sha256"], second_output["terminal_output"]["content_sha256"])
        self.assertNotEqual(first_output["final_text"]["extracted_utf8_sha256"], second_output["final_text"]["extracted_utf8_sha256"])
        item = first_output["done_items"]["items"][0]["item"]
        self.assertEqual(item["item_type"], "reasoning")
        self.assertEqual(item["id_sha256"], probe.sha(b"private-reasoning-id"))
        for key in ("content", "summary", "encrypted_content"):
            self.assertIn(key, item["field_summary"]["fields"])
        renamed = self.evidence(dict(reasoning, id="different-private-reasoning-id"))["response_output_evidence"]["terminal_output"]["items"][0]["item"]
        self.assertNotEqual(item["content_sha256"], renamed["content_sha256"])
        self.assertNotEqual(item["id_sha256"], renamed["id_sha256"])
        self.assertEqual(item["field_summary"]["fields"]["summary"], renamed["field_summary"]["fields"]["summary"])
        for secret in ("private reasoning", "private nested thought", "private-cipher", "private-reasoning-id", "private-message-id", "private-response-id"):
            self.assertNotIn(secret, json.dumps(first))

    def test_missing_null_empty_and_done_terminal_sources_remain_distinct(self):
        events = [{"type": "response.created", "response": {"id": "r"}}]
        for index, item in enumerate(({"type": "reasoning"}, {"type": "reasoning", "summary": None}, {"type": "reasoning", "summary": []})):
            event = {"type": "response.output_item.done", "response_id": "r", "item": item}
            if index:
                event["output_index"] = None if index == 1 else 0
            events.append(event)
        events.append({"type": "response.completed", "response": {"id": "r", "status": "completed", "output": []}})
        evidence = self.collect(events, chunk=4096)["response_output_evidence"]
        items = evidence["done_items"]["items"]
        self.assertEqual(len({item["item"]["content_sha256"] for item in items}), 3)
        self.assertNotIn("summary", items[0]["item"]["field_summary"]["fields"])
        self.assertEqual(items[1]["item"]["field_summary"]["fields"]["summary"]["json_type"], "null")
        self.assertEqual(items[2]["item"]["field_summary"]["fields"]["summary"]["json_type"], "array")
        self.assertFalse(items[0]["output_index_present"])
        self.assertIsNone(items[1]["output_index"])
        self.assertEqual(items[2]["output_index"], 0)
        self.assertEqual(evidence["terminal_output"]["items"], [])
        for present, value in ((False, None), (True, None), (True, [])):
            response = {"id": "r", "status": "completed"}
            if present:
                response["output"] = value
            output = self.collect([{"type": "response.completed", "response": response}], chunk=4096)["response_output_evidence"]["terminal_output"]
            self.assertEqual(output["present"], present)
            if present:
                self.assertEqual(output["json_type"], "null" if value is None else "array")

    def test_exact_numbers_key_order_string_escapes_and_terminal_data_digest(self):
        def collect_raw(item):
            payload = b'{"type":"response.completed","response":{"id":"r","status":"completed","output":[' + item + b']}}'
            collector = probe.SSECollector()
            collector.feed(b"data: " + payload + b"\n\n")
            return collector.report(200)["response_output_evidence"], payload
        first, raw = collect_raw(b'{"type":"reasoning","summary":[],"private-amount":9007199254740993.123456789,"content":"a\\u0062"}')
        reordered, _ = collect_raw(b'{"content":"ab", "private-amount":9007199254740993.123456789, "summary":[], "type":"reasoning"}')
        changed, _ = collect_raw(b'{"type":"reasoning","summary":[],"private-amount":9007199254740993.123456790,"content":"ab"}')
        self.assertEqual(first["terminal_output"]["content_sha256"], reordered["terminal_output"]["content_sha256"])
        self.assertNotEqual(first["terminal_output"]["content_sha256"], changed["terminal_output"]["content_sha256"])
        self.assertEqual(first["terminal_event_data"]["sha256"], probe.sha(raw))
        self.assertEqual(first["terminal_event_data"]["bytes"], len(raw))
        self.assertNotEqual(first["terminal_event_data"]["sha256"], reordered["terminal_event_data"]["sha256"])
        self.assertNotIn("private-amount", json.dumps(first))
        self.assertIn("exact_number_lexemes", first["summary_basis"])
        one, _ = collect_raw(b'{"type":"reasoning","summary":[1]}')
        one_decimal, _ = collect_raw(b'{"type":"reasoning","summary":[1.0]}')
        self.assertNotEqual(one["terminal_output"]["content_sha256"], one_decimal["terminal_output"]["content_sha256"])
        with self.assertRaisesRegex(probe.ExactProbeError, "invalid_json"):
            collect_raw(b'{"type":"reasoning","summary":[],"summary":[1]}')

    def test_unknown_names_types_nested_secrets_and_redaction_hashes_do_not_leak(self):
        reasoning = {"type": "private-unknown-output-type", "id": TOKEN, "private-field-name": {"private-nested-name": WORKSPACE},
                     "content": {"private-thought-key": "private-thought-value"}, "summary": TOKEN, "encrypted_content": WORKSPACE}
        report = self.evidence(reasoning)
        item = report["response_output_evidence"]["terminal_output"]["items"][0]["item"]
        self.assertEqual(item["item_type"], "unknown")
        self.assertIn("sha256:" + probe.sha(b"private-field-name"), item["field_summary"]["fields"])
        for secret in (TOKEN, WORKSPACE, "private-unknown-output-type", "private-field-name", "private-nested-name", "private-thought-key", "private-thought-value"):
            self.assertNotIn(secret, json.dumps(report))
        raw_text = TOKEN + "\n最终答案：21个"
        report = self.collect([{"type": "response.completed", "response": {"id": "r", "status": "completed", "output": [
            {"type": "message", "role": "assistant", "content": [{"type": "output_text", "text": raw_text}]}]}}], chunk=4096)
        summary = report["response_output_evidence"]["final_text"]
        self.assertEqual(summary["extracted_utf8_sha256"], probe.sha(raw_text.encode()))
        self.assertEqual(summary["redacted_utf8_sha256"], probe.sha(report["final_output"].encode()))
        self.assertNotEqual(summary["extracted_utf8_sha256"], summary["redacted_utf8_sha256"])

    def test_item_field_and_response_bounds_are_explicit_not_silently_complete(self):
        count = 130
        item = dict({"type": "reasoning", "summary": "x" * (1 << 20)}, **{"private-%03d" % index: index for index in range(40)})
        output = [item] + [{"type": "reasoning", "summary": []} for _ in range(count - 1)]
        events = [{"type": "response.output_item.done", "item": {"type": "reasoning", "summary": []}} for _ in range(count)]
        events.append({"type": "response.completed", "response": {"id": "r", "status": "completed", "output": output}})
        report = self.collect(events, chunk=1 << 20)
        evidence = report["response_output_evidence"]
        for key in ("done_items", "terminal_output"):
            self.assertFalse(evidence[key]["complete"])
            self.assertEqual(evidence[key]["items_captured"], 128)
        fields = evidence["terminal_output"]["items"][0]["item"]["field_summary"]
        self.assertEqual(fields["fields_captured"], 32)
        self.assertFalse(fields["complete"])
        self.assertEqual(fields["fields"]["summary"]["canonical_json_bytes"], (1 << 20) + 2)
        self.assertLess(len(json.dumps(report)), 300000)
        with mock.patch.object(probe, "MAX_RESPONSE", 32), self.assertRaisesRegex(probe.ExactProbeError, "response_limit"):
            probe.SSECollector().feed(b"x" * 33)

    def test_foreign_reasoning_is_rejected_before_its_evidence_is_retained(self):
        collector = probe.SSECollector()
        collector.feed(b'data: {"type":"response.created","response":{"id":"original"}}\n\n')
        with self.assertRaisesRegex(probe.ExactProbeError, "conflicting_response_identity"):
            collector.feed(b'data: {"type":"response.output_item.done","response_id":"foreign","item":{"type":"reasoning","summary":["private-foreign-reasoning"]}}\n\n')
        report = collector.report(200)
        self.assertEqual(report["response_output_evidence"]["done_items"]["items"], [])
        self.assertFalse(report["protocol_success"])
        self.assertNotIn("private-foreign-reasoning", json.dumps(report))


@unittest.skipUnless(os.name == "posix", "real Rust helper uses POSIX IPC")
class NativeRustTests(unittest.TestCase):
    def test_real_native_single_post_preserves_bytes_and_reaps_helper(self):
        name = os.environ.get("CODEX_NATIVE_HTTP_TEST_BIN")
        if not name or not Path(name).is_file():
            self.skipTest("explicit real Rust helper required")
        helper = Path(name)
        received = []
        class Handler(BaseHTTPRequestHandler):
            def do_POST(self):
                raw = self.rfile.read(int(self.headers["Content-Length"]))
                received.append((raw, self.headers.get("Authorization"), self.headers.get("ChatGPT-Account-Id"), self.headers.get("Session-Id")))
                event = {"type": "response.completed", "response": {"id": "r", "status": "completed", "output": [
                    {"type": "reasoning", "id": "private-native-reasoning-id", "summary": [{"type": "summary_text", "text": "private-native-thought"}],
                     "encrypted_content": "private-native-cipher", "private-native-name": TOKEN}]}}
                wire = b"data: " + json.dumps(event).encode() + b"\n\n"
                self.send_response(200)
                self.send_header("Content-Type", "text/event-stream")
                self.send_header("Content-Length", str(len(wire)))
                self.end_headers()
                self.wfile.write(wire)
            def log_message(self, *_):
                pass
        server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        worker = threading.Thread(target=server.serve_forever)
        worker.start()
        try:
            result = probe.native_request(helper, probe.file_digest(helper), BODY, head(), TOKEN, WORKSPACE,
                                          [TOKEN, WORKSPACE], 20, "http://127.0.0.1:%d" % server.server_port)
        finally:
            server.shutdown()
            server.server_close()
            worker.join(timeout=3)
        self.assertTrue(result["protocol_success"], result)
        self.assertEqual(received, [(BODY, "Bearer " + TOKEN, WORKSPACE, "fixed-session-private")])
        self.assertEqual(result["native_dispatch_count"], 1)
        self.assertEqual(result["actual_ipc_body_sha256"], probe.sha(BODY))
        self.assertTrue(result["helper_reaped"])
        self.assertTrue(result["local_socket_absent"])
        self.assertFalse(result["helper_forced_kill"])
        output = result["response_output_evidence"]["terminal_output"]
        self.assertTrue(output["complete"])
        self.assertEqual(output["items"][0]["item"]["item_type"], "reasoning")
        self.assertEqual(output["items"][0]["item"]["id_sha256"], probe.sha(b"private-native-reasoning-id"))
        for secret in (TOKEN, WORKSPACE, "private-instruction", "private prompt", "private-native-thought", "private-native-cipher", "private-native-name", "private-native-reasoning-id"):
            self.assertNotIn(secret, json.dumps(result))


if __name__ == "__main__":
    unittest.main()
