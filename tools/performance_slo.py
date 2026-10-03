"""Freeze current sources and measure real loopback HTTP on synthetic PostgreSQL.

One-command 10M run: python tools/performance_slo.py --rows 10000000
Fast PG regression: python tools/performance_slo.py --test-run '^TestPGOperationsSQLParity$'
Retest retained fixture: python tools/performance_slo.py --reuse <previous report.json>
"""
from __future__ import annotations

import argparse
import base64
import datetime as dt
import gzip
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import re
import secrets
import shlex
import shutil
import subprocess
import time
from concurrent.futures import ThreadPoolExecutor
from itertools import product

from integration_env import ROOT, REMOTE, connect, run, write_remote
from gateway_source_race import isolated_environment
from project_layout import backend_source_paths, server_package
from run_linux_checks import scrub


# Retained synthetic fixtures used by the resource-gate regression predate
# migration 018.  This exception is intentionally narrow: it must never turn
# the migration guard into a general bypass for a newer candidate.
ALLOWED_MIGRATION_DELTA_NAME = 'migrations/018_proxy_resource_write_gate.sql'
ALLOWED_MIGRATION_DELTA_SHA256 = '95068285b8a2f06a3b3d9563467fd2c74d6a8d771364e4bb3978ee75aea72ace'


def validate_allowed_migration_delta(actual, expected):
    """Validate the one retained-fixture migration exception.

    ``actual`` may be a strict subset of ``expected`` only when the sole
    missing migration is 018 and its source digest is the pinned digest above.
    Every applied entry must exist in the candidate and have the exact source
    digest; unknown rows and checksum changes are rejected.
    """
    if not isinstance(actual, dict) or not isinstance(expected, dict):
        raise RuntimeError('retained fixture migration checksums are not an object')
    mismatched = sorted(name for name, digest in actual.items()
                        if expected.get(name) != digest)
    if mismatched:
        raise RuntimeError('applied migration checksum differs from source: ' + ', '.join(mismatched))
    pending = sorted(set(expected) - set(actual))
    if pending != [ALLOWED_MIGRATION_DELTA_NAME]:
        detail = ', '.join(pending) if pending else 'none'
        raise RuntimeError('retained fixture migration delta is not exactly 018: ' + detail)
    if expected.get(ALLOWED_MIGRATION_DELTA_NAME) != ALLOWED_MIGRATION_DELTA_SHA256:
        raise RuntimeError('migration 018 source checksum is not the pinned reviewed digest')
    return pending


def read_container_logs(client, identifier):
    """Transfer full diagnostic evidence without streaming huge SQL literals.

    EXPLAIN plans for 10k-element arrays can be tens of megabytes. Compress the
    transport only: the stored run.log and parsed JSON remain byte-for-byte
    complete, and pipefail preserves a failed docker-log command as an error.
    """
    if not re.fullmatch(r'[0-9a-f]{64}', identifier):
        raise ValueError('invalid isolated test container identifier')
    pipeline = 'docker logs ' + identifier + ' 2>&1 | gzip -1 | base64'
    encoded = run(client, shlex.join(['bash', '-o', 'pipefail', '-c', pipeline]), timeout=90)
    return gzip.decompress(base64.b64decode(encoded)).decode('utf-8', 'replace')


def upload_binary(client, binary, remote):
    packed=gzip.compress(binary.read_bytes(),compresslevel=1,mtime=0)
    parts=[]
    for offset in range(0,len(packed),4*1024*1024):
        content=packed[offset:offset+4*1024*1024]
        digest=hashlib.sha256(content).hexdigest()
        parts.append((remote+'/part-'+str(len(parts))+'-'+digest,content,digest))
    def upload(part):
        path,content,digest=part
        for attempt in range(2):
            connection=connect()
            try:
                existing=run(connection,'if test -f '+shlex.quote(path)+'; then sha256sum '+shlex.quote(path)+'; fi').strip()
                if not existing or existing.split()[0]!=digest:
                    with connection.open_sftp() as sftp:
                        sftp.get_channel().settimeout(90)
                        with sftp.file(path,'wb') as handle:handle.set_pipelined(True);handle.write(content)
                    if run(connection,'sha256sum '+shlex.quote(path)).split()[0]!=digest:raise RuntimeError('upload part checksum mismatch')
                print(json.dumps({'uploaded_part':path.rsplit('/',1)[-1],'bytes':len(content)}),flush=True)
                return
            except Exception:
                if attempt:raise
            finally:connection.close()
    with ThreadPoolExecutor(max_workers=4) as pool:list(pool.map(upload,parts))
    run(client,'cat '+' '.join(shlex.quote(p[0]) for p in parts)+' | gzip -d > '+shlex.quote(remote+'/server.test')+'\nchmod 700 '+shlex.quote(remote+'/server.test'))


def freeze(output,source_root=ROOT,overlay_harness=True):
    source = output / 'source'
    manifest = {}
    for path in backend_source_paths(source_root):
        name = path.relative_to(source_root).as_posix()
        target = source / name
        target.parent.mkdir(parents=True, exist_ok=True)
        shutil.copy2(path, target)
        manifest[name] = hashlib.sha256(target.read_bytes()).hexdigest()
    # Compare implementations using identical current fixture/HTTP checks.
    harness='internal/server/performance_slo_scale_test.go'
    if overlay_harness:
        for harness in ('internal/server/performance_slo_scale_test.go','internal/server/performance_slo_uniform_test.go','internal/server/performance_additional_reads_test.go','internal/server/performance_complex_filters_test.go'):
            if (ROOT/harness).exists():
                shutil.copy2(ROOT/harness,source/harness)
                manifest[harness]=hashlib.sha256((source/harness).read_bytes()).hexdigest()
    digest = hashlib.sha256(json.dumps(manifest, sort_keys=True).encode()).hexdigest()
    (output / 'source-manifest.json').write_text(json.dumps({'sha256':digest,'files':manifest},indent=2),encoding='utf-8')
    return source, digest


def inventory(source):
    routes = []
    for path in (source / 'internal/server').glob('*.go'):
        if path.name.endswith('_test.go'):
            continue
        raw=path.read_text(encoding='utf-8')
        variables={name:[value] for name,value in re.findall(r'\b(\w+)\s*:=\s*"([^"]+)"',raw)}
        for name,values in re.findall(r'for\s+_,\s*(\w+)\s*:=\s*range\s*\[\]string\{([^}]+)\}',raw):
            variables[name]=re.findall(r'"([^"]+)"',values)
        discovered=[]
        for expression,handler in re.findall(r'mux\.HandleFunc\(([^\n]+?),\s*p\.(\w+)\)',raw):
            terms=[term.strip() for term in expression.split('+')]
            values=[]
            for term in terms:
                if term.startswith('"') and term.endswith('"'):values.append([json.loads(term)])
                elif term in variables:values.append(variables[term])
                else:raise ValueError('Unresolved route registration in '+path.name+': '+expression)
            discovered.extend((''.join(items),handler) for items in product(*values))
        for registration,handler in discovered:
            method,route=registration.split(' ',1)
            category = 'local_read' if method == 'GET' else 'local_write'
            if any(word in route for word in ('test-stream','/refresh','/upstream','/oauth','/reauthorize','/sync/crs','/proxies/test','/api-call')):
                category = 'upstream_or_streaming'
            elif '/export' in route or handler in ('handleCPADownload','sub2ExportData'):
                category = 'bulk_export'
            if method=='GET' and handler=='handleManagedUpstreamSnapshot':category='local_read'
            if handler=='handleCPAModels':category='upstream_or_streaming'
            routes.append({'method':method,'path':route,'handler':handler,'category':category,'file':path.relative_to(source).as_posix(),'coverage':'not_measured'})
        if path.name=='web.go':
            for route,handler in re.findall(r'mux\.Handle\("([^"]+)",\s*([^\n]+)\)',raw):
                if route in ('/','/api/','/api/v1/admin/','/v0/management/'):continue
                routes.append({'method':'ANY','path':route,'handler':handler,'category':'local_read' if route=='/health' else 'upstream_or_streaming','file':path.relative_to(source).as_posix(),'coverage':'not_measured'})
    return routes


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--rows',type=int,default=10000000)
    parser.add_argument('--samples',type=int,default=20)
    parser.add_argument('--phase',choices=('all','seed','measure'),default='all')
    parser.add_argument('--reuse',type=Path)
    parser.add_argument('--cases',default='')
    parser.add_argument('--test-run',default='^TestPerformanceSLOScale$')
    parser.add_argument('--output',type=Path)
    parser.add_argument('--source',type=Path,default=ROOT,help='Implementation source; current SLO test is overlaid and included in the manifest')
    parser.add_argument('--exact-source',action='store_true',help='Preserve all tests from --source too; no current harness overlay')
    parser.add_argument('--binary-report',type=Path,help='Reuse a verified already uploaded test binary from this report; requires --reuse and --remote-binary, no build/upload/migrations')
    parser.add_argument('--remote-binary',help='Existing server.test under the isolated checks directory')
    parser.add_argument('--uniform-migrate',action='store_true',help='Explicitly migrate an existing perf_uniform schema before HTTP sampling')
    parser.add_argument('--uniform-rebuild',action='store_true',help='Measure actual durable rebuild on uniform fixture and verify totals/hourly/legacy invariants')
    parser.add_argument('--uniform-maintenance-only',action='store_true',help='Stop uniform fixture test before HTTP timing, after explicitly requested maintenance/invariant checks')
    parser.add_argument('--import-sizes',help='Explicit synthetic bulk import sizes, e.g. 100,1000,10000; used only by opt-in bulk-write test')
    parser.add_argument('--functional-only',action='store_true',help='For opt-in mixed/write tests, retain timing as diagnostic only; never claim SLO acceptance')
    parser.add_argument('--prepare-only',action='store_true',help='Freeze/build/upload and verify the test binary only; do not create credentials or run any test')
    parser.add_argument('--apply-missing-migrations',action='store_true',help='With reused binary, explicitly allow missing migrations on the retained synthetic burst fixture; applied checksums must still match')
    parser.add_argument('--standalone-test',action='store_true',help='Run an isolated PG/unit selector from a reused binary without requiring a retained performance schema')
    parser.add_argument('--allow-migration-delta',action='store_true',help='Allow only the pinned migration 018 delta on a retained synthetic fixture')
    parser.add_argument('--timeout-minutes',type=int,default=90)
    args=parser.parse_args()
    if args.uniform_migrate and (args.test_run!='^TestPerformanceUniformHTTP$' or not args.reuse):parser.error('--uniform-migrate is only supported for retained uniform HTTP fixtures')
    if args.uniform_rebuild and (args.test_run!='^TestPerformanceUniformHTTP$' or not args.reuse):parser.error('--uniform-rebuild requires an existing uniform HTTP fixture')
    if args.uniform_maintenance_only and args.test_run!='^TestPerformanceUniformHTTP$':parser.error('--uniform-maintenance-only is only for uniform fixture maintenance')
    if args.import_sizes and not re.fullmatch(r'(?:100|1000|10000)(?:,(?:100|1000|10000))*',args.import_sizes):parser.error('--import-sizes accepts 100,1000,10000 only')
    if args.apply_missing_migrations and (not args.binary_report or not args.reuse or 'Scale' not in args.test_run and args.test_run!='^TestPerformanceMaintenanceMigrations$'):parser.error('--apply-missing-migrations requires reused binary plus explicit retained fixture migration test')
    if args.standalone_test and (not args.binary_report or not args.reuse or 'TestPG' not in args.test_run):parser.error('--standalone-test requires a reused binary and a TestPG selector')
    if args.allow_migration_delta and (not args.reuse or not args.binary_report):parser.error('--allow-migration-delta requires --reuse and --binary-report')
    if args.allow_migration_delta and args.standalone_test:parser.error('--allow-migration-delta cannot be combined with --standalone-test')
    binary_source=None
    if args.binary_report:
        if not args.reuse or not args.remote_binary or not re.fullmatch(re.escape(REMOTE)+r'/checks/performance-slo-[a-z0-9-]+/server\.test',args.remote_binary):
            parser.error('--binary-report requires --reuse and the existing isolated --remote-binary')
        binary_source=json.loads(args.binary_report.read_text(encoding='utf-8'))
        if binary_source.get('synthetic_only') is not True:parser.error('binary source must be synthetic')
        args.source=args.binary_report.resolve().parent/'source';args.exact_source=True
    if not 1000<=args.rows<=10000000 or not 1<=args.samples<=200:
        parser.error('rows must be 1000..10000000; samples must be 1..200')
    stamp=dt.datetime.now(dt.timezone.utc).strftime('%Y%m%dT%H%M%S').lower()+'-'+secrets.token_hex(3)
    output=(args.output or ROOT/'.local/performance-slo'/stamp).resolve()
    output.mkdir(parents=True,exist_ok=False)
    schema='perf_slo_'+stamp.replace('-','_')
    if args.reuse:
        prior=json.loads(args.reuse.read_text(encoding='utf-8'))
        if prior.get('synthetic_only') is not True: raise ValueError('reuse report must be synthetic')
        schema=prior['schema'];args.rows=prior.get('requested_rows',prior.get('rows'));args.phase='measure'
    if args.test_run=='^TestPerformanceUniformHTTP$':
        allowed_schema=r'perf_uniform_[a-z0-9_]{8,60}'
    elif args.test_run in ('^TestPerformanceOverviewPlan$','^TestPerformanceMaintenanceMigrations$','^TestPerformancePhase3FailurePlans$'):
        allowed_schema=r'perf_(slo|uniform)_[a-z0-9_]{8,60}'
    else:
        allowed_schema=r'perf_slo_[a-z0-9_]{8,60}'
    if not re.fullmatch(allowed_schema,schema): raise ValueError('invalid synthetic schema')
    values,private=isolated_environment('codex-agent:integration:slo:'+stamp+':')
    values.update(CGO_ENABLED='0',GOMAXPROCS='2',STATS_TIMEZONE='UTC',PERFORMANCE_SLO='1',PERFORMANCE_SLO_SCHEMA=schema,
                  PERFORMANCE_SLO_ROWS=str(args.rows),PERFORMANCE_SLO_SAMPLES=str(args.samples),PERFORMANCE_SLO_PHASE=args.phase,
                  PERFORMANCE_SLO_CASES=args.cases)
    if args.uniform_migrate:values['PERFORMANCE_UNIFORM_MIGRATE']='1'
    if args.uniform_rebuild:values['PERFORMANCE_UNIFORM_REBUILD']='1'
    if args.uniform_maintenance_only:values['PERFORMANCE_UNIFORM_MAINTENANCE_ONLY']='1'
    if args.import_sizes:
        values['PERFORMANCE_IMPORT_SIZES']=args.import_sizes
        values['PERFORMANCE_RESOURCE_IMPORT_SIZES']=args.import_sizes
    if args.functional_only:
        values['PERFORMANCE_LOCAL_WRITE_FUNCTIONAL']='1';values['PERFORMANCE_TEST_INTERFERENCE']='explicit functional-only run; may share resources with fixture maintenance'
    report={'status':'running','recorded_at_utc':stamp,'synthetic_only':True,'production_touched':False,'schema':schema,
            'requested_rows':args.rows,'samples_per_concurrency':args.samples,'phase':args.phase,'test_run':args.test_run,
            'runner_sha256':hashlib.sha256(Path(__file__).read_bytes()).hexdigest(),
            'driver_resources':{'cpus':2,'memory_bytes':2147483648},'slo_ms':3000 if args.rows>=10000000 else 1000,
            'scope':'real loopback HTTP + actual admin routing/auth/JSON + isolated same-host PostgreSQL; upstream inference and WAN excluded',
            'cold_scope':'first request of endpoint in fresh test process, shared application state may already be warm; OS/PG caches uncontrolled',
            'events':{},'limitations':['Read endpoint sampling is not complete route coverage','Bulk fixture construction is not ingestion throughput','10M events/logs, 1000 accounts; not 10M accounts','Deterministic 30-day bursts produce about 30k account-minute buckets; high-cardinality per-record dimensions are not modeled','No physical disk-cold cache eviction on shared host']}
    report['functional_only']=args.functional_only
    if args.test_run=='^TestPerformanceUniformHTTP$':
        report['distribution']='uniform30days'
        report['limitations']=['Six HTTP shapes only; not complete route coverage','1000 synthetic accounts and 43200 minutes; distinct account-minute per event','Real account documents and durable aggregates are constructed before sampling; fixture preparation is not ingestion throughput','Old retained rolling24h windows may now be empty; populated operations requires a separate recent fixture','OS/PG caches uncontrolled; no physical cold-cache eviction']
    client=None;identifier='';remote=REMOTE+'/checks/performance-slo-'+stamp;envpath=remote+'/test.env'
    try:
        source,digest=freeze(output,args.source.resolve(),not args.exact_source)
        report['implementation_source']=str(args.source.resolve())
        report['source_sha256']=digest;report['route_inventory']=inventory(source)
        report['route_inventory_scope']='ServeMux method registrations plus gateway dispatch prefixes; a prefix may dispatch multiple protocol paths'
        if binary_source:
            if digest!=binary_source['source_sha256']:raise RuntimeError('source of reused binary no longer matches its manifest')
            report['binary_sha256']=binary_source['binary_sha256'];report['go_version']=binary_source.get('go_version')
            report['binary_reused_from']=str(args.binary_report.resolve());report['remote_binary']=args.remote_binary
            print(json.dumps({'reused_binary':report['binary_sha256'],'source':digest,'report':str(output/'report.json'),'schema':schema}),flush=True)
        else:
            env=os.environ.copy();env.update(GOOS='linux',GOARCH='amd64',CGO_ENABLED='0',GOWORK='off')
            binary=output/'server.test'
            built=subprocess.run(['go','test','-c','-ldflags=-s -w','-o',str(binary),server_package(source)],cwd=source,env=env,capture_output=True,timeout=300)
            (output/'build.log').write_bytes(built.stdout+built.stderr)
            if built.returncode:raise RuntimeError('build failed; inspect build.log')
            report['binary_sha256']=hashlib.sha256(binary.read_bytes()).hexdigest()
            report['go_version']=subprocess.check_output(['go','version'],text=True).strip()
            print(json.dumps({'built':report['binary_sha256'],'source':digest,'report':str(output/'report.json'),'schema':schema}),flush=True)
        client=connect()
        run(client,'test -f '+shlex.quote(REMOTE+'/.codex-agent-integration'))
        image=run(client,"docker image inspect --format '{{.Id}}' postgres:18-alpine").strip()
        report['postgres_resources']=json.loads(run(client,"docker inspect --format '{{json .HostConfig}}' codex-agent-integration-postgres"))
        report['postgres_resources']={k:report['postgres_resources'].get(k) for k in ('Memory','NanoCpus','CpuQuota','CpuPeriod')}
        settings="SELECT json_object_agg(name,setting) FROM pg_settings WHERE name IN ('server_version','shared_buffers','work_mem','max_parallel_workers_per_gather','jit','effective_cache_size')"
        report['postgres_settings']=json.loads(run(client,shlex.join(['docker','exec','codex-agent-integration-postgres','psql','-X','-U','postgres','-Atc',settings])))
        run(client,'install -d -m 700 '+shlex.quote(remote))
        binary_folder=remote
        if binary_source:
            binary_folder=str(PurePosixPath(args.remote_binary).parent)
            query='SELECT json_object_agg(name,checksum) FROM '+schema+'.schema_migrations'
            if args.standalone_test:
                report['migration_preflight']='standalone PostgreSQL selector; no retained performance schema required'
                actual=None
            else:
                actual=json.loads(run(client,shlex.join(['docker','exec','codex-agent-integration-postgres','psql','-X','-U','postgres','-d','codex_agent_integration','-Atc',query])))
            expected={'migrations/'+p.name:hashlib.sha256(p.read_bytes()).hexdigest() for p in (source/'migrations').glob('*.sql')}
            if args.standalone_test:
                pass
            elif args.allow_migration_delta:
                report['pending_migrations']=validate_allowed_migration_delta(actual, expected)
                report['migration_preflight']='applied checksums match; only pinned migration 018 is pending'
            elif args.apply_missing_migrations or args.uniform_migrate:
                prefixes=('perf_slo_','perf_uniform_') if args.test_run=='^TestPerformanceMaintenanceMigrations$' else (('perf_uniform_',) if args.uniform_migrate else ('perf_slo_',))
                if not schema.startswith(prefixes) or any(expected.get(name)!=digest for name,digest in actual.items()):raise RuntimeError('applied migration checksum differs from source; cannot apply missing migrations')
                report['pending_migrations']=sorted(expected.keys()-actual.keys())
                report['migration_preflight']='applied checksums match; explicitly authorized missing migrations run against the retained synthetic fixture'
            else:
                if actual!=expected:raise RuntimeError('reused fixture migration checksums differ; no migration will be attempted')
                report['migration_preflight']='all existing migration checksums match; no new migration required'
        else:upload_binary(client,binary,remote)
        if run(client,'sha256sum '+shlex.quote(binary_folder+'/server.test')).split()[0]!=report['binary_sha256']:raise RuntimeError('binary SHA mismatch')
        report['remote_binary']=binary_folder+'/server.test'
        if args.prepare_only:
            report.update(status='prepared_not_executed',remote_binary=binary_folder+'/server.test',all_routes_slo_proven=False)
            print(json.dumps({'status':report['status'],'report':str(output/'report.json'),'remote_binary':report['remote_binary']}),flush=True)
            return 0
        write_remote(client,envpath,''.join(k+'='+v+'\n' for k,v in values.items()))
        command=['docker','run','-d','--name','codex-slo-'+stamp,'--label','codex-agent.integration=performance-slo','--cpus','2','--memory','2g','--pids-limit','128','--network','host','--cap-drop','ALL','--security-opt','no-new-privileges','--env-file',envpath,'-v',binary_folder+':/evidence:ro','--entrypoint','/evidence/server.test',image,'-test.run='+args.test_run,'-test.v','-test.count=1','-test.timeout='+str(args.timeout_minutes)+'m']
        identifier=run(client,shlex.join(command)).strip()
        if not re.fullmatch(r'[0-9a-f]{64}',identifier):raise RuntimeError('invalid container identity')
        report['container_id']=identifier
        profile=output/'resource-profile.json'
        if profile.exists():report['resource_profile_changes']=json.loads(profile.read_text(encoding='utf-8'))
        (output/'report.json').write_text(json.dumps(report,indent=2),encoding='utf-8')
        deadline=time.monotonic()+args.timeout_minutes*60+30;seen=0
        poll_started=time.monotonic()
        while True:
            state=json.loads(run(client,"docker inspect --format '{{json .State}}' "+identifier))
            raw=scrub(read_container_logs(client,identifier),private)
            (output/'run.log').write_text(raw,encoding='utf-8')
            progress=[line.strip() for line in raw.splitlines() if 'PERFORMANCE_SLO_' in line]
            for line in progress[seen:]:
                if '_RESULT ' not in line:
                    if len(line)>1500:
                        print(line[:500]+'... [full evidence in run.log]',flush=True)
                    else:print(line,flush=True)
            seen=len(progress)
            report['container_running']=state['Running']
            report['elapsed_runner_s']=round(time.monotonic()-poll_started,3)
            report['last_progress']=progress[-1] if progress else (raw.splitlines()[-1] if raw else 'starting')
            report['last_checked_at_utc']=dt.datetime.now(dt.timezone.utc).isoformat()
            (output/'report.json').write_text(json.dumps(report,indent=2),encoding='utf-8')
            if not state['Running']:break
            if time.monotonic()>deadline:raise TimeoutError('synthetic runner exceeded deadline; incomplete data is failure')
            time.sleep(5)
        report['exit_code']=state['ExitCode']
        for kind,payload in re.findall(r'PERFORMANCE_SLO_(\w+) (\{[^\n]+\})',raw):
            report['events'].setdefault(kind,[]).append(json.loads(payload))
        measured=report['events'].get('RESULT',[])+report['events'].get('FIRST',[])
        measured+=report['events'].get('FILTER_SAMPLE',[])
        measured+=report['events'].get('ACCOUNT_WRITE',[])
        measured += [dict(row,**row['route']) for row in report['events'].get('WRITE_RESULT',[]) if row.get('route')]
        static_paths={(r['method'],r['path']) for r in report['route_inventory'] if '{' not in r['path']}
        for route in report['route_inventory']:
            pattern=re.escape(re.sub(r'\{[^}]+\}','@PARAM@',route['path'])).replace('@PARAM@','[^/]+')
            matched=[row for row in measured if route['method']==row.get('method','GET') and re.fullmatch(pattern,row['path'].split('?')[0])
                     and ('{' not in route['path'] or (row.get('method','GET'),row['path'].split('?')[0]) not in static_paths)]
            if matched:
                route['coverage']='sampled_shape_only'
                if any(row.get('failed',0) or row.get('over_slo',0) or row.get('slo_pass') is False or row.get('sample',{}).get('error') for row in matched):route['coverage']='sampled_shape_with_failure'
        report['failed_tests']=re.findall(r'^\s*--- FAIL: (\S+)',raw,re.M)
        report['passed_tests']=re.findall(r'^\s*--- PASS: (\S+)',raw,re.M)
        report['skipped_tests']=re.findall(r'^\s*--- SKIP: (\S+)',raw,re.M)
        report['status']='pass' if state['ExitCode']==0 and report['passed_tests'] and not report['skipped_tests'] else 'fail'
        if args.test_run=='^TestPerformanceSLOScale$' and args.phase!='seed' and not report['events'].get('RESULT'):report['status']='fail'
        report['all_routes_slo_proven']=False
    except Exception as error:
        report.update(status='fail',error=scrub(str(error),private))
    finally:
        if client:
            if re.fullmatch(r'[0-9a-f]{64}',identifier):
                try:run(client,'docker rm -f '+identifier)
                except Exception:report['container_cleanup_failed']=True
            try:
                with client.open_sftp() as sftp:sftp.remove(envpath)
            except Exception:pass
            client.close()
        profile=output/'resource-profile.json'
        if profile.exists():report['resource_profile_changes']=json.loads(profile.read_text(encoding='utf-8'))
        (output/'report.json').write_text(json.dumps(report,indent=2),encoding='utf-8')
    print(json.dumps({'status':report['status'],'report':str(output/'report.json'),'error':report.get('error')}),flush=True)
    return 0 if report['status']=='pass' else 1


if __name__=='__main__':raise SystemExit(main())
