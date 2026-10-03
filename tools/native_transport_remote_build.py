"""Build a frozen native helper in an isolated container on the project host."""
import argparse
from datetime import datetime, timezone
import json
from pathlib import Path
import shlex
import sys

from integration_env import ROOT, connect, run
import native_transport_local as local

OUT=ROOT/'.local/fixes-20260924/native-build'

def main():
 parser=argparse.ArgumentParser(description=__doc__);parser.add_argument('action',choices=['start','status','collect']);args=parser.parse_args()
 OUT.mkdir(parents=True,exist_ok=True)
 path=OUT/'run.json'
 client=connect()
 try:
  if args.action=='start':
   if path.exists():raise RuntimeError('Inspect the existing build before starting another')
   runid=datetime.now(timezone.utc).strftime('%Y%m%dT%H%M%SZ')
   meta={'container':'codex-agent-native-fix-'+runid.lower(),'remote':'/opt/codex-agent-builds/native-fix-'+runid,'profile':'release'}
   local.freeze(OUT)
   (OUT/'build_driver.py').write_text(local.BUILD_DRIVER,encoding='utf-8')
   (OUT/'entrypoint.sh').write_text(local.ENTRYPOINT,encoding='utf-8',newline='\n')
   run(client,'install -d -m 700 '+meta['remote']+'/snapshot '+meta['remote']+'/work')
   with client.open_sftp() as sftp:
    for name in ['source.tar.gz','source-manifest.json','build_driver.py','entrypoint.sh']:
     sftp.put(str(OUT/name),meta['remote']+'/snapshot/'+name)
   command=['docker','run','-d','--name',meta['container'],'--label','codex-agent.task=fixes-20260924','--cpus','4','--memory','4g','--pids-limit','512','-e','CODEX_NATIVE_PROFILE=release','-v',meta['remote']+'/snapshot:/snapshot:ro','-v',meta['remote']+'/work:/work',local.IMAGE,'sh','/snapshot/entrypoint.sh']
   path.write_text(json.dumps(meta,indent=2),encoding='utf-8')
   print(run(client,shlex.join(command),timeout=90).strip())
  else:
   meta=json.loads(path.read_text())
   status=json.loads(run(client,shlex.join(['docker','inspect',meta['container'],'--format','{{json .State}}'])))
   print(json.dumps({'running':status['Running'],'exit_code':status['ExitCode']}))
   print(run(client,shlex.join(['docker','logs','--tail','25',meta['container']])+' 2>&1',timeout=30))
   if args.action=='collect':
    if status['Running']:raise RuntimeError('Build is still running')
    with client.open_sftp() as sftp:
     for name in local.ARTIFACTS:
      try:sftp.get(meta['remote']+'/work/'+name,str(OUT/name))
      except FileNotFoundError:pass
     if status['ExitCode']==0:sftp.get(meta['remote']+'/work/target/release/codex-native-transport',str(OUT/'codex-native-transport'))
    if status['ExitCode']!=0:sys.exit(status['ExitCode'])
 finally:client.close()

if __name__=='__main__':main()
