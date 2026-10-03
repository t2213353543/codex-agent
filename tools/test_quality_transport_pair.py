import base64
import copy
import hashlib
import json
import unittest

import quality_transport_pair as pair


def header(name, value):
    return [name, base64.b64encode(value.encode()).decode()]


def capture(raw=None):
    if raw is None:
        # Deliberately retain unusual escaping, numeric lexemes and whitespace.
        # Model-visible content is opaque; the converter must never round-trip
        # floats or normalize a tools/input payload to produce its WS form.
        raw = br'''{
  "model":"gpt-5.6-sol",
  "input":[{"type":"additional_tools","id":"at-unchanged","role":"developer","tools":[{"type":"function","name":"test","parameters":{"threshold":1.2300e+20}}]},
           {"type":"message","id":"msg-unchanged","role":"user","content":[{"type":"input_text","text":"\u539f\u9898 exact question"}],"internal_chat_message_metadata_passthrough":{"create_time":9007199254740993}}],
  "tool_choice":"auto","parallel_tool_calls":false,"reasoning":{"effort":"medium","context":"all_turns"},
  "store":false,"stream":true,"include":["reasoning.encrypted_content"],"prompt_cache_key":"session-fixed",
  "text":{"verbosity":"low"},
  "client_metadata" : { "session_id":"session-fixed", "thread_id":"thread-fixed", "turn_id":"turn-fixed", "opaque":"\u4fdd\u7559", "x-codex-turn-metadata":"{\"turn_id\":\"turn-fixed\"}" }
}'''
    head = {"schema": "codex-quality-captured-request-v1", "method": "POST", "path": pair.PATH,
        "request_compression": "zstd", "body_sha256": hashlib.sha256(raw).hexdigest(), "body_bytes": len(raw),
        "headers": [header("Accept", "text/event-stream"), header("Content-Type", "application/json"),
            header("User-Agent", "codex_exec/0.155.0 (Synthetic OS; x86_64) unknown"), header("Originator", "codex_exec"),
            header("Version", "0.155.0"), header("session-id", "session-fixed"), header("thread-id", "thread-fixed"),
            header("x-client-request-id", "thread-fixed"), header(pair.LITE_HEADER, "true"),
            header("x-codex-routing-hint", "model=gpt-5.6-sol") ]}
    return raw, head


def altered(change):
    raw, _ = capture()
    body = json.loads(raw)
    change(body)
    return capture(json.dumps(body, separators=(",", ":")).encode())


class TransportPairTests(unittest.TestCase):
    def convert(self, raw=None, head=None, **kwargs):
        if raw is None:
            raw, head = capture()
        return pair.make_websocket_pair(raw, head, request_start_ms=1790000000123, **kwargs)

    def test_original_raw_field_values_and_metadata_are_exactly_preserved(self):
        raw, head = capture()
        before = copy.deepcopy(head)
        output, ws_head, report = self.convert(raw, head)
        expected = json.loads(raw)
        expected["type"] = "response.create"
        expected["client_metadata"].update({pair.LITE_METADATA: "true", pair.TIMING_METADATA: "1790000000123"})
        self.assertEqual(json.loads(output), expected)
        self.assertEqual(head, before)
        self.assertEqual(ws_head["method"], "GET")
        self.assertNotEqual(output, raw)
        self.assertIn(b"1.2300e+20", output)
        self.assertIn(b"9007199254740993", output)
        self.assertIn(br"\u539f\u9898 exact question", output)
        self.assertTrue(all(v["raw_value_unchanged"] for k, v in report["original_fields"].items() if k != "client_metadata"))
        self.assertTrue(all(v["raw_value_unchanged"] for v in report["original_client_metadata_fields"].values()))
        self.assertNotIn("exact question", json.dumps(report))
        self.assertNotIn("session-fixed", json.dumps(report))
        self.assertNotIn("thread-fixed", json.dumps(report))
        self.assertEqual(report["model_calls"], 0)

    def test_native_ws_keeps_stream_and_does_not_add_background_or_previous(self):
        body = json.loads(self.convert()[0])
        self.assertIs(body["stream"], True)
        self.assertIs(body["store"], False)
        for field in ("background", "previous_response_id", "generate", "instructions", "tools"):
            self.assertNotIn(field, body)

    def test_handshake_projection_keeps_identity_and_leaves_upgrade_to_connector(self):
        raw, head = capture()
        for name, value in (("Host", "localhost:1234"), ("Content-Encoding", "zstd"), ("Content-Length", str(len(raw))), ("OpenAI-Beta", "old-beta")):
            head["headers"].append(header(name, value))
        _, out, report = self.convert(raw, head)
        actual = {n.lower():base64.b64decode(v) for n,v in out["headers"]}
        self.assertEqual(actual["openai-beta"], pair.WS_BETA.encode())
        self.assertEqual(actual["session-id"], b"session-fixed")
        self.assertEqual(actual["x-codex-routing-hint"], b"model=gpt-5.6-sol")
        for name in pair.REMOVED_HEADERS | {pair.LITE_HEADER, "authorization", "chatgpt-account-id", "upgrade", "connection", "sec-websocket-key", "sec-websocket-extensions"}:
            self.assertNotIn(name, actual)
        self.assertFalse(report["full_body_bytes_equal"])

    def test_trace_headers_project_to_client_metadata(self):
        raw, head = capture()
        head["headers"] += [header("traceparent", "00-trace-fixture"), header("tracestate", "vendor=fixture")]
        out, ws, _ = self.convert(raw, head)
        metadata = json.loads(out)["client_metadata"]
        self.assertEqual(metadata["ws_request_header_traceparent"], "00-trace-fixture")
        self.assertEqual(metadata["ws_request_header_tracestate"], "vendor=fixture")
        self.assertFalse({"traceparent", "tracestate"} & {n.lower() for n,v in ws["headers"]})

    def test_timing_metrics_capability_is_explicit_not_inferred(self):
        _, head, report = self.convert(include_timing_metrics=True)
        self.assertIn(header("x-responsesapi-include-timing-metrics", "true"), head["headers"])
        self.assertTrue(report["ws_timing_metrics_requested"])

    def test_unknown_http_controls_and_existing_ws_state_are_not_silently_removed(self):
        for name,value in (("background", False),("temperature",.2),("type","response.create"),("previous_response_id",None),("generate",True)):
            with self.subTest(name=name):
                raw, head = altered(lambda body:body.update({name:value}))
                with self.assertRaisesRegex(pair.TransportPairError,"non_native_or_incomplete_http_fields"):
                    self.convert(raw,head)

    def test_go_normalized_empty_instructions_are_rejected_as_a_different_capture_boundary(self):
        for field,value in (("instructions",""),("tools",None)):
            raw,head=altered(lambda body:body.update({field:value}))
            with self.assertRaisesRegex(pair.TransportPairError,"not_native_lite_serialization"):
                self.convert(raw,head)

    def test_mismatched_session_cache_routing_and_lite_cannot_be_paired(self):
        for edit in (lambda b:b.update(prompt_cache_key="different"),lambda b:b["client_metadata"].update(thread_id="different"),
                     lambda b:b["reasoning"].update(context="current_turn"),lambda b:b.update(model="other-model"),
                     lambda b:b["client_metadata"].update({"x-codex-turn-state":"old-server-state"}),lambda b:b.update(stream=False)):
            raw,head=altered(edit)
            with self.assertRaises(pair.TransportPairError):self.convert(raw,head)

    def test_credential_headers_duplicate_names_and_bad_values_rejected(self):
        for row in (header("Authorization","Bearer synthetic-secret"),header("ChatGPT-Account-Id","workspace"),
                    header("SESSION-ID","second"),header("User-Agent","bad\r\nvalue"),["Traceparent","%%%"],
                    header("Upgrade","websocket"),header("Sec-WebSocket-Key","fixture"),header("x-codex-unreviewed","fixture")):
            raw,head=capture();head["headers"].append(row)
            with self.assertRaises(pair.TransportPairError) as result:self.convert(raw,head)
            self.assertNotIn("synthetic-secret",str(result.exception))

    def test_duplicate_nested_fields_and_nonfinite_numbers_rejected(self):
        raw,_=capture()
        for bad in (raw.replace(b'"model":"gpt-5.6-sol"',b'"model":"gpt-5.6-sol","model":"other"'),
                    raw.replace(b'"threshold":1.2300e+20',b'"threshold":1,"threshold":2'),
                    raw.replace(b'1.2300e+20',b'NaN'),b'null',b'[]',b'{',b'\xff'):
            _,head=capture(bad)
            with self.assertRaises(pair.TransportPairError):self.convert(bad,head)

    def test_digest_pins_invalid_timestamp_and_bad_head_fail_closed(self):
        raw,head=capture();head["body_sha256"]="0"*64
        with self.assertRaisesRegex(pair.TransportPairError,"digest_mismatch"):self.convert(raw,head)
        raw,head=capture()
        for value in (None,True,0,-1,1.0,9007199254740992):
            with self.assertRaises(pair.TransportPairError):pair.make_websocket_pair(raw,head,request_start_ms=value)
        with self.assertRaises(pair.TransportPairError):self.convert(raw,None)

    def test_optional_native_fields_preserved_with_matching_routing(self):
        raw,head=altered(lambda body:body.update(service_tier="priority",stream_options={"reasoning_summary_delivery":"sequential_cutoff"},access_programs=["fixture"]))
        head["headers"]=[header(n,"model=gpt-5.6-sol;tier=priority") if n=="x-codex-routing-hint" else [n,v] for n,v in head["headers"]]
        out,_,report=self.convert(raw,head)
        for name in ("service_tier","stream_options","access_programs"):
            self.assertEqual(json.loads(out)[name],json.loads(raw)[name])
            self.assertTrue(report["original_fields"][name]["raw_value_unchanged"])

    def test_numeric_lexemes_cannot_masquerade_as_native_string_fields(self):
        raw, head = altered(lambda body:body.update(model=123))
        head["headers"] = [header(n,"model=123") if n == "x-codex-routing-hint" else [n,v] for n,v in head["headers"]]
        with self.assertRaisesRegex(pair.TransportPairError,"invalid_native_model_controls"):
            self.convert(raw,head)
        raw, head = altered(lambda body:(body.update(prompt_cache_key=123),body["client_metadata"].update(session_id="123")))
        head["headers"] = [header(n,"123") if n == "session-id" else [n,v] for n,v in head["headers"]]
        with self.assertRaisesRegex(pair.TransportPairError,"nonstandard_first_turn_cache_key"):
            self.convert(raw,head)


if __name__ == "__main__":
    unittest.main()
