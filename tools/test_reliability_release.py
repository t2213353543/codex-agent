"""Pure temporary-file reliability release tests; no production side effects."""
import contextlib
import copy
import importlib.util
import json
import os
from pathlib import Path, PureWindowsPath
import shutil
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[1]
SPEC = importlib.util.spec_from_file_location('reliability_executor_test', ROOT / 'deploy/reliability_release.py')
release = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(release)
TOOL_SPEC = importlib.util.spec_from_file_location('reliability_prepare_test', ROOT / 'tools/reliability_release.py')
prepare_release = importlib.util.module_from_spec(TOOL_SPEC)
TOOL_SPEC.loader.exec_module(prepare_release)
REAL_CANDIDATE_COMMAND = release.candidate_command
REAL_HEALTH = release.health
ELF = b'\x7fELF\x02\x01' + bytes(12) + b'\x3e\x00' + b'synthetic ELF fixture'


class ReliabilityReleaseTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name).resolve()
        self.root.chmod(0o755)
        self.old = self.root / 'releases' / ('a' * 64)
        self.new = self.root / 'releases' / ('b' * 64)
        self.unit, self.env = self.root / 'service', self.root / 'production.env'
        self.migration_env = self.root / 'migration.env'
        self.runtime_dsn = 'postgresql://codex_agent_runtime:runtime-secret@127.0.0.1:25433/codex_agent?sslmode=disable'
        self.migration_dsn = 'postgresql://codex_agent_migrator:migrator-secret@127.0.0.1:25433/codex_agent?sslmode=disable'
        self.runtime_environment = {'SECRET': 'never-persist-process-env', 'DATABASE_URL': self.runtime_dsn}
        self.cli_source = self.root / 'original-cli'
        self.stack = contextlib.ExitStack()
        self.addCleanup(self.stack.close)
        for name, value in [('ROOT', self.root), ('UNIT', self.unit), ('ENV', self.env)]:
            self.stack.enter_context(patch.object(release.base, name, value))
        self.stack.enter_context(patch.object(release, 'CLI_SOURCE', self.cli_source))
        self.stack.enter_context(patch.object(release, 'MIGRATION_ENV', self.migration_env))
        self.write(self.migration_env, ('DATABASE_URL=' + self.migration_dsn + '\n').encode(), 0o600)
        self.write(self.old / 'codex-agent', ELF + b'old', 0o755)
        self.write(self.old / 'frontend/dist/index.html', b'old UI')
        self.original_unit = f'[Service]\nExecStart={self.old / "codex-agent"} --web {self.old / "frontend/dist"}\nTimeoutStopSec=180\n'
        self.write(self.unit, self.original_unit.encode())
        self.write(self.env, b'SECRET=never-persist-process-env\n')
        self.data = self.root / 'user-data'
        self.write(self.data, b'current user data')
        self.write(self.cli_source / 'bin/codex', ELF + b'cli', 0o755)
        self.write(self.cli_source / 'bin/codex-code-mode-host', ELF + b'host', 0o755)
        self.write(self.cli_source / 'codex-package.json', json.dumps({'layoutVersion': 1, 'entrypoint': 'bin/codex'}).encode())
        self.cli = {'source_root': self.cli_source.as_posix(), 'entrypoint': 'bin/codex', 'files': {p.relative_to(self.cli_source).as_posix(): release.base.digest(p) for p in self.cli_source.rglob('*') if p.is_file()}}
        self.cli['binary_sha256'] = self.cli['files']['bin/codex']
        self.migrations = {name: 'd' * 64 for name in release.LEGACY_NAMES}
        self.write(self.new / release.ADDED, (ROOT / release.ADDED).read_bytes())
        self.migrations[release.ADDED] = release.base.digest(self.new / release.ADDED)
        self.schema = {k: v for k, v in self.migrations.items() if k != release.ADDED}
        self.source = {'files': {'main.go': '1' * 64, 'go.mod': '2' * 64, 'frontend/src/app.vue': '3' * 64,
                       'internal/server/admin_account_window_cost.go': '4' * 64,
                       'internal/server/account_window_prices.json': '5' * 64,
                       'internal/server/admin_account_window_parity_test.go': '6' * 64, **self.migrations}}
        self.source['sha256'] = release.map_digest(self.source['files'])
        self.write(self.new / 'codex-agent', ELF + b'new', 0o755)
        self.write(self.new / 'frontend/dist/index.html', b'new UI')
        for name in ('deploy/powerpoint_release.py', 'deploy/reliability_release.py'):
            self.write(self.new / name, b'# synthetic executor\n')
        self.write(self.new / 'source-manifest.json', json.dumps(self.source).encode())
        self.write(self.new / 'evidence/synthetic', b'passing fixture evidence')
        reference = {'path': '.local/evidence.json', 'artifact_path': 'evidence/synthetic', 'sha256': release.base.digest(self.new / 'evidence/synthetic')}
        self.acceptance = {'passed': True, 'source_sha256': self.source['sha256'], 'binary_sha256': release.base.digest(self.new / 'codex-agent'),
                           'checks': dict.fromkeys(release.GATES, True), 'evidence_files': [reference],
                           'gates': {name: {'passed': True, 'scope': 'full', 'source_files': copy.deepcopy(self.source['files']), 'source_sha256': self.source['sha256'], 'evidence_files': [reference]} for name in release.GATES}}
        self.acceptance['gates']['frontend']['artifact_files'] = {'frontend/dist/index.html': release.base.digest(self.new / 'frontend/dist/index.html')}
        self.manifest = {'version': '0.4.0', 'backend': 'selectable_official_runtime', 'source_sha256': self.source['sha256'], 'migration_checksums': self.migrations, 'official_cli': self.cli}
        self.rehash()
        release.stage_cli(self.cli)
        self.commands, self.storage_commands = [], []
        self.stack.enter_context(patch.object(release.base, 'schema', side_effect=lambda: copy.deepcopy(self.schema)))
        self.stack.enter_context(patch.object(release.base, 'current', side_effect=self.current))
        self.stack.enter_context(patch.object(release.base, 'command', side_effect=self.command))
        self.stop = self.stack.enter_context(patch.object(release.base, 'stop_service'))
        self.stack.enter_context(patch.object(release.base, 'health', side_effect=lambda *a, **kw: {'binary': str(self.old / 'codex-agent')}))
        self.stack.enter_context(patch.object(release, 'health', side_effect=self.health))
        self.stack.enter_context(patch.object(release, 'process_environment', side_effect=lambda _: dict(self.runtime_environment)))
        self.stack.enter_context(patch.object(release, 'maintenance_role_facts', return_value={'reviewed_permissions': True}))
        self.candidate = self.stack.enter_context(patch.object(release, 'candidate_command', side_effect=self.candidate_command))
        self.fail_command = None

    def write(self, path, raw, mode=0o644):
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(raw)
        path.chmod(mode)

    def rehash(self):
        self.write(self.new / 'acceptance.json', json.dumps(self.acceptance).encode())
        self.manifest['files'] = {p.relative_to(self.new).as_posix(): release.base.digest(p) for p in self.new.rglob('*') if p.is_file() and p.name != 'manifest.json'}
        self.write(self.new / 'manifest.json', json.dumps(self.manifest).encode())

    def current(self):
        return {'pid': 42, 'binary': str(self.old / 'codex-agent'), 'web': str(self.old / 'frontend/dist'),
                'binary_sha256': release.base.digest(self.old / 'codex-agent'), 'web_sha256': release.base.digest(self.old / 'frontend/dist/index.html'),
                'unit_sha256': release.base.digest(self.unit), 'env_sha256': release.base.digest(self.env), 'auxiliary_units': {}}

    def command(self, *args, **kwargs):
        self.commands.append(args)
        return '3min' if 'TimeoutStopUSec' in args else ''

    def candidate_command(self, binary, action, environment):
        self.storage_commands.append(action)
        self.assertEqual(environment, {**self.runtime_environment, 'DATABASE_URL': self.migration_dsn})
        if action == 'migrate':
            self.schema = copy.deepcopy(self.migrations)
        if action == self.fail_command:
            raise RuntimeError('private DB diagnostic must never persist')
        return {'migrated' if action == 'migrate' else 'rebuilt': True}

    def health(self, path, manifest, environment):
        self.assertEqual(environment, self.runtime_environment)
        self.assertNotIn('migrator-secret', json.dumps(environment))
        return {'version': manifest['version']}

    def activate(self):
        release.perform('preflight', self.new)
        return release.perform('activate', self.new)

    def state(self):
        return release.base.read_json(self.root / 'release-state' / self.new.name / 'state.json')

    def test_success_migrates_rebuilds_and_keeps_environment_data(self):
        result = self.activate()
        self.assertTrue(result['deployed'])
        self.assertTrue(result['usage_rebuilt'])
        self.assertEqual(self.storage_commands, ['migrate', 'rebuild-usage'])
        self.assertEqual(self.schema, self.migrations)
        self.assertEqual(self.data.read_bytes(), b'current user data')
        self.assertEqual(self.env.read_bytes(), b'SECRET=never-persist-process-env\n')
        self.assertIn('CODEX_CLI_PATH=' + str(release.cli_destination(self.cli) / 'bin/codex'), self.unit.read_text())
        self.assertIn('CODEX_RUNTIME_DIR=' + release.CLI_RUNTIME, self.unit.read_text())
        self.assertIn('TasksMax=infinity\n', self.unit.read_text())
        self.assertIn('MemoryHigh=infinity\n', self.unit.read_text())
        self.assertIn('MemoryMax=infinity\n', self.unit.read_text())
        self.assertIn('MemorySwapMax=infinity\n', self.unit.read_text())
        self.assertIn('CPUQuota=\n', self.unit.read_text())
        state = self.state()
        self.assertEqual(state['phase'], 'active')
        self.assertEqual(state['schema_actual'], self.migrations)
        self.assertNotIn('schema_unchanged', state)
        self.assertNotIn('never-persist-process-env', json.dumps(state))

    def test_failed_rebuild_rolls_back_binary_but_retains_nullable_schema(self):
        self.fail_command = 'rebuild-usage'
        with self.assertRaisesRegex(RuntimeError, 'old binary was restored'):
            self.activate()
        self.assertEqual(self.unit.read_text(), self.original_unit)
        self.assertEqual(self.schema, self.migrations)
        self.assertEqual(self.state()['phase'], 'rolled_back')
        self.assertEqual(self.state()['schema_actual'], self.migrations)
        self.assertFalse(self.state()['database_restored'])
        self.assertNotIn('private DB diagnostic', json.dumps(self.state()))
        self.assertFalse(any('restore' in str(command) for command in self.commands))

    def test_migration_process_failure_after_commit_records_actual_schema(self):
        self.fail_command = 'migrate'
        with self.assertRaises(RuntimeError):
            self.activate()
        self.assertEqual(self.schema, self.migrations)
        self.assertEqual(self.state()['schema_actual'], self.migrations)
        self.assertEqual(self.storage_commands, ['migrate'])

    def test_unreviewed_schema_is_rejected_before_stop(self):
        for schema in ({**self.schema, 'migrations/other.sql': '9' * 64},
                       {**self.schema, 'migrations/000_core.sql': '8' * 64}):
            self.schema = schema
            with self.assertRaisesRegex(RuntimeError, 'unchanged first five or exact reviewed six'):
                release.perform('preflight', self.new)
        self.stop.assert_not_called()

    def test_exact_six_migrations_allow_safe_rebuild_retry(self):
        self.schema = copy.deepcopy(self.migrations)
        result = self.activate()
        self.assertTrue(result['deployed'])
        self.assertEqual(self.storage_commands, ['migrate', 'rebuild-usage'])
        self.assertEqual(self.state()['schema_before'], self.migrations)
        self.assertEqual(self.state()['schema_actual'], self.migrations)

    def test_patch_release_preserves_exact_schema_and_current_compact_ui(self):
        self.manifest['version'] = '0.4.1'
        self.schema = copy.deepcopy(self.migrations)
        self.acceptance['checks']['schema_unchanged'] = True
        # A reviewed UI-only hot update need not match the old package manifest.
        # The executor freezes actual running index.html, then publishes only
        # the current candidate build bound by its frontend evidence.
        self.write(self.old / 'frontend/dist/index.html', b'live compact UI hot update')
        self.write(self.old / 'frontend/dist/assets/compact.js', b'live compact UI asset')
        self.write(self.new / 'frontend/dist/index.html', b'compact UI plus hotfix log fields')
        self.write(self.new / 'frontend/dist/assets/compact-log-hotfix.js', b'current built UI asset')
        self.acceptance['gates']['frontend']['artifact_files'] = {
            p.relative_to(self.new).as_posix(): release.base.digest(p)
            for p in (self.new / 'frontend/dist').rglob('*') if p.is_file()}
        self.rehash()
        result = self.activate()
        self.assertTrue(result['deployed'])
        self.assertEqual(result['runtime']['version'], '0.4.1')
        self.assertEqual(self.state()['schema_before'], self.migrations)
        self.assertEqual(self.state()['schema_actual'], self.migrations)
        self.assertEqual(self.schema, self.migrations)
        self.assertIn(str(self.new / 'frontend/dist'), self.unit.read_text())
        self.assertEqual((self.new / 'frontend/dist/index.html').read_bytes(), b'compact UI plus hotfix log fields')
        self.assertEqual((self.old / 'frontend/dist/index.html').read_bytes(), b'live compact UI hot update')
        self.assertEqual(self.env.read_bytes(), b'SECRET=never-persist-process-env\n')
        self.assertEqual(self.data.read_bytes(), b'current user data')

    def test_patch_requires_existing_six_migrations_before_stop(self):
        self.manifest['version'] = '0.4.1'
        self.rehash()
        with self.assertRaisesRegex(RuntimeError, 'exact existing six migrations'):
            release.perform('preflight', self.new)
        self.stop.assert_not_called()

    def test_safe_source_version_is_bound_to_source_manifest(self):
        path = self.root / 'source/internal/server/main.go'
        for version in ('0.4.0', '0.4.1', '0.4.12'):
            self.write(path, f'package server\nconst version = "{version}"\n'.encode())
            source = {'files': {'internal/server/main.go': release.base.digest(path)}}
            self.assertEqual(prepare_release.source_version(self.root / 'source', source), version)
        for text in ('const version = "0.4.1"\nconst version = "0.4.2"\n',
                     'var version = "0.4.1"\n', 'const version = "0.4.1\\n--evil"\n',
                     'const version = "0.4.01"\n', 'const version = "0.5.0"\n'):
            self.write(path, text.encode())
            source = {'files': {'internal/server/main.go': release.base.digest(path)}}
            with self.assertRaises(RuntimeError):
                prepare_release.source_version(self.root / 'source', source)
        self.write(path, b'const version = "0.4.1"\n')
        with self.assertRaisesRegex(RuntimeError, 'version source changed'):
            prepare_release.source_version(self.root / 'source', {'files': {'internal/server/main.go': '0' * 64}})

    def test_manifest_version_rejects_unsafe_or_unreviewed_series(self):
        for version in ('0.4.1\n', '0.4.1-beta', '0.4.01', '0.5.0', '../0.4.1', None, 4):
            with self.subTest(version=version), self.assertRaises(RuntimeError):
                release.release_version(version)
        for version in ('0.4.0', '0.4.1'):
            self.manifest['version'] = version
            self.rehash()
            self.assertEqual(release.validate(self.new)['version'], version)

    def test_private_migration_dsn_must_match_runtime_target_and_role(self):
        for dsn in [self.migration_dsn.replace('25433', '25434'), self.migration_dsn.replace('codex_agent?', 'another_db?'),
                    self.migration_dsn.replace('codex_agent_migrator:', 'postgres:'), self.migration_dsn.replace('sslmode=disable', 'sslmode=disable&options=-csearch_path=other')]:
            self.write(self.migration_env, ('DATABASE_URL=' + dsn + '\n').encode(), 0o600)
            with self.assertRaisesRegex(RuntimeError, 'targets or roles differ'):
                release.perform('preflight', self.new)
        self.stop.assert_not_called()

    def test_migration_file_hash_is_frozen_before_service_stop(self):
        release.perform('preflight', self.new)
        self.write(self.migration_env, ('# changed configuration\nDATABASE_URL=' + self.migration_dsn + '\n').encode(), 0o600)
        with self.assertRaisesRegex(RuntimeError, 'changed since preflight'):
            release.perform('activate', self.new)
        self.stop.assert_not_called()

    def test_candidate_failure_diagnostic_only_includes_sqlstate_and_command(self):
        result = subprocess.CompletedProcess([], 1, b'', b'postgresql://secret:password@host failed: SQLSTATE 42501 private-query')
        with patch.object(release.subprocess, 'run', return_value=result):
            with self.assertRaises(release.CandidateCommandError) as error:
                REAL_CANDIDATE_COMMAND(self.new / 'codex-agent', 'migrate', self.runtime_environment)
        self.assertEqual(error.exception.sqlstate, '42501')
        self.assertEqual(error.exception.action, 'migrate')
        self.assertNotIn('secret', str(error.exception))
        self.assertNotIn('private-query', str(error.exception))

    def test_health_checks_effective_official_runtime_resource_limits(self):
        status = {'version': '0.4.0', 'inference_mode': 'powerpoint_bps', 'inference': {'backend': 'powerpoint_bps'}, 'websocket_enabled': True}
        for tasks, memory, high, swap, passed in [('infinity', 'infinity', 'infinity', 'infinity', True), ('2048', 'infinity', 'infinity', 'infinity', False), ('infinity', '2147483648', 'infinity', 'infinity', False), ('infinity', 'infinity', '6442450944', 'infinity', False), ('infinity', 'infinity', 'infinity', '0', False)]:
            with self.subTest(tasks=tasks, memory=memory, high=high, swap=swap), patch.object(release, 'candidate_command', return_value=status), patch.object(release.base, 'command', side_effect=lambda *args, **kwargs: tasks if 'TasksMax' in args else high if 'MemoryHigh' in args else swap if 'MemorySwapMax' in args else 'infinity' if 'CPUQuotaPerSecUSec' in args else memory):
                if passed:
                    facts = REAL_HEALTH(self.new, self.manifest, self.runtime_environment)
                    self.assertEqual(facts['resource_limits'], {'TasksMax': tasks, 'MemoryHigh': high, 'MemoryMax': memory, 'MemorySwapMax': swap, 'CPUQuotaPerSecUSec': 'infinity'})
                else:
                    with self.assertRaisesRegex(RuntimeError, 'resource limits'):
                        REAL_HEALTH(self.new, self.manifest, self.runtime_environment)

    def test_health_requires_candidate_manifest_version_and_accepts_selected_mode(self):
        self.manifest['version'] = '0.4.1'
        status = {'version': '0.4.1', 'inference_mode': 'official_app_server', 'inference': {'backend': 'official_app_server'}, 'websocket_enabled': True}
        with patch.object(release, 'candidate_command', return_value=status), patch.object(release.base, 'command', return_value='infinity'):
            facts = REAL_HEALTH(self.new, self.manifest, self.runtime_environment)
            self.assertEqual(facts['runtime_status']['inference_mode'], 'official_app_server')
            status['version'] = '0.4.0'
            with self.assertRaisesRegex(RuntimeError, 'runtime settings mismatch'):
                REAL_HEALTH(self.new, self.manifest, self.runtime_environment)

    def test_health_rejects_a_remaining_one_core_cpu_quota(self):
        status = {'version': '0.4.0', 'inference_mode': 'official_app_server', 'inference': {'backend': 'official_app_server'}, 'websocket_enabled': True}
        with patch.object(release, 'candidate_command', return_value=status), patch.object(release.base, 'command', side_effect=lambda *args, **kwargs: '1s' if 'CPUQuotaPerSecUSec' in args else 'infinity'):
            with self.assertRaisesRegex(RuntimeError, 'resource limits'):
                REAL_HEALTH(self.new, self.manifest, self.runtime_environment)

    def test_gate_true_flags_without_matching_source_coverage_are_rejected(self):
        gate = self.acceptance['gates']['official_live']
        gate['source_files'] = {'main.go': self.source['files']['main.go']}
        gate['source_sha256'] = release.map_digest(gate['source_files'])
        self.rehash()
        with self.assertRaisesRegex(RuntimeError, 'scope coverage'):
            release.perform('preflight', self.new)
        self.stop.assert_not_called()

    def test_external_bps_failure_stays_failed_for_limited_official_release(self):
        acceptance = copy.deepcopy(self.acceptance)
        acceptance['verification_scope'] = 'official_app_server'
        acceptance['checks']['bps_live'] = False
        acceptance['gates']['bps_live']['passed'] = False
        acceptance['external_limitations'] = {'bps_live': {'status': 'blocked_upstream', 'http_status': 403, 'message': 'This request was blocked by our usage policy.'}}
        release.validate_evidence(acceptance, self.source, '0.4.3')
        with self.assertRaises(RuntimeError):
            release.validate_evidence(acceptance, self.source, '0.4.0')
        for mutation in ('another_failed_gate', 'false_success', 'wrong_scope', 'missing_proof'):
            value = copy.deepcopy(acceptance)
            if mutation == 'another_failed_gate': value['checks']['official_live'] = False
            elif mutation == 'false_success': value['gates']['bps_live']['passed'] = True
            elif mutation == 'wrong_scope': value['verification_scope'] = 'powerpoint_bps'
            else: value['external_limitations']['bps_live']['http_status'] = 500
            with self.assertRaises(RuntimeError):
                release.validate_evidence(value, self.source, '0.4.3')

    def test_backend_gate_does_not_depend_on_unrelated_packaging_scripts(self):
        source = copy.deepcopy(self.source)
        source['files']['tools/new_release.py'] = 'a' * 64
        source['sha256'] = release.map_digest(source['files'])
        acceptance = copy.deepcopy(self.acceptance)
        acceptance['source_sha256'] = source['sha256']
        for name, gate in acceptance['gates'].items():
            gate['scope'] = 'frontend' if name == 'frontend' else 'backend'
            gate['source_files'] = release.source_scope(source['files'], gate['scope'])
            gate['source_sha256'] = release.map_digest(gate['source_files'])
        release.validate_evidence(acceptance, source)
        source['files']['main.go'] = 'b' * 64
        with self.assertRaisesRegex(RuntimeError, 'stale'):
            release.validate_evidence(acceptance, source)

    def test_retired_runtime_requires_real_bps_gate_and_explicit_removal(self):
        value=copy.deepcopy(self.acceptance)
        value['verification_scope']='powerpoint_bps'
        value['retired_features']=['official_app_server','hosted_codex_tasks']
        value['checks']['official_live']=False
        value['gates']['official_live']['passed']=False
        release.validate_evidence(value,self.source,'0.4.5')
        for change in ('bps_failed','removal_missing','pretend_official_passed'):
            invalid=copy.deepcopy(value)
            if change=='bps_failed': invalid['checks']['bps_live']=False
            elif change=='removal_missing': invalid['retired_features']=[]
            else: invalid['gates']['official_live']['passed']=True
            with self.assertRaises(RuntimeError): release.validate_evidence(invalid,self.source,'0.4.5')

    def test_observed_current_bps_rejection_keeps_live_gate_failed(self):
        value=copy.deepcopy(self.acceptance)
        value.update(verification_scope='powerpoint_bps',retired_features=['official_app_server','hosted_codex_tasks'],bps_composed_verification={'current_full_go_race_and_storage_required':True},external_limitations={'bps_live':{'status':'blocked_upstream','http_status':403,'message':'Current live inference returned HTTP 403'}})
        for name in ('official_live','bps_live'):
            value['checks'][name]=False;value['gates'][name]['passed']=False
        release.validate_evidence(value,self.source,'0.4.6')
        for field in ('signature','local_gate','pretend_success'):
            broken=copy.deepcopy(value)
            if field=='signature': broken['external_limitations']['bps_live']['http_status']=500
            elif field=='local_gate': broken['checks']['race']=False
            else: broken['gates']['bps_live']['passed']=True
            with self.assertRaises(RuntimeError): release.validate_evidence(broken,self.source,'0.4.6')

    def test_runtime_gate_keeps_every_production_source_but_not_unrelated_tests(self):
        source = copy.deepcopy(self.source)
        source['files']['internal/server/example_test.go'] = 'a' * 64
        source['sha256'] = release.map_digest(source['files'])
        acceptance = copy.deepcopy(self.acceptance)
        acceptance['source_sha256'] = source['sha256']
        for name, evidence in acceptance['gates'].items():
            evidence['scope'] = 'frontend' if name == 'frontend' else 'runtime' if name in ('bps_live', 'official_live', 'race', 'redis', 'pg') else 'backend'
            evidence['source_files'] = release.source_scope(source['files'], evidence['scope'])
            evidence['source_sha256'] = release.map_digest(evidence['source_files'])
        release.validate_evidence(acceptance, source)
        source['files']['main.go'] = 'b' * 64
        with self.assertRaisesRegex(RuntimeError, 'stale'):
            release.validate_evidence(acceptance, source)

    def test_no_unchanged_schema_claim_or_unbound_evidence_allowed(self):
        for mutation in ('schema', 'evidence'):
            value = copy.deepcopy(self.acceptance)
            if mutation == 'schema':
                value['checks']['schema_unchanged'] = True
            else:
                value['gates']['pg']['evidence_files'] = [{'path': 'unbound', 'sha256': 'f' * 64}]
            with self.assertRaises(RuntimeError):
                release.validate_evidence(value, self.source)

    def test_stable_ingress_artifacts_are_verified_without_widening_schema(self):
        self.write(self.new / 'codex-agent-ingress', ELF + b'ingress', 0o755)
        self.write(self.new / 'deploy/zero_downtime_release.py', b'# controller\n')
        self.rehash()
        self.assertEqual(release.validate(self.new)['files'], self.manifest['files'])
        self.write(self.new / 'codex-agent-ingress', b'not ELF', 0o755)
        self.rehash()
        with self.assertRaisesRegex(RuntimeError, 'ingress is not Linux'):
            release.validate(self.new)

    def test_legacy_rollback_and_stop_refused_after_stable_ingress_install(self):
        self.stack.enter_context(patch.object(Path, 'exists', return_value=True))
        with self.assertRaisesRegex(RuntimeError, 'stable ingress installed'):
            release.rollback(self.root / 'missing')
        with self.assertRaisesRegex(RuntimeError, 'stable ingress installed'):
            release.base.rollback(self.root / 'missing')
        with self.assertRaisesRegex(RuntimeError, 'stable ingress installed'):
            release.base.require_legacy_service_control()
        self.stop.assert_not_called()
        self.assertEqual(self.commands, [])

    def test_artifact_or_cli_bundle_drift_is_rejected(self):
        self.write(release.cli_destination(self.cli) / 'unreviewed', b'changed')
        with self.assertRaisesRegex(RuntimeError, 'inventory'):
            release.perform('preflight', self.new)
        self.stop.assert_not_called()

    def test_dangerous_005_sql_rejected_even_with_fresh_artifact_hash(self):
        self.write(self.new / release.ADDED, b'DROP TABLE usage_events;')
        self.rehash()
        with self.assertRaisesRegex(RuntimeError, 'reviewed nullable'):
            release.validate(self.new)

    def test_environment_file_override_cannot_shadow_unit_cli_setting(self):
        self.write(self.env, b'CODEX_CLI_PATH=/unexpected/codex\n')
        with self.assertRaisesRegex(RuntimeError, 'already overrides'):
            release.perform('preflight', self.new)
        self.stop.assert_not_called()

    def test_environment_change_after_preflight_prevents_stop(self):
        release.perform('preflight', self.new)
        self.write(self.env, b'SECRET=changed\n')
        with self.assertRaisesRegex(RuntimeError, 'changed since preflight'):
            release.perform('activate', self.new)
        self.stop.assert_not_called()

    def test_rollback_refuses_unknown_schema(self):
        self.activate()
        self.schema['migrations/unreviewed.sql'] = 'f' * 64
        stops = self.stop.call_count
        with self.assertRaisesRegex(RuntimeError, 'unreviewed schema'):
            release.perform('rollback', self.new)
        self.assertEqual(self.stop.call_count, stops)

    def test_frontend_build_cannot_be_replaced_under_valid_source_manifest(self):
        self.write(self.new / 'frontend/dist/index.html', b'different built UI')
        self.rehash()
        with self.assertRaisesRegex(RuntimeError, 'frontend build evidence'):
            release.validate(self.new)

    def test_windows_source_path_serializes_as_remote_posix(self):
        source = PureWindowsPath('/opt/codex-agent-integration/checks/quality-20260924/official-bundle')
        spec = copy.deepcopy(self.cli)
        spec['source_root'] = source.as_posix()
        with patch.object(release, 'CLI_SOURCE', source):
            release.validate_cli_spec(spec)
            spec['source_root'] = str(source)
            with self.assertRaisesRegex(RuntimeError, 'unexpected official CLI source'):
                release.validate_cli_spec(spec)

    def test_remote_executor_import_does_not_create_unmanifested_bytecode(self):
        folder = self.root / 'import-fixture'
        folder.mkdir()
        for name in ('reliability_release.py', 'powerpoint_release.py'):
            shutil.copyfile(ROOT / 'deploy' / name, folder / name)
        result = subprocess.run([sys.executable, str(folder / 'reliability_release.py'), '--help'], capture_output=True, timeout=20)
        self.assertEqual(result.returncode, 0)
        self.assertFalse((folder / '__pycache__').exists())

    def test_all_service_environment_files_are_frozen_and_overrides_rejected(self):
        extra = self.root / 'quality.env'
        self.write(extra, b'UNCHANGED_PUBLIC_SETTING=true\n')
        def command(*args, **kwargs):
            if 'EnvironmentFiles' in args:
                return str(self.env) + ' (ignore_errors=no)\n' + str(extra) + ' (ignore_errors=no)'
            return self.command(*args, **kwargs)
        with patch.object(release.base, 'command', side_effect=command):
            # Test fixtures run on Windows; systemd supplies Linux paths only.
            with patch.object(release, 'environment_files', side_effect=lambda: {str(self.env): release.base.digest(self.env), str(extra): release.base.digest(extra)}):
                release.perform('preflight', self.new)
                self.write(extra, b'UNCHANGED_PUBLIC_SETTING=false\n')
                with self.assertRaisesRegex(RuntimeError, 'changed since preflight'):
                    release.perform('activate', self.new)
                self.stop.assert_not_called()
                self.write(extra, b'CODEX_RUNTIME_DIR=/overridden\n')
                with self.assertRaisesRegex(RuntimeError, 'already overrides'):
                    release.perform('preflight', self.new)


if __name__ == '__main__':
    unittest.main()
