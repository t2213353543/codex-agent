"""Immutable performance-package validation without SSH or production changes."""
import copy
import importlib.util
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[1]
SPEC = importlib.util.spec_from_file_location('performance_release_test', ROOT / 'deploy/performance_release.py')
release = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(release)
ELF = b'\x7fELF\x02\x01' + bytes(12) + b'\x3e\x00' + b'synthetic ELF fixture'


class PerformanceReleaseTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name).resolve()
        self.root.chmod(0o755)
        self.package = self.root / 'releases' / ('a' * 64)
        self.package.mkdir(parents=True)
        root_patch = patch.object(release.base, 'ROOT', self.root)
        root_patch.start()
        self.addCleanup(root_patch.stop)
        self.write('codex-agent', ELF, 0o755)
        self.write('frontend/dist/index.html', b'unchanged production frontend')
        for name in release.CONTROLLERS | release.MIGRATION_NAMES:
            self.write(name, (ROOT / name).read_bytes())
        self.write('evidence/functional.json', b'{"passed":true,"scope":"management-functional"}')
        self.write('evidence/performance.json', b'{"slo_met":false,"p95_ms":1304.2}')
        self.source = {'files': {name: 'd' * 64 for name in (
            'main.go', 'go.mod', 'go.sum', 'internal/server/main.go',
            'internal/server/database.go', 'migrations/embed.go')}}
        self.source['files'].update({name: release.base.digest(self.package / name)
                                     for name in release.CONTROLLERS | release.MIGRATION_NAMES})
        self.source['sha256'] = release.map_digest(self.source['files'])
        reference = lambda name: {'artifact_path': name, 'sha256': release.base.digest(self.package / name)}
        functional = reference('evidence/functional.json')
        performance = reference('evidence/performance.json')
        self.acceptance = {
            'schema': release.ACCEPTANCE_SCHEMA, 'source_sha256': self.source['sha256'],
            'binary_sha256': release.base.digest(self.package / 'codex-agent'),
            'functional': {'passed': True, 'checks': dict.fromkeys(release.FUNCTIONAL_CHECKS, True),
                           'evidence_files': [functional]},
            'performance': {'slo_met': False, 'remaining': ['proxy resource import exceeds 1s'],
                            'evidence_files': [performance]},
            'frontend': {'artifact_files': {'frontend/dist/index.html': release.base.digest(self.package / 'frontend/dist/index.html')}},
            'evidence_files': [functional, performance]}
        self.manifest = {'schema': release.SCHEMA, 'backend': release.BACKEND, 'version': '0.4.99',
                         'source_sha256': self.source['sha256'],
                         'migration_checksums': {name: self.source['files'][name] for name in release.MIGRATION_NAMES}}
        self.seal()

    def write(self, name, raw, mode=0o644):
        path = self.package / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(raw)
        path.chmod(mode)

    def seal(self):
        self.write('source-manifest.json', json.dumps(self.source).encode())
        self.write('acceptance.json', json.dumps(self.acceptance).encode())
        self.manifest['files'] = {path.relative_to(self.package).as_posix(): release.base.digest(path)
                                  for path in self.package.rglob('*') if path.is_file() and path.name != 'manifest.json'}
        self.manifest['release_id'] = release.release_identity(self.manifest)
        target = self.package.parent / self.manifest['release_id']
        if self.package != target:
            self.package.rename(target)
            self.package = target
        self.write('manifest.json', json.dumps(self.manifest).encode())

    def test_unmet_slo_is_truthful_and_does_not_block_passing_functionality(self):
        with patch.object(release.base, 'schema') as schema, patch.object(release.base, 'command') as command:
            self.assertEqual(release.validate(self.package)['release_id'], self.package.name)
            schema.assert_not_called()
            command.assert_not_called()
        self.assertFalse(self.acceptance['performance']['slo_met'])

    def test_failed_functional_check_is_rejected(self):
        self.acceptance['functional']['checks']['pg'] = False
        self.seal()
        with self.assertRaisesRegex(RuntimeError, 'functional checks'):
            release.validate(self.package)

    def test_missing_functional_evidence_is_rejected(self):
        self.acceptance['functional']['evidence_files'] = []
        self.seal()
        with self.assertRaisesRegex(RuntimeError, 'evidence references missing'):
            release.validate(self.package)

    def test_fabricated_slo_success_with_remaining_work_is_rejected(self):
        self.acceptance['performance']['slo_met'] = True
        self.seal()
        with self.assertRaisesRegex(RuntimeError, 'contradicts'):
            release.validate(self.package)

    def test_false_slo_without_remaining_limits_is_rejected(self):
        self.acceptance['performance']['remaining'] = []
        self.seal()
        with self.assertRaisesRegex(RuntimeError, 'contradicts'):
            release.validate(self.package)

    def test_binary_tamper_is_rejected(self):
        self.write('codex-agent', ELF + b'mutated', 0o755)
        with self.assertRaisesRegex(RuntimeError, 'checksum mismatch'):
            release.validate(self.package)

    def test_omitted_migration_cannot_be_resealed_away(self):
        name = 'migrations/020_request_log_error_class_index.sql'
        del self.manifest['migration_checksums'][name]
        self.seal()
        with self.assertRaisesRegex(RuntimeError, 'exact reviewed 28 migrations'):
            release.validate(self.package)

    def test_sql_bytes_must_match_frozen_source(self):
        self.write('migrations/018_proxy_resource_write_gate.sql', b'SELECT 1;')
        self.seal()
        with self.assertRaisesRegex(RuntimeError, 'migration artifact mismatch'):
            release.validate(self.package)

    def test_controller_must_match_frozen_source(self):
        self.write('deploy/zero_downtime_release.py', b'# stale controller')
        self.seal()
        with self.assertRaisesRegex(RuntimeError, 'controller source mismatch'):
            release.validate(self.package)

    def test_evidence_reference_must_match_packaged_file(self):
        self.acceptance['functional']['evidence_files'][0]['sha256'] = '1' * 64
        self.seal()
        with self.assertRaisesRegex(RuntimeError, 'evidence artifact mismatch'):
            release.validate(self.package)

    def test_untracked_file_is_rejected(self):
        self.write('untracked.txt', b'not immutable')
        with self.assertRaisesRegex(RuntimeError, 'unmanifested'):
            release.validate(self.package)

    def test_identity_binds_manifest_content(self):
        modified = copy.deepcopy(self.manifest)
        modified['version'] = '0.4.100'
        self.write('manifest.json', json.dumps(modified).encode())
        with self.assertRaisesRegex(RuntimeError, 'identity mismatch'):
            release.validate(self.package)

    def test_source_map_digest_must_match_its_contents(self):
        self.source['files']['internal/server/main.go'] = 'e' * 64
        self.seal()
        with self.assertRaisesRegex(RuntimeError, 'source identity mismatch'):
            release.validate(self.package)

    def test_duplicate_manifest_members_are_rejected(self):
        original = (self.package / 'manifest.json').read_text()
        self.write('manifest.json', ('{"backend":"forged",' + original[1:]).encode())
        with self.assertRaisesRegex(RuntimeError, 'duplicate JSON field'):
            release.validate(self.package)

    def test_artifact_symlink_is_rejected(self):
        artifact = self.package / 'frontend/dist/index.html'
        target = self.root / 'outside.html'
        target.write_bytes(artifact.read_bytes())
        artifact.unlink()
        try:
            artifact.symlink_to(target)
        except OSError:
            self.skipTest('symlink creation is not available')
        with self.assertRaises(RuntimeError):
            release.validate(self.package)


if __name__ == '__main__':
    unittest.main()
