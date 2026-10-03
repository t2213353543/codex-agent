#!/usr/bin/env python3
"""Run the official CLI against a synthetic loopback gateway in a private netns."""
from datetime import datetime, timezone
import gzip
import hashlib
import json
import os
import shlex
import subprocess
from integration_env import ROOT, connect

def main():
    output = ROOT / ".local/bps-ws-error-fixture" / datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")
    output.mkdir(parents=True)
    binary = output / "fixture.test"
    subprocess.run(["go", "test", "-c", "-o", str(binary), "./internal/server"], cwd=ROOT,
                   env={**os.environ, "GOOS":"linux", "GOARCH":"amd64", "CGO_ENABLED":"0"}, check=True)
    data = binary.read_bytes()
    digest = hashlib.sha256(data).hexdigest()
    archive = output / "fixture.test.gz"
    archive.write_bytes(gzip.compress(data, compresslevel=1, mtime=0))
    remote = "/opt/codex-agent-integration/checks/synthetic-ws-error-" + digest[:16] + ".test"
    client = connect()
    try:
        with client.open_sftp() as sftp:
            sftp.put(str(archive), remote + ".gz")
        install = "import gzip,pathlib,hashlib;p=pathlib.Path("+repr(remote)+");b=gzip.decompress(pathlib.Path(str(p)+'.gz').read_bytes());assert hashlib.sha256(b).hexdigest()=="+repr(digest)+";p.write_bytes(b);p.chmod(0o700)"
        _, out, err = client.exec_command("python3 -B -c " + shlex.quote(install), timeout=40)
        if out.channel.recv_exit_status():
            raise RuntimeError("synthetic_fixture_install_failed")
        script = "import os,subprocess;subprocess.run(['ip','link','set','lo','up'],check=True);env={'PATH':'/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin','LANG':'C.UTF-8','BPS_SYNTHETIC_CODEX_BINARY':'/opt/codex-agent-integration/checks/quality-20260924/official-bundle/bin/codex'};os.execve("+repr(remote)+",["+repr(remote)+",'-test.run','^TestBPSGatewayOfficialCLIStatuslessWSErrorTerminates$','-test.v','-test.timeout=60s'],env)"
        command = "unshare --net python3 -B -c " + shlex.quote(script)
        _, out, err = client.exec_command(command, timeout=80)
        stdout, stderr = out.read(), err.read()
        status = out.channel.recv_exit_status()
        (output / "fixture.log").write_bytes(stdout)
        result = {"exit_code":status,"stderr_bytes":len(stderr),"binary_sha256":digest,
                  "inference_calls":0,"credential_access":False,"network":"loopback-only private netns","output":str(output)}
        (output / "result.json").write_text(json.dumps(result,indent=2),encoding="utf-8")
        print(stdout.decode("utf-8","replace"))
        print(json.dumps(result))
        raise SystemExit(status)
    finally:
        client.close()

if __name__ == "__main__":
    main()
