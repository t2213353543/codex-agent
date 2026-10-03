"""Verify and freeze the no-billing management candidate without deploying it."""
from __future__ import annotations

import argparse
from collections import Counter
from datetime import datetime, timezone
import hashlib
import io
import json
from pathlib import Path, PurePosixPath
import re
import tarfile
import zipfile

ROOT = Path(__file__).resolve().parents[1]
BASE = ROOT / '.local/management-parity'
REPORT_NAME = 'MANAGEMENT-NO-BILLING-VERIFICATION-2026-09-18.json'
ACCOUNT_GROUPS = ['management', 'operations', 'lifecycle', 'migration', 'runtime', 'lease', 'no_billing']
LOG_GROUPS = ['behavior', 'cleanup', 'noBilling', 'observations', 'review', 'geo', 'session']
BILLING_SOURCE = re.compile(r'LogPricing|LogTimePricing|LogSale|LogCostPolicy|AccountCost|'
                            r'logPricingState|logTimePricingState|logSale|logCostPolicy|accountCost', re.I)


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def sha(raw):
    return hashlib.sha256(raw).hexdigest()


def read(path):
    return json.loads(path.read_text(encoding='utf-8-sig'))


def relative(path):
    return path.relative_to(ROOT).as_posix()


def aggregate(files):
    return sha(json.dumps(files, sort_keys=True, separators=(',', ':'), ensure_ascii=False).encode())


def child(base, name):
    parts = PurePosixPath(name)
    require(not parts.is_absolute() and '..' not in parts.parts and '\\' not in name,
            'Unsafe manifest member: ' + name)
    target = base.joinpath(*parts.parts)
    require(target.resolve().is_relative_to(base.resolve()) and not target.is_symlink(),
            'Manifest path escaped its root: ' + name)
    return target


def verify_files(base, files, exact=False):
    for name, checksum in files.items():
        require(re.fullmatch(r'[0-9a-f]{64}', checksum) is not None, 'Invalid file hash: ' + name)
        target = child(base, name)
        require(target.is_file() and sha(target.read_bytes()) == checksum, 'Changed file: ' + relative(target))
    if exact:
        actual = {path.relative_to(base).as_posix() for path in base.rglob('*') if path.is_file()}
        require(actual == set(files), 'Artifact contains missing or extra files: ' + relative(base))
        require(not any(path.is_symlink() for path in base.rglob('*')), 'Symlink in artifact directory')


def archive_members(path, expected):
    with tarfile.open(path, 'r:gz') as archive:
        members = archive.getmembers()
        require(len(members) == len(expected) and {member.name for member in members} == set(expected),
                'Archive membership mismatch: ' + relative(path))
        for member in members:
            require(member.isfile(), 'Non-file archive entry: ' + member.name)
            require(sha(archive.extractfile(member).read()) == expected[member.name],
                    'Archive content mismatch: ' + member.name)


def go_events(path):
    totals, top, names = Counter(), Counter(), {'pass': set(), 'skip': set(), 'fail': set()}
    for line in path.read_text(encoding='utf-8-sig').splitlines():
        if not line.startswith('{'):
            continue
        try:
            row = json.loads(line)
        except json.JSONDecodeError:
            continue
        action, name = row.get('Action'), row.get('Test')
        if action not in names or not name:
            continue
        totals[action] += 1
        names[action].add(name)
        if '/' not in name:
            top[action] += 1
    return ({key: top[key] for key in names}, {key: totals[key] for key in names}, names)


def validate(args):
    candidate, run = BASE / args.iteration, BASE / args.go_run
    build_raw = (candidate / 'build-manifest.json').read_bytes()
    build, build_sha = json.loads(build_raw), sha(build_raw)
    require(build_sha == args.expected_frontend_manifest, 'Wrong frontend candidate')
    require(len(build['files']) == 54 and build['unchanged_baseline_resources'] == 48,
            'Expected the verified 54-file, 48-baseline restored shell')
    require(build.get('bundled_second_runtime') is False, 'A second runtime was bundled')
    verify_files(ROOT, build['source_files'])
    verify_files(candidate / 'dist', build['files'], exact=True)
    require(aggregate(build['files']) == build['frontend_sha256'], 'Frontend aggregate mismatch')
    require(not any(BILLING_SOURCE.search(name) for name in build['source_files']),
            'Retired billing source remains reachable')

    baseline = ROOT / '.local/ui-releases' / build['legacy_release']
    legacy_raw = (baseline / 'ui-manifest.json').read_bytes()
    require(sha(legacy_raw) == build['legacy_manifest_sha256'], 'Legacy manifest changed')
    legacy = json.loads(legacy_raw)
    verify_files(baseline / 'frontend/dist', legacy['frontend'], exact=True)
    overlay = ROOT / '.local/settings-restore'
    overlay_raw = (overlay / 'build-manifest.json').read_bytes()
    require(sha(overlay_raw) == build['settings_overlay_manifest_sha256'], 'Settings manifest changed')
    overlay_files = json.loads(overlay_raw)['files']
    verify_files(overlay / 'release-dist', overlay_files, exact=True)
    unchanged = {name: value for name, value in overlay_files.items() if name != 'index.html'}
    require(len(unchanged) == 48 and all(build['files'].get(name) == value for name, value in unchanged.items()),
            'Preserved shell resources changed')

    node = read(candidate / 'frontend-unit-report.json')
    accounts = read(candidate / 'accounts-no-billing-verification.json')
    account_browser = read(candidate / 'account-browser/report.json')
    logs = read(candidate / 'logs-verification.json')
    log_browser = read(candidate / 'logs-browser/report.json')
    require(node['status'] == 'pass' and node['exit_code'] == 0 and node['source_matches_build'],
            'Frontend unit validation failed')
    require(node['counts'] == {'tests': 137, 'pass': 137, 'fail': 0, 'skipped': 0}, 'Unexpected active unit results')
    tap = (candidate / 'frontend-unit-tests.log').read_text(encoding='utf-8-sig')
    for key, value in node['counts'].items():
        require(re.search(r'^# ' + key + r' ' + str(value) + r'\s*$', tap, re.M) is not None,
                'Node log disagrees with summary: ' + key)
    require(re.search(r'^# cancelled 0\s*$', tap, re.M), 'Canceled unit tests')
    require(accounts['status'] == 'passed' and not accounts['browser_errors'], 'Accounts validation failed')
    require(account_browser['status'] == 'passed' and not account_browser['errors'], 'Accounts browser failed')
    require(logs['status'] == 'passed' and not logs['page_errors'], 'Logs validation failed')
    require(log_browser['status'] == 'pass' and not log_browser['errors'], 'Logs browser failed')
    require(build_sha == node['build_manifest_sha256'] == accounts['manifest_sha256'] ==
            account_browser['manifest_sha256'] == logs['manifest_sha256'], 'Reports reference different candidates')
    require(logs['scope'] == log_browser['scope'] == 'operational_logs_without_billing', 'Wrong Logs scope')
    require(accounts['scope'] == 'account_management_without_billing', 'Wrong Accounts scope')
    for report in [account_browser, log_browser]:
        require(Path(report['dist']).resolve() == (candidate / 'dist').resolve(), 'Browser used another directory')
    account_count = sum(len(account_browser[key]) for key in ACCOUNT_GROUPS)
    account_count += len(account_browser['external_leases']['assertions'])
    log_count = sum(len(log_browser[key]['assertions']) for key in LOG_GROUPS)
    require(account_count == accounts['browser_groups'] == account_browser['group_count'] == 47,
            'Account behavior count mismatch')
    require(log_count == logs['browser_assertions'] == 35, 'Log behavior count mismatch')
    require(accounts['retired_suites'] and logs['retired_suites'] and log_browser['retirement_reason'],
            'Billing-suite retirement must remain explicit')
    require(logs['sources'] == build['source_files'], 'Logs evidence references different source')
    require(logs['browser_report_sha256'] == sha((candidate / 'logs-browser/report.json').read_bytes()),
            'Logs report changed after verification')
    verify_files(ROOT, accounts['sources'])
    verify_files(candidate, accounts['evidence'])
    require(accounts['evidence'].get('account-browser/report.json') ==
            sha((candidate / 'account-browser/report.json').read_bytes()), 'Accounts report is not hash-bound')
    verify_files(candidate / 'logs-browser', logs['screenshot_sha256'])
    require(set(logs['screenshots']) == set(logs['screenshot_sha256']), 'Screenshot list/hash mismatch')

    source, race, local = [read(run / name) for name in ['source-manifest.json', 'report.json', 'local-verification.json']]
    require(source['sha256'] == race['go_source_sha256'] == local['manifest_sha256'] == args.expected_go_hash,
            'Go reports do not reference the supplied frozen source')
    require(aggregate(source['files']) == source['sha256'] and len(source['files']) == 214, 'Go aggregate mismatch')
    verify_files(run / 'local-source', source['files'])
    require(sha((run / 'source.tar.gz').read_bytes()) == source['archive_sha256'] == local['archive_sha256'],
            'Go source archive changed')
    archive_members(run / 'source.tar.gz', source['files'])
    require(race['status'] == 'pass' and race['exit_code'] == 0 and not race['failed_tests'] and
            not race['skipped_tests'] and not race['missing_tests'], 'Management race did not pass cleanly')
    require(all(race.get(key) is True for key in ['temporary_credentials_removed', 'temporary_container_removed',
                                               'temporary_network_removed']) and not race['cleanup_failures'],
            'Race resources were not cleaned up')
    race_top, race_events, race_names = go_events(run / 'race.log')
    require(race_top == {'pass': 81, 'skip': 0, 'fail': 0} and race_events == {'pass': 212, 'skip': 0, 'fail': 0},
            'Race event log disagrees with expected results')
    require(set(race['selected_top_level_tests']) == {name for name in race_names['pass'] if '/' not in name},
            'Race selected tests were omitted')
    require(local['passed'] and local['source_unchanged'] and local['source_after_sha256'] == source['sha256'] and
            local['archive_members_verified'] == 214 and not local['extra_source_files'], 'Frozen local checks failed')
    checks = {check['label']: check for check in local['checks']}
    require(set(checks) == {'test', 'vet', 'windows-build', 'linux-build'} and
            all(check['exit_code'] == 0 for check in checks.values()), 'Go validation check missing or failed')
    top, events, _ = go_events(run / 'local-test.jsonl')
    require(top == checks['test']['top_level_tests'] == {'pass': 439, 'skip': 92, 'fail': 0}, 'Go test count mismatch')
    require(events == checks['test']['test_events_including_subtests'] == {'pass': 1121, 'skip': 105, 'fail': 0},
            'Go subtest count mismatch')
    for name, artifact in local['artifacts'].items():
        binary = child(run, name)
        require(sha(binary.read_bytes()) == artifact['sha256'] and binary.stat().st_size == artifact['size'],
                'Go binary changed: ' + name)

    preview = BASE / 'no-billing-preview'
    preview_api = read(preview / 'api-verification.json')
    preview_browser = read(preview / 'browser-verification.json')
    preview_stop = read(preview / 'retired-http-stop-report.json')
    for report in [preview_api, preview_browser]:
        require(report['passed'] and report['source_sha256'] == source['sha256'] and
                report['frontend_manifest_sha256'] == build_sha and not report['production_touched'] and
                not report['accounts_imported'] and not report['upstream_requests'], 'Preview evidence mismatch')
    require(preview_api['binary_sha256'] == local['artifacts']['codex-agent-windows-amd64.exe']['sha256'],
            'Preview used another binary')
    for key in ['page_errors', 'static_failures', 'api_failures', 'unexpected_writes', 'external_requests', 'billing_requests']:
        require(not preview_browser[key], 'Preview browser error: ' + key)
    require(preview_browser['credentials_logged'] is False, 'Preview credential logging detected')
    require(preview_stop['passed'] and not preview_stop['old_files_modified'] and not preview_stop['production_touched'],
            'Retired preview stop verification failed')
    return candidate, run, build, build_sha, source, race, local, node, accounts, logs, preview_browser


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--iteration', default='no-billing-20260918')
    parser.add_argument('--go-run', required=True)
    parser.add_argument('--expected-go-hash', required=True)
    parser.add_argument('--expected-frontend-manifest', required=True)
    parser.add_argument('--check-only', action='store_true')
    args = parser.parse_args()
    require(all(re.fullmatch(r'[a-z0-9][a-z0-9-]{0,95}', value) for value in [args.iteration, args.go_run]),
            'Only simple candidate directory names are allowed')
    require(all(re.fullmatch(r'[0-9a-f]{64}', value) for value in [args.expected_go_hash, args.expected_frontend_manifest]),
            'Expected hashes must be SHA-256 values')
    candidate, run, build, build_sha, source, race, local, node, accounts, logs, preview = validate(args)
    if args.check_only:
        print(json.dumps({'status': 'verified', 'manifest_sha256': build_sha, 'go_source_sha256': source['sha256'],
                          'accounts_groups': 47, 'logs_groups': 35, 'node_tests': 137}))
        return
    zip_path = candidate / ('frontend-' + build_sha[:12] + '.zip')
    tar_path = candidate / ('frontend-source-' + build_sha[:12] + '.tar.gz')
    evidence_dir = candidate / ('frozen-evidence-' + build_sha[:12])
    report_paths = [ROOT / 'docs' / REPORT_NAME, candidate / 'frozen-verification.json']
    for target in [zip_path, tar_path, evidence_dir, *report_paths]:
        require(not target.exists(), 'Refusing to replace a frozen artifact: ' + relative(target))

    with zipfile.ZipFile(zip_path, 'x', compression=zipfile.ZIP_DEFLATED) as archive:
        for name, checksum in sorted(build['files'].items()):
            raw = child(candidate / 'dist', name).read_bytes()
            require(sha(raw) == checksum, 'Artifact changed during packaging: ' + name)
            entry = zipfile.ZipInfo(name, (2026, 9, 18, 0, 0, 0))
            entry.compress_type = zipfile.ZIP_DEFLATED
            archive.writestr(entry, raw)
    with zipfile.ZipFile(zip_path) as archive:
        require(len(archive.namelist()) == 54 and set(archive.namelist()) == set(build['files']), 'ZIP membership mismatch')
        for name, checksum in build['files'].items():
            require(sha(archive.read(name)) == checksum, 'ZIP byte verification failed: ' + name)
    with tarfile.open(tar_path, 'x:gz') as archive:
        for name, checksum in sorted(build['source_files'].items()):
            raw = child(ROOT, name).read_bytes()
            require(sha(raw) == checksum, 'Frontend source changed during packaging: ' + name)
            entry = tarfile.TarInfo(name)
            entry.size, entry.mode, entry.mtime = len(raw), 0o644, 0
            archive.addfile(entry, io.BytesIO(raw))
    archive_members(tar_path, build['source_files'])

    # Copy only named verification artifacts; preview folders also contain secrets.
    originals = [(candidate / name, name) for name in ['build-manifest.json', 'frontend-unit-report.json',
                 'frontend-unit-tests.log', 'accounts-no-billing-verification.json', 'logs-verification.json',
                 'account-browser/report.json', 'logs-browser/report.json']]
    originals += [(candidate / name, name) for name in accounts['evidence'] if name.endswith('.png')]
    originals += [(candidate / 'logs-browser' / name, 'logs-browser/' + name) for name in logs['screenshots']]
    originals += [(run / name, 'go/' + name) for name in ['source-manifest.json', 'report.json', 'race.log',
                  'local-verification.json', 'local-test.jsonl', 'local-test-stderr.log', 'local-vet.log',
                  'local-vet-stderr.log', 'local-windows-build.log', 'local-linux-build.log']]
    preview_dir = BASE / 'no-billing-preview'
    originals += [(preview_dir / name, 'real-preview/' + name) for name in ['api-verification.json',
                  'browser-verification.json', 'retired-http-stop-report.json', *preview['screenshots']]]
    evidence_dir.mkdir()
    evidence = []
    for original, name in originals:
        target = child(evidence_dir, name)
        target.parent.mkdir(parents=True, exist_ok=True)
        with target.open('xb') as output:
            output.write(original.read_bytes())
        require(sha(target.read_bytes()) == sha(original.read_bytes()), 'Evidence changed while copying: ' + name)
        evidence.append({'path': relative(target), 'sha256': sha(target.read_bytes())})
    for target in [run / 'source.tar.gz', zip_path, tar_path, Path(__file__).resolve()]:
        evidence.append({'path': relative(target), 'sha256': sha(target.read_bytes())})
    require(sha((candidate / 'build-manifest.json').read_bytes()) == build_sha, 'Build changed during freeze')

    report = {
        'schema': 'codex-no-billing-management-candidate-v1', 'status': 'verified_management_candidate',
        'completed_at': datetime.now(timezone.utc).isoformat(), 'iteration': args.iteration,
        'deployed': False, 'full_reference_parity': False, 'previous_candidates_unchanged': True,
        'scope': 'User-requested removal of all management billing, retaining account and operational usage workflows.',
        'billing': {'enabled': False, 'historical_data_deleted': False,
                    'retired_test_suites': {'accounts': accounts['retired_suites'], 'logs': logs['retired_suites']},
                    'retired_suites_counted_as_passed': False, 'archive': '.local/retired-billing-20260918'},
        'go_source_sha256': source['sha256'], 'go_source_files': len(source['files']),
        'source_archive': relative(run / 'source.tar.gz'), 'source_archive_sha256': source['archive_sha256'],
        'binaries': {name: {'path': relative(run / name), **artifact} for name, artifact in local['artifacts'].items()},
        'frontend': {'manifest_sha256': build_sha, 'frontend_sha256': build['frontend_sha256'],
                     'source_aggregate_sha256': aggregate(build['source_files']), 'entries': build['entries'],
                     'source_file_count': len(build['source_files']), 'file_count': 54, 'unchanged_baseline_resources': 48,
                     'archive': relative(zip_path), 'archive_sha256': sha(zip_path.read_bytes()),
                     'source_archive': relative(tar_path), 'source_archive_sha256': sha(tar_path.read_bytes())},
        'checks': {'management_linux_race': {'passed_top_level': 81, 'passed_events': 212, 'failed': 0, 'skipped': 0,
                                            'postgres_version': race['postgres_version'], 'redis_version': race['redis_version']},
                   'full_go_test': {'pass': 439, 'skip': 92, 'fail': 0},
                   'full_go_test_events': {'pass': 1121, 'skip': 105, 'fail': 0},
                   'vet': 'pass', 'windows_build': 'pass', 'linux_build': 'pass', 'frontend_node': node['counts'],
                   'accounts_browser_groups': 47, 'logs_browser_groups': 35, 'browser_network': 'isolated synthetic API',
                   'screenshots': {'accounts': sum(name.endswith('.png') for name in accounts['evidence']),
                                   'logs': len(logs['screenshots']), 'real_preview': len(preview['screenshots'])}},
        'local_preview': {'url': preview['url'], 'passed': True, 'empty_real_postgres_redis': True,
                          'accounts_imported': 0, 'upstream_requests': 0, 'billing_requests': 0,
                          'production_touched': False, 'credential_files_read_by_freeze': False},
        'source_delta_computed': False,
        'limits': ['Retired billing suites are outside the new user-requested scope, not skipped failures or passed tests.',
                   'The shared frozen Go snapshot includes separately owned gateway code; this management freeze is not its release approval.',
                   'Environment-dependent full-suite skips are reported separately from passes.',
                   'The local preview uses empty isolated stores and performs no account import or real inference.',
                   'No production deployment, data deletion, service startup, service shutdown or rebuild is performed by this tool.'],
        'evidence': evidence,
    }
    encoded = json.dumps(report, ensure_ascii=False, indent=2) + '\n'
    for target in report_paths:
        with target.open('x', encoding='utf-8') as output:
            output.write(encoded)
    print(json.dumps({'status': report['status'], 'report': relative(report_paths[0]),
                      'frontend': report['frontend'], 'go_source_sha256': source['sha256']}, ensure_ascii=False))


if __name__ == '__main__':
    main()
