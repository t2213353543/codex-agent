"""Verify compatibility routes through the deployed 0.4.9 gateway."""
import json
from pathlib import Path
import shlex
from integration_env import ROOT, connect, run

PROGRAM=r'''
KEY=admin_key()
before=api('/api/settings')
assert before['version']=='0.4.9' and before['inference_mode']=='powerpoint_bps'
accounts=api('/api/accounts?limit=100')['accounts']
config=sorted((a['id'],a['enabled'],a['concurrency'],a['priority'],a['weight']) for a in accounts)
results=[]
for model,effort,want in [('gpt-5.5','max','gpt-5.6-sol'),('codex-auto','max','gpt-6-astra'),('codex-auto-review','ultra','gpt-6-astra'),('gpt-reserve','max','gpt-5.6-sol')]:
 marker='gateway-release-validation/0.4.9-'+uuid.uuid4().hex
 body={'model':model,'reasoning':{'effort':effort},'stream':False,'input':'Reply exactly OK without tools.'}
 request=urllib.request.Request('http://127.0.0.1:18879/v1/responses',data=json.dumps(body).encode(),headers={'Authorization':'Bearer '+KEY,'Content-Type':'application/json','User-Agent':marker,'Session-Id':marker.rsplit('-',1)[1]})
 start=time.monotonic()
 try:
  with HTTP.open(request,timeout=120) as response:status=response.status;value=json.load(response)
 except urllib.error.HTTPError as e:status=e.code;value=json.loads(e.read())
 row={'requested_model':model,'requested_effort':effort,'http':status,'status':value.get('status'),'returned_model':value.get('model'),'elapsed_ms':round((time.monotonic()-start)*1000)}
 rows=[]
 for _ in range(20):
  rows=json.loads(sql("SELECT coalesce(json_agg(record),'[]'::json) FROM request_logs WHERE record->>'user_agent'='"+marker+"'"))
  if rows:break
  time.sleep(.2)
 if rows:
  record=rows[-1]
  row.update(recorded_requested_model=record.get('model'),sent_model=record.get('sent_model'),recorded_requested_effort=record.get('requested_reasoning_effort'),sent_reasoning_effort=record.get('sent_reasoning_effort'),usage_known=record.get('usage_known'))
 row['passed']=status==200 and value.get('status')=='completed' and value.get('model')==want and row.get('sent_model')==want and row.get('sent_reasoning_effort')=='xhigh' and row.get('recorded_requested_model')==model and row.get('recorded_requested_effort')==effort
 results.append(row)
after=api('/api/settings');remaining=api('/api/accounts?limit=100')['accounts']
assert before['settings_version']==after['settings_version']
assert sorted((a['id'],a['enabled'],a['concurrency'],a['priority'],a['weight']) for a in remaining)==config
print(json.dumps({'version':after['version'],'mode':after['inference_mode'],'passed':all(x['passed'] for x in results),'routes':results,'generation_requests':len(results),'settings_unchanged':True,'account_configuration_unchanged':True,'checked_at':time.strftime('%Y-%m-%dT%H:%M:%SZ',time.gmtime())}))
'''

def main():
    prefix=(ROOT/'.local/reliability-modes/remote_probe.py').read_text(encoding='utf-8').split('KEY=admin_key()')[0]
    client=connect()
    try:
        raw=run(client,'python3 -B -c '+shlex.quote(prefix+PROGRAM),timeout=420)
        (ROOT/'.local/model-routing-release-20260928/postdeploy-routes.json').write_text(raw,encoding='utf-8')
        print(raw)
        if not json.loads(raw)['passed']:raise SystemExit(1)
    finally:client.close()

if __name__=='__main__':main()
