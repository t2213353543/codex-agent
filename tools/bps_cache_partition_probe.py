"""Run up to eight alternating synthetic cache-key requests; secrets stay on server."""
from datetime import datetime, timezone
import gzip
import hashlib
import json
import os
import shlex
import subprocess
from integration_env import ROOT, connect
from bps_minimal_policy_probe import REMOTE

def main():
    folder=ROOT/'.local/bps-cache-partition-probe'/datetime.now(timezone.utc).strftime('%Y%m%dT%H%M%SZ')
    folder.mkdir(parents=True)
    source=ROOT/'tools/bps_cache_partition_probe.go';binary=folder/'probe'
    subprocess.run(['go','build','-trimpath','-ldflags=-s -w','-o',str(binary),str(source)],cwd=ROOT,env={**os.environ,'GOOS':'linux','GOARCH':'amd64','CGO_ENABLED':'0'},check=True)
    digest=hashlib.sha256(binary.read_bytes()).hexdigest()
    archive=folder/'probe.gz';archive.write_bytes(gzip.compress(binary.read_bytes(),compresslevel=1,mtime=0))
    remote='/opt/codex-agent-integration/checks/bps-cache-partition-'+digest[:16]
    client=connect()
    try:
        with client.open_sftp() as sftp:
            sftp.get_channel().settimeout(90);sftp.put(str(archive),remote+'.gz');sftp.chmod(remote+'.gz',0o600)
        install="import pathlib,gzip,hashlib;p=pathlib.Path("+repr(remote)+");v=gzip.decompress(pathlib.Path(str(p)+'.gz').read_bytes());assert hashlib.sha256(v).hexdigest()=="+repr(digest)+";p.write_bytes(v);p.chmod(0o700)"
        _,out,err=client.exec_command('python3 -B -c '+shlex.quote(install),timeout=40)
        if out.channel.recv_exit_status():raise RuntimeError('probe_install_failed')
        script=REMOTE.replace("[:2]","[:1]").replace("assert len(rows)==2,'Two current enabled accounts required'","assert len(rows)==1,'One current enabled account required'")
        script=script.replace("'prompt':'synthetic_minimal_only'","'prompt':'synthetic_cache_partition_only'")
        start=script.index('result=subprocess.run([BINARY]')
        script=script[:start]+'''
process=subprocess.Popen([BINARY],stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=subprocess.PIPE,env={'PATH':'/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin'})
process.stdin.write(json.dumps({'accounts':accounts,'tools_version':version}).encode());process.stdin.close()
for line in iter(process.stdout.readline,b''):
 print(line.decode(),end='',flush=True)
status=process.wait(timeout=650);stderr=process.stderr.read()
print(json.dumps({'kind':'diagnostic_exit','code':status,'stderr_bytes':len(stderr),'generation_success_is_not_implied':True}),flush=True)
raise SystemExit(status)
'''
        code='BINARY='+repr(remote)+'\n'+script
        stdin,out,err=client.exec_command('python3 -B -',timeout=700);stdin.write(code);stdin.channel.shutdown_write()
        with (folder/'evidence.jsonl').open('w',encoding='utf-8') as log:
            for line in out:log.write(line);log.flush();print(line,end='',flush=True)
        status=out.channel.recv_exit_status();stderr=err.read()
        report={'exit_code':status,'stderr_bytes':len(stderr),'binary_sha256':digest,'source_sha256':hashlib.sha256(source.read_bytes()).hexdigest(),'max_generation_requests':8,'customer_history_replayed':False,'production_request_log_writes':False,'output':str(folder)}
        (folder/'result.json').write_text(json.dumps(report,indent=2),encoding='utf-8');print(json.dumps(report))
        if status:raise SystemExit(status)
    finally:client.close()

if __name__=='__main__':main()
