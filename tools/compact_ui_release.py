"""Freeze and hot-publish static UI over a verified immutable backend release."""
import argparse
import importlib.util
import json
from pathlib import Path
import shlex
import shutil
import tarfile

from integration_env import ROOT, connect

spec = importlib.util.spec_from_file_location('compact_ui_remote', ROOT / 'deploy/compact_ui_release.py')
remote = importlib.util.module_from_spec(spec)
spec.loader.exec_module(remote)
BASELINE_ID = 'cd2b4b1b075735e468759f196044d4930989db1ef07cad1b74356c9babd218a3'
BASELINE = ROOT / '.local/reliability-release' / BASELINE_ID / 'package/manifest.json'
OUTPUT = ROOT / '.local/compact-ui-release'


def select_backend_manifest(path):
    """Bind an explicit current release; its server manifest is checked again."""
    global BASELINE, BASELINE_ID
    candidate = Path(path).resolve(strict=True)
    remote.require(candidate.is_relative_to((ROOT / '.local/reliability-release').resolve()), 'backend manifest must belong to the local reliability releases')
    manifest = remote.load(candidate)
    identity = {key: value for key, value in manifest.items() if key not in ('release_id', 'built_at')}
    release_id = manifest.get('release_id')
    remote.require(isinstance(release_id, str) and remote.SHA.fullmatch(release_id), 'invalid backend release identity')
    remote.require(remote.object_sha(identity) == release_id, 'backend release manifest identity mismatch')
    remote.require(candidate.name == 'manifest.json' and candidate.parent.name == 'package' and candidate.parent.parent.name == release_id, 'backend manifest location differs from identity')
    remote.require('codex-agent' in manifest.get('files', {}) and 'frontend/dist/index.html' in manifest['files'], 'backend manifest is incomplete')
    BASELINE, BASELINE_ID = candidate, release_id


def source_manifest():
    folder = ROOT / 'frontend'
    files = {}
    for path in folder.rglob('*'):
        relative = path.relative_to(folder)
        if {'node_modules', 'dist', 'test-results', 'artifacts', '__pycache__', '.git'} & set(relative.parts):
            continue
        if path.is_file():
            files[relative.as_posix()] = remote.sha(path)
    return {'sha256': remote.object_sha(files), 'files': dict(sorted(files.items()))}


def fingerprint():
    return {'source_sha256': source_manifest()['sha256'],
            'dist_sha256': remote.object_sha(remote.file_map(ROOT / 'frontend/dist'))}


def prepare(args):
    evidence_path = Path(args.evidence).resolve()
    evidence = remote.load(evidence_path)
    before = source_manifest()
    dist = remote.file_map(ROOT / 'frontend/dist')
    remote.require(evidence.get('passed') is True, 'passing UI verification report required')
    remote.require(evidence.get('source_sha256') == before['sha256'], 'verification is for different frontend sources')
    remote.require(evidence.get('dist_sha256') == remote.object_sha(dist), 'verification is for a different build')
    remote.require(all(evidence.get('checks', {}).get(key) is True for key in ('build', 'unit', 'browser')),
                   'build, unit and browser verification required')
    baseline = remote.load(BASELINE)
    prefix = '/opt/codex-agent/releases/' + BASELINE_ID
    identity = {'schema': 'codex-agent-compact-ui-v1', 'source_manifest_sha256': '',
                'source_sha256': before['sha256'], 'dist': dist,
                'evidence_sha256': remote.sha(evidence_path),
                'remote_script_sha256': remote.sha(ROOT / 'deploy/compact_ui_release.py'),
                'baseline': {'binary': prefix + '/codex-agent',
                             'binary_sha256': baseline['files']['codex-agent'],
                             'web': prefix + '/frontend/dist',
                             'manifest': prefix + '/manifest.json', 'manifest_sha256': remote.sha(BASELINE),
                             'frontend': {name[len('frontend/dist/'):]: digest for name, digest in baseline['files'].items()
                                          if name.startswith('frontend/dist/')}}}
    source_raw = (json.dumps(before, indent=2, sort_keys=True) + '\n').encode()
    identity['source_manifest_sha256'] = remote.hashlib.sha256(source_raw).hexdigest()
    release_id = remote.object_sha(identity)
    output = OUTPUT / release_id
    output.mkdir(parents=True, exist_ok=False)
    shutil.copytree(ROOT / 'frontend/dist', output / 'dist')
    shutil.copyfile(ROOT / 'deploy/compact_ui_release.py', output / 'deploy.py')
    shutil.copyfile(evidence_path, output / 'evidence.json')
    (output / 'source.json').write_bytes(source_raw)
    remote.save(output / 'manifest.json', {**identity, 'release_id': release_id})
    remote.verify_package(output)
    remote.require(source_manifest() == before, 'frontend source changed during freeze')
    remote.require(remote.file_map(ROOT / 'frontend/dist') == dist, 'frontend build changed during freeze')
    with tarfile.open(output / 'release.tar.gz', 'w:gz') as archive:
        for path in sorted(output.rglob('*')):
            if path.is_file() and path.name != 'release.tar.gz':
                archive.add(path, arcname=path.relative_to(output).as_posix())
    print(str(output / 'manifest.json'))


def remote_action(args):
    manifest_path = Path(args.release).resolve(strict=True)
    release = manifest_path.parent
    manifest = remote.verify_package(release)
    destination = '/opt/codex-agent/ui-releases/' + manifest['release_id']
    client = connect()
    try:
        if args.action == 'stage':
            client.exec_command('mkdir -p /opt/codex-agent/ui-releases')[1].channel.recv_exit_status()
            archive = release / 'release.tar.gz'
            target = destination + '.tar.gz'
            with client.open_sftp() as sftp:
                sftp.get_channel().settimeout(60)
                sftp.put(str(archive), target, confirm=True)
            script = '''import pathlib,hashlib,tarfile,os
archive=pathlib.Path(%r); root=pathlib.Path(%r)
assert hashlib.sha256(archive.read_bytes()).hexdigest()==%r
assert not root.exists(), 'release already staged'
root.mkdir(mode=0o755)
with tarfile.open(archive,'r:gz') as source:
 for member in source.getmembers():
  path=root/member.name
  assert member.isfile() and not member.issym() and not member.islnk()
  assert path.resolve().is_relative_to(root) and path != root
  path.parent.mkdir(parents=True,exist_ok=True)
  path.write_bytes(source.extractfile(member).read());os.chmod(path,0o644)
print('{"staged": true}')
''' % (target, destination, remote.sha(archive))
            stdin, stdout, stderr = client.exec_command('python3 -', timeout=120)
            stdin.write(script)
            stdin.channel.shutdown_write()
        else:
            command = ['python3', destination + '/deploy.py', args.action, '--release', destination]
            stdin, stdout, stderr = client.exec_command(shlex.join(command), timeout=180)
        output = stdout.read().decode('utf-8', 'replace')
        status = stdout.channel.recv_exit_status()
        errors = stderr.read()
        (release / (args.action + '.json')).write_text(output, encoding='utf-8')
        print(output)
        remote.require(status == 0, 'UI release operation failed; remote stderr bytes=' + str(len(errors)))
    finally:
        client.close()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action', choices=['fingerprint', 'prepare', 'stage', 'preflight', 'activate', 'rollback', 'status'])
    parser.add_argument('--evidence')
    parser.add_argument('--release')
    parser.add_argument('--backend-manifest', type=Path, help='Verified current backend manifest for a UI-only release')
    args = parser.parse_args()
    if args.backend_manifest:
        select_backend_manifest(args.backend_manifest)
    if args.action == 'fingerprint':
        print(json.dumps(fingerprint(), indent=2))
    elif args.action == 'prepare':
        if not args.evidence:
            parser.error('--evidence is required')
        prepare(args)
    else:
        if not args.release:
            parser.error('--release is required')
        remote_action(args)


if __name__ == '__main__':
    main()
