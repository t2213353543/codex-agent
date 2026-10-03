#!/usr/bin/env python3
"""Journaled PostgreSQL-to-PostgreSQL Codex Agent upgrade.

V1 preserves its exact 000-002 schema gate. Explicit V2 applies only the reviewed
additive 003 migration after draining. Neither path imports legacy data or
restores a database. Rollback preserves newly written credentials and requests.
"""
from __future__ import annotations

import argparse
import base64
import contextlib
import datetime as dt
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import re
import stat
import subprocess
import time
import urllib.parse
import urllib.request
import uuid

ROOT = Path('/opt/codex-agent')
UNIT = Path('/etc/systemd/system/codex-agent.service')
RUNTIME_ENV = Path('/etc/codex-agent/production.env')
MIGRATION_ENV = Path('/etc/codex-agent/migration.env')
SERVICE = 'codex-agent.service'
PG_CONTAINER = 'codex-agent-postgres'
BASE_URL = 'http://127.0.0.1:18879'
SHA = re.compile(r'[a-f0-9]{64}')
MIGRATIONS = {
    'migrations/000_core.sql': '19740ff6964922f8f4f61f5976c47c9a5816562ade930b26445a77262c76482c',
    'migrations/001_accounts.sql': '75e030134787e60a248574ccce9d1e916eeb784fff9f3b322fb1dc65a54933cb',
    'migrations/002_metrics.sql': 'fa015a9a64cc69c7428d1f420171774b94f549268782c5f73b12d9b8077d7c68',
}
REQUIRED_CHECKS = frozenset({'go_tests', 'go_vet', 'backend_build', 'frontend_unit',
                            'frontend_build', 'frontend_browser', 'storage', 'schema_unchanged'})
V2_SCHEMA = 'codex-agent-pg-upgrade-v2'
TARGET_MIGRATIONS = {**MIGRATIONS,
    'migrations/003_management_cleanup.sql': '31ca0e1e58bb846c9691bd505718a1c13ef3aba03f9abf157034317e85cde56e'}
V2_CHECKS = frozenset({'go_tests', 'go_vet', 'backend_build', 'storage', 'frontend_preserved',
    'frontend_compatibility', 'additive_migration', 'rollback_compatibility'})
MIGRATION_CHECKS = frozenset({'migration_apply', 'idempotent', 'checksum_guard', 'credential_decryption',
    'data_preserved', 'runtime_permissions', 'owner_verified', 'failure_atomicity'})
ROLLBACK_CHECKS = frozenset({'old_binary_start', 'read_write', 'credential_decryption', 'data_preserved'})
V2_EVIDENCE = {'migration_evidence_sha256': 'evidence/additive-migration.json',
    'rollback_evidence_sha256': 'evidence/rollback-compatibility.json',
    'frontend_evidence_sha256': 'evidence/production-frontend.json',
    'source_manifest_sha256': 'source-manifest.json'}


class UpgradeError(RuntimeError):
    """Only fixed messages, never raw child output or credential values."""


def now():
    return dt.datetime.now(dt.timezone.utc).isoformat()


def sha(path):
    result = hashlib.sha256()
    with Path(path).open('rb') as handle:
        while block := handle.read(1024 * 1024):
            result.update(block)
    return result.hexdigest()


def bytes_sha(value):
    return hashlib.sha256(value).hexdigest()


def regular(path):
    path = Path(path)
    if path.is_symlink() or not stat.S_ISREG(path.stat().st_mode):
        raise UpgradeError('Expected a regular file without symlinks')
    if path.resolve(strict=True) != path:
        raise UpgradeError('File has a symlink path component')
    return path


def directory(path):
    path = Path(path)
    if not path.is_dir() or path.resolve(strict=True) != path:
        raise UpgradeError('Expected a directory without symlink components')
    return path


def scoped_release(path):
    path = Path(path)
    if not path.is_absolute() or path.parent != ROOT / 'releases' or not SHA.fullmatch(path.name):
        raise UpgradeError('Release must be a hash directory inside /opt/codex-agent/releases')
    return path


def relative(value):
    if not isinstance(value, str):
        raise UpgradeError('Artifact path must be a string')
    path = PurePosixPath(value)
    if (not value or path.is_absolute() or '..' in path.parts or '\\' in value
            or str(path) != value or any(ord(c) < 32 for c in value)):
        raise UpgradeError('Invalid relative artifact path')
    return value


def file_map(folder):
    directory(folder)
    files = {}
    for path in sorted(folder.rglob('*')):
        if path.is_symlink():
            raise UpgradeError('Artifact tree contains a symlink')
        if path.is_file():
            files[path.relative_to(folder).as_posix()] = sha(regular(path))
        elif not path.is_dir():
            raise UpgradeError('Artifact tree contains a non-regular file')
    return files


def fsync_directory(path):
    fd = os.open(path, os.O_RDONLY | getattr(os, 'O_DIRECTORY', 0))
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def atomic_write(path, data, mode=0o600):
    path = Path(path)
    directory(path.parent)
    if path.exists() or path.is_symlink():
        regular(path)
    temporary = path.with_name('.' + path.name + '.' + uuid.uuid4().hex)
    fd = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL, mode)
    try:
        with os.fdopen(fd, 'wb') as handle:
            handle.write(data)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary, path)
        fsync_directory(path.parent)
    finally:
        if temporary.exists():
            temporary.unlink()


def write_json(path, value):
    atomic_write(path, (json.dumps(value, indent=2, sort_keys=True) + '\n').encode())


@contextlib.contextmanager
def lock():
    directory(ROOT)
    fd = os.open(ROOT / '.deployment.lock', os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW, 0o600)
    with os.fdopen(fd, 'w') as handle:
        import fcntl
        fcntl.flock(handle, fcntl.LOCK_EX)
        yield


def command(arguments, timeout=240, **kwargs):
    result = subprocess.run(arguments, capture_output=True, timeout=timeout, **kwargs)
    if result.returncode:
        raise UpgradeError('Required local server command failed')
    return result.stdout


def system(*args):
    return command(['systemctl', *args]).decode().strip()


def properties():
    raw = system('show', SERVICE, '-p', 'ActiveState', '-p', 'SubState', '-p', 'MainPID',
                 '-p', 'FragmentPath', '-p', 'DropInPaths', '-p', 'Result', '-p', 'ControlGroup')
    result = dict(line.split('=', 1) for line in raw.splitlines() if '=' in line)
    if result.get('FragmentPath') != str(UNIT) or result.get('DropInPaths'):
        raise UpgradeError('Unexpected systemd fragment or unreviewed drop-ins')
    return result


def parse_unit(raw):
    """Parse only the known PG service. This function does not touch disk."""
    text = raw.decode('utf-8')
    if '\\\n' in text or '\r' in text or '\x00' in text:
        raise UpgradeError('Unsupported unit formatting')
    section, directives = '', {}
    for line in text.splitlines():
        if line.startswith('[') and line.endswith(']'):
            section = line
        elif section == '[Service]' and line and not line.startswith(('#', ';')) and '=' in line:
            key, value = line.split('=', 1)
            directives.setdefault(key, []).append(value)
    if (directives.get('Type') != ['exec']
            or directives.get('EnvironmentFile') != [str(RUNTIME_ENV)]
            or directives.get('DynamicUser') != ['yes']):
        raise UpgradeError('Expected the existing DynamicUser PostgreSQL service')
    if any(key in directives for key in ('ExecStartPre', 'ExecStartPost', 'ExecStop', 'ExecStopPost')):
        raise UpgradeError('Unexpected service lifecycle command')
    if any(re.search(r'(?:DATABASE_URL|REDIS_URL|CREDENTIAL_ENCRYPTION_KEY|REDIS_NAMESPACE|REDIS_KEY_PREFIX)=', value)
           for value in directives.get('Environment', [])):
        raise UpgradeError('Storage configuration must come from the fixed runtime environment')
    starts = directives.get('ExecStart', [])
    if len(starts) != 1:
        raise UpgradeError('Expected exactly one ExecStart')
    prefix = re.escape(str(ROOT))
    match = re.fullmatch(prefix + r'/releases/([a-f0-9]{64})/codex-agent --listen 0\.0\.0\.0:18879 --web ('
                         + prefix + r'/(?:ui-releases|releases)/[a-f0-9]{64}/frontend/dist)', starts[0])
    if not match:
        raise UpgradeError('Unexpected backend, listen address or frontend arguments')
    token = ('ExecStart=' + starts[0]).encode()
    if raw.count(token) != 1:
        raise UpgradeError('Ambiguous ExecStart directive')
    return {'binary': ROOT / 'releases' / match[1] / 'codex-agent',
            'web': Path(match[2]), 'token': token}


def plan_unit(original, release):
    scoped_release(release)
    parsed = parse_unit(original)
    replacement = ('ExecStart=' + str(release / 'codex-agent')
                   + ' --listen 0.0.0.0:18879 --web ' + str(release / 'frontend/dist')).encode()
    changed = original.replace(parsed['token'], replacement, 1)
    parse_unit(changed)
    return changed


def rollback_plan(current, original, changed):
    """Never overwrite a later operator's unit; restore the complete previous unit."""
    parse_unit(original)
    parse_unit(changed)
    if current not in (original, changed):
        raise UpgradeError('Another deployment changed the unit; rollback refused')
    return original


def environment_facts():
    path = regular(RUNTIME_ENV)
    if path.stat().st_mode & 0o077:
        raise UpgradeError('Runtime secrets file must be private')
    values = {}
    for line in path.read_text().splitlines():
        if line and not line.startswith('#'):
            if '=' not in line:
                raise UpgradeError('Unsupported runtime environment syntax')
            key, value = line.split('=', 1)
            if key in values:
                raise UpgradeError('Duplicate runtime environment variable')
            values[key] = value
    pg = urllib.parse.urlsplit(values.get('DATABASE_URL', ''))
    redis = urllib.parse.urlsplit(values.get('REDIS_URL', ''))
    if (pg.scheme not in ('postgres', 'postgresql') or pg.hostname != '127.0.0.1'
            or pg.port != 25433 or pg.path != '/codex_agent' or pg.username != 'codex_agent_runtime'):
        raise UpgradeError('Runtime PostgreSQL target differs from the dedicated project instance')
    if (redis.scheme != 'redis' or redis.hostname != '127.0.0.1' or redis.port != 26380
            or redis.username != 'codex_agent_runtime' or redis.path not in ('', '/0')):
        raise UpgradeError('Runtime Redis target differs from the dedicated project instance')
    if values.get('REDIS_KEY_PREFIX', values.get('REDIS_NAMESPACE')) != 'codex-agent:production:':
        raise UpgradeError('Unexpected production Redis namespace')
    if any(values.get(key, 'codex-agent:production:') != 'codex-agent:production:'
           for key in ('REDIS_KEY_PREFIX', 'REDIS_NAMESPACE')):
        raise UpgradeError('Conflicting production Redis namespace')
    if not values.get('CREDENTIAL_ENCRYPTION_KEY') or not values.get('CREDENTIAL_ENCRYPTION_KEY_ID'):
        raise UpgradeError('Credential encryption configuration missing')
    return {'sha256': sha(path), 'variable_names': sorted(values),
            'database': 'codex_agent', 'redis_namespace': 'codex-agent:production:'}


def read_schema_checksums():
    port = command(['docker', 'port', PG_CONTAINER, '5432/tcp']).decode().strip()
    if port != '127.0.0.1:25433':
        raise UpgradeError('Dedicated PostgreSQL container port mismatch')
    raw = command(['docker', 'exec', PG_CONTAINER, 'psql', '-X', '-q', '-A', '-t', '-v',
                   'ON_ERROR_STOP=1', '-U', 'postgres', '-d', 'codex_agent', '-c',
                   'SELECT coalesce(json_object_agg(name,checksum),\'{}\'::json) FROM schema_migrations;'])
    return json.loads(raw)


def schema_checksums():
    result = read_schema_checksums()
    if result != MIGRATIONS:
        raise UpgradeError('Database schema differs from the reviewed unchanged baseline')
    return result


def schema_checksums_v2():
    result = read_schema_checksums()
    if result not in (MIGRATIONS, TARGET_MIGRATIONS):
        raise UpgradeError('Database is neither the reviewed v2 baseline nor the exact target schema')
    return result


def require_checks(report, expected, schema):
    if report.get('schema') != schema or report.get('status') != 'pass':
        raise UpgradeError('Required v2 evidence schema or passing status is absent')
    checks = report.get('checks')
    if not isinstance(checks, dict) or any(checks.get(name) is not True for name in expected):
        raise UpgradeError('Required v2 evidence checks are missing or failed')


def validate_v2_evidence(manifest, acceptance, migration, rollback, frontend, source):
    require_checks(acceptance, V2_CHECKS, 'codex-agent-pg-upgrade-acceptance-v2')
    for key in ('source_sha256', 'binary_sha256', 'frontend', 'migration_evidence_sha256',
                'rollback_evidence_sha256', 'frontend_evidence_sha256'):
        if acceptance.get(key) != manifest.get(key):
            raise UpgradeError('V2 acceptance does not bind these exact release artifacts')
    files = source.get('files')
    runners = [name for name in ('database.go', 'internal/server/database.go') if isinstance(files, dict) and name in files]
    if (not isinstance(files, dict) or len(runners) != 1 or not {'go.mod', 'go.sum'} <= files.keys()
            or any(not isinstance(value, str) or not SHA.fullmatch(value) for value in files.values())):
        raise UpgradeError('Invalid frozen backend source manifest')
    for name in files:
        relative(name)
        if not (name in ('go.mod', 'go.sum') or ('/' not in name and name.endswith('.go'))
                or (name.startswith(('internal/', 'cmd/', 'migrations/', 'native-transport/tests/')) and name.endswith('.go'))
                or (name.startswith('migrations/') and name.endswith('.sql'))):
            raise UpgradeError('Frozen backend source manifest contains an unexpected path')
    source_sha = bytes_sha(json.dumps(files, sort_keys=True).encode())
    if source.get('sha256') != source_sha or manifest.get('source_sha256') != source_sha:
        raise UpgradeError('Frozen backend source digest does not match the release')
    if {name: value for name, value in files.items() if name.startswith('migrations/') and name.endswith('.sql')} != TARGET_MIGRATIONS:
        raise UpgradeError('Frozen source does not contain precisely the reviewed migrations')
    require_checks(migration, MIGRATION_CHECKS, 'codex-agent-pg-additive-migration-v1')
    if (migration.get('synthetic_only') is not True or migration.get('scope') != 'migration-focused'
            or not isinstance(migration.get('source_sha256'), str) or not SHA.fullmatch(migration['source_sha256'])
            or migration.get('baseline_checksums') != MIGRATIONS or migration.get('target_checksums') != TARGET_MIGRATIONS
            or migration.get('migration_runner_sha256') != files.get(runners[0])):
        raise UpgradeError('Isolated migration evidence differs from the reviewed migration implementation')
    require_checks(rollback, ROLLBACK_CHECKS, 'codex-agent-pg-rollback-compatibility-v1')
    if (rollback.get('synthetic_only') is not True or rollback.get('target_checksums') != TARGET_MIGRATIONS
            or not isinstance(rollback.get('previous_binary_sha256'), str)
            or not SHA.fullmatch(rollback['previous_binary_sha256'])):
        raise UpgradeError('Rollback evidence does not cover the exact additive target and previous executable')
    if (frontend.get('schema') != 'codex-agent-production-frontend-v1' or frontend.get('status') != 'pass'
            or frontend.get('frontend') != manifest.get('frontend')
            or frontend.get('binary_sha256') != rollback['previous_binary_sha256']
            or not isinstance(frontend.get('unit_sha256'), str) or not SHA.fullmatch(frontend['unit_sha256'])):
        raise UpgradeError('Production frontend capture or rollback executable binding differs')


def private_environment(path):
    path = regular(path)
    if path.stat().st_mode & 0o077 or path.stat().st_uid != 0:
        raise UpgradeError('Storage environment must be a private root-owned file')
    values = {}
    for line in path.read_text().splitlines():
        if not line or line.startswith('#'):
            continue
        if '=' not in line or '\x00' in line:
            raise UpgradeError('Unsupported storage environment syntax')
        key, value = line.split('=', 1)
        if not re.fullmatch(r'[A-Z][A-Z0-9_]*', key) or key in values:
            raise UpgradeError('Invalid or duplicate storage environment variable')
        values[key] = value
    return values


def v2_migration_environment():
    environment_facts()
    runtime, migration = private_environment(RUNTIME_ENV), private_environment(MIGRATION_ENV)
    if set(migration) != {'DATABASE_URL'}:
        raise UpgradeError('Migration environment must contain only DATABASE_URL')
    pg = urllib.parse.urlsplit(migration['DATABASE_URL'])
    runtime_pg = urllib.parse.urlsplit(runtime.get('DATABASE_URL', ''))
    if not runtime_pg.password or urllib.parse.parse_qs(runtime_pg.query) != {'sslmode': ['disable']} or runtime_pg.fragment:
        raise UpgradeError('Runtime PostgreSQL connection options differ from the reviewed project target')
    if (pg.scheme not in ('postgres', 'postgresql') or pg.hostname != '127.0.0.1' or pg.port != 25433
            or pg.path != '/codex_agent' or pg.username != 'codex_agent_migrator' or not pg.password
            or urllib.parse.parse_qs(pg.query) != {'sslmode': ['disable']} or pg.fragment):
        raise UpgradeError('Migration role or target differs from the dedicated project owner')
    try:
        key = base64.b64decode(runtime.get('CREDENTIAL_ENCRYPTION_KEY', ''), validate=True)
    except ValueError:
        raise UpgradeError('Credential encryption key encoding is invalid') from None
    if len(key) != 32 or not re.fullmatch(r'[A-Za-z0-9_.-]{1,128}', runtime.get('CREDENTIAL_ENCRYPTION_KEY_ID', '')):
        raise UpgradeError('Credential encryption configuration is invalid')
    return {'PATH': '/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin',
        'LANG': 'C.UTF-8', **runtime, **migration}


def v2_storage_facts(check_running_key=False):
    environment = v2_migration_environment()
    key_id = environment['CREDENTIAL_ENCRYPTION_KEY_ID']
    # Only ownership/permission booleans leave PostgreSQL; no credential material.
    sql = """SELECT json_build_object(
        'database', current_database(),
        'owner', (SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname=current_database()),
        'migrator_restricted', (SELECT rolcanlogin AND NOT rolsuper AND NOT rolcreatedb AND NOT rolcreaterole FROM pg_roles WHERE rolname='codex_agent_migrator'),
        'runtime_restricted', (SELECT rolcanlogin AND NOT rolsuper AND NOT rolcreatedb AND NOT rolcreaterole FROM pg_roles WHERE rolname='codex_agent_runtime'),
        'runtime_no_create', NOT has_schema_privilege('codex_agent_runtime','public','CREATE'),
        'tables_owned', (SELECT bool_and(pg_get_userbyid(c.relowner)='codex_agent_migrator') FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' AND c.relkind IN ('r','p')),
        'default_runtime_crud', (SELECT count(DISTINCT x.privilege_type)=4 FROM pg_default_acl d CROSS JOIN LATERAL aclexplode(d.defaclacl) x WHERE d.defaclrole='codex_agent_migrator'::regrole AND d.defaclnamespace='public'::regnamespace AND d.defaclobjtype='r' AND x.grantee='codex_agent_runtime'::regrole AND x.privilege_type IN ('SELECT','INSERT','UPDATE','DELETE')),
        'key_ids_match', NOT EXISTS(SELECT 1 FROM account_credentials WHERE encryption_key_id <> '%s'))""" % key_id
    facts = json.loads(command(['docker', 'exec', PG_CONTAINER, 'psql', '-X', '-q', '-A', '-t', '-v',
        'ON_ERROR_STOP=1', '-U', 'postgres', '-d', 'codex_agent', '-c', sql]))
    required = ('migrator_restricted', 'runtime_restricted', 'runtime_no_create', 'tables_owned',
        'default_runtime_crud', 'key_ids_match')
    if facts.get('database') != 'codex_agent' or facts.get('owner') != 'codex_agent_migrator' or any(facts.get(k) is not True for k in required):
        raise UpgradeError('Dedicated database ownership, permissions or encryption key identity differs')
    if check_running_key:
        pid = properties().get('MainPID', '0')
        if not pid.isdigit() or int(pid) < 1:
            raise UpgradeError('Running service is required to verify encryption continuity')
        values = dict(item.split(b'=', 1) for item in (Path('/proc') / pid / 'environ').read_bytes().split(b'\0') if b'=' in item)
        for name in ('CREDENTIAL_ENCRYPTION_KEY', 'CREDENTIAL_ENCRYPTION_KEY_ID'):
            if values.get(name.encode()) != environment[name].encode():
                raise UpgradeError('Encryption configuration differs from the running service')
    return {**facts, 'migration_env_sha256': sha(MIGRATION_ENV), 'runtime_env_sha256': sha(RUNTIME_ENV),
        'running_encryption_checked': check_running_key}


def validate_acceptance(report, source_sha256):
    if report.get('status') != 'pass' or report.get('source_sha256') != source_sha256:
        raise UpgradeError('Upgrade acceptance report did not pass for this source snapshot')
    checks = report.get('checks')
    if not isinstance(checks, dict) or any(checks.get(name) is not True for name in REQUIRED_CHECKS):
        raise UpgradeError('Upgrade acceptance has missing or failed required checks')


def runtime_readable(path, executable=False):
    path = regular(path)
    required = 0o005 if executable else 0o004
    if path.stat().st_mode & required != required:
        raise UpgradeError('DynamicUser cannot read or execute the candidate artifact')
    for parent in path.parents:
        directory(parent)
        if parent.stat().st_mode & 0o001 != 0o001:
            raise UpgradeError('DynamicUser cannot traverse the candidate directory')


def validate_manifest(release):
    scoped_release(release)
    directory(release)
    manifest = json.loads(regular(release / 'upgrade-manifest.json').read_text())
    is_v2 = manifest.get('schema') == V2_SCHEMA
    if manifest.get('schema') not in ('codex-agent-pg-upgrade-v1', V2_SCHEMA) or manifest.get('release_id') != release.name:
        raise UpgradeError('Unexpected upgrade manifest identity')
    if is_v2 and bytes_sha(json.dumps({key: value for key, value in manifest.items() if key != 'release_id'}, sort_keys=True).encode()) != release.name:
        raise UpgradeError('V2 release identity does not bind the exact immutable manifest')
    for key in ('source_sha256', 'binary_sha256', 'acceptance_report_sha256', 'remote_script_sha256'):
        if not isinstance(manifest.get(key), str) or not SHA.fullmatch(manifest[key]):
            raise UpgradeError('Required manifest hash is missing')
    if manifest.get('tests_passed') is not True or manifest.get('migration_checksums') != (TARGET_MIGRATIONS if is_v2 else MIGRATIONS):
        raise UpgradeError('Acceptance or unchanged-schema gate is not passed')
    binary = regular(release / 'codex-agent')
    if sha(binary) != manifest['binary_sha256'] or not os.access(binary, os.X_OK):
        raise UpgradeError('Candidate executable identity or permissions differ')
    runtime_readable(binary, executable=True)
    if sha(regular(release / 'deploy/upgrade_remote.py')) != manifest['remote_script_sha256']:
        raise UpgradeError('Upgrade helper hash mismatch')
    frontend = manifest.get('frontend')
    if not isinstance(frontend, dict) or 'index.html' not in frontend:
        raise UpgradeError('Candidate frontend manifest is absent')
    for name, value in frontend.items():
        relative(name)
        if not isinstance(value, str) or not SHA.fullmatch(value):
            raise UpgradeError('Invalid frontend hash')
    if file_map(release / 'frontend/dist') != frontend:
        raise UpgradeError('Candidate frontend files differ from the manifest')
    for name in frontend:
        runtime_readable(release / 'frontend/dist' / name)
    evidence = regular(release / 'evidence/upgrade-acceptance.json')
    if sha(evidence) != manifest['acceptance_report_sha256']:
        raise UpgradeError('Upgrade acceptance report hash mismatch')
    report = json.loads(evidence.read_text())
    if is_v2:
        if {name: sha(regular(release / name)) for name in TARGET_MIGRATIONS} != TARGET_MIGRATIONS:
            raise UpgradeError('Packaged additive migration bytes differ from the reviewed checksums')
        documents = {}
        for key, name in V2_EVIDENCE.items():
            expected = manifest.get(key)
            evidence_file = regular(release / name)
            if not isinstance(expected, str) or not SHA.fullmatch(expected) or sha(evidence_file) != expected:
                raise UpgradeError('V2 evidence file hash mismatch')
            documents[key] = json.loads(evidence_file.read_text())
        validate_v2_evidence(manifest, report, documents['migration_evidence_sha256'],
            documents['rollback_evidence_sha256'], documents['frontend_evidence_sha256'], documents['source_manifest_sha256'])
    else:
        validate_acceptance(report, manifest['source_sha256'])
    return manifest


def verify_health(binary, web, expected_binary, expected_frontend, timeout=60):
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        try:
            state = properties()
            pid = state.get('MainPID', '0')
            if state.get('ActiveState') != 'active' or not pid.isdigit() or int(pid) < 1:
                raise UpgradeError('Service is not running')
            if sha(Path('/proc') / pid / 'exe') != expected_binary:
                raise UpgradeError('Running binary identity mismatch')
            args = (Path('/proc') / pid / 'cmdline').read_bytes().split(b'\0')
            if args[:5] != [str(binary).encode(), b'--listen', b'0.0.0.0:18879', b'--web', str(web).encode()]:
                raise UpgradeError('Running process arguments mismatch')
            codes = {}
            for route in ('/health/live', '/health/ready', '/'):
                with opener.open(BASE_URL + route, timeout=5) as response:
                    body = response.read(4 * 1024 * 1024)
                    codes[route] = response.status
                    if response.status != 200 or (route == '/' and bytes_sha(body) != expected_frontend['index.html']):
                        raise UpgradeError('Readiness or served frontend identity mismatch')
            index = regular(web / 'index.html').read_text()
            assets = sorted(set(re.findall(r'(?:src|href)=[\"\']/?(assets/[^\"\'?#]+)', index)))
            if not assets:
                raise UpgradeError('Candidate index contains no static assets')
            for asset in assets:
                if asset not in expected_frontend:
                    raise UpgradeError('Index asset is absent from frontend manifest')
                with opener.open(BASE_URL + '/' + asset, timeout=5) as response:
                    if response.status != 200 or bytes_sha(response.read(16 * 1024 * 1024)) != expected_frontend[asset]:
                        raise UpgradeError('Served static asset differs from the manifest')
            latest = properties()
            if latest.get('MainPID') != pid or latest.get('ActiveState') != 'active':
                raise UpgradeError('Process identity changed during verification')
            return {'checked_at': now(), 'main_pid': int(pid), 'status_codes': codes,
                    'runtime_binary_sha256': expected_binary, 'root_sha256': expected_frontend['index.html'],
                    'entry_assets_verified': len(assets)}
        except (OSError, ValueError, UpgradeError):
            time.sleep(0.5)
    raise UpgradeError('Stable process, health or static asset verification timed out')


def assert_stopped():
    state = properties()
    if state.get('MainPID') != '0' or state.get('ActiveState') not in ('inactive', 'failed'):
        raise UpgradeError('Previous service has not completely stopped')
    group = state.get('ControlGroup', '')
    if group:
        if group != '/system.slice/' + SERVICE:
            raise UpgradeError('Unexpected service control group')
        folder = Path('/sys/fs/cgroup') / group.lstrip('/')
        if folder.exists():
            for path in folder.rglob('cgroup.procs'):
                if path.read_text().strip():
                    raise UpgradeError('Service cgroup still contains running processes')


def stop_service():
    system('stop', SERVICE)
    assert_stopped()
    if properties().get('Result') in ('timeout', 'signal', 'core-dump', 'watchdog'):
        raise UpgradeError('Previous service did not drain cleanly')


class Transaction:
    def __init__(self, release):
        self.release = scoped_release(release)
        directory(release)
        self.private = release / '.upgrade-private'
        self.private.mkdir(mode=0o700, exist_ok=True)
        directory(self.private)
        os.chmod(self.private, 0o700)
        self.state_path = self.private / 'state.json'
        self.state = json.loads(regular(self.state_path).read_text()) if self.state_path.exists() else {
            'release_id': release.name, 'schema': 'codex-agent-pg-upgrade-state-v1',
            'phase': 'staged', 'deployed': False, 'schema_migrated': False,
            'database_restored': False, 'backup_directory': str(self.private)}
        if self.state.get('release_id') != release.name:
            raise UpgradeError('Saved upgrade state belongs to another release')

    def save(self, phase, **updates):
        self.state.update(updates, phase=phase, updated_at=now())
        write_json(self.state_path, self.state)
        journal = self.private / 'journal.jsonl'
        if journal.exists() or journal.is_symlink():
            regular(journal)
        fd = os.open(journal, os.O_WRONLY | os.O_APPEND | os.O_CREAT | os.O_NOFOLLOW, 0o600)
        with os.fdopen(fd, 'ab') as handle:
            entry = {'at': now(), 'phase': phase}
            if self.state.get('schema') == 'codex-agent-pg-upgrade-state-v2':
                entry.update({key: self.state.get(key) for key in ('deployed', 'schema_migrated',
                    'schema_state', 'database_restored', 'migration_executed_this_attempt')})
            handle.write((json.dumps(entry) + '\n').encode())
            handle.flush()
            os.fsync(handle.fileno())

    def before(self):
        raw = regular(self.private / 'previous.service').read_bytes()
        if bytes_sha(raw) != self.state.get('previous_unit_sha256'):
            raise UpgradeError('Previous unit backup checksum mismatch')
        parse_unit(raw)
        return raw

    def check_schema(self):
        return schema_checksums()

    def validate_baseline(self, manifest, parsed, original, frontend):
        return {}

    def before_switch(self, manifest):
        # V1 deliberately never invokes a migration command.
        return None

    def preflight(self):
        manifest = validate_manifest(self.release)
        facts = environment_facts()
        schema = self.check_schema()
        properties()
        original = regular(UNIT).read_bytes()
        parsed = parse_unit(original)
        regular(parsed['binary'])
        old_frontend = file_map(parsed['web'])
        health = verify_health(parsed['binary'], parsed['web'], sha(parsed['binary']), old_frontend)
        if self.state['phase'] == 'deployed':
            if original != plan_unit(self.before(), self.release):
                raise UpgradeError('Another release changed the active unit')
            return self.state
        if self.state['phase'] not in ('staged', 'preflight_passed', 'rolled_back'):
            raise UpgradeError('Interrupted upgrade needs rollback before preflight')
        if parsed['binary'] == self.release / 'codex-agent':
            raise UpgradeError('Candidate is running without a completed activation journal')
        if self.state['phase'] == 'rolled_back' and original != self.before():
            raise UpgradeError('Another release replaced the rollback baseline')
        extra = self.validate_baseline(manifest, parsed, original, old_frontend)
        self.save('preflight_passed', tests_passed=True, source_sha256=manifest['source_sha256'],
                  candidate_binary_sha256=manifest['binary_sha256'], environment=facts,
                  migration_checksums=schema, preflight_unit_sha256=bytes_sha(original), baseline_health=health, **extra)
        return self.state

    def backup(self, original):
        atomic_write(self.private / 'previous.service', original)
        atomic_write(self.private / 'production.env', regular(RUNTIME_ENV).read_bytes())
        if MIGRATION_ENV.exists():
            atomic_write(self.private / 'migration.env', regular(MIGRATION_ENV).read_bytes())
        parsed = parse_unit(original)
        self.save('backup_started', previous_unit_sha256=bytes_sha(original),
                  previous_binary=str(parsed['binary']), previous_binary_sha256=sha(parsed['binary']),
                  previous_frontend=str(parsed['web']), previous_frontend_files=file_map(parsed['web']))
        temporary = self.private / ('database-' + uuid.uuid4().hex + '.dump')
        fd = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        with os.fdopen(fd, 'wb') as output:
            result = subprocess.run(['docker', 'exec', PG_CONTAINER, 'pg_dump', '-U', 'postgres',
                                     '-d', 'codex_agent', '--format=custom'], stdout=output,
                                    stderr=subprocess.PIPE, timeout=600)
            output.flush()
            os.fsync(output.fileno())
        if result.returncode or temporary.stat().st_size < 100:
            raise UpgradeError('Private PostgreSQL backup did not complete')
        with temporary.open('rb') as handle:
            result = subprocess.run(['docker', 'exec', '-i', PG_CONTAINER, 'pg_restore', '--list'],
                                    stdin=handle, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=120)
        if result.returncode or b'TABLE DATA' not in result.stdout:
            raise UpgradeError('Private PostgreSQL backup validation failed')
        self.save('backup_complete', database_backup=str(temporary), database_backup_sha256=sha(temporary),
                  database_backup_bytes=temporary.stat().st_size,
                  backup_note='Consistent pre-switch snapshot; rollback preserves the current database')

    def unchanged_environment(self):
        if environment_facts()['sha256'] != self.state.get('environment', {}).get('sha256'):
            raise UpgradeError('Runtime environment changed after preflight')

    def rollback(self, automatic=False):
        properties()
        self.unchanged_environment()
        original = self.before()
        changed = plan_unit(original, self.release)
        rollback_plan(regular(UNIT).read_bytes(), original, changed)
        parsed = parse_unit(original)
        if sha(regular(parsed['binary'])) != self.state['previous_binary_sha256']:
            raise UpgradeError('Preserved previous executable changed')
        if file_map(parsed['web']) != self.state['previous_frontend_files']:
            raise UpgradeError('Preserved previous frontend changed')
        if self.state['phase'] == 'rolled_back':
            health = verify_health(parsed['binary'], parsed['web'], self.state['previous_binary_sha256'],
                                   self.state['previous_frontend_files'])
            return {**self.state, 'health': health, 'reused': True}
        self.save('rollback_started', deployed=False, automatic_rollback=automatic)
        # Stop before restoring ExecStart so no credential refresher can overlap.
        system('stop', SERVICE)
        assert_stopped()
        atomic_write(UNIT, original, 0o644)
        system('daemon-reload')
        system('reset-failed', SERVICE)
        system('start', SERVICE)
        health = verify_health(parsed['binary'], parsed['web'], self.state['previous_binary_sha256'],
                               self.state['previous_frontend_files'])
        self.save('rolled_back', deployed=False, health=health, rollback_unit_sha256=sha(UNIT))
        return self.state

    def activate(self):
        manifest = validate_manifest(self.release)
        if self.state['phase'] == 'deployed':
            self.unchanged_environment()
            if regular(UNIT).read_bytes() != plan_unit(self.before(), self.release):
                raise UpgradeError('Another release changed the active unit')
            health = verify_health(self.release / 'codex-agent', self.release / 'frontend/dist',
                                   manifest['binary_sha256'], manifest['frontend'])
            return {**self.state, 'health': health, 'reused': True}
        if self.state['phase'] not in ('staged', 'preflight_passed', 'rolled_back'):
            self.rollback(automatic=True)
            raise UpgradeError('Interrupted upgrade recovered; rerun activate after reviewing the state')
        self.preflight()
        original = regular(UNIT).read_bytes()
        if bytes_sha(original) != self.state['preflight_unit_sha256']:
            raise UpgradeError('Unit changed since preflight')
        self.backup(original)
        self.unchanged_environment()
        changed = plan_unit(original, self.release)
        try:
            self.save('stop_requested', deployed=False, write_freeze_started_at=now())
            stop_service()
            # Recheck the files after draining before publishing any unit change.
            if regular(UNIT).read_bytes() != original:
                raise UpgradeError('Unit changed during shutdown')
            self.unchanged_environment()
            validate_manifest(self.release)
            self.before_switch(manifest)
            self.save('switch_requested', candidate_unit_sha256=bytes_sha(changed))
            atomic_write(UNIT, changed, 0o644)
            system('daemon-reload')
            system('reset-failed', SERVICE)
            system('start', SERVICE)
            health = verify_health(self.release / 'codex-agent', self.release / 'frontend/dist',
                                   manifest['binary_sha256'], manifest['frontend'])
            self.save('deployed', deployed=True, deployed_at=now(), health=health,
                      write_freeze_ended_at=now(), current_unit_sha256=sha(UNIT))
            return self.state
        except Exception as error:
            self.save('activation_failed', deployed=False, failure_type=type(error).__name__)
            try:
                self.rollback(automatic=True)
            except Exception as recovery:
                self.save('rollback_failed', deployed=False, rollback_failure_type=type(recovery).__name__)
                raise UpgradeError('Activation and automatic recovery failed; inspect private state') from None
            raise UpgradeError('Activation failed; previous PostgreSQL service is restored and healthy') from None

    def status(self):
        properties()
        active = False
        if self.state['phase'] == 'deployed':
            active = regular(UNIT).read_bytes() == plan_unit(self.before(), self.release)
            if active:
                state = properties()
                pid = state.get('MainPID', '0')
                active = (state.get('ActiveState') == 'active' and pid.isdigit() and int(pid) > 0
                          and sha(Path('/proc') / pid / 'exe') == self.state.get('candidate_binary_sha256'))
        return {**self.state, 'currently_active': active, 'deployed': active}


class AdditiveTransaction(Transaction):
    """Explicit v2 state machine; the v1 unchanged-schema rules stay intact."""
    def __init__(self, release):
        super().__init__(release)
        self.state['schema'] = 'codex-agent-pg-upgrade-state-v2'
        self.state['migration_policy'] = 'reviewed-additive-003'

    def check_schema(self):
        return schema_checksums_v2()

    def activate(self):
        if self.state['phase'] == 'deployed' and self.check_schema() != TARGET_MIGRATIONS:
            raise UpgradeError('Previously activated v2 release no longer has the reviewed target schema')
        return super().activate()

    def validate_baseline(self, manifest, parsed, original, frontend):
        capture = json.loads(regular(self.release / V2_EVIDENCE['frontend_evidence_sha256']).read_text())
        if (frontend != manifest['frontend'] or capture['frontend'] != frontend
                or sha(parsed['binary']) != capture['binary_sha256']
                or bytes_sha(original) != capture['unit_sha256']):
            raise UpgradeError('Live binary, unit or exact frontend differs from the reviewed rollback baseline')
        facts = v2_storage_facts(check_running_key=True)
        return {'migration_environment': facts, 'schema_migrated': self.check_schema() == TARGET_MIGRATIONS,
            'database_restored': False, 'frontend_preserved': True}

    def unchanged_environment(self):
        super().unchanged_environment()
        if sha(regular(MIGRATION_ENV)) != self.state.get('migration_environment', {}).get('migration_env_sha256'):
            raise UpgradeError('Migration environment changed after preflight')

    def observe_schema(self):
        try:
            checksums = self.check_schema()
            return {'schema_migrated': checksums == TARGET_MIGRATIONS,
                'schema_state': 'target' if checksums == TARGET_MIGRATIONS else 'baseline',
                'migration_checksums': checksums, 'database_restored': False}
        except Exception:
            # An unavailable database must not be recorded as "not migrated".
            return {'schema_migrated': None, 'schema_state': 'unknown', 'database_restored': False}

    def before_switch(self, manifest):
        assert_stopped()
        self.unchanged_environment()
        before = self.check_schema()
        v2_storage_facts()
        if before == TARGET_MIGRATIONS:
            self.save('migration_already_applied', schema_migrated=True, schema_state='target',
                migration_checksums=before, migration_executed_this_attempt=False)
            return
        self.save('migration_started', schema_migrated=False, schema_state='baseline',
            migration_checksums=before, migration_executed_this_attempt=True, migration_started_at=now())
        try:
            # A fixed command with a clean, private environment; no shell sourcing,
            # credential argv, import-legacy, or data-restoration fallback.
            response = command([str(self.release / 'codex-agent'), 'migrate'],
                timeout=1800, env=v2_migration_environment(), cwd=self.release)
            if json.loads(response) != {'migrated': True} or self.check_schema() != TARGET_MIGRATIONS:
                raise UpgradeError('Candidate migration did not establish the exact reviewed target')
            assert_stopped()
            v2_storage_facts()
            self.save('migration_complete', schema_migrated=True, schema_state='target',
                migration_checksums=TARGET_MIGRATIONS, migration_completed_at=now())
        except Exception:
            self.save('migration_failed', **self.observe_schema())
            raise

    def rollback(self, automatic=False):
        # The existing rollback drains candidate before starting preserved old PG
        # binary; it never runs pg_restore/import-legacy or changes database data.
        result = super().rollback(automatic=automatic)
        self.save('rolled_back', **self.observe_schema())
        return {**result, **self.state}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action', choices=('preflight', 'activate', 'status', 'rollback'))
    parser.add_argument('--release', type=Path, required=True)
    args = parser.parse_args()
    if os.name != 'posix' or os.geteuid() != 0:
        print(json.dumps({'ok': False, 'error': 'Linux root execution is required'}))
        return 1
    try:
        with lock():
            scoped_release(args.release)
            kind = json.loads(regular(args.release / 'upgrade-manifest.json').read_text()).get('schema')
            if kind not in ('codex-agent-pg-upgrade-v1', V2_SCHEMA):
                raise UpgradeError('Unknown explicit upgrade version')
            transaction = (AdditiveTransaction if kind == V2_SCHEMA else Transaction)(args.release)
            result = getattr(transaction, args.action)()
        print(json.dumps({'ok': True, **result}, indent=2))
        return 0
    except Exception as error:
        # The type is sufficient for unexpected errors and cannot expose values.
        message = str(error) if isinstance(error, UpgradeError) else type(error).__name__
        print(json.dumps({'ok': False, 'error': message}))
        return 1


if __name__ == '__main__':
    raise SystemExit(main())
