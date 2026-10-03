"""Explicit performance-schema rollout and read-only production verification.

Run start-migration, poll migration-status, then use hot_release.py to switch.
Migration and application activation are deliberately separate operations.
"""
import argparse
import hashlib
import json
from pathlib import Path
import shlex

from integration_env import connect, run
from hot_release import read_release

MIGRATE = r'''
import fcntl,hashlib,importlib.util,json,os,pathlib,re,shlex,subprocess,time,urllib.parse
release=pathlib.Path(PAYLOAD['remote'])
control=pathlib.Path('/var/lib/codex-agent-release')
work=control/('performance-'+release.name)
def write(value):
 p=work/'result.json';tmp=work/'result.pending'
 tmp.write_text(json.dumps(value,indent=2));os.chmod(tmp,0o600);os.replace(tmp,p)
def sql(query):
 p=subprocess.run(['docker','exec','-e','PGOPTIONS=-c statement_timeout=30000','codex-agent-postgres','psql','-X','-q','-At','-v','ON_ERROR_STOP=1','-U','postgres','-d','codex_agent','-c',query],capture_output=True,text=True,timeout=40)
 if p.returncode:raise RuntimeError('production SQL check failed')
 return p.stdout.strip()
def ledger():return json.loads(sql('SELECT json_object_agg(name,checksum) FROM schema_migrations'))
state={'status':'running','release_id':release.name,'started_at_unix':time.time(),'production_touched':False}
write(state)
try:
 with (control/'zero-downtime.lock').open('a') as lock:
  fcntl.flock(lock,fcntl.LOCK_EX)
  manifest=json.loads((release/'manifest.json').read_text())
  for name in ('deploy/performance_release.py','deploy/powerpoint_release.py'):
   assert hashlib.sha256((release/name).read_bytes()).hexdigest()==PAYLOAD['files'][name]
  spec=importlib.util.spec_from_file_location('performance_validator',release/'deploy/performance_release.py');module=importlib.util.module_from_spec(spec);spec.loader.exec_module(module);module.validate(release)
  runtime=json.loads((control/'active-runtime.json').read_text())['active']
  assert runtime['binary_sha256']==PAYLOAD['expected_active_sha256']
  assert hashlib.sha256(pathlib.Path('/proc/'+str(runtime['pid'])+'/exe').read_bytes()).hexdigest()==runtime['binary_sha256']
  before=ledger();expected=manifest['migration_checksums']
  assert before and all(expected.get(k)==v for k,v in before.items())
  state.update(schema_before=before,pending=sorted(set(expected)-set(before)));write(state)
  values={os.fsdecode(k):os.fsdecode(v) for k,v in (item.split(b'=',1) for item in pathlib.Path('/proc/'+str(runtime['pid'])+'/environ').read_bytes().split(b'\0') if b'=' in item)}
  migration={}
  for line in pathlib.Path('/etc/codex-agent/migration.env').read_text().splitlines():
   if not line.strip() or line.lstrip().startswith('#'):continue
   k,v=line.split('=',1);migration[k]=shlex.split(v)[0]
  assert set(migration)=={'DATABASE_URL'}
  parsed=urllib.parse.urlsplit(migration['DATABASE_URL'])
  assert parsed.hostname=='127.0.0.1' and parsed.port==25433 and parsed.path=='/codex_agent' and parsed.username=='codex_agent_migrator'
  current=urllib.parse.urlsplit(values['DATABASE_URL']);assert (current.hostname,current.port,current.path)==(parsed.hostname,parsed.port,parsed.path)
  assert current.username=='codex_agent_runtime'
  values.update(migration)
  state['production_touched']=True;write(state)
  with (work/'migration-private.log').open('wb') as output:
   result=subprocess.run([str(release/'codex-agent'),'migrate'],env=values,stdout=output,stderr=subprocess.STDOUT,timeout=1830)
  state['exit_code']=result.returncode;state['schema_after']=ledger()
  if result.returncode!=0 or state['schema_after']!=expected:raise RuntimeError('migration failed; inspect private log and ledger before resuming')
  tables=['usage_hourly_global','retained_log_minute','retained_usage_minute','retained_usage_unsafe_minute','retained_usage_daily','retained_operations','usage_account_top_day','usage_account_top_unsafe_day','retained_usage_model_minute','retained_usage_model_daily','retained_usage_model_unsafe_minute']
  if 'migrations/024_request_log_analytics.sql' in expected:
   tables.append('request_log_analytics')
  sql('GRANT SELECT,INSERT,UPDATE,DELETE,TRUNCATE ON '+','.join(tables)+' TO codex_agent_runtime')
  checks=sql("SELECT bool_and(has_table_privilege('codex_agent_runtime',name,'SELECT,INSERT,UPDATE,DELETE,TRUNCATE')) FROM unnest(ARRAY["+','.join("'"+v+"'" for v in tables)+"]::text[]) name")
  assert checks=='t'
  if 'migrations/026_gateway_response_history.sql' in expected:
   sql('GRANT SELECT,INSERT,UPDATE,DELETE ON gateway_response_history TO codex_agent_runtime')
   assert sql("SELECT has_table_privilege('codex_agent_runtime','gateway_response_history','SELECT,INSERT,UPDATE,DELETE')")=='t'
  if 'migrations/027_account_drains.sql' in expected:
   sql('GRANT SELECT,INSERT,UPDATE,DELETE ON account_drains TO codex_agent_runtime')
   assert sql("SELECT has_table_privilege('codex_agent_runtime','account_drains','SELECT,INSERT,UPDATE,DELETE')")=='t'
  if 'migrations/024_request_log_analytics.sql' in state['pending']:
   state.update(phase='analytics_backfill');write(state)
   result=subprocess.run([str(release/'codex-agent'),'backfill-log-analytics','--batch-size','1000'],env=values,capture_output=True,text=True,timeout=1830)
   if result.returncode!=0:
    (work/'analytics-backfill-private.log').write_text(result.stderr)
    raise RuntimeError('analytics backfill failed; old runtime retained')
   backfill=json.loads(result.stdout);state['analytics_backfill']=backfill
   if backfill.get('complete') is not True or backfill.get('remaining')!=0:raise RuntimeError('analytics backfill incomplete')
   sql('ANALYZE request_log_analytics')
  values.clear();migration.clear()
  state.update(status='pass',runtime_summary_permissions=True)
except Exception as error:
 state.update(status='fail',error_type=type(error).__name__)
finally:
 state.update(finished_at_unix=time.time(),elapsed_seconds=round(time.time()-state['started_at_unix'],3));write(state)
'''

VERIFY = r'''
import hashlib,json,pathlib,subprocess,time,urllib.request
manifest=json.loads((pathlib.Path(PAYLOAD['remote'])/'manifest.json').read_text())
registry=json.loads(pathlib.Path('/var/lib/codex-agent-release/active-runtime.json').read_text())
active=registry['active'];actual=hashlib.sha256(pathlib.Path('/proc/'+str(active['pid'])+'/exe').read_bytes()).hexdigest()
assert actual==manifest['files']['codex-agent'] and active['release']==PAYLOAD['remote']
opener=urllib.request.build_opener(urllib.request.ProxyHandler({}))
runtime_env=dict(item.split(b'=',1) for item in pathlib.Path('/proc/'+str(active['pid'])+'/environ').read_bytes().split(b'\0') if b'=' in item)
encrypted=json.loads(subprocess.check_output(['docker','exec','codex-agent-postgres','psql','-X','-q','-At','-U','postgres','-d','codex_agent','-c',"SELECT value FROM settings WHERE key='admin_key'"],text=True))['ciphertext']
value={'Key':runtime_env[b'CREDENTIAL_ENCRYPTION_KEY'].decode(),'KeyID':runtime_env.get(b'CREDENTIAL_ENCRYPTION_KEY_ID',b'primary').decode(),'Ciphertext':encrypted}
key=json.loads(subprocess.check_output(['/opt/codex-agent-integration/checks/decrypt-helper-5ca8b78cde0c9bc4'],input=json.dumps(value).encode()))
results=[]
for path in ['/health/live','/health/ready','/','/api/accounts/manage?page_size=50','/api/logs?page_size=50','/api/logs/options?field=account','/api/logs/stats','/api/overview','/api/operations?range=24h','/api/groups','/api/proxies','/api/keys']:
 start=time.monotonic()
 request=urllib.request.Request('http://127.0.0.1:18878'+path,headers={'Authorization':'Bearer '+key} if path.startswith('/api/') else {})
 with opener.open(request,timeout=30) as r:body=r.read();status=r.status
 results.append({'path':path,'status':status,'ms':round((time.monotonic()-start)*1000,3),'bytes':len(body)})
 assert status==200
 if path.startswith('/api/'):json.loads(body)
 if path=='/':assert hashlib.sha256(body).hexdigest()==manifest['files']['frontend/dist/index.html']
print(json.dumps({'status':'pass','active_binary_sha256':actual,'active_pid':active['pid'],'active_port':active['listen_port'],'release_id':manifest['release_id'],'checks':results,'draining_generations':len(registry.get('draining',[]))}))
'''

def execute(client, script, timeout=120):
    return run(client, 'python3 -B -c ' + shlex.quote(script), timeout=timeout)

def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action', choices=('start-migration', 'migration-status', 'verify'))
    parser.add_argument('--release', required=True)
    parser.add_argument('--expected-active-sha256')
    args = parser.parse_args()
    payload = read_release(args.release)
    payload['expected_active_sha256'] = args.expected_active_sha256
    work = '/var/lib/codex-agent-release/performance-' + payload['release_id']
    client = connect()
    try:
        if args.action == 'start-migration':
            if not args.expected_active_sha256 or len(args.expected_active_sha256) != 64:
                raise ValueError('observed --expected-active-sha256 is required')
            source = 'PAYLOAD=' + repr(payload) + '\n' + MIGRATE
            bootstrap = 'WORK=' + repr(work) + '\nSOURCE=' + repr(source) + '\n' + r'''
import json,pathlib,subprocess
work=pathlib.Path(WORK)
assert work.parent==pathlib.Path('/var/lib/codex-agent-release') and not work.is_symlink()
work.mkdir(mode=0o700,exist_ok=True)
if (work/'result.json').exists():
 print((work/'result.json').read_text())
else:
 script=work/'migrate.py';script.write_text(SOURCE);script.chmod(0o600)
 unit='codex-agent-performance-migrate-'+work.name[-16:]
 p=subprocess.run(['systemd-run','--unit='+unit,'--property=RuntimeMaxSec=1900','--property=UMask=0077','/usr/bin/python3','-B',str(script)],capture_output=True,text=True)
 assert p.returncode==0
 print(json.dumps({'status':'started','unit':unit,'result':str(work/'result.json')}))
'''
            raw = execute(client, bootstrap)
        elif args.action == 'migration-status':
            raw = execute(client, 'import pathlib\nprint(pathlib.Path(' + repr(work+'/result.json') + ').read_text())')
        else:
            raw = execute(client, 'PAYLOAD=' + repr(payload) + '\n' + VERIFY, timeout=420)
        output = Path(args.release).resolve().parent.parent / (args.action + '.json')
        output.write_text(raw, encoding='utf-8')
        print(raw)
    finally:
        client.close()

if __name__ == '__main__':
    main()
