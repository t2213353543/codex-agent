"""Offline destination-deletion and crash-window recovery contracts.

Run: python -m unittest discover -s tools -p 'test_loophub_bridge_recovery.py' -v
LOOPHUB_BRIDGE_UNDER_TEST may select a saved pre-fix source for red/green runs.
All credentials are synthetic. No network, OAuth, SSH, or deployment is used.
"""
import base64
import copy
import importlib.util
import json
import os
from pathlib import Path
import tempfile
import time
import unittest
import urllib.parse


ROOT = Path(__file__).resolve().parents[1]
SOURCE = Path(os.environ.get('LOOPHUB_BRIDGE_UNDER_TEST', ROOT / 'deploy/loophub_codex_bridge.py'))
SPEC = importlib.util.spec_from_file_location('bridge_recovery_subject', SOURCE)
bridge = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(bridge)


def synthetic_token(generation=1, user='synthetic-user', workspace='synthetic-workspace'):
    claims = {
        'exp': int(time.time()) + 3600,
        'jti': f'synthetic-generation-{generation}',
        'https://api.openai.com/auth': {
            'chatgpt_account_id': workspace, 'chatgpt_user_id': user,
        },
        'email': 'synthetic@example.invalid',
    }
    encoded = base64.urlsafe_b64encode(json.dumps(claims).encode()).decode().rstrip('=')
    return 'e30.' + encoded + '.synthetic'


class Destination:
    """A versioned store with distinct ID/name lookups and recorded mutations."""

    def __init__(self):
        self.rows = {}
        self.secrets = {}
        self.calls = []
        self.failures = {}
        self.sequence = 0

    def add(self, account_id, name, credentials, tags, *, enabled=False, groups=None):
        self.rows[account_id] = {
            'id': account_id, 'name': name, 'version': 7,
            'has_refresh_token': bool(credentials.get('refresh_token')),
            'tags': list(tags), 'groups': list(groups or []), 'enabled': enabled,
            'expires_at': credentials['expires_at'],
            'credential_status': 'unverified', 'status': 'unverified', 'last_error': '',
        }
        self.secrets[account_id] = copy.deepcopy(credentials)

    def lookup(self, reference):
        if reference in self.rows:
            return self.rows[reference]
        matches = [row for row in self.rows.values() if row['name'] == reference]
        if len(matches) == 1:
            return matches[0]
        raise bridge.BridgeError('agent_http_404')

    @staticmethod
    def check_version(row, data):
        if data.get('version') != row['version']:
            raise bridge.BridgeError('agent_http_409', 409)

    def call(self, path, data=None, method=None):
        actual_method = method or ('POST' if data is not None else 'GET')
        self.calls.append((actual_method, path, copy.deepcopy(data)))
        failure = self.failures.get((actual_method, path))
        if failure:
            raise bridge.BridgeError(failure, 409 if failure.endswith('_409') else 502)
        if path == '/api/accounts/export/credentials':
            item, = data['items']
            row = self.lookup(item['id'])
            self.check_version(row, item)
            return {'accounts': [{'name': row['name'], 'credentials': copy.deepcopy(self.secrets[row['id']])}]}
        if path == '/api/accounts' and actual_method == 'POST':
            if data.get('replace') is not False:
                raise AssertionError('recovery must not replace an unrelated name')
            incoming, = data['accounts']
            if any(row['name'] == incoming['name'] for row in self.rows.values()):
                raise AssertionError('existing stable name must be recovered before import')
            self.sequence += 1
            account_id = f'new-{self.sequence}'
            self.add(account_id, incoming['name'], incoming['credentials'], incoming['tags'],
                     enabled=incoming['enabled'], groups=incoming.get('groups'))
            return {'results': [{'id': account_id, 'status': 'added'}]}
        parts = path.split('/')
        if len(parts) not in (4, 5) or parts[:3] != ['', 'api', 'accounts']:
            raise AssertionError('unexpected offline management route: ' + path)
        row = self.lookup(urllib.parse.unquote(parts[3]))
        account_id = row['id']
        suffix = parts[4] if len(parts) == 5 else ''
        if suffix == 'metadata' and actual_method == 'GET':
            facts = bridge.bridge_token_facts(self.secrets[account_id]['access_token'])
            return {'id': account_id, 'version': row['version'],
                    'identity': {'workspace_id': facts['workspace'], 'user_id': facts['user']}}
        if suffix == 'upstream' and actual_method == 'GET':
            return {'id': account_id, 'snapshots': {}}
        if suffix == 'reauthorize' and actual_method == 'POST':
            self.check_version(row, data)
            if data.get('allow_identity_change') is not False:
                raise AssertionError('recovery must not authorize an owner change')
            self.secrets[account_id] = copy.deepcopy(data['credentials'])
            row['version'] += 1
            row['expires_at'] = data['credentials']['expires_at']
            return {'id': account_id, 'version': row['version']}
        if not suffix and actual_method == 'PUT':
            self.check_version(row, data)
            row.update(copy.deepcopy(data))
            row['version'] += 1
            return {'ok': True}
        if not suffix and actual_method == 'GET':
            return {'account': copy.deepcopy(row)}
        raise AssertionError('unexpected offline mutation: ' + path)

    def mutations(self):
        return [(method, path, data) for method, path, data in self.calls
                if method != 'GET' and path != '/api/accounts/export/credentials']


class RecoveryTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        root = Path(temporary.name)
        (root / 'key').write_text('synthetic-ingress-key-' * 3)
        self.config = {
            'workspace_id': 'synthetic-workspace', 'group_id': 'delivery-group',
            'state_file': str(root / 'state.json'), 'ingress_key_file': str(root / 'key'),
        }
        self.api = Destination()
        self.instance = bridge.Bridge(self.config, api=self.api)
        self.filename = 'synthetic@example.invalid.json'
        self.payload = {'type': 'codex', 'email': 'synthetic@example.invalid',
                        'account_id': 'synthetic-workspace', 'access_token': synthetic_token()}
        self.credentials, self.facts = self.instance.credentials(self.filename, self.payload)
        self.name = 'Loophub-28765-' + bridge.digest('synthetic-workspace:synthetic@example.invalid')[:16]

    def checkpoint(self, account_id='deleted-id'):
        mapping = {**self.facts, 'account_id': account_id, 'name': self.name,
                   'initialized': True, 'withdrawn': False}
        self.instance.state['accounts'][self.filename] = mapping
        self.instance.save()
        return mapping

    def add_destination(self, account_id='recovered-id', *, generation=None, user='synthetic-user', tags=None):
        credentials = copy.deepcopy(self.credentials)
        if generation is not None or user != 'synthetic-user':
            credentials['access_token'] = synthetic_token(generation if generation is not None else 1, user=user)
        self.api.add(account_id, self.name, credentials,
                     [self.instance.tag] if tags is None else tags,
                     enabled=True, groups=['operator-group'])
        return self.api.rows[account_id]

    def assert_unchanged_failure(self, expected, action):
        receipt = copy.deepcopy(self.instance.state)
        stored_receipt = self.instance.state_path.read_bytes() if self.instance.state_path.exists() else None
        rows, secrets = copy.deepcopy(self.api.rows), copy.deepcopy(self.api.secrets)
        with self.assertRaisesRegex(bridge.BridgeError, expected):
            action()
        self.assertEqual(self.instance.state, receipt)
        current_receipt = self.instance.state_path.read_bytes() if self.instance.state_path.exists() else None
        self.assertEqual(current_receipt, stored_receipt)
        self.assertEqual(self.api.rows, rows)
        self.assertEqual(self.api.secrets, secrets)
        self.assertEqual(self.api.mutations(), [])

    def test_deleted_mapped_destination_is_reimported_and_receipt_survives_reload(self):
        self.checkpoint()
        self.instance = bridge.Bridge(self.config, api=self.api)
        self.assertTrue(self.instance.upload(self.filename, self.payload)['ok'])
        mapping = self.instance.state['accounts'][self.filename]
        self.assertNotEqual(mapping['account_id'], 'deleted-id')
        account = self.api.rows[mapping['account_id']]
        self.assertEqual(account['name'], self.name)
        self.assertTrue(account['enabled'])
        self.assertIn('delivery-group', account['groups'])
        self.assertEqual(self.api.secrets[account['id']]['access_token'], self.payload['access_token'])
        self.assertEqual([path for _, path, _ in self.api.mutations()], ['/api/accounts', '/api/accounts/' + account['id']])
        restored = bridge.Bridge(self.config, api=self.api)
        self.assertTrue(restored.state['accounts'][self.filename]['initialized'])
        self.assertEqual(restored.list_files()['files'][0]['status'], 'active')
        self.assertNotIn(self.payload['access_token'], self.instance.state_path.read_text())

    def test_no_checkpoint_recovers_same_tag_name_and_replaces_different_token(self):
        self.add_destination(generation=0)
        self.assertNotEqual(self.api.secrets['recovered-id']['access_token'], self.payload['access_token'])
        self.assertTrue(self.instance.upload(self.filename, self.payload)['ok'])
        self.assertEqual(self.instance.state['accounts'][self.filename]['account_id'], 'recovered-id')
        self.assertEqual(self.api.secrets['recovered-id']['access_token'], self.payload['access_token'])
        self.assertEqual(self.api.rows['recovered-id']['groups'], ['operator-group', 'delivery-group'])
        writes = self.api.mutations()
        self.assertEqual([path for _, path, _ in writes], ['/api/accounts/recovered-id/reauthorize', '/api/accounts/recovered-id'])
        self.assertEqual(writes[0][2]['version'], 7)
        self.assertFalse(writes[0][2]['allow_identity_change'])

    def test_deleted_mapping_recovers_tagged_name_without_importing_duplicate(self):
        self.checkpoint()
        self.add_destination(generation=2)
        self.assertTrue(self.instance.upload(self.filename, self.payload)['ok'])
        self.assertEqual(self.instance.state['accounts'][self.filename]['account_id'], 'recovered-id')
        self.assertFalse(any(path == '/api/accounts' for _, path, _ in self.api.mutations()))
        self.assertEqual(self.api.secrets['recovered-id']['access_token'], self.payload['access_token'])

    def test_mapped_read_401_500_and_transport_failure_do_not_trigger_recovery(self):
        self.checkpoint()
        self.add_destination()
        for code in ('agent_http_401', 'agent_http_500', 'agent_http_409', 'agent_unavailable_or_invalid_response'):
            with self.subTest(code=code):
                self.api.calls.clear()
                self.api.failures[('GET', '/api/accounts/deleted-id')] = code
                self.assert_unchanged_failure(code, lambda: self.instance.upload(self.filename, self.payload))
                self.assertEqual(len(self.api.calls), 1)

    def test_stable_name_lookup_401_and_500_do_not_trigger_import(self):
        self.checkpoint()
        for code in ('agent_http_401', 'agent_http_500'):
            with self.subTest(code=code):
                self.api.calls.clear()
                self.api.failures[('GET', '/api/accounts/' + self.name)] = code
                self.assert_unchanged_failure(code, lambda: self.instance.upload(self.filename, self.payload))
                self.assertEqual(len(self.api.calls), 2)

    def test_existing_id_ownership_change_does_not_fall_back_to_stable_name(self):
        self.checkpoint('recovered-id')
        row = self.add_destination()
        for patch in ({'name': 'changed-name'}, {'has_refresh_token': True}, {'id': 'changed-id'}):
            with self.subTest(patch=patch):
                before = copy.deepcopy(row)
                row.update(patch)
                self.api.calls.clear()
                self.assert_unchanged_failure('destination_ownership_changed', lambda: self.instance.upload(self.filename, self.payload))
                self.assertEqual(len(self.api.calls), 1)
                row.clear()
                row.update(before)

    def test_same_name_different_owner_is_not_adopted_even_with_source_tag(self):
        for checkpoint in (False, True):
            with self.subTest(checkpoint=checkpoint):
                if checkpoint:
                    self.checkpoint()
                self.add_destination(user='different-user')
                self.api.calls.clear()
                self.assert_unchanged_failure('destination_ownership_changed', lambda: self.instance.upload(self.filename, self.payload))

    def test_same_name_without_source_tag_is_not_adopted_after_mapping_deletion(self):
        self.checkpoint()
        self.add_destination(tags=['operator-tag'])
        self.assert_unchanged_failure('destination_ownership_changed', lambda: self.instance.upload(self.filename, self.payload))

    def test_changed_incoming_identity_is_rejected_before_any_destination_lookup(self):
        self.checkpoint()
        self.payload['access_token'] = synthetic_token(user='different-user')
        self.assert_unchanged_failure('source_identity_changed', lambda: self.instance.upload(self.filename, self.payload))
        self.assertEqual(self.api.calls, [])

    def test_withdraw_missing_id_is_durable_and_idempotent(self):
        self.checkpoint()
        self.assertTrue(self.instance.withdraw(self.filename)['ok'])
        self.assertTrue(self.instance.state['accounts'][self.filename]['withdrawn'])
        self.assertEqual(self.api.mutations(), [])
        count = len(self.api.calls)
        self.instance = bridge.Bridge(self.config, api=self.api)
        self.assertTrue(self.instance.withdraw(self.filename)['ok'])
        self.assertEqual(len(self.api.calls), count)
        self.assertEqual(self.instance.list_files()['files'], [])

    def test_withdraw_401_500_do_not_claim_success(self):
        self.checkpoint()
        for code in ('agent_http_401', 'agent_http_500'):
            with self.subTest(code=code):
                self.api.calls.clear()
                self.api.failures[('GET', '/api/accounts/deleted-id')] = code
                self.assert_unchanged_failure(code, lambda: self.instance.withdraw(self.filename))

    def test_withdraw_rejects_different_identity_even_if_original_tag_remains(self):
        self.checkpoint('recovered-id')
        self.add_destination(user='different-user')
        self.assert_unchanged_failure('destination_ownership_changed', lambda: self.instance.withdraw(self.filename))

    def test_withdraw_metadata_404_is_not_destination_deletion(self):
        self.checkpoint('recovered-id')
        self.add_destination(tags=['operator-tag'])
        self.api.failures[('GET', '/api/accounts/recovered-id/metadata')] = 'agent_http_404'
        self.assert_unchanged_failure('agent_http_404|destination_ownership_changed', lambda: self.instance.withdraw(self.filename))

    def test_withdraw_export_404_is_not_destination_deletion(self):
        self.checkpoint('recovered-id')
        self.add_destination()
        self.api.failures[('POST', '/api/accounts/export/credentials')] = 'agent_http_404'
        self.assert_unchanged_failure('agent_http_404', lambda: self.instance.withdraw(self.filename))

    def test_additional_group_configuration_rejects_malformed_values(self):
        for groups in (None, True, 7, 'business-group', {}, ('business-group',),
                       [''], [' business-group'], ['business-group '], [False], [7], [[]], [{}]):
            with self.subTest(groups=groups):
                with self.assertRaisesRegex(bridge.BridgeError, 'invalid_additional_group_ids'):
                    bridge.Bridge({**self.config, 'additional_group_ids': groups}, api=self.api)
        self.assertEqual(self.api.calls, [])

    def test_recovered_delivery_merges_required_and_operator_groups_without_duplicates(self):
        self.config['additional_group_ids'] = ['business-group', 'delivery-group', 'business-group']
        self.instance = bridge.Bridge(self.config, api=self.api)
        self.checkpoint()
        self.add_destination()
        self.assertTrue(self.instance.upload(self.filename, self.payload)['ok'])
        self.assertEqual(self.api.rows['recovered-id']['groups'], ['operator-group', 'delivery-group', 'business-group'])
        self.assertEqual(self.instance.list_files()['files'][0]['status'], 'active')
        self.assertTrue(self.instance.upload(self.filename, self.payload)['ok'])
        self.assertEqual(self.api.rows['recovered-id']['groups'], ['operator-group', 'delivery-group', 'business-group'])

    def test_new_child_delivery_receives_every_required_group(self):
        self.config['additional_group_ids'] = ['business-group']
        self.instance = bridge.Bridge(self.config, api=self.api)
        self.assertTrue(self.instance.upload(self.filename, self.payload)['ok'])
        account_id = self.instance.state['accounts'][self.filename]['account_id']
        self.assertEqual(self.api.rows[account_id]['groups'], ['delivery-group', 'business-group'])
        self.assertEqual(self.instance.list_files()['files'][0]['status'], 'active')

    def test_missing_any_required_group_is_unavailable_without_claiming_token_failure(self):
        self.config['additional_group_ids'] = ['business-group', 'second-business-group']
        self.instance = bridge.Bridge(self.config, api=self.api)
        self.checkpoint('recovered-id')
        account = self.add_destination()
        required = ['delivery-group', 'business-group', 'second-business-group']
        for omitted in required:
            with self.subTest(omitted=omitted):
                account['groups'] = ['operator-group'] + [group for group in required if group != omitted]
                self.api.calls.clear()
                status = self.instance.list_files()['files'][0]
                self.assertEqual(status['status'], 'error')
                self.assertTrue(status['disabled'])
                self.assertTrue(status['unavailable'])
                self.assertFalse(status['needs_reauth'])
                self.assertEqual(status['status_message'], 'destination_unavailable')
                self.assertEqual(self.api.mutations(), [])
        account['groups'] = ['operator-group'] + required
        self.assertEqual(self.instance.list_files()['files'][0]['status'], 'active')


if __name__ == '__main__':
    unittest.main(verbosity=2)
