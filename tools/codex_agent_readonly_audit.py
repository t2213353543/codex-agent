#!/usr/bin/env python3
"""Inspect production metadata and persisted metrics without inference calls."""
from datetime import datetime, timezone
import json
from pathlib import Path
from integration_env import connect, ROOT

REMOTE = r'''
import importlib.util,json,os,hashlib
from pathlib import Path
spec=importlib.util.spec_from_file_location('monitor','/opt/codex-agent-integration/live-monitor/codex_agent_live_monitor.py')
m=importlib.util.module_from_spec(spec);spec.loader.exec_module(m)
runtime,settings=m.read_runtime()
runtime['executable']=os.readlink('/proc/'+str(runtime['pid'])+'/exe')
binary=Path('/proc/'+str(runtime['pid'])+'/exe').read_bytes()
runtime['private422_capture_schema']=('v2' if b'private-inference-422-v2' in binary else 'v1' if b'private-inference-422-v1' in binary else None)
runtime['private422_capture_code_present']=runtime['private422_capture_schema'] is not None and b'diagnostic:inference422:' in binary
del binary
runtime['observer_timer']=m.command(['systemctl','show','codex-agent-observer.timer','--property=ActiveState,SubState,LastTriggerUSec,NextElapseUSecRealtime'])
runtime['observer_service']=m.command(['systemctl','show','codex-agent-observer.service','--property=ActiveState,Result,ExecMainStatus,ExecMainExitTimestamp'])
report={'runtime':runtime,'settings':{k:settings.get(k) for k in m.SAFE_SETTINGS},'inference_calls':0}
report['accounts']=m.sql("SELECT coalesce(json_agg(t),'[]'::json) FROM (SELECT md5(a.id) account_hash,a.enabled,a.concurrency,a.priority,a.weight,a.credential_status,a.version,c.credential_version FROM accounts a LEFT JOIN account_credentials c ON c.account_id=a.id ORDER BY a.ordinal)t")
report['retained_span']=m.sql("SELECT json_build_object('min',min(created_at),'max',max(created_at),'count',count(*)) FROM request_logs")
report['record_keys']=m.sql("SELECT coalesce(json_agg(key),'[]'::json) FROM (SELECT DISTINCT jsonb_object_keys(record) key FROM request_logs WHERE created_at>=now()-interval '24 hours')t")
report['usage_schema_windows']=m.sql("""WITH r AS (SELECT *,coalesce((record->>'input_tokens')::bigint,0) input_tokens,
coalesce((record->>'cached_tokens')::bigint,0) cached_tokens FROM request_logs WHERE created_at>=now()-interval '24 hours')
SELECT coalesce(json_agg(t),'[]'::json) FROM (SELECT w.hours,record->>'endpoint' endpoint,record->>'request_type' request_type,
coalesce(record->>'websocket_warmup','false') websocket_warmup,status,count(*) requests,
count(*) FILTER(WHERE record ? 'usage_known') usage_known_present,
count(*) FILTER(WHERE record->>'usage_known'='true') usage_known_true,
count(*) FILTER(WHERE record->>'usage_known'='false') usage_known_false,
count(*) FILTER(WHERE record ? 'cached_tokens_known') cached_tokens_known_present,
count(*) FILTER(WHERE record->>'cached_tokens_known'='true') cached_tokens_known_true,
count(*) FILTER(WHERE record->>'cached_tokens_known'='false') cached_tokens_known_false,
count(*) FILTER(WHERE cached_tokens>0) cached_positive,count(*) FILTER(WHERE cached_tokens=0) cached_zero,
count(*) FILTER(WHERE record ? 'cache_write_tokens') cache_write_tokens_present,
sum(coalesce((record->>'cache_write_tokens')::bigint,0)) cache_write_tokens_sum,
count(*) FILTER(WHERE record ? 'cache_creation_input_tokens') cache_creation_input_tokens_present,
sum(coalesce((record->>'cache_creation_input_tokens')::bigint,0)) cache_creation_input_tokens_sum,
sum(input_tokens) input_tokens,sum(cached_tokens) cached_tokens
FROM r CROSS JOIN (VALUES(1),(24))w(hours) WHERE created_at>=now()-w.hours*interval '1 hour'
GROUP BY w.hours,endpoint,request_type,websocket_warmup,status ORDER BY 1,2,3,4,5)t""")
errors=m.sql("""SELECT coalesce(json_agg(t),'[]'::json) FROM (SELECT id,created_at,status,model,md5(account_ref) account_hash,
record->>'endpoint' endpoint,record->>'request_type' request_type,record->>'http_code' http_code,
record->>'error_code' error_code,record->>'error_source' error_source,record->>'error_phase' error_phase,
record->>'final_upstream_error_code' final_upstream_error_code,record->>'final_upstream_error_source' final_upstream_error_source,record->>'error_param' error_param,record->'structured_mixed' structured_mixed,
record->>'error' error,record->>'latency_ms' latency_ms,record->'attempts' attempts
FROM request_logs WHERE created_at>=now()-interval '6 hours' AND status<>'ok' ORDER BY id)t""")
for row in errors:
    row['error']=m.redact(row.get('error'))
    attempts=row.get('attempts') if isinstance(row.get('attempts'),list) else []
    row['attempt_count']=len(attempts)
    row['attempts']=[{k:(m.redact(v) if isinstance(v,str) else v) for k,v in attempt.items()
      if k in {'status','http_code','status_code','observed_http_status_code','error_code','error_source','error_phase','phase','error','latency_ms','duration_ms','header_ms','ttft_ms','attempt','attempt_no'}} for attempt in attempts if isinstance(attempt,dict)]
report['errors']=errors
report['timers']=m.command(['systemctl','list-timers','--all','--no-pager','--no-legend','codex-agent*'])
print(json.dumps(report,ensure_ascii=False))
'''


def main():
    client = connect()
    try:
        stdin, stdout, stderr = client.exec_command("python3 -B -", timeout=90)
        stdin.write(REMOTE)
        stdin.channel.shutdown_write()
        output = stdout.read()
        error = stderr.read()
        status = stdout.channel.recv_exit_status()
        if status:
            raise RuntimeError(f"readonly_audit_failed:{status}:{len(error)}")
        report = json.loads(output)
        folder = ROOT / ".local/readonly-audit" / datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")
        folder.mkdir(parents=True)
        (folder / "audit.json").write_text(json.dumps(report,ensure_ascii=False,indent=2),encoding="utf-8")
        print(json.dumps({"evidence":str(folder / "audit.json"),"accounts":report["accounts"],"runtime":report["runtime"],
                          "retained_span":report["retained_span"],"error_records":len(report["errors"])},ensure_ascii=False))
    finally:
        client.close()


if __name__ == "__main__":
    main()
