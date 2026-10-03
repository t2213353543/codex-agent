"""Prepare, stage and activate an evidence-bound PowerPoint gateway release."""
from __future__ import annotations
import argparse
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import shlex
import subprocess
import tarfile

from acceptance import source_manifest
from integration_env import ROOT, connect

def sha(path):return hashlib.sha256(Path(path).read_bytes()).hexdigest()
def json_file(path):return json.loads(Path(path).read_text(encoding='utf-8-sig'))

def prepare(args):
    evidence=json_file(args.evidence)
    required={'go','vet','frontend','live_cli','live_ws','race','redis','schema_unchanged'}
    if evidence.get('passed') is not True or any(evidence.get('checks',{}).get(k) is not True for k in required):
        raise RuntimeError('Complete passing migration evidence is required')
    before=source_manifest()
    if evidence.get('source_sha256')!=before['sha256']:
        raise RuntimeError('Migration evidence does not match current sources')
    for record in evidence.get('evidence_files',[]):
        path=(ROOT/record['path']).resolve()
        if not path.is_relative_to(ROOT) or sha(path)!=record['sha256']:
            raise RuntimeError('Referenced verification evidence changed')
    out=ROOT/'.local/powerpoint-release'
    out.mkdir(parents=True,exist_ok=True)
    binary=out/'codex-agent-linux'
    subprocess.run(['go','build','-trimpath','-ldflags=-s -w','-o',str(binary),'.'],cwd=ROOT,
        env={**os.environ,'GOOS':'linux','GOARCH':'amd64','CGO_ENABLED':'0'},check=True)
    if source_manifest()!=before:raise RuntimeError('Sources changed during build')
    frontend=ROOT/'frontend/dist'
    if not (frontend/'index.html').is_file():raise RuntimeError('Frontend build missing')
    evidence['binary_sha256']=sha(binary)
    files={'codex-agent':sha(binary),'deploy/powerpoint_release.py':sha(ROOT/'deploy/powerpoint_release.py')}
    files.update({'frontend/dist/'+p.relative_to(frontend).as_posix():sha(p) for p in frontend.rglob('*') if p.is_file()})
    acceptance_raw=(json.dumps(evidence,ensure_ascii=False,indent=2)+'\n').encode()
    source_raw=(json.dumps(before,indent=2)+'\n').encode()
    files['acceptance.json']=hashlib.sha256(acceptance_raw).hexdigest()
    files['source-manifest.json']=hashlib.sha256(source_raw).hexdigest()
    identity={'backend':'powerpoint_bps','source_sha256':before['sha256'],'files':files,
        'migration_checksums':{'migrations/'+p.name:sha(p) for p in (ROOT/'migrations').glob('*.sql')}}
    rid=hashlib.sha256(json.dumps(identity,sort_keys=True).encode()).hexdigest()
    release=out/rid;release.mkdir(exist_ok=False)
    package=release/'package';package.mkdir()
    (package/'acceptance.json').write_bytes(acceptance_raw)
    (package/'source-manifest.json').write_bytes(source_raw)
    for name in files:
        if name in ['acceptance.json','source-manifest.json']:continue
        target=package/name;target.parent.mkdir(parents=True,exist_ok=True)
        original=binary if name=='codex-agent' else ROOT/name
        target.write_bytes(original.read_bytes())
    manifest={**identity,'release_id':rid,'built_at':datetime.now(timezone.utc).isoformat()}
    (package/'manifest.json').write_text(json.dumps(manifest,indent=2),encoding='utf-8')
    def mode(info):
        info.uid=info.gid=0;info.uname=info.gname='';info.mode=0o755 if info.isdir() or info.name=='codex-agent' else 0o644;return info
    with tarfile.open(release/'release.tar.gz','w:gz',compresslevel=1) as archive:
        for p in sorted(package.rglob('*')):
            if p.is_file():archive.add(p,arcname=p.relative_to(package).as_posix(),filter=mode)
    print(str(package/'manifest.json'))

def remote(args):
    manifest_path=Path(args.release).resolve();manifest=json_file(manifest_path)
    rid=manifest['release_id']
    if len(rid)!=64 or any(c not in '0123456789abcdef' for c in rid):raise RuntimeError('Invalid release identity')
    destination='/opt/codex-agent/releases/'+rid
    client=connect()
    try:
        if args.action=='stage':
            archive=manifest_path.parent.parent/'release.tar.gz'
            staging=destination+'.tar.gz'
            with client.open_sftp() as sftp:
                sftp.get_channel().settimeout(60);sftp.put(str(archive),staging,confirm=True)
            script="""import pathlib,hashlib,tarfile
archive=pathlib.Path(ARCHIVE);root=pathlib.Path(DESTINATION)
assert hashlib.sha256(archive.read_bytes()).hexdigest()==DIGEST
if root.exists(): raise RuntimeError('Release path already exists; inspect existing release')
root.mkdir(mode=0o755)
with tarfile.open(archive,'r:gz') as source:
 for member in source.getmembers():
  path=root/member.name
  assert not member.issym() and not member.islnk() and path.resolve().is_relative_to(root)
 source.extractall(root,filter='data')
print('Release staged')
"""
            script=script.replace('ARCHIVE',repr(staging)).replace('DESTINATION',repr(destination)).replace('DIGEST',repr(sha(archive)))
            stdin,stdout,stderr=client.exec_command('python3 -',timeout=90);stdin.write(script);stdin.channel.shutdown_write()
        else:
            stdin,stdout,stderr=client.exec_command('python3 '+shlex.quote(destination+'/deploy/powerpoint_release.py')+' '+args.action+' --release '+shlex.quote(destination),timeout=600)
        raw=stdout.read().decode('utf-8','replace');status=stdout.channel.recv_exit_status();error=stderr.read()
        output=manifest_path.parent.parent/(args.action+'.json')
        output.write_text(raw,encoding='utf-8')
        print(raw)
        if status:raise RuntimeError('Release action failed; remote stderr bytes='+str(len(error)))
    finally:client.close()

def main():
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument('action',choices=['prepare','stage','preflight','activate','rollback','status'])
    p.add_argument('--evidence');p.add_argument('--release')
    args=p.parse_args()
    if args.action=='prepare':
        if not args.evidence:p.error('--evidence is required')
        prepare(args)
    else:
        if not args.release:p.error('--release is required')
        remote(args)

if __name__=='__main__':main()
