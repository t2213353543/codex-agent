"""Offline selector/report checks; no Go build, SSH, Redis or model requests."""
import contextlib
import io
import json
from pathlib import Path
import re
import tempfile
import unittest
from unittest.mock import patch

import bps_race as runner


class RaceSelectionTests(unittest.TestCase):
    def fixture(self, root):
        folder = root / 'internal/server'
        folder.mkdir(parents=True)
        for name in set(runner.COLLABORATIVE_TESTS) | set(runner.REDIS_COLLABORATIVE_TESTS):
            tests = runner.COLLABORATIVE_TESTS.get(name, ()) + runner.REDIS_COLLABORATIVE_TESTS.get(name, ())
            (folder / name).write_text(''.join('func '+test+'(t *testing.T) {}\n' for test in tests))
        (folder / 'official_test.go').write_text('\n'.join(
            'func '+name+'(t *testing.T) {}' for name in (
                'TestOfficialSyntheticControl', 'TestOfficialUsageRepairPGOriginal',
                'TestOfficialUsageRepairCacheKnownPGLegacyDryRunCommitAndConflict',
                'TestOfficialUsageRepairCacheKnownFreshEvidence',
                'TestOfficialLiveBoundary', 'TestOfficialFixtureHelper',
                'TestInferenceModeControl', 'TestDashboardRealtimeControl')))

    def test_exact_base_and_redis_sets_selected_from_real_source(self):
        for include_redis in (False, True):
            names = runner.collaborative_synthetic_tests(runner.ROOT, include_redis=include_redis)
            self.assertEqual(len(names), 20 if include_redis else 19)
            for official in ([], runner.official_synthetic_tests(runner.ROOT)):
                selected = re.compile(runner.server_test_pattern(official, names))
                self.assertTrue(all(selected.fullmatch(n) for n in names))
                self.assertIsNotNone(selected.fullmatch('TestBPSStructuredErrorAccounting'))
                self.assertIsNone(selected.fullmatch('TestPGGatewayCooldownSelectionBindingAndReplacement'))
                self.assertIsNone(selected.fullmatch('TestPGRateLimitCooldownAdminPersistence'))
                self.assertIsNone(selected.fullmatch('TestTeamCRateLimitUnregistered'))
                self.assertEqual(bool(selected.fullmatch('TestRateLimitCooldownRedisDisableDoesNotDeleteSharedDeadline')), include_redis)

    def test_missing_file_is_refused(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder)
            self.fixture(root)
            (root / 'internal/server/gateway_bps_cooldown_handoff_test.go').unlink()
            with self.assertRaises(FileNotFoundError):
                runner.collaborative_synthetic_tests(root)

    def test_missing_declaration_is_refused(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder)
            self.fixture(root)
            (root / 'internal/server/gateway_failover_abort_record_test.go').write_text('// absent\n')
            with self.assertRaisesRegex(RuntimeError, 'Required collaborative race tests missing'):
                runner.collaborative_synthetic_tests(root)

    def test_official_selector_excludes_both_pg_name_shapes(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder)
            self.fixture(root)
            selected = runner.official_synthetic_tests(root)
            self.assertEqual(selected, sorted([
                'TestOfficialSyntheticControl', 'TestOfficialUsageRepairCacheKnownFreshEvidence',
                'TestInferenceModeControl', 'TestDashboardRealtimeControl']))

    def test_required_report_refuses_skipped_failed_missing(self):
        names = runner.collaborative_synthetic_tests(runner.ROOT)
        for bad in ('skip', 'fail', None):
            with self.subTest(outcome=bad):
                values = {'codex-agent/internal/server/'+name: 'pass' for name in names}
                key = 'codex-agent/internal/server/'+names[0]
                if bad is None:
                    del values[key]
                else:
                    values[key] = bad
                report = {'tests': values, 'status': 'pass'}
                runner.record_required_test_results(report, 'collaborative_test_results', names)
                self.assertEqual(report['status'], 'fail')
                self.assertEqual(len(report['collaborative_test_results']), 19)

    def test_required_report_preserves_pass_and_prior_failure(self):
        names = runner.collaborative_synthetic_tests(runner.ROOT)
        for original in ('pass', 'fail'):
            report = {'tests': {'codex-agent/internal/server/'+n: 'pass' for n in names}, 'status': original}
            runner.record_required_test_results(report, 'collaborative_test_results', names)
            self.assertEqual(report['status'], original)
            self.assertTrue(all(v == 'pass' for v in report['collaborative_test_results'].values()))

    def test_main_prepare_only_wires_all_non_redis_modes_without_network(self):
        for flags in ([], ['--with-redis'], ['--with-redis', '--include-official'], ['--redis-only']):
            with self.subTest(flags=flags), tempfile.TemporaryDirectory() as folder:
                root = Path(folder)

                def frozen(output):
                    self.fixture(output / 'source')
                    return output / 'synthetic-not-uploaded.tar.gz', {
                        'source_sha256': 'synthetic', 'archive_sha256': 'synthetic',
                        'archive_bytes': 0, 'source_files': {}}

                with patch.object(runner, 'ROOT', root), patch.object(runner, 'freeze', side_effect=frozen), \
                     patch.object(runner, 'source_hashes', return_value={}), \
                     patch.object(runner, 'connect', side_effect=AssertionError('network forbidden')) as connect, \
                     patch.object(runner.sys, 'argv', ['bps_race.py', '--prepare-only', *flags]), \
                     contextlib.redirect_stdout(io.StringIO()):
                    self.assertEqual(runner.main(), 0)
                connect.assert_not_called()
                report = json.loads(next(root.rglob('report.json')).read_text())
                names = report['collaborative_expected_tests']
                self.assertEqual(len(names), 0 if '--redis-only' in flags else 20 if '--with-redis' in flags else 19)
                self.assertEqual('TestRateLimitCooldownRedisDisableDoesNotDeleteSharedDeadline' in names, '--with-redis' in flags)
                self.assertFalse(any(name.startswith('TestPG') for name in names))
                if names:
                    self.assertTrue(all(name in report['scope'][1] for name in names))
                self.assertEqual(bool(report['official_expected_tests']), '--include-official' in flags)


if __name__ == '__main__':
    unittest.main()
