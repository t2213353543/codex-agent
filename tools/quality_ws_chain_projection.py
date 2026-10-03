"""Offline captured WS warmup+delta -> derived full-input HTTP comparison.

No credentials, network or model calls. This is a derived logical request, not
captured original CLI HTTP bytes. The actual WS chain must independently bind
its second frame to its real warmup response; synthetic prior IDs are not live.
"""
from __future__ import annotations

import base64
from copy import deepcopy
import hashlib
import json
import re

from quality_transport_pair import _object, _canonical, _Number, TransportPairError

SCHEMA = 'codex-quality-ws-chain-http-projection-v1'
PATH = '/backend-api/codex/responses'
MAX_BODY = 32 << 20
LITE = 'ws_request_header_x_openai_internal_codex_responses_lite'
STAMP = 'x-codex-ws-stream-request-start-ms'
EMBEDDED = 'x-codex-turn-metadata'
TRACE = {'ws_request_header_traceparent': 'traceparent', 'ws_request_header_tracestate': 'tracestate'}
WS_FIELDS = {'type', 'generate', 'previous_response_id'}
DELTA_FIELDS = WS_FIELDS | {'input', 'client_metadata', 'stream_options', 'access_programs'}
REQUIRED = {'type', 'model', 'input', 'tool_choice', 'parallel_tool_calls', 'reasoning',
            'store', 'stream', 'include', 'prompt_cache_key', 'client_metadata'}
PUBLIC_BODY_FIELDS = REQUIRED | DELTA_FIELDS | {'instructions', 'tools', 'text', 'service_tier'}
COMPAT_ORDER = ('installation_id', 'session_id', 'thread_id', 'agent_name', 'turn_id', 'window_id',
    'window_number', 'context_window_id', 'request_kind', 'forked_from_thread_id',
    'forked_from_ordinal_exclusive', 'parent_thread_id', 'parent_turn_id', 'root_turn_id',
    'subagent_kind', 'thread_source', 'turn_trigger', 'sandbox', 'sandbox_mode', 'auto_review_enabled',
    'node_repl_auto_review_required', 'node_repl_disabled', 'workspaces', 'tool_namespaces_info',
    'turn_started_at_unix_ms', 'history_ingest_requested', 'compaction')
PUBLIC_METADATA = set(COMPAT_ORDER) | {LITE, STAMP, EMBEDDED, *TRACE, 'x-codex-installation-id',
    'x-codex-window-id', 'x-codex-parent-thread-id', 'x-openai-subagent'}
APP_HEADERS = {'user-agent', 'originator', 'version', 'session-id', 'thread-id', 'x-client-request-id',
    'x-openai-subagent', 'x-openai-memgen-request', 'x-oai-attestation',
    'x-openai-internal-codex-residency', 'x-responsesapi-include-timing-metrics'}
DROP_HEADERS = {'accept', 'content-type', 'content-length', 'content-encoding', 'accept-encoding',
    'host', 'connection', 'upgrade', 'sec-websocket-key', 'sec-websocket-version',
    'sec-websocket-extensions', 'sec-websocket-protocol', 'x-responsesapi-include-timing-metrics'}


class WSChainProjectionError(ValueError):
    """Fixed labels only, never include raw request or header values."""


def _sha(raw):
    return hashlib.sha256(raw.encode() if isinstance(raw, str) else raw).hexdigest()


def _label(name, allowed):
    return name if name in allowed else 'name_sha256:' + _sha(name)


def _parse(raw):
    try:
        value, spans = _object(raw)
        pending, count = [(value, 0)], 0
        while pending:
            item, depth = pending.pop()
            count += 1
            if depth > 64 or count > 262144:
                raise WSChainProjectionError('json_structure_limit')
            if isinstance(item, dict):
                if len(item) > 1024:
                    raise WSChainProjectionError('json_field_limit')
                pending.extend((v, depth + 1) for v in item.values())
            elif isinstance(item, list):
                pending.extend((v, depth + 1) for v in item)
            elif isinstance(item, str):
                item.encode('utf-8')
        return value, spans
    except (TransportPairError, UnicodeError, ValueError, RecursionError) as error:
        if isinstance(error, WSChainProjectionError):
            raise
        raise WSChainProjectionError('invalid_or_duplicate_json') from None


def _assembled(raw, spans, *, remove=(), replace=None):
    replace = replace or {}
    # Only object framing/names are reserialized; every retained value byte span
    # comes directly from the original frame, including unknown extensions.
    return b'{' + b','.join(_canonical(name) + b':' + replace.get(name, raw[start:end])
                           for name, (start, end) in spans.items() if name not in remove) + b'}'


def _ascii_json(value):
    if isinstance(value, _Number):
        return value.encode('ascii')
    if isinstance(value, dict):
        return b'{' + b','.join(_ascii_json(k) + b':' + _ascii_json(v) for k, v in value.items()) + b'}'
    if isinstance(value, list):
        return b'[' + b','.join(_ascii_json(v) for v in value) + b']'
    return json.dumps(value, ensure_ascii=True, allow_nan=False, separators=(',', ':')).encode('ascii')


def _compatibility_metadata(embedded):
    extra = set(embedded) - set(COMPAT_ORDER)
    if len(extra) > 16 or any(not re.fullmatch(r'[A-Za-z][A-Za-z0-9_.-]{0,63}', k)
                            or type(embedded[k]) is not str or len(embedded[k].encode()) > 128 for k in extra):
        raise WSChainProjectionError('unsupported_compatibility_metadata_extension')
    unsigned = {'window_number', 'forked_from_ordinal_exclusive'}
    signed = {'turn_started_at_unix_ms'}
    booleans = {'auto_review_enabled', 'node_repl_auto_review_required', 'node_repl_disabled', 'history_ingest_requested'}
    objects = {'workspaces', 'tool_namespaces_info', 'compaction'}
    for name, value in embedded.items():
        if name in extra:
            continue
        if name in unsigned | signed:
            if not isinstance(value, _Number) or not re.fullmatch(r'-?[0-9]{1,20}', value):
                raise WSChainProjectionError('invalid_compatibility_field_type')
            number = int(value)
            if not (0 <= number <= 18446744073709551615 if name in unsigned else -9223372036854775808 <= number <= 9223372036854775807):
                raise WSChainProjectionError('invalid_compatibility_field_type')
        elif name in booleans and type(value) is not bool:
            raise WSChainProjectionError('invalid_compatibility_field_type')
        elif name in objects and not isinstance(value, dict):
            raise WSChainProjectionError('invalid_compatibility_field_type')
        elif name == 'thread_source':
            if type(value) not in (str, dict):
                raise WSChainProjectionError('invalid_compatibility_field_type')
        elif name not in unsigned | signed | booleans | objects and type(value) is not str:
            raise WSChainProjectionError('invalid_compatibility_field_type')
    result = {k: embedded[k] for k in COMPAT_ORDER if k in embedded and k != 'tool_namespaces_info'}
    result.update({k: embedded[k] for k in sorted(extra)})
    return result


def _metadata(frame, raw, spans, phase):
    metadata = frame.get('client_metadata')
    if not isinstance(metadata, dict) or any(type(v) is not str for v in metadata.values()):
        raise WSChainProjectionError('invalid_client_metadata')
    if metadata.get(LITE) != 'true' or not re.fullmatch(r'[0-9]{1,16}', metadata.get(STAMP, '')):
        raise WSChainProjectionError('missing_lite_or_send_timestamp')
    if not 1 <= int(metadata[STAMP]) <= 9007199254740991:
        raise WSChainProjectionError('invalid_send_timestamp')
    if any(k.startswith('ws_request_header_') and k not in {LITE, *TRACE} for k in metadata):
        raise WSChainProjectionError('unsupported_ws_header_projection')
    if 'x-codex-turn-state' in metadata:
        raise WSChainProjectionError('captured_chain_has_server_turn_state')
    for name in ('session_id', 'thread_id', 'x-codex-window-id'):
        if not metadata.get(name):
            raise WSChainProjectionError('missing_session_metadata')
    if type(metadata.get(EMBEDDED)) is not str:
        raise WSChainProjectionError('missing_turn_metadata')
    embedded_raw = metadata[EMBEDDED].encode()
    if len(embedded_raw) > 1 << 20:
        raise WSChainProjectionError('embedded_metadata_limit')
    embedded, _ = _parse(embedded_raw)
    if embedded.get('request_kind') != phase:
        raise WSChainProjectionError('wrong_metadata_request_kind')
    for name in ('session_id', 'thread_id', 'turn_id'):
        if type(metadata.get(name)) is not str or embedded.get(name) != metadata[name]:
            raise WSChainProjectionError('embedded_flat_identity_mismatch')
    if phase == 'turn' and not metadata['turn_id']:
        raise WSChainProjectionError('missing_inference_turn_id')
    start, end = spans['client_metadata']
    meta_raw = raw[start:end]
    _, meta_spans = _parse(meta_raw)
    return metadata, embedded, meta_raw, meta_spans


def _headers(head):
    if (type(head) is not dict or head.get('method') != 'GET' or head.get('path') != PATH
            or not isinstance(head.get('headers'), list) or len(head['headers']) > 120):
        raise WSChainProjectionError('invalid_ws_head')
    values, rows, total = {}, [], 0
    for pair in head['headers']:
        if type(pair) is not list or len(pair) != 2 or any(type(x) is not str for x in pair):
            raise WSChainProjectionError('invalid_header_pair')
        name, encoded = pair
        lower = name.lower()
        if not re.fullmatch(r"[!#$%&'*+.^_`|~0-9A-Za-z-]{1,256}", name) or lower in values:
            raise WSChainProjectionError('invalid_or_duplicate_header')
        if lower in {'authorization', 'chatgpt-account-id', 'cookie', 'proxy-authorization', 'api-key', 'x-api-key'}:
            raise WSChainProjectionError('credential_header_in_capture')
        try:
            raw = base64.b64decode(encoded, validate=True)
        except (ValueError, UnicodeError):
            raise WSChainProjectionError('invalid_header_encoding') from None
        total += len(raw) + len(name)
        if total > 64 << 10 or any(c < 32 and c != 9 or c == 127 for c in raw):
            raise WSChainProjectionError('invalid_header_value')
        if re.search(rb'(?i)bearer\s|\beyJ[A-Za-z0-9_-]*\.[A-Za-z0-9_-]+\.|\bsk-[A-Za-z0-9_-]{12,}', raw):
            raise WSChainProjectionError('credential_in_application_header')
        if lower not in APP_HEADERS | DROP_HEADERS | {'openai-beta', 'traceparent', 'tracestate'} and not lower.startswith('x-codex-'):
            raise WSChainProjectionError('unsupported_application_header')
        if lower == 'x-codex-turn-state':
            raise WSChainProjectionError('captured_chain_has_server_turn_state')
        values[lower] = raw
        rows.append((name, lower, raw))
    if values.get('openai-beta') != b'responses_websockets=2026-02-06':
        raise WSChainProjectionError('unexpected_ws_beta')
    if not values.get('user-agent') or not values.get('originator') or values.get('version') != b'0.155.0':
        raise WSChainProjectionError('missing_captured_cli_identity')
    return rows, values


def _completion(value, expected_prior, output_item_count):
    if (type(value) is not dict or value.get('type') != 'response.completed'
            or type(value.get('response')) is not dict or type(output_item_count) is not int or output_item_count != 0):
        raise WSChainProjectionError('warmup_completion_evidence_required')
    response = value['response']
    if (type(expected_prior) is not str or not expected_prior or len(expected_prior.encode()) > 1024
            or response.get('id') != expected_prior or response.get('status') != 'completed'
            or response.get('output') != [] or response.get('error') is not None):
        raise WSChainProjectionError('synthetic_warmup_completion_mismatch')
    usage = response.get('usage')
    if not isinstance(usage, dict) or any(type(usage.get(k)) is not int or usage[k] != 0 for k in ('input_tokens', 'output_tokens', 'total_tokens')):
        raise WSChainProjectionError('synthetic_warmup_usage_not_zero')
    for details in ('input_tokens_details', 'output_tokens_details'):
        if details in usage and usage[details] is not None:
            if not isinstance(usage[details], dict) or any(type(v) is not int or v != 0 for v in usage[details].values()):
                raise WSChainProjectionError('synthetic_warmup_usage_not_zero')


def derive_http_comparison(warmup_body, inference_body, ws_head, *, expected_warmup_sha256,
                          expected_inference_sha256, expected_prior, warmup_completion,
                          warmup_output_item_count, http_request_compression='zstd'):
    """Return (HTTP body bytes, native non-auth head, hash-only derivation receipt)."""
    for raw, pin in ((warmup_body, expected_warmup_sha256), (inference_body, expected_inference_sha256)):
        if type(raw) is not bytes or type(pin) is not str or not re.fullmatch(r'[0-9a-f]{64}', pin) or _sha(raw) != pin:
            raise WSChainProjectionError('frame_pin_mismatch')
    if http_request_compression != 'zstd':
        raise WSChainProjectionError('comparison_requires_registered_cli_zstd')
    _completion(warmup_completion, expected_prior, warmup_output_item_count)
    warmup, ws = _parse(warmup_body)
    inference, ins = _parse(inference_body)
    for frame in (warmup, inference):
        if (not REQUIRED <= frame.keys() or frame.get('type') != 'response.create'
                or frame.get('store') is not False or frame.get('stream') is not True
                or type(frame.get('input')) is not list or not frame['input']
                or type(frame.get('model')) is not str or not frame['model']
                or frame.get('parallel_tool_calls') is not False or frame.get('tool_choice') != 'auto'
                or frame.get('include') != ['reasoning.encrypted_content']
                or not isinstance(frame.get('reasoning'), dict) or frame['reasoning'].get('context') != 'all_turns'
                or type(frame.get('prompt_cache_key')) is not str or not frame['prompt_cache_key']):
            raise WSChainProjectionError('not_complete_native_lite_frame')
    if warmup.get('generate') is not False or 'previous_response_id' in warmup:
        raise WSChainProjectionError('not_first_warmup')
    if ('generate' in inference and inference['generate'] is not True) or inference.get('previous_response_id') != expected_prior:
        raise WSChainProjectionError('not_bound_inference_delta')
    if not isinstance(warmup['input'][0], dict) or warmup['input'][0].get('type') != 'additional_tools':
        raise WSChainProjectionError('missing_lite_prefix')
    stable = (set(warmup) | set(inference)) - DELTA_FIELDS
    for field in stable:
        if field not in warmup or field not in inference or _canonical(warmup[field]) != _canonical(inference[field]):
            raise WSChainProjectionError('non_input_control_mismatch')
    wm, we, _, _ = _metadata(warmup, warmup_body, ws, 'prewarm')
    im, ie, im_raw, im_spans = _metadata(inference, inference_body, ins, 'turn')
    for key in ('session_id', 'thread_id', 'x-codex-installation-id', 'x-codex-window-id'):
        if wm.get(key) != im.get(key):
            raise WSChainProjectionError('chain_session_identity_changed')
    rows, headers = _headers(ws_head)
    for header, field in (('session-id', 'session_id'), ('thread-id', 'thread_id'), ('x-client-request-id', 'thread_id'), ('x-codex-window-id', 'x-codex-window-id')):
        if headers.get(header) != im[field].encode():
            raise WSChainProjectionError('handshake_session_metadata_mismatch')
    if EMBEDDED not in headers:
        raise WSChainProjectionError('missing_warmup_compatibility_header')
    head_metadata, _ = _parse(headers[EMBEDDED])
    if _canonical(head_metadata) != _canonical(_compatibility_metadata(we)):
        raise WSChainProjectionError('handshake_warmup_metadata_mismatch')
    # Preserve inference frame values, including current turn metadata. Remove
    # only WS-specific wrappers and project WS header metadata back to headers.
    wi = warmup_body[slice(*ws['input'])][1:-1]
    ii = inference_body[slice(*ins['input'])][1:-1]
    complete_input = b'[' + wi + b',' + ii + b']'
    http_metadata = _assembled(im_raw, im_spans, remove={LITE, STAMP, *TRACE})
    http_body = _assembled(inference_body, ins, remove=WS_FIELDS,
                           replace={'input': complete_input, 'client_metadata': http_metadata})
    if len(http_body) > MAX_BODY:
        raise WSChainProjectionError('derived_body_limit')
    projected, projected_spans = _parse(http_body)
    if _canonical(projected['input']) != _canonical(warmup['input'] + inference['input']):
        raise WSChainProjectionError('input_concatenation_failed')
    # Fixed core fields use its struct order. Source also flattens bounded extra
    # string metadata. Preserve that supported extension, never drop it silently.
    compat = _compatibility_metadata(ie)
    compat_bytes = _ascii_json(compat)
    if len(compat_bytes) > 65536:
        raise WSChainProjectionError('compatibility_header_limit')
    replacements = {EMBEDDED: compat_bytes, 'x-openai-internal-codex-responses-lite': b'true',
                    'accept': b'text/event-stream', 'content-type': b'application/json'}
    for source, header in TRACE.items():
        if source in im:
            encoded = im[source].encode()
            if any(c < 32 or c > 126 for c in encoded) or not encoded:
                raise WSChainProjectionError('invalid_trace_projection')
            replacements[header] = encoded
    for header, field in (('x-codex-parent-thread-id', 'x-codex-parent-thread-id'), ('x-openai-subagent', 'x-openai-subagent')):
        if field in im:
            replacements[header] = im[field].encode()
        elif header in headers:
            raise WSChainProjectionError('compatibility_header_missing_inference_metadata')
    out_rows, seen = [], set()
    for name, lower, raw in rows:
        if lower in DROP_HEADERS | {'openai-beta', 'traceparent', 'tracestate'}:
            continue
        value = replacements.get(lower, raw)
        out_rows.append([name, base64.b64encode(value).decode()])
        seen.add(lower)
    for name, raw in replacements.items():
        if name not in seen:
            out_rows.append([name, base64.b64encode(raw).decode()])
    http_head = {'schema': 'codex-quality-captured-request-v1', 'method': 'POST', 'path': PATH,
                 'headers': out_rows, 'request_compression': 'zstd', 'body_sha256': _sha(http_body), 'body_bytes': len(http_body)}
    evidence = {'schema': SCHEMA, 'source_cli_version': '0.155.0', 'derived_not_captured_original_http': True,
        'model_calls': 0, 'network_calls': 0, 'warmup_sha256': _sha(warmup_body), 'inference_sha256': _sha(inference_body),
        'source_ws_head_canonical_sha256': _sha(_canonical(ws_head)), 'synthetic_prior_sha256': _sha(expected_prior),
        'synthetic_completion_sha256': _sha(json.dumps(warmup_completion, sort_keys=True, separators=(',', ':')).encode()),
        'http_body_sha256': _sha(http_body), 'http_head_canonical_sha256': _sha(_canonical(http_head)),
        'warmup_input_items': len(warmup['input']), 'inference_delta_items': len(inference['input']),
        'derived_input_items': len(projected['input']), 'input_formula': 'warmup.input + [] observed warmup output items + inference.input',
        'warmup_output_item_count': 0, 'input_canonical_sha256': _sha(_canonical(projected['input'])),
        'removed_top_level': sorted(set(inference) & WS_FIELDS), 'removed_ws_metadata': sorted(set(im) & {LITE, STAMP, *TRACE}),
        'preserved_inference_field_values': {}, 'preserved_metadata_values': {},
        'http_compatibility_header': {'basis': 'inference_embedded_turn_metadata; remove tool_namespaces_info; fixed struct order plus validated extra strings',
                                     'sha256': _sha(compat_bytes), 'from_warmup_handshake_changed': headers.get(EMBEDDED) != compat_bytes,
                                     'source_tool_inventory_present': 'tool_namespaces_info' in ie},
        'header_changes': [], 'response_scoped_differences': [], 'limits': [
            'derived HTTP request is not an actual captured CLI HTTP request',
            'WS true warmup is an extra server action and may change backend state; HTTP has one full-input request',
            'input prefix may contain old internal message metadata ignored by CLI prefix equality; captured bytes are preserved',
            'compression and connection protocols differ; no raw TLS equivalence claim',
            'synthetic prior must never be sent to official servers; actual WS chain binds to actual warmup response']}
    for field in inference.keys() - WS_FIELDS - {'input', 'client_metadata'}:
        source = inference_body[slice(*ins[field])]
        actual = http_body[slice(*projected_spans[field])]
        if source != actual:
            raise WSChainProjectionError('unexpected_value_mutation')
        evidence['preserved_inference_field_values'][_label(field, PUBLIC_BODY_FIELDS)] = _sha(source)
    for key in im.keys() - {LITE, STAMP, *TRACE}:
        evidence['preserved_metadata_values'][_label(key, PUBLIC_METADATA)] = _sha(im_raw[slice(*im_spans[key])])
    for field in ('stream_options', 'access_programs'):
        if _canonical(warmup.get(field)) != _canonical(inference.get(field)) or (field in warmup) != (field in inference):
            evidence['response_scoped_differences'].append(field)
    out = {name.lower(): base64.b64decode(value) for name, value in out_rows}
    for name in sorted(set(headers) | set(out)):
        if headers.get(name) != out.get(name):
            evidence['header_changes'].append({'name': _label(name, APP_HEADERS | DROP_HEADERS | {EMBEDDED, 'openai-beta', *TRACE.values(), 'x-openai-internal-codex-responses-lite'}),
                'before_sha256': _sha(headers[name]) if name in headers else None, 'after_sha256': _sha(out[name]) if name in out else None})
    return http_body, deepcopy(http_head), evidence


__all__ = ['WSChainProjectionError', 'derive_http_comparison']
