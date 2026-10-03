"""Release report lifecycle regressions with mocked checks; no npm/browser calls."""
import contextlib
import io
import json
from pathlib import Path
import subprocess
import sys
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

import frontend_release_gates as gates


class FrontendGateReportTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name).resolve()
        self.work = self.root / 'work'
        self.work.mkdir()
        self.report = self.work / 'frontend-gates.json'
        self.report.write_text(json.dumps({'status': 'pass', 'checked_at': 'old-fixture'}), encoding='utf-8')
        self.archived_log = self.work / 'retained-older-check.log'
        self.archived_log.write_text('retained evidence', encoding='utf-8')
        self.sources = {'frontend/src/example.js': 'fixture-source-sha'}
        for name in ('frontend/test-results/settings/results.json', 'frontend/test-results/app-server-metrics/report.json'):
            path = self.root / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(json.dumps({'passed': True}), encoding='utf-8')
        dist = self.root / 'frontend/dist/index.html'
        dist.parent.mkdir(parents=True)
        dist.write_text('<html>fixture</html>', encoding='utf-8')

    def invoke(self, process):
        with patch.object(gates, 'ROOT', self.root), patch.object(gates, 'frontend_sources', return_value=self.sources), \
                patch.object(gates.subprocess, 'run', side_effect=process), \
                patch.object(sys, 'argv', ['frontend_release_gates.py', '--work', str(self.work)]), \
                contextlib.redirect_stdout(io.StringIO()):
            gates.main()

    def successful_process(self, command, cwd, stdout, stderr, timeout):
        name = Path(stdout.name).name
        if name == 'final-frontend-unit.log':
            stdout.write(b'pass 198\nfail 0\n')
        elif name == 'final-usage-browser.log':
            stdout.write(b'Official usage browser passed: fixture\n')
        else:
            stdout.write(b'fixture succeeded\n')
        return SimpleNamespace(returncode=0)

    def test_failed_rerun_invalidates_old_pass_report_and_keeps_logs(self):
        def failed_process(command, cwd, stdout, stderr, timeout):
            stdout.write(b'fixture check failed\n')
            return SimpleNamespace(returncode=1)
        with self.assertRaisesRegex(RuntimeError, 'frontend check failed'):
            self.invoke(failed_process)
        self.assertFalse(self.report.exists())
        self.assertEqual(self.archived_log.read_text(), 'retained evidence')
        self.assertEqual((self.work / 'final-frontend-unit.log').read_text(), 'fixture check failed\n')

    def test_timeout_invalidates_old_pass_report(self):
        with self.assertRaises(subprocess.TimeoutExpired):
            self.invoke(lambda *args, **kwargs: (_ for _ in ()).throw(subprocess.TimeoutExpired('fixture', 300)))
        self.assertFalse(self.report.exists())

    def test_build_change_during_browser_checks_cannot_be_sealed(self):
        def concurrent_build(command, cwd, stdout, stderr, timeout):
            if Path(stdout.name).name == 'final-settings-browser.log':
                (self.root / 'frontend/dist/index.html').write_text('unverified concurrent build', encoding='utf-8')
            return self.successful_process(command, cwd, stdout, stderr, timeout)
        with self.assertRaisesRegex(RuntimeError, 'build changed during browser verification'):
            self.invoke(concurrent_build)
        self.assertFalse(self.report.exists())

    def test_completed_rerun_writes_fresh_bound_report(self):
        self.invoke(self.successful_process)
        report = json.loads(self.report.read_text())
        self.assertEqual(report['status'], 'pass')
        self.assertNotEqual(report['checked_at'], 'old-fixture')
        self.assertEqual(report['source_files'], self.sources)
        self.assertEqual(report['checks']['unit_pass'], 198)
        for evidence in report['evidence_files']:
            self.assertEqual(gates.digest(self.root / evidence['path']), evidence['sha256'])

    def verify(self, sources=None):
        with patch.object(gates, 'ROOT', self.root):
            return gates.verify_report(self.work, sources if sources is not None else self.sources)

    def rewrite_report(self, mutate):
        report = json.loads(self.report.read_text())
        mutate(report)
        self.report.write_text(json.dumps(report), encoding='utf-8')

    def reseal(self, name, text):
        path = self.work / name
        path.write_text(text, encoding='utf-8')
        key = path.relative_to(self.root).as_posix()
        def update(report):
            for evidence in report['evidence_files']:
                if evidence['path'] == key:
                    evidence['sha256'] = gates.digest(path)
        self.rewrite_report(update)

    def test_sealed_report_does_not_read_shared_browser_results(self):
        self.invoke(self.successful_process)
        for name in ('frontend/test-results/settings/results.json', 'frontend/test-results/app-server-metrics/report.json'):
            (self.root / name).write_text('{"passed":false}', encoding='utf-8')
        verified = self.verify({**self.sources, 'internal/server/main.go': 'unrelated-backend-sha'})
        self.assertEqual(verified['status'], 'pass')

    def test_added_removed_or_changed_source_is_rejected(self):
        self.invoke(self.successful_process)
        for sources in ({**self.sources, 'frontend/src/new.js': 'new-source'}, {},
                        {'frontend/src/example.js': 'changed-source'}):
            with self.subTest(sources=sources), self.assertRaisesRegex(RuntimeError, 'frontend source'):
                self.verify(sources)

    def test_added_changed_or_missing_build_artifact_is_rejected(self):
        self.invoke(self.successful_process)
        path = self.root / 'frontend/dist/extra.js'
        path.write_text('new artifact')
        with self.assertRaisesRegex(RuntimeError, 'artifacts differ'):
            self.verify()
        path.unlink()
        index = self.root / 'frontend/dist/index.html'
        index.write_text('changed artifact')
        with self.assertRaisesRegex(RuntimeError, 'artifacts differ'):
            self.verify()
        index.unlink()
        with self.assertRaisesRegex(RuntimeError, 'no index.html'):
            self.verify()

    def test_changed_or_missing_evidence_is_rejected(self):
        self.invoke(self.successful_process)
        evidence = self.work / 'settings-results.json'
        evidence.write_text('{"passed":false}', encoding='utf-8')
        with self.assertRaisesRegex(RuntimeError, 'evidence changed'):
            self.verify()
        evidence.unlink()
        with self.assertRaises(FileNotFoundError):
            self.verify()

    def test_incomplete_duplicate_or_foreign_evidence_paths_are_rejected(self):
        for mutation in (lambda refs: refs.pop(), lambda refs: refs.append(refs[0]),
                         lambda refs: refs.__setitem__(0, refs[1]),
                         lambda refs: refs[0].update(path='../foreign.json')):
            with self.subTest(mutation=mutation):
                self.invoke(self.successful_process)
                self.rewrite_report(lambda report: mutation(report['evidence_files']))
                with self.assertRaisesRegex(RuntimeError, 'evidence'):
                    self.verify()

    def test_incomplete_gates_or_unit_count_mismatch_are_rejected(self):
        for key, value in [('build', False), ('settings_browser', False), ('official_usage_browser', False),
                           ('app_server_metrics_browser', False), ('unit_pass', 199), ('unit_fail', 1)]:
            with self.subTest(key=key):
                self.invoke(self.successful_process)
                self.rewrite_report(lambda report: report['checks'].update({key: value}))
                with self.assertRaises(RuntimeError):
                    self.verify()

    def test_failed_sealed_browser_result_is_rejected_even_with_matching_hash(self):
        for name in ('settings-results.json', 'metrics-results.json'):
            with self.subTest(name=name):
                self.invoke(self.successful_process)
                self.reseal(name, '{"passed":false}')
                with self.assertRaisesRegex(RuntimeError, 'browser did not pass'):
                    self.verify()

    def test_failed_sealed_unit_log_is_rejected_even_with_matching_hash(self):
        self.invoke(self.successful_process)
        self.reseal('final-frontend-unit.log', 'pass 198\nfail 1\n')
        with self.assertRaisesRegex(RuntimeError, 'unit log did not pass'):
            self.verify()


if __name__ == '__main__':
    unittest.main()
