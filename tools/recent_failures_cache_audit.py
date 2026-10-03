"""Read-only production failure/cache audit; no inference or settings writes.

python tools/recent_failures_cache_audit.py
"""
import json
from pathlib import Path
import shlex
from integration_env import connect, run

REMOTE = r'''
import collections, hashlib, json, pathlib, re, subprocess, urllib.request
def command(args):
    return subprocess.check_output(args, text=True, timeout=45).strip()
container = 'codex-agent-postgres'
config = json.loads(command(['docker','inspect',container]))[0]
env = dict(item.split('=',1) for item in config['Config']['Env'] if '=' in item)
def sql(query):
    raw = command(['docker','exec',container,'psql','-X','-q','-A','-t','-v','ON_ERROR_STOP=1',
                   '-U',env.get('POSTGRES_USER','postgres'),'-d','codex_agent',
                   '-c',"BEGIN READ ONLY; SET LOCAL statement_timeout='30s'; " + query + '; COMMIT;'])
    return json.loads(raw or 'null')
def redact(text):
    text = re.sub(r'(?i)Bearer\s+\S+', 'Bearer [redacted]', text)
    text = re.sub(r'\b[\w.+-]+@[\w.-]+\.[A-Za-z]{2,}\b', '[email]', text)
    text = re.sub(r'\b(?:sk-|eyJ)[A-Za-z0-9_.-]{15,}', '[credential]', text)
    return text[:1800]
key = pathlib.Path('/var/lib/codex-agent/client-key').read_text().strip()
opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
def api(path):
    request = urllib.request.Request('http://127.0.0.1:18879'+path,headers={'Authorization':'Bearer '+key})
    with opener.open(request,timeout=20) as response: return json.load(response)
settings = api('/api/settings'); settings = settings.get('settings',settings)
pid = command(['systemctl','show','codex-agent.service','-p','MainPID','--value'])
report = {'captured_at':command(['date','-u','+%Y-%m-%dT%H:%M:%SZ']),
          'runtime_sha256':hashlib.sha256(pathlib.Path('/proc/'+pid+'/exe').read_bytes()).hexdigest(),
          'settings':{k:settings.get(k) for k in ('settings_version','sticky_sessions_enabled','sticky_session_ttl_seconds',
                 'prompt_cache_key_enabled','builtin_prompt_enabled','builtin_prompt_mode','compatibility_normalization_enabled',
                 'websocket_enabled','codex_fingerprint_enabled')},
          'health':api('/health/ready')}
# Keep raw usage/error totals, but only eligible, observed cache reads share a
# rate denominator. Missing creation is not input minus reads or an invented 0.
cache_eligible="NOT coalesce((l.record->>'websocket_warmup')::boolean,false) AND regexp_replace(coalesce(l.record->>'endpoint',''),'^/?(backend-api/codex/|v1/)?','') IN ('','responses','responses/compact','compact','chat/completions','codex/tasks','guardian','guardian-classifier','alpha/search','images/generations','images/edits','memories/trace_summarize','realtime/calls','realtime','live')"
read_known="("+cache_eligible+") AND coalesce((l.record->>'usage_known')::boolean,e.input_tokens<>0 OR e.output_tokens<>0 OR e.cached_tokens<>0) AND coalesce((l.record->>'cached_tokens_known')::boolean,e.cached_tokens>0) AND e.cached_tokens>=0 AND e.input_tokens>=e.cached_tokens"
aggregate="""count(*) AS requests,count(*) FILTER(WHERE status='ok') AS ok,
 count(*) FILTER(WHERE status='error') AS errors,count(*) FILTER(WHERE status='aborted') AS aborted,
 coalesce(sum(input_tokens),0) AS input_tokens,coalesce(sum(cached_tokens),0) AS cached_tokens,
 round(100.0*sum(cached_tokens) FILTER(WHERE read_known)/nullif(sum(input_tokens) FILTER(WHERE read_known),0),2) AS cache_percent,
 count(*) FILTER(WHERE read_known) AS cache_read_samples,
 count(*) FILTER(WHERE cache_eligible AND NOT coalesce(read_known,false)) AS cache_read_unknown_requests,
 coalesce(sum(input_tokens) FILTER(WHERE read_known),0) AS cache_read_input_tokens,
 count(*) FILTER(WHERE read_known AND cached_tokens>0) AS cache_hit_requests,
 sum(cache_write) FILTER(WHERE cache_eligible AND cache_write>=0) AS cache_write_tokens,
 count(cache_write) FILTER(WHERE cache_eligible AND cache_write>=0) AS cache_write_samples"""
cte="WITH r AS (SELECT e.*,l.record,("+cache_eligible+") cache_eligible,("+read_known+") read_known,(l.record->>'cache_write_tokens')::bigint cache_write FROM usage_events e LEFT JOIN request_logs l USING(event_id) WHERE e.created_at>now()-interval '168 hours') "
report['windows'] = sql(cte+"""SELECT json_agg(t) FROM (
 SELECT hours,totals.* FROM (VALUES (1),(6),(24),(168)) w(hours)
 CROSS JOIN LATERAL (SELECT """+aggregate+""" FROM r WHERE created_at>now()-hours*interval '1 hour') totals
 ORDER BY hours) t""")
# The detailed breakdown keeps its original retained-request-log scope.
log_cte="WITH e AS (SELECT l.*,coalesce((record->>'input_tokens')::bigint,0) input_tokens,coalesce((record->>'output_tokens')::bigint,0) output_tokens,coalesce((record->>'cached_tokens')::bigint,0) cached_tokens FROM request_logs l WHERE created_at>now()-interval '24 hours' AND status='ok'), r AS (SELECT e.*,("+cache_eligible.replace('l.record','e.record')+") cache_eligible,("+read_known.replace('l.record','e.record')+") read_known,(record->>'cache_write_tokens')::bigint cache_write FROM e) "
report['cache_by_model_type_24h'] = sql(log_cte+"""SELECT json_agg(t) FROM (
 SELECT model, record->>'request_type' AS request_type,"""+aggregate+"""
 FROM r GROUP BY 1,2 ORDER BY input_tokens DESC) t""")
report['cache_by_client_length_24h'] = sql(log_cte+"""SELECT json_agg(t) FROM (
 SELECT coalesce(record->>'user_agent','unknown') AS user_agent,
 CASE WHEN input_tokens<1024 THEN '<1024' WHEN input_tokens<4096 THEN '1024-4095' ELSE '>=4096' END AS input_bucket,
 """+aggregate+""" FROM r GROUP BY 1,2 ORDER BY input_tokens DESC) t""")
report['formula'] = 'cache_percent uses observed cache reads and input on those same eligible requests; control/warmup excluded from cache observations; request/error and raw token totals retain all requests; cache creation is independent; missing observations stay unknown'
rows = sql("""SELECT coalesce(json_agg(t),'[]'::json) FROM (
 SELECT id,created_at,status,model, record->>'http_code' AS http_code, record->>'error' AS error,
 record->>'endpoint' AS endpoint, record->>'request_type' AS request_type,
 record->>'error_source' AS error_source,record->>'error_phase' AS error_phase,
 record->>'latency_ms' AS latency_ms,record->>'user_agent' AS user_agent,
 record->'attempts' AS attempts,record->'attempts_truncated' AS attempts_truncated
 FROM request_logs WHERE created_at > now()-interval '24 hours' AND status <> 'ok'
 ORDER BY created_at DESC,id DESC LIMIT 2000) t""")
for row in rows:
    row['error'] = redact(row.get('error') or '')
    row['attempts'] = [{k:v for k,v in item.items() if k in ('attempt','phase','endpoint','status_code','duration_ms','header_ms','error_source','error_code','http_code','error_phase','error','status','latency_ms')}
                       for item in (row.get('attempts') or [])]
    for attempt in row['attempts']:
        if 'error' in attempt: attempt['error'] = redact(attempt['error'])
report['recent_failures'] = rows
counts = collections.Counter((r['status'],r['http_code'],r['error_source'],r['error_phase'],r['error']) for r in rows)
report['failure_groups'] = [dict(zip(('status','http_code','source','phase','error'),entry),count=count) for entry,count in counts.most_common(40)]
report['inference_calls'] = 0
print(json.dumps(report,ensure_ascii=False))
'''

if __name__ == '__main__':
    client = connect()
    try:
        report = json.loads(run(client, 'python3 -c ' + shlex.quote(REMOTE), timeout=120))
    finally:
        client.close()
    out = Path(__file__).resolve().parents[1] / '.local/failure-cache-20260925'
    out.mkdir(parents=True, exist_ok=True)
    (out / 'production-audit.json').write_text(json.dumps(report, ensure_ascii=False, indent=2), encoding='utf-8')
    print(json.dumps({k:v for k,v in report.items() if k != 'recent_failures'}, ensure_ascii=False, indent=2))
