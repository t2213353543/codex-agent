"""Build and verify the exact Linen/Orbit UI candidate, then seal release evidence."""
import concurrent.futures
import datetime
import json
import os
from pathlib import Path
import shutil
import subprocess
import time

import ui_release as ui

ROOT = ui.ROOT
OUT = Path(os.environ.get('CODEX_UI_OUTPUT', str(ROOT / '.local/console-ui-20260924'))).resolve()


def command(name, args, cwd=ROOT, environment=None):
    before = ui.frontend_source()['sha256']
    started = time.monotonic()
    result = subprocess.run(args, cwd=cwd, env=environment or os.environ.copy(), capture_output=True,
                            text=True, encoding='utf-8', errors='replace',
                            creationflags=getattr(subprocess, 'CREATE_NO_WINDOW', 0))
    (OUT / (name + '.log')).write_text(result.stdout + result.stderr, encoding='utf-8')
    after = ui.frontend_source()['sha256']
    facts = {'status': 'pass' if result.returncode == 0 and before == after else 'fail',
             'exit_code': result.returncode, 'source_before': before, 'source_after': after,
             'duration_seconds': round(time.monotonic() - started, 2)}
    (OUT / (name + '-command.json')).write_text(json.dumps(facts, indent=2), encoding='utf-8')
    print(name + ': ' + facts['status'], flush=True)
    if facts['status'] != 'pass':
        raise RuntimeError(name + ' failed; see ' + str(OUT / (name + '.log')))
    return facts


def artifact(path):
    path = Path(path)
    return {'path': path.relative_to(ROOT).as_posix(), 'sha256': ui.remote.sha(path)}


def main():
    OUT.mkdir(parents=True, exist_ok=True)
    ui.baseline_report(None)
    source_before = ui.frontend_source()
    command('build', ['node', 'node_modules/vite/bin/vite.js', 'build'], ROOT / 'frontend')
    command('unit', ['node', '--test', *[str(p.relative_to(ROOT / 'frontend')) for p in sorted((ROOT / 'frontend/tests').glob('*.test.js'))]], ROOT / 'frontend')
    dist = OUT / 'dist'
    if dist.exists():
        previous = OUT / ('dist-previous-' + datetime.datetime.now(datetime.timezone.utc).strftime('%H%M%S'))
        dist.rename(previous)
    shutil.copytree(ROOT / 'frontend/dist', dist)
    files = ui.dist_map(dist)
    (OUT / 'build-manifest.json').write_text(json.dumps({'files': files}, indent=2), encoding='utf-8')
    binding = ui.fingerprint()
    (OUT / 'verified-fingerprint.json').write_text(json.dumps(binding, indent=2), encoding='utf-8')
    environment = {**os.environ, 'CODEX_UI_OUTPUT': str(OUT), 'MANAGEMENT_UI_DIST': str(dist),
                   'ACCOUNTS_NO_BILLING_REPORT_DIR': str(OUT / 'accounts'),
                   'MANAGEMENT_ACCOUNT_REPORT_DIR': str(OUT / 'account-runtime'),
                   'CHARTS_TEST_OUTPUT': str(OUT / 'dashboard'),
                   'RESOURCE_BROWSER_OUTPUT': str(OUT / 'resources')}
    suites = {
        'accounts': ['node', 'frontend/tests/management-accounts-no-billing-browser.mjs'],
        'account-runtime': ['node', 'frontend/tests/management-accounts-runtime-browser.mjs'],
        'logs': ['node', 'frontend/tests/run-log-management.mjs', '--dist', str(dist), '--output', str(OUT / 'logs')],
        'resources': ['node', 'frontend/tests/resource-management-browser.mjs'],
        'settings': ['node', 'frontend/tests/settings-browser.mjs'],
        'console-browser': ['node', 'frontend/tests/console-browser.mjs'],
        'dashboard': ['node', 'frontend/tests/charts-browser.mjs'],
        'workflows': ['node', 'frontend/tests/workflows-browser.mjs'],
        'controls': ['node', 'frontend/tests/controls-browser.mjs'],
    }
    with concurrent.futures.ThreadPoolExecutor(max_workers=3) as pool:
        pending = {name: pool.submit(command, name, args, environment=environment) for name, args in suites.items()}
        results = {name: future.result() for name, future in pending.items()}
    assert ui.frontend_source() == source_before, 'Source changed during acceptance'
    assert ui.dist_map() == files == ui.dist_map(dist), 'Candidate changed during acceptance'
    command('release-tool-tests', ['python', 'tools/test_console_ui_release.py'])
    command('live-preflight', ['python', 'tools/console_ui_live_check.py', '--output', str(OUT / 'live-preflight.json')])
    live = json.loads((OUT / 'live-preflight.json').read_text(encoding='utf-8'))
    assert live['passed'] is True and live['browser']['passed'] is True
    assert all(live[key] == value for key, value in binding.items())
    reports = [OUT / 'accounts/report.json', OUT / 'logs/report.json', OUT / 'resources/report.json',
               ROOT / 'frontend/test-results/settings/results.json', OUT / 'console-browser/report.json', OUT / 'dashboard/results.json',
               OUT / 'account-runtime/report.json', ROOT / '.local/workflows-ui-20260924/report.json',
               ROOT / '.local/admin-ui-improvements/after/results.json']
    for report_path in reports:
        value = json.loads(report_path.read_text(encoding='utf-8'))
        assert value.get('passed') is True or value.get('status') in ('pass', 'passed'), 'Nonpassing browser report'
    checks = [
        {'id': 'build', 'status': 'pass', **binding, 'artifacts': [artifact(OUT / 'build.log'), artifact(OUT / 'build-command.json')]},
        {'id': 'unit', 'status': 'pass', **binding, 'artifacts': [artifact(OUT / 'unit.log'), artifact(OUT / 'unit-command.json'), artifact(OUT / 'release-tool-tests.log')]},
        {'id': 'browser', 'status': 'pass', **binding, 'artifacts': [artifact(p) for p in reports] + [artifact(OUT / (name + '-command.json')) for name in suites]},
        {'id': 'real_api', 'status': 'pass', **binding, **{key: live[key] for key in ['synthetic_data', 'data_mode', 'business_writes', 'gateway_requests', 'session_only_mutations', 'allowed_post_paths', 'ephemeral_snapshot_writes']}, 'artifacts': [artifact(OUT / 'live-preflight.json')]},
    ]
    assert ui.fingerprint() == binding
    report = {'schema': 'codex-agent-ui-acceptance-v1', 'status': 'pass', **binding, 'checks': checks,
              'completed_at': datetime.datetime.now(datetime.timezone.utc).isoformat()}
    (OUT / 'acceptance.json').write_text(json.dumps(report, indent=2), encoding='utf-8')
    print(json.dumps({'passed': True, 'evidence': str(OUT / 'acceptance.json'), 'suites': list(results)}), flush=True)


if __name__ == '__main__':
    main()
