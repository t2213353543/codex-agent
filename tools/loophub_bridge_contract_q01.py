"""Child process for TestLoophubBridgeQ01RealManagementContract (loopback only)."""
import importlib.util
import json
from pathlib import Path
import sys
import tempfile
import time
import urllib.parse


def main():
    params = json.load(sys.stdin)
    target = urllib.parse.urlsplit(params['base'])
    if target.scheme != 'http' or target.hostname != '127.0.0.1':
        raise AssertionError('contract only permits the parent Go loopback server')
    source = Path(__file__).resolve().parents[1] / 'deploy/loophub_codex_bridge.py'
    spec = importlib.util.spec_from_file_location('loophub_bridge_contract_q01', source)
    bridge = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(bridge)
    with tempfile.TemporaryDirectory() as folder:
        root = Path(folder)
        (root / 'key').write_text('synthetic-local-ingress-key-' * 2)
        api = bridge.API(params['base'], params['key'])
        instance = bridge.Bridge({'workspace_id': 'synthetic-workspace',
            'group_id': params['group_id'], 'state_file': str(root / 'state.json'),
            'ingress_key_file': str(root / 'key')}, api=api)
        instance.state['accounts']['synthetic@example.invalid.json'] = {
            'account_id': params['account_id'], 'name': 'q01-contract',
            'email': 'synthetic@example.invalid', 'initialized': True,
            # Intentionally old: readiness must follow the real current metadata.
            'expires_at': int(time.time()) - 100}
        result = instance.list_files()['files'][0]
        for key, expected in params['expected'].items():
            if result.get(key) != expected:
                raise AssertionError(f'{key}: got {result.get(key)!r}, want {expected!r}')
        print(json.dumps({'ok': True, 'checks': len(params['expected']), 'result': result}))


if __name__ == '__main__':
    main()
