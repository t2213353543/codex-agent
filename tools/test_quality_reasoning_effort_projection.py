import base64
import copy
import hashlib
import json
import unittest
from quality_reasoning_effort_projection import project_effort,EffortProjectionError

RAW='''{ "model":"gpt-5.6-sol", "stream":true,"store":false,"input":[{"role":"user","content":"原题🙂","effort":"medium","n":9007199254740993}],"reasoning": {"context":"all_turns","effort" : "medium","opaque":1.230000e12},"client_metadata":{"session_id":"private","effort":"medium"},"unknown":{"effort":"medium"}}'''.encode()
def head(raw):return {'schema':'codex-quality-captured-request-v1','method':'POST','path':'/backend-api/codex/responses',
    'request_compression':'zstd','body_sha256':hashlib.sha256(raw).hexdigest(),'body_bytes':len(raw),
    'headers':[[n,base64.b64encode(v.encode()).decode()] for n,v in (('User-Agent','codex_exec/0.155.0'),('Originator','codex_exec'),('Accept','text/event-stream'),('Content-Type','application/json'))]}
def project(raw=RAW,effort='xhigh',**kwargs):return project_effort(raw,head(raw),effort,supported_efforts=['medium','high','xhigh'],**kwargs)

class EffortTests(unittest.TestCase):
    def test_only_the_target_value_changes_byte_for_byte(self):
        h=head(RAW);snapshot=copy.deepcopy(h)
        body,updated,proof=project_effort(RAW,h,'xhigh',supported_efforts=['medium','xhigh'])
        self.assertEqual(body,RAW.replace(b'"effort" : "medium"',b'"effort" : "xhigh"',1))
        self.assertEqual(h,snapshot);self.assertEqual(updated['headers'],h['headers'])
        span=proof['value_span'];self.assertEqual(body[:span['projected_start']],RAW[:span['original_start']]);self.assertEqual(body[span['projected_end']:],RAW[span['original_end']:])
        self.assertIn(b'9007199254740993',body);self.assertIn(b'1.230000e12',body)
        self.assertNotIn('private',json.dumps(proof));self.assertTrue(proof['non_effort_bytes_unchanged'])
    def test_equal_control_preserves_original_escape_spelling(self):
        raw=RAW.replace(b'"effort" : "medium"',br'"ef\u0066ort" : "me\u0064ium"')
        body,_,proof=project(raw,'medium');self.assertEqual(body,raw);self.assertFalse(proof['body_changed'])
        body,_,_=project(raw);self.assertEqual(body,raw.replace(br'"me\u0064ium"',b'"xhigh"'))
    def test_duplicate_or_invalid_structure_is_rejected(self):
        for raw in (RAW.replace(b'"context":"all_turns"',b'"effort":"low"'),
                    RAW.replace(b'"n":9007199254740993',b'"n":1,"n":2'),
                    RAW.replace(b'"medium","opaque"',b'3,"opaque"'),
                    RAW.replace(b'"reasoning"',b'"Reasoning"'),
                    RAW.replace(b'"stream":true',b'"stream":false'),
                    RAW.replace(b'"store":false',b'"store":false,"previous_response_id":null')):
            with self.subTest(raw=hashlib.sha256(raw).hexdigest()),self.assertRaises(EffortProjectionError):project(raw)
    def test_declared_catalog_and_original_effort_are_required(self):
        for kwargs in ({'supported_efforts':['medium','high']},{'supported_efforts':['xhigh']},
                       {'supported_efforts':['medium','xhigh'],'expected_original':'low'},
                       {'supported_efforts':['medium',3]}):
            with self.subTest(kwargs=kwargs),self.assertRaises(EffortProjectionError):project_effort(RAW,head(RAW),'xhigh',**kwargs)
    def test_bad_head_pin_rejected_without_echoing_content(self):
        h=head(RAW);h['body_sha256']='0'*64
        with self.assertRaises(EffortProjectionError) as e:project_effort(RAW,h,'xhigh',supported_efforts=['medium','xhigh'])
        self.assertNotIn('private',str(e.exception))

if __name__=='__main__':unittest.main()
