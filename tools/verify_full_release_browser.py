"""Validate deployed management pages over SSH; never requests model traffic."""
import csv
import hashlib
import io
import json
import os
from pathlib import Path
import select
import socketserver
import stat
import subprocess
import threading

from integration_env import ROOT, connect, run

OUT=ROOT/'.local/full-release-20260919'
manifest=json.loads((OUT/'package/full-release.json').read_text())
config_path=OUT/'browser-private/config.json'
config_path.parent.mkdir(mode=0o700,exist_ok=True)
if os.name=='nt':
    identity=subprocess.check_output(['whoami','/user','/fo','csv','/nh'],text=True)
    sid=next(csv.reader(io.StringIO(identity.strip())))[1]
    subprocess.run(['icacls',str(config_path.parent),'/inheritance:r','/grant:r','*'+sid+':(OI)(CI)F','*S-1-5-18:(OI)(CI)F'],check=True,capture_output=True,creationflags=subprocess.CREATE_NO_WINDOW)
client=server=None
result={'passed':False,'gateway_requests':0,'credentials_written_to_report':False}
try:
    client=connect()
    observed=run(client,'pid=$(systemctl show codex-agent.service --property MainPID --value)\nsha256sum /proc/$pid/exe').split()[0]
    assert observed==manifest['binary_sha256'],'Unexpected deployed binary'
    with client.open_sftp() as sftp:
        assert stat.S_IMODE(sftp.stat('/var/lib/codex-agent/client-key').st_mode)==0o600
        with sftp.file('/var/lib/codex-agent/client-key','rb') as handle:key=handle.read().decode().strip()
    transport=client.get_transport();transport.set_keepalive(15)
    class Handler(socketserver.BaseRequestHandler):
        def handle(self):
            channel=transport.open_channel('direct-tcpip',('127.0.0.1',18879),self.request.getpeername())
            try:
                while True:
                    ready,_,_=select.select([self.request,channel],[],[],5)
                    for source,destination in [(self.request,channel),(channel,self.request)]:
                        if source in ready:
                            data=source.recv(65536)
                            if not data:return
                            destination.sendall(data)
            finally:channel.close()
    class Forward(socketserver.ThreadingTCPServer):
        allow_reuse_address=True
        daemon_threads=True
    server=Forward(('127.0.0.1',0),Handler)
    threading.Thread(target=server.serve_forever,daemon=True).start()
    frontend_manifest=ROOT/'.local/management-parity/release-20260919/build-manifest.json'
    config={'base_url':'http://127.0.0.1:'+str(server.server_address[1]),'admin_key':key,
            'output':str(OUT/'production-browser'),'release_id':manifest['release_id'],'binary_sha256':observed,
            'frontend_manifest_path':str(frontend_manifest),'frontend_manifest_sha256':hashlib.sha256(frontend_manifest.read_bytes()).hexdigest()}
    with config_path.open('x',encoding='utf-8') as handle:json.dump(config,handle)
    process=subprocess.run(['node',str(ROOT/'integration/tests/management-release-readonly.cjs'),'--run-after-deploy',str(config_path)],cwd=ROOT,capture_output=True,text=True,encoding='utf-8',timeout=420)
    report=json.loads((OUT/'production-browser/browser-report.json').read_text())
    result.update(passed=process.returncode==0 and report['status']=='pass',browser=report)
finally:
    if config_path.exists():config_path.unlink()
    if server:server.shutdown();server.server_close()
    if client:client.close()
    result.update(temporary_credentials_removed=not config_path.exists(),ssh_forward_closed=True)
    (OUT/'browser-verification.json').write_text(json.dumps(result,indent=2)+'\n',encoding='utf-8')
print(json.dumps({'passed':result['passed'],'checks':len(result.get('browser',{}).get('checks',[])),'private_config_removed':result['temporary_credentials_removed']}))
raise SystemExit(0 if result['passed'] else 1)
