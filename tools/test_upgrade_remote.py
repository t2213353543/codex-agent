"""PG upgrade guard/state-machine tests. Never contacts SSH, Docker or systemd."""
import importlib.util
import base64
import copy
import json
from pathlib import Path, PurePosixPath
import tempfile
import unittest
from unittest.mock import patch, Mock


SPEC = importlib.util.spec_from_file_location('upgrade_remote', Path(__file__).resolve().parents[1] / 'deploy/upgrade_remote.py')
upgrade = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(upgrade)
ROOT = PurePosixPath('/opt/codex-agent')
ENV = PurePosixPath('/etc/codex-agent/production.env')
SERVICE_FILE = PurePosixPath('/etc/systemd/system/codex-agent.service')
OLD = ROOT / 'releases' / ('a' * 64)
NEW = ROOT / 'releases' / ('b' * 64)
WEB = ROOT / 'ui-releases' / ('c' * 64) / 'frontend/dist'
UNIT = ('[Unit]\nDescription=existing production\n[Service]\nType=exec\nDynamicUser=yes\n'
        'EnvironmentFile=' + str(ENV) + '\nWorkingDirectory=' + str(OLD)
        + '\nExecStart=' + str(OLD / 'codex-agent') + ' --listen 0.0.0.0:18879 --web ' + str(WEB)
        + '\nTimeoutStopSec=180\nMemoryMax=512M\n').encode()


class PosixTests(unittest.TestCase):
    def setUp(self):
        self.context = patch.multiple(upgrade, ROOT=ROOT, RUNTIME_ENV=ENV, UNIT=SERVICE_FILE, Path=PurePosixPath)
        self.context.start()
        self.addCleanup(self.context.stop)


class ParserTests(PosixTests):
    def test_acceptance_requires_current_source_and_every_required_check(self):
        report = {'status': 'pass', 'source_sha256': 'd' * 64,
                  'checks': dict.fromkeys(upgrade.REQUIRED_CHECKS, True)}
        upgrade.validate_acceptance(report, 'd' * 64)
        for changed in ({**report, 'source_sha256': 'e' * 64},
                        {**report, 'status': 'blocked'}, {**report, 'checks': {}},
                        {**report, 'checks': {**report['checks'], 'storage': 'true'}}):
            with self.subTest(report=changed):
                with self.assertRaises(upgrade.UpgradeError):
                    upgrade.validate_acceptance(changed, 'd' * 64)

    def test_upgrade_preserves_every_other_unit_byte_and_current_ui_rollback(self):
        original = upgrade.parse_unit(UNIT)
        self.assertEqual(original['web'], WEB)
        changed = upgrade.plan_unit(UNIT, NEW)
        replacement = upgrade.parse_unit(changed)
        self.assertEqual(replacement['binary'], NEW / 'codex-agent')
        self.assertEqual(replacement['web'], NEW / 'frontend/dist')
        self.assertEqual(UNIT.replace(original['token'], b'REPLACED'),
                         changed.replace(replacement['token'], b'REPLACED'))
        self.assertEqual(upgrade.rollback_plan(changed, UNIT, changed), UNIT)

    def test_rejects_legacy_unscoped_and_ambiguous_service(self):
        mutations = [
            UNIT.replace(b'Type=exec', b'Type=simple'),
            UNIT.replace(b'DynamicUser=yes', b'DynamicUser=no'),
            UNIT.replace(b'18879', b'28765'),
            UNIT.replace(b'/releases/', b'/other/'),
            UNIT + b'ExecStart=/bin/sh\n',
            UNIT + b'ExecStartPre=/bin/true\n',
            UNIT + b'Environment=DATABASE_URL=postgres://wrong\n',
            UNIT.replace(b'EnvironmentFile=', b'EnvironmentFile=-'),
            UNIT.replace(b'\n', b'\r\n'),
            UNIT.replace(b'Type=exec', b'Type=\\\nexec'),
            UNIT.replace(b' --listen', b' --data /var/lib/codex-agent --listen'),
        ]
        for raw in mutations:
            with self.subTest(raw=raw):
                with self.assertRaises(upgrade.UpgradeError):
                    upgrade.parse_unit(raw)

    def test_rejects_changed_unit_on_rollback(self):
        changed = upgrade.plan_unit(UNIT, NEW)
        with self.assertRaises(upgrade.UpgradeError):
            upgrade.rollback_plan(changed.replace(b'MemoryMax=512M', b'MemoryMax=1G'), UNIT, changed)
        self.assertEqual(upgrade.rollback_plan(UNIT, UNIT, changed), UNIT)

    def test_release_scopes(self):
        self.assertEqual(upgrade.scoped_release(NEW), NEW)
        for path in (PurePosixPath('/opt/other/releases') / ('b' * 64), NEW / '..',
                     ROOT / 'releases' / ('B' * 64), PurePosixPath('relative') / ('b' * 64)):
            with self.subTest(path=path):
                with self.assertRaises(upgrade.UpgradeError):
                    upgrade.scoped_release(path)

    def test_artifact_traversal(self):
        self.assertEqual(upgrade.relative('assets/app-123456.js'), 'assets/app-123456.js')
        for value in ('../secret', '/etc/passwd', 'assets/../secret', 'assets//file',
                      'assets\\file', '', './file', 'assets/file\x00'):
            with self.subTest(value=value):
                with self.assertRaises(upgrade.UpgradeError):
                    upgrade.relative(value)

    def test_symlink_rejected(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory).resolve()
            source = root / 'original'
            source.write_bytes(b'test')
            target = root / 'link'
            try:
                target.symlink_to(source)
            except OSError:
                self.skipTest('Symlink creation unavailable on this platform')
            with patch.object(upgrade, 'Path', Path):
                with self.assertRaises(upgrade.UpgradeError):
                    upgrade.regular(target)


class RecoveryTests(PosixTests):
    def test_startup_failure_restores_previous_pg_unit_without_database_restore(self):
        transaction = upgrade.Transaction.__new__(upgrade.Transaction)
        transaction.release = NEW
        transaction.private = NEW / '.upgrade-private'
        transaction.state = {'phase': 'preflight_passed', 'preflight_unit_sha256': upgrade.bytes_sha(UNIT)}
        manifest = {'binary_sha256': 'd' * 64, 'frontend': {'index.html': 'e' * 64}}
        effects = []
        current_unit = [UNIT]
        unit_file = type('UnitFile', (), {'read_bytes': lambda self: current_unit[0]})()

        def save(phase, **values):
            transaction.state.update(values, phase=phase)
            effects.append(phase)

        def atomic(path, raw, mode):
            self.assertEqual(path, upgrade.UNIT)
            current_unit[0] = raw
            effects.append('install-candidate')

        def rollback(automatic=False):
            self.assertTrue(automatic)
            self.assertEqual(upgrade.rollback_plan(current_unit[0], UNIT, upgrade.plan_unit(UNIT, NEW)), UNIT)
            current_unit[0] = UNIT
            effects.append('rollback')

        with patch.object(upgrade, 'validate_manifest', return_value=manifest), \
                patch.object(upgrade, 'regular', return_value=unit_file), \
                patch.object(transaction, 'preflight'), patch.object(transaction, 'backup'), \
                patch.object(transaction, 'unchanged_environment'), patch.object(transaction, 'save', side_effect=save), \
                patch.object(transaction, 'rollback', side_effect=rollback), \
                patch.object(upgrade, 'stop_service', side_effect=lambda: effects.append('drained')), \
                patch.object(upgrade, 'atomic_write', side_effect=atomic), \
                patch.object(upgrade, 'system', return_value=''), \
                patch.object(upgrade, 'verify_health', side_effect=upgrade.UpgradeError('Startup failed')):
            with self.assertRaisesRegex(upgrade.UpgradeError, 'previous PostgreSQL service is restored'):
                transaction.activate()
        self.assertEqual(current_unit[0], UNIT)
        self.assertLess(effects.index('drained'), effects.index('install-candidate'))
        self.assertLess(effects.index('install-candidate'), effects.index('rollback'))

    def test_interrupted_activation_recovers_before_retry(self):
        transaction = upgrade.Transaction.__new__(upgrade.Transaction)
        transaction.release = NEW
        transaction.state = {'phase': 'switch_requested'}
        with patch.object(upgrade, 'validate_manifest', return_value={}), \
                patch.object(transaction, 'rollback', return_value={}) as rollback, \
                patch.object(transaction, 'preflight') as preflight:
            with self.assertRaisesRegex(upgrade.UpgradeError, 'Interrupted upgrade recovered'):
                transaction.activate()
        rollback.assert_called_once_with(automatic=True)
        preflight.assert_not_called()


def v2_evidence():
    source = {'files': {'database.go': 'd' * 64, 'main.go': '1' * 64,
        'go.mod': '2' * 64, 'go.sum': '3' * 64, **upgrade.TARGET_MIGRATIONS}}
    source['sha256'] = upgrade.bytes_sha(json.dumps(source['files'], sort_keys=True).encode())
    manifest = {'source_sha256': source['sha256'], 'binary_sha256': '4' * 64,
        'frontend': {'index.html': '5' * 64}, 'migration_evidence_sha256': '6' * 64,
        'rollback_evidence_sha256': '7' * 64, 'frontend_evidence_sha256': '8' * 64}
    acceptance = {**manifest, 'schema': 'codex-agent-pg-upgrade-acceptance-v2', 'status': 'pass',
        'checks': dict.fromkeys(upgrade.V2_CHECKS, True)}
    migration = {'schema': 'codex-agent-pg-additive-migration-v1', 'status': 'pass',
        'synthetic_only': True, 'scope': 'migration-focused', 'source_sha256': 'c' * 64, 'migration_runner_sha256': 'd' * 64,
        'baseline_checksums': upgrade.MIGRATIONS, 'target_checksums': upgrade.TARGET_MIGRATIONS,
        'checks': dict.fromkeys(upgrade.MIGRATION_CHECKS, True)}
    rollback = {'schema': 'codex-agent-pg-rollback-compatibility-v1', 'status': 'pass',
        'synthetic_only': True, 'previous_binary_sha256': '9' * 64,
        'target_checksums': upgrade.TARGET_MIGRATIONS, 'checks': dict.fromkeys(upgrade.ROLLBACK_CHECKS, True)}
    frontend = {'schema': 'codex-agent-production-frontend-v1', 'status': 'pass',
        'binary_sha256': '9' * 64, 'unit_sha256': 'a' * 64, 'frontend': manifest['frontend']}
    return manifest, acceptance, migration, rollback, frontend, source


class V2EvidenceTests(unittest.TestCase):
    def test_nested_server_and_embed_package_keep_exact_sql_migration_binding(self):
        arguments = v2_evidence()
        manifest, acceptance, migration, _, _, source = arguments
        files = source['files']
        files['internal/server/database.go'] = files.pop('database.go')
        files['internal/bps/request.go'] = 'b' * 64
        files['migrations/embed.go'] = 'e' * 64
        source['sha256'] = upgrade.bytes_sha(json.dumps(files, sort_keys=True).encode())
        manifest['source_sha256'] = acceptance['source_sha256'] = source['sha256']
        upgrade.validate_v2_evidence(*arguments)
        migration['migration_runner_sha256'] = 'f' * 64
        with self.assertRaises(upgrade.UpgradeError):
            upgrade.validate_v2_evidence(*arguments)

    def test_evidence_binds_exact_inputs_and_requires_real_rollback_checks(self):
        arguments = v2_evidence()
        upgrade.validate_v2_evidence(*arguments)
        mutations = [(1, 'binary_sha256', 'f' * 64), (2, 'migration_runner_sha256', 'e' * 64),
            (2, 'synthetic_only', False), (2, 'target_checksums', upgrade.MIGRATIONS),
            (3, 'previous_binary_sha256', 'b' * 64), (4, 'frontend', {'index.html': 'a' * 64}),
            (5, 'sha256', '0' * 64)]
        for index, field, value in mutations:
            with self.subTest(index=index, field=field):
                changed = copy.deepcopy(arguments)
                changed[index][field] = value
                with self.assertRaises(upgrade.UpgradeError):
                    upgrade.validate_v2_evidence(*changed)
        for index, required in ((1, upgrade.V2_CHECKS), (2, upgrade.MIGRATION_CHECKS), (3, upgrade.ROLLBACK_CHECKS)):
            for check in required:
                changed = copy.deepcopy(arguments)
                changed[index]['checks'][check] = 'true'
                with self.subTest(check=check), self.assertRaises(upgrade.UpgradeError):
                    upgrade.validate_v2_evidence(*changed)

    def test_v1_schema_gate_remains_exact_and_v2_rejects_unknown_migrations(self):
        for schema in (upgrade.MIGRATIONS, upgrade.TARGET_MIGRATIONS):
            with patch.object(upgrade, 'read_schema_checksums', return_value=schema):
                self.assertEqual(upgrade.schema_checksums_v2(), schema)
        with patch.object(upgrade, 'read_schema_checksums', return_value=upgrade.TARGET_MIGRATIONS):
            with self.assertRaises(upgrade.UpgradeError):
                upgrade.schema_checksums()
        for schema in ({**upgrade.MIGRATIONS, 'migrations/999.sql': 'a' * 64},
                       {**upgrade.TARGET_MIGRATIONS, 'migrations/000_core.sql': 'b' * 64}):
            with patch.object(upgrade, 'read_schema_checksums', return_value=schema), self.assertRaises(upgrade.UpgradeError):
                upgrade.schema_checksums_v2()

    def test_migration_environment_has_distinct_owner_and_exact_private_key(self):
        runtime = {'DATABASE_URL': 'postgres://codex_agent_runtime:synthetic@127.0.0.1:25433/codex_agent?sslmode=disable',
            'CREDENTIAL_ENCRYPTION_KEY': base64.b64encode(bytes(range(32))).decode(), 'CREDENTIAL_ENCRYPTION_KEY_ID': 'synthetic'}
        migration = {'DATABASE_URL': 'postgres://codex_agent_migrator:synthetic@127.0.0.1:25433/codex_agent?sslmode=disable'}
        with patch.object(upgrade, 'environment_facts'), patch.object(upgrade, 'private_environment', side_effect=[runtime, migration]):
            env = upgrade.v2_migration_environment()
        self.assertEqual(env['DATABASE_URL'], migration['DATABASE_URL'])
        self.assertEqual(env['CREDENTIAL_ENCRYPTION_KEY'], runtime['CREDENTIAL_ENCRYPTION_KEY'])
        self.assertNotIn('HOME', env)
        for broken in ({**migration, 'CREDENTIAL_ENCRYPTION_KEY': 'override'},
                       {'DATABASE_URL': migration['DATABASE_URL'].replace('codex_agent_migrator', 'codex_agent_runtime')},
                       {'DATABASE_URL': migration['DATABASE_URL'].replace('25433', '5432')},
                       {'DATABASE_URL': migration['DATABASE_URL'] + '&search_path=other'}):
            with patch.object(upgrade, 'environment_facts'), patch.object(upgrade, 'private_environment', side_effect=[runtime, broken]):
                with self.assertRaises(upgrade.UpgradeError):
                    upgrade.v2_migration_environment()
        with patch.object(upgrade, 'environment_facts'), patch.object(upgrade, 'private_environment',
                side_effect=[{**runtime, 'CREDENTIAL_ENCRYPTION_KEY': 'not-a-key'}, migration]):
            with self.assertRaises(upgrade.UpgradeError):
                upgrade.v2_migration_environment()

    def test_storage_guard_rejects_owner_privileges_and_wrong_key_identity(self):
        facts = {'database': 'codex_agent', 'owner': 'codex_agent_migrator',
            **dict.fromkeys(('migrator_restricted', 'runtime_restricted', 'runtime_no_create',
                'tables_owned', 'default_runtime_crud', 'key_ids_match'), True)}
        for changed in ({**facts, 'owner': 'other_project'}, {**facts, 'runtime_no_create': False},
                        {**facts, 'default_runtime_crud': False}, {**facts, 'key_ids_match': False}):
            with self.subTest(changed=changed), patch.object(upgrade, 'v2_migration_environment',
                    return_value={'CREDENTIAL_ENCRYPTION_KEY_ID': 'synthetic'}), \
                    patch.object(upgrade, 'command', return_value=json.dumps(changed).encode()), \
                    self.assertRaises(upgrade.UpgradeError):
                upgrade.v2_storage_facts()


class V2RecoveryTests(PosixTests):
    def transaction(self):
        transaction = upgrade.AdditiveTransaction.__new__(upgrade.AdditiveTransaction)
        transaction.release, transaction.private = NEW, NEW / '.upgrade-private'
        transaction.state = {'phase': 'preflight_passed', 'preflight_unit_sha256': upgrade.bytes_sha(UNIT)}
        transaction.events = []
        def save(phase, **values):
            transaction.state.update(values, phase=phase)
            transaction.events.append(phase)
        transaction.save = save
        return transaction

    def test_migration_runs_only_after_drain_and_keeps_secrets_out_of_arguments(self):
        transaction = self.transaction()
        calls = []
        env = {'DATABASE_URL': 'synthetic-private-dsn', 'CREDENTIAL_ENCRYPTION_KEY': 'synthetic-private-key'}
        with patch.object(upgrade, 'assert_stopped', side_effect=lambda: calls.append('drained')), \
                patch.object(transaction, 'unchanged_environment'), \
                patch.object(transaction, 'check_schema', side_effect=[upgrade.MIGRATIONS, upgrade.TARGET_MIGRATIONS]), \
                patch.object(upgrade, 'v2_storage_facts'), patch.object(upgrade, 'v2_migration_environment', return_value=env), \
                patch.object(upgrade, 'command', side_effect=lambda args, **kw: calls.append((args, kw)) or b'{"migrated":true}'):
            transaction.before_switch({})
        self.assertEqual(calls[0], 'drained')
        self.assertEqual(calls[1][0], [str(NEW / 'codex-agent'), 'migrate'])
        self.assertEqual(calls[1][1]['env'], env)
        self.assertNotIn('synthetic-private', str(calls[1][0]))
        self.assertEqual(transaction.state['phase'], 'migration_complete')
        self.assertIs(transaction.state['schema_migrated'], True)

    def test_target_schema_is_idempotent_without_running_migrate(self):
        transaction = self.transaction()
        with patch.object(upgrade, 'assert_stopped'), patch.object(transaction, 'unchanged_environment'), \
                patch.object(transaction, 'check_schema', return_value=upgrade.TARGET_MIGRATIONS), \
                patch.object(upgrade, 'v2_storage_facts'), patch.object(upgrade, 'command') as command:
            transaction.before_switch({})
        command.assert_not_called()
        self.assertEqual(transaction.state['phase'], 'migration_already_applied')
        self.assertIs(transaction.state['migration_executed_this_attempt'], False)

    def test_commit_then_command_error_records_target_not_false_no_migration(self):
        transaction = self.transaction()
        with patch.object(upgrade, 'assert_stopped'), patch.object(transaction, 'unchanged_environment'), \
                patch.object(transaction, 'check_schema', side_effect=[upgrade.MIGRATIONS, upgrade.TARGET_MIGRATIONS]), \
                patch.object(upgrade, 'v2_storage_facts'), patch.object(upgrade, 'v2_migration_environment', return_value={}), \
                patch.object(upgrade, 'command', side_effect=upgrade.UpgradeError('Synthetic command failure')):
            with self.assertRaises(upgrade.UpgradeError):
                transaction.before_switch({})
        self.assertEqual(transaction.state['phase'], 'migration_failed')
        self.assertIs(transaction.state['schema_migrated'], True)
        self.assertEqual(transaction.state['schema_state'], 'target')
        self.assertIs(transaction.state['database_restored'], False)

    def test_unavailable_schema_is_unknown(self):
        transaction = self.transaction()
        with patch.object(transaction, 'check_schema', side_effect=upgrade.UpgradeError('Database unavailable')):
            facts = transaction.observe_schema()
        self.assertIsNone(facts['schema_migrated'])
        self.assertEqual(facts['schema_state'], 'unknown')

    def test_interrupted_migration_recovers_without_running_it_again(self):
        transaction = self.transaction()
        transaction.state['phase'] = 'migration_started'
        with patch.object(upgrade, 'validate_manifest', return_value={}), \
                patch.object(transaction, 'rollback', return_value={}) as rollback, \
                patch.object(transaction, 'before_switch') as migration:
            with self.assertRaisesRegex(upgrade.UpgradeError, 'Interrupted upgrade recovered'):
                transaction.activate()
        rollback.assert_called_once_with(automatic=True)
        migration.assert_not_called()

    def test_changed_live_frontend_blocks_v2_before_storage_or_stop(self):
        transaction = self.transaction()
        capture = {'frontend': {'index.html': 'a' * 64}, 'binary_sha256': 'b' * 64,
            'unit_sha256': upgrade.bytes_sha(UNIT)}
        capture_file = Mock(read_text=Mock(return_value=json.dumps(capture)))
        with patch.object(upgrade, 'regular', return_value=capture_file), \
                patch.object(upgrade, 'v2_storage_facts') as facts:
            with self.assertRaisesRegex(upgrade.UpgradeError, 'exact frontend'):
                transaction.validate_baseline({'frontend': capture['frontend']}, upgrade.parse_unit(UNIT), UNIT,
                    {'index.html': 'c' * 64})
        facts.assert_not_called()

    def test_migration_failure_never_switches_unit_and_recovers_previous_pg(self):
        transaction = self.transaction()
        manifest = {'binary_sha256': 'd' * 64, 'frontend': {'index.html': 'e' * 64}}
        unit_file = Mock(read_bytes=Mock(return_value=UNIT))
        with patch.object(upgrade, 'validate_manifest', return_value=manifest), patch.object(upgrade, 'regular', return_value=unit_file), \
                patch.object(transaction, 'preflight'), patch.object(transaction, 'backup'), \
                patch.object(transaction, 'unchanged_environment'), patch.object(upgrade, 'stop_service'), \
                patch.object(transaction, 'before_switch', side_effect=upgrade.UpgradeError('Migration failed')), \
                patch.object(transaction, 'rollback', return_value={}) as rollback, \
                patch.object(upgrade, 'atomic_write') as atomic, patch.object(upgrade, 'system') as system:
            with self.assertRaisesRegex(upgrade.UpgradeError, 'previous PostgreSQL service is restored'):
                transaction.activate()
        rollback.assert_called_once_with(automatic=True)
        atomic.assert_not_called()
        system.assert_not_called()
        self.assertEqual(transaction.state['phase'], 'activation_failed')

    def test_startup_failure_retains_target_schema_during_rollback(self):
        transaction = self.transaction()
        manifest = {'binary_sha256': 'd' * 64, 'frontend': {'index.html': 'e' * 64}}
        unit_file = Mock(read_bytes=Mock(return_value=UNIT))
        def migration(_):
            transaction.state.update(schema_migrated=True, schema_state='target', database_restored=False)
        with patch.object(upgrade, 'validate_manifest', return_value=manifest), patch.object(upgrade, 'regular', return_value=unit_file), \
                patch.object(transaction, 'preflight'), patch.object(transaction, 'backup'), \
                patch.object(transaction, 'unchanged_environment'), patch.object(upgrade, 'stop_service'), \
                patch.object(transaction, 'before_switch', side_effect=migration), \
                patch.object(transaction, 'rollback', return_value={}) as rollback, \
                patch.object(upgrade, 'atomic_write'), patch.object(upgrade, 'system'), \
                patch.object(upgrade, 'verify_health', side_effect=upgrade.UpgradeError('Startup failed')):
            with self.assertRaises(upgrade.UpgradeError):
                transaction.activate()
        rollback.assert_called_once_with(automatic=True)
        self.assertIs(transaction.state['schema_migrated'], True)
        self.assertIs(transaction.state['database_restored'], False)

    def test_rollback_uses_only_old_binary_and_current_database(self):
        transaction = self.transaction()
        transaction.state.update(phase='migration_failed', previous_binary_sha256='a' * 64,
            previous_frontend_files={'index.html': 'b' * 64})
        calls = []
        unit_file = Mock(read_bytes=Mock(return_value=UNIT))
        with patch.object(upgrade, 'properties', return_value={}), patch.object(transaction, 'unchanged_environment'), \
                patch.object(transaction, 'before', return_value=UNIT), patch.object(upgrade, 'regular', return_value=unit_file), \
                patch.object(upgrade, 'sha', return_value='a' * 64), \
                patch.object(upgrade, 'file_map', return_value={'index.html': 'b' * 64}), \
                patch.object(upgrade, 'system', side_effect=lambda *args: calls.append(args)), \
                patch.object(upgrade, 'assert_stopped', side_effect=lambda: calls.append(('assert_stopped',))), \
                patch.object(upgrade, 'atomic_write', side_effect=lambda path, raw, mode: calls.append(('unit', raw))), \
                patch.object(upgrade, 'verify_health', return_value={'ok': True}), \
                patch.object(transaction, 'check_schema', return_value=upgrade.TARGET_MIGRATIONS), \
                patch.object(upgrade, 'command') as command:
            transaction.rollback(automatic=True)
        self.assertEqual(calls[:3], [('stop', upgrade.SERVICE), ('assert_stopped',), ('unit', UNIT)])
        self.assertEqual(calls[-1], ('start', upgrade.SERVICE))
        command.assert_not_called()
        self.assertEqual(transaction.state['phase'], 'rolled_back')
        self.assertIs(transaction.state['schema_migrated'], True)
        self.assertIs(transaction.state['database_restored'], False)


if __name__ == '__main__':
    unittest.main()
