"""Synthetic lifecycle, credential boundaries, and local HTTP verification."""
import base64
import copy
from http.server import ThreadingHTTPServer
import json
from pathlib import Path
import tempfile
import threading
import time
import unittest
import urllib.error
import urllib.request

import quality_push_receiver as q


def token(workspace='workspace1', user='user1', email='test1@example.test', exp=None, iat=None):
    now = int(time.time())
    claims = {'exp': exp or now + 7200, 'iat': iat or now - 120,
        'https://api.openai.com/auth': {'chatgpt_account_id': workspace,
            'chatgpt_user_id': user, 'chatgpt_plan_type': 'team'},
        'https://api.openai.com/profile': {'email': email}}
    return 'e30.' + base64.urlsafe_b64encode(json.dumps(claims).encode()).decode().rstrip('=') + '.synthetic'


class FakeSource:
    def __init__(self):
        self.runtime = {'container_id': 'synthetic', 'image_id': 'synthetic'}
        self.tokens = {'test1@example.test': token(), 'test2@example.test': token('workspace2', 'user2', 'test2@example.test')}
        self.owners = [{'id': 'owner' + str(n), 'workspace_id': 'workspace' + str(n), 'status': 'active',
                        'active_children': 1, 'destination_ids': ['original' + str(n)]} for n in (1, 2)]
        self.children = [{'owner_id': 'owner' + str(n), 'email': 'test' + str(n) + '@example.test',
            'status': 'active', 'oauth_reauth_status': 'success', 'user_id': 'user' + str(n),
            'assigned_at': 1000 + n, 'oauth_reauth_updated_at': 1100 + n} for n in (1, 2)]
        self.destinations = []
        self.writes = []

    def call(self, path, data=None, method=None):
        if path == '/api/team-rotation/status':
            return copy.deepcopy({'ok': True, 'config': {'enabled': True}, 'owners': self.owners,
                                  'children': self.children, 'destinations': self.destinations})
        if path.startswith('/api/registered/'):
            email = urllib.parse.unquote(path.rsplit('/', 1)[1])
            return {'data': {'access_token': self.tokens[email], 'refresh_token': 'source-only'}}
        self.writes.append((path, data, method))
        if path == '/api/team-rotation/sub2api-destinations':
            result = {**data, 'id': 'temporary-destination'}
            self.destinations.append(result)
            return {'ok': True, 'item': result}
        if path.startswith('/api/team-rotation/owners/'):
            for owner in self.owners:
                if owner['id'] == path.rsplit('/', 1)[1]:
                    owner['destination_ids'] = data['destination_ids']
                    return {'ok': True}
        if path.startswith('/api/team-rotation/sub2api-destinations/') and method == 'DELETE':
            self.destinations = [d for d in self.destinations if d['id'] != path.rsplit('/', 1)[1]]
            return {'ok': True}
        raise AssertionError('unexpected management operation')


class QualityReceiverTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name) / 'private-run'
        self.config = {'schema': 1, 'run_id': 'test-quality-20260924', 'state_dir': str(self.root),
            'listen_host': '172.19.0.1', 'listen_port': 18882, 'source_container': 'loophub-multitenant-app',
            'owners': [{'owner_id': 'owner' + str(n), 'workspace_id': 'workspace' + str(n),
                        'alias': 'q' + str(n)} for n in (1, 2)]}
        self.source = FakeSource()
        q.prepare(self.config, self.source)
        self.receiver = q.Receiver(self.config)

    def tearDown(self):
        self.temp.cleanup()

    def payload(self, value=None):
        return {'email': 'test1@example.test', 'type': 'codex', 'account_id': 'workspace1',
                'access_token': value or self.source.tokens['test1@example.test'],
                'refresh_token': 'must-never-be-stored', 'id_token': 'must-never-be-stored'}

    def test_prepare_is_readonly_and_snapshots_redacted(self):
        self.assertEqual(self.source.writes, [])
        summary = json.dumps(q.snapshot(self.config))
        self.assertNotIn('@', summary)
        self.assertNotIn('access_token', summary)
        self.assertNotIn('owner1', summary)
        self.assertIn('credential_file', summary)

    def test_full_append_restore_preserves_other_destinations_and_erases_at(self):
        q.configure(self.config, self.source, lambda *_: None)
        for owner in self.source.owners:
            self.assertEqual(len(owner['destination_ids']), 2)
        self.receiver.upload('test1@example.test.json', self.payload())
        self.source.owners[0]['destination_ids'].append('concurrent-extra')
        q.restore(self.config, self.source)
        self.assertEqual(self.source.owners[0]['destination_ids'], ['original1', 'concurrent-extra'])
        self.assertEqual(self.source.owners[1]['destination_ids'], ['original2'])
        self.assertFalse((self.root / 'credentials/q1.json').exists())
        self.assertEqual(q.restore(self.config, self.source)['phase'], 'restored')
        with self.assertRaisesRegex(q.ReceiverError, 'no_longer_accepts'):
            self.receiver.upload('test1@example.test.json', self.payload())

    def test_at_only_latest_upload_and_stale_rejection(self):
        self.receiver.upload('test1@example.test.json', self.payload())
        newer = token(iat=int(time.time()) - 30, exp=int(time.time()) + 8000)
        self.receiver.upload('test1@example.test.json', self.payload(newer))
        stored = q.read_private(self.root / 'credentials/q1.json')
        self.assertEqual(stored['access_token'], newer)
        self.assertNotIn('refresh_token', stored)
        self.assertNotIn('id_token', stored)
        with self.assertRaisesRegex(q.ReceiverError, 'stale_token'):
            self.receiver.upload('test1@example.test.json', self.payload())

    def test_newer_issue_with_shorter_lifetime_is_accepted(self):
        self.receiver.upload('test1@example.test.json', self.payload())
        newer = token(iat=int(time.time()) - 30, exp=int(time.time()) + 3000)
        self.receiver.upload('test1@example.test.json', self.payload(newer))
        self.assertEqual(q.read_private(self.root / 'credentials/q1.json')['access_token'], newer)

    def test_workspace_user_email_and_unknown_child_rejected(self):
        for value in (token(workspace='workspace2'), token(user='different'), token(email='wrong@example.test')):
            with self.assertRaises(q.ReceiverError):
                self.receiver.upload('test1@example.test.json', self.payload(value))
        with self.assertRaises(q.ReceiverError):
            self.receiver.upload('other@example.test.json', {**self.payload(), 'email': 'other@example.test'})

    def test_invalid_jwt_and_expiry_rejected(self):
        for value in ('invalid', token(exp=int(time.time()) - 1), token(iat=int(time.time()) + 900)):
            with self.assertRaises(q.ReceiverError):
                self.receiver.upload('test1@example.test.json', self.payload(value))

    def test_status_integrity_and_withdraw_without_credentials(self):
        self.receiver.upload('test1@example.test.json', self.payload())
        status = self.receiver.list_files('test1@example.test.json')
        self.assertEqual(status['files'][0]['status'], 'active')
        self.assertNotIn('access_token', json.dumps(status))
        self.assertNotIn('user1', json.dumps(status))
        self.assertEqual(self.receiver.list_files()['files'], [])
        self.receiver.withdraw('test1@example.test.json')
        self.assertEqual(self.receiver.list_files('test1@example.test.json')['files'], [])
        self.receiver.upload('test1@example.test.json', self.payload())
        self.assertFalse(q.snapshot(self.config)['accounts'][0]['withdrawn'])

    def test_configuration_rejects_changed_original_binding(self):
        self.source.owners[0]['destination_ids'] = ['changed']
        with self.assertRaisesRegex(q.ReceiverError, 'bindings_changed'):
            q.configure(self.config, self.source, lambda *_: None)
        self.assertEqual(self.source.writes, [])

    def test_configuration_rejects_child_assignment_change(self):
        self.source.children[0]['assigned_at'] += 10
        with self.assertRaisesRegex(q.ReceiverError, 'assignment_changed'):
            q.configure(self.config, self.source, lambda *_: None)
        self.assertEqual(self.source.writes, [])

    def test_recover_unknown_create_response_without_second_destination(self):
        original = self.source.call
        failed = False

        def lose_response(path, data=None, method=None):
            nonlocal failed
            result = original(path, data, method)
            if path == '/api/team-rotation/sub2api-destinations' and not failed:
                failed = True
                raise q.ReceiverError('synthetic_response_lost')
            return result

        self.source.call = lose_response
        with self.assertRaisesRegex(q.ReceiverError, 'response_lost'):
            q.configure(self.config, self.source, lambda *_: None)
        self.assertEqual(q.load_control(self.config)['phase'], 'creating_destination')
        q.configure(self.config, self.source, lambda *_: None)
        self.assertEqual(len(self.source.destinations), 1)
        writes = len(self.source.writes)
        q.configure(self.config, self.source, lambda *_: None)
        self.assertEqual(len(self.source.writes), writes)

    def test_changed_at_file_is_never_reported_ready(self):
        self.receiver.upload('test1@example.test.json', self.payload())
        file = self.root / 'credentials/q1.json'
        value = q.read_private(file)
        value['access_token'] = 'tampered'
        q.write_private(file, value)
        self.assertEqual(self.receiver.list_files('test1@example.test.json')['files'][0]['status'], 'error')

    def test_restore_refuses_unselected_owner_binding(self):
        q.configure(self.config, self.source, lambda *_: None)
        self.source.owners.append({'id': 'outsider', 'destination_ids': ['temporary-destination']})
        before = len(self.source.writes)
        with self.assertRaisesRegex(q.ReceiverError, 'unselected_owner'):
            q.restore(self.config, self.source)
        self.assertEqual(len(self.source.writes), before)

    def test_config_scope_and_private_listener(self):
        file = Path(self.temp.name) / 'config.json'
        for host in ('0.0.0.0', '127.0.0.1', '8.8.8.8', 'localhost'):
            file.write_text(json.dumps({**self.config, 'listen_host': host}))
            with self.assertRaisesRegex(q.ReceiverError, 'rfc1918'):
                q.load_config(file)
        with self.assertRaisesRegex(q.ReceiverError, 'scope_changed'):
            q.snapshot({**self.config, 'listen_port': 18883})

    def test_management_peer_allows_local_health_but_rejects_public(self):
        for address in ('127.0.0.1', '::1', '172.19.0.3', '10.0.0.2', '192.168.1.3'):
            self.assertTrue(q.management_peer_allowed(address))
        for address in ('8.8.8.8', '0.0.0.0', '169.254.1.1', 'hostname', '100.64.0.1'):
            self.assertFalse(q.management_peer_allowed(address))

    def test_exact_peer_exception_is_scoped_and_defaults_are_compatible(self):
        self.assertTrue(q.management_peer_allowed('203.0.113.10', ['203.0.113.10']))
        self.assertFalse(q.management_peer_allowed('203.0.113.11', ['203.0.113.10']))
        self.assertFalse(q.management_peer_allowed('203.0.113.10'))
        self.assertEqual(q.config_scope(self.config), q.config_scope({**self.config, 'allowed_peer_ips': []}))
        with self.assertRaisesRegex(q.ReceiverError, 'scope_changed'):
            q.snapshot({**self.config, 'allowed_peer_ips': ['203.0.113.10']})
        file = Path(self.temp.name) / 'config.json'
        file.write_text(json.dumps({**self.config, 'allowed_peer_ips': ['203.0.113.10']}))
        parsed = q.load_config(file)
        self.assertEqual(q.config_scope(parsed)['allowed_peer_ips'], ['203.0.113.10'])

    def test_exact_peer_configuration_rejects_networks_and_invalid_addresses(self):
        file = Path(self.temp.name) / 'config.json'
        for peers in (['0.0.0.0'], ['::'], ['224.0.0.1'], ['ff02::1'], ['203.0.113.0/24'],
                      ['hostname'], ['fe80::1%eth0'], [1], None, ['1.1.1.1'] * 2,
                      ['1.1.1.' + str(n) for n in range(1, 6)]):
            file.write_text(json.dumps({**self.config, 'allowed_peer_ips': peers}))
            with self.assertRaises(q.ReceiverError):
                q.load_config(file)

    def test_http_auth_multipart_and_bounded_errors(self):
        # Real loopback health uses the same mandatory key as Docker clients.
        with self.subTest('authenticated_loopback_management'):
            server = ThreadingHTTPServer(('127.0.0.1', 0), q.handler_for(self.receiver))
            thread = threading.Thread(target=server.serve_forever, daemon=True)
            thread.start()
            target = 'http://127.0.0.1:' + str(server.server_port) + '/v0/management/auth-files'
            try:
                with self.assertRaises(urllib.error.HTTPError) as error:
                    urllib.request.urlopen(target)
                self.assertEqual(error.exception.code, 401)
                error.exception.close()
                body = (b'--TEST\r\nContent-Disposition: form-data; name="file"; '
                        b'filename="test1@example.test.json"\r\nContent-Type: application/json\r\n\r\n'
                        + json.dumps(self.payload()).encode() + b'\r\n--TEST--\r\n')
                request = urllib.request.Request(target, body, headers={
                    'Authorization': 'Bearer ' + self.receiver.key,
                    'Content-Type': 'multipart/form-data; boundary=TEST'})
                with urllib.request.urlopen(request) as response:
                    self.assertTrue(json.load(response)['ok'])
                request = urllib.request.Request(target + '?name=test1%40example.test.json', headers={
                    'X-Management-Key': self.receiver.key})
                with urllib.request.urlopen(request) as response:
                    self.assertEqual(json.load(response)['files'][0]['status'], 'active')
            finally:
                server.shutdown()
                server.server_close()
                thread.join()


if __name__ == '__main__':
    unittest.main()
