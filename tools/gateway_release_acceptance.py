"""Build functional-upgrade v2 acceptance from freshly verified local evidence.

No capacity claim, migration, staging, SSH, or deployment is performed. All
unexpected failed/blocked gates are fatal; opt-in skips remain explicitly listed.
"""
from __future__ import annotations

import argparse
from datetime import datetime, timezone
import hashlib
import json
from pathlib import Path

import gateway_release_check as gate

ROOT = Path(__file__).resolve().parents[1]
DEFAULT_INPUTS = 'docs/GATEWAY-ACCEPTANCE-INPUTS-2026-09-18.json'
POLICY = gate.POLICY
REQUIRED_GATES = frozenset({'candidate_integrity', 'owner_evidence_integrity', 'management_linux_race',
    'full_go_suite', 'gateway_joint_acceptance', 'compact_timeout_regression', 'additive_migration_003',
    'preserved_production_frontend', 'previous_binary_rollback', 'candidate_with_preserved_frontend',
    'remaining_storage_race', 'management_performance', 'real_concurrency_30_50_70_100', 'production_activation'})
EXCLUDED_GATES = frozenset({'full_go_suite', 'real_concurrency_30_50_70_100', 'production_activation'})
OPT_IN_TESTS = frozenset({'TestCodexCLIIntegration', 'TestCodexFingerprintCLIConversation',
    'TestCodexCLIWorkloadFixture', 'TestCodexCLIWorkload', 'TestGatewayLiveSettings',
    'TestRealCodexCLIReadOnlyWindowsSandbox', 'TestRealCodexCLI', 'TestRealCodexCLIWebSocket',
    'TestSourceNativeCLITransport', 'TestSourceNativeHelperTLSComparison'})


class ReadinessError(ValueError):
    pass


def require(condition, message):
    if not condition:
        raise ReadinessError(message)


def digest(raw):
    return hashlib.sha256(raw).hexdigest()


def local_path(value, root=ROOT, base=None):
    path = Path(str(value).replace('\\', '/'))
    path = path if path.is_absolute() else (base or root) / path
    resolved = path.resolve()
    require(resolved.is_relative_to(root.resolve()) and not path.is_symlink(), 'Evidence path is outside the local project or a symlink')
    return resolved


def read(value, expected=None, root=ROOT, base=None):
    path = local_path(value, root, base)
    raw = path.read_bytes()
    require(expected is None or digest(raw) == expected, 'Evidence hash mismatch: ' + path.name)
    return raw


def document(value, expected=None, root=ROOT, base=None):
    data = json.loads(read(value, expected, root, base))
    require(isinstance(data, dict), 'Expected an evidence JSON object')
    return data


def resolve_inputs(path, root=ROOT):
    values = document(path, root=root)
    actions = {action.dest.replace('_', '-'): action for action in gate.parser()._actions
        if action.dest not in ('help', 'output')}
    require(set(values) <= set(actions), 'Unknown acceptance input field')
    require(all(isinstance(value, str) for value in values.values()), 'Acceptance input overrides must be paths')
    arguments = [item for key, value in values.items() for item in ('--' + key, value)]
    return gate.parser().parse_args(arguments)


def authorize_gates(report):
    require(report.get('schema') == 'codex-agent-gateway-release-audit-v1', 'Unexpected release audit schema')
    rows = report.get('gates', [])
    require(all(isinstance(row, dict) and isinstance(row.get('id'), str) for row in rows), 'Malformed release gate')
    gates = {row['id']: row for row in rows}
    require(len(gates) == len(rows) and REQUIRED_GATES <= gates.keys(), 'Missing or duplicate required release gate')
    for name, row in gates.items():
        status = row.get('status')
        require(status in ('pass', 'blocked', 'fail'), 'Unknown gate status: ' + name)
        require(status != 'fail', 'Release gate failed: ' + name)
        if name not in EXCLUDED_GATES:
            require(status == 'pass', 'Release gate remains blocked: ' + name)
    full = gates['full_go_suite']
    require(full.get('vet_passed') is True and full.get('events', {}).get('fail') == 0
        and full.get('events', {}).get('pass', 0) > 0, 'Full Go/vet did not pass executed checks')
    capacity = gates['real_concurrency_30_50_70_100']
    require(capacity.get('status') == 'blocked' and capacity.get('executed') is False
        and capacity.get('measurements') is None, 'Capacity omission must remain explicitly unexecuted')
    activation = gates['production_activation']
    require(activation.get('status') == 'blocked' and activation.get('migrated') is False
        and activation.get('deployed') is False, 'This builder only accepts not-yet-deployed functional readiness')
    return gates


def verify_full_go(candidate, source_hash, gates, root=ROOT):
    role = candidate.get('evidence_roles', {}).get('local_verification')
    require(isinstance(role, dict), 'Exact local-verification role is required')
    path = local_path(role['path'], root)
    report = document(path, role['sha256'], root)
    require(report.get('schema') == 'codex-agent-joint-local-verification-v1'
        and report.get('go_source_sha256') == source_hash and report.get('source_unchanged') is True,
        'Full Go report is not the immutable joint candidate')
    commands = {row['label']: row for row in report['checks']}
    require(len(commands) == len(report['checks']) and {'test', 'vet'} <= commands.keys(), 'Missing or duplicate full Go/vet command')
    summary = None
    for label, row in commands.items():
        require(row.get('exit_code') == 0, 'Local verification command failed: ' + label)
        stdout = read(row['stdout'], row['stdout_sha256'], root, path.parent).decode('utf-8')
        read(row['stderr'], row['stderr_sha256'], root, path.parent)
        if label == 'test':
            command = row.get('command', [])
            require(command[:2] == ['go', 'test'] and './...' in command
                and not any(value == '-run' or value.startswith('-run=') for value in command),
                'Full Go report must execute the whole repository, not a narrowed selection')
            summary = gate.go_log_summary(stdout)
            require(summary['events'] == row['events'] and summary['top_level'] == row['top_level'], 'Full Go report/log counts disagree')
            require(summary['events']['fail'] == 0 and not summary['data_race_reported'] and not summary['malformed_json_lines']
                and summary['package_outcomes'] and all(value == 'pass' for value in summary['package_outcomes'].values()),
                'Raw full Go log contains failure or lacks passing package terminal')
        if label == 'vet':
            require(row.get('command', [])[:2] == ['go', 'vet'] and './...' in row['command'], 'Vet report did not execute whole-repository go vet')
    require(summary is not None, 'No full Go raw test log')
    skipped = sorted(name for name, value in summary['outcomes'].items() if value == 'skip')
    require(skipped == sorted(gates['full_go_suite']['skipped_tests']), 'Audit and actual full Go skip lists differ')
    passed_elsewhere = set()
    for role_name in ('gateway_race', 'management_race'):
        role = candidate['evidence_roles'][role_name]
        evidence = document(role['path'], role['sha256'], root)
        require(evidence['go_source_sha256'] == source_hash and evidence['status'] == 'pass'
            and not evidence['skipped_tests'] and not evidence['failed_tests'], 'Supplemental race evidence is not a passing same-candidate run')
        passed_elsewhere.update(evidence['selected_top_level_tests'])
    storage = gates['remaining_storage_race']
    require(storage['status'] == 'pass' and isinstance(storage.get('required_gap_tests'), list), 'Database gaps are not explicitly closed')
    passed_elsewhere.update(storage['required_gap_tests'])
    skipped_top = {name.split('/')[0] for name in skipped}
    unexecuted = sorted(skipped_top - passed_elsewhere)
    require(set(unexecuted) <= OPT_IN_TESTS, 'Uncovered non-opt-in skipped tests: ' + ', '.join(sorted(set(unexecuted) - OPT_IN_TESTS)))
    return {'top_level': summary['top_level'], 'events': summary['events'], 'raw_skipped_tests': skipped,
        'covered_by_separate_linux_pg_redis_runs': sorted(skipped_top & passed_elsewhere),
        'unexecuted_explicit_opt_in_tests': unexecuted, 'vet_passed': True,
        'local_report': {'path': role_path(candidate, 'local_verification'), 'sha256': candidate['evidence_roles']['local_verification']['sha256']},
        'statement': 'Executed ordinary tests and vet passed; listed opt-in tests are unexecuted, not PASS'}


def role_path(candidate, name):
    return candidate['evidence_roles'][name]['path']


def verify_frontend_binding(browser, frontend):
    require(browser.get('frontend_files') == frontend.get('frontend'), 'Browser frontend bytes differ from current capture')
    return {'comparison': 'exact frontend file map, not capture timestamp or metadata-file digest',
        'current_capture_timestamp': frontend.get('captured_at'),
        'historical_browser_evidence_sha256': browser.get('frontend_evidence_sha256'),
        'same_frontend_files': True, 'historical_browser_report_modified': False}


def build(args, audit_report, root=ROOT):
    gates = authorize_gates(audit_report)
    plan = audit_report['candidate_release_plan']
    candidate = document(args.candidate, root=root)
    source_role = candidate['evidence_roles']['source_manifest']
    owner_source = document(source_role['path'], source_role['sha256'], root)
    aliases = gate.source_aliases(owner_source['files'])
    require(aliases == plan['source_aliases'] and aliases['owner_compact_sha256'] == candidate['go_source_sha256'],
        'Candidate source aliases differ from verified audit')
    full = verify_full_go(candidate, aliases['owner_compact_sha256'], gates, root)
    source = {'sha256': aliases['v2_default_json_sha256'], 'files': owner_source['files']}
    frontend = document(args.frontend, root=root)
    migration = document(args.migration, root=root)
    rollback = document(args.rollback, root=root)
    require(frontend['frontend'] == plan['frontend'], 'Frontend capture changed since gate evaluation')
    wrapper = document(args.candidate_browser, root=root)
    browser = document(wrapper['browser_report'], wrapper['browser_report_sha256'], root)
    frontend_binding = verify_frontend_binding(browser, frontend)
    frontend_binding['historical_wrapper_frontend_evidence_sha256'] = wrapper.get('frontend_evidence_sha256')
    bindings = {'source_sha256': source['sha256'], 'binary_sha256': plan['binary_sha256'], 'frontend': frontend['frontend'],
        'migration_evidence_sha256': digest(read(args.migration, root=root)),
        'rollback_evidence_sha256': digest(read(args.rollback, root=root)),
        'frontend_evidence_sha256': digest(read(args.frontend, root=root))}
    result = {'schema': 'codex-agent-pg-upgrade-acceptance-v2', 'status': 'pass', **bindings,
        'accepted_scope': 'functional upgrade readiness only, no capacity claim',
        'source_owner_compact_sha256': aliases['owner_compact_sha256'],
        'checks': dict.fromkeys(POLICY.V2_CHECKS, True),
        'check_evidence': {'go_tests': ['full_go_suite', 'gateway_joint_acceptance', 'management_linux_race', 'remaining_storage_race'],
            'go_vet': ['full_go_suite'], 'backend_build': ['candidate_integrity', 'owner_evidence_integrity'],
            'storage': ['gateway_joint_acceptance', 'management_linux_race', 'remaining_storage_race', 'management_performance'],
            'frontend_preserved': ['preserved_production_frontend'], 'frontend_compatibility': ['candidate_with_preserved_frontend'],
            'additive_migration': ['additive_migration_003'], 'rollback_compatibility': ['previous_binary_rollback']},
        'full_go_execution_scope': full, 'frontend_evidence_binding': frontend_binding,
        'excluded_or_narrowed_gates': [gates[name] for name in sorted(EXCLUDED_GATES)],
        'all_other_gates_passed': [name for name in gates if name not in EXCLUDED_GATES],
        'capacity_verified': False, 'real_concurrency_levels_executed': [], 'capacity_measurements': None,
        'production_migrated': False, 'production_deployed': False,
        'current_production_preflight_required': True, 'goal_completed': False,
        'generated_at': datetime.now(timezone.utc).isoformat(),
        'limitations': ['This acceptance covers functional upgrade readiness, not throughput, success-rate capacity, or real inference load.',
            'Explicit opt-in tests remain unexecuted; their absence is not converted to a passing test result.',
            'Deployment, live environment revalidation and migration are separate operations.',
            'Historical browser evidence is preserved; frontend compatibility is bound to exact file bytes.']}
    POLICY.validate_v2_evidence(bindings, result, migration, rollback, frontend, source)
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--inputs', default=DEFAULT_INPUTS)
    parser.add_argument('--output', help='New output directory within this project')
    options = parser.parse_args()
    stamp = datetime.now(timezone.utc).strftime('%Y%m%dT%H%M%S.%fZ')
    output = local_path(options.output or (ROOT / '.local/gateway-functional-acceptance' / stamp))
    output.mkdir(parents=True, exist_ok=False)
    watched = [local_path(options.inputs), Path(__file__).resolve(), ROOT / 'tools/gateway_release_check.py', ROOT / 'deploy/upgrade_remote.py']
    before = {p: digest(p.read_bytes()) for p in watched}
    args = resolve_inputs(options.inputs)
    report = gate.check(args)
    audit_path = output / 'release-audit.json'
    audit_path.write_text(json.dumps(report, ensure_ascii=False, indent=2) + '\n', encoding='utf-8')
    try:
        acceptance = build(args, report)
        for evidence in report['evidence']:
            read(evidence['path'], evidence['sha256'])
        require(all(digest(p.read_bytes()) == value for p, value in before.items()), 'Input or verifier changed while building acceptance')
        acceptance['supporting_evidence'] = [{'path': p.relative_to(ROOT).as_posix(), 'sha256': digest(p.read_bytes())}
            for p in [*watched, audit_path]]
        acceptance['supporting_report_evidence'] = report['evidence']
        target = output / 'upgrade-acceptance.json'
        target.write_text(json.dumps(acceptance, ensure_ascii=False, indent=2) + '\n', encoding='utf-8')
        print(json.dumps({'status': 'pass', 'scope': acceptance['accepted_scope'], 'capacity_verified': False,
            'source_sha256': acceptance['source_sha256'], 'acceptance': str(target)}, ensure_ascii=False))
        return 0
    except (ReadinessError, POLICY.UpgradeError, OSError, ValueError, KeyError, TypeError) as exc:
        (output / 'blocked.json').write_text(json.dumps({'status': 'blocked', 'acceptance_created': False,
            'reason': str(exc) if isinstance(exc, (ReadinessError, POLICY.UpgradeError)) else type(exc).__name__,
            'audit': str(audit_path)}, indent=2) + '\n', encoding='utf-8')
        print(json.dumps({'status': 'blocked', 'acceptance_created': False, 'report': str(output / 'blocked.json')}, ensure_ascii=False))
        return 2


if __name__ == '__main__':
    raise SystemExit(main())
