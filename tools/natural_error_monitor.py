"""Observe bounded natural WS/HTTP traffic; no inference, settings or account writes."""
import argparse
from datetime import datetime, timezone
import json
from pathlib import Path
import shlex
import time

from integration_env import ROOT, connect, run

SQL = r"""WITH recent AS MATERIALIZED (
 SELECT id,created_at,status,record FROM request_logs
 WHERE created_at>=now()-interval '15 minutes' ORDER BY created_at DESC,id DESC LIMIT 5000
), parsed AS MATERIALIZED (
 SELECT id,created_at,status,p.* FROM recent
 CROSS JOIN LATERAL jsonb_to_record(record) p(request_type text,http_code integer,error_source text,error_phase text,
 terminal_status text,final_upstream_error_code text,endpoint text,attempts jsonb,latency_ms bigint)
), classified AS MATERIALIZED (
 SELECT p.*,a.ws,a.ws101,a.ws_unavailable,a.http,
 CASE WHEN a.ws AND a.http THEN 'ws_then_http' WHEN a.ws THEN 'upstream_ws' ELSE 'upstream_http' END AS upstream_transport
 FROM parsed p CROSS JOIN LATERAL (
 SELECT COALESCE(bool_or(item->>'phase'='websocket'),false) ws,
 COALESCE(bool_or(item->>'phase'='websocket' AND item->>'observed_http_status_code'='101'),false) ws101,
 COALESCE(bool_or(item->>'phase'='websocket' AND item->>'observed_http_status_code' IN('404','405','426','501')),false) ws_unavailable,
 COALESCE(bool_or(item->>'phase'='request'),false) http
 FROM jsonb_array_elements(CASE WHEN jsonb_typeof(p.attempts)='array' THEN p.attempts ELSE '[]'::jsonb END) item
 ) a
), buckets AS (
 SELECT COALESCE(request_type,'unknown') request_type,upstream_transport,status,
 COALESCE(http_code,0) http_code,COALESCE(error_source,'') error_source,COALESCE(error_phase,'') error_phase,
 COALESCE(terminal_status,'') terminal_status,COALESCE(final_upstream_error_code,'') upstream_error_code,
 count(*) requests,count(*) FILTER(WHERE ws101) ws101,count(*) FILTER(WHERE ws_unavailable) ws_unavailable,
 min(id) first_id,max(id) last_id
 FROM classified GROUP BY 1,2,3,4,5,6,7,8
)
SELECT json_build_object('observed_at',now(),'scope','latest 5000 persisted requests within 15 minutes',
 'rows',(SELECT count(*) FROM recent),'oldest',(SELECT min(created_at) FROM recent),
 'latest_id',(SELECT max(id) FROM recent),'cap_reached',(SELECT count(*)=5000 FROM recent),
 'buckets',COALESCE((SELECT json_agg(b ORDER BY requests DESC) FROM buckets b),'[]'::json))"""

REMOTE = r'''
import pathlib,json,subprocess,hashlib
r=json.loads(pathlib.Path('/var/lib/codex-agent-release/active-runtime.json').read_text());a=r['active']
assert a['unit']=='codex-agent.service' or a['unit'].startswith('codex-agent-runtime-') and a['unit'].endswith('.service')
result=subprocess.run(['docker','exec','-e','PGOPTIONS=-c statement_timeout=10000 -c default_transaction_read_only=on',
 'codex-agent-postgres','psql','-X','-q','-At','-v','ON_ERROR_STOP=1','-U','postgres','-d','codex_agent','-c',QUERY],capture_output=True,text=True,timeout=15)
if result.returncode:raise RuntimeError('bounded read-only observation failed')
v=json.loads(result.stdout)
logs=subprocess.check_output(['journalctl','-u',a['unit'],'--since','15 minutes ago','--no-pager','-o','cat'],text=True)
v.update(active_binary_sha256=a['binary_sha256'],active_pid=a['pid'],release=a['release'],
 panic_count=logs.count('panic serving'),persistence_errors=logs.count('metrics persistence failed'),
 generated_requests=0,production_mutations=0)
print(json.dumps(v))
'''

def sample(output):
    client=connect()
    try:
        raw=run(client,'python3 -B -c '+shlex.quote('QUERY='+repr(SQL)+'\n'+REMOTE),timeout=25)
    finally:client.close()
    value=json.loads(raw)
    stamp=datetime.now(timezone.utc).strftime('%Y%m%dT%H%M%S%fZ')
    output.mkdir(parents=True,exist_ok=True)
    (output/(stamp+'.json')).write_text(json.dumps(value,indent=2),encoding='utf-8')
    (output/'latest.json').write_text(json.dumps(value,indent=2),encoding='utf-8')
    return value

def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--watch-seconds',type=int,default=0)
    parser.add_argument('--interval',type=int,default=30)
    parser.add_argument('--output',type=Path,default=ROOT/'.local/natural-errors-20260930')
    args=parser.parse_args()
    if not 15<=args.interval<=60 or not 0<=args.watch_seconds<=3600:parser.error('interval15..60, watch0..3600')
    deadline=time.monotonic()+args.watch_seconds
    previous=None
    while True:
        v=sample(args.output)
        key=(v['latest_id'],v['panic_count'],v['persistence_errors'])
        if key!=previous:
            print(json.dumps({'at':v['observed_at'],'latest_id':v['latest_id'],'rows':v['rows'],
              'cap_reached':v['cap_reached'],'ok':sum(b['requests'] for b in v['buckets'] if b['status']=='ok'),
              'errors':sum(b['requests'] for b in v['buckets'] if b['status']=='error'),
              'aborted':sum(b['requests'] for b in v['buckets'] if b['status']=='aborted'),
              'panic_count':v['panic_count'],'report':str(args.output/'latest.json')}),flush=True)
        previous=key
        if time.monotonic()>=deadline:break
        time.sleep(min(args.interval,max(0,deadline-time.monotonic())))

if __name__=='__main__':main()
