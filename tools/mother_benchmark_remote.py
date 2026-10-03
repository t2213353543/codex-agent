"""Run the mother benchmark on the gateway host and fetch credential-free results."""
import argparse
import datetime as dt
import json
from pathlib import Path
import shlex
import sys
import zipfile

from integration_env import ROOT, connect

REMOTE_ROOT = '/var/lib/codex-agent-mother-benchmark'
MONITOR = r'''
import hashlib, json, pathlib, subprocess, sys, threading, time, urllib.request
root=pathlib.Path('/var/lib/codex-agent-mother-benchmark')
out=pathlib.Path(sys.argv[1]);out.mkdir(mode=0o700,parents=True,exist_ok=True)
key=pathlib.Path('/var/lib/codex-agent/client-key').read_text().strip()
setup=json.loads((root/'setup.json').read_text())
stop=threading.Event();samples=[]
def runtime():
 pid=subprocess.check_output(['systemctl','show','codex-agent','-p','MainPID','--value']).decode().strip()
 return {'pid':pid,'sha256':hashlib.sha256(pathlib.Path('/proc/'+pid+'/exe').read_bytes()).hexdigest()}
runtime_before=runtime()
def monitor():
 while not stop.is_set():
  try:
   req=urllib.request.Request('http://127.0.0.1:18879/api/accounts',headers={'Authorization':'Bearer '+key})
   with urllib.request.urlopen(req,timeout=5) as r: rows=json.load(r).get('accounts',[])
   rows=[a for a in rows if setup['group_id'] in a.get('groups',[])]
   samples.append({'time':time.time(),'in_flight':sum(a.get('in_flight',0) for a in rows),'accounts':[{'id':a['id'],'in_flight':a.get('in_flight',0),'limit':a.get('concurrency'),'status':a.get('status')} for a in rows]})
  except Exception as e: samples.append({'time':time.time(),'error':type(e).__name__})
  stop.wait(.1)
t=threading.Thread(target=monitor,daemon=True);t.start()
started=time.time()
try:
 code=subprocess.call([sys.executable,'-u',str(root/'mother_account_benchmark.py'),'--base-url','http://127.0.0.1:18879/v1','--key-file',str(root/'gateway-key'),'--output-dir',str(out),*sys.argv[2:]])
finally:
 stop.set();t.join(8)
 runtime_after=runtime()
 (out/'gateway-concurrency.json').write_text(json.dumps({'started_at':started,'finished_at':time.time(),'runtime_before':runtime_before,'runtime_after':runtime_after,'runtime_stable':runtime_before==runtime_after,'scope':'gateway account admission, sampled every 100ms; not a direct upstream execution counter','peak_in_flight':max((s.get('in_flight',0) for s in samples),default=0),'samples':samples},ensure_ascii=False,indent=2))
 sql="SELECT coalesce(json_agg(record),'[]'::json) FROM request_logs WHERE record->>'key_id'='"+setup['key_id']+"' AND created_at >= to_timestamp("+str(started-1)+")"
 result=subprocess.check_output(['docker','exec','codex-agent-postgres','psql','-X','-q','-At','-U','postgres','-d','codex_agent','-c',sql])
 (out/'gateway-request-logs.json').write_bytes(result)
 print(json.dumps({'gateway_peak_in_flight':max((s.get('in_flight',0) for s in samples),default=0),'server_records':len(json.loads(result))}),flush=True)
sys.exit(code)
'''

def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--phase', choices=['functional','candy','load','compact','websocket'], required=True)
    args, extra = parser.parse_known_args()
    run_id = dt.datetime.now(dt.timezone.utc).strftime('%Y%m%dT%H%M%SZ')+'-'+args.phase
    local = ROOT/'.local/mother-benchmark'/run_id
    local.mkdir(parents=True, exist_ok=True)
    remote = REMOTE_ROOT+'/'+run_id
    client = connect()
    code = 1
    try:
        with client.open_sftp() as sftp:
            sftp.put(str(ROOT/'tools/mother_account_benchmark.py'),REMOTE_ROOT+'/mother_account_benchmark.py')
            if args.phase == 'websocket':
                sftp.put(str(ROOT/'tools/mother_websocket_probe.py'),REMOTE_ROOT+'/mother_websocket_probe.py')
            with sftp.file(REMOTE_ROOT+'/run_monitor.py','w') as f: f.write(MONITOR)
        if args.phase == 'websocket':
            command=shlex.join(['python3','-u',REMOTE_ROOT+'/mother_websocket_probe.py','--base-url','http://127.0.0.1:18879/v1','--key-file',REMOTE_ROOT+'/gateway-key','--output-dir',remote,*extra])
        else:
            command=shlex.join(['python3','-u',REMOTE_ROOT+'/run_monitor.py',remote,'--phase',args.phase,*extra])
        stdin, stdout, stderr=client.exec_command(command,timeout=1800)
        stdin.channel.shutdown_write()
        for line in stdout: print(line,end='',flush=True)
        errors=stderr.read().decode('utf-8','replace')
        if errors: print(errors,file=sys.stderr)
        code=stdout.channel.recv_exit_status()
        archive_code = "import pathlib,sys,zipfile; root=pathlib.Path(sys.argv[1]); z=zipfile.ZipFile(str(root)+'.zip','w',zipfile.ZIP_DEFLATED); [z.write(p,p.relative_to(root)) for p in root.rglob('*') if p.is_file()]; z.close()"
        _, packed, pack_error=client.exec_command(shlex.join(['python3','-c',archive_code,remote]),timeout=60)
        if packed.channel.recv_exit_status():raise RuntimeError('Result archive failed')
        archive_path=local/'artifacts.zip'
        with client.open_sftp() as sftp:sftp.get(remote+'.zip',str(archive_path))
        with zipfile.ZipFile(archive_path) as archive:
            for item in archive.infolist():
                destination=(local/item.filename).resolve()
                if local.resolve() not in destination.parents:raise ValueError('Unexpected archive path')
            archive.extractall(local)
        print(json.dumps({'output_dir':str(local),'exit_code':code},ensure_ascii=False))
    finally:client.close()
    return code

if __name__=='__main__':sys.exit(main())
