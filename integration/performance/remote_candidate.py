"""Run the isolated performance candidate beside the integration PG/Redis.

Only /opt/codex-agent-integration/performance and the labeled test container
are managed. SSH uses the current-main helper from sub2api-production.
"""
import argparse
import gzip
import hashlib
import json
from pathlib import Path
import shlex
import sys

ROOT=Path(__file__).resolve().parents[2]
sys.path.insert(0,str(ROOT/'tools'))
import integration_env as ie
import performance as perf
import acceptance

REMOTE='/opt/codex-agent-integration/performance'
CONTAINER='codex-agent-integration-performance'
LABEL='codex-agent.integration.performance'


def upload(client,path,content,mode=0o600):
    ie.write_remote(client,REMOTE+'/'+path,content,mode)


def assert_container(client):
    value=ie.run(client,"docker inspect --format '{{ index .Config.Labels \""+LABEL+"\" }}' "+CONTAINER).strip()
    if value!='true':
        raise RuntimeError('Refusing unlabeled container')


def deploy(client,args):
    binary=Path(args.binary).resolve()
    digest=hashlib.sha256(binary.read_bytes()).hexdigest()
    cfg=perf.config(ROOT/'.local/integration/performance-environment.json')
    cfg['base_url']='http://127.0.0.1:28880'
    key=(ROOT/'.local/integration/performance-data/client-key').read_text().strip()
    ie.run(client,'test -f /opt/codex-agent-integration/.codex-agent-integration && install -d -m 700 '+REMOTE+' '+REMOTE+'/tools')
    # Reuse an official image already installed by the isolated PG setup.
    ie.run(client,'docker image inspect postgres:18-alpine --format {{.Id}}')
    present=ie.run(client,'test ! -f '+REMOTE+'/candidate || sha256sum '+REMOTE+'/candidate').split()
    if not present or present[0]!=digest:
        packed=gzip.compress(binary.read_bytes(),compresslevel=1,mtime=0)
        names=[]
        for i,offset in enumerate(range(0,len(packed),4*1024*1024)):
            name=f'{digest}.gz.part{i:03d}'
            with client.open_sftp() as sftp:
                with sftp.file(REMOTE+'/'+name,'wb') as handle:
                    handle.set_pipelined(True)
                    handle.write(packed[offset:offset+4*1024*1024])
                sftp.chmod(REMOTE+'/'+name,0o600)
            names.append(REMOTE+'/'+name)
        ie.run(client,'cat '+' '.join(shlex.quote(n) for n in names)+' | gzip -d > '+REMOTE+'/candidate.next && chmod 755 '+REMOTE+'/candidate.next')
        actual=ie.run(client,'sha256sum '+REMOTE+'/candidate.next').split()[0]
        if actual!=digest:raise RuntimeError('Remote binary digest mismatch')
    exists=ie.run(client,'docker ps -a --filter name=^/'+CONTAINER+'$ --format {{.Names}}').strip()
    if exists:
        assert_container(client)
        ie.run(client,'docker stop -t 60 '+CONTAINER+' && docker rm '+CONTAINER)
    ie.run(client,'test ! -f '+REMOTE+'/candidate.next || mv '+REMOTE+'/candidate.next '+REMOTE+'/candidate')
    upload(client,'environment.json',json.dumps(cfg))
    env={k:str(v) for k,v in cfg.items() if k.isupper() and isinstance(v,(str,int,float))}
    env['ADMIN_KEY']=key
    upload(client,'candidate.env','\n'.join(k+'='+v for k,v in env.items())+'\n')
    upload(client,'admin-key',key+'\n')
    upload(client,'tools/performance.py',(ROOT/'tools/performance.py').read_text())
    manifest=acceptance.source_manifest()
    upload(client,'source-manifest.json',json.dumps(manifest))
    metadata_command="import sys,json;sys.path.insert(0,'"+REMOTE+"/tools');import performance as p;print(json.dumps(p.base_report(p.PsqlConnection()),default=str))"
    metadata=json.loads(ie.run(client,'python3 -c '+shlex.quote(metadata_command)))
    metadata['measurement_host']='test app, PG and Redis colocated on current main; isolated resources'
    metadata['candidate_binary_sha256']=digest
    upload(client,'metadata.json',json.dumps(metadata,default=str))
    command='docker run -d --name '+CONTAINER+' --label '+LABEL+'=true --network host --memory 512m --cpus 1 --pids-limit 128 --read-only --cap-drop ALL --security-opt no-new-privileges --user 65534:65534 --log-opt max-size=10m --log-opt max-file=2 --env-file '+REMOTE+'/candidate.env --mount type=bind,source='+REMOTE+'/candidate,target=/app/candidate,readonly --entrypoint /app/candidate postgres:18-alpine --listen 127.0.0.1:28880 --web /nonexistent'
    ie.run(client,command)
    perf.save('remote-candidate.json',{'binary_sha256':digest,'container':CONTAINER,'scope':REMOTE,'listen':'127.0.0.1:28880','database':'codex_agent_performance','memory_bytes':512*1024*1024,'cpus':1,'pids_limit':128,'production':False})
    print('Isolated Linux performance candidate started:',digest,flush=True)


def benchmark(client,args):
    assert_container(client)
    upload(client,'tools/performance.py',(ROOT/'tools/performance.py').read_text())
    statement='python3 -u '+REMOTE+'/tools/performance.py benchmark --config '+REMOTE+'/environment.json --metadata '+REMOTE+'/metadata.json --binary '+REMOTE+'/candidate --admin-key-file '+REMOTE+'/admin-key --samples '+str(args.samples)+' --concurrency '+str(args.concurrency)+' --measurement '+shlex.quote('Server-local HTTP timings. Test app, PG and Redis on same host; candidate container CPU=1, memory=512MiB. New HTTP connection per request; cookie authentication. No SSH latency in samples; no production traffic.')
    print(ie.run(client,statement,timeout=300),flush=True)
    path=f'integration/performance/results/http-c{args.concurrency}-session.json'
    destination=perf.OUT/f'http-server-c{args.concurrency}-session.json'
    with client.open_sftp() as sftp:sftp.get(REMOTE+'/'+path,str(destination))
    print('Downloaded actual server-local samples:',destination,flush=True)


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action',choices=['deploy','benchmark','explain','retention','retention-check','status','stop'])
    parser.add_argument('--binary',default=str(ROOT/'.local/integration/performance-server-linux'))
    parser.add_argument('--samples',type=int,default=30)
    parser.add_argument('--concurrency',type=int,default=1)
    parser.add_argument('--allow-no-expired',action='store_true')
    args=parser.parse_args()
    if not 5<=args.samples<=500 or not 1<=args.concurrency<=8:raise ValueError('Sample/concurrency outside permitted performance envelope')
    client=ie.connect()
    try:
        ie.run(client,'test -f /opt/codex-agent-integration/.codex-agent-integration')
        if args.action=='deploy':deploy(client,args)
        elif args.action=='benchmark':benchmark(client,args)
        elif args.action in ('explain','retention','retention-check'):
            upload(client,'tools/performance.py',(ROOT/'tools/performance.py').read_text())
            # SQL adapters use the exact frozen source snapshot alongside candidate.
            upload(client,'accounts_pg.go',(ROOT/'.local/integration/performance-source-current/accounts_pg.go').read_text())
            flags=' --allow-no-expired' if args.allow_no_expired else ''
            print(ie.run(client,'python3 -u '+REMOTE+'/tools/performance.py '+args.action+' --psql --config '+REMOTE+'/environment.json --binary '+REMOTE+'/candidate'+flags,timeout=300))
            with client.open_sftp() as sftp:sftp.get(REMOTE+'/integration/performance/results/'+args.action+'.json',str(perf.OUT/(args.action+'.json')))
        elif args.action=='stop':
            assert_container(client)
            print(ie.run(client,'docker stop -t 60 '+CONTAINER))
        else:
            assert_container(client)
            print(ie.run(client,"docker inspect --format '{{.State.Status}} {{.State.ExitCode}}' "+CONTAINER))
    finally:client.close()


if __name__=='__main__':main()
