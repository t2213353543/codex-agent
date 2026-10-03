"""Exact nested token replacement; no real model/account/capture data needed."""
import base64,copy,json,unittest
from unittest import mock
import quality_prompt_variant_projection as projection
from quality_semantics_variants import request_content,validate_model_content,VARIANT_IDS
from mother_account_benchmark import CANDY_PROMPT

def body(escaped=False):
 prompt=json.dumps(CANDY_PROMPT,ensure_ascii=escaped)
 return (' { "model":"gpt-5.6-sol", "stream":true,"store":false,"instructions":"KEEP_private_instruction",'
  '"input":[{"role":"developer","content":[{"type":"input_text","text":"private system"}]},'
  '{"role":"user","content":[{"type":"input_text","text":"private environment"}]},'
  '{"type":"message","role":"user","content":[{"type":"input_text","text":'+prompt+'},{"type":"input_image","image_url":"private image"}]}],'
  '"prompt_cache_key":"private_cache", "client_metadata":{"session_id":"private_session","nested":{"input":"private_value"}},'
  '"tools":[{"unknown":"工具","schema":{"number":9007199254740993,"decimal":1.2300e+009,"huge":1e1000,"zero":-0}}],'
  '"unknown_private_field":{"escape":"\\u0061","nested":[true,null,7]}} ').encode()
def head(raw):
 return {'schema':'codex-quality-captured-request-v1','method':'POST','path':'/backend-api/codex/responses','body_sha256':projection.sha(raw),'body_bytes':len(raw),'request_compression':'zstd','headers':[[n,base64.b64encode(v.encode()).decode()] for n,v in [('User-Agent','codex_exec/0.155.0 fixture'),('Originator','codex_exec'),('Accept','text/event-stream'),('Content-Type','application/json'),('Session-Id','private_session')]]}
class PromptProjectionTests(unittest.TestCase):
 def test_only_one_nested_token_and_head_body_pins_change(self):
  for escaped in (False,True):
   for variant in VARIANT_IDS:
    with self.subTest(escaped=escaped,variant=variant):
     raw=body(escaped);captured=head(raw);snapshot=copy.deepcopy(captured)
     changed,next_head,proof=projection.project_prompt_variant(raw,captured,variant)
     span=proof['text_span'];start,end=span['original_start'],span['original_end']
     expected=raw if variant=='baseline' else raw[:start]+json.dumps(request_content(variant)['input'],ensure_ascii=False,separators=(',',':')).encode()+raw[end:]
     self.assertEqual(changed,expected);self.assertEqual(captured,snapshot);self.assertEqual(proof['source_pointer'],'/input/2/content/0/text')
     self.assertEqual(next_head['headers'],captured['headers']);self.assertIsNot(next_head['headers'],captured['headers']);self.assertEqual(next_head['body_sha256'],projection.sha(changed));self.assertEqual(next_head['body_bytes'],len(changed))
     for key in captured.keys()-{'body_sha256','body_bytes'}:self.assertEqual(captured[key],next_head[key])
     self.assertTrue(proof['all_other_body_bytes_unchanged']);self.assertTrue(proof['instructions_fields_unchanged']);self.assertFalse(proof['answer_key_inserted_into_body'])
     for numeric in (b'9007199254740993',b'1.2300e+009',b'1e1000',b'-0'):self.assertIn(numeric,changed)
     self.assertNotIn('private_instruction',json.dumps(proof));self.assertNotIn('private_cache',json.dumps(proof))
     validate_model_content(request_content(variant))
 def test_escaped_member_names_and_same_named_nested_text(self):
  raw=body(True).replace(b'"input":',b'"\\u0069nput":',1).replace(b'"content":',b'"\\u0063ontent":').replace(b'"text":',b'"te\\u0078t":')
  changed,_,proof=projection.project_prompt_variant(raw,head(raw),'clarified_shape_choice');self.assertEqual(proof['source_pointer'],'/input/2/content/0/text');self.assertIn(b'"\\u0069nput":',changed);self.assertIn(b'"te\\u0078t":',changed)
 def test_duplicate_members_any_depth_rejected_including_escaped_alias(self):
  for raw in (body().replace(b'"stream":true',b'"stream":true,"stream":true'),body().replace(b'"nested":[',b'"x":0,"\\u0078":1,"nested":['),body().replace(b'"role":"user"',b'"role":"user","role":"user"',1)):
   with self.subTest(),self.assertRaises(projection.PromptVariantProjectionError):projection.project_prompt_variant(raw,head(raw),'blind_control')
 def test_original_prompt_must_be_unique_and_in_exact_user_text_role(self):
  original=body();p=json.dumps(CANDY_PROMPT,ensure_ascii=False).encode()
  variants=[original.replace(p,json.dumps(CANDY_PROMPT+' extra',ensure_ascii=False).encode()),original.replace(b'"private environment"',p),original.replace(b'"private_value"',p),original.replace(b'"type":"message","role":"user"',b'"type":"message","role":"assistant"'),original.replace(b'"text":'+p,b'"other":'+p),original.replace(b'"type":"input_text","text":'+p,b'"type":"output_text","text":'+p)]
  for raw in variants:
   with self.subTest(),self.assertRaises(projection.PromptVariantProjectionError):projection.project_prompt_variant(raw,head(raw),'clarified_shape_choice')
 def test_boolean_model_previous_response_stream_and_bad_json_rejected(self):
  variants=[body().replace(b'"model":"gpt-5.6-sol"',b'"model":true'),body().replace(b'"stream":true',b'"stream":false'),body().replace(b'"store":false',b'"store":true'),body().replace(b'"stream":true',b'"previous_response_id":null,"stream":true'),body().replace(b'9007199254740993',b'NaN'),body().replace(b'9007199254740993',b'01'),body()+b'{}',b'{"private_secret":'+b'['*70+b'0'+b']'*70+b'}']
  for raw in variants:
   with self.subTest(),self.assertRaises(projection.PromptVariantProjectionError):projection.project_prompt_variant(raw,head(raw),'blind_control')
 def test_bad_head_or_unknown_variant_cannot_escape_secret(self):
  raw=body()
  for mutate in ('digest','length','auth','duplicate','unknownvariant'):
   captured=head(raw);variant='blind_control'
   if mutate=='digest':captured['body_sha256']='0'*64
   if mutate=='length':captured['body_bytes']-=1
   if mutate=='auth':captured['headers'].append(['Authorization',base64.b64encode(b'Bearer PRIVATE_SECRET').decode()])
   if mutate=='duplicate':captured['headers'].append(captured['headers'][0])
   if mutate=='unknownvariant':variant='PRIVATE_SECRET'
   with self.subTest(mutate=mutate):
    with self.assertRaises(projection.PromptVariantProjectionError) as error:projection.project_prompt_variant(raw,captured,variant)
    self.assertNotIn('PRIVATE_SECRET',str(error.exception))
 def test_projection_size_limit_checks_before_output(self):
  raw=body()
  with mock.patch.object(projection.strict,'MAX_BODY',len(raw)+5),self.assertRaises(projection.PromptVariantProjectionError):projection.project_prompt_variant(raw,head(raw),'clarified_shape_choice')
 def test_baseline_escaped_token_is_literal_noop(self):
  raw=body(True);result,captured,proof=projection.project_prompt_variant(raw,head(raw),'baseline');self.assertEqual(result,raw);self.assertEqual(captured,head(raw));self.assertFalse(proof['body_bytes_changed']);self.assertTrue(proof['original_benchmark_eligible'])
if __name__=='__main__':unittest.main(verbosity=2)
