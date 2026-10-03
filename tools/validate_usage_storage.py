"""Run the two usage/timing integration tests against isolated synthetic PG.

Uses the existing .local/integration environment and a temporary SSH loopback
tunnel. Source is frozen locally; production DBs/services are never modified.
"""
import datetime as dt
import hashlib
import json
import os
from pathlib import Path
import select
import socketserver
import subprocess
import threading
from urllib.parse import urlparse, urlunparse

from integration_env import ROOT, REMOTE, connect, run
from project_layout import backend_source_paths


def main():
    stamp = dt.datetime.now(dt.timezone.utc).strftime('%Y%m%dT%H%M%S.%fZ')
    output = ROOT / '.local/reliability-modes/usage-pg' / stamp
    source = output / 'source'
    source.mkdir(parents=True)
    report = dict(status='fail', synthetic_only=True, database='codex_agent_integration',
                  database_user='codex_agent_test', remote_port=25432, started_at=stamp)
    client = server = None
    secrets = []
    try:
        config = json.loads((ROOT / '.local/integration/environment.json').read_text(encoding='utf-8-sig'))
        assert config['environment'] == 'isolated-synthetic-only' and config['remote_root'] == REMOTE
        dsn = config['TEST_DATABASE_URL']
        parsed = urlparse(dsn)
        assert parsed.hostname == '127.0.0.1' and parsed.port == 25432
        assert parsed.username == 'codex_agent_test' and parsed.path == '/codex_agent_integration'
        secrets = [dsn, parsed.password]
        hashes = {}
        for path in backend_source_paths(ROOT):
            raw = path.read_bytes()
            relative = path.relative_to(ROOT)
            hashes[relative.as_posix()] = hashlib.sha256(raw).hexdigest()
            target = source / relative
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_bytes(raw)
        report['source_sha256'] = hashlib.sha256(json.dumps(hashes, sort_keys=True).encode()).hexdigest()
        (output / 'source-manifest.json').write_text(json.dumps(hashes, indent=2), encoding='utf-8')
        client = connect()
        marker = run(client, 'cat ' + REMOTE + '/.codex-agent-integration').strip()
        assert marker == 'codex-agent-integration'
        inspection = json.loads(run(client, "docker inspect --format '{{json .}}' codex-agent-integration-postgres"))
        assert inspection['State']['Running'] is True
        ports = inspection['NetworkSettings']['Ports']['5432/tcp']
        assert ports == [{'HostIp': '127.0.0.1', 'HostPort': '25432'}]
        report['container_id'] = inspection['Id']
        report['container_loopback_verified'] = True
        transport = client.get_transport()

        class Handler(socketserver.BaseRequestHandler):
            def handle(self):
                channel = transport.open_channel('direct-tcpip', ('127.0.0.1', 25432), self.request.getpeername())
                try:
                    while True:
                        ready, _, _ = select.select([self.request, channel], [], [], 10)
                        if self.request in ready:
                            value = self.request.recv(65536)
                            if not value:
                                break
                            channel.sendall(value)
                        if channel in ready:
                            value = channel.recv(65536)
                            if not value:
                                break
                            self.request.sendall(value)
                finally:
                    channel.close()

        class Server(socketserver.ThreadingTCPServer):
            daemon_threads = True

        server = Server(('127.0.0.1', 0), Handler)
        threading.Thread(target=server.serve_forever, daemon=True).start()
        local_dsn = urlunparse(parsed._replace(netloc=parsed.netloc.rsplit(':', 1)[0] + ':' + str(server.server_address[1])))
        secrets.append(local_dsn)
        env = {k:v for k,v in os.environ.items() if not k.startswith(('TEST_', 'BPS_', 'CODEX_', 'OPENAI_')) and k not in ('DATABASE_URL', 'REDIS_URL')}
        env.update(TEST_DATABASE_URL=local_dsn, GOWORK='off', GOPROXY='off', GOSUMDB='off', GOTOOLCHAIN='local')
        pattern = '^TestPGManaged(AccountWindowCosts|TimingCoverage)$'
        result = subprocess.run(['go', 'test', './internal/server', '-run', pattern, '-count=1', '-json', '-timeout=3m'],
                                cwd=source, env=env, capture_output=True, text=True, encoding='utf-8', timeout=210)
        raw = result.stdout + result.stderr
        for secret in secrets:
            if secret:
                raw = raw.replace(secret, '[redacted]')
        (output / 'test.jsonl').write_text(raw, encoding='utf-8')
        outcomes = {}
        for line in result.stdout.splitlines():
            row = json.loads(line)
            if row.get('Test') and row['Action'] in ('pass', 'fail', 'skip'):
                outcomes[row['Test']] = row['Action']
        expected = ['TestPGManagedAccountWindowCosts', 'TestPGManagedTimingCoverage']
        report.update(exit_code=result.returncode, test_outcomes=outcomes,
                      status='pass' if result.returncode == 0 and all(outcomes.get(t) == 'pass' for t in expected) else 'fail')
        relevant = [p for p in hashes if p.startswith('migrations/') or Path(p).name.startswith(('stats', 'admin_account_window', 'management_timing'))]
        report['relevant_source_changes_after_freeze'] = [p for p in relevant if hashlib.sha256((ROOT/p).read_bytes()).hexdigest() != hashes[p]]
    except Exception as error:
        text = str(error)
        for secret in secrets:
            if secret:
                text = text.replace(secret, '[redacted]')
        report['error'] = text
    finally:
        if server:
            server.shutdown()
            server.server_close()
            report['temporary_tunnel_closed'] = True
        if client:
            client.close()
        (output / 'report.json').write_text(json.dumps(report, ensure_ascii=False, indent=2) + '\n', encoding='utf-8')
        print(json.dumps(dict(report=str(output/'report.json'), **report), ensure_ascii=False))
    return 0 if report['status'] == 'pass' else 1


if __name__ == '__main__':
    raise SystemExit(main())
