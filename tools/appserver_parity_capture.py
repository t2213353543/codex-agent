"""Capture isolated App Server and CLI-mode egress, keeping raw data server-side."""
import argparse
from datetime import datetime,timezone
import gzip
import hashlib
import json
import os
import re
from pathlib import Path
import shlex
import subprocess
from integration_env import ROOT,connect
from project_layout import backend_source_paths

REMOTE='/opt/codex-agent-integration/appserver-parity'
IMAGE='mitmproxy/mitmproxy:12.2.3'

def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--pilot',action='store_true')
    parser.add_argument('--lightweight',action='store_true',help='Enable the explicitly selected lightweight official_codex_cli capture adapter')
    parser.add_argument('--route',action='append',choices=['official_app_server','official_codex_cli','native_app_server','native_codex_exec'])
    parser.add_argument('--model',default='gpt-6-astra')
    parser.add_argument('--effort',default='xhigh')
    parser.add_argument('--native-helper',help='Explicit candidate helper path on the private test server')
    parser.add_argument('--native-helper-sha256',help='Expected SHA256 of the candidate helper')
    parser.add_argument('--reuse-capture-build',type=Path,help='Reuse an explicitly recorded capture test build; retain its original source manifest')
    args=parser.parse_args()
    if bool(args.native_helper)!=bool(args.native_helper_sha256):parser.error('candidate helper path and SHA256 must be provided together')
    if args.native_helper and (not args.native_helper.startswith('/opt/codex-agent-integration/') or not re.fullmatch('[a-f0-9]{64}',args.native_helper_sha256)):
        parser.error('candidate helper must be in the integration directory with a SHA256')
    stamp=datetime.now(timezone.utc).strftime('%Y%m%dT%H%M%S.%fZ')
    output=ROOT/'.local/appserver-parity'/stamp;output.mkdir(parents=True)
    if args.reuse_capture_build:
        previous=args.reuse_capture_build.resolve()
        assert previous.is_relative_to(ROOT/'.local/appserver-parity')
        sources=json.loads((previous/'source.json').read_text())
        prior_result=json.loads((previous/'result.json').read_text())
        digest=prior_result['binary_sha256']
        archive=previous/'capture.test.gz'
        assert hashlib.sha256(gzip.decompress(archive.read_bytes())).hexdigest()==digest
    else:
        sources={p.relative_to(ROOT).as_posix():hashlib.sha256(p.read_bytes()).hexdigest() for p in backend_source_paths(ROOT)}
        binary=output/'capture.test'
        subprocess.run(['go','test','-c','-o',str(binary),'./internal/server'],cwd=ROOT,env={**os.environ,'GOOS':'linux','GOARCH':'amd64','CGO_ENABLED':'0'},check=True)
        assert all(hashlib.sha256((ROOT/k).read_bytes()).hexdigest()==v for k,v in sources.items()),'sources_changed_during_build'
        digest=hashlib.sha256(binary.read_bytes()).hexdigest();archive=output/'capture.test.gz';archive.write_bytes(gzip.compress(binary.read_bytes(),compresslevel=1,mtime=0))
    (output/'source.json').write_text(json.dumps(sources,indent=2))
    addon=ROOT/'integration/appserver_parity/capture_proxy.py';addon_sha=hashlib.sha256(addon.read_bytes()).hexdigest()
    native=ROOT/'tools/appserver_parity_native_driver.py';native_sha=hashlib.sha256(native.read_bytes()).hexdigest()
    identity_helper=ROOT/'tools/capture_appserver_identity.py';identity_sha=hashlib.sha256(identity_helper.read_bytes()).hexdigest()
    artifact=REMOTE+'/artifacts/'+digest+'.test';addon_path=REMOTE+'/artifacts/capture-'+addon_sha+'.py'
    native_path=REMOTE+'/artifacts/native-driver-'+native_sha+'.py'
    identity_path=REMOTE+'/artifacts/identity-capture-'+identity_sha+'.py'
    client=connect()
    try:
        setup="import pathlib;root=pathlib.Path("+repr(REMOTE)+");root.mkdir(mode=0o700,exist_ok=True);(root/'artifacts').mkdir(mode=0o700,exist_ok=True);(root/'runs').mkdir(mode=0o700,exist_ok=True)"
        _,o,e=client.exec_command('python3 -B -c '+shlex.quote(setup));assert o.channel.recv_exit_status()==0;e.read()
        with client.open_sftp() as sftp:
            sftp.get_channel().settimeout(120)
            if not args.reuse_capture_build:sftp.put(str(archive),artifact+'.gz')
            for local,remote in [(addon,addon_path),(native,native_path),(identity_helper,identity_path)]:
                try:sftp.stat(remote)
                except FileNotFoundError:sftp.put(str(local),remote)
        install="import pathlib,gzip,hashlib;p=pathlib.Path("+repr(artifact)+");raw=p.read_bytes() if p.exists() else gzip.decompress(pathlib.Path(str(p)+'.gz').read_bytes());assert hashlib.sha256(raw).hexdigest()=="+repr(digest)+"\nif not p.exists(): p.write_bytes(raw);p.chmod(0o700)\n"
        install+='\n'.join("p=pathlib.Path("+repr(path)+");assert hashlib.sha256(p.read_bytes()).hexdigest()=="+repr(digest_value)+";p.chmod(0o600)" for path,digest_value in [(addon_path,addon_sha),(native_path,native_sha),(identity_path,identity_sha)])
        _,o,e=client.exec_command('python3 -B -c '+shlex.quote(install));assert o.channel.recv_exit_status()==0;e.read()
        config={'RUN_DIR':REMOTE+'/runs/'+stamp,'TEST_BINARY':artifact,'ADDON_PATH':addon_path,'NATIVE_DRIVER':native_path,'IDENTITY_CAPTURE_HELPER':identity_path,'IMAGE':IMAGE,'CODEX_BINARY':'/opt/codex-agent-integration/checks/quality-20260924/official-bundle/bin/codex','ROUTES':args.route or ['official_app_server','official_codex_cli'],'MODEL':args.model,'EFFORT':args.effort,'PILOT':args.pilot,'LIGHTWEIGHT':args.lightweight}
        config.update(NATIVE_HELPER_OVERRIDE=args.native_helper,NATIVE_HELPER_SHA256_OVERRIDE=args.native_helper_sha256)
        script='\n'.join(k+'='+repr(v) for k,v in config.items())+'\n'+(ROOT/'tools/appserver_parity_remote.py').read_text(encoding='utf-8')
        (output/'remote.py').write_text(script,encoding='utf-8')
        i,o,e=client.exec_command('python3 -B -',timeout=2000);i.write(script);i.channel.shutdown_write()
        with (output/'events.jsonl').open('w',encoding='utf-8') as log:
            for line in o:log.write(line);log.flush();print(line,end='',flush=True)
        code=o.channel.recv_exit_status();error=e.read()
        result={'exit_code':code,'stderr_bytes':len(error),'binary_sha256':digest,'addon_sha256':addon_sha,'native_driver_sha256':native_sha,'identity_capture_sha256':identity_sha,'native_helper_override_sha256':args.native_helper_sha256,'output':str(output),'private_remote':config['RUN_DIR']}
        (output/'result.json').write_text(json.dumps(result,indent=2));print(json.dumps(result),flush=True)
        return code
    finally:client.close()

if __name__=='__main__':raise SystemExit(main())
