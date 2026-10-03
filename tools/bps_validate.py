"""Build and execute the PowerPoint/Codex live acceptance on the authorized host.

Access tokens stay in a private server-side temporary file and are removed after
the test. This command neither deploys the gateway nor changes production data.
"""
from __future__ import annotations
import argparse
from datetime import datetime, timezone
import hashlib
import gzip
import json
import os
from pathlib import Path
import shlex
import subprocess

from integration_env import ROOT, connect
from project_layout import backend_source_paths

REMOTE = r'''
import json,os,pathlib,subprocess,tempfile,urllib.request,shutil,threading
base=pathlib.Path('/opt/codex-agent-integration/checks').resolve()
run=pathlib.Path(tempfile.mkdtemp(prefix='powerpoint-live-',dir=base)).resolve()
assert run.parent==base
private=run/'accounts.json'
evidence=run/'evidence.json'
key=pathlib.Path('/var/lib/codex-agent/client-key').read_text().strip()
http=urllib.request.build_opener(urllib.request.ProxyHandler({}))
def api(path,body=None):
 req=urllib.request.Request('http://127.0.0.1:18879'+path,headers={'Authorization':'Bearer '+key,'Content-Type':'application/json'},data=json.dumps(body).encode() if body is not None else None)
 with http.open(req,timeout=20) as response:return json.load(response)
try:
 listing=api('/api/accounts?limit=100')
 rows=[x for x in listing['accounts'] if x.get('enabled') is True]
 if listing.get('has_more') or len(rows)!=2:raise RuntimeError('expected exactly two enabled authorized test accounts')
 accounts=[]
 for index,row in enumerate(sorted(rows,key=lambda x:x['id'])):
  exported=api('/api/accounts/export/credentials',{'items':[{'id':row['id'],'version':row['version']}]})['accounts'][0]['credentials']
  if isinstance(exported,str):exported=json.loads(exported)
  accounts.append({'alias':'test'+str(index+1),'access_token':exported['access_token'],'account_id':exported.get('account_id') or row.get('account_id','')})
 fd=os.open(private,os.O_WRONLY|os.O_CREAT|os.O_EXCL,0o600)
 with os.fdopen(fd,'w') as output:json.dump({'accounts':accounts},output)
 env={k:v for k,v in os.environ.items() if k in ['PATH','LANG','LC_ALL','TZ']}
 env.update(BPS_LIVE_ACCOUNTS_FILE=str(private),BPS_LIVE_CODEX_BINARY='/opt/codex-agent-integration/checks/quality-20260924/official-bundle/bin/codex',BPS_LIVE_WORK_ROOT=str(run),BPS_LIVE_EVIDENCE_FILE=str(evidence))
 with (run/'stderr.log').open('w') as error_log:
  process=subprocess.Popen([TEST_BINARY,'-test.run',TEST_PATTERN,'-test.v','-test.timeout=35m'],env=env,cwd=run,text=True,stdout=subprocess.PIPE,stderr=error_log)
  timer=threading.Timer(2150,process.kill);timer.start()
  try:
   for line in process.stdout:print(line,end='',flush=True)
   exit_code=process.wait()
  finally:timer.cancel()
 print(json.dumps({'kind':'test_exit','code':exit_code,'stderr_present':(run/'stderr.log').stat().st_size>0}),flush=True)
 if evidence.exists():print(json.dumps({'kind':'evidence','data':json.loads(evidence.read_text())}),flush=True)
 if exit_code:raise SystemExit(exit_code)
finally:
 if run.parent==base and run.name.startswith('powerpoint-live-'):shutil.rmtree(run)
'''

def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--skip-build',action='store_true')
    parser.add_argument('--suite',choices=['all','core','websocket','search'],default='all')
    args=parser.parse_args()
    run_id=datetime.now(timezone.utc).strftime('%Y%m%dT%H%M%SZ')
    output=ROOT/'.local/powerpoint-migration'/run_id
    output.mkdir(parents=True,exist_ok=False)
    binary=ROOT/'.local/powerpoint-migration/bps-live.test'
    files={p.relative_to(ROOT).as_posix():hashlib.sha256(p.read_bytes()).hexdigest() for p in backend_source_paths(ROOT)}
    (output/'source.json').write_text(json.dumps({'files':files,'sha256':hashlib.sha256(json.dumps(files,sort_keys=True).encode()).hexdigest()},indent=2),encoding='utf-8')
    if not args.skip_build:
        subprocess.run(['go','test','-c','-o',str(binary),'./internal/server'],cwd=ROOT,
            env={**os.environ,'GOOS':'linux','GOARCH':'amd64','CGO_ENABLED':'0'},check=True)
    checksum=hashlib.sha256(binary.read_bytes()).hexdigest()
    if any(hashlib.sha256((ROOT/name).read_bytes()).hexdigest()!=value for name,value in files.items()):
        raise RuntimeError('Backend sources changed while building the live test')
    compressed=gzip.compress(binary.read_bytes(),compresslevel=1,mtime=0)
    archive=output/'bps-live.test.gz';archive.write_bytes(compressed)
    client=connect()
    destination='/opt/codex-agent-integration/checks/bps-live-'+checksum[:16]+'.test'
    try:
        with client.open_sftp() as sftp:
            sftp.get_channel().settimeout(60)
            sftp.put(str(archive),destination+'.gz',confirm=True)
        command="import gzip,hashlib,pathlib; p=pathlib.Path("+repr(destination)+"); data=gzip.decompress(pathlib.Path(str(p)+'.gz').read_bytes()); assert hashlib.sha256(data).hexdigest()=="+repr(checksum)+"; p.write_bytes(data); p.chmod(0o700); print('Verified test binary uploaded',flush=True)"
        _,check_out,check_err=client.exec_command('python3 -c '+shlex.quote(command),timeout=60)
        if check_out.channel.recv_exit_status():raise RuntimeError('Remote test binary verification failed')
        print(check_out.read().decode().strip(),flush=True)
        stdin,stdout,stderr=client.exec_command('python3 -',timeout=2200)
        pattern={'all':'^TestBPSLiveCodex(Integration|WebSocket)$','core':'^TestBPSLiveCodexIntegration$','websocket':'^TestBPSLiveCodexWebSocket$','search':'^TestBPSLiveCodexIntegration$/^search$'}[args.suite]
        stdin.write('TEST_BINARY='+repr(destination)+'\nTEST_PATTERN='+repr(pattern)+'\n'+REMOTE)
        stdin.channel.shutdown_write()
        with (output/'live.log').open('w',encoding='utf-8') as log:
            for line in stdout:
                log.write(line);log.flush();print(line,end='',flush=True)
        status=stdout.channel.recv_exit_status()
        # Avoid printing an unexpected upstream exception that may contain a URL.
        error=stderr.read()
        summary={'binary_sha256':checksum,'source_sha256':hashlib.sha256(json.dumps(files,sort_keys=True).encode()).hexdigest(),'suite':args.suite,'exit_code':status,'remote_stderr_bytes':len(error),'log':'live.log'}
        (output/'result.json').write_text(json.dumps(summary,indent=2),encoding='utf-8')
        print(str(output/'result.json'))
        if status:raise SystemExit(status)
    finally:
        client.close()

if __name__=='__main__':main()
