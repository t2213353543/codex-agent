import base64
import copy
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import io
import json
import os
import threading
import time
import unittest
from unittest import mock

import quality_gateway_pair_probe as probe


KEY = "synthetic-private-local-key"
BODY = b'{ "model":"fixture", "stream":true,"store":false,"input":[{"role":"user","content":"original private question"}],"reasoning":{"effort":"medium"},"client_metadata":{"session_id":"private-session"}}'


def capture(body=BODY):
    return {"schema":"codex-quality-captured-request-v1","method":"POST","path":"/backend-api/codex/responses",
        "body_sha256":probe.sha(body),"body_bytes":len(body),"request_compression":"zstd",
        "headers":[[name,base64.b64encode(value.encode()).decode()] for name,value in (
            ("User-Agent","codex_exec/0.155.0 (SyntheticOS; x64) unknown"),("Originator","codex_exec"),("Version","0.155.0"),
            ("Accept","text/event-stream"),("Content-Type","application/json"),("Session-Id","private-session"))]}


def final(text="最终答案：21个", *, kind="response.completed", status="completed", response_id="synthetic-private-response"):
    value={"type":kind,"response":{"id":response_id,"status":status,"model":"fixture",
        "usage":{"input_tokens":12,"output_tokens":9,"output_tokens_details":{"reasoning_tokens":4}},
        "output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":text}]}]}}
    return b'data: '+json.dumps(value,ensure_ascii=False).encode()+b'\n\n'


class GatewayPairTests(unittest.TestCase):
    def fixture(self, mode="success", payload=None):
        seen=[]
        class Handler(BaseHTTPRequestHandler):
            protocol_version="HTTP/1.1"
            def do_POST(self):
                seen.append({"method":"POST","path":self.path,"headers":list(self.headers.items()),
                    "body":self.rfile.read(int(self.headers["Content-Length"]))})
                try:
                    if mode=="header_trickle":
                        self.wfile.write(b"HTTP/1.1 200 OK\r\n");self.wfile.flush()
                        for _ in range(100):
                            self.wfile.write(b"X-Slow: x\r\n");self.wfile.flush();time.sleep(.1)
                        return
                    status=429 if mode=="429" else 307 if mode=="redirect" else 200
                    data=payload if payload is not None else final()
                    self.send_response(status)
                    self.send_header("Content-Type","application/json" if mode in ("429","redirect","json200","error_oversize") else "text/event-stream")
                    if mode=="redirect":self.send_header("Location","https://must-not-follow.invalid/private")
                    if mode=="429":self.send_header("Retry-After","12")
                    if mode=="bad_encoding":self.send_header("Content-Encoding","gzip")
                    self.send_header("X-Request-Id","synthetic-private-id")
                    if mode not in ("fragment","body_stall","body_trickle","terminal_open"):
                        self.send_header("Content-Length",str(len(data)))
                    self.end_headers()
                    if mode=="body_stall":time.sleep(2);return
                    if mode=="body_trickle":
                        for _ in range(100):
                            self.wfile.write(b": keepalive\n\n");self.wfile.flush();time.sleep(.1)
                        return
                    if mode=="fragment":
                        for start in range(0,len(data),3):self.wfile.write(data[start:start+3]);self.wfile.flush()
                    else:self.wfile.write(data);self.wfile.flush()
                    if mode=="terminal_open":time.sleep(2)
                except (BrokenPipeError,ConnectionResetError,ConnectionAbortedError):
                    pass
                finally:self.close_connection=True
            def log_message(self,*args):pass
        server=ThreadingHTTPServer(("127.0.0.1",0),Handler)
        server.daemon_threads=True
        thread=threading.Thread(target=lambda:server.serve_forever(poll_interval=.01),daemon=True);thread.start()
        self.addCleanup(lambda:(server.shutdown(),server.server_close(),thread.join(1)))
        return "http://127.0.0.1:%d/v1/responses"%server.server_port,seen

    def call(self,endpoint,**options):
        return probe.gateway_pair_request(endpoint,KEY,BODY,capture(),**options)

    def test_actual_loopback_exact_entity_header_identity_no_proxy_no_python_tls(self):
        endpoint,seen=self.fixture()
        raw=capture();before=copy.deepcopy(raw)
        with mock.patch.dict(os.environ,{"HTTP_PROXY":"http://must-not-use.invalid","HTTPS_PROXY":"http://must-not-use.invalid"}):
            result=probe.gateway_pair_request(endpoint,KEY,BODY,raw)
        self.assertTrue(result["protocol_success"],result)
        self.assertEqual(result["answer"],21)
        self.assertEqual(result["response_usage"]["output_tokens"],9)
        self.assertEqual(len(seen),1)
        self.assertEqual(seen[0]["body"],BODY)
        headers={k.lower():v for k,v in seen[0]["headers"]}
        self.assertEqual(headers["authorization"],"Bearer "+KEY)
        self.assertEqual(headers["session-id"],"private-session")
        self.assertEqual(headers["content-length"],str(len(BODY)))
        self.assertNotIn("content-encoding",headers)
        self.assertNotIn("accept-encoding",headers)
        self.assertNotIn("chatgpt-account-id",headers)
        self.assertEqual(result["source_request_compression"],"zstd")
        self.assertEqual(result["ingress_request_compression"],"identity")
        self.assertFalse(result["native_upstream_observed"])
        self.assertTrue(result["cleanup_completed"])
        self.assertEqual(raw,before)
        for secret in (KEY,"original private question","private-session","synthetic-private-response","synthetic-private-id"):
            self.assertNotIn(secret,json.dumps(result))

    def test_fragmented_sse_utf8_and_terminal_before_server_close(self):
        for mode in ("fragment","terminal_open"):
            with self.subTest(mode=mode):
                endpoint,_=self.fixture(mode)
                started=time.monotonic();result=self.call(endpoint,timeout=1)
                self.assertTrue(result["protocol_success"],result)
                self.assertEqual(result["answer"],21)
                self.assertLess(time.monotonic()-started,.8)
                self.assertFalse(result["response_eof_observed"])

    def test_429_error_and_redirect_preserved_without_answer_or_retry(self):
        for mode in ("429","redirect"):
            with self.subTest(mode=mode):
                raw=json.dumps({"error":{"code":"rate_limit_exceeded","message":KEY+" answer: 21"}}).encode()
                endpoint,seen=self.fixture(mode,raw);result=self.call(endpoint)
                self.assertFalse(result["protocol_success"])
                self.assertFalse(result["eligible_for_scoring"])
                self.assertIsNone(result["answer"])
                self.assertEqual(result["http_status"],429 if mode=="429" else 307)
                self.assertEqual(result["error_body"]["body_sha256"],probe.sha(raw))
                self.assertEqual(len(seen),1)
                self.assertEqual(result["redirects_followed"],0)
                self.assertNotIn(KEY,json.dumps(result))

    def test_failed_terminal_missing_terminal_and_empty_answer_not_quality_pass(self):
        cases=[final("answer: 21",kind="response.failed",status="failed"),b'data: {"type":"response.created","response":{"id":"r"}}\n\n',final("")]
        for payload in cases:
            endpoint,seen=self.fixture(payload=payload);result=self.call(endpoint)
            self.assertFalse(result["eligible_for_scoring"],result)
            self.assertIsNone(result["answer"])
            self.assertEqual(len(seen),1)

    def test_output_and_error_body_limits(self):
        for mode,payload,error in (("success",b": "+b"x"*(8<<20),"response_limit"),("error_oversize",b"x"*((64<<10)+1),"error_body_limit")):
            endpoint,seen=self.fixture(mode,payload);result=self.call(endpoint)
            self.assertFalse(result["protocol_success"])
            self.assertFalse(result["eligible_for_scoring"])
            self.assertEqual(result["error_class"],error)
            self.assertEqual(len(seen),1)

    def test_absolute_deadline_defeats_header_and_body_trickle(self):
        for mode in ("header_trickle","body_trickle","body_stall"):
            with self.subTest(mode=mode):
                endpoint,seen=self.fixture(mode)
                started=time.monotonic();result=self.call(endpoint,timeout=1,deadline=started+.35)
                self.assertFalse(result["protocol_success"])
                self.assertEqual(result["error_class"],"deadline_exceeded",result)
                self.assertLess(time.monotonic()-started,.9)
                self.assertEqual(len(seen),1)
                self.assertTrue(result["cleanup_completed"])

    def test_public_ambiguous_or_redirect_endpoints_never_connect(self):
        invalid=("https://127.0.0.1:1/v1/responses","http://localhost:1/v1/responses","http://127.1:1/v1/responses",
            "http://127.0.0.1:1/v1/responses?x=1","http://127.0.0.1:1/v1/responses#x","http://127.0.0.1:1/responses",
            "http://user:password@127.0.0.1:1/v1/responses","http://example.com:1/v1/responses","http://127.0.0.1:65536/v1/responses",
            "http://127.0.0.1:01/v1/responses","http://127.0.0.1:1/v1/%72esponses","http://127.0.0.1:1/v1/responses\n")
        for endpoint in invalid:
            with mock.patch.object(probe.http.client,"HTTPConnection") as connect:
                result=self.call(endpoint)
            connect.assert_not_called();self.assertEqual(result["model_post_attempts"],0)
            self.assertNotIn("password",json.dumps(result))

    def test_pin_or_unreviewed_credential_header_rejected_before_connection(self):
        for name,value in (("Cookie","secret"),("Authorization","Bearer other"),("ChatGPT-Account-Id","workspace"),
                           ("Content-Encoding","zstd"),("Host","evil.invalid"),("X-Codex-Unknown","unknown")):
            head=capture();head["headers"].append([name,base64.b64encode(value.encode()).decode()])
            with mock.patch.object(probe.http.client,"HTTPConnection") as connect:
                result=probe.gateway_pair_request("http://127.0.0.1:1/v1/responses",KEY,BODY,head)
            connect.assert_not_called();self.assertFalse(result["protocol_success"])
        with mock.patch.object(probe.http.client,"HTTPConnection") as connect:
            result=probe.gateway_pair_request("http://127.0.0.1:1/v1/responses",KEY,BODY+b" ",capture())
        connect.assert_not_called();self.assertEqual(result["model_post_attempts"],0)

    def test_duplicate_sse_and_foreign_response_identity_rejected(self):
        for raw in (b'data: {"type":"response.completed","type":"response.failed"}\n\n',
                    b'data: {"type":"response.created","response":{"id":"other"}}\n\n'+final()):
            endpoint,_=self.fixture(payload=raw);result=self.call(endpoint)
            self.assertFalse(result["protocol_success"])
            self.assertFalse(result["eligible_for_scoring"])

    def test_bad_media_type_or_encoding_is_not_model_answer(self):
        for mode in ("json200","bad_encoding"):
            endpoint,_=self.fixture(mode,payload=b'{"answer":21}')
            result=self.call(endpoint)
            self.assertFalse(result["protocol_success"])
            self.assertIsNone(result["answer"])

    def test_completed_answer_survives_cleanup_failure_without_becoming_scoreable(self):
        endpoint,_=self.fixture()
        original=probe.http.client.HTTPConnection.close
        def bad_close(connection):
            original(connection)
            raise OSError(KEY)
        with mock.patch.object(probe.http.client.HTTPConnection,"close",bad_close):
            result=self.call(endpoint)
        self.assertFalse(result["protocol_success"])
        self.assertFalse(result["eligible_for_scoring"])
        self.assertTrue(result["response_completion_observed"],result)
        self.assertIsNone(result["answer"])
        self.assertEqual(result["observed_completion"]["answer"],21)
        self.assertIn("21",result["observed_completion"]["final_output"])
        self.assertFalse(result["cleanup_completed"])
        self.assertNotIn(KEY,json.dumps(result))

    def test_connection_error_and_invalid_deadline_have_safe_reports(self):
        with mock.patch.object(probe.http.client.HTTPConnection,"connect",side_effect=OSError(KEY)):
            result=self.call("http://127.0.0.1:1/v1/responses")
        self.assertEqual(result["error_class"],"local_http_failure")
        self.assertEqual(result["model_post_attempts"],0)
        self.assertNotIn(KEY,json.dumps(result))
        for options in ({"timeout":True},{"timeout":241},{"deadline":float("nan")},{"deadline":time.monotonic()-1}):
            with mock.patch.object(probe.http.client,"HTTPConnection") as connect:
                self.call("http://127.0.0.1:1/v1/responses",**options)
            connect.assert_not_called()

    def test_unknown_sse_error_identifiers_remain_hash_only(self):
        data=b'data: {"type":"error","error":{"code":"private-server-code","type":"private-server-type","message":"private server message"}}\n\n'
        endpoint,_=self.fixture(payload=data);result=self.call(endpoint)
        self.assertFalse(result["protocol_success"])
        self.assertEqual(result["response_error"]["code"],"other")
        self.assertEqual(result["response_error"]["type"],"other")
        for secret in ("private-server-code","private-server-type","private server message"):
            self.assertNotIn(secret,json.dumps(result))


if __name__=="__main__":unittest.main()
