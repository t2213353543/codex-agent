"""Fault injection and real native helper loopback tests; synthetic credentials only."""
import base64
import copy
import hashlib
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

import quality_websocket_chain_probe as chain
from test_quality_exact_websocket_probe import (FakeSocket, FakeChild, head, handshake, event, frame,
    terminal, TOKEN, WORKSPACE, HELPER_SHA)

PRIOR = 'synthetic_capture_warmup'
REAL = 'private-server-response'
STAMP = 'x-codex-ws-stream-request-start-ms'
WARMUP = ('{ "type":"response.create", "model":"fixture", "generate":false, "store":false, "stream":true, '
          '"prompt_cache_key":"private-session", "client_metadata":{"session_id":"private-session", "'+STAMP+'":"1"}, '
          '"input":[{"role":"user","content":"private baseline","n":9007199254740993}] }').encode()
INFERENCE = WARMUP.replace(b'"generate":false',b'"previous_response_id":"synthetic_capture_warmup"').replace(b'private baseline',b'private delta')


def ack(sequence):
    return frame(70,struct.pack('!Q',sequence))


def complete(identity=REAL, **kwargs):
    response={'id':identity,'status':'completed','output':[],
              'usage':{'input_tokens':100,'output_tokens':0,'total_tokens':100,
                       'input_tokens_details':{'cached_tokens':50}}}
    response.update(kwargs)
    return {'type':'response.completed','response':response}


def happy():
    return handshake()+ack(1)+event(complete())+ack(2)+event(terminal(identity='private-final'),2)


def sent_messages(stream):
    raw=bytes(stream.sent);n=struct.unpack('!I',raw[:4])[0];position=n+4;messages=[]
    while position<len(raw):
        kind,size,sequence,message_type=struct.unpack('!BIQB',raw[position:position+14])
        assert kind==82 and message_type==1
        messages.append((sequence,raw[position+14:position+5+size]));position+=size+5
    return messages


class ChainTests(unittest.TestCase):
    def call(self,incoming=None,*,stream=None,child=None,ready_change=None,runtime_sha=HELPER_SHA,**kwargs):
        stream=stream or FakeSocket(happy() if incoming is None else incoming)
        child=child or FakeChild();popen=mock.Mock(return_value=child)
        def ready(_stream,_timeout):
            startup=json.loads(child.stdin.getvalue()[4:]);value={'protocol':1,'transport':'unix',
                'unix_socket':startup['unix_socket'],'http_client_lifecycle':'cli_request','capabilities':['websocket_message_v1']}
            value.update(ready_change or {});return json.dumps(value).encode()
        options=dict(helper=Path('synthetic-helper'),expected_helper_sha=HELPER_SHA,warmup_body=WARMUP,
            inference_body=INFERENCE,captured=head(),token=TOKEN,workspace=WORKSPACE,timeout=20)
        options.update(kwargs)
        fake_os=SimpleNamespace(name='posix',chmod=os.chmod,environ={'PATH':'/safe/path','HTTP_PROXY':'private-proxy'})
        with mock.patch.object(chain,'os',fake_os),mock.patch.object(chain,'file_digest',side_effect=[HELPER_SHA,runtime_sha]), \
             mock.patch.object(chain.subprocess,'Popen',popen),mock.patch.object(chain,'_ready_line',side_effect=ready), \
             mock.patch.object(chain.socket,'AF_UNIX',1,create=True),mock.patch.object(chain.socket,'socket',return_value=stream):
            result=chain.native_websocket_chain(**options)
        return result,stream,child,popen

    def test_one_helper_connection_exact_delta_and_distinct_accounting(self):
        captured=head();before=copy.deepcopy(captured)
        result,stream,child,popen=self.call(captured=captured)
        self.assertTrue(result['protocol_success'],result);self.assertEqual(result['answer'],21)
        self.assertEqual((result['native_create_count'],result['warmup_create_count'],result['inference_create_count']),(2,1,1))
        self.assertEqual(result['handshake_dispatch_count'],1);self.assertEqual(popen.call_count,1)
        self.assertTrue(result['same_websocket_connection'] and result['warmup_receipt_verified'])
        self.assertEqual(captured,before)
        messages=sent_messages(stream);self.assertEqual([n for n,_ in messages],[1,2])
        for index,(sequence,raw) in enumerate(messages):
            value=json.loads(raw);template=WARMUP if index==0 else INFERENCE
            without_stamp=raw.replace(json.dumps(value['client_metadata'][STAMP]).encode(),b'"1"')
            if index:
                self.assertEqual(value['previous_response_id'],REAL)
                without_stamp=without_stamp.replace(json.dumps(REAL).encode(),json.dumps(PRIOR).encode())
            self.assertEqual(without_stamp,template)
        self.assertEqual(result['request_body_sha256'],chain.sha(messages[1][1]))
        self.assertEqual(result['warmup']['response_usage']['input_tokens_details']['cached_tokens'],50)
        self.assertTrue(result['helper_reaped'] and result['local_socket_absent']);self.assertFalse(result['helper_forced_kill'])
        for secret in (TOKEN,WORKSPACE,REAL,PRIOR,'private-session','private baseline','private delta','private-final'):
            self.assertNotIn(secret,json.dumps(result))

    def test_terminal_before_ack_both_phases_and_global_sequence(self):
        result,stream,*_=self.call(handshake()+event(complete())+ack(1)+event(terminal(identity='last'),2)+ack(2))
        self.assertTrue(result['protocol_success'],result);self.assertEqual(len(sent_messages(stream)),2)
        self.assertEqual(result['warmup']['ws_ack_sequence'],1);self.assertEqual(result['inference']['ws_ack_sequence'],2)
        self.assertEqual(result['ws_received_sequence'],2)

    def test_warmup_failure_or_missing_receipt_never_sends_inference(self):
        failures=[event({'type':'response.failed','response':{'id':REAL,'status':'failed','error':{'code':'invalid_prompt'}}})+ack(1),
                  event(complete())+frame(69),ack(1)+frame(69),
                  event(complete())+ack(2),ack(1)+ack(1),
                  ack(1)+event(complete(),2),ack(1)+event(complete(),1,2),
                  ack(1)+event({'type':'response.created','response':{'id':'other'}})+event(complete(),2)]
        for incoming in failures:
            with self.subTest(incoming_hash=chain.sha(incoming)):
                result,stream,*_=self.call(handshake()+incoming)
                self.assertFalse(result['protocol_success']);self.assertEqual(len(sent_messages(stream)),1)
                self.assertEqual(result['inference_create_count'],0)

    def test_warmup_output_and_invalid_usage_rejected_cached_input_allowed(self):
        cases=[{'output':[{'type':'reasoning','summary':[]}]},
               {'usage':{'input_tokens':2,'output_tokens':1,'total_tokens':3}},
               {'usage':{'input_tokens':2,'output_tokens':0}},
               {'usage':{'input_tokens':True,'output_tokens':0,'total_tokens':0}},
               {'usage':{'input_tokens':2**63,'output_tokens':0,'total_tokens':0}},
               {'usage':{'input_tokens':2,'output_tokens':0,'total_tokens':2,'input_tokens_details':{'cached_tokens':True}}},
               {'usage':{'input_tokens':2,'output_tokens':0,'total_tokens':2,'output_tokens_details':{'reasoning_tokens':1}}}]
        for changed in cases:
            with self.subTest(changed=changed):
                result,stream,*_=self.call(handshake()+ack(1)+event(complete(**changed)))
                self.assertFalse(result['protocol_success']);self.assertEqual(len(sent_messages(stream)),1)
        for kind in ('response.output_text.delta','response.output_item.done','response.reasoning_text.delta'):
            result,stream,*_=self.call(handshake()+ack(1)+event({'type':kind,'response_id':REAL,'delta':'private generated'})+event(complete(),2))
            self.assertFalse(result['protocol_success']);self.assertEqual(len(sent_messages(stream)),1)
        result,*_=self.call(handshake()+ack(1)+event(complete(usage=None))+ack(2)+event(terminal(identity='last'),2))
        self.assertTrue(result['protocol_success'],result)

    def test_handshake_rejection_no_create_preserves_typed_error(self):
        result,stream,*_=self.call(handshake(429,b'{"error":{"code":"rate_limit_exceeded","message":"private-token"}}')+frame(69))
        self.assertEqual(result['failure_category'],'handshake_rejected');self.assertEqual(result['native_create_count'],0)
        self.assertEqual(result['response_error']['code'],'rate_limit_exceeded');self.assertEqual(sent_messages(stream),[])

    def test_wrong_final_identity_lane_ack_and_missing_ack_preserve_evidence(self):
        cases=[event(terminal(identity=REAL),2)+ack(2),ack(2)+event({**terminal(identity='last'),'stream_id':'other'},2),
               ack(2)+event({'type':'response.created','response':{'id':'other'}},2)+event(terminal(identity='last'),3)]
        for tail in cases:
            result,*_=self.call(handshake()+ack(1)+event(complete())+tail)
            self.assertFalse(result['protocol_success']);self.assertFalse(result['eligible_for_scoring'])
        result,*_=self.call(handshake()+ack(1)+event(complete())+event(terminal(identity='last'),2)+frame(69))
        self.assertFalse(result['protocol_success']);self.assertEqual(result['observed_completion']['answer'],21)

    def test_turn_state_handshake_and_metadata_first_value_persisted_as_hash_only(self):
        first='private-first-state';later='private-later-state'
        extra=event({'type':'response.metadata','headers':{'X-Codex-Turn-State':later}},1)
        incoming=handshake(headers=[['X-Codex-Turn-State',base64.b64encode(first.encode()).decode()]])+ack(1)+extra+event(complete(),2)+ack(2)+event(terminal(identity='last'),3)
        result,stream,*_=self.call(incoming)
        self.assertTrue(result['protocol_success'],result)
        for _,raw in sent_messages(stream):self.assertEqual(json.loads(raw)['client_metadata']['x-codex-turn-state'],first)
        self.assertEqual(result['turn_state']['source'],'handshake_header')
        self.assertNotIn(first,json.dumps(result));self.assertNotIn(later,json.dumps(result))
        result,stream,*_=self.call(handshake()+ack(1)+extra+event(complete(),2)+ack(2)+event(terminal(identity='last'),3))
        messages=sent_messages(stream);self.assertNotIn('x-codex-turn-state',json.loads(messages[0][1])['client_metadata'])
        self.assertEqual(json.loads(messages[1][1])['client_metadata']['x-codex-turn-state'],later)
        self.assertEqual(result['turn_state']['source'],'response.metadata.headers')

    def test_malformed_turnstate_refuses_next_phase(self):
        for value in (17,True,None,[],['first',3],{'secret':'private'}):
            result,stream,*_=self.call(handshake()+ack(1)+event({'type':'response.metadata','headers':{'x-codex-turn-state':value}}))
            self.assertFalse(result['protocol_success']);self.assertEqual(len(sent_messages(stream)),1)
        result,stream,*_=self.call(handshake(headers=[['x-codex-turn-state','YQ=='],['X-Codex-Turn-State','Yg==']]))
        self.assertFalse(result['protocol_success']);self.assertEqual(sent_messages(stream),[])

    def test_turn_state_existing_value_escaped_member_and_empty_oncelock(self):
        template=INFERENCE.replace(b'"session_id":"private-session"',b'"session_id":"private-session","x-codex-turn-\\u0073tate":"synthetic-state"')
        changed,proof=chain.project_frame(template,previous=REAL,expected_previous=PRIOR,restamp_timing=False,turn_state='opaque actual')
        self.assertEqual(changed,template.replace(b'"synthetic-state"',b'"opaque actual"').replace(json.dumps(PRIOR).encode(),json.dumps(REAL).encode()))
        self.assertEqual([row['field'] for row in proof['allowed_value_span_edits']],['previous_response_id','turn_state_value'])
        result,stream,*_=self.call(handshake(headers=[['x-codex-turn-state','']])+ack(1)+
            event({'type':'response.metadata','headers':{'x-codex-turn-state':['later']}})+event(complete(),2)+ack(2)+event(terminal(identity='last'),3))
        self.assertTrue(result['protocol_success'],result)
        self.assertEqual(json.loads(sent_messages(stream)[1][1])['client_metadata']['x-codex-turn-state'],'')

    def test_duplicate_terminal_late_warmup_and_sequence_reset_cannot_score(self):
        for tail in (event(complete(),2)+ack(2),ack(2)+event(terminal(identity='last'),1),
                     event(terminal(identity='last'),2)+event(terminal(identity='last'),3)+ack(2)):
            result,*_=self.call(handshake()+ack(1)+event(complete())+tail)
            self.assertFalse(result['protocol_success']);self.assertFalse(result['eligible_for_scoring'])
        result,stream,*_=self.call(handshake()+event(complete())+event(complete(),2)+ack(1))
        self.assertFalse(result['protocol_success']);self.assertEqual(len(sent_messages(stream)),1)

    def test_exact_span_escaped_keys_unicode_numbers_and_timing_disabled(self):
        template=INFERENCE.replace(b'"previous_response_id"',b'"previous_\\u0072esponse_id"').replace(b'"private delta"','"原题\\u0061"'.encode())
        updated,evidence=chain.project_frame(template,previous='resp-"\\雪',expected_previous=PRIOR,restamp_timing=False)
        self.assertEqual(updated,template.replace(json.dumps(PRIOR).encode(),json.dumps('resp-"\\雪',ensure_ascii=False).encode()))
        self.assertEqual(len(evidence['allowed_value_span_edits']),1)
        result,stream,*_=self.call(restamp_timing=False)
        self.assertTrue(result['protocol_success']);self.assertEqual(sent_messages(stream)[0][1],WARMUP)

    def test_invalid_template_properties_prior_or_credentials_never_start(self):
        cases=[{'inference_body':INFERENCE.replace(PRIOR.encode(),b'unknown-prior')},
               {'inference_body':INFERENCE.replace(b'"model":"fixture"',b'"model":true')},
               {'inference_body':INFERENCE.replace(b'"store":false',b'"store":null')},
               {'inference_body':INFERENCE.replace(b'"session_id":"private-session"',b'"session_id":"different"')},
               {'inference_body':INFERENCE.replace(b'"input":',b'"instructions":"different","input":')},
               {'warmup_body':WARMUP.replace(b'"generate":false',b'"generate":true')},
               {'warmup_body':WARMUP.replace(b'"n":9007199254740993',b'"n":1,"n":2')},
               {'inference_body':INFERENCE.replace(STAMP.encode(),b'unknown-timestamp')},
               {'token':'bad\r\n'}, {'timeout':True}, {'deadline':time.monotonic()-1}, {'upstream':'https://example.test'}]
        for kwargs in cases:
            result,_,_,popen=self.call(**kwargs)
            self.assertFalse(result['protocol_success']);popen.assert_not_called()

    def test_runtime_cleanup_deadline_and_partial_dispatch(self):
        result,_,_,_=self.call(runtime_sha='b'*64);self.assertFalse(result['protocol_success']);self.assertEqual(result['native_create_count'],0)
        result,*_=self.call(stream=FakeSocket(b'',receive_error=TimeoutError(TOKEN)))
        self.assertEqual(result['failure_category'],'timeout');self.assertNotIn(TOKEN,json.dumps(result))
        result,*_=self.call(child=FakeChild(returncode=4))
        self.assertFalse(result['eligible_for_scoring']);self.assertEqual(result['observed_completion']['answer'],21)
        stream=FakeSocket(handshake(),chunk=32);original=stream.send
        def broken(raw):
            if stream.incoming.tell()>0:
                if hasattr(stream,'started'):raise OSError(TOKEN)
                stream.started=True
            return original(raw)
        stream.send=broken
        result,_,_,popen=self.call(stream=stream)
        self.assertEqual(result['warmup']['ipc_bytes_accepted'],32);self.assertEqual(result['native_create_count'],0)
        self.assertEqual(popen.call_count,1);self.assertNotIn(TOKEN,json.dumps(result))


@unittest.skipUnless(os.name=='posix','native IPC requires POSIX')
class NativeLoopbackTests(unittest.TestCase):
    def test_real_9110_helper_requires_same_connection_actual_previous_and_turn_state(self):
        helper=os.environ.get('CODEX_NATIVE_HTTP_TEST_BIN')
        if not helper or not Path(helper).is_file():self.skipTest('pinned actual native helper required')
        self.assertEqual(chain.file_digest(Path(helper)),'9110b869e65db6bf7664e39fcfe3b2ec17fa7a0207ddbeccf28d73dbc12ccce8')
        seen=[];errors=[];state='private-loopback-turn-state'
        class Handler(socketserver.BaseRequestHandler):
            def handle(self):
                try:
                    self.request.settimeout(8);raw=b''
                    while not raw.endswith(b'\r\n\r\n'):raw+=chain.read_exact(self.request.recv,1)
                    headers={}
                    for line in raw.decode().split('\r\n')[1:]:
                        if ':' in line:
                            name,value=line.split(':',1);headers[name.lower()]=value.strip()
                    accept=base64.b64encode(hashlib.sha1((headers['sec-websocket-key']+'258EAFA5-E914-47DA-95CA-C5AB0DC85B11').encode()).digest())
                    self.request.sendall(b'HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: '+accept+b'\r\n\r\n')
                    for index in range(2):
                        first,second=chain.read_exact(self.request.recv,2)
                        if first!=0x81 or not second&128:raise AssertionError('invalid websocket framing')
                        size=second&127
                        if size==126:size=struct.unpack('!H',chain.read_exact(self.request.recv,2))[0]
                        elif size==127:size=struct.unpack('!Q',chain.read_exact(self.request.recv,8))[0]
                        mask=chain.read_exact(self.request.recv,4);raw=chain.read_exact(self.request.recv,size)
                        raw=bytes(value^mask[i%4] for i,value in enumerate(raw));value=json.loads(raw);seen.append(value)
                        if index==0:
                            if value.get('generate') is not False:raise AssertionError('warmup must precede inference')
                            self.send({'type':'response.metadata','headers':{'x-codex-turn-state':state}})
                            self.send(complete(identity=REAL))
                        else:
                            if value.get('previous_response_id')!=REAL or value['client_metadata'].get('x-codex-turn-state')!=state:raise AssertionError('state binding absent')
                            self.send(terminal(identity='private-final'))
                except Exception as error:errors.append(type(error).__name__)
            def send(self,value):
                raw=json.dumps(value).encode();prefix=bytes([0x81,len(raw)]) if len(raw)<126 else bytes([0x81,126])+struct.pack('!H',len(raw))
                self.request.sendall(prefix+raw)
        with socketserver.ThreadingTCPServer(('127.0.0.1',0),Handler) as server:
            worker=threading.Thread(target=server.serve_forever,daemon=True);worker.start()
            try:
                result=chain.native_websocket_chain(Path(helper),chain.file_digest(Path(helper)),WARMUP,INFERENCE,head(),TOKEN,WORKSPACE,
                    timeout=20,upstream='http://127.0.0.1:%d'%server.server_address[1])
            finally:server.shutdown();worker.join(timeout=3)
        self.assertFalse(errors,errors);self.assertTrue(result['protocol_success'],result)
        self.assertEqual(result['answer'],21);self.assertEqual(len(seen),2)
        self.assertEqual(result['helper_process_starts'],1);self.assertTrue(result['helper_reaped'] and result['local_socket_absent'])


if __name__=='__main__':unittest.main()
