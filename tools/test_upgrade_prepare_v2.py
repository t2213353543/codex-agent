"""V2 frozen packaging contract tests; synthetic fixtures and no remote calls."""
import hashlib
import importlib.util
import json
from pathlib import Path
import sys
import tarfile
import tempfile
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parent))
SPEC = importlib.util.spec_from_file_location('upgrade_packaging_under_test', Path(__file__).with_name('upgrade.py'))
packaging = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(packaging)


class FrozenPackagingTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name).resolve()
        self.workspace = self.root / 'workspace'
        self.source = self.root / 'frozen'
        self.frontend = self.root / 'captured-ui'
        for folder in (self.workspace / 'deploy', self.source / 'migrations', self.frontend / 'assets'):
            folder.mkdir(parents=True)
        actual = Path(__file__).resolve().parents[1]
        (self.workspace / 'deploy/upgrade_remote.py').write_bytes((actual / 'deploy/upgrade_remote.py').read_bytes())
        for name in packaging.remote.TARGET_MIGRATIONS:
            (self.source / name).write_bytes((actual / name).read_bytes())
        for name, body in {'go.mod': b'module synthetic\n', 'go.sum': b'synthetic\n',
                'database.go': b'package main\n', 'main.go': b'package main\n'}.items():
            (self.source / name).write_bytes(body)
        (self.frontend / 'index.html').write_text('<script src="/assets/synthetic.js"></script>')
        (self.frontend / 'assets/synthetic.js').write_text('/* synthetic */')
        self.binary = self.root / 'candidate'
        self.binary.write_bytes(b'\x7fELF\x02\x01' + bytes(12) + b'\x3e\x00' + b'synthetic executable')
        self.sha = lambda path: hashlib.sha256(Path(path).read_bytes()).hexdigest()
        self.snapshot = packaging.backend_manifest(self.source)
        frontend = packaging.remote.file_map(self.frontend)
        migration = {'schema': 'codex-agent-pg-additive-migration-v1', 'status': 'pass',
            'synthetic_only': True, 'scope': 'migration-focused', 'source_sha256': 'c' * 64,
            'migration_runner_sha256': self.snapshot['files']['database.go'],
            'baseline_checksums': packaging.remote.MIGRATIONS, 'target_checksums': packaging.remote.TARGET_MIGRATIONS,
            'checks': dict.fromkeys(packaging.remote.MIGRATION_CHECKS, True)}
        rollback = {'schema': 'codex-agent-pg-rollback-compatibility-v1', 'status': 'pass',
            'synthetic_only': True, 'previous_binary_sha256': 'a' * 64,
            'target_checksums': packaging.remote.TARGET_MIGRATIONS,
            'checks': dict.fromkeys(packaging.remote.ROLLBACK_CHECKS, True)}
        capture = {'schema': 'codex-agent-production-frontend-v1', 'status': 'pass',
            'binary_sha256': 'a' * 64, 'unit_sha256': 'b' * 64, 'frontend': frontend}
        self.files = {}
        for name, body in {'migration': migration, 'rollback': rollback, 'frontend': capture}.items():
            path = self.root / (name + '.json')
            path.write_text(json.dumps(body))
            self.files[name] = path
        acceptance = {'schema': 'codex-agent-pg-upgrade-acceptance-v2', 'status': 'pass',
            'source_sha256': self.snapshot['sha256'], 'binary_sha256': self.sha(self.binary), 'frontend': frontend,
            'migration_evidence_sha256': self.sha(self.files['migration']),
            'rollback_evidence_sha256': self.sha(self.files['rollback']),
            'frontend_evidence_sha256': self.sha(self.files['frontend']),
            'checks': dict.fromkeys(packaging.remote.V2_CHECKS, True)}
        self.files['acceptance'] = self.root / 'acceptance.json'
        self.files['acceptance'].write_text(json.dumps(acceptance))
        self.options = {'acceptance': self.files['acceptance'], 'source_dir': self.source,
            'candidate_binary': self.binary, 'frontend_dir': self.frontend,
            'frontend_evidence': self.files['frontend'], 'migration_evidence': self.files['migration'],
            'rollback_evidence': self.files['rollback']}
        for mocked in (patch.object(packaging, 'ROOT', self.workspace),
                patch.object(packaging, 'connect', side_effect=AssertionError('No SSH in packaging tests')),
                patch.object(packaging.subprocess, 'run', side_effect=AssertionError('Frozen candidates must not be rebuilt'))):
            mocked.start()
            self.addCleanup(mocked.stop)

    def test_builds_exact_captured_frontend_and_all_evidence_without_rebuilding(self):
        manifest_path = packaging.prepare_v2(**self.options)
        manifest = json.loads(manifest_path.read_text())
        self.assertEqual(manifest['source_sha256'], self.snapshot['sha256'])
        self.assertEqual(manifest['binary_sha256'], self.sha(self.binary))
        self.assertEqual(manifest['migration_checksums'], packaging.remote.TARGET_MIGRATIONS)
        with tarfile.open(manifest_path.parent / 'release.tar.gz') as archive:
            names = set(archive.getnames())
            self.assertIn('evidence/rollback-compatibility.json', names)
            self.assertIn('source-manifest.json', names)
            self.assertEqual(archive.extractfile('codex-agent').read(), self.binary.read_bytes())
            frontend = {name.removeprefix('frontend/dist/'): hashlib.sha256(archive.extractfile(name).read()).hexdigest()
                for name in names if name.startswith('frontend/dist/') and archive.getmember(name).isfile()}
            self.assertEqual(frontend, manifest['frontend'])
        self.assertEqual(packaging.prepare_v2(**self.options), manifest_path)

    def test_rejects_shared_tree_or_uncaptured_ui_before_packaging(self):
        for option, value in (('source_dir', self.workspace), ('frontend_dir', self.workspace / 'frontend/dist')):
            with self.subTest(option=option), self.assertRaisesRegex(RuntimeError, 'not the shared tree'):
                packaging.prepare_v2(**{**self.options, option: value})
        (self.frontend / 'assets/unreviewed.js').write_text('unreviewed')
        with self.assertRaises(packaging.remote.UpgradeError):
            packaging.prepare_v2(**self.options)

    def test_rejects_changed_candidate_and_migration_bytes(self):
        original = self.binary.read_bytes()
        self.binary.write_bytes(original + b'changed')
        with self.assertRaises(packaging.remote.UpgradeError):
            packaging.prepare_v2(**self.options)
        self.binary.write_bytes(original)
        (self.source / 'migrations/003_management_cleanup.sql').write_text('SELECT 1;')
        with self.assertRaises(packaging.remote.UpgradeError):
            packaging.prepare_v2(**self.options)

    def test_detects_input_changed_during_packaging_without_publishing(self):
        original_map = packaging.remote.file_map
        count = 0
        def frontend_map(path):
            nonlocal count
            count += 1
            result = original_map(path)
            if count == 2:
                result['index.html'] = '0' * 64
            return result
        with patch.object(packaging.remote, 'file_map', side_effect=frontend_map):
            with self.assertRaisesRegex(RuntimeError, 'changed during v2 packaging'):
                packaging.prepare_v2(**self.options)
        self.assertFalse(any(path.name[0] != '.' for path in (self.workspace / '.local/upgrades').iterdir()))


if __name__ == '__main__':
    unittest.main()
