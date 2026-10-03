"""Read current release/cache API and optionally exercise one minimal request."""
import argparse
import json
from pathlib import Path
import shlex
from integration_env import ROOT, connect, run

CHECK = r'''
KEY=admin_key()
before=api('/api/settings')
assert before['version']==EXPECTED_VERSION
accounts=api('/api/accounts?limit=100')['accounts']
configuration=sorted((a['id'],a['enabled'],a['concurrency'],a['priority'],a['weight']) for a in accounts)
now=int(time.time()); start=now-1200
params='?from='+str(start)+'&to='+str(now+1)+'&step=60'
overview=api('/api/overview'+params)
stats=api('/api/logs/stats'+params)
cards=overview['cards']
assert all(k in cards for k in ('cache_read_samples','cache_read_unknown_requests','cache_read_rate','cache_write_tokens','cache_write_samples'))
result={'version':before['version'],'mode':before['inference_mode'],'checked_at':time.strftime('%Y-%m-%dT%H:%M:%SZ',time.gmtime()),
 'cards':{k:v for k,v in cards.items() if k.startswith('cache_')},'statistics_readable':True,'generation_calls':0}
if SMOKE:
 marker='cache047-'+uuid.uuid4().hex
 payload={'model':'gpt-6-astra','input':'Reply exactly OK without tools.','reasoning':{'effort':'xhigh'},'stream':False,'stream_options':{'include_usage':True}}
 req=urllib.request.Request('http://127.0.0.1:18879/v1/responses',data=json.dumps(payload).encode(),headers={'Authorization':'Bearer '+KEY,'Content-Type':'application/json','Session-Id':marker,'User-Agent':marker})
 result['generation_calls']=1
 try:
  with HTTP.open(req,timeout=120) as response:
   value=json.load(response);status=response.status
 except urllib.error.HTTPError as error:
  status=error.code;value=json.loads(error.read())
 result['smoke']={'http':status,'status':value.get('status'),'returned_model':value.get('model'),'usage':value.get('usage')}
 result['smoke_passed']=status==200 and value.get('status')=='completed'
 if result['smoke_passed']:
  rows=[]
  for _ in range(20):
   rows=json.loads(sql("SELECT coalesce(json_agg(record),'[]'::json) FROM request_logs WHERE record->>'user_agent'='"+marker+"'"))
   if rows:break
   time.sleep(.2)
  assert len(rows)==1
  record=rows[0];usage=value['usage'];detail=usage.get('input_tokens_details') or {}
  assert record['input_tokens']==usage['input_tokens']
  assert record.get('cached_tokens',0)==detail.get('cached_tokens',0)
  if 'cache_write_tokens' in detail:assert record['cache_write_tokens']==detail['cache_write_tokens']
  assert record.get('prompt_cache',{}).get('coverage')=='complete'
  result['smoke']['persisted_read_write_match']=True
  result['smoke']['cache_observation']=record['prompt_cache']['comparison']
after=api('/api/settings')
remaining=api('/api/accounts?limit=100')['accounts']
assert sorted((a['id'],a['enabled'],a['concurrency'],a['priority'],a['weight']) for a in remaining)==configuration
assert before['settings_version']==after['settings_version']
result.update(settings_unchanged=True,account_configuration_unchanged=True)
print(json.dumps(result))
'''


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--smoke',action='store_true')
    parser.add_argument('--expected-version', default='0.4.7')
    parser.add_argument('--output', type=Path, default=ROOT/'.local/cache-reliability-20260928/postdeploy.json')
    args=parser.parse_args()
    prefix=(ROOT/'.local/reliability-modes/remote_probe.py').read_text(encoding='utf-8').split('KEY=admin_key()')[0]
    code=prefix+'\nSMOKE='+repr(args.smoke)+'\nEXPECTED_VERSION='+repr(args.expected_version)+'\n'+CHECK
    client=connect()
    try:
        value=run(client,'python3 -B -c '+shlex.quote(code),timeout=180)
        args.output.parent.mkdir(parents=True, exist_ok=True)
        args.output.write_text(value,encoding='utf-8')
        print(value)
        if args.smoke and json.loads(value).get('smoke_passed') is not True:
            raise SystemExit(1)
    finally:client.close()


if __name__=='__main__':main()
