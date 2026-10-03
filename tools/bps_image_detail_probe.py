"""Run two synthetic image-schema probes, with credentials remaining server-side."""
from datetime import datetime, timezone
import argparse
import gzip
import hashlib
import json
import os
import shlex
import subprocess
from integration_env import ROOT, connect
from bps_minimal_policy_probe import REMOTE

def main():
    parser=argparse.ArgumentParser(description=__doc__);parser.add_argument('--control',action='store_true');parser.add_argument('--attachment',action='store_true');args=parser.parse_args()
    folder=ROOT/'.local/image-detail-probe'/datetime.now(timezone.utc).strftime('%Y%m%dT%H%M%SZ')
    folder.mkdir(parents=True)
    source=ROOT/'tools/bps_image_detail_probe.go'
    binary=folder/'probe'
    subprocess.run(['go','build','-trimpath','-ldflags=-s -w','-o',str(binary),str(source)],env={**os.environ,'GOOS':'linux','GOARCH':'amd64','CGO_ENABLED':'0'},check=True)
    digest=hashlib.sha256(binary.read_bytes()).hexdigest()
    archive=folder/'probe.gz';archive.write_bytes(gzip.compress(binary.read_bytes(),compresslevel=1,mtime=0))
    remote='/opt/codex-agent-integration/checks/bps-image-detail-'+digest[:16]
    client=connect()
    try:
        with client.open_sftp() as s:s.put(str(archive),remote+'.gz')
        install="import pathlib,gzip,hashlib;p=pathlib.Path("+repr(remote)+");v=gzip.decompress(pathlib.Path(str(p)+'.gz').read_bytes());assert hashlib.sha256(v).hexdigest()=="+repr(digest)+";p.write_bytes(v);p.chmod(0o700)"
        _,out,err=client.exec_command('python3 -B -c '+shlex.quote(install),timeout=40)
        if out.channel.recv_exit_status():raise RuntimeError('probe_install_failed')
        script=REMOTE.replace("'tools_version':version}).encode()", "'tools_version':version,'image_control':"+repr(args.control)+",'image_attachment':"+repr(args.attachment)+"}).encode()")
        assert "'image_control':" in script
        code='BINARY='+repr(remote)+'\n'+script
        stdin,out,err=client.exec_command('python3 -B -',timeout=300);stdin.write(code);stdin.channel.shutdown_write()
        with (folder/'evidence.jsonl').open('w',encoding='utf-8') as log:
            for line in out:log.write(line);log.flush();print(line,end='',flush=True)
        status=out.channel.recv_exit_status();stderr=err.read()
        report={'exit_code':status,'stderr_bytes':len(stderr),'binary_sha256':digest,'source_sha256':hashlib.sha256(source.read_bytes()).hexdigest(),'max_generation_requests':2,'customer_history_replayed':False}
        (folder/'result.json').write_text(json.dumps(report,indent=2),encoding='utf-8');print(json.dumps(report))
        if status:raise SystemExit(status)
    finally:client.close()

if __name__=='__main__':main()
