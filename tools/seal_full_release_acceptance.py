"""Bind the completed backend, native and frontend evidence for this release."""
import hashlib
import json
from pathlib import Path

ROOT=Path(__file__).resolve().parents[1]
OUT=ROOT/'.local/full-release-20260919'
BACKEND=ROOT/'.local/backend-release-20260919/release-backend.json'
UI=ROOT/'.local/management-parity/release-20260919'
read=lambda p:json.loads(p.read_text(encoding='utf-8-sig'))
sha=lambda p:hashlib.sha256(p.read_bytes()).hexdigest()
backend=read(BACKEND);front=read(UI/'frontend-release-verification.json');build=read(UI/'build-manifest.json')
assert backend['status']=='passed_backend_release_checks' and backend['local']['passed'] and backend['local']['source_unchanged']
assert not backend['joint']['failed_tests'] and not backend['joint']['skipped_tests'] and not backend['joint']['missing_tests']
assert backend['native']['helper_sha256']=='6df426c0a8eb703929f888055192e09cec0d2f7eea9391f2ae891e1ca6213740'
assert backend['native']['passed_native']==30 and backend['native']['passed_go_cli']==1 and backend['native']['skipped']==[]
assert front['status']=='passed' and not front['page_errors'] and front['source_matches_prebuild']
assert front['frontend_unit']['pass']==front['frontend_unit']['tests'] and not front['frontend_unit']['fail']
evidence=[]
for item in backend['evidence']:
    assert sha(ROOT/item['path'])==item['sha256']
    evidence.append({'path':item['path'],'sha256':item['sha256']})
for name,digest in front['evidence'].items():
    assert sha(UI/name)==digest
    evidence.append({'path':(UI/name).relative_to(ROOT).as_posix(),'sha256':digest})
source=read(ROOT/backend['source_manifest'])
assert source['sha256']==backend['go_source_sha256']
for name,digest in source['files'].items():assert sha(ROOT/backend['source_directory']/name)==digest
for name,digest in build['source_files'].items():assert sha(ROOT/name)==digest
for name,digest in build['files'].items():assert sha(UI/'dist'/name)==digest
artifact=backend['artifacts']['codex-agent-linux-amd64']
assert sha(ROOT/artifact['path'])==artifact['sha256']
report={'schema':'codex-agent-full-release-acceptance-v1','passed':True,
        'source_sha256':source['sha256'],'binary_sha256':artifact['sha256'],'frontend':build['files'],
        'frontend_manifest_sha256':sha(UI/'build-manifest.json'),'native_sha256':backend['native']['helper_sha256'],
        'checks':{'full_go':backend['local'],'joint_race':backend['joint'],'native_loopback':backend['native'],
                  'frontend_unit':front['frontend_unit'],'accounts_groups':front['accounts_browser_groups'],'logs_groups':front['logs_browser_groups']},
        'native_enabled_by_user':True,'billing':False,'real_account_requests':0,
        'limits':['Full Go environment skips are reported, not counted as passing.','Production health and UI verification follow deployment; no live model probes.'],
        'evidence':evidence}
OUT.mkdir(exist_ok=True)
with (OUT/'acceptance.json').open('x',encoding='utf-8') as f:json.dump(report,f,indent=2)
print(json.dumps({'passed':True,'source':source['sha256'],'binary':artifact['sha256'],'native':backend['native']['helper_sha256'],'frontend_files':len(build['files'])}))
