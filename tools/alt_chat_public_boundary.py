"""One unauthenticated session baseline and safe persisted cookie metadata."""
from datetime import datetime,timezone
import json
from integration_env import ROOT,connect

REMOTE=r'''
import os,json,logging,hashlib,re,time
os.environ['AUTH_HTTP_TRACE']='0';os.environ['AUTH_TRACE_DUMP']='0';os.environ['AUTH_TRACE_INCLUDE_COOKIE']='0';logging.disable(logging.CRITICAL)
import psycopg
from psycopg.rows import dict_row
from auth_flow import AuthFlow
from config import Config
def sha(b):return hashlib.sha256(b).hexdigest()
with psycopg.connect(os.environ['DATABASE_URL'],options='-c default_transaction_read_only=on -c statement_timeout=10000',row_factory=dict_row) as con:
 rows=con.execute("SELECT c.email,r.session_token,r.device_id,r.cookie_header,r.extra_json FROM team_rotation_children c JOIN team_rotation_owners o ON o.id=c.owner_id JOIN registered r ON r.email=c.email WHERE c.status='active' AND r.deleted_at IS NULL AND md5(o.id) IN ('36802847ceb1c167305f48d627d32096','fa000e9a1ec463e1b4eac159f48574e4') ORDER BY md5(c.email)").fetchall()
for n,r in enumerate(rows):
 pairs={}
 for part in (r['cookie_header'] or '').split(';'):
  if '=' in part:
   name,value=part.strip().split('=',1);pairs[name]=value
 chunks=sorted([(k,v) for k,v in pairs.items() if k.startswith('__Secure-next-auth.session-token.')])
 joined=''.join(v for k,v in chunks)
 print(json.dumps({'alias':'child'+str(n+1),'stage':'cookie_metadata','session_cookie_names':[k for k in pairs if 'session-token' in k],'session_cookie_value_lengths':{k:len(v) for k,v in pairs.items() if 'session-token' in k},'stored_session_length':len(r['session_token'] or ''),'whole_session_present':'__Secure-next-auth.session-token' in pairs,'whole_session_matches_stored':pairs.get('__Secure-next-auth.session-token')==r['session_token'],'chunked_session_present':bool(chunks),'joined_chunk_length':len(joined),'joined_chunks_match_stored':bool(chunks) and joined==r['session_token'],'stored_device_cookie_matches':pairs.get('oai-did')==r['device_id']}),flush=True)
extra=rows[0]['extra_json'] or {};extra=json.loads(extra) if isinstance(extra,str) else extra
cfg=Config();cfg.proxy='socks5h://account-warp:40001'
flow=AuthFlow(cfg,fingerprint_profile=extra['fingerprint_profile']);flow._http_trace_enabled=False;flow._trace_dump_enabled=False
try:
 start=time.monotonic();res=flow.session.get('https://chatgpt.com/api/auth/session',headers=flow._common_headers('https://chatgpt.com/'),timeout=30,allow_redirects=False)
 try:d=res.json()
 except Exception:d={}
 banner=d.get('WARNING_BANNER') if isinstance(d,dict) else None
 if isinstance(banner,str):
  banner=re.sub(r'[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}','[email]',banner)
  banner=re.sub(r'eyJ[A-Za-z0-9_.-]{20,}','[token]',banner)
 print(json.dumps({'stage':'public_session_baseline','http':res.status_code,'elapsed_ms':round((time.monotonic()-start)*1000),'body_bytes':len(res.content),'body_sha256':sha(res.content),'root_keys':sorted(d.keys()) if isinstance(d,dict) else [],'warning_banner':banner,'cookies_sent':False,'authorization_sent':False,'cf_ray':res.headers.get('cf-ray')}),flush=True)
finally:flow.session.close()
'''
def main():
 output=ROOT/'.local/alternative-channels'/datetime.now(timezone.utc).strftime('%Y%m%dT%H%M%S.%fZ-web-public');output.mkdir(parents=True)
 (output/'remote.py').write_text(REMOTE,encoding='utf-8');client=connect()
 try:
  stdin,out,err=client.exec_command('docker exec -i -w /app loophub-multitenant-app python3 -B -',timeout=60);stdin.write(REMOTE);stdin.channel.shutdown_write()
  with (output/'evidence.jsonl').open('w',encoding='utf-8') as f:
   for line in out:f.write(line);f.flush();print(line,end='',flush=True)
  code=out.channel.recv_exit_status();error=err.read();summary={'exit_code':code,'stderr_bytes':len(error),'output':str(output)}
  (output/'result.json').write_text(json.dumps(summary,indent=2),encoding='utf-8');print(json.dumps(summary),flush=True)
  if code:raise SystemExit(code)
 finally:client.close()
if __name__=='__main__':main()
