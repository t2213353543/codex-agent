"""Synthetic redaction, exact WS state expansion and incomplete stream checks."""
import json
from pathlib import Path
import tempfile
import unittest

import appserver_parity_report as report


class ReportTest(unittest.TestCase):
    def test_lite_completed_output_restores_sorted_items_and_terminal_fields(self):
        output = report.CompletedOutput()
        output.observe({"output_index": 1, "item": {"id": "message", "type": "message", "role": "assistant", "phase": "final_answer", "content": ["old"]}})
        output.observe({"output_index": 0, "item": {"id": "reasoning", "type": "reasoning", "encrypted_content": "secret"}})
        items, provenance = output.complete({"id": "response", "output": []})
        self.assertEqual([item["id"] for item in items], ["reasoning", "message"])
        self.assertEqual(provenance, "completed_items_terminal_empty")
        items, provenance = output.complete({"id": "response", "output": [{"id": "message", "type": "message", "content": None}]})
        self.assertEqual([item["id"] for item in items], ["reasoning", "message"])
        self.assertIsNone(items[1]["content"])
        self.assertEqual(items[1]["phase"], "final_answer")
        self.assertEqual(items[0]["encrypted_content"], "secret")
        self.assertEqual(provenance, "completed_items_merged_with_terminal_overrides")

    def test_output_conflict_cannot_claim_exact_state(self):
        output = report.CompletedOutput()
        output.observe({"response_id": "response-a", "output_index": 0, "item": {"id": "known", "type": "message"}})
        items, provenance = output.complete({"id": "response-a", "output": [{"id": "unknown", "type": "message"}]})
        self.assertIsNone(items)
        self.assertEqual(provenance, "terminal_output_identity_unreconciled")
        output.observe({"response_id": "response-b", "output_index": 1, "item": {"id": "other", "type": "message"}})
        items, provenance = output.complete({"id": "response-b", "output": []})
        self.assertIsNone(items)
        self.assertEqual(provenance, "conflicting_response_ids")

    def test_missing_done_cannot_publish_incomplete_history(self):
        output = report.CompletedOutput()
        output.started({"output_index": 0, "item": {"id": "unfinished", "type": "message"}})
        items, provenance = output.complete({"id": "response", "output": []})
        self.assertIsNone(items)
        self.assertEqual(provenance, "started_output_missing_done")

    def test_encoded_turn_metadata_keeps_routing_and_policy_differences(self):
        a = json.dumps({"thread_id": "secret-left", "sandbox_mode": "read-only", "auto_review_enabled": False})
        b = json.dumps({"thread_id": "secret-right", "sandbox_mode": "read-only", "auto_review_enabled": True, "thread_source": "exec"})
        left = {"body": {"client_metadata": {"x-codex-turn-metadata": a}}, "header_turn_metadata": a}
        right = {"body": {"client_metadata": {"x-codex-turn-metadata": b}}, "header_turn_metadata": b}
        result = report.compare_turn_metadata(left, right)
        self.assertEqual([item["path"] for item in result["header"]["stable_diff"]], ["$.auto_review_enabled", "$.thread_source"])
        self.assertEqual(result["header"]["dynamic_fields"][0]["path"], "$.thread_id")
        self.assertNotIn("secret-", json.dumps(result))

    def test_secret_values_keys_paths_and_signed_query_are_hashed(self):
        secret = "SYNTHETIC_SECRET_NEVER_EXPORT"
        value = {secret: {"text": secret, "model": "gpt-6-astra"}, "reasoning": {"effort": "xhigh"}}
        output = json.dumps(report.field_tree(value))
        output += json.dumps(report.safe_url("https://chatgpt.com/backend-api/codex/responses/" + secret + "?signature=" + secret))
        self.assertNotIn(secret, output)
        self.assertIn("xhigh", output)
        self.assertIn("gpt-6-astra", output)
        self.assertIn("<key-sha256:", output)

    def test_dynamic_candidates_remain_explicit_and_body_text_is_private(self):
        diff, dynamic = report.semantic_diff({"input": "secret-a", "prompt_cache_key": "secret-b"},
                                             {"input": "secret-c", "prompt_cache_key": "secret-d"})
        self.assertEqual([x["path"] for x in diff], ["$.input"])
        self.assertEqual([x["path"] for x in dynamic], ["$.prompt_cache_key"])
        self.assertFalse(dynamic[0]["discarded_from_evidence"])
        self.assertNotIn("secret-", json.dumps([diff, dynamic]))

    def test_duplicate_json_is_rejected(self):
        with self.assertRaises(ValueError):
            report.parse('{"input":"one","input":"two"}')

    def test_network_trace_classifies_nonproxy_loopback_and_remote(self):
        with tempfile.TemporaryDirectory() as folder:
            path = Path(folder) / "trace"
            path.write_text('12 connect(4, {sa_family=AF_INET, sin_port=htons(4444), sin_addr=inet_addr("127.0.0.1")}, 16) = 0\n'
                            '13 connect(5, {sa_family=AF_INET, sin_port=htons(443), sin_addr=inet_addr("192.0.2.2")}, 16) = -1\n')
            result = report.network_summary(path, 4444)
            self.assertEqual(result["non_loopback_connect_attempt_count"], 1)
            self.assertTrue(result["destinations"][0]["matches_capture_proxy"])
            self.assertNotIn("192.0.2.2", json.dumps(result))
            self.assertFalse(result["all_possible_egress_proven"])

    def test_ws_previous_id_expansion_and_stream_terminal_classification(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder) / "official_app_server"
            capture = root / "capture"
            private = capture / "private"
            private.mkdir(parents=True)
            events = []
            def blob(value):
                raw = value if isinstance(value, bytes) else json.dumps(value).encode()
                name = str(len(list(private.iterdir()))) + ".bin"
                (private / name).write_bytes(raw)
                return {"file": "private/" + name, "length": len(raw), "sha256": report.digest(raw)}
            def request(flow, method, raw):
                return {"event": "request", "flow_id": flow, "phase": {"case": "text"}, "method": method,
                        "private_head": blob({"url": "https://chatgpt.com/backend-api/codex/responses"}),
                        "body": blob(raw), "decoded_body": blob(raw), "decode_status": "identity",
                        "http_version": "HTTP/1.1", "headers": []}
            events.append(request("ws", "GET", b""))
            def ws(client, value):
                events.append({"event": "websocket_message", "flow_id": "ws", "phase": {"case": "text"},
                               "from_client": client, "body": blob(value)})
            prewarm = {"type": "response.create", "generate": False, "input": [{"type": "message", "role": "developer", "content": "secret-instruction"}]}
            ws(True, prewarm)
            ws(False, {"type": "response.completed", "response": {"id": "resp-secret-1", "output": []}})
            ws(True, {"type": "response.create", "previous_response_id": "resp-secret-1", "input": [{"role": "user", "content": "secret-question"}]})
            ws(False, {"type": "response.created", "response": {"id": "resp-secret-2"}})
            ws(False, {"type": "response.output_item.done", "output_index": 1, "item": {"id": "message-secret", "type": "message", "role": "assistant", "content": "secret-answer"}})
            ws(False, {"type": "response.output_item.done", "output_index": 0, "item": {"id": "reasoning-secret", "type": "reasoning", "encrypted_content": "secret-encrypted"}})
            ws(False, {"type": "response.completed", "response": {"id": "resp-secret-2", "output": []}})
            ws(True, {"type": "response.create", "previous_response_id": "resp-secret-2", "input": [{"role": "user", "content": "secret-followup"}]})
            ws(True, {"type": "response.create", "previous_response_id": "resp-missing", "input": []})
            events.append(request("http", "POST", b'{"input":"secret-question"}'))
            events.append({"event": "response_headers", "flow_id": "http", "status": 200, "headers": [], "http_version": "HTTP/2"})
            entity = blob(b'data: {"type":"response.completed","response":{"id":"secret-response"}}\n\n')
            events.append(dict(entity, event="stream_end", flow_id="http", stream="response-entity", complete=False))
            events.append({"event": "flow_error", "flow_id": "http", "phase": {"case": "resume"}})
            (capture / "manifest.jsonl").write_text("\n".join(json.dumps(x) for x in events))
            (capture / "ready.json").write_text('{"port":4444}')
            public, inference, _ = report.route_report(root)
            self.assertEqual(len(inference[1]["effective_body"]["input"]), 2)
            self.assertEqual(len(inference[2]["effective_body"]["input"]), 5)
            self.assertEqual(inference[2]["effective_body"]["input"][2]["type"], "reasoning")
            self.assertEqual(inference[1]["response_output_provenance"], "completed_items_terminal_empty")
            self.assertEqual(inference[1]["response_completed_item_count"], 2)
            self.assertEqual(inference[1]["response_terminal_output_count"], 0)
            self.assertEqual(inference[3]["expansion"], "unresolved_previous_response_id")
            self.assertEqual(public["flow_errors"][0]["classification"], "after_model_terminal_before_http_entity_end")
            self.assertNotIn("secret-", json.dumps(public))
            comparisons = report.compare_inference(inference, [inference[-1]])
            self.assertNotIn("secret-", json.dumps(comparisons))
            self.assertTrue(comparisons[0]["normalization"]["expansion_resolved"])


if __name__ == "__main__":
    unittest.main()
