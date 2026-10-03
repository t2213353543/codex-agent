"""Read current authenticated ChatGPT HTML and observed public script contracts."""
from datetime import datetime,timezone
import json
from integration_env import ROOT,connect

REMOTE=r'''
import os,json,re,hashlib,pathlib,logging
from urllib.parse import urljoin,urlparse
os.environ['AUTH_HTTP_TRACE']='0';os.environ['AUTH_TRACE_DUMP']='0';logging.disable(logging.CRITICAL)
from auth_flow import AuthFlow
from config import Config
root=pathlib.Path('/tmp/codex-agent-alt-chat-dhmeegbl');saved=json.loads((root/'session.json').read_text());data=saved['recovered']
cfg=Config();cfg.proxy=saved['proxy'];flow=AuthFlow(cfg,fingerprint_profile=data['fingerprint_profile']);flow._http_trace_enabled=False;flow._trace_dump_enabled=False;flow.result.device_id=data['device_id']
pairs={}
for part in data.get('cookie_header','').split(';'):
 if '=' in part:
  k,v=part.strip().split('=',1);pairs[k]=v
prefix='__Secure-next-auth.session-token';pairs={k:v for k,v in pairs.items() if k!=prefix and not k.startswith(prefix+'.')}
token=data['session_token']
if len(token)>3933:
 for n,start in enumerate(range(0,len(token),3933)):pairs[prefix+'.'+str(n)]=token[start:start+3933]
else:pairs[prefix]=token
pairs['oai-did']=data['device_id']
for k,v in pairs.items():flow.session.cookies.set(k,v,domain='chatgpt.com',path='/',secure=True)
def emit(d):print(json.dumps(d),flush=True)
def write_private(path,text):
 fd=os.open(path,os.O_WRONLY|os.O_CREAT|os.O_TRUNC,0o600)
 with os.fdopen(fd,'w') as f:f.write(text)
def safe(text):
 for s in [saved['email'],saved['workspace_id'],saved['expected_user_id'],data['access_token'],data['session_token'],data['device_id']]:
  if s:text=text.replace(s,'[private]')
 return re.sub(r'eyJ[A-Za-z0-9_.-]{20,}','[token]',text)
try:
 res=flow.session.get('https://chatgpt.com/',headers=flow._navigation_headers('https://chatgpt.com/'),timeout=30,allow_redirects=False)
 emit({'stage':'html','http':res.status_code,'bytes':len(res.content),'sha256':hashlib.sha256(res.content).hexdigest(),'cf_mitigated':res.headers.get('cf-mitigated')})
 if res.status_code!=200 or res.headers.get('cf-mitigated')=='challenge':raise SystemExit(0)
 html=res.text;write_private(root/'authenticated.html',html)
 scripts=[]
 for url in re.findall(r'(?:src|href)=["\x27]([^"\x27]+\.js(?:\?[^"\x27]*)?)["\x27]',html):
  url=urljoin('https://chatgpt.com/',url)
  if urlparse(url).hostname in ('cdn.oaistatic.com','chatgpt.com') and url not in scripts:scripts.append(url)
 write_private(root/'script-urls.json',json.dumps(scripts))
 emit({'stage':'observed_scripts','count':len(scripts),'urls':scripts})
 assets=root/'assets';assets.mkdir(exist_ok=True,mode=0o700)
 needles=['/backend-api/conversation','conversation/prepare','sentinel/chat-requirements','openai-sentinel-chat-requirements-token','thinking_effort','reasoning_effort','gpt-6-pro','conversation_mode','/backend-api/f/conversation']
 for i,url in enumerate(scripts[:35]):
  r=flow.session.get(url,headers={'Accept':'*/*','Referer':'https://chatgpt.com/'},timeout=30)
  name=urlparse(url).path.rsplit('/',1)[-1];write_private(assets/name,r.text)
  found=[]
  for needle in needles:
   for match in list(re.finditer(re.escape(needle),r.text))[:5]:found.append({'needle':needle,'snippet':safe(r.text[max(0,match.start()-300):match.end()+650])})
  if found:emit({'stage':'contract','asset':name,'http':r.status_code,'matches':found})
 emit({'stage':'complete','assets_read':min(len(scripts),35),'generation_requests':0})
finally:flow.session.close()
'''
def main():
 output=ROOT/'.local/alternative-channels'/datetime.now(timezone.utc).strftime('%Y%m%dT%H%M%S.%fZ-web-frontend');output.mkdir(parents=True)
 (output/'remote.py').write_text(REMOTE,encoding='utf-8');client=connect()
 try:
  stdin,out,err=client.exec_command('docker exec -i -w /app loophub-multitenant-app python3 -B -',timeout=500);stdin.write(REMOTE);stdin.channel.shutdown_write()
  with (output/'evidence.jsonl').open('w',encoding='utf-8') as f:
   for line in out:f.write(line);f.flush();print(line,end='',flush=True)
  code=out.channel.recv_exit_status();error=err.read();report={'exit_code':code,'stderr_bytes':len(error),'output':str(output)}
  (output/'result.json').write_text(json.dumps(report,indent=2),encoding='utf-8');print(json.dumps(report),flush=True)
  if code:raise SystemExit(code)
 finally:client.close()
if __name__=='__main__':main()
