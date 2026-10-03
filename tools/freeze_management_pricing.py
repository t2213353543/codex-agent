"""Package a verified pricing candidate without deploying or replacing older packages."""
import hashlib
import io
import json
from pathlib import Path
import shutil
import tarfile
from datetime import datetime, timezone
import zipfile

ROOT = Path(__file__).resolve().parents[1]
BASE = ROOT / '.local/management-parity/pricing-next-20260918'
RACE = ROOT / '.local/management-parity/local-race-20260918t024218-3fdde7'
FULL = ROOT / '.local/management-parity/local-race-20260918t023843-623047'
PREVIOUS = ROOT / '.local/management-parity/linux-race-20260917T214152-2d29dc'
digest = lambda raw: hashlib.sha256(raw).hexdigest()
read = lambda path: json.loads(path.read_text(encoding='utf-8-sig'))
relative = lambda path: path.relative_to(ROOT).as_posix()

manifest = read(BASE / 'build-manifest.json')
manifest_sha = digest((BASE / 'build-manifest.json').read_bytes())
for name, expected in manifest['source_files'].items():
    assert digest((ROOT / name).read_bytes()) == expected, 'Frontend changed: ' + name
files = {path.relative_to(BASE / 'dist').as_posix(): digest(path.read_bytes()) for path in (BASE / 'dist').rglob('*') if path.is_file()}
assert files == manifest['files'] and len(files) == 54
assert manifest['unchanged_baseline_resources'] == 48 and not manifest['bundled_second_runtime']
node, accounts, logs = [read(BASE / name) for name in ['frontend-unit-report.json', 'account-procurement-verification.json', 'logs-verification.json']]
assert node['status'] == 'pass' and node['counts']['fail'] == 0 and node['counts']['skipped'] == 0
assert accounts['status'] == 'passed' and not accounts['browser_errors']
assert logs['status'] == 'passed' and not logs['page_errors']
assert node['build_manifest_sha256'] == accounts['build_manifest_sha256'] == logs['manifest_sha256'] == manifest_sha
for item in accounts['final_candidate_validation']['matched']:
    assert files[item['path']] == item['sha256']
for folder in ['account-cost-browser', 'account-regression-browser', 'account-runtime-browser']:
    result = read(BASE / folder / 'report.json')
    assert result['status'] == 'passed' and not result['errors']
result = read(BASE / 'logs-browser/report.json')
assert result['status'] == 'pass' and not result['errors']

race, local = read(RACE / 'report.json'), read(FULL / 'local-verification.json')
go_manifest = read(RACE / 'source-manifest.json')
assert race['status'] == 'pass' and not race['failed_tests'] and not race['skipped_tests'] and not race['missing_tests']
assert not race['go_source_changed_after_freeze'] and race['temporary_credentials_removed'] and not race['cleanup_failures']
assert local['passed'] and local['source_unchanged']
assert race['go_source_sha256'] == local['manifest_sha256'] == go_manifest['sha256']
assert go_manifest['files'] == read(FULL / 'source-manifest.json')['files']
assert digest((RACE / 'source.tar.gz').read_bytes()) == go_manifest['archive_sha256']
for name, info in local['artifacts'].items():
    assert digest((FULL / name).read_bytes()) == info['sha256']
old = read(PREVIOUS / 'source-manifest.json')['files']
added = sorted(set(go_manifest['files']) - set(old))
changed = sorted(name for name in set(old) & set(go_manifest['files']) if old[name] != go_manifest['files'][name])
assert not set(old) - set(go_manifest['files'])

archive = BASE / ('frontend-' + manifest_sha[:12] + '.zip')
with zipfile.ZipFile(archive, 'x', compression=zipfile.ZIP_DEFLATED) as out:
    for name in sorted(files):
        raw = (BASE / 'dist' / name).read_bytes()
        assert digest(raw) == files[name]
        entry = zipfile.ZipInfo(name, (2026, 9, 18, 0, 0, 0))
        entry.compress_type = zipfile.ZIP_DEFLATED
        out.writestr(entry, raw)
with zipfile.ZipFile(archive) as verify:
    assert set(verify.namelist()) == set(files)
    assert all(digest(verify.read(name)) == files[name] for name in files)
frontend_source = BASE / ('frontend-source-' + manifest_sha[:12] + '.tar.gz')
with tarfile.open(frontend_source, 'x:gz') as out:
    for name, expected in sorted(manifest['source_files'].items()):
        raw = (ROOT / name).read_bytes()
        assert digest(raw) == expected
        entry = tarfile.TarInfo(name)
        entry.size, entry.mode, entry.mtime = len(raw), 0o644, 0
        out.addfile(entry, io.BytesIO(raw))

evidence = []
items = [(BASE / 'build-manifest.json', 'frontend-build-manifest.json'),
         (BASE / 'frontend-unit-report.json', 'frontend-unit-report.json'),
         (BASE / 'frontend-unit-tests.log', 'frontend-unit-tests.log'),
         (BASE / 'account-procurement-verification.json', 'accounts-verification.json'),
         (BASE / 'logs-verification.json', 'logs-verification.json'),
         (BASE / 'logs-browser/report.json', 'logs-browser-report.json'),
         (FULL / 'local-verification.json', 'local-verification.json')]
for folder in ['account-cost-browser', 'account-regression-browser', 'account-runtime-browser']:
    items.append((BASE / folder / 'report.json', folder + '.json'))
for original, name in items:
    target = RACE / name
    assert not target.exists(), 'Do not overwrite evidence'
    shutil.copy2(original, target)
    evidence.append({'path': relative(target), 'sha256': digest(target.read_bytes())})
for target in [RACE / 'report.json', RACE / 'race.log', RACE / 'source-manifest.json', archive, frontend_source]:
    evidence.append({'path': relative(target), 'sha256': digest(target.read_bytes())})
checks = {check['label']: check for check in local['checks']}
report = {
    'schema': 'codex-management-pricing-verification-v1', 'status': 'verified_management_candidate',
    'completed_at': datetime.now(timezone.utc).isoformat(), 'deployed': False, 'full_reference_parity': False,
    'previous_candidates_unchanged': True, 'go_source_sha256': go_manifest['sha256'],
    'source_archive': relative(RACE / 'source.tar.gz'), 'source_archive_sha256': go_manifest['archive_sha256'],
    'linux_binary': relative(FULL / 'codex-agent-linux-amd64'),
    'linux_binary_sha256': local['artifacts']['codex-agent-linux-amd64']['sha256'],
    'checks': {
        'management_linux_race': {'passed_top_level': race['passed_top_level_tests'], 'passed_events': race['passed_test_events'], 'failed': 0, 'skipped': 0, 'postgres_version': race['postgres_version'], 'redis_version': race['redis_version']},
        'full_go_test': checks['test']['top_level_tests'], 'full_go_test_events': checks['test']['test_events_including_subtests'],
        'vet': 'pass', 'windows_build': 'pass', 'linux_build': 'pass',
        'frontend_node': node['counts'], 'accounts_browser_groups': sum(accounts['browser_groups'].values()),
        'logs_browser_groups': logs['browser_assertions'], 'browser_network': 'synthetic',
        'screenshots': {'accounts': accounts['visual_checks']['screenshots'], 'logs_four_theme': len(logs['screenshots'])},
    },
    'frontend': {'manifest_sha256': manifest_sha, 'frontend_sha256': manifest['frontend_sha256'], 'entries': manifest['entries'], 'file_count': len(files),
                 'unchanged_baseline_resources': 48, 'archive': relative(archive), 'archive_sha256': digest(archive.read_bytes()),
                 'source_archive': relative(frontend_source), 'source_archive_sha256': digest(frontend_source.read_bytes())},
    'source_delta_from_previous': {'added': added, 'modified': changed, 'removed': [],
        'management_files': [name for name in added + changed if name.startswith('admin_')],
        'separate_owner_gateway_files': [name for name in added + changed if not name.startswith('admin_')],
        'migrations_changed': False},
    'implemented': ['Explicit IANA weekday/time-window reference pricing, exact decimals, composition with long-context pricing, same file/PG estimates and exports.',
                    'Independent versioned procurement rates and multiplier by stable account or explicit group, immutable history and disable versions, CAS/atomic audit, actual-record preview.',
                    'Procurement amount and known/unknown coverage in request details, full stats, trend, dimensions, rankings and CSV/JSONL, with default source/supplier/identity redaction.',
                    'Detail now uses the displayed list pricing/record snapshot; PostgreSQL reference/procurement aggregates share a read-only repeatable-read transaction.'],
    'limits': ['Management tests use synthetic data and local ephemeral PG/Redis, never production inference.',
               'First local-race run completed Go tests but report parsing failed on go-env scalar output. The runner was corrected and the same source was rerun to completion; the failed report remains.',
               'Go archive includes separate gateway owners additions; this management acceptance does not certify their complete runtime behavior or authorize a shared-tree deployment.',
               'Unimplemented reference scope includes explicit customer-pricing group/KEY overrides, channel entities/model-price override precedence, subscription peak policy and procurement cost tiers.',
               'Per-account gateway policies, full retry/model/endpoint observation and historical missing usage remain separate unfinished requirements; no fake switches, prices or charges added.',
               'Production joint acceptance and reviewed migration-003 release remain separate; no deployment performed.'],
    'evidence': evidence,
}
for path in [ROOT / 'docs/MANAGEMENT-PRICING-VERIFICATION-2026-09-18.json', RACE / 'management-pricing-verification.json']:
    with path.open('x', encoding='utf-8') as out:
        out.write(json.dumps(report, ensure_ascii=False, indent=2) + '\n')
print(json.dumps({'status': report['status'], 'frontend': report['frontend'], 'checks': report['checks'], 'go_sha256': go_manifest['sha256']}, ensure_ascii=False))
