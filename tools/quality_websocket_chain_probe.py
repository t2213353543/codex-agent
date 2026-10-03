"""One native WS connection: exact captured warmup, then its captured delta.

No credential discovery, retries, refresh, fallback, or HTTP-to-WS conversion.
Only existing request-start timestamps, the inference previous_response_id, and
source-defined server turn-state metadata can change. Raw server IDs stay in memory.
"""
from __future__ import annotations

import base64
import json
import math
import os
from pathlib import Path
import re
import secrets
import socket
import struct
import subprocess
import tempfile
import time

from quality_exact_websocket_probe import (ExactProbeError, MAX_BODY, MAX_HEAD, MAX_RESPONSE,
    PATH, WebSocketCollector, _error_metadata, _frame, _handshake, _invalidate, _json,
    _origin, _pairs, _ready_line, deadline_io, file_digest, sha, summarize_body,
    summarize_head, write_all, read_exact)
from quality_transport_pair import HTTP_FIELDS, TIMING_METADATA, TransportPairError, _object, _canonical

SCHEMA = 'codex-quality-websocket-chain-v1'
TURN_STATE = 'x-codex-turn-state'


class _TurnState:
    """CLI OnceLock: handshake first, then response.metadata headers only."""
    def __init__(self, collectors):
        self.value = None
        self.source = None
        self.collectors = collectors

    def observe(self, value, source):
        if isinstance(value, list):
            if not value or not all(type(item) is str for item in value):
                raise ExactProbeError('invalid_turn_state_metadata')
            value = value[0]
        if type(value) is not str or len(value.encode()) > 16384 or any(ord(c) < 32 or ord(c) == 127 for c in value):
            raise ExactProbeError('invalid_turn_state_metadata')
        if value:
            for collector in self.collectors:
                collector.sse.hidden = tuple(collector.sse.hidden) + (value,)
        if self.value is None:
            self.value, self.source = value, source

    def event(self, event):
        if not isinstance(event, dict) or event.get('type') != 'response.metadata':
            return
        headers = event.get('headers')
        if headers is None:
            return
        if not isinstance(headers, dict):
            raise ExactProbeError('invalid_turn_state_metadata')
        matches = [value for name, value in headers.items() if name.lower() == TURN_STATE]
        if len(matches) > 1:
            raise ExactProbeError('ambiguous_turn_state_metadata')
        if matches:
            self.observe(matches[0], 'response.metadata.headers')

    def report(self):
        return {'observed': self.value is not None, 'source': self.source,
                'sha256': sha(self.value.encode()) if self.value is not None else None,
                'bytes': len(self.value.encode()) if self.value is not None else 0,
                'policy': 'first_valid_value_OnceLock_including_empty_string'}


def _response_id(value):
    if (type(value) is not str or not 1 <= len(value) <= 1024
            or any(ord(char) < 32 or ord(char) == 127 for char in value)):
        raise ExactProbeError('invalid_chain_response_id')
    try:
        value.encode('utf-8')
    except UnicodeError:
        raise ExactProbeError('invalid_chain_response_id') from None
    return value


def _timing_span(raw, fields):
    try:
        first, last = fields['client_metadata']
        values, spans = _object(raw[first:last])
        start, end = spans[TIMING_METADATA]
        value = values[TIMING_METADATA]
        if (type(value) is not str or not re.fullmatch(r'[0-9]{1,16}', value)
                or not 1 <= int(value) <= 9007199254740991):
            raise ValueError()
        return first + start, first + end
    except (KeyError, ValueError, TypeError, TransportPairError):
        raise ExactProbeError('invalid_existing_request_start_timestamp') from None


def validate_chain(warmup, inference, captured, expected_previous_response_id, restamp_timing):
    if type(restamp_timing) is not bool:
        raise ExactProbeError('invalid_timing_policy')
    _response_id(expected_previous_response_id)
    if (not isinstance(captured, dict) or set(captured) - {'schema', 'method', 'path', 'headers'}
            or captured.get('method') != 'GET' or captured.get('path') != PATH):
        raise ExactProbeError('invalid_handshake_contract')
    _pairs(captured.get('headers'), request=True)
    result = []
    for index, raw in enumerate((warmup, inference)):
        if not isinstance(raw, bytes) or not 0 < len(raw) <= MAX_BODY:
            raise ExactProbeError('invalid_request_body_size')
        value = _json(raw)  # Bounded depth, duplicates, scalar types and UTF-8.
        if (not isinstance(value, dict) or set(value) - (HTTP_FIELDS | {'type', 'generate', 'previous_response_id'})
                or value.get('type') != 'response.create' or type(value.get('model')) is not str or not value['model']
                or value.get('stream') is not True or value.get('store') is not False
                or not isinstance(value.get('input'), list) or not isinstance(value.get('client_metadata'), dict)
                or type(value.get('prompt_cache_key')) is not str or not value['prompt_cache_key']
                or any(type(item) is not str for item in value['client_metadata'].values())):
            raise ExactProbeError('invalid_captured_chain_request')
        if index == 0:
            if value.get('generate') is not False or value.get('previous_response_id') not in (None, ''):
                raise ExactProbeError('requires_first_warmup_generate_false')
        elif (value.get('previous_response_id') != expected_previous_response_id
                or value.get('generate') is not None and value.get('generate') is not True):
            raise ExactProbeError('requires_pinned_inference_previous_response')
        try:
            _, fields = _object(raw)
        except TransportPairError:
            raise ExactProbeError('invalid_request_json_spans') from None
        if restamp_timing:
            _timing_span(raw, fields)
        result.append(value)
    exact_values = [_object(raw)[0] for raw in (warmup, inference)]
    for field in ('model', 'instructions', 'tools', 'tool_choice', 'parallel_tool_calls',
                  'reasoning', 'store', 'stream', 'include', 'service_tier', 'prompt_cache_key', 'text'):
        if _canonical(exact_values[0].get(field)) != _canonical(exact_values[1].get(field)):
            raise ExactProbeError('chain_reuse_properties_mismatch')
    # Frame metadata may differ by request kind, turn IDs and timestamps. The
    # session, when supplied, must still refer to the same captured conversation.
    for field in ('session_id', 'session-id', 'thread_id', 'thread-id'):
        values = [value['client_metadata'].get(field) for value in result]
        if values[0] != values[1]:
            raise ExactProbeError('chain_session_mismatch')
    return result


def project_frame(raw, *, previous=None, expected_previous=None, restamp_timing=True, turn_state=None):
    """Replace only authorized existing UTF-8 JSON value spans, never serialize the tree."""
    try:
        value, fields = _object(raw)
    except TransportPairError:
        raise ExactProbeError('invalid_request_json_spans') from None
    edits = []
    stamp = None
    if previous is not None:
        _response_id(previous)
        if value.get('previous_response_id') != expected_previous:
            raise ExactProbeError('previous_response_template_changed')
        first, last = fields['previous_response_id']
        edits.append((first, last, json.dumps(previous, ensure_ascii=False).encode(), 'previous_response_id'))
    if restamp_timing:
        first, last = _timing_span(raw, fields)
        stamp = int(time.time() * 1000)
        if not 1 <= stamp <= 9007199254740991:
            raise ExactProbeError('invalid_request_clock')
        edits.append((first, last, json.dumps(str(stamp)).encode(), 'request_start_timestamp'))
    if turn_state is not None:
        first, last = fields['client_metadata']
        metadata, spans = _object(raw[first:last])
        encoded = json.dumps(turn_state, ensure_ascii=False).encode()
        if TURN_STATE in spans:
            start, end = spans[TURN_STATE]
            edits.append((first+start, first+end, encoded, 'turn_state_value'))
        else:
            # Insert a single member at the closing brace. Every pre-existing
            # whitespace, escape spelling, numeric lexeme and field stays exact.
            end = first + raw[first:last].rfind(b'}')
            insertion = (b',' if metadata else b'') + json.dumps(TURN_STATE).encode() + b':' + encoded
            edits.append((end, end, insertion, 'turn_state_member_insert'))
    edits.sort()
    pieces, evidence, segments, position = [], [], [], 0
    for first, last, replacement, field in edits:
        if first < position or first > last:
            raise ExactProbeError('overlapping_request_edits')
        unchanged = raw[position:first]
        pieces.extend((unchanged, replacement))
        segments.append({'bytes': len(unchanged), 'sha256': sha(unchanged)})
        evidence.append({'field': field, 'original_value_sha256': sha(raw[first:last]),
                         'next_value_sha256': sha(replacement), 'original_value_bytes': last-first,
                         'next_value_bytes': len(replacement)})
        position = last
    pieces.append(raw[position:])
    segments.append({'bytes': len(raw)-position, 'sha256': sha(raw[position:])})
    updated = b''.join(pieces)
    if len(updated) > MAX_BODY:
        raise ExactProbeError('projected_request_limit')
    _json(updated)
    return updated, {'template_sha256': sha(raw), 'body_sha256': sha(updated), 'body_bytes': len(updated),
                     'allowed_value_span_edits': evidence, 'unchanged_segments': segments,
                     'nonedited_bytes_preserved': True, 'request_start_ms': stamp}


def _warmup_valid(collector):
    result = collector.report(101)
    response = collector.sse.response
    if (result['protocol_success'] is not True or response.get('output') != []
            or collector.sse.done_item_count or collector.sse.done_messages
            or any(name.startswith(('response.output_', 'response.reasoning_', 'response.function_', 'response.custom_tool_'))
                   for name in collector.sse.counts)):
        raise ExactProbeError('warmup_did_not_complete_without_output')
    identity = _response_id(response.get('id'))
    usage = response.get('usage')
    if usage is not None:
        if not isinstance(usage, dict) or any(type(usage.get(key)) is not int or not 0 <= usage[key] <= 9223372036854775807
                for key in ('input_tokens', 'output_tokens', 'total_tokens')):
            raise ExactProbeError('invalid_warmup_usage')
        if usage['output_tokens'] != 0:
            raise ExactProbeError('warmup_generated_output_tokens')
        details = usage.get('output_tokens_details')
        if details is not None and (not isinstance(details, dict) or type(details.get('reasoning_tokens')) is not int
                or details['reasoning_tokens'] != 0):
            raise ExactProbeError('warmup_generated_reasoning_or_invalid_usage')
        details = usage.get('input_tokens_details')
        if details is not None and (not isinstance(details, dict)
                or any(type(details.get(key, 0)) is not int or not 0 <= details.get(key, 0) <= 9223372036854775807
                       for key in ('cached_tokens', 'cache_write_tokens'))
                or 'cached_tokens' not in details):
            raise ExactProbeError('invalid_warmup_input_usage')
    return identity


def _stage(name, raw):
    return {'phase': name, 'template_sha256': sha(raw), 'template_bytes': len(raw),
            'native_dispatch_count': 0, 'ipc_dispatch_attempted': False, 'ipc_bytes_accepted': 0,
            'ws_write_acknowledged': False, 'protocol_success': False, 'eligible_for_scoring': False}


def _read_stage(stream, deadline, collector, stage, sequence, send_sequence, start, turn_state, forbidden_id=None):
    recv = lambda size: deadline_io(stream, deadline, 'recv', size)
    ack = False
    while True:
        kind, raw = _frame(recv)
        if kind == 66:
            received, message_type = struct.unpack('!QB', raw[:9])
            if received != sequence + 1 or message_type != 1:
                raise ExactProbeError('invalid_message_sequence_or_kind')
            sequence = received
            event = _json(raw[9:])
            turn_state.event(event)
            if forbidden_id is not None and isinstance(event, dict):
                nested = event.get('response')
                if event.get('response_id') == forbidden_id or isinstance(nested, dict) and nested.get('id') == forbidden_id:
                    raise ExactProbeError('warmup_response_reused_for_inference')
            collector.feed(raw[9:])
            stage.setdefault('first_message_latency_ms', round((time.monotonic()-start)*1000))
            stage['ws_received_sequence_last'] = sequence
            if collector.sse.terminal is not None:
                stage['terminal_latency_ms'] = round((time.monotonic()-start)*1000)
        elif kind == 70:
            if ack or struct.unpack('!Q', raw)[0] != send_sequence:
                raise ExactProbeError('invalid_or_duplicate_write_ack')
            ack = True
            stage.update(ws_write_acknowledged=True, ws_ack_sequence=send_sequence,
                         write_ack_latency_ms=round((time.monotonic()-start)*1000))
        elif kind == 68:
            stage['native_error'] = _error_metadata(raw, True)
            raise ExactProbeError('native_message_error')
        elif kind == 67:
            value = _json(raw)
            if not isinstance(value, dict) or type(value.get('reason')) is not str:
                raise ExactProbeError('invalid_native_close')
            code = value.get('code')
            if code is not None and (type(code) is not int or not 1000 <= code <= 4999):
                raise ExactProbeError('invalid_native_close')
            stage['ws_close'] = {'code': code, 'reason_sha256': sha(value['reason'].encode())}
            raise ExactProbeError('closed_before_completed_and_ack')
        elif kind == 69:
            raise ExactProbeError('end_before_completed_and_ack')
        else:
            raise ExactProbeError('unexpected_frame_after_handshake')
        if collector.sse.terminal is not None and ack:
            stage.update(collector.report(101))
            stage['receipt_complete'] = True
            return sequence


def _exchange(stream, bodies, captured, head, report, collectors, deadline, expected_previous, restamp):
    send = lambda raw: deadline_io(stream, deadline, 'send', raw)
    recv = lambda size: deadline_io(stream, deadline, 'recv', size)
    encoded = json.dumps(head, separators=(',', ':')).encode()
    if len(encoded) > MAX_HEAD:
        raise ExactProbeError('ipc_head_limit')
    report.update(handshake_dispatch_attempted=True, handshake_ipc_bytes_accepted=0)
    def accepted(part):
        report['handshake_ipc_bytes_accepted'] += len(part)
    write_all(send, struct.pack('!I', len(encoded)) + encoded, accepted)
    report.update(handshake_dispatch_count=1, handshake_ipc_write_complete=True)
    kind, raw = _frame(recv)
    if kind == 68:
        report['native_error'] = _error_metadata(raw, False)
        raise ExactProbeError('native_handshake_error')
    if kind != 65:
        raise ExactProbeError('frame_before_handshake')
    handshake, hashes, tail = _handshake(raw)
    status = handshake['status']
    report.update(http_status=status, handshake_status=status, response_header_hashes=hashes,
                  handshake_latency_ms=round((time.monotonic()-report['_started'])*1000),
                  handshake_body_complete=None, handshake_body_scope=handshake['body_scope'])
    if status != 101:
        report.update(handshake_error_body_sha256=sha(tail), handshake_error_body_bytes=len(tail),
                      handshake_tail_truncated=handshake['tail_truncated'])
        if tail:
            try:
                parsed = _json(tail)
                if isinstance(parsed, dict):
                    collectors[0].sse.data = [json.dumps({'type':'error','error':parsed.get('error',{})}).encode()]
                    collectors[0].sse.dispatch()
                    report['response_error'] = collectors[0].sse.error
            except ExactProbeError:
                pass
        if _frame(recv)[0] != 69:
            raise ExactProbeError('rejected_handshake_missing_end')
        _invalidate(report, 'handshake_rejected', 'handshake_rejected')
        return
    report['websocket_connections_accepted'] = 1
    turn_state = _TurnState(collectors)
    matches = [pair for pair in handshake['headers'] if pair[0].lower() == TURN_STATE]
    if len(matches) > 1:
        raise ExactProbeError('ambiguous_turn_state_handshake')
    if matches:
        try:
            turn_state.observe(base64.b64decode(matches[0][1], validate=True).decode(), 'handshake_header')
        except (ValueError, UnicodeError):
            raise ExactProbeError('invalid_turn_state_handshake') from None
    sequence, previous = 0, None
    for index, name in enumerate(('warmup', 'inference')):
        stage = report[name]
        report['active_phase'] = name
        updated, projection = project_frame(bodies[index], previous=previous if index else None,
            expected_previous=expected_previous if index else None, restamp_timing=restamp, turn_state=turn_state.value)
        stage['turn_state_at_send'] = turn_state.report()
        stage.update(projection=projection, request_body_sha256=sha(updated), request_body_bytes=len(updated),
                     request_start_ms=projection['request_start_ms'],
                     request_start_ms_scope='existing_value_replaced_immediately_before_send' if restamp else 'captured_value_unchanged')
        if index:
            report.update(request_body_sha256=sha(updated), request_body_bytes=len(updated))
        wire = struct.pack('!BIQB', 82, len(updated)+9, index+1, 1) + updated
        start = time.monotonic()
        stage.update(ipc_dispatch_attempted=True, send_sequence=index+1,
                     dispatch_start_elapsed_ms=round((start-report['_started'])*1000))
        def accepted(part):
            stage['ipc_bytes_accepted'] += len(part)
        write_all(send, wire, accepted)
        stage.update(native_dispatch_count=1, local_ipc_write_complete=True,
                     actual_ipc_body_sha256=sha(updated), actual_ipc_request_sha256=sha(wire))
        if index:
            report['actual_ipc_body_sha256'] = sha(updated)
        report['native_dispatch_count'] += 1
        report['ws_send_count'] += 1
        sequence = _read_stage(stream, deadline, collectors[index], stage, sequence, index+1, start, turn_state, previous)
        report['turn_state'] = turn_state.report()
        report['ws_received_sequence'] = sequence
        if index == 0:
            previous = _warmup_valid(collectors[0])
            for collector in collectors:
                collector.sse.hidden = tuple(collector.sse.hidden) + (previous,)
            stage.update(no_generated_output_verified=True, eligible_for_scoring=False,
                         actual_response_id_sha256=sha(previous.encode()))
            report['previous_response_link_sha256'] = sha(previous.encode())
        else:
            report.update(collectors[1].report(101))
            report.update(request_body_sha256=stage['request_body_sha256'],
                          request_body_bytes=stage['request_body_bytes'],
                          actual_ipc_body_sha256=stage['actual_ipc_body_sha256'])
            report.update(eligible_for_scoring=report['protocol_success'] and report['generated_answer_available'],
                          receive_stop_reason='inference_terminal_and_write_ack_observed',
                          later_transport_events='not_observed_after_terminal_and_ack',
                          same_websocket_connection=True, warmup_receipt_verified=True)


def native_websocket_chain(helper, expected_helper_sha, warmup_body, inference_body, captured, token,
                           workspace, hidden=(), timeout=240, upstream='https://chatgpt.com', *, deadline=None,
                           expected_previous_response_id='synthetic_capture_warmup', restamp_timing=True):
    """One helper, one WS handshake, one warmup and at most one inference.

    Dispatch counts describe complete native IPC SEND writes, not a claim of
    server acceptance. Absolute deadline includes startup and reserved cleanup.
    All original JSON outside two permitted value spans remains byte-exact.
    """
    started = time.monotonic()
    report = {'schema': SCHEMA, 'transport': 'websocket', 'attempts': 0, 'native_dispatch_count': 0,
              'ws_send_count': 0, 'handshake_dispatch_count': 0, 'websocket_connections_accepted': 0,
              'automatic_retries': False, 'automatic_refresh': False, 'http_fallback': False,
              'protocol_success': False, 'generated_answer_available': False, 'answer': None, 'final_output': '',
              'eligible_for_scoring': False, 'http_status': None, 'handshake_status': None,
              'response_completion_observed': False, 'helper_process_starts': 0, 'active_phase': 'preflight',
              'scope_id': secrets.token_hex(16), 'upstream_wire_capture': False,
              'dispatch_count_scope': 'complete_native_IPC_SEND_writes_not_upstream_acceptance',
              'environment_policy': 'isolated_PATH_LANG_no_proxy_or_CA_overrides', '_started': started}
    process = stream = temporary = path = None
    collectors = []
    end = started + 240
    try:
        if (type(timeout) not in (int, float) or not math.isfinite(timeout) or not 1 <= timeout <= 240
                or deadline is not None and (type(deadline) not in (int, float) or not math.isfinite(deadline))):
            raise ExactProbeError('invalid_deadline')
        end = min(started+timeout, deadline) if deadline is not None else started+timeout
        if end-started < 1:
            raise ExactProbeError('deadline_before_start')
        operation_end = end-min(5, (end-started)/4)
        if os.name != 'posix' or not hasattr(socket, 'AF_UNIX'):
            raise ExactProbeError('linux_posix_required')
        if type(expected_helper_sha) is not str or not re.fullmatch(r'[0-9a-f]{64}', expected_helper_sha):
            raise ExactProbeError('invalid_helper_digest')
        helper = Path(helper)
        if file_digest(helper) != expected_helper_sha:
            raise ExactProbeError('helper_digest_mismatch')
        _origin(upstream)
        validate_chain(warmup_body, inference_body, captured, expected_previous_response_id, restamp_timing)
        captured = json.loads(json.dumps(captured))
        if (type(token) is not str or not token or len(token)>64<<10 or not token.isascii()
                or any(ord(char)<=32 or ord(char)==127 for char in token)
                or type(workspace) is not str or not re.fullmatch(r'[A-Za-z0-9_-]{1,160}', workspace)):
            raise ExactProbeError('invalid_credential_input')
        hidden = tuple(dict.fromkeys([token, workspace, expected_previous_response_id]+
                    [value for value in hidden if isinstance(value,str) and value]))
        collectors = [WebSocketCollector(hidden), WebSocketCollector(hidden)]
        report.update(helper_sha256=expected_helper_sha, token_sha256=sha(token.encode()),
                      workspace_sha256=sha(workspace.encode()), warmup=_stage('warmup', warmup_body),
                      inference=_stage('inference', inference_body),
                      expected_previous_response_id_sha256=sha(expected_previous_response_id.encode()),
                      restamp_timing=restamp_timing)
        temporary = tempfile.TemporaryDirectory(prefix='qchainws-')
        os.chmod(temporary.name,0o700)
        path = str(Path(temporary.name)/'s')
        ipc_token = secrets.token_hex(32)
        startup = {'protocol':1,'unix_socket':path,'upstream':upstream,'ipc_token':ipc_token,
                   'headers_timeout_ms':max(1,min(90000,int((operation_end-time.monotonic())*1000))),
                   'idle_timeout_ms':0,'http_client_lifecycle':'cli_request'}
        process = subprocess.Popen([str(helper)],stdin=subprocess.PIPE,stdout=subprocess.PIPE,
            stderr=subprocess.DEVNULL,close_fds=True,bufsize=0,
            env={'PATH':os.environ.get('PATH','/usr/bin:/bin'),'LANG':'C.UTF-8'})
        report.update(helper_process_starts=1, helper_pid=process.pid)
        encoded = json.dumps(startup,separators=(',',':')).encode()
        write_all(process.stdin.write,struct.pack('!I',len(encoded))+encoded)
        remaining = operation_end-time.monotonic()
        if remaining<=0:
            raise TimeoutError()
        ready = _json(_ready_line(process.stdout,min(9,remaining)))
        if (not isinstance(ready,dict) or type(ready.get('protocol')) is not int or ready['protocol']!=1
                or ready.get('transport')!='unix' or ready.get('unix_socket')!=path
                or ready.get('http_client_lifecycle')!='cli_request'
                or not isinstance(ready.get('capabilities'),list) or 'websocket_message_v1' not in ready['capabilities']):
            raise ExactProbeError('helper_readiness_mismatch')
        runtime = file_digest(Path('/proc')/str(process.pid)/'exe')
        report['runtime_executable_sha256']=runtime
        if runtime!=expected_helper_sha:
            raise ExactProbeError('helper_runtime_mismatch')
        headers = [list(pair) for pair in captured['headers']]
        headers.extend([['Authorization',base64.b64encode(('Bearer '+token).encode()).decode()],
                        ['ChatGPT-Account-Id',base64.b64encode(workspace.encode()).decode()]])
        head={'protocol':1,'ipc_token':ipc_token,'operation':'websocket_message_v1','method':'GET','path':PATH,
              'headers':headers,'body_length':0,'max_message_bytes':min(MAX_BODY,MAX_RESPONSE),
              'write_timeout_ms':max(1,min(60000,int((operation_end-time.monotonic())*1000)))}
        head_summary,sensitive=summarize_head(head)
        report.update(request_header_summary=head_summary,handshake_nonauth_header_hashes=_pairs(captured['headers'],request=True))
        report['warmup']['template_summary']=summarize_body(warmup_body,sensitive)
        report['inference']['template_summary']=summarize_body(inference_body,sensitive)
        stream=socket.socket(socket.AF_UNIX,socket.SOCK_STREAM)
        remaining=operation_end-time.monotonic()
        if remaining<=0:
            raise TimeoutError()
        stream.settimeout(remaining);stream.connect(path)
        report['attempts']=1
        _exchange(stream,(warmup_body,inference_body),captured,head,report,collectors,operation_end,
                  expected_previous_response_id,restamp_timing)
    except Exception as error:
        for index,name in enumerate(('warmup','inference')):
            if len(collectors)>index:
                try:
                    evidence=collectors[index].report(report.get('handshake_status'))
                    report[name].update(evidence)
                    if name=='inference' and collectors[index].sse.terminal is not None:
                        report.update(evidence)
                except Exception:
                    pass
        category='timeout' if isinstance(error,(TimeoutError,socket.timeout)) else 'probe_error'
        code=error.args[0] if isinstance(error,ExactProbeError) else 'unexpected_probe_failure'
        name=report.get('active_phase')
        if name in ('warmup','inference'):
            _invalidate(report[name],category,code)
        _invalidate(report,category,code)
    finally:
        cleanup_ok=True
        if stream is not None:
            try: stream.close()
            except (OSError,ValueError): cleanup_ok=False
        if process is not None:
            try:
                process.stdin.close()
                process.wait(timeout=max(.001,min(2,end-time.monotonic())))
                report['helper_forced_kill']=False
            except (OSError,ValueError,subprocess.TimeoutExpired):
                cleanup_ok=False;report['helper_forced_kill']=True
                try:
                    process.kill();process.wait(timeout=max(.001,min(2,end-time.monotonic())))
                except (OSError,ValueError,subprocess.TimeoutExpired): pass
            try: process.stdout.close()
            except (OSError,ValueError): cleanup_ok=False
            report.update(helper_returncode=process.returncode,helper_reaped=process.poll() is not None)
            cleanup_ok=cleanup_ok and report['helper_reaped'] and process.returncode==0
        if temporary is not None:
            try: temporary.cleanup()
            except (OSError,ValueError): cleanup_ok=False
        report['local_socket_absent']=not Path(path).exists() if path else None
        if not cleanup_ok:
            _invalidate(report,'helper_cleanup_failed','helper_cleanup_failed')
        report.update(duration_ms=round((time.monotonic()-started)*1000),
                      inference_dispatch_count=report.get('inference',{}).get('native_dispatch_count',0),
                      warmup_dispatch_count=report.get('warmup',{}).get('native_dispatch_count',0),
                      native_create_count=report.get('native_dispatch_count',0),
                      actual_application_messages=report.get('native_dispatch_count',0),
                      inference_create_count=report.get('inference',{}).get('native_dispatch_count',0),
                      warmup_create_count=report.get('warmup',{}).get('native_dispatch_count',0))
        report.pop('_started',None)
    return report
