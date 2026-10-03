import base64
from copy import deepcopy
import hashlib
import json
import unittest

import quality_ws_chain_projection as subject
from quality_cookie_lifecycle_probe import _validate_capture
from quality_transport_pair import _object


def sha(value):
    return hashlib.sha256(value).hexdigest()


def fixture():
    meta = {'session_id': 'synthetic-private-session', 'thread_id': 'synthetic-private-thread',
            'turn_id': '', 'x-codex-installation-id': 'synthetic-private-install', 'x-codex-window-id': 'window',
            subject.LITE: 'true', subject.STAMP: '1790000000000'}
    embedded = {'session_id': meta['session_id'], 'thread_id': meta['thread_id'], 'turn_id': '',
                'installation_id': meta['x-codex-installation-id'], 'window_id': 'window', 'request_kind': 'prewarm',
                'agent_name': '中文😀', 'window_number': 9007199254740993, 'synthetic_extra': 'private-extra'}
    meta[subject.EMBEDDED] = json.dumps(embedded, ensure_ascii=False, separators=(',', ':'))
    warmup = {'type': 'response.create', 'model': 'gpt-5.6-sol', 'store': False, 'stream': True, 'generate': False,
              'input': [{'type': 'additional_tools', 'tools': []}, {'type': 'message', 'role': 'developer', 'content': 'prefix'}],
              'tool_choice': 'auto', 'parallel_tool_calls': False, 'reasoning': {'effort': 'medium', 'context': 'all_turns'},
              'include': ['reasoning.encrypted_content'], 'prompt_cache_key': meta['session_id'], 'client_metadata': meta,
              'private-unknown-field': {'number': 9007199254740993}}
    inference = deepcopy(warmup)
    inference.pop('generate')
    inference['previous_response_id'] = 'synthetic_capture_warmup'
    inference['input'] = [{'type': 'message', 'role': 'user', 'content': '原题 only sentinel'}]
    inference['client_metadata']['turn_id'] = 'current-turn'
    inference['client_metadata'][subject.STAMP] = '1790000000001'
    embedded2 = dict(embedded, request_kind='turn', turn_id='current-turn', tool_namespaces_info={'not-in-header': {'large': 1}})
    inference['client_metadata'][subject.EMBEDDED] = json.dumps(embedded2, ensure_ascii=False, separators=(',', ':'))
    head = {'method': 'GET', 'path': subject.PATH, 'headers': [[name, base64.b64encode(value.encode()).decode()] for name, value in (
        ('User-Agent', 'codex_exec/0.155.0'), ('Originator', 'codex_exec'), ('Version', '0.155.0'),
        ('session-id', meta['session_id']), ('thread-id', meta['thread_id']), ('x-client-request-id', meta['thread_id']),
        ('x-codex-window-id', 'window'), ('OpenAI-Beta', 'responses_websockets=2026-02-06'),
        ('x-codex-turn-metadata', meta[subject.EMBEDDED]))]}
    completion = {'type': 'response.completed', 'response': {'id': 'synthetic_capture_warmup', 'status': 'completed',
        'output': [], 'usage': {'input_tokens': 0, 'output_tokens': 0, 'total_tokens': 0}}}
    return warmup, inference, head, completion


def encoded(value):
    return json.dumps(value, ensure_ascii=False, separators=(',', ':')).encode()


def derive(warmup, inference, head, completion, **kwargs):
    w = warmup if isinstance(warmup, bytes) else encoded(warmup)
    i = inference if isinstance(inference, bytes) else encoded(inference)
    options = dict(expected_warmup_sha256=sha(w), expected_inference_sha256=sha(i),
                   expected_prior='synthetic_capture_warmup', warmup_completion=completion, warmup_output_item_count=0)
    options.update(kwargs)
    return subject.derive_http_comparison(w, i, head, **options)


class WSChainProjectionTests(unittest.TestCase):
    def test_complete_input_and_actual_probe_contract_preserve_unknown_values(self):
        w, i, head, completion = fixture()
        source = encoded(i).replace(b'9007199254740993', b'9007199254740993', 1)
        body, http, report = derive(w, source, head, completion)
        value = json.loads(body)
        self.assertEqual(value['input'], w['input'] + i['input'])
        self.assertNotIn('instructions', value)
        self.assertFalse(set(value) & subject.WS_FIELDS)
        self.assertTrue(report['derived_not_captured_original_http'])
        self.assertEqual(report['derived_input_items'], 3)
        _validate_capture(body, http, None)
        for secret in ('synthetic-private-session', 'synthetic-private-thread', 'synthetic-private-install',
                       'private-unknown-field', 'private-extra', '原题 only sentinel'):
            self.assertNotIn(secret, json.dumps(report, ensure_ascii=False))
        parsed, spans = _object(source)
        _, target_spans = _object(body)
        for name in parsed.keys() - subject.WS_FIELDS - {'input', 'client_metadata'}:
            self.assertEqual(source[slice(*spans[name])], body[slice(*target_spans[name])])

    def test_compatibility_header_is_current_turn_without_inventory(self):
        w, i, head, completion = fixture()
        body, http, report = derive(w, i, head, completion)
        headers = {k.lower(): base64.b64decode(v) for k, v in http['headers']}
        compat = json.loads(headers[subject.EMBEDDED])
        self.assertEqual(compat['request_kind'], 'turn')
        self.assertEqual(compat['turn_id'], 'current-turn')
        self.assertEqual(compat['window_number'], 9007199254740993)
        self.assertEqual(compat['agent_name'], '中文😀')
        self.assertTrue(headers[subject.EMBEDDED].isascii())
        self.assertNotIn('tool_namespaces_info', compat)
        self.assertIn('tool_namespaces_info', json.loads(json.loads(body)['client_metadata'][subject.EMBEDDED]))
        self.assertNotIn('openai-beta', headers)
        self.assertEqual(headers['x-openai-internal-codex-responses-lite'], b'true')
        self.assertTrue(report['http_compatibility_header']['from_warmup_handshake_changed'])

    def test_trace_projects_from_inference_not_handshake(self):
        w, i, head, completion = fixture()
        i['client_metadata']['ws_request_header_traceparent'] = '00-' + 'a' * 32 + '-' + 'b' * 16 + '-01'
        head['headers'].append(['traceparent', base64.b64encode(b'stale-warmup-trace').decode()])
        body, http, _ = derive(w, i, head, completion)
        headers = {k.lower(): base64.b64decode(v) for k, v in http['headers']}
        self.assertEqual(headers['traceparent'].decode(), i['client_metadata']['ws_request_header_traceparent'])
        self.assertNotIn('ws_request_header_traceparent', json.loads(body)['client_metadata'])

    def test_semantic_controls_can_differ_in_object_order_but_not_number_lexemes(self):
        w, i, head, completion = fixture()
        a, b = encoded(w), encoded(i)
        a = a.replace(b'"number":9007199254740993', b'"number":1.2300e+20')
        b = b.replace(b'"number":9007199254740993', b'"number":1.2300e+20')
        body, _, _ = derive(a, b, head, completion)
        self.assertIn(b'1.2300e+20', body)
        with self.assertRaises(subject.WSChainProjectionError):
            derive(a, b.replace(b'1.2300e+20', b'123000000000000000000'), head, completion)

    def test_stable_or_unknown_control_change_rejected(self):
        for key, value in (('model', 'other'), ('instructions', 'new'), ('prompt_cache_key', 'other'), ('private-unknown-field', None)):
            w, i, h, c = fixture()
            i[key] = value
            with self.subTest(key=key), self.assertRaises(subject.WSChainProjectionError):
                derive(w, i, h, c)

    def test_response_scoped_fields_preserve_inference(self):
        w, i, h, c = fixture()
        i['stream_options'] = {'include_usage': True}
        i['access_programs'] = {'synthetic': True}
        body, _, evidence = derive(w, i, h, c)
        self.assertEqual(json.loads(body)['stream_options'], i['stream_options'])
        self.assertEqual(evidence['response_scoped_differences'], ['stream_options', 'access_programs'])

    def test_completion_nonempty_output_or_item_event_never_silently_omitted(self):
        w, i, h, c = fixture()
        bad = deepcopy(c)
        bad['response']['output'] = [{'type': 'reasoning'}]
        for completion, count in ((bad, 0), (c, 1)):
            with self.assertRaises(subject.WSChainProjectionError):
                derive(w, i, h, completion, warmup_output_item_count=count)

    def test_completion_wrong_id_or_nonzero_or_missing_usage_rejected(self):
        for field, value in (('id', 'unrelated'), ('usage', {'input_tokens': 0, 'output_tokens': 0}),
                             ('usage', {'input_tokens': 0, 'output_tokens': 1, 'total_tokens': 1})):
            w, i, h, c = fixture()
            c['response'][field] = value
            with self.assertRaises(subject.WSChainProjectionError):
                derive(w, i, h, c)

    def test_duplicate_root_nested_and_metadata_rejected(self):
        w, i, h, c = fixture()
        for raw in (encoded(i).replace(b'"stream":true', b'"stream":true,"stream":true'),
                    encoded(i).replace(b'"effort":"medium"', b'"effort":"medium","effort":"medium"')):
            with self.assertRaises(subject.WSChainProjectionError):
                derive(w, raw, h, c)
        i['client_metadata'][subject.EMBEDDED] = '{"session_id":"a","session_id":"b"}'
        with self.assertRaises(subject.WSChainProjectionError):
            derive(w, i, h, c)

    def test_bad_prior_generate_and_frame_pin_rejected(self):
        for key, value in (('previous_response_id', 'other'), ('generate', False), ('generate', None), ('store', True)):
            w, i, h, c = fixture()
            i[key] = value
            with self.assertRaises(subject.WSChainProjectionError):
                derive(w, i, h, c)
        with self.assertRaises(subject.WSChainProjectionError):
            derive(*fixture(), expected_warmup_sha256='0' * 64)

    def test_identity_mismatch_server_state_and_duplicate_headers_rejected(self):
        for scenario in ('session', 'state', 'duplicate', 'auth'):
            w, i, h, c = fixture()
            if scenario == 'session':
                i['client_metadata']['session_id'] = 'other'
            elif scenario == 'state':
                i['client_metadata']['x-codex-turn-state'] = 'server-state'
            elif scenario == 'duplicate':
                h['headers'].append(h['headers'][0])
            else:
                h['headers'].append(['Authorization', base64.b64encode(b'Bearer private').decode()])
            with self.subTest(scenario=scenario), self.assertRaises(subject.WSChainProjectionError):
                derive(w, i, h, c)

    def test_unknown_ws_header_projection_rejected_unknown_body_preserved(self):
        w, i, h, c = fixture()
        i['client_metadata']['ws_request_header_future'] = 'unknown'
        with self.assertRaises(subject.WSChainProjectionError):
            derive(w, i, h, c)

    def test_handshake_embedded_state_must_match_original_warmup(self):
        w, i, h, c = fixture()
        old = json.loads(w['client_metadata'][subject.EMBEDDED])
        old['request_kind'] = 'turn'
        h['headers'][-1][1] = base64.b64encode(encoded(old)).decode()
        with self.assertRaisesRegex(subject.WSChainProjectionError, 'handshake_warmup_metadata_mismatch'):
            derive(w, i, h, c)

    def test_ws_timing_request_header_not_copied_to_http(self):
        w, i, h, c = fixture()
        h['headers'].append(['x-responsesapi-include-timing-metrics', base64.b64encode(b'true').decode()])
        _, http, _ = derive(w, i, h, c)
        self.assertNotIn('x-responsesapi-include-timing-metrics', {k.lower() for k, _ in http['headers']})

    def test_compatibility_extra_nonstring_and_bool_as_number_rejected(self):
        for field, bad in (('synthetic_extra', {}), ('window_number', True)):
            w, i, h, c = fixture()
            metadata = json.loads(i['client_metadata'][subject.EMBEDDED])
            metadata[field] = bad
            i['client_metadata'][subject.EMBEDDED] = json.dumps(metadata)
            with self.subTest(field=field), self.assertRaises(subject.WSChainProjectionError):
                derive(w, i, h, c)


if __name__ == '__main__':
    unittest.main(verbosity=2)
