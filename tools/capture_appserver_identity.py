"""Capture the pinned App Server initialize identity without logging in.

Run during provisioning, not in a generation request. On Linux a fresh network
namespace is required; no user config, credentials or inherited proxy is used.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import platform
import queue
import shutil
import subprocess
import tempfile
import threading
import time


def capture(binary, output):
    binary, output = Path(binary).resolve(strict=True), Path(output).absolute()
    digest = hashlib.sha256(binary.read_bytes()).hexdigest()
    if output.exists():
        raise ValueError('identity_destination_already_exists')
    if platform.system() != 'Linux' or shutil.which('unshare') is None:
        raise ValueError('offline_capture_requires_linux_network_namespace')
    with tempfile.TemporaryDirectory(prefix='codex-appserver-identity-') as work:
        env = {'PATH': os.environ.get('PATH', '/usr/bin:/bin'), 'HOME': work,
               'CODEX_HOME': work, 'LANG': 'C.UTF-8', 'NO_COLOR':'1', 'RUST_LOG':'off,codex_models_manager=error'}
        # The subprocess receives no inherited credentials or provider config.
        process = subprocess.Popen(['unshare', '--net', '--', str(binary), 'app-server'],
            cwd=work, env=env, stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
        messages = queue.Queue()
        def read():
            for line in process.stdout:
                try: messages.put(json.loads(line))
                except ValueError: messages.put({'capture_error': 'invalid_initialize_json'})
            messages.put(None)
        threading.Thread(target=read, daemon=True).start()
        try:
            request = {'id': 1, 'method': 'initialize', 'params': {
                'clientInfo': {'name': 'codex_agent_gateway', 'version': '1.0'},
                'capabilities': {'experimentalApi': True}}}
            process.stdin.write(json.dumps(request).encode()+b'\n'); process.stdin.flush()
            deadline, result = time.monotonic()+30, None
            while time.monotonic() < deadline:
                message = messages.get(timeout=max(.1, deadline-time.monotonic()))
                if message is None or 'capture_error' in message:
                    raise RuntimeError('offline_initialize_failed')
                if message.get('id') == 1:
                    result = message.get('result'); break
            if not isinstance(result, dict) or not isinstance(result.get('userAgent'), str):
                raise RuntimeError('offline_initialize_has_no_identity')
            user_agent = result['userAgent']
            prefix = 'codex_agent_gateway/'
            if not user_agent.startswith(prefix):
                raise ValueError('unexpected_initialize_originator')
            version = user_agent[len(prefix):].split(' ', 1)[0]
            machine = platform.machine()
            architecture = {'x86_64': 'amd64', 'aarch64': 'arm64'}.get(machine)
            if architecture is None:
                raise ValueError('unsupported_identity_architecture')
            identity = {'schema': 'codex-runtime-identity-v1', 'cli_path': str(binary),
                'source_cli_sha256': digest, 'cli_version': version, 'originator': 'codex_agent_gateway',
                'user_agent': user_agent, 'platform_os': 'linux', 'platform_arch': architecture}
            output.parent.mkdir(parents=True, exist_ok=True)
            fd = os.open(output, os.O_WRONLY|os.O_CREAT|os.O_EXCL, 0o600)
            with os.fdopen(fd, 'w', encoding='utf-8') as handle:
                json.dump(identity, handle, indent=2); handle.write('\n')
            print(json.dumps({'identity_file': str(output), 'source_cli_sha256': digest,
                'cli_version': version, 'originator': identity['originator'],
                'user_agent_sha256': hashlib.sha256(user_agent.encode()).hexdigest(),
                'network_namespace': 'isolated', 'official_initialize_count': 1,
                'login_performed': False, 'model_generation_performed': False}), flush=True)
        finally:
            if process.poll() is None:
                process.terminate()
                try: process.wait(timeout=5)
                except subprocess.TimeoutExpired: process.kill(); process.wait(timeout=5)


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--binary', required=True)
    parser.add_argument('--output', required=True)
    options = parser.parse_args()
    capture(options.binary, options.output)
