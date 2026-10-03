"""One native BPS candy request using the isolated child-B Office OAuth grant.

Credentials remain in the existing server-private profile. No OAuth, refresh,
managed-account write, production setting change, or automatic model retry.
"""
from __future__ import annotations

import argparse
import base64
import contextlib
import hashlib
import json
import logging
import os
from pathlib import Path
import re
import sys
import time
import uuid
from urllib.parse import urlparse

CHILD = '2d3524dbef4877afde33d7d5a0ad0edf'
EXPERIMENT = Path('/app/data/oauth-profile-experiment-20260928')
BASE = 'https://bps.openai.com/basispoints/api'
MODEL = 'gpt-6-astra'
VERSION = 'tools-powerpoint-core-2026-08-10-2b886486'


def sha(value):
    return hashlib.sha256(value if isinstance(value, bytes) else value.encode()).hexdigest()


def worker():
    if not sys.platform.startswith('linux'):
        raise RuntimeError('Linux server process required')
    sys.path.insert(0, '/app')
    sys.path.insert(0, str(EXPERIMENT / 'code'))
    import webui
    webui.__path__.insert(0, str(EXPERIMENT / 'code/webui'))
    from auth_flow import AuthFlow
    from config import Config
    from webui.team_rotation import TeamRotationController, decode_jwt_identity
    from webui import db
    from credential_utils import validated_protocol_fingerprint
    selectors = json.loads((EXPERIMENT / 'selectors.json').read_text())
    selector = next(x for x in selectors if x['child_ref'] == CHILD)
    credentials_path = EXPERIMENT / 'credentials' / (CHILD + '-office_plugin.json')
    assert credentials_path.stat().st_mode & 0o077 == 0
    credentials_before = sha(credentials_path.read_bytes())
    grant = json.loads(credentials_path.read_text())
    assert grant['auth_profile'] == 'office_plugin'
    identity = decode_jwt_identity(grant['access_token'])
    assert identity['account_id'] == selector['workspace_id']
    segment = grant['access_token'].split('.')[1]
    claims = json.loads(base64.urlsafe_b64decode(segment + '=' * (-len(segment) % 4)))
    assert claims.get('client_id') == 'app_fnr0pYvVwwFDocDumLG3H2Bp'
    assert claims['exp'] > time.time()
    child = db.get_team_rotation_child(selector['email'])
    owner = db.get_team_rotation_owner(selector['owner_id'])
    assert child['owner_id'] == selector['owner_id'] and child['status'] == 'active'
    assert float(child['assigned_at']) == selector['assigned_at']
    assert owner['workspace_id'] == selector['workspace_id'] and owner['status'] == 'active'
    controller = TeamRotationController()
    cfg = controller.config()
    cfg['proxy_required'] = True
    network = controller._oauth_network(cfg, owner)
    assert network.require_proxy and urlparse(network.proxy_url).hostname == 'account-warp'
    fingerprint = validated_protocol_fingerprint(grant['fingerprint_profile'])
    config = Config()
    config.proxy = network.proxy_url
    flow = AuthFlow(config, fingerprint_profile=fingerprint, auth_profile='office_plugin')
    flow.result.device_id = grant['device_id']
    assert flow._fingerprint == fingerprint
    prompt = (EXPERIMENT / 'code/candy-prompt.txt').read_text(encoding='utf-8')
    assert '圆形 7 9 8' in prompt and '五角星形 7 6 4' in prompt and '21' not in prompt
    private_values = [grant.get(x) for x in ('access_token', 'refresh_token', 'id_token', 'device_id')]
    private_values.extend([selector['email'], selector['workspace_id'], identity.get('user_id')])

    def safe_message(value, limit=None):
        message = str(value or '')
        for secret in private_values:
            if isinstance(secret, str) and secret:
                message = message.replace(secret, '[REDACTED]')
        message = re.sub(r'(?i)Bearer\s+\S+', 'Bearer [REDACTED]', message)
        message = re.sub(r'[\w.+-]+@[\w.-]+', '[EMAIL]', message)
        message = re.sub(r'eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+', '[JWT]', message)
        return message[:limit] if limit else message

    def error_fields(value):
        if not isinstance(value, dict):
            return {}
        error = value.get('error')
        if not isinstance(error, dict):
            return {}
        result = {'error_message': safe_message(error.get('message'), 1200),
                  'error_message_sha256': sha(str(error.get('message') or ''))}
        for key in ('code', 'type', 'param'):
            val = error.get(key)
            if val is None or isinstance(val, str) and re.fullmatch('[A-Za-z0-9_.:-]{0,100}', val):
                result['error_' + key] = val
        return result

    headers = {'Authorization': 'Bearer ' + grant['access_token'], 'Accept': 'application/json',
               'X-Basispoints-Auth-Mode': 'chatgpt', 'ChatGPT-Account-ID': selector['workspace_id'],
               'X-OpenAI-Account-Id': selector['workspace_id'], 'oai-device-id': grant['device_id']}
    auth = claims.get('https://api.openai.com/auth') or claims.get('auth') or {}
    if auth.get('chatgpt_account_user_id'):
        headers['X-OpenAI-Account-User-Id'] = auth['chatgpt_account_user_id']
    for key, value in {'Product': 'basispoints-powerpoint-plugin', 'Platform': 'powerpoint',
        'Agent-Profile': 'powerpoint', 'Editor': 'powerpoint', 'Host': 'office', 'Runtime': 'web',
        'Platform-Class': 'OfficeOnline'}.items():
        headers['X-Openai-Internal-Basispoints-Client-' + key] = value
    report = {'experiment': 'bps-office-grant', 'child_ref': CHILD, 'auth_profile': 'office_plugin',
              'started_at': time.time(), 'grant_sha256': sha(grant['access_token']),
              'prompt_sha256': sha(prompt), 'generation_requests': 0, 'automatic_retries': 0,
              'proxy_required': True, 'proxy_sha256': sha(network.proxy_url),
              'fingerprint_sha256': sha(json.dumps(fingerprint, sort_keys=True)),
              'device_sha256': sha(grant['device_id']), 'completed': False, 'usage': None}
    output = EXPERIMENT / 'bps-office-grant-probes'
    output.mkdir(mode=0o700, exist_ok=True)
    # One-time guard is separate from the root experiment journal and does not
    # lock child A or overwrite either profile's credentials.
    guard = output / (CHILD + '-generation-started.json')
    fd = os.open(guard, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, 'w') as stream:
        json.dump({'started_at': report['started_at'], 'grant_sha256': report['grant_sha256']}, stream)
    response = None
    started = time.monotonic()
    try:
        access = flow.session.get(BASE + '/responses/access?include_models=true', headers=headers,
                                  timeout=30, allow_redirects=False)
        try:
            data = access.json()
        except ValueError:
            data = {}
        report['access'] = {'http': access.status_code, 'allowed': data.get('allowed'),
                            'body_sha256': sha(access.content), **error_fields(data)}
        if access.status_code != 200 or data.get('allowed') is not True:
            report['stopped_reason'] = 'native_access_not_allowed'
            return report
        model = next((x for x in data.get('model_catalog', {}).get('models', []) if x.get('id') == MODEL), None)
        if not model:
            report['stopped_reason'] = 'exact_model_not_in_native_catalog'
            return report
        efforts = [x.get('value') if isinstance(x, dict) else x for x in model.get('efforts', [])]
        rank = ('none', 'minimal', 'low', 'medium', 'high', 'xhigh', 'max', 'ultra')
        supported = [x for x in rank if x in efforts]
        report['access']['model'] = MODEL
        report['access']['supported_efforts'] = efforts
        if not supported:
            report['stopped_reason'] = 'native_effort_catalog_missing'
            return report
        effort = supported[-1]
        payload = {'model': MODEL, 'input': [{'type': 'message', 'role': 'user', 'content': [
            {'type': 'input_text', 'text': prompt}]}], 'stream': True, 'store': False,
            'reasoning_effort': effort, 'model_selection': 'explicit', 'metadata': {
                'task_id': str(uuid.uuid4()), 'turn_id': str(uuid.uuid4()),
                'bps_tools_version_id': VERSION, 'agent_iteration': '1'}}
        headers.update({'Accept': 'text/event-stream', 'Content-Type': 'application/json'})
        report.update(requested_model=MODEL, requested_effort=effort,
                      request_body_sha256=sha(json.dumps(payload, separators=(',', ':'))), generation_requests=1)
        generation_started = time.monotonic()
        response = flow.session.post(BASE + '/responses', headers=headers, json=payload,
                                     timeout=300, stream=True, allow_redirects=False)
        report.update(http=response.status_code, header_ms=round((time.monotonic()-generation_started)*1000),
            content_type=response.headers.get('Content-Type'), request_id=response.headers.get('X-Request-Id'))
        raw_path = output / (str(time.time_ns()) + '-response.bin')
        report['private_response_file'] = raw_path.name
        raw_fd = os.open(raw_path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        chunks, output_items = [], []
        event_types = set()
        report['first_event_ms'] = None
        report['ttft_ms'] = None
        buffer, body = b'', bytearray()
        terminal = False

        def event_received(event):
            nonlocal terminal, output_items
            if not isinstance(event, dict):
                return
            if report['first_event_ms'] is None:
                report['first_event_ms'] = round((time.monotonic()-generation_started)*1000)
            kind = event.get('type', '')
            event_types.add(kind)
            if kind == 'response.output_text.delta':
                chunks.append(event.get('delta') or '')
                if report['ttft_ms'] is None and chunks[-1]:
                    report['ttft_ms'] = round((time.monotonic()-generation_started)*1000)
            if kind == 'response.output_item.done' and isinstance(event.get('item'), dict):
                output_items.append(event['item'])
            if kind in ('response.completed', 'response.failed', 'response.incomplete', 'error'):
                result = event.get('response') or {}
                report.update(terminal=kind, completed=kind == 'response.completed' and result.get('status') == 'completed',
                              returned_model=result.get('model'), usage=result.get('usage'))
                report.update(error_fields(result) or error_fields(event))
                if result.get('output'):
                    output_items = result['output']
                terminal = True

        with os.fdopen(raw_fd, 'wb') as raw_file:
            for chunk in response.iter_content():
                if not chunk:
                    continue
                raw_file.write(chunk)
                raw_file.flush()
                body.extend(chunk)
                if response.status_code != 200:
                    continue
                buffer += chunk
                while b'\n' in buffer:
                    line, buffer = buffer.split(b'\n', 1)
                    if not line.startswith(b'data:'):
                        continue
                    event_raw = line[5:].strip()
                    if event_raw == b'[DONE]':
                        continue
                    try:
                        event_received(json.loads(event_raw))
                    except ValueError:
                        pass
                if terminal:
                    break
            if response.status_code == 200 and buffer.startswith(b'data:'):
                try:
                    event_received(json.loads(buffer[5:].strip()))
                except ValueError:
                    pass
        report.update(response_bytes_observed=len(body), response_sha256=sha(bytes(body)))
        if response.status_code != 200:
            report['response_body_drained'] = True
            try:
                report.update(error_fields(json.loads(body)))
            except ValueError:
                pass
            return report
        if not event_types:
            try:
                result = json.loads(body)
            except ValueError:
                result = {}
            if isinstance(result, dict):
                report.update(response_format='json', completed=result.get('status') == 'completed',
                              returned_model=result.get('model'), usage=result.get('usage'), terminal=result.get('status'))
                report.update(error_fields(result))
                output_items = result.get('output') or []
        else:
            report['response_format'] = 'sse'
        final = ''.join(part.get('text', '') for item in output_items if item.get('type') == 'message'
                        for part in item.get('content', []) if part.get('type') == 'output_text')
        report.update(assistant_text=safe_message(final or ''.join(chunks)), event_types=sorted(event_types),
                      output_types=[x.get('type') for x in output_items])
        usage = report.get('usage') or {}
        cached = (usage.get('input_tokens_details') or {}).get('cached_tokens')
        input_tokens = usage.get('input_tokens')
        if isinstance(cached, int) and isinstance(input_tokens, int) and input_tokens > 0:
            report['cached_input_ratio'] = cached / input_tokens
        return report
    except Exception as error:
        report.update(error_class=type(error).__name__, error_sha256=sha(str(error)))
        return report
    finally:
        if response is not None:
            response.close()
        flow.session.close()
        report.update(elapsed_ms=round((time.monotonic()-started)*1000), finished_at=time.time(),
                      credentials_unchanged=credentials_before == sha(credentials_path.read_bytes()),
                      production_modified=False, journal_modified=False)
        serialized = json.dumps(report, ensure_ascii=False)
        assert all(not secret or secret not in serialized for secret in private_values if isinstance(secret, str))
        destination = output / (str(time.time_ns()) + '-result.json')
        fd = os.open(destination, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        with os.fdopen(fd, 'w', encoding='utf-8') as stream:
            stream.write(serialized)


def run():
    from integration_env import connect
    from oauth_profile_compare_ops import locations, remote, NAME
    client = connect()
    try:
        location = locations(client)
        host_root = location['data'] + '/' + NAME
        blob = Path(__file__).read_bytes()
        basename = 'bps-office-grant-probe-' + sha(blob)[:16] + '.py'
        host_path = host_root + '/' + basename
        with client.open_sftp() as sftp:
            with sftp.open(host_path, 'wb') as out:
                out.write(blob)
            sftp.chmod(host_path, 0o600)
        # This launcher is assembled on Windows but runs inside Linux. Keep
        # the container path POSIX instead of stringifying a Windows Path.
        script_path = '/app/data/oauth-profile-experiment-20260928/' + basename
        source = 'SCRIPT=' + repr(script_path) + '\nHOST_ROOT=' + repr(host_root) + '\n' + r'''
import json,subprocess,pathlib,os,time
p=pathlib.Path(HOST_ROOT)/('bps-office-operator-'+str(time.time_ns())+'.log')
fd=os.open(p,os.O_CREAT|os.O_EXCL|os.O_WRONLY,0o600)
with os.fdopen(fd,'wb') as log:
 result=subprocess.run(['docker','exec','loophub-multitenant-app','python',SCRIPT,'--worker'],stdout=log,stderr=subprocess.STDOUT,timeout=400)
rows=[]
for line in p.read_text(errors='replace').splitlines():
 try:
  row=json.loads(line)
  if isinstance(row,dict) and row.get('experiment')=='bps-office-grant':rows.append(row)
 except ValueError:pass
print(json.dumps({'exit_code':result.returncode,'summaries':rows,'private_log_retained':True}))
'''
        result = remote(client, source, timeout=430)
        folder = Path(__file__).resolve().parents[1] / '.local/latest-operations/project28765'
        folder.mkdir(exist_ok=True)
        (folder / ('bps-office-grant-' + str(time.time_ns()) + '.json')).write_text(result, encoding='utf-8')
        print(result)
    finally:
        client.close()


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--worker', action='store_true')
    args = parser.parse_args()
    if args.worker:
        logging.disable(logging.CRITICAL)
        log_path = EXPERIMENT / 'bps-office-grant-private.log'
        try:
            fd = os.open(log_path, os.O_WRONLY | os.O_CREAT | os.O_APPEND, 0o600)
            with os.fdopen(fd, 'a') as log, contextlib.redirect_stdout(log), contextlib.redirect_stderr(log):
                result = worker()
            print(json.dumps(result, ensure_ascii=False))
        except Exception as error:
            print(json.dumps({'experiment': 'bps-office-grant', 'error_class': type(error).__name__, 'error_sha256': sha(str(error))}))
            raise SystemExit(1)
    else:
        run()
