"""Synthetic unit tests for the read-only release evidence audit."""
import argparse
from copy import deepcopy
import importlib.util
import io
import json
from pathlib import Path
import tarfile
import tempfile
import unittest
from unittest.mock import patch

SPEC = importlib.util.spec_from_file_location('gateway_release_check_test', Path(__file__).with_name('gateway_release_check.py'))
audit = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(audit)


def events(*outcomes):
    rows = [{'Action': action, 'Test': name, 'Package': 'synthetic'} for name, action in outcomes]
    rows.append({'Action': 'pass', 'Package': 'synthetic'})
    return '\n'.join(json.dumps(row) for row in rows)


class AuditTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name).resolve()
        self.checker = audit.Audit(self.root)

    def write(self, name, raw):
        path = self.root / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(raw)
        return path

    def test_missing_evidence_is_blocked_not_pass(self):
        self.checker.gate('missing', lambda: self.checker.document('does-not-exist.json'))
        self.assertEqual(self.checker.gates[0]['status'], 'blocked')

    def test_wrong_hash_is_integrity_failure_not_missing(self):
        self.write('report.json', b'{"status":"pass"}')
        self.checker.gate('hash', lambda: self.checker.read('report.json', '0' * 64))
        self.assertEqual(self.checker.gates[0]['status'], 'fail')
        self.assertEqual(self.checker.evidence['report.json']['hash_binding'], 'mismatch')

    def test_skipped_or_missing_test_execution_cannot_count_as_pass(self):
        summary = audit.go_log_summary(events(('TestDone', 'pass'), ('TestStorage', 'skip')))
        self.assertEqual(summary['top_level'], {'pass': 1, 'fail': 0, 'skip': 1})
        self.assertEqual(audit.test_status(summary), 'blocked')
        self.assertEqual(audit.test_status(audit.go_log_summary('')), 'blocked')
        self.assertEqual(audit.test_status(audit.go_log_summary(events(('TestDone', 'pass')))), 'pass')

    def test_subtest_skip_blocks_even_when_parent_passes(self):
        summary = audit.go_log_summary(events(('TestParent/Storage', 'skip'), ('TestParent', 'pass')))
        self.assertEqual(summary['top_level']['skip'], 0)
        self.assertEqual(summary['events']['skip'], 1)
        self.assertEqual(audit.test_status(summary), 'blocked')

    def test_data_race_or_malformed_json_is_failure(self):
        passed = events(('TestDone', 'pass'))
        for raw in (passed + '\nWARNING: DATA RACE', passed + '\n{broken', events(('TestBroken', 'fail'))):
            self.assertEqual(audit.test_status(audit.go_log_summary(raw)), 'fail')

    def test_owner_compact_and_v2_default_digests_are_named_aliases(self):
        files = {'go.mod': 'a' * 64, 'main.go': 'b' * 64}
        result = audit.source_aliases(files)
        self.assertNotEqual(result['owner_compact_sha256'], result['v2_default_json_sha256'])
        self.assertEqual(result['owner_compact_sha256'], audit.digest(json.dumps(files, sort_keys=True, separators=(',', ':')).encode()))
        self.assertEqual(result['v2_default_json_sha256'], audit.digest(json.dumps(files, sort_keys=True).encode()))
        self.assertIs(result['same_file_map'], True)

    def test_percentiles_recomputed_from_exact_raw_samples(self):
        sample = {'raw_ms': list(range(100)), 'samples': 100, 'p50_ms': 49, 'p95_ms': 94}
        self.assertEqual(audit.latency_summary(sample)['p95_ms'], 94)
        for changed in (dict(sample, p95_ms=1), dict(sample, samples=99), dict(sample, raw_ms=[1] * 10),
                        dict(sample, raw_ms=[float('nan')] * 100)):
            with self.assertRaises(audit.EvidenceError):
                audit.latency_summary(changed)

    def source_fixture(self, files=None):
        files = files or {'database.go': b'package main\n', 'main.go': b'package main\n', 'go.mod': b'module synthetic\n', 'go.sum': b''}
        hashes = {name: audit.digest(raw) for name, raw in files.items()}
        archive = self.root / 'source.tar.gz'
        with tarfile.open(archive, 'w:gz') as tar:
            for name, raw in files.items():
                member = tarfile.TarInfo(name)
                member.size = len(raw)
                tar.addfile(member, io.BytesIO(raw))
                self.write('local-source/' + name, raw)
        archive_hash = audit.digest(archive.read_bytes())
        source = {'files': hashes, 'sha256': audit.source_aliases(hashes)['owner_compact_sha256'], 'archive_sha256': archive_hash}
        manifest = self.write('source-manifest.json', json.dumps(source).encode())
        binary = self.write('candidate', b'\x7fELF\x02\x01' + bytes(12) + b'\x3e\x00')
        build = self.write('local-verification.json', json.dumps({'manifest_sha256': source['sha256'],
            'source_unchanged': True, 'artifacts': {'codex-agent-linux-amd64': {'sha256': audit.digest(binary.read_bytes())}},
            'checks': [{'label': 'linux-build', 'exit_code': 0}]}).encode())
        candidate = {'schema': 'codex-agent-management-parity-followup-v1', 'status': 'verified_management_candidate',
            'go_source_sha256': source['sha256'], 'source_archive': 'source.tar.gz', 'source_archive_sha256': archive_hash,
            'linux_binary': 'candidate', 'linux_binary_sha256': audit.digest(binary.read_bytes()),
            'evidence': [{'path': 'source-manifest.json', 'sha256': audit.digest(manifest.read_bytes())},
                         {'path': 'local-verification.json', 'sha256': audit.digest(build.read_bytes())}]}
        self.write('candidate.json', json.dumps(candidate).encode())
        return candidate

    def test_source_archive_binary_and_actual_files_all_verified(self):
        self.source_fixture()
        result = self.checker.source('candidate.json')
        self.assertEqual(result['archive_members'], 4)
        self.assertEqual(result['source_files'], 4)
        self.write('local-source/main.go', b'changed while candidate unchanged')
        self.checker.gate('candidate_integrity', lambda: self.checker.source('candidate.json'))
        self.assertEqual(self.checker.gates[-1]['status'], 'fail')

    def test_nested_server_manifest_is_verified_and_unmanifested_code_rejected(self):
        files = {'main.go': b'package main\n', 'internal/server/database.go': b'package server\n',
                 'internal/bps/request.go': b'package bps\n', 'migrations/embed.go': b'package migrations\n',
                 'migrations/000_core.sql': b'SELECT 1;\n', 'go.mod': b'module synthetic\n', 'go.sum': b''}
        self.source_fixture(files)
        result = self.checker.source('candidate.json')
        self.assertEqual(result['source_files'], len(files))
        self.write('local-source/internal/bps/unmanifested.go', b'package bps\n')
        self.checker.gate('candidate_integrity', lambda: self.checker.source('candidate.json'))
        self.assertEqual(self.checker.gates[-1]['status'], 'fail')

    def test_archive_content_not_trusted_from_pass_or_outer_hash(self):
        candidate = self.source_fixture()
        with tarfile.open(self.root / 'source.tar.gz', 'w:gz') as tar:
            entry = tarfile.TarInfo('../outside')
            entry.size = 1
            tar.addfile(entry, io.BytesIO(b'x'))
        new_hash = audit.digest((self.root / 'source.tar.gz').read_bytes())
        manifest = json.loads((self.root / 'source-manifest.json').read_text())
        manifest['archive_sha256'] = new_hash
        saved = self.write('source-manifest.json', json.dumps(manifest).encode())
        candidate['source_archive_sha256'] = new_hash
        candidate['evidence'][0]['sha256'] = audit.digest(saved.read_bytes())
        self.write('candidate.json', json.dumps(candidate).encode())
        self.checker.gate('candidate_integrity', lambda: self.checker.source('candidate.json'))
        self.assertEqual(self.checker.gates[-1]['status'], 'fail')

    def test_absent_all_evidence_still_emits_non_publishable_blocked_audit(self):
        args = audit.parser().parse_args([])
        report = audit.check(args, self.root)
        self.assertEqual(report['status'], 'blocked')
        self.assertIs(report['publish_eligible'], False)
        self.assertIs(report['candidate_migrated'], False)
        self.assertIs(report['candidate_deployed'], False)
        workload = next(g for g in report['gates'] if g['id'] == 'real_concurrency_30_50_70_100')
        self.assertIs(workload['executed'], False)
        self.assertIsNone(workload['measurements'])
        self.assertIs(report['candidate_release_plan']['management_frontend_reference_only']['included_in_release'], False)

    def test_policy_rejection_is_recorded_without_aborting_audit(self):
        self.checker.gate('policy', lambda: audit.POLICY.require_checks({}, {'rollback'}, 'required'))
        self.assertEqual(self.checker.gates[0]['status'], 'fail')

    def test_failed_rollback_report_cannot_become_pass(self):
        self.checker.frontend = {'binary_sha256': 'a' * 64}
        self.write('rollback.json', json.dumps({'status': 'fail', 'previous_binary_sha256': 'a' * 64,
            'checks': {'old_binary_start': False}}).encode())
        result = self.checker.rollback('rollback.json')
        self.assertEqual(result['status'], 'blocked')
        self.assertEqual(result['evidence_status'], 'fail')

    def timeout_report(self):
        self.checker.aliases = {'owner_compact_sha256': 'a' * 64}
        self.checker.candidate = {'linux_binary_sha256': 'b' * 64}
        self.checker.files = {'gateway_client_pool.go': 'c' * 64}
        test = 'TestExplicitCompactTimeout'
        baseline = events((test, 'fail')).replace('"Action": "pass", "Package":', '"Action": "fail", "Package":')
        repaired = events((test, 'pass'))
        self.write('baseline.jsonl', baseline.encode())
        self.write('repaired.jsonl', repaired.encode())
        report = {'schema': 'codex-agent-compact-timeout-regression-v1',
            'frozen_source_sha256': 'a' * 64, 'candidate_binary_sha256': 'b' * 64,
            'source_impl_before': {'gateway_client_pool.go': 'c' * 64}, 'required_tests': [test],
            'baseline': {'exit_code': 1, 'known_defect': True, 'tests': {test: 'fail'},
                'log_path': 'baseline.jsonl', 'log_sha256': audit.digest(baseline.encode())},
            'repaired_overlay': {'exit_code': 0, 'tests': {test: 'pass'},
                'log_path': 'repaired.jsonl', 'log_sha256': audit.digest(repaired.encode())}}
        self.write('timeout.json', json.dumps(report).encode())
        return report

    def test_repaired_overlay_does_not_make_defective_candidate_pass(self):
        self.timeout_report()
        result = self.checker.compact_timeout('timeout.json')
        self.assertEqual(result['status'], 'fail')
        self.assertTrue(result['known_defect'])
        self.assertTrue(result['repaired_overlay_passed'])
        self.assertFalse(result['candidate_binary_fixed'])

    def test_single_file_timeout_hash_can_use_reported_scalar_form(self):
        report = self.timeout_report()
        report['source_impl_before'] = 'c' * 64
        self.write('timeout.json', json.dumps(report).encode())
        self.assertTrue(self.checker.compact_timeout('timeout.json')['known_defect'])

    def test_timeout_regression_for_another_candidate_is_rejected(self):
        report = self.timeout_report()
        report['candidate_binary_sha256'] = 'd' * 64
        self.write('timeout.json', json.dumps(report).encode())
        with self.assertRaisesRegex(audit.EvidenceError, 'bound to this candidate'):
            self.checker.compact_timeout('timeout.json')

    def test_timeout_compile_error_is_not_a_reproduced_test_failure(self):
        report = self.timeout_report()
        raw = b'compile error only\n'
        self.write('baseline.jsonl', raw)
        report['baseline'].update(log_sha256=audit.digest(raw), tests={})
        self.write('timeout.json', json.dumps(report).encode())
        with self.assertRaisesRegex(audit.EvidenceError, 'actual required terminal'):
            self.checker.compact_timeout('timeout.json')

    def test_joint_fix_requires_new_candidate_test_outcomes(self):
        report = self.timeout_report()
        report['source_impl_after'] = 'e' * 64
        report['evidence'] = {'baseline.jsonl': report['baseline']['log_sha256'],
                              'repaired.jsonl': report['repaired_overlay']['log_sha256']}
        self.write('timeout.json', json.dumps(report).encode())
        self.checker.candidate.update(schema='codex-agent-joint-candidate-v1', base_go_sha256='a' * 64)
        self.checker.files['gateway_client_pool.go'] = 'e' * 64
        with self.assertRaisesRegex(audit.EvidenceError, 'has not passed'):
            self.checker.compact_timeout('timeout.json')
        self.checker.gateway_outcomes = dict.fromkeys(report['required_tests'], 'pass')
        self.assertEqual(self.checker.compact_timeout('timeout.json')['status'], 'pass')

    def race_fixture(self):
        files = {'database.go': 'c' * 64}
        self.checker.files = files
        self.checker.aliases = {'owner_compact_sha256': 'a' * 64}
        source = self.write('source-manifest.json', json.dumps({'files': files}).encode())
        raw = 'go version go1.26 linux/amd64\nlinux\namd64\n1\ngcc\n' + events(('TestDatabase', 'pass'))
        log = self.write('race.log', raw.encode())
        return {'schema': 'codex-agent-local-source-race-v1', 'go_source_sha256': 'a' * 64,
            'status': 'pass', 'exit_code': 0, 'selected_top_level_tests': ['TestDatabase'],
            'passed_top_level_tests': 1, 'passed_test_events': 1,
            'dependencies_ready': True, 'postgres_non_superuser_verified': True,
            'redis_acl_boundary_verified': True, 'cleanup_complete': True, 'go_source_changed_after_freeze': False,
            'artifacts': [{'path': p.name, 'sha256': audit.digest(p.read_bytes())} for p in (source, log)]}

    def test_local_race_requires_exact_source_and_isolation(self):
        report = self.race_fixture()
        self.assertEqual(self.checker.local_linux_race(report, 'synthetic')['status'], 'pass')
        report['redis_acl_boundary_verified'] = False
        with self.assertRaisesRegex(audit.EvidenceError, 'dependency/isolation'):
            self.checker.local_linux_race(report, 'synthetic')

    def test_local_race_log_without_source_manifest_cannot_pass(self):
        report = self.race_fixture()
        report['artifacts'] = [row for row in report['artifacts'] if row['path'] == 'race.log']
        with self.assertRaisesRegex(audit.EvidenceError, 'raw log and source manifest'):
            self.checker.local_linux_race(report, 'synthetic')

    def test_local_race_cannot_shrink_hash_bound_expected_selection(self):
        report = self.race_fixture()
        with self.assertRaisesRegex(audit.EvidenceError, 'role test coverage'):
            self.checker.local_linux_race(report, 'synthetic', ['TestDatabase', 'TestRequiredMissing'])

    def test_source_binary_mix_is_not_accepted_from_elf_and_self_report(self):
        candidate = self.source_fixture()
        binary = self.write('another-binary', b'\x7fELF\x02\x01' + bytes(12) + b'\x3e\x00other')
        candidate.update(linux_binary=binary.name, linux_binary_sha256=audit.digest(binary.read_bytes()))
        self.write('candidate.json', json.dumps(candidate).encode())
        with self.assertRaisesRegex(audit.EvidenceError, 'Built binary provenance'):
            self.checker.source('candidate.json')

    def test_browser_rejects_hidden_failures_even_with_required_passes(self):
        clean = {'checks': [{'id': 'required', 'status': 'pass'}], 'errors': [],
            'api_responses': [{'path': '/api/session', 'status': 401}, {'path': '/api/accounts/id', 'status': 409},
                              {'path': '/api/overview', 'status': 200}],
            'resource_responses': [{'path': '/assets/app.js', 'status': 200}]}
        self.assertEqual(audit.browser_observations(clean, clean['checks']), {'required': 'pass'})
        for kind in ('extra-fail', 'duplicate', 'script-error', 'http-500', 'resource-404', 'missing-observations'):
            broken = deepcopy(clean)
            if kind == 'extra-fail':
                broken['checks'].append({'id': 'unexpected', 'status': 'fail'})
            elif kind == 'duplicate':
                broken['checks'].append({'id': 'required', 'status': 'pass'})
            elif kind == 'script-error':
                broken['errors'].append('synthetic error')
            elif kind == 'http-500':
                broken['api_responses'].append({'path': '/api/overview', 'status': 500})
            elif kind == 'resource-404':
                broken['resource_responses'][0]['status'] = 404
            else:
                broken['api_responses'] = []
            with self.subTest(kind=kind), self.assertRaises(audit.EvidenceError):
                audit.browser_observations(broken, broken['checks'])

    def test_real_local_rollback_checks_exact_retained_binary_bytes(self):
        old = self.write('old-binary', b'old synthetic binary')
        new = self.write('candidate-binary', b'new synthetic binary')
        old_hash, new_hash = audit.digest(old.read_bytes()), audit.digest(new.read_bytes())
        self.checker.frontend = {'binary_sha256': old_hash}
        self.checker.candidate = {'linux_binary_sha256': new_hash}
        report = {'schema': 'codex-agent-pg-rollback-compatibility-v1', 'status': 'pass',
            'checks': dict.fromkeys(audit.POLICY.ROLLBACK_CHECKS, True), 'target_checksums': audit.POLICY.TARGET_MIGRATIONS,
            'previous_binary_sha256': old_hash, 'runtime_binary_sha256': old_hash,
            'candidate_binary_sha256': new_hash, 'synthetic_only': True, 'production_touched': False,
            'gateway_requests': 0, 'isolated_process_stopped': True}
        self.write('rollback.json', json.dumps(report).encode())
        self.assertEqual(self.checker.rollback('rollback.json')['status'], 'pass')
        old.write_bytes(b'changed old binary')
        with self.assertRaisesRegex(audit.EvidenceError, 'Artifact hash mismatch'):
            self.checker.rollback('rollback.json')

    def test_explicit_role_is_unambiguous_among_same_named_reports(self):
        self.checker.candidate = {'evidence': [{'path': 'old/report.json'}, {'path': 'new/report.json'}],
            'evidence_roles': {'gateway_race': {'path': 'new/report.json', 'sha256': 'a' * 64}}}
        self.assertEqual(self.checker.role('gateway_race')['path'], 'new/report.json')
        with self.assertRaises(audit.MissingEvidence):
            self.checker.role('unassigned')


if __name__ == '__main__':
    unittest.main()
