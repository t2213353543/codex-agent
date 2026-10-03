"""Synthetic, network-free checks for the access-token-only workbench bridge.

Run from the repository root: python integration/gateway_environment/bridge_smoke.py
The source and destination APIs, docker inspection, and private key path are
replaced in memory. These checks do not test the payment application.
"""
import base64
import copy
import importlib.util
import json
import os
from pathlib import Path
import re
import tempfile
import time
import unittest
from unittest import mock
import urllib.parse


BRIDGE_PATH = Path(__file__).resolve().parents[2] / 'deploy' / 'workbench_bridge.py'
SPEC = importlib.util.spec_from_file_location('workbench_bridge_under_test', BRIDGE_PATH)
bridge = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(bridge)
REGISTERED_ROUTES = re.findall(r'mux\.HandleFunc\("([A-Z]+ [^"]+)"',
    (lambda root: ((root / 'internal/server/admin.go') if (root / 'internal/server').is_dir() else (root / 'admin.go')).read_text(encoding='utf-8'))(Path(os.environ.get('BRIDGE_TEST_SOURCE_ROOT', BRIDGE_PATH.parent.parent))))


def registered_route(method, path):
    for route in REGISTERED_ROUTES:
        pattern = re.escape(route)
        for parameter in re.findall(r'\{[^}]+\}', route):
            pattern = pattern.replace(re.escape(parameter), '[^/]+')
        if re.fullmatch(pattern, method + ' ' + path):
            return True
    return False


def synthetic_token(expiry=None):
    payload = {'sub': 'synthetic-user', 'exp': expiry or int(time.time()) + 3600,
        'https://api.openai.com/auth': {'chatgpt_account_id': 'synthetic-account',
            'chatgpt_user_id': 'synthetic-user', 'chatgpt_plan_type': 'pro'}}
    encoded = base64.urlsafe_b64encode(json.dumps(payload).encode()).decode().rstrip('=')
    return 'synthetic.' + encoded + '.not-a-real-signature'


def eligible_job():
    return {'id': 'synthetic-job', 'status': 'succeeded', 'recharge_status': 'success',
        'recharge_plan_type': 'pro5', 'has_refresh_token': True,
        'is_refunded': False, 'has_active_refund': False,
        'can_delete': False, 'delete_block_reason': '成功且尚未退款的注册任务不能删除'}


class Source:
    def __init__(self):
        self.job = eligible_job()
        self.token = synthetic_token()
        self.error = None
        self.listed = True
        self.calls = []

    def call(self, path, data=None, method=None):
        self.calls.append(path)
        if self.error:
            raise self.error
        if '/export-sub?' in path:
            return {'accounts': [{'credentials': {'access_token': self.token,
                'refresh_token': 'synthetic-source-only-refresh-secret',
                'chatgpt_account_id': 'synthetic-account'}}]}
        if '?' in path:
            rows = [copy.deepcopy(self.job)] if self.listed else []
            # Match the production list shape: active-refund evidence is detail-only.
            for row in rows:
                row.pop('has_active_refund', None)
            return {'items': rows, 'total': len(rows)}
        return copy.deepcopy(self.job)


class Destination:
    """Minimal semantic fake of the inspected import/PUT contract, not a server."""
    def __init__(self):
        self.accounts = {}
        self.calls = []
        self.reauthorize_error = None

    @property
    def account_writes(self):
        return [(path, data, method) for path, data, method in self.calls
                if data is not None and path != '/api/accounts/import/preview']

    def call(self, path, data=None, method=None):
        verb = method or ('POST' if data is not None else 'GET')
        if not registered_route(verb, path):
            raise AssertionError('Destination request does not match admin.go: ' + verb + ' ' + path)
        self.calls.append((path, copy.deepcopy(data), method))
        if path == '/api/session':
            return {'authenticated': True}
        if path == '/api/groups':
            if data:
                raise AssertionError('Fixture group should already exist')
            return {'groups': [{'id': 'synthetic-group', 'name': bridge.GROUP}]}
        if path.endswith('/reauthorize'):
            if self.reauthorize_error:
                raise bridge.SafeError(self.reauthorize_error)
            account_id = urllib.parse.unquote(path.split('/')[-2])
            account = self.accounts[account_id]
            if account['version'] != data['version']:
                raise bridge.SafeError('http_409')
            account['_token'] = data['credentials']['access_token']
            account['version'] += 1
            return {'ok': True, 'id': account_id, 'version': account['version']}
        if path in ('/api/accounts', '/api/accounts/import/preview'):
            row = copy.deepcopy(data['accounts'][0])
            token = row['credentials']['access_token']
            duplicate = next((a for a in self.accounts.values() if a['_token'] == token), None)
            named = next((a for a in self.accounts.values() if a['name'] == row['name']), None)
            if duplicate:
                result = {'status': 'duplicate', 'id': duplicate['id']}
            elif named and not data['replace']:
                result = {'status': 'failed', 'code': 'name_conflict'}
            elif named:
                result = {'status': 'replaced', 'id': named['id']}
                if path == '/api/accounts':
                    named['_token'] = token
                    named['version'] += 1
            else:
                result = {'status': 'added', 'id': 'synthetic-destination-' + str(len(self.accounts) + 1)}
                if path == '/api/accounts':
                    self.accounts[result['id']] = {**row, 'id': result['id'], 'version': 1,
                        'groups': [], 'has_refresh_token': False, '_token': token}
            return {'results': [result]}
        account_id = urllib.parse.unquote(path.rsplit('/', 1)[1])
        if account_id not in self.accounts:
            raise bridge.SafeError('http_404')
        account = self.accounts[account_id]
        if method == 'PUT':
            if data['version'] != account['version']:
                raise bridge.SafeError('http_409')
            account.update({key: value for key, value in data.items() if key != 'version'})
            account['version'] += 1
            return {'ok': True}
        return {'account': copy.deepcopy(account)}


class BridgeSmoke(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.key = self.root / 'synthetic-key'
        self.key.write_text('synthetic-management-key')
        self.source = Source()
        self.destination = Destination()
        patches = [
            mock.patch.object(bridge, 'ROOT', self.root),
            mock.patch.object(bridge, 'Path', lambda _: self.key),
            mock.patch.object(bridge, 'source_api', return_value=(self.source, 'synthetic-image')),
            mock.patch.object(bridge, 'API', return_value=self.destination),
            mock.patch.object(bridge.subprocess, 'check_output', side_effect=AssertionError('No docker allowed')),
            mock.patch.object(bridge.urllib.request.OpenerDirector, 'open', side_effect=AssertionError('No network allowed')),
        ]
        for patch in patches:
            patch.start()
            self.addCleanup(patch.stop)

    def state(self):
        return json.loads((self.root / 'state.json').read_text())

    def import_once(self):
        report = bridge.run(apply=True)
        self.assertEqual(report['counts'].get('added'), 1)
        account_id = self.state()['accounts'][self.source.job['id']]['account_id']
        return self.destination.accounts[account_id]

    def test_only_access_token_forwarded_and_reports_have_no_secrets(self):
        self.import_once()
        payloads = [data for path, data, method in self.destination.calls if path == '/api/accounts']
        self.assertEqual(len(payloads), 1)
        credentials = payloads[0]['accounts'][0]['credentials']
        self.assertEqual(credentials['access_token'], self.source.token)
        self.assertNotIn('refresh_token', credentials)
        evidence = (self.root / 'state.json').read_text() + (self.root / 'latest.json').read_text()
        for secret in (self.source.token, 'synthetic-source-only-refresh-secret', 'synthetic-management-key'):
            self.assertNotIn(secret, evidence)

    def test_preview_does_not_mutate_destination_or_write_state(self):
        result = bridge.run(apply=False)
        self.assertEqual(result['counts'], {'preview_added': 1})
        self.assertEqual(self.destination.accounts, {})
        self.assertEqual(self.destination.account_writes, [])
        self.assertFalse((self.root / 'state.json').exists())

    def test_repeated_apply_is_idempotent(self):
        self.import_once()
        self.destination.calls.clear()
        report = bridge.run(apply=True)
        self.assertEqual(report['counts'], {'unchanged': 1})
        self.assertEqual(self.destination.account_writes, [])
        self.assertEqual(len(self.destination.accounts), 1)

    def test_refund_disables_owned_account_once(self):
        account = self.import_once()
        self.source.job['is_refunded'] = True
        self.source.listed = False
        self.destination.calls.clear()
        report = bridge.run(apply=True)
        self.assertFalse(account['enabled'])
        self.assertEqual(report['counts'], {'disabled_ineligible': 1})
        self.destination.calls.clear()
        bridge.run(apply=True)
        self.assertEqual(self.destination.account_writes, [])

    def test_refund_does_not_modify_account_after_ownership_change(self):
        for mutation in ({'tags': []}, {'has_refresh_token': True}):
            with self.subTest(mutation=mutation):
                self.source.job = eligible_job()
                self.source.listed = True
                self.destination.accounts.clear()
                (self.root / 'state.json').unlink(missing_ok=True)
                account = self.import_once()
                account.update(mutation)
                self.source.job['is_refunded'] = True
                self.source.listed = False
                self.destination.calls.clear()
                with self.assertRaisesRegex(bridge.SafeError, '^destination_ownership_changed$'):
                    bridge.run(apply=True)
                self.assertTrue(account['enabled'])
                self.assertEqual(self.destination.account_writes, [])

    def test_source_transport_failure_leaves_destination_and_state_unchanged(self):
        account = self.import_once()
        before = (self.root / 'state.json').read_bytes()
        self.source.error = bridge.SafeError('http_503')
        self.destination.calls.clear()
        with self.assertRaisesRegex(bridge.SafeError, '^http_503$'):
            bridge.run(apply=True)
        self.assertTrue(account['enabled'])
        self.assertEqual(self.destination.account_writes, [])
        self.assertEqual((self.root / 'state.json').read_bytes(), before)

    def test_duplicate_credential_does_not_adopt_external_account(self):
        self.destination.accounts['external'] = {'id': 'external', 'name': 'Manual account',
            'version': 1, 'enabled': False, 'groups': [], 'tags': [],
            'has_refresh_token': True, '_token': self.source.token}
        before = copy.deepcopy(self.destination.accounts)
        report = bridge.run(apply=True)
        self.assertEqual(report['counts'], {'failed': 1})
        self.assertEqual(report['items'][0]['error_code'], 'duplicate_owned_elsewhere')
        self.assertEqual(self.state()['accounts'], {})
        self.assertEqual(self.destination.accounts, before)

    def test_detail_active_refund_blocks_stale_list_candidate(self):
        self.source.job['has_active_refund'] = True
        report = bridge.run(apply=True)
        self.assertEqual(report['counts'], {'failed': 1})
        self.assertEqual(report['items'][0]['error_code'], 'source_no_longer_eligible')
        self.assertEqual(self.destination.accounts, {})
        self.assertEqual(self.destination.account_writes, [])

    def test_incomplete_or_mistyped_detail_never_disables_existing_account(self):
        account = self.import_once()
        original = copy.deepcopy(self.source.job)
        before = (self.root / 'state.json').read_bytes()
        broken = [dict(original, has_active_refund=None), dict(original, is_refunded='false'),
                  {k: v for k, v in original.items() if k not in ('has_active_refund', 'delete_block_reason')}]
        for job in broken:
            with self.subTest(job=job):
                self.source.job = job
                self.destination.calls.clear()
                with self.assertRaisesRegex(bridge.SafeError, '^source_schema_mismatch$'):
                    bridge.run(apply=True)
                self.assertTrue(account['enabled'])
                self.assertEqual(self.destination.account_writes, [])
                self.assertEqual((self.root / 'state.json').read_bytes(), before)

    def test_detail_identity_must_match_requested_job(self):
        with self.assertRaisesRegex(bridge.SafeError, '^source_schema_mismatch$'):
            bridge.checked_job(eligible_job(), detail=True, expected_id='different-job')

    def test_runtime_refund_serialization_is_explicitly_supported(self):
        self.source.job.pop('has_active_refund')
        account = self.import_once()
        self.source.job['delete_block_reason'] = '退款执行中，请完成后删除'
        report = bridge.run(apply=True)
        self.assertEqual(report['counts'].get('disabled_ineligible'), 1)
        self.assertFalse(account['enabled'])

    def test_unknown_runtime_refund_reason_does_not_disable(self):
        account = self.import_once()
        self.source.job.pop('has_active_refund')
        self.source.job['delete_block_reason'] = 'unrecognized future reason'
        self.destination.calls.clear()
        with self.assertRaisesRegex(bridge.SafeError, '^source_schema_mismatch$'):
            bridge.run(apply=True)
        self.assertTrue(account['enabled'])
        self.assertEqual(self.destination.account_writes, [])

    def test_pro20_name_and_access_token_only_credentials(self):
        self.source.job['recharge_plan_type'] = 'pro20'
        account = self.import_once()
        self.assertTrue(account['name'].startswith('Pro20-workbench-'))
        self.assertFalse(account['has_refresh_token'])

    def test_changed_token_uses_stable_id_cas_and_preserves_disabled_state(self):
        account = self.import_once()
        account.update(name='Operator renamed account', enabled=False, concurrency=7)
        expected_version = account['version']
        self.source.token = synthetic_token(int(time.time()) + 7200)
        self.destination.calls.clear()
        report = bridge.run(apply=True)
        self.assertEqual(report['counts'], {'reauthorized': 1})
        writes = self.destination.account_writes
        self.assertEqual(len(writes), 1)
        path, data, method = writes[0]
        self.assertEqual(path, '/api/accounts/' + account['id'] + '/reauthorize')
        self.assertEqual(method, 'POST')
        self.assertEqual(data['version'], expected_version)
        self.assertIs(data['allow_identity_change'], False)
        self.assertNotIn('refresh_token', data['credentials'])
        self.assertFalse(account['enabled'])
        self.assertEqual(account['concurrency'], 7)
        self.assertEqual(account['name'], 'Operator renamed account')
        self.assertEqual(self.state()['accounts'][self.source.job['id']]['token_sha256'], bridge.digest(self.source.token))

    def test_missing_or_conflicting_cas_never_falls_back_to_name_replace(self):
        account = self.import_once()
        original_token = account['_token']
        original_state = (self.root / 'state.json').read_bytes()
        self.source.token = synthetic_token(int(time.time()) + 7200)
        for error in ('http_404', 'http_405', 'http_409'):
            with self.subTest(error=error):
                self.destination.reauthorize_error = error
                self.destination.calls.clear()
                report = bridge.run(apply=True)
                self.assertEqual(report['counts'], {'failed': 1})
                expected = 'credential_update_requires_cas_api' if error in ('http_404', 'http_405') else error
                self.assertEqual(report['items'][0]['error_code'], expected)
                self.assertEqual(account['_token'], original_token)
                self.assertEqual((self.root / 'state.json').read_bytes(), original_state)
                self.assertFalse(any(path == '/api/accounts' for path, _, _ in self.destination.calls))

    def test_changed_token_preview_is_explicitly_unvalidated_and_read_only(self):
        self.import_once()
        original_state = (self.root / 'state.json').read_bytes()
        self.source.token = synthetic_token(int(time.time()) + 7200)
        self.destination.calls.clear()
        report = bridge.run(apply=False)
        self.assertEqual(report['counts'], {'would_reauthorize': 1})
        self.assertEqual(self.destination.account_writes, [])
        self.assertEqual((self.root / 'state.json').read_bytes(), original_state)

    def test_optional_proxy_is_bound_only_during_initialization(self):
        bridge.atomic(self.root / 'state.json', {'schema': 1, 'accounts': {}, 'proxy_id': 'synthetic-proxy'})
        account = self.import_once()
        self.assertEqual(account['proxies'], ['synthetic-proxy'])
        self.destination.calls.clear()
        bridge.run(apply=True)
        self.assertEqual(self.destination.account_writes, [])

    def test_fake_rejects_routes_not_registered_in_actual_admin_source(self):
        self.assertIn('POST /api/accounts', REGISTERED_ROUTES)
        self.assertIn('POST /api/accounts/import/preview', REGISTERED_ROUTES)
        self.assertIn('POST /api/accounts/{name}/reauthorize', REGISTERED_ROUTES)
        with self.assertRaisesRegex(AssertionError, 'does not match admin.go'):
            self.destination.call('/api/accounts/import', {'accounts': []})


if __name__ == '__main__':
    unittest.main(verbosity=2)
