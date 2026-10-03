"""Offline fault injection and optional real-helper, synthetic WebSocket tests."""
import base64
import copy
import hashlib
import io
import json
import os
from pathlib import Path
import socketserver
import struct
import threading
import time
from types import SimpleNamespace
import unittest
from unittest import mock

import quality_exact_websocket_probe as probe

TOKEN, WORKSPACE, HELPER_SHA = "private-synthetic-access", "private-workspace", "a" * 64
BODY = b'{ "type":"response.create", "model":"fixture", "store":false, "stream":true, "prompt_cache_key":"private-session", "client_metadata":{"session_id":"private-session"}, "input":[{"role":"user","content":"private prompt"}] }'


def head():
    return {"method": "GET", "path": probe.PATH, "headers": [[name, base64.b64encode(value.encode()).decode()]
        for name, value in (("User-Agent", "codex_exec/0.155.0 (TestOS 1; x86_64) synthetic"),
                            ("Originator", "codex_exec"), ("Session-Id", "private-session"))]}


def frame(kind, raw=b""):
    return struct.pack("!BI", kind, len(raw)) + raw


def handshake(status=101, body=b"", **kwargs):
    value = {"status": status, "headers": [], "body_scope": "handshake_tail", "body_complete": None,
             "body_base64": base64.b64encode(body).decode(), "tail_truncated": False}
    value.update(kwargs)
    return frame(65, json.dumps(value).encode())


def event(value, sequence=1, kind=1):
    raw = json.dumps(value, ensure_ascii=False).encode() if isinstance(value, dict) else value
    return frame(66, struct.pack("!QB", sequence, kind) + raw)


def terminal(answer=21, identity="private-response"):
    return {"type": "response.completed", "response": {"id": identity, "status": "completed", "model": "fixture",
        "reasoning": {"effort": "medium"}, "usage": {"input_tokens": 9, "output_tokens": 7, "output_tokens_details": {"reasoning_tokens": 4}},
        "output": [{"type": "message", "role": "assistant", "content": [{"type": "output_text", "text": "最终答案：%d个" % answer}]}]}}


ACK = frame(70, struct.pack("!Q", 1))


class FakeSocket:
    def __init__(self, response, chunk=7, send_fail_after=None, receive_error=None):
        self.incoming, self.sent = io.BytesIO(response), bytearray()
        self.chunk, self.send_fail_after, self.receive_error = chunk, send_fail_after, receive_error
        self.timeouts, self.closed = [], False

    def settimeout(self, value):
        self.timeouts.append(value)

    def connect(self, path):
        self.path = path

    def send(self, raw):
        if self.send_fail_after is not None and len(self.sent) >= self.send_fail_after:
            raise OSError(TOKEN)
        size = min(len(raw), self.chunk)
        self.sent.extend(raw[:size])
        return size

    def recv(self, size):
        if self.receive_error:
            raise self.receive_error
        return self.incoming.read(min(size, self.chunk))

    def close(self):
        self.closed = True


class RetainedBuffer(io.BytesIO):
    def close(self):
        self.was_closed = True


class FakeChild:
    pid = 999

    def __init__(self, returncode=0, stuck=False):
        self.stdin, self.stdout = RetainedBuffer(), io.BytesIO()
        self.returncode, self.stuck, self.killed = returncode, stuck, False

    def wait(self, timeout):
        if self.stuck and not self.killed:
            raise probe.subprocess.TimeoutExpired(TOKEN, timeout)
        return self.returncode

    def kill(self):
        self.killed, self.returncode = True, -9

    def poll(self):
        return self.returncode


class ProbeTests(unittest.TestCase):
    def call(self, incoming=None, *, stream=None, child=None, ready_change=None, runtime_sha=HELPER_SHA, **kwargs):
        stream = stream or FakeSocket(handshake() + ACK + event(terminal()) if incoming is None else incoming)
        child = child or FakeChild()
        popen = mock.Mock(return_value=child)
        def ready(_stream, _timeout):
            config = json.loads(child.stdin.getvalue()[4:])
            value = {"protocol": 1, "transport": "unix", "unix_socket": config["unix_socket"],
                     "http_client_lifecycle": "cli_request", "capabilities": ["websocket_message_v1"]}
            value.update(ready_change or {})
            return json.dumps(value).encode()
        options = dict(helper=Path("synthetic-helper"), expected_helper_sha=HELPER_SHA, body=BODY,
                       captured=head(), token=TOKEN, workspace=WORKSPACE, hidden=[TOKEN, WORKSPACE], timeout=20)
        options.update(kwargs)
        fake_os = SimpleNamespace(name="posix", chmod=os.chmod, environ={"PATH": "/safe/path", "HTTP_PROXY": "secret-proxy"})
        with mock.patch.object(probe, "os", fake_os), mock.patch.object(probe, "file_digest", side_effect=[HELPER_SHA, runtime_sha]), \
             mock.patch.object(probe.subprocess, "Popen", popen), mock.patch.object(probe, "_ready_line", side_effect=ready), \
             mock.patch.object(probe.socket, "AF_UNIX", 1, create=True), mock.patch.object(probe.socket, "socket", return_value=stream):
            result = probe.native_websocket_request(**options)
        return result, stream, child, popen

    def test_exact_body_single_send_fresh_helper_headers_and_safe_report(self):
        captured = head()
        before = copy.deepcopy(captured)
        result, stream, child, popen = self.call(captured=captured)
        self.assertTrue(result["protocol_success"], result)
        self.assertEqual(result["http_status"], 101)
        self.assertEqual(result["answer"], 21)
        self.assertEqual(result["native_dispatch_count"], 1)
        self.assertTrue(result["ws_write_acknowledged"])
        self.assertEqual(result["response_usage"]["output_tokens_details"]["reasoning_tokens"], 4)
        self.assertEqual(captured, before)
        n = struct.unpack("!I", stream.sent[:4])[0]
        request = json.loads(stream.sent[4:n+4])
        self.assertEqual((request["method"], request["operation"], request["body_length"]), ("GET", "websocket_message_v1", 0))
        self.assertEqual(request["headers"][:3], captured["headers"])
        values = {name.lower(): base64.b64decode(value).decode() for name, value in request["headers"]}
        self.assertEqual(values["authorization"], "Bearer " + TOKEN)
        self.assertEqual(values["chatgpt-account-id"], WORKSPACE)
        self.assertEqual(bytes(stream.sent[n+4:]), struct.pack("!BIQB", 82, len(BODY)+9, 1, 1)+BODY)
        self.assertEqual(result["actual_ipc_body_sha256"], probe.sha(BODY))
        self.assertEqual(popen.call_count, 1)
        self.assertEqual(popen.call_args.kwargs["env"], {"PATH": "/safe/path", "LANG": "C.UTF-8"})
        self.assertEqual(popen.call_args.kwargs["stderr"], probe.subprocess.DEVNULL)
        self.assertTrue(stream.closed and child.stdin.was_closed and result["helper_reaped"] and result["local_socket_absent"])
        self.assertFalse(result["helper_forced_kill"])
        for secret in (TOKEN, WORKSPACE, "private-session", "private prompt", "private-response"):
            self.assertNotIn(secret, json.dumps(result))

    def test_terminal_before_ack_is_retained_and_waits_for_ack(self):
        result, *_ = self.call(handshake() + event(terminal()) + ACK)
        self.assertTrue(result["protocol_success"], result)
        self.assertEqual(result["answer"], 21)
        evidence = result["response_output_evidence"]["terminal_event_data"]
        self.assertIn("exact_websocket", evidence["basis"])
        for after in (b"", frame(69), frame(68, b'{"accepted":true,"phase":"message","code":"ws_transport"}')):
            result, *_ = self.call(handshake() + event(terminal()) + after)
            self.assertFalse(result["protocol_success"])
            self.assertIsNone(result["answer"])
            self.assertFalse(result["ws_write_acknowledged"])
            self.assertTrue(result["response_completion_observed"])
            self.assertEqual(result["observed_completion"]["answer"], 21)
            self.assertIn("21", result["observed_completion"]["final_output"])
            self.assertFalse(result["eligible_for_scoring"])

    def test_completed_and_ack_stop_without_requiring_peer_close(self):
        result, stream, *_ = self.call(handshake() + ACK + event(terminal()) + frame(68, b'{"accepted":true,"phase":"message","code":"ws_transport"}'))
        self.assertTrue(result["protocol_success"])
        self.assertEqual(result["answer"], 21)
        self.assertGreater(len(stream.incoming.read()), 0)

    def test_header_rejection_no_create_and_safe_typed_error(self):
        tail = json.dumps({"error": {"code": "rate_limit_exceeded", "message": TOKEN}}).encode()
        for status in (400, 401, 403, 429, 503):
            with self.subTest(status=status):
                result, stream, *_ = self.call(handshake(status, tail) + frame(69))
                self.assertFalse(result["protocol_success"])
                self.assertEqual(result["http_status"], status)
                self.assertEqual(result["native_dispatch_count"], 0)
                self.assertEqual(result["response_error"]["code"], "rate_limit_exceeded")
                n = struct.unpack("!I", stream.sent[:4])[0]
                self.assertEqual(len(stream.sent), n+4)
                self.assertNotIn(TOKEN, json.dumps(result))

    def test_handshake_non101_never_scores_embedded_answer_or_dispatches(self):
        result, *_ = self.call(handshake(200, json.dumps(terminal()).encode()) + frame(69))
        self.assertFalse(result["protocol_success"])
        self.assertIsNone(result["answer"])
        self.assertEqual(result["native_dispatch_count"], 0)

    def test_handshake_native_error_is_typed_without_create(self):
        for code in ("ws_handshake_timeout", TOKEN):
            result, *_ = self.call(frame(68, json.dumps({"accepted": False, "phase": "handshake", "code": code}).encode()))
            self.assertEqual(result["native_dispatch_count"], 0)
            self.assertFalse(result["protocol_success"])
            self.assertEqual(result["native_error"]["code"], code if code != TOKEN else "unknown_native_error")
            self.assertNotIn(TOKEN, json.dumps(result))

    def test_foreign_identity_conflicts_and_bad_terminal_never_score(self):
        created = event({"type": "response.created", "response": {"id": "first"}})
        cases = [terminal(identity="foreign"), dict(terminal(identity="first"), response_id="foreign"),
                 {"type": "response.completed", "response": {"id": "first", "status": "incomplete"}},
                 {"type": "response.failed", "response": {"id": "first", "status": "failed"}},
                 dict(terminal(identity="first"), stream_id="other-lane")]
        for value in cases:
            result, *_ = self.call(handshake() + ACK + created + event(value, 2))
            self.assertFalse(result["protocol_success"])
            self.assertIsNone(result["answer"])

    def test_tool_request_is_evidence_not_an_answer_or_execution(self):
        value = terminal()
        value["response"]["output"] = [{"type": "function_call", "name": "private-tool", "arguments": TOKEN}]
        result, _, _, popen = self.call(handshake() + ACK + event(value))
        self.assertTrue(result["protocol_success"])
        self.assertFalse(result["generated_answer_available"])
        self.assertIsNone(result["answer"])
        self.assertEqual(popen.call_count, 1)
        self.assertNotIn("private-tool", json.dumps(result))

    def test_sequence_binary_duplicate_ack_and_illegal_order(self):
        cases = [handshake()+ACK+event(terminal(),2), handshake()+ACK+event(terminal(),1,2),
                 handshake()+ACK+ACK, handshake()+frame(70,struct.pack("!Q",2)),
                 ACK, event(terminal()), handshake()+handshake(), handshake()+frame(69),
                 handshake()+frame(67,b'{"code":1000,"reason":"done"}'),
                 handshake()+frame(67,b'{"code":1005,"reason":"invalid"}'),
                 handshake()+frame(68,b'{"accepted":false,"phase":"message","code":"ws_transport"}')]
        for wire in cases:
            result, *_ = self.call(wire)
            self.assertFalse(result["protocol_success"])
            self.assertIsNone(result["answer"])

    def test_truncated_oversized_unknown_frame_and_invalid_json(self):
        cases = [handshake()[:-1], handshake()+ACK[:-1], handshake()+frame(66,b'x'),
                 handshake()+struct.pack("!BI",66,probe.MAX_RESPONSE+10), frame(99),
                 handshake()+ACK+event(b'{"type":"response.completed","type":"error"}'),
                 handshake()+ACK+event(b'{"type":"response.created","x":1e9999}'),
                 handshake()+ACK+event(b'{"type":"response.created","x":"\\ud800"}'),
                 handshake()+ACK+event(b'\xff')]
        for wire in cases:
            result, *_ = self.call(wire)
            self.assertFalse(result["protocol_success"])
            self.assertTrue(result["helper_reaped"])

    def test_malformed_handshake_headers_status_tail_and_end(self):
        cases = [handshake(status=True), handshake(body=b'bad'), handshake(body_complete=True),
                 handshake(headers=[["private-header", "not-b64"]]), handshake(headers=[["bad\nheader", ""]]),
                 handshake(429)+ACK, handshake(429)+frame(69,b'x')]
        for wire in cases:
            result, *_ = self.call(wire)
            self.assertFalse(result["protocol_success"])
            self.assertEqual(result["native_dispatch_count"], 0)

    def test_bad_readiness_runtime_or_capability_never_sends(self):
        for change in ({"ready_change":{"protocol":True}}, {"ready_change":{"http_client_lifecycle":"pooled"}},
                       {"ready_change":{"capabilities":[]}}, {"runtime_sha":"b"*64}):
            result, stream, *_ = self.call(**change)
            self.assertFalse(result["protocol_success"])
            self.assertEqual(stream.sent,b'')
            self.assertEqual(result["attempts"],0)

    def test_cleanup_failures_invalidate_scoring_but_reap(self):
        for child in (FakeChild(returncode=4), FakeChild(stuck=True)):
            result, *_ = self.call(child=child)
            self.assertFalse(result["protocol_success"])
            self.assertEqual(result["failure_category"], "helper_cleanup_failed")
            self.assertIsNone(result["answer"])
            self.assertTrue(result["helper_reaped"] and result["local_socket_absent"])
            self.assertEqual(result["observed_completion"]["answer"], 21)

    def test_partial_dispatch_is_recorded_without_retry_or_secret_error(self):
        # Large chunks write the header once, then write a partial create and fail.
        stream=FakeSocket(handshake(),chunk=32)
        original=stream.send
        def send(raw):
            if stream.incoming.tell()>0:
                if hasattr(stream,"create_started"):
                    raise OSError(TOKEN)
                stream.create_started=True
            return original(raw)
        stream.send=send
        result, _, _, popen=self.call(stream=stream)
        self.assertTrue(result["ipc_dispatch_attempted"])
        self.assertEqual(result["ipc_bytes_accepted"],32)
        self.assertEqual(result["native_dispatch_count"],0)
        self.assertEqual(popen.call_count,1)
        self.assertNotIn(TOKEN,json.dumps(result))

    def test_deadline_timeout_no_fallback_and_each_io_recalculates(self):
        result, *_=self.call(stream=FakeSocket(b'',receive_error=TimeoutError(TOKEN)))
        self.assertEqual(result["failure_category"],"timeout")
        self.assertFalse(result["http_fallback"])
        stream=FakeSocket(b'xx',chunk=1)
        with mock.patch.object(probe.time,"monotonic",side_effect=[10.1,10.5,11.1]):
            with self.assertRaises(TimeoutError):
                probe.read_exact(lambda n:probe.deadline_io(stream,11,"recv",n),3)
        self.assertEqual(len(stream.timeouts),2)

    def test_request_validation_preserves_official_stream_and_rejects_nonfull(self):
        self.assertEqual(probe.validate_request(BODY,head())["stream"],True)
        for replacement in ({"previous_response_id":"foreign"},{"generate":False},{"store":True},
                            {"stream":False},{"input":"text"},{"type":"response.cancel"}):
            value=json.loads(BODY);value.update(replacement)
            result, _, _, popen=self.call(body=json.dumps(value).encode())
            self.assertFalse(result["protocol_success"])
            popen.assert_not_called()

    def test_request_auth_hop_headers_duplicates_and_untrusted_origin_rejected_before_start(self):
        for name in ("Authorization","ChatGPT-Account-Id","Cookie","Proxy-Authorization","Sec-WebSocket-Key","Connection","Upgrade"):
            captured=head();captured['headers'].append([name,base64.b64encode(b'private').decode()])
            result, _, _, popen=self.call(captured=captured)
            self.assertFalse(result['protocol_success']);popen.assert_not_called()

    def test_factory_invokes_after_verified_101_once_and_only_updates_timestamp(self):
        from quality_transport_pair import TIMING_METADATA
        template=BODY.replace(b'"private-session"}',b'"private-session","'+TIMING_METADATA.encode()+b'":"1"}')
        updated=template.replace(b'":"1"}',b'":"123456"}')
        stream=FakeSocket(handshake()+ACK+event(terminal()))
        calls=[]
        captured=head()
        def factory():
            calls.append(stream.incoming.tell())
            # Mutation of caller-owned input cannot affect the fixed handshake.
            captured['headers'].append(['x-codex-new',base64.b64encode(b'changed').decode()])
            return updated, {'access_token':TOKEN,'public':TOKEN+' retained','preserved':True}
        result,stream,_,_=self.call(stream=stream,body=template,captured=captured,body_factory=factory)
        self.assertTrue(result['protocol_success'],result)
        self.assertEqual(len(calls),1)
        self.assertEqual(calls[0],len(handshake()))
        self.assertEqual(result['request_body_sha256'],probe.sha(updated))
        self.assertEqual(result['actual_ipc_body_sha256'],probe.sha(updated))
        self.assertEqual(result['request_start_ms'],123456)
        self.assertEqual(result['body_factory_invocations'],1)
        self.assertNotIn(TOKEN,json.dumps(result))
        size=struct.unpack('!I',stream.sent[:4])[0]
        wire_head=json.loads(stream.sent[4:4+size])
        self.assertNotIn('x-codex-new',[row[0] for row in wire_head['headers']])
        self.assertTrue(bytes(stream.sent).endswith(updated))

    def test_factory_not_called_on_rejected_handshake_and_failure_never_sends_create(self):
        for status in (400,401,429):
            factory=mock.Mock(side_effect=RuntimeError(TOKEN))
            result,*_=self.call(handshake(status)+frame(69),body_factory=factory)
            factory.assert_not_called()
            self.assertEqual(result['native_dispatch_count'],0)
        factory=mock.Mock(side_effect=RuntimeError(TOKEN))
        result,stream,*_=self.call(body_factory=factory)
        self.assertEqual(factory.call_count,1)
        self.assertEqual(result['native_dispatch_count'],0)
        self.assertNotIn(TOKEN,json.dumps(result))
        self.assertEqual(len(stream.sent),4+struct.unpack('!I',stream.sent[:4])[0])

    def test_factory_non_timing_changes_missing_stamp_bad_projection_rejected(self):
        from quality_transport_pair import TIMING_METADATA
        template=BODY.replace(b'"private-session"}',b'"private-session","'+TIMING_METADATA.encode()+b'":"1"}')
        updated=template.replace(b'":"1"}',b'":"123456"}')
        for result_value in ((updated.replace(b'private prompt',b'different task'),{}),(BODY,{}),
                             (updated.replace(b'":"123456"}',b'":123456}'),{}),(updated,[]),
                             (updated,{'bad':float('nan')}),(updated,{'huge':'x'*(probe.MAX_HEAD+1)})):
            result,stream,*_=self.call(body=template,body_factory=lambda:result_value)
            self.assertFalse(result['protocol_success'])
            self.assertEqual(result['native_dispatch_count'],0)
            self.assertEqual(len(stream.sent),4+struct.unpack('!I',stream.sent[:4])[0])
        captured=head();captured['headers'].append(['user-agent',captured['headers'][0][1]])
        for kwargs in ({'captured':captured},{'upstream':'https://example.test'},{'upstream':'http://chatgpt.com'},
                       {'upstream':'http://127.0.0.1:80/path'},{'timeout':241},{'timeout':True},
                       {'deadline':time.monotonic()-1},{'body':b'{"x":1,"x":2}'}):
            result, _, _, popen=self.call(**kwargs)
            self.assertFalse(result['protocol_success']);popen.assert_not_called()


@unittest.skipUnless(os.name == "posix", "real helper uses POSIX IPC")
class NativeLoopbackTests(unittest.TestCase):
    def test_real_helper_exact_ws_single_message_terminal_then_abrupt_close(self):
        helper=os.environ.get('CODEX_NATIVE_HTTP_TEST_BIN')
        if not helper or not Path(helper).is_file():
            self.skipTest('explicit real Rust helper required')
        observed=[];errors=[]
        class Handler(socketserver.BaseRequestHandler):
            def handle(self):
                try:
                    self.request.settimeout(5)
                    raw=b''
                    while not raw.endswith(b'\r\n\r\n'):
                        raw+=probe.read_exact(self.request.recv,1)
                    headers={}
                    for line in raw.decode().split('\r\n')[1:]:
                        if ':' in line:
                            name,value=line.split(':',1);headers[name.lower()]=value.strip()
                    accept=base64.b64encode(hashlib.sha1((headers['sec-websocket-key']+'258EAFA5-E914-47DA-95CA-C5AB0DC85B11').encode()).digest())
                    self.request.sendall(b'HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: '+accept+b'\r\n\r\n')
                    first,second=probe.read_exact(self.request.recv,2)
                    self.assert_valid(first,second)
                    size=second&127
                    if size==126:size=struct.unpack('!H',probe.read_exact(self.request.recv,2))[0]
                    elif size==127:size=struct.unpack('!Q',probe.read_exact(self.request.recv,8))[0]
                    mask=probe.read_exact(self.request.recv,4)
                    body=probe.read_exact(self.request.recv,size)
                    body=bytes(value^mask[i%4] for i,value in enumerate(body))
                    observed.append((body,headers['authorization'],headers['chatgpt-account-id']))
                    payload=json.dumps(terminal(),ensure_ascii=False).encode()
                    frame_head=bytes([0x81,126])+struct.pack('!H',len(payload))
                    self.request.sendall(frame_head+payload)
                except Exception as error:
                    errors.append(type(error).__name__)
            def assert_valid(self,first,second):
                if first!=0x81 or second&0x80==0:raise AssertionError('unexpected framing')
        with socketserver.ThreadingTCPServer(('127.0.0.1',0),Handler) as server:
            worker=threading.Thread(target=server.serve_forever,daemon=True);worker.start()
            try:
                result=probe.native_websocket_request(Path(helper),probe.file_digest(Path(helper)),BODY,head(),TOKEN,WORKSPACE,
                    timeout=15,upstream='http://127.0.0.1:%d'%server.server_address[1])
            finally:
                server.shutdown();worker.join(timeout=3)
        self.assertFalse(errors,errors)
        self.assertTrue(result['protocol_success'],result)
        self.assertEqual(result['answer'],21)
        self.assertEqual(observed,[(BODY,'Bearer '+TOKEN,WORKSPACE)])
        self.assertEqual(result['ws_send_count'],1)
        self.assertTrue(result['helper_reaped'] and result['local_socket_absent'])
        self.assertFalse(result['helper_forced_kill'])


if __name__ == '__main__':
    unittest.main()
