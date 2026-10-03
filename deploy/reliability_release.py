#!/usr/bin/env python3
"""Journaled runtime releases with the reviewed six-migration schema."""
import argparse
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tempfile
import time
import urllib.parse

# The release directory is immutable and its inventory is exact. Importing the
# reviewed helper must not create a new unmanifested __pycache__ in that tree.
sys.dont_write_bytecode = True
_spec = importlib.util.spec_from_file_location('reviewed_powerpoint_release', Path(__file__).with_name('powerpoint_release.py'))
base = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(base)
require = base.require
GATES = ('go', 'vet', 'frontend', 'bps_live', 'official_live', 'race', 'redis', 'pg', 'cost_parity')
ADDED = 'migrations/005_usage_latency_known.sql'
LEGACY_NAMES = {'migrations/' + name for name in (
    '000_core.sql', '001_accounts.sql', '002_metrics.sql',
    '003_management_cleanup.sql', '004_account_credential_request_state.sql')}
CLI_SOURCE = Path('/opt/codex-agent-integration/checks/quality-20260924/official-bundle')
CLI_RUNTIME = '/var/lib/codex-agent/official-runtime'
# User explicitly requested no service-level memory, swap or task ceiling.
OFFICIAL_TASKS_MAX = 'infinity'
OFFICIAL_MEMORY_HIGH = 'infinity'
OFFICIAL_MEMORY_MAX = 'infinity'
OFFICIAL_SWAP_MAX = 'infinity'
MIGRATION_ENV = Path('/etc/codex-agent/migration.env')
MODES = {'powerpoint_bps', 'official_codex_cli', 'official_app_server'}
VERSION = re.compile(r'(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)')


def release_version(value):
    require(isinstance(value, str) and len(value) <= 32 and VERSION.fullmatch(value), 'invalid release version')
    # A new minor release requires a separate review of this executor's schema
    # and runtime contract. Patch releases preserve the already installed six.
    require(value.startswith('0.4.'), 'unsupported runtime release series')
    return value


def map_digest(files):
    return hashlib.sha256(json.dumps(files, sort_keys=True).encode()).hexdigest()


def source_scope(files, scope):
    if scope == 'full':
        return dict(files)
    if scope == 'frontend':
        return {k: v for k, v in files.items() if k.startswith('frontend/')}
    if scope == 'scripts':
        return {k: v for k, v in files.items() if k.startswith(('tools/', 'deploy/'))}
    if scope == 'runtime':
        return {k: v for k, v in source_scope(files, 'backend').items() if not k.endswith('_test.go')}
    if scope == 'powerpoint_runtime':
        return {k: v for k, v in source_scope(files, 'runtime').items() if not Path(k).name.startswith(('gateway_official_', 'official_'))}
    if scope == 'backend':
        return {k: v for k, v in files.items() if k in ('go.mod', 'go.sum', 'internal/server/account_window_prices.json')
                or k.endswith('.go') and ('/' not in k or k.startswith(('internal/', 'cmd/', 'migrations/', 'native-transport/tests/')))
                or k.startswith('migrations/') and k.endswith('.sql')}
    raise RuntimeError('unsupported evidence source scope')


def validate_evidence(evidence, source, version='0.4.0'):
    version = release_version(version)
    require(evidence.get('passed') is True and evidence.get('source_sha256') == source['sha256'], 'acceptance source mismatch')
    require(version != '0.4.0' or evidence.get('checks', {}).get('schema_unchanged') is not True, 'additive migration must not claim unchanged schema')
    scope = evidence.get('verification_scope', 'all_modes')
    require(scope in ('all_modes', 'official_app_server', 'powerpoint_bps'), 'unsupported verification scope')
    blocked_bps = scope == 'official_app_server'
    retired_official = scope == 'powerpoint_bps'
    blocked_current_bps = retired_official and 'bps_live' in evidence.get('external_limitations', {})
    if blocked_current_bps:
        require(evidence['external_limitations']['bps_live'] == {'status':'blocked_upstream','http_status':403,'message':'Current live inference returned HTTP 403'}, 'unverified active-backend limitation')
        require(evidence.get('bps_composed_verification', {}).get('current_full_go_race_and_storage_required') is True, 'blocked live release requires reviewed base plus current complete local gates')
    if retired_official:
        require(version != '0.4.0', 'initial runtime release requires all modes')
        require(evidence.get('retired_features') == ['official_app_server', 'hosted_codex_tasks'], 'retired runtime scope is not explicit')
    if blocked_bps:
        require(version != '0.4.0', 'initial runtime release requires all modes')
        limitation = evidence.get('external_limitations', {}).get('bps_live', {})
        require(limitation == {'status': 'blocked_upstream', 'http_status': 403, 'message': 'This request was blocked by our usage policy.'}, 'unverified BPS external limitation')
    excluded = {'bps_live'} if blocked_bps else {'official_live','bps_live'} if blocked_current_bps else {'official_live'} if retired_official else set()
    require(all(evidence.get('checks', {}).get(name) is (name not in excluded) for name in GATES), 'required reliability checks missing')
    for name in GATES:
        gate = evidence.get('gates', {}).get(name, {})
        require(gate.get('passed') is (name not in excluded) and gate.get('evidence_files'), 'gate has no passing evidence: ' + name)
        subset = gate.get('source_files')
        require(isinstance(subset, dict) and subset and all(source['files'].get(k) == v for k, v in subset.items()), 'gate source files are stale: ' + name)
        scope = gate.get('scope', 'full')
        if scope == 'files':
            require(name == 'cost_parity', 'explicit source scope is reserved for pricing parity')
            required = {'internal/server/admin_account_window_cost.go', 'internal/server/account_window_prices.json', 'internal/server/admin_account_window_parity_test.go'}
        else:
            expected = source_scope(source['files'], scope)
            require(subset == expected, 'gate scope coverage is incomplete: ' + name)
            require(scope != 'runtime' or name in ('bps_live', 'official_live', 'race', 'redis', 'pg'), 'runtime-only source scope is invalid for this gate')
            require(scope != 'powerpoint_runtime' or name == 'bps_live', 'PowerPoint source scope is invalid for this gate')
            required = set(source_scope(source['files'], scope if scope in ('runtime', 'powerpoint_runtime') else 'frontend' if name == 'frontend' else 'backend'))
            if name == 'cost_parity':
                required = {'internal/server/admin_account_window_cost.go', 'internal/server/account_window_prices.json', 'internal/server/admin_account_window_parity_test.go'}
        require(required <= set(subset), 'gate omits required source coverage: ' + name)
        require(gate.get('source_sha256') == map_digest(subset), 'gate source digest mismatch: ' + name)
    references = evidence.get('evidence_files')
    require(isinstance(references, list) and references, 'acceptance evidence references missing')
    known = {(r.get('path'), r.get('sha256')) for r in references}
    for gate in evidence['gates'].values():
        for reference in gate.get('evidence_files', []):
            require((reference.get('path'), reference.get('sha256')) in known, 'gate evidence not included in acceptance')


def validate_cli_spec(spec):
    require(spec.get('source_root') == CLI_SOURCE.as_posix() and spec.get('entrypoint') == 'bin/codex', 'unexpected official CLI source')
    files = spec.get('files')
    require(isinstance(files, dict) and {'bin/codex', 'codex-package.json'} <= set(files), 'official CLI bundle incomplete')
    require('bin/codex-code-mode-host' in files or 'codex-resources/codex-code-mode-host' in files, 'official code-mode companion missing')
    for name, digest in files.items():
        base.relative_name(name)
        require(isinstance(digest, str) and base.HASH.fullmatch(digest), 'invalid official CLI digest')
    require(spec.get('binary_sha256') == files['bin/codex'], 'official CLI identity mismatch')


def validate(release):
    release = base.release_path(release)
    manifest = base.read_json(base.artifact(release, 'manifest.json'))
    version = release_version(manifest.get('version'))
    require(manifest.get('backend') == 'selectable_official_runtime', 'incorrect reliability release')
    files = manifest.get('files', {})
    required = {'codex-agent', 'frontend/dist/index.html', 'acceptance.json', 'source-manifest.json',
                'deploy/powerpoint_release.py', 'deploy/reliability_release.py', ADDED}
    require(required <= set(files) and 'manifest.json' not in files, 'release incomplete')
    optional = {'codex-agent-ingress', 'deploy/zero_downtime_release.py'}
    for name, expected in files.items():
        require(name in required | optional or name.startswith(('frontend/dist/', 'evidence/')), 'unexpected release artifact')
        require(isinstance(expected, str) and base.HASH.fullmatch(expected), 'invalid artifact digest')
        path = base.artifact(release, name)
        require(base.digest(path) == expected, 'release artifact checksum mismatch')
        if os.name == 'posix':
            require(path.stat().st_mode & 0o004 and all(p.stat().st_mode & 0o001 for p in path.parents), 'unreadable release artifact')
    actual = set()
    for path in release.rglob('*'):
        require(not path.is_symlink() and (path.is_file() or path.is_dir()), 'invalid release member')
        if path.is_file():
            actual.add(path.relative_to(release).as_posix())
    require(actual == set(files) | {'manifest.json'}, 'unmanifested release artifact')
    binary = base.artifact(release, 'codex-agent')
    with binary.open('rb') as handle:
        header = handle.read(20)
    require(header[:6] == b'\x7fELF\x02\x01' and header[18:20] == b'\x3e\x00', 'candidate is not Linux amd64 ELF')
    require(os.name != 'posix' or binary.stat().st_mode & 0o001, 'candidate is not executable')
    if 'codex-agent-ingress' in files:
        require('deploy/zero_downtime_release.py' in files, 'stable ingress controller missing')
        ingress = base.artifact(release, 'codex-agent-ingress')
        with ingress.open('rb') as handle:
            header = handle.read(20)
        require(header[:6] == b'\x7fELF\x02\x01' and header[18:20] == b'\x3e\x00', 'ingress is not Linux amd64 ELF')
        require(os.name != 'posix' or ingress.stat().st_mode & 0o001, 'ingress is not executable')
    source = base.read_json(base.artifact(release, 'source-manifest.json'))
    require(isinstance(source.get('files'), dict) and source['files'], 'source manifest missing')
    for name, digest in source['files'].items():
        require(not {'.local', 'data', '.git'}.intersection(base.relative_name(name).parts) and isinstance(digest, str) and base.HASH.fullmatch(digest), 'invalid source entry')
    require(source.get('sha256') == manifest.get('source_sha256') == map_digest(source['files']), 'source identity mismatch')
    acceptance = base.read_json(base.artifact(release, 'acceptance.json'))
    validate_evidence(acceptance, source, version)
    require(acceptance.get('binary_sha256') == files['codex-agent'], 'binary evidence mismatch')
    require(acceptance['gates']['frontend'].get('artifact_files') == {k: v for k, v in files.items() if k.startswith('frontend/dist/')}, 'frontend build evidence mismatch')
    for reference in acceptance['evidence_files']:
        require(files.get(reference.get('artifact_path')) == reference.get('sha256'), 'packaged evidence mismatch')
    migrations = manifest.get('migration_checksums', {})
    require(set(migrations) == LEGACY_NAMES | {ADDED}, 'only reviewed migration 005 may be added')
    require(migrations == {k: v for k, v in source['files'].items() if k.startswith('migrations/') and k.endswith('.sql')}, 'migration source mismatch')
    sql = base.artifact(release, ADDED).read_text(encoding='utf-8')
    sql = re.sub(r'(?m)--[^\n]*', '', sql).strip()
    require(sql == 'ALTER TABLE usage_events ADD COLUMN IF NOT EXISTS latency_known BOOLEAN;', 'migration is not the reviewed nullable additive column')
    require(files[ADDED] == migrations[ADDED], 'migration artifact mismatch')
    validate_cli_spec(manifest.get('official_cli', {}))
    return manifest


def cli_destination(spec):
    return base.ROOT / 'official-cli' / spec['binary_sha256']


def verify_bundle(root, spec):
    require(root.is_absolute() and root.is_dir() and not root.is_symlink() and not any(p.is_symlink() for p in root.parents), 'invalid official CLI directory')
    actual = set()
    for path in root.rglob('*'):
        require(not path.is_symlink() and (path.is_file() or path.is_dir()), 'non-regular official CLI asset')
        if path.is_file():
            actual.add(path.relative_to(root).as_posix())
    require(actual == set(spec['files']), 'official CLI bundle inventory changed')
    for name, digest in spec['files'].items():
        require(base.digest(base.regular(root / name)) == digest, 'official CLI bundle checksum changed')
    package = base.read_json(root / 'codex-package.json')
    require(package.get('entrypoint') == 'bin/codex' and package.get('layoutVersion') == 1, 'official package layout changed')
    for name in ('bin/codex', 'bin/codex-code-mode-host', 'codex-resources/codex-code-mode-host'):
        if name in actual:
            require(os.name != 'posix' or (root / name).stat().st_mode & 0o001, 'official CLI asset not executable')


def stage_cli(spec):
    validate_cli_spec(spec)
    target = cli_destination(spec)
    if target.exists():
        verify_bundle(target, spec)
        return target
    verify_bundle(CLI_SOURCE, spec)
    parent = target.parent
    require(not any(p.is_symlink() for p in (parent, *parent.parents)), 'invalid official CLI staging parent')
    parent.mkdir(parents=True, exist_ok=True, mode=0o755)
    temporary = Path(tempfile.mkdtemp(prefix='.stage-', dir=parent))
    try:
        shutil.copytree(CLI_SOURCE, temporary, dirs_exist_ok=True)
        for path in (temporary, *temporary.rglob('*')):
            path.chmod(0o755 if path.is_dir() or path.stat().st_mode & 0o111 else 0o644)
        verify_bundle(temporary, spec)
        os.replace(temporary, target)
    finally:
        if temporary.exists():
            shutil.rmtree(temporary)
    return target


def candidate_unit(original, baseline, release, spec):
    result = base.candidate_unit(original, baseline, release)
    for path in environment_files():
        require(not re.search(r'(?m)^\s*(?:export\s+)?(?:CODEX_CLI_PATH|CODEX_RUNTIME_DIR)\s*=', base.regular(Path(path)).read_text()), 'runtime environment file already overrides CLI settings')
    require(not re.search(r'(?m)^UnsetEnvironment=.*(?:CODEX_CLI_PATH|CODEX_RUNTIME_DIR)', original), 'unit unsets required runtime configuration')
    return result.rstrip() + '\n[Service]\nEnvironment="CODEX_CLI_PATH=' + str(cli_destination(spec) / 'bin/codex') + '"\nEnvironment="CODEX_RUNTIME_DIR=' + CLI_RUNTIME + '"\nTasksMax=' + str(OFFICIAL_TASKS_MAX) + '\nMemoryHigh=' + str(OFFICIAL_MEMORY_HIGH) + '\nMemoryMax=' + str(OFFICIAL_MEMORY_MAX) + '\nMemorySwapMax=' + str(OFFICIAL_SWAP_MAX) + '\nCPUQuota=\n'


def environment_files():
    # EnvironmentFile overrides unit Environment, and production has several
    # historical native-helper files. Freeze every one without reading values
    # into a journal or assuming production.env is the only source.
    raw = base.command('systemctl', 'show', base.SERVICE, '-p', 'EnvironmentFiles', '--value')
    paths = {str(base.ENV)}
    for line in raw.splitlines():
        match = re.fullmatch(r'(/[^\r\n]+) \(ignore_errors=(?:yes|no)\)', line.strip())
        require(match is not None, 'unsupported systemd environment file list')
        paths.add(match.group(1))
    return {name: base.digest(base.regular(Path(name))) for name in sorted(paths)}


def process_environment(baseline):
    require(base.current() == baseline, 'running process changed before environment capture')
    raw = (base.PROC / str(baseline['pid']) / 'environ').read_bytes()
    require(len(raw) <= 1024 * 1024, 'process environment exceeds bound')
    result = {}
    for part in raw.split(b'\0'):
        if not part:
            continue
        require(b'=' in part, 'invalid process environment')
        key, value = part.split(b'=', 1)
        result[os.fsdecode(key)] = os.fsdecode(value)
    require(result, 'empty production process environment')
    return result


def migration_environment_facts():
    path = base.regular(MIGRATION_ENV)
    stat = path.stat()
    require(os.name != 'posix' or stat.st_uid == 0 and stat.st_mode & 0o777 == 0o600, 'migration environment must be root-owned mode 0600')
    return {'sha256': base.digest(path), 'root_owned_private': True}


def migration_environment(runtime):
    migration_environment_facts()
    values = {}
    for line in MIGRATION_ENV.read_text().splitlines():
        if not line or line.startswith('#'):
            continue
        require('=' in line and '\x00' not in line, 'unsupported migration environment syntax')
        key, value = line.split('=', 1)
        require(key == 'DATABASE_URL' and key not in values, 'migration environment must contain only DATABASE_URL')
        values[key] = value
    require(set(values) == {'DATABASE_URL'}, 'migration database configuration missing')
    try:
        migration = urllib.parse.urlsplit(values['DATABASE_URL'])
        current = urllib.parse.urlsplit(runtime.get('DATABASE_URL', ''))
        expected = ('127.0.0.1', 25433, '/codex_agent')
        valid = all(dsn.scheme in ('postgres', 'postgresql') and (dsn.hostname, dsn.port, dsn.path) == expected
                    and dsn.password and urllib.parse.parse_qs(dsn.query) == {'sslmode': ['disable']} and not dsn.fragment for dsn in (migration, current))
        valid = valid and migration.username == 'codex_agent_migrator' and current.username == 'codex_agent_runtime'
    except ValueError:
        valid = False
    require(valid, 'migration and runtime database targets or roles differ from the reviewed configuration')
    return {**runtime, **values}


def maintenance_role_facts():
    # Read-only metadata. Credentials never enter argv, SQL or a journal.
    sql = """SELECT json_build_object(
        'database', current_database(),
        'database_owner', (SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname=current_database()),
        'usage_owner', (SELECT pg_get_userbyid(relowner) FROM pg_class WHERE oid='usage_events'::regclass),
        'ledger_owner', (SELECT pg_get_userbyid(relowner) FROM pg_class WHERE oid='schema_migrations'::regclass),
        'migrator_restricted', (SELECT rolcanlogin AND NOT rolsuper AND NOT rolcreatedb AND NOT rolcreaterole FROM pg_roles WHERE rolname='codex_agent_migrator'),
        'schema_create', has_schema_privilege('codex_agent_migrator','public','CREATE'),
        'aggregate_permissions', (SELECT bool_and(has_table_privilege('codex_agent_migrator',name,'TRUNCATE') AND has_table_privilege('codex_agent_migrator',name,'INSERT') AND has_table_privilege('codex_agent_migrator',name,'DELETE')) FROM unnest(ARRAY['usage_minute','usage_daily','usage_totals']) name),
        'events_read', has_table_privilege('codex_agent_migrator','usage_events','SELECT'))"""
    raw = base.command('docker', 'exec', '-e', 'PGOPTIONS=-c statement_timeout=15000', 'codex-agent-postgres', 'psql', '-X', '-q', '-At', '-v', 'ON_ERROR_STOP=1', '-U', 'postgres', '-d', 'codex_agent', '-c', sql, timeout=45)
    facts = json.loads(raw)
    require(facts.get('database') == 'codex_agent' and all(facts.get(k) == 'codex_agent_migrator' for k in ('database_owner', 'usage_owner', 'ledger_owner'))
            and all(facts.get(k) is True for k in ('migrator_restricted', 'schema_create', 'aggregate_permissions', 'events_read')), 'dedicated migration role lacks reviewed schema or aggregate privileges')
    return facts


class CandidateCommandError(RuntimeError):
    def __init__(self, action, code, sqlstate=None):
        self.action, self.code, self.sqlstate = action, code, sqlstate
        super().__init__('candidate command failed: ' + action + ' (' + code + ')')


def candidate_command(binary, action, environment):
    require(action in ('migrate', 'rebuild-usage', 'runtime-status'), 'unsupported candidate command')
    try:
        result = subprocess.run([str(binary), action], env=environment, capture_output=True, timeout=1800 if action == 'rebuild-usage' else 90)
    except subprocess.TimeoutExpired:
        raise CandidateCommandError(action, 'timeout') from None
    if result.returncode:
        match = re.search(rb'\bSQLSTATE\s+([0-9A-Z]{5})\b', result.stderr[:65536])
        sqlstate = match.group(1).decode('ascii') if match else None
        raise CandidateCommandError(action, 'database_error' if sqlstate else 'process_exit', sqlstate)
    require(len(result.stdout) <= 65536, 'candidate command output exceeds limit')
    data = json.loads(result.stdout)
    require(isinstance(data, dict), 'candidate command did not return an object')
    if action != 'runtime-status':
        require(data == {('migrated' if action == 'migrate' else 'rebuilt'): True}, 'candidate storage command did not confirm completion')
    else:
        require(set(data) <= {'version', 'settings_version', 'inference_mode', 'inference', 'websocket_enabled'}, 'unexpected runtime status fields')
    return data


def health(release, manifest, environment):
    facts = base.health(manifest['files']['codex-agent'], release / 'frontend/dist', manifest['files']['frontend/dist/index.html'], expected_path=release / 'codex-agent')
    status = candidate_command(release / 'codex-agent', 'runtime-status', environment)
    require(status.get('version') == release_version(manifest.get('version')) and status.get('inference_mode') in MODES, 'running runtime settings mismatch')
    acceptance_path = release / 'acceptance.json'
    if acceptance_path.exists() and base.read_json(acceptance_path).get('verification_scope') == 'official_app_server':
        require(status.get('inference_mode') == 'official_app_server', 'limited verification requires active official_app_server mode')
    if acceptance_path.exists() and base.read_json(acceptance_path).get('verification_scope') == 'powerpoint_bps':
        require(status.get('inference_mode') == 'powerpoint_bps', 'release requires the verified active inference mode')
    require(status.get('inference', {}).get('backend') == status['inference_mode'], 'reported inference mode differs from backend')
    require(isinstance(status.get('websocket_enabled'), bool), 'runtime transport settings missing')
    limits = {name: base.command('systemctl', 'show', base.SERVICE, '-p', name, '--value') for name in ('TasksMax', 'MemoryHigh', 'MemoryMax', 'MemorySwapMax', 'CPUQuotaPerSecUSec')}
    require(limits == {'TasksMax': str(OFFICIAL_TASKS_MAX), 'MemoryHigh': str(OFFICIAL_MEMORY_HIGH), 'MemoryMax': str(OFFICIAL_MEMORY_MAX), 'MemorySwapMax': str(OFFICIAL_SWAP_MAX), 'CPUQuotaPerSecUSec': 'infinity'}, 'official runtime resource limits differ from candidate')
    facts['runtime_status'] = status
    facts['resource_limits'] = limits
    return facts


def rollback(state_dir):
    base.require_legacy_service_control()
    state = base.read_json(base.regular(state_dir / 'state.json'))
    require(environment_files() == state.get('environment_files'), 'environment files changed; rollback refused')
    actual = base.schema()
    require(actual in (state['schema_before'], state['schema_after_expected']), 'unreviewed schema change prevents rollback')
    state.update(migration_checksums=actual, schema_actual=actual, schema_migrated=actual == state['schema_after_expected'])
    base.write_json(state_dir / 'state.json', state)
    return base.rollback(state_dir)


def perform(action, release):
    # The stable ingress owns routing once installed. The legacy executor must
    # never stop its old codex-agent.service while that ingress is in use.
    require(action not in ('activate', 'rollback') or not Path('/var/lib/codex-agent-release/active-runtime.json').exists(),
            'stable ingress installed; use zero_downtime_release.py for activation or rollback')
    release = base.release_path(release)
    state_dir = base.state_directory(release)
    state_path = state_dir / 'state.json'
    if action == 'rollback':
        return {'rolled_back': True, 'runtime': rollback(state_dir), 'database_restored': False, 'schema_actual': base.schema()}
    if action == 'status':
        return {'runtime': base.current(), 'schema_actual': base.schema(), 'state': base.read_json(state_path) if state_path.exists() else None}
    manifest = validate(release)
    if action == 'stage-cli':
        return {'official_cli_staged': str(stage_cli(manifest['official_cli']))}
    if state_path.exists() and base.read_json(state_path).get('phase') in ('switching', 'rolling_back', 'rollback_failed'):
        rollback(state_dir)
        raise RuntimeError('interrupted activation recovered; review schema before a new release')
    legacy = {k: v for k, v in manifest['migration_checksums'].items() if k != ADDED}
    before = base.schema()
    require(before in (legacy, manifest['migration_checksums']), 'migration ledger must contain unchanged first five or exact reviewed six migrations')
    require(manifest['version'] == '0.4.0' or before == manifest['migration_checksums'], 'patch release requires the exact existing six migrations')
    verify_bundle(cli_destination(manifest['official_cli']), manifest['official_cli'])
    baseline = base.current()
    migration_facts = migration_environment_facts()
    runtime_check = process_environment(baseline)
    maintenance_check = {}
    try:
        maintenance_check = migration_environment(runtime_check)
        role_facts = maintenance_role_facts()
        if base.read_json(base.artifact(release, 'acceptance.json')).get('verification_scope') == 'official_app_server':
            status = candidate_command(release / 'codex-agent', 'runtime-status', runtime_check)
            require(status.get('inference_mode') == 'official_app_server', 'limited verification requires active official_app_server mode')
        if base.read_json(base.artifact(release, 'acceptance.json')).get('verification_scope') == 'powerpoint_bps':
            status = candidate_command(release / 'codex-agent', 'runtime-status', runtime_check)
            require(status.get('inference_mode') == 'powerpoint_bps', 'release requires the verified active inference mode')
    finally:
        runtime_check.clear()
        maintenance_check.clear()
    original = base.regular(base.UNIT).read_text(encoding='utf-8')
    candidate = candidate_unit(original, baseline, release, manifest['official_cli'])
    freeze = {'baseline': baseline, 'schema_before': before, 'schema_after_expected': manifest['migration_checksums'],
              'environment_files': environment_files(),
              'migration_environment': migration_facts, 'maintenance_role': role_facts,
              'manifest_sha256': base.digest(base.artifact(release, 'manifest.json')), 'candidate_binary_sha256': manifest['files']['codex-agent'],
              'candidate_unit_sha256': hashlib.sha256(candidate.encode()).hexdigest()}
    if action == 'preflight':
        base.stop_timeout(base.command('systemctl', 'show', base.SERVICE, '-p', 'TimeoutStopUSec', '--value'))
        base.write_json(state_dir / 'preflight.json', freeze)
        return {'preflight': True, **freeze, 'schema_change': {'add': [ADDED] if before == legacy else [], 'rollback_keeps_nullable_column': True}}
    require(action == 'activate', 'invalid release action')
    require(base.read_json(base.regular(state_dir / 'preflight.json')) == freeze, 'runtime, schema or artifacts changed since preflight')
    environment = process_environment(baseline)  # Never persisted or printed.
    maintenance = migration_environment(environment)
    require(migration_environment_facts() == migration_facts, 'migration environment changed before draining')
    base.atomic_write(state_dir / 'previous.service', original)
    state = dict(freeze, phase='switching', schema_actual=before, usage_rebuilt=False)
    base.write_json(state_path, state)
    stage = 'stop'
    try:
        base.stop_service()
        stage = 'configuration'
        require(base.digest(base.regular(base.UNIT)) == baseline['unit_sha256'] and base.digest(base.regular(base.ENV)) == baseline['env_sha256'], 'runtime configuration changed while draining')
        require(base.auxiliary_units() == baseline.get('auxiliary_units', {}), 'auxiliary configuration changed while draining')
        require(environment_files() == freeze['environment_files'], 'environment files changed while draining')
        require(migration_environment_facts() == migration_facts, 'migration environment changed while draining')
        stage = 'artifacts'
        require(base.digest(base.artifact(release, 'manifest.json')) == freeze['manifest_sha256'], 'manifest changed while draining')
        validate(release)
        verify_bundle(cli_destination(manifest['official_cli']), manifest['official_cli'])
        stage = 'schema'
        require(base.schema() == before, 'schema changed while draining')
        stage = 'migrate'
        candidate_command(release / 'codex-agent', 'migrate', maintenance)
        actual = base.schema()
        require(actual == manifest['migration_checksums'], 'candidate migration ledger differs')
        state.update(schema_actual=actual, schema_migrated=True)
        base.write_json(state_path, state)
        stage = 'rebuild-usage'
        candidate_command(release / 'codex-agent', 'rebuild-usage', maintenance)
        state['usage_rebuilt'] = True
        base.write_json(state_path, state)
        stage = 'unit_write'
        base.atomic_write(base.UNIT, candidate, 0o644)
        stage = 'reload'
        base.command('systemctl', 'daemon-reload')
        stage = 'start'
        base.command('systemctl', 'start', base.SERVICE)
        stage = 'health'
        facts = health(release, manifest, environment)
        stage = 'post_configuration'
        require(base.digest(base.regular(base.UNIT)) == freeze['candidate_unit_sha256'] and base.auxiliary_units() == baseline.get('auxiliary_units', {}) and base.digest(base.regular(base.ENV)) == baseline['env_sha256'], 'runtime configuration changed during activation')
        require(environment_files() == freeze['environment_files'], 'environment files changed during activation')
        require(migration_environment_facts() == migration_facts, 'migration environment changed during activation')
        stage = 'post_schema'
        require(base.schema() == manifest['migration_checksums'], 'schema changed during activation')
        state.update(phase='active', runtime=facts, database_restored=False, environment_restored=False)
        base.write_json(state_path, state)
        return {'deployed': True, 'runtime': facts, 'schema_actual': state['schema_actual'], 'usage_rebuilt': True}
    except Exception as error:
        state['activation_failure'] = base.failure_diagnostic(stage, error)
        if stage in ('migrate', 'rebuild-usage'):
            state['activation_failure']['stage'] = stage
        if isinstance(error, CandidateCommandError):
            state['activation_failure'].update(command=error.action, code=error.code)
            if error.sqlstate:
                state['activation_failure']['sqlstate'] = error.sqlstate
        try:
            base.write_json(state_path, state)
        except Exception:
            pass
        rollback(state_dir)
        raise RuntimeError('candidate failed and old binary was restored; reviewed nullable migration and user data retained') from None
    finally:
        environment.clear()
        maintenance.clear()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action', choices=['stage-cli', 'preflight', 'activate', 'rollback', 'status'])
    parser.add_argument('--release', required=True)
    args = parser.parse_args()
    with base.deployment_lock():
        base.configure_effective_unit()
        result = perform(args.action, Path(os.path.abspath(args.release)))
    print(json.dumps(result))


if __name__ == '__main__':
    main()
