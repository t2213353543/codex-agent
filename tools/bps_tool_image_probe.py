"""Synthetic custom-tool image control/detail/attachment probes (max 3 generations).

Rerun: python tools/bps_tool_image_probe.py
Credentials stay on the server; no captured customer content is replayed.
"""
from datetime import datetime, timezone
import gzip
import hashlib
import json
import os
import shlex
import subprocess
from integration_env import ROOT, connect
from bps_minimal_policy_probe import REMOTE


def main():
    folder = ROOT / '.local/tool-image-probe' / datetime.now(timezone.utc).strftime('%Y%m%dT%H%M%S.%fZ')
    folder.mkdir(parents=True)
    source = (ROOT / 'tools/bps_tool_image_probe.go').read_bytes()
    (folder / 'probe-source.go').write_bytes(source)
    binary = folder / 'probe'
    subprocess.run(['go', 'build', '-trimpath', '-ldflags=-s -w', '-o', str(binary), 'tools/bps_tool_image_probe.go'], cwd=ROOT,
                   env={**os.environ, 'GOOS': 'linux', 'GOARCH': 'amd64', 'CGO_ENABLED': '0'}, check=True)
    digest = hashlib.sha256(binary.read_bytes()).hexdigest()
    archive = folder / 'probe.gz'
    archive.write_bytes(gzip.compress(binary.read_bytes(), compresslevel=1, mtime=0))
    remote = '/opt/codex-agent-integration/checks/bps-tool-image-' + digest[:16]
    client = connect()
    try:
        with client.open_sftp() as sftp:
            sftp.get_channel().settimeout(90)
            sftp.put(str(archive), remote + '.gz')
        install = 'import gzip,pathlib,hashlib;p=pathlib.Path(' + repr(remote) + ');v=gzip.decompress(pathlib.Path(str(p)+\'.gz\').read_bytes());assert hashlib.sha256(v).hexdigest()==' + repr(digest) + ';p.write_bytes(v);p.chmod(0o700)'
        _, out, err = client.exec_command('python3 -B -c ' + shlex.quote(install), timeout=60)
        if out.channel.recv_exit_status():
            raise RuntimeError('Artifact verification failed')
        script = 'BINARY=' + repr(remote) + '\n' + REMOTE
        (folder / 'remote.py').write_text(script, encoding='utf-8')
        stdin, out, err = client.exec_command('python3 -B -', timeout=650)
        stdin.write(script)
        stdin.channel.shutdown_write()
        with (folder / 'evidence.jsonl').open('w', encoding='utf-8') as log:
            for line in out:
                log.write(line)
                log.flush()
                print(line, end='', flush=True)
        code = out.channel.recv_exit_status()
        stderr = err.read()
        result = {'exit_code': code, 'stderr_bytes': len(stderr), 'binary_sha256': digest,
                  'source_sha256': hashlib.sha256(source).hexdigest(), 'output': str(folder), 'max_generation_requests': 3}
        (folder / 'result.json').write_text(json.dumps(result, indent=2), encoding='utf-8')
        print(json.dumps(result), flush=True)
        if code:
            raise SystemExit(code)
    finally:
        client.close()


if __name__ == '__main__':
    main()
