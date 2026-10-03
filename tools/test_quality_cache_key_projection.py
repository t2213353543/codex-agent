"""Single-value byte preservation, strict inputs and offline dispatch pin tests."""
import base64
import copy
import hashlib
import json
import unittest
from unittest import mock

import quality_cache_key_projection as projection


RAW = br'''{
 "model":"gpt-5.6-sol", "stream":true, "store":false,
 "input":[{"role":"user","content":"\u539f\u9898 private prompt","id":"private-item-id","n":9007199254740993,"decimal":1.234567890123456789e+20}],
 "prompt_cache_key" : "private-session",
 "client_metadata":{"session_id":"private-session","thread_id":"private-thread","turn_id":"private-turn","other":{"prompt_cache_key":"private-nested-key"}},
 "tools":[{"name":"private-tool","schema":{"value":9007199254740995}}],
 "private-field-name":{"private-property":"private-value","numeric":1e1000},
 "text":{"format":{"type":"json_schema","schema":{"properties":{"prompt_cache_key":{"const":"private-session"}}}}}
}'''


def sha(raw):
    return hashlib.sha256(raw).hexdigest()


def head(body=RAW):
    return {"schema": projection.CAPTURE_SCHEMA, "method": "POST", "path": "/backend-api/codex/responses",
        "headers": [[key, base64.b64encode(value.encode()).decode()] for key, value in (
            ("User-Agent", "codex_exec/0.155.0 (TestOS 1; x86_64) test"), ("Originator", "codex_exec"),
            ("Accept", "text/event-stream"), ("Content-Type", "application/json"),
            ("Session-Id", "private-session"), ("Thread-Id", "private-thread"))],
        "request_compression": "zstd", "body_sha256": sha(body), "body_bytes": len(body)}


class ProjectionTests(unittest.TestCase):
    def test_only_cache_value_span_changes_preserving_all_opaque_bytes(self):
        captured = head()
        before = copy.deepcopy(captured)
        new = "fresh-experimental-override"
        body, changed_head, evidence = projection.project_cache_key(RAW, captured, new)
        expected = RAW.replace(b'"prompt_cache_key" : "private-session"', b'"prompt_cache_key" : "fresh-experimental-override"', 1)
        self.assertEqual(body, expected)
        self.assertEqual(captured, before)
        self.assertEqual(changed_head["headers"], before["headers"])
        self.assertEqual(changed_head["body_sha256"], sha(body))
        self.assertEqual(changed_head["body_bytes"], len(body))
        self.assertIsNot(changed_head, captured)
        self.assertIsNot(changed_head["headers"], captured["headers"])
        for key in captured.keys()-{"body_sha256", "body_bytes"}:
            self.assertEqual(captured[key], changed_head[key])
        self.assertTrue(evidence["original_cache_key_matches_metadata_session"])
        self.assertFalse(evidence["projected_cache_key_matches_metadata_session"])
        self.assertTrue(evidence["non_cache_fields_byte_identical"])
        self.assertEqual([field["name"] for field in evidence["fields"] if not field["raw_value_unchanged"]], ["prompt_cache_key"])
        for number in (b'9007199254740993', b'1.234567890123456789e+20', b'9007199254740995', b'1e1000'):
            self.assertIn(number, body)
        self.assertIn(br'\u539f\u9898', body)
        self.assertEqual(evidence["model_calls"], 0)
        self.assertEqual(evidence["network_calls"], 0)
        for secret in ("private-session", "private prompt", "private-item-id", "private-nested-key", "private-thread", "private-turn",
                       "private-tool", "private-field-name", "private-property", "private-value", new):
            self.assertNotIn(secret, json.dumps(evidence))

    def test_escaped_field_name_and_value_target_decoded_exact_name(self):
        raw = RAW.replace(b'"prompt_cache_key" : "private-session"', br'"pr\u006fmpt_cache_key" : "private-\u0073ession"')
        body, _, evidence = projection.project_cache_key(raw, head(raw), "override")
        self.assertEqual(body, raw.replace(br'"private-\u0073ession"', b'"override"', 1))
        self.assertIn(br'"pr\u006fmpt_cache_key"', body)
        self.assertTrue(evidence["original_cache_key_matches_metadata_session"])
        self.assertEqual(evidence["original_key"]["utf8_sha256"], sha(b"private-session"))

    def test_unicode_before_key_correct_byte_offsets_and_suffix_preserved(self):
        raw = RAW.replace(br'\u539f\u9898', "原题非ASCII🙂".encode())
        changed, _, evidence = projection.project_cache_key(raw, head(raw), "second")
        a = evidence["cache_value_span"]
        self.assertEqual(raw[a["original_start"]:a["original_end"]], b'"private-session"')
        self.assertEqual(changed[a["projected_start"]:a["projected_end"]], b'"second"')
        self.assertEqual(raw[:a["original_start"]], changed[:a["projected_start"]])
        self.assertEqual(raw[a["original_end"]:], changed[a["projected_end"]:])
        self.assertEqual(evidence["prefix"]["original_sha256"], evidence["prefix"]["projected_sha256"])
        self.assertEqual(evidence["suffix"]["original_sha256"], evidence["suffix"]["projected_sha256"])

    def test_override_need_not_match_session_uuid_or_original_key_shape(self):
        raw = RAW.replace(b'"prompt_cache_key" : "private-session"', b'"prompt_cache_key" : "existing-override"')
        for new in ("x", "namespace:root/agent?part=two", "a"*256, 'quotes"and\\slashes', "a b"):
            changed, _, evidence = projection.project_cache_key(raw, head(raw), new)
            parsed, _ = projection._object(changed)
            self.assertEqual(parsed["prompt_cache_key"], new)
            self.assertFalse(evidence["original_cache_key_matches_metadata_session"])
            self.assertFalse(evidence["projected_cache_key_matches_metadata_session"])
            self.assertEqual(parsed["client_metadata"]["session_id"], "private-session")

    def test_same_key_is_valid_and_reports_actual_byte_change(self):
        identical, next_head, evidence = projection.project_cache_key(RAW, head(), "private-session")
        self.assertEqual(identical, RAW)
        self.assertFalse(evidence["body_bytes_changed"])
        escaped = RAW.replace(b'"prompt_cache_key" : "private-session"', br'"prompt_cache_key" : "private-\u0073ession"')
        changed, _, evidence = projection.project_cache_key(escaped, head(escaped), "private-session")
        self.assertNotEqual(changed, escaped)
        self.assertEqual(evidence["original_key"]["utf8_sha256"], evidence["projected_key"]["utf8_sha256"])
        self.assertTrue(evidence["body_bytes_changed"])

    def test_missing_or_wrong_case_key_invalid_model_and_nonfull_inputs_reject(self):
        cases = [RAW.replace(b'"prompt_cache_key" : "private-session",', b''),
                 RAW.replace(b'"prompt_cache_key" :', b'"Prompt_Cache_Key" :'),
                 RAW.replace(b'"prompt_cache_key" : "private-session"', b'"prompt_cache_key" : 5'),
                 RAW.replace(b'"prompt_cache_key" : "private-session"', b'"prompt_cache_key" : null'),
                 RAW.replace(b'"prompt_cache_key" : "private-session"', b'"prompt_cache_key" : ""'),
                 RAW.replace(b'"model":"gpt-5.6-sol"', b'"model":true'),
                 RAW.replace(b'"model":"gpt-5.6-sol"', b'"model":1'),
                 RAW.replace(b'"stream":true', b'"stream":1'), RAW.replace(b'"store":false', b'"store":0'),
                 RAW.replace(b'"stream":true', b'"stream":false'), RAW.replace(b'"store":false', b'"store":true'),
                 RAW.replace(b'"input":[', b'"Input":['), RAW[:-1]+b',"previous_response_id":null}',
                 RAW[:-1]+b',"previous_response_id":"old"}']
        for raw in cases:
            with self.subTest(body_sha=sha(raw)), self.assertRaises(projection.CacheKeyProjectionError):
                projection.project_cache_key(raw, head(raw), "new")

    def test_duplicate_any_depth_even_escaped_equivalent_is_rejected(self):
        cases = [RAW[:-1]+b',"prompt_cache_key":"second"}', RAW[:-1]+br',"pr\u006fmpt_cache_key":"second"}',
                 RAW.replace(b'"other":{"prompt_cache_key":"private-nested-key"}', b'"other":{"x":1,"x":2}'),
                 RAW.replace(b'"tools":[', b'"unknown":[{"nested":{"k":1,"k":2}}],"tools":[')]
        for raw in cases:
            with self.assertRaisesRegex(projection.CacheKeyProjectionError, "duplicate_json_member"):
                projection.project_cache_key(raw, head(raw), "new")

    def test_unknown_case_variants_preserved_and_not_interpreted_as_protocol_key(self):
        raw = RAW[:-1]+b',"Prompt_Cache_Key":{"unknown":9007199254740997},"Input":"opaque"}'
        changed, _, evidence = projection.project_cache_key(raw, head(raw), "override")
        self.assertIn(b'"Prompt_Cache_Key":{"unknown":9007199254740997}', changed)
        self.assertIn(b'"Input":"opaque"', changed)
        self.assertTrue(evidence["non_cache_fields_byte_identical"])
        self.assertNotIn("Prompt_Cache_Key", json.dumps(evidence))

    def test_new_key_invalid_types_control_nonascii_and_bound_do_not_leak(self):
        for invalid in (None, 1, True, [], {}, "", "x"*257, "private\nvalue", "private\x7fvalue", "中文", "\ud800"):
            with self.assertRaises(projection.CacheKeyProjectionError) as error:
                projection.project_cache_key(RAW, head(), invalid)
            self.assertNotIn("private", str(error.exception))

    def test_header_pin_mismatch_type_mutation_and_secret_headers_reject(self):
        for change in ({"body_sha256":"b"*64}, {"body_bytes":True}, {"body_bytes":len(RAW)+1}, {"method":"GET"},
                       {"path":"https://example.test"}, {"request_compression":"gzip"}, {"schema":"unknown"}, {"extra":1}):
            cap=head();cap.update(change)
            with self.assertRaises(projection.CacheKeyProjectionError):
                projection.project_cache_key(RAW,cap,"new")
        for name,value in (("Authorization","Bearer private-secret"),("Cookie","private-cookie"),("Content-Length","100"),
                           ("x-codex-secret","Bearer private-secret"),("Accept","text/event-stream")):
            cap=head();cap['headers'].append([name,base64.b64encode(value.encode()).decode()])
            with self.assertRaises(projection.CacheKeyProjectionError) as error:
                projection.project_cache_key(RAW,cap,"new")
            self.assertNotIn("private",str(error.exception))

    def test_malformed_json_utf8_surrogates_and_limits_are_fixed_errors(self):
        for raw in (b'',b'[]',RAW+b'{}',RAW.replace(b'1e1000',b'NaN'),RAW.replace(b'1e1000',b'Infinity'),
                    RAW.replace(b'private-session',b'\xff',1),RAW.replace(b'private-session',br'\ud800',1),b'['*70+b']'*70):
            with self.assertRaises(projection.CacheKeyProjectionError):
                projection.project_cache_key(raw,head(raw),'new')
        with mock.patch.object(projection,'MAX_BODY',len(RAW)):
            with self.assertRaisesRegex(projection.CacheKeyProjectionError,'projected_body_limit'):
                projection.project_cache_key(RAW,head(),'x'*256)
        with mock.patch.object(projection,'MAX_NODES',5):
            with self.assertRaisesRegex(projection.CacheKeyProjectionError,'json_structure_limit'):
                projection.project_cache_key(RAW,head(),'x')

    def test_pin_checks_decoded_key_not_json_spelling_and_never_rewrites(self):
        raw=RAW.replace(b'"prompt_cache_key" : "private-session"',br'"prompt_cache_key" : "private-\u0073ession"')
        evidence=projection.verify_cache_key_pin(raw,head(raw),sha(b'private-session'))
        self.assertTrue(evidence['cache_key_pin_matched'])
        self.assertEqual(evidence['body_sha256'],sha(raw))
        for expected in (sha(b'"private-session"'),sha(b'wrong'),None,True,'notahash'):
            with self.assertRaises(projection.CacheKeyProjectionError):
                projection.verify_cache_key_pin(raw,head(raw),expected)
        projected,cap,_=projection.project_cache_key(raw,head(raw),'fresh')
        with self.assertRaisesRegex(projection.CacheKeyProjectionError,'cache_key_pin_mismatch'):
            projection.verify_cache_key_pin(projected,cap,sha(b'private-session'))
        self.assertTrue(projection.verify_cache_key_pin(projected,cap,sha(b'fresh'))['cache_key_pin_matched'])

    def test_existing_http_lifecycle_accepts_override_unequal_to_session(self):
        from quality_cookie_lifecycle_probe import _validate_capture
        # Its strict numeric parser rejects extreme exponents independently of
        # cache policy; use the same exact ordinary big integer body here.
        raw=RAW.replace(b'1e1000',b'1e100')
        projected,cap,evidence=projection.project_cache_key(raw,head(raw),'explicit-override')
        identity=_validate_capture(projected,cap,None)
        self.assertEqual(identity['originator'],'codex_exec')
        self.assertFalse(evidence['projected_cache_key_matches_metadata_session'])


if __name__=='__main__':
    unittest.main()
