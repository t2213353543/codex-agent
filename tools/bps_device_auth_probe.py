"""Independent BPS device-auth diagnostic. Defaults are offline; no account writes.

--plan: show the contract extracted from the captured official bundle.
--prepare: exercise the extracted contract against a local HTTP fixture.
--start/--poll/--baseline: explicit server-only actions, for use after review.
Tokens never leave the server; baseline uses the existing Go transport probe once.
"""
from __future__ import annotations

import argparse
import base64
from datetime import datetime, timezone
import hashlib
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import math
import os
from pathlib import Path
import re
import secrets
import shlex
import stat
import subprocess
import sys
import tempfile
import threading
import time
import urllib.error
import urllib.parse
import urllib.request

ROOT = Path(__file__).resolve().parents[1]
REMOTE_ROOT = '/opt/codex-agent-integration/checks'
AUTH = 'https://auth.openai.com'
BPS = 'https://bps.openai.com/basispoints/api'
KNOWN_DEVICE_ERRORS = {'deviceauth_authorization_pending', 'deviceauth_user_code_expired',
                       'deviceauth_authorization_failed', 'deviceauth_invalid_state',
                       'deviceauth_authorization_unknown'}


def digest(value):
    return hashlib.sha256(value if isinstance(value, bytes) else value.encode()).hexdigest()


def contract():
    bundle = ROOT / '.local/office-proxy-verification/powerpoint/x-square-DUrhLSGN.js'
    raw = bundle.read_bytes()
    source = raw.decode()
    client_raw = bundle.with_name('client-info-BZSGgJF-.js').read_bytes()
    index = json.loads((ROOT / '.local/latest-operations/bps-current-public-bundle.json').read_text())
    assert digest(raw) == next(x['sha256'] for x in index['artifacts'] if x['file'] == bundle.name)
    assert digest(client_raw) == next(x['sha256'] for x in index['artifacts'] if x['file'] == 'client-info-BZSGgJF-.js')
    client = re.search(r'a6=ay==="staging"\?"[^"]+":"([^"]+)"', source).group(1)
    expected = ['efe("usercode",{client_id:a6}',
                'efe("token",{client_id:a6,device_auth_id:r.device_auth_id,user_code:r.user_code}',
                'grant_type:"authorization_code",code:e,client_id:a6,redirect_uri:r,code_verifier:t',
                'GJt=`${sy}/basispoints/deviceauth/callback`',
                '`${sy}/oauth/token?unified=true`']
    assert all(x in source for x in expected), 'Captured official contract changed'
    html = bundle.with_name('index.html').read_text(encoding='utf-8')
    auth_base = re.search(r'<meta name="bps-auth-base-path" content="([^"]+)"', html).group(1)
    scope = re.search(r'oMe="([^"]+)"', source).group(1)
    audience = re.search(r'iMe="([^"]+)"', source).group(1)
    return {'client_id': client, 'issuer': AUTH,
            'usercode_url': AUTH + '/api/accounts/deviceauth/usercode',
            'poll_url': AUTH + '/api/accounts/deviceauth/token',
            'exchange_url': AUTH + '/oauth/token?unified=true',
            'verification_url': AUTH + '/api/accounts/deviceauth/authorize?' + urllib.parse.urlencode({'client_id': client}),
            'redirect_uri': AUTH + '/basispoints/deviceauth/callback',
            'browser_authorization': {'url': AUTH + '/api/accounts/authorize',
                'scope': scope, 'audience': audience, 'response_type': 'code',
                'code_challenge_method': 'S256', 'state_required': True,
                'redirect_uri_from_captured_html': 'https://bps.openai.com' + auth_base + 'auth/callback',
                'redirect_note': 'Device flow uses the separate fixed device callback; do not substitute Codex localhost callback'},
            'bundle_sha256': digest(raw),
            'source_offsets': {x: source.index(x) for x in expected},
            'tools_version': re.search(r'tools-powerpoint-core-\d{4}-\d{2}-\d{2}-[a-f0-9]+', client_raw.decode()).group(0),
            'credential_isolation': 'separate server directory; no managed-account update or refresh',
            'baseline_transport': 'existing Go diagnostic, mode=egress_once, one request',
            'recovery_claim': False}


class DiagnosticError(Exception):
    def __init__(self, code, status=None):
        self.code, self.status = code, status
        super().__init__(code)


class HTTP:
    def __init__(self, origin=None):
        self.origin = origin
        class NoRedirect(urllib.request.HTTPRedirectHandler):
            def redirect_request(self, *args, **kwargs):
                return None
        self.opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())

    def post(self, url, data, form=False, timeout=15):
        parsed = urllib.parse.urlsplit(url)
        if self.origin:
            url = self.origin + parsed.path + ('?' + parsed.query if parsed.query else '')
        elif parsed.scheme != 'https' or parsed.netloc != 'auth.openai.com':
            raise DiagnosticError('unexpected_auth_endpoint')
        payload = urllib.parse.urlencode(data).encode() if form else json.dumps(data).encode()
        request = urllib.request.Request(url, data=payload, headers={
            'Content-Type': 'application/x-www-form-urlencoded' if form else 'application/json'})
        try:
            response = self.opener.open(request, timeout=timeout)
        except urllib.error.HTTPError as error:
            response = error
        except Exception:
            raise DiagnosticError('auth_transport_error') from None
        with response:
            status, raw = response.status, response.read()
        try:
            value = json.loads(raw)
        except ValueError:
            raise DiagnosticError('auth_non_json_response', status) from None
        if not isinstance(value, dict):
            raise DiagnosticError('auth_invalid_response', status)
        if not 200 <= status < 300:
            error = value.get('error')
            code = error.get('code') if isinstance(error, dict) else None
            raise DiagnosticError(code if code in KNOWN_DEVICE_ERRORS else 'auth_http_error', status)
        return value


def private_write(path, value):
    path = Path(path)
    temp = path.with_name(path.name + '.' + secrets.token_hex(8))
    fd = os.open(temp, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    try:
        with os.fdopen(fd, 'w', encoding='utf-8') as out:
            json.dump(value, out, separators=(',', ':'))
            out.flush()
            os.fsync(out.fileno())
        os.replace(temp, path)
        os.chmod(path, 0o600)
    finally:
        if temp.exists():
            temp.unlink()


def parse_expiry(text):
    try:
        value = datetime.fromisoformat(text.replace('Z', '+00:00'))
        if value.tzinfo is None:
            raise ValueError()
        return value.timestamp()
    except (ValueError, TypeError, AttributeError):
        raise DiagnosticError('device_expiry_invalid') from None


def start_session(c, http, now=time.time):
    started = now()
    value = http.post(c['usercode_url'], {'client_id': c['client_id']})
    device, code, interval = value.get('device_auth_id'), value.get('user_code'), value.get('interval')
    if not isinstance(device, str) or not device or not isinstance(code, str) or not re.fullmatch(r'[A-Z0-9]{4}-[A-Z0-9]{5}', code):
        raise DiagnosticError('device_code_invalid')
    try:
        interval = float(interval)
    except (TypeError, ValueError):
        raise DiagnosticError('device_interval_invalid') from None
    if not math.isfinite(interval) or interval <= 0:
        raise DiagnosticError('device_interval_invalid')
    deadline = min(parse_expiry(value.get('expires_at')), started + 15 * 60)
    if deadline <= now():
        raise DiagnosticError('device_expired')
    interval = max(5, interval)
    return {'device_auth_id': device, 'user_code': code, 'deadline': deadline,
            'interval': interval, 'next_poll_at': now() + interval, 'status': 'pending'}


def poll_session(c, state, http, now=time.time):
    if state['status'] != 'pending':
        return {'status': state['status']}, None
    if now() >= state['deadline']:
        state['status'] = 'expired'
        return {'status': 'expired'}, None
    if now() < state['next_poll_at']:
        return {'status': 'pending', 'retry_after_seconds': math.ceil(state['next_poll_at'] - now())}, None
    try:
        value = http.post(c['poll_url'], {'client_id': c['client_id'],
            'device_auth_id': state['device_auth_id'], 'user_code': state['user_code']},
            timeout=min(15, state['deadline'] - now()))
    except DiagnosticError as error:
        if error.code == 'deviceauth_authorization_pending':
            state['next_poll_at'] = now() + state['interval']
            return {'status': 'pending', 'retry_after_seconds': state['interval']}, None
        if error.status == 429 or (error.status is not None and error.status >= 500) or error.code == 'auth_transport_error':
            backoff = max(state['interval'], min(30, state.get('backoff', state['interval']) * 2))
            state.update(backoff=backoff, next_poll_at=now() + backoff)
            return {'status': 'pending', 'retry_after_seconds': backoff, 'transient_error': error.code}, None
        state['status'] = 'expired' if error.code == 'deviceauth_user_code_expired' else 'failed'
        raise
    code, verifier = value.get('authorization_code'), value.get('code_verifier')
    if not isinstance(code, str) or not code or not isinstance(verifier, str) or not re.fullmatch(r'[A-Za-z0-9_-]{43}', verifier):
        state['status'] = 'failed'
        raise DiagnosticError('device_authorization_result_invalid')
    if now() >= state['deadline']:
        state['status'] = 'expired'
        return {'status': 'expired'}, None
    # The official device endpoint supplies the PKCE verifier. Never invent or
    # substitute a verifier, and never refresh an imported account credential.
    try:
        tokens = http.post(c['exchange_url'], {'grant_type': 'authorization_code', 'code': code,
            'client_id': c['client_id'], 'redirect_uri': c['redirect_uri'], 'code_verifier': verifier},
            form=True, timeout=min(15, state['deadline'] - now()))
    except DiagnosticError:
        state['status'] = 'failed'
        raise
    if not isinstance(tokens.get('access_token'), str) or not tokens['access_token'].strip():
        state['status'] = 'failed'
        raise DiagnosticError('exchange_missing_access_token')
    state['status'] = 'authorized'
    return {'status': 'authorized'}, tokens


def token_metadata(tokens):
    token = tokens['access_token']
    try:
        body = token.split('.')[1]
        claims = json.loads(base64.urlsafe_b64decode(body + '=' * (-len(body) % 4)))
        auth = claims.get('https://api.openai.com/auth') or claims.get('auth') or {}
    except (ValueError, IndexError, TypeError):
        raise DiagnosticError('access_token_claims_invalid') from None
    account = auth.get('chatgpt_account_id')
    if not isinstance(account, str) or not account or '\r' in account or '\n' in account:
        raise DiagnosticError('access_token_account_missing')
    return {'access_token_sha256': digest(token), 'account_claim_sha256': digest(account),
            'client_id': claims.get('client_id') or claims.get('azp'), 'issuer': claims.get('iss'),
            'audience': claims.get('aud'), 'scopes': claims.get('scp') or claims.get('scope'),
            'iat': claims.get('iat'), 'exp': claims.get('exp')}, account


def server_dir(session):
    if os.name != 'posix' or not re.fullmatch('[a-f0-9]{32}', session):
        raise DiagnosticError('server_session_invalid')
    folder = Path(REMOTE_ROOT) / ('bps-device-auth-' + session)
    if folder.parent.resolve() != Path(REMOTE_ROOT).resolve() or folder.is_symlink():
        raise DiagnosticError('server_session_path_invalid')
    return folder


def clear_diagnostic_credentials(folder, state):
    """Clear only this tool's isolated diagnostic files, never managed accounts."""
    (folder / 'credentials.json').unlink(missing_ok=True)
    state.pop('device_auth_id', None)
    state.pop('user_code', None)
    state['status'] = 'baseline_attempted_credentials_cleared'
    private_write(folder / 'state.json', state)


def worker(action, value):
    c = value['contract']
    if c['client_id'] != 'app_fnr0pYvVwwFDocDumLG3H2Bp' or c['issuer'] != AUTH or c['redirect_uri'] != AUTH + '/basispoints/deviceauth/callback':
        raise DiagnosticError('unreviewed_auth_contract')
    folder = server_dir(value['session'])
    if action == 'start':
        folder.mkdir(mode=0o700)
        state = start_session(c, HTTP())
        private_write(folder / 'state.json', state)
        private_write(folder / 'contract.json', c)
        return {'status': 'pending', 'session': value['session'], 'user_code': state['user_code'],
                'verification_url': c['verification_url'], 'expires_at': state['deadline'],
                'retry_after_seconds': state['interval'], 'managed_accounts_modified': False}
    import fcntl
    with (folder / 'lock').open('a') as lock:
        os.chmod(folder / 'lock', 0o600)
        fcntl.flock(lock, fcntl.LOCK_EX)
        state = json.loads((folder / 'state.json').read_text())
        if c != json.loads((folder / 'contract.json').read_text()):
            raise DiagnosticError('auth_contract_changed')
        if action == 'poll':
            try:
                result, tokens = poll_session(c, state, HTTP())
                if tokens:
                    meta, _ = token_metadata(tokens)
                    # The full newly-issued token response stays in this private
                    # directory. It is never imported into production storage.
                    private_write(folder / 'credentials.json', tokens)
                    result['token_metadata'] = meta
                result['managed_accounts_modified'] = False
                return result
            finally:
                private_write(folder / 'state.json', state)
        if action != 'baseline' or state['status'] != 'authorized':
            raise DiagnosticError('authorization_required')
        if state.get('baseline_attempted'):
            raise DiagnosticError('single_baseline_already_attempted')
        tokens = json.loads((folder / 'credentials.json').read_text())
        meta, account = token_metadata(tokens)
        if meta.get('client_id') != c['client_id']:
            raise DiagnosticError('authorized_client_id_mismatch')
        if not isinstance(meta.get('exp'), (int, float)) or meta['exp'] <= time.time():
            raise DiagnosticError('authorized_token_expired')
        body = base64.b64decode(value['body_base64'], validate=True)
        json.loads(body)
        binary = Path(value['probe_binary'])
        if binary.parent.resolve() != Path(REMOTE_ROOT).resolve() or not binary.name.startswith('bps-device-baseline-'):
            raise DiagnosticError('baseline_binary_invalid')
        if digest(binary.read_bytes()) != value['probe_sha256']:
            raise DiagnosticError('baseline_binary_changed')
        state['baseline_attempted'] = True
        private_write(folder / 'state.json', state)
        fixture = {'accounts': [{'alias': 'native-bps-login', 'access_token': tokens['access_token'],
                    'account_id': account, 'credential_epoch': 'independent-device-login'}],
                   'tools_version': c['tools_version'], 'mode': 'egress_once', 'body_base64': value['body_base64']}
        try:
            process = subprocess.run([str(binary)], input=json.dumps(fixture).encode(), capture_output=True,
                                     timeout=200, env={'PATH': '/usr/bin:/bin'})
        finally:
            # This login exists only for one diagnostic. Do not retain a new
            # refresh token or access token after any attempted baseline.
            clear_diagnostic_credentials(folder, state)
        if process.returncode:
            raise DiagnosticError('baseline_process_failed')
        result = json.loads(process.stdout)
        encoded = json.dumps(result)
        if any(isinstance(secret, str) and secret and secret in encoded for secret in
               [tokens.get('access_token'), tokens.get('refresh_token'), tokens.get('id_token'), account]):
            raise DiagnosticError('baseline_output_redacted')
        result.update(token_metadata=meta, managed_accounts_modified=False, single_baseline=True,
                      credentials_cleared=True)
        private_write(folder / 'baseline-result.json', result)
        return result


def prepare(c):
    calls, responses = [], []
    class Handler(BaseHTTPRequestHandler):
        def log_message(self, *args):
            pass
        def do_POST(self):
            raw = self.rfile.read(int(self.headers['Content-Length']))
            form = self.headers['Content-Type'] == 'application/x-www-form-urlencoded'
            data = dict(urllib.parse.parse_qsl(raw.decode())) if form else json.loads(raw)
            calls.append({'path': self.path, 'form': form, 'data': data})
            status, value = responses.pop(0)
            body = json.dumps(value).encode()
            self.send_response(status)
            self.send_header('Content-Type', 'application/json')
            self.send_header('Content-Length', str(len(body)))
            self.end_headers()
            self.wfile.write(body)
    server = ThreadingHTTPServer(('127.0.0.1', 0), Handler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    http = HTTP('http://127.0.0.1:' + str(server.server_port))
    now = [1800000000.0]
    checks = []
    try:
        responses.append((200, {'device_auth_id': 'SYNTHETIC_PRIVATE_DEVICE', 'user_code': 'ABCD-EFG12',
            'interval': 1, 'expires_at': datetime.fromtimestamp(now[0] + 2000, timezone.utc).isoformat()}))
        state = start_session(c, http, lambda: now[0])
        assert calls[-1]['data'] == {'client_id': c['client_id']}
        assert state['interval'] == 5 and state['deadline'] == now[0] + 900
        checks.append('official_usercode_fields_and_expiry')
        before = len(calls)
        result, _ = poll_session(c, state, http, lambda: now[0])
        assert result['status'] == 'pending' and len(calls) == before
        checks.append('no_early_poll')
        now[0] += 5
        responses.append((400, {'error': {'code': 'deviceauth_authorization_pending'}}))
        result, _ = poll_session(c, state, http, lambda: now[0])
        assert result['status'] == 'pending' and set(calls[-1]['data']) == {'client_id', 'device_auth_id', 'user_code'}
        checks.append('pending_error_and_poll_fields')
        now[0] += 5
        responses.append((503, {'error': {'message': 'SYNTHETIC_PRIVATE_UPSTREAM_ERROR'}}))
        result, _ = poll_session(c, state, http, lambda: now[0])
        assert result['status'] == 'pending' and state['next_poll_at'] == now[0] + 10
        assert 'SYNTHETIC_PRIVATE' not in json.dumps(result)
        checks.append('transient_error_backoff_without_secret')
        now[0] += 10
        verifier = 'v' * 43
        responses.extend([(200, {'authorization_code': 'SYNTHETIC_PRIVATE_CODE', 'code_verifier': verifier}),
                          (200, {'access_token': 'SYNTHETIC_PRIVATE_AT', 'refresh_token': 'SYNTHETIC_PRIVATE_RT'})])
        result, tokens = poll_session(c, state, http, lambda: now[0])
        assert result == {'status': 'authorized'}
        assert calls[-1] == {'path': '/oauth/token?unified=true', 'form': True, 'data': {
            'grant_type': 'authorization_code', 'code': 'SYNTHETIC_PRIVATE_CODE',
            'client_id': c['client_id'], 'redirect_uri': c['redirect_uri'], 'code_verifier': verifier}}
        checks.append('exact_pkce_verifier_code_exchange')
        assert 'SYNTHETIC_PRIVATE' not in json.dumps(result)
        checks.append('auth_result_does_not_disclose_tokens')
        before = len(calls)
        result, repeated = poll_session(c, state, http, lambda: now[0])
        assert result['status'] == 'authorized' and repeated is None and len(calls) == before
        checks.append('authorized_session_cannot_exchange_twice')
        state.update(status='pending', deadline=now[0] - 1)
        before = len(calls)
        result, _ = poll_session(c, state, http, lambda: now[0])
        assert result['status'] == 'expired' and len(calls) == before
        checks.append('expired_never_polls_or_exchanges')
        state.update(status='pending', deadline=now[0] + 50, next_poll_at=now[0])
        responses.append((200, {'authorization_code': 'SYNTHETIC_PRIVATE_CODE', 'code_verifier': 'bad'}))
        try:
            poll_session(c, state, http, lambda: now[0])
            raise AssertionError('bad verifier accepted')
        except DiagnosticError as error:
            assert error.code == 'device_authorization_result_invalid'
        checks.append('invalid_pkce_verifier_rejected')
        state.update(status='pending', next_poll_at=now[0])
        responses.append((403, {'error': {'code': 'SYNTHETIC_PRIVATE_AT', 'message': 'SYNTHETIC_PRIVATE_RT'}}))
        try:
            poll_session(c, state, http, lambda: now[0])
            raise AssertionError('HTTP error accepted')
        except DiagnosticError as error:
            assert error.code == 'auth_http_error' and error.status == 403
            assert 'SYNTHETIC_PRIVATE' not in str(error)
        checks.append('upstream_error_secrets_not_reflected')
        with tempfile.TemporaryDirectory() as temp:
            file = Path(temp) / 'credentials.json'
            private_write(file, tokens)
            assert json.loads(file.read_text()) == tokens
            if os.name == 'posix':
                assert stat.S_IMODE(file.stat().st_mode) == 0o600
            unrelated = Path(temp) / 'managed-account-sentinel.json'
            unrelated.write_text('{"unchanged":true}', encoding='utf-8')
            clear_diagnostic_credentials(Path(temp), state)
            assert not file.exists() and 'device_auth_id' not in state and 'user_code' not in state
            assert unrelated.read_text() == '{"unchanged":true}'
        checks.append('atomic_private_credential_write')
        checks.append('only_isolated_diagnostic_credentials_cleared')
    finally:
        server.shutdown()
        server.server_close()
        thread.join()
    result = {'passed': True, 'checks': checks, 'http_requests': len(calls),
              'external_requests': 0, 'real_device_code_requested': False,
              'posix_0600_runtime_checked': os.name == 'posix', 'contract': c}
    output = ROOT / '.local/latest-operations/bps-device-auth-prepare.json'
    output.write_text(json.dumps(result, indent=2) + '\n', encoding='utf-8')
    return result


def remote(action, c, session, body_file):
    from integration_env import connect
    value = {'contract': c, 'session': session}
    client = connect()
    try:
        if action == 'baseline':
            if not body_file:
                raise DiagnosticError('explicit_synthetic_body_file_required')
            body = Path(body_file).read_bytes()
            json.loads(body)
            output = ROOT / '.local/latest-operations/bps-device-auth-build'
            output.mkdir(exist_ok=True)
            binary = output / 'probe'
            subprocess.run(['go', 'build', '-trimpath', '-ldflags=-s -w', '-o', str(binary),
                            'tools/bps_minimal_policy_probe.go'], cwd=ROOT,
                           env={**os.environ, 'GOOS': 'linux', 'GOARCH': 'amd64', 'CGO_ENABLED': '0'},
                           check=True, capture_output=True)
            sha = digest(binary.read_bytes())
            target = REMOTE_ROOT + '/bps-device-baseline-' + sha
            with client.open_sftp() as sftp:
                sftp.put(str(binary), target)
                sftp.chmod(target, 0o700)
            value.update(probe_binary=target, probe_sha256=sha, body_base64=base64.b64encode(body).decode())
        script = Path(__file__).read_bytes()
        script_sha = digest(script)
        target = REMOTE_ROOT + '/bps-device-auth-worker-' + script_sha + '.py'
        with client.open_sftp() as sftp:
            with sftp.open(target, 'wb') as out:
                out.write(script)
            sftp.chmod(target, 0o700)
        stdin, stdout, stderr = client.exec_command('python3 -B ' + shlex.quote(target) + ' --worker ' + action, timeout=240)
        stdin.write(json.dumps(value))
        stdin.channel.shutdown_write()
        data = stdout.read()
        error_bytes = len(stderr.read())
        code = stdout.channel.recv_exit_status()
        if error_bytes or not data:
            raise DiagnosticError('remote_worker_failed')
        result = json.loads(data)
        if code:
            raise DiagnosticError(result.get('error', 'remote_worker_failed'), result.get('http_status'))
        return result
    finally:
        client.close()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    group = parser.add_mutually_exclusive_group()
    for name in ['plan', 'prepare', 'start', 'poll', 'baseline']:
        group.add_argument('--' + name, action='store_true')
    group.add_argument('--worker', choices=['start', 'poll', 'baseline'], help=argparse.SUPPRESS)
    parser.add_argument('--session')
    parser.add_argument('--body-file', help='Explicit synthetic body for the single Go baseline')
    args = parser.parse_args()
    if args.worker:
        result = worker(args.worker, json.load(sys.stdin))
    else:
        c = contract()
        if args.prepare:
            result = prepare(c)
        elif args.start or args.poll or args.baseline:
            session = secrets.token_hex(16) if args.start else args.session
            if not session or not re.fullmatch('[a-f0-9]{32}', session):
                raise DiagnosticError('session_required')
            result = remote('start' if args.start else 'poll' if args.poll else 'baseline', c, session, args.body_file)
        else:
            result = {'mode': 'offline_plan', 'contract': c, 'external_requests': 0,
                      'next': '--prepare; review before --start; then --poll --session SESSION; baseline requires --body-file'}
    print(json.dumps(result, ensure_ascii=False))


if __name__ == '__main__':
    try:
        main()
    except DiagnosticError as error:
        print(json.dumps({'error': error.code, 'http_status': error.status, 'generation_recovery_is_not_implied': True}))
        raise SystemExit(1)
    except Exception as error:
        # OAuth exceptions and subprocess output can contain authorization data.
        print(json.dumps({'error': 'diagnostic_internal_failure', 'exception_type': type(error).__name__}))
        raise SystemExit(1)
