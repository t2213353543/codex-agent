"""Pure temporary-file release tests; no SSH, systemd, HTTP or database calls."""
import contextlib
import copy
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import MagicMock, patch


ROOT = Path(__file__).resolve().parents[1]
SPEC = importlib.util.spec_from_file_location('powerpoint_release_remote', ROOT / 'deploy/powerpoint_release.py')
release = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(release)
ELF = b'\x7fELF\x02\x01' + bytes(12) + b'\x3e\x00' + b'synthetic ELF fixture'


class PowerPointReleaseTests(unittest.TestCase):
    def test_quoted_effective_dropin_is_selected_and_rewritten(self):
        with tempfile.TemporaryDirectory() as temporary:
            root=Path(temporary);base=root/'service';first=root/'a.conf';last=root/'z.conf'
            base.write_text('[Service]\nExecStart=/old/base --web /old/web\n')
            first.write_text('[Service]\nEnvironment=SECRET=hidden\n')
            last.write_text('[Service]\nExecStart=\nExecStart="/runtime/codex-agent" "--listen" "127.0.0.1:18879" "--web" "/runtime/frontend/dist"\n')
            chosen,others=release.select_effective_unit(base,[first,last])
            self.assertEqual(chosen,last);self.assertEqual(others,[base,first])
            updated=release.candidate_unit(last.read_text(),{'binary':'/runtime/codex-agent','web':'/runtime/frontend/dist'},Path('/candidate'))
            self.assertIn('"'+str(Path('/candidate')/'codex-agent')+'"',updated);self.assertIn('"'+str(Path('/candidate')/'frontend/dist')+'"',updated)

    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name).resolve()
        self.root.chmod(0o755)
        self.old = self.root / 'releases' / ('a' * 64)
        self.new = self.root / 'releases' / ('b' * 64)
        self.unit = self.root / 'service'
        self.env = self.root / 'production.env'
        self.key = self.root / 'client-key'
        self.data = self.root / 'user-data.json'
        self.write(self.old / 'codex-agent', ELF + b'old', 0o755)
        self.write(self.old / 'frontend/dist/index.html', b'old UI')
        self.write(self.env, b'SYNTHETIC_ENV=keep\n')
        self.write(self.key, b'synthetic-admin-secret')
        self.write(self.data, b'current user data')
        self.original = f'[Service]\nExecStart={self.old / "codex-agent"} --listen 127.0.0.1:18879 --web {self.old / "frontend/dist"}\nTimeoutStopSec=180\n'
        self.write(self.unit, self.original.encode())
        self.migrations = {'migrations/000_core.sql': 'd' * 64}
        self.source_files = {'main.go': '1' * 64, **self.migrations}
        self.source_hash = hashlib.sha256(json.dumps(self.source_files, sort_keys=True).encode()).hexdigest()
        self.write(self.new / 'codex-agent', ELF + b'new', 0o755)
        self.write(self.new / 'frontend/dist/index.html', b'new UI')
        self.write(self.new / 'deploy/powerpoint_release.py', b'# synthetic executor\n')
        self.write(self.new / 'source-manifest.json', json.dumps({'sha256': self.source_hash, 'files': self.source_files}).encode())
        self.acceptance = {'passed': True, 'source_sha256': self.source_hash,
                           'binary_sha256': release.digest(self.new / 'codex-agent'),
                           'checks': dict.fromkeys(release.REQUIRED_CHECKS, True)}
        self.manifest = {'backend': 'powerpoint_bps', 'source_sha256': self.source_hash,
                         'migration_checksums': self.migrations, 'files': {}}
        self.rehash()
        self.stack = contextlib.ExitStack()
        self.addCleanup(self.stack.close)
        for name, value in [('ROOT', self.root), ('UNIT', self.unit), ('ENV', self.env), ('ADMIN_KEY', self.key)]:
            self.stack.enter_context(patch.object(release, name, value))
        self.baseline = {'pid': 42, 'binary': str(self.old / 'codex-agent'), 'web': str(self.old / 'frontend/dist'),
                         'binary_sha256': release.digest(self.old / 'codex-agent'), 'web_sha256': release.digest(self.old / 'frontend/dist/index.html'),
                         'unit_sha256': release.digest(self.unit), 'env_sha256': release.digest(self.env)}
        self.commands = []
        self.stack.enter_context(patch.object(release, 'schema', return_value=copy.deepcopy(self.migrations)))
        self.stack.enter_context(patch.object(release, 'current', return_value=copy.deepcopy(self.baseline)))
        self.stack.enter_context(patch.object(release, 'command', side_effect=self.command))
        self.stop = self.stack.enter_context(patch.object(release, 'stop_service'))
        self.health = self.stack.enter_context(patch.object(release, 'health', return_value={'binary_sha256': self.manifest['files']['codex-agent']}))

    def write(self, path, raw, mode=0o644):
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(raw)
        path.chmod(mode)

    def rehash(self):
        self.write(self.new / 'acceptance.json', json.dumps(self.acceptance).encode())
        self.manifest['files'] = {p.relative_to(self.new).as_posix(): release.digest(p)
                                  for p in self.new.rglob('*') if p.is_file() and p.name != 'manifest.json'}
        self.write(self.new / 'manifest.json', json.dumps(self.manifest).encode())

    def command(self, *args, **kwargs):
        self.commands.append((args, kwargs))
        if '-p' in args and args[args.index('-p') + 1] == 'TimeoutStopUSec':
            return '3min'
        return ''

    def preflight(self):
        return release.perform('preflight', self.new)

    def test_valid_artifacts_and_successful_switch_preserve_data_and_environment(self):
        self.assertEqual(release.validate(self.new), self.manifest)
        self.assertTrue(self.preflight()['preflight'])
        result = release.perform('activate', self.new)
        self.assertTrue(result['deployed'])
        self.assertIn(str(self.new / 'codex-agent'), self.unit.read_text())
        self.assertEqual(self.data.read_bytes(), b'current user data')
        self.assertEqual(self.env.read_bytes(), b'SYNTHETIC_ENV=keep\n')
        self.assertTrue(self.health.call_args.kwargs['require_bps'])
        self.assertEqual(self.health.call_args.kwargs['expected_path'], self.new / 'codex-agent')
        self.assertFalse(any(x[0][0] == 'docker' for x in self.commands))
        state = release.read_json(self.root / 'release-state' / self.new.name / 'state.json')
        self.assertEqual(state['phase'], 'active')

    def test_manifest_paths_and_unmanifested_files_are_rejected(self):
        for name in ['../escape', '/absolute', 'C:/escape', 'frontend\\dist\\evil', 'frontend//dist/evil']:
            with self.subTest(name=name), self.assertRaises(RuntimeError):
                release.relative_name(name)
        self.write(self.new / 'unmanifested-file', b'not reviewed')
        with self.assertRaisesRegex(RuntimeError, 'unmanifested'):
            release.validate(self.new)

    def test_symlink_release_or_artifact_is_not_resolved_into_trust(self):
        link = self.root / 'releases' / ('e' * 64)
        try:
            link.symlink_to(self.new, target_is_directory=True)
        except OSError:
            self.skipTest('OS account cannot create a fixture symlink')
        with self.assertRaisesRegex(RuntimeError, 'release directory'):
            release.validate(link)
        (self.new / 'frontend/dist/index.html').unlink()
        (self.new / 'frontend/dist/index.html').symlink_to(self.old / 'frontend/dist/index.html')
        with self.assertRaises(RuntimeError):
            release.validate(self.new)

    def test_bad_checks_binary_and_source_binding_fail_before_stop(self):
        for mutation in ['check', 'binary', 'source', 'elf']:
            with self.subTest(mutation=mutation):
                accepted = copy.deepcopy(self.acceptance)
                original_binary = (self.new / 'codex-agent').read_bytes()
                if mutation == 'check':
                    self.acceptance['checks']['redis'] = 'true'
                elif mutation == 'binary':
                    self.acceptance['binary_sha256'] = '0' * 64
                elif mutation == 'source':
                    self.acceptance['source_sha256'] = '0' * 64
                else:
                    self.write(self.new / 'codex-agent', b'not ELF', 0o755)
                    self.acceptance['binary_sha256'] = release.digest(self.new / 'codex-agent')
                self.rehash()
                with self.assertRaises(RuntimeError):
                    self.preflight()
                self.stop.assert_not_called()
                self.acceptance = accepted
                self.write(self.new / 'codex-agent', original_binary, 0o755)
                self.rehash()

    def test_duplicate_json_manifest_field_is_rejected(self):
        (self.new / 'manifest.json').write_text('{"backend":"powerpoint_bps","backend":"other"}')
        with self.assertRaisesRegex(RuntimeError, 'duplicate'):
            release.validate(self.new)

    def test_source_map_content_is_hashed_instead_of_trusting_its_claim(self):
        self.write(self.new / 'source-manifest.json', json.dumps({'sha256': self.source_hash, 'files': {'main.go': 'f' * 64, **self.migrations}}).encode())
        self.rehash()
        with self.assertRaisesRegex(RuntimeError, 'source manifest identity'):
            release.validate(self.new)

    def test_current_supports_distinct_legacy_release_roots_and_checks_executable_bytes(self):
        module = {'__name__': 'test_powerpoint_runtime'}
        exec(SPEC.loader.get_code(SPEC.name), module)
        old_binary = self.root / 'compact-allocation-releases' / ('e' * 64) / 'codex-agent'
        old_web = self.root / 'sub2-cache-releases' / ('f' * 64) / 'frontend/dist'
        self.write(old_binary, ELF + b'legacy', 0o755)
        self.write(old_web / 'index.html', b'legacy UI')
        proc = self.root / 'proc'
        args = [str(old_binary), '--web', str(old_web)]
        self.write(proc / '99/cmdline', ('\0'.join(args) + '\0').encode())
        self.write(proc / '99/exe', old_binary.read_bytes())
        module.update(ROOT=self.root, PROC=proc, UNIT=self.unit, ENV=self.env, command=lambda *a, **k: '99')
        facts = module['current']()
        self.assertEqual(facts['binary'], str(old_binary))
        self.assertEqual(facts['web'], str(old_web))
        old_binary.write_bytes(ELF + b'replaced on disk')
        with self.assertRaisesRegex(RuntimeError, 'on-disk baseline binary differ'):
            module['current']()

    def test_preflight_binds_manifest_and_actual_running_baseline(self):
        self.preflight()
        self.manifest['note'] = 'changed after review'
        self.rehash()
        with self.assertRaisesRegex(RuntimeError, 'changed since preflight'):
            release.perform('activate', self.new)
        self.stop.assert_not_called()
        self.preflight()
        with patch.object(release, 'current', return_value={**self.baseline, 'binary_sha256': 'f' * 64}):
            with self.assertRaisesRegex(RuntimeError, 'changed since preflight'):
                release.perform('activate', self.new)
        self.stop.assert_not_called()

    def test_failed_candidate_rolls_back_only_unit_and_keeps_new_user_data(self):
        self.preflight()
        def check(*args, **kwargs):
            if kwargs.get('require_bps'):
                self.data.write_bytes(b'user data written during candidate runtime')
                raise RuntimeError('synthetic startup failure')
            return self.baseline
        self.health.side_effect = check
        with self.assertRaisesRegex(RuntimeError, 'was rolled back'):
            release.perform('activate', self.new)
        self.assertEqual(self.unit.read_text(), self.original)
        self.assertEqual(self.data.read_bytes(), b'user data written during candidate runtime')
        self.assertEqual(self.env.read_bytes(), b'SYNTHETIC_ENV=keep\n')
        state = release.read_json(self.root / 'release-state' / self.new.name / 'state.json')
        self.assertEqual(state['phase'], 'rolled_back')
        self.assertIs(state['database_restored'], False)
        self.assertIs(state['environment_restored'], False)

    def test_health_failure_stage_survives_successful_rollback_without_secret_text(self):
        self.preflight()
        self.health.side_effect = [release.HealthCheckError('inference_http', 12), self.baseline]
        with self.assertRaisesRegex(RuntimeError, r'was rolled back \(stage=inference_http\)'):
            release.perform('activate', self.new)
        state = release.read_json(self.root / 'release-state' / self.new.name / 'state.json')
        self.assertEqual(state['phase'], 'rolled_back')
        self.assertEqual(state['activation_failure'], {'stage': 'health', 'code': 'health_timeout',
                                                       'health_stage': 'inference_http', 'attempts': 12})
        self.assertEqual(self.unit.read_text(), self.original)
        self.assertEqual(self.data.read_bytes(), b'current user data')

    def test_external_exception_text_is_never_written_to_failure_journal(self):
        self.preflight()
        private_message = 'SECRET_TOKEN response-body contains private content'
        self.health.side_effect = [RuntimeError(private_message), self.baseline]
        with self.assertRaises(RuntimeError) as caught:
            release.perform('activate', self.new)
        raw = (self.root / 'release-state' / self.new.name / 'state.json').read_text()
        self.assertNotIn(private_message, raw)
        self.assertNotIn('SECRET_TOKEN', raw + str(caught.exception))
        self.assertEqual(json.loads(raw)['activation_failure'], {'stage': 'health', 'code': 'check_failed'})

    def test_failed_rollback_records_its_separate_stage_and_preserves_original_failure(self):
        self.preflight()
        self.health.side_effect = [release.HealthCheckError('root_hash', 3), release.HealthCheckError('ready', 4)]
        with self.assertRaisesRegex(RuntimeError, 'rollback incomplete'):
            release.perform('activate', self.new)
        state = release.read_json(self.root / 'release-state' / self.new.name / 'state.json')
        self.assertEqual(state['phase'], 'rollback_failed')
        self.assertEqual(state['activation_failure']['health_stage'], 'root_hash')
        self.assertEqual(state['rollback_failure']['health_stage'], 'ready')

    def test_rollback_does_not_overwrite_an_external_unit_change(self):
        self.preflight()
        changed = '[Service]\nExecStart=/operator/changed\n'
        def check(*args, **kwargs):
            self.unit.write_text(changed)
            raise RuntimeError('synthetic failure')
        self.health.side_effect = check
        with self.assertRaisesRegex(RuntimeError, 'another deployment'):
            release.perform('activate', self.new)
        self.assertEqual(self.unit.read_text(), changed)

    def test_schema_drift_never_runs_a_restore_or_old_writer(self):
        self.preflight()
        self.health.side_effect = RuntimeError('synthetic failure')
        with patch.object(release, 'schema', side_effect=[self.migrations, self.migrations, {'unexpected.sql': 'f' * 64}]):
            with self.assertRaisesRegex(RuntimeError, 'rollback incomplete'):
                release.perform('activate', self.new)
        starts = [c for c, _ in self.commands if c[:2] == ('systemctl', 'start')]
        self.assertEqual(len(starts), 1)  # Candidate only; incompatible old writer remains stopped.
        self.assertEqual(self.data.read_bytes(), b'current user data')
        state = release.read_json(self.root / 'release-state' / self.new.name / 'state.json')
        self.assertEqual(state['phase'], 'rollback_failed')

    def test_interrupted_activation_recovers_before_new_preflight(self):
        self.preflight()
        private = self.root / 'release-state' / self.new.name
        freeze = release.read_json(private / 'preflight.json')
        release.atomic_write(private / 'previous.service', self.original)
        release.write_json(private / 'state.json', dict(freeze, phase='switching'))
        self.unit.write_bytes(release.candidate_unit(self.original, self.baseline, self.new).encode('utf-8'))
        self.health.return_value = self.baseline
        with self.assertRaisesRegex(RuntimeError, 'interrupted activation recovered'):
            release.perform('activate', self.new)
        self.assertEqual(self.unit.read_text(), self.original)
        self.assertEqual(release.read_json(private / 'state.json')['phase'], 'rolled_back')

    def test_timeout_covers_service_drain_and_rejects_unbounded_values(self):
        self.assertEqual(release.stop_timeout('3min'), 210)
        self.assertEqual(release.stop_timeout('1min 30s'), 120)
        for value in ['infinity', '0', '0s', '11min', 'unknown']:
            with self.subTest(value=value), self.assertRaises(RuntimeError):
                release.stop_timeout(value)
        # Invoke the actual function, replacing only the external system command.
        actual = SPEC.loader.get_code(SPEC.name)
        module = {'__name__': 'test_powerpoint_timeout'}
        exec(actual, module)
        events = []
        def system(*args, **kwargs):
            events.append((args, kwargs))
            if '-p' in args:
                return {'TimeoutStopUSec': '3min', 'MainPID': '0', 'ActiveState': 'inactive'}[args[args.index('-p') + 1]]
            return ''
        module['command'] = system
        module['stop_service']()
        stopped = next(kw for args, kw in events if args[:2] == ('systemctl', 'stop'))
        self.assertEqual(stopped['timeout'], 210)


class PowerPointHealthTests(unittest.TestCase):
    def setUp(self):
        self.real_health = release.health

    def test_frontend_hash_and_backend_are_checked_without_returning_admin_secret(self):
        with tempfile.TemporaryDirectory() as tmp:
            key = Path(tmp).resolve() / 'key'
            key.write_text('synthetic-secret-only-in-memory')
            facts = {'binary_sha256': 'b' * 64, 'binary': '/candidate/codex-agent', 'web': '/candidate/frontend/dist'}
            seen = []
            def http(path, token=None):
                seen.append((path, token))
                if path == '/':
                    return b'expected frontend'
                if path == '/api/settings':
                    return b'{"inference":{"backend":"powerpoint_bps","codex_fallback":false},"ignored":"do not expose settings"}'
                return b'{}'
            with patch.object(release, 'ADMIN_KEY', key), patch.object(release, 'current', return_value=facts), patch.object(release, 'http_get', side_effect=http):
                result = self.real_health('b' * 64, '/candidate/frontend/dist', hashlib.sha256(b'expected frontend').hexdigest(), require_bps=True, expected_path='/candidate/codex-agent')
            self.assertEqual(result['inference_backend'], 'powerpoint_bps')
            self.assertNotIn('secret', json.dumps(result))
            self.assertNotIn('ignored', result)
            self.assertEqual(seen[-1], ('/api/settings', 'synthetic-secret-only-in-memory'))

    def test_wrong_root_or_backend_cannot_pass_health(self):
        facts = {'binary_sha256': 'b' * 64, 'binary': '/candidate/codex-agent', 'web': '/candidate/frontend/dist'}
        with patch.object(release, 'current', return_value=facts), patch.object(release, 'http_get', return_value=b'wrong UI'), \
                patch.object(release.time, 'monotonic', side_effect=[0, 0, 61]), patch.object(release.time, 'sleep'):
            with self.assertRaisesRegex(RuntimeError, 'health verification'):
                self.real_health('b' * 64, '/candidate/frontend/dist', 'e' * 64)

    def test_last_failed_health_stage_is_precise_and_excludes_response_or_token(self):
        good = {'binary_sha256': 'b' * 64, 'binary': '/candidate/codex-agent', 'web': '/candidate/frontend/dist'}
        stages = ['current', 'binary', 'path', 'web', 'live', 'ready', 'root', 'root_hash',
                  'admin_credential', 'inference_http', 'inference_json', 'inference']
        for wanted in stages:
            with self.subTest(stage=wanted), contextlib.ExitStack() as stack:
                facts = dict(good)
                if wanted in ('binary', 'path', 'web'):
                    facts[{'binary': 'binary_sha256', 'path': 'binary', 'web': 'web'}[wanted]] = 'wrong'
                if wanted == 'current':
                    stack.enter_context(patch.object(release, 'current', side_effect=RuntimeError('PRIVATE_RESPONSE')))
                else:
                    stack.enter_context(patch.object(release, 'current', return_value=facts))
                def http(path, token=None):
                    label = {'/health/live': 'live', '/health/ready': 'ready', '/': 'root', '/api/settings': 'inference_http'}[path]
                    if label == wanted:
                        raise RuntimeError('PRIVATE_RESPONSE containing SECRET_TOKEN')
                    if path == '/':
                        return b'wrong root' if wanted == 'root_hash' else b'expected frontend'
                    if path == '/api/settings':
                        if wanted == 'inference_json':
                            return b'PRIVATE_RESPONSE not valid JSON'
                        return b'{"inference":{"backend":"wrong"}}'
                    return b'{}'
                stack.enter_context(patch.object(release, 'http_get', side_effect=http))
                credential = patch.object(release, 'read_admin_credential', side_effect=RuntimeError('SECRET_TOKEN')) if wanted == 'admin_credential' else patch.object(release, 'read_admin_credential', return_value='SECRET_TOKEN')
                stack.enter_context(credential)
                stack.enter_context(patch.object(release.time, 'monotonic', side_effect=[0, 0, 61]))
                stack.enter_context(patch.object(release.time, 'sleep'))
                with self.assertRaises(release.HealthCheckError) as caught:
                    self.real_health('b' * 64, '/candidate/frontend/dist', hashlib.sha256(b'expected frontend').hexdigest(), require_bps=True, expected_path='/candidate/codex-agent')
                self.assertEqual(caught.exception.stage, wanted)
                self.assertEqual(caught.exception.attempts, 1)
                diagnostic = json.dumps(release.failure_diagnostic('health', caught.exception))
                self.assertNotIn('SECRET_TOKEN', diagnostic + str(caught.exception))
                self.assertNotIn('PRIVATE_RESPONSE', diagnostic + str(caught.exception))

    def test_admin_read_supports_dynamicuser_parent_link_without_relaxing_artifacts(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp).resolve()
            actual = root / 'private/codex-agent'
            actual.mkdir(parents=True)
            key = actual / 'client-key'
            key.write_text('synthetic-parent-link-key')
            exposed = root / 'codex-agent'
            try:
                exposed.symlink_to(actual, target_is_directory=True)
            except OSError:
                self.skipTest('OS account cannot create a fixture directory symlink')
            with patch.object(release, 'ADMIN_KEY', exposed / 'client-key'):
                self.assertEqual(release.read_admin_credential(), 'synthetic-parent-link-key')
            with self.assertRaisesRegex(RuntimeError, 'symlink file ancestor'):
                release.regular(exposed / 'client-key')
            leaf = root / 'key-link'
            leaf.symlink_to(key)
            with patch.object(release, 'ADMIN_KEY', leaf), self.assertRaisesRegex(RuntimeError, 'credential file'):
                release.read_admin_credential()

    def test_redirects_are_disabled_before_sending_admin_credentials_elsewhere(self):
        opener = MagicMock()
        response = opener.open.return_value.__enter__.return_value
        response.status = 200
        response.geturl.return_value = 'http://127.0.0.1:18879/api/settings'
        response.read.return_value = b'{}'
        with patch.object(release.urllib.request, 'build_opener', return_value=opener) as build:
            release.http_get('/api/settings', 'synthetic-secret')
        handlers = build.call_args.args
        redirect = next(h for h in handlers if isinstance(h, release.urllib.request.HTTPRedirectHandler))
        self.assertIsNone(redirect.redirect_request(None, None, 302, '', {}, 'https://outside.invalid'))
        self.assertEqual(next(h for h in handlers if isinstance(h, release.urllib.request.ProxyHandler)).proxies, {})


if __name__ == '__main__':
    unittest.main()
