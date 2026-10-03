"""Resume 100k-transaction append from the existing synthetic 1M uniform fixture.

python tools/performance_uniform_extend.py --fixture-report .local/performance-uniform/20260928T224734-987eb8/report.json --source .local/performance-slo/20260928t234046-15a493/source

Existing event IDs are preserved. Each committed batch atomically advances the
checkpoint with its real events/logs and all enabled retained-summary triggers.
The HTTP/durable aggregate rebuild is a separate explicit maintenance operation.
"""
from __future__ import annotations

import argparse
import datetime as dt
import hashlib
import json
from pathlib import Path
import re
import secrets
import shlex
import time
from urllib.parse import urlsplit

from gateway_source_race import isolated_environment
from integration_env import ROOT,REMOTE,connect,run,write_remote
from run_linux_checks import scrub


def make_sql(schema,start,target):
    sql=['\\set ON_ERROR_STOP on','\\timing on','\\pset pager off','\\pset format unaligned','\\pset tuples_only on',
         "SET application_name='codex-uniform-extend';","SET statement_timeout='30min';","SET lock_timeout='10s';",'SET jit=off;',
         'SET search_path="'+schema+'";',"SELECT pg_advisory_lock(hashtextextended(current_schema()||':uniform_extend',77193));",
         '''CREATE TABLE IF NOT EXISTS performance_uniform_extend_checkpoint(singleton boolean PRIMARY KEY DEFAULT true CHECK(singleton),next_i bigint NOT NULL,target_rows bigint NOT NULL,original_rows bigint NOT NULL,seed_start bigint NOT NULL,last_committed_at timestamptz NOT NULL DEFAULT now());''',
         f'''DO $$ DECLARE n bigint;high bigint;BEGIN
IF NOT EXISTS(SELECT 1 FROM performance_uniform_extend_checkpoint) THEN
 SELECT count(*),max(substring(event_id FROM 11)::bigint) INTO n,high FROM usage_events;
 IF n<>1000000 OR high<>999999 OR (SELECT count(*) FROM request_logs)<>1000000 OR EXISTS(SELECT 1 FROM usage_events WHERE event_id!~'^uniform_e_[0-9]+$') THEN RAISE EXCEPTION 'existing fixture does not have original 1M identity';END IF;
 INSERT INTO performance_uniform_extend_checkpoint(next_i,target_rows,original_rows,seed_start) VALUES(1000000,{target},1000000,{start});
END IF;
IF EXISTS(SELECT 1 FROM performance_uniform_extend_checkpoint WHERE target_rows<>{target} OR original_rows<>1000000 OR seed_start<>{start} OR next_i<1000000 OR next_i>{target}) THEN RAISE EXCEPTION 'checkpoint identity mismatch';END IF;
END $$;''',
         "SELECT 'UNIFORM_EXTEND_CHECKPOINT '||row_to_json(c)::text FROM performance_uniform_extend_checkpoint c;"]
    for low in range(1000000,target,100000):
        high=min(low+99999,target-1)
        sql += ['BEGIN;',f"DO $$ BEGIN IF (SELECT next_i FROM performance_uniform_extend_checkpoint)<{low} THEN RAISE EXCEPTION 'checkpoint gap';END IF;END $$;",
                f'SELECT next_i={low} AS append_batch FROM performance_uniform_extend_checkpoint \\gset','\\if :append_batch',
                f'''WITH input AS (
 SELECT i,((i*7919)%43200) minute_slot,((i/43200+37*(i%43200))%1000+1) account_no FROM generate_series({low}::bigint,{high}::bigint)i
),events AS (
 INSERT INTO usage_events(event_id,created_at,account_ref,account_name,key_id,status,input_tokens,output_tokens,cached_tokens,latency_ms,latency_known)
 SELECT 'uniform_e_'||i,to_timestamp({start}+minute_slot*60+(i*17)%60),'uniform_a_'||account_no,'Synthetic account '||account_no,'uniform_key',
 CASE WHEN i%10=0 THEN 'error' ELSE 'ok' END,1000+i%400,100+i%200,800,50+i%1000,true FROM input RETURNING *
)
INSERT INTO request_logs(event_id,account_ref,account_name,created_at,status,model,key_name,record)
SELECT event_id,account_ref,account_name,created_at,status,'synthetic-model','Synthetic key',jsonb_build_object(
 'event_id',event_id,'account_id',account_ref,'account',account_name,'time',extract(epoch FROM created_at)::bigint,
 'status',status,'model','synthetic-model','key_id',key_id,'key_name','Synthetic key','input_tokens',input_tokens,'output_tokens',output_tokens,
 'cached_tokens',cached_tokens,'latency_ms',latency_ms,'usage_known',true,'http_code',CASE WHEN status='ok' THEN 200 ELSE 429 END,
 'error',CASE WHEN status='ok' THEN '' ELSE 'Synthetic rate limit' END) FROM events;''',
                f'UPDATE performance_uniform_extend_checkpoint SET next_i={high+1},last_committed_at=clock_timestamp();','\\endif','COMMIT;',
                "SELECT 'UNIFORM_EXTEND_PROGRESS '||row_to_json(c)::text FROM performance_uniform_extend_checkpoint c;"]
    sql += ['ANALYZE usage_events,request_logs,retained_log_minute,retained_usage_minute,retained_usage_daily,retained_operations;',
            "SELECT 'UNIFORM_EXTEND_COUNTS '||json_build_object('events',(SELECT count(*) FROM usage_events),'logs',(SELECT count(*) FROM request_logs),'account_minutes',(SELECT count(DISTINCT (account_ref,floor(extract(epoch FROM created_at)/60))) FROM request_logs),'retained_requests',(SELECT sum(requests)::bigint FROM retained_log_minute),'dimensional_requests',(SELECT sum(requests)::bigint FROM retained_usage_minute WHERE kind='total'),'old_event_count',(SELECT count(*) FROM usage_events WHERE substring(event_id FROM 11)::bigint<1000000),'max_event_suffix',(SELECT max(substring(event_id FROM 11)::bigint) FROM usage_events))::text;",
            "SELECT 'UNIFORM_EXTEND_STORAGE '||row_to_json(t)::text FROM (SELECT c.relname,pg_total_relation_size(c.oid) total_bytes,c.reltuples::bigint estimated_rows FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=current_schema() AND c.relkind='r' ORDER BY c.relname)t;",
            "SELECT pg_advisory_unlock(hashtextextended(current_schema()||':uniform_extend',77193));"]
    return '\n'.join(sql)+'\n'


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--fixture-report',type=Path,required=True);parser.add_argument('--source',type=Path,required=True)
    parser.add_argument('--rows',type=int,default=10000000);parser.add_argument('--timeout-minutes',type=int,default=360);parser.add_argument('--output',type=Path)
    args=parser.parse_args();original=json.loads(args.fixture_report.read_text(encoding='utf-8'));schema=original['schema']
    if original.get('synthetic_only') is not True or not re.fullmatch(r'perf_uniform_[a-z0-9_]+',schema) or not 1000000<=args.rows<=10000000 or args.rows%100000:raise ValueError('uniform identity/target invalid')
    stamp=dt.datetime.now(dt.timezone.utc).strftime('%Y%m%dT%H%M%S').lower()+'-'+secrets.token_hex(3)
    output=(args.output or ROOT/'.local/performance-uniform'/('extend-'+stamp)).resolve();output.mkdir(parents=True,exist_ok=False)
    sql=make_sql(schema,original['anchor']-30*86400,args.rows);(output/'extend.sql').write_text(sql,encoding='utf-8')
    expected={'migrations/'+p.name:hashlib.sha256(p.read_bytes()).hexdigest() for p in (args.source/'migrations').glob('*.sql')}
    manifest=json.loads((args.source.parent/'source-manifest.json').read_text())
    report={'status':'starting','synthetic_only':True,'production_touched':False,'schema':schema,'rows':args.rows,'anchor':original['anchor'],'source_sha256':manifest['sha256'],'fixture_report':str(args.fixture_report.resolve()),'original_rows':1000000,'batch_rows':100000,'sql_sha256':hashlib.sha256(sql.encode()).hexdigest(),'durable_aggregates_require_rebuild':True,'distribution':'uniform30days','read_slo_proven':False}
    values,private=isolated_environment('codex-agent:integration:uniform-extend:'+stamp+':');pg=urlsplit(values['TEST_DATABASE_URL'])
    env=dict(PGHOST=pg.hostname,PGPORT=str(pg.port),PGUSER=pg.username,PGPASSWORD=pg.password,PGDATABASE=pg.path.lstrip('/'))
    client=None;identifier='';remote=REMOTE+'/checks/uniform-extend-'+stamp;envpath=remote+'/test.env';started=time.monotonic();seen=0
    try:
        client=connect();run(client,'test -f '+shlex.quote(REMOTE+'/.codex-agent-integration'))
        active=run(client,"docker ps --filter label=codex-agent.integration --format '{{.Names}} {{.Label \"codex-agent.integration\"}}'").strip().splitlines()
        peers=[{'name':line.rsplit(' ',1)[0],'label':line.rsplit(' ',1)[1]} for line in active]
        report['maintenance_peers_at_start']=peers
        report['interference_note']='Bulk fixture maintenance can share the test host with isolated functional/race tasks; elapsed batch time is not an ingestion performance claim.'
        if any(p['label'] in ('performance-slo','system-performance','uniform-distribution','uniform-extend') for p in peers):raise RuntimeError('Performance sampling/another append active; finish that coordinated window first')
        actual=json.loads(run(client,shlex.join(['docker','exec','codex-agent-integration-postgres','psql','-X','-U','postgres','-d','codex_agent_integration','-Atc','SELECT json_object_agg(name,checksum) FROM '+schema+'.schema_migrations'])))
        if actual!=expected:raise RuntimeError('existing fixture migrations differ from explicit source; no migrations applied')
        disk=run(client,'df -B1 --output=avail /opt/codex-agent-integration').split();available=int(disk[-1])
        sizes=int(run(client,shlex.join(['docker','exec','codex-agent-integration-postgres','psql','-X','-U','postgres','-d','codex_agent_integration','-Atc',"SELECT sum(pg_total_relation_size(c.oid)) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='"+schema+"' AND c.relkind='r'"])).strip())
        checkpoint_exists=run(client,shlex.join(['docker','exec','codex-agent-integration-postgres','psql','-X','-U','postgres','-d','codex_agent_integration','-Atc',"SELECT to_regclass('"+schema+".performance_uniform_extend_checkpoint') IS NOT NULL"])).strip()=='t'
        current_rows=1000000
        if checkpoint_exists:current_rows=int(run(client,shlex.join(['docker','exec','codex-agent-integration-postgres','psql','-X','-U','postgres','-d','codex_agent_integration','-Atc','SELECT next_i FROM '+schema+'.performance_uniform_extend_checkpoint'])).strip())
        required=max(60*1024**3,int(sizes*args.rows/max(1000000,current_rows)*3));report['capacity_preflight']={'disk_available_bytes':available,'current_schema_bytes':sizes,'checkpoint_rows':current_rows,'required_free_bytes':required,'estimate':'3x projected target footprint at current density, minimum 60GiB including WAL/temp/rebuild'}
        if available<required:raise RuntimeError('insufficient isolated-host free space for uniform extension budget')
        resources=json.loads(run(client,"docker inspect --format '{{json .HostConfig}}' codex-agent-integration-postgres"));report['postgres_resources']={k:resources[k] for k in ('Memory','NanoCpus')}
        run(client,'install -d -m 700 '+shlex.quote(remote));write_remote(client,envpath,''.join(k+'='+str(v)+'\n' for k,v in env.items()));write_remote(client,remote+'/extend.sql',sql)
        image=run(client,"docker image inspect --format '{{.Id}}' postgres:18-alpine").strip()
        command=['docker','run','-d','--name','uniform-extend-'+stamp,'--label','codex-agent.integration=uniform-extend','--cpus','2','--memory','1g','--pids-limit','128','--network','host','--cap-drop','ALL','--security-opt','no-new-privileges','--env-file',envpath,'-v',remote+':/experiment:ro','--entrypoint','psql',image,'-X','-f','/experiment/extend.sql']
        identifier=run(client,shlex.join(command)).strip()
        if not re.fullmatch('[0-9a-f]{64}',identifier):raise RuntimeError('invalid append container identity')
        report.update(status='running',container_id=identifier,remote_directory=remote);print(json.dumps({'started':str(output/'report.json'),'container_id':identifier,'schema':schema,'capacity':report['capacity_preflight']}),flush=True)
        last_notice=0.0
        while True:
            state=json.loads(run(client,"docker inspect --format '{{json .State}}' "+identifier));raw=scrub(run(client,'docker logs '+identifier+' 2>&1',timeout=90),private);(output/'run.log').write_text(raw,encoding='utf-8')
            progress=[json.loads(s) for s in re.findall(r'UNIFORM_EXTEND_(?:CHECKPOINT|PROGRESS) (\{[^\n]+\})',raw)]
            for item in progress[seen:]:print(json.dumps({'checkpoint':item,'elapsed_s':round(time.monotonic()-started,2)}),flush=True)
            seen=len(progress);report.update(elapsed_s=round(time.monotonic()-started,3),container_running=state['Running'],last_checked_at_utc=dt.datetime.now(dt.timezone.utc).isoformat(),checkpoint=progress[-1] if progress else None)
            (output/'report.json').write_text(json.dumps(report,indent=2),encoding='utf-8')
            if not state['Running']:break
            if time.monotonic()-started>args.timeout_minutes*60:raise TimeoutError('append run deadline; committed checkpoint permits restart')
            if time.monotonic()-last_notice>30:print(json.dumps({'running':True,'elapsed_s':report['elapsed_s'],'next_i':report['checkpoint']['next_i'] if progress else None}),flush=True);last_notice=time.monotonic()
            time.sleep(5)
        counts=re.findall(r'UNIFORM_EXTEND_COUNTS (\{[^\n]+\})',raw);report['counts']=json.loads(counts[-1]) if counts else None
        report['storage']=[json.loads(s) for s in re.findall(r'UNIFORM_EXTEND_STORAGE (\{[^\n]+\})',raw)]
        report['exit_code']=state['ExitCode'];c=report['counts'] or {}
        okay=state['ExitCode']==0 and all(c.get(k)==args.rows for k in ('events','logs','account_minutes','retained_requests','dimensional_requests')) and c.get('old_event_count')==1000000 and c.get('max_event_suffix')==args.rows-1
        report['status']='fixture_complete' if okay else 'fail'
    except Exception as err:report.update(status='fail',error=scrub(str(err),private))
    finally:
        if client:
            if re.fullmatch('[0-9a-f]{64}',identifier):
                try:run(client,'docker rm -f '+identifier);report['temporary_container_removed']=True
                except Exception:report['temporary_container_removed']=False
            try:
                with client.open_sftp() as sftp:sftp.remove(envpath)
            except Exception:pass
            client.close()
        report['elapsed_s']=round(time.monotonic()-started,3);(output/'report.json').write_text(json.dumps(report,indent=2),encoding='utf-8')
    print(json.dumps({'status':report['status'],'report':str(output/'report.json'),'error':report.get('error')}),flush=True)
    return 0 if report['status']=='fixture_complete' else 1


if __name__=='__main__':raise SystemExit(main())
