import base64
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

import quality_native_ipc_capture as capture
import quality_nested_session_evidence as nested


def evidence(raw):
    return nested.summarize_nested_session_evidence(raw, number_factory=capture.ExactJSONNumber, value_summary=capture._value_summary)


class NestedSessionEvidenceTests(unittest.TestCase):
    def test_launcher_rejects_changed_nested_dependency_before_helper_start(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            tool = root / 'quality_native_ipc_capture.py'
            tool.write_bytes(Path(capture.__file__).read_bytes())
            dependency = root / 'quality_nested_session_evidence.py'
            dependency.write_bytes(Path(nested.__file__).read_bytes())
            helper = root / 'synthetic-helper'
            helper.write_bytes(b'synthetic helper must never be executed')
            launcher = root / 'launcher'
            with patch.object(capture, '__file__', str(tool)):
                capture.install_launcher(helper, launcher, root / 'output', str(Path(Path(sys.executable).anchor) / 'python3'))
            dependency.write_bytes(b'raise AssertionError("must not execute modified dependency")')
            result = subprocess.run([sys.executable, str(launcher)], input=b'', stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=5)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn(b'capture_dependency_digest_mismatch', result.stderr)
            self.assertEqual(result.stdout, b'')

    def test_exact_number_hashes_match_parent_and_private_values_never_leave(self):
        raw = b'{"session_id":"synthetic-private-session","private-name":{"v":9007199254740993},"turn_started_at_unix_ms":1.2300e+20}'
        actual = evidence(raw)
        obj = json.loads(raw, parse_int=capture.ExactJSONNumber, parse_float=capture.ExactJSONNumber)
        self.assertTrue(actual['complete'])
        self.assertEqual(actual['content_sha256'], capture._value_summary(obj)['content_sha256'])
        self.assertNotIn('synthetic-private-session', json.dumps(actual))
        self.assertNotIn('private-name', json.dumps(actual))
        self.assertEqual(actual['fields']['turn_started_at_unix_ms']['json_type'], 'number')
        self.assertNotEqual(actual['fields']['turn_started_at_unix_ms']['content_sha256'], evidence(raw.replace(b'1.2300e+20', b'123000000000000000000'))['fields']['turn_started_at_unix_ms']['content_sha256'])

    def test_real_body_and_header_entrypoints_use_same_canonical_evidence(self):
        embedded = '{"session_id":"synthetic-session","thread_id":"synthetic-thread","installation_id":"private-install","big":9007199254740993}'
        body = json.dumps({'model': 'test', 'input': [], 'client_metadata': {'x-codex-turn-metadata': embedded}}).encode()
        result = capture.summarize_body(body, [])
        head, _ = capture.summarize_head({'headers': [['X-Codex-Turn-Metadata', base64.b64encode(embedded.encode()).decode()]]})
        self.assertEqual(result['client_metadata_turn_metadata_summary'], head['turn_metadata_summary'])
        self.assertTrue(result['body_summary_complete'])
        self.assertNotIn('private-install', json.dumps(head))

    def test_session_changes_leave_unrelated_fields_identical(self):
        a = evidence(b'{"session_id":"old","thread_id":"thread","installation_id":"install","turn_id":"turn","window_id":"window","request_id":"request"}')
        b = evidence(b'{"session_id":"new","thread_id":"other-thread","installation_id":"install","turn_id":"turn","window_id":"window","request_id":"request"}')
        changed = [name for name in a['fields'] if a['fields'][name] != b['fields'][name]]
        self.assertEqual(changed, ['session_id', 'thread_id'])

    def test_duplicate_and_escaped_duplicate_rejected_without_partial_fields(self):
        for raw in (b'{"session_id":"a","session_\\u0069d":"b"}', b'{"opaque":{"a":1,"a":2}}'):
            result = evidence(raw)
            self.assertFalse(result['complete'])
            self.assertEqual(result['error'], 'duplicate_json_member')
            self.assertEqual(result['fields'], {})

    def test_invalid_types_and_encoding_are_not_absent(self):
        for raw in (b'null', b'[]', b'1', b'"s"', b'{"a":NaN}', b'\xff', b'{"a":"\\ud800"}'):
            result = evidence(raw)
            self.assertFalse(result['complete'])
            self.assertEqual(result['parse_status'], 'invalid')
            self.assertTrue(result['present'])
        for value in (None, True, 1, [], {}):
            body = json.dumps({'client_metadata': {'x-codex-turn-metadata': value}}).encode()
            result = capture.summarize_body(body, [])['client_metadata_turn_metadata_summary']
            self.assertTrue(result['present'])
            self.assertFalse(result['complete'])

    def test_size_and_depth_are_bounded(self):
        with patch.object(nested, 'MAX_BYTES', 5):
            self.assertEqual(evidence(b'{"a":1}')['error'], 'nested_metadata_size_limit')
        raw = b'{"a":' + b'[' * 34 + b'0' + b']' * 34 + b'}'
        self.assertEqual(evidence(raw)['error'], 'nested_metadata_structure_limit')

    def test_duplicate_header_is_explicitly_incomplete(self):
        encoded = base64.b64encode(b'{"session_id":"private"}').decode()
        head, _ = capture.summarize_head({'headers': [['X-Codex-Turn-Metadata', encoded], ['x-codex-turn-metadata', encoded]]})
        self.assertFalse(head['turn_metadata_summary']['complete'])
        self.assertEqual(head['turn_metadata_summary']['error'], 'duplicate_turn_metadata_header')


if __name__ == '__main__':
    unittest.main(verbosity=2)
