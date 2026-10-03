"""Unified synthetic-only acceptance for environment and workbench integration."""

from project_layout import backend_source_paths
import argparse
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys

ROOT = Path(__file__).resolve().parents[1]


def digest(source_root):
    files = backend_source_paths(source_root)
    mapping = {'source/' + p.relative_to(source_root).as_posix(): hashlib.sha256(p.read_bytes()).hexdigest() for p in sorted(files)}
    harness = list((ROOT / 'integration/gateway_environment').glob('*.py'))
    harness += [ROOT / 'integration/gateway_environment/environment_test.go.txt', ROOT / 'deploy/workbench_bridge.py',
                ROOT / 'integration/gateway_environment/bridge_contract_test.go.txt',
                ROOT / 'deploy/gateway_load_remote.py', ROOT / 'deploy/upgrade_remote.py',
                ROOT / 'tools/upgrade.py', ROOT / 'tools/gateway_release_check.py', ROOT / 'tools/project_layout.py', Path(__file__).resolve()]
    harness += list((ROOT / 'tools').glob('test_upgrade*.py')) + [ROOT / 'tools/test_gateway_release_check.py']
    mapping.update({'harness/' + p.relative_to(ROOT).as_posix(): hashlib.sha256(p.read_bytes()).hexdigest() for p in sorted(harness)})
    return hashlib.sha256(json.dumps(mapping, sort_keys=True).encode()).hexdigest()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--race', action='store_true', help='Requires an existing local CGO compiler')
    parser.add_argument('--release-audit', action='store_true', help='Also enforce the read-only publication audit; missing release prerequisites keep the command nonzero')
    parser.add_argument('--release-config', type=Path, help='JSON of immutable candidate and evidence paths; runs local tests and publication audit')
    selection = parser.add_mutually_exclusive_group()
    selection.add_argument('--source-dir', type=Path, help='Explicit Go source directory; shared workspace remains the default')
    selection.add_argument('--snapshot', type=Path, help='Owner archive directory with source-manifest.json and local-source; also checks Python-to-Go HTTP contract')
    args = parser.parse_args()
    release_options = {}
    release_config_hash = None
    if args.release_config:
        if args.snapshot or args.source_dir:
            parser.error('--release-config owns the snapshot selection')
        raw = args.release_config.read_bytes()
        release_options = json.loads(raw)
        allowed = {'candidate', 'rollback', 'candidate-browser', 'storage-race', 'gateway', 'management-race',
                   'full-go', 'migration', 'frontend', 'network-blocked', 'compact-timeout-regression', 'performance', 'shutdown-addendum'}
        if (not isinstance(release_options, dict) or not release_options.get('candidate')
                or set(release_options) - allowed or any(not isinstance(value, str) or not value for value in release_options.values())):
            raise RuntimeError('Release configuration must contain only explicit candidate/evidence paths')
        release_options = {name: str((ROOT / value).resolve()) for name, value in release_options.items()}
        if any(not Path(value).is_relative_to(ROOT) for value in release_options.values()):
            raise RuntimeError('Release evidence must remain inside the project')
        record = json.loads(Path(release_options['candidate']).read_text())
        source_record = (ROOT / record['evidence_roles']['source_manifest']['path']).resolve()
        if not source_record.is_relative_to(ROOT):
            raise RuntimeError('Candidate source manifest must remain inside the project')
        args.snapshot = source_record.parent
        args.release_audit = True
        release_config_hash = hashlib.sha256(raw).hexdigest()
    source_root = (args.snapshot.resolve() / 'local-source' if args.snapshot else
                   args.source_dir.resolve() if args.source_dir else ROOT)
    if args.snapshot:
        manifest = json.loads((args.snapshot / 'source-manifest.json').read_text())
        if any(hashlib.sha256((source_root / name).read_bytes()).hexdigest() != expected
               for name, expected in manifest['files'].items()):
            raise RuntimeError('Frozen source files differ from the owner manifest')
    if args.release_audit:
        if not args.snapshot:
            parser.error('--release-audit requires an explicit --snapshot')
        candidate = json.loads(Path(release_options.get('candidate', ROOT / 'docs/MANAGEMENT-PARITY-FOLLOWUP-2026-09-18.json')).read_text())
        if candidate.get('go_source_sha256') != manifest.get('sha256'):
            raise RuntimeError('Publication audit evidence targets a different snapshot')
    folder = ROOT / '.local/gateway-acceptance' / datetime.now(timezone.utc).strftime('%Y%m%dT%H%M%S.%fZ')
    folder.mkdir(parents=True)
    before = digest(source_root)
    commands = [
        ('bridge', [sys.executable, 'integration/gateway_environment/bridge_smoke.py']),
        ('load-monitor', [sys.executable, '-m', 'unittest', 'discover', '-s', 'integration/gateway_environment', '-p', 'test_*.py', '-v']),
        ('go-environment', [sys.executable, 'integration/gateway_environment/run.py', '--source-dir', str(source_root)] + (['--race'] if args.race else [])),
        ('upgrade-unit', [sys.executable, '-m', 'unittest', 'discover', '-s', 'tools', '-p', 'test_upgrade*.py', '-v']),
        ('release-gate-unit', [sys.executable, '-m', 'unittest', 'discover', '-s', 'tools', '-p', 'test_gateway_release_check.py', '-v']),
    ]
    if args.snapshot:
        commands.append(('bridge-real-http', [sys.executable, 'integration/gateway_environment/bridge_contract.py',
                                              '--snapshot', str(args.snapshot.resolve())]))
    if args.release_audit:
        command = [sys.executable, 'tools/gateway_release_check.py']
        for name, value in release_options.items():
            command += ['--' + name, value]
        commands.append(('release-audit', command))
    checks = []
    for name, command in commands:
        with (folder / (name + '.txt')).open('w', encoding='utf-8') as output:
            result = subprocess.run(command, cwd=ROOT, stdout=output, stderr=subprocess.STDOUT,
                                    env={**os.environ, 'BRIDGE_TEST_SOURCE_ROOT': str(source_root)})
        checks.append({'check': name, 'exit_code': result.returncode})
        state = 'PASS' if result.returncode == 0 else 'BLOCKED' if name == 'release-audit' and result.returncode == 2 else 'FAIL'
        print(name + ': ' + state, flush=True)
    after = digest(source_root)
    report = {'scope': 'synthetic_only_no_production_requests', 'source_sha256_before': before,
        'source_sha256_after': after, 'source_stable': before == after, 'checks': checks,
        'source_directory': str(source_root),
        'real_go_bridge_contract_executed': bool(args.snapshot),
        'release_audit_executed': args.release_audit,
        'release_config_sha256': release_config_hash,
        'local_tests_passed': before == after and all(x['exit_code'] == 0 for x in checks if x['check'] != 'release-audit'),
        'passed': before == after and all(x['exit_code'] == 0 for x in checks),
        'real_load_executed': False, 'deployed': False}
    (folder / 'report.json').write_text(json.dumps(report, indent=2), encoding='utf-8')
    print(str(folder / 'report.json'))
    return 0 if report['passed'] else 1


if __name__ == '__main__':
    raise SystemExit(main())
