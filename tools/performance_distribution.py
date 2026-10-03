"""Prepare/run a separate uniform30days retained-statistics SQL experiment.

Prepare exact current SQL locally, without PostgreSQL/SSH:
  python tools/performance_distribution.py --rows 1000000 --prepare-only
Execute a NEW isolated synthetic schema (never changes the burst fixture):
  python tools/performance_distribution.py --rows 1000000

This measures SQL EXPLAIN and fixture costs, not HTTP latency or an SLO pass.
"""
from __future__ import annotations

import argparse
import datetime as dt
import hashlib
import json
import os
from pathlib import Path
import re
import secrets
import shlex
import subprocess
import time
from urllib.parse import urlsplit

from gateway_source_race import isolated_environment
from integration_env import ROOT, REMOTE, connect, run, write_remote
from performance_slo import freeze
from run_linux_checks import scrub


CAPTURE_TEST = r'''package server
import (
 "encoding/json"
 "os"
 "strconv"
 "testing"
 "time"
)
func TestPerformanceUniformSQLCapture(t *testing.T) {
 from,e:=strconv.ParseInt(os.Getenv("UNIFORM_FROM"),10,64);if e!=nil {t.Fatal(e)}
 to,e:=strconv.ParseInt(os.Getenv("UNIFORM_TO"),10,64);if e!=nil {t.Fatal(e)}
 q:=managedLogQuery{From:from,To:to,Step:86400,Timezone:"UTC"}
 sql,args:=managedUsageRollupQuery(q,managedUsageStats{Step:86400})
 types:=make([]string,len(args))
 for i,arg:=range args {switch arg.(type) {case time.Time:types[i]="timestamptz";case int64,int:types[i]="bigint";case string:types[i]="text";default:t.Fatalf("unsupported SQL parameter %T",arg)}}
 raw,e:=json.Marshal(map[string]any{"sql":sql,"args":args,"types":types});if e!=nil {t.Fatal(e)}
 t.Logf("UNIFORM_QUERY %s",raw)
}
'''


def literal(value, kind):
    if kind == 'bigint':
        if not isinstance(value, int):
            raise ValueError('noninteger SQL bigint')
        return str(value)
    if kind not in ('text', 'timestamptz') or not isinstance(value, str):
        raise ValueError('unsupported generated SQL parameter')
    return "'" + value.replace("'", "''") + "'::" + kind


def prepare(output: Path, rows: int, source_root: Path = ROOT, existing=None):
    output.mkdir(parents=True, exist_ok=False)
    source, digest = freeze(output, source_root, overlay_harness=False)
    anchor = int(dt.datetime.now(dt.timezone.utc).replace(hour=0, minute=0, second=0, microsecond=0).timestamp())
    if existing:anchor=existing['anchor']
    start = anchor - 30 * 86400
    schema = 'perf_uniform_' + dt.datetime.now(dt.timezone.utc).strftime('%Y%m%d%H%M%S') + '_' + secrets.token_hex(4)
    if existing:
        schema=existing['schema']
        if existing.get('synthetic_only') is not True or not re.fullmatch(r'perf_uniform_[a-z0-9_]+',schema) or existing['rows']!=rows:raise ValueError('existing uniform report identity/rows differ')
    overlay = source / 'internal/server/uniform_query_capture_test.go'
    if overlay.exists():
        if overlay.read_text(encoding='utf-8')!=CAPTURE_TEST:raise RuntimeError('capture overlay would replace different frozen source')
        manifest=json.loads((output/'source-manifest.json').read_text(encoding='utf-8'))
        manifest['files'].pop('internal/server/uniform_query_capture_test.go',None)
        digest=hashlib.sha256(json.dumps(manifest['files'],sort_keys=True).encode()).hexdigest()
        manifest['sha256']=digest
        (output/'source-manifest.json').write_text(json.dumps(manifest,indent=2),encoding='utf-8')
    overlay.write_text(CAPTURE_TEST, encoding='utf-8')
    env = {k: v for k, v in os.environ.items() if not k.startswith(('TEST_', 'PERFORMANCE_', 'BPS_', 'OPENAI_')) and k not in ('DATABASE_URL', 'REDIS_URL')}
    env.update(UNIFORM_FROM=str(start), UNIFORM_TO=str(anchor), GOWORK='off')
    command = ['go', 'test', './internal/server', '-run', '^TestPerformanceUniformSQLCapture$', '-count=1', '-v']
    capture = subprocess.run(command, cwd=source, env=env, capture_output=True, text=True, encoding='utf-8', timeout=300)
    (output / 'capture.log').write_text(capture.stdout + capture.stderr, encoding='utf-8')
    found = re.findall(r'UNIFORM_QUERY (\{[^\n]+\})', capture.stdout)
    if capture.returncode or len(found) != 1:
        raise RuntimeError('cannot capture real rollup SQL; inspect capture.log')
    query = json.loads(found[0])
    (output / 'query.json').write_text(json.dumps(query, indent=2), encoding='utf-8')
    migration_sources = []
    migration_claims = []
    for path in sorted((source / 'migrations').glob('*.sql')):
        raw = path.read_bytes()
        migration_sources.append(raw.decode('utf-8'))
        checksum = hashlib.sha256(raw).hexdigest()
        migration_claims.append("INSERT INTO schema_migrations(name,checksum) VALUES(" + literal('migrations/' + path.name, 'text') + ',' + literal(checksum, 'text') + ');')
    sql = ['\\set ON_ERROR_STOP on', '\\timing on', '\\pset pager off', '\\pset format unaligned', '\\pset tuples_only on',
           'SET jit=off;', "SET statement_timeout='30min';", "SET lock_timeout='5s';",
           'CREATE SCHEMA "' + schema + '";', 'SET search_path="' + schema + '";',
           'BEGIN;', 'CREATE TABLE schema_migrations(name text PRIMARY KEY,checksum text NOT NULL,applied_at timestamptz NOT NULL DEFAULT now());',
           *migration_sources, *migration_claims, 'COMMIT;', '\\echo UNIFORM_MIGRATIONS_COMPLETE']
    if existing:
        sql=['\\set ON_ERROR_STOP on','\\timing on','\\pset pager off','\\pset format unaligned','\\pset tuples_only on',"SET default_transaction_read_only=on;","SET statement_timeout='5min';","SET jit=off;",'SET search_path="'+schema+'";']
    for low in range(0, 0 if existing else rows, 100000):
        high = min(low + 99999, rows - 1)
        sql.append(f'''WITH input AS (
 SELECT i,((i*7919)%43200) AS minute_slot,((i/43200+37*(i%43200))%1000+1) AS account_no FROM generate_series({low}::bigint,{high}::bigint)i
),events AS (
 INSERT INTO usage_events(event_id,created_at,account_ref,account_name,key_id,status,input_tokens,output_tokens,cached_tokens,latency_ms,latency_known)
 SELECT 'uniform_e_'||i,to_timestamp({start}+minute_slot*60+(i*17)%60),'uniform_a_'||account_no,'Synthetic account '||account_no,'uniform_key',
 CASE WHEN i%10=0 THEN 'error' ELSE 'ok' END,1000+i%400,100+i%200,800,50+i%1000,true FROM input RETURNING *
)
INSERT INTO request_logs(event_id,account_ref,account_name,created_at,status,model,key_name,record)
SELECT event_id,account_ref,account_name,created_at,status,'synthetic-model','Synthetic key',
 jsonb_build_object('event_id',event_id,'account_id',account_ref,'account',account_name,'time',extract(epoch FROM created_at)::bigint,
 'status',status,'model','synthetic-model','key_id',key_id,'key_name','Synthetic key','input_tokens',input_tokens,'output_tokens',output_tokens,
 'cached_tokens',cached_tokens,'latency_ms',latency_ms,'usage_known',true,'http_code',CASE WHEN status='ok' THEN 200 ELSE 429 END,
 'error',CASE WHEN status='ok' THEN '' ELSE 'Synthetic rate limit' END) FROM events;''')
        sql.append('\\echo UNIFORM_SEED_PROGRESS ' + json.dumps({'completed_rows':high+1,'expected_rows':rows},separators=(',',':')))
    if not existing:sql+=['ANALYZE request_logs; ANALYZE retained_usage_minute; ANALYZE retained_usage_unsafe_minute;']
    sql += [
            "SELECT 'UNIFORM_COUNTS '||json_build_object('request_logs',(SELECT count(*) FROM request_logs),'account_minutes',(SELECT count(DISTINCT (account_ref,floor(extract(epoch FROM created_at)/60))) FROM request_logs),'summary_live_rows',(SELECT count(*) FROM retained_usage_minute WHERE requests>0),'summary_heap_bytes',pg_relation_size('retained_usage_minute'),'summary_index_bytes',pg_indexes_size('retained_usage_minute'),'unsafe_minutes',(SELECT count(*) FROM retained_usage_unsafe_minute WHERE requests>0))::text;",
            "SELECT 'UNIFORM_DIMENSION '||row_to_json(t)::text FROM (SELECT kind,count(*) AS rows,count(*) FILTER(WHERE requests>0) AS live_rows FROM retained_usage_minute GROUP BY kind ORDER BY kind)t;",
            "SELECT 'UNIFORM_STORAGE '||row_to_json(t)::text FROM (SELECT c.relname,pg_total_relation_size(c.oid) total_bytes,pg_relation_size(c.oid) heap_bytes,c.reltuples::bigint estimated_rows FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=current_schema() AND c.relkind='r' AND c.relname LIKE 'retained_%' ORDER BY c.relname)t;",
            'PREPARE uniform_stats(' + ','.join(query['types']) + ') AS ' + query['sql'] + ';',
            '\\echo UNIFORM_PLAN_BEGIN',
            'EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) EXECUTE uniform_stats(' + ','.join(literal(v, k) for v, k in zip(query['args'], query['types'])) + ');',
            '\\echo UNIFORM_PLAN_END']
    path = output / 'experiment.sql'
    path.write_text('\n'.join(sql) + '\n', encoding='utf-8')
    report = dict(status='prepared_not_executed', synthetic_only=True, production_touched=False,
                  source_sha256=digest, capture_overlay_sha256=hashlib.sha256(CAPTURE_TEST.encode()).hexdigest(),
                  sql_sha256=hashlib.sha256(path.read_bytes()).hexdigest(), schema=schema, rows=rows,
                  distribution='uniform30days', distinct_account_minutes_expected=rows, anchor=anchor,
                  measured_scope='SQL only; fixture has retained logs/events, not full account documents or durable aggregates',
                  slo_proven=False, limitations=['No HTTP sampling', 'No cold-cache eviction', 'Bulk fixture cost is not single-request ingestion throughput'])
    (output / 'report.json').write_text(json.dumps(report, indent=2), encoding='utf-8')
    if existing:
        report.update(read_only=True,measured_scope='Read-only actual frozen rollup SQL against retained uniform fixture',existing_fixture_report_source_sha256=existing['source_sha256'])
        (output/'report.json').write_text(json.dumps(report,indent=2),encoding='utf-8')
    return report


def execute(output: Path, report):
    values, private = isolated_environment('codex-agent:integration:uniform:' + report['schema'] + ':')
    pg = urlsplit(values['TEST_DATABASE_URL'])
    env = dict(PGHOST=pg.hostname, PGPORT=str(pg.port), PGUSER=pg.username, PGPASSWORD=pg.password, PGDATABASE=pg.path.lstrip('/'))
    remote = REMOTE + '/checks/' + report['schema']
    envpath = remote + '/test.env'
    client = None
    identifier = ''
    started = time.monotonic()
    try:
        client = connect()
        run(client, 'test -f ' + shlex.quote(REMOTE + '/.codex-agent-integration'))
        for label in ('performance-slo', 'system-performance-race', 'system-performance', 'uniform-distribution'):
            if run(client, 'docker ps -q --filter ' + shlex.quote('label=codex-agent.integration=' + label)).strip():
                raise RuntimeError('wait for active isolated performance/race work before starting this distribution experiment')
        resources = json.loads(run(client, "docker inspect --format '{{json .HostConfig}}' codex-agent-integration-postgres"))
        report['postgres_resources'] = {key: resources.get(key) for key in ('Memory', 'NanoCpus', 'CpuQuota', 'CpuPeriod')}
        image = run(client, "docker image inspect --format '{{.Id}}' postgres:18-alpine").strip()
        run(client, 'install -d -m 700 ' + shlex.quote(remote))
        write_remote(client, envpath, ''.join(k + '=' + str(v) + '\n' for k, v in env.items()))
        with client.open_sftp() as sftp:
            sftp.put(str(output / 'experiment.sql'), remote + '/experiment.sql')
        if run(client, 'sha256sum ' + shlex.quote(remote + '/experiment.sql')).split()[0] != report['sql_sha256']:
            raise RuntimeError('transferred SQL checksum mismatch')
        command = ['docker', 'run', '-d', '--name', report['schema'].replace('_', '-'), '--label', 'codex-agent.integration=uniform-distribution',
                   '--cpus', '2', '--memory', '1g', '--pids-limit', '128', '--network', 'host', '--cap-drop', 'ALL',
                   '--security-opt', 'no-new-privileges', '--env-file', envpath, '-v', remote + ':/experiment:ro',
                   '--entrypoint', 'psql', image, '-X', '-f', '/experiment/experiment.sql']
        identifier = run(client, shlex.join(command)).strip()
        if not re.fullmatch(r'[0-9a-f]{64}', identifier):
            raise RuntimeError('unexpected isolated container identity')
        deadline = time.monotonic() + 3600
        report['status'] = 'running'
        report['container_id']=identifier
        report['remote_directory']=remote
        report['started_at_utc']=dt.datetime.now(dt.timezone.utc).isoformat()
        (output / 'report.json').write_text(json.dumps(report, indent=2), encoding='utf-8')
        print(json.dumps({'started':str(output/'report.json'),'container_id':identifier,'schema':report['schema'],'postgres_resources':report['postgres_resources']}),flush=True)
        seen=0
        last_update=time.monotonic()
        while True:
            state = json.loads(run(client, "docker inspect --format '{{json .State}}' " + identifier))
            raw = scrub(run(client, 'docker logs ' + identifier + ' 2>&1', timeout=90), private)
            (output / 'run.log').write_text(raw,encoding='utf-8')
            progress=[line for line in raw.splitlines() if line.startswith(('UNIFORM_MIGRATIONS_COMPLETE','UNIFORM_SEED_PROGRESS','UNIFORM_COUNTS','UNIFORM_PLAN_BEGIN','UNIFORM_PLAN_END'))]
            for line in progress[seen:]:print(line,flush=True)
            seen=len(progress)
            report['last_progress']=progress[-1] if progress else 'initializing schema'
            report['elapsed_s']=round(time.monotonic()-started,3)
            report['last_checked_at_utc']=dt.datetime.now(dt.timezone.utc).isoformat()
            report['container_running']=state['Running']
            (output/'report.json').write_text(json.dumps(report,indent=2),encoding='utf-8')
            if time.monotonic()-last_update>=30:
                print(json.dumps({'running':state['Running'],'elapsed_s':report['elapsed_s'],'last_progress':report['last_progress']}),flush=True)
                last_update=time.monotonic()
            if not state['Running']:
                break
            if time.monotonic() > deadline:
                raise TimeoutError('uniform experiment exceeded one hour')
            time.sleep(5)
        raw = scrub(run(client, 'docker logs ' + identifier + ' 2>&1', timeout=90), private)
        (output / 'run.log').write_text(raw, encoding='utf-8')
        counts = re.findall(r'UNIFORM_COUNTS (\{[^\n]+\})', raw)
        report['counts'] = json.loads(counts[-1]) if counts else None
        report['dimensions']=[json.loads(item) for item in re.findall(r'UNIFORM_DIMENSION (\{[^\n]+\})',raw)]
        report['storage']=[json.loads(item) for item in re.findall(r'UNIFORM_STORAGE (\{[^\n]+\})',raw)]
        if 'UNIFORM_PLAN_BEGIN' in raw and 'UNIFORM_PLAN_END' in raw:
            section=raw.split('UNIFORM_PLAN_BEGIN',1)[1].split('UNIFORM_PLAN_END',1)[0]
            bracket=section.find('[')
            if bracket>=0:
                plan=json.JSONDecoder().raw_decode(section[bracket:])[0]
                (output/'plan.json').write_text(json.dumps(plan,indent=2),encoding='utf-8')
                report['plan_summary']={key:plan[0].get(key) for key in ('Planning Time','Execution Time','Settings')}
                report['plan_summary']['root']={key:plan[0]['Plan'].get(key) for key in ('Node Type','Actual Rows','Actual Total Time','Shared Hit Blocks','Shared Read Blocks','Temp Read Blocks','Temp Written Blocks')}
        complete = (state['ExitCode'] == 0 and report['counts'] and report['counts']['request_logs'] == report['rows']
                    and report['counts']['account_minutes'] == report['rows'] and report.get('plan_summary'))
        report.update(status='experiment_complete' if complete else 'fail', exit_code=state['ExitCode'], schema_retained=True)
    except Exception as error:
        report.update(status='fail', error=scrub(str(error), private))
    finally:
        if client:
            if re.fullmatch(r'[0-9a-f]{64}', identifier):
                try:
                    run(client, 'docker rm -f ' + identifier)
                    report['temporary_container_removed'] = True
                except Exception:
                    report.update(status='fail', temporary_container_removed=False)
            try:
                with client.open_sftp() as sftp:
                    sftp.remove(envpath)
                report['temporary_credentials_removed'] = True
            except FileNotFoundError:
                report['temporary_credentials_removed'] = True
            except Exception:
                report.update(status='fail', temporary_credentials_removed=False)
            client.close()
        report['elapsed_s'] = round(time.monotonic() - started, 3)
        (output / 'report.json').write_text(json.dumps(report, indent=2), encoding='utf-8')
    return report


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--rows', type=int, choices=(100000, 1000000), default=1000000)
    parser.add_argument('--prepare-only', action='store_true')
    parser.add_argument('--output', type=Path)
    parser.add_argument('--source',type=Path,default=ROOT,help='Frozen implementation source for consistent scale comparison')
    parser.add_argument('--existing-report',type=Path,help='Read-only EXPLAIN against an existing uniform fixture; no migrations or seeding')
    args = parser.parse_args()
    stamp = dt.datetime.now(dt.timezone.utc).strftime('%Y%m%dT%H%M%S') + '-' + secrets.token_hex(3)
    output = (args.output or ROOT / '.local/performance-uniform' / stamp).resolve()
    existing=json.loads(args.existing_report.read_text(encoding='utf-8')) if args.existing_report else None
    if existing:args.rows=existing['rows']
    report = prepare(output, args.rows,args.source.resolve(),existing)
    print(json.dumps({'prepared':str(output/'report.json'),'source_sha256':report['source_sha256'],'schema':report['schema'],'rows':args.rows}),flush=True)
    if not args.prepare_only:
        report = execute(output, report)
    print(json.dumps({'status': report['status'], 'report': str(output / 'report.json'), 'slo_proven': False}))
    return 1 if report['status'] == 'fail' else 0


if __name__ == '__main__':
    raise SystemExit(main())
