"""Start/collect the authorized four-tier test on Codex Agent only."""
import argparse
from datetime import datetime, timezone
import hashlib
import json
from pathlib import Path
import re
import shlex

from integration_env import ROOT, connect, run

REMOTE = '/opt/codex-agent-integration/gateway-load'


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action', choices=['start', 'status', 'collect'])
    parser.add_argument('--scope', choices=['single', 'pool'], default='single')
    parser.add_argument('--run-id')
    parser.add_argument('--model', default='gpt-5.6-luna')
    parser.add_argument('--levels', default='30,50,70,100')
    parser.add_argument('--waves', type=int, default=2)
    args = parser.parse_args()
    if args.action == 'start':
        if args.run_id:
            raise ValueError('Start allocates an immutable new run')
        rid = datetime.now(timezone.utc).strftime('%Y%m%dT%H%M%S') + '-' + args.scope
    else:
        rid = args.run_id or ''
    if not re.fullmatch(r'\d{8}T\d{6}-(single|pool)', rid):
        raise ValueError('A valid run id is required')
    folder = REMOTE + '/' + rid
    unit = 'codex-agent-load-' + rid.lower()
    client = connect()
    try:
        if args.action == 'start':
            run(client, 'test ! -e ' + folder + '\ninstall -d -m 700 ' + folder)
            files = {'live_load.py': ROOT / 'integration/gateway_environment/live_load.py',
                'monitor.py': ROOT / 'integration/gateway_environment/monitor.py',
                'runner.py': ROOT / 'deploy/gateway_load_remote.py'}
            expected = {}
            with client.open_sftp() as sftp:
                for name, path in files.items():
                    content = path.read_bytes()
                    expected[name] = hashlib.sha256(content).hexdigest()
                    with sftp.file(folder + '/' + name, 'wb') as f:
                        f.write(content)
                    sftp.chmod(folder + '/' + name, 0o600)
            verify = 'import pathlib,hashlib,json; p=pathlib.Path(' + repr(folder) + '); e=' + repr(expected) + '; assert all(hashlib.sha256((p/k).read_bytes()).hexdigest()==v for k,v in e.items())'
            run(client, 'python3 -c ' + shlex.quote(verify))
            command = ['systemd-run', '--unit=' + unit, '--property=MemoryMax=256M',
                '--property=CPUQuota=50%', '--property=TasksMax=256', '--property=RuntimeMaxSec=1800',
                '--property=UMask=0077', '--collect', '/usr/bin/python3', folder + '/runner.py',
                '--scope', args.scope, '--model', args.model, '--levels', args.levels, '--waves', str(args.waves)]
            run(client, shlex.join(command), 30)
            local = ROOT / '.local/gateway-environment' / rid
            local.mkdir(parents=True)
            (local / 'launch.json').write_text(json.dumps({'run_id': rid, 'unit': unit, 'remote': folder, 'scripts': expected}, indent=2))
            print(json.dumps({'run_id': rid, 'remote': folder, 'unit': unit}))
        else:
            code = ('import pathlib,json; p=pathlib.Path(' + repr(folder) + '); out={}; '
                '\nfor name in ["runner.json","load/report.json","monitor/report.json"]:'
                '\n f=p/name'
                '\n if f.exists():'
                '\n  d=json.loads(f.read_text()); out[name]=d if name=="runner.json" else {k:v for k,v in d.items() if k in ("status","summary","sanity","samples","max_gateway_admitted","gateway_peak_inflight","metadata","cooldowns","sample_count")}'
                '\nprint(json.dumps(out))')
            print(run(client, 'python3 -c ' + shlex.quote(code), 30))
            if args.action == 'collect':
                local = ROOT / '.local/gateway-environment' / rid
                local.mkdir(parents=True, exist_ok=True)
                with client.open_sftp() as sftp:
                    for name in ['runner.json', 'metadata.json', 'load/report.json', 'monitor/report.json',
                                 'monitor/samples.jsonl', 'load-output.txt', 'monitor-output.txt']:
                        try:
                            sftp.stat(folder + '/' + name)
                        except FileNotFoundError:
                            continue
                        target = local / name
                        target.parent.mkdir(parents=True, exist_ok=True)
                        sftp.get(folder + '/' + name, str(target))
                print('Collected content-free evidence: ' + str(local))
    finally:
        client.close()


if __name__ == '__main__':
    main()
