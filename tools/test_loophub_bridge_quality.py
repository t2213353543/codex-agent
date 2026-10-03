"""Offline credential-state regression for the 28765 CPA delivery bridge.

Run: python tools/test_loophub_bridge_quality.py
No account export, model request, refresh, deployment, or source mutation.
"""
import copy
import importlib.util
import json
from pathlib import Path
import tempfile
import time
import unittest


ROOT = Path(__file__).resolve().parents[1]
SPEC = importlib.util.spec_from_file_location('loophub_bridge_q01', ROOT / 'deploy/loophub_codex_bridge.py')
bridge = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(bridge)


class Destination:
    def __init__(self):
        self.account = {}
        self.snapshots = {'id': 'a_synthetic', 'snapshots': {}}
        self.calls = []
        self.error = False
        self.change_after_snapshot = None

    def call(self, path, data=None, method=None):
        if data is not None or method not in (None, 'GET'):
            raise AssertionError('status listing must never write or probe upstream')
        self.calls.append(path)
        if path == '/api/accounts/a_synthetic/upstream':
            if self.error:
                raise bridge.BridgeError('agent_unavailable_or_invalid_response')
            result = copy.deepcopy(self.snapshots)
            if self.change_after_snapshot:
                self.account.update(self.change_after_snapshot)
            return result
        if path != '/api/accounts/a_synthetic':
            raise AssertionError('unexpected management route: ' + path)
        return {'account': copy.deepcopy(self.account)}


class CredentialState(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        key = self.root / 'key'
        key.write_text('synthetic-' * 5)
        self.api = Destination()
        self.bridge = bridge.Bridge({'workspace_id': 'synthetic-workspace',
            'group_id': 'synthetic-group', 'state_file': str(self.root / 'state.json'),
            'ingress_key_file': str(key)}, api=self.api)
        self.now = int(time.time())
        self.mapping = {'account_id': 'a_synthetic', 'name': 'synthetic-account',
            'email': 'synthetic@example.invalid', 'expires_at': self.now + 3600,
            'initialized': True, 'updated_at': self.now - 20}
        self.bridge.state['accounts']['synthetic@example.invalid.json'] = self.mapping
        self.api.account = {'id': 'a_synthetic', 'name': 'synthetic-account',
            'version': 9, 'tags': [self.bridge.tag], 'has_refresh_token': False,
            'groups': ['synthetic-group'], 'enabled': True,
            'expires_at': self.now + 3600, 'status': 'unverified',
            'credential_status': 'unverified', 'last_error': ''}

    def listing(self):
        return self.bridge.list_files()['files'][0]

    def snapshot(self, **fields):
        row = {'id': 'a_synthetic', 'version': 2, 'credential_version': 2,
            'stale': False, 'checked_at': self.now, 'ok': False, 'http_code': 401,
            'kind': 'stream', 'error': {'code': 'upstream_http_error', 'message': 'unused'}}
        row.update(fields)
        self.api.snapshots['snapshots']['stream'] = row
        return row

    def test_live_revoked_snapshot_cannot_be_reported_active(self):
        # Before the fix, enabled + future mapping expiry always returned active.
        self.api.account['status'] = 'error'
        self.snapshot()
        row = self.listing()
        self.assertEqual(row['status'], 'error')
        self.assertTrue(row['needs_reauth'])
        self.assertEqual(row['status_message'], 'upstream_http_401')
        self.assertTrue(all(not value.endswith('/test-stream') for value in self.api.calls))

    def test_snapshot_metadata_version_does_not_equal_credential_version(self):
        self.snapshot(version=2, credential_version=2)
        self.api.account['version'] = 9
        self.assertTrue(self.listing()['needs_reauth'])

    def test_expiry_uses_current_destination_metadata(self):
        self.mapping['expires_at'] = self.now - 100
        self.assertEqual(self.listing()['status'], 'active')
        self.mapping['expires_at'] = self.now + 3600
        self.api.account['expires_at'] = self.now - 1
        row = self.listing()
        self.assertTrue(row['needs_reauth'])
        self.assertEqual(row['status_message'], 'token_expired')

    def test_exact_codes_and_structured_status_only(self):
        self.api.account.update(status='error', credential_status='invalid')
        for value in ('token_revoked', 'token_invalidated', 'upstream returned HTTP 401',
            json.dumps({'error': {'code': 'token_revoked'}}), json.dumps({'http_code': 401})):
            with self.subTest(value=value):
                self.api.account['last_error'] = value
                self.assertTrue(self.listing()['needs_reauth'])

    def test_capacity_network_and_message_markers_never_request_reauth(self):
        for value in ('connection failed', 'no available account', 'invalid model',
            'HTTP 429 capacity exceeded', 'request message contains token_revoked and 401',
            json.dumps({'message': 'token_invalidated'}),
            json.dumps({'error': {'message': 'token_revoked'}}),
            json.dumps({'http_code': 429, 'code': 'token_revoked'})):
            with self.subTest(value=value):
                self.api.account.update(status='error', credential_status='refresh_failed', last_error=value)
                row = self.listing()
                self.assertFalse(row['needs_reauth'])
                self.assertEqual(row['status_message'], '')
                self.assertEqual(row['status'], 'active')
                self.assertFalse(row['unavailable'])
                self.assertFalse(row['disabled'])
                self.assertNotIn(value, json.dumps(row))

    def test_429_403_and_network_snapshots_do_not_request_reauth(self):
        for status in (0, 403, 408, 429, 500, 503):
            with self.subTest(status=status):
                self.snapshot(http_code=status, error={'code': 'upstream_http_error', 'message': 'HTTP 401 token_revoked'})
                self.assertFalse(self.listing()['needs_reauth'])

    def test_stale_or_malformed_snapshot_cannot_invalidate_current_at(self):
        for changed in ({'stale': True}, {'stale': None}, {'credential_version': 0},
            {'credential_version': True}, {'checked_at': self.now - 301},
            {'checked_at': self.now + 60}, {'id': 'different-account'}):
            with self.subTest(changed=changed):
                self.snapshot(**changed)
                self.assertFalse(self.listing()['needs_reauth'])

    def test_newer_success_supersedes_old_rejection(self):
        self.snapshot(checked_at=self.now - 10)
        self.api.snapshots['snapshots']['models'] = {
            **self.api.snapshots['snapshots']['stream'], 'kind': 'models',
            'checked_at': self.now, 'ok': True, 'http_code': 200}
        self.assertFalse(self.listing()['needs_reauth'])

    def test_equal_second_contradictory_observations_are_inconclusive(self):
        self.snapshot()
        self.api.snapshots['snapshots']['models'] = {
            **self.api.snapshots['snapshots']['stream'], 'kind': 'models', 'ok': True, 'http_code': 200}
        self.assertFalse(self.listing()['needs_reauth'])

    def test_replacement_during_snapshot_read_never_reuses_old_401(self):
        self.snapshot()
        self.api.change_after_snapshot = {'version': 10, 'expires_at': self.now + 7200}
        row = self.listing()
        self.assertFalse(row['needs_reauth'])
        self.assertEqual(row['status_message'], 'credential_observation_unavailable')
        self.assertEqual(row['status'], 'active')

    def test_ownership_change_during_snapshot_read_is_reported_unavailable(self):
        self.snapshot()
        self.api.change_after_snapshot = {'has_refresh_token': True, 'version': 10}
        row = self.listing()
        self.assertEqual(row['status_message'], 'destination_ownership_changed')
        self.assertTrue(row['unavailable'])
        self.assertFalse(row['needs_reauth'])

    def test_missing_optional_observation_preserves_delivery_not_reauth(self):
        self.api.error = True
        row = self.listing()
        self.assertFalse(row['needs_reauth'])
        self.assertEqual(row['status'], 'active')
        self.assertFalse(row['unavailable'])
        self.assertFalse(row['disabled'])
        self.assertEqual(row['status_message'], 'credential_observation_unavailable')

    def test_actual_delivery_controls_still_make_file_unavailable(self):
        for patch in ({'enabled': False}, {'groups': []}, {'expires_at': 0}):
            with self.subTest(patch=patch):
                before = copy.deepcopy(self.api.account)
                self.api.account.update(patch)
                row = self.listing()
                self.assertEqual(row['status'], 'error')
                self.assertTrue(row['unavailable'])
                self.assertFalse(row['needs_reauth'])
                self.api.account = before
        self.mapping['initialized'] = False
        self.assertTrue(self.listing()['unavailable'])

    def test_repeated_capacity_observations_never_mutate_delivery(self):
        self.api.account.update(status='error', last_error='HTTP 429 capacity exceeded')
        self.snapshot(http_code=429)
        before = copy.deepcopy(self.bridge.state)
        for _ in range(3):
            self.assertEqual(self.listing()['status'], 'active')
        self.assertEqual(self.bridge.state, before)

    def test_withdrawn_or_unselected_accounts_are_not_observed(self):
        self.assertEqual(self.bridge.list_files('other.json'), {'files': []})
        self.mapping['withdrawn'] = True
        self.assertEqual(self.bridge.list_files(), {'files': []})
        self.assertEqual(self.api.calls, [])


if __name__ == '__main__':
    unittest.main(verbosity=2)
