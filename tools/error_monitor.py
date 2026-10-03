"""Install/read bounded production error monitoring; stores no request contents."""
import argparse
import hashlib
import json
from pathlib import Path
import shlex
from integration_env import ROOT, connect, run

REMOTE = '/opt/codex-agent-maintenance/error-monitor'


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action', choices=('install', 'status', 'sample'))
    args = parser.parse_args()
    client = connect()
    try:
        if args.action == 'install':
            source = ROOT / 'deploy/error_monitor.py'
            checksum = hashlib.sha256(source.read_bytes()).hexdigest()
            run(client, 'install -d -m 700 ' + shlex.quote(REMOTE))
            with client.open_sftp() as sftp:
                sftp.put(str(source), REMOTE + '/error_monitor.py')
                sftp.chmod(REMOTE + '/error_monitor.py', 0o700)
            code = 'EXPECTED=' + repr(checksum) + '\n' + r'''
import hashlib,json,pathlib,subprocess
p=pathlib.Path('/opt/codex-agent-maintenance/error-monitor/error_monitor.py')
assert hashlib.sha256(p.read_bytes()).hexdigest()==EXPECTED
base=pathlib.Path('/etc/systemd/system')
(base/'codex-agent-error-monitor.service').write_text('[Unit]\nDescription=Codex Agent bounded error metadata observation\nAfter=docker.service\n[Service]\nType=oneshot\nExecStart=/usr/bin/python3 -B /opt/codex-agent-maintenance/error-monitor/error_monitor.py\nRuntimeMaxSec=20\nTimeoutStartSec=20\nMemoryMax=128M\nNice=15\nUMask=0077\nStandardOutput=null\nStandardError=journal\n')
(base/'codex-agent-error-monitor.timer').write_text('[Unit]\nDescription=Observe Codex Agent errors every minute\n[Timer]\nOnBootSec=60\nOnUnitActiveSec=60\nAccuracySec=5\nPersistent=false\nUnit=codex-agent-error-monitor.service\n[Install]\nWantedBy=timers.target\n')
for args in (['systemctl','daemon-reload'],['systemctl','enable','--now','codex-agent-error-monitor.timer'],['systemctl','start','codex-agent-error-monitor.service']):
 result=subprocess.run(args,capture_output=True,text=True);assert result.returncode==0
print(json.dumps({'status':'installed','interval_seconds':60,'retained_observations':60,'request_contents_saved':False,'sha256':EXPECTED}))
'''
        elif args.action == 'sample':
            code = r'''
import json,subprocess,pathlib
r=subprocess.run(['systemctl','start','codex-agent-error-monitor.service'],capture_output=True);assert r.returncode==0
print((pathlib.Path('/var/lib/codex-agent-observation/latest.json')).read_text())
'''
        else:
            code = r'''
import json,pathlib,subprocess
root=pathlib.Path('/var/lib/codex-agent-observation')
result={'health':json.loads((root/'health.json').read_text()),'latest':json.loads((root/'latest.json').read_text()),'retained_bytes':sum(p.stat().st_size for p in root.iterdir() if p.is_file())}
result['timer']=subprocess.check_output(['systemctl','show','codex-agent-error-monitor.timer','-p','ActiveState','-p','NextElapseUSecMonotonic'],text=True).strip()
print(json.dumps(result))
'''
        print(run(client, 'python3 -B -c ' + shlex.quote(code), timeout=35))
    finally:
        client.close()


if __name__ == '__main__':
    main()
