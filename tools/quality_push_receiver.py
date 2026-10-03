"""Private, temporary CPA receiver for existing 28765 child credentials.

Run entirely on the gateway host. Credential material never appears on stdout.
Lifecycle: prepare -> serve (separate process) -> configure -> snapshot -> restore.
This tool never logs in, refreshes OAuth, buys seats, or changes rotation settings.
Binding a destination wakes the existing 28765 scheduler; observe its activity.
"""
from __future__ import annotations

import argparse
import base64
from contextlib import contextmanager
from email import policy
from email.parser import BytesParser
import hashlib
import hmac
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import ipaddress
import json
import os
from pathlib import Path
import re
import secrets
import stat
import subprocess
import sys
import tempfile
import threading
import time
import urllib.error
import urllib.parse
import urllib.request


class ReceiverError(Exception):
    def __init__(self, code: str, status: int = 409):
        super().__init__(code)
        self.code, self.status = code, status


def sha(value: str) -> str:
    return hashlib.sha256(value.encode()).hexdigest()


def private_directory(path: Path) -> None:
    if path.is_symlink():
        raise ReceiverError('state_directory_symlink')
    path.mkdir(parents=True, exist_ok=True, mode=0o700)
    if os.name == 'posix':
        os.chmod(path, 0o700)


def write_private(path: Path, value) -> None:
    private_directory(path.parent)
    if path.is_symlink():
        raise ReceiverError('state_file_symlink')
    fd, temporary = tempfile.mkstemp(prefix='.' + path.name + '-', dir=path.parent)
    try:
        with os.fdopen(fd, 'w', encoding='utf-8') as stream:
            json.dump(value, stream, ensure_ascii=False, separators=(',', ':'))
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temporary, path)
        if os.name == 'posix':
            directory = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY)
            try:
                os.fsync(directory)
            finally:
                os.close(directory)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


def read_private(path: Path):
    if path.is_symlink():
        raise ReceiverError('state_file_symlink')
    if os.name == 'posix' and stat.S_IMODE(path.stat().st_mode) & 0o077:
        raise ReceiverError('state_file_permissions')
    return json.loads(path.read_text(encoding='utf-8'))


@contextmanager
def lifecycle_lock(root: Path):
    private_directory(root)
    path = root / 'lifecycle.lock'
    if path.is_symlink():
        raise ReceiverError('state_file_symlink')
    fd = os.open(path, os.O_RDWR | os.O_CREAT, 0o600)
    stream = os.fdopen(fd, 'r+b')
    try:
        if os.name == 'posix':
            import fcntl
            fcntl.flock(stream, fcntl.LOCK_EX | fcntl.LOCK_NB)
        else:
            import msvcrt
            stream.write(b'0')
            stream.flush()
            stream.seek(0)
            msvcrt.locking(stream.fileno(), msvcrt.LK_NBLCK, 1)
        yield
    finally:
        stream.close()


@contextmanager
def credential_lock(root: Path):
    """Serialize the receiver and restore processes across their final writes."""
    path = root / 'credentials.lock'
    if path.is_symlink():
        raise ReceiverError('state_file_symlink')
    fd = os.open(path, os.O_RDWR | os.O_CREAT, 0o600)
    stream = os.fdopen(fd, 'r+b')
    try:
        if os.name == 'posix':
            import fcntl
            fcntl.flock(stream, fcntl.LOCK_EX)
        else:
            import msvcrt
            stream.write(b'0')
            stream.flush()
            stream.seek(0)
            msvcrt.locking(stream.fileno(), msvcrt.LK_LOCK, 1)
        yield
    finally:
        stream.close()


def private_ipv4(value: str) -> bool:
    try:
        ip = ipaddress.ip_address(value)
        return ip.version == 4 and any(ip in ipaddress.ip_network(cidr) for cidr in (
            '10.0.0.0/8', '172.16.0.0/12', '192.168.0.0/16'))
    except ValueError:
        return False


def management_peer_allowed(value: str, allowed_peer_ips=()) -> bool:
    """Private Docker clients and authenticated local readiness checks."""
    try:
        address = ipaddress.ip_address(value)
        return private_ipv4(value) or address.is_loopback or str(address) in allowed_peer_ips
    except ValueError:
        return False


def load_config(path: Path) -> dict:
    config = json.loads(path.read_text(encoding='utf-8'))
    if not isinstance(config, dict) or config.get('schema', 1) != 1:
        raise ReceiverError('invalid_config')
    if not re.fullmatch(r'[a-z0-9][a-z0-9-]{5,63}', str(config.get('run_id', ''))):
        raise ReceiverError('invalid_run_id')
    if not private_ipv4(str(config.get('listen_host', ''))):
        raise ReceiverError('listen_host_must_be_explicit_rfc1918_ipv4')
    port = config.get('listen_port', 18882)
    if type(port) is not int or not 1024 <= port <= 65535:
        raise ReceiverError('invalid_listen_port')
    config['listen_port'] = port
    peers = config.get('allowed_peer_ips', [])
    if not isinstance(peers, list) or len(peers) > 4:
        raise ReceiverError('invalid_allowed_peer_ips')
    normalized_peers = []
    for value in peers:
        try:
            if not isinstance(value, str) or '%' in value:
                raise ValueError()
            address = ipaddress.ip_address(value)
            if address.is_unspecified or address.is_multicast:
                raise ValueError()
        except ValueError:
            raise ReceiverError('allowed_peer_ips_require_exact_unicast_ips', 400) from None
        normalized_peers.append(str(address))
    if len(set(normalized_peers)) != len(normalized_peers):
        raise ReceiverError('duplicate_allowed_peer_ips')
    config['allowed_peer_ips'] = sorted(normalized_peers)
    root = Path(config.get('state_dir', ''))
    if not root.is_absolute() or root == Path(root.anchor) or root.is_symlink():
        raise ReceiverError('state_dir_must_be_dedicated_absolute_path')
    config['state_dir'] = str(root)
    owners = config.get('owners')
    if not isinstance(owners, list) or not 1 <= len(owners) <= 12:
        raise ReceiverError('owners_allowlist_required')
    for field in ('owner_id', 'workspace_id', 'alias'):
        values = [row.get(field) for row in owners if isinstance(row, dict)]
        if len(values) != len(owners) or any(not isinstance(v, str) or not v for v in values):
            raise ReceiverError('invalid_owner_allowlist')
        if len(set(values)) != len(values):
            raise ReceiverError('duplicate_owner_allowlist_field')
    if any(not re.fullmatch(r'[a-z][a-z0-9_-]{0,39}', row['alias']) for row in owners):
        raise ReceiverError('invalid_alias')
    for row in owners:
        for field in ('owner_id', 'workspace_id'):
            if not re.fullmatch(r'[a-zA-Z0-9_-]{1,100}', row[field]):
                raise ReceiverError('invalid_owner_or_workspace_id')
    container = config.get('source_container', 'loophub-multitenant-app')
    if not re.fullmatch(r'[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}', container):
        raise ReceiverError('invalid_source_container')
    config['source_container'] = container
    return config


def config_scope(config: dict) -> dict:
    result = {key: config[key] for key in (
        'run_id', 'state_dir', 'listen_host', 'listen_port', 'owners', 'source_container')}
    # Existing prepared runs omitted this optional field. Empty defaults keep
    # their scope stable; adding an explicit peer requires a fresh prepared run.
    if config.get('allowed_peer_ips'):
        result['allowed_peer_ips'] = list(config['allowed_peer_ips'])
    return result


def base_url(config: dict) -> str:
    return f"http://{config['listen_host']}:{config['listen_port']}"


def jwt_identity(token: str) -> dict:
    try:
        if not isinstance(token, str) or len(token) > 65536:
            raise ValueError()
        pieces = token.split('.')
        if len(pieces) != 3:
            raise ValueError()
        claims = json.loads(base64.urlsafe_b64decode(pieces[1] + '=' * (-len(pieces[1]) % 4)))
        auth = claims['https://api.openai.com/auth']
        profile = claims.get('https://api.openai.com/profile') or {}
        user = auth.get('chatgpt_user_id') or claims.get('sub')
        workspace = auth['chatgpt_account_id']
        exp, iat = claims['exp'], claims.get('iat', 0)
        if (type(exp) is not int or type(iat) is not int or exp <= 0 or iat < 0
                or not isinstance(workspace, str) or not workspace
                or not isinstance(user, str) or not user):
            raise ValueError()
        return {'workspace_id': workspace, 'user_id': user, 'expires_at': exp,
                'issued_at': iat, 'email': str(profile.get('email') or claims.get('email') or '').lower(),
                'plan': str(auth.get('chatgpt_plan_type') or '').lower(),
                'identity_sha256': sha(workspace + ':' + user)}
    except (ValueError, KeyError, TypeError, AttributeError, UnicodeError):
        raise ReceiverError('invalid_token_claims', 400) from None


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *_args, **_kwargs):
        raise ReceiverError('management_redirect_refused', 502)


class Source:
    def __init__(self, config: dict):
        try:
            inspect = json.loads(subprocess.check_output(
                ['docker', 'inspect', config['source_container']], timeout=15,
                stderr=subprocess.DEVNULL))[0]
            if not inspect['State']['Running']:
                raise ReceiverError('source_not_running')
            env = dict(item.split('=', 1) for item in inspect['Config']['Env'] if '=' in item)
            self.key = env['ADMIN_API_KEY']
            if not self.key:
                raise ReceiverError('source_admin_key_missing')
            networks = inspect['NetworkSettings']['Networks']
            receiver_ip = ipaddress.ip_address(config['listen_host'])
            candidates = []
            for name, entry in networks.items():
                if config.get('source_network') and name != config['source_network']:
                    continue
                address = entry.get('IPAddress')
                if address and receiver_ip in ipaddress.ip_network(
                        f"{address}/{entry['IPPrefixLen']}", strict=False):
                    candidates.append(address)
            if len(candidates) != 1:
                raise ReceiverError('source_receiver_network_ambiguous')
            self.base = 'http://' + candidates[0] + ':8765'
            self.runtime = {'container_id': inspect['Id'], 'image_id': inspect['Image']}
        except (KeyError, IndexError, ValueError, subprocess.SubprocessError):
            raise ReceiverError('source_runtime_inspection_failed', 502) from None
        self.http = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())

    def call(self, path, data=None, method=None):
        request = urllib.request.Request(self.base + path,
            headers={'x-admin-key': self.key, 'Content-Type': 'application/json'},
            data=json.dumps(data).encode() if data is not None else None, method=method)
        try:
            with self.http.open(request, timeout=40) as response:
                body = response.read(32 * 1024 * 1024 + 1)
            if len(body) > 32 * 1024 * 1024:
                raise ReceiverError('source_response_too_large', 502)
            value = json.loads(body)
            if not isinstance(value, dict) or value.get('ok') is False:
                raise ReceiverError('source_operation_failed', 502)
            return value
        except urllib.error.HTTPError as exc:
            raise ReceiverError('source_http_' + str(exc.code), 502) from None
        except (urllib.error.URLError, TimeoutError, ValueError):
            raise ReceiverError('source_unavailable_or_invalid_response', 502) from None


def preflight(config: dict, source: Source) -> tuple[dict, list[dict]]:
    status = source.call('/api/team-rotation/status')
    if not status.get('config', {}).get('enabled'):
        raise ReceiverError('source_rotation_disabled_no_automatic_delivery')
    prepared = []
    for selected in config['owners']:
        matches = [row for row in status.get('owners', []) if row.get('id') == selected['owner_id']]
        if len(matches) != 1:
            raise ReceiverError('source_owner_not_unique')
        owner = matches[0]
        if owner.get('workspace_id') != selected['workspace_id'] or owner.get('status') != 'active':
            raise ReceiverError('source_owner_workspace_or_status_changed')
        children = [row for row in status.get('children', [])
                    if row.get('owner_id') == owner['id'] and row.get('status') == 'active']
        if len(children) != 1 or int(owner.get('active_children') or 0) != 1:
            raise ReceiverError('source_owner_requires_exactly_one_existing_active_child')
        child = children[0]
        if child.get('oauth_reauth_status') != 'success':
            raise ReceiverError('source_child_oauth_not_already_verified')
        email = str(child.get('email') or '').strip().lower()
        registered = source.call('/api/registered/' + urllib.parse.quote(email, safe=''))['data']
        token = registered.get('access_token') or ''
        identity = jwt_identity(token)
        if (identity['workspace_id'] != selected['workspace_id']
                or identity['expires_at'] < time.time() + 900
                or identity['user_id'] != child.get('user_id')
                or (identity['email'] and identity['email'] != email)):
            raise ReceiverError('source_child_token_identity_or_lifetime_invalid')
        prepared.append({**selected, 'email_sha256': sha(email),
            'filename_sha256': sha(email + '.json'), 'identity_sha256': identity['identity_sha256'],
            'token_sha256_at_prepare': sha(token), 'expires_at_prepare': identity['expires_at'],
            'assigned_at': child.get('assigned_at'), 'oauth_updated_at': child.get('oauth_reauth_updated_at'),
            'previous_destination_ids': list(owner.get('destination_ids') or [])})
    return status, prepared


def load_control(config: dict) -> dict:
    control = read_private(Path(config['state_dir']) / 'control.json')
    if control.get('scope') != config_scope(config):
        raise ReceiverError('prepared_scope_changed')
    return control


def prepare(config: dict, source=None) -> dict:
    root = Path(config['state_dir'])
    with lifecycle_lock(root):
        if (root / 'control.json').exists():
            load_control(config)
            return snapshot(config)
        source = source or Source(config)
        status, owners = preflight(config, source)
        destination_name = 'codex-quality-' + config['run_id']
        if any(row.get('name') == destination_name for row in status.get('destinations', [])):
            raise ReceiverError('run_destination_already_exists')
        control = {'schema': 1, 'scope': config_scope(config), 'phase': 'prepared',
            'created_at': int(time.time()), 'source_runtime': source.runtime,
            'destination_name': destination_name, 'owners': owners,
            'destination_id': None, 'bound_owner_ids': []}
        write_private(root / 'ingress-key.json', {'key': secrets.token_urlsafe(48)})
        write_private(root / 'allowlist.json', {'schema': 1, 'run_id': config['run_id'], 'owners': owners})
        write_private(root / 'control.json', control)
    return snapshot(config)


def receiver_ready(config: dict, key: str) -> None:
    http = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())
    request = urllib.request.Request(base_url(config) + '/v0/management/auth-files',
                                     headers={'Authorization': 'Bearer ' + key})
    try:
        with http.open(request, timeout=10) as response:
            body = json.load(response)
        if body.get('run_id') != config['run_id'] or not isinstance(body.get('files'), list):
            raise ReceiverError('receiver_scope_mismatch')
    except (urllib.error.URLError, TimeoutError, ValueError):
        raise ReceiverError('receiver_not_ready', 502) from None


def destination_matches(config: dict, control: dict, row: dict) -> bool:
    return (row.get('name') == control['destination_name'] and row.get('platform') == 'cpa'
            and str(row.get('url') or '').rstrip('/') == base_url(config) and row.get('enabled') is True)


def configure(config: dict, source=None, ready_check=receiver_ready) -> dict:
    root = Path(config['state_dir'])
    with lifecycle_lock(root):
        control = load_control(config)
        if control['phase'] == 'restored':
            raise ReceiverError('run_already_restored_use_new_run')
        source = source or Source(config)
        status, current = preflight(config, source)
        for saved, now in zip(control['owners'], current):
            if any(saved.get(k) != now.get(k) for k in (
                    'owner_id', 'workspace_id', 'email_sha256', 'identity_sha256', 'assigned_at')):
                raise ReceiverError('source_child_assignment_changed')
            permitted = set(saved['previous_destination_ids'])
            if control['destination_id']:
                permitted.add(control['destination_id'])
            if not set(now['previous_destination_ids']).issubset(permitted) or not set(
                    saved['previous_destination_ids']).issubset(now['previous_destination_ids']):
                raise ReceiverError('source_bindings_changed_before_configure')
        key = read_private(root / 'ingress-key.json')['key']
        ready_check(config, key)
        matches = [row for row in status.get('destinations', []) if row.get('name') == control['destination_name']]
        if len(matches) > 1 or (matches and not destination_matches(config, control, matches[0])):
            raise ReceiverError('destination_ownership_changed')
        if control['destination_id'] and (not matches or matches[0]['id'] != control['destination_id']):
            raise ReceiverError('destination_id_changed')
        if matches:
            destination = matches[0]
        else:
            control['phase'] = 'creating_destination'
            write_private(root / 'control.json', control)
            destination = source.call('/api/team-rotation/sub2api-destinations', {
                'platform': 'cpa', 'name': control['destination_name'], 'enabled': True,
                'url': base_url(config), 'api_key': key, 'timeout': 30})['item']
            if not destination_matches(config, control, destination):
                raise ReceiverError('destination_create_result_mismatch')
        control.update(destination_id=destination['id'], phase='binding')
        write_private(root / 'control.json', control)
        for saved in control['owners']:
            fresh = source.call('/api/team-rotation/status')
            owner = next((o for o in fresh.get('owners', []) if o.get('id') == saved['owner_id']), {})
            before = list(owner.get('destination_ids') or [])
            allowed = set(saved['previous_destination_ids']) | {destination['id']}
            if (owner.get('workspace_id') != saved['workspace_id'] or set(before) - allowed
                    or not set(saved['previous_destination_ids']).issubset(before)):
                raise ReceiverError('source_binding_conflict')
            if destination['id'] not in before:
                source.call('/api/team-rotation/owners/' + saved['owner_id'],
                            {'destination_ids': before + [destination['id']]}, 'PATCH')
            after = source.call('/api/team-rotation/status')
            actual = next((o for o in after.get('owners', []) if o.get('id') == saved['owner_id']), {})
            if set(actual.get('destination_ids') or []) != allowed:
                raise ReceiverError('source_binding_verification_failed')
            if saved['owner_id'] not in control['bound_owner_ids']:
                control['bound_owner_ids'].append(saved['owner_id'])
            write_private(root / 'control.json', control)
        control.update(phase='configured', configured_at=int(time.time()))
        write_private(root / 'control.json', control)
    return snapshot(config)


def restore(config: dict, source=None) -> dict:
    root = Path(config['state_dir'])
    with lifecycle_lock(root):
        control = load_control(config)
        source = source or Source(config)
        status = source.call('/api/team-rotation/status')
        destination_id = control.get('destination_id')
        if not destination_id:
            matches = [d for d in status.get('destinations', []) if d.get('name') == control['destination_name']]
            if len(matches) > 1 or (matches and not destination_matches(config, control, matches[0])):
                raise ReceiverError('destination_ownership_changed')
            destination_id = matches[0]['id'] if matches else None
        if destination_id:
            rows = [d for d in status.get('destinations', []) if d.get('id') == destination_id]
            if rows and not destination_matches(config, control, rows[0]):
                raise ReceiverError('destination_ownership_changed')
            selected = {row['owner_id'] for row in control['owners']}
            if any(destination_id in (o.get('destination_ids') or []) and o.get('id') not in selected
                   for o in status.get('owners', [])):
                raise ReceiverError('destination_bound_to_unselected_owner')
            control['phase'] = 'restoring'
            write_private(root / 'control.json', control)
            for saved in control['owners']:
                status = source.call('/api/team-rotation/status')
                owner = next((o for o in status.get('owners', []) if o.get('id') == saved['owner_id']), {})
                current = list(owner.get('destination_ids') or [])
                if owner.get('workspace_id') != saved['workspace_id'] or not set(
                        saved['previous_destination_ids']).issubset(current):
                    raise ReceiverError('source_original_bindings_or_workspace_changed')
                if destination_id in current:
                    source.call('/api/team-rotation/owners/' + saved['owner_id'],
                                {'destination_ids': [d for d in current if d != destination_id]}, 'PATCH')
                after = source.call('/api/team-rotation/status')
                actual = next((o for o in after.get('owners', []) if o.get('id') == saved['owner_id']), {})
                if set(actual.get('destination_ids') or []) != set(current) - {destination_id}:
                    raise ReceiverError('restore_binding_verification_failed')
            if rows:
                latest = source.call('/api/team-rotation/status')
                if any(destination_id in (owner.get('destination_ids') or [])
                       for owner in latest.get('owners', [])):
                    raise ReceiverError('destination_rebound_before_delete')
                source.call('/api/team-rotation/sub2api-destinations/' + destination_id, method='DELETE')
            final = source.call('/api/team-rotation/status')
            if (any(d.get('id') == destination_id for d in final.get('destinations', []))
                    or any(destination_id in (o.get('destination_ids') or []) for o in final.get('owners', []))):
                raise ReceiverError('restore_destination_verification_failed')
        # Block late uploads before erasing any remaining credential files.
        with credential_lock(root):
            control.update(phase='restored', restored_at=int(time.time()))
            write_private(root / 'control.json', control)
            for row in control['owners']:
                path = root / 'credentials' / (row['alias'] + '.json')
                if path.is_symlink():
                    raise ReceiverError('credential_file_symlink')
                path.unlink(missing_ok=True)
    return snapshot(config)


def snapshot(config: dict) -> dict:
    root = Path(config['state_dir'])
    control = load_control(config)
    rows = []
    for saved in control['owners']:
        status_file = root / 'receipts' / (saved['alias'] + '.json')
        receipt = read_private(status_file) if status_file.exists() else {}
        rows.append({'alias': saved['alias'], 'owner_sha256': sha(saved['owner_id']),
            'workspace_sha256': sha(saved['workspace_id']), 'identity_sha256': saved['identity_sha256'],
            'source_token_sha256_at_prepare': saved['token_sha256_at_prepare'],
            'source_oauth_updated_at': saved['oauth_updated_at'],
            'credential_present': (root / 'credentials' / (saved['alias'] + '.json')).exists(),
            'credential_file': str(root / 'credentials' / (saved['alias'] + '.json')),
            **{k: receipt.get(k) for k in ('token_sha256', 'expires_at', 'issued_at', 'received_at',
                                         'upload_count', 'withdrawn', 'withdrawn_at')}})
    return {'ok': True, 'run_id': config['run_id'], 'phase': control['phase'],
            'destination_id': control.get('destination_id'), 'accounts': rows,
            'scope': 'stored_AT_identity_and_expiry_only; upstream_quality_not_asserted'}


class Receiver:
    def __init__(self, config: dict):
        self.config = config
        self.root = Path(config['state_dir'])
        control = load_control(config)
        if control['phase'] == 'restored':
            raise ReceiverError('run_already_restored_use_new_run')
        allowlist = read_private(self.root / 'allowlist.json')
        if allowlist.get('owners') != control['owners'] or allowlist.get('run_id') != config['run_id']:
            raise ReceiverError('allowlist_scope_changed')
        self.owners = allowlist['owners']
        self.key = read_private(self.root / 'ingress-key.json')['key']
        self.lock = threading.RLock()

    def paths(self, alias: str):
        return (self.root / 'credentials' / (alias + '.json'), self.root / 'receipts' / (alias + '.json'))

    def find(self, filename: str) -> dict:
        matches = [row for row in self.owners if row['filename_sha256'] == sha(filename)]
        if len(matches) != 1:
            raise ReceiverError('source_identity_not_allowlisted', 403)
        return matches[0]

    def upload(self, filename: str, payload: dict) -> dict:
        email = str(payload.get('email') or '').strip().lower()
        if (not email or '@' not in email or filename != email + '.json' or len(filename) > 320
                or '/' in filename or '\\' in filename or payload.get('type') != 'codex'):
            raise ReceiverError('invalid_auth_file_identity', 400)
        saved = self.find(filename)
        token = payload.get('access_token') or ''
        identity = jwt_identity(token)
        if (sha(email) != saved['email_sha256'] or identity['identity_sha256'] != saved['identity_sha256']
                or identity['workspace_id'] != saved['workspace_id']
                or payload.get('account_id') != saved['workspace_id']
                or identity['expires_at'] < time.time() + 60 or identity['issued_at'] > time.time() + 60
                or (identity['email'] and identity['email'] != email)):
            raise ReceiverError('token_workspace_identity_or_expiry_mismatch', 400)
        credential_file, receipt_file = self.paths(saved['alias'])
        with self.lock, credential_lock(self.root):
            if load_control(self.config)['phase'] not in ('prepared', 'creating_destination', 'binding', 'configured'):
                raise ReceiverError('run_no_longer_accepts_uploads', 410)
            old = read_private(receipt_file) if receipt_file.exists() else {}
            if (old.get('token_sha256') != sha(token)
                    and (identity['issued_at'] < old.get('issued_at', 0)
                         or (identity['issued_at'] == old.get('issued_at', 0)
                             and identity['expires_at'] < old.get('expires_at', 0)))):
                raise ReceiverError('stale_token_upload_rejected')
            credential = {'alias': saved['alias'], 'access_token': token,
                'account_id': identity['workspace_id'], 'expires_at': identity['expires_at'],
                'identity_sha256': identity['identity_sha256']}
            write_private(credential_file, credential)
            write_private(receipt_file, {'alias': saved['alias'], 'token_sha256': sha(token),
                'identity_sha256': identity['identity_sha256'], 'issued_at': identity['issued_at'],
                'expires_at': identity['expires_at'], 'received_at': int(time.time()),
                'upload_count': int(old.get('upload_count', 0)) + 1, 'withdrawn': False})
        return {'ok': True, 'status': 'ok', 'file_name': filename}

    def list_files(self, filename='') -> dict:
        files = []
        if filename:
            try:
                saved = self.find(filename)
            except ReceiverError:
                return {'files': [], 'run_id': self.config['run_id']}
            with self.lock, credential_lock(self.root):
                credential_file, receipt_file = self.paths(saved['alias'])
                if credential_file.exists() and receipt_file.exists():
                    credential, receipt = read_private(credential_file), read_private(receipt_file)
                    ready = (not receipt.get('withdrawn')
                             and receipt.get('token_sha256') == sha(credential.get('access_token', ''))
                             and credential.get('expires_at', 0) > time.time() + 60
                             and load_control(self.config)['phase'] != 'restored')
                    files.append({'name': filename, 'type': 'codex', 'status': 'active' if ready else 'error',
                        'disabled': not ready, 'unavailable': not ready, 'websockets': True,
                        'default_websockets': True})
        return {'files': files, 'run_id': self.config['run_id']}

    def withdraw(self, filename: str) -> dict:
        saved = self.find(filename)
        with self.lock, credential_lock(self.root):
            credential_file, receipt_file = self.paths(saved['alias'])
            if credential_file.is_symlink():
                raise ReceiverError('credential_file_symlink')
            credential_file.unlink(missing_ok=True)
            old = read_private(receipt_file) if receipt_file.exists() else {'alias': saved['alias']}
            write_private(receipt_file, {**old, 'withdrawn': True, 'withdrawn_at': int(time.time())})
        return {'ok': True, 'status': 'ok'}


def multipart_file(content_type: str, body: bytes) -> tuple[str, dict]:
    if '\r' in content_type or '\n' in content_type or len(content_type) > 1000:
        raise ReceiverError('invalid_content_type', 400)
    message = BytesParser(policy=policy.default).parsebytes(
        ('Content-Type: ' + content_type + '\r\nMIME-Version: 1.0\r\n\r\n').encode() + body)
    if not message.is_multipart():
        raise ReceiverError('multipart_required', 400)
    files = [part for part in message.iter_parts()
             if part.get_param('name', header='content-disposition') == 'file']
    if len(files) != 1:
        raise ReceiverError('one_auth_file_required', 400)
    try:
        value = json.loads(files[0].get_payload(decode=True))
    except (ValueError, TypeError, UnicodeError):
        raise ReceiverError('invalid_auth_json', 400) from None
    if not isinstance(value, dict):
        raise ReceiverError('invalid_auth_json', 400)
    return files[0].get_filename() or '', value


def handler_for(receiver: Receiver):
    class Handler(BaseHTTPRequestHandler):
        protocol_version = 'HTTP/1.0'

        def log_message(self, *_args):
            pass

        def setup(self):
            super().setup()
            self.connection.settimeout(15)

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
                if not management_peer_allowed(self.client_address[0],
                                               receiver.config.get('allowed_peer_ips') or ()):
                    raise ReceiverError('private_source_required', 403)
                bearer = self.headers.get('Authorization', '')
                key = bearer[7:] if bearer.startswith('Bearer ') else self.headers.get('X-Management-Key', '')
                if not hmac.compare_digest(key, receiver.key):
                    raise ReceiverError('unauthorized', 401)
                target = urllib.parse.urlsplit(self.path)
                if target.path != '/v0/management/auth-files' or target.scheme or target.netloc:
                    raise ReceiverError('not_found', 404)
                query = urllib.parse.parse_qs(target.query, keep_blank_values=True)
                if set(query) - {'name'} or len(query.get('name', [])) > 1:
                    raise ReceiverError('invalid_query', 400)
                filename = query.get('name', [''])[0]
                if self.command == 'GET':
                    return self.reply(200, receiver.list_files(filename))
                if self.command == 'DELETE':
                    if not filename:
                        raise ReceiverError('filename_required', 400)
                    return self.reply(200, receiver.withdraw(filename))
                if self.command in ('POST', 'PUT'):
                    lengths = self.headers.get_all('Content-Length') or []
                    if len(lengths) != 1 or self.headers.get('Transfer-Encoding'):
                        raise ReceiverError('invalid_body_size', 413)
                    size = int(lengths[0])
                    if not 0 < size <= 256 * 1024:
                        raise ReceiverError('invalid_body_size', 413)
                    body = self.rfile.read(size)
                    if len(body) != size:
                        raise ReceiverError('incomplete_body', 400)
                    name, payload = multipart_file(self.headers.get('Content-Type', ''), body)
                    return self.reply(200, receiver.upload(name, payload))
                raise ReceiverError('method_not_allowed', 405)
            except ReceiverError as exc:
                self.reply(exc.status, {'ok': False, 'error': exc.code})
            except (ValueError, TypeError):
                self.reply(400, {'ok': False, 'error': 'invalid_request'})
            except Exception:
                self.reply(500, {'ok': False, 'error': 'receiver_internal_error'})

        do_GET = dispatch
        do_POST = dispatch
        do_PUT = dispatch
        do_DELETE = dispatch

    return Handler


def serve(config: dict):
    receiver = Receiver(config)
    server = ThreadingHTTPServer((config['listen_host'], config['listen_port']), handler_for(receiver))
    server.daemon_threads = True
    server.serve_forever()


def main(argv=None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action', choices=('prepare', 'serve', 'configure', 'snapshot', 'restore'))
    parser.add_argument('--config', type=Path, required=True)
    args = parser.parse_args(argv)
    try:
        config = load_config(args.config)
        if args.action == 'serve':
            serve(config)
        else:
            result = globals()[args.action](config)
            print(json.dumps(result, ensure_ascii=False))
        return 0
    except ReceiverError as exc:
        print(json.dumps({'ok': False, 'error': exc.code}))
    except Exception as exc:
        print(json.dumps({'ok': False, 'error': type(exc).__name__}))
    return 1


if __name__ == '__main__':
    sys.exit(main())
