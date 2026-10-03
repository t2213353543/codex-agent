"""Verify real production UI snapshot/filter shapes without changing accounts."""
import argparse
import json
from pathlib import Path
import shlex
from integration_env import connect, run

REMOTE = r'''
import json,pathlib,subprocess,urllib.request,urllib.error,time,urllib.parse,hashlib
registry=json.loads(pathlib.Path('/var/lib/codex-agent-release/active-runtime.json').read_text())
runtime=registry['candidate' if CANDIDATE else 'active']
assert runtime['binary_sha256']==EXPECTED_BINARY
values=dict(item.split(b'=',1) for item in pathlib.Path('/proc/'+str(runtime['pid'])+'/environ').read_bytes().split(b'\0') if b'=' in item)
def sql(query):return subprocess.check_output(['docker','exec','codex-agent-postgres','psql','-X','-q','-At','-U','postgres','-d','codex_agent','-c',query],text=True).strip()
encrypted=json.loads(sql("SELECT value FROM settings WHERE key='admin_key'"))['ciphertext']
key=json.loads(subprocess.check_output(['/opt/codex-agent-integration/checks/decrypt-helper-5ca8b78cde0c9bc4'],input=json.dumps({'Key':values[b'CREDENTIAL_ENCRYPTION_KEY'].decode(),'KeyID':values.get(b'CREDENTIAL_ENCRYPTION_KEY_ID',b'primary').decode(),'Ciphertext':encrypted}).encode()))
base='http://127.0.0.1:'+str(runtime['listen_port'] if CANDIDATE else 18878)
opener=urllib.request.build_opener(urllib.request.ProxyHandler({}));results=[]
def request(name,path,body=None):
 start=time.monotonic()
 req=urllib.request.Request(base+path,data=None if body is None else json.dumps(body).encode(),headers={'Authorization':'Bearer '+key,'Content-Type':'application/json'})
 try:
  with opener.open(req,timeout=25) as response:raw=response.read();status=response.status
 except urllib.error.HTTPError as error:raw=error.read();status=error.code
 data=json.loads(raw);entry={'name':name,'status':status,'ms':round((time.monotonic()-start)*1000,3)}
 if status!=200:entry['code']=data.get('error',{}).get('code')
 results.append(entry);return data if status==200 else None
for name,filters in [('all',{}),('errors',{'status':'error'}),('rate_limit',{'error_category':'rate_limit'})]:
 snapshot=request(name+'_snapshot','/api/logs/snapshot',{'filters':filters})
 if snapshot is None:break
 stats=request(name+'_snapshot_stats','/api/logs/stats?'+urllib.parse.urlencode(dict(filters,as_of=snapshot['as_of'])))
 if stats is None:break
 results[-1]['total_requests']=stats.get('total_requests')
 if request(name+'_current_stats','/api/logs/stats?'+urllib.parse.urlencode(filters)) is None:break
facts=json.loads(sql("SELECT json_build_object('account_history_fk_removed',NOT EXISTS(SELECT 1 FROM pg_constraint WHERE conrelid='request_logs'::regclass AND conname='request_logs_account_id_fkey'),'migrations',(SELECT count(*) FROM schema_migrations),'invalid_indexes',(SELECT count(*) FROM pg_index i JOIN pg_class c ON c.oid=i.indexrelid WHERE c.relnamespace='public'::regnamespace AND NOT i.indisvalid))"))
passed=len(results)==9 and all(row['status']==200 for row in results) and facts['account_history_fk_removed'] and facts['migrations']==EXPECTED_MIGRATIONS and facts['invalid_indexes']==0
print(json.dumps({'status':'pass' if passed else 'fail','candidate':CANDIDATE,'binary_sha256':runtime['binary_sha256'],'database':facts,'checks':results,'skipped_checks':9-len(results),'failure_policy':'stop expensive probes after first failed response','mutation_scope':'temporary query snapshots only; no account deletion or credential change'}))
'''

def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--release',required=True)
    parser.add_argument('--candidate',action='store_true')
    args=parser.parse_args()
    path=Path(args.release).resolve();manifest=json.loads(path.read_text(encoding='utf-8'))
    script='CANDIDATE='+repr(args.candidate)+'\nEXPECTED_BINARY='+repr(manifest['files']['codex-agent'])+'\nEXPECTED_MIGRATIONS='+repr(len(manifest['migration_checksums']))+'\n'+REMOTE
    client=connect()
    try:raw=run(client,'python3 -B -c '+shlex.quote(script),timeout=300)
    finally:client.close()
    output=path.parent.parent/('candidate-fixes.json' if args.candidate else 'production-fixes.json')
    output.write_text(raw,encoding='utf-8');print(raw)
    return 0 if json.loads(raw)['status']=='pass' else 1

if __name__=='__main__':raise SystemExit(main())
