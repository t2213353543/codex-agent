"""Package the verified operational fixes without rebuilding or touching services."""
import io
import json
import tarfile
import zipfile
from datetime import datetime, timezone
from pathlib import Path
from freeze_no_billing_candidate import ROOT, BASE, read, sha, aggregate, child, verify_files, archive_members, go_events, relative, require

candidate = BASE / 'operational-fixes-20260918'
run = BASE / 'local-race-20260918t065425-d36576'
expected_frontend = '0e286edbfa6e169513ab62d8bdfa57552867e8a7a7ad3cdf664a140805ab36af'
expected_go = 'bdfbf92ba3102028440c00f75fb8d728f117fc5f4f3f227782429198b2147c82'
build = read(candidate / 'build-manifest.json')
require(sha((candidate / 'build-manifest.json').read_bytes()) == expected_frontend, 'Wrong frontend manifest')
verify_files(ROOT, build['source_files'])
verify_files(candidate / 'dist', build['files'], exact=True)
require(len(build['files']) == 54 and build['unchanged_baseline_resources'] == 48 and not build['bundled_second_runtime'], 'Wrong restored shell')
require(aggregate(build['files']) == build['frontend_sha256'], 'Frontend aggregate mismatch')
baseline = read(ROOT / '.local/settings-restore/build-manifest.json')['files']
require(sum(name != 'index.html' and build['files'].get(name) == digest for name, digest in baseline.items()) == 48, 'Baseline resource mismatch')

logs, accounts, unit = [read(candidate / name) for name in ['logs-verification.json', 'accounts-no-billing-verification.json', 'frontend-unit-report.json']]
for report in [logs, accounts]:
    require(report['status'] == 'passed' and report['manifest_sha256'] == expected_frontend and report['frontend_sha256'] == build['frontend_sha256'], 'Browser evidence candidate mismatch')
    verify_files(candidate, report['evidence'])
require(logs['browser_assertions'] == 38 and not logs['page_errors'], 'Logs acceptance failed')
require(accounts['browser_groups'] == 53 and not accounts['browser_errors'], 'Accounts acceptance failed')
require(unit['status'] == 'pass' and unit['source_matches_build'] and unit['build_manifest_sha256'] == expected_frontend and unit['counts'] == {'tests': 146, 'pass': 146, 'fail': 0, 'skipped': 0}, 'Node acceptance failed')
source, race, local = [read(run / name) for name in ['source-manifest.json', 'report.json', 'local-verification.json']]
require(source['sha256'] == race['go_source_sha256'] == local['manifest_sha256'] == expected_go and local['source_unchanged'], 'Go source mismatch')
require(sha((run / 'source.tar.gz').read_bytes()) == source['archive_sha256'] == local['archive_sha256'], 'Go archive changed')
archive_members(run / 'source.tar.gz', source['files'])
require(race['status'] == 'pass' and not any(race[name] for name in ['failed_tests', 'skipped_tests', 'missing_tests']), 'Linux race failed')
top, events, names = go_events(run / 'race.log')
require(top == {'pass': 94, 'skip': 0, 'fail': 0} and events == {'pass': 277, 'skip': 0, 'fail': 0}, 'Race counts mismatch')
require(set(race['selected_top_level_tests']).issubset(names['pass']), 'Race plan incomplete')
full_top, full_events, _ = go_events(run / 'local-test.jsonl')
require(full_top == {'pass': 454, 'skip': 103, 'fail': 0} and full_events == {'pass': 1176, 'skip': 116, 'fail': 0}, 'Full Go counts mismatch')
require(local['passed'] and all(item['exit_code'] == 0 for item in local['checks']), 'Go checks failed')
verify_files(run, {name: value['sha256'] for name, value in local['artifacts'].items()})

suffix = expected_frontend[:12]
zip_path, tar_path = candidate / f'frontend-{suffix}.zip', candidate / f'frontend-source-{suffix}.tar.gz'
evidence_dir = candidate / f'frozen-evidence-{suffix}'
reports = [candidate / 'frozen-verification.json', ROOT / 'docs/MANAGEMENT-OPERATIONAL-FIXES-VERIFICATION-2026-09-18.json']
for target in [zip_path, tar_path, evidence_dir, *reports]:
    require(not target.exists(), 'Refusing to overwrite: ' + relative(target))
with zipfile.ZipFile(zip_path, 'x', compression=zipfile.ZIP_DEFLATED) as archive:
    for name, digest in sorted(build['files'].items()):
        raw = child(candidate / 'dist', name).read_bytes()
        require(sha(raw) == digest, 'Asset changed while packaging')
        entry = zipfile.ZipInfo(name, (2026, 9, 18, 0, 0, 0)); entry.compress_type = zipfile.ZIP_DEFLATED
        archive.writestr(entry, raw)
with zipfile.ZipFile(zip_path) as archive:
    require(set(archive.namelist()) == set(build['files']), 'ZIP members differ')
    for name, digest in build['files'].items():
        require(sha(archive.read(name)) == digest, 'ZIP byte mismatch')
with tarfile.open(tar_path, 'x:gz') as archive:
    for name, digest in sorted(build['source_files'].items()):
        raw = child(ROOT, name).read_bytes(); require(sha(raw) == digest, 'Source changed while packaging')
        entry = tarfile.TarInfo(name); entry.size = len(raw); entry.mode = 0o644; entry.mtime = 0
        archive.addfile(entry, io.BytesIO(raw))
archive_members(tar_path, build['source_files'])
evidence_dir.mkdir()
evidence_names = {'build-manifest.json', 'logs-verification.json', 'accounts-no-billing-verification.json', 'frontend-unit-report.json', *logs['evidence'], *accounts['evidence']}
originals = [(child(candidate, name), name) for name in sorted(evidence_names)]
originals += [(run / name, 'go/' + name) for name in ['source-manifest.json', 'report.json', 'race.log', 'local-verification.json', 'local-test.jsonl']]
evidence = []
for original, name in originals:
    target = child(evidence_dir, name); target.parent.mkdir(parents=True, exist_ok=True)
    raw = original.read_bytes()
    with target.open('xb') as output: output.write(raw)
    require(sha(target.read_bytes()) == sha(raw), 'Evidence copy mismatch')
    evidence.append({'path': relative(target), 'sha256': sha(raw)})
for file in [zip_path, tar_path, run / 'source.tar.gz', Path(__file__).resolve()]:
    evidence.append({'path': relative(file), 'sha256': sha(file.read_bytes())})
report = {
    'schema': 'codex-management-operational-fixes-v1', 'status': 'verified_management_candidate',
    'completed_at': datetime.now(timezone.utc).isoformat(), 'iteration': candidate.name,
    'deployed': False, 'full_reference_parity': False, 'previous_candidates_unchanged': True,
    'scope': 'Non-billing account scheduling/query resilience and operational usage time/coverage fixes.',
    'separate_owner_changes': ['The frozen Go source also contains separately owned native startup and WebSocket budget changes; this management artifact is not their release approval.'],
    'billing_enabled': False, 'retired_suites_counted_as_passed': False,
    'go_source_sha256': expected_go, 'go_source_files': len(source['files']),
    'source_archive': relative(run / 'source.tar.gz'), 'source_archive_sha256': source['archive_sha256'],
    'binaries': {name: {'path': relative(run / name), **value} for name, value in local['artifacts'].items()},
    'frontend': {'manifest_sha256': expected_frontend, 'frontend_sha256': build['frontend_sha256'], 'source_aggregate_sha256': aggregate(build['source_files']), 'entries': build['entries'], 'file_count': 54, 'source_file_count': len(build['source_files']), 'unchanged_baseline_resources': 48, 'archive': relative(zip_path), 'archive_sha256': sha(zip_path.read_bytes()), 'source_archive': relative(tar_path), 'source_archive_sha256': sha(tar_path.read_bytes())},
    'checks': {'management_linux_race': top, 'management_linux_race_events': events, 'full_go_test': full_top, 'full_go_test_events': full_events, 'vet': 'pass', 'windows_build': 'pass', 'linux_build': 'pass', 'frontend_node': unit['counts'], 'accounts_browser_groups': 53, 'logs_browser_groups': 38, 'browser_network': 'isolated synthetic API', 'screenshots': {'accounts': sum(name.endswith('.png') for name in accounts['evidence']), 'logs': len(logs['screenshots'])}},
    'preview_evidence': 'Published separately by the local preview owner; no preview directories or credentials read by this packager.',
    'limits': ['Environment-dependent Go skips remain separate from passes.', 'Historical missing observations remain unknown.', 'No deployment, rebuild, service change, upstream request or user data deletion performed.'],
    'evidence': evidence,
}
for target in reports:
    with target.open('x', encoding='utf-8') as output: json.dump(report, output, ensure_ascii=False, indent=2); output.write('\n')
print(json.dumps({'status': report['status'], 'report': relative(reports[1]), 'frontend': report['frontend']}, ensure_ascii=False))
