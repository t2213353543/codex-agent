"""Assemble checked PowerPoint migration evidence for the release packager."""
import argparse
import hashlib
import json
from pathlib import Path

from acceptance import ROOT, source_manifest
from project_layout import backend_source_paths

def read(path):return json.loads(Path(path).read_text(encoding='utf-8-sig'))
def digest(path):return hashlib.sha256(Path(path).read_bytes()).hexdigest()
def require(value,message):
    if not value:raise RuntimeError(message)

def main():
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument('--live-run',type=Path,required=True)
    p.add_argument('--race',type=Path,required=True)
    p.add_argument('--storage',type=Path,required=True)
    p.add_argument('--delta-review',type=Path)
    p.add_argument('--output',type=Path,required=True)
    args=p.parse_args()
    live=read(args.live_run/'result.json');require(live['exit_code']==0,'Live suite did not pass')
    live_rows=[]
    for line in (args.live_run/'live.log').read_text(encoding='utf-8').splitlines():
        index=line.find('{"case":')
        if index<0:continue
        try:row=json.loads(line[index:])
        except ValueError:continue
        if 'cli' in row:live_rows.append(row)
    expected={'file','resume','compact','apply_patch','search','mcp_parallel','candy_account_1','candy_account_2','ws_file','ws_resume','ws_compact'}
    require({x['case'] for x in live_rows}==expected and len(live_rows)==11,'Live suite coverage differs')
    require(all(x['passed'] and x['cli']['completed'] and x['cli']['exit_code']==0 for x in live_rows),'Live case failed')
    backend={x.relative_to(ROOT).as_posix():digest(x) for x in backend_source_paths(ROOT)}
    previous=read(args.live_run/'source.json')['files']
    changed=sorted(k for k in set(previous)|set(backend) if previous.get(k)!=backend.get(k))
    delta=None
    if changed:
        require(args.delta_review is not None,'Backend differs from live-tested source; review delta explicitly')
        delta=read(args.delta_review)
        require(delta.get('passed') is True and delta.get('changed_files')==changed,'Delta review does not cover changed files')
        require(all(delta.get('current_sha256',{}).get(k)==backend[k] for k in changed),'Delta review is stale')
    race=read(args.race);require(race.get('status')=='pass' and race.get('exit_code')==0 and not race.get('race_warning') and not race.get('source_changed_after_freeze'),'Linux race failed or source changed')
    redis=race.get('redis_test_results',{});require(len(redis)==4 and all(v=='pass' for v in redis.values()),'Actual Redis tests missing')
    race_source=read(args.race.parent/'source-manifest.json')
    frozen=race_source.get('source_files') or race_source.get('files')
    require(isinstance(frozen,dict),'Race source manifest missing')
    require(frozen==backend,'Current backend differs from final race snapshot')
    storage=read(args.storage);require(storage.get('status')=='pass' and storage.get('passed',0)>=20 and storage.get('exit_code')==0,'Isolated storage checks did not pass')
    go_log=ROOT/'.local/powerpoint-migration/final-go.jsonl'
    go=[json.loads(x) for x in go_log.read_text(encoding='utf-8-sig').splitlines()]
    require(not any(x['Action']=='fail' for x in go),'Go test failure')
    require(all(any(x['Action']=='pass' and x['Package']==name and not x.get('Test') for x in go) for name in ['codex-agent/internal/server','codex-agent/internal/bps']),'Go package test completion missing')
	
    vet_path=ROOT/'.local/powerpoint-migration/vet.json';vet=read(vet_path)
    require(vet.get('passed') is True and vet.get('exit_code')==0,'Go vet evidence missing')
    if delta:
        require(all(any(x['Action']=='pass' and x.get('Test')==name for x in go) for name in delta['tests']),'Delta tests not in current Go run')
    browser_path=ROOT/'frontend/test-results/settings/results.json';browser=read(browser_path)
    require(browser.get('passed') is True and not browser.get('errors'),'Frontend browser failed')
    unit=ROOT/'.local/powerpoint-migration/frontend-unit.log';build=ROOT/'.local/powerpoint-migration/frontend-build.log'
    require('fail 0' in unit.read_text(encoding='utf-8') and 'pass 175' in unit.read_text(encoding='utf-8'),'Frontend unit evidence missing')
    require('built in' in build.read_text(encoding='utf-8'),'Frontend build evidence missing')
    schema_path=ROOT/'.local/powerpoint-migration/schema-evidence.json';schema=read(schema_path)
    actual_sql={'migrations/'+x.name:digest(x) for x in (ROOT/'migrations').glob('*.sql')}
    require(schema.get('passed') and schema['checksums']==actual_sql,'Schema changed')
    evidence=[args.live_run/'result.json',args.live_run/'live.log',args.live_run/'source.json',args.race,args.race.parent/'source-manifest.json',args.storage,go_log,vet_path,browser_path,unit,build,schema_path]
    if args.delta_review:evidence.append(args.delta_review)
    result={'passed':True,'source_sha256':source_manifest()['sha256'],
        'checks':{k:True for k in ['go','vet','frontend','live_cli','live_ws','race','redis','schema_unchanged']},
        'test_counts':{'go_pass_including_subtests':sum(x['Action']=='pass' and bool(x.get('Test')) for x in go),'live':11,'storage':storage['passed'],'frontend_unit':175},
        'live_source_delta_review':delta,
        'evidence_files':[{'path':x.resolve().relative_to(ROOT).as_posix(),'sha256':digest(x)} for x in evidence]}
    args.output.parent.mkdir(parents=True,exist_ok=True)
    args.output.write_text(json.dumps(result,ensure_ascii=False,indent=2),encoding='utf-8')
    print(str(args.output))

if __name__=='__main__':main()
