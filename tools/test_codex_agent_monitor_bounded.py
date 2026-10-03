"""Bounded observer semantics; optional tests execute synthetic SQL only."""
from copy import deepcopy
import json
import os
import subprocess
import unittest
from unittest.mock import patch

import codex_agent_live_monitor as monitor


def totals():
    return dict.fromkeys(('successful_usage_unreported', 'input_tokens', 'output_tokens',
                         'inference_requests', 'cached_tokens', 'cache_write_tokens',
                         'cache_read_samples', 'cache_write_samples',
                         'cache_read_unknown_requests', 'control_requests'), 0)


class BoundedMonitorTests(unittest.TestCase):
    def report(self, excluded):
        data = {'captured_at': '2026-09-30T00:10:00Z', 'last_id': 9000, 'errors': [],
                'windows': [{'minutes': minutes, 'totals': totals()} for minutes in (1, 20, 60, 360)],
                'sampling': {'rows': 5000, 'row_limit': 5000, 'truncated': True,
                             'first_excluded_at': excluded, 'diagnostics_truncated': 0}}
        data['windows'][0]['totals'].update(inference_requests=5000, input_tokens=10000, output_tokens=2000)
        return data

    def test_partial_minute_never_reports_complete_rpm_tpm(self):
        with patch.object(monitor, 'read_runtime', return_value=({}, {})), \
             patch.object(monitor, 'sql', return_value=self.report('2026-09-30T00:09:30Z')), \
             patch.object(monitor, 'read_ingress', return_value={}):
            result = monitor.snapshot(10)
        self.assertIsNone(result['realtime']['rpm'])
        self.assertIsNone(result['realtime']['tpm'])
        self.assertFalse(result['realtime']['window_complete'])
        self.assertFalse(result['new_errors_complete'])
        self.assertFalse(result['error_windows_complete'])
        self.assertEqual(result['realtime']['observed_inference_requests'], 5000)
        self.assertEqual(result['realtime']['reported_tpm'], 12000)
        self.assertTrue(all(not window['complete'] for window in result['windows']))

    def test_recent_minute_can_be_complete_while_long_windows_are_samples(self):
        with patch.object(monitor, 'read_runtime', return_value=({}, {})), \
             patch.object(monitor, 'sql', return_value=self.report('2026-09-30T00:08:59Z')), \
             patch.object(monitor, 'read_ingress', return_value={}):
            result = monitor.snapshot(10)
        self.assertEqual(result['realtime']['rpm'], 5000)
        self.assertEqual(result['realtime']['tpm'], 12000)
        self.assertEqual([row['complete'] for row in result['windows']], [True, False, False, False])

    def test_truncated_diagnostic_is_not_misclassified_as_a_known_failure(self):
        data = self.report('2026-09-30T00:08:59Z')
        data['sampling']['diagnostics_truncated'] = 1
        data['errors'] = [{'id': 50, 'created_at': '2026-09-30T00:09:59Z', 'request_type': 'http_sse',
                           'http_code': '429', 'error_source': 'upstream', 'error_phase': 'read',
                           'error': 'rate limit exceeded', 'diagnostic_truncated': True}]
        with patch.object(monitor, 'read_runtime', return_value=({}, {})), \
             patch.object(monitor, 'sql', return_value=data), patch.object(monitor, 'read_ingress', return_value={}):
            result = monitor.snapshot(10)
        self.assertEqual(result['new_errors'][0]['category'], 'truncated_diagnostic')
        self.assertFalse(result['new_errors'][0]['classification_complete'])

    def test_read_budget_limits_memory_parallelism_and_temporary_files(self):
        with patch.object(monitor.subprocess, 'run', return_value=subprocess.CompletedProcess([], 0, b'{}', b'')) as run:
            monitor.sql('SELECT json_build_object()')
        args = run.call_args.args[0]
        options = next(arg for arg in args if arg.startswith('PGOPTIONS='))
        for setting in ('default_transaction_read_only=on', 'statement_timeout=10000', 'work_mem=4MB',
                        'hash_mem_multiplier=1', 'temp_file_limit=128MB', 'max_parallel_workers_per_gather=0', 'jit=off'):
            self.assertIn(setting, options)


@unittest.skipUnless(os.environ.get('CODEX_MONITOR_TEST_POSTGRES') == '1', 'requires isolated SQL fixture opt-in')
class BoundedMonitorSQLTests(unittest.TestCase):
    def query(self, fixture):
        query = monitor.snapshot_query({})
        return monitor.sql('WITH request_logs AS MATERIALIZED (' + fixture + '),' + query[len('WITH '):])

    def test_row_limit_happens_before_json_decode_and_window_counts_are_exact_for_sample(self):
        # The first excluded record is deliberately invalid for numeric casts.
        # If JSON decoding is moved before LIMIT, this query must fail.
        fixture = """SELECT i::bigint id,now()-i*interval '1 second' created_at,
 'account'::text account_ref,'ok'::text status,'fixture'::text model,
 jsonb_build_object('endpoint','responses','input_tokens',CASE WHEN i>5000 THEN 'invalid' ELSE '20' END,
 'output_tokens',2,'cached_tokens',10,'usage_known',true,'cached_tokens_known',true) record
 FROM generate_series(1,6002)i"""
        result = self.query(fixture)
        self.assertEqual(result['sampling']['rows'], 5000)
        self.assertTrue(result['sampling']['truncated'])
        windows = {item['minutes']: item['totals'] for item in result['windows']}
        self.assertEqual(windows[1]['requests'], 60)
        self.assertEqual(windows[20]['requests'], 1200)
        self.assertEqual(windows[360]['requests'], 5000)
        self.assertEqual(windows[360]['input_tokens'], 100000)
        self.assertEqual(windows[360]['cache_percent'], 50)
        self.assertEqual(result['last_id'], 6002)

    def test_error_arrays_are_bounded_before_final_json_aggregation(self):
        fixture = """SELECT 1::bigint id,now()-interval '1 second' created_at,
 'account'::text account_ref,'error'::text status,'fixture'::text model,
 jsonb_build_object('endpoint','responses','error',repeat('large diagnostic ',10000),
 'attempts',jsonb_build_array(jsonb_build_object('error_code','old','body',repeat('private',10000)),jsonb_build_object('error_code','rateLimitExceeded','body',repeat('private',10000))),
 'tool_policy',jsonb_build_object('private',repeat('private',10000)),
 'final_upstream_error_code','rateLimitExceeded','final_upstream_error_source','upstream') record"""
        result = self.query(fixture)
        row = result['errors'][0]
        self.assertEqual(len(row['error']), monitor.MAX_ERROR_CHARS)
        self.assertEqual(row['attempts'], [{'error_code': 'rateLimitExceeded'}])
        self.assertIsNone(row['tool_policy'])
        self.assertTrue(row['diagnostic_truncated'])
        self.assertLess(len(json.dumps(result)), 25000)
        self.assertNotIn('private', json.dumps(result))

    def test_untruncated_sample_preserves_all_aggregate_semantics(self):
        records = [
            {'endpoint': 'responses', 'status': 'ok', 'input_tokens': 20, 'cached_tokens': 10, 'cached_tokens_known': True, 'cache_write_tokens': 5},
            {'endpoint': 'models', 'status': 'error', 'input_tokens': 1000, 'cached_tokens': 1000},
            {'endpoint': 'responses', 'status': 'ok'},
            {'endpoint': 'responses', 'status': 'ok', 'websocket_warmup': True},
        ]
        raw = json.dumps(records).replace("'", "''")
        fixture = "SELECT ordinality::bigint id,now()-interval '1 second' created_at,''::text account_ref,value->>'status' status,'fixture'::text model,value record FROM jsonb_array_elements('"+raw+"'::jsonb) WITH ORDINALITY"
        result = self.query(fixture)
        baseline = monitor.sql("WITH fixture AS (SELECT value record FROM jsonb_array_elements('"+raw+"'::jsonb)), source AS (SELECT record,record->>'status' status,("+monitor.INFERENCE_RECORD_SQL+") is_inference,coalesce((record->>'input_tokens')::bigint,0) input_tokens,coalesce((record->>'output_tokens')::bigint,0) output_tokens,coalesce((record->>'cached_tokens')::bigint,0) cached_tokens FROM fixture) SELECT row_to_json(t) FROM (SELECT "+monitor.AGGREGATE_SQL+" FROM source)t")
        self.assertFalse(result['sampling']['truncated'])
        for window in result['windows']:
            self.assertEqual(window['totals'], baseline)


if __name__ == '__main__':
    unittest.main(verbosity=2)
