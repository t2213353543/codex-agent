"""Postdeploy exit/report regressions using fake SSH results; no remote calls."""
import contextlib
import io
import json
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import MagicMock, patch

import cache_reliability_postdeploy as probe


class PostdeployExitTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        prefix = self.root / '.local/reliability-modes/remote_probe.py'
        prefix.parent.mkdir(parents=True)
        prefix.write_text('# Offline fixture.\n', encoding='utf-8')
        self.output = self.root / 'evidence/postdeploy.json'
        self.client = MagicMock()

    def invoke(self, result, smoke=True):
        argv = ['cache_reliability_postdeploy.py', '--output', str(self.output)]
        if smoke:
            argv.append('--smoke')
        raw = json.dumps(result)
        stdout = io.StringIO()
        with patch.object(probe, 'ROOT', self.root), patch.object(probe, 'connect', return_value=self.client), \
                patch.object(probe, 'run', return_value=raw), patch.object(sys, 'argv', argv), \
                contextlib.redirect_stdout(stdout):
            try:
                probe.main()
                code = 0
            except SystemExit as error:
                code = error.code
        self.assertEqual(self.output.read_text(encoding='utf-8'), raw)
        self.assertEqual(json.loads(stdout.getvalue()), result)
        self.client.close.assert_called_once_with()
        return code

    def test_failed_smoke_preserves_evidence_and_returns_failure(self):
        for http in (403, 200):
            with self.subTest(http=http):
                self.client.reset_mock()
                result = {'smoke': {'http': http, 'status': 'failed'}, 'smoke_passed': False}
                self.assertEqual(self.invoke(result), 1)

    def test_smoke_requires_explicit_boolean_success(self):
        for result in ({}, {'smoke_passed': None}, {'smoke_passed': 'true'}, {'smoke_passed': 1}):
            with self.subTest(result=result):
                self.client.reset_mock()
                self.assertEqual(self.invoke(result), 1)

    def test_successful_smoke_still_returns_success(self):
        self.assertEqual(self.invoke({'smoke_passed': True}), 0)

    def test_readonly_probe_does_not_require_smoke_result(self):
        self.assertEqual(self.invoke({'statistics_readable': True, 'generation_calls': 0}, smoke=False), 0)


if __name__ == '__main__':
    unittest.main()
