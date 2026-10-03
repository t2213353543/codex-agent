"""Freeze a fully verified management iteration without deploying it."""
from __future__ import annotations

import argparse
from datetime import datetime, timezone
import hashlib
import io
import json
from pathlib import Path
import re
import shutil
import tarfile
import zipfile

ROOT = Path(__file__).resolve().parents[1]
BASE = ROOT / '.local/management-parity'
sha = lambda raw: hashlib.sha256(raw).hexdigest()
read = lambda path: json.loads(path.read_text(encoding='utf-8-sig'))
relative = lambda path: path.relative_to(ROOT).as_posix()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--iteration', required=True)
    parser.add_argument('--go-run', required=True)
    parser.add_argument('--previous-run', required=True)
    parser.add_argument('--report-name', required=True)
    parser.add_argument('--expected-go-hash', required=True)
    args = parser.parse_args()
    for value in [args.iteration, args.go_run, args.previous_run]:
        if not re.fullmatch(r'[a-z0-9][a-zA-Z0-9-]{0,95}', value):
            raise ValueError('Expected a simple artifact directory name')
    if not re.fullmatch(r'[A-Z0-9-]+', args.report_name):
        raise ValueError('Expected an uppercase report stem')
    candidate, run, previous = BASE / args.iteration, BASE / args.go_run, BASE / args.previous_run
    target_report = ROOT / 'docs' / (args.report_name + '.json')
    if target_report.exists() or (run / 'management-verification.json').exists():
        raise RuntimeError('Refusing to overwrite a fixed candidate report')
    build_raw = (candidate / 'build-manifest.json').read_bytes()
    build, build_sha = json.loads(build_raw), sha(build_raw)
    for name, checksum in build['source_files'].items():
        path = (ROOT / name).resolve()
        assert path.is_relative_to(ROOT.resolve()) and sha(path.read_bytes()) == checksum, 'Frontend source changed: ' + name
    files = {path.relative_to(candidate / 'dist').as_posix(): sha(path.read_bytes())
             for path in (candidate / 'dist').rglob('*') if path.is_file()}
    assert files == build['files'] and build['unchanged_baseline_resources'] == 48
    assert not build['bundled_second_runtime']
    node = read(candidate / 'frontend-unit-report.json')
    accounts = read(candidate / 'account-procurement-verification.json')
    logs = read(candidate / 'logs-verification.json')
    assert node['status'] == 'pass' and not node['counts']['fail'] and not node['counts']['skipped']
    assert accounts['status'] == 'passed' and not accounts['browser_errors']
    assert logs['status'] == 'passed' and not logs['page_errors']
    assert build_sha == node['build_manifest_sha256'] == accounts['build_manifest_sha256'] == logs['manifest_sha256']
    for item in accounts['final_candidate_validation']['matched']:
        assert files[item['path']] == item['sha256'], 'Account resources changed'
    for folder in ['account-cost-browser', 'account-runtime-browser', 'account-regression-browser', 'logs-browser']:
        result = read(candidate / folder / 'report.json')
        assert result['status'] in ('passed', 'pass') and not result['errors']
    extra_folders = []
    extra_groups = {}
    if (candidate / 'account-daily-browser/report.json').exists():
        result = read(candidate / 'account-daily-browser/report.json')
        assert result['status'] == 'passed' and not result['errors']
        for name, checksum in result['asset_sha256'].items():
            assert files[name] == checksum, 'Daily stats loaded another candidate'
        extra_folders.append('account-daily-browser')
    if (candidate / 'lease-browser/report.json').exists():
        result = read(candidate / 'lease-browser/report.json')
        assert result['passed'] and not result['errors'] and result['manifest_sha256'] == build_sha
        extra_folders.append('lease-browser')
        extra_groups['lease_restore'] = len(result['checks'])
    if (candidate / 'cost-backup-browser/report.json').exists():
        result = read(candidate / 'cost-backup-browser/report.json')
        assert result['status'] == 'passed' and not result['errors'] and result['manifest_sha256'] == build_sha
        extra_folders.append('cost-backup-browser')
    if (candidate / 'external-import-browser/report.json').exists():
        result = read(candidate / 'external-import-browser/report.json')
        assert result['status'] == 'passed' and not result['errors'] and result['manifest_sha256'] == build_sha
        for name, checksum in result['assets'].items():
            assert files[name] == checksum, 'External import loaded another candidate'
        extra_folders.append('external-import-browser')
        extra_groups['external_import'] = len(result['behavior']['assertions'])
        if result.get('cost_behavior'):
            extra_groups['external_cost_import'] = len(result['cost_behavior']['assertions'])
    race, local, source = [read(run / name) for name in ['report.json', 'local-verification.json', 'source-manifest.json']]
    assert race['status'] == 'pass' and not race['failed_tests'] and not race['skipped_tests'] and not race['missing_tests']
    assert race['temporary_credentials_removed'] and race['temporary_container_removed'] and not race.get('cleanup_failures')
    assert local['passed'] and local['source_unchanged']
    assert source['sha256'] == race['go_source_sha256'] == local['manifest_sha256'] == args.expected_go_hash
    assert sha((run / 'source.tar.gz').read_bytes()) == source['archive_sha256']
    for name, checksum in source['files'].items():
        assert sha((run / 'local-source' / name).read_bytes()) == checksum, 'Frozen Go file changed'
    for name, artifact in local['artifacts'].items():
        assert sha((run / name).read_bytes()) == artifact['sha256'], 'Binary changed'
    before = read(previous / 'source-manifest.json')['files']
    added = sorted(set(source['files']) - set(before))
    changed = sorted(name for name in before.keys() & source['files'].keys() if before[name] != source['files'][name])
    removed = sorted(set(before) - set(source['files']))
    zip_path = candidate / ('frontend-' + build_sha[:12] + '.zip')
    tar_path = candidate / ('frontend-source-' + build_sha[:12] + '.tar.gz')
    assert not zip_path.exists() and not tar_path.exists(), 'Do not replace fixed archives'
    with zipfile.ZipFile(zip_path, 'x', compression=zipfile.ZIP_DEFLATED) as archive:
        for name in sorted(files):
            raw = (candidate / 'dist' / name).read_bytes()
            assert sha(raw) == files[name]
            entry = zipfile.ZipInfo(name, (2026, 9, 18, 0, 0, 0))
            entry.compress_type = zipfile.ZIP_DEFLATED
            archive.writestr(entry, raw)
    with zipfile.ZipFile(zip_path) as archive:
        assert set(archive.namelist()) == set(files)
        assert all(sha(archive.read(name)) == checksum for name, checksum in files.items())
    with tarfile.open(tar_path, 'x:gz') as archive:
        for name, checksum in sorted(build['source_files'].items()):
            raw = (ROOT / name).read_bytes()
            assert sha(raw) == checksum
            entry = tarfile.TarInfo(name)
            entry.size, entry.mode, entry.mtime = len(raw), 0o644, 0
            archive.addfile(entry, io.BytesIO(raw))
    evidence = []
    originals = [(candidate / name, name) for name in ['build-manifest.json', 'frontend-unit-report.json', 'frontend-unit-tests.log', 'account-procurement-verification.json', 'logs-verification.json']]
    originals += [(candidate / folder / 'report.json', folder + '-report.json') for folder in ['account-cost-browser', 'account-runtime-browser', 'account-regression-browser', 'logs-browser']]
    originals += [(candidate / folder / 'report.json', folder + '-report.json') for folder in extra_folders]
    evidence_dir = run / 'management-evidence'
    evidence_dir.mkdir(exist_ok=False)
    for original, name in originals:
        path = evidence_dir / name
        shutil.copy2(original, path)
        evidence.append({'path': relative(path), 'sha256': sha(path.read_bytes())})
    for path in [run / 'report.json', run / 'race.log', run / 'local-verification.json', run / 'source-manifest.json', zip_path, tar_path]:
        evidence.append({'path': relative(path), 'sha256': sha(path.read_bytes())})
    checks = {item['label']: item for item in local['checks']}
    report = {
        'schema': 'codex-management-candidate-v1', 'status': 'verified_management_candidate',
        'completed_at': datetime.now(timezone.utc).isoformat(), 'iteration': args.iteration,
        'deployed': False, 'full_reference_parity': False, 'previous_candidates_unchanged': True,
        'go_source_sha256': source['sha256'], 'source_archive': relative(run / 'source.tar.gz'),
        'source_archive_sha256': source['archive_sha256'], 'linux_binary': relative(run / 'codex-agent-linux-amd64'),
        'linux_binary_sha256': local['artifacts']['codex-agent-linux-amd64']['sha256'],
        'checks': {
            'management_linux_race': {'passed_top_level': race['passed_top_level_tests'], 'passed_events': race['passed_test_events'], 'failed': 0, 'skipped': 0, 'postgres_version': race.get('postgres_version'), 'redis_version': race.get('redis_version')},
            'full_go_test': checks['test']['top_level_tests'], 'full_go_test_events': checks['test']['test_events_including_subtests'],
            'vet': 'pass', 'windows_build': 'pass', 'linux_build': 'pass', 'frontend_node': node['counts'],
            'accounts_browser_groups': sum(accounts['browser_groups'].values()), 'logs_browser_groups': logs['browser_assertions'], 'additional_browser_groups': extra_groups,
            'browser_network': 'synthetic', 'screenshots': {'accounts': accounts['visual_checks']['screenshots'], 'logs_four_theme': len(logs['screenshots'])}},
        'frontend': {'manifest_sha256': build_sha, 'frontend_sha256': build['frontend_sha256'], 'entries': build['entries'], 'file_count': len(files), 'unchanged_baseline_resources': 48,
                     'archive': relative(zip_path), 'archive_sha256': sha(zip_path.read_bytes()), 'source_archive': relative(tar_path), 'source_archive_sha256': sha(tar_path.read_bytes())},
        'source_delta': {'previous_go_sha256': read(previous / 'source-manifest.json')['sha256'], 'added': added, 'modified': changed, 'removed': removed,
                         'management_files': [name for name in added + changed if name.startswith('admin_')],
                         'separate_owner_files': [name for name in added + changed if not name.startswith('admin_')],
                         'migrations_changed': any(name.startswith('migrations/') for name in added + changed + removed)},
        'scope': 'Codex accounts and retained usage management, preserving restored blue UI; no deployment or live inference.',
        'limits': ['The Go snapshot includes separately owned gateway changes. Management acceptance does not certify or release the gateway owners in-progress features.',
                   'User/group billing entities, full account behavior and remaining passive observation requirements must still be audited against the full objective.',
                   'Synthetic backend/browser tests do not prove live third-party availability, real charges, or restoration of historically missing usage.',
                   'Unknown and true zero remain distinct; configured reference, procurement and sale estimates are not actual subscription debits.'],
        'evidence': evidence,
    }
    for path in [target_report, run / 'management-verification.json']:
        with path.open('x', encoding='utf-8') as out:
            out.write(json.dumps(report, ensure_ascii=False, indent=2) + '\n')
    print(json.dumps({'status': report['status'], 'report': relative(target_report), 'frontend': report['frontend'], 'checks': report['checks']}, ensure_ascii=False))


if __name__ == '__main__':
    main()
