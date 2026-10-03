"""Offline lifecycle controls; optional real-helper HTTP loopback proves reuse, not cookies."""
import base64
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import io
import json
import os
from pathlib import Path
import struct
import select
import shutil
import socket
import socketserver
import ssl
import subprocess
import tempfile
import threading
import time
from types import SimpleNamespace
import unittest
from unittest import mock

import quality_cookie_lifecycle_probe as probe

TOKEN, WORKSPACE, HELPER_SHA = "private-access", "private-workspace", "a" * 64
BODY = b'{ "model":"fixture","stream":true,"store":false,"input":[{"role":"user","content":"private prompt"}],"client_metadata":{"session_id":"private-session"} }'
IDENTITY = {"user_agent": "codex_exec/0.155.0 (TestOS 1; x86_64) synthetic", "originator": "codex_exec", "version": "0.155.0"}


def headers(rows):
    return [[name, base64.b64encode(value.encode()).decode()] for name, value in rows]


def captured(body=BODY):
    return {"schema": probe.CAPTURE_HEAD_SCHEMA, "method": "POST", "path": "/backend-api/codex/responses",
            "body_sha256": probe.sha(body), "body_bytes": len(body), "request_compression": "none",
            "headers": headers([("User-Agent", IDENTITY["user_agent"]), ("Originator", IDENTITY["originator"]),
                ("Version", "0.155.0"), ("Accept", "text/event-stream"), ("Content-Type", "application/json"),
                ("Session-Id", "private-session"), ("x-openai-internal-codex-responses-lite", "true"),
                ("x-codex-routing-hint", "model=fixture")])}


def frame(kind, raw=b""):
    return struct.pack("!BI", kind, len(raw))+raw


def http_wire(body, status=200, response_headers=None):
    return frame(1,json.dumps({"status":status,"version":"HTTP/2.0","headers":response_headers or []}).encode())+frame(2,body)+frame(3)


def models_wire():
    return http_wire(catalog_body(),response_headers=headers([
        ("Set-Cookie","__oailb=private-cookie; Domain=.chatgpt.com; Path=/; Secure; HttpOnly")]))


def catalog_body():
    return json.dumps({'models':[{'slug':'fixture','display_name':'private model','supported_reasoning_levels':[
        {'effort':'medium','description':'private description'}],'shell_type':'unified_exec','visibility':'list',
        'supported_in_api':True,'priority':1,'support_verbosity':True,'truncation_policy':{'mode':'tokens','limit':10000},
        'experimental_supported_tools':[],'base_instructions':'private catalog instructions','tool_mode':'code_mode_only'}]}).encode()


def final_wire():
    value={"type":"response.completed","response":{"id":"private-response","status":"completed","model":"fixture",
        "usage":{"input_tokens":4,"output_tokens":9,"output_tokens_details":{"reasoning_tokens":6}},
        "output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"最终答案：21个"}]}]}}
    return http_wire(b'data: '+json.dumps(value,ensure_ascii=False).encode()+b'\n\n')


class Buffer(io.BytesIO):
    def close(self):
        self.was_closed=True


class Child:
    def __init__(self,pid,exitcode=0,stuck=False):
        self.pid,self.exitcode,self.stuck=pid,exitcode,stuck
        self.stdin,self.stdout=Buffer(),Buffer()
        self.returncode=None
        self.killed=False

    def poll(self):
        return self.returncode

    def wait(self,timeout):
        if self.stuck and not self.killed:
            raise probe.subprocess.TimeoutExpired(TOKEN,timeout)
        self.returncode=-9 if self.killed else self.exitcode
        return self.returncode

    def kill(self):
        self.killed=True


class Socket:
    def __init__(self,response,chunk=7,recv_error=None,send_limit=None):
        self.incoming,self.sent=io.BytesIO(response),bytearray()
        self.chunk,self.recv_error,self.send_limit=chunk,recv_error,send_limit
        self.closed=False
        self.timeouts=[]

    def settimeout(self,value):self.timeouts.append(value)
    def connect(self,path):self.path=path
    def send(self,raw):
        if self.send_limit is not None and len(self.sent)>=self.send_limit:raise OSError(TOKEN)
        n=min(len(raw),self.chunk);self.sent.extend(raw[:n]);return n
    def recv(self,n):
        if self.recv_error:raise self.recv_error
        return self.incoming.read(min(n,self.chunk))
    def close(self):self.closed=True


def request(stream):
    size=struct.unpack('!I',stream.sent[:4])[0]
    return json.loads(stream.sent[4:size+4]),bytes(stream.sent[size+4:])


class LifecycleTests(unittest.TestCase):
    def call(self,mode='cold',*,responses=None,sockets=None,children=None,ready_change=None,runtime_sha=HELPER_SHA,**kwargs):
        responses=responses if responses is not None else ([final_wire()] if mode=='cold' else [models_wire(),final_wire()])
        sockets=sockets or [Socket(raw) for raw in responses]
        children=children or [Child(7001+i) for i in range(2 if mode=='separate_helper' else 1)]
        launches=[]
        def popen(*args,**opts):
            child=children[len(launches)];launches.append((args,opts,child));return child
        def ready(stdout,timeout):
            child=next(child for child in children if child.stdout is stdout)
            config=json.loads(child.stdin.getvalue()[4:])
            value={"protocol":1,"transport":"unix","unix_socket":config['unix_socket'],"http_client_lifecycle":"cli_request",
                   "capabilities":["request_zstd_v1"]}
            value.update(ready_change or {})
            return json.dumps(value).encode()
        fake_os=SimpleNamespace(name='posix',chmod=os.chmod,environ={'PATH':'/safe/path','HTTPS_PROXY':TOKEN})
        args=dict(helper=Path('synthetic-helper'),expected_helper_sha=HELPER_SHA,body=BODY,captured=captured(),
                  token=TOKEN,workspace=WORKSPACE,hidden=[TOKEN,WORKSPACE],timeout=30,mode=mode,application_identity=IDENTITY)
        args.update(kwargs)
        with mock.patch.object(probe,'os',fake_os),mock.patch.object(probe,'file_digest',side_effect=lambda p:runtime_sha if p.parts[1:2]==('proc',) else HELPER_SHA),\
             mock.patch.object(probe.subprocess,'Popen',side_effect=popen) as factory,mock.patch.object(probe,'_ready_line',side_effect=ready),\
             mock.patch.object(probe.socket,'AF_UNIX',1,create=True),mock.patch.object(probe.socket,'socket',side_effect=sockets) as dialer:
            report=probe.native_lifecycle_request(**args)
        return report,sockets,children,factory,dialer,launches

    def test_three_arms_counts_exact_body_and_process_reuse(self):
        for mode in ('cold','same_helper','separate_helper'):
            with self.subTest(mode=mode):
                report,sockets,children,factory,dialer,launches=self.call(mode)
                self.assertTrue(report['protocol_success'],report)
                self.assertEqual(report['answer'],21)
                self.assertEqual(report['native_dispatch_count'],1)
                self.assertEqual(report['model_post_attempts'],1)
                self.assertEqual(report['metadata_get_count'],int(mode!='cold'))
                self.assertEqual(factory.call_count,2 if mode=='separate_helper' else 1)
                self.assertEqual(dialer.call_count,1 if mode=='cold' else 2)
                self.assertEqual(report['same_helper_reuse_verified'],mode=='same_helper')
                self.assertEqual(report['separate_helper_verified'],mode=='separate_helper')
                post,body=request(sockets[-1])
                self.assertEqual(body,BODY)
                self.assertEqual(post['headers'][:-2],captured()['headers'])
                self.assertEqual(report['actual_ipc_body_sha256'],probe.sha(BODY))
                self.assertTrue(report['helper_reaped'] and report['local_socket_absent'])
                self.assertFalse(report['helper_forced_kill'])
                for item in launches:
                    self.assertEqual(item[1]['env'],{'PATH':'/safe/path','LANG':'C.UTF-8'})
                    self.assertEqual(item[1]['stderr'],probe.subprocess.DEVNULL)
                for item in sockets:self.assertTrue(item.closed)
                for secret in (TOKEN,WORKSPACE,'private-cookie','private-session','private prompt','private-response'):
                    self.assertNotIn(secret,json.dumps(report))

    def test_models_get_uses_fixed_cli_identity_without_post_flags_or_invented_accept(self):
        report,sockets,*_=self.call('same_helper')
        head,body=request(sockets[0])
        values={name.lower():base64.b64decode(value).decode() for name,value in head['headers']}
        self.assertEqual(set(values),{'user-agent','originator','version','authorization','chatgpt-account-id'})
        self.assertEqual(values['user-agent'],IDENTITY['user_agent'])
        self.assertEqual(values['authorization'],'Bearer '+TOKEN)
        self.assertEqual(values['chatgpt-account-id'],WORKSPACE)
        self.assertEqual((head['method'],head['path'],head['body_length']),('GET',probe.MODELS_PATH,0))
        self.assertEqual(body,b'')
        self.assertNotIn('request_compression',head)
        self.assertTrue(report['metadata_get']['ipc_end_observed'])
        self.assertTrue(report['metadata_get']['helper_alive_after_complete_get'])
        self.assertEqual(report['metadata_get']['metadata_model_calls'],0)

    def test_explicit_captured_get_accept_and_residency_preserved_without_caller_mutation(self):
        rows=headers([('User-Agent',IDENTITY['user_agent']),('Originator',IDENTITY['originator']),
                      ('Version','0.155.0'),('Accept','*/*'),('x-openai-internal-codex-residency','private-region')])
        before=json.dumps(rows)
        cap=captured();cap['headers']+=headers([('x-openai-internal-codex-residency','private-region')])
        report,sockets,*_=self.call('same_helper',models_headers=rows,captured=cap)
        self.assertTrue(report['protocol_success'])
        self.assertEqual(request(sockets[0])[0]['headers'][:-2],rows)
        self.assertEqual(json.dumps(rows),before)
        self.assertEqual(report['metadata_get']['header_source'],'captured_models_get')

    def test_get_http_error_stops_before_model_and_retains_safe_error(self):
        raw=json.dumps({'error':{'code':'rate_limit_exceeded','message':TOKEN,'type':'private-error-id'}}).encode()
        wire=http_wire(raw,429,headers([('Retry-After','12'),('Set-Cookie','__cf_bm=private-cookie; Path=/')]))
        for mode in ('same_helper','separate_helper'):
            report,sockets,children,factory,dialer,_=self.call(mode,responses=[wire])
            self.assertFalse(report['protocol_success'])
            self.assertEqual(report['model_post_attempts'],0)
            self.assertEqual(report['native_dispatch_count'],0)
            self.assertEqual(factory.call_count,1)
            self.assertEqual(dialer.call_count,1)
            self.assertEqual(report['metadata_get']['error_body']['error']['code'],'rate_limit_exceeded')
            self.assertEqual(report['metadata_get']['error_body']['error']['type'],'other')
            self.assertEqual(report['metadata_get']['retry_after'][0]['seconds'],12)
            for secret in (TOKEN,'private-cookie','private-error-id'):self.assertNotIn(secret,json.dumps(report))

    def test_get_invalid_json_duplicate_nonfinite_bad_models_and_truncation_never_post(self):
        cases=[http_wire(b'{}'),http_wire(b'{"models":"wrong"}'),http_wire(b'{"models":[4]}'),
               http_wire(b'{"models":[],"models":[]}'),http_wire(b'{"models":[],"number":1e9999}'),
               http_wire(b'{"models":[],"bad":"\\ud800"}'),models_wire()[:-5],models_wire()[:-1],
               frame(2,b'{}'),frame(3),frame(4,b'{"code":"private-native-error"}')]
        for wire in cases:
            with self.subTest(size=len(wire)):
                report,_,_,factory,dialer,_=self.call('same_helper',responses=[wire])
                self.assertFalse(report['protocol_success'])
                self.assertEqual(report['model_post_attempts'],0)
                self.assertEqual(dialer.call_count,1)
                self.assertEqual(factory.call_count,1)

    def test_catalog_required_fields_target_uniqueness_and_hashed_full_metadata(self):
        valid=json.loads(catalog_body())
        cases=[{'models':[]},dict(valid,error={}),{'models':valid['models']*2}]
        for field,bad in [('slug','wrong-target'),('priority',True),('supported_reasoning_levels',[{'effort':'medium'}]),
                          ('truncation_policy',None),('supported_in_api','true')]:
            value=json.loads(catalog_body());value['models'][0][field]=bad;cases.append(value)
        for value in cases:
            report,_,_,_,dialer,_=self.call('same_helper',responses=[http_wire(json.dumps(value).encode())])
            self.assertEqual(report['model_post_attempts'],0)
            self.assertFalse(report['protocol_success'])
            self.assertEqual(dialer.call_count,1)
        report,*_=self.call('same_helper')
        metadata=report['metadata_get']
        self.assertEqual(metadata['expected_model_count'],1)
        self.assertTrue(metadata['expected_model_fields']['base_instructions']['present'])
        self.assertEqual(metadata['catalog_schema_issues'],[])
        for value in ('private model','private description','private catalog instructions'):
            self.assertNotIn(value,json.dumps(report))

    def test_get_timeout_and_partial_write_never_post_or_retry(self):
        for stream in (Socket(b'',recv_error=TimeoutError(TOKEN)),Socket(models_wire(),send_limit=7)):
            report,_,_,factory,dialer,_=self.call('same_helper',sockets=[stream])
            self.assertFalse(report['protocol_success'])
            self.assertEqual(report['model_post_attempts'],0)
            self.assertTrue(report['metadata_get']['ipc_dispatch_attempted'])
            self.assertEqual(factory.call_count,1)
            self.assertEqual(dialer.call_count,1)
            self.assertNotIn(TOKEN,json.dumps(report))

    def test_separate_requires_successful_get_helper_cleanup_before_new_process(self):
        for child in (Child(1,exitcode=7),Child(1,stuck=True)):
            report,_,_,factory,dialer,_=self.call('separate_helper',responses=[models_wire()],children=[child])
            self.assertTrue(report['metadata_get']['protocol_success'])
            self.assertEqual(report['model_post_attempts'],0)
            self.assertEqual(factory.call_count,1)
            self.assertEqual(dialer.call_count,1)
            self.assertEqual(report['failure_category'],'helper_cleanup_failed')

    def test_post_completion_then_cleanup_error_preserves_observed_answer_not_score(self):
        report,*_=self.call(children=[Child(4,exitcode=9)])
        self.assertFalse(report['protocol_success'])
        self.assertFalse(report['eligible_for_scoring'])
        self.assertTrue(report['response_completion_observed'])
        self.assertEqual(report['observed_completion']['answer'],21)
        self.assertIn('21',report['observed_completion']['final_output'])
        self.assertEqual(report['observed_completion']['response_usage']['output_tokens'],9)

    def test_post_error_body_and_cookies_not_counted_as_answer(self):
        raw=json.dumps({'error':{'code':TOKEN,'message':'最终答案：21个'}}).encode()
        report,*_=self.call(responses=[http_wire(raw,429,headers([('Retry-After','Wed, 21 Oct 2030 07:28:00 GMT')]))])
        self.assertFalse(report['protocol_success'])
        self.assertIsNone(report['answer'])
        self.assertEqual(report['http_status'],429)
        self.assertEqual(report['error_body']['error']['code'],'other')
        self.assertNotIn(TOKEN,json.dumps(report))
        self.assertNotIn('2030',json.dumps(report['retry_after']))

    def test_post_foreign_identity_conflict_no_scoring(self):
        events=b'data: {"type":"response.created","response":{"id":"first"}}\n\ndata: {"type":"response.completed","response":{"id":"other","status":"completed","output":[]}}\n\n'
        report,*_=self.call(responses=[http_wire(events)])
        self.assertFalse(report['protocol_success'])
        self.assertIsNone(report['answer'])

    def test_real_native_ascii_error_and_sse_unknown_error_codes_are_safe(self):
        report,*_=self.call('same_helper',responses=[frame(4,b'upstream_connect')])
        self.assertEqual(report['metadata_get']['native_error']['code'],'upstream_connect')
        self.assertEqual(report['model_post_attempts'],0)
        raw=b'data: {"type":"error","error":{"code":"private-error-identifier","type":"private-error-type","message":"private error text"}}\n\n'
        report,*_=self.call(responses=[http_wire(raw)])
        self.assertFalse(report['protocol_success'])
        self.assertEqual(report['response_error']['code'],'other')
        self.assertEqual(report['response_error']['type'],'other')
        self.assertNotIn('private-error',json.dumps(report))

    def test_bad_helper_readiness_runtime_lifecycle_never_dispatch(self):
        for change in ({'ready_change':{'protocol':True}},{'ready_change':{'http_client_lifecycle':'pooled'}},
                       {'runtime_sha':'b'*64}):
            report,sockets,_,_,dialer,_=self.call('same_helper',**change)
            self.assertFalse(report['protocol_success'])
            self.assertEqual(dialer.call_count,0)
            self.assertEqual(report['model_post_attempts'],0)

    def test_helper_dying_between_get_and_post_is_not_restarted(self):
        child=Child(1);stream=Socket(models_wire())
        original=stream.close
        def close():original();child.returncode=6
        stream.close=close
        report,_,_,factory,dialer,_=self.call('same_helper',sockets=[stream],children=[child])
        self.assertEqual(factory.call_count,1)
        self.assertEqual(dialer.call_count,1)
        self.assertEqual(report['model_post_attempts'],0)

    def test_missing_zstd_capability_never_model_post(self):
        cap=captured();cap['request_compression']='zstd'
        report,_,_,_,dialer,_=self.call('same_helper',captured=cap,ready_change={'capabilities':[]})
        self.assertFalse(report['protocol_success'])
        self.assertEqual(report['model_post_attempts'],0)
        self.assertEqual(dialer.call_count,1)

    def test_input_pins_identity_headers_and_invalid_modes_reject_before_helper(self):
        bad=captured();bad['body_sha256']='b'*64
        bad_length=captured();bad_length['body_bytes']=True
        bad_header=captured();bad_header['headers']+=headers([('Cookie','private-cookie')])
        bad_identity=dict(IDENTITY,originator='invented')
        get_bad=headers([('User-Agent',IDENTITY['user_agent']),('Originator',IDENTITY['originator']),('x-codex-routing-hint','model=x')])
        for kwargs in ({'captured':bad},{'captured':bad_length},{'captured':bad_header},{'application_identity':bad_identity},
                       {'models_headers':get_bad},{'upstream':'https://example.com'},{'timeout':241},{'timeout':True},
                       {'deadline':time.monotonic()-1},{'body':BODY+b' ', 'captured':captured()},
                       {'token':'bad\rsecret'}):
            report,_,_,factory,_,_=self.call(**kwargs)
            self.assertFalse(report['protocol_success']);factory.assert_not_called()
        report,_,_,factory,_,_=self.call('not-a-mode')
        self.assertFalse(report['protocol_success']);factory.assert_not_called()

    def test_explicit_models_version_or_residency_mismatch_rejects_without_start(self):
        for pairs in ([('User-Agent',IDENTITY['user_agent']),('Originator',IDENTITY['originator'])],
                      [('User-Agent',IDENTITY['user_agent']),('Originator',IDENTITY['originator']),('Version','other')],
                      [('User-Agent',IDENTITY['user_agent']),('Originator',IDENTITY['originator']),('Version','0.155.0'),('x-openai-internal-codex-residency','new')]):
            report,_,_,factory,_,_=self.call('same_helper',models_headers=headers(pairs))
            self.assertFalse(report['protocol_success']);factory.assert_not_called()

    def test_partial_start_closed_pipe_cleanup_cannot_escape_receipt(self):
        child=Child(3)
        class ClosedPipe:
            def __init__(self):self.buffer=Buffer()
            def write(self,raw):return self.buffer.write(raw)
            def getvalue(self):return self.buffer.getvalue()
            def close(self):raise ValueError('private pipe error')
        child.stdin=ClosedPipe()
        report,*_=self.call(children=[child])
        self.assertFalse(report['protocol_success'])
        self.assertTrue(report['helper_reaped'])
        self.assertNotIn('private pipe error',json.dumps(report))

    def test_response_header_order_limits_and_large_get_bound(self):
        head=frame(1,b'{"status":200,"version":"HTTP/2.0","headers":[]}')
        cases=[head+head,frame(1,b'{"status":true,"version":"HTTP/2.0","headers":[]}'),
               http_wire(b'{}',response_headers=[['Set-Cookie','bad64']]),head+frame(3,b'x'),
               struct.pack('!BI',2,probe.MAX_HEAD+1),frame(99),
               head+frame(2,b'x'*probe.MAX_HEAD)*33+frame(3)]
        for wire in cases:
            report,_,_,_,dialer,_=self.call('same_helper',responses=[wire])
            self.assertFalse(report['protocol_success'])
            self.assertEqual(dialer.call_count,1)
            self.assertEqual(report['model_post_attempts'],0)


class HeaderPrivacyTests(unittest.TestCase):
    def test_cookie_allowlist_and_attribute_hashes_do_not_expose_private_names_or_values(self):
        rows=headers([('Set-Cookie','__oailb=private-value; Domain=private-domain; Path=/private-path; Max-Age=12; Secure; HttpOnly; SameSite=Lax'),
                      ('Set-Cookie','private-cookie-name=private-cookie-value; private-attr=private-attribute'),
                      ('Set-Cookie','cf_chl_private-id=private-challenge; Max-Age=-1')])
        result=probe.safe_response_headers(rows)
        self.assertEqual(result['set_cookie'][0]['name'],'__oailb')
        self.assertTrue(result['set_cookie'][0]['allowed_infrastructure_name'])
        self.assertFalse(result['set_cookie'][1]['allowed_infrastructure_name'])
        self.assertEqual(result['set_cookie'][2]['name'],'cf_chl_*')
        self.assertEqual(result['set_cookie'][0]['domain_sha256'],probe.sha(b'private-domain'))
        self.assertEqual(result['set_cookie'][0]['path_sha256'],probe.sha(b'/private-path'))
        self.assertNotIn('private',json.dumps(result))

    def test_error_unknown_fields_nonjson_and_retryafter_are_hash_only(self):
        value={'private-key':'private-value','error':{'code':'private-code','message':{'private-nested':'private-message'},'status':429}}
        result=probe.safe_error_body(json.dumps(value).encode())
        self.assertNotIn('private',json.dumps(result))
        self.assertEqual(result['error']['status'],429)
        self.assertEqual(probe.safe_error_body(b'private not JSON')['json_valid'],False)
        result=probe.safe_response_headers(headers([('Retry-After','12'),('Retry-After','private-date')]))
        self.assertEqual(result['retry_after'][0]['seconds'],12)
        self.assertNotIn('private',json.dumps(result))


@unittest.skipUnless(os.name=='posix','real native helper uses POSIX IPC')
class RealHelperLoopbackTests(unittest.TestCase):
    def test_all_modes_same_token_exact_post_and_process_reuse(self):
        helper=os.environ.get('CODEX_NATIVE_HTTP_TEST_BIN')
        if not helper or not Path(helper).is_file():self.skipTest('explicit Rust helper required')
        observations=[]
        class Handler(BaseHTTPRequestHandler):
            def do_GET(self):
                observations.append(('GET',self.path,b'',self.headers.get('Authorization'),self.headers.get('ChatGPT-Account-Id')))
                raw=catalog_body()
                self.send_response(200);self.send_header('Content-Type','application/json')
                self.send_header('Set-Cookie','__oailb=synthetic-loopback; Path=/')
                self.send_header('Content-Length',str(len(raw)));self.end_headers();self.wfile.write(raw)
            def do_POST(self):
                raw=self.rfile.read(int(self.headers['Content-Length']))
                observations.append(('POST',self.path,raw,self.headers.get('Authorization'),self.headers.get('ChatGPT-Account-Id')))
                value={'type':'response.completed','response':{'id':'r','status':'completed','output':[{'type':'message','role':'assistant','content':[{'type':'output_text','text':'最终答案：21个'}]}]}}
                data=b'data: '+json.dumps(value,ensure_ascii=False).encode()+b'\n\n'
                self.send_response(200);self.send_header('Content-Type','text/event-stream')
                self.send_header('Content-Length',str(len(data)));self.end_headers();self.wfile.write(data)
            def log_message(self,*args):pass
        server=ThreadingHTTPServer(('127.0.0.1',0),Handler)
        worker=threading.Thread(target=server.serve_forever,daemon=True);worker.start()
        try:
            for mode in ('cold','same_helper','separate_helper'):
                with self.subTest(mode=mode):
                    start=len(observations)
                    result=probe.native_lifecycle_request(Path(helper),probe.file_digest(Path(helper)),BODY,captured(),TOKEN,WORKSPACE,
                        timeout=20,mode=mode,application_identity=IDENTITY,upstream='http://127.0.0.1:%d'%server.server_port)
                    self.assertTrue(result['protocol_success'],result)
                    self.assertEqual(result['answer'],21)
                    actual=observations[start:]
                    self.assertEqual([entry[0] for entry in actual],['POST'] if mode=='cold' else ['GET','POST'])
                    self.assertEqual(actual[-1][2],BODY)
                    self.assertTrue(all(entry[3:] == ('Bearer '+TOKEN,WORKSPACE) for entry in actual))
                    self.assertEqual(result['same_helper_reuse_verified'],mode=='same_helper')
                    self.assertEqual(result['separate_helper_verified'],mode=='separate_helper')
                    self.assertTrue(result['helper_reaped'] and result['local_socket_absent'])
                    self.assertFalse(result['helper_forced_kill'])
                    # HTTP loopback cannot validate the HTTPS ChatGPT cookie jar.
                    self.assertFalse(result['upstream_wire_capture'])
        finally:
            server.shutdown();server.server_close();worker.join(timeout=3)

    def test_https_chatgpt_same_credentials_cookie_jar_requires_same_helper(self):
        helper=os.environ.get('CODEX_NATIVE_HTTP_TEST_BIN')
        if not helper or not Path(helper).is_file():self.skipTest('explicit Rust helper required')
        openssl=shutil.which('openssl')
        if not openssl:self.skipTest('openssl required for temporary trusted synthetic certificate')
        seen=[];sni=[];proxy_errors=[]
        with tempfile.TemporaryDirectory(prefix='qcookie-tls-fixture-') as directory:
            key,cert=Path(directory)/'key.pem',Path(directory)/'cert.pem'
            ca_key,ca_cert=Path(directory)/'ca-key.pem',Path(directory)/'ca.pem'
            csr,extensions=Path(directory)/'leaf.csr',Path(directory)/'leaf.ext'
            generated=subprocess.run([openssl,'req','-x509','-newkey','rsa:2048','-nodes','-days','1',
                '-subj','/CN=Synthetic cookie lifecycle root',
                '-addext','basicConstraints=critical,CA:TRUE','-addext','keyUsage=critical,keyCertSign,cRLSign',
                '-keyout',str(ca_key),'-out',str(ca_cert)],
                stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,timeout=15)
            self.assertEqual(generated.returncode,0)
            generated=subprocess.run([openssl,'req','-new','-newkey','rsa:2048','-nodes','-subj','/CN=chatgpt.com',
                '-keyout',str(key),'-out',str(csr)],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,timeout=15)
            self.assertEqual(generated.returncode,0)
            extensions.write_text('basicConstraints=critical,CA:FALSE\nsubjectAltName=DNS:chatgpt.com\nkeyUsage=critical,digitalSignature,keyEncipherment\nextendedKeyUsage=serverAuth\n')
            generated=subprocess.run([openssl,'x509','-req','-in',str(csr),'-CA',str(ca_cert),'-CAkey',str(ca_key),
                '-CAcreateserial','-days','1','-extfile',str(extensions),'-out',str(cert)],
                stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,timeout=15)
            self.assertEqual(generated.returncode,0)
            for path in (key,cert,ca_key,ca_cert):os.chmod(path,0o600)
            class Handler(BaseHTTPRequestHandler):
                def respond(self,raw,content):
                    self.send_response(200);self.send_header('Content-Type',content)
                    if self.command=='GET':
                        self.send_header('Set-Cookie','__oailb=synthetic-route; Secure; Path=/backend-api/codex; Max-Age=60')
                        self.send_header('Set-Cookie','__cf_bm=synthetic-cf; Secure; Path=/backend-api/codex; Max-Age=60')
                        self.send_header('Set-Cookie','session=synthetic-never-store; Secure; Path=/')
                        self.send_header('Set-Cookie','__cflb=synthetic-wrong-path; Secure; Path=/other-only')
                    self.send_header('Content-Length',str(len(raw)));self.end_headers();self.wfile.write(raw)
                def do_GET(self):
                    seen.append({'method':'GET','cookie':self.headers.get('Cookie'),'token':self.headers.get('Authorization'),
                                 'workspace':self.headers.get('ChatGPT-Account-Id')})
                    self.respond(catalog_body(),'application/json')
                def do_POST(self):
                    raw=self.rfile.read(int(self.headers['Content-Length']))
                    seen.append({'method':'POST','cookie':self.headers.get('Cookie'),'token':self.headers.get('Authorization'),
                                 'workspace':self.headers.get('ChatGPT-Account-Id'),'body':raw})
                    value={'type':'response.completed','response':{'id':'synthetic-cookie-response','status':'completed',
                        'output':[{'type':'message','role':'assistant','content':[{'type':'output_text','text':'最终答案：21个'}]}]}}
                    self.respond(b'data: '+json.dumps(value,ensure_ascii=False).encode()+b'\n\n','text/event-stream')
                def log_message(self,*args):pass
            tls_server=ThreadingHTTPServer(('127.0.0.1',0),Handler)
            context=ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
            context.minimum_version=ssl.TLSVersion.TLSv1_2
            context.load_cert_chain(str(cert),str(key))
            context.set_servername_callback(lambda sock,name,ctx:sni.append(name))
            tls_server.socket=context.wrap_socket(tls_server.socket,server_side=True)
            tls_worker=threading.Thread(target=tls_server.serve_forever,daemon=True);tls_worker.start()
            class Tunnel(socketserver.BaseRequestHandler):
                def handle(self):
                    remote=None
                    try:
                        self.request.settimeout(5)
                        request=b''
                        while not request.endswith(b'\r\n\r\n'):
                            request+=probe.read_exact(self.request.recv,1)
                            if len(request)>16384:raise ValueError('oversized tunnel request')
                        if request.split(b'\r\n')[0] != b'CONNECT chatgpt.com:443 HTTP/1.1':
                            raise ValueError('non-synthetic target')
                        remote=socket.create_connection(tls_server.server_address,timeout=5)
                        self.request.sendall(b'HTTP/1.1 200 Connection Established\r\n\r\n')
                        while True:
                            ready,_,_=select.select([self.request,remote],[],[],5)
                            if not ready:return
                            for source in ready:
                                data=source.recv(65536)
                                if not data:return
                                (remote if source is self.request else self.request).sendall(data)
                    except Exception as error:
                        proxy_errors.append(type(error).__name__)
                    finally:
                        if remote:remote.close()
            proxy_server=socketserver.ThreadingTCPServer(('127.0.0.1',0),Tunnel)
            proxy_server.daemon_threads=True
            proxy_worker=threading.Thread(target=proxy_server.serve_forever,daemon=True);proxy_worker.start()
            proxy_url='http://127.0.0.1:%d'%proxy_server.server_address[1]
            real_popen=probe.subprocess.Popen
            class StartupProxy:
                def __init__(self,stream):self.stream,self.buffer=stream,bytearray();self.started=False
                def write(self,data):
                    if self.started:return self.stream.write(data)
                    self.buffer.extend(data)
                    if len(self.buffer)>=4:
                        size=struct.unpack('!I',self.buffer[:4])[0]
                        if len(self.buffer)>=size+4:
                            if len(self.buffer)!=size+4:raise ValueError('unexpected startup tail')
                            value=json.loads(self.buffer[4:]);value['proxy']=proxy_url
                            raw=json.dumps(value,separators=(',',':')).encode()
                            probe.write_all(self.stream.write,struct.pack('!I',len(raw))+raw)
                            self.started=True
                    return len(data)
                def close(self):self.stream.close()
            def start_fixture_process(*args,**kwargs):
                # Only this test injects a temporary CA and an explicit loopback
                # CONNECT route. Public probe API/environment remains fixed.
                kwargs['env']=dict(kwargs['env'],CODEX_CA_CERTIFICATE=str(ca_cert))
                child=real_popen(*args,**kwargs);child.stdin=StartupProxy(child.stdin)
                return child
            try:
                for mode in ('cold','same_helper','separate_helper'):
                    with self.subTest(mode=mode):
                        first=len(seen)
                        with mock.patch.object(probe.subprocess,'Popen',side_effect=start_fixture_process):
                            report=probe.native_lifecycle_request(Path(helper),probe.file_digest(Path(helper)),BODY,captured(),TOKEN,WORKSPACE,
                                timeout=20,mode=mode,application_identity=IDENTITY)
                        self.assertTrue(report['protocol_success'],report)
                        rows=seen[first:]
                        self.assertEqual([row['method'] for row in rows],['POST'] if mode=='cold' else ['GET','POST'])
                        self.assertTrue(all(row['token']=='Bearer '+TOKEN and row['workspace']==WORKSPACE for row in rows))
                        self.assertEqual(rows[-1]['body'],BODY)
                        cookie=rows[-1]['cookie'] or ''
                        if mode=='same_helper':
                            self.assertIn('__oailb=synthetic-route',cookie)
                            self.assertIn('__cf_bm=synthetic-cf',cookie)
                            self.assertTrue(report['same_helper_reuse_verified'])
                        else:self.assertEqual(cookie,'')
                        self.assertNotIn('session=',cookie)
                        self.assertNotIn('__cflb=',cookie)
                        self.assertTrue(report['helper_reaped'] and report['local_socket_absent'])
                        self.assertFalse(report['helper_forced_kill'])
                self.assertEqual(len(sni),5) # cli_request builds a fresh client/connection per request.
                self.assertTrue(all(name=='chatgpt.com' for name in sni))
                self.assertFalse(proxy_errors,proxy_errors)
            finally:
                proxy_server.shutdown();proxy_server.server_close();proxy_worker.join(timeout=3)
                tls_server.shutdown();tls_server.server_close();tls_worker.join(timeout=3)


if __name__=='__main__':unittest.main()
