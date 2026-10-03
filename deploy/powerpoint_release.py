#!/usr/bin/env python3
"""Journaled schema-preserving PowerPoint release; never restores user data."""
import argparse
from contextlib import contextmanager
import hashlib
import json
import math
import os
from pathlib import Path, PurePosixPath
import re
import shlex
import subprocess
import tempfile
import time
import urllib.request
try:
    import fcntl
except ImportError:
    fcntl = None  # Pure validation and mocked tests remain importable on Windows.

ROOT = Path('/opt/codex-agent')
UNIT = Path('/etc/systemd/system/codex-agent.service')
ENV = Path('/etc/codex-agent/production.env')
ADMIN_KEY = Path('/var/lib/codex-agent/client-key')
PROC = Path('/proc')
SERVICE = 'codex-agent.service'
AUXILIARY_UNITS = []
HASH = re.compile(r'[a-f0-9]{64}')
REQUIRED_CHECKS = ('go', 'vet', 'frontend', 'live_cli', 'live_ws', 'race', 'redis', 'schema_unchanged')
HEALTH_STAGES = frozenset(('current', 'binary', 'path', 'web', 'live', 'ready', 'root',
                           'root_hash', 'admin_credential', 'inference_http', 'inference_json', 'inference'))
RELEASE_STAGES = frozenset(('stop', 'configuration', 'manifest', 'artifacts', 'schema',
                            'unit_write', 'reload', 'start', 'health', 'post_configuration',
                            'post_schema', 'state_write', 'unknown'))


class HealthCheckError(RuntimeError):
    """Only bounded diagnostic labels; never preserve response text or a token."""
    def __init__(self, stage, attempts):
        self.stage = stage if stage in HEALTH_STAGES else 'current'
        self.attempts = max(0, min(int(attempts), 1000))
        super().__init__('release health verification did not pass (stage=' + self.stage + ')')


def failure_diagnostic(stage, error):
    """Do not serialize exception messages/types supplied by external systems."""
    result = {'stage': stage if stage in RELEASE_STAGES else 'unknown', 'code': 'check_failed'}
    if isinstance(error, HealthCheckError):
        result.update(code='health_timeout', health_stage=error.stage, attempts=error.attempts)
    return result


def require(value, message):
    if not value:
        raise RuntimeError(message)


def digest(path):
    value=hashlib.sha256()
    with Path(path).open('rb') as handle:
        while block:=handle.read(1024*1024):value.update(block)
    return value.hexdigest()


def command(*args, timeout=90):
    result = subprocess.run(args, capture_output=True, text=True, timeout=timeout)
    require(result.returncode == 0, 'release system command failed')
    return result.stdout.strip()


def read_json(path):
    def unique(pairs):
        result = {}
        for key, value in pairs:
            require(key not in result, 'duplicate JSON field')
            result[key] = value
        return result
    value = json.loads(Path(path).read_text(encoding='utf-8'), object_pairs_hook=unique)
    require(isinstance(value, dict), 'JSON document must be an object')
    return value


def atomic_write(path, content, mode=0o600):
    path = Path(path)
    require(not path.is_symlink(), 'refusing symlink write target')
    content = content.encode('utf-8') if isinstance(content, str) else content
    fd, name = tempfile.mkstemp(prefix='.' + path.name + '-', dir=path.parent)
    temporary = Path(name)
    try:
        with os.fdopen(fd, 'wb') as handle:
            os.fchmod(handle.fileno(), mode) if hasattr(os, 'fchmod') else os.chmod(name, mode)
            handle.write(content)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary, path)
        if os.name == 'posix':
            directory = os.open(path.parent, os.O_RDONLY | getattr(os, 'O_DIRECTORY', 0))
            try:
                os.fsync(directory)
            finally:
                os.close(directory)
    finally:
        if temporary.exists():
            temporary.unlink()


def write_json(path, value):
    atomic_write(path, json.dumps(value, indent=2, sort_keys=True) + '\n')


def relative_name(name):
    require(isinstance(name, str) and name and not any(c in name for c in '\\:\r\n\0'), 'invalid manifest path')
    parsed = PurePosixPath(name)
    require(not parsed.is_absolute() and '..' not in parsed.parts and str(parsed) == name, 'invalid manifest path')
    return parsed


def regular(path):
    path = Path(path)
    require(path.is_absolute() and path.is_file() and not path.is_symlink(), 'invalid regular file')
    require(not any(parent.is_symlink() for parent in path.parents), 'symlink file ancestor')
    return path


def release_path(release):
    release = Path(release)
    require(release.is_absolute() and release.parent == ROOT / 'releases' and HASH.fullmatch(release.name), 'invalid release path')
    require(release.is_dir() and not release.is_symlink() and not any(p.is_symlink() for p in release.parents), 'invalid release directory')
    return release


def artifact(release, name):
    path = release / str(relative_name(name))
    require(path.resolve().is_relative_to(release), 'release file escaped its root')
    return regular(path)


def validate(release):
    release = release_path(release)
    manifest = read_json(artifact(release, 'manifest.json'))
    require(manifest.get('backend') == 'powerpoint_bps', 'wrong inference backend')
    require(isinstance(manifest.get('source_sha256'), str) and HASH.fullmatch(manifest['source_sha256']), 'invalid source digest')
    files = manifest.get('files')
    require(isinstance(files, dict) and files, 'invalid artifact manifest')
    required = {'codex-agent', 'frontend/dist/index.html', 'acceptance.json', 'source-manifest.json', 'deploy/powerpoint_release.py'}
    require(required <= set(files) and 'manifest.json' not in files, 'release incomplete')
    for name, expected in files.items():
        relative_name(name)
        require(name in required or name.startswith('frontend/dist/'), 'unexpected release artifact')
        require(isinstance(expected, str) and HASH.fullmatch(expected), 'invalid artifact digest')
        path = artifact(release, name)
        require(digest(path) == expected, 'release artifact checksum mismatch')
        if os.name == 'posix':
            require(path.stat().st_mode & 0o004 and all(p.stat().st_mode & 0o001 for p in path.parents), 'runtime cannot read release artifact')
    actual = set()
    for path in release.rglob('*'):
        require(not path.is_symlink(), 'release contains a symlink')
        if path.is_file():
            actual.add(path.relative_to(release).as_posix())
        else:
            require(path.is_dir(), 'release contains non-regular artifact')
    require(actual == set(files) | {'manifest.json'}, 'release contains unmanifested or missing files')
    binary = artifact(release, 'codex-agent')
    with binary.open('rb') as handle:
        header = handle.read(20)
    require(len(header) == 20 and header[:6] == b'\x7fELF\x02\x01' and header[18:20] == b'\x3e\x00', 'candidate is not Linux amd64 ELF')
    if os.name == 'posix':
        require(binary.stat().st_mode & 0o001, 'candidate is not runtime executable')
    acceptance = read_json(artifact(release, 'acceptance.json'))
    require(acceptance.get('passed') is True, 'acceptance did not pass')
    require(acceptance.get('source_sha256') == manifest['source_sha256'], 'source evidence mismatch')
    require(acceptance.get('binary_sha256') == files['codex-agent'], 'binary evidence mismatch')
    checks = acceptance.get('checks')
    require(isinstance(checks, dict) and all(checks.get(x) is True for x in REQUIRED_CHECKS), 'release checks missing')
    source = read_json(artifact(release, 'source-manifest.json'))
    source_files = source.get('files')
    require(isinstance(source_files, dict) and source_files, 'source manifest file map missing')
    for name, expected in source_files.items():
        parts = relative_name(name).parts
        require(not {'.local', 'data', '.git'}.intersection(parts) and isinstance(expected, str) and HASH.fullmatch(expected), 'invalid source manifest entry')
    actual_source = hashlib.sha256(json.dumps(source_files, sort_keys=True).encode()).hexdigest()
    require(source.get('sha256') == manifest['source_sha256'] == actual_source, 'source manifest identity mismatch')
    migrations = manifest.get('migration_checksums')
    require(isinstance(migrations, dict) and migrations, 'migration checksums missing')
    for name, value in migrations.items():
        require(str(relative_name(name)).startswith('migrations/') and name.endswith('.sql') and isinstance(value, str) and HASH.fullmatch(value), 'invalid migration checksum')
    require({name: value for name, value in source_files.items() if name.startswith('migrations/') and name.endswith('.sql')} == migrations, 'source migrations do not match release schema')
    return manifest


def current():
    pid = int(command('systemctl', 'show', SERVICE, '-p', 'MainPID', '--value'))
    require(pid > 0, 'service not running')
    argv = (PROC / str(pid) / 'cmdline').read_bytes().decode().rstrip('\0').split('\0')
    require(argv.count('--web') == 1 and argv.index('--web') + 1 < len(argv), 'unexpected production process')
    binary, web = Path(argv[0]), Path(argv[argv.index('--web') + 1])
    require(binary.name == 'codex-agent', 'unexpected production executable')
    # Earlier reviewed deployments used distinct immutable binary/UI release
    # trees. Baseline ownership is established by ROOT containment and hashes;
    # only the new candidate must use ROOT/releases/<id>.
    require(binary.is_absolute() and binary.is_relative_to(ROOT) and web.is_absolute() and web.is_relative_to(ROOT), 'runtime paths escaped application root')
    require(web.parts[-2:] == ('frontend', 'dist'), 'unexpected runtime frontend path')
    regular(binary)
    runtime_hash = digest(PROC / str(pid) / 'exe')
    require(digest(binary) == runtime_hash, 'running and on-disk baseline binary differ')
    return {'pid': pid, 'binary': str(binary), 'web': str(web), 'binary_sha256': runtime_hash,
            'effective_unit':str(UNIT),'auxiliary_units':auxiliary_units(),
            'web_sha256': digest(regular(web / 'index.html')), 'unit_sha256': digest(regular(UNIT)), 'env_sha256': digest(regular(ENV))}

def auxiliary_units():
    return {str(p):digest(regular(p)) for p in AUXILIARY_UNITS}

def select_effective_unit(fragment,dropins):
    chosen=Path(fragment)
    paths=[chosen]+[Path(p) for p in dropins]
    for path in paths:
        content=regular(path).read_text(encoding='utf-8')
        if re.search(r'(?m)^ExecStart=.+$',content):chosen=path
    return chosen,[p for p in paths if p!=chosen]

def configure_effective_unit():
    global UNIT,AUXILIARY_UNITS
    fragment=command('systemctl','show',SERVICE,'-p','FragmentPath','--value')
    dropins=shlex.split(command('systemctl','show',SERVICE,'-p','DropInPaths','--value'))
    UNIT,AUXILIARY_UNITS=select_effective_unit(fragment,dropins)


def schema():
    raw = command('docker', 'exec', '-e', 'PGOPTIONS=-c statement_timeout=15000', 'codex-agent-postgres',
                  'psql', '-X', '-q', '-At', '-v', 'ON_ERROR_STOP=1', '-U', 'postgres', '-d', 'codex_agent', '-c',
                  "SELECT coalesce(json_object_agg(name,checksum),'{}'::json) FROM schema_migrations", timeout=45)
    value = json.loads(raw)
    require(isinstance(value, dict), 'schema query did not return an object')
    return value


def http_get(path, token=None):
    headers = {'Authorization': 'Bearer ' + token} if token is not None else {}
    request = urllib.request.Request('http://127.0.0.1:18879' + path, headers=headers)
    class NoRedirect(urllib.request.HTTPRedirectHandler):
        def redirect_request(self, req, fp, code, msg, headers, newurl):
            return None
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())
    with opener.open(request, timeout=3) as response:
        require(response.status == 200 and response.geturl() == request.full_url, 'health endpoint failed or redirected')
        raw = response.read(16 * 1024 * 1024 + 1)
        require(len(raw) <= 16 * 1024 * 1024, 'health response exceeded limit')
        return raw


def read_admin_credential():
    """Read systemd StateDirectory's credential through its managed parent link.

    DynamicUser exposes /var/lib/codex-agent via /var/lib/private/codex-agent.
    Only this read-only credential path permits such a parent link; release and
    unit artifact validation continue to require an entirely regular path.
    """
    path = Path(ADMIN_KEY)
    require(path.is_absolute() and not path.is_symlink(), 'invalid local admin credential file')
    resolved = regular(path.resolve(strict=True))
    fd = os.open(resolved, os.O_RDONLY | getattr(os, 'O_NOFOLLOW', 0))
    with os.fdopen(fd, 'rb') as handle:
        opened, named = os.fstat(handle.fileno()), resolved.stat()
        require((opened.st_dev, opened.st_ino) == (named.st_dev, named.st_ino), 'admin credential changed during open')
        raw = handle.read(8193)
    require(len(raw) <= 8192, 'admin credential exceeds limit')
    token = raw.decode('utf-8').strip()
    require(token and '\r' not in token and '\n' not in token, 'local admin credential unavailable')
    return token


def health(expected_binary, expected_web=None, expected_root=None, *, require_bps=False, expected_path=None):
    deadline = time.monotonic() + 60
    stage, attempts = 'current', 0
    while time.monotonic() < deadline:
        attempts += 1
        try:
            stage = 'current'
            facts = current()
            stage = 'binary'
            require(facts['binary_sha256'] == expected_binary, 'wrong running binary')
            if expected_path is not None:
                stage = 'path'
                require(facts['binary'] == str(expected_path), 'wrong running executable path')
            if expected_web is not None:
                stage = 'web'
                require(facts['web'] == str(expected_web), 'wrong running frontend path')
            for stage, path in (('live', '/health/live'), ('ready', '/health/ready')):
                http_get(path)
            stage = 'root'
            root = http_get('/')
            if expected_root is not None:
                stage = 'root_hash'
                require(hashlib.sha256(root).hexdigest() == expected_root, 'served frontend differs from candidate')
            if require_bps:
                stage = 'admin_credential'
                token = read_admin_credential()
                stage = 'inference_http'
                raw_settings = http_get('/api/settings', token)
                stage = 'inference_json'
                settings = json.loads(raw_settings)
                require(isinstance(settings, dict), 'settings must be an object')
                stage = 'inference'
                inference = settings.get('inference', {})
                require(isinstance(inference, dict), 'inference must be an object')
                require(inference.get('backend') == 'powerpoint_bps' and inference.get('codex_fallback') is False, 'running inference backend differs')
                facts['inference_backend'] = 'powerpoint_bps'
            return facts
        except Exception:
            time.sleep(0.5)
    raise HealthCheckError(stage, attempts) from None


def stop_timeout(value):
    units = {'us': 0.000001, 'ms': 0.001, 's': 1, 'min': 60, 'h': 3600}
    parts = re.findall(r'(\d+(?:\.\d+)?)(us|ms|min|s|h)', value)
    require(parts and ''.join(n + u for n, u in parts) == value.replace(' ', ''), 'unsupported service stop timeout')
    seconds = sum(float(n) * units[u] for n, u in parts)
    require(0 < seconds <= 600, 'service stop timeout outside bounded release window')
    return math.ceil(seconds) + 30


def require_legacy_service_control():
    require(not Path('/var/lib/codex-agent-release/active-runtime.json').exists(),
            'stable ingress installed; legacy service stop or rollback refused')


def stop_service():
    require_legacy_service_control()
    timeout = stop_timeout(command('systemctl', 'show', SERVICE, '-p', 'TimeoutStopUSec', '--value'))
    command('systemctl', 'stop', SERVICE, timeout=timeout)
    require(command('systemctl', 'show', SERVICE, '-p', 'MainPID', '--value') == '0', 'old process not drained')
    require(command('systemctl', 'show', SERVICE, '-p', 'ActiveState', '--value') in ('inactive', 'failed'), 'service did not stop')


def candidate_unit(original, baseline, release):
    old, web = baseline['binary'], baseline['web']
    require(original.count(old) == 1 and original.count(web) == 1, 'cannot locate exact active unit paths')
    require(len(re.findall(r'(?m)^ExecStart=.+$', original)) == 1 and not re.search(r'(?m)^ExecStart=.*\\$', original), 'unsupported ExecStart syntax')
    start = re.search(r'(?m)^ExecStart=(.+)$', original).group(1)
    executable_matches=start.startswith(old+' ') or start.startswith('"'+old+'" ')
    web_matches=any(value in start for value in ['--web '+web,'--web "'+web+'"','"--web" "'+web+'"'])
    require(executable_matches and web_matches, 'unit ExecStart does not match running process')
    changed = original.replace(old, str(release / 'codex-agent')).replace(web, str(release / 'frontend/dist'))
    require(changed != original, 'candidate already active')
    return changed


def state_directory(release):
    path = ROOT / 'release-state' / release.name
    require(not any(p.is_symlink() for p in (path, *path.parents)), 'invalid release-state directory')
    path.mkdir(parents=True, exist_ok=True, mode=0o700)
    return path


def rollback(state_dir):
    require_legacy_service_control()
    state = read_json(regular(state_dir / 'state.json'))
    baseline = state['baseline']
    original = regular(state_dir / 'previous.service').read_bytes()
    require(hashlib.sha256(original).hexdigest() == baseline['unit_sha256'], 'rollback unit checksum mismatch')
    require(digest(regular(ENV)) == baseline['env_sha256'], 'runtime environment changed; rollback refused')
    require(auxiliary_units()==baseline.get('auxiliary_units',{}),'auxiliary service configuration changed; rollback refused')
    require(digest(regular(Path(baseline['binary']))) == baseline['binary_sha256'], 'rollback executable changed')
    require(digest(regular(Path(baseline['web']) / 'index.html')) == baseline['web_sha256'], 'rollback frontend changed')
    require(digest(regular(UNIT)) in (baseline['unit_sha256'], state['candidate_unit_sha256']), 'another deployment changed the unit; rollback refused')
    state['phase'] = 'rolling_back'
    write_json(state_dir / 'state.json', state)
    stage = 'stop'
    try:
        stop_service()
        stage = 'configuration'
        require(digest(regular(ENV)) == baseline['env_sha256'], 'runtime environment changed while rollback drained')
        require(digest(regular(UNIT)) in (baseline['unit_sha256'], state['candidate_unit_sha256']), 'unit changed while rollback drained')
        require(auxiliary_units()==baseline.get('auxiliary_units',{}),'auxiliary service configuration changed while rollback drained')
        stage = 'schema'
        require(schema() == state['migration_checksums'], 'schema changed; automatic old-writer restart refused')
        stage = 'unit_write'
        atomic_write(UNIT, original, 0o644)
        stage = 'reload'
        command('systemctl', 'daemon-reload')
        command('systemctl', 'reset-failed', SERVICE)
        stage = 'start'
        command('systemctl', 'start', SERVICE)
        stage = 'health'
        facts = health(baseline['binary_sha256'], baseline['web'], baseline['web_sha256'], expected_path=baseline['binary'])
        stage = 'post_schema'
        require(schema() == state['migration_checksums'], 'schema changed during rollback')
        state.update(phase='rolled_back', runtime=facts, database_restored=False, environment_restored=False)
        stage = 'state_write'
        write_json(state_dir / 'state.json', state)
        return facts
    except Exception as error:
        state['phase'] = 'rollback_failed'
        state['rollback_failure'] = failure_diagnostic(stage, error)
        write_json(state_dir / 'state.json', state)
        raise RuntimeError('rollback incomplete; inspect release state before further activation') from None


def perform(action, release):
    if action in ('activate', 'rollback'):
        require_legacy_service_control()
    release = release_path(release)
    state_dir = state_directory(release)
    state_path = state_dir / 'state.json'
    if action == 'rollback':
        return {'rolled_back': True, 'runtime': rollback(state_dir), 'database_restored': False}
    if action == 'status':
        return {'runtime': current(), 'state': read_json(state_path) if state_path.exists() else None}
    if state_path.exists():
        state = read_json(regular(state_path))
        if state.get('phase') in ('switching', 'rolling_back', 'rollback_failed'):
            rollback(state_dir)
            raise RuntimeError('interrupted activation recovered; run a fresh preflight')
    manifest = validate(release)
    require(schema() == manifest['migration_checksums'], 'database schema differs; no automatic migration permitted')
    baseline = current()
    original = regular(UNIT).read_text(encoding='utf-8')
    candidate = candidate_unit(original, baseline, release)
    candidate_hash = hashlib.sha256(candidate.encode('utf-8')).hexdigest()
    freeze = {'baseline': baseline, 'migration_checksums': manifest['migration_checksums'],
              'manifest_sha256': digest(artifact(release, 'manifest.json')),
              'candidate_binary_sha256': manifest['files']['codex-agent'], 'candidate_unit_sha256': candidate_hash}
    if action == 'preflight':
        stop_timeout(command('systemctl', 'show', SERVICE, '-p', 'TimeoutStopUSec', '--value'))
        write_json(state_dir / 'preflight.json', freeze)
        return {'preflight': True, 'baseline': baseline, 'schema_unchanged': True}
    require(action == 'activate', 'invalid release action')
    preflight = read_json(regular(state_dir / 'preflight.json'))
    require(preflight == freeze, 'runtime, schema or release artifacts changed since preflight')
    atomic_write(state_dir / 'previous.service', original)
    state = dict(freeze, phase='switching')
    write_json(state_path, state)
    stage = 'stop'
    try:
        stop_service()
        stage = 'configuration'
        require(digest(regular(UNIT)) == baseline['unit_sha256'] and digest(regular(ENV)) == baseline['env_sha256'], 'runtime configuration changed while draining')
        require(auxiliary_units()==baseline.get('auxiliary_units',{}),'auxiliary service configuration changed while draining')
        stage = 'manifest'
        require(digest(artifact(release, 'manifest.json')) == freeze['manifest_sha256'], 'manifest changed while draining')
        stage = 'artifacts'
        validate(release)
        stage = 'schema'
        require(schema() == manifest['migration_checksums'], 'schema changed while draining')
        stage = 'unit_write'
        atomic_write(UNIT, candidate, 0o644)
        stage = 'reload'
        command('systemctl', 'daemon-reload')
        stage = 'start'
        command('systemctl', 'start', SERVICE)
        stage = 'health'
        facts = health(manifest['files']['codex-agent'], release / 'frontend/dist', manifest['files']['frontend/dist/index.html'], require_bps=True, expected_path=release / 'codex-agent')
        stage = 'post_configuration'
        require(digest(regular(UNIT))==candidate_hash and auxiliary_units()==baseline.get('auxiliary_units',{}),'service configuration changed during activation')
        stage = 'post_schema'
        require(schema() == manifest['migration_checksums'] and digest(regular(ENV)) == baseline['env_sha256'], 'schema or environment changed during activation')
        state.update(phase='active', runtime=facts, schema_unchanged=True)
        stage = 'state_write'
        write_json(state_path, state)
        return {'deployed': True, 'runtime': facts, 'schema_unchanged': True}
    except Exception as error:
        diagnostic = failure_diagnostic(stage, error)
        state['activation_failure'] = diagnostic
        # A diagnostic journal failure must not suppress the existing rollback.
        try:
            write_json(state_path, state)
        except Exception:
            pass
        rollback(state_dir)
        label = diagnostic.get('health_stage', diagnostic['stage'])
        raise RuntimeError('candidate activation failed and was rolled back (stage=' + label + '); database and environment preserved') from None


@contextmanager
def deployment_lock():
    require(fcntl is not None, 'activation requires Linux flock')
    path = ROOT / '.deployment.lock'
    require(not path.is_symlink(), 'invalid deployment lock')
    fd = os.open(path, os.O_RDWR | os.O_CREAT | getattr(os, 'O_NOFOLLOW', 0), 0o600)
    with os.fdopen(fd, 'a') as handle:
        fcntl.flock(handle, fcntl.LOCK_EX)
        yield


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action', choices=['preflight', 'activate', 'rollback', 'status'])
    parser.add_argument('--release', required=True)
    args = parser.parse_args()
    release = Path(os.path.abspath(args.release))  # Preserve lexical symlinks for validation.
    with deployment_lock():
        configure_effective_unit()
        result = perform(args.action, release)
    print(json.dumps(result))


if __name__ == '__main__':
    main()
