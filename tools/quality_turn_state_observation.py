"""Passive first-response-header observation; opaque state is never parsed or sent.

The single request orchestration below is copied from the SHA-pinned route probe
with only a local helper subclass and a response-header observer added. No frozen
module globals are patched. Native TLS, routing, request bytes, old response
parsing and cleanup remain with that implementation. Observation errors make the
sample ineligible; completed answer evidence is retained as observed_completion.
Absence is only known when the header observation is complete.
"""
from __future__ import annotations
import base64
import binascii
import json
import math
import os
import re
import secrets
import socket
import struct
import time
from pathlib import Path
import quality_route_lifecycle_probe as route_probe

base = route_probe.base
SCHEMA = route_probe.SCHEMA
RouteError = route_probe.RouteError
_route, _target, _SOCKSTunnel = route_probe._route, route_probe._target, route_probe._SOCKSTunnel
ROUTE_SOURCE_SHA256 = "138a021d11ac60183bf22f57ad1dcea6cdd3a317d98586685b1da99aa65c0527"
OBSERVATION_SCHEMA = "codex-quality-turn-state-observation-v1"
MAX_HEADERS = 32
MAX_VALUE_BYTES = 16 * 1024
_NAME = re.compile(r"[!#$%&'*+.^_`|~0-9A-Za-z-]{1,256}")


def summarize_turn_state_headers(headers):
    """Summarize IPC base64 header values, NOT the opaque state contents.

    All pairs are validated and bounded. Duplicate state headers remain ordered,
    including empty values. UTF-8 character length counts code points, not bytes,
    graphemes or base64-decoded state. No quality interpretation is made.
    """
    if type(headers) is not list or len(headers) > MAX_HEADERS:
        raise ValueError("turn_state_header_count_invalid")
    values = []
    for index, pair in enumerate(headers):
        if (type(pair) is not list or len(pair) != 2 or type(pair[0]) is not str
                or not _NAME.fullmatch(pair[0]) or type(pair[1]) is not str):
            raise ValueError("turn_state_header_pair_invalid")
        if len(pair[1]) > 4 * ((MAX_VALUE_BYTES + 2) // 3):
            raise ValueError("turn_state_header_value_limit")
        try:
            raw = base64.b64decode(pair[1], validate=True)
        except (ValueError, binascii.Error):
            raise ValueError("turn_state_header_base64_invalid") from None
        if len(raw) > MAX_VALUE_BYTES:
            raise ValueError("turn_state_header_value_limit")
        if pair[0].lower() != "x-codex-turn-state":
            continue
        try:
            decoded = raw.decode("utf-8", errors="strict")
            utf8_length = len(decoded)
        except UnicodeError:
            utf8_length = None
        values.append({"header_index": index, "value_bytes": len(raw),
                       "ascii_valid": raw.isascii(), "utf8_valid": utf8_length is not None,
                       "utf8_char_length": utf8_length, "value_sha256": base.sha(raw)})
    return {"schema": OBSERVATION_SCHEMA, "present": bool(values), "count": len(values),
            "headers_seen": len(headers), "values": values}


class _HeaderObserver:
    """Bounded tap of the first native frame only; never reads from a socket."""
    def __init__(self):
        self._buffer = bytearray()
        self._size = None
        self._done = False
        self._result = {"schema": OBSERVATION_SCHEMA, "complete": False,
                        "headers_frame_observed": False, "present": None, "count": None,
                        "headers_seen": None, "values": [], "error": None,
                        "scope": "first_native_response_headers_frame",
                        "opaque_state_parsed": False, "state_replayed": False,
                        "additional_requests": 0}

    def _fail(self, code):
        self._result["error"] = code
        self._buffer.clear()
        self._done = True

    def feed(self, chunk):
        if self._done:
            return
        position = 0
        if self._size is None:
            take = min(5 - len(self._buffer), len(chunk))
            self._buffer.extend(chunk[:take])
            position += take
            if len(self._buffer) != 5:
                return
            kind, self._size = struct.unpack("!BI", self._buffer)
            self._buffer.clear()
            if kind != 1:
                self._fail("first_frame_not_headers")
                return
            self._result["headers_frame_observed"] = True
            if self._size > base.MAX_HEAD:
                self._fail("native_header_frame_limit")
                return
        take = min(self._size - len(self._buffer), len(chunk) - position)
        self._buffer.extend(chunk[position:position + take])
        if len(self._buffer) != self._size:
            return
        try:
            value = base._json(bytes(self._buffer))
            if (type(value) is not dict or type(value.get("status")) is not int
                    or not 200 <= value["status"] <= 599
                    or value.get("version") not in ("HTTP/1.0", "HTTP/1.1", "HTTP/2.0", "HTTP/2", "HTTP/3.0")):
                raise ValueError("native_response_headers_invalid")
            summary = summarize_turn_state_headers(value.get("headers"))
        except ValueError as error:
            allowed = {"turn_state_header_count_invalid", "turn_state_header_pair_invalid",
                       "turn_state_header_value_limit", "turn_state_header_base64_invalid",
                       "native_response_headers_invalid"}
            self._fail(error.args[0] if error.args and error.args[0] in allowed else "native_response_headers_invalid")
            return
        except Exception:
            self._fail("native_response_headers_invalid")
            return
        self._result.update(summary, complete=True, http_status=value["status"], error=None)
        self._buffer.clear()
        self._done = True

    def finish(self):
        if not self._done:
            self._fail("native_headers_not_observed" if not self._buffer and self._size is None
                       else "native_header_frame_incomplete")

    def summary(self):
        self.finish()
        return dict(self._result, values=[dict(item) for item in self._result["values"]])


class _ObservedSocket:
    def __init__(self, stream, observer):
        self._stream, self._observer = stream, observer

    def recv(self, count, *args, **kwargs):
        raw = self._stream.recv(count, *args, **kwargs)
        if raw:
            self._observer.feed(raw)
        else:
            self._observer.finish()
        return raw

    def close(self):
        self._observer.finish()
        return self._stream.close()

    def __getattr__(self, name):
        return getattr(self._stream, name)


class _StateHelper(route_probe._RouteHelper):
    def __init__(self, *args, observer, **kwargs):
        super().__init__(*args, **kwargs)
        self._observer = observer

    def connect(self, phase):
        return _ObservedSocket(super().connect(phase), self._observer)


def native_state_request(helper,expected_helper_sha,body,captured,token,workspace,hidden=(),timeout=240,*,proxy_url=None,deadline=None,application_identity=None,upstream='https://chatgpt.com'):
    """Single exact first-turn POST. None proxy is an explicit direct control.

    Run this entire function in the desired namespace. No nsenter, DNS setup,
    CA injection or identity generation is performed here. Scheme/URL values
    stay private; the receipt contains only scheme and cryptographic summaries.
    """
    began=time.monotonic();report={'schema':SCHEMA,'transport':'http','mode':'cold','protocol_success':False,'eligible_for_scoring':False,'answer':None,'final_output':'','generated_answer_available':False,'response_completion_observed':False,'automatic_retries':False,'automatic_refresh':False,'http_fallback':False,'direct_fallback':False,'model_post_attempts':0,'native_dispatch_count':0,'metadata_get_count':0,'inference':{'native_dispatch_count':0},'helpers':[],'route':{},'upstream_wire_capture':False,'environment_policy':'PATH_LANG_only_explicit_IPC_route_no_proxy_environment_or_CA_override'}
    child=bridge=collector=None;end=began+240;observer=_HeaderObserver()
    try:
        if base.file_digest(Path(route_probe.__file__)) != ROUTE_SOURCE_SHA256:raise RouteError('route_source_digest_mismatch')
        if type(timeout) not in (int,float) or not math.isfinite(timeout) or not 1<=timeout<=240 or deadline is not None and (type(deadline) not in (int,float) or not math.isfinite(deadline)):raise RouteError('invalid_deadline')
        end=min(began+timeout,deadline) if deadline is not None else began+timeout
        if end-began<1:raise RouteError('deadline_before_start')
        operation_end=end-min(8,(end-began)/4)
        if os.name!='posix' or not hasattr(socket,'AF_UNIX'):raise RouteError('linux_posix_required')
        helper=Path(helper)
        if type(expected_helper_sha) is not str or not re.fullmatch('[a-f0-9]{64}',expected_helper_sha) or base.file_digest(helper)!=expected_helper_sha:raise RouteError('helper_digest_mismatch')
        route=_route(proxy_url);target=_target(upstream);identity=base._validate_capture(body,captured,application_identity)
        if type(token) is not str or not token or not token.isascii() or len(token)>65536 or any(ord(c)<=32 or ord(c)==127 for c in token) or type(workspace) is not str or not re.fullmatch('[A-Za-z0-9_-]{1,160}',workspace):raise RouteError('invalid_credential_input')
        private_route=[] if route is None else [proxy_url,route[1].decode(),route[2].decode()]
        hidden=tuple(dict.fromkeys([token,workspace]+[v for v in list(hidden)+private_route if type(v) is str and v]));collector=base.SSECollector(hidden)
        captured=json.loads(json.dumps(captured))
        report.update(helper_sha256=expected_helper_sha,request_body_sha256=base.sha(body),request_body_bytes=len(body),token_sha256=base.sha(token.encode()),workspace_sha256=base.sha(workspace.encode()),application_identity_sha256=base.sha(json.dumps(identity,sort_keys=True,separators=(',',':')).encode()),caller_uid=os.geteuid(),caller_network_namespace_sha256=base.sha(os.readlink('/proc/self/ns/net').encode()))
        public=report['route'];public.update(mode='direct' if route is None else 'explicit_proxy',explicit_proxy=route is not None,scheme='direct' if route is None else route[0].scheme,proxy_url_sha256=base.sha(proxy_url.encode()) if route else None,route_configuration_scope='native_Config.proxy',no_proxy_environment_ignored=True)
        native_proxy=proxy_url
        if route and route[0].scheme=='socks5h':
            bridge=_SOCKSTunnel(route,target,operation_end,end,public);native_proxy=bridge.start();public['route_configuration_scope']='native_HTTP_CONNECT_to_single_use_SOCKS5h_byte_tunnel'
        record={'role':'post','scope_id':secrets.token_hex(16),'process_started':False};report['helpers'].append(record)
        child=_StateHelper(helper,expected_helper_sha,upstream,operation_end,end,record,proxy_url=native_proxy,observer=observer);child.start()
        if captured['request_compression']=='zstd' and 'request_zstd_v1' not in child.capabilities:raise RouteError('helper_compression_unavailable')
        base._transaction(child,base.exact_head(captured,body,token,workspace,child.ipc_token,1),body,report['inference'],collector)
        report.update(base._collector_report(collector,report['inference'].get('http_status')));report.update(response_completion_observed=report['protocol_success'],eligible_for_scoring=report['protocol_success'] and report['generated_answer_available'])
    except Exception as error:
        if collector is not None:
            try:
                observed=base._collector_report(collector,report['inference'].get('http_status'));report.update(observed,response_completion_observed=observed['protocol_success'])
            except Exception:pass
        base._invalidate(report,'timeout' if isinstance(error,(TimeoutError,socket.timeout)) else 'probe_error',error.args[0] if isinstance(error,base.ExactProbeError) else 'route_probe_failed')
    finally:
        cleaned=True
        if child is not None:cleaned=child.close() and cleaned
        if bridge is not None:cleaned=bridge.close() and cleaned
        report['route'].update(connect_count=report['route'].get('local_connect_attempts') if bridge else None,connect_count_scope='accepted_local_CONNECT_tunnels' if bridge else 'native_transport_not_instrumented',tunnel_reaped=report['route'].get('bridge_joined') if bridge else None,route_cleanup_ok=cleaned)
        post=report['inference'];report.update(native_dispatch_count=post.get('native_dispatch_count',0),model_post_attempts=int(bool(post.get('ipc_dispatch_attempted'))),attempts=int(bool(post.get('ipc_dispatch_attempted'))),helper_reaped=bool(child) and child.record.get('reaped') is True,helper_forced_kill=bool(child) and child.record.get('forced_kill') is True,local_socket_absent=bool(child) and child.record.get('local_socket_absent') is True,duration_ms=round((time.monotonic()-began)*1000))
        for key in ('request_header_summary','request_body_summary','actual_ipc_body_sha256','local_ipc_write_complete','ipc_dispatch_attempted','ipc_bytes_accepted','http_version','response_header_hashes','set_cookie','retry_after','error_body'):
            if key in post:report[key]=post[key]
        if not cleaned:base._invalidate(report,'helper_cleanup_failed','route_or_helper_cleanup_failed')
        elif report['route'].get('bridge_error'):base._invalidate(report,'proxy_error',report['route']['bridge_error'])
    report['turn_state_observation']=observer.summary()
    if not report['turn_state_observation']['complete'] and report.get('protocol_success'):
        base._invalidate(report,'turn_state_observation_failed',report['turn_state_observation']['error'])
    return report
