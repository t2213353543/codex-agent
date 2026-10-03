"""Offline regressions for the frozen PG check's source identity gate."""
import hashlib
from pathlib import Path
import tempfile
import unittest

from cache_observation_pg_linux_check import changed_backend_sources


class CacheObservationSourceDriftTests(unittest.TestCase):
    def test_new_source_after_freeze_is_detected(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            folder = root / 'internal/server'
            folder.mkdir(parents=True)
            original = folder / 'original.go'
            original.write_bytes(b'package server\n')
            manifest = {'internal/server/original.go': hashlib.sha256(original.read_bytes()).hexdigest()}
            self.assertEqual(changed_backend_sources(root, manifest), [])
            added = folder / 'added.go'
            added.write_bytes(b'package server\nfunc added() {}\n')
            self.assertEqual(changed_backend_sources(root, manifest), ['internal/server/added.go'])

    def test_changed_and_removed_sources_report_exact_sorted_paths(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            folder = root / 'internal/server'
            folder.mkdir(parents=True)
            unchanged = folder / 'unchanged.go'
            changed = folder / 'changed.go'
            unchanged.write_bytes(b'package server\n')
            changed.write_bytes(b'package server\nfunc changed() {}\n')
            manifest = {
                'internal/server/unchanged.go': hashlib.sha256(unchanged.read_bytes()).hexdigest(),
                'internal/server/changed.go': hashlib.sha256(b'package server\n').hexdigest(),
                'internal/server/removed.go': hashlib.sha256(b'package server\n').hexdigest(),
            }
            self.assertEqual(changed_backend_sources(root, manifest), [
                'internal/server/changed.go', 'internal/server/removed.go'])

    def test_private_runtime_artifacts_do_not_invalidate_backend_evidence(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            for name in ['.local/private.go', 'data/private.go', 'internal/server/.local/private.go']:
                artifact = root / name
                artifact.parent.mkdir(parents=True, exist_ok=True)
                artifact.write_bytes(b'package server\n')
            self.assertEqual(changed_backend_sources(root, {}), [])


if __name__ == '__main__':
    unittest.main()
