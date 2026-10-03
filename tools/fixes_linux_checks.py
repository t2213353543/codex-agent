"""Run frozen-source integration and race checks in the isolated test services."""
import argparse
import json
import shlex
import time
from pathlib import Path

from integration_env import ROOT, connect, run
from run_linux_checks import scrub

OUT=ROOT/'.local/fixes-20260924/linux-checks'
BACKEND=ROOT/'.local/fixes-20260924/backend'
REMOTE='/opt/codex-agent-builds/fixes-20260924-linux-checks'
CONTAINER='codex-agent-fixes-20260924-linux-checks'

def main():
 global CONTAINER, OUT
 parser=argparse.ArgumentParser(description=__doc__);parser.add_argument('action',choices=['start','status','collect']);parser.add_argument('--run-id',default='1');parser.add_argument('--tests',default='');args=parser.parse_args()
 if args.run_id!='1':
  if not args.run_id.isdigit():raise ValueError('Numeric run-id required')
  CONTAINER+='-'+args.run_id;OUT=OUT/('run-'+args.run_id)
 OUT.mkdir(parents=True,exist_ok=True)
 config=json.loads((ROOT/'.local/integration/environment.json').read_text())
 assert config['purpose']=='codex-agent-integration' and config['synthetic_data']
 native=json.loads((ROOT/'.local/fixes-20260924/native-build/run.json').read_text())
 values={key:str(value) for key,value in config.items() if key.isupper() and isinstance(value,(str,int,float))}
 values.update(CGO_ENABLED='1',GOMAXPROCS='4',GOFLAGS='-mod=readonly',GOCACHE='/cache/build',GOMODCACHE='/cache/modules',CODEX_NATIVE_HTTP_TEST_BIN='/native/codex-native-transport',TEST_REDIS_PREFIX='codex-agent:integration:')
 secrets=[value for key,value in values.items() if any(s in key for s in ['URL','KEY','PASSWORD','TOKEN'])]
 client=connect()
 try:
  if args.action=='start':
   run(client,'install -d -m 700 '+REMOTE+'/source '+REMOTE+'/cache')
   with client.open_sftp() as sftp:
    sftp.put(str(BACKEND/'source.tar.gz'),REMOTE+'/source.tar.gz')
    with sftp.file(REMOTE+'/test.env','w') as f:f.write('\n'.join(k+'='+v for k,v in values.items())+'\n')
    sftp.chmod(REMOTE+'/test.env',0o600)
   run(client,'tar -xzf '+REMOTE+'/source.tar.gz -C '+REMOTE+'/source')
   command_args=['go','test','-race','-count=1','-timeout=12m','-json']
   if args.tests:command_args+=['-run',args.tests]
   command_args+=['./...']
   script='set -eu\ncd /source\ngo version\n'+shlex.join(command_args)
   command=['docker','run','-d','--name',CONTAINER,'--label','codex-agent.task=fixes-20260924','--cpus','4','--memory','4g','--pids-limit','1024','--network','host','--env-file',REMOTE+'/test.env','-v',REMOTE+'/source:/source:ro','-v',REMOTE+'/cache:/cache','-v',native['remote']+'/work/target/release:/native:ro','codex-agent-integration-race:ce864e7223ac17b1-v1','sh','-c',script]
   print(run(client,shlex.join(command),timeout=90).strip())
   (OUT/'source-manifest.json').write_bytes((BACKEND/'source-manifest.json').read_bytes())
  else:
   state=json.loads(run(client,shlex.join(['docker','inspect',CONTAINER,'--format','{{json .State}}'])))
   if args.action=='status':
    print(json.dumps({'running':state['Running'],'exit_code':state['ExitCode']}))
    print(scrub(run(client,shlex.join(['docker','logs','--tail','8',CONTAINER])+' 2>&1'),secrets))
   else:
    if state['Running']:raise RuntimeError('Checks are still running')
    log=scrub(run(client,shlex.join(['docker','logs',CONTAINER])+' 2>&1',timeout=60),secrets)
    (OUT/'race.jsonl').write_text(log,encoding='utf-8')
    events=[]
    for line in log.splitlines():
     try:events.append(json.loads(line))
     except ValueError:pass
    report={'exit_code':state['ExitCode'],'oom_killed':state.get('OOMKilled'),'passed':[e['Test'] for e in events if e.get('Action')=='pass' and e.get('Test')],'failed':[e.get('Test',e.get('Package')) for e in events if e.get('Action')=='fail'],'skipped':[e['Test'] for e in events if e.get('Action')=='skip' and e.get('Test')]}
    (OUT/'report.json').write_text(json.dumps(report,indent=2),encoding='utf-8')
    print(json.dumps({'exit_code':report['exit_code'],'passed':len(report['passed']),'failed':report['failed'],'skipped':report['skipped']}))
 finally:client.close()

if __name__=='__main__':main()
