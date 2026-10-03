"""Prepare dedicated test keys/groups for already synchronized accounts.

Run on the current main host with workbench_bridge.py in the same directory.
No inference, OAuth refresh or source database changes.
"""
import argparse
import json
from pathlib import Path
import subprocess
import ipaddress

from workbench_bridge import API, ROOT, SafeError, atomic


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--proxy-only', action='store_true')
    args = parser.parse_args()
    api = API('http://127.0.0.1:18879', Path('/var/lib/codex-agent/client-key').read_text().strip())
    path = ROOT / 'state.json'
    state = json.loads(path.read_text()) if path.exists() else {'schema': 1, 'accounts': {}}
    if args.proxy_only:
        host = subprocess.check_output(['docker', 'exec', 'workspace-transfer-tool-backend', 'python', '-c',
            "import socket;print(socket.gethostbyname('account-warp'))"], timeout=10).decode().strip()
        ipaddress.ip_address(host)
        name = '18876 OAuth 固定出口'
        matches = [p for p in api.call('/api/proxies')['proxies'] if p['name'] == name]
        if len(matches) > 1:
            raise SafeError('ambiguous_proxy')
        if matches:
            proxy = matches[0]
            if (proxy.get('host') != host or proxy.get('port') != 40001
                or proxy.get('type') != 'socks5' or not proxy.get('enabled')):
                raise SafeError('existing_proxy_config_mismatch')
        else:
            proxy = api.call('/api/proxies', {'name': name, 'type': 'socks5',
                'host': host, 'port': 40001, 'enabled': True})
        state['proxy_id'] = proxy['id']
        atomic(path, state)
        check = api.call('/api/proxies/test', {'id': proxy['id']})
        print(json.dumps({k: check.get(k) for k in ('ok', 'http_code', 'stage', 'error_code', 'latency_ms')}))
        return
    mappings = sorted(state['accounts'].items())
    if len(mappings) < 2:
        raise SafeError('need_two_distinct_synced_accounts')
    reserved = state.get('test_assignments') or {
        'single': [mappings[0][1]['account_id']],
        'pool': [m['account_id'] for _, m in mappings[:-1]],
        'cli': [mappings[-1][1]['account_id']]}
    state['test_assignments'] = reserved
    atomic(path, state)
    output = {}
    for kind, ids in reserved.items():
        name = 'workbench-acceptance-' + kind
        groups = [g for g in api.call('/api/groups')['groups'] if g['name'] == name]
        if len(groups) > 1:
            raise SafeError('ambiguous_test_group')
        group = groups[0] if groups else api.call('/api/groups', {'name': name,
            'description': '受控验收分组；CLI 与压力账号隔离'})
        for account_id in ids:
            account = api.call('/api/accounts/' + account_id)['account']
            if 'workbench-18876' not in account.get('tags', []) or account.get('has_refresh_token'):
                raise SafeError('account_ownership_changed')
            if group['id'] not in account.get('groups', []):
                api.call('/api/accounts/' + account_id, {'version': account['version'],
                    'groups': account.get('groups', []) + [group['id']]}, 'PUT')
        key_path = ROOT / (kind + '-key')
        metadata_path = ROOT / (kind + '-key-metadata.json')
        if not key_path.exists():
            keys = [k for k in api.call('/api/keys')['keys'] if k['name'] == name]
            if keys:
                raise SafeError('test_key_exists_without_private_material')
            key = api.call('/api/keys', {'name': name, 'groups': [group['id']]})
            import os
            fd = os.open(key_path, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
            with os.fdopen(fd, 'w') as f:
                f.write(key['key'])
                f.flush()
                os.fsync(f.fileno())
            atomic(metadata_path, {'id': key['id'], 'group_id': group['id']})
        output[kind] = {'accounts': len(ids), 'key_file': str(key_path)}
    print(json.dumps(output))


if __name__ == '__main__':
    try:
        main()
    except Exception as exc:
        print(json.dumps({'ok': False, 'error_code': str(exc) if isinstance(exc, SafeError) else type(exc).__name__}))
        raise SystemExit(1)
