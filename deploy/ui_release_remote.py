#!/usr/bin/env python3
"""Static UI release transaction; never migrates, exports, or restores databases."""
from __future__ import annotations

import argparse
import contextlib
import datetime as dt
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import re
import shutil
import stat
import subprocess
import time
import urllib.parse
import urllib.request
import uuid

BASELINE_ID = 'c6cffc50c2d83ade105b92ec6b79691af0d438ec6c02f067f3b659120b119545'
BINARY = Path('/opt/codex-agent/releases') / BASELINE_ID / 'codex-agent'
BINARY_SHA = '43e1af500093ae649d71e4cfd76e95b4e791eab5968af5b11d93d5ab7ae375da'
ROOT = Path('/opt/codex-agent')
SERVICE = 'codex-agent.service'
UNIT = Path('/etc/systemd/system') / SERVICE
ENVIRONMENT = Path('/etc/codex-agent/production.env')
BASE_URL = 'http://127.0.0.1:18879'
LISTEN = '0.0.0.0:18879'
REHEARSAL = False
SHA = re.compile(r'[a-f0-9]{64}')
ASSET = re.compile(r'assets/(?:[A-Za-z0-9_.-]+/)*[A-Za-z0-9_.-]+-[A-Za-z0-9_-]{6,}\.[A-Za-z0-9]+')
CHECKS = {'build', 'unit', 'browser', 'real_api'}
UI_VALIDATION_POST_PATHS = {'/api/session', '/api/logs/snapshot',
                            '/api/accounts/last-used', '/api/accounts/usage-batch', '/api/accounts/upstream/batch'}


class UIReleaseError(RuntimeError):
    """Do not include environment values or unsanitized subprocess output."""


def configure_rehearsal():
    global ROOT, SERVICE, UNIT, ENVIRONMENT, BASE_URL, LISTEN, REHEARSAL
    REHEARSAL = True
    ROOT = Path('/opt/codex-agent-integration/ui-rehearsal')
    SERVICE = 'codex-agent-integration-ui-rehearsal.service'
    UNIT = Path('/etc/systemd/system') / SERVICE
    ENVIRONMENT = Path('/opt/codex-agent-integration/rehearsal/config/production.env')
    BASE_URL, LISTEN = 'http://127.0.0.1:28884', '127.0.0.1:28884'


def sha(path):
    h = hashlib.sha256()
    with Path(path).open('rb') as handle:
        while block := handle.read(1024 * 1024):
            h.update(block)
    return h.hexdigest()


def object_sha(value):
    return hashlib.sha256(json.dumps(value, sort_keys=True).encode()).hexdigest()


def now():
    return dt.datetime.now(dt.timezone.utc).isoformat()


def regular(path):
    path = Path(path)
    if path.is_symlink() or not stat.S_ISREG(path.stat().st_mode):
        raise UIReleaseError('Expected a regular file')
    return path


def safe_directory(path):
    path = Path(path)
    if path.resolve(strict=True) != path or not path.is_dir():
        raise UIReleaseError('Directory must be real and must not contain symlink components')


def relative(value):
    p = PurePosixPath(value)
    if not value or p.is_absolute() or '..' in p.parts or str(p) != value or '\\' in value:
        raise UIReleaseError('Invalid relative artifact path')
    return value


def file_map(folder):
    safe_directory(folder)
    result = {}
    for path in sorted(folder.rglob('*')):
        if path.is_symlink():
            raise UIReleaseError('Artifact contains symlink')
        if path.is_file():
            result[path.relative_to(folder).as_posix()] = sha(regular(path))
        elif not path.is_dir():
            raise UIReleaseError('Artifact contains non-regular file')
    return result


def fsync_dir(path):
    fd = os.open(path, os.O_RDONLY | getattr(os, 'O_DIRECTORY', 0))
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def atomic_write(path, data, mode=0o600):
    path = Path(path)
    temporary = path.with_name('.' + path.name + '.' + uuid.uuid4().hex)
    fd = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL, mode)
    try:
        with os.fdopen(fd, 'wb') as handle:
            handle.write(data)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary, path)
        fsync_dir(path.parent)
    finally:
        if temporary.exists():
            temporary.unlink()


def write_json(path, value, mode=0o600):
    atomic_write(path, (json.dumps(value, indent=2, sort_keys=True) + '\n').encode(), mode)


@contextlib.contextmanager
def lock():
    import fcntl
    safe_directory(ROOT)
    target = ROOT / '.deployment.lock'
    fd = os.open(target, os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW, 0o600)
    with os.fdopen(fd, 'w') as handle:
        fcntl.flock(handle, fcntl.LOCK_EX)
        yield


def system(*args):
    result = subprocess.run(['systemctl', *args], capture_output=True, text=True, timeout=240)
    if result.returncode:
        raise UIReleaseError('systemctl operation failed: ' + args[0])
    return result.stdout.strip()


def validate_runtime():
    if sha(regular(BINARY)) != BINARY_SHA:
        raise UIReleaseError('Deployed PostgreSQL backend binary differs from the pinned release')
    if system('show', SERVICE, '--property=FragmentPath', '--value') != str(UNIT):
        raise UIReleaseError('Unexpected systemd unit fragment')
    if system('show', SERVICE, '--property=DropInPaths', '--value'):
        raise UIReleaseError('Unreviewed systemd drop-ins are present')
    regular(ENVIRONMENT)
    values = {}
    for line in ENVIRONMENT.read_text().splitlines():
        if line and not line.startswith('#') and '=' in line:
            key, value = line.split('=', 1)
            values[key] = value
    parsed = urllib.parse.urlparse(values.get('DATABASE_URL', ''))
    port, database = (25432, '/codex_agent_rehearsal') if REHEARSAL else (25433, '/codex_agent')
    if parsed.scheme not in ('postgres', 'postgresql') or parsed.hostname != '127.0.0.1' or parsed.port != port or parsed.path != database:
        raise UIReleaseError('Unit must retain the dedicated PostgreSQL runtime database')
    if not values.get('CREDENTIAL_ENCRYPTION_KEY') or not values.get('REDIS_URL'):
        raise UIReleaseError('Runtime encryption/Redis configuration is missing')


def parse_unit(raw):
    text = raw.decode('utf-8')
    if '\\\n' in text or '\r' in text:
        raise UIReleaseError('Unsupported unit line continuation or line endings')
    directives = {}
    section = ''
    for line in text.splitlines():
        if line.startswith('[') and line.endswith(']'):
            section = line
        elif section == '[Service]' and line and not line.startswith(('#', ';')) and '=' in line:
            key, value = line.split('=', 1)
            directives.setdefault(key, []).append(value)
    if directives.get('Type') != ['exec'] or directives.get('EnvironmentFile') != [str(ENVIRONMENT)]:
        raise UIReleaseError('Expected the existing Type=exec PostgreSQL service')
    if any(key in directives for key in ('ExecStartPre', 'ExecStartPost', 'ExecStop', 'ExecStopPost')):
        raise UIReleaseError('Unexpected service lifecycle command')
    if any('DATABASE_URL=' in value for value in directives.get('Environment', [])):
        raise UIReleaseError('Database environment must come from the pinned runtime environment file')
    commands = directives.get('ExecStart', [])
    if len(commands) != 1:
        raise UIReleaseError('Expected one ExecStart')
    expected = str(BINARY) + ' --listen ' + LISTEN + ' --web '
    if not commands[0].startswith(expected):
        raise UIReleaseError('ExecStart backend or arguments differ from pinned PostgreSQL service')
    web = commands[0][len(expected):]
    path = Path(web)
    if not re.fullmatch(r'/[A-Za-z0-9_./-]+', web) or path.name != 'dist' or path.parent.name != 'frontend':
        raise UIReleaseError('Unexpected frontend path')
    release = path.parent.parent
    allowed = release.parent == ROOT / 'ui-releases' and SHA.fullmatch(release.name)
    baseline = path == BINARY.parent / 'frontend/dist'
    if not (allowed or baseline):
        raise UIReleaseError('Frontend path is not an immutable Codex Agent release')
    safe_directory(path)
    regular(path / 'index.html')
    # Replacing this exact token leaves every other unit byte unchanged.
    token = ('ExecStart=' + commands[0]).encode()
    if raw.count(token) != 1:
        raise UIReleaseError('Ambiguous ExecStart directive')
    return path, token


def production_readonly_valid(value, binding):
    return (isinstance(value, dict)
            and value.get('synthetic_data') is False
            and value.get('data_mode') == 'production-readonly'
            and type(value.get('business_writes')) is int and value['business_writes'] == 0
            and type(value.get('gateway_requests')) is int and value['gateway_requests'] == 0
            and value.get('session_only_mutations') is True
            and value.get('backend_binary_sha256') == BINARY_SHA
            and all(value.get(key) == digest for key, digest in binding.items()))


def production_validation_valid(value, binding, require_requests=False):
    if not (isinstance(value, dict)
            and value.get('synthetic_data') is False
            and value.get('data_mode') == 'production-ui-validation'
            and type(value.get('business_writes')) is int and value['business_writes'] == 0
            and type(value.get('gateway_requests')) is int and value['gateway_requests'] == 0
            and value.get('session_only_mutations') is False
            and isinstance(value.get('allowed_post_paths'), list)
            and all(isinstance(path, str) for path in value['allowed_post_paths'])
            and len(value['allowed_post_paths']) == len(UI_VALIDATION_POST_PATHS)
            and set(value['allowed_post_paths']) == UI_VALIDATION_POST_PATHS
            and type(value.get('ephemeral_snapshot_writes')) is int
            and value['ephemeral_snapshot_writes'] >= 0
            and value.get('backend_binary_sha256') == BINARY_SHA
            and all(value.get(key) == digest for key, digest in binding.items())):
        return False
    if not require_requests:
        return True
    requests = value.get('requests')
    if not isinstance(requests, list) or not requests:
        return False
    snapshots = 0
    for request in requests:
        if not isinstance(request, dict):
            return False
        method, path, status = request.get('method'), request.get('path'), request.get('status')
        if (not isinstance(path, str) or not re.fullmatch(r'/api/[A-Za-z0-9_./-]+', path)
                or '//' in path or any(part in ('.', '..') for part in path.split('/'))
                or type(status) is not int or not 100 <= status <= 599):
            return False
        if method in ('GET', 'HEAD'):
            continue
        if method == 'DELETE' and path == '/api/session':
            continue
        if method != 'POST' or path not in UI_VALIDATION_POST_PATHS:
            return False
        if path == '/api/logs/snapshot':
            snapshots += 1
    return snapshots == value['ephemeral_snapshot_writes']


def evidence_valid(report, manifest, artifact_loader=None):
    if report.get('schema') != 'codex-agent-ui-acceptance-v1' or report.get('status') != 'pass':
        raise UIReleaseError('Passing UI acceptance evidence is required')
    binding = {'frontend_source_sha256': manifest['frontend_source']['sha256'],
               'dist_sha256': object_sha(manifest['frontend']),
               'backend_source_sha256': manifest['backend_source']['sha256']}
    if any(report.get(k) != v for k, v in binding.items()):
        raise UIReleaseError('Acceptance source or dist binding is stale')
    checks = report.get('checks', [])
    if len(checks) != len(CHECKS) or {c.get('id') for c in checks} != CHECKS:
        raise UIReleaseError('Build, unit, browser, and real API acceptance are all required')
    artifacts = manifest['evidence_artifacts']
    for check in checks:
        if check.get('status') != 'pass' or any(check.get(k) != v for k, v in binding.items()):
            raise UIReleaseError('A UI check failed or was run against another candidate')
        entries = check.get('artifacts', [])
        if not entries or any(a.get('path') not in artifacts or a.get('sha256') != artifacts[a['path']]['sha256'] for a in entries):
            raise UIReleaseError('UI check artifacts are missing or not hash-bound')
        if check['id'] == 'real_api':
            if check.get('backend_binary_sha256') != BINARY_SHA:
                raise UIReleaseError('Real API checks must use the pinned deployed PostgreSQL binary')
            if check.get('synthetic_data') is True and check.get('data_mode') not in ('production-readonly', 'production-ui-validation'):
                continue
            readonly = production_readonly_valid(check, binding)
            validation = production_validation_valid(check, binding)
            if not (readonly or validation):
                raise UIReleaseError('Production real API checks require read-only data, zero business writes '
                                     'and gateway requests, or the exact bounded UI validation contract')
            if artifact_loader is None:
                raise UIReleaseError('Production read-only evidence requires a hash-bound report artifact')
            verified_report = False
            for entry in entries:
                if not entry['path'].lower().endswith('.json'):
                    continue
                try:
                    artifact = json.loads(artifact_loader(entry['path']))
                except (ValueError, UnicodeError, OSError):
                    raise UIReleaseError('Production read-only report artifact is unreadable') from None
                artifact_matches = (production_readonly_valid(artifact, binding) if readonly
                                    else production_validation_valid(artifact, binding, require_requests=True))
                if validation and artifact_matches:
                    artifact_matches = artifact['ephemeral_snapshot_writes'] == check['ephemeral_snapshot_writes']
                if (artifact_matches
                        and (artifact.get('status') == 'pass' or artifact.get('passed') is True)
                        and artifact.get('status', 'pass') == 'pass'
                        and artifact.get('passed', True) is True):
                    verified_report = True
            if not verified_report:
                raise UIReleaseError('No passing production read-only artifact binds this candidate and backend')


def release_manifest(folder, allow_compat=True):
    safe_directory(folder)
    # Includes metadata parents: a nested symlink must not bypass regular().
    file_map(folder)
    manifest = json.loads(regular(folder / 'ui-manifest.json').read_text())
    if manifest.get('schema') != 'codex-agent-ui-release-v1' or manifest.get('backend_binary_sha256') != BINARY_SHA:
        raise UIReleaseError('Unexpected UI release manifest or backend identity')
    if manifest.get('backend_binary_path') != str(BINARY):
        raise UIReleaseError('Unexpected backend path')
    identity = {k: v for k, v in manifest.items() if k not in ('release_id', 'prepared_at')}
    if object_sha(identity) != manifest.get('release_id'):
        raise UIReleaseError('UI manifest identity mismatch')
    for section in ('frontend_source', 'backend_source'):
        if object_sha(manifest[section]['files']) != manifest[section]['sha256']:
            raise UIReleaseError('Source map digest mismatch')
    for name in manifest['frontend']:
        relative(name)
    if 'index.html' not in manifest['frontend']:
        raise UIReleaseError('Frontend index is missing')
    compat = {}
    compat_file = folder / 'compatibility.json'
    if allow_compat and compat_file.exists():
        metadata = json.loads(regular(compat_file).read_text())
        if metadata.get('release_id') != manifest['release_id']:
            raise UIReleaseError('Compatibility manifest identity mismatch')
        compat = metadata['files']
        if set(compat) & set(manifest['frontend']) or any(not ASSET.fullmatch(n) for n in compat):
            raise UIReleaseError('Compatibility files must be nonconflicting hashed assets')
    if file_map(folder / 'frontend/dist') != {**manifest['frontend'], **compat}:
        raise UIReleaseError('Frontend files do not match the immutable manifest')
    if sha(regular(folder / 'deploy/ui_release_remote.py')) != manifest['remote_script_sha256']:
        raise UIReleaseError('Release transaction helper hash mismatch')
    report_path = regular(folder / 'evidence/report.json')
    if sha(report_path) != manifest['acceptance_sha256']:
        raise UIReleaseError('Acceptance evidence hash mismatch')
    for artifact in manifest['evidence_artifacts'].values():
        name = relative(artifact['packaged_path'])
        if not name.startswith('evidence/artifacts/') or sha(regular(folder / name)) != artifact['sha256']:
            raise UIReleaseError('Packaged acceptance artifact hash mismatch')
    evidence_valid(json.loads(report_path.read_text()), manifest,
                   artifact_loader=lambda key: (folder / manifest['evidence_artifacts'][key]['packaged_path']).read_text(encoding='utf-8'))
    return manifest


def scoped_release(path):
    if path.parent != ROOT / 'ui-releases' or not SHA.fullmatch(path.name):
        raise UIReleaseError('Release must be a hash directory inside this project UI release root')


def stage(incoming, destination):
    scoped_release(destination)
    if incoming.parent != ROOT / '.ui-staging' or incoming.name != destination.name:
        raise UIReleaseError('Unexpected upload staging directory')
    with lock():
        validate_runtime()
        previous, _ = parse_unit(regular(UNIT).read_bytes())
        manifest = release_manifest(incoming, allow_compat=False)
        if destination.name != manifest['release_id']:
            raise UIReleaseError('Directory and release ID differ')
        if destination.exists():
            if release_manifest(destination)['release_id'] != manifest['release_id']:
                raise UIReleaseError('Immutable release already exists with different content')
            return {'staged': True, 'deployed': False, 'reused': True, 'release_id': destination.name}
        compatibility = {}
        for name, value in file_map(previous).items():
            if ASSET.fullmatch(name) and name not in manifest['frontend']:
                target = incoming / 'frontend/dist' / name
                target.parent.mkdir(parents=True, exist_ok=True, mode=0o755)
                shutil.copyfile(previous / name, target)
                os.chmod(target, 0o644)
                compatibility[name] = value
            elif ASSET.fullmatch(name) and manifest['frontend'].get(name) != value:
                raise UIReleaseError('Conflicting content under the same hashed asset filename')
        write_json(incoming / 'compatibility.json', {'release_id': destination.name, 'source': str(previous), 'files': compatibility}, 0o644)
        release_manifest(incoming)
        for path in incoming.iterdir():
            if path.name == 'ui-release.tar.gz' or re.fullmatch(r'[0-9]+\.[a-f0-9]{64}\.part', path.name):
                regular(path).unlink()
        for path in incoming.rglob('*'):
            os.chmod(path, 0o755 if path.is_dir() else 0o644)
        os.chmod(incoming / 'evidence', 0o700)
        os.chmod(incoming, 0o755)
        # Commit file data before publishing the directory name.
        for path in incoming.rglob('*'):
            if path.is_file():
                with path.open('rb') as handle:
                    os.fsync(handle.fileno())
        for path in sorted((p for p in incoming.rglob('*') if p.is_dir()), reverse=True):
            fsync_dir(path)
        fsync_dir(incoming)
        destination.parent.mkdir(mode=0o755, exist_ok=True)
        safe_directory(destination.parent)
        os.rename(incoming, destination)
        fsync_dir(destination.parent)
        return {'staged': True, 'deployed': False, 'release_id': destination.name, 'compatibility_files': len(compatibility)}


def wait_healthy(web, timeout=75):
    deadline = time.monotonic() + timeout
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    expected_index = sha(web / 'index.html')
    while time.monotonic() < deadline:
        try:
            if system('is-active', SERVICE) != 'active':
                raise UIReleaseError('Service is inactive')
            pid = int(system('show', SERVICE, '--property=MainPID', '--value'))
            if pid < 1 or sha(Path('/proc') / str(pid) / 'exe') != BINARY_SHA:
                raise UIReleaseError('Running backend binary identity mismatch')
            arguments = (Path('/proc') / str(pid) / 'cmdline').read_bytes().split(b'\0')
            if str(web).encode() not in arguments or b'--web' not in arguments:
                raise UIReleaseError('Running process serves another frontend')
            for path in ('/health/live', '/health/ready', '/'):
                with opener.open(BASE_URL + path, timeout=5) as response:
                    body = response.read()
                    if response.status != 200 or (path == '/' and hashlib.sha256(body).hexdigest() != expected_index):
                        raise UIReleaseError('Health or frontend response mismatch')
            return {'health_live': 200, 'health_ready': 200, 'root': 200, 'binary_sha256': BINARY_SHA,
                    'index_sha256': expected_index, 'checked_at': now()}
        except Exception:
            time.sleep(1)
    raise UIReleaseError('PostgreSQL service or new UI failed health verification')


class Transaction:
    def __init__(self, release):
        scoped_release(release)
        self.release = release
        self.manifest = release_manifest(release)
        if self.manifest['release_id'] != release.name:
            raise UIReleaseError('Directory and release identity differ')
        self.private = ROOT / '.ui-state' / release.name
        self.state_path = self.private / 'state.json'
        self.state = json.loads(regular(self.state_path).read_text()) if self.state_path.exists() else {
            'release_id': release.name, 'phase': 'staged', 'deployed': False, 'rehearsal': REHEARSAL,
            'database_migrated_by_ui_release': False, 'backend_binary_sha256': BINARY_SHA}

    def save(self, phase, **updates):
        self.private.parent.mkdir(mode=0o700, exist_ok=True)
        self.private.mkdir(mode=0o700, exist_ok=True)
        safe_directory(self.private)
        os.chmod(self.private.parent, 0o700)
        os.chmod(self.private, 0o700)
        self.state.update(updates, phase=phase, updated_at=now())
        if phase != 'deployed':
            self.state['rehearsal_deployed'] = False
        write_json(self.state_path, self.state)
        with (self.private / 'journal.jsonl').open('ab') as handle:
            handle.write((json.dumps({'at': now(), 'phase': phase}) + '\n').encode())
            handle.flush()
            os.fsync(handle.fileno())

    def before(self):
        data = regular(self.private / 'previous.service').read_bytes()
        if hashlib.sha256(data).hexdigest() != self.state['previous_unit_sha256']:
            raise UIReleaseError('Saved PostgreSQL unit checksum mismatch')
        parse_unit(data)
        return data

    def changed(self):
        original = self.before()
        previous, token = parse_unit(original)
        target = str(self.release / 'frontend/dist').encode()
        return original.replace(token, token[:-len(str(previous).encode())] + target)

    def rollback(self, automatic=False):
        validate_runtime()
        if self.state['phase'] == 'staged':
            raise UIReleaseError('No activation exists to roll back')
        original, changed = self.before(), self.changed()
        current = regular(UNIT).read_bytes()
        if current not in (original, changed):
            raise UIReleaseError('Current unit was changed by another deployment; rollback refused')
        if self.state['phase'] == 'rolled_back' and current == original:
            health = wait_healthy(parse_unit(original)[0])
            return {**self.state, 'health': health, 'reused': True}
        self.save('rollback_started', deployed=False, automatic_rollback=automatic)
        atomic_write(UNIT, original, 0o644)
        system('daemon-reload')
        system('restart', SERVICE)
        health = wait_healthy(parse_unit(original)[0])
        self.save('rolled_back', deployed=False, health=health, rollback_unit_sha256=sha(UNIT))
        return self.state

    def activate(self, fault=None):
        validate_runtime()
        if fault and not REHEARSAL:
            raise UIReleaseError('Fault injection is limited to synthetic rehearsal')
        if self.state['phase'] == 'deployed':
            if regular(UNIT).read_bytes() != self.changed():
                raise UIReleaseError('Another deployment changed the current unit')
            health = wait_healthy(self.release / 'frontend/dist')
            return {**self.state, 'health': health, 'reused': True}
        if self.state['phase'] not in ('staged', 'rolled_back'):
            # A killed command is recovered conservatively to the prior PG unit.
            result = self.rollback(automatic=True)
            raise UIReleaseError('Interrupted UI activation was rolled back; rerun activate to retry')
        current = regular(UNIT).read_bytes()
        previous, _ = parse_unit(current)
        if previous == self.release / 'frontend/dist':
            raise UIReleaseError('Candidate is active without its activation journal')
        if self.state['phase'] == 'rolled_back' and current != self.before():
            raise UIReleaseError('Another deployment replaced the rollback baseline')
        wait_healthy(previous)
        self.private.parent.mkdir(mode=0o700, exist_ok=True)
        self.private.mkdir(mode=0o700, exist_ok=True)
        safe_directory(self.private)
        atomic_write(self.private / 'previous.service', current)
        self.save('prepared', previous_unit_sha256=hashlib.sha256(current).hexdigest(), previous_frontend=str(previous))
        changed = self.changed()
        self.save('switch_requested', target_unit_sha256=hashlib.sha256(changed).hexdigest())
        try:
            atomic_write(UNIT, changed, 0o644)
            system('daemon-reload')
            system('restart', SERVICE)
            if fault == 'startup':
                system('stop', SERVICE)
                raise UIReleaseError('Synthetic startup fault injected')
            health = wait_healthy(self.release / 'frontend/dist')
            self.save('deployed', deployed=not REHEARSAL, rehearsal_deployed=REHEARSAL, health=health)
            return self.state
        except Exception as error:
            self.save('activation_failed', deployed=False, failure_type=type(error).__name__)
            try:
                self.rollback(automatic=True)
            except Exception as rollback_error:
                self.save('rollback_failed', deployed=False, rollback_failure_type=type(rollback_error).__name__)
                raise UIReleaseError('UI activation failed and automatic rollback needs recovery; inspect private state') from None
            raise UIReleaseError('UI activation failed; previous PostgreSQL unit restored and healthy') from None

    def status(self):
        current = regular(UNIT).read_bytes()
        active = self.state['phase'] == 'deployed' and current == self.changed()
        return {**self.state, 'currently_active': active, 'deployed': active and not REHEARSAL,
                'rehearsal_deployed': active and REHEARSAL}


def self_test():
    """Local state-machine simulation; no SSH, systemd, or database operations."""
    import tempfile
    from unittest.mock import patch
    results = []

    def record(name, action):
        action()
        results.append({'id': name, 'status': 'pass'})

    def rejected(action):
        try:
            action()
        except UIReleaseError:
            return
        raise AssertionError('Expected rejection')

    def unit_validation():
        body = ('[Service]\nType=exec\nEnvironmentFile=' + str(ENVIRONMENT) + '\nExecStart=' + str(BINARY)
                + ' --listen ' + LISTEN + ' --web ' + (BINARY.parent / 'frontend/dist').as_posix() + '\nTimeoutStopSec=180\n').encode()
        with patch(__name__ + '.safe_directory'), patch(__name__ + '.regular', side_effect=lambda p: Path(p)):
            assert parse_unit(body)[0] == BINARY.parent / 'frontend/dist'
            rejected(lambda: parse_unit(body.replace(str(BINARY).encode(), b'/opt/codex-agent/codex-agent')))
            rejected(lambda: parse_unit(body.replace(b'Type=exec', b'Type=simple')))
            rejected(lambda: parse_unit(body + b'ExecStartPre=/bin/true\n'))

    def evidence_validation():
        binding = {'frontend_source_sha256': 'f' * 64, 'backend_source_sha256': 'b' * 64,
                   'dist_sha256': object_sha({'index.html': 'd' * 64})}
        report = {'schema': 'codex-agent-ui-acceptance-v1', 'status': 'pass', **binding,
                  'checks': [{'id': item, 'status': 'pass', **binding, 'synthetic_data': True,
                              'backend_binary_sha256': BINARY_SHA, 'artifacts': [{'path': item, 'sha256': 'a' * 64}]}
                             for item in sorted(CHECKS)]}
        manifest = {'frontend_source': {'sha256': 'f' * 64}, 'backend_source': {'sha256': 'b' * 64},
                    'frontend': {'index.html': 'd' * 64},
                    'evidence_artifacts': {item: {'sha256': 'a' * 64} for item in CHECKS}}
        evidence_valid(report, manifest)
        rejected(lambda: evidence_valid({**report, 'dist_sha256': '0' * 64}, manifest))
        rejected(lambda: evidence_valid({**report, 'checks': report['checks'][:-1]}, manifest))
        altered = json.loads(json.dumps(report))
        altered['checks'][0]['artifacts'][0]['sha256'] = '0' * 64
        rejected(lambda: evidence_valid(altered, manifest))

    record('reject_legacy_unit_and_lifecycle_changes', unit_validation)
    record('reject_stale_or_incomplete_acceptance', evidence_validation)
    with tempfile.TemporaryDirectory(prefix='codex-agent-ui-test-') as directory:
        test_root = Path(directory).resolve()
        old_web = test_root / 'old/frontend/dist'
        release = test_root / 'ui-releases' / ('a' * 64)
        new_web = release / 'frontend/dist'
        for folder in (old_web, new_web):
            folder.mkdir(parents=True)
            (folder / 'index.html').write_text(folder.name)
        unit = test_root / 'codex-agent.service'
        original = ('[Service]\nType=exec\nEnvironmentFile=preserved-pg.env\nExecStart=preserved-binary --web '
                    + str(old_web) + '\nWorkingDirectory=preserved\nTimeoutStopSec=180\n').encode()
        unit.write_bytes(original)
        calls = []

        def parse_test_unit(raw):
            command = next(line for line in raw.splitlines() if line.startswith(b'ExecStart='))
            return Path(command.split(b' --web ', 1)[1].decode()), command

        def healthy(web, timeout=75):
            calls.append(('health', str(web)))
            return {'root': 200, 'health_live': 200, 'health_ready': 200, 'binary_sha256': BINARY_SHA}

        with patch(__name__ + '.ROOT', test_root), patch(__name__ + '.UNIT', unit), \
                patch(__name__ + '.validate_runtime'), patch(__name__ + '.parse_unit', side_effect=parse_test_unit), \
                patch(__name__ + '.release_manifest', return_value={'release_id': release.name}), \
                patch(__name__ + '.fsync_dir'), patch(__name__ + '.system', side_effect=lambda *a: calls.append(a)), \
                patch(__name__ + '.wait_healthy', side_effect=healthy), patch(__name__ + '.REHEARSAL', True):
            def successful_activation():
                result = Transaction(release).activate()
                assert result['rehearsal_deployed'] and not result['deployed']
                assert unit.read_bytes() == original.replace(str(old_web).encode(), str(new_web).encode())
                assert Transaction(release).status()['currently_active']
            record('activate_preserves_backend_env_and_other_unit_bytes', successful_activation)

            def repeat_activation():
                count = calls.count(('restart', SERVICE))
                assert Transaction(release).activate()['reused']
                assert calls.count(('restart', SERVICE)) == count
            record('activate_is_idempotent_without_second_restart', repeat_activation)

            def later_change_guard():
                candidate = unit.read_bytes()
                unit.write_bytes(candidate + b'# another deployment\n')
                rejected(lambda: Transaction(release).rollback())
                assert unit.read_bytes().endswith(b'# another deployment\n')
                unit.write_bytes(candidate)
            record('rollback_refuses_later_unit_change', later_change_guard)

            def successful_rollback():
                result = Transaction(release).rollback()
                assert result['phase'] == 'rolled_back' and not result['rehearsal_deployed']
                assert unit.read_bytes() == original
                count = calls.count(('restart', SERVICE))
                assert Transaction(release).rollback()['reused']
                assert calls.count(('restart', SERVICE)) == count
            record('rollback_restores_previous_pg_unit_idempotently', successful_rollback)

            def startup_failure():
                rejected(lambda: Transaction(release).activate('startup'))
                assert unit.read_bytes() == original
                state = Transaction(release).state
                assert state['phase'] == 'rolled_back' and state['automatic_rollback']
            record('startup_failure_automatically_restores_pg_unit', startup_failure)

            def interrupted_activation():
                transaction = Transaction(release)
                transaction.save('switch_requested')
                atomic_write(unit, transaction.changed())
                rejected(lambda: Transaction(release).activate())
                assert unit.read_bytes() == original and Transaction(release).state['phase'] == 'rolled_back'
            record('interrupted_activation_recovers_to_previous_pg_unit', interrupted_activation)

            def tampered_backup():
                transaction = Transaction(release)
                (transaction.private / 'previous.service').write_bytes(b'changed')
                rejected(lambda: Transaction(release).rollback())
                assert unit.read_bytes() == original
            record('rollback_refuses_tampered_backup', tampered_backup)
    return {'schema': 'codex-agent-ui-tool-simulation-v1', 'status': 'pass',
            'scope': 'Local simulation only; production and rehearsal remain unexecuted', 'checks': results}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action', choices=['stage', 'activate', 'rollback', 'status', 'self-test'])
    parser.add_argument('--release', type=Path)
    parser.add_argument('--incoming', type=Path)
    parser.add_argument('--rehearsal', action='store_true')
    parser.add_argument('--fault', choices=['startup'])
    args = parser.parse_args()
    if args.action == 'self-test':
        print(json.dumps(self_test(), indent=2))
        return
    if args.release is None:
        parser.error('--release is required')
    if args.rehearsal:
        configure_rehearsal()
    if args.action == 'stage':
        if args.incoming is None:
            parser.error('--incoming is required for stage')
        result = stage(args.incoming, args.release)
    else:
        with lock():
            transaction = Transaction(args.release)
            if args.action == 'activate':
                result = transaction.activate(args.fault)
            elif args.action == 'rollback':
                result = transaction.rollback()
            else:
                result = transaction.status()
    print(json.dumps(result, indent=2, sort_keys=True))


if __name__ == '__main__':
    try:
        main()
    except UIReleaseError as error:
        raise SystemExit(str(error)) from None
