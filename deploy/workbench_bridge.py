"""Server-local, access-token-only delivery from the 18876 workbench.

No payment, OAuth refresh, source database writes, or credential logging.
"""
import argparse
import base64
import collections
import datetime as dt
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

ROOT = Path('/var/lib/codex-agent-workbench-bridge')
TAG = 'workbench-18876'
GROUP = '工作台 Pro · 并发验收'


class SafeError(Exception):
    pass


def digest(value):
    return hashlib.sha256(value.encode()).hexdigest()


def atomic(path, value):
    path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    tmp = path.with_suffix('.tmp')
    fd = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(fd, 'w') as f:
        json.dump(value, f, ensure_ascii=False)
        f.flush()
        os.fsync(f.fileno())
    os.replace(tmp, path)
    if os.name == 'posix':
        directory = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)


class API:
    def __init__(self, base, token=''):
        self.base, self.token = base, token
        self.http = urllib.request.build_opener(urllib.request.ProxyHandler({}))

    def call(self, path, data=None, method=None):
        headers = {'Content-Type': 'application/json', 'Cache-Control': 'no-store'}
        if self.token:
            headers['Authorization'] = 'Bearer ' + self.token
        request = urllib.request.Request(self.base + path, headers=headers,
            data=json.dumps(data).encode() if data is not None else None, method=method)
        try:
            with self.http.open(request, timeout=20) as response:
                raw = response.read(32 * 1024 * 1024 + 1)
                if len(raw) > 32 * 1024 * 1024:
                    raise SafeError('response_too_large')
                return json.loads(raw)
        except urllib.error.HTTPError as exc:
            raise SafeError('http_' + str(exc.code)) from None
        except (urllib.error.URLError, TimeoutError, ValueError):
            raise SafeError('api_unavailable_or_invalid_response') from None


def source_api():
    raw = subprocess.check_output(['docker', 'inspect', 'workspace-transfer-tool-backend'], timeout=10)
    info = json.loads(raw)[0]
    env = dict(x.split('=', 1) for x in info['Config']['Env'] if '=' in x)
    ips = [x['IPAddress'] for x in info['NetworkSettings']['Networks'].values() if x.get('IPAddress')]
    if not ips or not env.get('ADMIN_PASSWORD'):
        raise SafeError('source_configuration_unavailable')
    api = API('http://' + ips[0] + ':8000')
    login = api.call('/api/admin/login', {'password': env['ADMIN_PASSWORD']})
    api.token = login.get('token', '')
    if not api.token:
        raise SafeError('source_login_failed')
    return api, info['Image']


def eligible(job):
    return (job.get('status') == 'succeeded' and job.get('recharge_status') in ('paid', 'success')
        and job.get('recharge_plan_type') in ('pro5', 'pro20')
        and job.get('has_refresh_token') and not job.get('is_refunded')
        and not job.get('has_active_refund'))


def checked_job(job, detail=False, expected_id=None):
    if isinstance(job, dict) and detail and 'has_active_refund' not in job:
        # Current 18876 serializer removes its internal flag and exposes this
        # exhaustive pair instead (runtime source verified 2026-09-18).
        # Unknown/missing reasons are schema errors, never implied eligibility.
        active_reason = '退款执行中，请完成后删除'
        held_reason = '成功且尚未退款的注册任务不能删除'
        reason = job.get('delete_block_reason')
        if ('delete_block_reason' not in job or not isinstance(job.get('can_delete'), bool)
            or (job['can_delete'] and reason is not None)
            or (not job['can_delete'] and reason not in (active_reason, held_reason))):
            raise SafeError('source_schema_mismatch')
        job = {**job, 'has_active_refund': reason == active_reason}
    required = {'id', 'status', 'recharge_status', 'recharge_plan_type', 'has_refresh_token', 'is_refunded'}
    if detail:
        required.add('has_active_refund')
    if not isinstance(job, dict) or not required.issubset(job):
        raise SafeError('source_schema_mismatch')
    if (not isinstance(job['id'], str) or not job['id']
        or any(not isinstance(job[k], str) for k in ('status', 'recharge_status', 'recharge_plan_type'))
        or any(not isinstance(job[k], bool) for k in ('has_refresh_token', 'is_refunded'))
        or (detail and not isinstance(job['has_active_refund'], bool))
        or (expected_id is not None and job['id'] != expected_id)):
        raise SafeError('source_schema_mismatch')
    return job


def access_only(job, bundle):
    accounts = bundle.get('accounts', [])
    if len(accounts) != 1:
        raise SafeError('source_export_count_mismatch')
    credentials = accounts[0].get('credentials', {})
    token = credentials.get('access_token', '')
    try:
        payload = token.split('.')[1]
        claims = json.loads(base64.urlsafe_b64decode(payload + '=' * (-len(payload) % 4)))
        auth = claims.get('https://api.openai.com/auth', {})
        expiry = int(claims.get('exp', 0))
        identity = auth.get('chatgpt_account_id')
        user = auth.get('chatgpt_user_id') or claims.get('sub')
        if (auth.get('chatgpt_plan_type') not in ('pro', 'prolite') or not identity or not user
            or expiry < time.time() + 600):
            raise ValueError()
        if credentials.get('chatgpt_account_id') not in (None, '', identity):
            raise ValueError()
    except (ValueError, TypeError, IndexError, KeyError):
        raise SafeError('source_credential_not_current_paid_identity') from None
    # Imported RT is deliberately absent: existing deliveries may already own it.
    data = {'name': ('Pro20' if job.get('recharge_plan_type') == 'pro20' else 'Pro5') + '-workbench-' + digest(job['id'])[:12],
        'credentials': {'access_token': token, 'account_id': identity,
                        'client_id': credentials.get('client_id', ''), 'expires_at': expiry},
        'enabled': False, 'concurrency': 100, 'priority': -100, 'weight': 1,
        'tags': [TAG], 'notes': '18876 自动同步；仅访问令牌，源系统持有刷新凭据。'}
    return data, {'token_sha256': digest(token), 'identity_sha256': digest(identity + ':' + user),
                  'expires_at': expiry, 'plan': auth['chatgpt_plan_type']}


def run(apply=False):
    source, image = source_api()
    destination = API('http://127.0.0.1:18879', Path('/var/lib/codex-agent/client-key').read_text().strip())
    destination.call('/api/session')
    state_path = ROOT / 'state.json'
    state = json.loads(state_path.read_text()) if state_path.exists() else {'schema': 1, 'accounts': {}}
    jobs = []
    for page in range(1, 101):
        data = source.call('/api/admin/registrations?' + urllib.parse.urlencode({
            'page': page, 'page_size': 100, 'status': 'succeeded',
            'recharge_status': 'paid', 'codex_status': 'authorized'}))
        jobs.extend(x for x in data['items'] if eligible(checked_job(x)))
        if page * 100 >= data['total']:
            break
    else:
        raise SafeError('source_inventory_exceeds_bound')
    report = {'schema': 'codex-agent-workbench-bridge-v1', 'apply': apply,
        'checked_at': dt.datetime.now(dt.timezone.utc).isoformat(), 'source_image': image,
        'eligible': len(jobs), 'refresh_tokens_forwarded': 0, 'counts': collections.Counter(), 'items': []}
    group_id = state.get('group_id')
    if apply and not group_id:
        matches = [g for g in destination.call('/api/groups')['groups'] if g['name'] == GROUP]
        if len(matches) > 1:
            raise SafeError('ambiguous_bridge_group')
        group = matches[0] if matches else destination.call('/api/groups', {'name': GROUP,
            'description': '18876 Pro 访问令牌同步；网关压力验收专用分组'})
        group_id = state['group_id'] = group['id']
        atomic(state_path, state)
    # Existing mappings are examined separately so refund/deletion is not hidden by filtering.
    for job_id, mapping in list(state['accounts'].items()):
        try:
            job = checked_job(source.call('/api/admin/registrations/' + urllib.parse.quote(job_id, safe='')),
                              detail=True, expected_id=job_id)
        except SafeError as exc:
            if str(exc) != 'http_404':
                raise
            job = {}
        if eligible(job):
            continue
        account = destination.call('/api/accounts/' + mapping['account_id'])['account']
        if TAG not in account.get('tags', []) or account.get('has_refresh_token'):
            raise SafeError('destination_ownership_changed')
        if account['enabled']:
            if apply:
                destination.call('/api/accounts/' + account['id'],
                    {'version': account['version'], 'enabled': False}, 'PUT')
            report['counts']['disabled_ineligible' if apply else 'would_disable_ineligible'] += 1
    for job in jobs:
        item = {'ref': digest(job['id'])[:12]}
        try:
            job = checked_job(source.call('/api/admin/registrations/' + urllib.parse.quote(job['id'], safe='')),
                              detail=True, expected_id=job['id'])
            if not eligible(job):
                raise SafeError('source_no_longer_eligible')
            bundle = source.call('/api/admin/registrations/export-sub?' + urllib.parse.urlencode({'ids': job['id']}))
            row, facts = access_only(job, bundle)
            item.update(plan=facts['plan'], expires_at=facts['expires_at'])
            mapping = state['accounts'].get(job['id'])
            existing = None
            if mapping:
                existing = destination.call('/api/accounts/' + mapping['account_id'])['account']
                if TAG not in existing.get('tags', []) or existing.get('has_refresh_token'):
                    raise SafeError('destination_ownership_changed')
                if facts['identity_sha256'] != mapping['identity_sha256']:
                    raise SafeError('source_identity_changed')
                if facts['token_sha256'] == mapping['token_sha256'] and mapping.get('initialized'):
                    item['status'] = 'unchanged'
                    report['counts'][item['status']] += 1
                    report['items'].append(item)
                    continue
                row['name'] = existing['name']
                if facts['token_sha256'] == mapping['token_sha256']:
                    # Resume a previously imported account whose initialization was interrupted.
                    result = {'status': 'duplicate', 'id': mapping['account_id']}
                elif not apply:
                    # This is a proposed CAS write, not an executed validation of the runtime API.
                    result = {'status': 'would_reauthorize', 'id': mapping['account_id']}
                else:
                    try:
                        updated = destination.call('/api/accounts/' + urllib.parse.quote(mapping['account_id'], safe='')
                            + '/reauthorize', {'version': existing['version'],
                                'credentials': row['credentials'], 'allow_identity_change': False}, 'POST')
                    except SafeError as exc:
                        if str(exc) in ('http_404', 'http_405'):
                            raise SafeError('credential_update_requires_cas_api') from None
                        raise
                    if updated.get('id') != mapping['account_id']:
                        raise SafeError('destination_identity_mismatch')
                    result = {'status': 'reauthorized', 'id': mapping['account_id']}
            else:
                # Initial delivery never permits replacing an existing display name.
                payload = {'accounts': [row], 'replace': False, 'dry_run': not apply}
                result = destination.call('/api/accounts' if apply else '/api/accounts/import/preview', payload)['results'][0]
                if result['status'] not in ('added', 'duplicate'):
                    raise SafeError('import_' + result.get('code', 'failed'))
            if apply:
                account = destination.call('/api/accounts/' + result['id'])['account']
                # Never adopt a credential duplicate belonging to an existing manual account.
                if (account['name'] != row['name'] or TAG not in account.get('tags', [])
                    or account.get('has_refresh_token')
                    or (mapping and account['id'] != mapping['account_id'])):
                    raise SafeError('duplicate_owned_elsewhere')
                mapping = {**(mapping or {}), **facts, 'account_id': account['id']}
                state['accounts'][job['id']] = mapping
                atomic(state_path, state)
                if not mapping.get('initialized'):
                    initialization = {'version': account['version'],
                        'groups': list(dict.fromkeys(account.get('groups', []) + [group_id])),
                        'enabled': True, 'concurrency': 100, 'priority': -100}
                    if state.get('proxy_id'):
                        initialization['proxies'] = [state['proxy_id']]
                    destination.call('/api/accounts/' + account['id'], initialization, 'PUT')
                    mapping['initialized'] = True
                    atomic(state_path, state)
            item['status'] = result['status'] if apply or result['status'] == 'would_reauthorize' else 'preview_' + result['status']
        except SafeError as exc:
            item['status'] = 'failed'
            item['error_code'] = str(exc)
        report['counts'][item['status']] += 1
        report['items'].append(item)
    report['counts'] = dict(report['counts'])
    if apply:
        atomic(ROOT / 'latest.json', report)
    return report


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--apply', action='store_true')
    args = parser.parse_args()
    if args.apply:
        import fcntl
        ROOT.mkdir(mode=0o700, parents=True, exist_ok=True)
        lock = (ROOT / '.lock').open('a')
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            raise SafeError('bridge_already_running') from None
    result = run(args.apply)
    print(json.dumps(result, ensure_ascii=False))
    return 1 if result['counts'].get('failed') else 0


if __name__ == '__main__':
    try:
        sys.exit(main())
    except Exception as exc:
        # Do not expose HTTP response bodies, URLs, tokens or subprocess arguments.
        code = str(exc) if isinstance(exc, SafeError) else type(exc).__name__
        print(json.dumps({'ok': False, 'error_code': code}))
        sys.exit(1)
