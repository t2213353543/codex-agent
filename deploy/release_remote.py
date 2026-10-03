#!/usr/bin/env python3
"""Linux-only, journaled JSON-to-PostgreSQL cutover for codex-agent.

This helper never provisions databases and never reads another project's state.
The local deploy tool stages immutable, verified artifacts and provisions the two
root-only environment files before calling this helper. Command output and legacy
snapshots stay in the release's private directory, never on stdout.
"""
from __future__ import annotations

import argparse
import contextlib
import datetime as dt
import hashlib
import http.cookies
import json
import os
from pathlib import Path
import re
import shlex
import shutil
import socket
import stat
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid

ROOT = Path('/opt/codex-agent')
RELEASES = ROOT / 'releases'
DATA = Path('/var/lib/codex-agent')
UNIT = Path('/etc/systemd/system/codex-agent.service')
RUNTIME_ENV = Path('/etc/codex-agent/production.env')
MIGRATION_ENV = Path('/etc/codex-agent/migration.env')
SERVICE = 'codex-agent.service'
BASE_URL = 'http://127.0.0.1:18879'
LISTEN_HOST = '0.0.0.0'
LISTEN_PORT = 18879
PG_PORT = 25433
REDIS_PORT = 26380
DATABASE_NAME = 'codex_agent'
REDIS_PREFIX = 'codex-agent:production:'
REHEARSAL = False
STARTUP_FAILURE = False
LEGACY_FILES = frozenset({
    'accounts.json', 'groups.json', 'keys.json', 'proxies.json',
    'settings.json', 'stats.json', 'logs.jsonl', 'client-key',
})


class ReleaseError(RuntimeError):
    """Messages must contain neither environment values nor raw child output."""


def configure_rehearsal() -> None:
    """The test switch chooses constants; no user-selected server paths/ports."""
    global ROOT, RELEASES, DATA, UNIT, RUNTIME_ENV, MIGRATION_ENV, SERVICE
    global BASE_URL, LISTEN_HOST, LISTEN_PORT, PG_PORT, REDIS_PORT
    global DATABASE_NAME, REDIS_PREFIX, REHEARSAL
    REHEARSAL = True
    ROOT = Path('/opt/codex-agent-integration/rehearsal')
    RELEASES = ROOT / 'releases'
    DATA = ROOT / 'data'
    SERVICE = 'codex-agent-integration-rehearsal.service'
    UNIT = Path('/etc/systemd/system') / SERVICE
    RUNTIME_ENV = ROOT / 'config/production.env'
    MIGRATION_ENV = ROOT / 'config/migration.env'
    LISTEN_HOST, LISTEN_PORT = '127.0.0.1', 28883
    BASE_URL = 'http://127.0.0.1:28883'
    PG_PORT, REDIS_PORT = 25432, 26379
    DATABASE_NAME = 'codex_agent_rehearsal'
    REDIS_PREFIX = 'codex-agent:integration:rehearsal:'


def prepare_rehearsal(args) -> None:
    """Prepare only synthetic legacy files; DB roles are provisioned separately."""
    if not REHEARSAL:
        raise ReleaseError('Rehearsal preparation cannot run in production mode')
    source = args.source_release
    if source is None or source.parent != Path('/opt/codex-agent/releases') or not re.fullmatch(
            r'[0-9a-f]{64}', source.name):
        raise ReleaseError('Rehearsal source must be a staged production artifact directory')
    if source.resolve(strict=True) != source:
        raise ReleaseError('Rehearsal source must not contain symlinks')
    if args.release.parent != RELEASES or args.release.name != source.name:
        raise ReleaseError('Rehearsal destination must preserve the staged artifact release ID')
    if ROOT.exists():
        marker = ROOT / '.synthetic-rehearsal'
        regular_file(marker)
    else:
        if ROOT.parent.resolve(strict=True) != ROOT.parent:
            raise ReleaseError('Integration root must not be a symlink')
        ROOT.mkdir(mode=0o700)
        atomic_write(ROOT / '.synthetic-rehearsal', b'codex-agent-synthetic-rehearsal\n')
    with deployment_lock():
        _prepare_rehearsal_files(args, source)


def _prepare_rehearsal_files(args, source: Path) -> None:
    # Refuse to reset old test data or overwrite a prepared release on rerun.
    if args.release.exists() or DATA.exists() or UNIT.exists():
        raise ReleaseError('Rehearsal already prepared; inspect/reuse it without overwriting state')
    (ROOT / 'config').mkdir(mode=0o700, exist_ok=True)
    load_environment(RUNTIME_ENV)
    load_environment(MIGRATION_ENV)
    regular_file(source / 'release-manifest.json')
    regular_file(Path('/opt/codex-agent/codex-agent'))
    legacy_frontend = Path('/opt/codex-agent/frontend/dist')
    regular_file(legacy_frontend / 'index.html')
    RELEASES.mkdir(mode=0o755, exist_ok=True)
    args.release.mkdir(mode=0o755)
    for relative in ('codex-agent', 'release-manifest.json'):
        regular_file(source / relative)
        shutil.copy2(source / relative, args.release / relative)
    check_tree(source / 'frontend/dist')
    shutil.copytree(source / 'frontend/dist', args.release / 'frontend/dist')
    os.chmod(args.release / 'codex-agent', 0o755)
    shutil.copy2('/opt/codex-agent/codex-agent', ROOT / 'codex-agent')
    os.chmod(ROOT / 'codex-agent', 0o755)
    check_tree(legacy_frontend)
    shutil.copytree(legacy_frontend, ROOT / 'frontend/dist')
    DATA.mkdir(mode=0o700)
    import base64
    def segment(value):
        return base64.urlsafe_b64encode(json.dumps(value).encode()).decode().rstrip('=')
    timestamp = int(time.time())
    token = '.'.join((segment({'alg': 'none', 'typ': 'JWT'}),
                      segment({'sub': 'synthetic-deployment-rehearsal', 'exp': timestamp + 30 * 86400}),
                      'synthetic-never-send-upstream'))
    account = {'name': 'synthetic-deployment-rehearsal', 'access_token': token,
               'refresh_token': 'synthetic-refresh-never-send-upstream',
               'account_id': 'synthetic-workspace', 'expires_at': timestamp + 30 * 86400,
               'enabled': False, 'concurrency': 1, 'weight': 1, 'window_days': 30,
               'created_at': timestamp}
    fixtures = {
        'accounts.json': {'active': account['name'], 'accounts': [account]},
        'groups.json': {'groups': []}, 'keys.json': {'keys': []},
        'proxies.json': {'proxies': []}, 'settings.json': {'admin_api': False},
        'stats.json': {'saved_at': timestamp, 'minutes': {}, 'accounts': {}},
    }
    for name, value in fixtures.items():
        atomic_write(DATA / name, (json.dumps(value, indent=2) + '\n').encode())
    atomic_write(DATA / 'logs.jsonl', b'')
    atomic_write(DATA / 'client-key', ('synthetic_' + uuid.uuid4().hex + uuid.uuid4().hex).encode())
    old_unit = f'''[Unit]
Description=Codex Agent synthetic migration rehearsal
After=network-online.target
[Service]
Type=exec
User=root
WorkingDirectory={ROOT}
ExecStart={ROOT}/codex-agent --data {DATA} --listen 127.0.0.1:28883 --web {ROOT}/frontend/dist
Restart=on-failure
TimeoutStopSec=180
UMask=0077
[Install]
WantedBy=multi-user.target
'''.encode()
    release = Release(args.release)
    release.validate()
    release.migration_environment()
    release.journal('prepare-synthetic-rehearsal', 'before')
    release.install_unit(old_unit, 'install-rehearsal-legacy-unit')
    release.systemctl('start', step='start-rehearsal-legacy')
    release.wait_health(candidate=False)
    release.save(rehearsal_prepared_at=now(),
                 copied_legacy_binary_sha256=digest(ROOT / 'codex-agent'))
    release.journal('prepare-synthetic-rehearsal', 'after')


def now() -> str:
    return dt.datetime.now(dt.timezone.utc).isoformat()


def digest(path: Path) -> str:
    result = hashlib.sha256()
    with path.open('rb') as handle:
        for block in iter(lambda: handle.read(1024 * 1024), b''):
            result.update(block)
    return result.hexdigest()


def sync_directory(path: Path) -> None:
    descriptor = os.open(path, os.O_RDONLY | os.O_DIRECTORY)
    try:
        os.fsync(descriptor)
    finally:
        os.close(descriptor)


def atomic_write(path: Path, raw: bytes, mode: int = 0o600,
                 owner: tuple[int, int] | None = None) -> None:
    if path.is_symlink():
        raise ReleaseError('Refusing a symlink destination')
    temporary = path.with_name('.' + path.name + '.' + uuid.uuid4().hex + '.tmp')
    descriptor = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL, mode)
    try:
        if owner is not None:
            os.fchown(descriptor, *owner)
        with os.fdopen(descriptor, 'wb') as handle:
            handle.write(raw)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary, path)
        sync_directory(path.parent)
    finally:
        if temporary.exists():
            temporary.unlink()


def atomic_copy(source: Path, destination: Path, owner: tuple[int, int]) -> None:
    """Keep large JSONL exports bounded in memory while preserving legacy UID."""
    if destination.is_symlink():
        raise ReleaseError('Refusing a symlink destination')
    temporary = destination.with_name('.' + destination.name + '.' + uuid.uuid4().hex + '.tmp')
    descriptor = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    try:
        with os.fdopen(descriptor, 'wb') as output, source.open('rb') as incoming:
            os.fchown(output.fileno(), *owner)
            shutil.copyfileobj(incoming, output, 1024 * 1024)
            output.flush()
            os.fsync(output.fileno())
        os.replace(temporary, destination)
        sync_directory(destination.parent)
    finally:
        if temporary.exists():
            temporary.unlink()


def regular_file(path: Path) -> None:
    if path.is_symlink() or not path.is_file():
        raise ReleaseError('A required regular file is missing or is a symlink')


def check_tree(path: Path) -> None:
    """Reject links/devices so snapshots cannot escape this project's directory."""
    for directory, subdirs, files in os.walk(path, followlinks=False):
        for name in subdirs + files:
            item = Path(directory) / name
            mode = item.lstat().st_mode
            if not (stat.S_ISREG(mode) or stat.S_ISDIR(mode)):
                raise ReleaseError('Project tree contains a symlink or special file')


def scoped_data_directory() -> Path:
    """Allow only systemd's specific top-level StateDirectory indirection."""
    resolved = DATA.resolve(strict=True)
    if DATA.is_symlink():
        expected = Path('/var/lib/private/codex-agent')
        if REHEARSAL or DATA != Path('/var/lib/codex-agent') or resolved != expected:
            raise ReleaseError('State directory symlink is outside the systemd project mapping')
        private_parent = expected.parent
        if DATA.lstat().st_uid != 0 or private_parent.resolve(strict=True) != private_parent:
            raise ReleaseError('State directory mapping must be managed by root')
        parent_meta = private_parent.stat()
        if parent_meta.st_uid != 0 or stat.S_IMODE(parent_meta.st_mode) & 0o077:
            raise ReleaseError('Systemd private state parent must be root-owned and private')
        metadata = resolved.stat()
        allowed_uids = {65534}  # systemd id-mapped StateDirectory host ownership.
        try:
            import pwd
            allowed_uids.add(pwd.getpwnam('codex-agent').pw_uid)
        except KeyError:
            pass
        if metadata.st_uid not in allowed_uids and not 61184 <= metadata.st_uid <= 65519:
            raise ReleaseError('State directory owner is not the scoped service identity')
    elif resolved != DATA:
        raise ReleaseError('State directory has an unexpected ancestor symlink')
    metadata = resolved.stat()
    if not stat.S_ISDIR(metadata.st_mode) or stat.S_IMODE(metadata.st_mode) & 0o077:
        raise ReleaseError('State directory must be private and contain regular files only')
    check_tree(resolved)
    return resolved


METRIC_FIELDS = ('Requests', 'OK', 'Errors', 'Tokens', 'Input', 'Output',
                 'Cached', 'Latency', 'LatencyN')


def metric_counts(value: dict | None) -> dict[str, int]:
    value = value or {}
    counts = {key: value.get(key, 0) for key in METRIC_FIELDS}
    if any(type(number) is not int or number < 0 for number in counts.values()):
        raise ReleaseError('Legacy statistics contain invalid metric counts')
    if counts['Requests'] != counts['OK'] + counts['Errors'] or counts['Tokens'] != counts['Input'] + counts['Output']:
        raise ReleaseError('Legacy statistics contain inconsistent metric totals')
    return counts


def merged_legacy_statistics(snapshot: dict, logs: Path, id_names: dict[str, str]) -> dict:
    """Independent readback oracle for the documented event/snapshot boundary."""
    aggregates: dict[tuple[str, str, str], dict[str, int]] = {}
    if logs.exists():
        with logs.open(encoding='utf-8') as handle:
            for line in handle:
                if not line.strip():
                    continue
                record = json.loads(line)
                timestamp = int(record['time'])
                input_tokens = record.get('input_tokens', 0)
                output_tokens = record.get('output_tokens', 0)
                latency = record.get('latency_ms', 0)
                counts = metric_counts({
                    'Requests': 1, 'OK': int(record['status'] == 'ok'),
                    'Errors': int(record['status'] != 'ok'),
                    'Tokens': input_tokens + output_tokens,
                    'Input': input_tokens, 'Output': output_tokens,
                    'Cached': record.get('cached_tokens', 0),
                    'Latency': latency, 'LatencyN': int(latency > 0),
                })
                keys = [('minutes', '', str(timestamp // 60 * 60))]
                name = id_names.get(record.get('account_id', ''), record.get('account', ''))
                if name:
                    keys.append(('accounts', name, str(timestamp // 86400 * 86400)))
                for key in keys:
                    target = aggregates.setdefault(key, metric_counts(None))
                    for field in METRIC_FIELDS:
                        target[field] += counts[field]
    for section in ('minutes', 'accounts'):
        groups = {'': snapshot.get(section) or {}} if section == 'minutes' else snapshot.get(section) or {}
        for owner, buckets in groups.items():
            for bucket, raw in (buckets or {}).items():
                if raw is None:
                    continue
                key = (section, owner, str(bucket))
                old = metric_counts(raw)
                events = aggregates.get(key, metric_counts(None))
                lower = any(old[field] < events[field] for field in METRIC_FIELDS)
                higher = any(old[field] > events[field] for field in METRIC_FIELDS)
                if lower and higher:
                    raise ReleaseError('Legacy snapshot and logs overlap inconsistently')
                # Whole-vector dominance preserves newer logs, or adds exactly
                # the snapshot baseline absent from logs, without double count.
                aggregates[key] = events if lower else old
    return aggregates


def load_environment(path: Path) -> dict[str, str]:
    regular_file(path)
    metadata = path.stat()
    if metadata.st_uid != 0 or stat.S_IMODE(metadata.st_mode) & 0o077:
        raise ReleaseError('Environment files must be owned by root and private')
    values: dict[str, str] = {}
    for line in path.read_text(encoding='utf-8').splitlines():
        stripped = line.strip()
        if not stripped or stripped.startswith('#'):
            continue
        if '=' not in stripped:
            raise ReleaseError('Invalid environment file assignment')
        key, raw = stripped.split('=', 1)
        if not re.fullmatch(r'[A-Z][A-Z0-9_]*', key) or key in values:
            raise ReleaseError('Invalid or duplicate environment variable name')
        # The provisioning tool writes simple single-line assignments. No shell
        # evaluation, interpolation, export statements, or multiline syntax.
        parsed = shlex.split(raw, comments=False, posix=True)
        if len(parsed) > 1 or '\x00' in raw:
            raise ReleaseError('Environment values must be single-line literals')
        values[key] = parsed[0] if parsed else ''
    forbidden = {'LD_PRELOAD', 'LD_LIBRARY_PATH', 'PYTHONPATH', 'PYTHONHOME'}
    if forbidden.intersection(values):
        raise ReleaseError('Loader override variables are not accepted')
    return values


@contextlib.contextmanager
def deployment_lock():
    import fcntl
    if ROOT.resolve(strict=True) != ROOT:
        raise ReleaseError('Application root must not be a symlink')
    descriptor = os.open(ROOT / '.deployment.lock',
                         os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW, 0o600)
    try:
        try:
            fcntl.flock(descriptor, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError as exc:
            raise ReleaseError('Another codex-agent deployment operation is active') from exc
        yield
    finally:
        os.close(descriptor)


class Release:
    def __init__(self, path: Path):
        if path.parent != RELEASES or not re.fullmatch(r'[0-9a-f]{64}', path.name):
            raise ReleaseError('Release must be an absolute hash directory under /opt/codex-agent/releases')
        if path.resolve(strict=True) != path or RELEASES.resolve(strict=True) != RELEASES:
            raise ReleaseError('Release paths must not contain symlinks')
        self.path = path
        self.rehearsal = REHEARSAL
        self.binary = path / 'codex-agent'
        self.private = path / 'private'
        if self.private.is_symlink():
            raise ReleaseError('Private release directory must not be a symlink')
        self.private.mkdir(mode=0o700, exist_ok=True)
        os.chmod(self.private, 0o700)
        self.state_path = self.private / 'state.json'
        self.journal_path = self.private / 'journal.jsonl'
        if self.state_path.exists():
            regular_file(self.state_path)
            self.state = json.loads(self.state_path.read_text())
            if self.state.get('release_id') != path.name:
                raise ReleaseError('Saved state belongs to another release')
            if self.state.get('mode', 'production') != ('rehearsal' if self.rehearsal else 'production'):
                raise ReleaseError('Saved state belongs to another deployment mode')
        else:
            self.state = {
                'state_version': 1, 'release_id': path.name, 'phase': 'staged',
                'tests_passed': False, 'migrated': False, 'deployed': False,
                'candidate_start_requested': False, 'created_at': now(),
            }
        self.state['mode'] = 'rehearsal' if self.rehearsal else 'production'
        self.state['synthetic_only'] = self.rehearsal
        if self.rehearsal:
            self.state['production_dynamic_user_verified'] = False
            self.state['migrated'] = self.state['deployed'] = False
        self.manifest: dict = {}
        self.environment: dict[str, str] = {}
        self.changed_production = False

    def save(self, **changes) -> None:
        if self.rehearsal:
            for key in ('migrated', 'deployed'):
                if key in changes:
                    changes['rehearsal_' + key] = changes.pop(key)
            changes['migrated'] = changes['deployed'] = False
        self.state.update(changes)
        self.state['updated_at'] = now()
        atomic_write(self.state_path,
                     (json.dumps(self.state, indent=2) + '\n').encode())

    def journal(self, step: str, event: str, **details) -> None:
        entry = {'at': now(), 'mode': self.state['mode'], 'step': step, 'event': event, **details}
        descriptor = os.open(self.journal_path,
                             os.O_WRONLY | os.O_CREAT | os.O_APPEND | os.O_NOFOLLOW,
                             0o600)
        with os.fdopen(descriptor, 'ab') as handle:
            handle.write((json.dumps(entry) + '\n').encode())
            handle.flush()
            os.fsync(handle.fileno())
        sync_directory(self.private)

    def command(self, step: str, arguments: list[str], *, environment=None,
                timeout=180, mutate=True) -> bytes:
        if mutate:
            self.journal(step, 'before')
        output = self.private / (step + '-' + uuid.uuid4().hex + '.log')
        descriptor = os.open(output, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        try:
            with os.fdopen(descriptor, 'wb') as handle:
                result = subprocess.run(arguments, cwd=self.path, env=environment,
                                        stdout=handle, stderr=subprocess.STDOUT,
                                        timeout=timeout, check=False)
                handle.flush()
                os.fsync(handle.fileno())
        except subprocess.TimeoutExpired as exc:
            self.journal(step, 'failed', reason='timeout', output=output.name)
            raise ReleaseError(f'Command timed out at {step}; private evidence retained') from exc
        if result.returncode:
            self.journal(step, 'failed', exit_code=result.returncode, output=output.name)
            raise ReleaseError(f'Command failed at {step}; private evidence retained')
        self.journal(step, 'after', exit_code=0, output=output.name)
        return output.read_bytes()

    def systemctl(self, *arguments: str, step: str | None = None, mutate=True) -> bytes:
        return self.command(step or ('systemctl-' + arguments[0]),
                            ['systemctl', *arguments, SERVICE], timeout=210,
                            mutate=mutate)

    def properties(self) -> dict[str, str]:
        raw = self.systemctl('show', '-p', 'ActiveState', '-p', 'SubState', '-p',
                             'MainPID', '-p', 'DropInPaths', '-p', 'Result', '-p',
                             'ExecMainStatus', mutate=False).decode()
        return dict(line.split('=', 1) for line in raw.splitlines() if '=' in line)

    def validate(self) -> None:
        regular_file(self.path / 'release-manifest.json')
        self.manifest = json.loads((self.path / 'release-manifest.json').read_text())
        if self.manifest.get('release_id') != self.path.name:
            raise ReleaseError('Manifest release ID does not match release directory')
        if not self.rehearsal and self.manifest.get('tests_passed') is not True:
            raise ReleaseError('Candidate acceptance gate is not passed')
        if not self.rehearsal and not re.fullmatch(r'[0-9a-f]{64}', self.manifest.get('acceptance_report_sha256') or ''):
            raise ReleaseError('Acceptance report hash is missing')
        regular_file(self.binary)
        if not os.access(self.binary, os.X_OK):
            raise ReleaseError('Candidate binary is not executable')
        if digest(self.binary) != self.manifest.get('binary_sha256'):
            raise ReleaseError('Candidate binary hash differs from manifest')
        regular_file(self.path / 'frontend/dist/index.html')
        check_tree(self.path / 'frontend/dist')
        files = self.manifest.get('frontend_files')
        if files is None and isinstance(self.manifest.get('frontend'), dict):
            files = {'frontend/dist/' + path: value
                     for path, value in self.manifest['frontend'].items()}
        if not isinstance(files, dict) or not files:
            raise ReleaseError('Manifest frontend_files must map relative paths to SHA256 values')
        actual = {str(item.relative_to(self.path)).replace(os.sep, '/'): digest(item)
                  for item in (self.path / 'frontend/dist').rglob('*') if item.is_file()}
        if actual != files:
            raise ReleaseError('Frontend files differ from the immutable manifest')
        runtime = load_environment(RUNTIME_ENV)
        for key in ('DATABASE_URL', 'REDIS_URL', 'CREDENTIAL_ENCRYPTION_KEY',
                    'CREDENTIAL_ENCRYPTION_KEY_ID'):
            if not runtime.get(key):
                raise ReleaseError('Required production storage configuration is missing')
        database = urllib.parse.urlsplit(runtime['DATABASE_URL'])
        redis = urllib.parse.urlsplit(runtime['REDIS_URL'])
        if (database.hostname not in ('127.0.0.1', 'localhost') or database.port != PG_PORT
                or database.path.lstrip('/') != DATABASE_NAME):
            raise ReleaseError('Production PG target is not the dedicated codex-agent instance')
        if redis.hostname not in ('127.0.0.1', 'localhost') or redis.port != REDIS_PORT:
            raise ReleaseError('Production Redis target is not the dedicated codex-agent instance')
        if runtime.get('REDIS_KEY_PREFIX', runtime.get('REDIS_NAMESPACE', '')) != REDIS_PREFIX:
            raise ReleaseError('Redis key namespace is outside the selected environment')
        self.environment = {'PATH': '/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin',
                            'LANG': 'C.UTF-8', **runtime}
        data_directory = scoped_data_directory()
        data_owner = data_directory.stat()
        self.save(tests_passed=self.manifest.get('tests_passed') is True,
                  state_directory={'path': str(DATA), 'resolved_path': str(data_directory),
                                   'uid': data_owner.st_uid, 'gid': data_owner.st_gid},
                  binary_sha256=self.manifest['binary_sha256'],
                  frontend_manifest_sha256=hashlib.sha256(
                      json.dumps(files, sort_keys=True).encode()).hexdigest())

    def migration_environment(self) -> dict[str, str]:
        override = load_environment(MIGRATION_ENV)
        if set(override) != {'DATABASE_URL'}:
            raise ReleaseError('Migration environment must contain DATABASE_URL only')
        runtime = urllib.parse.urlsplit(self.environment['DATABASE_URL'])
        migration = urllib.parse.urlsplit(override['DATABASE_URL'])
        if (runtime.hostname, runtime.port, runtime.path) != (
                migration.hostname, migration.port, migration.path):
            raise ReleaseError('Migrator and runtime must target the same dedicated database')
        if runtime.username == migration.username:
            raise ReleaseError('Migrator and runtime database roles must be separate')
        return {**self.environment, **override}

    def storage(self, step: str, *arguments: str, migrate=False) -> bytes:
        return self.command(step, [str(self.binary), *arguments],
                            environment=self.migration_environment() if migrate else self.environment,
                            timeout=1800)

    def migration_summary(self, raw: bytes, *, dry_run: bool) -> dict:
        result = json.loads(raw)
        fields = ('accounts', 'groups', 'proxies', 'keys', 'account_groups',
                  'account_proxies', 'logs', 'log_requests', 'log_tokens',
                  'stats_requests', 'stats_tokens')
        if result.get('dry_run') is not dry_run or any(
                type(result.get(field)) is not int or result[field] < 0 for field in fields):
            raise ReleaseError('Migration CLI did not return the expected validated summary')
        for field in ('manifest', 'metadata_hash'):
            if not re.fullmatch(r'[0-9a-f]{64}', result.get(field, '')):
                raise ReleaseError('Migration CLI summary lacks source/metadata fingerprints')
        return {field: result[field] for field in (*fields, 'manifest', 'metadata_hash')}

    def preflight(self) -> None:
        self.validate()
        if self.state.get('rehearsal_deployed' if self.rehearsal else 'deployed'):
            self.verify_candidate()
            return
        if self.state['phase'] not in ('staged', 'preflight_passed', 'rolled_back'):
            raise ReleaseError('Incomplete cutover requires explicit rollback before preflight')
        properties = self.properties()
        if properties.get('DropInPaths'):
            raise ReleaseError('Existing service drop-ins require an explicit reviewed migration plan')
        regular_file(UNIT)
        old_unit = UNIT.read_text()
        if str(RUNTIME_ENV) in old_unit:
            raise ReleaseError('This state machine requires the old JSON service; PG upgrades need a schema rollback plan')
        if str(ROOT / 'codex-agent') not in old_unit or '--data ' + str(DATA) not in old_unit:
            raise ReleaseError('Old service does not match the scoped legacy service')
        self.storage('preflight-migrate', 'migrate', migrate=True)
        summary = self.migration_summary(self.storage(
            'preflight-dry-run', 'import-legacy', '--source', str(DATA), '--dry-run'), dry_run=True)
        self.save(phase='preflight_passed', preflight_at=now(), preflight_summary=summary)

    def assert_stopped(self) -> None:
        properties = self.properties()
        if properties.get('ActiveState') not in ('inactive', 'failed') or properties.get('MainPID') != '0':
            raise ReleaseError('Service has not completely stopped')
        for entry in Path('/proc').iterdir():
            if not entry.name.isdigit():
                continue
            try:
                executable = os.readlink(entry / 'exe').removesuffix(' (deleted)')
            except (FileNotFoundError, PermissionError, ProcessLookupError):
                continue
            if executable == str(ROOT / 'codex-agent') or (
                    executable.startswith(str(RELEASES) + '/') and executable.endswith('/codex-agent')):
                raise ReleaseError('A codex-agent application process still exists')
        with socket.socket() as listener:
            listener.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
            try:
                listener.bind(('127.0.0.1', LISTEN_PORT))
            except OSError as exc:
                raise ReleaseError('The scoped application port remains occupied after stopping') from exc

    def stop(self, label: str) -> None:
        self.systemctl('stop', step=label)
        self.assert_stopped()
        if self.properties().get('Result') in ('timeout', 'signal', 'core-dump', 'watchdog'):
            raise ReleaseError('Service did not exit gracefully; manual persistence recovery is required')

    def backup(self) -> None:
        self.journal('legacy-snapshot', 'before')
        snapshot = self.private / ('legacy-' + uuid.uuid4().hex)
        snapshot.mkdir(mode=0o700)
        regular_file(UNIT)
        shutil.copy2(UNIT, snapshot / 'codex-agent.service')
        shutil.copytree(scoped_data_directory(), snapshot / 'data')
        hashes = {}
        for item in sorted(snapshot.rglob('*')):
            if item.is_file():
                os.chmod(item, 0o600)
                with item.open('rb') as handle:
                    os.fsync(handle.fileno())
                hashes[str(item.relative_to(snapshot))] = digest(item)
        atomic_write(snapshot / 'snapshot-manifest.json',
                     (json.dumps(hashes, sort_keys=True, indent=2) + '\n').encode())
        sync_directory(snapshot)
        self.save(phase='backup_complete', snapshot_directory=str(snapshot),
                  snapshot_manifest_sha256=digest(snapshot / 'snapshot-manifest.json'),
                  legacy_binary_sha256=digest(ROOT / 'codex-agent'),
                  legacy_frontend_index_sha256=digest(ROOT / 'frontend/dist/index.html'))
        self.journal('legacy-snapshot', 'after', snapshot=snapshot.name)

    def candidate_unit(self) -> bytes:
        identity = ('User=root\nDynamicUser=no' if self.rehearsal else
                    'DynamicUser=yes\nStateDirectory=codex-agent\nStateDirectoryMode=0700')
        executable = str(self.binary) + ('.intentional-missing' if STARTUP_FAILURE else '')
        return f'''[Unit]
Description=Codex Agent management and gateway
After=network-online.target
Wants=network-online.target

[Service]
Type=exec
{identity}
WorkingDirectory={self.path}
EnvironmentFile={RUNTIME_ENV}
ExecStart={executable} --listen {LISTEN_HOST}:{LISTEN_PORT} --web {self.path}/frontend/dist
Restart=on-failure
RestartSec=3
TimeoutStartSec=60
TimeoutStopSec=180
KillMode=control-group
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
UMask=0077
MemoryMax=512M
CPUQuota=100%
TasksMax=128
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
'''.encode()

    def install_unit(self, raw: bytes, label: str) -> None:
        self.journal(label, 'before')
        atomic_write(UNIT, raw, 0o644)
        self.command(label + '-reload', ['systemctl', 'daemon-reload'])
        self.journal(label, 'after')

    def http(self, path: str, *, method='GET', payload=None, headers=None) -> tuple[int, bytes, object]:
        data = None if payload is None else json.dumps(payload).encode()
        request = urllib.request.Request(BASE_URL + path, data=data, method=method,
                                         headers={'Content-Type': 'application/json', **(headers or {})})
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
        try:
            with opener.open(request, timeout=4) as response:
                return response.status, response.read(4 * 1024 * 1024), response.headers
        except urllib.error.HTTPError as response:
            return response.code, response.read(65536), response.headers

    def wait_health(self, *, candidate: bool) -> dict:
        deadline = time.monotonic() + 45
        last = {}
        while time.monotonic() < deadline:
            try:
                paths = ('/health/live', '/health/ready', '/') if candidate else ('/health', '/')
                last = {path: self.http(path)[0] for path in paths}
                if all(status == 200 for status in last.values()):
                    return {'at': now(), 'status_codes': last}
            except (OSError, urllib.error.URLError):
                pass
            time.sleep(1)
        self.save(last_health={'at': now(), 'status_codes': last})
        raise ReleaseError('Service health or readiness did not become available')

    def verify_candidate(self) -> None:
        health = self.wait_candidate_ready()
        root_code, root_body, _ = self.http('/')
        if root_code != 200 or hashlib.sha256(root_body).hexdigest() != digest(self.path / 'frontend/dist/index.html'):
            raise ReleaseError('Served root page does not match candidate frontend')
        if self.http('/api/accounts')[0] != 401 or self.http('/api/session')[0] != 401:
            raise ReleaseError('Anonymous management authentication check failed')
        key = self.environment.get('ADMIN_KEY', '').strip()
        if not key and (DATA / 'client-key').is_file():
            key = (DATA / 'client-key').read_text().strip()
        if len(key) < 32:
            raise ReleaseError('Management authentication verification key is unavailable')
        code, _, headers = self.http('/api/session', method='POST', payload={'admin_key': key})
        if code != 200:
            raise ReleaseError('Management session creation check failed')
        cookie = http.cookies.SimpleCookie()
        for value in headers.get_all('Set-Cookie', []):
            cookie.load(value)
        if not cookie or not all(item['httponly'] and item['samesite'].lower() in ('lax', 'strict')
                                 for item in cookie.values()):
            raise ReleaseError('Management session cookie security flags are missing')
        if self.environment.get('ADMIN_COOKIE_SECURE') != 'false' and not all(
                item['secure'] for item in cookie.values()):
            raise ReleaseError('Management session Secure cookie flag is missing')
        cookie_header = '; '.join(item.key + '=' + item.value for item in cookie.values())
        authenticated = {'Cookie': cookie_header}
        try:
            if self.http('/api/accounts?limit=1', headers=authenticated)[0] != 200:
                raise ReleaseError('Authenticated account list check failed')
            if self.http('/api/overview', headers=authenticated)[0] != 200:
                raise ReleaseError('Authenticated overview check failed')
            if self.http('/api/logs?page_size=1', headers=authenticated)[0] != 200:
                raise ReleaseError('Authenticated log list check failed')
        finally:
            logout = self.http('/api/session', method='DELETE', headers=authenticated)[0]
        if logout not in (200, 204) or self.http('/api/accounts', headers=authenticated)[0] != 401:
            raise ReleaseError('Management session revocation check failed')
        self.save(last_health=health, auth_verified_at=now(), runtime_binary_sha256=self.manifest['binary_sha256'])
        self.journal('online-read-only-verification', 'after', status='passed')

    def wait_candidate_ready(self) -> dict:
        """exec completed != app initialized; bind readiness to a stable PID."""
        deadline = time.monotonic() + 45
        last = {}
        paths = ('/health/live', '/health/ready', '/')
        while time.monotonic() < deadline:
            properties = self.properties()
            pid = properties.get('MainPID', '0')
            observation = {'at': now(), 'service': properties,
                           'binary_matches': False, 'status_codes': {}}
            if properties.get('ActiveState') in ('failed', 'inactive'):
                self.save(last_startup_observation=observation)
                raise ReleaseError('Candidate service exited before readiness')
            if properties.get('SubState') == 'auto-restart':
                self.save(last_startup_observation=observation)
                raise ReleaseError('Candidate service failed during initialization')
            try:
                if properties.get('ActiveState') == 'active' and pid.isdigit() and pid != '0':
                    running_sha = digest(Path('/proc') / pid / 'exe')
                    observation['runtime_binary_sha256'] = running_sha
                    observation['binary_matches'] = running_sha == self.manifest['binary_sha256']
                    if observation['binary_matches']:
                        observation['status_codes'] = {path: self.http(path)[0] for path in paths}
                        if all(status == 200 for status in observation['status_codes'].values()):
                            latest = self.properties()
                            if (latest.get('ActiveState') == 'active' and latest.get('MainPID') == pid
                                    and digest(Path('/proc') / pid / 'exe') == running_sha):
                                self.save(last_startup_observation=observation)
                                return {'at': now(), 'status_codes': observation['status_codes'],
                                        'main_pid': pid, 'runtime_binary_sha256': running_sha}
            except (OSError, urllib.error.URLError):
                # A process can disappear between systemctl and /proc, and a
                # listener can appear after exec. Neither is success evidence.
                pass
            if observation != last:
                self.journal('candidate-startup-observation', 'observed',
                             service=properties, binary_matches=observation['binary_matches'],
                             runtime_binary_sha256=observation.get('runtime_binary_sha256'),
                             status_codes=observation['status_codes'])
            last = observation
            time.sleep(0.2)
        self.save(last_startup_observation=last)
        raise ReleaseError('Candidate did not reach stable process identity and readiness before timeout')

    def cutover(self) -> None:
        self.validate()
        if self.state.get('rehearsal_deployed' if self.rehearsal else 'deployed'):
            self.verify_candidate()
            return
        if self.state['phase'] != 'preflight_passed':
            raise ReleaseError('A passed preflight is required before cutover')
        if self.state.get('snapshot_directory'):
            self.snapshot()
            if (digest(ROOT / 'codex-agent') != self.state.get('legacy_binary_sha256')
                    or digest(ROOT / 'frontend/dist/index.html') != self.state.get('legacy_frontend_index_sha256')):
                raise ReleaseError('Legacy artifacts changed since the prior rollback')
        self.changed_production = True
        self.save(phase='stop_requested', migrated=False, migrated_at=None,
                  deployed=False, deployed_at=None, candidate_start_requested=False,
                  legacy_restart_requested=False, write_freeze_started_at=now())
        self.stop('stop-legacy')
        self.save(phase='legacy_stopped', legacy_stopped_at=now())
        self.backup()
        expected = self.migration_summary(self.storage(
            'final-dry-run', 'import-legacy', '--source', str(DATA), '--dry-run'), dry_run=True)
        self.save(phase='final_dry_run_passed', final_source_summary=expected)
        self.journal('final-import', 'intent')
        self.save(phase='import_requested')
        imported = self.migration_summary(self.storage(
            'final-import', 'import-legacy', '--source', str(DATA)), dry_run=False)
        if expected != imported:
            raise ReleaseError('Final import summary differs from the stopped-source dry run')
        self.verify_import(expected)
        self.save(phase='migrated', migrated=True, migrated_at=now())
        self.install_unit(self.candidate_unit(), 'install-candidate-unit')
        self.save(phase='candidate_start_requested', candidate_start_requested=True)
        self.systemctl('start', step='start-candidate')
        self.verify_candidate()
        self.save(phase='deployed', deployed=True, deployed_at=now())
        self.journal('cutover', 'completed', migrated=not self.rehearsal,
                     deployed=not self.rehearsal, rehearsal_completed=self.rehearsal)

    def verify_import(self, expected: dict) -> None:
        """Read PG back through the shipped exporter before any refresh can run."""
        target = self.private / ('verification-export-' + uuid.uuid4().hex)
        self.storage('verify-export-from-pg', 'export-legacy', '--target', str(target))
        self.validate_export(target)
        actual = self.migration_summary(self.storage(
            'verify-export-summary', 'import-legacy', '--source', str(target), '--dry-run'), dry_run=True)
        fields = ('accounts', 'groups', 'proxies', 'keys', 'account_groups',
                  'account_proxies', 'logs', 'log_requests', 'log_tokens')
        if any(actual[field] != expected[field] for field in fields):
            raise ReleaseError('PG readback counts, relations, or log usage differ from the source')
        source_file = DATA / 'accounts.json'
        source_accounts = json.loads(source_file.read_text()).get('accounts', []) if source_file.exists() else []
        target_accounts = json.loads((target / 'accounts.json').read_text()).get('accounts', [])
        by_name = {item['name']: item for item in target_accounts}
        if len(by_name) != len(source_accounts):
            raise ReleaseError('PG readback identity mapping is not one-to-one')
        mapping = []
        for old in source_accounts:
            new = by_name.get(old['name'])
            if new is None or not new.get('id'):
                raise ReleaseError('PG readback account identity is missing')
            if old.get('id') and old['id'] != new['id']:
                raise ReleaseError('PG readback stable account ID changed')
            for key in ('account_id', 'client_id', 'access_token', 'refresh_token'):
                if old.get(key, '') != new.get(key, ''):
                    raise ReleaseError('PG readback identity or decrypted credential differs')
            if old.get('expires_at', 0) != new.get('expires_at', 0):
                raise ReleaseError('PG readback credential expiry differs')
            for key in ('groups', 'proxies'):
                if sorted(old.get(key) or []) != sorted(new.get(key) or []):
                    raise ReleaseError('PG readback account relationship set differs')
            mapping.append({'source_name_sha256': hashlib.sha256(old['name'].encode()).hexdigest(),
                            'target_id': new['id'], 'expires_at': new.get('expires_at', 0)})
        if len({row['target_id'] for row in mapping}) != len(mapping):
            raise ReleaseError('PG readback target account IDs are not unique')
        # Source logs may be newer than the last snapshot; compare the merged
        # event/baseline result, never require the stale snapshot verbatim.
        source_stats_file = DATA / 'stats.json'
        old_stats = json.loads(source_stats_file.read_text()) if source_stats_file.exists() else {}
        new_stats = json.loads((target / 'stats.json').read_text())
        expected_stats = merged_legacy_statistics(old_stats, DATA / 'logs.jsonl',
                                                  {row['id']: row['name'] for row in target_accounts})
        actual_stats = {}
        for section in ('minutes', 'accounts'):
            groups = {'': new_stats.get(section) or {}} if section == 'minutes' else new_stats.get(section) or {}
            for owner, buckets in groups.items():
                for bucket, counts in (buckets or {}).items():
                    if counts is not None:
                        actual_stats[(section, owner, str(bucket))] = metric_counts(counts)
        # Export computes its own clock cutoff shortly after saved_at. Omit only
        # the exact boundary second; all other retained buckets are exact checks.
        cutoff = int(new_stats['saved_at']) - 92 * 86400
        compared = 0
        outside_export_retention = 0
        for key in expected_stats.keys() | actual_stats.keys():
            if int(key[2]) <= cutoff:
                outside_export_retention += int(key in expected_stats)
                continue
            expected_counts = expected_stats.get(key, metric_counts(None))
            actual_counts = actual_stats.get(key, metric_counts(None))
            if expected_counts != actual_counts:
                raise ReleaseError('PG readback retained usage differs from merged snapshot and logs')
            compared += 1
        result = {'verified_at': now(), 'source_summary': expected, 'target_summary': actual,
                  'identity_mapping': mapping, 'retained_stats_buckets_compared': compared,
                  'expected_stats_buckets_outside_legacy_export_retention': outside_export_retention,
                  'statistics_comparison': 'merged_events_and_snapshot_baseline',
                  'credential_plaintext_roundtrip_verified': True}
        atomic_write(self.private / 'migration-verification.json',
                     (json.dumps(result, indent=2) + '\n').encode())
        self.save(migration_verification_sha256=digest(self.private / 'migration-verification.json'))

    def snapshot(self) -> Path:
        raw = self.state.get('snapshot_directory')
        if not raw:
            raise ReleaseError('No complete legacy snapshot has been recorded')
        path = Path(raw)
        if path.parent != self.private or not path.name.startswith('legacy-') or path.resolve(strict=True) != path:
            raise ReleaseError('Saved snapshot path is outside this release')
        regular_file(path / 'snapshot-manifest.json')
        if digest(path / 'snapshot-manifest.json') != self.state.get('snapshot_manifest_sha256'):
            raise ReleaseError('Snapshot manifest integrity check failed')
        expected = json.loads((path / 'snapshot-manifest.json').read_text())
        actual = {str(item.relative_to(path)): digest(item) for item in path.rglob('*')
                  if item.is_file() and item.name != 'snapshot-manifest.json'}
        if actual != expected:
            raise ReleaseError('Legacy snapshot integrity check failed')
        check_tree(path)
        return path

    def validate_export(self, target: Path) -> None:
        check_tree(target)
        actual = {item.name for item in target.iterdir() if item.is_file()}
        if not LEGACY_FILES.issubset(actual):
            raise ReleaseError('Legacy export is incomplete; service remains stopped')
        for name in LEGACY_FILES:
            regular_file(target / name)
            if name.endswith('.json'):
                json.loads((target / name).read_text())
            elif name == 'client-key' and len((target / name).read_text().strip()) < 32:
                raise ReleaseError('Legacy export management key is invalid')
        with (target / 'logs.jsonl').open() as handle:
            for line in handle:
                if line.strip():
                    json.loads(line)

    def restore_export(self, target: Path) -> None:
        self.validate_export(target)
        data_directory = scoped_data_directory()
        current = data_directory.stat()
        self.journal('restore-latest-legacy-export', 'before')
        for name in sorted(LEGACY_FILES):
            self.journal('replace-legacy-file', 'before', filename=name)
            # Newest credentials only. The frozen snapshot is never copied back
            # after the candidate might have started refreshing credentials.
            atomic_copy(target / name, data_directory / name,
                        owner=(current.st_uid, current.st_gid))
            self.journal('replace-legacy-file', 'after', filename=name)
        self.save(latest_export_applied_at=now())
        self.journal('restore-latest-legacy-export', 'after')

    def rollback(self) -> None:
        if self.state['phase'] == 'rolled_back':
            self.wait_health(candidate=False)
            return
        if self.state['phase'] in ('staged', 'preflight_passed'):
            raise ReleaseError('No production cutover exists to roll back')
        self.save(phase='rollback_requested', deployed=False)
        self.stop('stop-before-rollback')
        started = self.state.get('candidate_start_requested', False)
        # After the legacy start was requested, its files may contain tokens
        # newer than PG. A retry must never export the now-stale PG over them.
        legacy_may_have_restarted = self.state.get('legacy_restart_requested', False)
        if started and not legacy_may_have_restarted:
            # Validation loads current runtime credentials; never print them.
            self.validate()
            target = self.private / ('latest-export-' + uuid.uuid4().hex)
            self.save(phase='latest_export_requested', latest_export_directory=str(target))
            self.storage('export-current-credentials', 'export-legacy', '--target', str(target))
            self.restore_export(target)
        if self.state.get('snapshot_directory'):
            if (digest(ROOT / 'codex-agent') != self.state.get('legacy_binary_sha256')
                    or digest(ROOT / 'frontend/dist/index.html') != self.state.get('legacy_frontend_index_sha256')):
                raise ReleaseError('Preserved legacy binary or frontend was changed; rollback is stopped')
            saved_unit = (self.snapshot() / 'codex-agent.service').read_bytes()
            self.install_unit(saved_unit, 'restore-legacy-unit')
        elif started:
            raise ReleaseError('Candidate may have run but the legacy service snapshot is absent')
        else:
            # A stop failure or snapshot failure happened before the unit could
            # change. Do not overwrite data just to recover an unchanged unit.
            regular_file(UNIT)
            if str(RUNTIME_ENV) in UNIT.read_text():
                raise ReleaseError('Legacy unit backup is absent; refusing to start an unknown unit')
        self.save(phase='legacy_restart_requested', legacy_restart_requested=True)
        self.systemctl('start', step='start-legacy')
        health = self.wait_health(candidate=False)
        self.save(phase='rolled_back', deployed=False, rolled_back_at=now(),
                  rollback_health=health, candidate_start_requested=False)
        self.journal('rollback', 'completed', latest_credentials_exported=started)

    def inspect(self) -> dict:
        return {**self.state, 'service': self.properties(),
                'release_directory': str(self.path), 'private_evidence': str(self.private)}


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action', choices=('inspect', 'preflight', 'cutover', 'rollback', 'prepare-rehearsal'))
    parser.add_argument('--release', type=Path, required=True)
    parser.add_argument('--rehearsal', action='store_true',
                        help='use only the fixed isolated synthetic rehearsal environment')
    parser.add_argument('--fault', choices=('startup',),
                        help='rehearsal only: inject a candidate ExecStart failure')
    parser.add_argument('--source-release', type=Path,
                        help='prepare-rehearsal only: already staged immutable artifacts')
    args = parser.parse_args()
    if args.fault and not args.rehearsal:
        parser.error('--fault is permitted only together with --rehearsal')
    if args.fault and args.action != 'cutover':
        parser.error('--fault applies only to rehearsal cutover')
    if args.action == 'prepare-rehearsal' and not args.rehearsal:
        parser.error('prepare-rehearsal requires --rehearsal')
    if args.source_release is not None and args.action != 'prepare-rehearsal':
        parser.error('--source-release applies only to prepare-rehearsal')
    if args.rehearsal:
        configure_rehearsal()
    global STARTUP_FAILURE
    STARTUP_FAILURE = args.fault == 'startup'
    if sys.platform != 'linux' or os.geteuid() != 0:
        print(json.dumps({'ok': False, 'error': 'Linux root execution is required'}))
        return 2
    release = None
    try:
        if args.action == 'prepare-rehearsal':
            prepare_rehearsal(args)
            print(json.dumps({'ok': True, 'mode': 'rehearsal', 'synthetic_only': True,
                              'migrated': False, 'deployed': False,
                              'release_directory': str(args.release)}, indent=2))
            return 0
        with deployment_lock():
            release = Release(args.release)
            try:
                if args.action != 'inspect':
                    getattr(release, args.action)()
            except Exception as original:
                release.journal(args.action, 'failed', error_type=type(original).__name__)
                release.save(last_failure={'at': now(), 'action': args.action,
                                           'error_type': type(original).__name__})
                if args.action == 'cutover' and release.changed_production:
                    try:
                        release.rollback()
                    except Exception as rollback_error:
                        release.journal('rollback', 'blocked', error_type=type(rollback_error).__name__)
                        release.save(phase='rollback_blocked', deployed=False,
                                     rollback_error_type=type(rollback_error).__name__)
                elif args.action == 'rollback':
                    release.save(phase='rollback_blocked', deployed=False)
                raise
            print(json.dumps({'ok': True, **release.inspect()}, indent=2))
            return 0
    except Exception as error:
        # Child exceptions and connection URLs may contain secrets. Only our
        # fixed error messages and error classes are allowed on stdout.
        message = str(error) if isinstance(error, ReleaseError) else 'Operation failed; private evidence retained'
        result = {'ok': False, 'error': message, 'error_type': type(error).__name__}
        if release is not None:
            result.update({'phase': release.state['phase'],
                           'migrated': release.state.get('migrated', False),
                           'deployed': release.state.get('deployed', False),
                           'private_evidence': str(release.private)})
        print(json.dumps(result, indent=2))
        return 1


if __name__ == '__main__':
    raise SystemExit(main())
