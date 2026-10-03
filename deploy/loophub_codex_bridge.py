"""Private CPA-compatible ingress for one Loophub workspace.

Run: python3 loophub_codex_bridge.py --config /absolute/path/config.json
Only access tokens enter Codex Agent. Loophub retains refresh-token ownership.
DELETE disables this bridge's account and preserves its request history.
"""
import argparse
import base64
from email import policy
from email.parser import BytesParser
import hashlib
import hmac
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import math
import os
from pathlib import Path
import threading
import time
import re
import urllib.error
import urllib.parse
import urllib.request


class BridgeError(Exception):
    def __init__(self, code, status=502):
        self.code, self.status = code, status
        super().__init__(code)


def digest(value):
    return hashlib.sha256(value.encode()).hexdigest()


def unique_json_object(pairs):
    value = {}
    for name, item in pairs:
        if name in value:
            raise ValueError('duplicate_json_member')
        value[name] = item
    return value


def bridge_token_facts(token):
    """Read local owner claims; no signature/issuer verification is implied."""
    try:
        if (type(token) is not str or not 0 < len(token) <= 128 * 1024
            or not token.isascii() or any(char.isspace() for char in token)):
            raise ValueError()
        pieces = token.split('.')
        if len(pieces) != 3 or not all(pieces) or any(re.fullmatch(r'[A-Za-z0-9_-]+', part) is None for part in pieces):
            raise ValueError()
        part = pieces[1]
        raw = base64.b64decode(part + '=' * (-len(part) % 4), altchars=b'-_', validate=True)
        if len(raw) > 128 * 1024 or base64.urlsafe_b64encode(raw).decode().rstrip('=') != part:
            raise ValueError()
        claims = json.loads(raw, object_pairs_hook=unique_json_object,
                            parse_constant=lambda _: (_ for _ in ()).throw(ValueError()))
        auth = claims['https://api.openai.com/auth']
        if not isinstance(claims, dict) or not isinstance(auth, dict):
            raise ValueError()
        workspace = auth.get('chatgpt_account_id')
        user = auth.get('chatgpt_user_id')
        # Official 0.155.0 Option::or only falls back for missing/null. Empty
        # or malformed primary user IDs cannot be replaced with sub/secondary.
        if user is None:
            user = auth.get('user_id')
        if any(type(value) is not str or not 1 <= len(value) <= 512
               or any(char.isspace() or ord(char) < 32 or ord(char) == 127 for char in value)
               for value in (workspace, user)):
            raise ValueError()
        expiry = claims.get('exp')
        if type(expiry) not in (int, float) or not math.isfinite(expiry) or expiry != int(expiry) or not 0 < expiry <= 9007199254740991:
            raise ValueError()
        profile = claims.get('https://api.openai.com/profile')
        if profile is not None and not isinstance(profile, dict):
            raise ValueError()
        email = claims.get('email')
        if email is None:
            email = profile.get('email') if isinstance(profile, dict) else None
        if email is not None and (type(email) is not str or len(email) > 320 or any(ord(char) < 32 for char in email)):
            raise ValueError()
        return {'workspace': workspace, 'user': user, 'email': email.lower() if email else '',
                'expires_at': int(expiry), 'identity_sha256': digest(workspace + ':' + user),
                'token_sha256': digest(token)}
    except (ValueError, TypeError, KeyError, IndexError, AttributeError, OverflowError, RecursionError):
        raise BridgeError('token_workspace_identity_or_expiry_mismatch', 400) from None


AUTH_FAILURE_CODES = frozenset({'token_revoked', 'token_invalidated', 'token_expired',
    'access_token_expired', 'invalidated_oauth_token'})


def explicit_auth_failure(value):
    """Only classify exact codes/structured status, never arbitrary message text."""
    if isinstance(value, str):
        if value in AUTH_FAILURE_CODES:
            return value
        if value == 'upstream returned HTTP 401':
            return 'upstream_http_401'
        try:
            value = json.loads(value)
        except (ValueError, TypeError):
            return ''
    if not isinstance(value, dict):
        return ''
    statuses = [value[name] for name in ('http_code', 'http_status', 'status_code')
        if type(value.get(name)) is int and value[name] > 0]
    if any(status not in (200, 401) for status in statuses):
        return ''
    for name in ('http_code', 'http_status', 'status_code'):
        if type(value.get(name)) is int and value[name] == 401:
            return 'upstream_http_401'
    for name in ('code', 'error_code'):
        code = value.get(name)
        if isinstance(code, str) and code in AUTH_FAILURE_CODES:
            return code
    # The model's message/output/reason fields are not authentication evidence.
    if isinstance(value.get('error'), dict):
        return explicit_auth_failure(value['error'])
    return ''


def current_credential_failure(account, snapshots, now):
    """Classify the current metadata and read-only, credential-scoped snapshots."""
    expiry = account.get('expires_at')
    if type(expiry) is int and 0 < expiry <= now:
        return 'token_expired'
    credential_status = account.get('credential_status')
    if isinstance(credential_status, str) and credential_status in AUTH_FAILURE_CODES:
        return credential_status
    if credential_status in ('error', 'invalid', 'refresh_failed', 'failed') or account.get('status') == 'error':
        reason = explicit_auth_failure(account.get('last_error'))
        if reason:
            return reason
    if not isinstance(snapshots, dict) or snapshots.get('id') != account.get('id'):
        return ''
    rows = snapshots.get('snapshots')
    if not isinstance(rows, dict):
        return ''
    current = []
    for row in rows.values():
        if not isinstance(row, dict):
            continue
        checked = row.get('checked_at')
        version = row.get('credential_version')
        if (row.get('id') != account.get('id') or row.get('stale') is not False
            or type(version) is not int or version <= 0
            or type(checked) is not int or not now - 300 <= checked <= now + 5):
            continue
        current.append(row)
    if not current:
        return ''
    # A later successful observation supersedes an old rejection, including
    # different check kinds. Equal-second contradictory snapshots are inconclusive.
    latest = max(row['checked_at'] for row in current)
    latest_rows = [row for row in current if row['checked_at'] == latest]
    if any(row.get('ok') is True for row in latest_rows):
        return ''
    reasons = [explicit_auth_failure(row) for row in latest_rows]
    return next((reason for reason in reasons if reason), '')


def atomic_json(path, data):
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    temporary = path.with_name(path.name + '.tmp')
    fd = os.open(str(temporary), os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(fd, 'w', encoding='utf-8') as stream:
        json.dump(data, stream, ensure_ascii=False)
        stream.flush()
        os.fsync(stream.fileno())
    os.replace(temporary, path)
    if os.name == 'posix':
        directory = os.open(str(path.parent), os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)


class API:
    def __init__(self, base, key):
        self.base, self.key = base.rstrip('/'), key
        self.http = urllib.request.build_opener(urllib.request.ProxyHandler({}))

    def call(self, path, data=None, method=None):
        request = urllib.request.Request(self.base + path,
            headers={'Authorization': 'Bearer ' + self.key, 'Content-Type': 'application/json'},
            data=json.dumps(data).encode() if data is not None else None, method=method)
        try:
            with self.http.open(request, timeout=20) as response:
                body = response.read(4 * 1024 * 1024 + 1)
            if len(body) > 4 * 1024 * 1024:
                raise BridgeError('agent_response_too_large')
            return json.loads(body)
        except urllib.error.HTTPError as exc:
            raise BridgeError('agent_http_' + str(exc.code), 409 if exc.code == 409 else 502) from None
        except (urllib.error.URLError, TimeoutError, ValueError):
            raise BridgeError('agent_unavailable_or_invalid_response') from None


class Bridge:
    def __init__(self, config, api=None):
        self.config = config
        self.workspace = str(config['workspace_id'])
        self.group = str(config['group_id'])
        additional_groups = config.get('additional_group_ids', [])
        if (not isinstance(additional_groups, list)
            or any(not isinstance(group, str) or not group or group != group.strip()
                   for group in additional_groups)):
            raise BridgeError('invalid_additional_group_ids', 500)
        # Each workspace keeps its explicit delivery policy across child
        # rotation. Existing operator-owned memberships are merged, not replaced.
        self.required_groups = tuple(dict.fromkeys([self.group] + additional_groups))
        self.tag = 'loophub-28765-' + digest(self.workspace)[:16]
        self.state_path = Path(config['state_file'])
        self.key = Path(config['ingress_key_file']).read_text().strip()
        if len(self.key) < 32:
            raise BridgeError('ingress_key_too_short', 500)
        self.api = api or API(config.get('agent_base', 'http://127.0.0.1:18879'),
            Path(config['agent_key_file']).read_text().strip())
        self.state = (json.loads(self.state_path.read_text()) if self.state_path.exists()
            else {'schema': 1, 'workspace_id': self.workspace, 'accounts': {}})
        if self.state.get('workspace_id') != self.workspace or self.state.get('schema') != 1:
            raise BridgeError('state_workspace_mismatch', 500)
        self.lock = threading.RLock()

    def save(self):
        atomic_json(self.state_path, self.state)

    def owned(self, account, name=None, mapping=None):
        if (account.get('has_refresh_token')
            or (name is not None and account.get('name') != name)
            or (mapping is not None and account.get('id') != mapping.get('account_id'))):
            raise BridgeError('destination_ownership_changed', 409)
        if self.tag not in (account.get('tags') or []):
            # Tags are editable management metadata. Recover only an existing
            # durable mapping, never an unrelated same-name/token import.
            if (not mapping or name is None or mapping.get('name') != name
                or account.get('has_refresh_token') is not False
                or not mapping.get('identity_sha256')
                or type(account.get('version')) is not int or account['version'] <= 0):
                raise BridgeError('destination_ownership_changed', 409)
            try:
                metadata = self.api.call('/api/accounts/'
                    + urllib.parse.quote(account['id'], safe='') + '/metadata')
            except BridgeError as exc:
                # A missing auxiliary identity response does not prove that
                # the actual account was deleted. Never recreate or mark a
                # withdrawal complete on this ambiguous ownership boundary.
                if exc.code == 'agent_http_404':
                    raise BridgeError('destination_ownership_changed', 409) from None
                raise
            if not isinstance(metadata, dict) or not isinstance(metadata.get('identity'), dict):
                raise BridgeError('destination_ownership_changed', 409)
            identity = metadata['identity']
            workspace, user = identity.get('workspace_id'), identity.get('user_id')
            if (metadata.get('id') != account['id']
                or type(metadata.get('version')) is not int
                or metadata['version'] != account['version']
                or workspace != self.workspace or not isinstance(user, str) or not user
                or digest(workspace + ':' + user) != mapping['identity_sha256']):
                raise BridgeError('destination_ownership_changed', 409)
        return account

    def get_account(self, account_id, name=None, mapping=None):
        return self.owned(self.api.call('/api/accounts/'
            + urllib.parse.quote(account_id, safe=''))['account'], name, mapping)

    def credentials(self, filename, payload):
        email = str(payload.get('email') or '').strip().lower()
        if (not email or '@' not in email or filename != email + '.json'
            or '/' in filename or '\\' in filename or len(filename) > 320
            or payload.get('type') != 'codex'):
            raise BridgeError('invalid_auth_file_identity', 400)
        token = payload.get('access_token')
        parsed = bridge_token_facts(token)
        workspace, expiry = parsed['workspace'], parsed['expires_at']
        if (workspace != self.workspace or expiry < time.time() + 60
            or payload.get('account_id') != self.workspace or parsed['email'] and parsed['email'] != email):
            raise BridgeError('token_workspace_identity_or_expiry_mismatch', 400) from None
        credentials = {'access_token': token, 'account_id': workspace, 'expires_at': expiry}
        return credentials, {'identity_sha256': parsed['identity_sha256'],
            'token_sha256': digest(token), 'expires_at': expiry, 'email': email}

    def destination_token_facts(self, account, expected_identity):
        """Version-bound readback of the actual destination, never the checkpoint.

        Do not rotate/import an unchanged token just to erase an existing 401.
        The bridge checkpoint records delivery history, not authoritative storage.
        """
        if type(account.get('version')) is not int or account['version'] < 1:
            raise BridgeError('destination_credential_readback_invalid', 409)
        result = self.api.call('/api/accounts/export/credentials',
            {'items': [{'id': account['id'], 'version': account['version']}]}, 'POST')
        rows = result.get('accounts') if isinstance(result, dict) else None
        if not isinstance(rows, list) or len(rows) != 1 or not isinstance(rows[0], dict) or rows[0].get('name') != account['name']:
            raise BridgeError('destination_credential_readback_invalid', 409)
        secret = rows[0].get('credentials')
        if isinstance(secret, str):
            try:
                secret = json.loads(secret, object_pairs_hook=unique_json_object)
            except (ValueError, TypeError):
                raise BridgeError('destination_credential_readback_invalid', 409) from None
        if not isinstance(secret, dict) or secret.get('refresh_token') not in (None, ''):
            raise BridgeError('destination_ownership_changed', 409)
        try:
            facts = bridge_token_facts(secret.get('access_token'))
        except BridgeError:
            raise BridgeError('destination_credential_readback_invalid', 409) from None
        if (secret.get('account_id') != self.workspace or facts['workspace'] != self.workspace
            or facts['identity_sha256'] != expected_identity):
            raise BridgeError('destination_ownership_changed', 409)
        return facts

    def upload(self, filename, payload):
        credentials, facts = self.credentials(filename, payload)
        name = 'Loophub-28765-' + digest(self.workspace + ':' + facts['email'])[:16]
        with self.lock:
            mapping = self.state['accounts'].get(filename)
            if mapping and mapping.get('identity_sha256') != facts['identity_sha256']:
                raise BridgeError('source_identity_changed', 409)
            account = None
            if mapping:
                try:
                    account = self.get_account(mapping['account_id'], name, mapping)
                except BridgeError as exc:
                    if exc.code != 'agent_http_404':
                        raise
                    # A deleted destination must not permanently poison the
                    # source's stable filename. The incoming identity was
                    # checked against the durable receipt above; recover via
                    # the same tagged-name/import path as a first delivery.
            if account is None:
                # Recover the narrow crash window after import but before state fsync.
                try:
                    account = self.get_account(name, name)
                except BridgeError as exc:
                    if exc.code != 'agent_http_404':
                        raise
            if account is not None and mapping:
                mapping = {**mapping, 'account_id': account['id']}
            destination = self.destination_token_facts(account, facts['identity_sha256']) if account is not None else None
            if account is None:
                row = {'name': name, 'credentials': credentials, 'enabled': False,
                    'concurrency': int(self.config.get('concurrency', 80)), 'priority': -100,
                    'weight': 1, 'tags': [self.tag],
                    'notes': '28765 workspace delivery; access token only; source owns refresh token.'}
                result = self.api.call('/api/accounts', {'accounts': [row], 'replace': False})
                items = result.get('results') or []
                if len(items) != 1 or items[0].get('status') not in ('added', 'duplicate'):
                    raise BridgeError('agent_import_failed')
                account = self.get_account(items[0]['id'], name)
                # A duplicate outside this dedicated bridge is never adopted.
            elif destination['token_sha256'] != facts['token_sha256']:
                result = self.api.call('/api/accounts/' + account['id'] + '/reauthorize',
                    {'version': account['version'], 'credentials': credentials,
                     'allow_identity_change': False}, 'POST')
                if result.get('id') != account['id']:
                    raise BridgeError('agent_reauthorize_identity_mismatch')
                account = self.get_account(account['id'], name, mapping)
            delivered = self.destination_token_facts(account, facts['identity_sha256'])
            if delivered['token_sha256'] != facts['token_sha256']:
                raise BridgeError('destination_token_not_replaced', 409)
            mapping = {**(mapping or {}), **facts, 'account_id': account['id'],
                'name': name, 'withdrawn': False, 'initialized': False, 'updated_at': int(time.time()),
                'destination_token_sha256': delivered['token_sha256'], 'destination_readback_version': account['version']}
            self.state['accounts'][filename] = mapping
            self.save()
            groups = list(dict.fromkeys((account.get('groups') or []) + list(self.required_groups)))
            patch = {'version': account['version'], 'groups': groups, 'enabled': True,
                'concurrency': int(self.config.get('concurrency', 80))}
            if self.tag not in (account.get('tags') or []):
                patch['tags'] = list(dict.fromkeys((account.get('tags') or []) + [self.tag]))
            if 'proxy_ids' in self.config:
                patch['proxies'] = self.config['proxy_ids']
            self.api.call('/api/accounts/' + account['id'], patch, 'PUT')
            account = self.get_account(account['id'], name, mapping)
            delivered = self.destination_token_facts(account, facts['identity_sha256'])
            if delivered['token_sha256'] != facts['token_sha256']:
                raise BridgeError('destination_token_changed_during_delivery', 409)
            mapping['destination_readback_version'] = account['version']
            mapping['initialized'] = True
            self.save()
            return {'status': 'ok', 'file_name': filename, 'ok': True}

    def list_files(self, filename=''):
        with self.lock:
            files = []
            for name, mapping in self.state['accounts'].items():
                if mapping.get('withdrawn') or (filename and filename != name):
                    continue
                try:
                    files.append(self.file_status(name, mapping))
                except BridgeError as exc:
                    if exc.code not in ('agent_http_404', 'destination_ownership_changed'):
                        raise
                    files.append({'name': name, 'email': mapping['email'], 'type': 'codex',
                        'status': 'error', 'disabled': True, 'unavailable': True,
                        'needs_reauth': False, 'status_message': exc.code,
                        'websockets': True, 'default_websockets': True})
            return {'files': files}

    def file_status(self, name, mapping):
        account = self.get_account(mapping['account_id'], mapping['name'], mapping)
        now = time.time()
        failure = current_credential_failure(account, None, now)
        observation_unavailable = False
        if not failure:
            try:
                snapshots = self.api.call('/api/accounts/'
                    + urllib.parse.quote(account['id'], safe='') + '/upstream')
            except BridgeError:
                snapshots = None
                observation_unavailable = True
            failure = current_credential_failure(account, snapshots, now)
            if failure:
                # A replacement between the two reads must not make a
                # previous credential's rejection refresh the new AT.
                current = self.get_account(mapping['account_id'], mapping['name'], mapping)
                if current.get('version') != account.get('version'):
                    account = current
                    failure = current_credential_failure(current, None, time.time())
                    observation_unavailable = not bool(failure)
        expiry = account.get('expires_at')
        current_expiry = type(expiry) is int and expiry > time.time()
        ready = (bool(account.get('enabled')) and mapping.get('initialized')
            and current_expiry and all(group in (account.get('groups') or []) for group in self.required_groups)
            and not failure)
        # CPA's active/disabled/unavailable fields describe whether this
        # credential is delivered and enabled. Management status also
        # reflects the last inference request, including 429/capacity/
        # network failures; it is not credential validity. Reporting
        # those as unavailable makes the source re-upload (potentially
        # refreshing OAuth) without repairing the transient condition.
        # Failure to read an optional observation is likewise unknown,
        # not proof that an otherwise valid delivery needs replacement.
        # CPA monitor accepts needs_reauth directly. Return fixed codes
        # only: forwarding an upstream message could trigger its broader
        # text heuristic for an unrelated capacity/network error.
        status_message = failure or ('credential_observation_unavailable'
            if observation_unavailable else '' if ready else 'destination_unavailable')
        return {'name': name, 'email': mapping['email'], 'type': 'codex',
            'status': 'active' if ready else 'error', 'disabled': not ready,
            'needs_reauth': bool(failure), 'status_message': status_message,
            'unavailable': not ready, 'websockets': True, 'default_websockets': True}

    def withdraw(self, filename):
        with self.lock:
            mapping = self.state['accounts'].get(filename)
            if mapping and not mapping.get('withdrawn'):
                try:
                    account = self.get_account(mapping['account_id'], mapping['name'], mapping)
                except BridgeError as exc:
                    if exc.code != 'agent_http_404':
                        raise
                    account = None
                if account is not None and account.get('enabled'):
                    self.destination_token_facts(account, mapping.get('identity_sha256'))
                    self.api.call('/api/accounts/' + account['id'],
                        {'version': account['version'], 'enabled': False}, 'PUT')
                mapping['withdrawn'] = True
                mapping['updated_at'] = int(time.time())
                self.save()
            return {'ok': True, 'status': 'ok'}


def multipart_file(content_type, body):
    message = BytesParser(policy=policy.default).parsebytes(
        ('Content-Type: ' + content_type + '\r\nMIME-Version: 1.0\r\n\r\n').encode() + body)
    if not message.is_multipart():
        raise BridgeError('multipart_required', 400)
    files = [part for part in message.iter_parts()
        if part.get_param('name', header='content-disposition') == 'file']
    if len(files) != 1:
        raise BridgeError('one_auth_file_required', 400)
    try:
        value = json.loads(files[0].get_payload(decode=True), object_pairs_hook=unique_json_object,
                           parse_constant=lambda _: (_ for _ in ()).throw(ValueError()))
    except (ValueError, TypeError):
        raise BridgeError('invalid_auth_json', 400) from None
    if not isinstance(value, dict):
        raise BridgeError('invalid_auth_json', 400)
    return files[0].get_filename() or '', value


def handler_for(bridge):
    class Handler(BaseHTTPRequestHandler):
        protocol_version = 'HTTP/1.0'

        def log_message(self, *_args):
            pass  # No credentials, filenames, or source response bodies in logs.

        def setup(self):
            super().setup()
            self.connection.settimeout(30)

        def reply(self, status, value):
            body = json.dumps(value).encode()
            self.send_response(status)
            self.send_header('Content-Type', 'application/json')
            self.send_header('Cache-Control', 'no-store')
            self.send_header('Content-Length', str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def dispatch(self):
            try:
                target = urllib.parse.urlsplit(self.path)
                if self.command == 'GET' and target.path == '/health':
                    return self.reply(200, {'ok': True})
                bearer = self.headers.get('Authorization', '')
                key = bearer[7:] if bearer.startswith('Bearer ') else self.headers.get('X-Management-Key', '')
                if not hmac.compare_digest(key, bridge.key):
                    raise BridgeError('unauthorized', 401)
                if target.path != '/v0/management/auth-files':
                    raise BridgeError('not_found', 404)
                filename = urllib.parse.parse_qs(target.query).get('name', [''])[0]
                if self.command == 'GET':
                    return self.reply(200, bridge.list_files(filename))
                if self.command == 'DELETE':
                    if not filename:
                        raise BridgeError('filename_required', 400)
                    return self.reply(200, bridge.withdraw(filename))
                if self.command == 'POST':
                    size = int(self.headers.get('Content-Length', '0'))
                    if self.headers.get('Transfer-Encoding') or not 0 < size <= 256 * 1024:
                        raise BridgeError('invalid_body_size', 413)
                    body = self.rfile.read(size)
                    if len(body) != size:
                        raise BridgeError('incomplete_body', 400)
                    name, payload = multipart_file(self.headers.get('Content-Type', ''), body)
                    return self.reply(200, bridge.upload(name, payload))
                raise BridgeError('method_not_allowed', 405)
            except BridgeError as exc:
                self.reply(exc.status, {'ok': False, 'error': exc.code})
            except (ValueError, TypeError):
                self.reply(400, {'ok': False, 'error': 'invalid_request'})
            except Exception:
                self.reply(500, {'ok': False, 'error': 'bridge_internal_error'})

        do_GET = dispatch
        do_POST = dispatch
        do_DELETE = dispatch
    return Handler


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--config', required=True, type=Path)
    args = parser.parse_args()
    config = json.loads(args.config.read_text())
    bridge = Bridge(config)
    server = ThreadingHTTPServer((config.get('listen_host', '127.0.0.1'),
        int(config.get('listen_port', 18881))), handler_for(bridge))
    server.daemon_threads = True
    server.serve_forever()


if __name__ == '__main__':
    main()
