"""Offline semantic boundaries; never network or production."""
import json
from pathlib import Path
import tempfile
import unittest

import quality_failure_summary as q


NOW = 1790246400


class FailureEvidence(unittest.TestCase):
    def summary(self, **row):
        return q.summarize_failure(row, now=NOW)

    def test_usage_429_and_answer_error_words_are_not_failures(self):
        row = self.summary(protocol_success=True, response_usage={'output_tokens': 429},
            output_text='HTTP 429 token_revoked error is the answer', message='429 Too Many Requests')
        self.assertEqual(row['category'], 'completed')
        self.assertEqual(row['http_statuses'], [])

    def test_only_status_429_cannot_give_recovery_deadline(self):
        row = self.summary(http_status=429)
        self.assertEqual(row['category'], 'rate_limited')
        self.assertTrue(row['missing_recovery_evidence'])
        self.assertIsNone(row['not_before_utc'])

    def test_retry_after_delta_from_observed_time_not_analysis_time(self):
        row = self.summary(http_status=429, started_at=NOW-10,
                           response_headers={'Retry-After': '60'})
        self.assertEqual(row['not_before_utc'], q.iso(NOW+50))

    def test_retry_after_without_observation_cannot_fabricate_deadline(self):
        row = self.summary(http_status=429, response_headers={'Retry-After': '60'})
        self.assertIsNone(row['not_before_utc'])

    def test_retry_after_http_date(self):
        row = self.summary(http_status=503, response_headers={'Retry-After': 'Thu, 24 Sep 2026 10:41:00 GMT'})
        self.assertEqual(row['not_before_utc'], '2026-09-24T10:41:00Z')

    def test_quota_error_is_not_temporary_429(self):
        for marker in sorted(q.QUOTA_CODES):
            with self.subTest(marker=marker):
                row = self.summary(http_status=429, error={'code': marker},
                    response_headers={'Retry-After': '60'}, started_at=NOW)
                self.assertEqual(row['category'], 'quota_exhausted')
                self.assertEqual(row['wait_state'], 'quota_or_entitlement_action_required')
                self.assertIsNone(row['not_before_utc'])

    def test_codex_error_type_and_epoch_resets_at(self):
        row = self.summary(http_status=429, body=json.dumps({'error': {
            'type': 'usage_limit_reached', 'resets_at': NOW+3600}}))
        self.assertEqual(row['not_before_utc'], q.iso(NOW+3600))

    def test_expired_deadline_requires_recheck_not_healthy(self):
        row = self.summary(http_status=429, error={'resets_at': NOW-1})
        self.assertEqual(row['wait_state'], 'recorded_deadline_elapsed_recheck_required')
        self.assertIsNone(row['not_before_utc'])

    def test_codex_header_uses_active_limit_family_and_exhausted_window(self):
        row = self.summary(http_status=429, response_headers={
            'X-Codex-Active-Limit': 'codex_other',
            'X-Codex-Primary-Used-Percent': '100', 'X-Codex-Primary-Reset-At': str(NOW+9000),
            'X-Codex-Other-Primary-Used-Percent': '100',
            'X-Codex-Other-Primary-Reset-At': str(NOW+600),
            'X-Codex-Other-Secondary-Used-Percent': '10',
            'X-Codex-Other-Secondary-Reset-At': str(NOW+5000)})
        self.assertEqual(row['not_before_utc'], q.iso(NOW+600))
        self.assertEqual(len(row['rate_limit_windows']), 3)

    def test_empty_incorrect_or_nonfinite_headers_are_not_quota(self):
        row = self.summary(http_status=429, response_headers={
            'X-Codex-Primary-Used-Percent-Wrong': '100',
            'X-Codex-Primary-Used-Percent': 'NaN',
            'X-Codex-Primary-Reset-At': 'garbage', 'Retry-After': '-100'})
        self.assertEqual(row['rate_limit_windows'], [])
        self.assertIsNone(row['not_before_utc'])

    def test_conflicting_duplicate_retry_headers_unknown(self):
        row = self.summary(http_status=429, started_at=NOW,
                           response_headers={'Retry-After': ['60', '3600']})
        self.assertIsNone(row['not_before_utc'])

    def test_codex_rate_limits_event_retains_window_without_claiming_error(self):
        row = self.summary(type='codex.rate_limits', metered_limit_name='codex_other',
            rate_limits={'primary': {'used_percent': 100, 'window_minutes': 300, 'reset_at': NOW+60}})
        self.assertEqual(row['category'], 'unknown')
        self.assertEqual(row['rate_limit_windows'][0]['limit_id'], 'codex_other')
        self.assertEqual(row['rate_limit_windows'][0]['reset_at'], NOW+60)

    def test_auth_and_forbidden_are_separate(self):
        self.assertEqual(self.summary(http_status=401)['category'], 'authentication')
        row = self.summary(http_status=403, error={'message': 'token_revoked 429'})
        self.assertEqual(row['category'], 'forbidden')
        self.assertEqual(row['error_codes'], [])

    def test_error_code_after_200_sse_is_failure(self):
        row = self.summary(http_status=200, safe_errors=[{'error': {'code': 'rate_limit_exceeded'}}])
        self.assertEqual(row['category'], 'rate_limited')

    def test_conflicting_explicit_errors_never_cherry_pick(self):
        row = self.summary(http_status=403, error={'http_status': 429})
        self.assertEqual(row['category'], 'conflicting_failure_evidence')

    def test_cli_fixed_diagnostic_only_in_error_event(self):
        for message, expected in (
            ('Reconnecting... 2/5 (unexpected status 403 Forbidden: Unknown error)', 403),
            ('Falling back from WebSockets to HTTPS transport. unexpected status 403 Forbidden: Unknown error', 403),
            ('exceeded retry limit, last status: 429 Too Many Requests, request id: [REDACTED_ID]', 429),
        ):
            with self.subTest(message=message):
                event = {'type': 'error', 'phase': 'turn', 'error': {'message_excerpt': message}}
                self.assertEqual(self.summary(**event)['http_statuses'], [expected])
                event['type'] = 'response.output_text.delta'
                self.assertEqual(self.summary(**event)['http_statuses'], [])

    def test_raw_http_status_line_header_and_body(self):
        raw = ('HTTP/1.1 429 Too Many Requests\r\nRetry-After: 90\r\n'
               'Set-Cookie: private\r\n\r\n{"error":{"type":"usage_limit_reached"}}')
        row = self.summary(rawHTTP=raw, observed_at=NOW)
        self.assertEqual(row['not_before_utc'], q.iso(NOW+90))
        self.assertEqual(row['error_types'], ['usage_limit_reached'])
        self.assertNotIn('private', json.dumps(row))

    def test_raw_body_message_cannot_be_status(self):
        row = self.summary(raw_http='question: HTTP/1.1 429 Too Many Requests')
        self.assertEqual(row['category'], 'unknown')

    def test_raw_http_with_no_headers_still_reads_error_body(self):
        row = self.summary(raw_http='HTTP/2 429\r\n\r\n{"error":{"code":"insufficient_quota"}}')
        self.assertEqual(row['category'], 'quota_exhausted')

    def test_secrets_not_copied(self):
        row = self.summary(http_status=429, error={'code': 'usage_limit_reached',
            'message': 'Bearer SECRET user@example.invalid'}, account_alias='user@example.invalid',
            response_headers={'Authorization': 'Bearer SECRET', 'Cookie': 'secret', 'CF-Ray': 'secret'})
        self.assertNotIn('SECRET', json.dumps(row))
        self.assertNotIn('example.invalid', json.dumps(row))

    def test_json_and_jsonl_reader(self):
        with tempfile.TemporaryDirectory() as folder:
            path = Path(folder)/'rows.jsonl'
            path.write_text('{"http_status":429}\n{"http_status":403}\n')
            self.assertEqual(len(q.read_records(path)), 2)


class CachedState(unittest.TestCase):
    def setUp(self):
        self.row = {'identity_matches': True, 'credential_expires_at': NOW+3600,
            'source_child': {'status': 'active', 'oauth_reauth_status': 'success',
                             'last_checked_at': NOW},
            'source_history': {'quota_observed_at': NOW-10, 'primary_used_percent': 10,
                               'secondary_used_percent': 20}}

    def summary(self):
        return q.summarize_source_state(self.row, now=NOW)

    def test_active_recent_cached_quota_is_testable_not_model_proof(self):
        row = self.summary()
        self.assertEqual(row['state'], 'testable')
        self.assertFalse(row['live_model_called'])

    def test_old_snapshot_cannot_prove_current_testable(self):
        self.row['source_history']['quota_observed_at'] = NOW-301
        self.assertEqual(self.summary()['state'], 'unknown')

    def test_old_exhausted_snapshot_cannot_prove_still_limited(self):
        self.row['source_history'].update(quota_observed_at=NOW-301,
            primary_used_percent=100, primary_reset_at=NOW+1000)
        self.assertEqual(self.summary()['state'], 'unknown')

    def test_child_checked_at_cannot_substitute_quota_observed_at(self):
        del self.row['source_history']['quota_observed_at']
        self.assertEqual(self.summary()['state'], 'unknown')

    def test_exhausted_cached_future_window_requires_wait(self):
        self.row['source_history'].update(primary_used_percent=100, primary_reset_at=NOW+500)
        self.assertEqual(self.summary()['state'], 'rate_limited')
        self.assertEqual(self.summary()['not_before_utc'], q.iso(NOW+500))

    def test_elapsed_quota_reset_needs_recheck(self):
        self.row['source_history'].update(primary_used_percent=100, primary_reset_at=NOW-1)
        self.assertEqual(self.summary()['state'], 'unknown')

    def test_gateway_runtime_uses_milliseconds_and_does_not_clear(self):
        self.row['gateway_runtime'] = {'checked_at': NOW*1000, 'cooldowns': {'entries': [
            {'kind': 'rate_limit', 'until': (NOW+100)*1000}]}}
        before = json.dumps(self.row, sort_keys=True)
        self.assertEqual(self.summary()['state'], 'rate_limited')
        self.assertEqual(self.summary()['not_before_utc'], q.iso(NOW+100))
        self.assertEqual(json.dumps(self.row, sort_keys=True), before)

    def test_identity_change_always_unknown(self):
        self.row['identity_matches'] = False
        self.assertEqual(self.summary()['state'], 'unknown')

    def test_quota_before_current_assignment_is_not_reused(self):
        self.row['source_child']['assigned_at'] = NOW-1
        self.assertEqual(self.summary()['state'], 'unknown')

    def test_disabled_gateway_is_not_a_rate_limit(self):
        self.row['gateway_runtime'] = {'checked_at': NOW*1000,
            'effective_policy': {'account': {'enabled': False}}}
        self.assertEqual(self.summary()['state'], 'unknown')

    def test_capacity_and_expiry_do_not_become_rate_limit(self):
        self.row['gateway_runtime'] = {'checked_at': NOW*1000, 'admission': {'saturated': True}}
        self.assertEqual(self.summary()['state'], 'unknown')
        self.row.pop('gateway_runtime')
        self.row['credential_expires_at'] = NOW
        self.assertEqual(self.summary()['state'], 'unknown')


if __name__ == '__main__':
    unittest.main(verbosity=2)
