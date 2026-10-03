"""Prepare and publish evidence-bound runtime/usage releases."""
import argparse
from datetime import datetime, timezone
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import re
import shlex
import subprocess
import tarfile

from acceptance import source_manifest
from integration_env import ROOT, connect

_spec = importlib.util.spec_from_file_location('reliability_release_executor', ROOT / 'deploy/reliability_release.py')
executor = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(executor)
require = executor.require


def read(path):
    return json.loads(Path(path).read_text(encoding='utf-8-sig'))


def sha(path):
    return executor.base.digest(path)


def source_version(root, source):
    name = 'internal/server/main.go'
    path = root / name
    require(sha(path) == source.get('files', {}).get(name), 'release version source changed')
    matches = re.findall(r'(?m)^[ \t]*const[ \t]+version[ \t]*=[ \t]*"([^"\r\n]+)"[ \t]*(?://[^\r\n]*)?$', path.read_text(encoding='utf-8-sig'))
    require(len(matches) == 1, 'release version must be one literal Go constant')
    return executor.release_version(matches[0])


def prepare(args):
    evidence = read(args.evidence)
    before = source_manifest()
    version = source_version(ROOT, before)
    executor.validate_evidence(evidence, before, version)
    cli = read(args.official_cli_manifest)
    executor.validate_cli_spec(cli)
    originals = {}
    for reference in evidence['evidence_files']:
        name = str(executor.base.relative_name(reference['path']))
        path = ROOT / name
        require(path.resolve().is_relative_to(ROOT) and not path.is_symlink() and path.is_file(), 'evidence escaped workspace')
        require(sha(path) == reference['sha256'], 'verification evidence changed')
        artifact = 'evidence/' + reference['sha256']
        originals[artifact] = path
        reference['artifact_path'] = artifact
    frontend = {p.relative_to(ROOT).as_posix(): sha(p) for p in (ROOT / 'frontend/dist').rglob('*') if p.is_file()}
    require('frontend/dist/index.html' in frontend, 'frontend build missing')
    require(evidence['gates']['frontend'].get('artifact_files') == frontend, 'frontend evidence does not bind current build artifacts')
    output = ROOT / '.local/reliability-release'
    output.mkdir(parents=True, exist_ok=True)
    binary = output / 'codex-agent-linux'
    subprocess.run(['go', 'build', '-trimpath', '-ldflags=-s -w', '-o', str(binary), '.'], cwd=ROOT,
                   env={**os.environ, 'GOOS': 'linux', 'GOARCH': 'amd64', 'CGO_ENABLED': '0'}, check=True)
    ingress = output / 'codex-agent-ingress-linux'
    if (ROOT / 'cmd/codex-agent-ingress/main.go').exists():
        subprocess.run(['go', 'build', '-trimpath', '-ldflags=-s -w', '-o', str(ingress), './cmd/codex-agent-ingress'], cwd=ROOT,
                       env={**os.environ, 'GOOS': 'linux', 'GOARCH': 'amd64', 'CGO_ENABLED': '0'}, check=True)
    require(source_manifest() == before, 'sources changed during release build')
    evidence['binary_sha256'] = sha(binary)
    acceptance_raw = (json.dumps(evidence, ensure_ascii=False, indent=2) + '\n').encode()
    source_raw = (json.dumps(before, indent=2) + '\n').encode()
    names = ['deploy/powerpoint_release.py', 'deploy/reliability_release.py', executor.ADDED]
    if (ROOT / 'deploy/zero_downtime_release.py').exists():
        names.append('deploy/zero_downtime_release.py')
    originals.update({name: ROOT / name for name in names + list(frontend)})
    originals['codex-agent'] = binary
    if (ROOT / 'cmd/codex-agent-ingress/main.go').exists():
        originals['codex-agent-ingress'] = ingress
    files = {name: sha(path) for name, path in originals.items()}
    files.update({'acceptance.json': hashlib.sha256(acceptance_raw).hexdigest(), 'source-manifest.json': hashlib.sha256(source_raw).hexdigest()})
    identity = {'version': version, 'backend': 'selectable_official_runtime', 'source_sha256': before['sha256'], 'files': files,
                'migration_checksums': {'migrations/' + p.name: sha(p) for p in (ROOT / 'migrations').glob('*.sql')}, 'official_cli': cli}
    release_id = executor.map_digest(identity)
    release = output / release_id
    package = release / 'package'
    package.mkdir(parents=True, exist_ok=False)
    for name, path in originals.items():
        target = package / name
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_bytes(path.read_bytes())
        require(sha(target) == files[name], 'release artifact changed during packaging')
    (package / 'acceptance.json').write_bytes(acceptance_raw)
    (package / 'source-manifest.json').write_bytes(source_raw)
    manifest = {**identity, 'release_id': release_id, 'built_at': datetime.now(timezone.utc).isoformat()}
    (package / 'manifest.json').write_text(json.dumps(manifest, indent=2), encoding='utf-8')
    require(source_manifest() == before, 'sources changed during packaging')
    def mode(info):
        info.uid = info.gid = 0
        info.uname = info.gname = ''
        info.mode = 0o755 if info.isdir() or info.name in ('codex-agent', 'codex-agent-ingress') else 0o644
        return info
    with tarfile.open(release / 'release.tar.gz', 'w:gz', compresslevel=1) as archive:
        for path in sorted(package.rglob('*')):
            if path.is_file():
                archive.add(path, arcname=path.relative_to(package).as_posix(), filter=mode)
    print(str(package / 'manifest.json'))


def execute_remote(client, command, script=None, timeout=2400):
    stdin, stdout, stderr = client.exec_command(command, timeout=timeout)
    if script is not None:
        stdin.write(script)
        stdin.channel.shutdown_write()
    result = stdout.read().decode('utf-8', 'replace')
    status = stdout.channel.recv_exit_status()
    stderr.read()  # Never expose credentials from a child diagnostic.
    require(status == 0, 'remote release command failed; inspect its bounded state journal')
    return result


def inspect_cli(args):
    script = """import hashlib,json,pathlib
root=pathlib.Path(SOURCE)
assert root.is_dir() and not any(p.is_symlink() for p in (root,*root.parents))
files={}
for p in sorted(root.rglob('*')):
 assert not p.is_symlink() and (p.is_file() or p.is_dir())
 if p.is_file():
  digest=hashlib.sha256()
  with p.open('rb') as h:
   while block:=h.read(1024*1024):digest.update(block)
  files[p.relative_to(root).as_posix()]=digest.hexdigest()
print(json.dumps({'source_root':str(root),'entrypoint':'bin/codex','binary_sha256':files['bin/codex'],'files':files}))
""".replace('SOURCE', repr(executor.CLI_SOURCE.as_posix()))
    client = connect()
    try:
        raw = execute_remote(client, 'python3 -B -', script, timeout=120)
    finally:
        client.close()
    executor.validate_cli_spec(json.loads(raw))
    output = Path(args.output)
    output.parent.mkdir(parents=True, exist_ok=True)
    output.write_text(raw, encoding='utf-8')
    print(str(output))


def remote(args):
    manifest_path = Path(args.release).resolve()
    manifest = read(manifest_path)
    release_id = manifest['release_id']
    require(isinstance(release_id, str) and executor.base.HASH.fullmatch(release_id), 'invalid release identity')
    destination = '/opt/codex-agent/releases/' + release_id
    client = connect()
    try:
        if args.action == 'stage':
            archive = manifest_path.parent.parent / 'release.tar.gz'
            remote_archive = destination + '.tar.gz'
            with client.open_sftp() as sftp:
                sftp.get_channel().settimeout(120)
                sftp.put(str(archive), remote_archive, confirm=True)
            script = """import hashlib,pathlib,tarfile
archive=pathlib.Path(ARCHIVE);root=pathlib.Path(DESTINATION)
assert hashlib.sha256(archive.read_bytes()).hexdigest()==DIGEST
assert not root.exists() and not any(p.is_symlink() for p in root.parents)
root.mkdir(mode=0o755)
with tarfile.open(archive,'r:gz') as source:
 members=source.getmembers()
 for member in members:
  target=root/member.name
  assert member.isfile() and not member.issym() and not member.islnk() and target.resolve().is_relative_to(root)
 for member in members:
  target=root/member.name;target.parent.mkdir(parents=True,exist_ok=True,mode=0o755)
  with source.extractfile(member) as reader,target.open('xb') as writer:
   import shutil;shutil.copyfileobj(reader,writer)
  target.chmod(0o755 if member.name in ('codex-agent','codex-agent-ingress') else 0o644)
print('Release artifacts staged')
""".replace('ARCHIVE', repr(remote_archive)).replace('DESTINATION', repr(destination)).replace('DIGEST', repr(sha(archive)))
            execute_remote(client, 'python3 -B -', script, timeout=120)
            action = 'stage-cli'
        else:
            action = args.action
        command = 'python3 -B ' + shlex.quote(destination + '/deploy/reliability_release.py') + ' ' + action + ' --release ' + shlex.quote(destination)
        raw = execute_remote(client, command)
        (manifest_path.parent.parent / (args.action + '.json')).write_text(raw, encoding='utf-8')
        print(raw)
    finally:
        client.close()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action', choices=['inspect-cli', 'prepare', 'stage', 'preflight', 'activate', 'rollback', 'status'])
    parser.add_argument('--evidence')
    parser.add_argument('--official-cli-manifest')
    parser.add_argument('--release')
    parser.add_argument('--output', default=str(ROOT / '.local/reliability-modes/official-cli-manifest.json'))
    args = parser.parse_args()
    if args.action == 'inspect-cli':
        inspect_cli(args)
    elif args.action == 'prepare':
        if not args.evidence or not args.official_cli_manifest:
            parser.error('prepare requires --evidence and --official-cli-manifest')
        prepare(args)
    else:
        if not args.release:
            parser.error('--release is required')
        remote(args)


if __name__ == '__main__':
    main()
