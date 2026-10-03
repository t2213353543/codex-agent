"""Deploy/run the private 28765 OAuth comparison without restarting its workers.

Only reviewed Python sources are overlaid in a separate diagnostic process.
Account credentials and captured authentication logs never leave the server.
"""
from __future__ import annotations

import argparse
import hashlib
import json
from pathlib import Path
import shlex
import sys

from integration_env import connect

ROOT = Path(__file__).resolve().parents[1]
PROJECT = ROOT.parent / "loophub-mother-checkout-20260920"
NAME = "oauth-profile-experiment-20260928"
APP_ROOT = "/app/data/" + NAME
FILES = ("oauth_profiles.py", "auth_flow.py", "webui/oauth_repair.py", "webui/db.py",
         "scripts/oauth_profile_experiment.py")


def remote(client, source, timeout=180):
    si, so, se = client.exec_command("python3 -", timeout=timeout)
    si.write(source)
    si.flush()
    si.channel.shutdown_write()
    result, errors = so.read(), se.read()
    if so.channel.recv_exit_status():
        raise RuntimeError("Server operation failed; diagnostic detail retained server-side")
    return result.decode("utf-8")


def locations(client):
    return json.loads(remote(client, '''
import json,pathlib,subprocess
c=json.loads(subprocess.check_output(['docker','inspect','loophub-multitenant-app']))[0]
mounts=[m['Source'] for m in c['Mounts'] if m['Destination']=='/app/data']
assert len(mounts)==1
root=pathlib.Path(mounts[0]).resolve()
assert root.is_relative_to(pathlib.Path('/root/loophub-28765').resolve())
print(json.dumps({'data':str(root),'image':c['Config']['Image']}))
'''))


def stage(client, probe_only=False):
    loc = locations(client)
    root = loc["data"] + "/" + NAME
    expected = json.loads((ROOT / ".local/latest-operations/project28765/mother-child-inventory.json").read_text())["source_hashes"]
    baseline = {name: expected[name]["runtime_sha256"] for name in FILES if name in expected}
    remote(client, "import json,subprocess\nb=" + repr(baseline) + "\n" + '''
p="import hashlib,json,pathlib; b="+repr(b)+"; assert all(hashlib.sha256((pathlib.Path('/app')/n).read_bytes()).hexdigest()==h for n,h in b.items())"
subprocess.run(['docker','exec','loophub-multitenant-app','python','-c',p],check=True,capture_output=True)
''')
    blobs = {name: (PROJECT / name).read_bytes() for name in FILES}
    launcher = '''import logging,runpy,sys
from pathlib import Path
logging.disable(logging.CRITICAL)
code=Path(__file__).resolve().parent
sys.path.insert(0,'/app')
sys.path.insert(0,str(code))
import webui
webui.__path__.insert(0,str(code/'webui'))
runpy.run_path(str(code/'scripts/oauth_profile_experiment.py'),run_name='__main__')
'''
    blobs["launch.py"] = launcher.encode()
    if (PROJECT / "scripts/oauth_profile_generation_probe.py").exists():
        blobs["scripts/oauth_profile_generation_probe.py"] = (PROJECT / "scripts/oauth_profile_generation_probe.py").read_bytes()
        blobs["launch_probe.py"] = launcher.replace("runpy.run_path(str(code/'scripts/oauth_profile_experiment.py'),run_name='__main__')", "sys.path.insert(0,str(code/'scripts'))\nrunpy.run_path(str(code/'scripts/oauth_profile_generation_probe.py'),run_name='__main__')").encode()
        blobs["candy-prompt.txt"] = (ROOT / ".local/alternative-channels/20260928-web-research/candy-prompt.txt").read_bytes()
    if probe_only:
        blobs = {k: v for k, v in blobs.items() if k in ("scripts/oauth_profile_generation_probe.py", "launch_probe.py", "candy-prompt.txt")}
    dirs = [root, root + "/code", root + "/code/webui", root + "/code/scripts"]
    remote(client, "import os\npaths=" + repr(dirs) + "\nfor p in paths:\n os.makedirs(p,mode=0o700,exist_ok=True)\n os.chmod(p,0o700)\n")
    hashes = {}
    with client.open_sftp() as sftp:
        for name, body in blobs.items():
            destination = root + "/code/" + name
            temporary = destination + ".upload"
            with sftp.file(temporary, "wb") as stream:
                stream.write(body)
            sftp.chmod(temporary, 0o600)
            sftp.posix_rename(temporary, destination)
            hashes[name] = hashlib.sha256(body).hexdigest()
    program = "import hashlib,json,pathlib; b=" + repr(hashes) + "; p=pathlib.Path(" + repr(APP_ROOT + "/code") + "); assert all(hashlib.sha256((p/n).read_bytes()).hexdigest()==h for n,h in b.items()); [compile((p/n).read_bytes(),n,'exec') for n in b if n.endswith('.py')]; print(json.dumps({'staged':True,'sha256':b,'application_restarted':False}))"
    return remote(client, "import subprocess\np=" + repr(program) + "\nr=subprocess.run(['docker','exec','loophub-multitenant-app','python','-c',p],check=True,capture_output=True); print(r.stdout.decode())\n")


def invoke(client, args, probe=False):
    arguments = ["docker", "exec", "loophub-multitenant-app", "python", APP_ROOT + "/code/" + ("launch_probe.py" if probe else "launch.py"), *args]
    # A private output file captures legacy authentication prints. Only the
    # experiment's explicitly framed JSON summary is returned to the operator.
    program = '''
import json,os,pathlib,subprocess,time
args=ARGS
folder=pathlib.Path(FOLDER)
folder.mkdir(parents=True,exist_ok=True,mode=0o700)
log=folder/('operator-'+str(time.time_ns())+'.log')
fd=os.open(log,os.O_CREAT|os.O_EXCL|os.O_WRONLY,0o600)
with os.fdopen(fd,'wb') as output:
 result=subprocess.run(args,stdout=output,stderr=subprocess.STDOUT,timeout=900)
text=log.read_text(errors='replace')
summaries=[]
for line in text.splitlines():
 try:
  row=json.loads(line)
  if isinstance(row,dict) and row.get('experiment')=='oauth-profile-comparison':summaries.append(row)
 except (ValueError,TypeError):pass
print(json.dumps({'exit_code':result.returncode,'summaries':summaries,'private_log_retained':True}))
'''.replace("ARGS", repr(arguments)).replace("FOLDER", repr(locations(client)["data"] + "/" + NAME))
    return remote(client, program, timeout=960)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("operation", choices=("stage", "stage-probe", "run", "probe"))
    parser.add_argument("arguments", nargs=argparse.REMAINDER)
    args = parser.parse_args()
    client = connect()
    try:
        output = stage(client, args.operation == "stage-probe") if args.operation in ("stage", "stage-probe") else invoke(client, args.arguments, args.operation == "probe")
        print(output, end="")
        destination = ROOT / ".local/latest-operations/project28765"
        destination.mkdir(parents=True, exist_ok=True)
        import time
        (destination / ("oauth-comparison-" + args.operation + "-" + str(time.time_ns()) + ".json")).write_text(output, encoding="utf-8")
    finally:
        client.close()


if __name__ == "__main__":
    main()
