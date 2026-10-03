"""Package/stage Codex Agent; production changes require matching release acceptance."""
import argparse
import base64
from datetime import datetime, timezone
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import secrets
import subprocess
import tarfile
from integration_env import connect, run

ROOT = Path(__file__).resolve().parents[1]
REMOTE = '/opt/codex-agent'
DBROOT = '/opt/codex-agent-db'


def digest(path):
    return hashlib.sha256(Path(path).read_bytes()).hexdigest()


def source():
    spec = importlib.util.spec_from_file_location('acceptance', ROOT/'tools/acceptance.py')
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module.source_manifest()


def check_report(path, required=True):
    if not path or not Path(path).is_file():
        if required: raise RuntimeError('A passing release-profile acceptance report is required')
        return None
    report = json.loads(Path(path).read_text(encoding='utf-8'))
    if report.get('source', {}).get('sha256') != source()['sha256']:
        raise RuntimeError('Acceptance source fingerprint is stale')
    if required and not (report.get('profile') == 'release' and report.get('release_ready') is True):
        raise RuntimeError('Complete release acceptance has not passed')
    return report


def prepare(args):
    before = source()
    report = check_report(args.acceptance, required=False)
    if not (ROOT/'frontend/dist/index.html').is_file(): raise RuntimeError('Build delivered frontend first')
    output = ROOT/'.local/releases'
    output.mkdir(parents=True, exist_ok=True)
    binary = output/'candidate-linux-amd64'
    subprocess.run(['go','build','-trimpath','-ldflags=-s -w','-o',str(binary),'.'],cwd=ROOT,
        env={**os.environ,'GOOS':'linux','GOARCH':'amd64','CGO_ENABLED':'0'},check=True)
    frontend = {p.relative_to(ROOT/'frontend/dist').as_posix():digest(p) for p in sorted((ROOT/'frontend/dist').rglob('*')) if p.is_file()}
    if before['sha256'] != source()['sha256']: raise RuntimeError('Source changed during package build')
    acceptance_hash=digest(args.acceptance) if args.acceptance else None
    rid = hashlib.sha256(json.dumps({'source':before['sha256'],'binary':digest(binary),'frontend':frontend,'acceptance':acceptance_hash},sort_keys=True).encode()).hexdigest()
    folder = output/rid
    folder.mkdir(exist_ok=True)
    manifest = {'release_id':rid,'prepared_at':datetime.now(timezone.utc).isoformat(),'source_sha256':before['sha256'],
        'binary_sha256':digest(binary),'frontend':frontend,'readiness_path':'/health/ready',
        'tests_passed':bool(report and report.get('release_ready')),'migrated':False,'deployed':False,
        'acceptance_report_sha256':acceptance_hash,
        'versions':{name:digest(ROOT/name) if (ROOT/name).is_file() else None for name in ['docs/BACKEND-HANDOFF.md','frontend/FRONTEND-HANDOFF.md']}}
    path=folder/'release-manifest.json'
    if path.is_file():
        previous=json.loads(path.read_text(encoding='utf-8'))
        manifest['prepared_at']=previous['prepared_at']
        if previous!=manifest: raise RuntimeError('Release identity collision; existing immutable manifest differs')
    path.write_text(json.dumps(manifest,ensure_ascii=False,indent=2),encoding='utf-8')
    with tarfile.open(folder/'release.tar.gz','w:gz') as tar:
        tar.add(binary,arcname='codex-agent')
        tar.add(ROOT/'frontend/dist',arcname='frontend/dist')
        tar.add(path,arcname='release-manifest.json')
        for name in ['deploy/release_remote.py','deploy/codex-agent.service','docs/API-CONTRACT.md','docs/BACKEND-HANDOFF.md','frontend/FRONTEND-HANDOFF.md']:
            if (ROOT/name).is_file(): tar.add(ROOT/name,arcname=name)
    print(path)
    print(f"Packaged; tests_passed={manifest['tests_passed']}; migrated=False; deployed=False")


def load_release(args):
    if not args.release: raise RuntimeError('--release must name a local release-manifest.json')
    path=Path(args.release).resolve()
    manifest=json.loads(path.read_text(encoding='utf-8'))
    rid=manifest['release_id']
    if len(rid)!=64 or any(c not in '0123456789abcdef' for c in rid): raise RuntimeError('Invalid release ID')
    return path,manifest,REMOTE+'/releases/'+rid


def put(client,path,content,mode=0o600):
    with client.open_sftp() as sftp:
        with sftp.file(path,'w') as handle:
            handle.set_pipelined(True)
            handle.write(content)
        sftp.chmod(path,mode)


def stage(client,path,manifest,destination):
    archive=path.parent/'release.tar.gz'
    if not archive.is_file(): raise RuntimeError('Release archive missing')
    import shlex
    def verify(target):
        expected={'codex-agent':manifest['binary_sha256'],'release-manifest.json':digest(path)}
        expected.update({'frontend/dist/'+name:value for name,value in manifest['frontend'].items()})
        script="import hashlib,json,pathlib; root=pathlib.Path("+repr(target)+"); expected=json.loads("+repr(json.dumps(expected))+"); assert all(hashlib.sha256((root/name).read_bytes()).hexdigest()==value for name,value in expected.items()); print('artifact hashes verified')"
        run(client,'python3 -c '+shlex.quote(script))
    if run(client,'test -d '+destination+' && echo exists || true').strip():
        verify(destination)
        print('Identical immutable candidate already staged; service unchanged');return
    staging=REMOTE+'/releases/.stage-'+manifest['release_id']
    run(client,'test -f /opt/codex-agent/codex-agent.service; test ! -L '+staging+'; install -d -m 755 '+staging)
    chunks=[]
    with archive.open('rb') as handle:
        index=0
        while block:=handle.read(4*1024*1024):
            checksum=hashlib.sha256(block).hexdigest()
            part=staging+'/'+str(index)+'.'+checksum+'.part'
            existing=run(client,'test -f '+part+' && sha256sum '+part+' || true').split()
            if not existing or existing[0]!=checksum: put(client,part,block)
            chunks.append(part); index+=1
    run(client,'cat '+' '.join(chunks)+' > '+staging+'/release.tar.gz')
    if run(client,'sha256sum '+staging+'/release.tar.gz').split()[0]!=digest(archive): raise RuntimeError('Archive checksum mismatch')
    run(client,'tar -xzf '+staging+'/release.tar.gz -C '+staging+'; chmod 755 '+staging+'/codex-agent')
    verify(staging)
    run(client,'flock /opt/codex-agent/.deployment.lock sh -ec '+shlex.quote('test ! -e '+destination+'; mv '+staging+' '+destination))
    print('Candidate staged: '+destination+'; production service unchanged')


def provision(client):
    marker=DBROOT+'/.codex-agent-db'
    if run(client,'test -f '+marker+' && echo exists || true').strip():
        run(client,'test -s /etc/codex-agent/production.env; test -s /etc/codex-agent/migration.env')
        run(client,'docker compose -p codex-agent-db -f '+DBROOT+'/compose.json up -d --wait')
        print('Dedicated dependencies started; existing secrets preserved; release readiness remains separate'); return
    run(client,'test ! -e '+DBROOT+'; install -d -m 700 '+DBROOT+' /etc/codex-agent; install -d -m 755 '+DBROOT+'/initdb')
    admin,migrator,runtime,redis_pw=[secrets.token_hex(32) for _ in range(4)]
    key=base64.b64encode(secrets.token_bytes(32)).decode()
    sql=f"CREATE ROLE codex_agent_migrator LOGIN PASSWORD '{migrator}' NOSUPERUSER NOCREATEDB NOCREATEROLE;\nCREATE ROLE codex_agent_runtime LOGIN PASSWORD '{runtime}' NOSUPERUSER NOCREATEDB NOCREATEROLE CONNECTION LIMIT 16;\nCREATE DATABASE codex_agent OWNER codex_agent_migrator;\nREVOKE CONNECT ON DATABASE codex_agent FROM PUBLIC;\nGRANT CONNECT ON DATABASE codex_agent TO codex_agent_runtime;\n\\connect codex_agent\nREVOKE CREATE ON SCHEMA public FROM PUBLIC;\nGRANT USAGE ON SCHEMA public TO codex_agent_runtime;\nALTER DEFAULT PRIVILEGES FOR ROLE codex_agent_migrator IN SCHEMA public GRANT SELECT,INSERT,UPDATE,DELETE ON TABLES TO codex_agent_runtime;\nALTER DEFAULT PRIVILEGES FOR ROLE codex_agent_migrator IN SCHEMA public GRANT USAGE,SELECT ON SEQUENCES TO codex_agent_runtime;\n"
    put(client,DBROOT+'/initdb/01-user.sql',sql,0o644)
    put(client,DBROOT+'/redis.conf',f'bind 0.0.0.0\nprotected-mode yes\nappendonly yes\nappendfsync everysec\nmaxmemory 2gb\nmaxmemory-policy noeviction\nauto-aof-rewrite-percentage 100\nauto-aof-rewrite-min-size 64mb\nuser default off\nuser codex_agent_runtime on >{redis_pw} ~codex-agent:production:* &codex-agent:production:* +@all -@dangerous\n',0o644)
    logging={'driver':'json-file','options':{'max-size':'20m','max-file':'3'}}
    compose={'name':'codex-agent-db','services':{
        'postgres':{'image':'postgres:18-alpine','container_name':'codex-agent-postgres','environment':{'POSTGRES_PASSWORD':admin},'ports':['127.0.0.1:25433:5432'],'volumes':['pgdata:/var/lib/postgresql','./initdb:/docker-entrypoint-initdb.d:ro'],'command':['postgres','-c','max_connections=40','-c','shared_buffers=128MB','-c','statement_timeout=15000','-c','lock_timeout=5000'],'restart':'unless-stopped','mem_limit':'768m','cpus':1.0,'pids_limit':128,'healthcheck':{'test':['CMD-SHELL','pg_isready -U postgres -d postgres'],'interval':'2s','timeout':'2s','retries':30},'logging':logging},
        'redis':{'image':'redis:8-alpine','container_name':'codex-agent-redis','ports':['127.0.0.1:26380:6379'],'volumes':['redisdata:/data','./redis.conf:/usr/local/etc/redis/redis.conf:ro'],'command':['redis-server','/usr/local/etc/redis/redis.conf'],'restart':'unless-stopped','mem_limit':'4g','pids_limit':128,'logging':logging}},'volumes':{'pgdata':{},'redisdata':{}}}
    put(client,DBROOT+'/compose.json',json.dumps(compose,indent=2))
    env={'DATABASE_URL':f'postgres://codex_agent_runtime:{runtime}@127.0.0.1:25433/codex_agent?sslmode=disable','REDIS_URL':f'redis://codex_agent_runtime:{redis_pw}@127.0.0.1:26380/0','REDIS_KEY_PREFIX':'codex-agent:production:','REDIS_NAMESPACE':'codex-agent:production:','CREDENTIAL_ENCRYPTION_KEY':key,'CREDENTIAL_ENCRYPTION_KEY_ID':'production-v1','ADMIN_SESSION_TTL':'28800','ADMIN_COOKIE_SECURE':'false','STATS_TIMEZONE':'Asia/Shanghai','PG_MAX_CONNS':'12'}
    put(client,'/etc/codex-agent/production.env',''.join(k+'='+v+'\n' for k,v in env.items()))
    put(client,'/etc/codex-agent/migration.env',f'DATABASE_URL=postgres://codex_agent_migrator:{migrator}@127.0.0.1:25433/codex_agent?sslmode=disable\n')
    put(client,marker,'codex-agent-db\n')
    run(client,'docker compose -p codex-agent-db -f '+DBROOT+'/compose.json up -d --wait',180)
    print('Dedicated dependencies ready; application not migrated or switched')


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action',choices=['prepare','stage','provision','preflight','cutover','rollback','status'])
    parser.add_argument('--release',help='local release-manifest.json')
    parser.add_argument('--acceptance',help='matching release-profile report.json')
    args=parser.parse_args()
    if args.action=='prepare': prepare(args); return
    path,manifest,destination=load_release(args)
    if args.action in ('provision','preflight','cutover'):
        check_report(args.acceptance)
        if not manifest.get('tests_passed') or manifest.get('source_sha256')!=source()['sha256'] or manifest.get('acceptance_report_sha256')!=digest(args.acceptance):
            raise RuntimeError('Candidate is not bound to the passing release report')
    client=connect()
    try:
        if args.action=='stage': stage(client,path,manifest,destination)
        elif args.action=='provision': provision(client)
        else:
            action='inspect' if args.action=='status' else args.action
            print(run(client,'python3 '+destination+'/deploy/release_remote.py '+action+' --release '+destination,timeout=2100))
    finally: client.close()


if __name__=='__main__': main()
