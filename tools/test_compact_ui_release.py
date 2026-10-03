"""Exercise the index transaction against a real static HTTP server."""
import functools
import http.server
import importlib.util
from pathlib import Path
import tempfile
import threading
import unittest
from unittest import mock

spec = importlib.util.spec_from_file_location('compact_ui_remote', Path(__file__).resolve().parents[1] / 'deploy/compact_ui_release.py')
release = importlib.util.module_from_spec(spec)
spec.loader.exec_module(release)


class Static(http.server.SimpleHTTPRequestHandler):
    def do_GET(self):
        if self.path == '/health':
            self.send_response(200)
            self.end_headers()
            self.wfile.write(b'{"status":"ok"}')
        else:
            super().do_GET()

    def log_message(self, *args):
        pass


class TransactionTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        base = Path(self.temporary.name)
        self.web, self.package = base / 'web', base / 'release'
        (self.web / 'assets').mkdir(parents=True)
        (self.package / 'dist/assets').mkdir(parents=True)
        (self.web / 'index.html').write_text('old index')
        (self.web / 'assets/old-123.js').write_text('old script')
        (self.package / 'dist/index.html').write_text('new index')
        (self.package / 'dist/assets/new-456.js').write_text('new script')
        self.facts = {'pid': 123, 'web': str(self.web), 'binary_sha256': 'same'}
        self.manifest = {'release_id': 'test', 'dist': release.file_map(self.package / 'dist'),
                         'baseline': {'frontend': release.file_map(self.web), 'manifest': '/old/manifest.json'}}
        self.server = http.server.ThreadingHTTPServer(('127.0.0.1', 0), functools.partial(Static, directory=str(self.web)))
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        self.addCleanup(self.server.server_close)
        self.addCleanup(self.server.shutdown)
        runtime = mock.patch.object(release, 'verify_runtime', return_value=self.facts)
        runtime.start()
        self.addCleanup(runtime.stop)
        url = mock.patch.object(release, 'BASE_URL', 'http://127.0.0.1:' + str(self.server.server_address[1]))
        url.start()
        self.addCleanup(url.stop)

    def test_publish_and_rollback_keep_old_and_new_tabs_usable(self):
        release.preflight(self.package, self.manifest)
        result = release.activate(self.package, self.manifest)
        self.assertEqual(result['status'], 'active')
        self.assertEqual((self.web / 'index.html').read_text(), 'new index')
        self.assertEqual((self.web / 'assets/old-123.js').read_text(), 'old script')
        release.rollback(self.package, self.manifest)
        self.assertEqual((self.web / 'index.html').read_text(), 'old index')
        self.assertEqual((self.web / 'assets/new-456.js').read_text(), 'new script')

    def test_conflicting_asset_rejected_before_index_switch(self):
        (self.package / 'dist/assets/old-123.js').write_text('collision')
        self.manifest['dist'] = release.file_map(self.package / 'dist')
        with self.assertRaisesRegex(RuntimeError, 'different content'):
            release.preflight(self.package, self.manifest)
        self.assertEqual((self.web / 'index.html').read_text(), 'old index')

    def test_failed_post_switch_check_restores_index(self):
        release.preflight(self.package, self.manifest)
        with mock.patch.object(release, 'health', side_effect=RuntimeError('check failed')):
            with self.assertRaisesRegex(RuntimeError, 'check failed'):
                release.activate(self.package, self.manifest)
        self.assertEqual((self.web / 'index.html').read_text(), 'old index')
        self.assertEqual(release.load(self.package / 'journal.json')['status'], 'rolled_back')
        self.assertEqual((self.web / 'assets/old-123.js').read_text(), 'old script')


if __name__ == '__main__':
    unittest.main()
