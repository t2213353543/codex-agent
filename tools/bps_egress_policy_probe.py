"""Compare one fixed synthetic request over two authorized server egresses.

Exactly one generation on master and one on app214. Account credentials are
loaded on master and passed directly to peer over SSH stdin, never to this client.
"""
from datetime import datetime, timezone
import gzip
import hashlib
import importlib.util
import json
import os
import shlex
import subprocess

from integration_env import ROOT, connect

REMOTE = r'''
import base64,hashlib,json,os,pathlib,shlex,subprocess,tempfile,urllib.request,uuid
config=json.load(__import__('sys').stdin)
assert config['peer_host']=='43.225.196.214' and config['peer_port']==13965
root=pathlib.Path('/opt/codex-agent-integration/checks')
run=pathlib.Path(tempfile.mkdtemp(prefix='bps-egress-policy-',dir=root))
askpass=run/'askpass.py'
askpass.write_text('#!/usr/bin/python3\nimport os\nprint(os.environ["BPS_DIAG_PEER_PASSWORD"])\n')
askpass.chmod(0o700)
env={'PATH':'/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin','DISPLAY':':0','SSH_ASKPASS':str(askpass),'SSH_ASKPASS_REQUIRE':'force','BPS_DIAG_PEER_PASSWORD':config.pop('peer_password')}
ssh=['ssh','-p',str(config['peer_port']),'-o','ConnectTimeout=10','-o','StrictHostKeyChecking=yes','-o','NumberOfPasswordPrompts=1','root@'+config['peer_host']]
peer_binary='/tmp/codex-agent-bps-egress-'+config['binary_sha256'][:16]
def sshrun(command,input=None,timeout=40):
 p=subprocess.run(ssh+[command],input=input,stdout=subprocess.PIPE,stderr=subprocess.PIPE,env=env,timeout=timeout)
 if p.returncode:raise RuntimeError('peer diagnostic operation failed: '+str(p.returncode)+'; stderr bytes='+str(len(p.stderr)))
 return p.stdout
try:
 architecture=sshrun('uname -m').decode().strip()
 assert architecture=='x86_64','unsupported peer architecture'
 transfer="import sys,pathlib,hashlib;p=pathlib.Path("+repr(peer_binary)+");b=sys.stdin.buffer.read();assert hashlib.sha256(b).hexdigest()=="+repr(config['binary_sha256'])+";p.write_bytes(b);p.chmod(0o700)"
 binary=pathlib.Path(config['binary'])
 assert hashlib.sha256(binary.read_bytes()).hexdigest()==config['binary_sha256']
 sshrun('python3 -B -c '+shlex.quote(transfer),input=binary.read_bytes(),timeout=60)
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
 rows=[x for x in api('/api/accounts?limit=100')['accounts'] if x.get('enabled')]
 assert rows,'current enabled account required'
 row=rows[0];assert not row.get('proxy_id'),'Account proxy needs matched diagnostic'
 c=api('/api/accounts/export/credentials',{'items':[{'id':row['id'],'version':row['version']}]})['accounts'][0]['credentials']
 if isinstance(c,str):c=json.loads(c)
 account={'alias':'test1','access_token':c['access_token'],'account_id':c.get('account_id') or row.get('account_id',''),'metadata_version':row.get('version')}
 version=runtime_env.get(b'BPS_TOOLS_VERSION',b'tools-powerpoint-core-2026-08-10-2b886486').decode()
 body=json.dumps({'input':[{'type':'message','role':'user','content':[{'type':'input_text','text':'Reply exactly OK.'}]}],'model':'gpt-6-astra','reasoning_effort':'xhigh','stream':True,'store':False,'model_selection':'explicit','metadata':{'task_id':str(uuid.uuid4()),'turn_id':str(uuid.uuid4()),'bps_tools_version_id':version,'agent_iteration':'1'}},separators=(',',':'),sort_keys=True).encode()
 fixture=json.dumps({'accounts':[account],'tools_version':version,'mode':'egress_once','body_base64':base64.b64encode(body).decode()}).encode()
 trace="import urllib.request,json;h=urllib.request.build_opener(urllib.request.ProxyHandler({}));r=h.open('https://www.cloudflare.com/cdn-cgi/trace',timeout=20).read().decode();d=dict(x.split('=',1) for x in r.splitlines() if '=' in x);print(json.dumps({k:d.get(k) for k in ['ip','colo','loc']}))"
 master_trace=json.loads(subprocess.check_output(['python3','-B','-c',trace],timeout=25))
 peer_trace=json.loads(sshrun('python3 -B -c '+shlex.quote(trace),timeout=30))
 assert master_trace.get('ip') and peer_trace.get('ip') and master_trace['ip']!=peer_trace['ip'],'different actual egress IPs required'
 print(json.dumps({'kind':'egress_boundary','master':master_trace,'app214':peer_trace,'binary_sha256':config['binary_sha256'],'same_request_body_sha256':hashlib.sha256(body).hexdigest(),'production_mutations':False,'max_generations':2}),flush=True)
 p=subprocess.run([str(binary)],input=fixture,stdout=subprocess.PIPE,stderr=subprocess.PIPE,timeout=190,env={'PATH':env['PATH']})
 assert p.returncode==0,'master diagnostic failed'
 result=json.loads(p.stdout);result['node']='master';print(json.dumps(result),flush=True)
 result=json.loads(sshrun('env -i PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin '+shlex.quote(peer_binary),input=fixture,timeout=190));result['node']='app214';print(json.dumps(result),flush=True)
finally:
 try:
  cleanup="import pathlib;p=pathlib.Path("+repr(peer_binary)+");assert p.parent==pathlib.Path('/tmp') and p.name.startswith('codex-agent-bps-egress-');p.unlink(missing_ok=True)"
  sshrun('python3 -B -c '+shlex.quote(cleanup),timeout=20)
 finally:
  askpass.unlink(missing_ok=True);run.rmdir()
'''

def main():
    from pathlib import Path
    skill=Path(os.environ.get('CODEX_HOME','D:/ChatGPT/CodexHome'))/'skills/sub2api-production/scripts/sub2api_ssh.py'
    spec=importlib.util.spec_from_file_location('bps_egress_ssh',skill)
    ssh_module=importlib.util.module_from_spec(spec);spec.loader.exec_module(ssh_module)
    peer=ssh_module.node_meta('app214');master=ssh_module.node_meta('master')
    assert peer['host']!=master['host']
    output=ROOT/'.local/latest-operations/bps-egress-policy'/datetime.now(timezone.utc).strftime('%Y%m%dT%H%M%S.%fZ');output.mkdir(parents=True)
    source=(ROOT/'tools/bps_minimal_policy_probe.go').read_bytes()
    (output/'probe-source.go').write_bytes(source)
    binary=output/'probe'
    subprocess.run(['go','build','-trimpath','-ldflags=-s -w','-o',str(binary),'tools/bps_minimal_policy_probe.go'],cwd=ROOT,env={**os.environ,'GOOS':'linux','GOARCH':'amd64','CGO_ENABLED':'0'},check=True)
    digest=hashlib.sha256(binary.read_bytes()).hexdigest()
    archive=output/'probe.gz';archive.write_bytes(gzip.compress(binary.read_bytes(),compresslevel=1,mtime=0))
    remote='/opt/codex-agent-integration/checks/bps-egress-policy-'+digest[:16]
    client=connect()
    try:
        with client.open_sftp() as sftp:sftp.get_channel().settimeout(90);sftp.put(str(archive),remote+'.gz')
        install="import gzip,pathlib,hashlib;p=pathlib.Path("+repr(remote)+");v=gzip.decompress(pathlib.Path(str(p)+'.gz').read_bytes());assert hashlib.sha256(v).hexdigest()=="+repr(digest)+";p.write_bytes(v);p.chmod(0o700)"
        _,out,err=client.exec_command('python3 -B -c '+shlex.quote(install),timeout=60)
        if out.channel.recv_exit_status():raise RuntimeError('Artifact verification failed')
        (output/'remote.py').write_text(REMOTE,encoding='utf-8')
        config={'peer_host':peer['host'],'peer_port':peer['port'],'peer_password':ssh_module.load_password('app214'),'binary':remote,'binary_sha256':digest}
        stdin,out,err=client.exec_command('python3 -B -c '+shlex.quote(REMOTE),timeout=500)
        stdin.write(json.dumps(config));stdin.channel.shutdown_write();config.pop('peer_password')
        with (output/'evidence.jsonl').open('w',encoding='utf-8') as log:
            for line in out:log.write(line);log.flush();print(line,end='',flush=True)
        code=out.channel.recv_exit_status();error=err.read()
        result={'diagnostic_exit':code,'stderr_bytes':len(error),'binary_sha256':digest,'source_sha256':hashlib.sha256(source).hexdigest(),'output':str(output)}
        (output/'result.json').write_text(json.dumps(result,indent=2),encoding='utf-8');print(json.dumps(result),flush=True)
        if code:raise SystemExit(code)
    finally:client.close()

if __name__=='__main__':main()
