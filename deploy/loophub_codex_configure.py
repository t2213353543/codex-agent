"""Bind exactly one Loophub mother to the already-running private Agent bridge.

Run on the main host: python3 loophub_codex_configure.py --config CONFIG
Existing destination definitions and other mothers are preserved.
"""
import argparse
import json
from pathlib import Path
import subprocess
import urllib.error
import urllib.request


def configure(config):
    owner_email = config.get('owner_email', 'radios.plucks-60@icloud.com').lower()
    image = json.loads(subprocess.check_output(['docker', 'inspect',
        config.get('source_container', 'loophub-multitenant-app')], timeout=15))[0]
    env = dict(value.split('=', 1) for value in image['Config']['Env'] if '=' in value)
    network = next(value for value in image['NetworkSettings']['Networks'].values() if value.get('IPAddress'))
    base = 'http://' + network['IPAddress'] + ':8765'
    http = urllib.request.build_opener(urllib.request.ProxyHandler({}))

    def call(path, data=None, method=None):
        request = urllib.request.Request(base + path,
            headers={'x-admin-key': env['ADMIN_API_KEY'], 'Content-Type': 'application/json'},
            data=json.dumps(data).encode() if data is not None else None, method=method)
        with http.open(request, timeout=40) as response:
            return json.load(response)

    bridge_url = 'http://' + config['listen_host'] + ':' + str(config.get('listen_port', 18881))
    ingress_key = Path(config['ingress_key_file']).read_text().strip()
    ready = urllib.request.Request(bridge_url + '/v0/management/auth-files',
        headers={'Authorization': 'Bearer ' + ingress_key})
    with http.open(ready, timeout=10) as response:
        assert isinstance(json.load(response).get('files'), list), 'adapter_not_ready'
    source = call('/api/team-rotation/status')
    matches = [owner for owner in source['owners'] if owner.get('email', '').lower() == owner_email]
    assert len(matches) == 1, 'source_owner_ambiguous'
    owner = matches[0]
    assert owner['workspace_id'] == config['workspace_id'], 'source_workspace_changed'
    assert owner['status'] == 'active', 'source_owner_not_active'
    name = 'codex-agent · ' + owner_email
    matches = [item for item in source['destinations'] if item.get('name') == name]
    assert len(matches) <= 1, 'source_destination_ambiguous'
    if matches:
        destination = matches[0]
        assert destination.get('platform') == 'cpa' and destination.get('url') == bridge_url, 'destination_endpoint_changed'
        assert destination.get('enabled'), 'destination_disabled'
    else:
        destination = call('/api/team-rotation/sub2api-destinations',
            {'platform': 'cpa', 'name': name, 'enabled': True, 'url': bridge_url,
             'api_key': ingress_key, 'timeout': 120})['item']
    previous = owner.get('destination_ids') or []
    selected = list(dict.fromkeys(previous + [destination['id']]))
    if selected != previous:
        call('/api/team-rotation/owners/' + owner['id'], {'destination_ids': selected}, 'PATCH')
    result = call('/api/team-rotation/status')
    after = next(item for item in result['owners'] if item['id'] == owner['id'])
    assert after['destination_ids'] == selected or set(after['destination_ids']) == set(selected), 'binding_not_applied'
    return {'ok': True, 'owner_id': owner['id'], 'workspace_id': owner['workspace_id'],
        'active_children': after.get('active_children'), 'previous_destination_ids': previous,
        'destination_id': destination['id'], 'destination_ids': after['destination_ids'],
        'rotation_enabled': result.get('config', {}).get('enabled')}


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--config', type=Path, required=True)
    arguments = parser.parse_args()
    try:
        print(json.dumps(configure(json.loads(arguments.config.read_text()))))
    except Exception as error:
        # Never include HTTP response bodies or credential-bearing commands.
        print(json.dumps({'ok': False, 'error': type(error).__name__,
            'http_status': getattr(error, 'code', None)}))
        raise SystemExit(1)
