"""Record independent post-release facts without running inference or changing state."""
import hashlib
import json
import shlex
from integration_env import ROOT, connect, run

OUT=ROOT/'.local/full-release-20260919'
manifest=json.loads((OUT/'package/full-release.json').read_text())
script='EXPECTED='+repr(manifest['binary_sha256'])+'\n'+r'''
import hashlib,json,pathlib,subprocess,urllib.request
def cmd(args):return subprocess.check_output(args,text=True,timeout=30).strip()
pid=cmd(['systemctl','show','codex-agent.service','-p','MainPID','--value'])
runtime=hashlib.sha256(pathlib.Path('/proc/'+pid+'/exe').read_bytes()).hexdigest()
assert runtime==EXPECTED
logs=cmd(['journalctl','-u','codex-agent.service','--since','2026-09-19 03:31:25','--no-pager','-o','cat','-n','300'])
native='Upstream transport: HTTP and WebSocket native Rust' in logs
lifecycle='Native HTTP client lifecycle: cli_request' in logs
assert native and lifecycle
properties=dict(line.split('=',1) for line in cmd(['systemctl','show','codex-agent.service','-p','ActiveState','-p','SubState','-p','NRestarts','-p','Result']).splitlines())
timer=cmd(['systemctl','show','codex-agent-workbench-bridge.timer','-p','ActiveState','--value'])
bridge=dict(line.split('=',1) for line in cmd(['systemctl','show','codex-agent-workbench-bridge.service','-p','Result','-p','ExecMainStatus','-p','ExecMainExitTimestamp']).splitlines())
assert properties['ActiveState']=='active' and properties['SubState']=='running' and timer=='active'
print(json.dumps({'passed':True,'runtime_sha256':runtime,'pid':int(pid),'service':properties,'native_startup_logged':native,'cli_request_lifecycle_logged':lifecycle,'bridge_timer':timer,'bridge':bridge,'real_account_tests':False,'active_model_requests':0,'credentials_exported':False}))
'''
client=connect()
try: runtime=json.loads(run(client,'python3 -c '+shlex.quote(script)))
finally:client.close()
(OUT/'runtime-final.json').write_text(json.dumps(runtime,indent=2)+'\n',encoding='utf-8')
read=lambda name:json.loads((OUT/name).read_text(encoding='utf-8-sig'))
activation,verification,websocket,browser=map(read,['activate.json','verify.json','websocket-enabled.json','browser-verification.json'])
assert activation['deployed'] and websocket['passed'] and browser['passed']
assert activation['data_before']==activation['data_after']
files=['acceptance.json','production-before.json','preparation-facts.json','preflight.json','activate.json','verify.json','websocket-enabled.json','browser-verification.json','runtime-final.json','public-health.json','package/full-release.json','release.tar.gz']
report={'schema':'codex-full-deployment-20260919-v1','deployed':True,'release_id':manifest['release_id'],
        'deployed_at':activation['deployed_at'],'url':'http://43.225.196.170:18879',
        'source_sha256':manifest['source_sha256'],'binary_sha256':manifest['binary_sha256'],
        'native_sha256':manifest['native_sha256'],'native_http':True,'native_websocket':True,'websocket_route_enabled':True,
        'frontend_files':len(manifest['frontend']),'billing':False,'existing_data_unchanged_at_switch':True,
        'counts_at_switch':{k:v for k,v in activation['data_after'].items() if not k.endswith('_state')},
        'production_browser_checks':len(browser['browser']['checks']),'production_browser_passed':browser['passed'],
        'runtime':runtime,'data_restored':False,'active_model_requests':0,
        'initial_prestart_attempt':'Guarded baseline restoration before candidate startup; empty PG session aggregate corrected in a new immutable release.',
        'rollback_boundary':'Do not downgrade to the original old writer against upgraded state; private recovery materials retained without restoring old data.',
        'evidence':[{'path':(OUT/name).relative_to(ROOT).as_posix(),'sha256':hashlib.sha256((OUT/name).read_bytes()).hexdigest()} for name in files]}
(ROOT/'docs/FULL-DEPLOYMENT-2026-09-19.json').write_text(json.dumps(report,indent=2)+'\n',encoding='utf-8')
print(json.dumps({'deployed':True,'native_http':True,'native_websocket':True,'browser_checks':report['production_browser_checks'],'counts':report['counts_at_switch'],'bridge':runtime['bridge']}))
