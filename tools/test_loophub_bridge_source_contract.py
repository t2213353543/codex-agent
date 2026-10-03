"""Use the actual source CPA health predicate without importing its application.

python tools/test_loophub_bridge_source_contract.py
Optional LOOPHUB_SOURCE points at a read-only copy of the runtime team_rotation.py.
No networking, OAuth, database writes, application imports, or production calls.
"""
import ast
import hashlib
import json
import os
from pathlib import Path
from types import SimpleNamespace
from typing import Any
import unittest

import test_loophub_bridge_quality as fixture


SOURCE = Path(os.environ.get('LOOPHUB_SOURCE', str(Path(__file__).resolve().parents[2]
    / 'loophub-mother-checkout-20260920/webui/team_rotation.py')))


class SourceDeliveryContract(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        tree = ast.parse(SOURCE.read_text(encoding='utf-8-sig'))
        wanted = {'_destination_clone_accounts_healthy', '_destination_sync_hash'}
        nodes = [node for node in ast.walk(tree)
                 if isinstance(node, ast.FunctionDef) and node.name in wanted]
        if {node.name for node in nodes} != wanted or len(nodes) != 2:
            raise AssertionError('source CPA predicate missing or ambiguous')
        cls.code = compile(ast.fix_missing_locations(ast.Module(body=nodes, type_ignores=[])),
                           str(SOURCE), 'exec')

    def setUp(self):
        self.fixture = fixture.CredentialState()
        self.fixture.setUp()
        self.addCleanup(self.fixture.doCleanups)
        self.events = []
        self.remote = None
        self.scope = {'Any': Any, 'json': json, 'hashlib': hashlib,
            'db': SimpleNamespace(list_team_rotation_sub2api_accounts=lambda *_: [self.mapping],
                                 add_team_rotation_event=lambda *a, **k: self.events.append((a, k))),
            'exporter': SimpleNamespace(get_cpa_auth_file=lambda *_: self.remote)}
        exec(self.code, self.scope)
        self.destination = {'id': 'synthetic-destination', 'platform': 'cpa'}
        self.mapping = {'status': 'active', 'remote_account_id': 'synthetic.json',
            'credential_hash': self.scope['_destination_sync_hash']('synthetic-hash', self.destination)}
        self.owner = {'id': 'synthetic-owner', 'workspace_id': 'synthetic-workspace'}
        self.manager = SimpleNamespace(_destination_platform=lambda _: 'cpa', _destination_cfg=lambda _: {})

    def healthy(self):
        return self.scope['_destination_clone_accounts_healthy'](self.manager, self.owner,
            'synthetic@example.invalid', self.destination, 'synthetic-hash')

    def test_transient_request_failure_preserves_source_delivery(self):
        for status in (403, 429, 502, 503):
            with self.subTest(status=status):
                self.fixture.api.account.update(status='error', credential_status='refresh_failed',
                    last_error=f'upstream returned HTTP {status}')
                self.fixture.snapshot(http_code=status)
                self.remote = self.fixture.listing()
                self.assertTrue(self.healthy())
                self.assertFalse(self.remote['needs_reauth'])

    def test_old_unavailable_shape_provably_triggers_reupload(self):
        self.remote = self.fixture.listing()
        self.remote.update(status='error', disabled=True, unavailable=True,
                           status_message='destination_unavailable')
        self.assertFalse(self.healthy())

    def test_optional_snapshot_outage_preserves_source_delivery(self):
        self.fixture.api.error = True
        self.remote = self.fixture.listing()
        self.assertTrue(self.healthy())
        self.assertEqual(self.remote['status_message'], 'credential_observation_unavailable')

    def test_confirmed_401_remains_unhealthy(self):
        self.fixture.snapshot(http_code=401)
        self.remote = self.fixture.listing()
        self.assertTrue(self.remote['needs_reauth'])
        self.assertFalse(self.healthy())

    def test_disabled_delivery_remains_unhealthy(self):
        self.fixture.api.account['enabled'] = False
        self.remote = self.fixture.listing()
        self.assertFalse(self.healthy())

    def test_credential_replacement_during_observation_avoids_reupload(self):
        self.fixture.snapshot(http_code=401)
        self.fixture.api.change_after_snapshot = {'version': 10}
        self.remote = self.fixture.listing()
        self.assertTrue(self.healthy())
        self.assertFalse(self.remote['needs_reauth'])


if __name__ == '__main__':
    unittest.main(verbosity=2)
