"""One disabled synthetic-account production CRUD/CAS acceptance, explicit opt-in.

The private management key stays in server memory. No inference, refresh, account
enable, restart, direct database mutation or real-credential export is allowed.
"""
import argparse
import base64
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import re
import shlex

ROOT = Path(__file__).resolve().parents[1]
RELEASE = '35624e0cafd9f60cae88f61d772464de2f0b891638c6a950f5f0a17668ebee70'
EXPECTED = 'a66c7be3de344221d6433a6bc7140523d95a6a068fd2312757c80f2dc50c073d'
REMOTE = r'''
import base64,hashlib,json,pathlib,re,secrets,subprocess,time,urllib.error,urllib.parse,urllib.request
expected=json.loads(base64.b64decode(EXPECTED_PAYLOAD))
base='http://127.0.0.1:18879'
result={'schema':'codex-agent-live-disabled-crud-v1','status':'fail','checks':{},'failures':[],
 'checked_at_unix':int(time.time()),'runtime_binary_sha256':expected['binary'],
 'synthetic_only':True,'account_enable_operations':0,'gateway_or_refresh_requests':0,'service_restarts':0,
 'direct_database_writes':0,'cleanup_complete':False}
class Failure(Exception): pass
def require(value,code):
 if not value: raise Failure(code)
def command(args):
 p=subprocess.run(args,capture_output=True,timeout=20)
 require(p.returncode==0,'server_command_failed')
 return p.stdout.decode()
def sql(statement):
 raw=command(['docker','exec','-e','PGOPTIONS=-c statement_timeout=10000 -c default_transaction_read_only=on',
  'codex-agent-postgres','psql','-X','-q','-At','-v','ON_ERROR_STOP=1','-U','postgres','-d','codex_agent','-c',statement])
 return json.loads(raw)
def fingerprint(value): return hashlib.sha256(value.encode()).hexdigest()
def identity():
 state=dict(line.split('=',1) for line in command(['systemctl','show','codex-agent.service','-p','MainPID','-p','ActiveState']).splitlines() if '=' in line)
 require(state.get('ActiveState')=='active' and state.get('MainPID','').isdigit(),'service_not_active')
 pid=state['MainPID'];require(int(pid)>0,'service_pid_missing')
 require(hashlib.sha256(pathlib.Path('/proc/'+pid+'/exe').read_bytes()).hexdigest()==expected['binary'],'binary_mismatch')
 return pid
token='';owned_id='';created_attempted=False
nonce=secrets.token_hex(16);name='synthetic-live-crud-'+nonce;tag='acceptance-crud-'+nonce
upstream='synthetic-upstream-'+nonce;subject='synthetic-user-'+nonce
plain=urllib.request.build_opener(urllib.request.ProxyHandler({}))
def api(path,method='GET',body=None,want=200):
 allowed=(path=='/api/session' and method=='GET') or (path=='/api/accounts' and method=='POST')
 if owned_id:
  own='/api/accounts/'+urllib.parse.quote(owned_id,safe='')
  allowed=allowed or (path==own and method in ('GET','PUT')) or (path==own+'/reauthorize' and method=='POST') or (path.startswith(own+'?version=') and method=='DELETE')
 require(allowed and 'refresh' not in path,'api_outside_scoped_crud')
 require(not isinstance(body,dict) or body.get('enabled') is not True,'enabling_refused')
 request=urllib.request.Request(base+path,method=method,headers={'Authorization':'Bearer '+token,'Content-Type':'application/json'},data=None if body is None else json.dumps(body).encode())
 try: response=plain.open(request,timeout=20)
 except urllib.error.HTTPError as error: response=error
 with response:
  raw=response.read(1024*1024+1);require(len(raw)<=1024*1024,'response_too_large')
  require(response.status==want,'unexpected_http_'+str(response.status))
  value=json.loads(raw)
 return value
def jwt(revision):
 encode=lambda obj:base64.urlsafe_b64encode(json.dumps(obj,separators=(',',':')).encode()).decode().rstrip('=')
 return encode({'alg':'RS256','typ':'JWT'})+'.'+encode({'exp':int(time.time())+86400,'sub':subject,'jti':nonce+'-'+str(revision),
  'https://api.openai.com/auth':{'chatgpt_account_id':upstream,'chatgpt_user_id':subject,'chatgpt_plan_type':'pro'}})+'.invalid-synthetic-signature'
def own_where():
 require(bool(re.fullmatch(r'[a-zA-Z0-9_-]{1,128}',owned_id)),'invalid_owned_id')
 return "a.id='"+owned_id+"'"
def pg_account():
 return sql("SELECT coalesce((SELECT json_build_object('id',a.id,'name',a.display_name,'enabled',a.enabled,'tags',a.tags,'has_refresh_token',a.has_refresh_token,'version',a.version,'concurrency',a.concurrency,'credential_version',c.credential_version,'cipher_hash',md5(c.credentials_cipher),'groups',(SELECT count(*) FROM account_groups WHERE account_id=a.id),'proxies',(SELECT count(*) FROM account_proxies WHERE account_id=a.id)) FROM accounts a JOIN account_credentials c ON c.account_id=a.id WHERE "+own_where()+"),'null'::json)")
def assert_owned(account):
 require(account and account.get('id')==owned_id and account.get('name')==name and account.get('enabled') is False
  and account.get('tags')==[tag] and account.get('has_refresh_token') is False
  and not account.get('groups') and not account.get('proxies'),'ownership_or_disabled_guard_failed')
def usage():
 identifier=owned_id if owned_id else ''
 require(not identifier or re.fullmatch(r'[a-zA-Z0-9_-]{1,128}',identifier),'invalid_usage_id')
 clause="account_ref='"+identifier+"' OR account_name='"+name+"'"
 return sql("SELECT json_build_object('events',(SELECT count(*) FROM usage_events WHERE "+clause+"),'logs',(SELECT count(*) FROM request_logs WHERE "+clause+"),'daily',(SELECT count(*) FROM usage_daily WHERE "+clause+"),'minute',(SELECT count(*) FROM usage_minute WHERE "+clause+"),'totals',(SELECT count(*) FROM usage_totals WHERE scope='account' AND subject='"+identifier+"'))")
def record(check): result['checks'][check]=True
try:
 pid=identity()
 require(command(['docker','port','codex-agent-postgres','5432/tcp']).strip()=='127.0.0.1:25433','dedicated_pg_port_mismatch')
 keyfile=pathlib.Path('/var/lib/codex-agent/client-key')
 require(keyfile.is_file() and keyfile.stat().st_mode&0o077==0,'key_file_not_private')
 token=keyfile.read_text().strip();require(len(token)>=32,'management_key_missing')
 api('/api/session');record('runtime_identity_and_authentication')
 require(sql("SELECT count(*) FROM accounts WHERE display_name='"+name+"'")==0,'synthetic_name_collision')
 original={'name':name,'enabled':False,'concurrency':3,'priority':-100,'weight':1,'tags':[tag],
  'groups':[],'proxies':[],'credentials':{'access_token':jwt(1),'account_id':upstream}}
 created_attempted=True
 imported=api('/api/accounts','POST',{'accounts':[original],'replace':False})
 rows=imported.get('results',[])
 require(len(rows)==1 and rows[0].get('status')=='added' and imported.get('added')==1 and imported.get('replaced')==0,'synthetic_import_not_added')
 owned_id=rows[0]['id'];own_where();result['synthetic_id_sha256']=fingerprint(owned_id);result['synthetic_name_sha256']=fingerprint(name)
 first=api('/api/accounts/'+owned_id)['account'];assert_owned(first)
 pg_first=pg_account();assert_owned(pg_first)
 require(first['version']==pg_first['version'] and pg_first['credential_version']==1,'initial_pg_api_version_mismatch')
 record('disabled_create_and_pg_stable_identity')
 api('/api/accounts/'+owned_id,'PUT',{'version':first['version'],'notes':'synthetic production acceptance metadata','concurrency':4})
 updated=api('/api/accounts/'+owned_id)['account'];assert_owned(updated)
 pg_updated=pg_account();assert_owned(pg_updated)
 require(updated['concurrency']==4 and updated['version']==first['version']+1 and pg_updated['version']==updated['version']
  and pg_updated['credential_version']==pg_first['credential_version'] and pg_updated['cipher_hash']==pg_first['cipher_hash'],'metadata_update_contract_failed')
 record('metadata_cas_preserves_credentials_and_disabled')
 credentials={'access_token':jwt(2),'account_id':upstream}
 reauthorized=api('/api/accounts/'+owned_id+'/reauthorize','POST',{'version':updated['version'],'credentials':credentials,'allow_identity_change':False})
 require(reauthorized.get('id')==owned_id,'reauthorize_id_changed')
 after=api('/api/accounts/'+owned_id)['account'];assert_owned(after)
 pg_after=pg_account();assert_owned(pg_after)
 require(after['name']==name and after['concurrency']==4 and after['version']==updated['version']+1
  and pg_after['version']==after['version'] and pg_after['credential_version']==pg_updated['credential_version']+1
  and pg_after['cipher_hash']!=pg_updated['cipher_hash'],'reauthorize_persistence_or_metadata_failed')
 result['credential_versions']=[pg_first['credential_version'],pg_updated['credential_version'],pg_after['credential_version']]
 result['cipher_hash_changed_on_reauthorize']=True;record('same_identity_reauthorize_persisted')
 conflict=api('/api/accounts/'+owned_id+'/reauthorize','POST',{'version':updated['version'],'credentials':{'access_token':jwt(3),'account_id':upstream},'allow_identity_change':False},409)
 code=conflict.get('error',{}).get('code','')
 require(code=='version_conflict','stale_version_error_code_mismatch')
 require(pg_account()==pg_after,'stale_version_modified_database')
 result['stale_reauthorize_status']=409;result['stale_reauthorize_code']=code;record('stale_reauthorize_409_without_pg_mutation')
 require(all(value==0 for value in usage().values()),'synthetic_model_usage_created')
 record('synthetic_account_created_no_model_usage')
 require(identity()==pid,'service_changed_during_crud');record('service_identity_unchanged')
except Exception as error:
 result['failures'].append({'code':str(error) if isinstance(error,Failure) else 'internal_'+type(error).__name__})
finally:
 try:
  if created_attempted and not owned_id:
   rows=sql("SELECT coalesce(json_agg(id),'[]'::json) FROM accounts WHERE display_name='"+name+"' AND tags=ARRAY['"+tag+"']::text[]")
   require(len(rows)<=1,'cleanup_mapping_ambiguous')
   if rows: owned_id=rows[0];own_where();result['synthetic_id_sha256']=fingerprint(owned_id)
  if owned_id:
   account=api('/api/accounts/'+owned_id)['account'];assert_owned(account);assert_owned(pg_account())
   api('/api/accounts/'+owned_id+'?version='+str(account['version']),'DELETE')
   api('/api/accounts/'+owned_id,want=404)
   require(pg_account() is None,'synthetic_account_survived_delete')
   require(sql("SELECT count(*) FROM accounts WHERE display_name='"+name+"'")==0,'synthetic_name_survived_delete')
   record('owned_disabled_record_deleted_api_and_pg')
   require(all(value==0 for value in usage().values()),'synthetic_usage_survived_cleanup');record('no_synthetic_logs_or_statistics_after_cleanup')
  result['cleanup_complete']=True
 except Exception as error:
  result['cleanup_complete']=False
  result['failures'].append({'code':str(error) if isinstance(error,Failure) else 'cleanup_'+type(error).__name__})
 result['status']='pass' if len(result['checks'])==9 and not result['failures'] and result['cleanup_complete'] else 'fail'
 print(json.dumps(result))
'''


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--run', action='store_true')
    parser.add_argument('--expected-binary', required=True)
    parser.add_argument('--output', type=Path, default=ROOT / '.local/upgrades' / RELEASE / 'live-crud.json')
    args = parser.parse_args()
    if not args.run or args.expected_binary != EXPECTED:
        parser.error('Explicit --run and the deployed a66 binary SHA are required')
    output = args.output.resolve()
    if not output.is_relative_to(ROOT / '.local') or output.exists():
        parser.error('Output must be a new project-local evidence file')
    if os.environ.get('CODEX_AGENT_CLASH_REGION') != 'hk' or os.environ.get('CODEX_AGENT_SSH_PROXY') != 'http://127.0.0.1:7897':
        parser.error('Use the existing explicitly authorized HK Clash route on 7897')
    from integration_env import connect
    payload = base64.b64encode(json.dumps({'binary': EXPECTED}).encode()).decode()
    script = 'EXPECTED_PAYLOAD=' + repr(payload) + '\n' + REMOTE
    client = connect()
    try:
        _, stdout, stderr = client.exec_command('python3 -c ' + shlex.quote(script), timeout=240)
        raw = stdout.read();stderr.read()
        if stdout.channel.recv_exit_status() != 0:
            raise RuntimeError('live_crud_remote_execution_failed')
        report = json.loads(raw)
    finally:
        client.close()
    report['runner_sha256'] = hashlib.sha256(Path(__file__).read_bytes()).hexdigest()
    report['completed_at'] = datetime.now(timezone.utc).isoformat()
    report['scope'] = 'One disabled synthetic record; no real credential/account changes; other-account totals not assumed stable under bridge activity'
    output.parent.mkdir(parents=True, exist_ok=True)
    with output.open('x', encoding='utf-8') as handle:
        json.dump(report, handle, indent=2)
        handle.write('\n')
    print(json.dumps({'status': report['status'], 'checks': report['checks'], 'cleanup_complete': report['cleanup_complete'],
        'failures': report['failures'], 'report': str(output)}, ensure_ascii=False))
    return 0 if report['status']=='pass' else 1


if __name__ == '__main__':
    raise SystemExit(main())
