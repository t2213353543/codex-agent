"""One leased existing-child browser login; no credential persistence or generation."""
from datetime import datetime,timezone
import json
from integration_env import ROOT,connect

REMOTE=r'''
import os,json,hashlib,time,logging,pathlib,tempfile,sys,contextlib
from urllib.parse import urlparse
os.environ['AUTH_HTTP_TRACE']='0';os.environ['AUTH_TRACE_DUMP']='0';os.environ['AUTH_TRACE_INCLUDE_COOKIE']='0'
import psycopg
from psycopg.rows import dict_row
from webui import db
from webui.oauth_repair import reauthorize_registered_session
from webui.team_rotation import decode_jwt_identity
from auth_flow import AuthFlow
from config import Config
CHILD='2d3524dbef4877afde33d7d5a0ad0edf'
fields=['oauth_reauth_status','oauth_reauth_error','oauth_reauth_updated_at','oauth_reauth_failures','oauth_reauth_next_at']
run=pathlib.Path(tempfile.mkdtemp(prefix='codex-agent-alt-chat-',dir='/tmp'));run.chmod(0o700)
def emit(d):print(json.dumps(d),flush=True)
def sha(s):return hashlib.sha256(s.encode() if isinstance(s,str) else s).hexdigest()
def private_file(name,data):
 p=run/name;fd=os.open(p,os.O_CREAT|os.O_EXCL|os.O_WRONLY,0o600)
 with os.fdopen(fd,'w') as f:json.dump(data,f)
 return p
def response_summary(r,stage):
 result={'stage':stage,'http':r.status_code,'bytes':len(r.content),'body_sha256':sha(r.content),'cf_ray':r.headers.get('cf-ray'),'x_request_id':r.headers.get('x-request-id'),'cf_mitigated':r.headers.get('cf-mitigated')}
 try:d=r.json()
 except Exception:return result,{}
 if not isinstance(d,dict):return result,{}
 result['root_keys']=sorted(d)
 e=d.get('error') or d.get('detail')
 if isinstance(e,dict):result.update(error_code=e.get('code'),error_type=e.get('type'),error_message_sha256=sha(str(e.get('message') or '')))
 elif isinstance(e,str):result['error_message_sha256']=sha(e)
 return result,d
with psycopg.connect(os.environ['DATABASE_URL'],row_factory=dict_row) as con:
 with con.transaction():
  old=con.execute("SELECT * FROM team_rotation_children WHERE md5(email)=%s FOR UPDATE",(CHILD,)).fetchone()
  assert old and old['status']=='active','exact current active child required'
  now=time.time()
  if old['oauth_reauth_status'] in ('running','cancelled') or float(old['oauth_reauth_next_at'] or 0)>now:
   emit({'stage':'lease_busy','child_ref':CHILD,'generation_requests':0});raise SystemExit(0)
  owner=con.execute('SELECT * FROM team_rotation_owners WHERE id=%s',(old['owner_id'],)).fetchone()
  assert owner and owner['status']=='active','active owner required'
  cur=con.execute("UPDATE team_rotation_children SET oauth_reauth_status='running',oauth_reauth_error=NULL,oauth_reauth_updated_at=%s,updated_at=%s WHERE email=%s AND owner_id=%s AND assigned_at=%s AND status='active'",(now,now,old['email'],old['owner_id'],old['assigned_at']))
  assert cur.rowcount==1,'lease acquisition lost'
attempt=now
emit({'stage':'lease_acquired','child_ref':CHILD,'attempt_at':attempt,'private_directory':str(run)})
try:
 cred=db.get_registered_for_export(old['email']);assert cred,'registered identity missing'
 original_identity=decode_jwt_identity(cred.get('access_token',''));expected_user=original_identity.get('user_id')
 assert expected_user and original_identity.get('email')==old['email'],'original registered identity mismatch'
 with psycopg.connect(os.environ['DATABASE_URL'],options='-c default_transaction_read_only=on',row_factory=dict_row) as con:
  settings={r['key']:r['value'] for r in con.execute("SELECT key,value FROM settings WHERE key IN ('team_rotation_proxy','team_rotation_oauth_proxy')")}
 proxy=owner.get('oauth_proxy') or settings.get('team_rotation_oauth_proxy') or owner.get('proxy') or settings.get('team_rotation_proxy')
 assert proxy and urlparse(proxy).hostname=='account-warp','existing required route missing'
 log_path=run/'auth.log';log_fd=os.open(log_path,os.O_CREAT|os.O_EXCL|os.O_WRONLY,0o600)
 with os.fdopen(log_fd,'w') as log:
  logging.basicConfig(stream=log,level=logging.INFO,force=True)
  with contextlib.redirect_stdout(log),contextlib.redirect_stderr(log):
   recovered=reauthorize_registered_session(old['email'],proxy=proxy,persist=False,credential=cred,expected_user_id=expected_user,require_proxy=True)
 identity=decode_jwt_identity(recovered['access_token'])
 assert identity.get('email')==old['email'] and identity.get('user_id')==expected_user,'recovered identity mismatch'
 assert identity.get('expires_at',0)>time.time(),'recovered web access token expired'
 private_file('session.json',{'child_ref':CHILD,'email':old['email'],'workspace_id':owner['workspace_id'],'expected_user_id':expected_user,'proxy':proxy,'recovered':recovered})
 emit({'stage':'web_session_recovered','child_ref':CHILD,'same_user_verified':True,'expires_at':identity.get('expires_at'),'private_session_saved':True,'credentials_written_to_production':False})
 cfg=Config();cfg.proxy=proxy;flow=AuthFlow(cfg,fingerprint_profile=recovered['fingerprint_profile']);flow._http_trace_enabled=False;flow._trace_dump_enabled=False;flow.result.device_id=recovered['device_id']
 pairs={}
 for part in recovered.get('cookie_header','').split(';'):
  if '=' in part:
   n,v=part.strip().split('=',1);pairs[n]=v
 prefix='__Secure-next-auth.session-token';pairs={n:v for n,v in pairs.items() if n!=prefix and not n.startswith(prefix+'.')}
 token=recovered['session_token']
 if len(token)>3933:
  for n,start in enumerate(range(0,len(token),3933)):pairs[prefix+'.'+str(n)]=token[start:start+3933]
 else:pairs[prefix]=token
 pairs['oai-did']=recovered['device_id']
 for n,v in pairs.items():flow.session.cookies.set(n,v,domain='chatgpt.com',path='/',secure=True)
 try:
  headers=flow._common_headers('https://chatgpt.com/');headers.update({'Authorization':'Bearer '+recovered['access_token'],'oai-device-id':recovered['device_id']})
  r=flow.session.get('https://chatgpt.com/backend-api/accounts/check/v4-2023-04-27',headers=headers,timeout=30,allow_redirects=False)
  report,data=response_summary(r,'workspace_access');accounts=data.get('accounts') or {};report['expected_workspace_present']=isinstance(accounts,dict) and owner['workspace_id'] in accounts;emit(report)
  if r.status_code==200 and report['expected_workspace_present']:
   headers['ChatGPT-Account-ID']=owner['workspace_id']
   r=flow.session.get('https://chatgpt.com/backend-api/models',headers=headers,timeout=30,allow_redirects=False)
   report,data=response_summary(r,'web_models');models=data.get('models') or []
   if isinstance(models,list):report['models']=[{k:m[k] for k in ['slug','title','id','max_tokens','tags','description','reasoning_effort','supported_reasoning_efforts','enabled','disabled'] if k in m} for m in models if isinstance(m,dict)]
   emit(report);private_file('models.json',data)
 finally:flow.session.close()
except Exception as exc:
 emit({'stage':'web_recovery_failed','error_type':type(exc).__name__,'error_message_sha256':sha(str(exc)),'generation_requests':0,'private_directory':str(run)})
finally:
 with psycopg.connect(os.environ['DATABASE_URL'],row_factory=dict_row) as con:
  query='UPDATE team_rotation_children SET '+','.join(k+'=%s' for k in fields)+",updated_at=%s WHERE email=%s AND owner_id=%s AND assigned_at=%s AND status='active' AND oauth_reauth_status='running' AND oauth_reauth_updated_at=%s"
  params=[old[k] for k in fields]+[time.time(),old['email'],old['owner_id'],old['assigned_at'],attempt]
  cur=con.execute(query,params);restored=cur.rowcount==1;con.commit()
 emit({'stage':'lease_release','restored_original_oauth_fields':restored,'child_ref':CHILD,'generation_requests':0,'production_credentials_written':False})
'''

def main():
 output=ROOT/'.local/alternative-channels'/datetime.now(timezone.utc).strftime('%Y%m%dT%H%M%S.%fZ-web-recovery');output.mkdir(parents=True)
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
