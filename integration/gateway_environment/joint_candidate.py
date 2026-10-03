"""Verify and adapt the immutable gateway overlay without using the shared Go tree.

Produces review evidence, not a publishable acceptance or deployment. Full local
Go/vet commands run only against a newly copied frozen source directory.
"""
from __future__ import annotations

import argparse
from collections import Counter
from datetime import datetime, timezone
import fnmatch
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import subprocess
import tarfile

ROOT = Path(__file__).resolve().parents[2]
DEFAULT_OWNER = '.local/source-20260918/gateway-overlay-20260918t023414-8e3565'
DEFAULT_RACE = '.local/source-20260918/local-linux-race-20260918t023552-c45439'
DEFAULT_BASE = '.local/management-parity/linux-race-20260917T214152-2d29dc'
FIX_HASH = '234a6bebee0550a68d053f1efa0da145805fb47fff210769fe5f46d54e42e6ba'
PATTERNS = ('gateway*.go', 'proxy.go', 'response*.go', 'source_native_helper_test.go')
REQUIRED_REGRESSIONS = {'TestGatewayTimeoutPoolDefaultsAndCustomRoundTripper',
    'TestGatewayTimeoutPoolCompactTotalTimeoutLeavesHTTPBodyAlive', 'TestGatewayTimeoutPoolDisabledOrDeletedGuardsAllVariants'}


def require(condition, message):
    if not condition:
        raise ValueError(message)


def sha(raw):
    return hashlib.sha256(raw).hexdigest()


def owner_digest(files):
    return sha(json.dumps(files, sort_keys=True, separators=(',', ':')).encode())


def summary(raw):
    outcomes, totals, packages = {}, Counter(), {}
    for line in raw.splitlines():
        if not line.startswith('{'):
            continue
        event = json.loads(line)
        action = event.get('Action')
        if action in ('pass', 'fail', 'skip'):
            if event.get('Test'):
                outcomes[event['Test']] = action
                totals[action] += 1
            else:
                packages[event.get('Package', '')] = action
    return {'top_level': {s: sum(v == s and '/' not in k for k, v in outcomes.items()) for s in ('pass', 'fail', 'skip')},
        'events': {s: totals[s] for s in ('pass', 'fail', 'skip')}, 'outcomes': outcomes,
        'packages': packages, 'data_race': 'WARNING: DATA RACE' in raw}


def delta(base, target, overlay):
    expected = {**base, **overlay}
    require(target == expected, 'Joint source must be exactly management baseline plus declared overlay')
    require(all(any(fnmatch.fnmatchcase(name, pattern) for pattern in PATTERNS) for name in overlay),
        'Overlay changed files outside gateway ownership')
    return {'added': sorted(set(target) - set(base)),
        'modified': sorted(name for name in base if target[name] != base[name]), 'removed': [],
        'inherited_files': sorted(name for name in base if target[name] == base[name]),
        'management_and_stats_preserved': all(target[name] == value for name, value in base.items()
            if not any(fnmatch.fnmatchcase(name, pattern) for pattern in PATTERNS))}


class Review:
    def __init__(self, root):
        self.root = root
        self.evidence = {}

    def read(self, path, expected=None):
        path = Path(path).resolve()
        require(path.is_relative_to(self.root) and not path.is_symlink(), 'Evidence outside project or symlink')
        raw = path.read_bytes()
        actual = sha(raw)
        require(expected is None or actual == expected, 'Evidence hash mismatch: ' + path.name)
        self.evidence[path.relative_to(self.root).as_posix()] = {'path': path.relative_to(self.root).as_posix(),
            'sha256': actual, 'bytes': len(raw), 'expected_sha256': expected}
        return raw

    def document(self, path, expected=None):
        return json.loads(self.read(path, expected))

    def archive(self, path, files, expected):
        self.read(path, expected)
        contents = {}
        with tarfile.open(path) as archive:
            for member in archive:
                name = PurePosixPath(member.name)
                require(member.isfile() and not name.is_absolute() and '..' not in name.parts
                    and '\\' not in member.name and member.name not in contents and member.name in files,
                    'Archive contains unexpected/non-regular/duplicate source member')
                require(member.size <= 16 * 1024 * 1024, 'Source member exceeds size bound')
                raw = archive.extractfile(member).read()
                require(sha(raw) == files[member.name], 'Source member hash differs')
                contents[member.name] = raw
        require(set(contents) == set(files), 'Archive source file set differs')
        return contents


def run_local(source, output, files):
    names = ('SystemRoot', 'WINDIR', 'TEMP', 'TMP', 'PATH', 'PATHEXT', 'USERPROFILE', 'LOCALAPPDATA', 'APPDATA',
        'HOMEDRIVE', 'HOMEPATH', 'ProgramFiles', 'ProgramFiles(x86)', 'ProgramData', 'COMSPEC')
    env = {name: os.environ[name] for name in names if name in os.environ}
    # Resolve only local cache/toolchain paths before disabling persistent GOENV.
    caches = json.loads(subprocess.check_output(['go', 'env', '-json', 'GOPATH', 'GOMODCACHE', 'GOCACHE', 'GOROOT'], text=True))
    env.update(caches)
    env.update(CGO_ENABLED='0', GOPROXY='off', GOSUMDB='off', GOWORK='off', GOENV='off',
        GOFLAGS='-mod=readonly', HTTP_PROXY='', HTTPS_PROXY='', ALL_PROXY='', NO_PROXY='*')
    checks = []
    for label, command in (('test', ['go', 'test', '-count=1', '-json', '-timeout=4m', './...']),
                           ('vet', ['go', 'vet', './...'])):
        result = subprocess.run(command, cwd=source, env=env, capture_output=True, timeout=300)
        (output / ('local-' + label + '.log')).write_bytes(result.stdout)
        (output / ('local-' + label + '-stderr.log')).write_bytes(result.stderr)
        check = {'label': label, 'command': command, 'exit_code': result.returncode,
            'stdout': 'local-' + label + '.log', 'stdout_sha256': sha(result.stdout),
            'stderr': 'local-' + label + '-stderr.log', 'stderr_sha256': sha(result.stderr)}
        if label == 'test':
            check.update(summary(result.stdout.decode('utf-8', 'replace')))
        checks.append(check)
    unchanged = all(sha((source / name).read_bytes()) == expected for name, expected in files.items())
    require(unchanged, 'Frozen source changed during local verification')
    report = {'schema': 'codex-agent-joint-local-verification-v1', 'go_source_sha256': owner_digest(files),
        'source_unchanged': True, 'checks': checks, 'local_commands_passed': all(c['exit_code'] == 0 for c in checks),
        'complete_acceptance': all(c['exit_code'] == 0 for c in checks) and not checks[0]['events']['skip'],
        'environment_policy': 'OS allowlist only; no inherited TEST/CODEX/PG/Redis/credentials/proxy opt-ins; offline Go',
        'production_requests': 0, 'race_executed': False}
    (output / 'local-verification.json').write_text(json.dumps(report, indent=2) + '\n', encoding='utf-8')
    return report


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--owner', default=DEFAULT_OWNER)
    parser.add_argument('--race', default=DEFAULT_RACE)
    parser.add_argument('--base', default=DEFAULT_BASE)
    parser.add_argument('--management-race', help='Optional same-candidate independent management race report')
    parser.add_argument('--skip-local', action='store_true', help='Inspect/copy only, explicitly leaving local Go/vet unexecuted')
    args = parser.parse_args()
    owner, race, base = ((ROOT / value).resolve() for value in (args.owner, args.race, args.base))
    review = Review(ROOT)
    source = review.document(owner / 'source-manifest.json')
    overlay = review.document(owner / 'overlay-manifest.json')
    baseline = review.document(base / 'source-manifest.json')
    require(owner_digest(source['files']) == source['sha256'] == overlay['go_source_sha256'], 'Overlay source digest mismatch')
    require(owner_digest(baseline['files']) == baseline['sha256'] == overlay['base_go_sha256'], 'Baseline source digest mismatch')
    review.read(base / 'report.json', overlay['base_report_sha256'])
    review.archive(base / 'source.tar.gz', baseline['files'], overlay['base_archive_sha256'])
    require(owner_digest(overlay['overlay_files']) == overlay['overlay_sha256'], 'Overlay map digest mismatch')
    require(tuple(overlay['overlay_patterns']) == PATTERNS, 'Unreviewed overlay ownership patterns')
    changes = delta(baseline['files'], source['files'], overlay['overlay_files'])
    require(source['files']['gateway_client_pool.go'] == FIX_HASH, 'Reviewed compact timeout fix absent')
    from project_layout import backend_source_paths
    on_disk = {p.relative_to(owner / 'source').as_posix(): sha(p.read_bytes()) for p in backend_source_paths(owner / 'source')}
    require(on_disk == source['files'], 'Owner source directory differs from immutable manifest')
    race_report = review.document(race / 'report.json')
    for artifact in race_report['artifacts']:
        review.read(ROOT / artifact['path'].replace('\\', '/'), artifact['sha256'])
    race_source = review.document(race / 'source-manifest.json')
    require(race_source['files'] == source['files'] and race_report['go_source_sha256'] == source['sha256'], 'Race source is not the candidate')
    contents = review.archive(race / 'source.tar.gz', source['files'], race_source['archive_sha256'])
    raw_race = review.read(race / 'race.log').decode()
    race_summary = summary(raw_race)
    selected = set(race_report['selected_top_level_tests'])
    require(selected == {name for name in race_summary['outcomes'] if '/' not in name}, 'Race selected test set differs from raw log')
    require(race_report['status'] == 'pass' and race_report['exit_code'] == 0 and race_summary['events']['fail'] == 0
        and race_summary['events']['skip'] == 0 and not race_summary['data_race']
        and race_summary['top_level']['pass'] == race_report['passed_top_level_tests']
        and race_summary['events']['pass'] == race_report['passed_test_events'], 'Race report/log terminal evidence mismatch')
    require('linux/amd64' in raw_race and '\n1\ngcc\n' in raw_race.replace('\r', ''), 'Race Linux/CGO toolchain evidence absent')
    require(race_report['dependencies_ready'] is True and race_report['postgres_non_superuser_verified'] is True
        and race_report['redis_acl_boundary_verified'] is True, 'Actual isolated PG/Redis boundary not verified')
    hooks = {name for name in selected if name.startswith('TestGatewayObservationHooks')}
    require(len(hooks) == 9 and 'TestGatewayObservationHooksHTTPProvenanceAndBytes' in hooks
        and REQUIRED_REGRESSIONS <= selected, 'Observation bytes/hooks or fixed timeout regression tests missing')
    build = review.document(owner / 'build-verification.json')
    require(build['go_source_sha256'] == source['sha256'] and build['builds_passed'] is True
        and build['source_unchanged_after_build'] is True, 'Binary build/source evidence mismatch')
    binary = None
    for artifact in build['artifacts']:
        raw = review.read(ROOT / artifact['path'], artifact['sha256'])
        require(len(raw) == artifact['bytes'], 'Built binary size mismatch')
        if Path(artifact['path']).name == 'codex-agent-linux-amd64':
            require(raw[:6] == b'\x7fELF\x02\x01' and raw[18:20] == b'\x3e\x00', 'Expected Linux amd64 ELF')
            binary = raw
    require(binary is not None, 'Linux candidate missing')
    stamp = datetime.now(timezone.utc).strftime('%Y%m%dT%H%M%S.%fZ')
    output = ROOT / '.local/joint-candidate' / stamp
    local_source = output / 'local-source'
    local_source.mkdir(parents=True, exist_ok=False)
    for name, raw in contents.items():
        destination = local_source / name
        destination.parent.mkdir(parents=True, exist_ok=True)
        destination.write_bytes(raw)
    (output / 'source.tar.gz').write_bytes((race / 'source.tar.gz').read_bytes())
    (output / 'source-manifest.json').write_text(json.dumps(race_source, indent=2) + '\n', encoding='utf-8')
    (output / 'codex-agent-linux-amd64').write_bytes(binary)
    local = {'local_commands_passed': False, 'status': 'unexecuted'} if args.skip_local else run_local(local_source, output, source['files'])
    management = {'status': 'blocked', 'inherited_evidence_source': baseline['sha256'],
        'reason': 'Unchanged management files do not prove management transactions on the new runtime'}
    if args.management_race:
        management_path = (ROOT / args.management_race).resolve()
        management_report = review.document(management_path)
        for artifact in management_report['artifacts']:
            review.read(ROOT / artifact['path'].replace('\\', '/'), artifact['sha256'])
        management_log = review.read(management_path.parent / 'race.log').decode()
        management_summary = summary(management_log)
        original_management = review.document(base / 'report.json', overlay['base_report_sha256'])
        require(management_report['go_source_sha256'] == source['sha256'] and management_report['status'] == 'pass'
            and management_report['exit_code'] == 0 and not management_summary['events']['skip']
            and not management_summary['events']['fail'] and not management_summary['data_race'], 'Joint management race did not pass exact candidate')
        require(set(management_report['selected_top_level_tests']) == set(original_management['selected_top_level_tests'])
            == {name for name in management_summary['outcomes'] if '/' not in name}, 'Management selection differs from reviewed baseline suite')
        require(management_report['dependencies_ready'] is True and management_report['postgres_non_superuser_verified'] is True
            and management_report['redis_acl_boundary_verified'] is True and 'linux/amd64' in management_log,
            'Management real Linux PG/Redis boundary unverified')
        management = {'status': 'pass', 'source_sha256': source['sha256'], 'top_level': management_summary['top_level'],
            'events': management_summary['events'], 'report': args.management_race, 'report_sha256': sha(management_path.read_bytes()),
            'evidence_scope': 'Independent same-candidate Linux PG/Redis race, not inherited baseline PASS'}
    baseline_candidate = review.document(ROOT / 'docs/MANAGEMENT-PARITY-FOLLOWUP-2026-09-18.json')
    record = {'schema': 'codex-agent-joint-candidate-v1', 'status': 'verified_joint_source_candidate',
        'publish_eligible': False, 'deployed': False, 'migrated': False,
        'go_source_sha256': source['sha256'],
        'v2_source_sha256': sha(json.dumps(source['files'], sort_keys=True).encode()),
        'source_directory': str(local_source.relative_to(ROOT)).replace('\\', '/'),
        'source_archive': str((output / 'source.tar.gz').relative_to(ROOT)).replace('\\', '/'),
        'source_archive_sha256': race_source['archive_sha256'],
        'linux_binary': str((output / 'codex-agent-linux-amd64').relative_to(ROOT)).replace('\\', '/'),
        'linux_binary_sha256': sha(binary), 'base_go_sha256': baseline['sha256'],
        'overlay_delta': changes, 'overlay_sha256': overlay['overlay_sha256'],
        'known_defect_status': {'issue': 'custom-direct-compact-explicit-timeout', 'old_binary_still_defective': True,
            'candidate_contains_reviewed_fix': True, 'fix_file_sha256': FIX_HASH, 'same_source_regressions_passed': True,
            'regression_tests': sorted(REQUIRED_REGRESSIONS)},
        'observation_hooks': {'tests': sorted(hooks), 'passed_on_candidate_linux_race': True,
            'request_response_bytes_test': 'TestGatewayObservationHooksHTTPProvenanceAndBytes'},
        'checks': {'gateway_linux_pg_redis_race': {'source_sha256': source['sha256'],
            'top_level': race_summary['top_level'], 'events': race_summary['events'], 'status': 'pass'},
            'full_local_go_vet': local,
            'management_race': management},
        'frontend': {**baseline_candidate['frontend'], 'included_in_release': False, 'evidence_scope': 'baseline management delivery only'},
        'release_frontend_policy': 'Preserve captured production 47 files; management 54-file artifact is reference only',
        'evidence': list(review.evidence.values()), 'completed_at': datetime.now(timezone.utc).isoformat()}
    for name in ('source-manifest.json', 'source.tar.gz', 'codex-agent-linux-amd64', 'local-verification.json',
                 'local-test.log', 'local-test-stderr.log', 'local-vet.log', 'local-vet-stderr.log'):
        if (output / name).is_file():
            record['evidence'].append({'path': str((output / name).relative_to(ROOT)).replace('\\', '/'),
                'sha256': sha((output / name).read_bytes())})
    role_paths = {'source_manifest': output / 'source-manifest.json', 'gateway_race': race / 'report.json',
        'overlay_manifest': owner / 'overlay-manifest.json', 'base_candidate': ROOT / 'docs/MANAGEMENT-PARITY-FOLLOWUP-2026-09-18.json'}
    if not args.skip_local:
        role_paths['local_verification'] = output / 'local-verification.json'
    if args.management_race:
        role_paths['management_race'] = (ROOT / args.management_race).resolve()
    record['evidence_roles'] = {role: {'path': path.relative_to(ROOT).as_posix(), 'sha256': sha(path.read_bytes())}
        for role, path in role_paths.items()}
    if not local.get('local_commands_passed'):
        record['status'] = 'blocked'
    report = output / 'joint-candidate.json'
    report.write_text(json.dumps(record, indent=2) + '\n', encoding='utf-8')
    print(json.dumps({'status': record['status'], 'publish_eligible': False, 'report': str(report),
        'source_sha256': source['sha256'], 'source_delta': {k: len(v) for k, v in changes.items() if isinstance(v, list)}}))
    return 0 if local.get('local_commands_passed') else 2


if __name__ == '__main__':
    raise SystemExit(main())
