"""Summarize retained HTTP/CLI failures and quota snapshots without network I/O.

python tools/quality_failure_summary.py evidence.jsonl [...] --output summary.json
HTTP captures may contain {http_status, response_headers, error}, raw_http with
{status, headers, body}, or raw HTTP response text. Only explicit structured
error fields and fixed CLI error-event status formats are evidence. Answers,
messages, usage counters and arbitrary text containing 429 are never scanned.
No model requests, OAuth refresh, quota calls, source mutation or deployment.
"""
from __future__ import annotations

import argparse
from collections import Counter
from datetime import datetime, timezone
from email.utils import parsedate_to_datetime
import json
import math
from pathlib import Path
import re
import time


QUOTA_CODES = {'insufficient_quota', 'credit_balance_exhausted',
    'organization_spend_limit_exceeded', 'project_spend_limit_exceeded',
    'organization_usage_limit_exceeded', 'usage_not_included'}
RATE_CODES = {'usage_limit_reached', 'rate_limit_exceeded', 'rate_limit_error',
    'rate_limit_reached', 'too_many_requests'}
AUTH_CODES = {'token_revoked', 'token_invalidated', 'token_expired',
    'access_token_expired', 'invalidated_oauth_token', 'invalid_api_key'}
OVERLOAD_CODES = {'server_is_overloaded', 'service_unavailable_error',
    'overloaded_error', 'overloaded'}
STATUS_KEYS = ('http_status', 'http_code', 'status_code', 'http_status_code', 'status')
ERROR_KEYS = ('error', 'http_error', 'response_error', 'upstream_error')
HEADER_RE = re.compile(r'^x-(codex(?:-[a-z0-9]+)*)-(primary|secondary)-'
                       r'(used-percent|window-minutes|reset-at)$')
CODE_RE = re.compile(r'^[a-z][a-z0-9_]{0,79}$')
CLI_STATUS = (
    re.compile(r'^(?:Reconnecting\.\.\. \d+/\d+ \()?unexpected status ([45]\d\d)\b', re.I),
    re.compile(r'^Falling back from WebSockets to HTTPS transport\. unexpected status ([45]\d\d)\b', re.I),
    re.compile(r'^exceeded retry limit, last status: ([45]\d\d)\b', re.I),
)


def number(value):
    if isinstance(value, bool) or not isinstance(value, (int, float, str)):
        return None
    try:
        result = float(value)
    except (ValueError, OverflowError):
        return None
    return result if math.isfinite(result) else None


def timestamp(value):
    parsed = number(value)
    if parsed is not None:
        # Epoch milliseconds are explicitly accepted for gateway runtime fields.
        return parsed / 1000 if parsed > 10**11 else parsed if parsed > 0 else None
    if isinstance(value, str):
        try:
            result = datetime.fromisoformat(value.replace('Z', '+00:00'))
            return result.timestamp() if result.tzinfo else None
        except ValueError:
            pass
    return None


def iso(value):
    try:
        return datetime.fromtimestamp(value, timezone.utc).isoformat().replace('+00:00', 'Z')
    except (TypeError, ValueError, OSError, OverflowError):
        return None


def status(value):
    return value if type(value) is int and 100 <= value <= 599 else None


def code(value):
    return value if isinstance(value, str) and CODE_RE.fullmatch(value) else None


def obj(value):
    if isinstance(value, dict):
        return value
    if isinstance(value, str):
        try:
            decoded = json.loads(value)
            return decoded if isinstance(decoded, dict) else {}
        except ValueError:
            pass
    return {}


def raw_response(value):
    if isinstance(value, dict):
        return value
    if not isinstance(value, str):
        return {}
    # Require a response status line at offset zero; never look inside body text.
    line, _, tail = value.partition('\n')
    match = re.fullmatch(r'HTTP/(?:1\.[01]|2|3) ([1-5]\d\d)(?: [^\r\n]*)?\r?', line)
    if not match:
        return {}
    tail = tail.replace('\r\n', '\n')
    header_text, separator, body = ('', '\n', tail[1:]) if tail.startswith('\n') else tail.partition('\n\n')
    headers = {}
    if separator:
        for header in header_text.splitlines():
            name, colon, entry = header.partition(':')
            if colon:
                headers.setdefault(name.strip().lower(), []).append(entry.strip())
    return {'status': int(match[1]), 'headers': headers, 'body': body}


def header_map(value):
    if not isinstance(value, dict):
        return {}
    result = {}
    for name, entry in value.items():
        if not isinstance(name, str):
            continue
        entries = entry if isinstance(entry, list) else [entry]
        entries = [str(item).strip() for item in entries if isinstance(item, (str, int, float))
                   and not isinstance(item, bool)]
        if entries:
            result.setdefault(name.lower(), []).extend(entries)
    return result


def one_header(headers, name):
    values = set(headers.get(name, []))
    # Conflicting repeated control headers are not an authoritative deadline.
    return next(iter(values)) if len(values) == 1 else None


def retry_deadline(value, observed_at):
    if value is None:
        return None
    if re.fullmatch(r'\d+', value):
        delta = number(value)
        return observed_at + delta if observed_at is not None and delta is not None else None
    try:
        parsed = parsedate_to_datetime(value)
        return parsed.timestamp() if parsed.tzinfo else None
    except (TypeError, ValueError, OverflowError):
        return None


def summarize_failure(row, now=None):
    """Return bounded fields only; never emit raw body, messages, tokens or URLs."""
    now = time.time() if now is None else now
    observed_at = next((value for key in ('started_at', 'observed_at', 'checked_at')
                        if (value := timestamp(row.get(key))) is not None), None)
    raw = raw_response(row.get('raw_http', row.get('rawHTTP')))
    statuses = {value for key in STATUS_KEYS if (value := status(row.get(key))) is not None}
    raw_status = status(raw.get('status', raw.get('status_code')))
    if raw_status is not None:
        statuses.add(raw_status)
    headers = header_map(row.get('response_headers', row.get('headers', raw.get('headers'))))
    errors = []
    for source in (row, obj(row.get('body')), obj(raw.get('body')),
                   obj(row.get('response'))):
        for key in ERROR_KEYS:
            if isinstance(source.get(key), dict):
                errors.append(source[key])
    for key in ('safe_errors', 'safeerrors'):
        if isinstance(row.get(key), list):
            for error in row[key]:
                if isinstance(error, dict):
                    errors.append(error.get('error') if isinstance(error.get('error'), dict) else error)
    event_type = row.get('type')
    error_event = event_type in {'error', 'turn.failed', 'response.failed', 'response.incomplete'} or (
        event_type == 'item.completed' and (row.get('item_type') == 'error' or
        obj(row.get('item')).get('type') == 'error'))
    if event_type == 'error':
        errors.append(row)
    codes, types, body_resets = set(), set(), []
    for error in errors:
        for key in STATUS_KEYS:
            value = status(error.get(key))
            if value is not None:
                statuses.add(value)
        for key in ('code', 'error_code'):
            value = code(error.get(key))
            if value:
                codes.add(value)
        value = code(error.get('type'))
        if value and value != 'error':
            types.add(value)
        reset = timestamp(error.get('resets_at', error.get('reset_at')))
        if reset is not None:
            body_resets.append(reset)
    if error_event:
        # Legacy safe CLI records lack structured status. Parse only exact CLI
        # diagnostic templates from the error event, not a model response.
        message = obj(row.get('error')).get('message_excerpt')
        if isinstance(message, str):
            # CLI 0.155 retry_after.rs identifies this exact message as
            # ServerOverloaded, independently from HTTP status/usage limits.
            if message == 'Selected model is at capacity. Please try a different model.':
                codes.add('server_is_overloaded')
            for pattern in CLI_STATUS:
                match = pattern.match(message)
                if match:
                    statuses.add(int(match[1]))
                    break
    markers = codes | types
    failure_statuses = {value for value in statuses if value >= 400}
    if len(failure_statuses) > 1:
        category = 'conflicting_failure_evidence'
    elif 429 in statuses:
        category = 'quota_exhausted' if markers & QUOTA_CODES else 'rate_limited'
    elif 401 in statuses:
        category = 'authentication'
    elif 403 in statuses:
        category = 'forbidden'
    elif statuses & {502, 503, 504}:
        category = 'upstream_unavailable'
    elif failure_statuses:
        category = 'http_error'
    elif markers & QUOTA_CODES:
        category = 'quota_exhausted'
    elif markers & RATE_CODES:
        category = 'rate_limited'
    elif markers & AUTH_CODES:
        category = 'authentication'
    elif markers & OVERLOAD_CODES:
        category = 'upstream_unavailable'
    elif row.get('protocol_success') is True:
        category = 'completed'
    elif errors or error_event:
        category = 'unclassified_error'
    else:
        category = 'unknown'
    windows = {}
    for name in headers:
        match = HEADER_RE.fullmatch(name)
        value = number(one_header(headers, name))
        if not match or value is None:
            continue
        family, window, field = match.groups()
        item = windows.setdefault((family.replace('-', '_'), window), {})
        if field == 'reset-at':
            # Codex headers use epoch seconds; never interpret as delay seconds.
            if value > 0 and value.is_integer():
                item['reset_at'] = int(value)
        elif field == 'used-percent' and value >= 0:
            item['used_percent'] = value
        elif field == 'window-minutes' and value >= 0 and value.is_integer():
            item['window_minutes'] = int(value)
    active = one_header(headers, 'x-codex-active-limit')
    active = active.lower().replace('-', '_') if isinstance(active, str) else None
    if row.get('type') == 'codex.rate_limits':
        event_limit = row.get('metered_limit_name', row.get('limit_name', 'codex'))
        event_limit = event_limit.strip().lower().replace('-', '_') if isinstance(event_limit, str) else ''
        if code(event_limit):
            for window in ('primary', 'secondary'):
                values = obj(obj(row.get('rate_limits')).get(window))
                used = number(values.get('used_percent'))
                if used is None or used < 0:
                    continue
                fields = {'used_percent': used}
                reset = timestamp(values.get('reset_at'))
                minutes = number(values.get('window_minutes'))
                if reset is not None and iso(reset):
                    fields['reset_at'] = reset
                if minutes is not None and minutes >= 0 and minutes.is_integer():
                    fields['window_minutes'] = int(minutes)
                windows[(event_limit, window)] = fields
    hints = []
    retry = retry_deadline(one_header(headers, 'retry-after'), observed_at)
    if retry is not None and iso(retry):
        hints.append({'source': 'retry-after', 'not_before': retry})
    if category == 'rate_limited':
        hints.extend({'source': 'error.resets_at', 'not_before': value}
                     for value in body_resets if iso(value))
        for (family, window), fields in windows.items():
            if fields.get('used_percent', 0) >= 100 and fields.get('reset_at') and (
                    not active or family == active):
                hints.append({'source': f'x-{family.replace("_", "-")}-{window}-reset-at',
                              'not_before': fields['reset_at']})
    future = [hint['not_before'] for hint in hints if hint['not_before'] > now]
    deadline = max(future, default=None)
    wait = 'unknown'
    if category == 'quota_exhausted':
        wait = 'quota_or_entitlement_action_required'
    elif category in {'authentication', 'forbidden'}:
        wait = 'no_time_based_recovery_proven'
    elif category in {'rate_limited', 'upstream_unavailable'}:
        if deadline is not None:
            wait = 'wait_until_not_before'
        elif hints:
            wait = 'recorded_deadline_elapsed_recheck_required'
    elif category == 'completed':
        wait = 'not_applicable'
    output = {'category': category, 'http_statuses': sorted(statuses),
        'error_codes': sorted(codes), 'error_types': sorted(types),
        'observed_at': iso(observed_at), 'wait_state': wait,
        'not_before_utc': iso(deadline) if wait == 'wait_until_not_before' else None,
        'rate_limit_windows': [{'limit_id': family, 'window': window, **fields}
                              for (family, window), fields in sorted(windows.items())],
        'deadline_evidence': [{'source': hint['source'], 'not_before_utc': iso(hint['not_before'])}
                              for hint in hints],
        'missing_recovery_evidence': category == 'rate_limited' and not hints}
    for key in ('account_alias', 'arm', 'phase'):
        value = row.get(key)
        if isinstance(value, str) and re.fullmatch(r'[a-zA-Z0-9_-]{1,64}', value):
            output[key] = value
    return output


def summarize_source_state(observation, now=None, max_age=300):
    """Offline projection of current cached state; `testable` is not quota proof.

    Input: {identity_matches, source_child, source_history?, gateway_runtime?,
    credential_expires_at}. Caller collects only local management state. Cached
    history's quota_observed_at is authoritative; child.last_checked_at is shared
    with failure/lifecycle writes and must not be treated as a fresh quota check.
    """
    now = time.time() if now is None else now
    child = obj(observation.get('source_child'))
    history = obj(observation.get('source_history'))
    runtime = obj(observation.get('gateway_runtime'))
    reasons, deadlines = [], []
    result = {'state': 'unknown', 'reasons': reasons, 'not_before_utc': None,
              'live_model_called': False, 'upstream_quota_called': False}
    if observation.get('identity_matches') is not True:
        reasons.append('identity_not_verified_or_changed')
        return result
    if child.get('status') != 'active' or child.get('oauth_reauth_status') != 'success':
        reasons.append('source_identity_not_active_and_oauth_ready')
        return result
    quota_at = timestamp(history.get('quota_observed_at'))
    assigned_at = timestamp(child.get('assigned_at'))
    if quota_at is not None and assigned_at is not None and quota_at < assigned_at:
        reasons.append('cached_quota_precedes_current_assignment')
        return result
    quota_fresh = quota_at is not None and -5 <= now - quota_at <= max_age
    for prefix in ('primary', 'secondary'):
        used = number(history.get(prefix + '_used_percent'))
        reset = timestamp(history.get(prefix + '_reset_at'))
        if quota_fresh and used is not None and used >= 100:
            if reset is not None and reset > now:
                reasons.append(prefix + '_quota_exhausted_before_cached_reset')
                deadlines.append(reset)
            elif quota_fresh and reset is None:
                reasons.append(prefix + '_quota_exhausted_no_reset')
    checked = timestamp(runtime.get('checked_at'))
    runtime_fresh = checked is not None and -5 <= now - checked <= max_age
    cooldowns = obj(runtime.get('cooldowns'))
    if runtime_fresh:
        for entry in cooldowns.get('entries', []):
            if not isinstance(entry, dict):
                continue
            until = timestamp(entry.get('until'))
            if until is not None and until > now and entry.get('kind') == 'rate_limit':
                reasons.append('gateway_rate_limit_cooldown')
                deadlines.append(until)
    if reasons:
        result['state'] = 'rate_limited'
        result['not_before_utc'] = iso(max(deadlines)) if deadlines else None
        return result
    expiry = timestamp(observation.get('credential_expires_at'))
    if expiry is None or expiry <= now + 60:
        reasons.append('credential_expiry_not_ready')
    if runtime and not runtime_fresh:
        reasons.append('runtime_observation_stale')
    if runtime_fresh:
        account_policy = obj(obj(runtime.get('effective_policy')).get('account'))
        if account_policy.get('enabled') is False:
            reasons.append('gateway_delivery_disabled')
        if cooldowns.get('shared_state') == 'unavailable':
            reasons.append('gateway_shared_state_unavailable')
        if obj(runtime.get('admission')).get('saturated') is True:
            reasons.append('gateway_capacity_saturated')
        if any(timestamp(entry.get('until')) is not None and timestamp(entry.get('until')) > now
               for entry in cooldowns.get('entries', []) if isinstance(entry, dict)):
            reasons.append('gateway_nonquota_cooldown')
    quota_values = [number(history.get(prefix + '_used_percent')) for prefix in ('primary', 'secondary')]
    if not quota_fresh or not any(value is not None for value in quota_values):
        reasons.append('fresh_cached_quota_evidence_missing')
    elif any(value is not None and not 0 <= value < 100 for value in quota_values):
        reasons.append('cached_quota_reset_elapsed_recheck_required')
    if not reasons:
        result['state'] = 'testable'
        reasons.append('active_identity_valid_token_and_fresh_cached_quota')
    return result


def read_records(path):
    content = path.read_text(encoding='utf-8-sig')
    try:
        value = json.loads(content)
        rows = value if isinstance(value, list) else [value]
    except ValueError:
        rows = [json.loads(line) for line in content.splitlines() if line.strip()]
    if not all(isinstance(row, dict) for row in rows):
        raise ValueError('records must be JSON objects')
    return rows


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('inputs', nargs='+', type=Path)
    parser.add_argument('--output', type=Path)
    parser.add_argument('--now', help='Explicit UTC timestamp for reproducible historical analysis')
    parser.add_argument('--source-state', action='store_true', help='Analyze sanitized cached-state inputs')
    args = parser.parse_args(argv)
    now = timestamp(args.now) if args.now else time.time()
    if now is None:
        parser.error('--now requires an epoch or timezone-qualified ISO timestamp')
    try:
        summarize = summarize_source_state if args.source_state else summarize_failure
        rows = [summarize(row, now=now) for path in args.inputs for row in read_records(path)]
        counts = dict(Counter(row.get('category', row.get('state')) for row in rows))
        result = {'schema': 'codex-quality-failure-v1', 'analyzed_at': iso(now),
                  'network_calls': 0, 'counts': counts, 'records': rows}
        encoded = json.dumps(result, ensure_ascii=False, indent=2) + '\n'
        if args.output:
            args.output.parent.mkdir(parents=True, exist_ok=True)
            args.output.write_text(encoded, encoding='utf-8')
            print(json.dumps({'counts': counts, 'network_calls': 0}))
        else:
            print(encoded, end='')
    except (OSError, ValueError, TypeError, OverflowError):
        print(json.dumps({'error': 'invalid_or_unreadable_evidence', 'network_calls': 0}))
        return 2
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
