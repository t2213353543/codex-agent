"""Read-only preparation facts for this Codex full release, without credentials."""
import json
import shlex
from integration_env import connect, run, ROOT

SCRIPT = r'''
import json,pathlib,subprocess,urllib.request
key=pathlib.Path('/var/lib/codex-agent/client-key').read_text().strip()
opener=urllib.request.build_opener(urllib.request.ProxyHandler({}))
def api(path):
 request=urllib.request.Request('http://127.0.0.1:18879'+path,headers={'Authorization':'Bearer '+key})
 with opener.open(request,timeout=20) as response:return json.load(response)
settings=api('/api/settings')
proxies=api('/api/proxies')
rows=proxies.get('proxies',proxies.get('items',[])) if isinstance(proxies,dict) else proxies
counts={}
for row in rows:
 if row.get('enabled'):
  typ=row.get('type','unknown');counts[typ]=counts.get(typ,0)+1
sessions=subprocess.check_output(['docker','exec','codex-agent-postgres','psql','-X','-q','-At','-U','postgres','-d','codex_agent','-c',"SELECT coalesce(json_agg(json_build_object('application',application_name,'state',state,'client',client_addr::text)), '[]'::json) FROM pg_stat_activity WHERE datname='codex_agent' AND usename='codex_agent_runtime'"],text=True,timeout=20)
print(json.dumps({'transport_settings':{key:settings.get(key) for key in ['websocket_enabled','http_stream_enabled','http_json_enabled','codex_fingerprint_enabled','codex_fingerprint_profile','settings_version']},'enabled_proxy_types':counts,'runtime_connections':json.loads(sessions),'model_requests':0,'mutations':0}))
'''

client=connect()
try:
    result=json.loads(run(client,'python3 -c '+shlex.quote(SCRIPT)))
finally:client.close()
target=ROOT/'.local/full-release-20260919/preparation-facts.json'
target.write_text(json.dumps(result,indent=2)+'\n',encoding='utf-8')
print(json.dumps(result))
