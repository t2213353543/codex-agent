"""Deploy/inspect only the independent 18876 -> Codex Agent delivery adapter."""
import argparse
import datetime as dt
import hashlib
import json
from pathlib import Path
import shlex

from integration_env import ROOT, connect, run

REMOTE = '/opt/codex-agent/workbench-bridge'
STATE = '/var/lib/codex-agent-workbench-bridge'
SERVICE = 'codex-agent-workbench-bridge'

UNIT = '''[Unit]
Description=Deliver paid workbench access tokens to Codex Agent
After=docker.service codex-agent.service

[Service]
Type=oneshot
User=root
ExecStart=/usr/bin/python3 /opt/codex-agent/workbench-bridge/workbench_bridge.py --apply
TimeoutStartSec=180
UMask=0077
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/var/lib/codex-agent-workbench-bridge
MemoryMax=96M
CPUQuota=10%
TasksMax=32
LimitNOFILE=128
'''
TIMER = '''[Unit]
Description=Synchronize paid workbench accounts every minute

[Timer]
OnBootSec=90
OnUnitInactiveSec=60
RandomizedDelaySec=5
Unit=codex-agent-workbench-bridge.service

[Install]
WantedBy=timers.target
'''


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action', choices=['preview', 'apply', 'install', 'status', 'stop', 'prepare-proxy', 'prepare-tests'])
    args = parser.parse_args()
    client = connect()
    try:
        if args.action in ('preview', 'apply'):
            code = (ROOT / 'deploy/workbench_bridge.py').read_text(encoding='utf-8')
            command = 'python3 -c ' + shlex.quote(code) + (' --apply' if args.action == 'apply' else '')
            _, stdout, stderr = client.exec_command(command, timeout=180)
            output = stdout.read().decode('utf-8')
            status = stdout.channel.recv_exit_status()
            result = json.loads(output)
            folder = ROOT / '.local/gateway-environment' / dt.datetime.now(dt.timezone.utc).strftime('%Y%m%dT%H%M%S.%fZ')
            folder.mkdir(parents=True)
            (folder / ('bridge-' + args.action + '.json')).write_text(json.dumps(result, ensure_ascii=False, indent=2), encoding='utf-8')
            print(json.dumps(result, ensure_ascii=False, indent=2))
            if status:
                raise SystemExit(status)
        elif args.action in ('install', 'prepare-proxy', 'prepare-tests'):
            source = (ROOT / 'deploy/workbench_bridge.py').read_bytes()
            run(client, 'install -d -m 700 ' + REMOTE + '\ninstall -d -m 700 ' + STATE)
            files = {REMOTE + '/workbench_bridge.py': source,
                REMOTE + '/workbench_test_setup.py': (ROOT / 'deploy/workbench_test_setup.py').read_bytes()}
            if args.action == 'install':
                files.update({'/etc/systemd/system/' + SERVICE + '.service': UNIT.encode(),
                    '/etc/systemd/system/' + SERVICE + '.timer': TIMER.encode()})
            with client.open_sftp() as sftp:
                for name, data in files.items():
                    with sftp.file(name + '.tmp', 'wb') as f:
                        f.write(data)
                    sftp.chmod(name + '.tmp', 0o600 if name.endswith('.py') else 0o644)
                    sftp.posix_rename(name + '.tmp', name)
            expected = hashlib.sha256(source).hexdigest()
            actual = run(client, 'sha256sum ' + REMOTE + '/workbench_bridge.py').split()[0]
            if actual != expected:
                raise RuntimeError('Bridge artifact hash mismatch')
            if args.action != 'install':
                print(run(client, 'python3 ' + REMOTE + '/workbench_test_setup.py'
                    + (' --proxy-only' if args.action == 'prepare-proxy' else ''), 90))
                return
            run(client, 'systemctl daemon-reload\nsystemctl enable --now ' + SERVICE + '.timer')
            # A partial delivery remains observable while the timer keeps processing new jobs.
            _, stdout, stderr = client.exec_command('systemctl start ' + SERVICE + '.service', timeout=180)
            stdout.read()
            status = stdout.channel.recv_exit_status()
            print(run(client, 'systemctl is-active ' + SERVICE + '.timer\ncat ' + STATE + '/latest.json'))
            print(json.dumps({'timer_installed': True, 'initial_cycle_exit_code': status}))
        elif args.action == 'status':
            print(run(client, 'systemctl show ' + SERVICE + '.timer --property=ActiveState --property=NextElapseUSecMonotonic\n'
                'systemctl show ' + SERVICE + '.service --property=Result --property=ExecMainStatus\n'
                'cat ' + STATE + '/latest.json'))
        elif args.action == 'stop':
            print(run(client, 'systemctl disable --now ' + SERVICE + '.timer\n'
                'systemctl stop ' + SERVICE + '.service\nsystemctl show ' + SERVICE + '.timer --property=ActiveState'))
    finally:
        client.close()


if __name__ == '__main__':
    main()
