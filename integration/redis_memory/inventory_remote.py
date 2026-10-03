"""Bounded, read-only Redis inventory. No key values leave the server."""
import argparse
import collections
import datetime
import hashlib
import json
from pathlib import Path
import socket
import subprocess
import time
from urllib.parse import urlparse, unquote


class Redis:
    def __init__(self):
        env = dict(line.split('=', 1) for line in Path('/etc/codex-agent/production.env').read_text().splitlines() if '=' in line and not line.startswith('#'))
        url = urlparse(env['REDIS_URL'])
        assert url.hostname == '127.0.0.1' and url.port == 26380
        self.sock = socket.create_connection((url.hostname, url.port), timeout=15)
        self.file = self.sock.makefile('rb')
        self.call('AUTH', unquote(url.username), unquote(url.password))
        if url.path.strip('/'):
            self.call('SELECT', url.path.strip('/'))

    def read(self):
        line = self.file.readline()
        if not line:
            raise RuntimeError('Redis disconnected')
        kind, rest = line[:1], line[1:-2]
        if kind == b'-':
            raise RuntimeError(rest.decode())
        if kind == b'+':
            return rest.decode()
        if kind == b':':
            return int(rest)
        if kind == b'$':
            size = int(rest)
            if size < 0:
                return None
            data = self.file.read(size)
            assert self.file.read(2) == b'\r\n'
            return data
        if kind == b'*':
            return [self.read() for _ in range(int(rest))]
        raise RuntimeError('Unexpected Redis protocol')

    def pipeline(self, commands):
        parts = []
        for command in commands:
            parts.append(('*%s\r\n' % len(command)).encode())
            for arg in command:
                value = arg if isinstance(arg, bytes) else str(arg).encode()
                parts.extend([('$%s\r\n' % len(value)).encode(), value, b'\r\n'])
        self.sock.sendall(b''.join(parts))
        return [self.read() for _ in commands]

    def call(self, *args):
        return self.pipeline([args])[0]


def info(redis, section):
    return dict(line.split(':', 1) for line in redis.call('INFO', section).decode().splitlines() if ':' in line)


def memory_stats(redis):
    def scalar(value):
        if isinstance(value, bytes):
            value = value.decode('ascii')
            try:
                return float(value) if '.' in value or 'e' in value.lower() else int(value)
            except ValueError:
                return value
        if isinstance(value, list):
            return {scalar(value[i]): scalar(value[i + 1]) for i in range(0, len(value), 2)}
        return value
    return scalar(redis.call('MEMORY', 'STATS'))


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--sample', type=int, default=2000)
    args = parser.parse_args()
    redis = Redis()
    report = {'at': datetime.datetime.now(datetime.timezone.utc).isoformat()}
    # MEMORY STATS is permitted by the runtime ACL even when INFO is denied.
    report['memory_stats'] = memory_stats(redis)
    for section in ('server', 'memory', 'persistence', 'stats', 'keyspace', 'clients'):
        try:
            value = info(redis, section)
        except RuntimeError as exc:
            value = {'unavailable': str(exc)}
        if section == 'server' and 'redis_version' in value:
            value = {k: value[k] for k in ('redis_version', 'uptime_in_seconds', 'process_id', 'config_file')}
        report[section] = value
    conf = Path('/opt/codex-agent-db/redis.conf').read_text().splitlines()
    report['configuration'] = [line for line in conf if line.split(' ', 1)[0] in ('maxmemory', 'maxmemory-policy', 'appendonly', 'appendfsync', 'save', 'activedefrag', 'lazyfree-lazy-expire', 'auto-aof-rewrite-percentage', 'auto-aof-rewrite-min-size')]
    report['acl_permissions'] = [' '.join(line.split()[:2] + [word for word in line.split()[2:] if word.startswith(('+', '-'))]) for line in conf if line.startswith('user ')]
    inspect = json.loads(subprocess.check_output(['docker', 'inspect', 'codex-agent-redis']))[0]
    report['container_limits'] = {k: inspect['HostConfig'].get(k) for k in ('Memory', 'MemorySwap', 'NanoCpus', 'PidsLimit')}
    report['container_pid'] = inspect['State']['Pid']
    cursor = 0
    complete = False
    seen = set()
    groups = collections.defaultdict(lambda: {'count': 0, 'memory_bytes': 0, 'max_bytes': 0, 'permanent': 0, 'ttl_min': None, 'ttl_max': None, 'types': collections.Counter()})
    largest = []
    started = time.monotonic()
    while len(seen) < args.sample and time.monotonic() - started < 90:
        cursor, keys = redis.call('SCAN', cursor, 'COUNT', 100)
        keys = [key for key in keys if key not in seen]
        commands = [(verb, *params) for key in keys for verb, params in [('TYPE', (key,)), ('MEMORY', ('USAGE', key, 'SAMPLES', 5)), ('PTTL', (key,))]]
        values = redis.pipeline(commands) if commands else []
        for i, key in enumerate(keys):
            seen.add(key)
            kind, size, ttl = values[i * 3:i * 3 + 3]
            if size is None or ttl == -2:
                continue
            text = key.decode(errors='replace')
            prefix = ':'.join(text.split(':')[:4])
            item = groups[prefix]
            item['count'] += 1
            item['memory_bytes'] += size
            item['max_bytes'] = max(item['max_bytes'], size)
            item['permanent'] += ttl == -1
            item['types'][kind] += 1
            if ttl >= 0:
                item['ttl_min'] = ttl if item['ttl_min'] is None else min(ttl, item['ttl_min'])
                item['ttl_max'] = ttl if item['ttl_max'] is None else max(ttl, item['ttl_max'])
            largest.append({'prefix': prefix, 'key_sha256': hashlib.sha256(key).hexdigest()[:16], 'bytes': size, 'ttl_ms': ttl})
        if int(cursor) == 0:
            complete = True
            break
        time.sleep(.005)
    report['sampling'] = {'keys': len(seen), 'complete': complete, 'seconds': round(time.monotonic() - started, 3), 'groups': dict(sorted(groups.items(), key=lambda pair: -pair[1]['memory_bytes'])), 'largest': sorted(largest, key=lambda item: -item['bytes'])[:15]}
    print(json.dumps(report))


if __name__ == '__main__':
    main()
