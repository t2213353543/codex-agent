"""Freeze the September fixes, verify Go source, and build the Linux artifact."""

from project_layout import backend_source_paths
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import tarfile
import time

ROOT=Path(__file__).resolve().parents[1]
OUT=ROOT/'.local/fixes-20260924/backend'
def sha(path):return hashlib.sha256(path.read_bytes()).hexdigest()

def main():
 OUT.mkdir(parents=True,exist_ok=True);source=OUT/'source'
 if source.exists():raise RuntimeError('Frozen source already exists')
 source.mkdir()
 paths=backend_source_paths(ROOT)
 files={p.relative_to(ROOT).as_posix():sha(p) for p in paths}
 for path in paths:
  dst=source/path.relative_to(ROOT);dst.parent.mkdir(parents=True,exist_ok=True);shutil.copyfile(path,dst)
 manifest={'files':files,'sha256':hashlib.sha256(json.dumps(files,sort_keys=True).encode()).hexdigest()}
 (OUT/'source-manifest.json').write_text(json.dumps(manifest,indent=2),encoding='utf-8')
 with tarfile.open(OUT/'source.tar.gz','w:gz') as archive:
  for name in files:archive.add(source/name,arcname=name)
 checks={}
 for name,command,env in [('go-test',['go','test','-json','-count=1','./...'],os.environ.copy()),('go-vet',['go','vet','./...'],os.environ.copy()),('go-build',['go','build','-trimpath','-o',str(OUT/'codex-agent')],{**os.environ,'GOOS':'linux','GOARCH':'amd64','CGO_ENABLED':'0'})]:
  started=time.monotonic()
  with (OUT/(name+'.log')).open('w',encoding='utf-8') as log:
   result=subprocess.run(command,cwd=source,env=env,stdout=log,stderr=subprocess.STDOUT)
  checks[name]={'exit_code':result.returncode,'seconds':round(time.monotonic()-started,2)}
  print(json.dumps({name:checks[name]}),flush=True)
  if result.returncode:raise RuntimeError(name+' failed')
 for name,digest in files.items():
  if sha(ROOT/name)!=digest:raise RuntimeError('Shared source changed during verification: '+name)
 events=[]
 for line in (OUT/'go-test.log').read_text(encoding='utf-8').splitlines():
  try:events.append(json.loads(line))
  except ValueError:pass
 checks['tests']={'passed':sum(e.get('Action')=='pass' and bool(e.get('Test')) for e in events),'skipped':[e['Test'] for e in events if e.get('Action')=='skip' and e.get('Test')]}
 report={'passed':True,'source_sha256':manifest['sha256'],'binary_sha256':sha(OUT/'codex-agent'),'checks':checks}
 (OUT/'report.json').write_text(json.dumps(report,indent=2),encoding='utf-8')
 print(json.dumps({k:v for k,v in report.items() if k!='checks'}),flush=True)

if __name__=='__main__':main()
