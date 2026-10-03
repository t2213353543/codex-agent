"""Read existing 28765 browser-session/model boundary through its required proxy.

No login, refresh-token exchange, account writes or generation. Sensitive values
stay inside the running application's container and only schema/status escapes.
"""
from datetime import datetime, timezone
import json
from pathlib import Path

from integration_env import ROOT, connect

REMOTE = r'''
import os,json,hashlib,time,logging,base64
from urllib.parse import urlparse
os.environ['AUTH_HTTP_TRACE']='0';os.environ['AUTH_TRACE_DUMP']='0';os.environ['AUTH_TRACE_INCLUDE_COOKIE']='0'
logging.disable(logging.CRITICAL)
import psycopg
from psycopg.rows import dict_row
from auth_flow import AuthFlow,valid_auth_device_id
from config import Config
def sha(value):return hashlib.sha256(value.encode() if isinstance(value,str) else value).hexdigest()
def safe_error(resp):
 try:d=resp.json()
 except Exception:return {}
 if not isinstance(d,dict):return {}
 e=d.get('error') or d.get('detail')
 if isinstance(e,str):return {'error_message_sha256':sha(e),'error_message_length':len(e)}
 if isinstance(e,dict):return {'error_type':e.get('type'),'error_code':e.get('code'),'error_message_sha256':sha(str(e.get('message') or ''))}
 return {}
def result(resp,stage,alias,elapsed):
 content=resp.content
 return {'alias':alias,'stage':stage,'http':resp.status_code,'elapsed_ms':round(elapsed*1000),'content_type':resp.headers.get('content-type'),'bytes':len(content),'body_sha256':sha(content),'cf_mitigated':resp.headers.get('cf-mitigated'),'cf_ray':resp.headers.get('cf-ray'),'x_request_id':resp.headers.get('x-request-id'),'redirect_origin':urlparse(resp.headers.get('location','')).netloc,'challenge_detected':resp.headers.get('cf-mitigated')=='challenge' or b'cf-chl-' in content or b'Just a moment...' in content,**safe_error(resp)}
def claim_summary(token):
 try:
  part=token.split('.')[1];d=json.loads(base64.urlsafe_b64decode(part+'='*(-len(part)%4)))
  return {'audience':d.get('aud'),'client_id':d.get('client_id'),'expires_at':d.get('exp')}
 except Exception:return {'format':'opaque'}
with psycopg.connect(os.environ['DATABASE_URL'],options='-c default_transaction_read_only=on -c statement_timeout=10000',row_factory=dict_row) as conn:
 rows=conn.execute("SELECT c.email,c.owner_id,o.proxy,o.oauth_proxy,o.workspace_id,r.access_token,r.session_token,r.device_id,r.cookie_header,r.extra_json FROM team_rotation_children c JOIN team_rotation_owners o ON o.id=c.owner_id JOIN registered r ON r.email=c.email WHERE c.status='active' AND r.deleted_at IS NULL AND md5(o.id) IN ('36802847ceb1c167305f48d627d32096','fa000e9a1ec463e1b4eac159f48574e4') ORDER BY md5(c.email)").fetchall()
 settings={r['key']:r['value'] for r in conn.execute("SELECT key,value FROM settings WHERE key IN ('team_rotation_proxy','team_rotation_oauth_proxy','team_rotation_proxy_required')").fetchall()}
assert len(rows)==2,'Two exact active child sessions required'
for index,r in enumerate(rows):
 alias='child'+str(index+1)
 proxy=r['oauth_proxy'] or settings.get('team_rotation_oauth_proxy') or r['proxy'] or settings.get('team_rotation_proxy') or ''
 if '{sid}' in proxy:
  seed=('oauth-proxy-' if (r['oauth_proxy'] or settings.get('team_rotation_oauth_proxy')) else 'proxy-sid-')+r['owner_id']
  proxy=proxy.replace('{sid}',hashlib.blake2b(seed.encode(),digest_size=4).hexdigest())
 if proxy.startswith('socks5://'):proxy='socks5h://'+proxy[len('socks5://'):]
 assert proxy and urlparse(proxy).hostname=='account-warp','Unexpected existing proxy; do not select a replacement'
 extra=r['extra_json'] or {};extra=json.loads(extra) if isinstance(extra,str) else extra
 fp=extra.get('fingerprint_profile') or {}
 inventory={'alias':alias,'stage':'stored_session','child_ref':hashlib.md5(r['email'].encode()).hexdigest(),'has_session_token':bool(r['session_token']),'has_cookie_header':bool(r['cookie_header']),'valid_device_id':valid_auth_device_id(r['device_id']),'has_persisted_browser_fingerprint':bool(fp.get('impersonate') and fp.get('user_agent')),'proxy_host':urlparse(proxy).hostname,'stored_access_token_claims':claim_summary(r['access_token'] or ''),'no_login_or_refresh':True}
 print(json.dumps(inventory),flush=True)
 if not all([inventory['has_session_token'],inventory['valid_device_id'],inventory['has_persisted_browser_fingerprint']]):continue
 cfg=Config();cfg.proxy=proxy
 flow=AuthFlow(cfg,fingerprint_profile=fp)
 flow._http_trace_enabled=False;flow._trace_dump_enabled=False
 flow.result.device_id=r['device_id']
 pairs={}
 for pair in (r['cookie_header'] or '').split(';'):
  if '=' in pair:
   name,value=pair.strip().split('=',1)
   if name and '\r' not in name+value and '\n' not in name+value:pairs[name]=value
 if pairs.get('__Secure-next-auth.session-token') and pairs['__Secure-next-auth.session-token']!=r['session_token']:
  print(json.dumps({'alias':alias,'stage':'blocked','reason':'stored_session_cookie_mismatch'}),flush=True);flow.session.close();continue
 if pairs.get('oai-did') and pairs['oai-did']!=r['device_id']:
  print(json.dumps({'alias':alias,'stage':'blocked','reason':'stored_device_cookie_mismatch'}),flush=True);flow.session.close();continue
 # The persisted aggregate session may be newer than cookie_header's chunks.
 # Never combine an aggregate cookie with old numbered chunks: NextAuth joins
 # the matching names, creating a different invalid token.
 prefix='__Secure-next-auth.session-token'
 chunk_names=[name for name in pairs if name.startswith(prefix+'.')]
 chunk_size=max((len(pairs[name]) for name in chunk_names),default=3933)
 pairs={name:value for name,value in pairs.items() if name!=prefix and not name.startswith(prefix+'.')}
 session_token=r['session_token']
 if len(session_token)>chunk_size:
  for n,start in enumerate(range(0,len(session_token),chunk_size)):pairs[prefix+'.'+str(n)]=session_token[start:start+chunk_size]
 else:pairs[prefix]=session_token
 pairs['oai-did']=r['device_id']
 print(json.dumps({'alias':alias,'stage':'cookie_rebuild','session_bytes':len(session_token),'session_cookie_parts':len([name for name in pairs if name==prefix or name.startswith(prefix+'.')]),'uses_current_stored_session':True,'stale_parts_removed':True}),flush=True)
 for name,value in pairs.items():flow.session.cookies.set(name,value,domain='chatgpt.com',path='/',secure=True)
 try:
  headers=flow._common_headers('https://chatgpt.com/')
  start=time.monotonic();resp=flow.session.get('https://chatgpt.com/api/auth/session',headers=headers,timeout=30,allow_redirects=False)
  record=result(resp,'web_session',alias,time.monotonic()-start)
  try:data=resp.json()
  except Exception:data={}
  data=data if isinstance(data,dict) else {}
  web_token=data.get('accessToken','');user=data.get('user') or {}
  record.update(has_web_access_token=bool(web_token),session_keys=sorted(data.keys()),identity_email_match=bool(user.get('email') and user['email'].lower()==r['email'].lower()),terminal_recovery_failure=data.get('terminalRecoveryFailure'))
  if isinstance(data.get('error'),str) and len(data['error'])<100 and data['error'].replace('_','').isalnum():record['session_error_code']=data['error']
  if web_token:record['web_access_token_claims']=claim_summary(web_token)
  print(json.dumps(record),flush=True)
  if resp.status_code!=200 or not web_token or not record['identity_email_match'] or record['challenge_detected']:continue
  headers.update({'Authorization':'Bearer '+web_token,'oai-device-id':r['device_id'],'ChatGPT-Account-ID':r['workspace_id']})
  start=time.monotonic();resp=flow.session.get('https://chatgpt.com/backend-api/models',headers=headers,timeout=30,allow_redirects=False)
  record=result(resp,'web_models',alias,time.monotonic()-start)
  try:data=resp.json()
  except Exception:data={}
  if isinstance(data,dict):
   record['root_keys']=sorted(data.keys());models=data.get('models',[])
   if isinstance(models,list):
    record['models']=[{k:m[k] for k in ['slug','title','id','max_tokens','tags','description','reasoning_effort','supported_reasoning_efforts','enabled','disabled'] if k in m} for m in models if isinstance(m,dict)]
  print(json.dumps(record),flush=True)
 except Exception as exc:
  print(json.dumps({'alias':alias,'stage':'transport_error','error_type':type(exc).__name__,'message_sha256':sha(str(exc))}),flush=True)
 finally:flow.session.close()
print(json.dumps({'stage':'complete','generation_requests':0,'database_writes':0,'oauth_login_or_refresh_requests':0}),flush=True)
'''

def main():
    output=ROOT/'.local/alternative-channels'/datetime.now(timezone.utc).strftime('%Y%m%dT%H%M%S.%fZ-web-session')
    output.mkdir(parents=True)
    (output/'remote.py').write_text(REMOTE,encoding='utf-8')
    client=connect()
    try:
        stdin,out,err=client.exec_command('docker exec -i -w /app loophub-multitenant-app python3 -B -',timeout=150)
        stdin.write(REMOTE);stdin.channel.shutdown_write()
        with (output/'evidence.jsonl').open('w',encoding='utf-8') as log:
            for line in out:log.write(line);log.flush();print(line,end='',flush=True)
        code=out.channel.recv_exit_status();error=err.read()
        report={'exit_code':code,'stderr_bytes':len(error),'output':str(output)}
        if code:report['error_type_summary']=error.decode(errors='replace').splitlines()[-1].split(':')[0] if error else ''
        (output/'result.json').write_text(json.dumps(report,indent=2),encoding='utf-8');print(json.dumps(report),flush=True)
        if code:raise SystemExit(code)
    finally:client.close()

if __name__=='__main__':main()
