"""Launch a one-shot native browser on the original server-side proxy."""
from datetime import datetime,timezone
import base64,json,shlex
from integration_env import ROOT,connect

REMOTE=r'''
import subprocess,json,os,pathlib,hashlib
root=pathlib.Path('/opt/codex-agent-integration/checks/alt-chat-browser');root.mkdir(exist_ok=True,mode=0o700)
script=root/'probe.cjs';script.write_bytes(__import__('base64').b64decode(SCRIPT_B64));script.chmod(0o600)
session=subprocess.check_output(['docker','exec','loophub-multitenant-app','cat','/tmp/codex-agent-alt-chat-dhmeegbl/session.json'])
fixture={'mode':'inspect','session':json.loads(session)}
command=['docker','run','--rm','-i','--network','account-hub-cluster_app','--shm-size=512m','--entrypoint','node','-v',str(script)+':/probe.cjs:ro','loophub-ipv6-app:20260917-browser-mode-v7.3','/probe.cjs']
p=subprocess.Popen(command,stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=subprocess.PIPE,text=True)
p.stdin.write(json.dumps(fixture));p.stdin.close()
for line in p.stdout:print(line,end='',flush=True)
code=p.wait(timeout=180);error=p.stderr.read()
print(json.dumps({'stage':'container_exit','exit_code':code,'stderr_bytes':len(error),'no_ports_or_database_mounts':True}),flush=True)
raise SystemExit(code)
'''
def main():
 output=ROOT/'.local/alternative-channels'/datetime.now(timezone.utc).strftime('%Y%m%dT%H%M%S.%fZ-web-browser');output.mkdir(parents=True)
 script='SCRIPT_B64='+repr(base64.b64encode((ROOT/'tools/alt_chat_browser_probe.cjs').read_bytes()).decode())+'\n'+REMOTE
 (output/'remote.py').write_text(script,encoding='utf-8');client=connect()
 try:
  stdin,out,err=client.exec_command('python3 -B -',timeout=240);stdin.write(script);stdin.channel.shutdown_write()
  with (output/'evidence.jsonl').open('w',encoding='utf-8') as f:
   for line in out:f.write(line);f.flush();print(line,end='',flush=True)
  code=out.channel.recv_exit_status();error=err.read();report={'exit_code':code,'stderr_bytes':len(error),'output':str(output)}
  (output/'result.json').write_text(json.dumps(report,indent=2),encoding='utf-8');print(json.dumps(report),flush=True)
  if code:raise SystemExit(code)
 finally:client.close()
if __name__=='__main__':main()
