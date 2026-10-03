"""Package a verified management candidate using its recorded test evidence."""
import argparse
import io
import json
import re
import tarfile
import zipfile
from datetime import datetime, timezone
from pathlib import Path
from freeze_no_billing_candidate import ROOT, BASE, read, sha, aggregate, child, verify_files, archive_members, go_events, relative, require

parser = argparse.ArgumentParser(description=__doc__)
for name in ['iteration', 'go-run', 'frontend-hash', 'go-hash', 'report-stem']:
    parser.add_argument('--' + name, required=True)
parser.add_argument('--supplementary-go-run')
args = parser.parse_args()
require(all(re.fullmatch(r'[A-Za-z0-9_-]+', value) for value in [args.iteration, args.go_run, args.report_stem]), 'Unsafe artifact name')
require(not args.supplementary_go_run or re.fullmatch(r'[A-Za-z0-9_-]+', args.supplementary_go_run), 'Unsafe supplementary run name')
require(all(re.fullmatch(r'[0-9a-f]{64}', value) for value in [args.frontend_hash, args.go_hash]), 'Invalid SHA-256')
candidate, run = BASE / args.iteration, BASE / args.go_run
build = read(candidate / 'build-manifest.json')
require(sha((candidate / 'build-manifest.json').read_bytes()) == args.frontend_hash, 'Wrong frontend manifest')
verify_files(ROOT, build['source_files']); verify_files(candidate / 'dist', build['files'], exact=True)
require(len(build['files']) == 54 and build['unchanged_baseline_resources'] == 48 and not build['bundled_second_runtime'], 'Wrong restored shell')
require(aggregate(build['files']) == build['frontend_sha256'], 'Frontend aggregate mismatch')
baseline = read(ROOT / '.local/settings-restore/build-manifest.json')['files']
require(sum(name != 'index.html' and build['files'].get(name) == digest for name, digest in baseline.items()) == 48, 'Baseline resource mismatch')
logs, accounts, unit = [read(candidate / name) for name in ['logs-verification.json', 'accounts-no-billing-verification.json', 'frontend-unit-report.json']]
for report in [logs, accounts]:
    require(report['status'] == 'passed' and report['manifest_sha256'] == args.frontend_hash and report['frontend_sha256'] == build['frontend_sha256'], 'Browser evidence candidate mismatch')
    verify_files(candidate, report['evidence'])
log_browser, account_browser = [read(candidate / name) for name in ['logs-browser/report.json', 'account-browser/report.json']]
require(log_browser['status'] == 'pass' and not log_browser['errors'] and not logs['page_errors'], 'Logs acceptance failed')
log_count = sum(len(value['assertions']) for value in log_browser.values() if isinstance(value, dict) and isinstance(value.get('assertions'), list))
require(log_count == logs['browser_assertions'], 'Logs group count mismatch')
require(account_browser['status'] == 'passed' and not account_browser['errors'] and account_browser['group_count'] == accounts['browser_groups'], 'Accounts acceptance failed')
require(unit['status'] == 'pass' and unit['source_matches_build'] and unit['build_manifest_sha256'] == args.frontend_hash, 'Node candidate mismatch')
tap = (candidate / 'frontend-unit.tap').read_text(encoding='utf-8-sig')
counts = {name: int(re.search(r'^# ' + name + r' (\d+)$', tap, re.M)[1]) for name in ['tests', 'pass', 'fail', 'skipped']}
require(counts == unit['counts'] and counts['tests'] == counts['pass'] and not counts['fail'] and not counts['skipped'], 'Node acceptance failed')
source, race, local = [read(run / name) for name in ['source-manifest.json', 'report.json', 'local-verification.json']]
require(source['sha256'] == race['go_source_sha256'] == local['manifest_sha256'] == args.go_hash and local['source_unchanged'], 'Go source mismatch')
require(sha((run / 'source.tar.gz').read_bytes()) == source['archive_sha256'] == local['archive_sha256'], 'Go archive changed')
archive_members(run / 'source.tar.gz', source['files'])
require(race['status'] == 'pass' and not any(race[name] for name in ['failed_tests', 'skipped_tests', 'missing_tests']), 'Linux race failed')
top, events, names = go_events(run / 'race.log')
require(top == {'pass': race['passed_top_level_tests'], 'skip': 0, 'fail': 0} and events == {'pass': race['passed_test_events'], 'skip': 0, 'fail': 0}, 'Race counts mismatch')
require(set(race['selected_top_level_tests']).issubset(names['pass']), 'Race plan incomplete')
full_top, full_events, _ = go_events(run / 'local-test.jsonl')
test_report = next(check for check in local['checks'] if check['label'] == 'test')
require(full_top == test_report['top_level_tests'] and full_events == test_report['test_events_including_subtests'] and not full_top['fail'], 'Full Go counts mismatch')
require(local['passed'] and all(item['exit_code'] == 0 for item in local['checks']), 'Go checks failed')
verify_files(run, {name: value['sha256'] for name, value in local['artifacts'].items()})
supplementary = None
if args.supplementary_go_run:
    extra_run = BASE / args.supplementary_go_run
    extra_source, extra_race = [read(extra_run / name) for name in ['source-manifest.json', 'report.json']]
    require(extra_source['sha256'] == extra_race['go_source_sha256'] == args.go_hash, 'Supplementary Go source mismatch')
    require(extra_source['files'] == source['files'], 'Supplementary source membership differs')
    require(extra_race['status'] == 'pass' and not any(extra_race[name] for name in ['failed_tests', 'skipped_tests', 'missing_tests']), 'Supplementary race failed')
    extra_top, extra_events, extra_names = go_events(extra_run / 'race.log')
    require(extra_top == {'pass': extra_race['passed_top_level_tests'], 'skip': 0, 'fail': 0} and extra_events == {'pass': extra_race['passed_test_events'], 'skip': 0, 'fail': 0}, 'Supplementary race counts mismatch')
    require(set(extra_race['selected_top_level_tests']).issubset(extra_names['pass']), 'Supplementary race plan incomplete')
    supplementary = {'run': relative(extra_run), 'go_source_sha256': args.go_hash, 'top_level': extra_top, 'events': extra_events, 'counted_additively': False}
suffix = args.frontend_hash[:12]
zip_path, tar_path = candidate / f'frontend-{suffix}.zip', candidate / f'frontend-source-{suffix}.tar.gz'
evidence_dir = candidate / f'frozen-evidence-{suffix}'
reports = [candidate / 'frozen-verification.json', ROOT / 'docs' / (args.report_stem + '.json')]
for target in [zip_path, tar_path, evidence_dir, *reports]: require(not target.exists(), 'Refusing to overwrite: ' + relative(target))
with zipfile.ZipFile(zip_path, 'x', compression=zipfile.ZIP_DEFLATED) as archive:
    for name, digest in sorted(build['files'].items()):
        raw = child(candidate / 'dist', name).read_bytes(); require(sha(raw) == digest, 'Asset changed while packaging')
        entry = zipfile.ZipInfo(name, (2026, 9, 18, 0, 0, 0)); entry.compress_type = zipfile.ZIP_DEFLATED; archive.writestr(entry, raw)
with zipfile.ZipFile(zip_path) as archive:
    require(set(archive.namelist()) == set(build['files']), 'ZIP members differ')
    for name, digest in build['files'].items(): require(sha(archive.read(name)) == digest, 'ZIP byte mismatch')
with tarfile.open(tar_path, 'x:gz') as archive:
    for name, digest in sorted(build['source_files'].items()):
        raw = child(ROOT, name).read_bytes(); require(sha(raw) == digest, 'Source changed while packaging')
        entry = tarfile.TarInfo(name); entry.size = len(raw); entry.mode = 0o644; entry.mtime = 0; archive.addfile(entry, io.BytesIO(raw))
archive_members(tar_path, build['source_files'])
evidence_dir.mkdir()
evidence_names = {'build-manifest.json', 'logs-verification.json', 'accounts-no-billing-verification.json', 'frontend-unit-report.json', *logs['evidence'], *accounts['evidence']}
originals = [(child(candidate, name), name) for name in sorted(evidence_names)]
originals += [(run / name, 'go/' + name) for name in ['source-manifest.json', 'report.json', 'race.log', 'local-verification.json', 'local-test.jsonl']]
if supplementary: originals += [(extra_run / name, 'supplementary-go/' + name) for name in ['source-manifest.json', 'report.json', 'race.log']]
originals += [(ROOT / 'tools' / name, 'tools/' + name) for name in ['package_management_candidate.py', 'freeze_no_billing_candidate.py']]
evidence = []
for original, name in originals:
    target = child(evidence_dir, name); target.parent.mkdir(parents=True, exist_ok=True); raw = original.read_bytes()
    with target.open('xb') as output: output.write(raw)
    require(sha(target.read_bytes()) == sha(raw), 'Evidence copy mismatch'); evidence.append({'path': relative(target), 'sha256': sha(raw)})
for file in [zip_path, tar_path, run / 'source.tar.gz']: evidence.append({'path': relative(file), 'sha256': sha(file.read_bytes())})
report = {
    'schema': 'codex-management-candidate-v2', 'status': 'verified_management_candidate', 'completed_at': datetime.now(timezone.utc).isoformat(), 'iteration': candidate.name,
    'deployed': False, 'full_reference_parity': False, 'previous_candidates_unchanged': True, 'billing_enabled': False, 'retired_suites_counted_as_passed': False,
    'separate_owner_changes': ['The frozen Go snapshot includes independently owned native startup/transport/WebSocket changes, not certified here for their external runtime release.'],
    'go_source_sha256': args.go_hash, 'go_source_files': len(source['files']), 'source_archive': relative(run / 'source.tar.gz'), 'source_archive_sha256': source['archive_sha256'],
    'binaries': {name: {'path': relative(run / name), **value} for name, value in local['artifacts'].items()},
    'frontend': {'manifest_sha256': args.frontend_hash, 'frontend_sha256': build['frontend_sha256'], 'source_aggregate_sha256': aggregate(build['source_files']), 'entries': build['entries'], 'file_count': 54, 'source_file_count': len(build['source_files']), 'unchanged_baseline_resources': 48, 'archive': relative(zip_path), 'archive_sha256': sha(zip_path.read_bytes()), 'source_archive': relative(tar_path), 'source_archive_sha256': sha(tar_path.read_bytes())},
    'checks': {'management_linux_race': top, 'management_linux_race_events': events, 'full_go_test': full_top, 'full_go_test_events': full_events, 'vet': 'pass', 'windows_build': 'pass', 'linux_build': 'pass', 'frontend_node': counts, 'accounts_browser_groups': accounts['browser_groups'], 'logs_browser_groups': log_count, 'browser_network': 'isolated synthetic API', 'screenshots': {'accounts': sum(name.endswith('.png') for name in accounts['evidence']), 'logs': len(logs['screenshots'])}},
    'preview_evidence': 'Published separately by the local preview owner; no preview directories or credentials read by this packager.',
    'limits': ['Environment-dependent Go skips remain separate from passes.', 'Historical missing observations remain unknown.', 'No deployment, rebuild, service change, upstream request or user data deletion performed.'], 'evidence': evidence,
}
if supplementary: report['supplementary_joint_race'] = supplementary
for target in reports:
    with target.open('x', encoding='utf-8') as output: json.dump(report, output, ensure_ascii=False, indent=2); output.write('\n')
print(json.dumps({'status': report['status'], 'report': relative(reports[1]), 'frontend': report['frontend']}, ensure_ascii=False))
