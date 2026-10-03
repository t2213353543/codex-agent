"""Package a v1 schema-preserving or explicit reviewed v2 additive PG upgrade."""

from project_layout import backend_source_paths, migration_runner_key
import argparse
from datetime import datetime, timezone
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import shlex
import subprocess
import tarfile
import uuid

from acceptance import ROOT, sha, source_manifest
from deploy import put
from integration_env import connect, run

spec = importlib.util.spec_from_file_location('upgrade_remote', ROOT / 'deploy/upgrade_remote.py')
remote = importlib.util.module_from_spec(spec)
spec.loader.exec_module(remote)


def prepare(report_path):
    report_path = Path(report_path).resolve()
    snapshot = source_manifest()
    remote.validate_acceptance(json.loads(report_path.read_text()), snapshot['sha256'])
    output = ROOT / '.local/upgrades'
    output.mkdir(parents=True, exist_ok=True)
    binary = output / 'candidate-linux-amd64'
    subprocess.run(['go', 'build', '-trimpath', '-ldflags=-s -w', '-o', str(binary), '.'],
                   cwd=ROOT, env={**os.environ, 'GOOS': 'linux', 'GOARCH': 'amd64', 'CGO_ENABLED': '0'}, check=True)
    frontend = {p.relative_to(ROOT / 'frontend/dist').as_posix(): sha(p)
                for p in sorted((ROOT / 'frontend/dist').rglob('*')) if p.is_file()}
    if 'index.html' not in frontend or source_manifest() != snapshot:
        raise RuntimeError('Frontend missing or source changed during packaging')
    migrations = {name: sha(ROOT / name) for name in remote.MIGRATIONS}
    if migrations != remote.MIGRATIONS:
        raise RuntimeError('Schema-preserving upgrade cannot change migrations')
    identity = {'source': snapshot['sha256'], 'binary': sha(binary), 'frontend': frontend,
                'acceptance': sha(report_path), 'script': sha(ROOT / 'deploy/upgrade_remote.py')}
    rid = hashlib.sha256(json.dumps(identity, sort_keys=True).encode()).hexdigest()
    folder = output / rid
    folder.mkdir(exist_ok=True)
    manifest = {'schema': 'codex-agent-pg-upgrade-v1', 'release_id': rid,
                'source_sha256': snapshot['sha256'], 'binary_sha256': identity['binary'],
                'frontend': frontend, 'migration_checksums': migrations, 'tests_passed': True,
                'acceptance_report_sha256': identity['acceptance'], 'remote_script_sha256': identity['script']}
    path = folder / 'upgrade-manifest.json'
    if path.exists() and json.loads(path.read_text()) != manifest:
        raise RuntimeError('Immutable release identity collision')
    path.write_text(json.dumps(manifest, indent=2), encoding='utf-8')
    (folder / 'source-manifest.json').write_text(json.dumps(snapshot, indent=2), encoding='utf-8')
    def public(info):
        info.uid = info.gid = 0
        info.uname = info.gname = ''
        info.mode = 0o755 if info.isdir() or info.name == 'codex-agent' else 0o644
        return info
    with tarfile.open(folder / 'release.tar.gz', 'w:gz') as tar:
        tar.add(binary, arcname='codex-agent', filter=public)
        tar.add(ROOT / 'frontend/dist', arcname='frontend/dist', filter=public)
        tar.add(path, arcname='upgrade-manifest.json', filter=public)
        tar.add(report_path, arcname='evidence/upgrade-acceptance.json', filter=public)
        for name in ('deploy/upgrade_remote.py', 'docs/API-CONTRACT.md'):
            tar.add(ROOT / name, arcname=name, filter=public)
    print(path)


def backend_manifest(folder):
    folder = Path(folder).resolve()
    paths = backend_source_paths(folder)
    files = {path.relative_to(folder).as_posix(): sha(remote.regular(path)) for path in sorted(paths)}
    if not {'go.mod', 'go.sum'} <= files.keys() or not migration_runner_key(files):
        raise RuntimeError('Frozen Go source directory is incomplete')
    return {'sha256': hashlib.sha256(json.dumps(files, sort_keys=True).encode()).hexdigest(), 'files': files}


def prepare_v2(*, acceptance, source_dir, candidate_binary, frontend_dir, frontend_evidence,
               migration_evidence, rollback_evidence):
    source_dir, candidate_binary, frontend_dir = (Path(p).resolve() for p in (source_dir, candidate_binary, frontend_dir))
    if source_dir == ROOT.resolve() or frontend_dir == (ROOT / 'frontend/dist').resolve():
        raise RuntimeError('V2 requires explicit frozen source and captured production frontend, not the shared tree')
    remote.directory(source_dir)
    snapshot = backend_manifest(source_dir)
    candidate_binary = remote.regular(candidate_binary)
    binary_hash = sha(candidate_binary)
    with candidate_binary.open('rb') as handle:
        header = handle.read(20)
    if len(header) != 20 or header[:6] != b'\x7fELF\x02\x01' or header[18:20] != b'\x3e\x00':
        raise RuntimeError('Candidate must be an explicitly built Linux amd64 ELF executable')
    frontend = remote.file_map(frontend_dir)
    if 'index.html' not in frontend:
        raise RuntimeError('Captured production frontend has no index.html')
    evidence_paths = {'acceptance_report_sha256': Path(acceptance).resolve(),
        'frontend_evidence_sha256': Path(frontend_evidence).resolve(),
        'migration_evidence_sha256': Path(migration_evidence).resolve(),
        'rollback_evidence_sha256': Path(rollback_evidence).resolve()}
    evidence_bytes = {key: remote.regular(path).read_bytes() for key, path in evidence_paths.items()}
    source_bytes = (json.dumps(snapshot, indent=2, sort_keys=True) + '\n').encode()
    manifest = {'schema': remote.V2_SCHEMA, 'source_sha256': snapshot['sha256'], 'binary_sha256': binary_hash,
        'frontend': frontend, 'migration_checksums': remote.TARGET_MIGRATIONS, 'tests_passed': True,
        'source_manifest_sha256': hashlib.sha256(source_bytes).hexdigest(),
        'remote_script_sha256': sha(ROOT / 'deploy/upgrade_remote.py'),
        **{key: hashlib.sha256(raw).hexdigest() for key, raw in evidence_bytes.items()}}
    remote.validate_v2_evidence(manifest, json.loads(evidence_bytes['acceptance_report_sha256']),
        json.loads(evidence_bytes['migration_evidence_sha256']), json.loads(evidence_bytes['rollback_evidence_sha256']),
        json.loads(evidence_bytes['frontend_evidence_sha256']), snapshot)
    rid = hashlib.sha256(json.dumps(manifest, sort_keys=True).encode()).hexdigest()
    manifest['release_id'] = rid
    output = ROOT / '.local/upgrades'
    output.mkdir(parents=True, exist_ok=True)
    folder = output / rid
    if folder.exists():
        if json.loads((folder / 'upgrade-manifest.json').read_text()) != manifest or not (folder / 'release.tar.gz').is_file():
            raise RuntimeError('Immutable v2 release directory differs or is incomplete')
        print(folder / 'upgrade-manifest.json')
        return folder / 'upgrade-manifest.json'
    temporary = output / ('.prepare-v2-' + uuid.uuid4().hex)
    temporary.mkdir()
    (temporary / 'source-manifest.json').write_bytes(source_bytes)
    manifest_path = temporary / 'upgrade-manifest.json'
    manifest_path.write_text(json.dumps(manifest, indent=2, sort_keys=True), encoding='utf-8')
    def public(info):
        info.uid = info.gid = 0
        info.uname = info.gname = ''
        info.mode = 0o755 if info.isdir() or info.name == 'codex-agent' else 0o644
        return info
    with tarfile.open(temporary / 'release.tar.gz', 'w:gz') as tar:
        tar.add(candidate_binary, arcname='codex-agent', filter=public)
        tar.add(frontend_dir, arcname='frontend/dist', filter=public)
        tar.add(manifest_path, arcname='upgrade-manifest.json', filter=public)
        tar.add(temporary / 'source-manifest.json', arcname='source-manifest.json', filter=public)
        for key, raw in evidence_bytes.items():
            name = 'evidence/upgrade-acceptance.json' if key == 'acceptance_report_sha256' else remote.V2_EVIDENCE[key]
            saved = temporary / name
            saved.parent.mkdir(parents=True, exist_ok=True)
            saved.write_bytes(raw)
            tar.add(saved, arcname=name, filter=public)
        for name in remote.TARGET_MIGRATIONS:
            tar.add(source_dir / name, arcname=name, filter=public)
        tar.add(ROOT / 'deploy/upgrade_remote.py', arcname='deploy/upgrade_remote.py', filter=public)
    if (backend_manifest(source_dir) != snapshot or sha(candidate_binary) != binary_hash
            or remote.file_map(frontend_dir) != frontend or sha(ROOT / 'deploy/upgrade_remote.py') != manifest['remote_script_sha256']):
        raise RuntimeError('Frozen input changed during v2 packaging; candidate was not published')
    temporary.rename(folder)
    print(folder / 'upgrade-manifest.json')
    return folder / 'upgrade-manifest.json'


def stage(client, path, manifest, destination):
    expected = {'codex-agent': manifest['binary_sha256'], 'upgrade-manifest.json': sha(path),
                'deploy/upgrade_remote.py': manifest['remote_script_sha256'],
                'evidence/upgrade-acceptance.json': manifest['acceptance_report_sha256']}
    expected.update({'frontend/dist/' + k: v for k, v in manifest['frontend'].items()})
    if manifest.get('schema') == remote.V2_SCHEMA:
        expected.update({name: manifest[key] for key, name in remote.V2_EVIDENCE.items()})
        expected.update(remote.TARGET_MIGRATIONS)
    def verify(target):
        script = ('import hashlib,json,pathlib; root=pathlib.Path(' + repr(target) + '); expected=json.loads('
                  + repr(json.dumps(expected)) + '); assert all(hashlib.sha256((root/n).read_bytes()).hexdigest()==v for n,v in expected.items()); print("artifact hashes verified")')
        run(client, 'python3 -c ' + shlex.quote(script))
    if run(client, 'test -d ' + destination + ' && echo exists || true').strip():
        verify(destination)
        print('Identical candidate already staged')
        return
    staging = '/opt/codex-agent/releases/.stage-upgrade-' + manifest['release_id']
    run(client, 'test -s /etc/codex-agent/production.env\ntest ! -L ' + staging + '\ninstall -d -m 755 ' + staging)
    archive = path.parent / 'release.tar.gz'
    chunks = []
    with archive.open('rb') as handle:
        while block := handle.read(4 * 1024 * 1024):
            digest = hashlib.sha256(block).hexdigest()
            part = staging + '/' + str(len(chunks)) + '.' + digest + '.part'
            existing = run(client, 'test -f ' + part + ' && sha256sum ' + part + ' || true').split()
            if not existing or existing[0] != digest:
                put(client, part, block)
            chunks.append(part)
    run(client, 'cat ' + ' '.join(chunks) + ' > ' + staging + '/release.tar.gz')
    if run(client, 'sha256sum ' + staging + '/release.tar.gz').split()[0] != sha(archive):
        raise RuntimeError('Archive checksum mismatch')
    run(client, 'tar -xzf ' + staging + '/release.tar.gz -C ' + staging)
    verify(staging)
    run(client, 'flock /opt/codex-agent/.deployment.lock sh -ec ' + shlex.quote('test ! -e ' + destination + '; mv ' + staging + ' ' + destination))
    print('Candidate staged: ' + destination)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action', choices=('prepare', 'stage', 'preflight', 'activate', 'status', 'rollback'))
    parser.add_argument('--acceptance', type=Path)
    parser.add_argument('--release', type=Path)
    parser.add_argument('--schema', choices=('v1', 'v2'), default='v1', help='v2 is an explicit additive 003 migration release')
    parser.add_argument('--source-dir', type=Path)
    parser.add_argument('--candidate-binary', type=Path)
    parser.add_argument('--frontend-dir', type=Path)
    parser.add_argument('--frontend-evidence', type=Path)
    parser.add_argument('--migration-evidence', type=Path)
    parser.add_argument('--rollback-evidence', type=Path)
    args = parser.parse_args()
    if args.action == 'prepare':
        if not args.acceptance:
            parser.error('prepare requires --acceptance')
        if args.schema == 'v2':
            fields = ('source_dir', 'candidate_binary', 'frontend_dir', 'frontend_evidence', 'migration_evidence', 'rollback_evidence')
            if any(getattr(args, name) is None for name in fields):
                parser.error('v2 prepare requires frozen source, candidate binary, captured frontend and all three evidence files')
            prepare_v2(acceptance=args.acceptance, **{name: getattr(args, name) for name in fields})
        else:
            if any(getattr(args, name) is not None for name in ('source_dir', 'candidate_binary', 'frontend_dir',
                   'frontend_evidence', 'migration_evidence', 'rollback_evidence')):
                parser.error('Frozen-input and migration options require explicit --schema v2')
            prepare(args.acceptance)
        return
    if not args.release:
        parser.error('--release is required')
    path = args.release.resolve()
    manifest = json.loads(path.read_text())
    rid = manifest.get('release_id', '')
    if not remote.SHA.fullmatch(rid) or manifest.get('schema') not in ('codex-agent-pg-upgrade-v1', remote.V2_SCHEMA):
        raise RuntimeError('Invalid upgrade manifest')
    destination = '/opt/codex-agent/releases/' + rid
    client = connect()
    try:
        if args.action == 'stage':
            stage(client, path, manifest, destination)
        else:
            result = run(client, 'python3 ' + destination + '/deploy/upgrade_remote.py ' + args.action
                         + ' --release ' + destination, timeout=4200 if manifest['schema'] == remote.V2_SCHEMA else 1200)
            report_path = path.parent / (args.action + '.json')
            report_path.write_text(result, encoding='utf-8')
            result = json.loads(result)
            print(json.dumps({k: result.get(k) for k in ('ok', 'phase', 'deployed', 'currently_active', 'health')}))
            print('Evidence: ' + str(report_path))
    finally:
        client.close()


if __name__ == '__main__':
    main()
