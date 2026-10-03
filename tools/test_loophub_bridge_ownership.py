"""Offline regression for persisted bridge ownership after management tag loss."""
import base64
import copy
import importlib.util
import json
from pathlib import Path
import tempfile
import time
import unittest


ROOT = Path(__file__).resolve().parents[1]
SPEC = importlib.util.spec_from_file_location('loophub_ownership', ROOT / 'deploy/loophub_codex_bridge.py')
bridge = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(bridge)


class Destination:
    def __init__(self):
        self.accounts = {}
        self.identities = {}
        self.credentials = {}
        self.calls = []
        self.metadata_patch = {}
        self.race_on_patch = False

    def call(self, path, data=None, method=None):
        self.calls.append((path, copy.deepcopy(data), method))
        if path == '/api/accounts/export/credentials':
            item = data['items'][0]
            self._check_version(self.accounts[item['id']], item)
            return {'accounts': [{'name': self.accounts[item['id']]['name'],
                                  'credentials': copy.deepcopy(self.credentials[item['id']])}]}
        parts = path.split('/')
        account_id = parts[3]
        if account_id not in self.accounts:
            raise bridge.BridgeError('agent_http_404')
        account = self.accounts[account_id]
        if path.endswith('/metadata'):
            return {'id': account_id, 'version': account['version'],
                    'identity': copy.deepcopy(self.identities[account_id]), **self.metadata_patch}
        if path.endswith('/upstream'):
            return {'id': account_id, 'snapshots': {}}
        if path.endswith('/reauthorize'):
            self._check_version(account, data)
            account['version'] += 1
            account['expires_at'] = data['credentials']['expires_at']
            self.credentials[account_id] = copy.deepcopy(data['credentials'])
            return {'id': account_id, 'version': account['version']}
        if method == 'PUT':
            if self.race_on_patch:
                account['version'] += 1
            self._check_version(account, data)
            account.update(copy.deepcopy(data))
            account['version'] += 1
            return {'ok': True}
        if data is not None or method not in (None, 'GET'):
            raise AssertionError('unexpected mutation')
        return {'account': copy.deepcopy(account)}

    @staticmethod
    def _check_version(account, data):
        if data['version'] != account['version']:
            raise bridge.BridgeError('agent_http_409', 409)


class OwnershipRecovery(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        root = Path(self.temp.name)
        key = root / 'key'
        key.write_text('synthetic-key-' * 4)
        self.api = Destination()
        self.config = {'workspace_id': 'synthetic-workspace', 'group_id': 'synthetic-group',
            'state_file': str(root / 'state.json'), 'ingress_key_file': str(key), 'proxy_ids': []}
        self.bridge = bridge.Bridge(self.config, api=self.api)
        self.filename = 'synthetic@example.invalid.json'
        self.expiry = int(time.time()) + 3600
        claims = {'exp': self.expiry, 'https://api.openai.com/auth': {
            'chatgpt_account_id': self.bridge.workspace, 'chatgpt_user_id': 'synthetic-user'},
            'https://api.openai.com/profile': {'email': 'synthetic@example.invalid'}}
        token = 'synthetic.' + base64.urlsafe_b64encode(json.dumps(claims).encode()).decode().rstrip('=') + '.synthetic'
        self.payload = {'type': 'codex', 'email': 'synthetic@example.invalid',
            'account_id': self.bridge.workspace, 'access_token': token}
        _, facts = self.bridge.credentials(self.filename, self.payload)
        name = 'Loophub-28765-' + bridge.digest(self.bridge.workspace + ':' + facts['email'])[:16]
        self.account_id = 'a_synthetic'
        self.mapping = {**facts, 'account_id': self.account_id, 'name': name, 'initialized': True}
        self.bridge.state['accounts'][self.filename] = self.mapping
        self.bridge.save()
        self.api.accounts[self.account_id] = {'id': self.account_id, 'name': name, 'version': 7,
            'has_refresh_token': False, 'tags': ['operator-tag'], 'enabled': True,
            'groups': ['synthetic-group'], 'expires_at': self.expiry}
        self.api.identities[self.account_id] = {'workspace_id': self.bridge.workspace, 'user_id': 'synthetic-user'}
        self.api.credentials[self.account_id] = {'access_token': token, 'account_id': self.bridge.workspace, 'expires_at': self.expiry}

    @property
    def account(self):
        return self.api.accounts[self.account_id]

    def assert_no_writes(self):
        self.assertTrue(all(data is None and method in (None, 'GET') for _, data, method in self.api.calls))

    def test_missing_tag_verified_listing_is_read_only(self):
        before = copy.deepcopy(self.bridge.state)
        self.assertEqual(self.bridge.list_files()['files'][0]['status'], 'active')
        self.assertEqual(self.bridge.state, before)
        self.assertEqual(self.account['tags'], ['operator-tag'])
        self.assert_no_writes()

    def test_upload_restores_tag_with_cas_and_keeps_other_tags(self):
        # Reload to prove recovery depends on the durable checkpoint.
        self.bridge = bridge.Bridge(self.config, api=self.api)
        self.assertTrue(self.bridge.upload(self.filename, self.payload)['ok'])
        self.assertEqual(self.account['tags'], ['operator-tag', self.bridge.tag])
        writes = [(path, data, method) for path, data, method in self.api.calls if data is not None and path != '/api/accounts/export/credentials']
        self.assertEqual(len(writes), 1)
        self.assertEqual(writes[0][1]['version'], 7)
        self.assertEqual(writes[0][2], 'PUT')
        self.assertTrue(self.bridge.state['accounts'][self.filename]['initialized'])

    def test_new_token_reauthorization_retains_identity_and_restores_tag(self):
        self.payload['access_token'] += '-new'
        self.bridge.upload(self.filename, self.payload)
        writes = [(path, data, method) for path, data, method in self.api.calls if data is not None and path != '/api/accounts/export/credentials']
        self.assertEqual(len(writes), 2)
        self.assertTrue(writes[0][0].endswith('/reauthorize'))
        self.assertFalse(writes[0][1]['allow_identity_change'])
        self.assertEqual(writes[1][1]['version'], 8)
        self.assertEqual(self.account['tags'], ['operator-tag', self.bridge.tag])

    def test_tag_restoration_rejects_concurrent_metadata_change(self):
        self.api.race_on_patch = True
        with self.assertRaisesRegex(bridge.BridgeError, 'agent_http_409'):
            self.bridge.upload(self.filename, self.payload)
        self.assertEqual(self.account['tags'], ['operator-tag'])
        self.assertFalse(self.bridge.state['accounts'][self.filename]['initialized'])

    def test_unknown_mapping_cannot_adopt_same_name_without_tag(self):
        self.bridge.state['accounts'].clear()
        self.api.accounts[self.account['name']] = self.account
        with self.assertRaisesRegex(bridge.BridgeError, 'destination_ownership_changed'):
            self.bridge.upload(self.filename, self.payload)
        self.assert_no_writes()

    def test_identity_version_or_metadata_id_drift_is_not_adopted(self):
        for patch in ({'id': 'a_other'}, {'version': 8}, {'version': True},
                      {'identity': {'workspace_id': 'other', 'user_id': 'synthetic-user'}},
                      {'identity': {'workspace_id': self.bridge.workspace, 'user_id': 'other'}},
                      {'identity': {}}, {'identity': None}, {'identity': ['malformed']}):
            with self.subTest(patch=patch):
                self.api.metadata_patch = patch
                with self.assertRaisesRegex(bridge.BridgeError, 'destination_ownership_changed'):
                    self.bridge.upload(self.filename, self.payload)
        self.assert_no_writes()

    def test_name_id_or_refresh_token_change_is_not_adopted(self):
        for patch in ({'id': 'a_other'}, {'name': 'renamed'}, {'has_refresh_token': True},
                      {'has_refresh_token': None}):
            with self.subTest(patch=patch):
                prior = copy.deepcopy(self.account)
                self.account.update(patch)
                with self.assertRaisesRegex(bridge.BridgeError, 'destination_ownership_changed'):
                    self.bridge.upload(self.filename, self.payload)
                self.api.accounts[self.account_id] = prior
        self.assert_no_writes()

    def test_missing_identity_checkpoint_is_not_adopted(self):
        self.mapping.pop('identity_sha256')
        with self.assertRaisesRegex(bridge.BridgeError, 'source_identity_changed'):
            self.bridge.upload(self.filename, self.payload)
        self.assert_no_writes()

    def test_unhealthy_mapping_does_not_break_other_files(self):
        healthy = copy.deepcopy(self.account)
        healthy.update(id='a_healthy', name='healthy', tags=[self.bridge.tag])
        self.api.accounts['a_healthy'] = healthy
        self.bridge.state['accounts']['healthy@example.invalid.json'] = {
            **self.mapping, 'account_id': 'a_healthy', 'name': 'healthy', 'email': 'healthy@example.invalid'}
        for failure in ('destination_ownership_changed', 'agent_http_404'):
            with self.subTest(failure=failure):
                if failure == 'agent_http_404':
                    del self.api.accounts[self.account_id]
                else:
                    self.account['has_refresh_token'] = True
                files = self.bridge.list_files()['files']
                self.assertEqual(len(files), 2)
                bad, good = files
                self.assertEqual(bad['status'], 'error')
                self.assertTrue(bad['unavailable'])
                self.assertFalse(bad['needs_reauth'])
                self.assertEqual(bad['status_message'], failure)
                self.assertEqual(good['status'], 'active')
        self.assert_no_writes()

    def test_withdraw_validates_identity_and_preserves_history(self):
        self.assertTrue(self.bridge.withdraw(self.filename)['ok'])
        self.assertFalse(self.account['enabled'])
        self.assertTrue(self.mapping['withdrawn'])
        self.assertIn(self.account_id, self.api.accounts)
        self.assertEqual(self.account['tags'], ['operator-tag'])

    def test_withdraw_rejects_changed_identity(self):
        self.api.identities[self.account_id]['user_id'] = 'other'
        with self.assertRaisesRegex(bridge.BridgeError, 'destination_ownership_changed'):
            self.bridge.withdraw(self.filename)
        self.assertTrue(self.account['enabled'])
        self.assert_no_writes()


if __name__ == '__main__':
    unittest.main(verbosity=2)
