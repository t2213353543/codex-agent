"""Server-side isolated App Server quality comparison; invoked by its runner.

Credentials stay in memory/on the authorized server. No refresh token, production
configuration change, or deployment is performed. Only sanitized final answers
and protocol observations leave this process.
"""
import base64
from collections import Counter
from concurrent.futures import ThreadPoolExecutor
import hashlib
import json
import os
from pathlib import Path
import queue
import re
import subprocess
import tempfile
import threading
import time
import urllib.request
import uuid

CLI = '/opt/codex-agent-integration/checks/quality-20260924/official-bundle/bin/codex'
MODEL = 'gpt-6-astra'
EFFORT = 'xhigh'
ROUTES = ('app', 'bps')
ADMIN = 'http://127.0.0.1:18879'
BPS = 'https://bps.openai.com/basispoints/api'
INSTRUCTION = 'Solve only the supplied problem. Do not call any tools. Return your answer directly.'
LOCK = threading.Lock()
SECRETS = []
HTTP = urllib.request.build_opener(urllib.request.ProxyHandler({}))


def safe(value):
    raw = json.dumps(value, ensure_ascii=False)
    for secret in sorted(set(x for x in SECRETS if isinstance(x, str) and len(x) > 3), key=len, reverse=True):
        raw = raw.replace(secret, '[redacted]')
    raw = re.sub(r'eyJ[A-Za-z0-9_-]{15,}\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+', '[redacted-jwt]', raw)
    return json.loads(raw)


def emit(value):
    with LOCK:
        print(json.dumps(safe(value), ensure_ascii=False), flush=True)


def claims(token):
    body = token.split('.')[1]
    return json.loads(base64.urlsafe_b64decode(body + '=' * (-len(body) % 4)))


def admin(path, body=None):
    req = urllib.request.Request(ADMIN + path, headers={'Authorization': 'Bearer ' + ADMIN_KEY, 'Content-Type': 'application/json'},
                                 data=json.dumps(body).encode() if body is not None else None)
    with HTTP.open(req, timeout=25) as response:
        return json.load(response)


def config_text():
    return '''model = "gpt-6-astra"
model_provider = "openai"
model_reasoning_effort = "xhigh"
approval_policy = "never"
sandbox_mode = "read-only"
web_search = "disabled"
cli_auth_credentials_store = "file"
project_doc_max_bytes = 0
suppress_unstable_features_warning = true
developer_instructions = "Solve only the supplied problem. Do not call any tools. Return your answer directly."
[history]
persistence = "none"
[analytics]
enabled = false
[feedback]
enabled = false
[skills]
include_instructions = false
[skills.bundled]
enabled = false
[tools.update_plan]
enabled = false
[tools.experimental_request_user_input]
enabled = false
[features]
shell_tool = false
view_image = false
sleep_tool = false
js_repl = false
code_mode = false
apps = false
enable_mcp_apps = false
multi_agent = false
multi_agent_v2 = false
multi_agent_mode = false
enable_fanout = false
image_generation = false
hooks = false
plugin_hooks = false
plugins = false
remote_plugin = false
skill_search = false
tool_search = false
tool_suggest = false
request_permissions_tool = false
skip_host_skill_discovery = true
token_budget = false
current_time_reminder = false
'''.replace('model_reasoning_effort = "xhigh"', 'model_reasoning_effort = "' + EFFORT + '"')


def environment(home):
    env = {k: v for k, v in os.environ.items() if k in ['PATH', 'LANG', 'LC_ALL', 'TZ']}
    env.update(CODEX_HOME=str(home), RUST_LOG='off', NO_COLOR='1',
               CODEX_REFRESH_TOKEN_URL_OVERRIDE='http://127.0.0.1:1/refresh-disabled',
               NO_PROXY='127.0.0.1,localhost,::1')
    return env


class RPC:
    def __init__(self, home, workspace):
        self.process = subprocess.Popen([CLI, 'app-server'], cwd=workspace, env=environment(home),
                                        stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                                        text=True, bufsize=1)
        self.messages = queue.Queue()
        self.pending = []
        self.stderr = []
        self.number = 0
        def reader():
            try:
                for line in self.process.stdout:
                    self.messages.put(json.loads(line))
            finally:
                self.messages.put(None)
        def errors():
            for line in self.process.stderr:
                if len(self.stderr) < 100:
                    self.stderr.append(line[:600])
        threading.Thread(target=reader, daemon=True).start()
        threading.Thread(target=errors, daemon=True).start()

    def send(self, obj):
        self.process.stdin.write(json.dumps(obj, ensure_ascii=False) + '\n')
        self.process.stdin.flush()

    def receive(self, timeout):
        msg = self.messages.get(timeout=timeout)
        if msg is None:
            raise RuntimeError('app_server_eof')
        if 'id' in msg and 'method' in msg:
            # Do not refresh accounts or approve unexpected execution during a reasoning test.
            self.send({'id': msg['id'], 'error': {'code': -32000, 'message': 'Operation unavailable in isolated reasoning probe'}})
        return msg

    def call(self, method, params, timeout=90):
        self.number += 1
        number = self.number
        self.send({'id': number, 'method': method, 'params': params})
        end = time.monotonic() + timeout
        while time.monotonic() < end:
            msg = self.receive(max(0.1, end - time.monotonic()))
            if msg.get('id') == number and 'method' not in msg:
                if 'error' in msg:
                    raise RuntimeError('RPC ' + method + ': ' + str(safe(msg['error']))[:600])
                return msg['result']
            self.pending.append(msg)
        raise TimeoutError('RPC deadline')

    def close(self):
        try:
            self.process.stdin.close()
            self.process.wait(timeout=5)
        except Exception:
            self.process.kill()
            self.process.wait(timeout=5)


def app_turn(rpc, workspace, case, alias, repetition, on_delta=None, route='app_server_codex'):
    started = time.monotonic()
    response = rpc.call('thread/start', {'model': MODEL, 'modelProvider': 'openai', 'cwd': str(workspace),
                                       'approvalPolicy': 'never', 'sandbox': 'read-only', 'ephemeral': True})
    thread_id = response['thread']['id']
    identity = {k: response.get(k) for k in ['model', 'modelProvider', 'reasoningEffort', 'serviceTier']}
    if identity['model'] != MODEL or identity['modelProvider'] != 'openai' or identity['reasoningEffort'] != EFFORT:
        raise RuntimeError('thread_identity_mismatch ' + str(identity))
    rpc.pending.clear()
    kickoff = rpc.call('turn/start', {'threadId': thread_id, 'input': [{'type': 'text', 'text': case['prompt']}],
                                      'model': MODEL, 'effort': EFFORT})
    methods, item_types, final, deltas, errors, usage = Counter(), Counter(), [], {}, [], None
    terminal = None
    end = time.monotonic() + (420 if EFFORT == 'max' else 240)
    while time.monotonic() < end:
        try:
            msg = rpc.pending.pop(0) if rpc.pending else rpc.receive(max(0.1, end - time.monotonic()))
        except queue.Empty:
            break
        method = msg.get('method', '')
        params = msg.get('params', {})
        if params.get('threadId') not in (None, thread_id):
            continue
        methods[method] += 1
        if method == 'item/agentMessage/delta':
            item_id = params.get('itemId', 'unknown')
            deltas[item_id] = deltas.get(item_id, '') + params.get('delta', '')
            if on_delta is not None:
                on_delta(params.get('delta', ''))
        if method == 'item/completed':
            item = params.get('item', {})
            item_types[item.get('type', '')] += 1
            if item.get('type') == 'agentMessage':
                final.append({'id': item.get('id'), 'text': item.get('text', ''), 'phase': item.get('phase')})
        if method == 'thread/tokenUsage/updated':
            usage = params.get('tokenUsage')
        if method in ['error', 'model/rerouted', 'account/chatgptAuthTokens/refresh']:
            errors.append({'method': method, 'details': safe(params)})
        if method == 'turn/completed':
            terminal = params['turn']
            break
    if terminal is None:
        rpc.call('turn/interrupt', {'threadId': thread_id, 'turnId': kickoff['turn']['id']}, timeout=5)
        raise TimeoutError('turn completion deadline')
    tools = {k: v for k, v in item_types.items() if k not in ['agentMessage', 'reasoning', 'userMessage']}
    answer = '\n'.join(x['text'] for x in final if x['phase'] in ('final_answer', None))
    record = {'kind': 'case', 'route': route, 'account': alias, 'case': case['id'], 'repetition': repetition,
              'prompt_sha256': hashlib.sha256(case['prompt'].encode()).hexdigest(), 'identity': identity,
              'instruction_sources': response.get('instructionSources', []),
              'status': terminal.get('status'), 'terminal_error': terminal.get('error'), 'final_output': answer,
              'elapsed_seconds': round(time.monotonic() - started, 3), 'stream_delta_events': methods['item/agentMessage/delta'],
              'delta_matches_completed': bool(final) and all(deltas.get(x['id']) == x['text'] for x in final),
              'event_counts': dict(methods), 'item_types': dict(item_types), 'tool_items': tools, 'errors': errors, 'usage': usage}
    emit(record)
    return record


def bps_turn(account, case, alias, repetition):
    started = time.monotonic()
    token, auth = account['access_token'], account['auth']
    headers = {'Authorization': 'Bearer ' + token, 'Content-Type': 'application/json', 'X-Basispoints-Auth-Mode': 'chatgpt',
               'User-Agent': 'Mozilla/5.0', 'Accept': 'application/json, text/event-stream',
               'X-Openai-Internal-Basispoints-Client-Product': 'basispoints-powerpoint-plugin',
               'X-Openai-Internal-Basispoints-Client-Platform': 'powerpoint',
               'X-Openai-Internal-Basispoints-Client-Agent-Profile': 'powerpoint',
               'X-Openai-Internal-Basispoints-Client-Editor': 'powerpoint',
               'X-Openai-Internal-Basispoints-Client-Host': 'office', 'X-Openai-Internal-Basispoints-Client-Runtime': 'web',
               'X-Openai-Internal-Basispoints-Client-Platform-Class': 'web',
               'ChatGPT-Account-ID': account['account_id'], 'X-OpenAI-Account-Id': account['account_id']}
    if auth.get('chatgpt_account_user_id'):
        headers['X-OpenAI-Account-User-Id'] = auth['chatgpt_account_user_id']
    payload = {'model': MODEL, 'model_selection': 'explicit', 'reasoning_effort': EFFORT, 'stream': True, 'store': False,
               'input': [{'role': 'developer', 'content': INSTRUCTION}, {'role': 'user', 'content': case['prompt']}],
               'metadata': {'task_id': str(uuid.uuid4()), 'turn_id': str(uuid.uuid4()), 'agent_iteration': '1',
                            'bps_tools_version_id': 'tools-powerpoint-core-2026-08-10-2b886486'}}
    req = urllib.request.Request(BPS + '/responses', headers=headers, data=json.dumps(payload).encode())
    events, terminal, tools = Counter(), None, []
    with HTTP.open(req, timeout=240) as response:
        for raw in response:
            if not raw.startswith(b'data:') or raw[5:].strip() == b'[DONE]':
                continue
            event = json.loads(raw[5:])
            kind = event.get('type', '')
            events[kind] += 1
            if kind == 'response.output_item.done':
                item = event.get('item', {})
                if item.get('type') not in ['message', 'reasoning']:
                    tools.append(item.get('type'))
            if kind in ['response.completed', 'response.failed', 'response.incomplete', 'error']:
                terminal = event
                break
    result = (terminal or {}).get('response', {})
    answer = ''.join(c.get('text', '') for item in result.get('output', []) if item.get('type') == 'message'
                     for c in item.get('content', []) if c.get('type') == 'output_text')
    record = {'kind': 'case', 'route': 'powerpoint_bps', 'account': alias, 'case': case['id'], 'repetition': repetition,
              'prompt_sha256': hashlib.sha256(case['prompt'].encode()).hexdigest(), 'identity': {'model': MODEL, 'effort': EFFORT},
              'status': (terminal or {}).get('type'), 'response_model': result.get('model'), 'final_output': answer,
              'elapsed_seconds': round(time.monotonic() - started, 3), 'event_counts': dict(events), 'tool_items': tools,
              'terminal_error': result.get('error') or (terminal or {}).get('error'), 'usage': result.get('usage')}
    emit(record)
    return record


def run_account(account, cases, pilot):
    alias = account['alias']
    parent = Path('/opt/codex-agent-integration/checks').resolve()
    with tempfile.TemporaryDirectory(prefix='app-server-quality-', dir=parent) as temporary:
        root = Path(temporary)
        home, workspace = root / 'home', root / 'workspace'
        home.mkdir(mode=0o700); workspace.mkdir(mode=0o700)
        (home / 'config.toml').write_text(config_text())
        rpc = RPC(home, workspace)
        try:
            initialize = rpc.call('initialize', {'clientInfo': {'name': 'app_server_quality_probe', 'version': '1.0'},
                                                'capabilities': {'experimentalApi': True}})
            rpc.send({'method': 'initialized', 'params': {}})
            login = {'type': 'chatgptAuthTokens', 'accessToken': account['access_token'], 'chatgptAccountId': account['account_id']}
            if account['auth'].get('chatgpt_plan_type'):
                login['chatgptPlanType'] = account['auth']['chatgpt_plan_type']
            login_result = rpc.call('account/login/start', login)
            current = rpc.call('account/read', {'refreshToken': False})
            configuration = rpc.call('config/read', {'includeLayers': True, 'cwd': str(workspace)})
            effective = configuration.get('config', {})
            provider_config = effective.get('model_providers') or {}
            provider_override = (provider_config.get('openai') or {}).get('base_url')
            assert not effective.get('openai_base_url') and not provider_override, 'custom_openai_endpoint'
            catalog = rpc.call('model/list', {'includeHidden': True, 'limit': 100})
            selected = [x for x in catalog.get('data', []) if x.get('model') == MODEL or x.get('id') == MODEL]
            assert selected, 'requested_model_not_advertised'
            efforts = [x['reasoningEffort'] for x in selected[0].get('supportedReasoningEfforts', [])]
            assert EFFORT in efforts, 'requested_effort_not_advertised'
            emit({'kind': 'runtime', 'account': alias, 'cli_version': CLI_VERSION, 'cli_sha256': CLI_SHA,
                  'initialize': initialize, 'login_type': login_result.get('type'),
                  'account_type': (current.get('account') or {}).get('type'),
                  'plan_type': (current.get('account') or {}).get('planType'), 'requires_openai_auth': current.get('requiresOpenaiAuth'),
                  'selected_model': MODEL, 'effort': EFFORT, 'advertised_efforts': efforts,
                  'config_read': {'model_provider': effective.get('model_provider'), 'openai_base_url': effective.get('openai_base_url'),
                                  'openai_provider_base_url': provider_override, 'layer_count': len(configuration.get('layers') or [])},
                  'environment_provider_overrides': False, 'config_sha256': hashlib.sha256(config_text().encode()).hexdigest(),
                  'system_config_present': [str(p) for p in [Path('/etc/codex/config.toml'), Path('/etc/codex/requirements.toml'), Path('/etc/codex/managed_config.toml')] if p.exists()]})
            sequence = [(case, 1) for case in cases]
            if not pilot:
                sequence.append((cases[0], 2))
            failed_routes = set()
            for case, repetition in sequence:
                order = ['app', 'bps'] if alias == 'account_1' else ['bps', 'app']
                if 'bridge' in ROUTES: order.append('bridge')
                order = [route for route in order if route in ROUTES]
                if pilot:
                    order = ['app']
                for route in order:
                    if route in failed_routes:
                        continue
                    emit({'kind': 'started', 'account': alias, 'route': route, 'case': case['id'], 'repetition': repetition})
                    try:
                        if route == 'app': app_turn(rpc, workspace, case, alias, repetition)
                        elif route == 'bridge':
                            namespace = {}
                            exec(compile(BRIDGE_SOURCE, 'quality_app_server_bridge.py', 'exec'), namespace)
                            namespace['bridge_turn'](rpc, workspace, case, alias, repetition, app_turn, MODEL, EFFORT, emit)
                        else: bps_turn(account, case, alias, repetition)
                    except Exception as error:
                        emit({'kind': 'case_error', 'account': alias, 'route': route, 'case': case['id'], 'repetition': repetition,
                              'error_type': type(error).__name__, 'message': str(error)[:700]})
                        if route in ['app', 'bridge']:
                            return
                        failed_routes.add(route)
        finally:
            rpc.close()
            emit({'kind': 'cleanup', 'account': alias, 'process_stopped': rpc.process.poll() is not None,
                  'stderr_lines': len(rpc.stderr), 'stderr_reroute_observed': any('rerout' in x.lower() for x in rpc.stderr)})
    emit({'kind': 'cleanup_directory', 'account': alias, 'temporary_directory_removed': not root.exists()})


def main(cases, pilot=False, effort='xhigh', routes=('app', 'bps')):
    global ADMIN_KEY, CLI_VERSION, CLI_SHA, EFFORT, ROUTES
    assert effort in ['xhigh', 'max']
    EFFORT = effort
    ROUTES = routes
    ADMIN_KEY = Path('/var/lib/codex-agent/client-key').read_text().strip()
    SECRETS.append(ADMIN_KEY)
    CLI_VERSION = subprocess.check_output([CLI, '--version'], text=True).strip()
    CLI_SHA = hashlib.sha256(Path(CLI).read_bytes()).hexdigest()
    settings = admin('/api/settings')
    emit({'kind': 'baseline', 'production_version': settings.get('version'), 'production_inference': settings.get('inference'),
          'started_at': time.time(), 'pilot': pilot, 'model': MODEL, 'effort': EFFORT,
          'app_server_upstream': 'built-in openai / ChatGPT authentication', 'powerpoint_upstream': BPS})
    listing = admin('/api/accounts?limit=100')
    rows = sorted([x for x in listing['accounts'] if x.get('enabled')], key=lambda x: x['id'])
    assert not listing.get('has_more') and len(rows) == 2, 'expected two authorized accounts'
    accounts = []
    for index, row in enumerate(rows):
        exported = admin('/api/accounts/export/credentials', {'items': [{'id': row['id'], 'version': row['version']}]})['accounts'][0]['credentials']
        if isinstance(exported, str): exported = json.loads(exported)
        token = exported['access_token']; decoded = claims(token); auth = decoded.get('https://api.openai.com/auth', {})
        assert decoded['exp'] > time.time() + 1800, 'token close to expiry; no refresh performed'
        account_id = exported.get('account_id') or auth.get('chatgpt_account_id')
        SECRETS.extend([token, account_id, row['id'], decoded.get('email'), decoded.get('sub')])
        SECRETS.extend(auth.get(k) for k in ['chatgpt_account_id', 'chatgpt_user_id', 'user_id', 'chatgpt_account_user_id'])
        profile = decoded.get('https://api.openai.com/profile', {})
        SECRETS.append(profile.get('email'))
        accounts.append({'alias': 'account_' + str(index + 1), 'access_token': token, 'account_id': account_id, 'auth': auth})
    if pilot:
        run_account(accounts[0], cases[:1], True)
    else:
        with ThreadPoolExecutor(max_workers=2) as pool:
            list(pool.map(lambda account: run_account(account, cases, False), accounts))
    after = admin('/api/settings')
    emit({'kind': 'finished', 'completed_at': time.time(), 'production_version_unchanged': after.get('version') == settings.get('version'),
          'production_inference_unchanged': after.get('inference') == settings.get('inference')})
