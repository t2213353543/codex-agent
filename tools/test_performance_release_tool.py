"""Package/upload tests use a fake Go build and local temporary directories."""
import argparse
import contextlib
import copy
import importlib.util
import io
import json
from pathlib import Path
import shutil
import sys
import tarfile
import tempfile
import types
import unittest
from unittest.mock import Mock, patch

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / 'tools'))
try:
    SPEC = importlib.util.spec_from_file_location('performance_prepare_test', ROOT / 'tools/performance_release.py')
    tool = importlib.util.module_from_spec(SPEC)
    SPEC.loader.exec_module(tool)
finally:
    sys.path.pop(0)
ELF = b'\x7fELF\x02\x01' + bytes(12) + b'\x3e\x00' + b'local mock build'


class PerformancePrepareTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name).resolve()
        self.root.chmod(0o755)
        for name in tool.CONTROLLERS | tool.executor.MIGRATION_NAMES:
            self.write(name, (ROOT / name).read_bytes())
        for name in ('main.go', 'go.mod', 'go.sum', 'internal/server/database.go', 'migrations/embed.go'):
            self.write(name, b'synthetic source')
        self.write('internal/server/main.go', b'package server\nconst version = "0.4.99"\n')
        self.write('frontend/dist/index.html', b'unchanged frontend build')
        self.write('.local/evidence/functional.log', b'237 tests passed, no failures')
        self.write('.local/evidence/performance.json', b'{"p95_ms":1576,"slo_met":false}')
        self.source = {'files': {p.relative_to(self.root).as_posix(): tool.sha(p)
                                  for p in self.root.rglob('*') if p.is_file()
                                  and '.local' not in p.parts and 'dist' not in p.parts}}
        self.source['sha256'] = tool.executor.map_digest(self.source['files'])
        ref = lambda name: {'path': name, 'sha256': tool.sha(self.root / name)}
        self.verification = {'schema': tool.VERIFICATION_SCHEMA, 'source_sha256': self.source['sha256'],
            'functional': {'passed': True, 'checks': {'go': True, 'vet': True, 'pg': True, 'frontend': True},
                'scope': 'explicit selected functional suite', 'evidence_files': [ref('.local/evidence/functional.log')]},
            'performance': {'slo_met': False, 'scope': 'measured 10M fixture',
                'remaining': ['proxy import remains over 1s'], 'evidence_files': [ref('.local/evidence/performance.json')]}}
        self.verification_path = self.root / '.local/verification.json'
        self.write('.local/verification.json', json.dumps(self.verification).encode())
        self.args = argparse.Namespace(verification=str(self.verification_path), output=None)
        self.stack = contextlib.ExitStack()
        self.addCleanup(self.stack.close)
        self.stack.enter_context(patch.object(tool, 'ROOT', self.root))
        self.stack.enter_context(patch.object(tool, 'source_manifest', side_effect=lambda: copy.deepcopy(self.source)))
        self.build = self.stack.enter_context(patch.object(tool.subprocess, 'run', side_effect=self.fake_build))

    def write(self, name, raw):
        path = self.root / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(raw)
        path.chmod(0o644)

    def fake_build(self, command, **kwargs):
        self.assertEqual(command[:4], ['go', 'build', '-trimpath', '-ldflags=-s -w'])
        self.assertEqual(kwargs['env']['GOOS'], 'linux')
        self.assertEqual(kwargs['env']['GOARCH'], 'amd64')
        Path(command[command.index('-o') + 1]).write_bytes(ELF)
        return types.SimpleNamespace(returncode=0)

    def save_verification(self):
        self.write('.local/verification.json', json.dumps(self.verification).encode())

    def test_prepare_binds_current_source_artifacts_evidence_and_truthful_slo(self):
        path = tool.prepare(self.args)
        _, manifest, archive, expected = tool.load_package(path)
        self.assertTrue(archive.is_file())
        self.assertEqual(set(manifest['migration_checksums']), tool.executor.MIGRATION_NAMES)
        acceptance = json.loads((path.parent / 'acceptance.json').read_text())
        self.assertFalse(acceptance['performance']['slo_met'])
        self.assertEqual(acceptance['functional']['scope'], 'explicit selected functional suite')
        self.assertEqual(len(acceptance['evidence_files']), 3)
        self.assertIn('deploy/reliability_release.py', expected)
        self.assertEqual(self.build.call_count, 1)

    def test_stale_source_is_rejected_before_build(self):
        self.verification['source_sha256'] = 'a' * 64
        self.save_verification()
        with self.assertRaisesRegex(RuntimeError, 'source is stale'):
            tool.prepare(self.args)
        self.build.assert_not_called()

    def test_failed_frontend_gate_is_rejected(self):
        self.verification['functional']['checks']['frontend'] = False
        self.save_verification()
        with self.assertRaisesRegex(RuntimeError, 'functional checks'):
            tool.prepare(self.args)
        self.build.assert_not_called()

    def test_changed_evidence_is_rejected_before_build(self):
        self.write('.local/evidence/functional.log', b'changed after verification')
        with self.assertRaisesRegex(RuntimeError, 'evidence changed'):
            tool.prepare(self.args)
        self.build.assert_not_called()

    def test_build_source_drift_is_rejected(self):
        after = copy.deepcopy(self.source)
        after['sha256'] = 'a' * 64
        with patch.object(tool, 'source_manifest', side_effect=[self.source, after]):
            with self.assertRaisesRegex(RuntimeError, 'source changed during'):
                tool.prepare(self.args)

    def test_archive_may_not_gain_a_traversal_or_extra_member(self):
        path = tool.prepare(self.args)
        archive = path.parent.parent / 'release.tar.gz'
        with tarfile.open(archive, 'w:gz') as tar:
            member = tarfile.TarInfo('../escaped')
            member.size = 5
            tar.addfile(member, io.BytesIO(b'wrong'))
        with self.assertRaisesRegex(RuntimeError, 'archive inventory mismatch'):
            tool.load_package(path)

    def remote(self, remote_root, payload):
        script = tool.REMOTE_VALIDATION.replace("root=pathlib.Path('/opt/codex-agent/releases')",
                                                'root=pathlib.Path(' + repr(str(remote_root)) + ')')
        with patch.dict(sys.modules, {'fcntl': Mock(LOCK_EX=2)}), contextlib.redirect_stdout(io.StringIO()) as stdout:
            exec(compile(script, '<local-upload-test>', 'exec'), {'PAYLOAD': payload})
        return json.loads(stdout.getvalue())

    def test_remote_publish_verifies_archive_and_reuses_only_exact_target(self):
        path = tool.prepare(self.args)
        _, manifest, archive, expected = tool.load_package(path)
        remote_root = self.root / 'simulated-remote'
        remote_root.mkdir()
        payload = {'release_id': manifest['release_id'], 'files': expected, 'action': 'probe'}
        self.assertEqual(self.remote(remote_root, payload)['state'], 'absent')
        incoming = '.performance-upload-synthetic'
        self.remote(remote_root, {**payload, 'action': 'create-upload', 'incoming': incoming})
        shutil.copyfile(archive, remote_root / incoming / 'release.tar.gz')
        publish = {**payload, 'action': 'publish', 'incoming': incoming, 'archive_sha256': tool.sha(archive)}
        self.assertEqual(self.remote(remote_root, publish)['state'], 'uploaded')
        self.assertEqual(self.remote(remote_root, payload)['state'], 'exact')
        (remote_root / manifest['release_id'] / 'codex-agent').write_bytes(b'changed runtime')
        with self.assertRaisesRegex(RuntimeError, 'checksum mismatch'):
            self.remote(remote_root, payload)
        self.assertEqual((remote_root / manifest['release_id'] / 'codex-agent').read_bytes(), b'changed runtime')

    def test_remote_corrupt_transfer_never_creates_release(self):
        path = tool.prepare(self.args)
        _, manifest, archive, expected = tool.load_package(path)
        remote_root = self.root / 'simulated-remote'
        remote_root.mkdir()
        payload = {'release_id': manifest['release_id'], 'files': expected, 'action': 'create-upload',
                   'incoming': '.performance-upload-synthetic'}
        self.remote(remote_root, payload)
        (remote_root / payload['incoming'] / 'release.tar.gz').write_bytes(b'incomplete transport')
        with self.assertRaisesRegex(RuntimeError, 'transferred archive mismatch'):
            self.remote(remote_root, {**payload, 'action': 'publish', 'archive_sha256': tool.sha(archive)})
        self.assertFalse((remote_root / manifest['release_id']).exists())


if __name__ == '__main__':
    unittest.main()
