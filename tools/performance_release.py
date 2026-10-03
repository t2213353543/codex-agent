"""Prepare/upload an immutable performance release, without migration or activation."""
import argparse
from datetime import datetime, timezone
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import re
import secrets
import shutil
import subprocess
import sys
import tarfile
import tempfile

try:
    from .acceptance import source_manifest
except ImportError:
    from acceptance import source_manifest

ROOT = Path(__file__).resolve().parents[1]
SPEC = importlib.util.spec_from_file_location('performance_package_validator', ROOT / 'deploy/performance_release.py')
executor = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(executor)
require = executor.require
VERIFICATION_SCHEMA = 'codex-agent-performance-verification-v1'
REMOTE_ROOT = '/opt/codex-agent/releases'
CONTROLLERS = executor.CONTROLLERS | {'deploy/reliability_release.py'}


def sha(path):
    return executor.base.digest(path)


def local_file(path):
    path = Path(path)
    if not path.is_absolute():
        path = ROOT / path
    require(path.is_file() and not path.is_symlink(), 'verification file is not a regular file')
    resolved = path.resolve()
    require(resolved.is_relative_to(ROOT) and not any(p.is_symlink() for p in path.parents),
            'verification file escaped workspace')
    return resolved


def verification_references(references):
    require(isinstance(references, list) and references, 'verification references missing')
    result = {}
    for reference in references:
        require(isinstance(reference, dict) and isinstance(reference.get('path'), str), 'invalid verification reference')
        path = local_file(reference['path'])
        digest = reference.get('sha256')
        require(executor.valid_hash(digest) and sha(path) == digest, 'verification evidence changed')
        result[digest] = path
    return result


def validate_verification(verification, source):
    require(verification.get('schema') == VERIFICATION_SCHEMA, 'incorrect verification schema')
    require(verification.get('source_sha256') == source['sha256'], 'verification source is stale')
    functional = verification.get('functional')
    require(isinstance(functional, dict) and functional.get('passed') is True, 'functional verification did not pass')
    checks = functional.get('checks')
    require(isinstance(checks, dict) and set(checks) >= executor.FUNCTIONAL_CHECKS
            and all(value is True for value in checks.values()), 'required functional checks missing or failed')
    performance = verification.get('performance')
    require(isinstance(performance, dict) and type(performance.get('slo_met')) is bool,
            'performance SLO result must be explicit')
    remaining = performance.get('remaining')
    require(isinstance(remaining, list) and all(isinstance(value, str) and value.strip() for value in remaining)
            and (not remaining) == performance['slo_met'], 'performance remaining work contradicts SLO result')
    for section in (functional, performance):
        require('scope' not in section or isinstance(section['scope'], str) and section['scope'].strip(),
                'verification scope must be a nonempty description')
    return verification_references(functional.get('evidence_files')), verification_references(performance.get('evidence_files'))


def frontend_files():
    root = ROOT / 'frontend/dist'
    require((root / 'index.html').is_file(), 'build frontend/dist before preparing release')
    result = {}
    for path in root.rglob('*'):
        require(not path.is_symlink() and (path.is_dir() or path.is_file()), 'invalid frontend artifact')
        if path.is_file():
            result[path.relative_to(ROOT).as_posix()] = sha(path)
    return result


def copy_exact(original, target, digest, executable=False):
    target.parent.mkdir(parents=True, exist_ok=True)
    shutil.copyfile(original, target)
    target.chmod(0o755 if executable else 0o644)
    require(sha(target) == digest, 'input artifact changed while packaging')


def write_json(path, value):
    path.write_text(json.dumps(value, ensure_ascii=False, indent=2) + '\n', encoding='utf-8')
    path.chmod(0o644)


def prepare(args):
    source = source_manifest()
    verification_path = local_file(args.verification)
    verification = executor.base.read_json(verification_path)
    verification_sha = sha(verification_path)
    functional_refs, performance_refs = validate_verification(verification, source)
    current_migrations = {'migrations/' + p.name for p in (ROOT / 'migrations').glob('*.sql')}
    require(current_migrations == executor.MIGRATION_NAMES, 'source migration inventory differs from reviewed 28')
    version_path = ROOT / 'internal/server/main.go'
    require(sha(version_path) == source['files'].get('internal/server/main.go'), 'version source changed')
    versions = re.findall(r'(?m)^\s*const\s+version\s*=\s*"(0\.4\.(?:0|[1-9][0-9]*))"\s*(?://[^\r\n]*)?$',
                          version_path.read_text(encoding='utf-8-sig'))
    require(len(versions) == 1, 'runtime version must be a single 0.4.x literal')
    frontend = frontend_files()
    output = Path(args.output).resolve() if args.output else ROOT / '.local/performance-release/packages'
    require(output.is_relative_to(ROOT) and not any(p.is_symlink() for p in (output, *output.parents)),
            'release output escaped workspace')
    output.mkdir(parents=True, exist_ok=True)
    staging = Path(tempfile.mkdtemp(prefix='.prepare-', dir=output))
    staging.chmod(0o755)
    package = staging / 'package'
    package.mkdir()
    binary = package / 'codex-agent'
    build_log = staging / 'build.log'
    with build_log.open('wb') as log:
        result = subprocess.run(['go', 'build', '-trimpath', '-ldflags=-s -w', '-o', str(binary), '.'], cwd=ROOT,
                                env={**os.environ, 'GOOS': 'linux', 'GOARCH': 'amd64', 'CGO_ENABLED': '0'},
                                stdout=log, stderr=subprocess.STDOUT)
    require(result.returncode == 0, 'Linux build failed; inspect retained prepare directory build.log')
    binary.chmod(0o755)
    for name in sorted(CONTROLLERS | executor.MIGRATION_NAMES):
        require(name in source['files'], 'packaged source file missing from source manifest')
        copy_exact(ROOT / name, package / name, source['files'][name])
    for name, digest in frontend.items():
        copy_exact(ROOT / name, package / name, digest)
    all_refs = {**functional_refs, **performance_refs, verification_sha: verification_path}
    for digest, original in all_refs.items():
        copy_exact(original, package / 'evidence' / digest, digest)
    def refs(items):
        return [{'artifact_path': 'evidence/' + digest, 'sha256': digest} for digest in sorted(items)]
    functional = {key: value for key, value in verification['functional'].items() if key != 'evidence_files'}
    functional['evidence_files'] = refs(functional_refs)
    performance = {key: value for key, value in verification['performance'].items() if key != 'evidence_files'}
    performance['evidence_files'] = refs(performance_refs)
    acceptance = {'schema': executor.ACCEPTANCE_SCHEMA, 'source_sha256': source['sha256'],
                  'binary_sha256': sha(binary), 'functional': functional, 'performance': performance,
                  'frontend': {'artifact_files': frontend}, 'evidence_files': refs(all_refs)}
    write_json(package / 'acceptance.json', acceptance)
    write_json(package / 'source-manifest.json', source)
    manifest = {'schema': executor.SCHEMA, 'backend': executor.BACKEND, 'version': versions[0],
                'source_sha256': source['sha256'],
                'migration_checksums': {name: source['files'][name] for name in sorted(executor.MIGRATION_NAMES)},
                'files': {p.relative_to(package).as_posix(): sha(p) for p in sorted(package.rglob('*')) if p.is_file()}}
    manifest['release_id'] = executor.release_identity(manifest)
    manifest['built_at'] = datetime.now(timezone.utc).isoformat()
    write_json(package / 'manifest.json', manifest)
    require(source_manifest() == source, 'source changed during performance package build')
    require(frontend_files() == frontend, 'frontend changed during performance package build')
    executor.validate_package(package, manifest['release_id'])
    def mode(info):
        info.uid = info.gid = 0
        info.uname = info.gname = ''
        info.mode = 0o755 if info.isdir() or info.name == 'codex-agent' else 0o644
        return info
    with tarfile.open(staging / 'release.tar.gz', 'x:gz', compresslevel=1) as archive:
        for path in sorted(package.rglob('*')):
            if path.is_file():
                archive.add(path, arcname=path.relative_to(package).as_posix(), recursive=False, filter=mode)
    target = output / manifest['release_id']
    require(not target.exists(), 'immutable local release already exists; use its verified manifest')
    staging.rename(target)
    return target / 'package/manifest.json'


def load_package(manifest_path):
    path = local_file(manifest_path)
    manifest = executor.base.read_json(path)
    identity = manifest.get('release_id')
    require(path.name == 'manifest.json' and executor.valid_hash(identity), 'invalid local release identity')
    executor.validate_package(path.parent, identity)
    archive = path.parent.parent / 'release.tar.gz'
    require(archive.is_file() and not archive.is_symlink(), 'immutable release archive missing')
    expected = {**manifest['files'], 'manifest.json': sha(path)}
    seen = set()
    with tarfile.open(archive, 'r:gz') as tar:
        for member in tar:
            require(member.isfile() and member.name in expected and member.name not in seen, 'archive inventory mismatch')
            handle = tar.extractfile(member)
            require(handle is not None and hashlib.sha256(handle.read()).hexdigest() == expected[member.name], 'archive artifact checksum mismatch')
            seen.add(member.name)
    require(seen == set(expected), 'archive inventory incomplete')
    return path, manifest, archive, expected


REMOTE_VALIDATION = r'''
import fcntl,hashlib,json,os,pathlib,tarfile
root=pathlib.Path('/opt/codex-agent/releases')
def require(value,message):
 if not value:raise RuntimeError(message)
def digest(path):
 h=hashlib.sha256()
 with path.open('rb') as f:
  while True:
   block=f.read(1024*1024)
   if not block:break
   h.update(block)
 return h.hexdigest()
def verify(path):
 require(path.is_dir() and not any(p.is_symlink() for p in (path,*path.parents)),'invalid immutable release directory')
 actual=set()
 for p in path.rglob('*'):
  require(not p.is_symlink() and (p.is_file() or p.is_dir()),'invalid immutable release member')
  if p.is_file():actual.add(p.relative_to(path).as_posix())
 require(actual==set(PAYLOAD['files']),'immutable inventory mismatch')
 require(all(digest(path/name)==checksum for name,checksum in PAYLOAD['files'].items()),'immutable artifact checksum mismatch')
 require(os.access(path/'codex-agent',os.X_OK),'runtime binary is not executable')
target=root/PAYLOAD['release_id']
require(root.is_dir() and not any(p.is_symlink() for p in (root,*root.parents)),'release root unavailable')
if PAYLOAD['action']=='probe':
 if target.exists() or target.is_symlink():verify(target);print(json.dumps({'state':'exact'}))
 else:print(json.dumps({'state':'absent'}))
elif PAYLOAD['action']=='create-upload':
 incoming=root/PAYLOAD['incoming']
 require(incoming.parent==root and incoming.name.startswith('.performance-upload-'),'invalid upload path')
 incoming.mkdir(mode=0o700)
 print(json.dumps({'state':'upload-ready'}))
else:
 require(PAYLOAD['action']=='publish','invalid upload action')
 incoming=root/PAYLOAD['incoming']
 require(incoming.parent==root and incoming.name.startswith('.performance-upload-') and incoming.is_dir()
         and not any(p.is_symlink() for p in (incoming,*incoming.parents)),'invalid upload directory')
 archive=incoming/'release.tar.gz'
 require(archive.is_file() and not archive.is_symlink() and digest(archive)==PAYLOAD['archive_sha256'],'transferred archive mismatch')
 package=incoming/'package';package.mkdir(mode=0o755)
 seen=set()
 with tarfile.open(archive,'r:gz') as tar:
  for member in tar:
   require(member.isfile() and member.name in PAYLOAD['files'] and member.name not in seen,'archive inventory mismatch')
   relative=pathlib.PurePosixPath(member.name)
   require(not relative.is_absolute() and '..' not in relative.parts and str(relative)==member.name,'invalid archive path')
   path=package/member.name;path.parent.mkdir(parents=True,exist_ok=True)
   with tar.extractfile(member) as source,path.open('xb') as destination:
    while True:
     block=source.read(1024*1024)
     if not block:break
     destination.write(block)
    destination.flush();os.fsync(destination.fileno())
   path.chmod(0o755 if member.name=='codex-agent' else 0o644);seen.add(member.name)
 require(seen==set(PAYLOAD['files']),'archive inventory incomplete');verify(package)
 with (root/'.performance-upload.lock').open('a') as lock:
  fcntl.flock(lock,fcntl.LOCK_EX)
  if target.exists() or target.is_symlink():verify(target)
  else:package.rename(target)
  verify(target)
 print(json.dumps({'state':'uploaded','release_id':PAYLOAD['release_id']}))
'''


def remote_json(client, payload):
    script = 'PAYLOAD=' + repr(payload) + '\n' + REMOTE_VALIDATION
    stdin, stdout, stderr = client.exec_command('python3 -B -', timeout=300)
    stdin.write(script)
    stdin.channel.shutdown_write()
    raw = stdout.read().decode('utf-8', 'replace')
    code = stdout.channel.recv_exit_status()
    stderr.read()
    require(code == 0, 'immutable upload verification failed; inspect retained upload directory')
    result = json.loads(raw)
    require(isinstance(result, dict), 'invalid upload response')
    return result


def upload(args):
    _, manifest, archive, expected = load_package(args.release)
    try:
        from .integration_env import connect
    except ImportError:
        from integration_env import connect
    client = connect()
    try:
        payload = {'release_id': manifest['release_id'], 'files': expected, 'action': 'probe'}
        if remote_json(client, payload)['state'] == 'exact':
            return {'state': 'already_uploaded', 'release_id': manifest['release_id'], 'activated': False, 'migrated': False}
        incoming = '.performance-upload-' + manifest['release_id'] + '-' + secrets.token_hex(8)
        remote_json(client, {**payload, 'action': 'create-upload', 'incoming': incoming})
        with client.open_sftp() as sftp:
            sftp.put(str(archive), REMOTE_ROOT + '/' + incoming + '/release.tar.gz', confirm=True)
        remote_json(client, {**payload, 'action': 'publish', 'incoming': incoming, 'archive_sha256': sha(archive)})
        require(remote_json(client, payload)['state'] == 'exact', 'published release did not verify')
        return {'state': 'uploaded', 'release_id': manifest['release_id'], 'activated': False, 'migrated': False}
    finally:
        client.close()


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest='action', required=True)
    prepare_parser = commands.add_parser('prepare')
    prepare_parser.add_argument('--verification', required=True)
    prepare_parser.add_argument('--output')
    upload_parser = commands.add_parser('upload')
    upload_parser.add_argument('--release', required=True)
    args = parser.parse_args(argv)
    try:
        result = prepare(args) if args.action == 'prepare' else upload(args)
        print(str(result) if isinstance(result, Path) else json.dumps(result))
        return 0
    except Exception as error:
        # Fixed operational errors only; do not print network/DSN/process data.
        print(json.dumps({'state': 'failed', 'error': str(error) if type(error) is RuntimeError else type(error).__name__}), file=sys.stderr)
        return 1


if __name__ == '__main__':
    raise SystemExit(main())
