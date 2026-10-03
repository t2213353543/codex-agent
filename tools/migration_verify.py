"""Synthetic legacy migration, restart/export and transaction fault acceptance."""
import argparse
import importlib.util
import json
import os
import select
import socket
import socketserver
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
import subprocess
import sys
import time
import urllib.parse
import uuid

ROOT=Path(__file__).resolve().parents[1]
sys.path.insert(0,str(ROOT/'.local/performance-python'))
sys.path.insert(0,str(ROOT/'integration/tests'))
sys.path.insert(0,str(ROOT/'integration/fixtures'))
from contract import Client, run as run_contract
import synthetic
import psycopg
from psycopg import sql


class RedisBridge:
    """Per-run disconnect injection without stopping the shared test Redis."""
    def __init__(self):
        self.connections=[]
        bridge=self
        class Handler(socketserver.BaseRequestHandler):
            def handle(self):
                try:
                    upstream=socket.create_connection(('127.0.0.1',26379),timeout=3)
                    bridge.connections.extend([self.request,upstream])
                    while True:
                        ready,_,_=select.select([self.request,upstream],[],[],1)
                        for reader in ready:
                            data=reader.recv(65536)
                            if not data:return
                            (upstream if reader is self.request else self.request).sendall(data)
                except OSError:pass
                finally:
                    if 'upstream' in locals():upstream.close()
        class Server(socketserver.ThreadingTCPServer):
            allow_reuse_address=True
            daemon_threads=True
        self.server=Server(('127.0.0.1',26381),Handler)
        threading.Thread(target=self.server.serve_forever,daemon=True).start()
    def stop(self):
        self.server.shutdown();self.server.server_close()
        for connection in self.connections:
            try:connection.shutdown(socket.SHUT_RDWR);connection.close()
            except OSError:pass


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--binary',required=True)
    parser.add_argument('--environment-file',default='.local/integration/environment.json')
    args=parser.parse_args()
    binary=str(Path(args.binary).resolve())
    config=json.loads(Path(args.environment_file).read_text())
    assert config['purpose']=='codex-agent-integration' and config['synthetic_data'] is True
    run_id=uuid.uuid4().hex[:12]
    folder=ROOT/'.local/integration'/('migration-'+run_id)
    folder.mkdir(parents=True)
    legacy=folder/'legacy';legacy.mkdir()
    schema='migration_'+run_id
    db=psycopg.connect(config['DATABASE_URL'],autocommit=True)
    db.execute(sql.SQL('CREATE SCHEMA {}').format(sql.Identifier(schema)))
    url=urllib.parse.urlsplit(config['DATABASE_URL'])
    query=urllib.parse.parse_qsl(url.query)+[('options','-csearch_path='+schema)]
    dsn=urllib.parse.urlunsplit(url._replace(query=urllib.parse.urlencode(query)))
    env={**os.environ,**{k:v for k,v in config.items() if k.isupper() and isinstance(v,str)}}
    env.update(DATABASE_URL=dsn,REDIS_URL=config['REDIS_URL'].replace(':26379/',':26381/'),REDIS_KEY_PREFIX='codex-agent:integration:'+schema+':',REDIS_NAMESPACE='codex-agent:integration:'+schema+':',ADMIN_KEY='ca_'+'synthetic_migration_admin_key_00000000')
    bridge=RedisBridge()
    scoped=psycopg.connect(dsn,autocommit=True)
    admin_key=env['ADMIN_KEY']
    now=int(time.time());expiry=synthetic.EXPIRY
    account={'name':'synthetic-migration-account','access_token':synthetic.jwt('migration-one'),
        'refresh_token':'SYNTHETIC-REFRESH-UNUSABLE','account_id':'synthetic-workspace','expires_at':expiry,
        'enabled':True,'groups':['g_synthetic'],'proxies':['p_synthetic'],'concurrency':1,'weight':1,'window_days':30,'created_at':now-100}
    today_start=((now+8*3600)//86400)*86400-8*3600
    record_times=[today_start-1,today_start+1,now-1]
    records=[{'time':record_times[i],'account':account['name'],'status':'error' if i==2 else 'ok','model':'synthetic-model',
        'input_tokens':100,'output_tokens':20,'cached_tokens':80,'latency_ms':10+i,'http_code':500 if i==2 else 200,'error':'synthetic failure' if i==2 else ''} for i in range(3)]
    docs={'accounts.json':{'active':account['name'],'accounts':[account]},
        'groups.json':{'groups':[{'id':'g_synthetic','name':'synthetic-group','created_at':now-100}]},
        'proxies.json':{'proxies':[{'id':'p_synthetic','name':'synthetic-proxy','type':'http','host':'127.0.0.1','port':29999,'password':'SYNTHETIC-PASSWORD','enabled':True,'created_at':now-100}]},
        'keys.json':{'keys':[]},'settings.json':{'admin_api':False}}
    for name,value in docs.items(): (legacy/name).write_text(json.dumps(value),encoding='utf-8')
    (legacy/'client-key').write_text(admin_key)
    (legacy/'logs.jsonl').write_text(''.join(json.dumps(r)+'\n' for r in records))
    checks=[];process=None
    def cli(*cmd,success=True):
        result=subprocess.run([binary,*cmd],env=env,cwd=ROOT,capture_output=True,text=True,timeout=180)
        if success and result.returncode: raise AssertionError('CLI '+cmd[0]+' failed: '+result.stderr[-800:])
        if not success: assert result.returncode!=0,'invalid source unexpectedly accepted'
        return json.loads(result.stdout) if result.stdout.strip().startswith('{') else result
    def record(identifier,callback):
        try: detail=callback();checks.append({'id':identifier,'status':'pass','detail':detail})
        except Exception as error: checks.append({'id':identifier,'status':'fail','detail':str(error)[:1000]})
        print(checks[-1]['id'],checks[-1]['status'],flush=True)
    def start():
        nonlocal process
        log=(folder/'server.log').open('ab')
        process=subprocess.Popen([binary,'--listen','127.0.0.1:28881','--web',str(ROOT/'frontend/dist')],env=env,cwd=ROOT,stdout=log,stderr=log)
        for _ in range(100):
            if process.poll() is not None: raise AssertionError('isolated server exited; inspect private server.log')
            try:
                if Client('http://127.0.0.1:28881').call('/health/ready')[0]==200: return
            except OSError: pass
            time.sleep(.1)
        raise AssertionError('isolated readiness timeout')
    def stop():
        nonlocal process
        if process and process.poll() is None: process.terminate();process.wait(timeout=20)
        process=None
    def migrate():
        cli('migrate');cli('migrate')
        count=scoped.execute('SELECT count(*) FROM schema_migrations').fetchone()[0]
        assert count>=3
        return f'Fresh and repeated migration succeeded; {count} migration checksums recorded'
    def dryrun_import():
        before=scoped.execute('SELECT count(*) FROM accounts').fetchone()[0]
        preview=cli('import-legacy','--source',str(legacy),'--dry-run')
        assert scoped.execute('SELECT count(*) FROM accounts').fetchone()[0]==before
        result=cli('import-legacy','--source',str(legacy))
        repeated=cli('import-legacy','--source',str(legacy))
        for key in ('accounts','groups','proxies','account_groups','account_proxies','logs','metadata_hash','manifest'):
            assert result[key]==preview[key]==repeated[key],key+' changed'
        assert (result['accounts'],result['account_groups'],result['account_proxies'],result['logs'])==(1,1,1,3)
        assert scoped.execute('SELECT count(*) FROM usage_events').fetchone()[0]==3
        tokens=scoped.execute("SELECT tokens FROM usage_totals WHERE scope='global'").fetchone()[0]
        assert tokens==360,f'usage total expected 360, got {tokens}'
        (folder/'dry-run.json').write_text(json.dumps(preview,indent=2))
        return 'Dry-run wrote nothing; JSON+JSONL import/reimport preserved identities, expiry, relationships and 360 tokens'
    api=Client('http://127.0.0.1:28881',admin_key)
    def atomicity():
        start()
        scoped.execute("CREATE FUNCTION fail_detach() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic detach failure'; END $$")
        for entity,table,identifier in [('groups','account_groups','g_synthetic'),('proxies','account_proxies','p_synthetic')]:
            scoped.execute(sql.SQL('CREATE TRIGGER inject_detach BEFORE DELETE ON {} FOR EACH ROW EXECUTE FUNCTION fail_detach()').format(sql.Identifier(table)))
            status,_,_=api.call('/api/'+entity+'/'+identifier,'DELETE')
            assert status>=500,f'{entity} deletion returned {status}'
            assert scoped.execute(sql.SQL('SELECT count(*) FROM {}').format(sql.Identifier(entity))).fetchone()[0]==1
            assert scoped.execute(sql.SQL('SELECT count(*) FROM {}').format(sql.Identifier(table))).fetchone()[0]==1
            scoped.execute(sql.SQL('DROP TRIGGER inject_detach ON {}').format(sql.Identifier(table)))
        return 'PG trigger failures rolled back both group/proxy entity deletion and account detach'
    def rotation():
        scoped.execute("CREATE FUNCTION fail_key() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.key='admin_key' THEN RAISE EXCEPTION 'synthetic key persistence failure'; END IF; RETURN NEW; END $$")
        scoped.execute('CREATE TRIGGER inject_key BEFORE INSERT OR UPDATE ON settings FOR EACH ROW EXECUTE FUNCTION fail_key()')
        status,_,_=api.call('/api/settings/rotate-key','POST',{'request_id':'rotation-failure-'+run_id})
        assert status>=500,f'rotation failure returned {status}'
        assert api.call('/api/accounts')[0]==200,'old key lost after failed rotation'
        scoped.execute('DROP TRIGGER inject_key ON settings')
        session=Client(api.base)
        assert session.call('/api/session','POST',{'admin_key':admin_key})[0]==200
        request={'request_id':'lost-response-'+run_id}
        first=session.call('/api/settings/rotate-key','POST',request)
        second=session.call('/api/settings/rotate-key','POST',request)
        assert first[0]==second[0]==200 and first[1]['admin_key']==second[1]['admin_key']
        api.key=second[1]['admin_key']
        return 'Persistence failure retains old key; cookie session replays same request_id to recover a lost rotation response'
    def statistics():
        overview=api.call('/api/overview')[1]
        cards=overview['cards']
        assert abs(cards['cache_rate']-.8)<1e-9,'cache ratio differs from cached/input'
        assert cards['tokens_total']==360
        assert cards['tokens_today']==240,'Asia/Shanghai today boundary did not exclude previous-day event'
        logs=api.call('/api/logs?page_size=2')[1]
        assert len(logs['items'])==2 and logs['has_more'] is True
        first_times=[x['time'] for x in logs['items']]
        assert first_times==sorted(record_times,reverse=True)[:2],'logs are not latest-first'
        assert logs.get('total_kind')=='not_computed' and logs.get('total')==-1
        cursor=urllib.parse.quote(logs['next_cursor'],safe='')
        tail=api.call('/api/logs?page_size=2&cursor='+cursor)[1]
        assert len(tail['items'])==1 and tail['items'][0]['time']==record_times[0]
        return 'cache_rate=0.8, total=360, Shanghai today=240; descending stable cursor pagination has exact remaining event'
    def redis_failure():
        nonlocal bridge
        session=Client(api.base)
        assert session.call('/api/session','POST',{'admin_key':api.key})[0]==200
        assert session.call('/api/overview')[0]==200
        bridge.stop()
        try:
            assert session.call('/api/accounts')[0]==503,'Redis failure did not fail cookie authentication closed'
            assert api.call('/api/overview')[0]==200,'valid Bearer overview cannot read PG during Redis failure'
            assert Client(api.base).call('/api/accounts')[0]==401,'missing auth bypassed during Redis outage'
            assert Client(api.base,'invalid-synthetic-key').call('/api/accounts')[0]==401
            assert Client(api.base).call('/health/ready')[0]==503
        finally:bridge=RedisBridge()
        for _ in range(30):
            if session.call('/api/accounts')[0]==200:break
            time.sleep(.1)
        else:raise AssertionError('session did not recover after Redis connectivity restored')
        return 'Redis connections cut locally: session503, invalid/missing401, authorized overview200 from PG; existing session recovers'
    def proxy_status():
        class ProxyHandler(BaseHTTPRequestHandler):
            def do_CONNECT(self):
                self.send_response(self.server.synthetic_status)
                self.end_headers()
            def log_message(self,*args):pass
        server=ThreadingHTTPServer(('127.0.0.1',0),ProxyHandler)
        threading.Thread(target=server.serve_forever,daemon=True).start()
        try:
            for code in (403,407,429):
                server.synthetic_status=code
                status,body,_=api.call('/api/proxies/test','POST',{'name':'synthetic-diagnostic','type':'http','host':'127.0.0.1','port':server.server_port,'username':'synthetic','password':'synthetic-proxy-secret'})
                assert status==200,f'CONNECT {code} probe request returned HTTP {status}'
                assert body.get('ok') is False,f'CONNECT {code} incorrectly reported ok=true'
                assert body.get('http_code')==code,f'CONNECT {code} diagnostic lost observed HTTP status'
                if code==407:assert body.get('authentication')=='failed','407 did not mark proxy authentication failed'
                assert body.get('error_code') and 'synthetic-proxy-secret' not in json.dumps(body)
        finally:server.shutdown();server.server_close()
        return 'Local synthetic proxy CONNECT 403/407/429 all fail diagnostics with redacted stable errors'
    def restart_export():
        before=api.call('/api/overview')[1]
        accounts=api.call('/api/accounts')[1]
        stop();start()
        after=api.call('/api/overview')[1]
        latest=api.call('/api/accounts')[1]
        assert before['cards']['tokens_total']==after['cards']['tokens_total']==360
        assert accounts['accounts'][0]['status']==latest['accounts'][0]['status']=='error'
        stop()
        exported=folder/'exported'
        cli('export-legacy','--target',str(exported))
        current=json.loads((exported/'accounts.json').read_text())['accounts'][0]
        assert current['access_token']==account['access_token'] and current['refresh_token']==account['refresh_token']
        assert current['expires_at']==expiry and current['groups']==['g_synthetic'] and current['proxies']==['p_synthetic']
        assert (exported/'client-key').read_text().strip()==api.key
        logs=[json.loads(x) for x in (exported/'logs.jsonl').read_text().splitlines() if x]
        assert len(logs)==3
        snapshot=json.loads((exported/'stats.json').read_text())
        assert sum(x['Tokens'] for x in snapshot['minutes'].values())==360
        return ('SIGTERM' if os.name!='nt' else 'Forced Windows')+' process restart preserved usage and error status; export retained latest admin key, credentials, relations, expiry and logs'
    def api_contract():
        configured=dict(config,base_url='http://127.0.0.1:28881')
        for check in run_contract(configured,configured['base_url'],api.key):
            checks.append(check)
            print(check['id'],check['status'],flush=True)
    try:
        record('database.migrations',migrate)
        record('migration.dry_run',dryrun_import)
        record('integration.delete_atomicity',atomicity)
        record('integration.key_rotation',rotation)
        record('integration.statistics',statistics)
        record('integration.redis_failure',redis_failure)
        record('integration.proxy_status',proxy_status)
        api_contract()
        record('integration.restart',restart_export)
    finally:
        stop();bridge.stop();scoped.close();db.close()
    report={'schema':schema,'synthetic_data':True,'checks':checks,'binary_sha256':__import__('hashlib').sha256(Path(binary).read_bytes()).hexdigest()}
    target=folder/'report.json';target.write_text(json.dumps(report,indent=2))
    (ROOT/'.local/integration/migration-latest.json').write_text(json.dumps(report,indent=2))
    print(target)
    return 1 if any(c['status']!='pass' for c in checks) else 0


if __name__=='__main__': raise SystemExit(main())
