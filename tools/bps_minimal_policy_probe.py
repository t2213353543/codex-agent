"""Bounded BPS policy diagnostic with server-only credential access (max 3 generations)."""
from datetime import datetime, timezone
import gzip
import hashlib
import json
import os
from pathlib import Path
import shlex
import subprocess

from integration_env import ROOT, connect

REMOTE = r'''
import hashlib,json,os,pathlib,subprocess,urllib.request
def sql(query):
 return subprocess.check_output(['docker','exec','-e','PGOPTIONS=-c statement_timeout=10000 -c default_transaction_read_only=on','codex-agent-postgres','psql','-X','-q','-At','-v','ON_ERROR_STOP=1','-U','postgres','-d','codex_agent','-c',query],text=True).strip()
pid=subprocess.check_output(['systemctl','show','codex-agent','--property=MainPID','--value'],text=True).strip()
runtime_env=dict(x.split(b'=',1) for x in pathlib.Path('/proc/'+pid+'/environ').read_bytes().split(b'\0') if b'=' in x)
encrypted=json.loads(sql("SELECT value FROM settings WHERE key='admin_key'"))['ciphertext']
value={'Key':runtime_env[b'CREDENTIAL_ENCRYPTION_KEY'].decode(),'KeyID':runtime_env.get(b'CREDENTIAL_ENCRYPTION_KEY_ID',b'primary').decode(),'Ciphertext':encrypted}
key=json.loads(subprocess.check_output(['/opt/codex-agent-integration/checks/decrypt-helper-5ca8b78cde0c9bc4'],input=json.dumps(value).encode()))
http=urllib.request.build_opener(urllib.request.ProxyHandler({}))
def api(path,body=None):
 req=urllib.request.Request('http://127.0.0.1:18879'+path,headers={'Authorization':'Bearer '+key,'Content-Type':'application/json'},data=json.dumps(body).encode() if body is not None else None)
 with http.open(req,timeout=25) as res:return json.load(res)
rows=[x for x in api('/api/accounts?limit=100')['accounts'] if x.get('enabled')][:2]
assert len(rows)==2,'Two current enabled accounts required'
accounts=[]
for i,row in enumerate(rows):
 assert not row.get('proxy_id'),'Account proxy requires a matched transport diagnostic'
 c=api('/api/accounts/export/credentials',{'items':[{'id':row['id'],'version':row['version']}]})['accounts'][0]['credentials']
 if isinstance(c,str):c=json.loads(c)
 accounts.append({'alias':'test'+str(i+1),'access_token':c['access_token'],'account_id':c.get('account_id') or row.get('account_id',''),'metadata_version':row.get('version')})
version=runtime_env.get(b'BPS_TOOLS_VERSION',b'tools-powerpoint-core-2026-08-10-2b886486').decode()
for name in ['HTTP_PROXY','HTTPS_PROXY','ALL_PROXY','http_proxy','https_proxy','all_proxy']:
 assert not runtime_env.get(name.encode()),'Production transport uses environment proxy; match before testing'
print(json.dumps({'kind':'runtime','binary_sha256':hashlib.sha256(pathlib.Path('/proc/'+pid+'/exe').read_bytes()).hexdigest(),'tools_version':version,'account_count':len(accounts),'transport':'Go DefaultTransport.Clone DisableCompression=true direct','prompt':'synthetic_minimal_only','production_mutations':False}),flush=True)
result=subprocess.run([BINARY],input=json.dumps({'accounts':accounts,'tools_version':version}).encode(),stdout=subprocess.PIPE,stderr=subprocess.PIPE,timeout=600,env={'PATH':'/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin'})
print(result.stdout.decode(),end='',flush=True)
print(json.dumps({'kind':'diagnostic_exit','code':result.returncode,'stderr_bytes':len(result.stderr),'generation_success_is_not_implied':True}),flush=True)
raise SystemExit(result.returncode)
'''

def main():
    output=ROOT/'.local/latest-operations/bps-minimal-policy'/datetime.now(timezone.utc).strftime('%Y%m%dT%H%M%S.%fZ')
    output.mkdir(parents=True)
    source=(ROOT/'tools/bps_minimal_policy_probe.go').read_bytes()
    (output/'probe-source.go').write_bytes(source)
    binary=output/'probe'
    subprocess.run(['go','build','-trimpath','-ldflags=-s -w','-o',str(binary),'tools/bps_minimal_policy_probe.go'],cwd=ROOT,env={**os.environ,'GOOS':'linux','GOARCH':'amd64','CGO_ENABLED':'0'},check=True)
    digest=hashlib.sha256(binary.read_bytes()).hexdigest()
    archive=output/'probe.gz';archive.write_bytes(gzip.compress(binary.read_bytes(),compresslevel=1,mtime=0))
    remote='/opt/codex-agent-integration/checks/bps-minimal-policy-'+digest[:16]
    client=connect()
    try:
        with client.open_sftp() as sftp:sftp.get_channel().settimeout(90);sftp.put(str(archive),remote+'.gz')
        install="import gzip,pathlib,hashlib;p=pathlib.Path("+repr(remote)+");v=gzip.decompress(pathlib.Path(str(p)+'.gz').read_bytes());assert hashlib.sha256(v).hexdigest()=="+repr(digest)+";p.write_bytes(v);p.chmod(0o700)"
        _,out,err=client.exec_command('python3 -B -c '+shlex.quote(install),timeout=60)
        if out.channel.recv_exit_status():raise RuntimeError('Artifact verification failed')
        script='BINARY='+repr(remote)+'\n'+REMOTE
        (output/'remote.py').write_text(script,encoding='utf-8')
        stdin,out,err=client.exec_command('python3 -B -',timeout=650);stdin.write(script);stdin.channel.shutdown_write()
        with (output/'evidence.jsonl').open('w',encoding='utf-8') as log:
            for line in out:log.write(line);log.flush();print(line,end='',flush=True)
        code=out.channel.recv_exit_status();error=err.read()
        result={'diagnostic_exit':code,'stderr_bytes':len(error),'binary_sha256':digest,'source_sha256':hashlib.sha256(source).hexdigest(),'output':str(output)}
        (output/'result.json').write_text(json.dumps(result,indent=2),encoding='utf-8');print(json.dumps(result),flush=True)
        if code:raise SystemExit(code)
    finally:client.close()

if __name__=='__main__':main()
