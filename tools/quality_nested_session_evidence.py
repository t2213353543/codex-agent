"""Bounded hash-only evidence for JSON carried inside turn-metadata strings.

The caller supplies its existing exact-number type/value summarizer so hashes
have the same meaning as its enclosing request evidence. No I/O or raw values.
"""
import hashlib
import json

MAX_BYTES = 1 << 20
MAX_FIELDS = 256
MAX_NODES = 16384
MAX_DEPTH = 32
KNOWN_NAMES = frozenset(('session_id', 'session-id', 'thread_id', 'thread-id',
    'installation_id', 'x-codex-installation-id', 'turn_id', 'parent_turn_id',
    'root_turn_id', 'window_id', 'x-codex-window-id', 'window_number',
    'context_window_id', 'request_id', 'request_kind', 'parent_thread_id',
    'turn_started_at_unix_ms', 'tool_namespaces_info', 'workspaces', 'sandbox',
    'sandbox_mode', 'auto_review_enabled', 'node_repl_auto_review_required',
    'node_repl_disabled', 'subagent_kind', 'thread_source', 'turn_trigger'))


class NestedMetadataError(ValueError):
    pass


def _unique(pairs):
    result = {}
    for name, value in pairs:
        if name in result:
            raise NestedMetadataError('duplicate_json_member')
        result[name] = value
    return result


def _nonfinite(_):
    raise NestedMetadataError('nonfinite_json_number')


def summarize_nested_session_evidence(raw, *, number_factory, value_summary):
    """Parse a UTF-8 JSON object, returning complete top-level field hashes.

None means absent. Invalid/duplicate/oversize metadata remains unchanged on the
wire; this observer returns an explicit incomplete receipt, not partial proof.
"""
    result = {'schema': 'codex-nested-session-evidence-v1', 'present': raw is not None,
              'complete': False, 'basis': 'canonical_JSON_hash_same_as_enclosing_summary', 'fields': {}}
    if raw is None:
        result.update(complete=True, parse_status='absent')
        return result
    if type(raw) is not bytes:
        result.update(parse_status='invalid', error='not_utf8_bytes')
        return result
    result.update(source_utf8_sha256=hashlib.sha256(raw).hexdigest(), source_utf8_bytes=len(raw))
    if len(raw) > MAX_BYTES:
        result.update(parse_status='invalid', error='nested_metadata_size_limit')
        return result
    try:
        obj = json.loads(raw.decode('utf-8'), object_pairs_hook=_unique,
                         parse_int=number_factory, parse_float=number_factory, parse_constant=_nonfinite)
        if not isinstance(obj, dict):
            raise NestedMetadataError('nested_metadata_not_object')
        if len(obj) > MAX_FIELDS:
            raise NestedMetadataError('nested_metadata_field_limit')
        pending, nodes = [(obj, 0)], 0
        while pending:
            value, depth = pending.pop()
            nodes += 1
            if depth > MAX_DEPTH or nodes > MAX_NODES:
                raise NestedMetadataError('nested_metadata_structure_limit')
            if isinstance(value, dict):
                for name, item in value.items():
                    name.encode('utf-8')
                    pending.append((item, depth + 1))
            elif isinstance(value, list):
                pending.extend((item, depth + 1) for item in value)
            elif isinstance(value, str):
                value.encode('utf-8')
        fields = {}
        for name in sorted(obj):
            digest = hashlib.sha256(name.encode()).hexdigest()
            label = name if name in KNOWN_NAMES else 'sha256:' + digest
            fields[label] = dict(value_summary(obj[name]), present=True, name_sha256=digest)
        result.update(value_summary(obj), fields=fields, field_count=len(obj), complete=True, parse_status='object')
    except NestedMetadataError as error:
        result.update(parse_status='invalid', error=error.args[0])
    except (ValueError, UnicodeError, RecursionError, OverflowError, TypeError):
        result.update(parse_status='invalid', error='invalid_nested_metadata_json')
    return result
