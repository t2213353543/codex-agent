"""Validate immutable management-performance releases; never deploy or migrate.

Functional verification and performance SLO attainment are separate facts. A
release may truthfully retain unresolved latency targets, but it must carry
passing functional evidence and the exact reviewed schema/artifact inventory.
"""
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import re
import sys

sys.dont_write_bytecode = True
_spec = importlib.util.spec_from_file_location('performance_release_base', Path(__file__).with_name('powerpoint_release.py'))
base = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(base)
require = base.require

SCHEMA = 'codex-agent-performance-release-v1'
ACCEPTANCE_SCHEMA = 'codex-agent-performance-acceptance-v1'
BACKEND = 'management_performance'
MIGRATION_NAMES = frozenset('migrations/' + name for name in (
    '000_core.sql', '001_accounts.sql', '002_metrics.sql',
    '003_management_cleanup.sql', '004_account_credential_request_state.sql',
    '005_usage_latency_known.sql', '006_metrics_query_indexes.sql',
    '007_metrics_hourly_global.sql', '008_retained_log_rollup.sql',
    '009_retained_usage_dimensions.sql', '010_retained_usage_daily.sql',
    '011_retained_operations.sql', '012_account_top_day.sql',
    '013_retained_account_identity_guard.sql', '014_request_log_filter_tags.sql',
    '015_request_log_filter_indexes.sql', '016_request_log_status_sort.sql',
    '017_retained_usage_model.sql', '018_proxy_resource_write_gate.sql',
    '019_request_log_error_classes_function.sql', '020_request_log_error_class_index.sql',
    '021_request_log_detached_account_history.sql', '022_request_log_error_classes_decode.sql',
    '023_request_log_error_class_scalar_indexes.sql', '024_request_log_analytics.sql',
    '025_request_log_error_classes_plpgsql.sql', '026_gateway_response_history.sql', '027_account_drains.sql'))
CONTROLLERS = frozenset(('deploy/performance_release.py', 'deploy/powerpoint_release.py',
                         'deploy/zero_downtime_release.py'))
REQUIRED = CONTROLLERS | MIGRATION_NAMES | frozenset((
    'codex-agent', 'frontend/dist/index.html', 'source-manifest.json', 'acceptance.json'))
OPTIONAL = frozenset(('codex-agent-ingress', 'deploy/reliability_release.py'))
FUNCTIONAL_CHECKS = frozenset(('go', 'vet', 'pg', 'frontend'))


def map_digest(value):
    return hashlib.sha256(json.dumps(value, sort_keys=True).encode()).hexdigest()


def release_identity(manifest):
    return map_digest({name: value for name, value in manifest.items()
                       if name not in ('release_id', 'built_at')})


def valid_hash(value):
    return isinstance(value, str) and base.HASH.fullmatch(value)


def validate_references(references, files):
    require(isinstance(references, list) and references, 'verification evidence references missing')
    found = set()
    for reference in references:
        require(isinstance(reference, dict), 'invalid evidence reference')
        name, checksum = reference.get('artifact_path'), reference.get('sha256')
        base.relative_name(name)
        require(name.startswith('evidence/') and valid_hash(checksum)
                and files.get(name) == checksum, 'verification evidence artifact mismatch')
        require(name not in found, 'duplicate evidence reference')
        found.add(name)
    return found


def validate_acceptance(acceptance, source, files):
    require(acceptance.get('schema') == ACCEPTANCE_SCHEMA, 'incorrect performance acceptance schema')
    require(acceptance.get('source_sha256') == source['sha256'], 'acceptance source mismatch')
    require(acceptance.get('binary_sha256') == files['codex-agent'], 'acceptance binary mismatch')
    known = validate_references(acceptance.get('evidence_files'), files)
    require(known == {name for name in files if name.startswith('evidence/')},
            'unreferenced verification evidence artifact')
    functional = acceptance.get('functional')
    require(isinstance(functional, dict) and functional.get('passed') is True,
            'functional verification did not pass')
    checks = functional.get('checks')
    require(isinstance(checks, dict) and set(checks) >= FUNCTIONAL_CHECKS
            and all(value is True for value in checks.values()), 'functional checks missing or failed')
    require(validate_references(functional.get('evidence_files'), files) <= known,
            'functional evidence is not part of acceptance')
    performance = acceptance.get('performance')
    require(isinstance(performance, dict) and type(performance.get('slo_met')) is bool,
            'performance SLO status must be explicit')
    remaining = performance.get('remaining')
    require(isinstance(remaining, list) and all(isinstance(item, str) and item.strip() for item in remaining),
            'performance limitations must be explicit')
    require((not remaining) == performance['slo_met'], 'performance SLO status contradicts remaining work')
    require(validate_references(performance.get('evidence_files'), files) <= known,
            'performance evidence is not part of acceptance')
    frontend = acceptance.get('frontend')
    require(isinstance(frontend, dict) and frontend.get('artifact_files') == {
        name: digest for name, digest in files.items() if name.startswith('frontend/dist/')},
        'frontend artifact evidence mismatch')


def validate(release):
    release = base.release_path(release)
    return validate_package(release, release.name)


def validate_package(release, expected_release_id):
    """Validate a local prepared directory with the same immutable contract."""
    release = Path(release)
    require(release.is_absolute() and release.is_dir() and not release.is_symlink()
            and not any(parent.is_symlink() for parent in release.parents), 'invalid release directory')
    manifest = base.read_json(base.artifact(release, 'manifest.json'))
    require(manifest.get('schema') == SCHEMA and manifest.get('backend') == BACKEND,
            'incorrect management performance release')
    require(isinstance(manifest.get('version'), str)
            and re.fullmatch(r'0\.4\.(?:0|[1-9][0-9]*)', manifest['version']), 'unsupported release version')
    require(manifest.get('release_id') == expected_release_id == release_identity(manifest),
            'release identity mismatch')
    files = manifest.get('files')
    require(isinstance(files, dict) and REQUIRED <= set(files) and 'manifest.json' not in files,
            'performance release incomplete')
    for name, expected in files.items():
        base.relative_name(name)
        require(name in REQUIRED | OPTIONAL or name.startswith(('frontend/dist/', 'evidence/')),
                'unexpected performance release artifact')
        require(valid_hash(expected), 'invalid artifact digest')
        path = base.artifact(release, name)
        require(base.digest(path) == expected, 'performance artifact checksum mismatch')
        if os.name == 'posix':
            require(path.stat().st_mode & 0o004 and all(p.stat().st_mode & 0o001 for p in path.parents),
                    'runtime cannot read performance artifact')
    actual = set()
    for path in release.rglob('*'):
        require(not path.is_symlink() and (path.is_file() or path.is_dir()), 'invalid release member')
        if path.is_file():
            actual.add(path.relative_to(release).as_posix())
    require(actual == set(files) | {'manifest.json'}, 'unmanifested performance release artifact')
    for name in ('codex-agent', 'codex-agent-ingress'):
        if name not in files:
            continue
        path = base.artifact(release, name)
        with path.open('rb') as handle:
            header = handle.read(20)
        require(len(header) == 20 and header[:6] == b'\x7fELF\x02\x01'
                and header[18:20] == b'\x3e\x00', 'candidate is not Linux amd64 ELF')
        require(os.name != 'posix' or path.stat().st_mode & 0o001, 'candidate is not executable')
    source = base.read_json(base.artifact(release, 'source-manifest.json'))
    source_files = source.get('files')
    require(isinstance(source_files, dict) and source_files, 'source manifest file map missing')
    for name, checksum in source_files.items():
        parts = base.relative_name(name).parts
        require(not {'.local', 'data', '.git', '__pycache__', 'node_modules'}.intersection(parts)
                and valid_hash(checksum), 'invalid source manifest entry')
    require({'main.go', 'go.mod', 'go.sum', 'internal/server/main.go', 'internal/server/database.go',
             'migrations/embed.go'} <= set(source_files), 'runtime source manifest incomplete')
    require(source.get('sha256') == manifest.get('source_sha256') == map_digest(source_files),
            'source identity mismatch')
    for name in CONTROLLERS | (OPTIONAL & set(files) - {'codex-agent-ingress'}):
        require(source_files.get(name) == files[name], 'controller source mismatch')
    migrations = manifest.get('migration_checksums')
    require(isinstance(migrations, dict) and set(migrations) == MIGRATION_NAMES,
            'performance release requires the exact reviewed 28 migrations')
    require(migrations == {name: checksum for name, checksum in source_files.items()
                           if name.startswith('migrations/') and name.endswith('.sql')},
            'migration source mismatch')
    require(all(files[name] == checksum for name, checksum in migrations.items()),
            'migration artifact mismatch')
    validate_acceptance(base.read_json(base.artifact(release, 'acceptance.json')), source, files)
    return manifest
