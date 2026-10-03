"""Run isolated current Go runtime acceptance with server-only account credentials."""
import argparse
from datetime import datetime, timezone
import gzip
import hashlib
import json
import os
from pathlib import Path
import shlex
import subprocess

from integration_env import ROOT, connect
from project_layout import backend_source_paths

REMOTE = r'''
import json,os,pathlib,subprocess,tempfile,urllib.request,shutil,time
root=pathlib.Path('/opt/codex-agent-integration/checks')
run=pathlib.Path(tempfile.mkdtemp(prefix='reliability-live-',dir=root))
def sql(query):
 return subprocess.check_output(['docker','exec','-e','PGOPTIONS=-c statement_timeout=10000 -c default_transaction_read_only=on','codex-agent-postgres','psql','-X','-q','-At','-v','ON_ERROR_STOP=1','-U','postgres','-d','codex_agent','-c',query],text=True).strip()
registry=pathlib.Path('/var/lib/codex-agent-release/active-runtime.json')
service=json.loads(registry.read_text())['active']['unit'] if registry.exists() else 'codex-agent.service'
assert service=='codex-agent.service' or service.startswith('codex-agent-runtime-') and service.endswith('.service') and '/' not in service
pid=subprocess.check_output(['systemctl','show',service,'--property=MainPID','--value'],text=True).strip()
runtime_env=dict(x.split(b'=',1) for x in pathlib.Path('/proc/'+pid+'/environ').read_bytes().split(b'\0') if b'=' in x)
encrypted=json.loads(sql("SELECT value FROM settings WHERE key='admin_key'"))['ciphertext']
value={'Key':runtime_env[b'CREDENTIAL_ENCRYPTION_KEY'].decode(),'KeyID':runtime_env.get(b'CREDENTIAL_ENCRYPTION_KEY_ID',b'primary').decode(),'Ciphertext':encrypted}
key=json.loads(subprocess.check_output(['/opt/codex-agent-integration/checks/decrypt-helper-5ca8b78cde0c9bc4'],input=json.dumps(value).encode()))
http=urllib.request.build_opener(urllib.request.ProxyHandler({}))
def api(path,body=None):
 req=urllib.request.Request('http://127.0.0.1:18879'+path,headers={'Authorization':'Bearer '+key,'Content-Type':'application/json'},data=json.dumps(body).encode() if body is not None else None)
 with http.open(req,timeout=25) as res:return json.load(res)
try:
 rows=[x for x in api('/api/accounts?limit=100')['accounts'] if x.get('enabled')][:2]
 assert rows,'No enabled authorized account available'
 accounts=[]
 for i,row in enumerate(rows):
  c=api('/api/accounts/export/credentials',{'items':[{'id':row['id'],'version':row['version']}]})['accounts'][0]['credentials']
  if isinstance(c,str):c=json.loads(c)
  accounts.append({'alias':'test'+str(i+1),'access_token':c['access_token'],'account_id':c.get('account_id') or row.get('account_id','')})
 private=run/'accounts.json'
 fd=os.open(private,os.O_WRONLY|os.O_CREAT|os.O_EXCL,0o600)
 with os.fdopen(fd,'w') as f:json.dump({'accounts':accounts},f)
 env={k:v for k,v in os.environ.items() if k in ['PATH','LANG','LC_ALL','TZ']}
 env.update(BPS_LIVE_ACCOUNTS_FILE=str(private),BPS_LIVE_CODEX_BINARY='/opt/codex-agent-integration/checks/quality-20260924/official-bundle/bin/codex',BPS_LIVE_WORK_ROOT=str(run),BPS_LIVE_EVIDENCE_FILE=str(run/'evidence.json'))
 for name in ['CODEX_NATIVE_HTTP_HELPER','CODEX_NATIVE_HTTP_SHA256','CODEX_NATIVE_WEBSOCKET','CODEX_NATIVE_HTTP_LIFECYCLE']:
  if name.encode() in runtime_env:env[name]=runtime_env[name.encode()].decode()
 with (run/'stderr.log').open('w') as errorlog:
  process=subprocess.Popen([TEST_BINARY,'-test.run',TEST_PATTERN,'-test.v','-test.timeout=30m'],cwd=run,env=env,stdout=subprocess.PIPE,stderr=errorlog,text=True)
  for line in process.stdout:print(line,end='',flush=True)
  code=process.wait()
 print(json.dumps({'kind':'exit','code':code,'stderr_bytes':(run/'stderr.log').stat().st_size}),flush=True)
 if (run/'evidence.json').exists():print(json.dumps({'kind':'evidence','data':json.loads((run/'evidence.json').read_text())}),flush=True)
 raise SystemExit(code)
finally:
 assert run.parent.resolve()==root.resolve() and run.name.startswith('reliability-live-')
 shutil.rmtree(run)
'''


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--run',default='^TestOfficialLiveRuntime$')
    args=parser.parse_args()
    output=ROOT/'.local/reliability-modes/live'/datetime.now(timezone.utc).strftime('%Y%m%dT%H%M%S.%fZ')
    output.mkdir(parents=True)
    before={p.relative_to(ROOT).as_posix():hashlib.sha256(p.read_bytes()).hexdigest() for p in backend_source_paths(ROOT)}
    (output/'source.json').write_text(json.dumps(before,indent=2),encoding='utf-8')
    binary=output/'runtime.test'
    subprocess.run(['go','test','-c','-o',str(binary),'./internal/server'],cwd=ROOT,env={**os.environ,'GOOS':'linux','GOARCH':'amd64','CGO_ENABLED':'0'},check=True)
    changed=[name for name,digest in before.items() if hashlib.sha256((ROOT/name).read_bytes()).hexdigest()!=digest]
    if changed:raise RuntimeError('Source changed during build: '+str(changed))
    digest=hashlib.sha256(binary.read_bytes()).hexdigest();archive=output/'runtime.test.gz'
    archive.write_bytes(gzip.compress(binary.read_bytes(),compresslevel=1,mtime=0))
    remote='/opt/codex-agent-integration/checks/reliability-'+digest[:16]+'-'+output.name+'.test'
    c=connect()
    try:
        with c.open_sftp() as sftp:sftp.get_channel().settimeout(90);sftp.put(str(archive),remote+'.gz')
        install="import gzip,pathlib,hashlib;p=pathlib.Path("+repr(remote)+");v=gzip.decompress(pathlib.Path(str(p)+'.gz').read_bytes());assert hashlib.sha256(v).hexdigest()=="+repr(digest)+";p.write_bytes(v);p.chmod(0o700)"
        _,out,err=c.exec_command('python3 -B -c '+shlex.quote(install),timeout=60)
        if out.channel.recv_exit_status():raise RuntimeError('Artifact verification failed')
        script='TEST_BINARY='+repr(remote)+'\nTEST_PATTERN='+repr(args.run)+'\n'+REMOTE
        (output/'remote.py').write_text(script,encoding='utf-8')
        stdin,out,err=c.exec_command('python3 -B -',timeout=1900);stdin.write(script);stdin.channel.shutdown_write()
        with (output/'live.log').open('w',encoding='utf-8') as log:
            for line in out:log.write(line);log.flush();print(line,end='',flush=True)
        code=out.channel.recv_exit_status();error=err.read()
        result={'exit_code':code,'stderr_bytes':len(error),'binary_sha256':digest,'run':args.run,'output':str(output)}
        (output/'result.json').write_text(json.dumps(result,indent=2),encoding='utf-8');print(json.dumps(result),flush=True)
        if code:raise SystemExit(code)
    finally:c.close()


if __name__=='__main__':main()
