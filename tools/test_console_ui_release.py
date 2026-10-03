"""Local rejection tests for the pinned 47c80 UI-only release boundary."""
from __future__ import annotations

import copy
import hashlib
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import ui_release as ui

remote = ui.remote


class ConsoleUIReleaseTests(unittest.TestCase):
    def evidence(self, production=True):
        binding = {'frontend_source_sha256': 'f' * 64, 'backend_source_sha256': 'b' * 64,
                   'dist_sha256': remote.object_sha({'index.html': 'd' * 64})}
        readonly = {'synthetic_data': False, 'data_mode': 'production-readonly',
                    'business_writes': 0, 'gateway_requests': 0, 'session_only_mutations': True,
                    'backend_binary_sha256': remote.BINARY_SHA}
        checks = [{'id': name, 'status': 'pass', **binding,
                   'artifacts': [{'path': name + '.json', 'sha256': 'a' * 64}]}
                  for name in sorted(remote.CHECKS)]
        real = next(check for check in checks if check['id'] == 'real_api')
        real.update(readonly if production else {'synthetic_data': True,
                                                'backend_binary_sha256': remote.BINARY_SHA})
        report = {'schema': 'codex-agent-ui-acceptance-v1', 'status': 'pass', **binding, 'checks': checks}
        manifest = {'frontend_source': {'sha256': 'f' * 64}, 'backend_source': {'sha256': 'b' * 64},
                    'frontend': {'index.html': 'd' * 64},
                    'evidence_artifacts': {name + '.json': {'sha256': 'a' * 64} for name in remote.CHECKS}}
        artifact = {'status': 'pass', **binding, **readonly}
        return report, manifest, real, artifact

    def test_existing_nine_state_machine_simulations(self):
        report = remote.self_test()
        self.assertEqual(report['status'], 'pass')
        self.assertEqual(len(report['checks']), 9)
        self.assertTrue(all(check['status'] == 'pass' for check in report['checks']))

    def test_synthetic_real_api_still_accepted(self):
        report, manifest, _, _ = self.evidence(production=False)
        remote.evidence_valid(report, manifest)

    def test_production_readonly_requires_bound_real_report(self):
        report, manifest, _, artifact = self.evidence()
        remote.evidence_valid(report, manifest, lambda key: json.dumps(artifact))
        with self.assertRaisesRegex(remote.UIReleaseError, 'hash-bound report'):
            remote.evidence_valid(report, manifest)

    def test_wrong_binary_rejected_for_both_real_api_modes(self):
        for production in (False, True):
            with self.subTest(production=production):
                report, manifest, real, artifact = self.evidence(production)
                real['backend_binary_sha256'] = '0' * 64
                with self.assertRaisesRegex(remote.UIReleaseError, 'pinned deployed'):
                    remote.evidence_valid(report, manifest, lambda key: json.dumps(artifact))

    def test_production_mutations_and_gateway_calls_rejected(self):
        changes = {'business_writes': 1, 'gateway_requests': 1, 'session_only_mutations': False,
                   'data_mode': 'production', 'synthetic_data': None}
        for key, value in changes.items():
            with self.subTest(field=key):
                report, manifest, real, artifact = self.evidence()
                real[key] = value
                with self.assertRaisesRegex(remote.UIReleaseError, 'read-only data'):
                    remote.evidence_valid(report, manifest, lambda name: json.dumps(artifact))

    def test_zero_counts_must_be_explicit_integers(self):
        for field in ('business_writes', 'gateway_requests'):
            for value in (False, '0', None):
                with self.subTest(field=field, value=value):
                    report, manifest, real, artifact = self.evidence()
                    real[field] = value
                    with self.assertRaises(remote.UIReleaseError):
                        remote.evidence_valid(report, manifest, lambda name: json.dumps(artifact))

    def test_mutating_or_unbound_artifact_cannot_hide_behind_passing_summary(self):
        for field, value in {'business_writes': 1, 'gateway_requests': 1, 'session_only_mutations': False,
                             'dist_sha256': '0' * 64, 'backend_source_sha256': '0' * 64,
                             'frontend_source_sha256': '0' * 64,
                             'backend_binary_sha256': '0' * 64, 'status': 'fail'}.items():
            with self.subTest(field=field):
                report, manifest, _, artifact = self.evidence()
                artifact[field] = value
                with self.assertRaisesRegex(remote.UIReleaseError, 'No passing production'):
                    remote.evidence_valid(report, manifest, lambda name: json.dumps(artifact))

    def test_unbound_source_or_artifact_hash_rejected(self):
        report, manifest, _, artifact = self.evidence()
        report['backend_source_sha256'] = '0' * 64
        with self.assertRaisesRegex(remote.UIReleaseError, 'binding is stale'):
            remote.evidence_valid(report, manifest, lambda name: json.dumps(artifact))
        report, manifest, real, artifact = self.evidence()
        real['artifacts'][0]['sha256'] = '0' * 64
        with self.assertRaisesRegex(remote.UIReleaseError, 'not hash-bound'):
            remote.evidence_valid(report, manifest, lambda name: json.dumps(artifact))

    def test_false_synthetic_marker_cannot_bypass_production_mode(self):
        report, manifest, real, artifact = self.evidence()
        real['synthetic_data'] = True
        with self.assertRaisesRegex(remote.UIReleaseError, 'read-only data'):
            remote.evidence_valid(report, manifest, lambda name: json.dumps(artifact))

    def ui_validation_evidence(self):
        report, manifest, real, artifact = self.evidence()
        contract = {'data_mode': 'production-ui-validation', 'session_only_mutations': False,
                    'allowed_post_paths': sorted(remote.UI_VALIDATION_POST_PATHS),
                    'ephemeral_snapshot_writes': 1}
        real.update(contract)
        artifact.update(contract)
        artifact['requests'] = [
            {'method': 'POST', 'path': '/api/session', 'status': 200},
            {'method': 'GET', 'path': '/api/accounts', 'status': 200},
            {'method': 'POST', 'path': '/api/accounts/last-used', 'status': 200},
            {'method': 'POST', 'path': '/api/accounts/usage-batch', 'status': 200},
            {'method': 'POST', 'path': '/api/logs/snapshot', 'status': 200},
            {'method': 'DELETE', 'path': '/api/session', 'status': 200},
        ]
        return report, manifest, real, artifact

    def test_bounded_production_ui_validation_accepts_query_posts(self):
        report, manifest, _, artifact = self.ui_validation_evidence()
        remote.evidence_valid(report, manifest, lambda name: json.dumps(artifact))

    def test_ui_validation_rejects_business_writes_gateway_and_other_mutations(self):
        for method, path in [('POST', '/api/accounts'), ('PUT', '/api/settings'),
                             ('PATCH', '/api/accounts/a1'), ('DELETE', '/api/accounts/a1'),
                             ('POST', '/api/accounts/a1/refresh'), ('POST', '/v1/responses'),
                             ('GET', '/v1/models'), ('GET', '/api/../v1/models'),
                             ('POST', '/api/logs/snapshot?unexpected=true')]:
            with self.subTest(method=method, path=path):
                report, manifest, _, artifact = self.ui_validation_evidence()
                artifact['requests'].append({'method': method, 'path': path, 'status': 200})
                with self.assertRaisesRegex(remote.UIReleaseError, 'No passing production'):
                    remote.evidence_valid(report, manifest, lambda name: json.dumps(artifact))

    def test_ui_validation_snapshot_counter_matches_requests_and_summary(self):
        for target in ('summary', 'artifact', 'requests'):
            with self.subTest(target=target):
                report, manifest, real, artifact = self.ui_validation_evidence()
                if target == 'summary':
                    real['ephemeral_snapshot_writes'] = 2
                elif target == 'artifact':
                    artifact['ephemeral_snapshot_writes'] = 2
                else:
                    artifact['requests'] = []
                with self.assertRaisesRegex(remote.UIReleaseError, 'No passing production'):
                    remote.evidence_valid(report, manifest, lambda name: json.dumps(artifact))

    def test_ui_validation_allowlist_cannot_expand(self):
        report, manifest, real, artifact = self.ui_validation_evidence()
        real['allowed_post_paths'].append('/api/settings')
        with self.assertRaisesRegex(remote.UIReleaseError, 'bounded UI validation'):
            remote.evidence_valid(report, manifest, lambda name: json.dumps(artifact))

    def baseline_fixture(self, folder, *, bad_source_digest=False):
        files = {'main.go': '1' * 64, 'go.mod': '2' * 64, 'go.sum': '3' * 64,
                 'migrations/000_core.sql': '4' * 64, 'native-transport/tests/probe.go': '5' * 64}
        digest = hashlib.sha256(json.dumps(files, sort_keys=True, separators=(',', ':')).encode()).hexdigest()
        if bad_source_digest:
            digest = '0' * 64
        source = {'schema': 'codex-agent-go-only-source-v1', 'sha256': digest, 'files': files}
        acceptance = {'schema': 'codex-agent-full-release-acceptance-v1', 'passed': True,
                      'source_sha256': digest, 'binary_sha256': remote.BINARY_SHA}
        (folder / 'evidence').mkdir()
        source_path = folder / 'evidence/source-manifest.json'
        source_path.write_text(json.dumps(source), encoding='utf-8')
        report_path = folder / 'evidence/acceptance.json'
        report_path.write_text(json.dumps(acceptance), encoding='utf-8')
        manifest = {'schema': 'codex-agent-full-release-v1', 'source_sha256': digest,
                    'binary_sha256': remote.BINARY_SHA,
                    'files': {'evidence/source-manifest.json': remote.sha(source_path),
                              'evidence/acceptance.json': remote.sha(report_path)}}
        release_id = remote.object_sha(manifest)
        manifest['release_id'] = release_id
        manifest_path = folder / 'full-release.json'
        manifest_path.write_text(json.dumps(manifest), encoding='utf-8')
        return manifest_path, release_id, files

    def test_full_release_baseline_checks_complete_file_map(self):
        with tempfile.TemporaryDirectory() as directory:
            folder = Path(directory)
            manifest, release_id, files = self.baseline_fixture(folder)
            with patch.object(ui, 'BASELINE', manifest), patch.object(remote, 'BASELINE_ID', release_id), \
                    patch.object(ui, 'backend_source', return_value={'files': files}):
                path, report = ui.baseline_report(None)
                self.assertEqual(path, folder / 'evidence/acceptance.json')
                self.assertTrue(report['passed'])
                altered = copy.deepcopy(files)
                altered['native-transport/tests/probe.go'] = 'f' * 64
                with patch.object(ui, 'backend_source', return_value={'files': altered}):
                    with self.assertRaisesRegex(RuntimeError, 'native-transport/tests/probe.go'):
                        ui.baseline_report(None)

    def test_full_release_unbound_source_manifest_rejected(self):
        with tempfile.TemporaryDirectory() as directory:
            folder = Path(directory)
            manifest, release_id, files = self.baseline_fixture(folder)
            with patch.object(ui, 'BASELINE', manifest), patch.object(remote, 'BASELINE_ID', release_id), \
                    patch.object(ui, 'backend_source', return_value={'files': files}):
                source = folder / 'evidence/source-manifest.json'
                source.write_text(source.read_text(encoding='utf-8') + '\n', encoding='utf-8')
                with self.assertRaisesRegex(RuntimeError, 'source manifest is absent or not bound'):
                    ui.baseline_report(None)

    def test_source_digest_must_bind_actual_mapping(self):
        with tempfile.TemporaryDirectory() as directory:
            manifest, release_id, files = self.baseline_fixture(Path(directory), bad_source_digest=True)
            with patch.object(ui, 'BASELINE', manifest), patch.object(remote, 'BASELINE_ID', release_id), \
                    patch.object(ui, 'backend_source', return_value={'files': files}):
                with self.assertRaisesRegex(RuntimeError, 'does not bind its file map'):
                    ui.baseline_report(None)

    def test_full_release_wrong_binary_or_changed_identity_rejected(self):
        with tempfile.TemporaryDirectory() as directory:
            manifest, release_id, files = self.baseline_fixture(Path(directory))
            with patch.object(ui, 'BASELINE', manifest), patch.object(remote, 'BASELINE_ID', release_id), \
                    patch.object(ui, 'backend_source', return_value={'files': files}):
                original = json.loads(manifest.read_text(encoding='utf-8'))
                altered = {**original, 'binary_sha256': '0' * 64}
                manifest.write_text(json.dumps(altered), encoding='utf-8')
                with self.assertRaisesRegex(RuntimeError, 'missing or changed'):
                    ui.baseline_report(None)
                manifest.write_text(json.dumps({**original, 'source_sha256': '0' * 64}), encoding='utf-8')
                with self.assertRaisesRegex(RuntimeError, 'identity mismatch'):
                    ui.baseline_report(None)


if __name__ == '__main__':
    unittest.main(verbosity=2)
