"""Freeze, stage, and deploy only Codex Agent static UI against the existing PG service."""
from __future__ import annotations

from project_layout import backend_source_paths

import argparse
import datetime as dt
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import shlex
import shutil
import sys
import tarfile

from integration_env import connect, run

ROOT = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location('ui_release_remote', ROOT / 'deploy/ui_release_remote.py')
remote = importlib.util.module_from_spec(spec)
sys.modules[spec.name] = remote
spec.loader.exec_module(remote)
BASELINE = ROOT / '.local/fixes-20260924/release/package/full-release.json'
OUTPUT = ROOT / '.local/ui-releases'
EXCLUDED = {'node_modules', 'dist', 'test-results', 'artifacts', '__pycache__', '.git'}


def mapped(paths):
    files = {p.relative_to(ROOT).as_posix(): remote.sha(p) for p in sorted(set(paths)) if p.is_file()}
    return {'sha256': remote.object_sha(files), 'files': files}


def frontend_source():
    paths = []
    for directory in ('src', 'tests', 'scripts', 'public'):
        folder = ROOT / 'frontend' / directory
        if folder.exists():
            paths.extend(p for p in folder.rglob('*') if p.is_file() and not EXCLUDED.intersection(p.parts))
    paths.extend(p for p in (ROOT / 'frontend').iterdir() if p.is_file() and p.suffix.lower() in {'.json', '.js', '.mjs', '.cjs', '.ts', '.html'})
    return mapped(paths)


def backend_source():
    # A UI-only release may bind the immutable source of the running backend
    # while unrelated backend development continues in the shared checkout.
    folder = Path(os.environ.get('CODEX_UI_BACKEND_SOURCE_DIR', str(ROOT))).resolve()
    paths = backend_source_paths(folder)
    paths.extend((folder / 'migrations').rglob('*.sql'))
    files = {p.relative_to(folder).as_posix(): remote.sha(p) for p in sorted(set(paths)) if p.is_file()}
    return {'sha256': remote.object_sha(files), 'files': files}


def dist_map(folder=None):
    folder = folder or ROOT / 'frontend/dist'
    if not (folder / 'index.html').is_file():
        raise RuntimeError('Build the delivered frontend before preparing a UI release')
    files = {}
    for path in folder.rglob('*'):
        if path.is_symlink():
            raise RuntimeError('UI artifact contains symlink')
        if path.is_file():
            files[path.relative_to(folder).as_posix()] = remote.sha(path)
    return dict(sorted(files.items()))


def fingerprint():
    frontend, backend, dist = frontend_source(), backend_source(), dist_map()
    return {'frontend_source_sha256': frontend['sha256'], 'backend_source_sha256': backend['sha256'],
            'dist_sha256': remote.object_sha(dist), 'backend_binary_sha256': remote.BINARY_SHA}


def baseline_report(argument):
    manifest = json.loads(BASELINE.read_text(encoding='utf-8'))
    if manifest.get('binary_sha256') != remote.BINARY_SHA or manifest.get('release_id') != remote.BASELINE_ID:
        raise RuntimeError('Pinned deployed PostgreSQL release manifest is missing or changed')
    if manifest.get('schema') == 'codex-agent-full-release-v1':
        identity = {key: value for key, value in manifest.items() if key != 'release_id'}
        if remote.object_sha(identity) != remote.BASELINE_ID:
            raise RuntimeError('Pinned full release manifest identity mismatch')
        source_path = BASELINE.parent / 'evidence/source-manifest.json'
        expected_source = manifest.get('files', {}).get('evidence/source-manifest.json')
        expected_report = manifest.get('files', {}).get('evidence/acceptance.json')
        path = Path(argument).resolve() if argument else BASELINE.parent / 'evidence/acceptance.json'
        if not expected_source or not source_path.is_file() or remote.sha(source_path) != expected_source:
            raise RuntimeError('Baseline source manifest is absent or not bound to the pinned full release')
        if not expected_report or not path.is_file() or remote.sha(path) != expected_report:
            raise RuntimeError('Baseline acceptance report is absent or not bound to the pinned full release')
        report = json.loads(path.read_text(encoding='utf-8'))
        source = json.loads(source_path.read_text(encoding='utf-8'))
        if (report.get('schema') != 'codex-agent-full-release-acceptance-v1'
                or report.get('passed') is not True
                or report.get('binary_sha256') != remote.BINARY_SHA
                or report.get('source_sha256') != manifest.get('source_sha256')):
            raise RuntimeError('Pinned full release acceptance does not bind the passing backend')
        files = source.get('files')
        if not isinstance(files, dict) or not files:
            raise RuntimeError('Baseline source manifest has no file map')
        for name, digest in files.items():
            remote.relative(name)
            if not isinstance(digest, str) or not remote.SHA.fullmatch(digest):
                raise RuntimeError('Invalid hash in the baseline source manifest')
        source_hashes = {remote.object_sha(files), hashlib.sha256(json.dumps(
            files, sort_keys=True, separators=(',', ':'), ensure_ascii=False).encode()).hexdigest()}
        if source.get('sha256') != manifest.get('source_sha256') or source.get('sha256') not in source_hashes:
            raise RuntimeError('Baseline source digest does not bind its file map')
        compare_backend_source(files)
        return path, report
    expected = manifest['acceptance_report_sha256']
    candidates = [Path(argument)] if argument else list((ROOT / '.local/acceptance').rglob('report.json'))
    for path in candidates:
        if path.is_file() and remote.sha(path) == expected:
            report = json.loads(path.read_text(encoding='utf-8'))
            if report.get('source', {}).get('sha256') != manifest['source_sha256'] or report.get('release_ready') is not True:
                raise RuntimeError('Pinned backend acceptance report is not a passing release gate')
            original = {key: value for key, value in report['source']['files'].items()
                        if (key.endswith('.go') and '/' not in key) or key in ('go.mod', 'go.sum')
                        or (key.startswith('migrations/') and key.endswith('.sql'))}
            compare_backend_source(original)
            return path, report
    raise RuntimeError('Cannot find the hash-matching baseline backend acceptance report; provide --baseline-acceptance')


def compare_backend_source(original):
    current = backend_source()['files']
    if current != original:
        differences = {'added': sorted(current.keys() - original.keys()),
                       'removed': sorted(original.keys() - current.keys()),
                       'changed': sorted(key for key in current.keys() & original.keys()
                                         if current[key] != original[key])}
        raise RuntimeError('Go or SQL source changed from the running PostgreSQL release; '
                           'use a backend release workflow: ' + json.dumps(differences, sort_keys=True))


def read_acceptance(path, frontend, backend, dist):
    path = Path(path).resolve()
    report = json.loads(path.read_text(encoding='utf-8'))
    artifacts = {}
    for check in report.get('checks', []):
        for item in check.get('artifacts', []):
            key = item['path']
            artifact = Path(key)
            if not artifact.is_absolute():
                artifact = ROOT / artifact
            artifact = artifact.resolve(strict=True)
            try:
                artifact.relative_to(ROOT)
            except ValueError:
                raise RuntimeError('Acceptance artifacts must be inside this project') from None
            if not artifact.is_file() or remote.sha(artifact) != item['sha256']:
                raise RuntimeError('Acceptance artifact is absent or changed: ' + key)
            extension = artifact.suffix if artifact.suffix.lower() in ('.json', '.log', '.txt', '.png', '.webp', '.jpg', '.html') else '.bin'
            artifacts[key] = {'sha256': item['sha256'], 'packaged_path': 'evidence/artifacts/' + item['sha256'] + extension,
                              'local_path': artifact}
    safe_artifacts = {key: {k: v for k, v in entry.items() if k != 'local_path'} for key, entry in artifacts.items()}
    remote.evidence_valid(report, {'frontend_source': frontend, 'backend_source': backend,
                                  'frontend': dist, 'evidence_artifacts': safe_artifacts},
                          artifact_loader=lambda key: artifacts[key]['local_path'].read_text(encoding='utf-8'))
    return report, artifacts, safe_artifacts


def prepare(args):
    if not args.evidence:
        raise RuntimeError('prepare requires --evidence with fresh build/unit/browser/real_api checks')
    baseline, _ = baseline_report(args.baseline_acceptance)
    frontend, backend, dist = frontend_source(), backend_source(), dist_map()
    _, artifacts, safe_artifacts = read_acceptance(args.evidence, frontend, backend, dist)
    identity = {'schema': 'codex-agent-ui-release-v1', 'frontend_source': frontend,
                'backend_source': backend, 'frontend': dist,
                'backend_binary_path': remote.BINARY.as_posix(), 'backend_binary_sha256': remote.BINARY_SHA,
                'baseline_release_id': remote.BASELINE_ID, 'baseline_acceptance_sha256': remote.sha(baseline),
                'acceptance_sha256': remote.sha(args.evidence), 'evidence_artifacts': safe_artifacts,
                'remote_script_sha256': remote.sha(ROOT / 'deploy/ui_release_remote.py'),
                'tests_passed': True, 'database_migrated_by_ui_release': False}
    release_id = remote.object_sha(identity)
    folder = OUTPUT / release_id
    manifest = {**identity, 'release_id': release_id, 'prepared_at': dt.datetime.now(dt.timezone.utc).isoformat()}
    if folder.exists():
        existing = json.loads((folder / 'ui-manifest.json').read_text(encoding='utf-8'))
        if {k: v for k, v in existing.items() if k not in ('release_id', 'prepared_at')} != identity or dist_map(folder / 'frontend/dist') != dist:
            raise RuntimeError('Immutable local release already exists with different content')
        print(json.dumps({'release_id': release_id, 'manifest': str(folder / 'ui-manifest.json'), 'reused': True}))
        return
    staging = OUTPUT / ('.prepare-' + release_id)
    if staging.exists():
        raise RuntimeError('Interrupted local prepare exists; inspect it before retrying: ' + str(staging))
    (staging / 'frontend').mkdir(parents=True)
    shutil.copytree(ROOT / 'frontend/dist', staging / 'frontend/dist')
    (staging / 'deploy').mkdir()
    shutil.copyfile(ROOT / 'deploy/ui_release_remote.py', staging / 'deploy/ui_release_remote.py')
    (staging / 'evidence/artifacts').mkdir(parents=True)
    shutil.copyfile(args.evidence, staging / 'evidence/report.json')
    for entry in artifacts.values():
        shutil.copyfile(entry['local_path'], staging / entry['packaged_path'])
    if frontend_source() != frontend or backend_source() != backend or dist_map(staging / 'frontend/dist') != dist or dist_map() != dist:
        raise RuntimeError('Source or dist changed during UI freeze')
    for entry in safe_artifacts.values():
        if remote.sha(staging / entry['packaged_path']) != entry['sha256']:
            raise RuntimeError('Acceptance artifact changed during UI freeze')
    if remote.sha(staging / 'evidence/report.json') != identity['acceptance_sha256'] or remote.sha(staging / 'deploy/ui_release_remote.py') != identity['remote_script_sha256']:
        raise RuntimeError('Evidence or transaction helper changed during freeze')
    (staging / 'ui-manifest.json').write_text(json.dumps(manifest, indent=2, sort_keys=True) + '\n', encoding='utf-8')
    with tarfile.open(staging / 'ui-release.tar.gz', 'w:gz') as tar:
        for name in ('frontend', 'deploy', 'evidence', 'ui-manifest.json'):
            tar.add(staging / name, arcname=name)
    staging.rename(folder)
    print(json.dumps({'release_id': release_id, 'manifest': str(folder / 'ui-manifest.json'), 'tests_passed': True,
                      'backend_source_sha256': backend['sha256'], 'backend_matches_deployed_baseline': True,
                      'backend_binary_sha256': remote.BINARY_SHA,
                      'deployed': False, 'database_migrated_by_ui_release': False}, indent=2))


def load_release(args, require_current=False):
    if not args.release:
        raise RuntimeError('--release must point to a local ui-manifest.json')
    path = Path(args.release).resolve(strict=True)
    manifest = json.loads(path.read_text(encoding='utf-8'))
    identity = {k: v for k, v in manifest.items() if k not in ('release_id', 'prepared_at')}
    if remote.object_sha(identity) != manifest['release_id'] or path.parent.name != manifest['release_id']:
        raise RuntimeError('UI manifest identity mismatch')
    if dist_map(path.parent / 'frontend/dist') != manifest['frontend']:
        raise RuntimeError('Frozen dist differs from UI manifest')
    if manifest['backend_binary_sha256'] != remote.BINARY_SHA or manifest['backend_binary_path'] != remote.BINARY.as_posix():
        raise RuntimeError('Unexpected PostgreSQL backend identity')
    for entry in manifest['evidence_artifacts'].values():
        if remote.sha(path.parent / entry['packaged_path']) != entry['sha256']:
            raise RuntimeError('Frozen acceptance artifact changed')
    remote.evidence_valid(json.loads((path.parent / 'evidence/report.json').read_text(encoding='utf-8')), manifest,
                          artifact_loader=lambda key: (path.parent / manifest['evidence_artifacts'][key]['packaged_path']).read_text(encoding='utf-8'))
    if remote.sha(path.parent / 'evidence/report.json') != manifest['acceptance_sha256'] or remote.sha(path.parent / 'deploy/ui_release_remote.py') != manifest['remote_script_sha256']:
        raise RuntimeError('Frozen evidence/helper changed')
    if require_current:
        baseline_report(args.baseline_acceptance)
        if frontend_source() != manifest['frontend_source'] or backend_source() != manifest['backend_source'] or dist_map() != manifest['frontend']:
            raise RuntimeError('Current source or dist differs from accepted UI release')
    return path, manifest


def put(client, path, content):
    with client.open_sftp() as sftp:
        with sftp.file(path, 'w') as handle:
            handle.set_pipelined(True)
            handle.write(content)
        sftp.chmod(path, 0o600)


def stage(client, local, manifest, root, rehearsal):
    rid = manifest['release_id']
    incoming, destination = root + '/.ui-staging/' + rid, root + '/ui-releases/' + rid
    # Upload only under this project's staging area. No executable backend or DB data is included.
    script = '''import os,pathlib
root=pathlib.Path(%r)
assert root.is_dir() and root.resolve()==root
for p in (root/'.ui-staging',root/'.ui-staging'/%r):
    assert not p.is_symlink()
    p.mkdir(exist_ok=True,mode=0o700)
''' % (root, rid)
    run(client, 'python3 -c ' + shlex.quote(script))
    archive = local.parent / 'ui-release.tar.gz'
    parts = []
    with archive.open('rb') as handle:
        index = 0
        while block := handle.read(4 * 1024 * 1024):
            digest = hashlib.sha256(block).hexdigest()
            part = incoming + '/' + str(index) + '.' + digest + '.part'
            output = run(client, 'test -f ' + part + ' && sha256sum ' + part + ' || true').split()
            if not output or output[0] != digest:
                put(client, part, block)
                if run(client, 'sha256sum ' + part).split()[0] != digest:
                    raise RuntimeError('Uploaded chunk checksum mismatch')
            parts.append(part)
            index += 1
    run(client, 'cat ' + ' '.join(parts) + ' > ' + incoming + '/ui-release.tar.gz')
    if run(client, 'sha256sum ' + incoming + '/ui-release.tar.gz').split()[0] != remote.sha(archive):
        raise RuntimeError('Uploaded archive checksum mismatch')
    # Only regular files and directories inside the allowlisted archive layout are extracted.
    extraction = '''import pathlib,tarfile,shutil
base=pathlib.Path(%r)
assert base.resolve()==base and base.parent.name=='.ui-staging'
with tarfile.open(base/'ui-release.tar.gz','r:gz') as archive:
    for member in archive.getmembers():
        p=pathlib.PurePosixPath(member.name)
        assert not p.is_absolute() and '..' not in p.parts and p.parts[0] in ('frontend','deploy','evidence','ui-manifest.json')
        assert member.isfile() or member.isdir()
        assert not (base/member.name).is_symlink()
    # Keep verified upload chunks; replace only this candidate's extracted tree.
    for name in ('frontend','deploy','evidence','ui-manifest.json','compatibility.json'):
        target=base/name
        assert not target.is_symlink()
        if target.exists():
            assert target.resolve()==target and target.parent==base
            if target.is_dir(): shutil.rmtree(target)
            else: target.unlink()
    archive.extractall(base)
''' % incoming
    run(client, 'python3 -c ' + shlex.quote(extraction))
    staged_script = incoming + '/deploy/ui_release_remote.py'
    if run(client, 'sha256sum ' + staged_script).split()[0] != manifest['remote_script_sha256']:
        raise RuntimeError('Uploaded transaction helper checksum mismatch')
    command = 'python3 ' + staged_script + ' stage --incoming ' + incoming + ' --release ' + destination
    if rehearsal:
        command += ' --rehearsal'
    print(run(client, command, timeout=240))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action', choices=['fingerprint', 'prepare', 'stage', 'activate', 'rollback', 'status', 'self-test'])
    parser.add_argument('--release', help='Frozen local ui-manifest.json')
    parser.add_argument('--evidence', help='Fresh UI acceptance evidence JSON')
    parser.add_argument('--baseline-acceptance', help='Hash-matching acceptance report for the deployed backend')
    parser.add_argument('--rehearsal', action='store_true')
    parser.add_argument('--fault', choices=['startup'])
    args = parser.parse_args()
    if args.action == 'self-test':
        print(json.dumps(remote.self_test(), indent=2))
        return
    if args.action == 'fingerprint':
        print(json.dumps(fingerprint(), indent=2))
        return
    if args.action == 'prepare':
        prepare(args)
        return
    local, manifest = load_release(args, require_current=args.action in ('stage', 'activate'))
    root = '/opt/codex-agent-integration/ui-rehearsal' if args.rehearsal else '/opt/codex-agent'
    destination = root + '/ui-releases/' + manifest['release_id']
    client = connect()
    try:
        if args.action == 'stage':
            stage(client, local, manifest, root, args.rehearsal)
        else:
            command = 'python3 ' + destination + '/deploy/ui_release_remote.py ' + args.action + ' --release ' + destination
            if args.rehearsal:
                command += ' --rehearsal'
            if args.fault:
                if not args.rehearsal or args.action != 'activate':
                    raise RuntimeError('--fault is allowed only for synthetic rehearsal activation')
                command += ' --fault ' + args.fault
            print(run(client, command, timeout=600))
    finally:
        client.close()


if __name__ == '__main__':
    try:
        main()
    except (RuntimeError, remote.UIReleaseError) as error:
        raise SystemExit(str(error)) from None
