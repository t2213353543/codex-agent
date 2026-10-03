import base64
import copy
import hashlib
import json
import unittest
from quality_reasoning_mode_projection import project_mode

def head(body):
    return {'schema':'codex-quality-captured-request-v1','method':'POST','path':'/backend-api/codex/responses','request_compression':'zstd',
        'body_sha256':hashlib.sha256(body).hexdigest(),'body_bytes':len(body),
        'headers':[[n,base64.b64encode(v.encode()).decode()] for n,v in [('Accept','text/event-stream'),('Content-Type','application/json')]]}

class ModeTests(unittest.TestCase):
    def body(self,reasoning=b'{"effort":"medium","future":9007199254740993}'):
        return b'{"model":"gpt-5.6-sol","stream":true,"store":false,"input":[{"x":"\\u4e2d"}],"reasoning":'+reasoning+b',"opaque":"unchanged"}'
    def test_only_span_changes_and_header_values_untouched(self):
        for mode in ('standard','pro'):
            for reasoning in (b'{"effort":"medium"}',b'{"effort":"medium", "mode":"standard"}',b'{"effort":"medium","future":9007199254740993 }'):
                with self.subTest(mode=mode,reasoning=reasoning):
                    raw=self.body(reasoning);h=head(raw);before=copy.deepcopy(h)
                    out,new,proof=project_mode(raw,h,mode);s=proof['span']
                    self.assertEqual(raw[:s['start']],out[:s['start']]);self.assertEqual(raw[s['end']:],out[s['start']+s['replacement_bytes']:])
                    self.assertEqual(h,before);self.assertEqual(h['headers'],new['headers'])
                    self.assertEqual(json.loads(out)['reasoning']['mode'],mode)
                    self.assertIn(b'"opaque":"unchanged"',out)
    def test_invalid_controls_duplicate_and_bad_headers_rejected(self):
        cases=[self.body(b'{"effort":"high"}'),self.body(b'{"effort":"medium","mode":"pro"}'),
               self.body(b'{"effort":"medium","mode":"standard","mode":"pro"}'),self.body().replace(b'"store":false',b'"store":true')]
        for raw in cases:
            with self.subTest(raw=raw),self.assertRaises(Exception):project_mode(raw,head(raw),'pro')
        raw=self.body();h=head(raw);h['body_sha256']='0'*64
        with self.assertRaises(Exception):project_mode(raw,h,'pro')
    def test_standard_existing_preserves_original_mode_lexeme(self):
        raw=self.body(b'{"effort":"medium","mode":"stan\\u0064ard"}')
        out,_,_=project_mode(raw,head(raw),'standard');self.assertEqual(out,raw)

if __name__=='__main__':unittest.main()
