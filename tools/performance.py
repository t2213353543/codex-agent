"""Reproducible synthetic-only PostgreSQL and management API performance checks.

The connection file is private; reports never contain connection strings, cookies,
keys, or account credentials. See integration/performance/README.md.
"""
from __future__ import annotations

from project_layout import backend_source_paths

import argparse
import base64
from concurrent.futures import ThreadPoolExecutor
from datetime import datetime, timezone
import hashlib
import http.client
import json
import math
import os
from pathlib import Path
import re
import subprocess
import sys
import time
from urllib.parse import urlparse

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / '.local' / 'performance-python'))
OUT = ROOT / 'integration' / 'performance' / 'results'
COUNTERS = 'requests,ok,errors,tokens,input,output,cached,latency,latency_n'
SUMS = ','.join(f'COALESCE(sum({c}),0)::bigint AS {c}' for c in COUNTERS.split(','))


def save(name, data):
    OUT.mkdir(parents=True, exist_ok=True)
    (OUT / name).write_text(json.dumps(data, indent=2, ensure_ascii=False, default=str) + '\n', encoding='utf-8')


def config(path):
    cfg = json.loads(Path(path).read_text(encoding='utf-8-sig'))
    dsn = urlparse(cfg['DATABASE_URL'])
    if (cfg.get('environment') != 'isolated-synthetic-only'
            or cfg.get('purpose') != 'codex-agent-performance'
            or cfg.get('synthetic_data') is not True
            or dsn.path != '/codex_agent_performance'
            or dsn.hostname not in ('localhost', '127.0.0.1')):
        raise ValueError('Refusing a database without the dedicated synthetic performance identity')
    return cfg


class PsqlConnection:
    """Server-local read adapter via the already provisioned PG container."""
    def __init__(self):
        self.rows=[]

    @staticmethod
    def literal(value):
        if value is None:return 'NULL'
        if isinstance(value,bool):return 'true' if value else 'false'
        if isinstance(value,(int,float)):return str(value)
        if isinstance(value,list):return 'ARRAY['+','.join(PsqlConnection.literal(v) for v in value)+']'
        return "'"+str(value).replace("'","''")+"'"

    def execute(self,statement,params=None):
        if params:
            parts=statement.split('%s')
            if len(parts)!=len(params)+1:raise ValueError('SQL placeholder mismatch')
            statement=''.join(parts[i]+self.literal(v) for i,v in enumerate(params))+parts[-1]
        if statement.startswith('SHOW '):
            key=statement[5:].strip()
            statement="SELECT current_setting("+self.literal(key)+")"
        is_plan=statement.startswith('EXPLAIN ')
        sql=statement if is_plan else 'SELECT row_to_json(q) FROM ('+statement+') q'
        result=subprocess.run(['docker','exec','-i','codex-agent-integration-postgres','psql','-X','-q','-t','-A','-v','ON_ERROR_STOP=1','-U','codex_agent_test','-d','codex_agent_performance'],input=sql+';\n',text=True,capture_output=True,timeout=90)
        if result.returncode:raise RuntimeError('Dedicated performance psql query failed')
        self.rows=[(json.loads(result.stdout),)] if is_plan else [tuple(json.loads(line).values()) for line in result.stdout.splitlines() if line.strip()]
        return self

    def fetchone(self):return self.rows[0] if self.rows else None
    def fetchall(self):return self.rows
    def __enter__(self):return self
    def __exit__(self,*args):return False


def connect(cfg, psql=False):
    if psql:return PsqlConnection()
    try:
        import psycopg
    except ImportError:
        raise RuntimeError('Install driver: python -m pip install --target .local/performance-python "psycopg[binary]>=3.2,<4"') from None
    conn = psycopg.connect(cfg['DATABASE_URL'], connect_timeout=5, autocommit=True)
    identity = conn.execute('SELECT current_database(),current_user').fetchone()
    if identity[0] != 'codex_agent_performance':
        conn.close()
        raise ValueError('Connected database does not match dedicated performance database')
    conn.execute("SET application_name='codex-agent-performance-harness'")
    conn.execute("SET statement_timeout='60s'")
    conn.execute("SET lock_timeout='5s'")
    return conn


def fingerprints():
    paths = backend_source_paths(ROOT)
    return {str(p.relative_to(ROOT)).replace('\\', '/'): hashlib.sha256(p.read_bytes()).hexdigest() for p in paths}


def base_report(conn):
    report = {
        'recorded_at_utc': datetime.now(timezone.utc).isoformat(),
        'database': 'codex_agent_performance', 'synthetic_only': True,
        'postgres_version': conn.execute('SHOW server_version').fetchone()[0],
        'rows': {table: conn.execute(f'SELECT count(*) FROM {table}').fetchone()[0]
                 for table in ('accounts', 'request_logs', 'usage_events', 'usage_minute', 'usage_daily')},
        'source_sha256_at_measurement': fingerprints(),
    }
    source=ROOT/'source-manifest.json'
    if source.exists():report['source_manifest_sha256']=json.loads(source.read_text())['sha256']
    return report


def seed(conn, cfg, args):
    """Bulk fixture setup, not a claim to validate normal request ingestion."""
    from cryptography.hazmat.primitives.ciphers.aead import AESGCM
    if not 1 <= args.accounts <= 100000 or not 1 <= args.events <= 2000000:
        raise ValueError('Fixture size outside safe performance envelope')
    if conn.execute('SELECT count(*) FROM accounts').fetchone()[0] or conn.execute('SELECT count(*) FROM usage_events').fetchone()[0]:
        raise ValueError('Seed requires an empty dedicated database; existing data was preserved')
    stamp = int(time.time()) // 60 * 60
    aes = AESGCM(base64.b64decode(cfg['CREDENTIAL_ENCRYPTION_KEY']))
    key_id = cfg.get('CREDENTIAL_ENCRYPTION_KEY_ID', 'integration-v1')
    with conn.transaction():
        conn.execute("INSERT INTO groups(id,name) SELECT 'perf_group_'||g,'Synthetic group '||g FROM generate_series(0,9) g")
        conn.execute("INSERT INTO proxies(id,name,type,host,port,enabled) SELECT 'perf_proxy_'||g,'Synthetic disabled proxy '||g,'http','127.0.0.1',9,false FROM generate_series(0,9) g")
        conn.execute("""INSERT INTO accounts(id,display_name,enabled,notes,tags,created_at,updated_at,window_start,expires_at,credential_status)
          SELECT 'perf_account_'||lpad(g::text,6,'0'),'Synthetic account '||lpad(g::text,6,'0'),g%%10<>0,
          'Synthetic performance fixture; never contacts an upstream',ARRAY['synthetic','cohort-'||(g%%10)],%s,%s,%s,%s,'unverified'
          FROM generate_series(1,%s) g""", (stamp, stamp, stamp-30*86400, stamp+365*86400, args.accounts))
        with conn.cursor().copy('COPY account_credentials(account_id,credentials_cipher,token_fingerprint,expires_at,encryption_key_id,updated_at) FROM STDIN') as copy:
            for i in range(1, args.accounts + 1):
                token = f'synthetic-performance-token-{i:06d}'
                plain = json.dumps({'access_token': token, 'expires_at': stamp+365*86400}).encode()
                nonce = os.urandom(12)
                encrypted = nonce + aes.encrypt(nonce, plain, key_id.encode())
                copy.write_row((f'perf_account_{i:06d}', encrypted, hashlib.sha256(token.encode()).digest(), stamp+365*86400, key_id, stamp))
        conn.execute("INSERT INTO account_groups SELECT id,'perf_group_'||(substring(id FROM 14)::integer%10) FROM accounts")
        conn.execute("INSERT INTO account_proxies SELECT id,'perf_proxy_'||(substring(id FROM 14)::integer%10),0 FROM accounts WHERE substring(id FROM 14)::integer%3=0")
        # Uniform 120-day event history and repeated seconds exercise stable ordering.
        conn.execute("""INSERT INTO usage_events(event_id,created_at,account_ref,account_name,key_id,status,input_tokens,output_tokens,cached_tokens,latency_ms)
          SELECT 'perf_event_'||lpad(g::text,9,'0'),to_timestamp(%s-(g*104729::bigint%%10368000)),
          'perf_account_'||lpad((1+(g%% %s))::text,6,'0'),'Synthetic account '||lpad((1+(g%% %s))::text,6,'0'),
          'perf_key_'||(g%%20),CASE WHEN g%%10=0 THEN 'error' ELSE 'ok' END,1000+g%%400,100+g%%200,800,50+g%%1000
          FROM generate_series(1,%s) g""", (stamp, args.accounts, args.accounts, args.events))
        conn.execute("""INSERT INTO request_logs(event_id,account_id,account_ref,account_name,created_at,status,model,key_name,record)
          SELECT event_id,account_ref,account_ref,account_name,created_at,status,'synthetic-model','Synthetic key',
          jsonb_build_object('event_id',event_id,'account_id',account_ref,'account',account_name,'time',extract(epoch FROM created_at)::bigint,
          'status',status,'model','synthetic-model','key_id',key_id,'key_name','Synthetic key','input_tokens',input_tokens,
          'output_tokens',output_tokens,'cached_tokens',cached_tokens,'latency_ms',latency_ms,
          'error',CASE WHEN status='ok' THEN '' ELSE 'Synthetic transient failure' END) FROM usage_events""")
        conn.execute("""INSERT INTO account_request_state(account_ref,account_name,last_at,last_log_id,record)
          SELECT DISTINCT ON(account_ref) account_ref,account_name,created_at,id,record||jsonb_build_object('id',id)
          FROM request_logs ORDER BY account_ref,created_at DESC,id DESC""")
    conn.execute('ANALYZE')
    report = base_report(conn)
    report.update({'fixture_at': stamp, 'history_days': 120, 'expected_events': args.events,
                   'expected_tokens': conn.execute('SELECT sum(input_tokens+output_tokens) FROM usage_events').fetchone()[0],
                   'setup': 'Bulk synthetic fixture only. Run actual rebuild-usage CLI before API timing.'})
    save('fixture.json', report)
    print(f'Seeded {args.accounts} synthetic accounts and {args.events} durable events in dedicated performance database')


def query_cases(conn):
    now = int(time.time())//60*60
    start = (now-30*86400)//86400*86400
    keys = [r[0] for r in conn.execute('SELECT id FROM accounts ORDER BY id LIMIT 50').fetchall()]
    if not keys:
        raise ValueError('EXPLAIN requires the synthetic account fixture')
    cursor = conn.execute('SELECT created_at,id FROM request_logs ORDER BY created_at DESC,id DESC OFFSET 50000 LIMIT 1').fetchone()
    cases = [
        ('logs_first_50', 'stats_pg.go/logsPG', 'SELECT id,record FROM request_logs WHERE TRUE ORDER BY created_at DESC,id DESC LIMIT %s', [51]),
        ('logs_account_50', 'stats_pg.go/logsPG', 'SELECT id,record FROM request_logs WHERE account_ref=%s ORDER BY created_at DESC,id DESC LIMIT %s', [keys[0],51]),
        ('logs_errors_50', 'stats_pg.go/logsPG', "SELECT id,record FROM request_logs WHERE status<>'ok' ORDER BY created_at DESC,id DESC LIMIT %s", [51]),
        ('overview_totals_30d', 'stats_pg.go/totalsPG', f'SELECT {SUMS} FROM usage_minute WHERE bucket >= %s AND bucket < %s', [now-30*86400,now+60]),
        ('overview_totals_retained_92d', 'stats_pg.go/totalsPG', f'SELECT {SUMS} FROM usage_minute WHERE bucket >= %s AND bucket < %s', [now-92*86400,now+60]),
        ('overview_series_30d', 'stats_pg.go/seriesPG', f'SELECT (bucket-%s)/%s AS slot,{SUMS} FROM usage_minute WHERE bucket >= %s AND bucket < %s GROUP BY slot', [now-30*86400,86400,now-30*86400,now+60]),
        ('overview_top_30d', 'stats_pg.go/accountTopPG', "SELECT max(account_name),sum(tokens)::bigint,sum(requests)::bigint,sum(errors)::bigint FROM usage_minute WHERE bucket >= %s AND bucket < %s AND account_name<>'' GROUP BY account_ref ORDER BY sum(tokens) DESC,account_ref LIMIT %s", [now-30*86400,now+60,10]),
        ('account_identity_50', 'stats_pg.go/BatchAccountMetrics', 'SELECT id,display_name FROM accounts WHERE id=ANY(%s::text[]) OR display_name=ANY(%s::text[])', [keys,keys]),
        ('account_metrics_50', 'stats_pg.go/BatchAccountMetrics', f'WITH wanted AS (SELECT * FROM unnest(%s::text[],%s::text[],%s::bigint[]) AS w(identity,account_ref,start_at)), selected AS MATERIALIZED (SELECT * FROM usage_daily WHERE account_ref=ANY(%s::text[]) AND bucket >= %s) SELECT w.identity,{SUMS} FROM wanted w JOIN selected u ON u.account_ref=w.account_ref AND u.bucket>=w.start_at GROUP BY w.identity', [keys,keys,[start]*len(keys),keys,start]),
        ('account_state_50', 'stats_pg.go/BatchAccountMetrics', 'SELECT account_ref,record FROM account_request_state WHERE account_ref=ANY(%s::text[])', [keys]),
    ]
    if cursor:
        cases.append(('logs_deep_cursor_50','stats_pg.go/logsPG','SELECT id,record FROM request_logs WHERE (created_at,id)<(%s,%s) ORDER BY created_at DESC,id DESC LIMIT %s',[cursor[0],cursor[1],51]))
    source = (ROOT/'accounts_pg.go').read_text(encoding='utf-8')
    match = re.search(r'const accountColumns = `([^`]+)`', source)
    if not match:
        raise ValueError('Cannot locate delivered accountColumns; update query adapter')
    account_columns=match.group(1)
    cases.extend([
        ('accounts_first_50','accounts_pg.go/ListPage','SELECT '+account_columns+' FROM accounts a WHERE true ORDER BY a.created_at DESC,a.id DESC LIMIT %s',[51]),
        ('accounts_first_500','accounts_pg.go/ListPage','SELECT '+account_columns+' FROM accounts a WHERE true ORDER BY a.created_at DESC,a.id DESC LIMIT %s',[501]),
        ('accounts_search_50','accounts_pg.go/ListPage','SELECT '+account_columns+' FROM accounts a WHERE strpos(lower(a.display_name),lower(%s))>0 ORDER BY a.created_at DESC,a.id DESC LIMIT %s',['Synthetic account 0000',51]),
        ('accounts_group_50','accounts_pg.go/ListPage','SELECT '+account_columns+' FROM accounts a WHERE EXISTS(SELECT 1 FROM account_groups g WHERE g.account_id=a.id AND g.group_id=%s) ORDER BY a.created_at DESC,a.id DESC LIMIT %s',['perf_group_1',51]),
        ('overview_all_accounts','accounts_pg.go/pgList(false)','SELECT '+account_columns+' FROM accounts a ORDER BY a.created_at,a.id',[]),
    ])
    more_keys=[r[0] for r in conn.execute('SELECT id FROM accounts ORDER BY id LIMIT 500').fetchall()]
    original=next(c for c in cases if c[0]=='account_metrics_50')
    cases.append(('account_metrics_500',original[1],original[2],[more_keys,more_keys,[start]*len(more_keys),more_keys,start]))
    return cases


def explain(conn, args):
    report = base_report(conn)
    if args.binary:
        report['binary_sha256']=hashlib.sha256(Path(args.binary).read_bytes()).hexdigest()
    report['cases'] = []
    for name, origin, statement, values in query_cases(conn):
        started = time.perf_counter()
        plan = conn.execute('EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) '+statement, values).fetchone()[0][0]
        report['cases'].append({'name':name,'source':origin,'sql':statement,'parameters':values,'client_ms':round((time.perf_counter()-started)*1000,3),'execution_ms':plan.get('Execution Time'),'planning_ms':plan.get('Planning Time'),'plan':plan})
        print(f'{name}: PostgreSQL execution {plan.get("Execution Time")} ms')
    report['pg_stat_statements_available'] = conn.execute("SELECT to_regclass('pg_stat_statements') IS NOT NULL").fetchone()[0]
    save('explain.json',report)


def api_request(base, path, headers=None, method='GET', body=None):
    parsed = urlparse(base)
    if parsed.hostname not in ('localhost','127.0.0.1') or parsed.port not in (28880,28882):
        raise ValueError('Performance HTTP target must be dedicated loopback port 28880 or its 28882 tunnel')
    client = http.client.HTTPConnection(parsed.hostname,parsed.port,timeout=35)
    start = time.perf_counter()
    try:
        client.request(method,path,body=body,headers=headers or {})
        response=client.getresponse()
        payload=response.read()
        elapsed=(time.perf_counter()-start)*1000
        return response.status,payload,dict(response.getheaders()),elapsed
    finally:
        client.close()


def quantile(values, p):
    ordered=sorted(values)
    return round(ordered[max(0,math.ceil(p*len(ordered))-1)],3) if ordered else None


def benchmark(conn,cfg,args):
    if not args.admin_key_file:
        raise ValueError('benchmark requires --admin-key-file; key content is never recorded')
    key=Path(args.admin_key_file).read_text(encoding='utf-8').strip()
    base=cfg.get('base_url','http://127.0.0.1:28880')
    headers={'Authorization':'Bearer '+key}
    if args.auth=='session':
        status,_,response_headers,_=api_request(base,'/api/session',{'Content-Type':'application/json'},'POST',json.dumps({'admin_key':key}))
        if status!=200 or not response_headers.get('Set-Cookie'):
            raise RuntimeError(f'Performance session creation failed: HTTP {status}')
        headers={'Cookie':response_headers['Set-Cookie'].split(';',1)[0]}
    now=int(time.time())
    cases=[('accounts_50','/api/accounts?limit=50'),('accounts_500','/api/accounts?limit=500'),
           ('accounts_filtered','/api/accounts?limit=50&search=Synthetic%20account%200000'),
           ('logs_50','/api/logs?page_size=50'),('logs_errors_50','/api/logs?page_size=50&status=failed'),
           ('logs_account_50','/api/logs?page_size=50&account_id=perf_account_000001'),('overview_cached','/api/overview')]
    for days,step in [(1,300),(7,3600),(30,86400)]:
        cases.append((f'overview_{days}d',f'/api/overview?from={now-days*86400}&to={now}&step={step}'))
    for name,path in list(cases[:1])+list(cases[3:4]):
        status,payload,_,_=api_request(base,path,headers)
        if status==200:
            cursor=json.loads(payload).get('next_cursor')
            if cursor:
                from urllib.parse import quote
                cases.append((name+'_cursor',path+'&cursor='+quote(cursor,safe='')))
    report=base_report(conn) if conn is not None else json.loads(Path(args.metadata).read_text(encoding='utf-8'))
    if args.binary:
        report['binary_sha256']=hashlib.sha256(Path(args.binary).read_bytes()).hexdigest()
    report.update({'auth':args.auth,'samples_per_case':args.samples,'warmups':args.warmups,'concurrency':args.concurrency,
                   'http_started_at_utc':datetime.now(timezone.utc).isoformat(),
                   'measurement':args.measurement,
                   'percentile_method':'nearest rank','cases':[]})
    for name,path in cases:
        for _ in range(args.warmups):
            api_request(base,path,headers)
        with ThreadPoolExecutor(max_workers=args.concurrency) as pool:
            results=list(pool.map(lambda _:api_request(base,path,headers),range(args.samples)))
        success=[r[3] for r in results if r[0]==200]
        item={'name':name,'path':path,'successes':len(success),'failures':len(results)-len(success),
              'status_codes':sorted(set(r[0] for r in results)), 'p50_ms':quantile(success,.5),'p95_ms':quantile(success,.95),
              'min_ms':round(min(success),3) if success else None,'max_ms':round(max(success),3) if success else None,
              'response_bytes':sorted(set(len(r[1]) for r in results)),'samples_ms':[round(r[3],3) for r in results]}
        report['cases'].append(item)
        print(f'{name}: P50={item["p50_ms"]} ms P95={item["p95_ms"]} ms failures={item["failures"]}')
        save(f'http-c{args.concurrency}-{args.auth}.json',report)
    if any(case['failures'] for case in report['cases']):
        raise RuntimeError('One or more benchmark requests failed; report preserved')
    report['http_finished_at_utc']=datetime.now(timezone.utc).isoformat()
    save(f'http-c{args.concurrency}-{args.auth}.json',report)


def verify_retained_aggregates(conn,args):
    """Read-only supplement: compare every aggregate field/bucket with events."""
    baselines=conn.execute('SELECT count(*) FROM usage_legacy_baselines').fetchone()[0]
    if baselines:
        raise ValueError('Supplement expects the synthetic fixture without legacy baselines')
    expressions="count(*)::bigint AS requests,count(*) FILTER(WHERE status='ok')::bigint AS ok,count(*) FILTER(WHERE status<>'ok')::bigint AS errors,sum(input_tokens+output_tokens)::bigint AS tokens,sum(input_tokens)::bigint AS input,sum(output_tokens)::bigint AS output,sum(cached_tokens)::bigint AS cached,sum(latency_ms)::bigint AS latency,count(*) FILTER(WHERE latency_ms>0)::bigint AS latency_n"
    expected=conn.execute('SELECT '+expressions+' FROM usage_events').fetchone()
    snapshots={'events':expected,'grand':conn.execute('SELECT '+COUNTERS+" FROM usage_totals WHERE scope='global' AND subject=''").fetchone(),
               'minute':conn.execute('SELECT '+SUMS+' FROM usage_minute').fetchone(),
               'daily':conn.execute('SELECT '+SUMS+' FROM usage_daily').fetchone()}
    checks={'all_snapshots_have_nine_fields':all(len(v)==9 for v in snapshots.values())}
    checks.update({k+'_nine_fields_equal_events':snapshots[k]==expected for k in ('grand','minute','daily')})
    bucket_differences={}
    for table,seconds in [('usage_minute',60),('usage_daily',86400)]:
        expected_sql=f'SELECT (extract(epoch FROM created_at)::bigint/{seconds})*{seconds} AS bucket,account_ref,{expressions} FROM usage_events GROUP BY bucket,account_ref'
        actual_sql='SELECT bucket,account_ref,'+COUNTERS+' FROM '+table
        sql='WITH expected AS ('+expected_sql+'), actual AS ('+actual_sql+'), missing AS (SELECT * FROM expected EXCEPT SELECT * FROM actual), extra AS (SELECT * FROM actual EXCEPT SELECT * FROM expected) SELECT (SELECT count(*) FROM missing) AS missing,(SELECT count(*) FROM extra) AS extra'
        missing,extra=conn.execute(sql).fetchone()
        bucket_differences[table]={'missing':missing,'extra':extra}
        checks[table+'_all_buckets_equal_events']=missing==0 and extra==0
    report=base_report(conn)
    if args.binary:report['binary_sha256']=hashlib.sha256(Path(args.binary).read_bytes()).hexdigest()
    report.update({'read_only':True,'fields':COUNTERS.split(','),'snapshots':snapshots,'bucket_differences':bucket_differences,'checks':checks,
        'boundary':'Post-rebuild current-state comparison, including every minute/day bucket. Supplements earlier retention report; does not recreate lost preprune snapshots.'})
    save('retention-check.json',report)
    print('Retained aggregate supplement: '+('PASS' if all(checks.values()) else 'FAIL'))
    if not all(checks.values()):raise RuntimeError('Retained aggregate supplement failed')


def verify_retention(conn,cfg,args):
    """Run delivered CLI rebuild and prune paths on the dedicated fixture."""
    if not args.binary:
        raise ValueError('retention requires --binary pointing to the delivered backend')
    env=os.environ.copy()
    env.update({k:str(v) for k,v in cfg.items() if k.isupper() and isinstance(v,(str,int,float))})
    binary=Path(args.binary).resolve()
    def command(name, *extra):
        start=time.perf_counter()
        result=subprocess.run([str(binary),name,*extra],env=env,cwd=ROOT,capture_output=True,timeout=120)
        if result.returncode:
            raise RuntimeError(f'Actual {name} CLI failed; private captured output was not printed')
        return round((time.perf_counter()-start)*1000,3),json.loads(result.stdout)
    def snapshot():
        return {'events':conn.execute('SELECT count(*) FROM usage_events').fetchone()[0],
                'detail_logs':conn.execute('SELECT count(*) FROM request_logs').fetchone()[0],
                'event_tokens':conn.execute('SELECT sum(input_tokens+output_tokens) FROM usage_events').fetchone()[0],
                'grand':conn.execute("SELECT "+COUNTERS+" FROM usage_totals WHERE scope='global' AND subject=''").fetchone(),
                'minute':conn.execute('SELECT '+SUMS+' FROM usage_minute').fetchone(),
                'daily':conn.execute('SELECT '+SUMS+' FROM usage_daily').fetchone()}
    first_ms,_=command('rebuild-usage')
    before=snapshot()
    cutoff=datetime.fromtimestamp(int(time.time())-92*86400,timezone.utc)
    old=conn.execute('SELECT count(*) FROM request_logs WHERE created_at<%s',[cutoff]).fetchone()[0]
    if not old and not args.allow_no_expired:
        raise ValueError('No old synthetic detail rows to exercise retention; existing data preserved')
    prune_ms,prune_output=command('prune-logs','--before',cutoff.isoformat())
    after_prune=snapshot()
    second_ms,_=command('rebuild-usage')
    after_rebuild=snapshot()
    checks={
        'exactly_expired_details_removed':before['detail_logs']-after_rebuild['detail_logs']==old,
        'durable_event_count_preserved':before['events']==after_prune['events']==after_rebuild['events'],
        'lifetime_tokens_preserved':before['event_tokens']==after_rebuild['event_tokens']==after_rebuild['grand'][3],
        'pruning_preserves_all_aggregates':all(before[k]==after_prune[k] for k in ('grand','minute','daily')),
        'rebuild_matches_preprune':all(before[k]==after_rebuild[k] for k in ('grand','minute','daily')),
        'cli_reports_exact_deleted_rows':prune_output.get('deleted_logs')==old,
    }
    report=base_report(conn)
    report.update({'binary_sha256':hashlib.sha256(binary.read_bytes()).hexdigest(),'before':before,'after_prune':after_prune,'after_rebuild':after_rebuild,
        'checks':checks,'expired_detail_rows':old,'nonempty_prune_exercised':old>0,'prune_cli':prune_output,'prune_ms':prune_ms,'initial_rebuild_ms':first_ms,'postprune_rebuild_ms':second_ms,
        'boundary':'Actual delivered rebuild-usage and prune-logs commands. Fixture writes are direct SQL, not ingestion-path throughput. Batch bound is source reviewed; per-batch rows are not instrumented.'})
    save('retention.json',report)
    print('Retention/rebuild: '+('PASS' if all(checks.values()) else 'FAIL'))
    if not all(checks.values()):
        raise RuntimeError('Retention/rebuild checks failed')


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action',choices=['seed','explain','benchmark','retention','retention-check'])
    parser.add_argument('--config',default=str(ROOT/'.local/integration/performance-environment.json'))
    parser.add_argument('--accounts',type=int,default=1000)
    parser.add_argument('--events',type=int,default=100000)
    parser.add_argument('--samples',type=int,default=30)
    parser.add_argument('--warmups',type=int,default=3)
    parser.add_argument('--concurrency',type=int,default=1)
    parser.add_argument('--auth',choices=['session','bearer'],default='session')
    parser.add_argument('--admin-key-file')
    parser.add_argument('--binary')
    parser.add_argument('--allow-no-expired',action='store_true',help='Rebuild verification on already-pruned fixture; records nonempty_prune_exercised=false if applicable')
    parser.add_argument('--metadata',help='Existing sanitized DB/source metadata; HTTP-only mode needs no PostgreSQL driver')
    parser.add_argument('--psql',action='store_true',help='Use server-local dedicated integration PG container for read-only SQL/retention checks')
    parser.add_argument('--measurement',default='Wall-clock per HTTP request; new localhost HTTP connection per request. App-to-PG/Redis SSH tunnel latency included. No production traffic.')
    args=parser.parse_args()
    if not 5<=args.samples<=500 or not 1<=args.concurrency<=8 or not 0<=args.warmups<=20:
        parser.error('samples=5..500, concurrency=1..8, warmups=0..20')
    cfg=config(args.config)
    if args.psql and args.action not in ('explain','retention','retention-check'):
        raise ValueError('Server-local psql adapter is limited to explain/retention')
    if args.action=='benchmark' and args.metadata:
        benchmark(None,cfg,args)
        return
    with connect(cfg,args.psql) as conn:
        if args.action=='seed':seed(conn,cfg,args)
        elif args.action=='explain':explain(conn,args)
        elif args.action=='benchmark':benchmark(conn,cfg,args)
        elif args.action=='retention':verify_retention(conn,cfg,args)
        elif args.action=='retention-check':verify_retained_aggregates(conn,args)


if __name__=='__main__':
    try:
        main()
    except Exception as exc:
        # Third-party connection failures can include DSNs; emit exception type only.
        message=str(exc) if isinstance(exc,(ValueError,RuntimeError)) else type(exc).__name__
        print('Performance check failed: '+message,file=sys.stderr)
        sys.exit(1)
