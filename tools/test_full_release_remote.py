"""Full-release guards and failure recovery. Pure local mocks; no SSH/systemd/PG."""
import contextlib
import importlib.util
import json
import os
from pathlib import Path
import sys
import tempfile
import types
import unittest
from unittest.mock import Mock, patch


ROOT = Path(__file__).resolve().parents[1]
SPEC = importlib.util.spec_from_file_location('upgrade_remote', ROOT / 'deploy/upgrade_remote.py')
base = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(base)
with patch.dict(sys.modules, {'upgrade_remote': base}):
    SPEC = importlib.util.spec_from_file_location('full_release_remote', ROOT / 'deploy/full_release_remote.py')
    release = importlib.util.module_from_spec(SPEC)
    SPEC.loader.exec_module(release)
with patch.dict(sys.modules, {
    'integration_env': types.SimpleNamespace(ROOT=ROOT, connect=Mock(), run=Mock()),
    'deploy': types.SimpleNamespace(put=Mock()),
}):
    SPEC = importlib.util.spec_from_file_location('full_release_local', ROOT / 'tools/full_release.py')
    local = importlib.util.module_from_spec(SPEC)
    SPEC.loader.exec_module(local)


class FullReleaseStateTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.unit = self.root / 'service'
        self.env = self.root / 'production.env'
        self.unit.write_bytes(b'OLD UNIT\n')
        self.env.write_bytes(b'EXISTING=unchanged\n')
        self.transaction = release.FullRelease.__new__(release.FullRelease)
        self.transaction.release = self.root / ('b' * 64)
        self.transaction.private = self.root / 'private'
        self.transaction.private.mkdir()
        self.transaction.manifest = {'native_sha256': 'c' * 64}
        self.transaction.state = {'phase': 'staged'}
        self.calls = []
        self.atomic_count = 0

    def activate(self, *, fail=None, external_change=False, timer_state='active', extra_writer=False):
        def save(phase, **fields):
            self.transaction.state.update(fields, phase=phase)
            self.calls.append(('save', phase))

        def atomic(path, data, mode=0o600):
            self.calls.append(('write', Path(path).name, data))
            if fail == 'unit-write' and path == self.unit and data == b'NEW UNIT\n':
                raise base.UpgradeError('synthetic atomic unit failure')
            Path(path).write_bytes(data)

        def system(*args):
            self.calls.append(('system', *args))
            if args[0] in ('is-active', 'show'):
                return '0' if '.service' in args[1] else timer_state
            if fail == 'start' and args == ('start', base.SERVICE):
                raise base.UpgradeError('synthetic candidate start failure')
            return ''

        def stopped():
            self.calls.append(('drain',))
            if external_change:
                self.env.write_bytes(b'OPERATOR=keep-this-change\n')

        def verify():
            if fail == 'verify':
                raise base.UpgradeError('synthetic candidate verification failure')
            return {'native_http': True, 'native_websocket': True}

        def validate(_):
            self.calls.append(('validate',))
            if fail == 'artifact':
                raise base.UpgradeError('synthetic changed artifact')
            return self.transaction.manifest

        summary = {'accounts': 2, 'credentials': 2, 'events': 3, 'tokens': 4}
        def sessions_sql(statement):
            self.assertIn("COALESCE(json_agg", statement)
            self.assertIn("'[]'::json", statement)
            return []
        with contextlib.ExitStack() as stack:
            for owner, name, replacement in [
                (base, 'UNIT', self.unit), (base, 'RUNTIME_ENV', self.env),
                (base, 'atomic_write', atomic), (base, 'system', system),
                (base, 'stop_service', stopped), (base, 'plan_unit', lambda *_: b'NEW UNIT\n'),
                (base, 'sha', lambda *_: 'hash'), (base, 'Transaction', Mock(return_value=Mock())),
                (release, 'matching_processes', lambda: [{'pid': 77}] if extra_writer else []), (release, 'sql', sessions_sql),
                (release, 'data_summary', lambda: dict(summary)),
                (release, 'native_environment', lambda *_: b'NEW ENV\n'),
                (release, 'validate', validate),
            ]:
                stack.enter_context(patch.object(owner, name, replacement))
            stack.enter_context(patch.object(self.transaction, 'preflight', lambda: None))
            stack.enter_context(patch.object(self.transaction, 'save', save))
            stack.enter_context(patch.object(self.transaction, 'verify', verify))
            return self.transaction.activate()

    def test_success_switches_only_owned_unit_env_and_resumes_bridge(self):
        result = self.activate()
        self.assertTrue(result['deployed'])
        self.assertEqual(self.unit.read_bytes(), b'NEW UNIT\n')
        self.assertEqual(self.env.read_bytes(), b'NEW ENV\n')
        self.assertIn(('system', 'start', release.BRIDGE + '.timer'), self.calls)
        self.assertEqual(sum(call == ('system', 'start', base.SERVICE) for call in self.calls), 1)

    def test_prestart_unit_failure_restores_old_owned_configuration(self):
        with self.assertRaises(base.UpgradeError):
            self.activate(fail='unit-write')
        self.assertEqual(self.unit.read_bytes(), b'OLD UNIT\n')
        self.assertEqual(self.env.read_bytes(), b'EXISTING=unchanged\n')
        self.assertEqual(self.transaction.state['phase'], 'prestart_failed_baseline_restored')
        self.assertEqual(sum(call == ('system', 'start', base.SERVICE) for call in self.calls), 1)

    def test_start_attempt_never_restores_incompatible_old_writer(self):
        with self.assertRaises(base.UpgradeError):
            self.activate(fail='start')
        self.assertEqual(self.transaction.state['phase'], 'candidate_needs_recovery')
        self.assertEqual(self.env.read_bytes(), b'NEW ENV\n')
        self.assertEqual(self.unit.read_bytes(), b'NEW UNIT\n')
        self.assertEqual(sum(call == ('system', 'start', base.SERVICE) for call in self.calls), 1)
        self.assertNotIn(('system', 'start', release.BRIDGE + '.timer'), self.calls)

    def test_poststart_health_failure_keeps_compatible_candidate_configuration(self):
        with self.assertRaises(base.UpgradeError):
            self.activate(fail='verify')
        self.assertEqual(self.transaction.state['phase'], 'candidate_needs_recovery')
        self.assertEqual(self.env.read_bytes(), b'NEW ENV\n')
        self.assertNotIn(('system', 'start', release.BRIDGE + '.timer'), self.calls)

    def test_detected_operator_drift_is_not_overwritten_during_recovery(self):
        with self.assertRaises(base.UpgradeError):
            self.activate(external_change=True)
        self.assertEqual(self.env.read_bytes(), b'OPERATOR=keep-this-change\n')
        self.assertNotIn(('system', 'start', base.SERVICE), self.calls)
        self.assertNotIn(('system', 'start', release.BRIDGE + '.timer'), self.calls)

    def test_previously_inactive_timer_is_accepted_and_not_enabled(self):
        self.activate(timer_state='inactive')
        self.assertFalse(self.transaction.state['bridge_timer_was_active'])
        self.assertNotIn(('system', 'start', release.BRIDGE + '.timer'), self.calls)
        self.assertNotIn(('system', 'is-active', release.BRIDGE + '.timer'), self.calls)

    def test_interrupted_candidate_state_does_not_start_any_writer(self):
        self.transaction.state['phase'] = 'starting_candidate'
        with self.assertRaises(base.UpgradeError):
            self.activate()
        self.assertFalse(any(call[:2] == ('system', 'start') for call in self.calls))

    def test_artifact_drift_after_drain_blocks_candidate_start(self):
        with self.assertRaisesRegex(base.UpgradeError, 'changed artifact'):
            self.activate(fail='artifact')
        self.assertIn(('validate',), self.calls)
        self.assertNotIn(('write', self.unit.name, b'NEW UNIT\n'), self.calls)
        self.assertEqual(self.transaction.state['phase'], 'prestart_failed_baseline_restored')

    def test_failed_writer_isolation_never_restarts_another_writer(self):
        with self.assertRaises(base.UpgradeError):
            self.activate(extra_writer=True)
        self.assertNotIn(('system', 'start', base.SERVICE), self.calls)
        self.assertNotIn(('system', 'start', release.BRIDGE + '.timer'), self.calls)


class NativeEnvironmentTests(unittest.TestCase):
    def environment(self, **entries):
        original = ''.join(key + '=' + value + '\n' for key, value in entries.items()).encode()
        result = release.native_environment(original, Path('/synthetic/release'), {'native_sha256': 'a' * 64})
        return dict(line.split('=', 1) for line in result.decode().splitlines())

    def test_default_trust_does_not_create_a_custom_ca_override(self):
        with patch.object(release, 'runtime_ca_readable') as readable:
            values = self.environment(EXISTING='untouched')
        self.assertFalse({'CODEX_CA_CERTIFICATE', 'SSL_CERT_FILE', 'SSL_CERT_DIR'} & values.keys())
        self.assertEqual(values['EXISTING'], 'untouched')
        readable.assert_not_called()

    def test_redeployment_cannot_reintroduce_default_ssl_cert_file(self):
        first = self.environment()
        self.assertEqual(self.environment(**first), first)
        self.assertNotIn('SSL_CERT_FILE', first)

    def test_empty_custom_ca_values_are_preserved_as_official_unset_selection(self):
        for entries in ({'SSL_CERT_FILE': ''}, {'CODEX_CA_CERTIFICATE': ''},
                        {'CODEX_CA_CERTIFICATE': '', 'SSL_CERT_FILE': ''}):
            with self.subTest(entries=entries), patch.object(release, 'runtime_ca_readable') as readable:
                values = self.environment(**entries)
                self.assertTrue(entries.items() <= values.items())
                readable.assert_not_called()

    def test_explicit_system_bundle_is_preserved_not_guessed_to_be_legacy(self):
        with patch.object(release, 'runtime_ca_readable') as readable:
            values = self.environment(SSL_CERT_FILE='/etc/pki/tls/certs/ca-bundle.crt')
        self.assertEqual(values['SSL_CERT_FILE'], '/etc/pki/tls/certs/ca-bundle.crt')
        readable.assert_called_once_with(Path(values['SSL_CERT_FILE']))

    def test_codex_override_wins_without_validating_unused_ssl_file(self):
        with tempfile.TemporaryDirectory() as folder:
            Path(folder).chmod(0o755)
            ca = Path(folder) / 'codex.pem'
            ca.write_text('fixture; real helper validates PEM at startup')
            missing = str(Path(folder) / 'not-selected.pem')
            with patch.object(base, 'runtime_readable') as readable:
                values = self.environment(CODEX_CA_CERTIFICATE=str(ca), SSL_CERT_FILE=missing)
            self.assertEqual(values['CODEX_CA_CERTIFICATE'], str(ca))
            self.assertEqual(values['SSL_CERT_FILE'], missing)
            readable.assert_called_once_with(ca.resolve())

    def test_invalid_codex_override_never_falls_back_to_valid_ssl_file(self):
        with tempfile.TemporaryDirectory() as folder:
            ca = Path(folder) / 'fallback.pem'
            ca.write_text('synthetic roots')
            with self.assertRaises(base.UpgradeError):
                self.environment(CODEX_CA_CERTIFICATE=str(Path(folder) / 'missing'), SSL_CERT_FILE=str(ca))

    def test_empty_codex_override_selects_ssl_file(self):
        with tempfile.TemporaryDirectory() as folder:
            Path(folder).chmod(0o755)
            ca = Path(folder) / 'fallback.pem'
            ca.write_text('synthetic roots')
            with patch.object(base, 'runtime_readable') as readable:
                values = self.environment(CODEX_CA_CERTIFICATE='', SSL_CERT_FILE=str(ca))
            self.assertEqual(values['CODEX_CA_CERTIFICATE'], '')
            readable.assert_called_once_with(ca.resolve())

    def test_ca_values_are_not_trimmed_into_a_different_selection(self):
        with self.assertRaisesRegex(base.UpgradeError, 'absolute'):
            self.environment(CODEX_CA_CERTIFICATE=' ', SSL_CERT_FILE='/ignored.pem')

    def test_relative_custom_ca_path_is_rejected(self):
        with self.assertRaisesRegex(base.UpgradeError, 'absolute'):
            self.environment(SSL_CERT_FILE='roots.pem')

    def test_ssl_cert_dir_preserves_multiple_directories_without_custom_ca_switch(self):
        with tempfile.TemporaryDirectory() as folder:
            Path(folder).chmod(0o755)
            first, second = Path(folder) / 'first', Path(folder) / 'second'
            first.mkdir(); second.mkdir()
            ca = first / 'roots.pem'; ca.write_text('synthetic roots')
            (first / 'nested').mkdir()  # Native roots scan only immediate files.
            configured = os.pathsep.join((str(first), str(second)))
            with patch.object(base, 'runtime_readable') as readable:
                values = self.environment(SSL_CERT_DIR=configured)
            self.assertEqual(values['SSL_CERT_DIR'], configured)
            self.assertNotIn('SSL_CERT_FILE', values)
            self.assertNotIn('CODEX_CA_CERTIFICATE', values)
            readable.assert_called_once_with(ca.resolve())

    def test_missing_ssl_cert_directory_is_rejected(self):
        with tempfile.TemporaryDirectory() as folder:
            with self.assertRaisesRegex(base.UpgradeError, 'unavailable'):
                self.environment(SSL_CERT_DIR=str(Path(folder) / 'missing'))

    def test_ssl_cert_dir_rejects_implicit_working_directory_in_path_list(self):
        with tempfile.TemporaryDirectory() as folder:
            Path(folder).chmod(0o755)
            with self.assertRaisesRegex(base.UpgradeError, 'absolute'):
                self.environment(SSL_CERT_DIR=folder + os.pathsep)

    @unittest.skipUnless(os.name == 'posix', 'requires real POSIX permission and symlink semantics')
    def test_symlinked_ca_preserves_original_path_and_checks_resolved_target(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder); root.chmod(0o755)
            target = root / 'roots.pem'; target.write_text('synthetic roots'); target.chmod(0o644)
            link = root / 'bundle.pem'; link.symlink_to(target.name)
            values = self.environment(SSL_CERT_FILE=str(link))
            self.assertEqual(values['SSL_CERT_FILE'], str(link))
            target.chmod(0o600)
            with self.assertRaisesRegex(base.UpgradeError, 'cannot read'):
                self.environment(SSL_CERT_FILE=str(link))

    @unittest.skipUnless(os.name == 'posix', 'requires real POSIX permission and symlink semantics')
    def test_symlinked_file_parent_requires_traverse_but_not_directory_listing(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder); root.chmod(0o755)
            directory = root / 'certs'; directory.mkdir(mode=0o711)
            target = directory / 'roots.pem'; target.write_text('synthetic roots'); target.chmod(0o644)
            link = root / 'linked-certs'; link.symlink_to(directory, target_is_directory=True)
            selected = str(link / target.name)
            self.assertEqual(self.environment(SSL_CERT_FILE=selected)['SSL_CERT_FILE'], selected)

    @unittest.skipUnless(os.name == 'posix', 'requires real POSIX permission and symlink semantics')
    def test_symlink_original_parent_permissions_cannot_be_hidden_by_resolve(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder); root.chmod(0o755)
            target = root / 'roots.pem'; target.write_text('synthetic roots'); target.chmod(0o644)
            private = root / 'private'; private.mkdir(mode=0o700)
            link = private / 'bundle.pem'; link.symlink_to(target)
            with self.assertRaisesRegex(base.UpgradeError, 'cannot traverse'):
                self.environment(SSL_CERT_FILE=str(link))

    @unittest.skipUnless(os.name == 'posix', 'requires real POSIX permission and symlink semantics')
    def test_intermediate_symlink_parent_permissions_are_checked(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder); root.chmod(0o755)
            target = root / 'roots.pem'; target.write_text('synthetic roots'); target.chmod(0o644)
            private = root / 'private'; private.mkdir(mode=0o700)
            middle = private / 'middle.pem'; middle.symlink_to(target)
            outer = root / 'bundle.pem'; outer.symlink_to(middle)
            with self.assertRaisesRegex(base.UpgradeError, 'cannot traverse'):
                self.environment(SSL_CERT_FILE=str(outer))

    @unittest.skipUnless(os.name == 'posix', 'requires real POSIX permission and symlink semantics')
    def test_ssl_cert_directory_requires_listing_and_readable_immediate_files(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder); root.chmod(0o755)
            directory = root / 'certs'; directory.mkdir(mode=0o711)
            with self.assertRaisesRegex(base.UpgradeError, 'cannot read or traverse'):
                self.environment(SSL_CERT_DIR=str(directory))
            directory.chmod(0o755)
            target = root / 'roots.pem'; target.write_text('synthetic roots'); target.chmod(0o600)
            (directory / '12345678.0').symlink_to(target)
            with self.assertRaisesRegex(base.UpgradeError, 'cannot read'):
                self.environment(SSL_CERT_DIR=str(directory))
            target.chmod(0o644)
            (directory / 'dangling.0').symlink_to(root / 'absent.pem')
            self.assertEqual(self.environment(SSL_CERT_DIR=str(directory))['SSL_CERT_DIR'], str(directory))

    @unittest.skipUnless(os.name == 'posix', 'requires real POSIX symlink semantics')
    def test_symlink_loop_is_a_bounded_configuration_error(self):
        with tempfile.TemporaryDirectory() as folder:
            link = Path(folder) / 'loop.pem'; link.symlink_to(link.name)
            with self.assertRaises(base.UpgradeError):
                self.environment(SSL_CERT_FILE=str(link))

    def test_preserves_existing_values_and_sets_exact_native_contract(self):
        with tempfile.TemporaryDirectory() as folder:
            Path(folder).chmod(0o755)
            ca = Path(folder) / 'roots.pem'
            ca.write_text('synthetic root bundle')
            original = ('# private note\nDATABASE_URL=synthetic-local\nEXISTING=value=with=equals\nSSL_CERT_FILE=' + str(ca) + '\n').encode()
            with patch.object(base, 'runtime_readable'):
                result = release.native_environment(original, Path(folder) / 'release', {'native_sha256': 'a' * 64})
            values = dict(line.split('=', 1) for line in result.decode().splitlines() if line and not line.startswith('#'))
            self.assertEqual(values['DATABASE_URL'], 'synthetic-local')
            self.assertEqual(values['EXISTING'], 'value=with=equals')
            self.assertEqual(values['SSL_CERT_FILE'], str(ca))
            self.assertEqual(values['CODEX_NATIVE_HTTP_LIFECYCLE'], 'cli_request')
            self.assertEqual(values['CODEX_NATIVE_WEBSOCKET'], '1')

    def test_duplicate_environment_fails_closed(self):
        with self.assertRaises(base.UpgradeError):
            release.native_environment(b'KEY=first\nKEY=second\n', Path('/synthetic/release'), {'native_sha256': 'a' * 64})

    def test_malformed_environment_fails_before_activation(self):
        with self.assertRaisesRegex(base.UpgradeError, 'syntax'):
            release.native_environment(b'NO_EQUALS\n', Path('/synthetic/release'), {'native_sha256': 'a' * 64})

    def test_missing_ca_fails_before_activation(self):
        with tempfile.TemporaryDirectory() as folder:
            original = ('SSL_CERT_FILE=' + str(Path(folder) / 'missing') + '\n').encode()
            with self.assertRaises(base.UpgradeError):
                release.native_environment(original, Path(folder), {'native_sha256': 'a' * 64})

    def test_ca_permissions_are_checked_for_dynamic_user(self):
        with tempfile.TemporaryDirectory() as folder:
            ca = Path(folder) / 'roots.pem'; ca.write_text('synthetic roots')
            with patch.object(base, 'runtime_readable', side_effect=base.UpgradeError('unreadable')) as readable:
                with self.assertRaises(base.UpgradeError):
                    release.native_environment(('SSL_CERT_FILE=' + str(ca) + '\n').encode(), Path(folder), {'native_sha256': 'a' * 64})
                readable.assert_called_once_with(ca.resolve())


class ManifestTests(unittest.TestCase):
    def fixture(self, folder):
        folder = folder.resolve()
        staging = folder / 'staging'; staging.mkdir()
        file_data = {
            'codex-agent': b'linux-fixture', 'native/codex-native-transport': b'native-fixture',
            'frontend/dist/index.html': b'<script src="/assets/test.js"></script>',
            'frontend/dist/assets/test.js': b'fixture', 'deploy/upgrade_remote.py': b'base',
            'deploy/full_release_remote.py': b'publisher',
            'migrations/000_core.sql': b'000', 'migrations/001_accounts.sql': b'001',
            'migrations/002_metrics.sql': b'002', 'migrations/003_management_cleanup.sql': b'003',
        }
        migrations = {name: base.bytes_sha(raw) for name, raw in file_data.items() if name.startswith('migrations/')}
        source_files = {'main.go': 'd' * 64, 'go.mod': 'e' * 64, **migrations}
        source_sha = base.bytes_sha(json.dumps(source_files, sort_keys=True, separators=(',', ':')).encode())
        frontend = {name.removeprefix('frontend/dist/'): base.bytes_sha(raw) for name, raw in file_data.items() if name.startswith('frontend/dist/')}
        acceptance = {'passed': True, 'source_sha256': source_sha, 'binary_sha256': base.bytes_sha(file_data['codex-agent']), 'frontend': frontend}
        file_data['evidence/acceptance.json'] = json.dumps(acceptance).encode()
        file_data['evidence/source-manifest.json'] = json.dumps({'sha256': source_sha, 'files': source_files}).encode()
        for name, raw in file_data.items():
            target = staging / name; target.parent.mkdir(parents=True, exist_ok=True); target.write_bytes(raw)
        manifest = {'schema': release.SCHEMA, 'source_sha256': source_sha, 'binary_sha256': acceptance['binary_sha256'],
                    'native_sha256': base.bytes_sha(file_data['native/codex-native-transport']), 'native_enabled': True,
                    'frontend': frontend, 'migration_checksums': migrations,
                    'files': {name: base.bytes_sha(raw) for name, raw in file_data.items()}}
        return staging, manifest, migrations

    def seal(self, staging, manifest):
        identity = {key: value for key, value in manifest.items() if key != 'release_id'}
        rid = base.bytes_sha(json.dumps(identity, sort_keys=True).encode())
        target = staging.parent / rid; staging.rename(target)
        (target / 'full-release.json').write_text(json.dumps({**identity, 'release_id': rid}))
        return target

    def test_valid_exact_manifest_is_accepted(self):
        with tempfile.TemporaryDirectory() as directory:
            staging, manifest, migrations = self.fixture(Path(directory))
            target = self.seal(staging, manifest)
            with patch.object(base, 'TARGET_MIGRATIONS', migrations), patch.object(base, 'runtime_readable'):
                self.assertEqual(release.validate(target)['release_id'], target.name)

    def test_omitted_acceptance_hash_is_rejected_even_when_file_exists(self):
        with tempfile.TemporaryDirectory() as directory:
            staging, manifest, migrations = self.fixture(Path(directory))
            del manifest['files']['evidence/acceptance.json']
            target = self.seal(staging, manifest)
            with patch.object(base, 'TARGET_MIGRATIONS', migrations), patch.object(base, 'runtime_readable'):
                with self.assertRaisesRegex(base.UpgradeError, 'omits required'):
                    release.validate(target)

    def test_mismatched_source_file_map_cannot_reuse_claimed_digest(self):
        with tempfile.TemporaryDirectory() as directory:
            staging, manifest, migrations = self.fixture(Path(directory))
            file = staging / 'evidence/source-manifest.json'
            source = json.loads(file.read_text()); source['files']['main.go'] = 'f' * 64
            file.write_text(json.dumps(source)); manifest['files']['evidence/source-manifest.json'] = base.sha(file)
            target = self.seal(staging, manifest)
            with patch.object(base, 'TARGET_MIGRATIONS', migrations), patch.object(base, 'runtime_readable'):
                with self.assertRaisesRegex(base.UpgradeError, 'Source digest'):
                    release.validate(target)


class StagingTests(unittest.TestCase):
    def test_stage_refuses_existing_targets_and_stops_on_extract_failure(self):
        with tempfile.TemporaryDirectory() as folder:
            output = Path(folder); (output / 'package').mkdir()
            (output / 'package/full-release.json').write_text(json.dumps({'release_id': 'a' * 64}))
            (output / 'release.tar.gz').write_bytes(b'fake archive')
            commands = []
            client = Mock(); client.open_sftp.return_value.__enter__ = Mock(return_value=Mock())
            client.open_sftp.return_value.__exit__ = Mock(return_value=False)

            def run(_client, command, **_):
                commands.append(command)
                if command.startswith('sha256sum'):
                    return local.sha(output / 'release.tar.gz') + ' archive'
                if 'tar -xzf' in command:
                    raise RuntimeError('synthetic extraction failure')
                return ''
            with patch.object(local, 'OUT', output), patch.object(local, 'connect', return_value=client), patch.object(local, 'run', run):
                with self.assertRaisesRegex(RuntimeError, 'extraction failure'):
                    local.remote_action('stage')
            self.assertTrue(commands[0].startswith('set -eu\n'))
            self.assertEqual(commands[0].count('test ! -e'), 2)
            self.assertTrue(commands[-1].startswith('set -eu\n'))
            self.assertIn('mv -T ', commands[-1])
            self.assertEqual(len(commands), 3)
            client.close.assert_called_once()

    def test_invalid_local_release_id_never_connects(self):
        with patch.object(local, 'load', return_value={'release_id': '../other-project'}), patch.object(local, 'connect') as connect:
            with self.assertRaisesRegex(ValueError, 'release identity'):
                local.remote_action('stage')
            connect.assert_not_called()

if __name__ == '__main__':
    unittest.main()
