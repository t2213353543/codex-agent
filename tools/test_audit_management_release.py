import importlib.util
import io
from pathlib import Path
import tarfile
import tempfile
import unittest

SPEC = importlib.util.spec_from_file_location('release_boundary', Path(__file__).with_name('audit_management_release.py'))
audit = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(audit)


class ReleaseBoundaryTests(unittest.TestCase):
    def test_classifies_runtime_and_new_frontend_without_claiming_release_approval(self):
        production = {'release_id': 'old', 'binary_sha256': 'old-binary', 'frontend': {'index.html': 'old'}, 'migration_checksums': {'migrations/000.sql': 'same'}}
        candidate = {'go_source_sha256': 'new', 'binaries': {'codex-agent-linux-amd64': {'sha256': 'new-binary'}}, 'checks': {'pass': True}}
        previous = {'gateway.go': 'old', 'admin.go': 'old', 'admin_usage.go': 'old', 'accounts.go': 'old', 'migrations/000.sql': 'same'}
        current = {**previous, 'gateway.go': 'new', 'admin_usage.go': 'new', 'accounts.go': 'new', 'gateway_test.go': 'test', 'admin_new.go': 'new', 'native-transport/tests/probe.go': 'test'}
        result = audit.assess(production, candidate, previous, current, {'files': {'index.html': 'new'}, 'frontend_sha256': 'new-ui'})
        self.assertEqual(result['non_management_runtime_changes'], ['accounts.go', 'gateway.go'])
        self.assertTrue(result['migration_checksums_unchanged'])
        self.assertFalse(result['publish_eligible'])
        self.assertEqual(len(result['blockers']), 3)
        self.assertIn('native-transport/tests/probe.go', result['changed_test_files'])

    def test_unchanged_bytes_still_require_live_and_writer_evidence(self):
        production = {'release_id': 'old', 'binary_sha256': 'old-binary', 'frontend': {}, 'migration_checksums': {}}
        candidate = {'go_source_sha256': 'old', 'binaries': {'codex-agent-linux-amd64': {'sha256': 'old-binary'}}, 'checks': {}}
        result = audit.assess(production, candidate, {}, {}, {'files': {}, 'frontend_sha256': 'old-ui'})
        self.assertFalse(result['publish_eligible'])
        self.assertEqual(len(result['blockers']), 1)

    def test_archive_inspection_checks_members_and_never_extracts(self):
        with tempfile.TemporaryDirectory() as tmp:
            archive = Path(tmp) / 'source.tar.gz'
            body = b'package main\n'
            with tarfile.open(archive, 'w:gz') as target:
                member = tarfile.TarInfo('main.go'); member.size = len(body)
                target.addfile(member, io.BytesIO(body))
            self.assertEqual(audit.archive_files(archive, {'main.go': audit.digest(body)}), {'main.go': audit.digest(body)})
            self.assertFalse((Path(tmp) / 'main.go').exists())
            with self.assertRaises(ValueError): audit.archive_files(archive, {'main.go': 'different'})

    def test_archive_rejects_traversal_duplicate_and_links(self):
        for names, link in [(['../escape'], False), (['same', 'same'], False), (['link'], True)]:
            with self.subTest(names=names, link=link), tempfile.TemporaryDirectory() as tmp:
                archive = Path(tmp) / 'source.tar.gz'
                with tarfile.open(archive, 'w:gz') as target:
                    for name in names:
                        member = tarfile.TarInfo(name)
                        if link: member.type = tarfile.SYMTYPE; member.linkname = 'outside'
                        target.addfile(member)
                with self.assertRaises(ValueError): audit.archive_files(archive, {})

    def test_repeat_audit_preserves_immutable_evidence(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / 'report.json'
            audit.record_report(path, {'audited_at': 'first', 'inputs': {'manifest': 'original'}})
            before = path.read_bytes()
            audit.record_report(path, {'audited_at': 'second', 'inputs': {'manifest': 'original'}})
            self.assertEqual(before, path.read_bytes())
            with self.assertRaises(ValueError): audit.record_report(path, {'inputs': {'manifest': 'changed'}})


if __name__ == '__main__':
    unittest.main()
