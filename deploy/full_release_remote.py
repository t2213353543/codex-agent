#!/usr/bin/env python3
"""Journaled full Codex release with native transport and writer isolation.

Unlike the legacy v2 publisher this accepts a newly verified UI, requires the
already-applied 003 schema, and never automatically runs an incompatible old
writer after the candidate has started. No database restoration is performed.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import subprocess
import time
import urllib.error
import urllib.request

import upgrade_remote as base

SCHEMA = 'codex-agent-full-release-v1'
BRIDGE = 'codex-agent-workbench-bridge'
NATIVE_KEYS = ('CODEX_NATIVE_HTTP_HELPER', 'CODEX_NATIVE_HTTP_SHA256',
               'CODEX_NATIVE_WEBSOCKET', 'CODEX_NATIVE_HTTP_LIFECYCLE')


def require(value, message):
    if not value:
        raise base.UpgradeError(message)


def runtime_ca_readable(path, *, directory=False):
    """Allow distribution CA symlinks without hiding inaccessible link parents."""
    pending = [(path, directory, True)]
    checked = set()
    try:
        while pending:
            candidate, is_directory, needs_read = pending.pop()
            if (candidate, is_directory, needs_read) in checked:
                continue
            checked.add((candidate, is_directory, needs_read))
            require(candidate.is_absolute(), 'CA paths must be absolute')
            require(candidate.is_dir() if is_directory else candidate.is_file(),
                    'Configured CA path unavailable')
            resolved = candidate.resolve(strict=True)
            if is_directory:
                required = 0o005 if needs_read else 0o001
                require(resolved.stat().st_mode & required == required,
                        'DynamicUser cannot read or traverse the CA directory')
            else:
                base.runtime_readable(resolved)
            # Checking only resolve() loses permissions on the original link's
            # ancestors and on intermediate links outside the resolved path.
            for component in (candidate, *candidate.parents, *resolved.parents):
                component_is_directory = component != candidate or is_directory
                if component_is_directory:
                    require(component.is_dir() and component.stat().st_mode & 0o001,
                            'DynamicUser cannot traverse the CA directory')
                if component.is_symlink():
                    target = Path(os.readlink(component))
                    if not target.is_absolute():
                        target = component.parent / target
                    pending.append((target, component_is_directory, needs_read and component == candidate))
    except base.UpgradeError:
        raise
    except (OSError, RuntimeError) as error:
        raise base.UpgradeError('Configured CA path cannot be resolved or read') from error


def native_environment(original, release, manifest):
    values = {}
    for line in original.decode().splitlines():
        if line and not line.startswith('#'):
            require('=' in line, 'Unsupported runtime environment syntax')
            key, value = line.split('=', 1)
            require(key not in values, 'Duplicate runtime environment')
            values[key] = value
    values.update(CODEX_NATIVE_HTTP_HELPER=str(release / 'native/codex-native-transport'),
                  CODEX_NATIVE_HTTP_SHA256=manifest['native_sha256'],
                  CODEX_NATIVE_WEBSOCKET='1', CODEX_NATIVE_HTTP_LIFECYCLE='cli_request')
    # Match official custom_ca.rs: the first nonempty value selects the bundle.
    # Any selected bundle forces HTTP rustls, even a system ca-bundle.crt. Never
    # inject SSL_CERT_FILE for default trust or delete an operator's existing
    # value by guessing from its pathname. Legacy injected values need a separate
    # migration with verified deployment provenance.
    selected_ca = values.get('CODEX_CA_CERTIFICATE') or values.get('SSL_CERT_FILE')
    if selected_ca:
        runtime_ca_readable(Path(selected_ca))
    # SSL_CERT_DIR can configure OpenSSL and rustls native roots without selecting
    # HTTP's custom-CA backend. Preserve it verbatim, including platform path-list
    # syntax. Like rustls-native-certs, inspect immediate files, not subdirectories.
    if values.get('SSL_CERT_DIR'):
        for name in values['SSL_CERT_DIR'].split(os.pathsep):
            directory = Path(name)
            runtime_ca_readable(directory, directory=True)
            for entry in directory.iterdir():
                if entry.is_file():
                    runtime_ca_readable(entry)
    # The gateway's real helper startup validates PEM and builds both HTTP and WS
    # configurations before readiness; file readability alone is not TLS proof.
    return ''.join(key + '=' + value + '\n' for key, value in values.items()).encode()


def sql(statement):
    raw = base.command(['docker', 'exec', '-e', 'PGOPTIONS=-c statement_timeout=15000',
                        base.PG_CONTAINER, 'psql', '-X', '-q', '-At', '-v', 'ON_ERROR_STOP=1',
                        '-U', 'postgres', '-d', 'codex_agent', '-c', statement], timeout=45)
    return json.loads(raw)


def data_summary():
    # Only counts and digests leave the server; encrypted secrets remain there.
    return sql("""SELECT json_build_object(
 'accounts',(SELECT count(*) FROM accounts),
 'credentials',(SELECT count(*) FROM account_credentials),
 'groups',(SELECT count(*) FROM account_groups),
 'proxies',(SELECT count(*) FROM account_proxies),
 'events',(SELECT count(*) FROM usage_events),
 'logs',(SELECT count(*) FROM request_logs),
 'tokens',COALESCE((SELECT tokens FROM usage_totals WHERE scope='global' AND subject=''),0),
 'account_state',md5(COALESCE((SELECT string_agg(to_jsonb(a)::text,'' ORDER BY id) FROM accounts a),'')),
 'credential_state',md5(COALESCE((SELECT string_agg(to_jsonb(c)::text,'' ORDER BY account_id) FROM account_credentials c),'')))""")


def matching_processes():
    result = []
    for folder in Path('/proc').iterdir():
        if not folder.name.isdigit():
            continue
        try:
            executable = os.readlink(folder / 'exe')
            arguments = (folder / 'cmdline').read_bytes().split(b'\0')
        except OSError:
            continue
        if executable.startswith('/opt/codex-agent/') and Path(executable).name == 'codex-agent':
            result.append({'pid': int(folder.name), 'executable': executable,
                           'mode': 'server' if b'--listen' in arguments else 'other'})
    return result


def assert_single_writer(pid):
    processes = matching_processes()
    require(len(processes) == 1 and processes[0]['pid'] == int(pid),
            'Unexpected additional Codex server or CLI writer')


def validate(release):
    manifest = json.loads(base.regular(release / 'full-release.json').read_text())
    require(manifest['schema'] == SCHEMA and manifest['release_id'] == release.name,
            'Invalid full release identity')
    identity = {key: value for key, value in manifest.items() if key != 'release_id'}
    require(base.bytes_sha(json.dumps(identity, sort_keys=True).encode()) == release.name,
            'Full release identity does not bind artifacts')
    required_files = {'codex-agent', 'native/codex-native-transport', 'evidence/acceptance.json',
                      'evidence/source-manifest.json', 'deploy/upgrade_remote.py',
                      'deploy/full_release_remote.py', *base.TARGET_MIGRATIONS}
    required_files.update('frontend/dist/' + name for name in manifest['frontend'])
    require(required_files <= manifest['files'].keys(), 'Release manifest omits required evidence or artifacts')
    for name, digest in manifest['files'].items():
        base.relative(name)
        require(isinstance(digest, str) and base.SHA.fullmatch(digest), 'Invalid artifact digest')
        require(base.sha(base.regular(release / name)) == digest, 'Release file hash mismatch')
    require(base.file_map(release / 'frontend/dist') == manifest['frontend'], 'Frontend file set mismatch')
    require({name: base.sha(release / name) for name in base.TARGET_MIGRATIONS} == base.TARGET_MIGRATIONS,
            'Migration bytes changed')
    require(manifest['migration_checksums'] == base.TARGET_MIGRATIONS, 'Wrong schema contract')
    require(manifest.get('native_enabled') is True, 'Native activation was not bound')
    for name in ('codex-agent', 'native/codex-native-transport'):
        base.runtime_readable(base.regular(release / name), executable=True)
    require(base.sha(release / 'codex-agent') == manifest['binary_sha256'], 'Executable mismatch')
    require(base.sha(release / 'native/codex-native-transport') == manifest['native_sha256'], 'Native helper mismatch')
    acceptance = json.loads((release / 'evidence/acceptance.json').read_text())
    require(acceptance.get('passed') is True and acceptance['source_sha256'] == manifest['source_sha256']
            and acceptance['binary_sha256'] == manifest['binary_sha256']
            and acceptance['frontend'] == manifest['frontend'], 'Acceptance does not bind release')
    source = json.loads((release / 'evidence/source-manifest.json').read_text())
    require(source.get('sha256') == manifest['source_sha256'] and isinstance(source.get('files'), dict),
            'Source evidence differs from release')
    files = source['files']
    for name, digest in files.items():
        base.relative(name)
        require(isinstance(digest, str) and base.SHA.fullmatch(digest), 'Invalid source artifact digest')
    source_hashes = {
        base.bytes_sha(json.dumps(files, sort_keys=True).encode()),
        base.bytes_sha(json.dumps(files, sort_keys=True, separators=(',', ':'), ensure_ascii=False).encode()),
    }
    require(manifest['source_sha256'] in source_hashes, 'Source digest does not bind its file map')
    require({name: files.get(name) for name in base.TARGET_MIGRATIONS} == base.TARGET_MIGRATIONS,
            'Source evidence migration bytes differ')
    return manifest


class FullRelease:
    def __init__(self, release):
        self.release = base.scoped_release(release)
        self.manifest = validate(self.release)
        self.private = self.release / '.full-private'
        self.private.mkdir(mode=0o700, exist_ok=True)
        base.directory(self.private)
        os.chmod(self.private, 0o700)
        self.state_path = self.private / 'state.json'
        self.state = json.loads(base.regular(self.state_path).read_text()) if self.state_path.exists() else {
            'schema': SCHEMA, 'release_id': release.name, 'phase': 'staged', 'deployed': False,
            'database_restored': False, 'active_probe_requests': 0}
        require(self.state.get('schema') == SCHEMA and self.state.get('release_id') == release.name,
                'Saved release state belongs to another release')

    def save(self, phase, **updates):
        self.state.update(phase=phase, updated_at=base.now(), **updates)
        base.write_json(self.state_path, self.state)

    def preflight(self):
        require(base.read_schema_checksums() == base.TARGET_MIGRATIONS, 'Expected exact already-applied schema')
        original = base.regular(base.UNIT).read_bytes()
        parsed = base.parse_unit(original)
        current = base.properties()
        require(current['ActiveState'] == 'active', 'Previous service is not active')
        assert_single_writer(current['MainPID'])
        expected = self.manifest['baseline']
        require(base.sha(parsed['binary']) == expected['binary_sha256']
                and base.bytes_sha(original) == expected['unit_sha256']
                and base.file_map(parsed['web']) == expected['frontend'], 'Live baseline drifted')
        environment = base.environment_facts()
        require(environment['sha256'] == expected['environment_sha256'], 'Runtime environment drifted')
        updated_env = native_environment(base.RUNTIME_ENV.read_bytes(), self.release, self.manifest)
        require(updated_env, 'Native environment invalid')
        health = base.verify_health(parsed['binary'], parsed['web'], expected['binary_sha256'], expected['frontend'])
        self.save('preflight_passed', baseline_health=health, baseline_pid=int(current['MainPID']),
                  before_preflight=data_summary(), native_requested=True)
        return self.state

    def verify(self):
        manifest = self.manifest
        health = base.verify_health(self.release / 'codex-agent', self.release / 'frontend/dist',
                                    manifest['binary_sha256'], manifest['frontend'], timeout=60)
        pid = health['main_pid']
        assert_single_writer(pid)
        values = dict(item.split(b'=', 1) for item in (Path('/proc') / str(pid) / 'environ').read_bytes().split(b'\0') if b'=' in item)
        require(values.get(b'CODEX_NATIVE_HTTP_HELPER') == str(self.release / 'native/codex-native-transport').encode()
                and values.get(b'CODEX_NATIVE_HTTP_SHA256') == manifest['native_sha256'].encode()
                and values.get(b'CODEX_NATIVE_WEBSOCKET') == b'1'
                and values.get(b'CODEX_NATIVE_HTTP_LIFECYCLE') == b'cli_request', 'Native environment not active')
        # Startup capability validation in configureGatewayNativeHTTP must have
        # returned successfully before the service could expose readiness.
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
        for name, digest in manifest['frontend'].items():
            with opener.open(base.BASE_URL + ('/' if name == 'index.html' else '/' + name), timeout=10) as response:
                require(base.bytes_sha(response.read(16 << 20)) == digest, 'Served frontend differs')
        key = Path('/var/lib/codex-agent/client-key').read_text().strip()
        api_checks = {}
        websocket_route_enabled = False
        routes = ['/api/session', '/api/accounts/manage?page_size=1', '/api/logs?exact_total=true&page_size=1',
                  '/api/logs/stats', '/api/ingress-events?limit=1', '/api/settings']
        for route in routes:
            request = urllib.request.Request(base.BASE_URL + route, headers={'Authorization': 'Bearer ' + key})
            with opener.open(request, timeout=20) as response:
                payload = json.loads(response.read(8 << 20))
                require(response.status == 200, 'Management API check failed')
                if route.startswith('/api/ingress-events'):
                    require(payload['collector']['configured'] and payload['collector']['running'], 'Ingress collector is not running')
                if route == '/api/settings':
                    websocket_route_enabled = payload.get('websocket_enabled') is True
                api_checks[route.split('?')[0]] = 200
        for route in ['/api/logs/pricing', '/api/logs/cost-rules', '/api/logs/sale-policy', '/api/logs/cost-policy']:
            try:
                opener.open(urllib.request.Request(base.BASE_URL + route, headers={'Authorization': 'Bearer ' + key}), timeout=10)
                raise base.UpgradeError('Retired billing endpoint still active')
            except urllib.error.HTTPError as error:
                require(error.code == 404, 'Retired route status mismatch')
        return {'health': health, 'native_http': True, 'native_websocket': True,
                'websocket_route_enabled': websocket_route_enabled,
                'helper_sha256': manifest['native_sha256'], 'frontend_files_verified': len(manifest['frontend']),
                'management_checks': api_checks, 'billing_routes_disabled': True,
                'active_upstream_requests': 0}

    def activate(self):
        if self.state['phase'] == 'deployed':
            return {**self.state, 'verification': self.verify(), 'reused': True}
        require(self.state['phase'] in ('staged', 'preflight_passed'), 'Interrupted release requires state review')
        self.preflight()
        original = base.UNIT.read_bytes()
        original_env = base.RUNTIME_ENV.read_bytes()
        # Existing journaled backup retains a private recovery snapshot. It is
        # not restored as a downgrade, which could erase fresh token rotations.
        backup = base.Transaction(self.release)
        backup.backup(original)
        base.atomic_write(self.private / 'previous.service', original)
        base.atomic_write(self.private / 'previous.env', original_env)
        timer_state = base.system('show', BRIDGE + '.timer', '-p', 'ActiveState', '--value')
        require(timer_state in ('active', 'inactive', 'failed'), 'Unexpected bridge timer state')
        was_timer = timer_state == 'active'
        self.save('writers_pausing', bridge_timer_was_active=was_timer)
        started_candidate = False
        writer_isolation_verified = False
        new_env = None
        new_unit = None
        try:
            base.system('stop', BRIDGE + '.timer')
            # Do not terminate an in-progress credential synchronization.
            deadline = time.monotonic() + 190
            while base.system('show', BRIDGE + '.service', '-p', 'MainPID', '--value') != '0':
                require(time.monotonic() < deadline, 'Synchronization did not drain')
                time.sleep(0.5)
            self.save('draining', write_freeze_started_at=base.now())
            base.stop_service()
            require(not matching_processes(), 'Another old writer remains')
            runtime_sessions = sql("SELECT COALESCE(json_agg(json_build_object('pid',pid,'state',state)), '[]'::json) FROM pg_stat_activity WHERE datname='codex_agent' AND usename='codex_agent_runtime'")
            require(not runtime_sessions, 'Other runtime database writers remain connected')
            writer_isolation_verified = True
            require(base.UNIT.read_bytes() == original and base.RUNTIME_ENV.read_bytes() == original_env, 'Configuration changed during drain')
            self.save('drained', data_before=data_summary())
            # Recheck content hashes after draining, before publishing a unit.
            validate(self.release)
            new_env = native_environment(original_env, self.release, self.manifest)
            new_unit = base.plan_unit(original, self.release)
            base.atomic_write(base.RUNTIME_ENV, new_env)
            base.atomic_write(base.UNIT, new_unit, 0o644)
            self.save('starting_candidate', runtime_environment_sha256=base.sha(base.RUNTIME_ENV))
            base.system('daemon-reload')
            base.system('reset-failed', base.SERVICE)
            # Any attempted start may reach a management writer. Once attempted,
            # never invoke the incompatible original binary automatically.
            started_candidate = True
            base.system('start', base.SERVICE)
            verification = self.verify()
            after = data_summary()
            before = self.state['data_before']
            require(after['accounts'] == before['accounts'] and after['credentials'] == before['credentials']
                    and after['events'] >= before['events'] and after['tokens'] >= before['tokens'], 'Persistent data comparison failed')
            self.save('deployed', deployed=True, deployed_at=base.now(), verification=verification,
                      data_after=after, write_freeze_ended_at=base.now(),
                      rollback_policy='Do not start original old writer against upgraded state; native may be disabled with this compatible binary during recovery.')
        except Exception:
            if not started_candidate:
                if not writer_isolation_verified:
                    self.save('prestart_recovery_requires_writer_review', deployed=False,
                              old_writer_not_started=True)
                    raise base.UpgradeError('Writer isolation was not verified; automatic baseline restart refused')
                if (base.RUNTIME_ENV.read_bytes() not in (original_env, new_env)
                        or base.UNIT.read_bytes() not in (original, new_unit)):
                    self.save('prestart_recovery_refused_config_drift', deployed=False,
                              old_writer_not_started=True)
                    raise base.UpgradeError('Configuration changed outside this release; baseline recovery refused')
                base.atomic_write(base.RUNTIME_ENV, original_env)
                base.atomic_write(base.UNIT, original, 0o644)
                base.system('daemon-reload')
                base.system('start', base.SERVICE)
                self.save('prestart_failed_baseline_restored', deployed=False)
            else:
                self.save('candidate_needs_recovery', deployed=False, old_writer_not_started=True)
            raise
        finally:
            if was_timer and self.state['phase'] in ('deployed', 'prestart_failed_baseline_restored'):
                base.system('start', BRIDGE + '.timer')
        return self.state


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action', choices=('preflight', 'activate', 'verify'))
    parser.add_argument('--release', type=Path, required=True)
    args = parser.parse_args()
    require(os.geteuid() == 0, 'Root is required')
    with base.lock():
        release = FullRelease(args.release)
        result = getattr(release, args.action)()
    print(json.dumps(result))


if __name__ == '__main__':
    main()
