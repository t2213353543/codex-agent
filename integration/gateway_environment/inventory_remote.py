"""Read-only, content-free inventory of codex-agent; no inference requests."""
import collections
import argparse
import datetime as dt
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import time
import urllib.request


def command(args, timeout=15):
    p = subprocess.run(args, capture_output=True, text=True, timeout=timeout)
    if p.returncode:
        raise RuntimeError('Read-only command failed: ' + args[0])
    return p.stdout.strip()


def sql(statement):
    value = command(['docker', 'exec', '-e', 'PGOPTIONS=-c statement_timeout=5000 -c default_transaction_read_only=on',
                     'codex-agent-postgres', 'psql', '-X', '-U', 'postgres', '-d', 'codex_agent',
                     '-At', '-v', 'ON_ERROR_STOP=1', '-c', statement], 12)
    return json.loads(value) if value else None


def snapshot(pid):
    proc = Path('/proc') / pid
    stats = proc.joinpath('stat').read_text().rsplit(') ', 1)[1].split()
    status = proc.joinpath('status').read_text()
    limits = [line.split()[-3:] for line in proc.joinpath('limits').read_text().splitlines() if line.startswith('Max open files')]
    result = {'at': time.time(), 'cpu_ticks': int(stats[11]) + int(stats[12]),
              'fd_count': len(list(proc.joinpath('fd').iterdir())), 'open_file_limits': limits,
              'rss_kib': int(re.search(r'VmRSS:\s+(\d+)', status).group(1)),
              'threads': int(re.search(r'Threads:\s+(\d+)', status).group(1))}
    # Resolve the process's actual cgroup, supporting v1 and v2 hosts.
    for line in proc.joinpath('cgroup').read_text().splitlines():
        _, controllers, location = line.split(':', 2)
        base = Path('/sys/fs/cgroup') / (controllers if controllers else '') / location.lstrip('/')
        for name in ('cpu.stat', 'memory.events', 'pids.events'):
            p = base / name
            if p.is_file():
                result[name] = dict(x.split() for x in p.read_text().splitlines() if len(x.split()) == 2)
    tcp = collections.Counter()
    socket_inodes = set()
    for p in proc.joinpath('fd').iterdir():
        try:
            target = os.readlink(p)
        except OSError:
            continue
        if target.startswith('socket:['):
            socket_inodes.add(target[8:-1])
    for name in ('tcp', 'tcp6'):
        for line in proc.joinpath('net', name).read_text().splitlines()[1:]:
            fields = line.split()
            if fields[9] in socket_inodes:
                tcp[fields[3]] += 1
    result['owned_tcp_states_hex'] = dict(tcp)
    return result


ERROR_SOURCES = frozenset({'gateway', 'client', 'upstream', 'transport'})
ERROR_PHASES = frozenset({'settings', 'request_body', 'request_policy', 'account_selection', 'proxy_route',
    'request_build', 'request_headers', 'response_headers', 'http_status', 'response_event',
    'response_body', 'response_write', 'request_read', 'websocket_handshake', 'connect'})


def observed_enum(value, allowed):
    return value if value in allowed else ('absent' if not value else 'other')


def classify(error, code, source='', phase=''):
    text = error.lower() if isinstance(error, str) else ''
    message = text
    codes = set()
    try:
        payload = json.loads(error)
        if isinstance(payload, dict):
            detail = payload.get('error', payload)
            if isinstance(detail, dict):
                codes = {detail[key].lower() for key in ('code', 'type') if isinstance(detail.get(key), str)}
                message = detail.get('message', '').lower() if isinstance(detail.get('message', ''), str) else ''
    except (ValueError, TypeError, RecursionError):
        pass
    if any(needle in text for needle in ('context canceled', 'context cancelled', 'client disconnected')):
        return 'client_cancelled'
    proxy_failure = ('proxy' in message or 'connect tunnel' in message or 'socks' in message
                     or source == 'gateway' and phase == 'proxy_route')
    if proxy_failure:
        if code == 407 or 'proxy authentication' in message:
            return 'proxy_auth_required'
        if code == 429 or re.search(r'\b429\b', message):
            return 'proxy_rate_limit'
        if code == 403 or re.search(r'\b403\b', message):
            return 'proxy_rejected'
    if codes & {'context_length_exceeded'} or 'context window' in text:
        return 'upstream_context_limit' if source == 'upstream' or 'context_length_exceeded' in codes else 'context_limit'
    if codes & {'unsupported_parameter', 'unsupported_value'} or any(needle in text for needle in ('unsupported parameter', 'unsupported value', 'invalid role')):
        return 'compatibility'
    if codes & {'insufficient_quota', 'usage_limit_reached', 'usage_limit_exceeded'}:
        return 'upstream_quota'
    if codes & {'rate_limit_exceeded', 'too_many_requests'}:
        return 'upstream_rate_limit'
    if codes & {'token_revoked', 'token_expired', 'invalid_token', 'authentication_error'} or source == 'upstream' and code == 401:
        return 'auth_failure'
    for label, patterns in (
        ('local_admission_busy', ('accounts in the key\'s groups are busy', 'session account is busy')),
        ('local_no_enabled_account', ('no enabled account', 'no accounts configured')),
        ('local_session_unavailable', ('session account is no longer', 'session account unavailable')),
        ('local_route_unavailable', ('proxy configuration changed', 'outbound proxy', 'no enabled proxy')),
        ('transport_failure', ('upstream connection failed', 'connection reset', 'tls handshake', 'no such host', 'dial tcp')),
        ('stream_or_deadline_timeout', ('timeout', 'deadline exceeded')),
        ('auth_failure', ('token_revoked', 'token has been invalidated', 'token expired', 'token has expired', 'authentication_error', 'unauthorized')),
        ('upstream_overload', ('overloaded', 'server is busy')),
        ('upstream_quota', ('usage_limit', 'usage limit', 'insufficient_quota', 'quota exceeded')),
        ('upstream_rate_limit', ('rate_limit', 'rate limit', 'too many requests')),
    ):
        if any(pattern in text for pattern in patterns):
            return label
    if source == 'upstream' and phase == 'response_event':
        return 'upstream_stream_error'
    if source == 'upstream' and phase == 'http_status':
        return 'upstream_http_error'
    if source == 'gateway' and phase == 'account_selection':
        return 'local_account_selection_error'
    if source == 'gateway' and phase == 'proxy_route':
        return 'local_route_unavailable'
    if source == 'client':
        return 'client_io_error'
    if source == 'transport':
        return 'transport_failure'
    return 'unclassified_http_' + str(code or 'unknown')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--since', help='Timezone-aware ISO instant for a post-release observation window')
    args = parser.parse_args()
    since = None
    if args.since:
        since = dt.datetime.fromisoformat(args.since.replace('Z', '+00:00'))
        if since.tzinfo is None or since.timestamp() < 0 or since > dt.datetime.now(dt.timezone.utc):
            raise ValueError('since must be a nonfuture timezone-aware instant')
    predicate = 'to_timestamp(' + str(since.timestamp()) + ')' if since else "now()-interval '48 hours'"
    properties = ('MainPID', 'ActiveState', 'SubState', 'ExecMainStartTimestamp', 'MemoryCurrent', 'MemoryMax',
                  'CPUQuotaPerSecUSec', 'TasksCurrent', 'TasksMax', 'LimitNOFILE', 'LimitNOFILESoft',
                  'TimeoutStopUSec', 'ControlGroup', 'NRestarts')
    values = command(['systemctl', 'show', 'codex-agent.service', *['--property=' + k for k in properties]])
    service = dict(line.split('=', 1) for line in values.splitlines() if '=' in line)
    pid = service['MainPID']
    assert pid.isdigit() and int(pid) > 0
    proc = Path('/proc') / pid
    argv = [x.decode() for x in proc.joinpath('cmdline').read_bytes().split(b'\0') if x]
    assert argv[0].startswith('/opt/codex-agent/') and '--web' in argv
    env = dict(x.split(b'=', 1) for x in proc.joinpath('environ').read_bytes().split(b'\0') if b'=' in x)
    report = {'schema': 'codex-agent-gateway-environment-v1', 'checked_at': dt.datetime.now(dt.timezone.utc).isoformat(),
              'scope': 'Read-only codex-agent process/dependencies and capped request metadata; no inference traffic',
              'service': service, 'binary_sha256': hashlib.sha256(proc.joinpath('exe').read_bytes()).hexdigest(),
              'binary_path': argv[0], 'web_path': argv[argv.index('--web') + 1],
              'gateway_proxy_env_present': {k: k.encode() in env for k in ('HTTP_PROXY', 'HTTPS_PROXY', 'ALL_PROXY', 'NO_PROXY')},
              'pg_max_conns': env.get(b'PG_MAX_CONNS', b'12').decode(), 'samples': []}
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    report['health'] = {}
    for route in ('/health/live', '/health/ready'):
        try:
            with opener.open('http://127.0.0.1:18879' + route, timeout=5) as response:
                report['health'][route] = response.status
        except Exception as error:
            report['health'][route] = type(error).__name__
    report['samples'].append(snapshot(pid))
    records = sql("""SELECT COALESCE(json_agg(t),'[]'::json) FROM (
      SELECT created_at,status,record->>'http_code' AS code,
             left(COALESCE(record->>'error',''),4096) AS error,
             record->>'error_source' AS error_source,record->>'error_phase' AS error_phase,
             COALESCE((record->>'latency_ms')::bigint,0) AS latency_ms,
             COALESCE(record->>'proxy_name','')<>'' AS has_proxy
      FROM request_logs WHERE created_at>=""" + predicate + """
      ORDER BY created_at DESC,id DESC LIMIT 50000) t""")
    groups = collections.Counter()
    hourly = collections.Counter()
    latencies = collections.defaultdict(list)
    for row in records:
        code = int(row['code']) if row['code'] and row['code'].isdigit() else 0
        source = observed_enum(row.get('error_source'), ERROR_SOURCES)
        phase = observed_enum(row.get('error_phase'), ERROR_PHASES)
        category = 'ok' if row['status'] == 'ok' else classify(row['error'], code, source, phase)
        groups[(code, row['status'], category, row['has_proxy'], source, phase)] += 1
        hourly[(row['created_at'][:13], code, category)] += 1
        latencies[(code, category)].append(row['latency_ms'])
    def percentiles(values):
        ordered = sorted(values)
        return {'n': len(ordered), 'p50_ms': ordered[(len(ordered)-1)//2], 'p95_ms': ordered[min(len(ordered)-1, int(len(ordered)*.95))]}
    report['logs'] = {'window_hours': None if since else 48, 'since': since.isoformat() if since else None,
        'limit': 50000, 'rows_examined': len(records), 'possibly_capped': len(records) == 50000,
        'oldest': records[-1]['created_at'] if records else None, 'newest': records[0]['created_at'] if records else None,
        'groups': [{'http_code': k[0], 'status': k[1], 'category': k[2], 'has_proxy': k[3],
                    'error_source': k[4], 'error_phase': k[5], 'count': v} for k,v in groups.most_common()],
        'hourly': [{'hour': k[0], 'http_code': k[1], 'category': k[2], 'count': v} for k,v in sorted(hourly.items())],
        'latency': [{'http_code': k[0], 'category': k[1], **percentiles(v)} for k,v in latencies.items()],
        'raw_error_text_exported': False, 'account_identifiers_exported': False}
    report['account_configuration'] = sql("""SELECT COALESCE(json_agg(t),'[]'::json) FROM (
      SELECT enabled,concurrency,priority,credential_status,
             (SELECT count(*) FROM account_proxies ap JOIN proxies p ON p.id=ap.proxy_id WHERE ap.account_id=a.id AND p.enabled) AS enabled_proxy_count,
             count(*) AS accounts
      FROM accounts a GROUP BY enabled,concurrency,priority,credential_status,enabled_proxy_count
      ORDER BY enabled DESC,concurrency DESC) t""")
    report['pg_activity'] = sql("""SELECT COALESCE(json_agg(t),'[]'::json) FROM (
      SELECT state,wait_event_type,wait_event,count(*) FROM pg_stat_activity
      WHERE datname='codex_agent' GROUP BY state,wait_event_type,wait_event) t""")
    report['pg_database'] = sql("SELECT row_to_json(t) FROM (SELECT numbackends,xact_commit,xact_rollback,deadlocks,temp_files,temp_bytes FROM pg_stat_database WHERE datname='codex_agent') t")
    report['samples'].append(snapshot(pid))
    report['same_pid_end'] = command(['systemctl', 'show', 'codex-agent.service', '--property=MainPID', '--value']) == pid
    report['quality_inference'] = 'Status codes alone do not establish account risk classification or model quality degradation'
    print(json.dumps(report, indent=2))


if __name__ == '__main__':
    main()
