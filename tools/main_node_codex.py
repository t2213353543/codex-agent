"""Install the official CLI and sync the current user's configuration to main."""
import argparse
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import shlex
import sqlite3
import time
import tomllib
from urllib.parse import urlsplit

from pip._vendor import tomli_w

ROOT = Path(__file__).resolve().parents[1]
HOME = Path(os.environ.get('CODEX_HOME', 'D:/ChatGPT/CodexHome'))
REMOTE_HOME = '/root/.codex'
PROVIDER_ID = '652a8bcf-396f-4587-8134-e3668f94dc3a'


def connect():
    path = HOME / 'skills/sub2api-production/scripts/sub2api_ssh.py'
    spec = importlib.util.spec_from_file_location('main_node_ssh', path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module.connect('master')


def run(client, command, timeout=120):
    _, stdout, stderr = client.exec_command(command, timeout=timeout)
    output = stdout.read().decode('utf-8', 'replace')
    error = stderr.read().decode('utf-8', 'replace')
    code = stdout.channel.recv_exit_status()
    if code:
        raise RuntimeError(f'Remote command failed ({code}): {error[-1500:]}')
    return output


def install(client):
    script = '''set -eu
umask 077
install -d -m 700 /root/.codex
curl --fail --silent --show-error --location --max-time 30 https://chatgpt.com/codex/install.sh -o /root/.codex/install-official.sh
env CODEX_NON_INTERACTIVE=1 CODEX_INSTALL_DIR=/usr/local/bin CODEX_HOME=/root/.codex sh /root/.codex/install-official.sh
/usr/local/bin/codex --version
'''
    print(run(client, script, 600), end='')


def provider_config():
    database = Path.home() / '.cc-switch/cc-switch.db'
    with sqlite3.connect(database.as_uri() + '?mode=ro', uri=True) as db:
        row = db.execute('SELECT name, settings_config FROM providers WHERE id=? AND app_type=?',
                         (PROVIDER_ID, 'codex')).fetchone()
    if not row or row[0].casefold() != '666ai pro':
        raise ValueError('The requested CC Switch Codex provider was not found')
    settings = json.loads(row[1])
    config = tomllib.loads(settings['config'])
    provider = config['model_providers'][config['model_provider']]
    url = provider['base_url']
    if urlsplit(url).scheme != 'https' or urlsplit(url).hostname != '666ai.top':
        raise ValueError('Unexpected provider endpoint; inspect the selected provider')
    if not settings['auth'].get('OPENAI_API_KEY'):
        raise ValueError('The selected provider has no API key')
    return settings['auth'], url, row[0]


def write_private(sftp, target, data):
    if not target.startswith(REMOTE_HOME + '/'):
        raise ValueError('Destination is outside Codex home')
    temporary = target + '.install-' + os.urandom(8).hex()
    with sftp.open(temporary, 'wx') as handle:
        sftp.chmod(temporary, 0o600)
        handle.write(data)
        handle.flush()
    sftp.posix_rename(temporary, target)
    if sftp.stat(target).st_mode & 0o777 != 0o600:
        raise RuntimeError('Private file permissions were not applied')


def sync(client):
    source = (HOME / 'config.toml').read_bytes()
    config = tomllib.loads(source.decode('utf-8-sig'))
    auth, endpoint, provider_name = provider_config()
    active = config['model_providers'][config['model_provider']]
    active['base_url'] = endpoint
    # An old inline token takes precedence over the newly selected auth.json.
    for key in ('experimental_bearer_token', 'env_key', 'env_key_instructions'):
        active.pop(key, None)
    for field in ('http_headers', 'env_http_headers'):
        if field in active:
            for name in list(active[field]):
                if name.lower() in ('authorization', 'api-key', 'x-api-key'):
                    del active[field][name]
    active['requires_openai_auth'] = True
    adaptations = []
    for field in ('notify', 'windows', 'windows_wsl_setup_acknowledged', 'desktop', 'marketplaces'):
        if field in config:
            del config[field]
            adaptations.append(field)
    projects = config.get('projects', {})
    windows_projects = [p for p in projects if len(p) > 2 and p[1] == ':']
    for path in windows_projects:
        del projects[path]
    if not projects:
        config.pop('projects', None)
    env = config.get('shell_environment_policy', {}).get('set', {})
    env.pop('NODE_REPL_TRUSTED_BROWSER_CLIENT_SHA256S', None)
    disabled_mcp = []
    for name, server in config.get('mcp_servers', {}).items():
        command = server.get('command', '')
        if (len(command) > 2 and command[1] == ':') or command.lower().endswith('.exe'):
            server['enabled'] = False
            disabled_mcp.append(name)
    uploads = {}
    instructions = config.get('model_instructions_file')
    if instructions:
        prompt = Path(instructions).read_bytes()
        destination = REMOTE_HOME + '/prompts/' + Path(instructions).name
        config['model_instructions_file'] = destination
        uploads[destination] = prompt
    encoded = tomli_w.dumps(config).encode('utf-8')
    assert tomllib.loads(encoded.decode()) == config
    uploads[REMOTE_HOME + '/auth.json'] = (json.dumps(auth, indent=2) + '\n').encode()
    uploads[REMOTE_HOME + '/config.toml'] = encoded
    run(client, 'install -d -m 700 /root/.codex /root/.codex/prompts')
    with client.open_sftp() as sftp:
        for target, content in uploads.items():
            try:
                with sftp.open(target, 'rb') as handle:
                    previous = handle.read()
            except FileNotFoundError:
                previous = None
            if previous is not None and previous != content:
                write_private(sftp, target + '.before-sync-' + str(time.time_ns()), previous)
            write_private(sftp, target, content)
        for target, content in uploads.items():
            with sftp.open(target, 'rb') as handle:
                if handle.read() != content:
                    raise RuntimeError('Remote configuration content verification failed')
    report = {
        'source_config': str(HOME / 'config.toml'), 'provider': provider_name,
        'base_url': endpoint, 'model': config.get('model'),
        'model_reasoning_effort': config.get('model_reasoning_effort'),
        'config_path': REMOTE_HOME + '/config.toml', 'auth_path': REMOTE_HOME + '/auth.json',
        'config_sha256': hashlib.sha256(encoded).hexdigest(),
        'key_matches_selected_provider': True, 'private_file_mode': '0600',
        'linux_omitted_sections': adaptations, 'omitted_windows_project_count': len(windows_projects),
        'windows_mcp_disabled': disabled_mcp,
        'desktop_plugin_cache_copied': False,
        'model_instructions_file': config.get('model_instructions_file'),
    }
    folder = ROOT / '.local/main-node-codex'
    folder.mkdir(parents=True, exist_ok=True)
    (folder / 'config-sync-report.json').write_text(json.dumps(report, indent=2), encoding='utf-8')
    print(json.dumps(report, ensure_ascii=True))


def full_access(client):
    target = REMOTE_HOME + '/config.toml'
    with client.open_sftp() as sftp:
        with sftp.open(target, 'rb') as handle:
            original = handle.read()
        config = tomllib.loads(original.decode('utf-8'))
        config['sandbox_mode'] = 'danger-full-access'
        config['approval_policy'] = 'never'
        updated = tomli_w.dumps(config).encode('utf-8')
        assert tomllib.loads(updated.decode()) == config
        if original != updated:
            write_private(sftp, target + '.before-full-access-' + str(time.time_ns()), original)
            write_private(sftp, target, updated)
        with sftp.open(target, 'rb') as handle:
            actual = tomllib.loads(handle.read().decode('utf-8'))
        if actual != config:
            raise RuntimeError('Remote permissions update verification failed')
    run(client, 'CODEX_HOME=/root/.codex codex features list >/dev/null', 30)
    print(json.dumps({'config_path': target, 'sandbox_mode': actual['sandbox_mode'],
                      'approval_policy': actual['approval_policy'], 'cli_config_check': 'pass',
                      'other_settings_unchanged': True, 'private_file_mode': '0600'}))


def verify(client):
    auth, endpoint, provider = provider_config()
    with client.open_sftp() as sftp:
        with sftp.open(REMOTE_HOME + '/config.toml', 'rb') as handle:
            config = tomllib.loads(handle.read().decode('utf-8'))
        with sftp.open(REMOTE_HOME + '/auth.json', 'rb') as handle:
            installed_auth = json.loads(handle.read())
        active = config['model_providers'][config['model_provider']]
        if active['base_url'] != endpoint or installed_auth.get('OPENAI_API_KEY') != auth['OPENAI_API_KEY'] or active.get('experimental_bearer_token'):
            raise RuntimeError('Installed provider does not match the selected CC Switch provider')
        for name in ('config.toml', 'auth.json'):
            if sftp.stat(REMOTE_HOME + '/' + name).st_mode & 0o777 != 0o600:
                raise RuntimeError('Codex configuration file permissions changed')
    # Captured raw CLI output stays in memory: report status, never auth values.
    source = '''import json, os, subprocess, tempfile
os.environ['CODEX_HOME'] = '/root/.codex'
results = {}
for name, command in [('version',['codex','--version']),('features',['codex','features','list']),('login',['codex','login','status'])]:
    result = subprocess.run(command, capture_output=True, text=True, timeout=30)
    results[name] = {'returncode':result.returncode}
    if name == 'version': results[name]['version'] = result.stdout.strip()
    if name == 'features': results[name]['config_parsed'] = result.returncode == 0
    if name == 'login': results[name]['authenticated'] = result.returncode == 0
print(json.dumps(results))
'''
    report = json.loads(run(client, 'python3 -c ' + shlex.quote(source), 100))
    report.update(provider=provider, endpoint=endpoint, key_matches_selected_provider=True,
                  model=config.get('model'), reasoning=config.get('model_reasoning_effort'), private_file_mode='0600')
    folder = ROOT / '.local/main-node-codex'
    folder.mkdir(parents=True, exist_ok=True)
    (folder / 'verification-report.json').write_text(json.dumps(report, indent=2), encoding='utf-8')
    print(json.dumps(report))


def check_endpoint(client):
    source = '''import json, urllib.request, urllib.error
with open('/root/.codex/auth.json') as f: key=json.load(f)['OPENAI_API_KEY']
req=urllib.request.Request('https://666ai.top/v1/models', headers={'Authorization':'Bearer '+key})
try:
    with urllib.request.urlopen(req,timeout=25) as response:
        data=json.load(response)
        models=data.get('data',data.get('models',[]))
        print(json.dumps({'http_status':response.status,'model_count':len(models),'configured_model_listed':any(x.get('id')=='gpt-6-astra' for x in models)}))
except urllib.error.HTTPError as error:
    print(json.dumps({'http_status':error.code}))
except Exception as error:
    print(json.dumps({'error_type':type(error).__name__}))
'''
    print(run(client, 'python3 -c ' + shlex.quote(source), 40))


def smoke(client, low_effort=False):
    source = '''import json, os, subprocess, tempfile, time
os.environ['CODEX_HOME'] = '/root/.codex'
started = time.monotonic()
with tempfile.TemporaryDirectory(prefix='codex-install-check-') as directory:
    command = ['codex','exec','--ephemeral','--skip-git-repo-check','--sandbox','read-only','--json',
               '-c','approval_policy="never"','-c','mcp_servers.playwright.enabled=false',
               'Do not use any tools or read any files. Reply with exactly CODEX_MAIN_READY and nothing else.']
    if LOW_EFFORT: command[2:2]=['-c','model_reasoning_effort="low"']
    timed_out=False
    try:
        result = subprocess.run(command,cwd=directory,stdin=subprocess.DEVNULL,capture_output=True,text=True,timeout=120 if LOW_EFFORT else 240)
    except subprocess.TimeoutExpired as error:
        timed_out=True
        result=subprocess.CompletedProcess(command,124,(error.stdout or b'').decode('utf-8','replace'),(error.stderr or b'').decode('utf-8','replace'))
events=[]
for line in result.stdout.splitlines():
    try: events.append(json.loads(line))
    except ValueError: pass
messages=[e.get('item',{}).get('text','') for e in events if e.get('type')=='item.completed' and e.get('item',{}).get('type')=='agent_message']
usage=[e.get('usage') for e in events if e.get('type')=='turn.completed']
success=result.returncode==0 and any(message.strip()=='CODEX_MAIN_READY' for message in messages)
report={'passed':success,'exit_code':result.returncode,'model':'gpt-6-astra','provider':'666AI pro','smoke_effort':'low' if LOW_EFFORT else 'configured','timed_out':timed_out,'marker_received':any('CODEX_MAIN_READY' in message for message in messages),'turn_completed':bool(usage),'usage':usage,'elapsed_seconds':round(time.monotonic()-started,2),'error_event_count':sum(e.get('type') in ('error','turn.failed') for e in events),'command_execution_count':sum(e.get('item',{}).get('type')=='command_execution' for e in events),'stderr_lines':len(result.stderr.splitlines())}
if not success:
    combined=(result.stderr+' '+result.stdout).lower()
    report['failure_signals']=[s for s in ['401','403','429','unauthorized','invalid api key','unsupported','model not found','failed to load','not found','mcp','timeout'] if s in combined]
    with open('/root/.codex/auth.json') as f: secret=json.load(f)['OPENAI_API_KEY']
    errors=[e.get('message') or e.get('error',{}).get('message','') for e in events if e.get('type') in ('error','turn.failed')]
    report['errors']=[str(message).replace(secret,'[REDACTED]')[:700] for message in errors[-3:]]
    report['stderr_tail']=result.stderr.replace(secret,'[REDACTED]')[-2000:]
print(json.dumps(report))
'''
    source = 'LOW_EFFORT=' + repr(low_effort) + '\n' + source
    report = json.loads(run(client, 'python3 -c ' + shlex.quote(source), 260))
    folder = ROOT / '.local/main-node-codex'
    folder.mkdir(parents=True, exist_ok=True)
    (folder / ('smoke-low-report.json' if low_effort else 'smoke-report.json')).write_text(json.dumps(report, indent=2), encoding='utf-8')
    print(json.dumps(report))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action', choices=['install', 'sync', 'verify', 'endpoint', 'smoke', 'full-access'])
    parser.add_argument('--low-effort', action='store_true', help='Override only the smoke request reasoning effort')
    args = parser.parse_args()
    client = connect()
    try:
        if args.action == 'install':
            install(client)
        elif args.action == 'sync':
            sync(client)
        elif args.action == 'verify':
            verify(client)
        elif args.action == 'endpoint':
            check_endpoint(client)
        elif args.action == 'full-access':
            full_access(client)
        else:
            smoke(client, args.low_effort)
    finally:
        client.close()


if __name__ == '__main__':
    main()
