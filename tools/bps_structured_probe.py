"""Run at most six synthetic BPS requests; credentials never leave the server.

Rerun: python tools/bps_structured_probe.py
"""
from datetime import datetime, timezone
import argparse
import gzip
import hashlib
import json
import os
import shlex
import subprocess

from integration_env import ROOT, connect
from bps_minimal_policy_probe import REMOTE


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--tool-schema', action='store_true', help='Run the tool-schema positive and negative control (at most two requests).')
    parser.add_argument('--tool-schema-once', action='store_true', help='Run only the corrected positive tool case (one request).')
    parser.add_argument('--tool-schema-invalid', action='store_true', help='Run only the negative tool schema case (one request).')
    args = parser.parse_args()
    output = ROOT / '.local/structured-probe' / datetime.now(timezone.utc).strftime('%Y%m%dT%H%M%S.%fZ')
    output.mkdir(parents=True)
    source = (ROOT / 'tools/bps_structured_probe.go').read_bytes()
    (output / 'probe-source.go').write_bytes(source)
    binary = output / 'probe'
    subprocess.run(['go', 'build', '-trimpath', '-ldflags=-s -w', '-o', str(binary), 'tools/bps_structured_probe.go'], cwd=ROOT,
                   env={**os.environ, 'GOOS': 'linux', 'GOARCH': 'amd64', 'CGO_ENABLED': '0'}, check=True)
    digest = hashlib.sha256(binary.read_bytes()).hexdigest()
    archive = output / 'probe.gz'
    archive.write_bytes(gzip.compress(binary.read_bytes(), compresslevel=1, mtime=0))
    remote = '/opt/codex-agent-integration/checks/bps-structured-probe-' + digest[:16]
    client = connect()
    try:
        with client.open_sftp() as sftp:
            sftp.get_channel().settimeout(90)
            sftp.put(str(archive), remote + '.gz')
        install = 'import gzip,pathlib,hashlib;p=pathlib.Path(' + repr(remote) + ');v=gzip.decompress(pathlib.Path(str(p)+\'.gz\').read_bytes());assert hashlib.sha256(v).hexdigest()==' + repr(digest) + ';p.write_bytes(v);p.chmod(0o700)'
        _, out, err = client.exec_command('python3 -B -c ' + shlex.quote(install), timeout=60)
        if out.channel.recv_exit_status():
            raise RuntimeError('Artifact verification failed')
        script = 'BINARY=' + repr(remote) + '\n' + REMOTE.replace('timeout=600', 'timeout=840')
        if args.tool_schema or args.tool_schema_once or args.tool_schema_invalid:
            script = script.replace("{'accounts':accounts,'tools_version':version}", "{'accounts':accounts,'tools_version':version,'tool_schema':True,'tool_schema_once':" + repr(args.tool_schema_once) + ",'tool_schema_invalid':" + repr(args.tool_schema_invalid) + "}")
        (output / 'remote.py').write_text(script, encoding='utf-8')
        stdin, out, err = client.exec_command('python3 -B -', timeout=900)
        stdin.write(script)
        stdin.channel.shutdown_write()
        with (output / 'evidence.jsonl').open('w', encoding='utf-8') as log:
            for line in out:
                log.write(line)
                log.flush()
                print(line, end='', flush=True)
        code = out.channel.recv_exit_status()
        error = err.read()
        result = {'diagnostic_exit': code, 'stderr_bytes': len(error), 'binary_sha256': digest,
                  'source_sha256': hashlib.sha256(source).hexdigest(), 'output': str(output), 'mode': 'tool_schema_invalid' if args.tool_schema_invalid else 'tool_schema_once' if args.tool_schema_once else 'tool_schema' if args.tool_schema else 'text_schema'}
        (output / 'result.json').write_text(json.dumps(result, indent=2), encoding='utf-8')
        print(json.dumps(result), flush=True)
        if code:
            raise SystemExit(code)
    finally:
        client.close()


if __name__ == '__main__':
    main()
