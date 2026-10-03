"""Bind completed runtime, storage and UI evidence to the release source tree."""
import argparse
import hashlib
import importlib.util
import json
from pathlib import Path
import re
from acceptance import ROOT, source_manifest
from frontend_release_gates import verify_report as verify_frontend_report

spec=importlib.util.spec_from_file_location('release_gate',ROOT/'deploy/reliability_release.py')
gate=importlib.util.module_from_spec(spec);spec.loader.exec_module(gate)


def read(path):return json.loads(Path(path).read_text(encoding='utf-8-sig'))
def sha(path):return hashlib.sha256(Path(path).read_bytes()).hexdigest()
def refs(paths):return [{'path':Path(path).resolve().relative_to(ROOT).as_posix(),'sha256':sha(path)} for path in paths]
def require(value,message):
    if not value:raise RuntimeError(message)


def source_differences(tested, current, scope='backend'):
    before=gate.source_scope(tested,scope);after=gate.source_scope(current,scope)
    return {name for name in set(before)|set(after) if before.get(name)!=after.get(name)}


def require_runtime_sources(tested, current):
    require(not source_differences(tested,current,'runtime'),'Tested runtime differs from current release source')
    # Preserve the explicitly reviewed pure-test exception; additions and
    # deletions of any other executed fixture still invalidate the evidence.
    require(source_differences(tested,current) <= {'internal/server/gateway_account_failover_test.go'},
            'Executed test or fixture changed after integration validation')


def powerpoint_fixtures(files):
    return {name:value for name,value in gate.source_scope(files,'backend').items()
            if name.endswith('_test.go') and not Path(name).name.startswith(('gateway_official_','official_'))
            and name!='internal/server/gateway_account_failover_test.go'}


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--bps',type=Path,required=True)
    parser.add_argument('--bps-429-recovery',type=Path,help='Verify the same-source apply_patch 429 recovery plus the three omitted live suites; retain both raw run outcomes')
    parser.add_argument('--bps-regression-base',type=Path,help='Reuse a verified complete suite only for the reviewed cache/account-rejection patch; require current live tests and the current complete Go/race/storage gates')
    parser.add_argument('--bps-current-upstream-403',action='store_true',help='Preserve current live gate as failed when both reviewed live probes are explicitly rejected with upstream 403; require the full current local gates and verified regression base')
    parser.add_argument('--official-only-upstream-bps-403',action='store_true',help='Keep BPS live marked failed and release only into the already-selected App Server mode after verified upstream 403')
    parser.add_argument('--official',type=Path)
    parser.add_argument('--bps-only-retired-official',action='store_true',help='Require complete BPS live evidence and record the removed hosted runtime as not applicable')
    parser.add_argument('--race',type=Path,required=True)
    parser.add_argument('--race-completion',type=Path,help='Complete only unfinished tests after a same-source package timeout; preserve the original failed run')
    parser.add_argument('--pg',type=Path,required=True)
    parser.add_argument('--work',type=Path,default=ROOT/'.local/reliability-modes')
    parser.add_argument('--output',type=Path,default=ROOT/'.local/reliability-modes/acceptance.json')
    args=parser.parse_args();current=source_manifest();backend=gate.source_scope(current['files'],'backend');front=gate.source_scope(current['files'],'frontend')
    require(not(args.bps_only_retired_official and args.official_only_upstream_bps_403),'incompatible verification scopes')
    require(not args.bps_429_recovery or args.bps_only_retired_official and not args.bps_regression_base and not args.bps_current_upstream_403,'429 recovery requires current-source BPS scope')
    require(not args.bps_current_upstream_403 or args.bps_regression_base and args.bps_only_retired_official,'upstream limitation requires the reviewed composed scope')
    work=args.work.resolve()
    def matched(files):require(not source_differences(files,current['files']),'Tested backend differs from current release source')
    def runtime_matched(files):
        require_runtime_sources(files,current['files'])
    def live(folder,expected,bps=False,verify_sources=True):
        result=read(folder/'result.json');require(result['exit_code']==0,'Live suite failed')
        if not verify_sources:
            pass
        elif bps:
            tested=read(folder/'source.json')
            require(gate.source_scope(tested,'powerpoint_runtime')==gate.source_scope(current['files'],'powerpoint_runtime'),'Tested PowerPoint runtime differs')
            require(powerpoint_fixtures(tested)==powerpoint_fixtures(current['files']),'Executed PowerPoint fixture changed')
        else:runtime_matched(read(folder/'source.json'))
        log=(folder/'live.log').read_text(encoding='utf-8');require('--- FAIL:' not in log and '\nPASS\n' in log,'Live log did not finish successfully')
        for name in expected:require(re.search(r'--- PASS: '+re.escape(name)+r'\b',log) is not None,'Missing live case '+name)
        return [folder/'result.json',folder/'source.json',folder/'live.log']
    bps_cases=['TestBPSLiveCodexIntegration/'+x for x in ['file','resume','compact','apply_patch','search','mcp_parallel','candy_account_1']]+['TestBPSLiveCodexWebSocket/'+x for x in ['file','resume','compact']]
    # The original websocket subtest names have a ws_ prefix in case records,
    # while Go's test names are verified from the actual suite below.
    regression=args.bps_regression_base or args.bps
    bps_log=(regression/'live.log').read_text(encoding='utf-8')
    reused_delta=None
    recovered_live=None
    if args.bps_429_recovery:
        from bps_live_recovery_evidence import verify
        from project_layout import backend_source_paths
        live_sources={path.relative_to(ROOT).as_posix():sha(path) for path in backend_source_paths(ROOT)}
        bps_paths,recovered_live=verify(args.bps,args.bps_429_recovery,live_sources)
    elif args.official_only_upstream_bps_403:
        bps_result=read(args.bps/'result.json');runtime_matched(read(args.bps/'source.json'))
        require(bps_result['exit_code']!=0 and '403: This request was blocked by our usage policy.' in bps_log,'BPS failure is not the verified upstream 403')
        bps_paths=[args.bps/'result.json',args.bps/'source.json',args.bps/'live.log']
    else:
        ws_names=re.findall(r'--- PASS: (TestBPSLiveCodexWebSocket/[^ ]+)',bps_log)
        require(len(ws_names)==3,'Three real websocket cases are required')
        if args.bps_regression_base:
            require(args.bps_only_retired_official,'composed BPS evidence requires retired-runtime release scope')
            baseline=read(regression/'source.json')
            now_runtime=gate.source_scope(current['files'],'runtime');old_runtime=gate.source_scope(baseline,'runtime')
            changed={k for k in set(now_runtime)|set(old_runtime) if now_runtime.get(k)!=old_runtime.get(k)}
            allowed={'internal/server/main.go','internal/server/gateway_bps.go','internal/server/gateway_bps_default_cache.go','internal/server/gateway_bps_failover.go','internal/server/gateway_official_failover.go'}
            require(changed<=allowed,'composed evidence contains an unreviewed runtime change')
            require(all(baseline.get(k)==v for k,v in backend.items() if Path(k).name in ('gateway_bps_live_test.go','gateway_bps_websocket_live_test.go')),'reused live fixtures changed')
            bps_paths=live(regression,bps_cases[:7]+ws_names,True,False)
            if args.bps_current_upstream_403:
                actual=read(args.bps/'result.json');runtime_matched(read(args.bps/'source.json'))
                log=(args.bps/'live.log').read_text(encoding='utf-8')
                require(actual['exit_code']==1,'expected actual upstream rejection evidence')
                failed=set(re.findall(r'^--- FAIL: ([A-Za-z0-9_]+) ',log,re.M))
                require(failed=={'TestBPSLiveGeneratedDefaultCache','TestBPSLiveRejectedAccountFailover'},'unexpected failed live case')
                require('BPS cache turn=0 status=403 bytes=153' in log and 'real account failover status=403 response_bytes=153' in log,'live rejection is not the observed upstream 403')
                bps_paths += [args.bps/'result.json',args.bps/'source.json',args.bps/'live.log']
            else:
                bps_paths+=live(args.bps,['TestBPSLiveGeneratedDefaultCache','TestBPSLiveRejectedAccountFailover'])
            reused_delta={'base_suite':regression.relative_to(ROOT).as_posix() if regression.is_absolute() else regression.as_posix(),'unchanged_runtime_files':len(now_runtime)-len(changed),'changed_runtime_files':sorted(changed),'current_live_cases':['TestBPSLiveGeneratedDefaultCache','TestBPSLiveRejectedAccountFailover'],'current_full_go_race_and_storage_required':True}
        else:
            bps_paths=live(args.bps,bps_cases[:7]+ws_names,True)
    official_names=['official_cli_native_text','app_server_text_resume_compact','app_server_client_function','hosted_cli_file_and_resume','official_codex_cli_real_cli_tools','official_app_server_real_cli_tools','app_server_native_websocket','app_server_history_retired_tools','app_server_encrypted_reasoning_missing_id']
    if args.bps_only_retired_official:
        main_source=(ROOT/'internal/server/main.go').read_text(encoding='utf-8')
        require('newOfficialRuntime(' not in main_source and 'newAppServerCLI(' not in main_source,'removed runtime is still initialized')
        official_paths=[work/'final-go.jsonl',work/'final-source.json']
    else:
        require(args.official is not None,'official live evidence is required for this scope')
        official_paths=live(args.official,['TestOfficialLiveRuntime/'+name for name in official_names]+['TestOfficialLiveUsageAndCache','TestOfficialLiveClientToolDiscovery','TestOfficialLiveImplicitToolDiscovery','TestOfficialLiveAccountFailover'])
    race=read(args.race/'report.json');runtime_matched(read(args.race/'source-manifest.json')['source_files'])
    race_completion=None
    race_paths=[args.race/'report.json',args.race/'source-manifest.json']
    if args.race_completion:
        from race_composition import verify
        from project_layout import backend_source_paths
        actual_sources={path.relative_to(ROOT).as_posix():sha(path) for path in backend_source_paths(ROOT)}
        race_paths,race_completion=verify(args.race,[args.race_completion],actual_sources)
    else:
        require(race['status']=='pass' and race['exit_code']==0 and not race['race_warning'] and not race['source_changed_after_freeze'],'Race did not pass the current source')
        require(len(race.get('redis_test_results',{}))==4 and all(v=='pass' for v in race['redis_test_results'].values()),'Actual Redis tests incomplete')
        require(race.get('official_test_results') and all(v=='pass' for v in race['official_test_results'].values()),'Official runtime race missing')
    pg=read(args.pg/'report.json');runtime_matched(read(args.pg/'source-manifest.json'))
    require(pg['status']=='pass' and all(pg['test_outcomes'].get(n)=='pass' for n in ['TestPGManagedAccountWindowCosts','TestPGManagedTimingCoverage']),'Actual PostgreSQL tests missing')
    frozen=read(work/'final-source.json');matched(frozen['files'])
    require(gate.source_scope(frozen['files'],'frontend')==front,'Frontend changed after final validation')
    events=[json.loads(line) for line in (work/'final-go.jsonl').read_text(encoding='utf-8').splitlines()]
    require(not any(x['Action']=='fail' for x in events),'Go tests failed')
    packages={x['Package']:x['Action'] for x in events if 'Test' not in x and x['Action'] in ['pass','fail','skip']}
    require(packages.get('codex-agent/internal/server')=='pass' and packages.get('codex-agent/internal/bps')=='pass','Go test completion missing')
    require((work/'final-vet.log').read_text(encoding='utf-8').strip()=='','go vet reported problems')
    unit=(work/'final-frontend-unit.log').read_text(encoding='utf-8');unit_count=re.search(r'pass (\d+)\b',unit)
    require(re.search(r'fail 0\b',unit) and unit_count and int(unit_count.group(1))>=188,'Frontend units incomplete')
    frontend_report=verify_frontend_report(work,front)
    require(read(work/'settings-results.json')['passed'] is True,'Settings browser failed')
    require('Official usage browser passed:' in (work/'final-usage-browser.log').read_text(encoding='utf-8'),'Official usage browser failed')
    require(read(work/'metrics-results.json')['passed'] is True,'App Server metrics browser failed')
    require('built in' in (work/'final-frontend-build.log').read_text(encoding='utf-8'),'Frontend build incomplete')
    parity=read(work/'sub2api-cost-parity.json');require(parity['status']=='pass' and parity['catalog_exact_match'] and parity['cases']>=588,'Sub2API cost parity failed')
    paths={
      'go':[work/'final-go.jsonl',work/'final-source.json'],
      'vet':[work/'final-vet.log',work/'final-source.json'],
      'frontend':[work/'frontend-gates.json',work/'final-frontend-unit.log',work/'final-frontend-build.log',work/'final-settings-browser.log',work/'final-usage-browser.log',work/'final-metrics-browser.log',work/'settings-results.json',work/'metrics-results.json',work/'final-source.json'],
      'bps_live':bps_paths,'official_live':official_paths,
      'race':race_paths,
      'redis':race_paths,
      'pg':[args.pg/'report.json',args.pg/'source-manifest.json',args.pg/'test.jsonl'],
      'cost_parity':[work/'sub2api-cost-parity.json',work/'sub2api-cost-parity-go.jsonl'] if (work/'sub2api-cost-parity-go.jsonl').exists() else [work/'sub2api-cost-parity.json'],
    }
    gates={}
    for name,files in paths.items():
        scope='frontend' if name=='frontend' else 'powerpoint_runtime' if name=='bps_live' and not args.official_only_upstream_bps_403 and not reused_delta else 'runtime' if name in ('bps_live','official_live','race','redis','pg') else 'backend';mapping=gate.source_scope(current['files'],scope)
        if name=='cost_parity':
            scope='files';mapping={key:current['files'][key] for key in ['internal/server/admin_account_window_cost.go','internal/server/account_window_prices.json','internal/server/admin_account_window_parity_test.go']}
        gates[name]={'passed':True,'scope':scope,'source_files':mapping,'source_sha256':gate.map_digest(mapping),'evidence_files':refs(files)}
    gates['frontend']['artifact_files']=frontend_report['artifact_files']
    evidence_files={r['path']:r for value in gates.values() for r in value['evidence_files']}
    result={'passed':True,'source_sha256':current['sha256'],'checks':dict.fromkeys(gate.GATES,True),'gates':gates,'evidence_files':list(evidence_files.values()),
            'counts':{'go_pass':sum(x.get('Test') is not None and x['Action']=='pass' for x in events),'go_skip':sum(x.get('Test') is not None and x['Action']=='skip' for x in events),'frontend':int(unit_count.group(1)),'sub2api_price_cases':parity['cases']},
            'schema_change':'No new migration; existing six-entry ledger and checksums unchanged'}
    if args.official_only_upstream_bps_403:
        result['verification_scope']='official_app_server'
        result['checks']['bps_live']=False
        result['gates']['bps_live']['passed']=False
        result['external_limitations']={'bps_live':{'status':'blocked_upstream','http_status':403,'message':'This request was blocked by our usage policy.'}}
    if args.bps_only_retired_official:
        result['verification_scope']='powerpoint_bps'
        result['retired_features']=['official_app_server','hosted_codex_tasks']
        result['checks']['official_live']=False
        result['gates']['official_live']['passed']=False
    if reused_delta:
        result['bps_composed_verification']=reused_delta
    if recovered_live:
        result['bps_same_source_recovery']=recovered_live
    if race_completion:
        result['race_same_source_completion']=race_completion
    if args.bps_current_upstream_403:
        result['checks']['bps_live']=False
        result['gates']['bps_live']['passed']=False
        result['external_limitations']={'bps_live':{'status':'blocked_upstream','http_status':403,'message':'Current live inference returned HTTP 403'}}
    version_text=(ROOT/'internal/server/main.go').read_text(encoding='utf-8')
    version=re.search(r'const version = "([^"]+)"',version_text).group(1)
    gate.validate_evidence(result,current,version)
    args.output.write_text(json.dumps(result,ensure_ascii=False,indent=2),encoding='utf-8');print(str(args.output))


if __name__=='__main__':main()
