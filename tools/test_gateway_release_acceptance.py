"""Synthetic functional acceptance policy tests, without SSH or deployment."""
from copy import deepcopy
import importlib.util
import json
from pathlib import Path
import sys
import tempfile
import unittest

sys.path.insert(0, str(Path(__file__).resolve().parent))
SPEC = importlib.util.spec_from_file_location('functional_acceptance_test', Path(__file__).with_name('gateway_release_acceptance.py'))
builder = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(builder)


def passing_gates():
    rows = {name: {'id': name, 'status': 'pass'} for name in builder.REQUIRED_GATES}
    rows['full_go_suite'].update(status='blocked', vet_passed=True,
        events={'pass': 1, 'fail': 0, 'skip': 1}, skipped_tests=['TestCodexCLIIntegration'])
    rows['real_concurrency_30_50_70_100'].update(status='blocked', executed=False, measurements=None)
    rows['production_activation'].update(status='blocked', migrated=False, deployed=False)
    rows['remaining_storage_race']['required_gap_tests'] = ['TestPGSynthetic']
    return {'schema': 'codex-agent-gateway-release-audit-v1', 'gates': list(rows.values())}


class AcceptancePolicyTests(unittest.TestCase):
    def test_only_explicit_three_scope_exceptions_are_allowed(self):
        report = passing_gates()
        gates = builder.authorize_gates(report)
        self.assertEqual({name for name, row in gates.items() if row['status'] == 'blocked'}, builder.EXCLUDED_GATES)
        report['gates'].append({'id': 'unreviewed_new_requirement', 'status': 'blocked'})
        with self.assertRaisesRegex(builder.ReadinessError, 'unreviewed_new_requirement'):
            builder.authorize_gates(report)

    def test_any_failed_gate_including_an_exception_stops_acceptance(self):
        for name in builder.REQUIRED_GATES | {'future_gate'}:
            report = passing_gates()
            found = next((row for row in report['gates'] if row['id'] == name), None)
            if found:
                found['status'] = 'fail'
            else:
                report['gates'].append({'id': name, 'status': 'fail'})
            with self.subTest(name=name), self.assertRaisesRegex(builder.ReadinessError, name):
                builder.authorize_gates(report)

    def test_missing_or_duplicate_gate_cannot_silently_disappear(self):
        report = passing_gates()
        report['gates'] = [row for row in report['gates'] if row['id'] != 'remaining_storage_race']
        with self.assertRaises(builder.ReadinessError):
            builder.authorize_gates(report)
        report = passing_gates()
        report['gates'].append(deepcopy(report['gates'][0]))
        with self.assertRaises(builder.ReadinessError):
            builder.authorize_gates(report)

    def test_capacity_is_not_relabelled_pass_or_populated_with_measurements(self):
        for field, value in (('status', 'pass'), ('executed', True), ('measurements', {'p95': 12})):
            report = passing_gates()
            row = next(row for row in report['gates'] if row['id'] == 'real_concurrency_30_50_70_100')
            row[field] = value
            with self.subTest(field=field), self.assertRaises(builder.ReadinessError):
                builder.authorize_gates(report)

    def test_frontend_metadata_timestamp_drift_is_accepted_only_for_identical_bytes(self):
        files = {'index.html': 'a' * 64, 'assets/app.js': 'b' * 64}
        browser = {'frontend_files': files, 'frontend_evidence_sha256': 'c' * 64}
        current = {'frontend': files, 'captured_at': 'a-new-capture-time'}
        result = builder.verify_frontend_binding(browser, current)
        self.assertTrue(result['same_frontend_files'])
        self.assertFalse(result['historical_browser_report_modified'])
        self.assertEqual(result['historical_browser_evidence_sha256'], 'c' * 64)
        with self.assertRaises(builder.ReadinessError):
            builder.verify_frontend_binding(browser, {**current, 'frontend': {'index.html': 'd' * 64}})


class RawEvidenceTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name).resolve()
        self.source = 'a' * 64

    def write(self, name, value):
        raw = value if isinstance(value, bytes) else json.dumps(value).encode()
        path = self.root / name
        path.write_bytes(raw)
        return {'path': name, 'sha256': builder.digest(raw)}

    def fixture(self, skip='TestCodexCLIIntegration'):
        rows = [{'Action': 'pass', 'Test': 'TestOrdinary', 'Package': 'synthetic'},
            {'Action': 'skip', 'Test': skip, 'Package': 'synthetic'},
            {'Action': 'pass', 'Package': 'synthetic'}]
        raw = '\n'.join(json.dumps(row) for row in rows).encode()
        self.write('tests.jsonl', raw)
        self.write('stderr.log', b'')
        self.write('vet.log', b'')
        summary = builder.gate.go_log_summary(raw.decode())
        checks = [{'label': 'test', 'exit_code': 0, 'command': ['go', 'test', '-json', './...'],
            'stdout': 'tests.jsonl', 'stdout_sha256': builder.digest(raw), 'stderr': 'stderr.log',
            'stderr_sha256': builder.digest(b''), 'events': summary['events'], 'top_level': summary['top_level']},
            {'label': 'vet', 'exit_code': 0, 'command': ['go', 'vet', './...'], 'stdout': 'vet.log',
            'stdout_sha256': builder.digest(b''), 'stderr': 'stderr.log', 'stderr_sha256': builder.digest(b'')}]
        report = {'schema': 'codex-agent-joint-local-verification-v1', 'go_source_sha256': self.source,
            'source_unchanged': True, 'checks': checks}
        local_role = self.write('local.json', report)
        race = {'go_source_sha256': self.source, 'status': 'pass', 'skipped_tests': [], 'failed_tests': [],
            'selected_top_level_tests': ['TestAnother']}
        race_role = self.write('race.json', race)
        candidate = {'evidence_roles': {'local_verification': local_role, 'gateway_race': race_role, 'management_race': race_role}}
        gates = builder.authorize_gates(passing_gates())
        gates['full_go_suite']['skipped_tests'] = [skip]
        return candidate, gates, report

    def test_explicit_opt_in_skip_is_listed_without_claiming_execution(self):
        candidate, gates, _ = self.fixture()
        result = builder.verify_full_go(candidate, self.source, gates, self.root)
        self.assertEqual(result['unexecuted_explicit_opt_in_tests'], ['TestCodexCLIIntegration'])
        self.assertEqual(result['events']['skip'], 1)
        self.assertTrue(result['vet_passed'])

    def test_unknown_skip_or_missing_pg_coverage_is_not_exempted(self):
        for skipped in ('TestRegressionUnexpected', 'TestPGUncovered'):
            candidate, gates, _ = self.fixture(skipped)
            with self.subTest(skip=skipped), self.assertRaisesRegex(builder.ReadinessError, 'Uncovered non-opt-in'):
                builder.verify_full_go(candidate, self.source, gates, self.root)

    def test_pg_skip_can_only_be_closed_by_explicit_passed_gap_gate(self):
        candidate, gates, _ = self.fixture('TestPGSynthetic')
        result = builder.verify_full_go(candidate, self.source, gates, self.root)
        self.assertEqual(result['covered_by_separate_linux_pg_redis_runs'], ['TestPGSynthetic'])
        self.assertEqual(result['unexecuted_explicit_opt_in_tests'], [])
        gates['remaining_storage_race']['status'] = 'blocked'
        with self.assertRaises(builder.ReadinessError):
            builder.verify_full_go(candidate, self.source, gates, self.root)

    def test_raw_log_tampering_is_rejected_even_when_report_says_pass(self):
        candidate, gates, _ = self.fixture()
        (self.root / 'tests.jsonl').write_bytes(b'{"Action":"pass"}')
        with self.assertRaisesRegex(builder.ReadinessError, 'hash mismatch'):
            builder.verify_full_go(candidate, self.source, gates, self.root)

    def test_focused_go_test_cannot_claim_full_repository_verification(self):
        candidate, gates, report = self.fixture()
        report['checks'][0]['command'] = ['go', 'test', '-run', 'TestOrdinary', './...']
        candidate['evidence_roles']['local_verification'] = self.write('local.json', report)
        with self.assertRaisesRegex(builder.ReadinessError, 'whole repository'):
            builder.verify_full_go(candidate, self.source, gates, self.root)

    def test_vet_failure_and_different_candidate_are_rejected(self):
        candidate, gates, report = self.fixture()
        report['checks'][1]['exit_code'] = 1
        candidate['evidence_roles']['local_verification'] = self.write('local.json', report)
        with self.assertRaisesRegex(builder.ReadinessError, 'command failed'):
            builder.verify_full_go(candidate, self.source, gates, self.root)
        candidate, gates, _ = self.fixture()
        with self.assertRaisesRegex(builder.ReadinessError, 'not the immutable'):
            builder.verify_full_go(candidate, 'b' * 64, gates, self.root)


if __name__ == '__main__':
    unittest.main()
