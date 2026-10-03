"""Isolated Codex Agent integration dependencies on the current main host.

No production database, Redis, application service, or firewall is modified.
"""
import argparse
import base64
import importlib.util
import json
import os
from pathlib import Path
import secrets
import shlex
import socketserver
import threading
import select
import socket
import subprocess
import time
from urllib.parse import urlparse

ROOT = Path(__file__).resolve().parents[1]
LOCAL = ROOT / '.local' / 'integration'
REMOTE = '/opt/codex-agent-integration'
PROJECT = 'codex-agent-integration'
PREFIX = 'codex-agent:integration:'


def connect():
    region = os.environ.get('CODEX_AGENT_CLASH_REGION')
    if not region:
        return _connect()
    if region not in ('hk', 'jp', 'sg') or os.name != 'nt':
        raise ValueError('Clash region must be hk, jp or sg on this Windows workstation')
    if os.environ.get('CODEX_AGENT_SSH_PROXY') != 'http://127.0.0.1:7897':
        raise ValueError('Existing Clash selection requires the explicit loopback 7897 proxy')
    # Serialize only the brief selector switch/SSH handshake. Established SSH
    # sockets retain their route after restoring the user's original selection.
    import msvcrt
    folder = ROOT / '.local/network-probe'
    folder.mkdir(parents=True, exist_ok=True)
    with (folder / 'clash-route.lock').open('a+b') as lock:
        if lock.tell() == 0:
            lock.write(b'0')
            lock.flush()
        deadline = time.monotonic() + 90
        while True:
            lock.seek(0)
            try:
                msvcrt.locking(lock.fileno(), msvcrt.LK_NBLCK, 1)
                break
            except OSError:
                if time.monotonic() >= deadline:
                    raise RuntimeError('Clash route handshake lock timed out') from None
                time.sleep(.2)
        client, state = None, None
        script = str(ROOT / 'tools/clash_route.cjs')
        try:
            selected = subprocess.run(['node', script, 'select', region], capture_output=True, timeout=30)
            if selected.returncode:
                raise RuntimeError('Existing Clash selector could not select the requested region')
            state = json.loads(selected.stdout)
            client = _connect()
        finally:
            try:
                if state:
                    try:
                        restored = subprocess.run(['node', script, 'restore', json.dumps(state)], capture_output=True, timeout=15)
                    except Exception:
                        if client:
                            client.close()
                        raise
                    if restored.returncode:
                        if client:
                            client.close()
                        raise RuntimeError('Clash selection changed concurrently or could not be restored')
            finally:
                lock.seek(0)
                msvcrt.locking(lock.fileno(), msvcrt.LK_UNLCK, 1)
        return client


def _connect():
    skill = Path(os.environ.get('CODEX_HOME', 'D:/ChatGPT/CodexHome')) / 'skills/sub2api-production/scripts/sub2api_ssh.py'
    spec = importlib.util.spec_from_file_location('codex_agent_ssh', skill)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    proxy_url = os.environ.get('CODEX_AGENT_SSH_PROXY')
    if not proxy_url:
        return module.connect('master')
    # Explicit per-project opt-in, authorized by the user on 2026-09-17.
    # The shared production skill and system proxy configuration stay untouched.
    proxy = urlparse(proxy_url)
    if proxy.scheme != 'http' or proxy.hostname not in ('127.0.0.1', 'localhost', '::1') or proxy.username or proxy.password:
        raise ValueError('CODEX_AGENT_SSH_PROXY must be an unauthenticated loopback HTTP proxy')
    meta = module.node_meta('master')
    endpoint = f"{meta['host']}:{meta['port']}"
    sock = socket.create_connection((proxy.hostname, proxy.port or 8080), timeout=20)
    try:
        sock.sendall(f'CONNECT {endpoint} HTTP/1.1\r\nHost: {endpoint}\r\n\r\n'.encode('ascii'))
        response = bytearray()
        while not response.endswith(b'\r\n\r\n'):
            chunk = sock.recv(1)
            if not chunk or len(response) >= 16384:
                raise RuntimeError('Invalid HTTP CONNECT response')
            response.extend(chunk)
        if response.split(b'\r\n', 1)[0].split()[1:2] != [b'200']:
            raise RuntimeError('Local proxy rejected SSH CONNECT')
        client = module.paramiko.SSHClient()
        client.set_missing_host_key_policy(module.paramiko.AutoAddPolicy())
        client.connect(hostname=str(meta['host']), port=int(meta['port']), username=module.USER,
                       password=module.load_password('master'), sock=sock, timeout=20,
                       banner_timeout=20, auth_timeout=20, look_for_keys=False, allow_agent=False)
        return client
    except Exception:
        sock.close()
        raise


def run(client, command, timeout=120):
    _, stdout, stderr = client.exec_command('set -eu\n' + command, timeout=timeout)
    output = stdout.read().decode('utf-8', 'replace')
    error = stderr.read().decode('utf-8', 'replace')
    if stdout.channel.recv_exit_status():
        # Commands can include credentials. Never echo command text.
        raise RuntimeError(error[-1500:] or output[-1500:] or 'Remote operation failed')
    return output


def write_remote(client, path, content, mode=0o600):
    if not path.startswith(REMOTE + '/'):
        raise ValueError('Path outside integration workspace')
    with client.open_sftp() as sftp:
        with sftp.file(path, 'w') as handle:
            handle.write(content)
        sftp.chmod(path, mode)


def up(client):
    LOCAL.mkdir(parents=True, exist_ok=True)
    config_file = LOCAL / 'environment.json'
    if config_file.exists():
        config = json.loads(config_file.read_text())
    else:
        pg_password, redis_password = secrets.token_hex(24), secrets.token_hex(24)
        config = {
            'environment': 'isolated-synthetic-only', 'remote_root': REMOTE,
            'DATABASE_URL': f'postgres://codex_agent_test:{pg_password}@127.0.0.1:25432/codex_agent_integration?sslmode=disable',
            'REDIS_URL': f'redis://codex_agent_test:{redis_password}@127.0.0.1:26379/0',
            'REDIS_KEY_PREFIX': PREFIX, 'REDIS_PREFIX': PREFIX,
            'CREDENTIAL_ENCRYPTION_KEY': base64.b64encode(secrets.token_bytes(32)).decode(),
            'CREDENTIAL_ENCRYPTION_KEY_ID': 'integration-v1',
            'ADMIN_COOKIE_SECURE': 'false', 'ADMIN_SESSION_TTL': '3600',
            'STATS_TIMEZONE': 'Asia/Shanghai',
            '_pg_password': pg_password, '_redis_password': redis_password,
            '_pg_admin_password': secrets.token_hex(24),
        }
        config['TEST_DATABASE_URL'] = config['DATABASE_URL']
        config['TEST_REDIS_URL'] = config['REDIS_URL']
        config_file.write_text(json.dumps(config, indent=2), encoding='utf-8')
    if config['environment'] != 'isolated-synthetic-only' or config['remote_root'] != REMOTE:
        raise RuntimeError('Unexpected integration configuration')
    config.update(purpose=PROJECT, synthetic_data=True, base_url='http://127.0.0.1:28879',
                  database={'name':'codex_agent_integration','user':'codex_agent_test'},
                  redis={'namespace':PREFIX}, REDIS_NAMESPACE=PREFIX, ADMIN_SESSION_TTL='3600')
    config_file.write_text(json.dumps(config, indent=2), encoding='utf-8')
    # The marker prevents adopting an unrelated directory or compose project.
    run(client, f'test ! -d {REMOTE} || test -f {REMOTE}/.codex-agent-integration')
    run(client, f'install -d -m 700 {REMOTE}; install -d -m 755 {REMOTE}/initdb')
    write_remote(client, REMOTE + '/.codex-agent-integration', PROJECT + '\n')
    init_sql = f"CREATE ROLE codex_agent_test LOGIN PASSWORD '{config['_pg_password']}' NOSUPERUSER NOCREATEDB NOCREATEROLE;\nCREATE DATABASE codex_agent_integration OWNER codex_agent_test;\n"
    write_remote(client, REMOTE + '/initdb/01-user.sql', init_sql, 0o644)
    redis_conf = f"bind 0.0.0.0\nprotected-mode yes\nappendonly no\nmaxmemory 96mb\nmaxmemory-policy allkeys-lru\nuser default off\nuser codex_agent_test on >{config['_redis_password']} ~{PREFIX}* &{PREFIX}* +@all -@dangerous\n"
    write_remote(client, REMOTE + '/redis.conf', redis_conf, 0o644)
    logging = {'driver': 'json-file', 'options': {'max-size': '10m', 'max-file': '2'}}
    compose = {'name': PROJECT, 'services': {
        'postgres': {'image': 'postgres:18-alpine', 'container_name': PROJECT + '-postgres',
            'environment': {'POSTGRES_PASSWORD': config['_pg_admin_password']},
            'ports': ['127.0.0.1:25432:5432'], 'volumes': ['pgdata:/var/lib/postgresql', './initdb:/docker-entrypoint-initdb.d:ro'],
            'command': ['postgres', '-c', 'max_connections=30', '-c', 'shared_buffers=128MB', '-c', 'statement_timeout=15000', '-c', 'lock_timeout=3000'],
            'mem_limit': '512m', 'cpus': 1.0, 'pids_limit': 128,
            'healthcheck': {'test': ['CMD-SHELL', 'pg_isready -U postgres -d postgres'], 'interval': '2s', 'timeout': '2s', 'retries': 30}, 'logging': logging},
        'redis': {'image': 'redis:8-alpine', 'container_name': PROJECT + '-redis',
            'ports': ['127.0.0.1:26379:6379'], 'volumes': ['./redis.conf:/usr/local/etc/redis/redis.conf:ro'],
            'command': ['redis-server', '/usr/local/etc/redis/redis.conf'],
            'mem_limit': '128m', 'cpus': 0.5, 'pids_limit': 64, 'logging': logging}},
        'volumes': {'pgdata': {}}, 'networks': {'default': {}}}
    write_remote(client, REMOTE + '/compose.json', json.dumps(compose, indent=2))
    run(client, f'docker compose -p {PROJECT} -f {REMOTE}/compose.json up -d --wait', timeout=180)
    print('Isolated PostgreSQL and Redis ready; private configuration: .local/integration/environment.json')
    status(client)


def status(client):
    print(run(client, f'docker compose -p {PROJECT} -f {REMOTE}/compose.json ps --format json').strip())


def performance(client, migration=False):
    config = json.loads((LOCAL / 'environment.json').read_text())
    database = 'codex_agent_migration' if migration else 'codex_agent_performance'
    check = "SELECT 1 FROM pg_database WHERE datname='"+database+"'"
    exists = run(client, 'docker exec codex-agent-integration-postgres psql -U postgres -Atc ' + shlex.quote(check)).strip()
    if exists != '1':
        run(client, 'docker exec codex-agent-integration-postgres psql -U postgres -v ON_ERROR_STOP=1 -c ' + shlex.quote('CREATE DATABASE '+database+' OWNER codex_agent_test'))
    config['DATABASE_URL'] = config['DATABASE_URL'].replace('/codex_agent_integration?', '/'+database+'?')
    config['TEST_DATABASE_URL'] = config['DATABASE_URL']
    config.update(purpose='codex-agent-performance', base_url='http://127.0.0.1:28880',
                  database={'name':database,'user':'codex_agent_test'},
                  REDIS_NAMESPACE=PREFIX+'performance:', ADMIN_SESSION_TTL='3600')
    config['REDIS_KEY_PREFIX'] = config['REDIS_NAMESPACE']
    config['REDIS_PREFIX'] = config['REDIS_NAMESPACE']
    if migration:
        config.update(purpose=PROJECT, base_url='http://127.0.0.1:28881')
    config['redis'] = {'namespace':config['REDIS_NAMESPACE']}
    (LOCAL / ('migration-environment.json' if migration else 'performance-environment.json')).write_text(json.dumps(config, indent=2), encoding='utf-8')
    print('Dedicated synthetic performance database ready')


def tunnel(client, http_only=False):
    transport = client.get_transport()
    transport.set_keepalive(20)
    servers = []
    mappings = [(28882, 28880)] if http_only else [(25432, 25432), (26379, 26379)]
    for port, remote_port in mappings:
        class Handler(socketserver.BaseRequestHandler):
            target_port = remote_port
            def handle(self):
                channel = transport.open_channel('direct-tcpip', ('127.0.0.1', self.target_port), self.request.getpeername())
                try:
                    while True:
                        ready, _, _ = select.select([self.request, channel], [], [], 10)
                        if self.request in ready:
                            data = self.request.recv(65536)
                            if not data: break
                            channel.sendall(data)
                        if channel in ready:
                            data = channel.recv(65536)
                            if not data: break
                            self.request.sendall(data)
                finally:
                    channel.close()
        class Server(socketserver.ThreadingTCPServer):
            allow_reuse_address = True
            daemon_threads = True
            # Concurrent pool open bursts (Go test pools, 8-way handler
            # concurrency) overflow the socketserver default backlog of 5 and
            # surface as instant "connection refused" against the tunnel.
            request_queue_size = 256
        server = Server(('127.0.0.1', port), Handler)
        servers.append(server)
        threading.Thread(target=server.serve_forever, daemon=True).start()
    print('SSH tunnels listening: '+', '.join(str(a)+' -> '+str(b) for a,b in mappings), flush=True)
    try:
        while transport.is_active(): time.sleep(1)
    finally:
        for server in servers: server.shutdown(); server.server_close()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action', choices=['up', 'status', 'tunnel', 'http-tunnel', 'stop', 'redis-stop', 'redis-start', 'performance', 'migration'])
    args = parser.parse_args()
    client = connect()
    try:
        if args.action == 'up': up(client)
        elif args.action == 'status': status(client)
        elif args.action == 'tunnel': tunnel(client)
        elif args.action == 'http-tunnel': tunnel(client, http_only=True)
        elif args.action == 'performance': performance(client)
        elif args.action == 'migration': performance(client, migration=True)
        else:
            run(client, f'test -f {REMOTE}/.codex-agent-integration')
            action = {'stop': 'stop', 'redis-stop': 'stop redis', 'redis-start': 'start redis'}[args.action]
            print(run(client, f'docker compose -p {PROJECT} -f {REMOTE}/compose.json {action}'))
    finally: client.close()


if __name__ == '__main__': main()
