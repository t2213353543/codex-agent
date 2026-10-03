"""Export only sanitized synthetic writer fixtures from a private egress capture.

Reads raw files only on the private host. Authorization, cookies, account headers,
server response bodies and opaque encrypted reasoning never leave that host.
The exporter verifies original prefix UUIDs before consistently renaming IDs.
"""
import argparse
import json
from pathlib import Path

from integration_env import ROOT, connect


REMOTE_SCRIPT = r'''
import base64, gzip, hashlib, json, os, pathlib, re, urllib.parse, uuid
private_output=globals().get('PRIVATE_OUTPUT')
root=pathlib.Path(CAPTURE_ROOT)
assert root.is_relative_to(pathlib.Path('/opt/codex-agent-integration/appserver-parity/runs'))
events=[json.loads(line) for line in (root/'manifest.jsonl').read_text().splitlines()]
def blob(info):
 p=root/info['file'];assert p.is_relative_to(root/'private');raw=p.read_bytes()
 if 'sha256' in info:assert hashlib.sha256(raw).hexdigest()==info['sha256']
 return raw
requests={e['flow_id']:e for e in events if e['event']=='request'}
models=[]
for fid,e in requests.items():
 head=json.loads(blob(e['private_head']))
 if urllib.parse.urlparse(head['url']).path!='/backend-api/codex/models':continue
 parts=[x for x in events if x.get('flow_id')==fid and x['event']=='stream_end' and x['stream']=='response-entity']
 for part in parts:
  raw=blob(part)
  if raw.startswith(b'\x1f\x8b'):raw=gzip.decompress(raw)
  models.extend(json.loads(raw)['models'])
ws=[e for e in events if e['event']=='websocket_message' and e.get('from_client')]
assert len(ws)==3,'fixture exporter is limited to the inspected three-message pilot'
payloads=[json.loads(blob(e['body'])) for e in ws]
assert all(p.get('type')=='response.create' for p in payloads)
first=payloads[0]
assert first.get('generate') is False and len(first['input'])==2
assert first['input'][0]['type']=='additional_tools'
tools=first['input'][0]['tools']
assert [t['name'] for t in tools]==['functions','clock','collaboration']
base=first['input'][1]['content'][0]['text']
assert base=='Complete only the supplied request. Use tools only when explicitly requested.', 'only the synthetic pilot prompt is exportable'
assert len(first['input'][1]['content'])==1
model=next(m for m in reversed(models) if m['slug']==first['model'])
allowed_model=['slug','use_responses_lite','default_reasoning_level','default_reasoning_summary','supports_reasoning_summary_parameter','support_verbosity','default_verbosity','supports_image_detail_original','multi_agent_reasoning_effort','supported_reasoning_levels','service_tiers']
model={k:model[k] for k in allowed_model if k in model}
original_thread=first['client_metadata']['thread_id']
original_ns=uuid.uuid5(uuid.NAMESPACE_OID,original_thread)
serialize=lambda obj:json.dumps(obj,separators=(',',':'),ensure_ascii=False)
assert first['input'][0]['id']=='at_'+str(uuid.uuid5(original_ns,serialize(tools)))
assert first['input'][1]['id']=='msg_'+str(uuid.uuid5(original_ns,base))
mapping={}
def mapped(value):
 if value not in mapping:mapping[value]=str(uuid.uuid5(uuid.NAMESPACE_OID,'capture-fixture-'+str(len(mapping))))
 return mapping[value]
uuid_re=re.compile(r'(?<![0-9a-f])[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}(?![0-9a-f])',re.I)
path_re=re.compile(r'/opt/codex-agent-integration/[^\s<>"\\]+')
def clean_string(value):
 if private_output:return value
 value=path_re.sub('/fixture/workspace',value)
 value=uuid_re.sub(lambda m:mapped(m.group()),value)
 # Never export an opaque server response ID, even when it is not a UUID.
 value=re.sub(r'\bresp_[A-Za-z0-9_-]+',lambda m:'resp_'+mapped(m.group()).replace('-',''),value)
 return value
def clean(value):
 if isinstance(value,str):return clean_string(value)
 if isinstance(value,list):return [clean(v) for v in value]
 if isinstance(value,dict):
  assert not any(k.lower() in ['authorization','cookie','set-cookie','access_token','refresh_token','id_token','chatgpt-account-id','encrypted_content'] for k in value)
  return {k:clean(v) for k,v in value.items()}
 return value
clean_payloads=clean(payloads)
thread=clean_payloads[0]['client_metadata']['thread_id'];ns=uuid.uuid5(uuid.NAMESPACE_OID,thread)
clean_tools=clean_payloads[0]['input'][0]['tools']
clean_payloads[0]['input'][0]['id']='at_'+str(uuid.uuid5(ns,serialize(clean_tools)))
clean_payloads[0]['input'][1]['id']='msg_'+str(uuid.uuid5(ns,clean_string(base)))
head=json.loads(blob(requests[ws[0]['flow_id']]['private_head']))
allowed_headers={'originator','x-codex-beta-features','x-client-request-id','session-id','thread-id','x-codex-window-id','x-codex-turn-metadata','openai-beta','x-codex-routing-hint'}
headers={base64.b64decode(k).decode().lower():clean_string(base64.b64decode(v).decode()) for k,v in head['headers'] if base64.b64decode(k).decode().lower() in allowed_headers}
if 'x-codex-routing-hint' in headers and not private_output:headers['x-codex-routing-hint']='fixture-routing-hint'
fixtures=[]
ws_special={'ws_request_header_x_openai_internal_codex_responses_lite','x-codex-ws-stream-request-start-ms','x-codex-turn-state','ws_request_header_traceparent','ws_request_header_tracestate'}
for idx,p in enumerate(clean_payloads):
 metadata=p['client_metadata'];extra={k:[v] for k,v in headers.items() if k in ['x-codex-window-id','x-codex-turn-metadata']}
 for k in ['x-codex-turn-state']:
  if k in metadata and not private_output:metadata[k]='fixture-turn-state'
 request={'ModelMetadataRaw':model,'ThreadID':metadata['thread_id'],'SessionID':metadata['session_id'],'Input':[], 'Tools':clean_tools, 'Instructions':clean_string(base),'Effort':'xhigh','ContentItemKinds':True,'ParallelToolCalls':True,'ClientMetadata':{k:v for k,v in metadata.items() if k not in ws_special},'Originator':headers.get('originator',''),'BetaFeatures':headers.get('x-codex-beta-features',''),'ExtraHeaders':extra,'OutputSchemaStrict':True,'TurnState':metadata.get('x-codex-turn-state','')}
 state={'Warmup':p.get('generate') is False,'StreamRequestStartMS':int(metadata['x-codex-ws-stream-request-start-ms']),'RoutingHint':headers.get('x-codex-routing-hint','')}
 if 'previous_response_id' in p:state.update(PreviousResponseID=p['previous_response_id'],InputDelta=p['input'])
 fixtures.append({'name':['prewarm','first_generation_delta','second_turn_delta'][idx],'request':request,'serialized_tools':serialize(clean_tools),'state':state,'expected':p,'expected_headers':headers if idx==0 else None,'source_body_sha256':ws[idx]['body']['sha256']})
result={'provenance':{'capture_run':root.parents[1].name,'route':'official_app_server','official_version':'0.155.0','original_prefix_ids_verified':True,'sanitization':['UUIDs consistently renamed','runtime paths replaced with /fixture/workspace','opaque server response IDs renamed','routing hint replaced','only safe HTTP headers allowlisted'],'scope':'final writer and WS envelope; captured context is supplied unchanged except sanitization, not reconstructed by this test'},'fixtures':fixtures}
encoded=json.dumps(result,ensure_ascii=False,indent=2)
assert not re.search(r'eyJ[A-Za-z0-9_-]{15,}\.[A-Za-z0-9_-]+',encoded),'JWT-shaped value rejected'
assert not re.search(r'(?i)bearer\s+[a-z0-9]|sk-[a-z0-9]{15}',encoded),'credential-shaped value rejected'
if private_output:
 dest=pathlib.Path(private_output);assert dest.is_relative_to(root.parent)
 with os.fdopen(os.open(dest,os.O_WRONLY|os.O_CREAT|os.O_TRUNC,0o600),'w') as handle:handle.write(encoded)
 print(json.dumps({'private_fixture_written':True,'fixtures':len(fixtures),'original_prefix_ids_verified':True}))
else:print(encoded)
'''


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('capture_root')
    parser.add_argument('--output', default='internal/server/testdata/appserver_writer_pilot.json')
    args = parser.parse_args()
    client = connect()
    try:
        stdin, stdout, stderr = client.exec_command('python3 -B -', timeout=90)
        stdin.write('CAPTURE_ROOT=' + repr(args.capture_root) + '\n' + REMOTE_SCRIPT)
        stdin.channel.shutdown_write()
        raw = stdout.read()
        code = stdout.channel.recv_exit_status()
        error = stderr.read()
        if code:
            # Tracebacks can contain assertions but do not dump raw captures.
            raise RuntimeError('fixture export failed: ' + error.decode(errors='replace'))
        value = json.loads(raw)
        output = ROOT / args.output
        output.parent.mkdir(parents=True, exist_ok=True)
        output.write_bytes(raw)
        print(json.dumps({'output': str(output), 'fixtures': len(value['fixtures']), 'bytes': len(raw), 'prefix_ids_verified': value['provenance']['original_prefix_ids_verified']}))
    finally:
        client.close()


if __name__ == '__main__':
    main()
