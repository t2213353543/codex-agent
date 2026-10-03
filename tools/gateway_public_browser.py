"""Read-only production UI validation through a short-lived SSH forward."""
import argparse
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import select
import socketserver
import stat
import subprocess
import threading

from integration_env import ROOT, connect, run

RELEASE = '35624e0cafd9f60cae88f61d772464de2f0b891638c6a950f5f0a17668ebee70'
BINARY = 'a66c7be3de344221d6433a6bc7140523d95a6a068fd2312757c80f2dc50c073d'


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--run',action='store_true',required=True,help='Only run after release owner confirms restart completed')
    parser.parse_args()
    output=ROOT/'.local/production-browser'/datetime.now(timezone.utc).strftime('%Y%m%dT%H%M%S.%fZ')
    output.mkdir(parents=True)
    report={'schema':'codex-agent-production-browser-runtime-v1','status':'running','release_id':RELEASE,'binary_sha256':BINARY,
            'business_writes':False,'gateway_inference_requests':0,'authenticated_screenshots_saved':0}
    private=output/'private-config.json'
    client=server=None
    try:
        client=connect()
        observed=run(client,"pid=$(systemctl show codex-agent.service --property MainPID --value)\nsha256sum /proc/$pid/exe").split()[0]
        if observed!=BINARY:raise RuntimeError('Unexpected current runtime binary')
        report['runtime_binary_verified']=True
        with client.open_sftp() as sftp:
            info=sftp.stat('/var/lib/codex-agent/client-key')
            if stat.S_IMODE(info.st_mode)!=0o600:raise RuntimeError('Management key file must remain mode 600')
            with sftp.file('/var/lib/codex-agent/client-key','r') as handle:key=handle.read().decode().strip()
        transport=client.get_transport();transport.set_keepalive(15)
        class Handler(socketserver.BaseRequestHandler):
            def handle(self):
                channel=transport.open_channel('direct-tcpip',('127.0.0.1',18879),self.request.getpeername())
                try:
                    while True:
                        readable,_,_=select.select([self.request,channel],[],[],5)
                        if self.request in readable:
                            data=self.request.recv(65536)
                            if not data:break
                            channel.sendall(data)
                        if channel in readable:
                            data=channel.recv(65536)
                            if not data:break
                            self.request.sendall(data)
                finally:channel.close()
        class Forward(socketserver.ThreadingTCPServer):
            allow_reuse_address=True
            daemon_threads=True
        server=Forward(('127.0.0.1',0),Handler)
        worker=threading.Thread(target=server.serve_forever,daemon=True);worker.start()
        config={'base_url':'http://127.0.0.1:'+str(server.server_address[1]),'admin_key':key,'output':str(output),
                'release_id':RELEASE,'binary_sha256':BINARY}
        descriptor=os.open(private,os.O_WRONLY|os.O_CREAT|os.O_EXCL,0o600)
        with os.fdopen(descriptor,'w',encoding='utf-8') as handle:json.dump(config,handle)
        completed=subprocess.run(['node',str(ROOT/'integration/tests/gateway-public.cjs'),str(private)],cwd=ROOT,
            capture_output=True,text=True,encoding='utf-8',errors='replace',timeout=300)
        browser=output/'browser-report.json'
        if not browser.is_file():raise RuntimeError('Browser produced no sanitized report')
        detail=json.loads(browser.read_text(encoding='utf-8'))
        report.update(status='pass' if completed.returncode==0 and detail['status']=='pass' else 'fail',
            browser_report=str(browser.relative_to(ROOT)),browser_report_sha256=hashlib.sha256(browser.read_bytes()).hexdigest(),
            checks=detail['checks'])
    except Exception as error:
        report.update(status='fail',error_type=type(error).__name__)
    finally:
        if private.exists():private.unlink()
        report['temporary_credentials_removed']=not private.exists()
        if server:server.shutdown();server.server_close()
        if client:client.close()
        report['ssh_forward_closed']=True
        report['completed_at']=datetime.now(timezone.utc).isoformat()
        (output/'report.json').write_text(json.dumps(report,indent=2)+'\n',encoding='utf-8')
    print(json.dumps({'status':report['status'],'report':str(output/'report.json'),'checks':report.get('checks',[])}))
    return 0 if report['status']=='pass' else 1


if __name__=='__main__':raise SystemExit(main())
