"""Read and freeze the actual Codex Agent frontend without changing the server."""
from datetime import datetime, timezone
import hashlib
import json
from pathlib import Path, PurePosixPath
import shlex

from integration_env import ROOT, connect, run


REMOTE = r'''
import hashlib,json,pathlib,re,subprocess,urllib.request,urllib.parse
root=pathlib.Path('/opt/codex-agent')
unit=pathlib.Path('/etc/systemd/system/codex-agent.service').read_bytes()
starts=[x for x in unit.decode().splitlines() if x.startswith('ExecStart=')]
assert len(starts)==1
m=re.fullmatch(r'ExecStart=(/opt/codex-agent/releases/[a-f0-9]{64}/codex-agent) --listen 0\.0\.0\.0:18879 --web (/opt/codex-agent/(?:releases|ui-releases)/[a-f0-9]{64}/frontend/dist)',starts[0])
assert m, 'Unexpected project paths'
binary,web=map(pathlib.Path,m.groups())
assert binary.resolve()==binary and web.resolve()==web
props=dict(x.split('=',1) for x in subprocess.check_output(['systemctl','show','codex-agent.service','-p','MainPID','-p','ActiveState','-p','DropInPaths']).decode().splitlines())
assert props['ActiveState']=='active' and not props['DropInPaths']
assert pathlib.Path('/proc/'+props['MainPID']+'/exe').resolve()==binary
opener=urllib.request.build_opener(urllib.request.ProxyHandler({}))
files={}
http={}
sizes={}
for p in sorted(web.rglob('*')):
    assert not p.is_symlink()
    if not p.is_file(): continue
    assert p.resolve()==p
    name=p.relative_to(web).as_posix()
    sizes[name]=p.stat().st_size
    assert sizes[name]<32*1024*1024
    files[name]=hashlib.sha256(p.read_bytes()).hexdigest()
    route='' if name=='index.html' else urllib.parse.quote(name)
    with opener.open('http://127.0.0.1:18879/'+route,timeout=20) as response:
        assert response.status==200
        http[name]=hashlib.sha256(response.read(32*1024*1024)).hexdigest()
assert 'index.html' in files and files==http
assert sum(sizes.values())<128*1024*1024
health={}
for name in ('live','ready'):
    with opener.open('http://127.0.0.1:18879/health/'+name,timeout=10) as response: health[name]=response.status
print(json.dumps({'binary':str(binary),'binary_sha256':hashlib.sha256(binary.read_bytes()).hexdigest(),'web':str(web),'unit_sha256':hashlib.sha256(unit).hexdigest(),'pid':int(props['MainPID']),'frontend':files,'sizes':sizes,'http_hashes_match':files==http,'health':health},sort_keys=True))
'''


def main():
    client = connect()
    try:
        before = json.loads(run(client, 'python3 -c ' + shlex.quote(REMOTE), timeout=150))
        identity = hashlib.sha256(json.dumps(before['frontend'], sort_keys=True).encode()).hexdigest()
        folder = ROOT / '.local/runtime-frontend' / identity
        folder.mkdir(parents=True, exist_ok=True)
        with client.open_sftp() as sftp:
            for name, expected in before['frontend'].items():
                relative = PurePosixPath(name)
                if relative.is_absolute() or '..' in relative.parts or '\\' in name:
                    raise RuntimeError('Unexpected frontend path')
                dest = folder / 'frontend/dist' / name
                dest.parent.mkdir(parents=True, exist_ok=True)
                if dest.exists():
                    if hashlib.sha256(dest.read_bytes()).hexdigest() != expected:
                        raise RuntimeError('Existing immutable frontend differs')
                    continue
                with sftp.file(before['web'] + '/' + name, 'rb') as handle:
                    data = handle.read(before['sizes'][name] + 1)
                if len(data) != before['sizes'][name] or hashlib.sha256(data).hexdigest() != expected:
                    raise RuntimeError('Runtime frontend changed during capture')
                dest.write_bytes(data)
        after = json.loads(run(client, 'python3 -c ' + shlex.quote(REMOTE), timeout=150))
        if before != after:
            raise RuntimeError('Service or static resources changed during capture')
        report = {'schema': 'codex-agent-runtime-frontend-v1', 'captured_at': datetime.now(timezone.utc).isoformat(),
                  'frontend_sha256': identity, 'frontend_directory': str(folder / 'frontend/dist'),
                  'source': before, 'stable': True, 'remote_changes': False}
        report_path = folder / ('capture-' + datetime.now(timezone.utc).strftime('%Y%m%dT%H%M%SZ') + '.json')
        report_path.write_text(json.dumps(report, indent=2), encoding='utf-8')
        evidence = {'schema': 'codex-agent-production-frontend-v1', 'status': 'pass',
                    'captured_at': report['captured_at'], 'binary_sha256': before['binary_sha256'],
                    'frontend': before['frontend'], 'unit_sha256': before['unit_sha256']}
        (folder / 'frontend-evidence.json').write_text(json.dumps(evidence, indent=2), encoding='utf-8')
        print(json.dumps({'files': len(before['frontend']), 'bytes': sum(before['sizes'].values()),
                          'frontend_sha256': identity, 'http_hashes_match': True, 'health': before['health']}))
        print(report_path)
    finally:
        client.close()


if __name__ == '__main__':
    main()
