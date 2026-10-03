"""Read-only verification of the 28765 OAuth selector release."""
import json
from pathlib import Path
from integration_env import connect
from oauth_profile_compare_ops import remote

RELEASE = "20260928-oauth-profile-selector-v2"
PROGRAM = r'''
import hashlib,json,os,pathlib,urllib.request
from datetime import datetime,timezone
def api(path):
 req=urllib.request.Request('http://127.0.0.1:8765'+path,headers={'x-admin-key':os.environ['ADMIN_API_KEY']})
 with urllib.request.urlopen(req,timeout=30) as response:return json.load(response)
health=api('/api/health')
status=api('/api/team-rotation/status')
schema=api('/openapi.json')
paths=schema['paths']
names=('TeamRotationConfigReq','TeamOwnerUpdateReq','TeamOwnerImportReq')
fields={name:schema['components']['schemas'][name]['properties'].get('oauth_auth_profile') for name in names}
owners=status.get('owners',[])
meta=json.loads(pathlib.Path('/app/webui/static/build-meta.json').read_text())
frontend_matches=all(hashlib.sha256((pathlib.Path('/app/webui/static')/name).read_bytes()).hexdigest()==digest for name,digest in meta['outputs'].items())
assets=''.join(path.read_text(errors='replace') for path in pathlib.Path('/app/webui/static/assets').glob('*.js'))
assert health.get('ok') is True
assert all(fields.values())
assert status['config']['oauth_auth_profile'] in ('codex','office_plugin')
assert all('oauth_auth_profile' in owner for owner in owners)
assert '/api/team-rotation/children/{email}/reauthorize' in paths
assert '/api/tenant/team/children/{email}/reauthorize' in paths
assert all(label in assets for label in ('插件专用授权','现有 Codex 授权','继承全局设置','重新授权'))
assert frontend_matches
print(json.dumps({'checked_at':datetime.now(timezone.utc).isoformat(),'health':health,
 'global_auth_profile':status['config']['oauth_auth_profile'],
 'owner_profiles':[{'owner_ref':hashlib.md5(str(o['id']).encode()).hexdigest(),'profile':o['oauth_auth_profile']} for o in owners],
 'flags':{key:status['config'][key] for key in ('enabled','monitoring_enabled','log_429_monitoring_enabled')},
 'api_fields':fields,'manual_reauthorization_routes':True,'frontend_labels_present':True,
 'frontend_hashes_match':frontend_matches,'frontend_theme':meta['theme'],
 'upstream_requests':0,'reauthorizations_triggered':0,'credentials_changed_by_verification':False}))
'''


def main():
    script = "program=" + repr(PROGRAM) + "\nrelease=" + repr(RELEASE) + "\n" + r'''
import hashlib,json,pathlib,subprocess,urllib.request
base=pathlib.Path('/root/loophub-28765')
folder=base/'releases'/release
state=json.loads((folder/'deployment.json').read_text())
manifest=json.loads((folder/'manifest.json').read_text())
c=json.loads(subprocess.check_output(['docker','inspect','loophub-multitenant-app']))[0]
assert state['stage']=='deployed' and c['Image']==state['new_image_id']
assert (base/'source').resolve()==folder/'source'
hash_program="import hashlib,json,pathlib; m="+repr(manifest['files'])+"; assert all(hashlib.sha256((pathlib.Path('/app')/n).read_bytes()).hexdigest()==h for n,h in m.items() if n!='.gitignore')"
subprocess.run(['docker','exec','loophub-multitenant-app','python','-c',hash_program],capture_output=True,check=True)
p=subprocess.run(['docker','exec','loophub-multitenant-app','python','-c',program],capture_output=True,check=True)
result=json.loads(p.stdout)
with urllib.request.urlopen('http://127.0.0.1:28765/api/health',timeout=15) as response:result['gateway_http']=response.status
result.update(release=release,image=c['Config']['Image'],image_id=c['Image'],source_hashes_match=True,
 scheduler_flags_preserved=result['flags']==state['flags'],backups_created=state.get('backups_created'))
assert result['scheduler_flags_preserved']
print(json.dumps(result))
'''
    client = connect()
    try:
        output = remote(client, script, timeout=120)
        destination = Path(__file__).resolve().parents[1] / '.local/latest-operations/project28765/oauth-setting-release-live.json'
        destination.write_text(output, encoding='utf-8')
        print(output)
    finally:
        client.close()


if __name__ == '__main__':
    main()
