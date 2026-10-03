"""Read-only local release audit. Never emits publishable acceptance or deploys.

Run: python tools/gateway_release_check.py
Exit 2 means publication remains blocked; individual gates distinguish missing
evidence from integrity failures. No SSH, subprocess, database or HTTP is used.
"""
from __future__ import annotations

from project_layout import backend_source_paths, migration_runner_key, valid_backend_source_path, BRIDGE_FIXTURES

import argparse
from collections import Counter
from datetime import datetime, timezone
import hashlib
import importlib.util
import json
import math
from pathlib import Path, PurePosixPath
import re
import tarfile

ROOT = Path(__file__).resolve().parents[1]
DEFAULT_CANDIDATE = 'docs/MANAGEMENT-PARITY-FOLLOWUP-2026-09-18.json'
DEFAULT_GATEWAY = '.local/frozen-gateway-race/20260918T022116-6f87b7/report.json'
DEFAULT_MIGRATION = '.local/migration-003/20260917T201629.573381Z/report.json'
DEFAULT_FRONTEND = '.local/runtime-frontend/d87032e071e7c72343ccb178857d4e56a0200ea7af87aa3b262083e73d15d1c6/frontend-evidence.json'
DEFAULT_ROLLBACK = '.local/migration-003-rollback/20260917T202020.331069Z/report.json'
DEFAULT_NETWORK = '.local/migration-003-rollback/network-blocked.json'
DEFAULT_COMPACT_REGRESSION = '.local/compact-timeout-regression/20260918T023719.775475Z/report.json'
HASH = re.compile(r'[0-9a-f]{64}')
SPEC = importlib.util.spec_from_file_location('release_audit_policy', ROOT / 'deploy/upgrade_remote.py')
POLICY = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(POLICY)


class EvidenceError(ValueError):
    pass


class MissingEvidence(FileNotFoundError):
    pass


def require(condition, message):
    if not condition:
        raise EvidenceError(message)


def digest(raw):
    return hashlib.sha256(raw).hexdigest()


def source_aliases(files):
    return {'owner_compact_sha256': digest(json.dumps(files, sort_keys=True, separators=(',', ':')).encode()),
        'v2_default_json_sha256': digest(json.dumps(files, sort_keys=True).encode()),
        'same_file_map': True, 'scope': 'root Go, go.mod, go.sum, migrations; no frontend',
        'owner_serialization': 'json.dumps(files, sort_keys=True, separators=(comma,colon))',
        'v2_serialization': 'json.dumps(files, sort_keys=True)'}


def go_log_summary(raw):
    outcomes, package_outcomes = {}, {}
    events = Counter()
    malformed = 0
    for line in raw.splitlines():
        if not line.lstrip().startswith('{'):
            continue
        try:
            event = json.loads(line)
        except ValueError:
            malformed += 1
            continue
        if not isinstance(event, dict):
            continue
        action, name = event.get('Action'), event.get('Test')
        if action in ('pass', 'fail', 'skip'):
            if name:
                events[action] += 1
                outcomes[name] = action
            else:
                package_outcomes[event.get('Package', '')] = action
    top = {name: outcome for name, outcome in outcomes.items() if '/' not in name}
    return {'top_level': {action: list(top.values()).count(action) for action in ('pass', 'fail', 'skip')},
        'events': {action: events[action] for action in ('pass', 'fail', 'skip')},
        'outcomes': outcomes, 'package_outcomes': package_outcomes,
        'data_race_reported': 'WARNING: DATA RACE' in raw, 'malformed_json_lines': malformed}


def test_status(summary):
    if (summary['malformed_json_lines'] or summary['data_race_reported'] or summary['events']['fail']
            or 'fail' in summary['package_outcomes'].values()):
        return 'fail'
    if not summary['events']['pass'] or not summary['package_outcomes'] or summary['events']['skip']:
        return 'blocked'
    return 'pass'


def browser_observations(browser, wrapper_checks):
    results = {item['id']: item['status'] for item in browser['checks']}
    require(len(results) == len(browser['checks']) and all(value == 'pass' for value in results.values())
        and browser.get('errors') == [], 'Browser contains failed, duplicate, or contradictory checks/errors')
    require(wrapper_checks == browser['checks'], 'Browser wrapper and nested check outcomes differ')
    require(browser.get('resource_responses') and browser.get('api_responses'), 'Browser response observations missing')
    for response in browser['resource_responses']:
        require(response.get('status') == 200, 'Browser resource request did not succeed')
    for response in browser['api_responses']:
        status, path_name = response.get('status'), response.get('path', '')
        expected_negative = (status == 401 and path_name.split('?')[0] in ('/api/session', '/api/accounts')) or (status == 409 and path_name.startswith('/api/accounts/'))
        require(isinstance(status, int) and (200 <= status < 300 or expected_negative),
            'Browser contains unexpected API failure')
    return results


def latency_summary(sample):
    values = sample.get('raw_ms', [])
    require(len(values) >= 100 and len(values) == sample.get('samples')
        and all(isinstance(value, (int, float)) and math.isfinite(value) and value >= 0 for value in values),
        'Latency sample count or raw measurements invalid')
    ordered = sorted(values)
    result = {'samples': len(values), 'p50_ms': ordered[math.ceil(len(values) * .5) - 1],
              'p95_ms': ordered[math.ceil(len(values) * .95) - 1]}
    require(all(result[key] == sample.get(key) for key in result), 'Reported percentile differs from raw samples')
    return result


class Audit:
    def __init__(self, root=ROOT):
        self.root = Path(root).resolve()
        self.evidence = {}
        self.gates = []
        self.candidate = {}
        self.files = {}
        self.frontend = {}
        self.aliases = {}
        self.gateway_outcomes = {}
        self.management_outcomes = {}
        self.local_outcomes = {}

    def role(self, name):
        row = self.candidate.get('evidence_roles', {}).get(name)
        if not isinstance(row, dict) or not row.get('path') or not row.get('sha256'):
            raise MissingEvidence('Candidate has no hash-bound evidence role: ' + name)
        return row

    def path(self, value, base=None):
        path = Path(str(value).replace('\\', '/'))
        path = path if path.is_absolute() else (base or self.root) / path
        resolved = path.resolve()
        require(resolved.is_relative_to(self.root), 'Evidence path escapes the project workspace')
        require(not path.is_symlink(), 'Evidence file must not be a symlink')
        return resolved

    def read(self, value, expected=None, base=None):
        path = self.path(value, base)
        if not path.is_file():
            raise MissingEvidence('Required evidence file is absent: ' + str(path.relative_to(self.root)))
        raw = path.read_bytes()
        actual = digest(raw)
        name = path.relative_to(self.root).as_posix()
        self.evidence[name] = {'path': name, 'sha256': actual, 'bytes': len(raw),
            'expected_sha256': expected, 'hash_binding': 'declared-pending-verification' if expected else 'observed-locally'}
        if expected is not None:
            self.evidence[name]['hash_binding'] = 'mismatch'
            require(isinstance(expected, str) and HASH.fullmatch(expected) and actual == expected,
                'Artifact hash mismatch: ' + name)
            self.evidence[name]['hash_binding'] = 'declared-and-matched'
        return raw

    def document(self, value, expected=None, base=None):
        result = json.loads(self.read(value, expected, base))
        require(isinstance(result, dict), 'Expected a JSON evidence object')
        return result

    def gate(self, identifier, callback):
        try:
            result = callback() or {}
            self.gates.append({'id': identifier, 'status': 'pass', **result})
        except MissingEvidence as exc:
            self.gates.append({'id': identifier, 'status': 'blocked', 'reason': str(exc)})
        except (EvidenceError, ValueError, KeyError, TypeError, OSError, tarfile.TarError,
                POLICY.UpgradeError, StopIteration) as exc:
            self.gates.append({'id': identifier, 'status': 'fail',
                'reason': str(exc) if isinstance(exc, (EvidenceError, MissingEvidence)) else type(exc).__name__})

    def candidate_evidence(self, basename):
        matches = [row for row in self.candidate.get('evidence', []) if Path(row['path']).name == basename]
        if not matches:
            raise MissingEvidence('Candidate does not bind evidence: ' + basename)
        require(len(matches) == 1, 'Ambiguous candidate evidence name: ' + basename)
        return matches[0]

    def source(self, candidate_path, source_manifest=None, source_dir=None):
        self.candidate = self.document(candidate_path)
        joint = self.candidate.get('schema') == 'codex-agent-joint-candidate-v1'
        require((joint and self.candidate.get('status') == 'verified_joint_source_candidate') or
            (self.candidate.get('schema') == 'codex-agent-management-parity-followup-v1'
             and self.candidate.get('status') == 'verified_management_candidate'), 'Unexpected owner candidate schema/status')
        row = self.role('source_manifest') if joint else self.candidate_evidence('source-manifest.json')
        path = self.path(source_manifest or row['path'])
        manifest = self.document(path, row['sha256'])
        self.files = manifest['files']
        require(isinstance(self.files, dict) and {'go.mod', 'go.sum'} <= self.files.keys() and bool(migration_runner_key(self.files)),
            'Frozen source manifest is incomplete')
        for name, value in self.files.items():
            normalized = PurePosixPath(name)
            require(not normalized.is_absolute() and '..' not in normalized.parts and '\\' not in name
                and valid_backend_source_path(name), 'Unexpected frozen source path')
            require(isinstance(value, str) and HASH.fullmatch(value), 'Invalid source file hash')
        self.aliases = source_aliases(self.files)
        owner = self.aliases['owner_compact_sha256']
        require(owner == manifest.get('sha256') == self.candidate.get('go_source_sha256'), 'Owner source digest mismatch')
        archive = self.path(self.candidate['source_archive'])
        self.read(archive, self.candidate['source_archive_sha256'])
        require(manifest.get('archive_sha256') == self.candidate['source_archive_sha256'], 'Source archive binding mismatch')
        members = {}
        with tarfile.open(archive) as tar:
            for member in tar:
                if member.isdir():
                    continue
                require(member.isfile() and member.name not in members and member.name in self.files,
                    'Source archive contains an extra, duplicate, or non-regular member')
                require(member.size <= 16 * 1024 * 1024, 'Unexpectedly large source member')
                members[member.name] = digest(tar.extractfile(member).read())
        require(members == self.files, 'Source archive member hashes differ from the frozen manifest')
        local = self.path(source_dir or path.parent / 'local-source')
        found = {p.relative_to(local).as_posix() for p in backend_source_paths(local)}
        found |= {name for name in BRIDGE_FIXTURES if (local / name).is_file()}
        require(found == set(self.files), 'Frozen on-disk source file set differs')
        for name, expected in self.files.items():
            self.read(local / name, expected)
        binary = self.read(self.candidate['linux_binary'], self.candidate['linux_binary_sha256'])
        require(binary[:6] == b'\x7fELF\x02\x01' and binary[18:20] == b'\x3e\x00', 'Candidate is not Linux amd64 ELF')
        build_row = self.candidate_evidence('build-verification.json' if joint else 'local-verification.json')
        build = self.document(build_row['path'], build_row['sha256'])
        if joint:
            require(build.get('go_source_sha256') == owner and build.get('builds_passed') is True
                and build.get('source_unchanged_after_build') is True, 'Build source provenance differs from the candidate')
            artifacts = [entry for entry in build.get('artifacts', [])
                         if Path(entry['path']).name == 'codex-agent-linux-amd64']
            require(len(artifacts) == 1 and artifacts[0]['sha256'] == self.candidate['linux_binary_sha256'],
                'Built binary provenance differs from candidate executable')
            self.read(artifacts[0]['path'], artifacts[0]['sha256'])
        else:
            require(build.get('manifest_sha256') == owner and build.get('source_unchanged') is True
                and build.get('artifacts', {}).get('codex-agent-linux-amd64', {}).get('sha256') == self.candidate['linux_binary_sha256']
                and any(check.get('label') == 'linux-build' and check.get('exit_code') == 0 for check in build.get('checks', [])),
                'Built binary provenance differs from candidate source/executable')
        return {'source_files': len(self.files), 'archive_members': len(members), 'source_aliases': self.aliases,
            'binary_sha256': self.candidate['linux_binary_sha256']}

    def owner_artifacts(self):
        require(bool(self.candidate), 'Candidate document was not loaded')
        for row in self.candidate['evidence']:
            self.read(row['path'], row['sha256'])
        frontend = self.candidate['frontend']
        self.read(frontend['archive'], frontend['archive_sha256'])
        self.read(frontend['source_archive'], frontend['source_archive_sha256'])
        return {'verified_references': len(self.candidate['evidence']),
            'management_frontend_files': frontend['file_count'], 'management_frontend_publish': False}

    def race(self, override=None):
        joint = self.candidate.get('schema') == 'codex-agent-joint-candidate-v1'
        row = self.role('management_race') if joint else self.candidate_evidence('report.json')
        path = self.path(override or row['path'])
        report = self.document(path, None if override else row['sha256'])
        require(report.get('go_source_sha256') == self.aliases.get('owner_compact_sha256'), 'Management race source differs')
        if report.get('schema') == 'codex-agent-local-source-race-v1':
            expected = self.document(row['path'], row['sha256'])['selected_top_level_tests']
            result = self.local_linux_race(report, 'Management selection on exact joint candidate', expected)
            if result['status'] == 'pass':
                self.management_outcomes = result.pop('outcomes')
            return result
        artifacts = report.get('artifacts', [])
        for artifact in artifacts:
            self.read(artifact['path'], artifact['sha256'])
        logs = [row for row in artifacts if Path(row['path'].replace('\\', '/')).name == 'race.log']
        require(len(logs) == 1, 'Management race log is not hash-bound')
        raw = self.read(logs[0]['path'], logs[0]['sha256']).decode()
        summary = go_log_summary(raw)
        require('linux/amd64' in raw[:200] and '\n1\ngcc\n' in raw[:300].replace('\r', ''), 'Linux CGO race toolchain evidence is absent')
        require(summary['events']['pass'] == report.get('passed_test_events')
            and summary['top_level']['pass'] == report.get('passed_top_level_tests'), 'Management race count mismatch')
        missing = sorted(set(report['selected_top_level_tests']) - set(summary['outcomes']))
        status = test_status(summary)
        if report.get('status') != 'pass' or report.get('exit_code') != 0 or missing:
            status = 'fail'
        return {'status': status, 'top_level': summary['top_level'], 'events': summary['events'],
            'missing_tests': missing, 'isolated_pg_redis': report.get('database_name') == 'codex_agent_integration',
            'scope': 'Management selection only, not gateway or whole-repository race'}

    def full_go(self, override=None):
        joint = self.candidate.get('schema') == 'codex-agent-joint-candidate-v1'
        row = self.role('local_verification') if joint else self.candidate_evidence('local-verification.json')
        path = self.path(override or row['path'])
        report = self.document(path, None if override else row['sha256'])
        if joint:
            require(report.get('schema') == 'codex-agent-joint-local-verification-v1'
                and report.get('go_source_sha256') == self.aliases.get('owner_compact_sha256')
                and report.get('source_unchanged') is True, 'Joint full Go report source mismatch')
            summaries = {}
            for check in report['checks']:
                raw = self.read(check['stdout'], check['stdout_sha256'], path.parent).decode()
                self.read(check['stderr'], check['stderr_sha256'], path.parent)
                require(check.get('exit_code') == 0, 'Joint local command failed')
                if check['label'] == 'test':
                    summaries['test'] = go_log_summary(raw)
                    require(summaries['test']['events'] == check['events']
                        and summaries['test']['top_level'] == check['top_level'], 'Joint local raw outcome mismatch')
                elif check['label'] == 'vet':
                    summaries['vet'] = True
            require('test' in summaries and summaries.get('vet'), 'Joint local Go/vet evidence incomplete')
            summary = summaries['test']
            self.local_outcomes = summary['outcomes']
            return {'status': test_status(summary), 'top_level': summary['top_level'], 'events': summary['events'],
                'vet_passed': True, 'skipped_tests': [name for name, value in summary['outcomes'].items() if value == 'skip'],
                'scope': 'Full local suite; PG/Redis and other opt-in skips remain explicitly unexecuted here'}
        require(report.get('manifest_sha256') == self.aliases.get('owner_compact_sha256')
            and report.get('source_after_sha256') == report['manifest_sha256']
            and report.get('archive_sha256') == self.candidate.get('source_archive_sha256'), 'Full Go report source binding mismatch')
        check = next(row for row in report['checks'] if row['label'] == 'test')
        raw = self.read(check['stdout'], base=path.parent).decode()
        self.read(check['stderr'], base=path.parent)
        summary = go_log_summary(raw)
        self.local_outcomes = summary['outcomes']
        require(summary['events'] == check['test_events_including_subtests']
            and summary['top_level'] == check['top_level_tests'], 'Full Go raw log counts differ from report')
        return {'status': test_status(summary) if check.get('exit_code') == 0 else 'fail',
            'top_level': summary['top_level'], 'events': summary['events'],
            'skipped_tests': [name for name, value in summary['outcomes'].items() if value == 'skip'],
            'log_binding': 'raw log locally hashed; owner report does not declare its digest',
            'reason': 'Skipped prerequisites remain unexecuted' if summary['events']['skip'] else 'Executed outcomes verified from raw log',
            'scope': 'Full local suite; skipped prerequisites are not counted as acceptance'}

    def gateway(self, path):
        joint = self.candidate.get('schema') == 'codex-agent-joint-candidate-v1'
        if joint:
            row = self.role('gateway_race')
            if path == DEFAULT_GATEWAY:
                path = row['path']
            canonical = self.document(row['path'], row['sha256'])
            report = self.document(path, row['sha256'] if path == row['path'] else None)
            result = self.local_linux_race(report, 'Gateway selection on exact joint candidate', canonical['selected_top_level_tests'])
            if result['status'] == 'pass':
                self.gateway_outcomes = result.pop('outcomes')
            return result
        path = self.path(path)
        report = self.document(path)
        require(report.get('source_sha256') == self.aliases.get('owner_compact_sha256')
            and report.get('candidate_binary_sha256') == self.candidate.get('linux_binary_sha256')
            and report.get('candidate_archive_sha256') == self.candidate.get('source_archive_sha256'), 'Gateway report is not this exact candidate')
        artifacts = report['artifacts']
        require('local-go-test.jsonl' in artifacts, 'Gateway raw log hash is absent')
        for name, expected in artifacts.items():
            self.read(name, expected, path.parent)
        summary = go_log_summary(self.read('local-go-test.jsonl', artifacts['local-go-test.jsonl'], path.parent).decode())
        require(summary['events']['pass'] == report.get('passed_test_events')
            and summary['top_level']['pass'] == report.get('passed_top_level_tests')
            and sorted(name for name, value in summary['outcomes'].items() if value == 'skip') == sorted(report['skipped_tests']),
            'Gateway report counts do not match actual test outcomes')
        missing = sorted(set(report['selected_tests']) - set(summary['outcomes']))
        require(not missing, 'Selected gateway tests lack terminal log evidence')
        status = test_status(summary)
        if not report.get('actual_pg_redis') or not report.get('race_executed'):
            status = 'blocked' if status != 'fail' else status
        return {'status': status, 'top_level': summary['top_level'], 'events': summary['events'],
            'actual_pg_redis': report.get('actual_pg_redis') is True, 'race_executed': report.get('race_executed') is True,
            'skipped_tests': report['skipped_tests'], 'scope': report.get('scope'),
            'reason': 'Same-candidate PG/Redis and Linux race remain unexecuted' if status == 'blocked' else 'Raw outcomes verified',
            'production_requests': 0}

    def local_linux_race(self, report, scope, minimum_tests=None):
        require(report.get('schema') == 'codex-agent-local-source-race-v1'
            and report.get('go_source_sha256') == self.aliases.get('owner_compact_sha256'),
            'Local Linux race does not target this candidate')
        logs = []
        source_verified = False
        for artifact in report['artifacts']:
            self.read(artifact['path'], artifact['sha256'])
            if Path(artifact['path'].replace('\\', '/')).name == 'race.log':
                logs.append(artifact)
            elif Path(artifact['path'].replace('\\', '/')).name == 'source-manifest.json':
                require(self.document(artifact['path'], artifact['sha256'])['files'] == self.files,
                    'Local race source files differ from candidate')
                source_verified = True
        require(len(logs) == 1 and source_verified, 'Local race has no unique bound raw log and source manifest')
        raw = self.read(logs[0]['path'], logs[0]['sha256']).decode()
        summary = go_log_summary(raw)
        require(minimum_tests is None or set(minimum_tests) <= set(summary['outcomes']),
            'Linux race omits required hash-bound role test coverage')
        require('linux/amd64' in raw and '\n1\ngcc\n' in raw.replace('\r', ''), 'Local race CGO toolchain absent')
        require({name for name in summary['outcomes'] if '/' not in name} == set(report['selected_top_level_tests'])
            and summary['top_level']['pass'] == report['passed_top_level_tests']
            and summary['events']['pass'] == report['passed_test_events'], 'Local race counts/selection mismatch')
        require(report.get('dependencies_ready') is True and report.get('postgres_non_superuser_verified') is True
            and report.get('redis_acl_boundary_verified') is True and report.get('cleanup_complete') is True
            and report.get('go_source_changed_after_freeze') is False, 'Local race dependency/isolation/cleanup evidence incomplete')
        status = test_status(summary)
        if report.get('status') != 'pass' or report.get('exit_code') != 0 or report.get('oom_killed'):
            status = 'fail'
        return {'status': status, 'top_level': summary['top_level'], 'events': summary['events'],
            'scope': scope, 'actual_pg_redis': True, 'race_executed': True, 'outcomes': summary['outcomes']}

    def remaining_storage(self, path, addendum_path=None):
        if not self.local_outcomes or not self.gateway_outcomes or not self.management_outcomes:
            raise MissingEvidence('Cannot derive database gaps without verified local/gateway/management evidence')
        skipped = {name.split('/')[0] for name, status in self.local_outcomes.items() if status == 'skip'}
        expected = {name for name in skipped if name.startswith(('TestPG', 'TestAdminKeyPG')) or 'Redis' in name}
        expected -= set(self.gateway_outcomes) | set(self.management_outcomes)
        result = self.local_linux_race(self.document(path), 'Remaining exact-candidate database requirements', expected)
        outcomes = result.pop('outcomes', {})
        result['required_gap_tests'] = sorted(expected)
        if result['status'] == 'fail' and addendum_path:
            test = 'TestPGShutdownPersistsRefreshAfterClientDisconnectAndSIGTERM'
            require({name for name, value in outcomes.items() if value == 'fail'} == {test}
                and not any(value == 'skip' for value in outcomes.values()), 'Storage failure exceeds the reviewed fixture-only issue')
            self.shutdown_addendum(addendum_path, path, test)
            require(all(outcomes.get(name) == 'pass' or name == test for name in expected),
                'Combined formal repair does not cover all required storage cases')
            result.update(status='pass', composition='Original observed 16 passes plus formally repaired fixture on identical production inputs',
                original_run_status='fail', formal_fixture_test=test, composed_coverage=len(expected),
                original_report_rewritten=False)
        return result

    def shutdown_addendum(self, path, prior_path, test):
        report = self.document(path)
        require(report.get('schema') == 'codex-agent-shutdown-release-addendum-v1' and report.get('status') == 'pass'
            and report.get('candidate_go_sha256') == self.aliases.get('owner_compact_sha256')
            and report.get('candidate_binary_sha256') == self.candidate.get('linux_binary_sha256'),
            'Formal fixture addendum targets a different candidate')
        roles = report['evidence_roles']
        documents = {}
        for name, entry in roles.items():
            raw = self.read(entry['path'], entry['sha256'])
            if entry['path'].endswith('.json'):
                documents[name] = json.loads(raw)
        require(self.path(roles['prior_storage_report']['path']) == self.path(prior_path), 'Fixture composition uses another original storage report')
        baseline = documents['baseline_source_manifest']['files']
        fixed_manifest = documents['fixture_source_manifest']
        fixed = fixed_manifest['files']
        require(baseline == self.files and set(fixed) == set(baseline)
            and [name for name in baseline if baseline[name] != fixed[name]] == ['accounts_shutdown_test.go'],
            'Formal fixture changes exceed the single reviewed test file')
        require(source_aliases(fixed)['owner_compact_sha256'] == report['fixture_go_sha256'] == fixed_manifest['sha256']
            and fixed['accounts_shutdown_test.go'] == report['formal_test_sha256'], 'Formal fixture digest mismatch')
        fixture = documents['fixture_manifest']
        require(fixture.get('selected_test') == test and fixture.get('baseline_go_sha256') == report['candidate_go_sha256'],
            'Formal fixture manifest selection differs')
        archive = self.path(fixture['archive'])
        self.read(archive, fixture['archive_sha256'])
        members = {}
        with tarfile.open(archive) as tar:
            for member in tar:
                require(member.isfile() and member.name in fixed and member.name not in members and member.size <= 16 * 1024 * 1024,
                    'Unexpected formal fixture archive member')
                raw = tar.extractfile(member).read()
                members[member.name] = digest(raw)
                if member.name == 'accounts_shutdown_test.go':
                    fixed_test = raw.decode().replace('\r\n', '\n')
        require(members == fixed, 'Formal fixture archive changed')
        baseline_dir = self.path(self.candidate.get('source_directory') or self.path(self.role('source_manifest')['path']).parent / 'local-source')
        old_test = self.read(baseline_dir / 'accounts_shutdown_test.go', baseline['accounts_shutdown_test.go']).decode().replace('\r\n', '\n')
        before = '\tif err := NewPGSettingsStore(s.DB).Set(Settings{AdminAPI: true}); err != nil {'
        after = '\tif _, err := s.DB.initializeAdminKey(context.Background(), shutdownTestAdminKey); err != nil {\n\t\tt.Fatal("cannot initialize synthetic admin key")\n\t}\n\tsettings := defaultSettings()\n\tsettings.AdminAPI = true\n\tif err := NewPGSettingsStore(s.DB).Set(settings); err != nil {'
        require(old_test.count(before) == 1 and old_test.replace(before, after, 1) == fixed_test,
            'Formal fixture modified assertions beyond reviewed initialization')
        formal = documents['formal_race_report']
        saved_files, saved_aliases = self.files, self.aliases
        try:
            self.files, self.aliases = fixed, source_aliases(fixed)
            result = self.local_linux_race(formal, 'Formal shutdown fixture only', [test])
        finally:
            self.files, self.aliases = saved_files, saved_aliases
        require(result['status'] == 'pass' and result['top_level']['pass'] == 1
            and result['outcomes'].get(test) == 'pass', 'Formal shutdown lifecycle did not pass')

    def migration(self, path):
        path = self.path(path)
        report = self.document(path)
        POLICY.require_checks(report, POLICY.MIGRATION_CHECKS, 'codex-agent-pg-additive-migration-v1')
        require(report.get('baseline_checksums') == POLICY.MIGRATIONS and report.get('target_checksums') == POLICY.TARGET_MIGRATIONS,
            'Migration evidence contains unreviewed SQL checksums')
        require(report.get('migration_runner_sha256') == self.files.get(migration_runner_key(self.files)), 'Candidate migration implementation differs from isolated evidence')
        for name, expected in POLICY.TARGET_MIGRATIONS.items():
            self.read(Path(name).name, expected, path.parent)
            require(self.files.get(name) == expected, 'Candidate migration checksum changed')
        summary = go_log_summary(self.read('go-test.jsonl', base=path.parent).decode())
        require(summary['outcomes'].get('TestManagementCleanupMigration') == 'pass' and test_status(summary) == 'pass',
            'Migration log does not prove completed isolated checks')
        return {'scope': 'Migration-focused, not candidate deployment or exact previous-binary rollback',
            'report_source_sha256': report['source_sha256'], 'candidate_implementation_matches': True,
            'production_migrated': False}

    def compact_timeout(self, path):
        path = self.path(path)
        report = self.document(path)
        if self.candidate.get('schema') == 'codex-agent-joint-candidate-v1':
            require(report.get('schema') == 'codex-agent-compact-timeout-regression-v1'
                and report.get('frozen_source_sha256') == self.candidate.get('base_go_sha256'),
                'Historical regression is not this joint candidate baseline')
            require(self.files.get('gateway_client_pool.go') == report.get('source_impl_after'),
                'Joint source does not include the reviewed timeout fix')
            for name, expected in report['evidence'].items():
                self.read(name, expected, path.parent)
            required = report['required_tests']
            require(required and all(self.gateway_outcomes.get(name) == 'pass' for name in required),
                'Exact joint candidate has not passed required timeout tests')
            return {'status': 'pass', 'known_defect': False, 'historical_baseline_defective': True,
                'candidate_contains_reviewed_fix': True, 'same_candidate_regressions_passed': required,
                'scope': 'Custom compact client factory regression; no claim of production latency benefit'}
        require(report.get('schema') == 'codex-agent-compact-timeout-regression-v1'
            and report.get('frozen_source_sha256') == self.aliases.get('owner_compact_sha256')
            and report.get('candidate_binary_sha256') == self.candidate.get('linux_binary_sha256'),
            'Compact timeout regression is not bound to this candidate')
        before = report.get('source_impl_before', {})
        if isinstance(before, str):
            before = {'gateway_client_pool.go': before}
        require(before == {'gateway_client_pool.go': self.files.get('gateway_client_pool.go')},
            'Timeout baseline implementation differs from the candidate')
        for name, expected in report.get('evidence', {}).items():
            self.read(name, expected, path.parent)
        required = report.get('required_tests')
        require(isinstance(required, list) and required and all(isinstance(name, str) for name in required),
            'Regression has no explicit required test selection')
        outcomes = {}
        for label in ('baseline', 'repaired_overlay'):
            observed = report[label]
            raw = self.read(observed['log_path'], observed['log_sha256'], path.parent).decode()
            summary = go_log_summary(raw)
            require(not summary['malformed_json_lines'] and not summary['data_race_reported']
                and all(name in summary['outcomes'] for name in required),
                'Timeout regression lacks actual required terminal test outcomes')
            require(observed.get('tests') == summary['outcomes'], 'Timeout report differs from actual Go test log')
            outcomes[label] = summary
        baseline = outcomes['baseline']
        failed = [name for name in required if baseline['outcomes'][name] == 'fail']
        if failed:
            require(report['baseline'].get('exit_code') != 0 and report['baseline'].get('known_defect') is True,
                'Timeout failure is not recorded as a reproduced candidate defect')
            return {'status': 'fail', 'known_defect': True, 'failed_tests': failed,
                'repaired_overlay_passed': report['repaired_overlay'].get('exit_code') == 0
                    and test_status(outcomes['repaired_overlay']) == 'pass',
                'candidate_binary_fixed': False,
                'scope': 'Injected direct custom RoundTripper client factory contract, not a demonstrated production request overrun',
                'reason': 'Frozen candidate client factory ignores explicit custom compact timeout; a passing repair overlay does not modify its binary'}
        return {'status': test_status(baseline) if report['baseline'].get('exit_code') == 0 else 'fail',
            'known_defect': False, 'reason': 'Baseline results checked independently of repair overlay'}

    def preserved_frontend(self, path):
        path = self.path(path)
        report = self.document(path)
        require(report.get('schema') == 'codex-agent-production-frontend-v1' and report.get('status') == 'pass', 'Invalid production frontend evidence')
        folder = path.parent / 'frontend/dist'
        actual = {p.relative_to(folder).as_posix(): digest(self.read(p)) for p in sorted(folder.rglob('*')) if p.is_file()}
        require(actual == report.get('frontend') and 'index.html' in actual, 'Captured frontend file set/hash mismatch')
        captures = sorted(path.parent.glob('capture-*.json'))
        if not captures:
            raise MissingEvidence('Missing original HTTP/SFTP frontend capture evidence')
        capture = self.document(captures[-1])
        require(capture.get('stable') is True and capture['source']['frontend'] == actual
            and capture['source']['binary_sha256'] == report['binary_sha256']
            and capture['source']['unit_sha256'] == report['unit_sha256']
            and capture['source'].get('http_hashes_match') is True, 'Original frontend capture does not match its release summary')
        self.frontend = report
        return {'file_count': len(actual), 'exact_preserved_hashes': actual,
            'captured_at': report.get('captured_at'), 'runtime_health_now_verified': False}

    def rollback(self, path, network_path=None):
        path = self.path(path)
        report = self.document(path)
        if network_path:
            self.document(network_path)
        require(report.get('previous_binary_sha256') == self.frontend.get('binary_sha256'), 'Rollback evidence targets a different old binary')
        if report.get('status') != 'pass':
            return {'status': 'blocked', 'evidence_status': report.get('status'), 'checks': report.get('checks', {}),
                'reason': 'Exact previous-binary target-schema compatibility has not completed; failed/network-blocked evidence is not PASS'}
        POLICY.require_checks(report, POLICY.ROLLBACK_CHECKS, 'codex-agent-pg-rollback-compatibility-v1')
        require(report.get('target_checksums') == POLICY.TARGET_MIGRATIONS, 'Rollback target schema differs')
        if report.get('candidate_binary_sha256') == self.candidate.get('linux_binary_sha256'):
            require(report.get('synthetic_only') is True and report.get('production_touched') is False
                and report.get('gateway_requests') == 0 and report.get('isolated_process_stopped') is True
                and report.get('runtime_binary_sha256') == report['previous_binary_sha256'],
                'Local rollback runtime/identity evidence incomplete')
            self.read(path.parent / 'old-binary', report['previous_binary_sha256'])
            self.read(path.parent / 'candidate-binary', report['candidate_binary_sha256'])
            return {'status': 'pass', 'checks': report['checks'], 'scope': 'Exact retained old binary on local synthetic target schema',
                'previous_binary_sha256': report['previous_binary_sha256'], 'production_migrated': False}
        return {'status': 'blocked', 'reason': 'Passing replacement report needs independently bound runtime artifacts before publication',
            'checks': report['checks']}

    def candidate_browser(self, path):
        if not path:
            raise MissingEvidence('Exact candidate plus preserved frontend browser/API evidence is absent')
        path = self.path(path)
        report = self.document(path)
        require(report.get('schema') == 'codex-agent-local-candidate-browser-runtime-v1'
            and report.get('status') == 'pass' and report.get('source_sha256') == self.aliases.get('owner_compact_sha256')
            and report.get('candidate_binary_sha256') == self.candidate.get('linux_binary_sha256'),
            'Browser runtime evidence targets another candidate or did not pass')
        require(report.get('synthetic_only') is True and report.get('production_requests') == 0
            and report.get('runtime_binary_verified') is True and report.get('old_process_stopped_verified') is True
            and report.get('candidate_process_stopped') is True and report.get('candidate_container_removed') is True,
            'Browser runtime isolation/cleanup evidence incomplete')
        browser_path = self.path(report['browser_report'])
        browser = self.document(browser_path, report['browser_report_sha256'])
        require(browser.get('schema') == 'codex-agent-legacy-frontend-real-api-v1' and browser.get('status') == 'pass'
            and browser.get('source_sha256') == report['source_sha256']
            and browser.get('candidate_binary_sha256') == report['candidate_binary_sha256']
            and browser.get('frontend_files') == self.frontend.get('frontend')
            and browser.get('api_mocked') is False and browser.get('gateway_inference_requests') == 0,
            'Browser did not cover exact retained frontend with real candidate API')
        required = {'retained_frontend_bytes', 'api_unauthorized', 'synthetic_account_create', 'page_unauthorized',
            'cookie_session_reload', 'two_page_account_cas', 'reauthorize_cas_compatibility', 'legacy_routes_real_api',
            'desktop_mobile_geometry', 'account_delete_and_filters', 'logs_statistics_preserved',
            'sensitive_fields_and_resources', 'logout_revokes_session'}
        results = browser_observations(browser, report.get('checks'))
        require(all(results.get(name) == 'pass' for name in required), 'Required actual API/browser checks incomplete')
        require(len(browser.get('screenshots', [])) >= 4, 'Browser screenshot evidence missing')
        for shot in browser['screenshots']:
            self.read(shot['path'], shot['sha256'], browser_path.parent)
        return {'status': 'pass', 'checks': sorted(required), 'screenshots': len(browser['screenshots']),
            'scope': 'Retained frontend and exact binary, synthetic PostgreSQL/Redis, no API mocks or live inference'}

    def management_performance(self, path):
        path = self.path(path)
        report = self.document(path)
        require(report.get('schema') == 'codex-agent-local-management-performance-v1' and report.get('status') == 'pass'
            and report.get('source_sha256') == self.aliases.get('owner_compact_sha256')
            and report.get('candidate_binary_sha256') == self.candidate.get('linux_binary_sha256'),
            'Performance report does not target this candidate')
        require(report.get('synthetic_only') is True and report.get('production_touched') is False
            and report.get('gateway_or_refresh_requests') == 0 and report.get('cleanup', {}).get('complete') is True,
            'Performance isolation or cleanup evidence incomplete')
        self.read(path.parent / 'candidate', report['candidate_binary_sha256'])
        files = report['dataset_files_sha256']
        require(digest(json.dumps(files, sort_keys=True).encode()) == report['dataset_sha256'], 'Dataset map digest mismatch')
        for name, expected in files.items():
            self.read(name, expected, path.parent / 'dataset')
        dry_run = self.document(path.parent / 'dry-run.json')
        counts = report['actual_dataset']
        require(dry_run.get('dry_run') is True and counts.get('enabled') == 0
            and counts.get('accounts') == dry_run.get('accounts') == 1000
            and counts.get('logs') == counts.get('events') == dry_run.get('logs') == 50000
            and counts.get('tokens') == dry_run.get('log_tokens') == 6250000,
            'Representative dry-run and actual dataset counts mismatch')
        required = {'accounts_legacy_50', 'accounts_management_50', 'logs_50',
                    'overview_warm_cache', 'overview_uncached_range', 'overview_cache_cold'}
        require(required <= report['latency'].keys(), 'Required management latency measurements absent')
        latencies = {}
        for name in sorted(required):
            sample = report['latency'][name]
            require(sample.get('http_status') == 200 and sample.get('concurrency') == 1, 'Performance success/scope mismatch')
            latencies[name] = latency_summary(sample['warm'])
        plans = self.document(path.parent / 'explain.json', report['explain_sha256'])
        self.document(path.parent / 'sql-call-deltas.json', report['sql_evidence_sha256'])
        require(len(plans) >= 7, 'Required query plans absent')
        for value in plans.values():
            require(digest(value['sql'].encode()) == value['sql_sha256'] and value.get('explain'),
                'Query plan SQL identity mismatch')
        return {'status': 'pass', 'dataset': counts, 'latency': latencies, 'query_plans': len(plans),
            'scope': 'Local serial management measurements, not production or real upstream concurrency capacity'}


def check(args, root=ROOT):
    audit = Audit(root)
    audit.gate('candidate_integrity', lambda: audit.source(args.candidate, args.source_manifest, args.source_dir))
    audit.gate('owner_evidence_integrity', audit.owner_artifacts)
    audit.gate('management_linux_race', lambda: audit.race(args.management_race))
    audit.gate('full_go_suite', lambda: audit.full_go(args.full_go))
    audit.gate('gateway_joint_acceptance', lambda: audit.gateway(args.gateway))
    if args.compact_timeout_regression:
        audit.gate('compact_timeout_regression', lambda: audit.compact_timeout(args.compact_timeout_regression))
    audit.gate('additive_migration_003', lambda: audit.migration(args.migration))
    audit.gate('preserved_production_frontend', lambda: audit.preserved_frontend(args.frontend))
    audit.gate('previous_binary_rollback', lambda: audit.rollback(args.rollback, args.network_blocked))
    audit.gate('candidate_with_preserved_frontend', lambda: audit.candidate_browser(args.candidate_browser))
    if args.storage_race:
        audit.gate('remaining_storage_race', lambda: audit.remaining_storage(args.storage_race, args.shutdown_addendum))
    if args.performance:
        audit.gate('management_performance', lambda: audit.management_performance(args.performance))
    audit.gates.extend([
        {'id': 'real_concurrency_30_50_70_100', 'status': 'blocked', 'executed': False,
            'levels': [30, 50, 70, 100], 'measurements': None, 'reason': 'Not executed; no local synthetic figures substituted'},
        {'id': 'production_activation', 'status': 'blocked', 'migrated': False, 'deployed': False,
            'reason': 'This command is read-only and does not migrate, stage or activate a release'},
    ])
    candidate = audit.candidate
    return {'schema': 'codex-agent-gateway-release-audit-v1', 'status': 'blocked', 'publish_eligible': False,
        'generated_at': datetime.now(timezone.utc).isoformat(), 'read_only': True,
        'is_publishable_acceptance': False, 'candidate_migrated': False, 'candidate_deployed': False,
        'candidate_release_plan': {'binary': candidate.get('linux_binary'), 'binary_sha256': candidate.get('linux_binary_sha256'),
            'source_archive': candidate.get('source_archive'), 'source_archive_sha256': candidate.get('source_archive_sha256'),
            'source_aliases': audit.aliases, 'migration_checksums': POLICY.TARGET_MIGRATIONS,
            'frontend_policy': 'Preserve captured production files exactly; do not use shared frontend/dist',
            'frontend': audit.frontend.get('frontend', {}), 'frontend_evidence': str(args.frontend),
            'management_frontend_reference_only': {**candidate.get('frontend', {}), 'included_in_release': False}},
        'gates': audit.gates, 'gate_counts': dict(Counter(gate['status'] for gate in audit.gates)),
        'evidence': list(audit.evidence.values()),
        'limitations': ['Local files and recorded logs are verified; current server health is not checked.',
            'Source aliases identify one exact file map using different JSON serialization, not different candidates.',
            'PASS records executed scope only. Skipped tests and unavailable evidence remain blocked.']}


def parser():
    result = argparse.ArgumentParser(description=__doc__)
    for name, default in {'candidate': DEFAULT_CANDIDATE, 'gateway': DEFAULT_GATEWAY,
            'migration': DEFAULT_MIGRATION, 'frontend': DEFAULT_FRONTEND, 'rollback': DEFAULT_ROLLBACK,
            'network-blocked': DEFAULT_NETWORK, 'source-manifest': None, 'source-dir': None,
            'management-race': None, 'full-go': None, 'compact-timeout-regression': DEFAULT_COMPACT_REGRESSION,
            'candidate-browser': None, 'storage-race': None, 'performance': None, 'shutdown-addendum': None}.items():
        result.add_argument('--' + name, default=default)
    result.add_argument('--output', help='New report path; existing reports are never overwritten')
    return result


def main():
    args = parser().parse_args()
    report = check(args)
    stamp = datetime.now(timezone.utc).strftime('%Y%m%dT%H%M%S.%fZ')
    output = Path(args.output).resolve() if args.output else ROOT / '.local/gateway-release-check' / stamp / 'report.json'
    require(output.is_relative_to(ROOT), 'Audit output must remain inside this project')
    output.parent.mkdir(parents=True, exist_ok=True)
    with output.open('x', encoding='utf-8') as handle:
        json.dump(report, handle, ensure_ascii=False, indent=2)
        handle.write('\n')
    print(json.dumps({'status': 'blocked', 'publish_eligible': False, 'gate_counts': report['gate_counts'],
        'report': str(output)}, ensure_ascii=False))
    return 2


if __name__ == '__main__':
    raise SystemExit(main())
