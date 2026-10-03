"""Inspect, seal and deploy an explicitly verified full release; no model probes."""
import argparse
import hashlib
import json
import os
import re
from pathlib import Path
import shlex
import tarfile

from integration_env import ROOT, connect, run
from deploy import put

OUT = ROOT / '.local/full-release-20260919'
REMOTE_ROOT = '/opt/codex-agent'


def sha(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def load(path):
    return json.loads(Path(path).read_text(encoding='utf-8-sig'))


def inspect():
    script = r'''
import hashlib,json,os,pathlib,subprocess,urllib.request
def cmd(args): return subprocess.check_output(args,text=True,timeout=30).strip()
def sha(path): return hashlib.sha256(pathlib.Path(path).read_bytes()).hexdigest()
pid=cmd(['systemctl','show','codex-agent.service','-p','MainPID','--value'])
args=pathlib.Path('/proc/'+pid+'/cmdline').read_bytes().decode().strip('\0').split('\0')
web=pathlib.Path(args[args.index('--web')+1]); binary=pathlib.Path(args[0])
assert str(binary).startswith('/opt/codex-agent/releases/') and str(web).startswith('/opt/codex-agent/')
env=dict(line.split('=',1) for line in pathlib.Path('/etc/codex-agent/production.env').read_text().splitlines() if line and not line.startswith('#'))
checks={}
for path in ['/','/health/live','/health/ready']:
 with urllib.request.urlopen('http://127.0.0.1:18879'+path,timeout=10) as res: checks[path]=res.status
print(json.dumps({'binary_path':str(binary),'binary_sha256':sha(binary),'runtime_sha256':sha('/proc/'+pid+'/exe'),
 'unit_sha256':sha('/etc/systemd/system/codex-agent.service'),'environment_sha256':sha('/etc/codex-agent/production.env'),
 'frontend':{p.relative_to(web).as_posix():sha(p) for p in web.rglob('*') if p.is_file()},'frontend_path':str(web),
 'pid':int(pid),'health':checks,'native_was_enabled':bool(env.get('CODEX_NATIVE_HTTP_HELPER')),
 'native_ws_was_enabled':env.get('CODEX_NATIVE_WEBSOCKET')=='1','system_ca_exists':pathlib.Path('/etc/pki/tls/certs/ca-bundle.crt').is_file(),
 'bridge_timer':cmd(['systemctl','show','codex-agent-workbench-bridge.timer','-p','ActiveState','--value']),
 'system':cmd(['uname','-sm'])}))
'''
    client = connect()
    try:
        value = json.loads(run(client, 'python3 -c ' + shlex.quote(script)))
    finally:
        client.close()
    OUT.mkdir(parents=True, exist_ok=True)
    path = OUT / 'production-before.json'
    with path.open('x', encoding='utf-8') as handle:
        json.dump(value, handle, indent=2)
    print(json.dumps({key: value[key] for key in ['pid','binary_sha256','health','native_was_enabled','system_ca_exists','bridge_timer']}))


def prepare(args):
    acceptance = load(args.acceptance)
    require = lambda value, text: None if value else (_ for _ in ()).throw(ValueError(text))
    require(acceptance['passed'] is True, 'Acceptance not passed')
    baseline = load(OUT / 'production-before.json')
    binary, frontend = Path(args.binary).resolve(), Path(args.frontend).resolve()
    source = load(args.source_manifest)
    require(sha(binary) == acceptance['binary_sha256'], 'Wrong binary')
    require(source['sha256'] == acceptance['source_sha256'], 'Wrong source')
    files = {p.relative_to(frontend).as_posix(): sha(p) for p in frontend.rglob('*') if p.is_file()}
    require(files == acceptance['frontend'], 'Wrong frontend')
    native_report = load(getattr(args, 'native_report', None) or ROOT / 'docs/NATIVE-REQUEST-COMPRESSION-RELEASE-2026-09-18.json')
    package = ROOT / native_report['package_path']
    require(sha(package) == native_report['package_sha256'], 'Native archive changed')
    staging = OUT / 'package'
    staging.mkdir(exist_ok=False)
    def copy(name, original, executable=False):
        target=staging/name;target.parent.mkdir(parents=True,exist_ok=True)
        target.write_bytes(original.read_bytes())
        if executable: target.chmod(0o755)
    copy('codex-agent',binary,True)
    for name in files: copy('frontend/dist/'+name,frontend/name)
    for name in ['upgrade_remote.py','full_release_remote.py']: copy('deploy/'+name,ROOT/'deploy'/name)
    copy('evidence/acceptance.json',Path(args.acceptance))
    copy('evidence/source-manifest.json',Path(args.source_manifest))
    migrations={}
    for path in (ROOT/'migrations').glob('*.sql'):
        copy('migrations/'+path.name,path);migrations['migrations/'+path.name]=sha(path)
    with tarfile.open(package,'r:gz') as archive:
        matches=[]
        for member in archive:
            if member.isfile() and member.size == native_report['binary_bytes']:
                raw=archive.extractfile(member).read()
                if hashlib.sha256(raw).hexdigest()==native_report['binary_sha256']: matches.append(raw)
        require(len(matches)==1,'Native executable ambiguous')
        target=staging/'native/codex-native-transport';target.parent.mkdir();target.write_bytes(matches[0]);target.chmod(0o755)
    manifest={'schema':'codex-agent-full-release-v1','source_sha256':source['sha256'],
              'binary_sha256':sha(binary),'native_sha256':native_report['binary_sha256'],'native_enabled':True,
              'frontend':files,'baseline':baseline,'migration_checksums':migrations,
              'files':{p.relative_to(staging).as_posix():sha(p) for p in staging.rglob('*') if p.is_file()}}
    rid=hashlib.sha256(json.dumps(manifest,sort_keys=True).encode()).hexdigest();manifest['release_id']=rid
    (staging/'full-release.json').write_text(json.dumps(manifest,indent=2)+'\n',encoding='utf-8')
    archive_path=OUT/'release.tar.gz'
    def mode(info):
        info.uid=info.gid=0;info.uname=info.gname='';info.mode=0o755 if info.isdir() or info.name in ('codex-agent','native/codex-native-transport') else 0o644
        return info
    with tarfile.open(archive_path,'x:gz') as archive:
        for path in sorted(staging.iterdir()): archive.add(path,arcname=path.name,filter=mode)
    print(json.dumps({'release_id':rid,'archive_sha256':sha(archive_path),'frontend_files':len(files),'native':manifest['native_sha256']}))


def remote_action(action):
    manifest=load(OUT/'package/full-release.json');rid=manifest['release_id']
    if not isinstance(rid, str) or not re.fullmatch(r'[a-f0-9]{64}', rid):
        raise ValueError('Invalid release identity')
    destination=REMOTE_ROOT+'/releases/'+rid
    client=connect()
    try:
        if action=='stage':
            require_sha=sha(OUT/'release.tar.gz');staging=REMOTE_ROOT+'/releases/.full-stage-'+rid
            run(client,'set -eu\ntest ! -e '+destination+'\ntest ! -e '+staging+'\ninstall -d -m 755 '+staging)
            with client.open_sftp() as sftp:
                sftp.put(str(OUT/'release.tar.gz'),staging+'/release.tar.gz',confirm=True)
            require=run(client,'sha256sum '+staging+'/release.tar.gz').split()[0]
            if require!=require_sha: raise RuntimeError('Transferred archive mismatch')
            run(client,'set -eu\ntest ! -e '+destination+'\ntar -xzf '+staging+'/release.tar.gz -C '+staging+'\nmv -T '+staging+' '+destination,timeout=60)
            print(json.dumps({'staged':True,'destination':destination}))
        else:
            result=run(client,'python3 '+destination+'/deploy/full_release_remote.py '+action+' --release '+destination,timeout=1200)
            data=json.loads(result)
            (OUT/(action+'.json')).write_text(json.dumps(data,indent=2)+'\n',encoding='utf-8')
            print(json.dumps({'action':action,'phase':data.get('phase'),'deployed':data.get('deployed'),'verification':data.get('verification',data if action=='verify' else None)}))
    finally: client.close()


def main():
    global OUT
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action',choices=['inspect','prepare','stage','preflight','activate','verify'])
    for name in ['acceptance','binary','frontend','source-manifest']: parser.add_argument('--'+name)
    parser.add_argument('--output-dir', type=Path)
    parser.add_argument('--native-report', type=Path)
    args=parser.parse_args()
    if args.output_dir:
        OUT = args.output_dir.resolve()
    if args.action=='inspect':inspect()
    elif args.action=='prepare':prepare(args)
    else:remote_action(args.action)


if __name__=='__main__':main()
