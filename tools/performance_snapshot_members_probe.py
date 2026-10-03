"""Prepare an exact, bounded snapshot-membership SQL experiment; no PG by default.

Prepare: python tools/performance_snapshot_members_probe.py
After integration PG is available: python tools/performance_snapshot_members_probe.py --execute
Only codex-agent-integration-postgres / codex_agent_integration is supported.
This is a synthetic SQL prototype, not an application or migration change.
"""
from __future__ import annotations

import argparse
import datetime as dt
import hashlib
import json
import pathlib
import secrets
import sys

ROOT = pathlib.Path(__file__).resolve().parents[1]
CONTAINER = "codex-agent-integration-postgres"
DATABASE = "codex_agent_integration"
ROLE = "codex_agent_test"
SCHEMA = "perf_snapshot_members_probe"
CHUNK = 4096
MAX_ROWS = 1_000_000
EXPECTED_LABELS = {"com.docker.compose.project": "codex-agent-integration", "com.docker.compose.service": "postgres"}
LOCK = 771603003
OPTIONS = "-c statement_timeout=30000 -c lock_timeout=5000 -c work_mem=8MB -c hash_mem_multiplier=1 -c temp_file_limit=256MB -c max_parallel_workers_per_gather=0 -c jit=off -c enable_hashagg=off"
BOUNDARIES = [-9223372036854775808, -4097, -4096, -1, 0, 9223372036854775807]


def capture(snapshot: str, where: str, packed: bool, barrier: bool = False) -> str:
    # Snapshot names and predicates are exclusively fixed strings in this tool.
    assert snapshot.replace("_", "").isalnum() and where in ("true", "status='selected'", "false")
    if not packed:
        # Match admin_usage_snapshot.go's INSERT ... SELECT. Its RowsAffected
        # comes from the command tag, without RETURNING, CTE or count scan.
        assert not barrier
        return f"INSERT INTO exact_members(snapshot_id,log_id,expires_at) SELECT '{snapshot}',id,now()+interval '5 minutes' FROM fixture_logs WHERE {where};\n"
    gate = f"barrier AS MATERIALIZED (SELECT pg_advisory_xact_lock({LOCK + 1}))," if barrier else ""
    from_gate = " CROSS JOIN barrier" if barrier else ""
    selected = f"WITH {gate} selected AS MATERIALIZED (SELECT id FROM fixture_logs{from_gate} WHERE {where})"
    body = f""", ranked AS (
 SELECT id,(row_number() OVER(ORDER BY id)-1)/{CHUNK} AS chunk_no FROM selected
), written AS (
 INSERT INTO packed_members(snapshot_id,first_id,last_id,ids,expires_at)
 SELECT '{snapshot}',min(id),max(id),array_agg(id ORDER BY id),now()+interval '5 minutes'
 FROM ranked GROUP BY chunk_no ORDER BY chunk_no
 RETURNING cardinality(ids) AS member_count
) INSERT INTO snapshots(snapshot_id,total,representation)
 SELECT '{snapshot}',coalesce(sum(member_count),0),'packed' FROM written"""
    return selected + body + ";\n"


def simultaneous_capture(snapshot: str, where: str, barrier: bool = False) -> str:
    """Both representations consume exactly one materialized statement snapshot."""
    text = capture(snapshot, where, True, barrier)
    text = text.replace(", ranked AS (", f""", reference_written AS (
 INSERT INTO exact_members(snapshot_id,log_id,expires_at)
 SELECT '{snapshot}',id,now()+interval '5 minutes' FROM selected RETURNING 1
), ranked AS (""", 1)
    # Reference CTE must execute even if selected is empty; count checks below
    # also ensure that both independent insert branches saw the same set.
    return text


def detail(snapshot: str, value: str) -> str:
    return f"""SELECT EXISTS(
 SELECT 1 FROM (
  SELECT last_id,ids FROM packed_members
  WHERE snapshot_id='{snapshot}' AND first_id<=({value})::bigint
  ORDER BY first_id DESC LIMIT 1
 ) candidate WHERE last_id>=({value})::bigint AND ({value})::bigint=ANY(ids)
) AS member"""


def joined(snapshot: str, where: str = "true") -> str:
    return f"""SELECT l.id FROM packed_members c
 CROSS JOIN LATERAL unnest(c.ids) AS member(log_id)
 JOIN fixture_logs l ON l.id=member.log_id
 WHERE c.snapshot_id='{snapshot}' AND {where}"""


def setup_sql(rows: int) -> str:
    boundaries = ",".join(f"('{n}'::bigint,'boundary-{i}','selected')" for i, n in enumerate(BOUNDARIES))
    return f"""-- Fixed disposable schema only. CREATE SCHEMA is executed separately by the guarded runner.
CREATE TABLE fixture_logs (
 id bigint PRIMARY KEY,event_id text NOT NULL UNIQUE,status text NOT NULL,
 payload text NOT NULL DEFAULT 'synthetic; no production records'
);
INSERT INTO fixture_logs(id,event_id,status)
 SELECT i*2,'synthetic-'||i,CASE WHEN i%3=0 THEN 'other' ELSE 'selected' END
 FROM generate_series(1,{rows}) AS series(i);
INSERT INTO fixture_logs(id,event_id,status) VALUES {boundaries};
CREATE TABLE snapshots(snapshot_id text PRIMARY KEY,total bigint NOT NULL,representation text NOT NULL);
CREATE TABLE exact_members (
 snapshot_id text NOT NULL,log_id bigint NOT NULL,expires_at timestamptz NOT NULL,
 PRIMARY KEY(snapshot_id,log_id)
);
CREATE INDEX exact_members_expiry_idx ON exact_members(expires_at);
CREATE FUNCTION valid_packed_ids(values_ bigint[]) RETURNS boolean LANGUAGE sql IMMUTABLE AS $f$
 SELECT cardinality(values_) BETWEEN 1 AND {CHUNK} AND array_ndims(values_)=1
 AND array_lower(values_,1)=1 AND NOT EXISTS (
  SELECT 1 FROM (SELECT value,lag(value) OVER(ORDER BY position) AS previous
   FROM unnest(values_) WITH ORDINALITY AS item(value,position)) checked
  WHERE value IS NULL OR value<=previous
 )
$f$;
CREATE TABLE packed_members (
 snapshot_id text NOT NULL,first_id bigint NOT NULL,last_id bigint NOT NULL,
 ids bigint[] NOT NULL,expires_at timestamptz NOT NULL,
 PRIMARY KEY(snapshot_id,first_id),
 CHECK(valid_packed_ids(ids)),CHECK(first_id=ids[1]),
 CHECK(last_id=ids[cardinality(ids)]),CHECK(first_id<=last_id)
);
CREATE INDEX packed_members_expiry_idx ON packed_members(expires_at);
CREATE FUNCTION assert_true(condition_ boolean,message_ text) RETURNS void LANGUAGE plpgsql AS $f$
 BEGIN IF condition_ IS DISTINCT FROM true THEN RAISE EXCEPTION 'prototype assertion: %',message_; END IF; END
$f$;
ANALYZE fixture_logs;
"""


def assertion_sql(snapshot: str) -> str:
    values = ",".join(f"('{n}'::bigint)" for n in [*BOUNDARIES, -2, 1, 2, 4, 6, 7, 9223372036854775806])
    return f"""SELECT assert_true(NOT EXISTS(
 (SELECT log_id FROM exact_members WHERE snapshot_id='{snapshot}' EXCEPT
  SELECT unnest(ids) FROM packed_members WHERE snapshot_id='{snapshot}')
 UNION ALL
 (SELECT unnest(ids) FROM packed_members WHERE snapshot_id='{snapshot}' EXCEPT
  SELECT log_id FROM exact_members WHERE snapshot_id='{snapshot}')
),'exact_membership_{snapshot}');
SELECT assert_true(NOT EXISTS(
 SELECT 1 FROM (SELECT first_id,lag(last_id) OVER(ORDER BY first_id) AS previous
  FROM packed_members WHERE snapshot_id='{snapshot}') c WHERE first_id<=previous
),'disjoint_chunks_{snapshot}');
SELECT assert_true(NOT EXISTS(
 SELECT 1 FROM (VALUES {values}) probe(id)
 WHERE ({detail(snapshot, 'probe.id')}) IS DISTINCT FROM
 EXISTS(SELECT 1 FROM exact_members e WHERE e.snapshot_id='{snapshot}' AND e.log_id=probe.id)
),'indexed_detail_parity_{snapshot}');
SELECT assert_true(NOT EXISTS(
 ({joined(snapshot, "l.status='selected'")} EXCEPT
  SELECT l.id FROM fixture_logs l JOIN exact_members e ON e.log_id=l.id
  WHERE e.snapshot_id='{snapshot}' AND l.status='selected')
 UNION ALL
 (SELECT l.id FROM fixture_logs l JOIN exact_members e ON e.log_id=l.id
  WHERE e.snapshot_id='{snapshot}' AND l.status='selected' EXCEPT
  {joined(snapshot, "l.status='selected'")})
),'current_filtered_join_parity_{snapshot}');
"""


def scripts(rows: int) -> dict[str, str]:
    result = {"01_setup.sql": setup_sql(rows)}
    for where, label in [("true", "all"), ("status='selected'", "filtered")]:
        for packed, representation in [(False, "row"), (True, "packed")]:
            name = f"{representation}_{label}"
            result[f"10_capture_{name}.sql"] = "EXPLAIN (ANALYZE,BUFFERS,WAL,SETTINGS,SUMMARY,FORMAT JSON) " + capture(name, where, packed)
    result["20_semantic_capture.sql"] = simultaneous_capture("semantic", "status='selected'") + assertion_sql("semantic")
    result["21_semantic_mutation.sql"] = """DELETE FROM fixture_logs WHERE id=4;
UPDATE fixture_logs SET status='other' WHERE id=2;
UPDATE fixture_logs SET status='selected' WHERE id=6;
INSERT INTO fixture_logs(id,event_id,status) VALUES(-2,'late-low-id','selected'),(9223372036854775806,'late-high-id','selected');
""" + assertion_sql("semantic") + """SELECT assert_true(NOT EXISTS(
 SELECT 1 FROM packed_members c CROSS JOIN LATERAL unnest(c.ids) member(log_id)
 JOIN fixture_logs l ON l.id=member.log_id
 WHERE c.snapshot_id='semantic' AND l.status='selected' AND l.id IN(-2,2,4,6,9223372036854775806)
),'deleted_filtered_out_and_late_ids_not_returned');
"""
    result["22_empty_snapshot.sql"] = simultaneous_capture("empty", "false") + assertion_sql("empty") + "SELECT assert_true((SELECT total=0 FROM snapshots WHERE snapshot_id='empty'),'empty_total');\n"
    result["23_concurrent_writer.sql"] = f"""BEGIN;
SELECT pg_advisory_xact_lock({LOCK + 1});
INSERT INTO fixture_logs(id,event_id,status) VALUES(-3,'uncommitted-low-id','selected');
UPDATE fixture_logs SET status='other' WHERE id=8;
-- The runner starts 24 on another connection now and confirms an advisory-lock wait.
-- COMMIT is issued only after that confirmation, so the capture's statement
-- snapshot precedes this writer's commit, including its reserved lower ID.
"""
    result["24_concurrent_capture.sql"] = "/* probe_capture_concurrent */\n" + simultaneous_capture("concurrent", "status='selected'", True)
    result["25_concurrent_assert.sql"] = assertion_sql("concurrent") + """SELECT assert_true(NOT (""" + detail("concurrent", "-3") + "),'late_uncommitted_low_id_excluded');\n" + "SELECT assert_true((" + detail("concurrent", "8") + "),'old_selected_version_retained_by_statement_mvcc');\n"
    result["30_explain_detail.sql"] = "EXPLAIN (ANALYZE,BUFFERS,SETTINGS,SUMMARY,FORMAT JSON) " + detail("packed_all", "9223372036854775807") + ";\n"
    result["31_explain_batch_join.sql"] = "EXPLAIN (ANALYZE,BUFFERS,SETTINGS,SUMMARY,FORMAT JSON) SELECT count(*) FROM (" + joined("packed_all") + ") selected;\n"
    result["32_explain_candidate_page.sql"] = """EXPLAIN (ANALYZE,BUFFERS,SETTINGS,SUMMARY,FORMAT JSON)
WITH page AS MATERIALIZED (SELECT id FROM fixture_logs ORDER BY id DESC LIMIT 100)
SELECT count(*) FROM page p JOIN LATERAL (
 SELECT ids,last_id FROM packed_members c WHERE c.snapshot_id='packed_all' AND c.first_id<=p.id
 ORDER BY c.first_id DESC LIMIT 1
) c ON c.last_id>=p.id AND p.id=ANY(c.ids);
"""
    result["33_explain_reference_join.sql"] = """EXPLAIN (ANALYZE,BUFFERS,SETTINGS,SUMMARY,FORMAT JSON)
SELECT count(*) FROM exact_members e JOIN fixture_logs l ON l.id=e.log_id WHERE e.snapshot_id='row_all';
"""
    result["34_explain_membership_antijoin.sql"] = """-- Prototype for the rollup dirty-minute membership guard, without per-row array scans.
EXPLAIN (ANALYZE,BUFFERS,SETTINGS,SUMMARY,FORMAT JSON)
WITH members AS (SELECT unnest(ids) AS id FROM packed_members WHERE snapshot_id='packed_all')
SELECT count(*) FROM fixture_logs l WHERE NOT EXISTS(SELECT 1 FROM members m WHERE m.id=l.id);
"""
    # Characterize, do not conceal, the legacy identity limit. This operation
    # intentionally demonstrates that fixed IDs alone do not detect ID reuse.
    result["40_same_id_reuse_characterization.sql"] = """INSERT INTO fixture_logs(id,event_id,status) VALUES(4,'explicit-new-event-same-id','selected');
SELECT json_build_object('case','explicit_same_id_reuse','legacy_visible',EXISTS(
 SELECT 1 FROM exact_members e JOIN fixture_logs l ON l.id=e.log_id WHERE e.snapshot_id='semantic' AND l.id=4),
 'packed_visible',EXISTS(SELECT 1 FROM packed_members c CROSS JOIN LATERAL unnest(c.ids) m(id)
 JOIN fixture_logs l ON l.id=m.id WHERE c.snapshot_id='semantic' AND l.id=4),
 'note','This inherited fixed-ID behavior is not an event-identity/tombstone guarantee');
"""
    return result


def fair_queries() -> dict[str, str]:
    page = "WITH page AS MATERIALIZED (SELECT id FROM fixture_logs WHERE id>=8192 AND id<8392 ORDER BY id LIMIT 100) "
    each = page + """SELECT p.id FROM page p JOIN LATERAL (
 SELECT first_id,ids,last_id FROM packed_members c WHERE c.snapshot_id=$1 AND c.first_id<=p.id
 ORDER BY c.first_id DESC LIMIT 1
) c ON c.last_id>=p.id AND p.id=ANY(c.ids)"""
    dedup = page + """, chunks AS MATERIALIZED (
 SELECT DISTINCT located.first_id FROM page p CROSS JOIN LATERAL (
  SELECT c.first_id FROM packed_members c WHERE c.snapshot_id=$1 AND c.first_id<=p.id
  ORDER BY c.first_id DESC LIMIT 1
 ) located
), members AS MATERIALIZED (
 SELECT unnest(c.ids) AS id FROM chunks k JOIN packed_members c USING(first_id) WHERE c.snapshot_id=$1
) SELECT p.id FROM page p JOIN members m ON m.id=p.id"""
    return {
        "detail": detail("packed_all", "8190").replace("'packed_all'", "$1"),
        "page_each": each,
        "page_dedup": dedup,
        "batch": "SELECT count(*) FROM (" + joined("packed_all").replace("'packed_all'", "$1") + ") members",
        "reference_batch": "SELECT count(*) FROM exact_members e JOIN fixture_logs l ON l.id=e.log_id WHERE e.snapshot_id=$1",
        "anti": "WITH members AS (SELECT unnest(ids) AS id FROM packed_members WHERE snapshot_id=$1) SELECT count(*) FROM fixture_logs l WHERE NOT EXISTS(SELECT 1 FROM members m WHERE m.id=l.id)",
        "sparse_hit": detail("sparse_full", "9223372036854775807").replace("'sparse_full'", "$1"),
        "sparse_miss": detail("sparse_full", "9223372036854775806").replace("'sparse_full'", "$1"),
        "sparse_page_each": "WITH page AS MATERIALIZED (SELECT ids[i] AS id FROM packed_members c,generate_series(3997,4096) i WHERE c.snapshot_id=$1) SELECT p.id FROM page p JOIN LATERAL (SELECT ids,last_id FROM packed_members c WHERE c.snapshot_id=$1 AND c.first_id<=p.id ORDER BY first_id DESC LIMIT 1) c ON c.last_id>=p.id AND p.id=ANY(c.ids)",
        "sparse_page_dedup": "WITH page AS MATERIALIZED (SELECT ids[i] AS id FROM packed_members c,generate_series(3997,4096) i WHERE c.snapshot_id=$1), members AS MATERIALIZED (SELECT unnest(ids) AS id FROM packed_members WHERE snapshot_id=$1) SELECT p.id FROM page p JOIN members m ON m.id=p.id",
    }


def fair_scripts(rows: int) -> dict[str, str]:
    result = scripts(rows)
    result["01_setup.sql"] += "ALTER TABLE exact_members SET(autovacuum_enabled=false); ALTER TABLE packed_members SET(autovacuum_enabled=false);\n"
    for where, label in [("true", "all"), ("status='selected'", "filtered")]:
        for packed, representation in [(False, "row"), (True, "packed")]:
            result[f"11_fair_{representation}_{label}.sql"] = capture(f"{representation}_{label}", where, packed)
    result["26_sparse_full.sql"] = """INSERT INTO packed_members(snapshot_id,first_id,last_id,ids,expires_at)
SELECT 'sparse_full',min(id),max(id),array_agg(id ORDER BY id),now()+interval '5 minutes'
FROM (SELECT (-9223372036854775808::numeric + floor(18446744073709551615::numeric*i/4095))::bigint AS id FROM generate_series(0,4095)i) s;
SELECT assert_true((SELECT cardinality(ids)=4096 AND ids[1]='-9223372036854775808'::bigint AND ids[4096]='9223372036854775807'::bigint FROM packed_members WHERE snapshot_id='sparse_full'),'full4096_sparse_extremes');
"""
    result["26_sparse_full.sql"] += "SELECT assert_true((" + detail("sparse_full", "9223372036854775807") + ") AND NOT (" + detail("sparse_full", "9223372036854775806") + "),'sparse_full_tail_hit_and_gap_miss');\n"
    for name, query in fair_queries().items():
        result[f"35_query_{name}.sql"] = query + ";\n"
    each, dedup = fair_queries()["page_each"], fair_queries()["page_dedup"]
    result["27_page_parity.sql"] = "SELECT assert_true(NOT EXISTS((" + each.replace("$1", "'packed_all'") + ") EXCEPT (" + dedup.replace("$1", "'packed_all'") + ")) AND NOT EXISTS((" + dedup.replace("$1", "'packed_all'") + ") EXCEPT (" + each.replace("$1", "'packed_all'") + ")),'page_dedup_membership_parity');\n"
    each, dedup = fair_queries()["sparse_page_each"], fair_queries()["sparse_page_dedup"]
    result["27_page_parity.sql"] += "SELECT assert_true(NOT EXISTS((" + each.replace("$1", "'sparse_full'") + ") EXCEPT (" + dedup.replace("$1", "'sparse_full'") + ")) AND NOT EXISTS((" + dedup.replace("$1", "'sparse_full'") + ") EXCEPT (" + each.replace("$1", "'sparse_full'") + ")),'sparse_full_page_membership_parity');\n"
    return result


REMOTE = r'''
import datetime,hashlib,json,os,pathlib,re,selectors,subprocess,time
CONTAINER='codex-agent-integration-postgres'
DATABASE='codex_agent_integration'
ROLE='codex_agent_test'
SCHEMA='perf_snapshot_members_probe'
LOCK=771603003
def run(args,**kwargs):return subprocess.run(args,capture_output=True,text=True,timeout=40,check=True,**kwargs).stdout.strip()
info=json.loads(run(['docker','inspect',CONTAINER]))[0]
assert info['Name']=='/'+CONTAINER and info['State']['Running'],'integration_container_not_running'
labels=info['Config'].get('Labels') or {}
assert labels.get('com.docker.compose.project')=='codex-agent-integration' and labels.get('com.docker.compose.service')=='postgres','integration_container_label_mismatch'
assert PAYLOAD['schema']==SCHEMA and PAYLOAD['database']==DATABASE and 100<=PAYLOAD['rows']<=1000000,'payload_guard'
assert re.fullmatch('[0-9a-f]{24}',PAYLOAD['run_id']),'run_marker_guard'
for name,sql in PAYLOAD['sql'].items():
 assert hashlib.sha256(sql.encode()).hexdigest()==PAYLOAD['sha256'][name],'sql_digest_mismatch'
# PostgreSQL restricts temp_file_limit at login. Bootstrap only this fixed
# integration session as postgres, then run all fixture SQL as the test role.
# No ALTER ROLE, grants, server settings or production credentials are changed.
base=['docker','exec','-i','-e','PGOPTIONS='+PAYLOAD['options'],CONTAINER,'psql','-X','-q','-A','-t','-v','ON_ERROR_STOP=1','-U','postgres','-d',DATABASE]
prefix='SET ROLE '+ROLE+'; SET search_path TO '+SCHEMA+',pg_catalog;\n'
class Session:
 def __init__(self):
  self.p=subprocess.Popen(base,stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=subprocess.STDOUT)
  self.buffer=b'';self.sel=selectors.DefaultSelector();self.sel.register(self.p.stdout,selectors.EVENT_READ)
 def query(self,sql):
  marker='probe_marker_'+str(time.monotonic_ns())
  self.p.stdin.write((sql+'\n\\echo '+marker+'\n').encode());self.p.stdin.flush()
  deadline=time.monotonic()+40
  while marker.encode()+b'\n' not in self.buffer:
   if time.monotonic()>deadline:raise TimeoutError('psql_session_timeout')
   if self.p.poll() is not None:raise RuntimeError('psql_session_ended')
   if self.sel.select(.1):
    block=os.read(self.p.stdout.fileno(),65536)
    if not block:raise RuntimeError('psql_session_closed: '+self.buffer.decode(errors='replace')[-1000:])
    self.buffer+=block
  result,self.buffer=self.buffer.split(marker.encode()+b'\n',1)
  return result.decode().strip()
 def close(self):
  try:
   if self.p.poll() is None:self.p.stdin.close();self.p.wait(timeout=3)
  except Exception:self.p.kill();self.p.wait(timeout=3)
  self.sel.close()
def sql(text):return run(base,input=prefix+text)
guard=Session();created=False;marker='codex-agent-snapshot-members-probe:'+PAYLOAD['run_id']
report={'synthetic_only':True,'run_id':PAYLOAD['run_id'],'container':CONTAINER,'database':DATABASE,'schema':SCHEMA,'rows':PAYLOAD['rows'],'maximum_chunk_ids':4096,'plans':{},'semantic_checks':[],'measurements':[],'production_touched':False,'resources':{k:info['HostConfig'].get(k) for k in ['Memory','NanoCpus','CpuQuota','CpuPeriod']}}
started=time.monotonic()
stage='guarded_schema_creation'
def capture_once(name):
 text=PAYLOAD['sql'][name]
 begin=time.monotonic()
 raw=guard.query('\\timing on\n\\set QUIET off\n'+text+'\n\\echo PROBE_ROW_COUNT :ROW_COUNT\n\\timing off\n\\set QUIET on\n')
 elapsed=(time.monotonic()-begin)*1000
 counts=re.findall(r'PROBE_ROW_COUNT (\d+)',raw)
 timings=re.findall(r'Time: ([0-9.]+) ms',raw)
 tags=re.findall(r'INSERT 0 (\d+)',raw)
 assert len(counts)==1 and len(timings)==1 and len(tags)==1,'capture_command_tag_or_timing_missing'
 assert counts[0]==tags[0],'row_count_did_not_match_insert_command_tag'
 return {'case':name,'wall_ms':round(elapsed,3),'psql_statement_ms':float(timings[0]),'command_tag_rows':int(counts[0])}
try:
 assert guard.query("SET ROLE "+ROLE+"; SELECT current_database()||':'||current_user;")==DATABASE+':'+ROLE,'database_role_guard'
 assert guard.query('SELECT pg_try_advisory_lock('+str(LOCK)+');')=='t','probe_already_running'
 assert guard.query("SELECT EXISTS(SELECT 1 FROM pg_namespace WHERE nspname='"+SCHEMA+"');")=='f','fixed_schema_already_exists_no_overwrite'
 guard.query('BEGIN; CREATE SCHEMA '+SCHEMA+' AUTHORIZATION '+ROLE+"; COMMENT ON SCHEMA "+SCHEMA+" IS '"+marker+"'; COMMIT;")
 created=True
 stage='01_setup.sql'
 sql(PAYLOAD['sql']['01_setup.sql'])
 report['postgres_settings']=json.loads(sql("SELECT json_build_object('version',version(),'work_mem',current_setting('work_mem'),'statement_timeout',current_setting('statement_timeout'),'temp_file_limit',current_setting('temp_file_limit'),'max_parallel_workers_per_gather',current_setting('max_parallel_workers_per_gather'),'enable_hashagg',current_setting('enable_hashagg'));"))
 guard.query(prefix)
 for label in ['all','filtered']:
  for repetition in range(6):
   guard.query('TRUNCATE exact_members,packed_members,snapshots;')
   order=['row','packed'] if repetition%2==0 else ['packed','row']
   for position,representation in enumerate(order):
    name='11_fair_'+representation+'_'+label+'.sql';stage=name
    entry=capture_once(name);entry.update(repetition=repetition,position=position,order=order)
    report['measurements'].append(entry)
   raw_count=int(guard.query("SELECT count(*) FROM exact_members WHERE snapshot_id='row_"+label+"';"))
   packed_count=int(guard.query("SELECT total FROM snapshots WHERE snapshot_id='packed_"+label+"';"))
   assert raw_count==packed_count,'fair_capture_member_count_mismatch'
   assert next(e for e in reversed(report['measurements']) if e['case']=='11_fair_row_'+label+'.sql')['command_tag_rows']==raw_count,'row_command_tag_lost'
 guard.query('TRUNCATE exact_members,packed_members,snapshots;')
 for name in ['11_fair_row_all.sql','11_fair_packed_all.sql','26_sparse_full.sql','27_page_parity.sql']:
  stage=name;sql(PAYLOAD['sql'][name])
 report['semantic_checks'].append({'case':'page_chunk_dedup_equal_to_per_row_membership','passed':True})
 report['plan_statistics']={}
 for stats_mode in ['fresh_no_analyze','analyzed']:
  if stats_mode=='analyzed':sql('ANALYZE packed_members; ANALYZE exact_members; ANALYZE fixture_logs;')
  report['plan_statistics'][stats_mode]=json.loads(sql("SELECT json_agg(json_build_object('table',relname,'reltuples',reltuples,'relpages',relpages)) FROM pg_class WHERE oid IN('packed_members'::regclass,'exact_members'::regclass);"))
  for plan_mode in ['force_custom_plan','force_generic_plan']:
   for name,query in PAYLOAD['queries'].items():
    stage=stats_mode+'/'+plan_mode+'/'+name
    snapshot='sparse_full' if name.startswith('sparse_') else 'row_all' if name=='reference_batch' else 'packed_all'
    raw=guard.query('SET plan_cache_mode='+plan_mode+'; PREPARE probe_plan(text) AS '+query+"; EXPLAIN (ANALYZE,BUFFERS,SETTINGS,SUMMARY,FORMAT JSON) EXECUTE probe_plan('"+snapshot+"'); DEALLOCATE probe_plan;")
    report['plans'][stage]=json.loads(raw)
 guard.query('SET plan_cache_mode=auto;')
 for packed,representation in [(False,'row'),(True,'packed')]:
  stage='capture_explain_'+representation
  text=PAYLOAD['sql']['11_fair_'+representation+'_all.sql'].replace(representation+'_all',representation+'_plan')
  report['plans'][stage]=json.loads(sql('EXPLAIN (ANALYZE,BUFFERS,WAL,SETTINGS,SUMMARY,FORMAT JSON) '+text))
 for name in ['20_semantic_capture.sql','21_semantic_mutation.sql','22_empty_snapshot.sql']:
  stage=name
  sql(PAYLOAD['sql'][name]);report['semantic_checks'].append({'case':name,'passed':True})
 writer=Session();pending=None
 try:
  stage='concurrent_statement_snapshot'
  writer.query(prefix+PAYLOAD['sql']['23_concurrent_writer.sql'])
  pending=subprocess.Popen(base,stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=subprocess.PIPE)
  pending.stdin.write((prefix+PAYLOAD['sql']['24_concurrent_capture.sql']).encode());pending.stdin.close();pending.stdin=None
  deadline=time.monotonic()+3;blocked=False
  while time.monotonic()<deadline:
   blocked=guard.query("RESET ROLE; SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND pid<>pg_backend_pid() AND state='active' AND wait_event_type='Lock' AND query LIKE '/* probe_capture_concurrent */%'); SET ROLE "+ROLE+';')=='t'
   if blocked:break
   time.sleep(.03)
  assert blocked,'capture_did_not_reach_statement_snapshot_barrier'
  writer.query('COMMIT;')
  out,err=pending.communicate(timeout=35)
  if pending.returncode:raise RuntimeError('concurrent_capture_failed: '+err.decode()[:1000])
  sql(PAYLOAD['sql']['25_concurrent_assert.sql']);report['semantic_checks'].append({'case':'concurrent_late_commit_after_statement_snapshot','passed':True})
 finally:
  if pending is not None and pending.poll() is None:pending.kill();pending.wait(timeout=3)
  writer.close()
 stage='identity_and_storage_characterization'
 report['identity_reuse_boundary']=json.loads(sql(PAYLOAD['sql']['40_same_id_reuse_characterization.sql']))
 report['storage']=json.loads(sql("SELECT json_build_object('row_table_bytes',pg_total_relation_size('exact_members'),'packed_table_bytes',pg_total_relation_size('packed_members'),'rows',(SELECT count(*) FROM exact_members),'chunks',(SELECT count(*) FROM packed_members),'packed_ids',(SELECT sum(cardinality(ids)) FROM packed_members),'largest_chunk',(SELECT max(cardinality(ids)) FROM packed_members),'snapshots',(SELECT json_agg(s ORDER BY snapshot_id) FROM snapshots s));"))
 report['status']='pass';report['elapsed_seconds']=round(time.monotonic()-started,3)
except Exception as exc:
 report['status']='fail';report['failure_type']=type(exc).__name__;report['elapsed_seconds']=round(time.monotonic()-started,3)
 report['failure_stage']=stage
 # Only fixed synthetic commands exist here, but avoid embedding full SQL or
 # subprocess arguments in the report; no production diagnostic is read.
 report['failure_hint']=str(exc)[:180] if isinstance(exc,(AssertionError,TimeoutError,RuntimeError)) else 'synthetic_command_failed'
 if isinstance(exc,subprocess.CalledProcessError):report['synthetic_sql_error']=(exc.stderr or '')[:2000]
finally:
 if created:
  try:
   assert guard.query("SELECT obj_description(oid,'pg_namespace')||':'||pg_get_userbyid(nspowner) FROM pg_namespace WHERE nspname='"+SCHEMA+"';")==marker+':'+ROLE,'cleanup_owner_marker_mismatch'
   guard.query('DROP SCHEMA '+SCHEMA+' CASCADE;');report['fixture_removed']=True
  except Exception:report['fixture_removed']=False
 guard.close()
print(json.dumps(report),flush=True)
raise SystemExit(0 if report.get('status')=='pass' and report.get('fixture_removed') else 1)
'''


def prepare(output: pathlib.Path, rows: int) -> dict:
    if not 100 <= rows <= MAX_ROWS:
        raise ValueError("rows must be between 100 and 1000000")
    output.mkdir(parents=True, exist_ok=True)
    if (output / "report.json").exists():
        raise ValueError("output already has evidence; choose a new --output directory")
    generated = fair_scripts(rows)
    for name, sql in generated.items():
        (output / name).write_text(sql, encoding="utf-8")
    payload = {"schema": SCHEMA, "database": DATABASE, "rows": rows, "options": OPTIONS,
               "run_id": secrets.token_hex(12), "sql": generated, "queries": fair_queries(),
               "sha256": {name: hashlib.sha256(sql.encode()).hexdigest() for name, sql in generated.items()}}
    (output / "prepared-payload.json").write_text(json.dumps(payload, indent=2), encoding="utf-8")
    (output / "remote_runner.py").write_text("PAYLOAD=" + repr(payload) + "\n" + REMOTE, encoding="utf-8")
    return payload


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--rows", type=int, default=100_000)
    parser.add_argument("--output", type=pathlib.Path, default=ROOT / ".local/snapshot-members-fair" / dt.datetime.now(dt.timezone.utc).strftime("%Y%m%dT%H%M%S.%fZ"))
    parser.add_argument("--execute", action="store_true", help="Run only when the shared isolated PostgreSQL slot is available")
    args = parser.parse_args()
    payload = prepare(args.output.resolve(), args.rows)
    if not args.execute:
        print(json.dumps({"status": "prepared_only", "rows": args.rows, "maximum_chunk_ids": CHUNK,
                          "postgres_connections": 0, "production_mutations": 0,
                          "sql_files": len(payload["sql"]), "output": str(args.output.resolve()),
                          "future_run": "python tools/performance_snapshot_members_probe.py --execute"}))
        return
    # Import SSH capabilities only in the explicit execution branch.
    from integration_env import connect
    client = connect()
    try:
        stdin, stdout, stderr = client.exec_command("python3 -B -", timeout=650)
        stdin.write((args.output.resolve() / "remote_runner.py").read_text(encoding="utf-8"))
        stdin.channel.shutdown_write()
        raw = stdout.read().decode(); errors = stderr.read(); code = stdout.channel.recv_exit_status()
    finally:
        client.close()
    try:
        report = json.loads(raw)
    except ValueError:
        report = {"status": "failed_before_report", "exit_code": code, "stderr_bytes": len(errors)}
    report["prepared_sql_sha256"] = payload["sha256"]
    report["tool_sha256"] = hashlib.sha256(pathlib.Path(__file__).read_bytes()).hexdigest()
    report["observed_at"] = dt.datetime.now(dt.timezone.utc).isoformat()
    (args.output.resolve() / "report.json").write_text(json.dumps(report, indent=2), encoding="utf-8")
    print(json.dumps({"status": report["status"], "report": str(args.output.resolve() / "report.json"),
                      "production_touched": False, "measurements": report.get("measurements", [])}))
    if code:
        raise SystemExit(code)


if __name__ == "__main__":
    main()
