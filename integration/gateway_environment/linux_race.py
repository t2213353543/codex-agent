"""Run only five synthetic gateway tests on a frozen, network-isolated Linux snapshot.

Uses the project's SSH helper and an existing integration compiler image. No
production account, database, service, host package, or live upstream is used.
"""
from __future__ import annotations

import argparse
import datetime as dt
import hashlib
import io
import json
import os
from pathlib import Path
import re
import shlex
import subprocess
import sys
import tarfile
import time

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / 'tools'))
from integration_env import REMOTE, connect, run, write_remote
from project_layout import backend_source_paths, server_source_dir, server_package, package_overlay

EXPECTED = {
    'TestGatewayEnvironmentSlowBoundAndReuse',
    'TestGatewayEnvironmentFailuresNoRetry',
    'TestGatewayEnvironmentCancellationReleasesConnections',
    'TestGatewayEnvironmentAccountRouteIsolation',
    'TestGatewayEnvironmentPoolCapacityAndDefaults',
}


def digest(raw):
    return hashlib.sha256(raw).hexdigest()


def watched_files():
    return sorted(set(backend_source_paths(ROOT)) |
        {ROOT / 'tools/project_layout.py', Path(__file__).with_name('environment_test.go.txt')})


def source_hashes():
    return {p.relative_to(ROOT).as_posix(): digest(p.read_bytes()) for p in watched_files()}


def freeze(output):
    snapshot = output / 'source'
    snapshot.mkdir()
    hashes = {}
    for path in watched_files():
        name = path.relative_to(ROOT).as_posix()
        raw = path.read_bytes()
        hashes[name] = digest(raw)
        target = snapshot / name
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_bytes(raw)
    if source_hashes() != hashes:
        raise RuntimeError('Source changed while freezing; rerun against a coherent snapshot')
    snapshot_sha = digest(json.dumps(hashes, sort_keys=True, separators=(',', ':')).encode())
    manifest = {'schema': 1, 'snapshot_sha256': snapshot_sha, 'files': hashes,
        'runner_sha256': digest(Path(__file__).read_bytes()), 'synthetic_only': True}
    environment = os.environ.copy()
    environment.update(GOPROXY='off', GOSUMDB='off', GOWORK='off', GOFLAGS='-mod=readonly')
    # Vendor only from this workstation's existing module cache. Frozen go.mod/
    # go.sum and root Go sources are not changed, and no network is permitted.
    vendor = subprocess.run(['go', 'mod', 'vendor'], cwd=snapshot, env=environment,
        stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True, timeout=180)
    (output / 'vendor.log').write_text(vendor.stdout, encoding='utf-8')
    if vendor.returncode:
        raise RuntimeError('Offline dependency snapshot failed; inspect vendor.log')
    for name, expected in hashes.items():
        if digest((snapshot / name).read_bytes()) != expected:
            raise RuntimeError('Dependency preparation changed frozen input: ' + name)
    actual = package_overlay(Path(__file__).with_name('environment_test.go.txt'), snapshot / 'environment-package-test.go.txt', snapshot)
    target = server_source_dir(snapshot) / 'zz_gateway_environment_overlay_test.go'
    overlay = {'Replace': {'/src/' + target.relative_to(snapshot).as_posix(): '/src/' + actual.name}}
    if target.exists():
        raise RuntimeError('Refusing to hide an existing Go source under the overlay')
    (snapshot / 'overlay.json').write_text(json.dumps(overlay), encoding='utf-8')
    archive = output / 'source.tar.gz'
    included = {}
    with tarfile.open(archive, 'w:gz') as tar:
        for path in sorted(snapshot.rglob('*')):
            if not path.is_file():
                continue
            name, raw = path.relative_to(snapshot).as_posix(), path.read_bytes()
            info = tarfile.TarInfo(name)
            info.size, info.mode, info.mtime = len(raw), 0o644, 0
            tar.addfile(info, io.BytesIO(raw))
            included[name] = digest(raw)
    manifest.update(archive_sha256=digest(archive.read_bytes()),
        archive_bytes=archive.stat().st_size, uploaded_files=included,
        offline_dependencies=True, source_changed_after_freeze=source_hashes() != hashes)
    (output / 'source-manifest.json').write_text(json.dumps(manifest, indent=2) + '\n', encoding='utf-8')
    return archive, manifest


def parse_log(raw):
    tests, metrics, started_tests = {}, [], []
    marker = 'GATEWAY_ENVIRONMENT_METRIC '
    for line in raw.splitlines():
        try:
            event = json.loads(line)
        except ValueError:
            continue
        if not isinstance(event, dict):
            continue
        name, action = event.get('Test'), event.get('Action')
        if name and action == 'run':
            started_tests.append(name)
        if name and action in ('pass', 'fail', 'skip'):
            tests[name] = action
        if marker in event.get('Output', ''):
            metrics.append(json.loads(event['Output'].split(marker, 1)[1]))
    return {'tests': tests, 'metrics': metrics, 'started_tests': started_tests,
        'race_executed': bool(started_tests), 'data_race_reported': 'WARNING: DATA RACE' in raw}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--image', default='codex-agent-integration-race:ce864e7223ac17b1-v1')
    parser.add_argument('--timeout', type=int, default=900)
    parser.add_argument('--prepare-only', action='store_true')
    args = parser.parse_args()
    if not 60 <= args.timeout <= 1800:
        parser.error('--timeout must be between 60 and 1800 seconds')
    stamp = dt.datetime.now(dt.timezone.utc).strftime('%Y%m%dT%H%M%S.%fZ')
    output = ROOT / '.local' / 'gateway-environment-linux-race' / stamp
    output.mkdir(parents=True, exist_ok=False)
    report = {'schema': 1, 'started_at': stamp, 'status': 'fail', 'synthetic_only': True,
        'production_requests': 0, 'race_requested': True, 'race_executed': False,
        'resources': {'cpus': 2, 'memory': '2g', 'pids': 128, 'network': 'none'},
        'scope': 'Five local httptest gateway cases; not production load or deployment',
        'ssh_proxy_opt_in': bool(os.environ.get('CODEX_AGENT_SSH_PROXY'))}
    client, container_id, manifest = None, '', None
    started = time.monotonic()
    try:
        archive, manifest = freeze(output)
        report.update(snapshot_sha256=manifest['snapshot_sha256'], archive_sha256=manifest['archive_sha256'],
            offline_dependencies=True)
        if args.prepare_only:
            report['status'] = 'prepared'
        else:
            client = connect()
            run(client, 'test -f ' + shlex.quote(REMOTE + '/.codex-agent-integration'))
            image = run(client, "docker image inspect --format '{{.Id}}' " + shlex.quote(args.image)).strip()
            if not re.fullmatch(r'sha256:[0-9a-f]{64}', image):
                raise RuntimeError('Existing race compiler image did not resolve to an immutable ID')
            report['image_id'] = image
            remote = REMOTE + '/gateway-environment-race/' + manifest['snapshot_sha256'] + '/' + stamp
            report['remote_directory'] = remote
            run(client, 'install -d -m 700 ' + shlex.quote(remote + '/source'))
            with client.open_sftp() as sftp:
                sftp.put(str(archive), remote + '/source.tar.gz')
                sftp.chmod(remote + '/source.tar.gz', 0o600)
            uploaded = run(client, 'sha256sum ' + shlex.quote(remote + '/source.tar.gz')).split()[0]
            if uploaded != manifest['archive_sha256']:
                raise RuntimeError('Uploaded archive SHA256 mismatch')
            write_remote(client, remote + '/source-manifest.json', (output / 'source-manifest.json').read_text())
            run(client, 'tar -xzf ' + shlex.quote(remote + '/source.tar.gz') + ' -C ' + shlex.quote(remote + '/source'))
            script = '\n'.join(['set -eu', 'cd /src', 'go version', 'gcc --version',
                'go env GOOS GOARCH CGO_ENABLED CC',
                'go test -mod=vendor -overlay=/src/overlay.json -race -json -count=1 -timeout=90s -run "^TestGatewayEnvironment" ' + server_package(ROOT)])
            command = ['docker', 'run', '-d', '--name', 'codex-agent-env-race-' + stamp.lower(),
                '--label', 'codex-agent.integration=gateway-environment-race', '--cpus', '2', '--memory', '2g',
                '--pids-limit', '128', '--network', 'none', '--cap-drop', 'ALL', '--security-opt', 'no-new-privileges',
                '-e', 'CGO_ENABLED=1', '-e', 'GOMAXPROCS=2', '-e', 'GOPROXY=off', '-e', 'GOSUMDB=off',
                '-e', 'GOWORK=off', '-e', 'GOCACHE=/tmp/go-build', '-e', 'GOMODCACHE=/tmp/go-mod',
                '-v', remote + '/source:/src:ro', image, 'sh', '-c', script]
            report['container_command'] = command
            container_id = run(client, shlex.join(command)).strip()
            if not re.fullmatch(r'[0-9a-f]{64}', container_id):
                raise RuntimeError('Invalid temporary container identifier')
            report['container_id'] = container_id
            print('Linux synthetic race started; snapshot=' + manifest['snapshot_sha256'] + '; container=' + container_id[:12], flush=True)
            deadline = time.monotonic() + args.timeout
            while True:
                state = json.loads(run(client, "docker inspect --format '{{json .State}}' " + container_id))
                if not state['Running']:
                    break
                if time.monotonic() >= deadline:
                    run(client, 'docker stop --time 10 ' + container_id)
                    raise TimeoutError('Isolated race container exceeded the configured deadline')
                time.sleep(5)
            raw = run(client, 'docker logs ' + container_id + ' 2>&1', timeout=60)
            (output / 'go-test.jsonl').write_text(raw, encoding='utf-8')
            report.update(parse_log(raw), exit_code=state['ExitCode'], oom_killed=state.get('OOMKilled', False))
            report['status'] = 'pass' if state['ExitCode'] == 0 and set(report['tests']) == EXPECTED and all(
                value == 'pass' for value in report['tests'].values()) and not report['data_race_reported'] else 'fail'
    except Exception as exc:
        report.update(status='fail', error_type=type(exc).__name__, error=str(exc))
        if client and re.fullmatch(r'[0-9a-f]{64}', container_id):
            try:
                raw = run(client, 'docker logs ' + container_id + ' 2>&1', timeout=60)
                (output / 'go-test.jsonl').write_text(raw, encoding='utf-8')
                report.update(parse_log(raw))
            except Exception:
                pass
    finally:
        if client and re.fullmatch(r'[0-9a-f]{64}', container_id):
            try:
                run(client, 'docker rm -f ' + container_id)
                report['temporary_container_removed'] = True
            except Exception as exc:
                report.update(status='fail', cleanup_error=str(exc))
        if client:
            client.close()
    if manifest:
        current = source_hashes()
        report['source_changed_after_freeze'] = current != manifest['files']
        report['changed_local_files'] = sorted(name for name in set(current) | set(manifest['files'])
            if current.get(name) != manifest['files'].get(name))
    report.update(duration_seconds=round(time.monotonic() - started, 3),
        completed_at=dt.datetime.now(dt.timezone.utc).isoformat())
    (output / 'report.json').write_text(json.dumps(report, ensure_ascii=False, indent=2) + '\n', encoding='utf-8')
    print(json.dumps({'status': report['status'], 'race_executed': report['race_executed'],
        'report': str(output / 'report.json'), 'snapshot_sha256': report.get('snapshot_sha256'),
        'error': report.get('error')}, ensure_ascii=False), flush=True)
    return 0 if report['status'] in ('pass', 'prepared') else 1


if __name__ == '__main__':
    raise SystemExit(main())
