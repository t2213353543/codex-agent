"""Offline release boundary audit. Never builds, deploys, or reads credentials."""
from __future__ import annotations

import argparse
from datetime import datetime, timezone
import hashlib
import json
from pathlib import Path, PurePosixPath
import tarfile

ROOT = Path(__file__).resolve().parents[1]


def digest(data):
    return hashlib.sha256(data).hexdigest()


def read(path):
    return json.loads(path.read_text(encoding='utf-8-sig'))


def workspace_path(name):
    path = Path(name)
    if not path.is_absolute():
        path = ROOT / path
    path = path.resolve()
    if not path.is_relative_to(ROOT.resolve()):
        raise ValueError('Evidence must be inside this workspace')
    return path


def archive_files(path, expected):
    """Inspect members without extracting or executing archived code."""
    seen = {}
    with tarfile.open(path, 'r:gz') as archive:
        for member in archive:
            name = PurePosixPath(member.name)
            if (not member.isfile() or name.is_absolute() or '..' in name.parts
                    or '\\' in member.name or ':' in member.name
                    or member.name in seen or member.size > 16 * 1024 * 1024):
                raise ValueError('Unsafe or duplicate source archive member')
            seen[member.name] = digest(archive.extractfile(member).read())
    if seen != expected:
        raise ValueError('Source archive bytes do not match the manifest')
    return seen


def delta(previous, current):
    return {
        'added': sorted(current.keys() - previous.keys()),
        'removed': sorted(previous.keys() - current.keys()),
        'modified': sorted(name for name in current.keys() & previous.keys() if current[name] != previous[name]),
    }


def assess(production, candidate, previous_files, current_files, build):
    source = delta(previous_files, current_files)
    frontend = delta(production['frontend'], build['files'])
    changed = sorted(set(source['added'] + source['removed'] + source['modified']))
    tests = [name for name in changed if name.endswith('_test.go') or 'tests' in PurePosixPath(name).parts]
    runtime = [name for name in changed if not Path(name).name.startswith('admin_') and Path(name).name != 'admin.go'
               and name not in tests and not name.startswith('migrations/')]
    migrations = {name: value for name, value in current_files.items() if name.startswith('migrations/')}
    blockers = []
    if runtime:
        blockers.append('Shared candidate changes non-management runtime files; management tests do not prove preserved gateway behavior.')
    if any(frontend.values()):
        blockers.append('The existing v2 publisher requires exact captured production frontend bytes; this new frontend needs a separately reviewed release contract.')
    if migrations != production['migration_checksums']:
        blockers.append('Candidate migration checksums differ from the last recorded production release.')
    blockers.append('No live production-state, candidate-specific writer, or rollback evidence was supplied to this offline audit.')
    return {
        'schema': 'codex-management-offline-release-boundary-v1',
        'audited_at': datetime.now(timezone.utc).isoformat(),
        'status': 'evidence_verified_release_not_ready',
        'publish_eligible': False, 'production_contacted': False, 'services_modified': False,
        'baseline_freshness': 'Last recorded production release, not a live server observation.',
        'recorded_release_id': production['release_id'],
        'recorded_binary_sha256': production['binary_sha256'],
        'candidate_source_sha256': candidate['go_source_sha256'],
        'candidate_linux_sha256': candidate['binaries']['codex-agent-linux-amd64']['sha256'],
        'candidate_frontend_sha256': build['frontend_sha256'],
        'candidate_tests': candidate['checks'],
        'source_delta': source, 'frontend_delta': frontend,
        'non_management_runtime_changes': runtime,
        'changed_test_files': tests,
        'migration_checksums_unchanged': migrations == production['migration_checksums'],
        'blockers': blockers,
        'not_inferred': ['Successful management tests are not gateway behavior equivalence.',
                         'An unchanged schema is not old-writer JSON/encrypted-document compatibility.',
                         'A local preview is not an online deployment.',
                         'No previous release evidence is relabeled for this candidate.'],
    }


def audit(production_path, baseline_path, candidate_path):
    production, baseline, candidate = map(read, [production_path, baseline_path, candidate_path])
    if (candidate.get('status') != 'verified_management_candidate'
            or production.get('binary_sha256') != baseline.get('linux_binary_sha256')):
        raise ValueError('Recorded production executable and frozen baseline are not bound')
    baseline_dir = workspace_path(baseline['source_directory']).parent
    previous = read(baseline_dir / 'source-manifest.json')
    current_archive = workspace_path(candidate['source_archive'])
    current = read(current_archive.parent / 'source-manifest.json')
    baseline_archive = workspace_path(baseline['source_archive'])
    for path, expected in [(baseline_archive, baseline['source_archive_sha256']),
                           (current_archive, candidate['source_archive_sha256'])]:
        if digest(path.read_bytes()) != expected:
            raise ValueError('Frozen source archive hash mismatch')
    if current['sha256'] != candidate['go_source_sha256']:
        raise ValueError('Candidate source manifests disagree')
    archive_files(baseline_archive, previous['files'])
    archive_files(current_archive, current['files'])
    # The production manifest uses a different aggregate serialization than the
    # Go-only harness, so compare its file map rather than mislabeling digests.
    production_source = read(production_path.parent / 'source-manifest.json')
    if production_source['files'] != previous['files']:
        raise ValueError('Recorded production source differs from frozen baseline')
    build_path = ROOT / '.local/management-parity' / candidate['iteration'] / 'build-manifest.json'
    build = read(build_path)
    if digest(build_path.read_bytes()) != candidate['frontend']['manifest_sha256']:
        raise ValueError('Candidate frontend manifest hash mismatch')
    for name, checksum in build['files'].items():
        member = PurePosixPath(name)
        if member.is_absolute() or '..' in member.parts or '\\' in name or ':' in name:
            raise ValueError('Unsafe frontend member')
        if digest((build_path.parent / 'dist' / name).read_bytes()) != checksum:
            raise ValueError('Candidate frontend file changed: ' + name)
    for value in candidate['binaries'].values():
        if digest(workspace_path(value['path']).read_bytes()) != value['sha256']:
            raise ValueError('Candidate executable changed')
    report = assess(production, candidate, previous['files'], current['files'], build)
    report['inputs'] = {str(path.relative_to(ROOT)): digest(path.read_bytes())
                        for path in [production_path, baseline_path, candidate_path, build_path]}
    return report


def record_report(path, report):
    if path.exists():
        previous = read(path)
        without_time = lambda value: {key: entry for key, entry in value.items() if key != 'audited_at'}
        if without_time(previous) != without_time(report):
            raise ValueError('Existing audit has different evidence; use a new output path')
        return
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open('x', encoding='utf-8') as output:
        json.dump(report, output, ensure_ascii=False, indent=2)
        output.write('\n')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--production', default='.local/upgrades/35624e0cafd9f60cae88f61d772464de2f0b891638c6a950f5f0a17668ebee70/upgrade-manifest.json')
    parser.add_argument('--baseline', default='.local/joint-candidate/20260918T024841.878837Z/joint-candidate.json')
    parser.add_argument('--candidate', default='docs/MANAGEMENT-ERROR-SEVERITY-VERIFICATION-2026-09-18.json')
    parser.add_argument('--output', default='.local/management-release-audit/release-boundary-final.json')
    args = parser.parse_args()
    report = audit(*[workspace_path(value) for value in [args.production, args.baseline, args.candidate]])
    path = workspace_path(args.output)
    record_report(path, report)
    print(json.dumps({'status': report['status'], 'publish_eligible': False,
                      'runtime_changed_files': len(report['non_management_runtime_changes']),
                      'report': str(path.relative_to(ROOT))}))


if __name__ == '__main__':
    main()
