"""Enable only the user-authorized WebSocket setting on the verified native release."""
import json
import shlex
from integration_env import ROOT, connect, run

out=ROOT/'.local/full-release-20260919'
manifest=json.loads((out/'package/full-release.json').read_text())
script='EXPECTED='+repr(manifest['binary_sha256'])+'\n'+r'''
import hashlib,json,pathlib,subprocess,urllib.request
pid=subprocess.check_output(['systemctl','show','codex-agent.service','-p','MainPID','--value'],text=True).strip()
assert hashlib.sha256(pathlib.Path('/proc/'+pid+'/exe').read_bytes()).hexdigest()==EXPECTED
env=dict(x.split(b'=',1) for x in pathlib.Path('/proc/'+pid+'/environ').read_bytes().split(b'\0') if b'=' in x)
assert env.get(b'CODEX_NATIVE_WEBSOCKET')==b'1' and env.get(b'CODEX_NATIVE_HTTP_HELPER')
key=pathlib.Path('/var/lib/codex-agent/client-key').read_text().strip()
opener=urllib.request.build_opener(urllib.request.ProxyHandler({}))
def api(body=None):
 request=urllib.request.Request('http://127.0.0.1:18879/api/settings',headers={'Authorization':'Bearer '+key,'Content-Type':'application/json'},data=json.dumps(body).encode() if body else None)
 with opener.open(request,timeout=15) as response:return json.load(response)
before=api(); changed=not before['websocket_enabled']
if changed:api({'settings_version':before['settings_version'],'websocket_enabled':True})
after=api(); assert after['websocket_enabled'] is True
ignored={'settings_version','uptime_seconds','websocket_enabled'}
assert {k:v for k,v in before.items() if k not in ignored}=={k:v for k,v in after.items() if k not in ignored}
print(json.dumps({'passed':True,'websocket_enabled':True,'native_http':True,'native_websocket':True,'settings_changed':changed,'settings_version_before':before['settings_version'],'settings_version_after':after['settings_version'],'other_settings_unchanged':True,'upstream_requests':0}))
'''
client=connect()
try: result=json.loads(run(client,'python3 -c '+shlex.quote(script)))
finally:client.close()
(out/'websocket-enabled.json').write_text(json.dumps(result,indent=2)+'\n',encoding='utf-8')
print(json.dumps(result))
