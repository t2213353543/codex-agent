"""Read-only inventory of Codex Agent only; never exports credentials."""
import json
import shlex
from datetime import datetime, timezone
from pathlib import Path
from integration_env import connect, run

SCRIPT = r'''
import hashlib,json,os,pathlib,subprocess,urllib.request
root=pathlib.Path('/opt/codex-agent')
pid=subprocess.check_output(['systemctl','show','codex-agent.service','--value','-p','MainPID'],text=True).strip()
def sha(p):
    return hashlib.sha256(pathlib.Path(p).read_bytes()).hexdigest() if pathlib.Path(p).is_file() else None
def http(path):
    try:
        with urllib.request.urlopen('http://127.0.0.1:18879'+path,timeout=10) as r:
            body=r.read()
            return {'status':r.status,'bytes':len(body),'json':json.loads(body) if path=='/health' else None}
    except Exception as e:return {'error':type(e).__name__}
result={'service':'codex-agent.service','address':'http://43.225.196.170:18879','main_host':'43.225.196.170','ssh_port':12865,'service_state':subprocess.check_output(['systemctl','show','codex-agent.service','-p','ActiveState','-p','SubState','-p','ExecStart','-p','FragmentPath','-p','TimeoutStopUSec','-p','DynamicUser'],text=True),'runtime_binary_sha256':sha('/proc/'+pid+'/exe'),'disk_binary_sha256':sha(root/'codex-agent'),'unit_sha256':sha('/etc/systemd/system/codex-agent.service'),'frontend_index_sha256':sha(root/'frontend/dist/index.html'),'data_path':'/var/lib/codex-agent','data_files':[{'name':p.name,'bytes':p.stat().st_size} for p in pathlib.Path('/var/lib/codex-agent').iterdir() if p.is_file()],'health':http('/health'),'root':http('/'),'environment_variable_names':[],'release_archives':[p.name for p in (root/'releases').glob('*.tar.gz')],'frontend_assets':[{'path':str(p.relative_to(root/'frontend/dist')),'bytes':p.stat().st_size} for p in (root/'frontend/dist').rglob('*') if p.is_file()]}
if pid!='0':
    result['environment_variable_names']=sorted(x.split(b'=',1)[0].decode() for x in pathlib.Path('/proc/'+pid+'/environ').read_bytes().split(b'\0') if b'=' in x)
print(json.dumps(result))
'''


if __name__ == '__main__':
    client = connect()
    try:
        result = json.loads(run(client, 'python3 -c ' + shlex.quote(SCRIPT)))
        result['checked_at'] = datetime.now(timezone.utc).isoformat()
        target = Path(__file__).resolve().parents[1] / '.local/acceptance/production-baseline.json'
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_text(json.dumps(result, ensure_ascii=False, indent=2), encoding='utf-8')
        print(json.dumps(result, ensure_ascii=False, indent=2))
    finally:
        client.close()
