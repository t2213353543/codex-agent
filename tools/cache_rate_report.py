"""Read-only weighted cache diagnostics from real traffic; never calls a model.

python tools/cache_rate_report.py --minutes 10
"""
import argparse
from datetime import datetime, timezone
import json
from pathlib import Path
import shlex
from integration_env import connect, run

WORKLOAD_SQL = """CASE
WHEN coalesce(record->>'user_agent','') ~ '^cache[0-9]{3}-[a-f0-9]{32}$'
  OR coalesce(record->>'user_agent','') LIKE 'gateway-release-validation/%' THEN 'release_smoke'
WHEN coalesce(record->>'client_request_id','') LIKE 'cache90-%' THEN 'bounded_experiment'
ELSE 'natural_traffic' END"""

REMOTE = r'''
import json,subprocess,time,hashlib
from pathlib import Path
since = __SINCE__
minutes = __MINUTES__
condition = "created_at >= '" + since + "'::timestamptz" if since else "created_at>=now()-interval '"+str(minutes)+" minutes'"
def sql(query):
    command=['docker','exec','codex-agent-postgres','psql','-X','-q','-A','-t','-U','postgres','-d','codex_agent','-v','ON_ERROR_STOP=1','-c',
         "BEGIN READ ONLY;SET LOCAL statement_timeout='20s';"+query+';COMMIT;']
    process=subprocess.run(command,capture_output=True,text=True,timeout=30)
    if process.returncode:raise RuntimeError('cache_report_query_failed: '+process.stderr[:200])
    return json.loads(process.stdout.strip() or 'null')
# Match stats_cache.go, including legacy endpoint-less records. Control traffic
# stays in request/error totals but is not a cache observation or an unknown.
cache_eligible="NOT coalesce((l.record->>'websocket_warmup')::boolean,false) AND regexp_replace(coalesce(l.record->>'endpoint',''),'^/?(backend-api/codex/|v1/)?','') IN ('','responses','responses/compact','compact','chat/completions','codex/tasks','guardian','guardian-classifier','alpha/search','images/generations','images/edits','memories/trace_summarize','realtime/calls','realtime','live')"
read_known="("+cache_eligible+") AND coalesce((l.record->>'usage_known')::boolean,e.input_tokens<>0 OR e.output_tokens<>0 OR e.cached_tokens<>0) AND coalesce((l.record->>'cached_tokens_known')::boolean,e.cached_tokens>0) AND e.cached_tokens>=0 AND e.input_tokens>=e.cached_tokens"
aggregate="count(*) requests,sum(input_tokens) input,sum(cached_tokens) cached,round(100.0*sum(cached_tokens) FILTER(WHERE read_known)/nullif(sum(input_tokens) FILTER(WHERE read_known),0),4) cache_percent,count(*) FILTER(WHERE read_known) cache_read_samples,count(*) FILTER(WHERE cache_eligible AND NOT coalesce(read_known,false)) cache_read_unknown_requests,sum(input_tokens) FILTER(WHERE read_known) cache_read_input_tokens,sum(cache_write) FILTER(WHERE cache_eligible AND cache_write>=0) cache_write_tokens,count(cache_write) FILTER(WHERE cache_eligible AND cache_write>=0) cache_write_samples"
cte="WITH r AS MATERIALIZED (SELECT e.*,l.model,l.record,l.record->'prompt_cache' d,("+cache_eligible+") cache_eligible,("+read_known+") read_known,(l.record->>'cache_write_tokens')::bigint cache_write FROM usage_events e LEFT JOIN request_logs l USING(event_id) WHERE e."+condition+") "
queries={
 'overall':"SELECT row_to_json(t) FROM(SELECT "+aggregate+",count(*) FILTER(WHERE status='ok') ok,count(*) FILTER(WHERE status<>'ok') failed FROM r)t",
 'coverage':"SELECT json_agg(t) FROM(SELECT coalesce(d->>'coverage','unobserved') coverage,coalesce(d->>'scope_kind','unobserved') scope_kind,coalesce(d->>'comparison','unobserved') comparison,coalesce(d->>'key_mode','unobserved') key_mode,status,"+aggregate+" FROM r GROUP BY 1,2,3,4,5 ORDER BY input DESC)t",
 'changes':"""SELECT json_agg(t) FROM(SELECT flags.flag,count(*) requests,sum(input_tokens) input,sum(cached_tokens) cached,
 sum(input_tokens-cached_tokens) FILTER(WHERE read_known) uncached,round(100.0*sum(cached_tokens) FILTER(WHERE read_known)/nullif(sum(input_tokens) FILTER(WHERE read_known),0),4) cache_percent,
 sum(cache_write) FILTER(WHERE cache_eligible AND cache_write>=0) cache_write_tokens,count(cache_write) FILTER(WHERE cache_eligible AND cache_write>=0) cache_write_samples
 FROM r CROSS JOIN LATERAL (VALUES
 ('account_changed',d->>'account_changed'='true'),('cache_key_changed',d->>'cache_key_changed'='true'),
 ('model_changed',d->>'model_changed'='true'),('instructions_changed',d->>'instructions_changed'='true'),
 ('tools_changed',d->>'tools_changed'='true'),('controls_changed',d->>'controls_changed'='true'),
 ('history_changed',d->>'input_history'='changed'),('history_extended',d->>'input_history'='extended'),
 ('history_unchanged',d->>'input_history'='unchanged'),('history_shortened',d->>'input_history'='shortened'),
 ('stable_configuration',d->>'account_changed'='false' AND d->>'cache_key_changed'='false' AND d->>'model_changed'='false'
 AND d->>'instructions_changed'='false' AND d->>'tools_changed'='false' AND d->>'controls_changed'='false'))flags(flag,present)
 WHERE d->>'comparison'='compared' AND flags.present GROUP BY 1 ORDER BY uncached DESC)t""",
 'models':"SELECT json_agg(t) FROM(SELECT model,"+aggregate+" FROM r GROUP BY 1 ORDER BY input DESC)t",
 'workloads':"SELECT json_agg(t) FROM(SELECT "+__WORKLOAD_SQL__+" workload,"+aggregate+" FROM r GROUP BY 1 ORDER BY input DESC)t",
 'prefixes':"SELECT json_agg(t) FROM(SELECT d->>'prefix_hash' prefix,coalesce(d->>'key_mode','unobserved') key_mode,count(DISTINCT d->>'scope_hash') scopes,count(DISTINCT d->>'cache_key_hash') cache_keys,"+aggregate+" FROM r WHERE d->>'coverage'='complete' GROUP BY 1,2 ORDER BY input DESC LIMIT 15)t",
 'tenant_partitions':"SELECT json_agg(t) FROM(SELECT md5(coalesce(record->>'key_id','')) tenant_hash,d->>'account_hash' account_hash,d->>'instance' instance,coalesce(record->>'sent_model',model) sent_model,d->>'prefix_hash' configuration_hash,d->>'key_mode' key_mode,count(DISTINCT d->>'cache_key_hash') cache_keys,"+aggregate+" FROM r WHERE d->>'coverage'='complete' AND status='ok' AND NOT (coalesce(record->>'user_agent','') LIKE 'gateway-release-validation/%') GROUP BY 1,2,3,4,5,6 ORDER BY input DESC LIMIT 20)t",
 'recent_compared':"""SELECT coalesce(json_agg(t),'[]'::json) FROM(SELECT created_at,model,input_tokens,cached_tokens,
 d->>'scope_hash' scope,d->>'key_mode' key_mode,d->>'previous_age_ms' previous_age_ms,
 d->>'input_items' input_items,d->>'previous_input_items' previous_input_items,
 d->>'shared_input_items' shared_input_items,d->>'input_history' input_history,
 d->>'shared_input_wire_prefix_bytes' shared_wire_bytes,d->>'input_wire_bytes' wire_bytes,
 d->>'instructions_changed' instructions_changed,d->>'tools_changed' tools_changed,d->>'controls_changed' controls_changed,
 d->>'cache_key_changed' cache_key_changed,d->>'account_changed' account_changed
 FROM r WHERE d->>'comparison'='compared' ORDER BY created_at DESC LIMIT 30)t""",
}
pid=subprocess.check_output(['systemctl','show','codex-agent.service','-p','MainPID','--value'],text=True).strip()
report={'captured_at':time.strftime('%Y-%m-%dT%H:%M:%SZ',time.gmtime()),'since':since or None,'minutes':minutes if not since else None,
        'runtime_sha256':hashlib.sha256(Path('/proc/'+pid+'/exe').read_bytes()).hexdigest(),
        'formula':'sum(observed cache-read tokens) / sum(input on the same observed requests); cache creation is separately reported, never input minus cache reads; unknown stays unknown; control/warmup excluded from cache observations; request/error and raw token totals retain all requests; change buckets overlap'}
# One PostgreSQL statement gives all sections the same MVCC snapshot and now().
# Concurrent completions cannot make the coverage denominator differ from totals.
report.update(sql(cte+"SELECT json_build_object("+','.join("'"+name+"',("+query+")" for name,query in queries.items())+")"))
print(json.dumps(report))
'''

def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--minutes',type=int,default=10)
    parser.add_argument('--since',help='ISO 8601 timestamp with timezone')
    parser.add_argument('--output',type=Path)
    args=parser.parse_args()
    if not 1<=args.minutes<=1440:parser.error('minutes must be 1..1440')
    since=''
    if args.since:
        stamp=datetime.fromisoformat(args.since.replace('Z','+00:00'))
        if stamp.tzinfo is None:parser.error('since requires timezone')
        since=stamp.astimezone(timezone.utc).isoformat()
    code=REMOTE.replace('__SINCE__',repr(since)).replace('__MINUTES__',repr(args.minutes)).replace('__WORKLOAD_SQL__',repr(WORKLOAD_SQL))
    client=connect()
    try:report=json.loads(run(client,'python3 -c '+shlex.quote(code),timeout=180))
    finally:client.close()
    out=args.output or Path(__file__).resolve().parents[1]/'.local/cache90-20260925'/('traffic-'+datetime.now(timezone.utc).strftime('%Y%m%dT%H%M%SZ')+'.json')
    out.parent.mkdir(parents=True,exist_ok=True);out.write_text(json.dumps(report,ensure_ascii=False,indent=2)+'\n',encoding='utf-8')
    print(json.dumps({k:v for k,v in report.items() if k not in ('prefixes','recent_compared')},ensure_ascii=False,indent=2))
    print('report='+str(out))

if __name__=='__main__':main()
