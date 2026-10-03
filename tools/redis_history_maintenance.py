"""Run and observe bounded server-local history archival; credentials stay remote."""
import argparse
import hashlib
import json
from pathlib import Path
import shlex

from integration_env import ROOT, connect, run

REMOTE = '/opt/codex-agent-maintenance/redis-history-20260930'

BOOTSTRAP = r'''
import hashlib,json,os,pathlib,subprocess
root=pathlib.Path(PAYLOAD['root'])
assert str(root)=='/opt/codex-agent-maintenance/redis-history-20260930'
assert not any(p.is_symlink() for p in (root,*root.parents))
binary=root/'redis-history-archive'
assert hashlib.sha256(binary.read_bytes()).hexdigest()==PAYLOAD['binary_sha256']
registry=json.loads(pathlib.Path('/var/lib/codex-agent-release/active-runtime.json').read_text())
assert registry.get('pending') is None and not registry.get('candidate')
if PAYLOAD['retire']:
 allowed=set(PAYLOAD['reader_sha256'])
 assert allowed and registry['active']['binary_sha256'] in allowed
 checked=[]
 for proc in pathlib.Path('/proc').iterdir():
  if not proc.name.isdigit():continue
  try:
   if (proc/'comm').read_text().strip()!='codex-agent':continue
   actual=hashlib.sha256((proc/'exe').read_bytes()).hexdigest()
  except FileNotFoundError:continue
  assert actual in allowed,'A legacy runtime still exists; retain Redis until it drains'
  checked.append(int(proc.name))
 assert registry['active']['pid'] in checked
env={}
for line in pathlib.Path('/etc/codex-agent/production.env').read_text().splitlines():
 if '=' in line and not line.startswith('#'):
  k,v=line.split('=',1);env[k]=v
from urllib.parse import urlparse
pg=urlparse(env['DATABASE_URL']);redis=urlparse(env['REDIS_URL'])
assert pg.hostname=='127.0.0.1' and pg.port==25433 and pg.path=='/codex_agent'
assert redis.hostname=='127.0.0.1' and redis.port==26380
assert env.get('REDIS_KEY_PREFIX')=='codex-agent:production:'
unit=PAYLOAD['unit']
assert unit.startswith('codex-agent-history-archive-') and all(c.isalnum() or c=='-' for c in unit)
args=['systemd-run','--unit='+unit,'--property=EnvironmentFile=/etc/codex-agent/production.env',
 '--property=UMask=0077','--property=Nice=10','--property=IOWeight=50',
 '--property=MemoryMax=2G','--property=RuntimeMaxSec=7200',
 '--property=StandardOutput=append:'+str(root/(unit+'.jsonl')),
 '--property=StandardError=append:'+str(root/(unit+'.stderr')),
 str(binary),'--apply','--workers='+str(PAYLOAD['workers']),'--delay='+PAYLOAD['delay']]
if PAYLOAD['retire']:args.append('--retire')
if PAYLOAD['limit']:args.append('--limit='+str(PAYLOAD['limit']))
p=subprocess.run(args,capture_output=True,text=True)
assert p.returncode==0,'maintenance launch failed'
print(json.dumps({'status':'started','unit':unit,'retire':PAYLOAD['retire'],'active_sha256':registry['active']['binary_sha256']}))
'''

STATUS = r'''
import json,pathlib,subprocess
root=pathlib.Path(PAYLOAD['root']);unit=PAYLOAD['unit']
assert unit.startswith('codex-agent-history-archive-') and all(c.isalnum() or c=='-' for c in unit)
p=subprocess.run(['systemctl','show',unit,'-p','ActiveState','-p','SubState','-p','Result','-p','ExecMainStatus','-p','MemoryCurrent'],capture_output=True,text=True)
result={'service':dict(line.split('=',1) for line in p.stdout.splitlines() if '=' in line)}
log=root/(unit+'.jsonl')
if log.exists():
 with log.open('rb') as handle:
  handle.seek(max(0,log.stat().st_size-4096));lines=handle.read().decode().splitlines()
 result['progress']=[json.loads(line) for line in lines[-2:] if line.startswith('{')]
redis=json.loads(subprocess.check_output(['docker','inspect','codex-agent-redis']))[0]
proc=pathlib.Path('/proc')/str(redis['State']['Pid'])
result['redis_process']={line.split(':',1)[0]:line.split(':',1)[1].strip() for line in (proc/'status').read_text().splitlines() if line.startswith(('VmRSS:','VmSwap:'))}
result['host_memory']={line.split(':',1)[0]:line.split(':',1)[1].strip() for line in pathlib.Path('/proc/meminfo').read_text().splitlines() if line.startswith(('MemTotal:','MemAvailable:','SwapFree:'))}
print(json.dumps(result))
'''


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action', choices=('upload', 'start', 'status'))
    parser.add_argument('--binary', type=Path, default=ROOT/'.local/redis-memory-20260930/redis-history-archive')
    parser.add_argument('--unit', default='codex-agent-history-archive-initial')
    parser.add_argument('--retire', action='store_true')
    parser.add_argument('--reader-sha256', action='append', default=[])
    parser.add_argument('--workers', type=int, choices=(1, 2, 3, 4), default=2)
    parser.add_argument('--delay', choices=('1ms', '3ms', '10ms', '30ms'), default='3ms')
    parser.add_argument('--limit', type=int, default=0)
    args = parser.parse_args()
    if args.action in ('upload', 'start'):
        raise SystemExit('Permanent history archival was retired on 2026-09-30. Use the bounded Redis cache; this tool supports status only.')
    payload = dict(root=REMOTE, unit=args.unit, retire=args.retire, reader_sha256=args.reader_sha256,
                   workers=args.workers, delay=args.delay, limit=args.limit)
    client = connect()
    try:
        if args.action == 'upload':
            digest = hashlib.sha256(args.binary.read_bytes()).hexdigest()
            run(client, 'install -d -m 700 ' + shlex.quote(REMOTE))
            with client.open_sftp() as sftp:
                sftp.put(str(args.binary), REMOTE+'/redis-history-archive')
                sftp.chmod(REMOTE+'/redis-history-archive', 0o700)
            script = 'import hashlib,json,pathlib\np=pathlib.Path('+repr(REMOTE+'/redis-history-archive')+')\nassert hashlib.sha256(p.read_bytes()).hexdigest()=='+repr(digest)+'\nprint(json.dumps({"status":"uploaded","sha256":'+repr(digest)+'}))'
        elif args.action == 'start':
            payload['binary_sha256'] = hashlib.sha256(args.binary.read_bytes()).hexdigest()
            script = 'PAYLOAD=' + repr(payload) + '\n' + BOOTSTRAP
        else:
            script = 'PAYLOAD=' + repr(payload) + '\n' + STATUS
        print(run(client, 'python3 -B -c '+shlex.quote(script), timeout=60))
    finally:
        client.close()


if __name__ == '__main__':
    main()
