"""Verify frozen management code using existing local Linux toolchain images.

No SSH, image pull, package install, production data or published network ports.
One internal Docker network owns disposable PostgreSQL, Redis and Go containers.
"""
from __future__ import annotations

from project_layout import server_package

import argparse
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import secrets
import shlex
import subprocess
import time

import gateway_source_race as source
from management_parity_race import management_tests

ROOT = Path(__file__).resolve().parents[1]
IMAGES = {'go': 'golang:1.26.7-alpine3.23', 'cc': 'rust:1.95.0-alpine3.23',
          'postgres': 'postgres:16-alpine', 'redis': 'redis:7-alpine'}


def command(args, timeout=60, check=True, cwd=None):
    result = subprocess.run(args, cwd=cwd, capture_output=True, text=True,
                            encoding='utf-8', errors='replace', timeout=timeout)
    if check and result.returncode:
        raise RuntimeError('Local command failed: ' + args[0] + ' ' + args[1] + ': ' + result.stderr[-2000:])
    return result


def docker(*args, **kwargs):
    return command(['docker', *args], **kwargs)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--expected-go-hash', required=True)
    parser.add_argument('--source-dir', type=Path)
    args = parser.parse_args()
    source_root = (args.source_dir or ROOT).resolve()
    manifest = source.go_manifest(source_root)
    if manifest['sha256'] != args.expected_go_hash:
        raise RuntimeError('Current Go files do not match the announced freeze')
    tests = management_tests(manifest, source_root)
    server = json.loads(docker('version', '--format', '{{json .Server}}').stdout)
    if server['Os'] != 'linux':
        raise RuntimeError('A local Linux Docker daemon is required')
    context = json.loads(docker('context', 'inspect').stdout)[0]
    if not context['Endpoints']['docker']['Host'].startswith(('npipe://', 'unix://')):
        raise RuntimeError('Only a local Docker socket is allowed')
    image_ids = {key: docker('image', 'inspect', '--format', '{{.Id}}', tag).stdout.strip() for key, tag in IMAGES.items()}
    cache = Path(command(['go', 'env', 'GOMODCACHE']).stdout.strip()).resolve()
    if not cache.is_dir():
        raise RuntimeError('Existing Go module cache is required; no downloads')
    stamp = datetime.now(timezone.utc).strftime('%Y%m%dT%H%M%S').lower() + '-' + secrets.token_hex(3)
    name = 'codex-management-' + stamp
    output = ROOT / '.local/management-parity' / ('local-race-' + stamp)
    output.mkdir(parents=True)
    source.snapshot(output, manifest, source_root)
    # The archive has already verified every member against the current manifest.
    unpacked = output / 'source'
    unpacked.mkdir()
    for relative, digest in manifest['files'].items():
        raw = (source_root / relative).read_bytes()
        if hashlib.sha256(raw).hexdigest() != digest:
            raise RuntimeError('Source changed while copying frozen files')
        target = unpacked / relative
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_bytes(raw)
    build = output / 'compiler'
    build.mkdir()
    (build / 'Dockerfile').write_text(
        'FROM ' + IMAGES['go'] + ' AS compiler\nFROM ' + IMAGES['cc'] +
        '\nCOPY --from=compiler /usr/local/go /usr/local/go\nENV PATH="/usr/local/go/bin:${PATH}"\n', encoding='ascii')
    password = secrets.token_urlsafe(32)
    private = [password]
    scrub = lambda value: source.scrub(value, private)
    network, pg, redis, test = name + '-net', name + '-pg', name + '-redis', name + '-test'
    compiler = name + ':compiler'
    owned_containers = []
    created_network = False
    built_image = False
    env_path = output / 'test.env'
    report = {'schema': 'codex-management-local-race-v1', 'status': 'running',
              'started_at': datetime.now(timezone.utc).isoformat(), 'go_source_sha256': manifest['sha256'],
              'selected_top_level_tests': tests, 'existing_image_ids': image_ids,
              'scope': 'local ephemeral Linux PostgreSQL/Redis; internal network; no published ports or production data',
              'local_socket': True, 'image_pull': False, 'package_install': False,
              'resources': {'cpus': 2, 'memory': '2g', 'pids': 192}}
    began = time.monotonic()
    try:
        built = docker('build', '--pull=false', '--network=none', '-t', compiler, str(build), timeout=300)
        built_image = True
        for key in ['go', 'cc']:
            if docker('image', 'inspect', '--format', '{{.Id}}', IMAGES[key]).stdout.strip() != image_ids[key]:
                raise RuntimeError('Existing compiler base changed during build')
        (output / 'compiler-build.log').write_text(scrub(built.stdout + built.stderr), encoding='utf-8')
        report['compiler_image_id'] = docker('image', 'inspect', '--format', '{{.Id}}', compiler).stdout.strip()
        docker('network', 'create', '--internal', '--label', 'codex-agent.synthetic-management=true', network)
        created_network = True
        pg_env = os.environ.copy()
        pg_env.update(POSTGRES_USER='codex_agent_test', POSTGRES_DB='codex_agent_integration', POSTGRES_PASSWORD=password)
        # No password is placed in shell arguments, logs, source snapshots or reports.
        owned_containers.append(pg)
        result = subprocess.run(['docker', 'run', '-d', '--pull=never', '--name', pg, '--network', network,
            '--label', 'codex-agent.synthetic-management=true', '--memory', '512m', '--cpus', '1',
            '--tmpfs', '/var/lib/postgresql/data:rw,size=512m', '-e', 'POSTGRES_USER', '-e', 'POSTGRES_DB', '-e', 'POSTGRES_PASSWORD', image_ids['postgres']],
            env=pg_env, capture_output=True, text=True, timeout=60)
        if result.returncode:
            raise RuntimeError('Disposable PostgreSQL start failed: ' + scrub(result.stderr))
        owned_containers.append(redis)
        docker('run', '-d', '--pull=never', '--name', redis, '--network', network,
               '--label', 'codex-agent.synthetic-management=true', '--memory', '128m', '--cpus', '0.5',
               image_ids['redis'], 'redis-server', '--save', '', '--appendonly', 'no')
        deadline = time.monotonic() + 60
        while docker('exec', pg, 'pg_isready', '-U', 'codex_agent_test', '-d', 'codex_agent_integration', check=False).returncode:
            if time.monotonic() > deadline:
                raise TimeoutError('Disposable PostgreSQL did not become ready')
            time.sleep(1)
        report['postgres_version'] = docker('exec', pg, 'postgres', '--version').stdout.strip()
        report['redis_version'] = docker('exec', redis, 'redis-server', '--version').stdout.strip()
        prefix = 'codex-agent:integration:' + stamp + ':'
        environment = {'TEST_DATABASE_URL': f'postgres://codex_agent_test:{password}@{pg}:5432/codex_agent_integration?sslmode=disable',
                       'TEST_REDIS_URL': f'redis://{redis}:6379/0', 'TEST_REDIS_PREFIX': prefix,
                       'REDIS_KEY_PREFIX': prefix, 'REDIS_PREFIX': prefix, 'REDIS_NAMESPACE': prefix,
                       'GOMAXPROCS': '2', 'CGO_ENABLED': '1', 'GOFLAGS': '-mod=readonly',
                       'GOCACHE': '/tmp/go-build', 'GOMODCACHE': '/go/pkg/mod', 'GOTOOLCHAIN': 'local',
                       'GOPROXY': 'off', 'GOSUMDB': 'off', 'GOENV': 'off'}
        env_path.write_text(''.join(key + '=' + value + '\n' for key, value in environment.items()), encoding='utf-8')
        regex = '^(' + '|'.join(tests) + ')$'
        script = '\n'.join(['set -eu', 'mkdir -p /work', 'cp -a /src/. /work/', 'cd /work',
            'go version', 'gcc --version | head -n 1', 'go env GOOS GOARCH CGO_ENABLED',
            shlex.join(['go', 'test', '-race', '-count=1', '-timeout=8m', '-json', '-run', regex, server_package(source_root)])])
        owned_containers.append(test)
        docker('run', '-d', '--pull=never', '--name', test, '--network', network,
               '--label', 'codex-agent.synthetic-management=true', '--cpus', '2', '--memory', '2g', '--pids-limit', '192',
               '--cap-drop', 'ALL', '--security-opt', 'no-new-privileges', '--env-file', str(env_path),
               '--mount', 'type=bind,src=' + str(unpacked.resolve()) + ',dst=/src,readonly',
               '--mount', 'type=bind,src=' + str(cache) + ',dst=/go/pkg/mod,readonly', compiler, 'sh', '-c', script)
        print(json.dumps({'started': True, 'source_frozen': manifest['sha256'], 'selected_tests': len(tests),
                          'output': str(output)}), flush=True)
        deadline = time.monotonic() + 900
        while True:
            state = json.loads(docker('inspect', '--format', '{{json .State}}', test).stdout)
            if not state['Running']:
                break
            if time.monotonic() > deadline:
                raise TimeoutError('Local Linux race exceeded 900 seconds')
            time.sleep(5)
        logs = docker('logs', test)
        raw = logs.stdout + logs.stderr
        (output / 'race.log').write_text(scrub(raw), encoding='utf-8')
        outcomes = {'pass': [], 'fail': [], 'skip': []}
        packages = []
        for line in raw.splitlines():
            try:
                event = json.loads(line)
            except ValueError:
                continue
            if not isinstance(event, dict):
                continue
            action, test_name = event.get('Action'), event.get('Test')
            if test_name and action in outcomes:
                outcomes[action].append(test_name)
            elif not test_name and action == 'pass':
                packages.append(event.get('Package'))
        missing = sorted(set(tests) - set(outcomes['pass']) - set(outcomes['fail']) - set(outcomes['skip']))
        passed = not state['ExitCode'] and not outcomes['fail'] and not outcomes['skip'] and not missing and packages
        report.update(status='pass' if passed else 'fail', exit_code=state['ExitCode'], oom_killed=state.get('OOMKilled', False),
                      passed_top_level_tests=sum('/' not in name for name in outcomes['pass']), passed_test_events=len(outcomes['pass']),
                      failed_tests=outcomes['fail'], skipped_tests=outcomes['skip'], missing_tests=missing, passed_packages=packages)
    except Exception as error:
        report.update(status='fail', error=scrub(type(error).__name__ + ': ' + str(error)))
    finally:
        failed_cleanup = []
        for container in reversed(owned_containers):
            if docker('rm', '-f', container, check=False).returncode:
                failed_cleanup.append(container)
        if created_network and docker('network', 'rm', network, check=False).returncode:
            failed_cleanup.append(network)
        if built_image and docker('image', 'rm', compiler, check=False).returncode:
            failed_cleanup.append(compiler)
        if env_path.exists():
            env_path.unlink()
        report.update(temporary_credentials_removed=not env_path.exists(), cleanup_failures=failed_cleanup,
                      temporary_container_removed=not failed_cleanup, temporary_network_removed=not failed_cleanup)
        if failed_cleanup:
            report['status'] = 'fail'
    report['go_source_changed_after_freeze'] = source.go_manifest(source_root)['sha256'] != manifest['sha256']
    report['source_directory'] = str(source_root)
    report['duration_seconds'] = round(time.monotonic() - began, 3)
    report['completed_at'] = datetime.now(timezone.utc).isoformat()
    (output / 'report.json').write_text(json.dumps(report, indent=2) + '\n', encoding='utf-8')
    print(json.dumps({'status': report['status'], 'passed_top_level_tests': report.get('passed_top_level_tests', 0),
                      'source_changed': report['go_source_changed_after_freeze'], 'report': str(output / 'report.json')}), flush=True)
    return 0 if report['status'] == 'pass' else 1


if __name__ == '__main__':
    raise SystemExit(main())
