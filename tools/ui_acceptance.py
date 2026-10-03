"""Run fresh UI acceptance against synthetic PG/Redis data; never deploy or migrate."""
import argparse
import datetime as dt
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import time

import ui_release as release
from integration_env import connect, run

ROOT = release.ROOT


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--output', type=Path)
    args = parser.parse_args()
    output = (args.output or ROOT / '.local/ui/runs' / dt.datetime.now(dt.timezone.utc).strftime('%Y%m%dT%H%M%SZ')).resolve()
    output.relative_to(ROOT)
    output.mkdir(parents=True, exist_ok=False)
    report = {'schema': 'codex-agent-ui-acceptance-v1', 'status': 'fail', 'checks': [],
              'started_at': dt.datetime.now(dt.timezone.utc).isoformat(), 'commands': []}
    report['harness'] = {name: release.remote.sha(ROOT / name) for name in
                         ('tools/ui_acceptance.py', 'integration/tests/browser-real.cjs')}
    start_frontend, start_backend = release.frontend_source(), release.backend_source()

    def artifact(path):
        return {'path': path.relative_to(ROOT).as_posix(), 'sha256': release.remote.sha(path)}

    def execute(name, argv, env=None):
        started = time.monotonic()
        log = output / (name + '.log')
        with log.open('wb') as handle:
            result = subprocess.run(argv, cwd=ROOT, env=env, stdout=handle, stderr=subprocess.STDOUT, timeout=900)
        report['commands'].append({'name': name, 'argv': argv, 'exit_code': result.returncode,
                                   'seconds': round(time.monotonic() - started, 3), 'log': artifact(log)})
        print(name + ': ' + ('PASS' if result.returncode == 0 else 'FAIL'), flush=True)
        if result.returncode:
            raise RuntimeError(name + ' failed; inspect ' + str(log))
        if release.frontend_source() != start_frontend or release.backend_source() != start_backend:
            raise RuntimeError('Source changed during acceptance')
        return log

    try:
        release.baseline_report(None)
        npm = shutil.which('npm.cmd' if os.name == 'nt' else 'npm')
        if not npm:
            raise RuntimeError('npm is unavailable')
        unit_log = execute('unit', [npm, '--prefix', 'frontend', 'test'])
        browser_log = execute('browser', [npm, '--prefix', 'frontend', 'run', 'test:browser'])
        binding = release.fingerprint()
        report.update(binding)
        for directory in ('browser', 'charts-browser'):
            shutil.copytree(ROOT / 'frontend/test-results' / directory, output / directory)
        browser = json.loads((output / 'browser/results.json').read_text(encoding='utf-8'))
        charts = json.loads((output / 'charts-browser/results.json').read_text(encoding='utf-8'))
        if browser.get('passed') is not True or charts.get('status') != 'passed':
            raise RuntimeError('Browser result files do not confirm passing tests')
        client = connect()
        try:
            binary = run(client, 'docker exec codex-agent-integration-performance sha256sum /proc/1/exe').split()[0]
            network = run(client, "docker inspect --format '{{.HostConfig.NetworkMode}}' codex-agent-integration-performance").strip()
            arguments = run(client, 'docker exec codex-agent-integration-performance cat /proc/1/cmdline').split('\x00')
        finally:
            client.close()
        if binary != binding['backend_binary_sha256'] or network != 'host' or '--listen' not in arguments or arguments[arguments.index('--listen')+1] != '127.0.0.1:28880':
            raise RuntimeError('Synthetic backend identity or endpoint does not match the deployed PG binary')
        backend_file = output / 'backend-identity.json'
        backend_file.write_text(json.dumps({'synthetic_data': True, 'container': 'codex-agent-integration-performance',
            'binary_sha256': binary, 'network': network, 'listen': '127.0.0.1:28880',
            'tunnel': 'http://127.0.0.1:28882'}, indent=2), encoding='utf-8')
        env = dict(os.environ, INTEGRATION_UI_STYLE='enterprise', INTEGRATION_UI_OUTPUT=str(output / 'real'))
        real_log = execute('real-api', ['node', 'integration/tests/browser-real.cjs'], env)
        real = json.loads((output / 'real/report.json').read_text(encoding='utf-8'))
        if real.get('status') != 'pass' or release.fingerprint() != binding:
            raise RuntimeError('Real API failed or accepted artifact changed')
        for name, paths in (
            ('build', [browser_log]),
            ('unit', [unit_log]),
            ('browser', [browser_log, output / 'browser/results.json', output / 'charts-browser/results.json']),
            ('real_api', [real_log, backend_file, output / 'real/report.json', *sorted((output / 'real').glob('*.png'))]),
        ):
            report['checks'].append({'id': name, 'status': 'pass', **binding,
                **({'synthetic_data': True} if name == 'real_api' else {}),
                'artifacts': [artifact(p) for p in paths]})
        report['status'] = 'pass'
        report['summary'] = {'browser_groups': len(browser['assertions']), 'chart_groups': len(charts['assertions']),
                             'real_api_groups': len(real['checks'])}
    except Exception as error:
        report['error'] = str(error)
    report['completed_at'] = dt.datetime.now(dt.timezone.utc).isoformat()
    (output / 'report.json').write_text(json.dumps(report, indent=2) + '\n', encoding='utf-8')
    print(json.dumps({'status': report['status'], 'report': str(output / 'report.json'), 'error': report.get('error')}))
    return 0 if report['status'] == 'pass' else 1


if __name__ == '__main__':
    sys.exit(main())
