#!/usr/bin/env python3
"""Export synthetic conditional-provider writer vectors; never read real captures."""
import argparse
import hashlib
import json
from pathlib import Path

from integration_env import ROOT, connect


REMOTE = r'''
import hashlib,json,pathlib,re,uuid
root=pathlib.Path(CAPTURE_ROOT)
assert root.parent==pathlib.Path('/opt/codex-agent-integration/appserver-parity/conditional')
summary=json.loads((root/'summary.json').read_text())
assert summary['network_namespace']=='isolated'
assert summary['official_binary_sha256']=='660e159a49e823ac8e5986cb238f73158ce4b957d40d9292f8de90862644b501'
assert all(not x['real_credentials_loaded'] and not x['real_upstream'] and x['status']=='completed' for x in summary['outcomes'])
mapping={}
def replacement(match):
 value=match.group()
 if value not in mapping:mapping[value]=str(uuid.uuid5(uuid.NAMESPACE_OID,'synthetic-conditional-'+str(len(mapping))))
 return mapping[value]
def clean(value):
 if isinstance(value,str):
  value=re.sub(r'/opt/codex-agent-integration/appserver-parity/conditional/[^\s<>"\']+', '/fixture/workspace',value)
  return re.sub(r'\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b',replacement,value)
 if isinstance(value,list):return[clean(x)for x in value]
 if isinstance(value,dict):return{k:clean(v)for k,v in value.items()}
 return value
vectors=[]
headers_allowed={'content-type','accept','authorization','chatgpt-account-id','originator','user-agent','x-codex-image-turn-id','openai-beta','x-codex-turn-metadata','x-client-request-id','x-openai-subagent','session-id','thread-id','x-codex-window-id','x-openai-internal-codex-responses-lite'}
for route in ['app','exec']:
 for case in ['images-generation','images-edit','guardian-classifier']:
  records=json.loads((root/(route+'-'+case)/'requests.json').read_text())
  for record in records:
   path=record['path'];ws=record.get('websocket_message',False)
   if '/images/'in path: endpoint=path.split('/codex/',1)[1]
   elif path.endswith('/guardian')and record['method']=='POST':endpoint='guardian'
   elif path.endswith('/guardian-classifier')and ws:endpoint='guardian-classifier'
   else:continue
   headers={k.lower():v for k,v in record['headers'].items()}
   assert headers.get('authorization')=='Bearer synthetic-access-not-a-real-token'
   assert headers.get('chatgpt-account-id')in[None,'fixture-account']
   safe=clean({k:v for k,v in headers.items()if k in headers_allowed})
   safe['authorization']='Bearer SYNTHETIC_FIXTURE_ACCESS_TOKEN'
   vectors.append({'name':route+'-'+endpoint.replace('/','-'),'route':route,'provider_relative_path':endpoint,
      'evidence':'official_binary_loopback_capture','real_upstream':False,'network_namespace':'isolated',
      'method':record['method'],'transport':'websocket_response_create'if ws else'http_json',
      'headers':safe,'body':clean(record['body']),'original_body_sha256':record['body_sha256'],
      'captured_case':case,'sanitization':['synthetic authorization placeholder','consistent UUID renaming','isolated workspace paths replaced']})
result={'schema':'codex-conditional-provider-vectors-v1','capture_run':root.name,'official_version':summary['official_version'],
  'official_binary_sha256':summary['official_binary_sha256'],'network_namespace':'isolated','vectors':vectors}
encoded=json.dumps(result,ensure_ascii=False,indent=2)
assert not re.search(r'eyJ[A-Za-z0-9_-]{12,}\.[A-Za-z0-9_-]+',encoded)
print(encoded)
'''


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("capture_root")
    parser.add_argument("--output", type=Path, default=ROOT / "integration/appserver_parity/conditional_vectors.json")
    parser.add_argument("--auxiliary-output", type=Path, default=ROOT / "internal/server/testdata/appserver_auxiliary_vectors.json")
    args = parser.parse_args()
    client = connect()
    try:
        stdin, stdout, stderr = client.exec_command("python3 -B -", timeout=60)
        stdin.write("CAPTURE_ROOT=" + repr(args.capture_root) + "\n" + REMOTE)
        stdin.channel.shutdown_write()
        raw = stdout.read()
        status = stdout.channel.recv_exit_status()
        error = stderr.read()
        if status:
            raise RuntimeError("synthetic_vector_export_failed_stderr_bytes=" + str(len(error)))
        result = json.loads(raw)
    finally:
        client.close()
    source_root = ROOT / ".local/codex-source-0.155.0/codex-rs"
    sources = ["codex-api/src/endpoint/images.rs", "codex-api/src/images.rs", "codex-api/src/endpoint/memories.rs",
               "codex-api/src/common.rs", "codex-api/src/endpoint/responses.rs", "codex-api/src/endpoint/session.rs",
               "core/src/client.rs", "ext/image-generation/src/backend.rs", "ext/image-generation/src/tool.rs",
               "ext/guardian-v2/src/async_scorer/sampler.rs", "ext/guardian-v2/src/async_scorer/sampler/connection_pool.rs"]
    result["source_files_sha256"] = {name: hashlib.sha256((source_root / name).read_bytes()).hexdigest() for name in sources}
    result["vectors"].append({"name": "source-only-memories-trace-summarize", "route": "shared_source",
        "provider_relative_path": "memories/trace_summarize", "evidence": "official_source_unit_fixture_not_executed",
        "real_upstream": False, "runtime_capture": False, "method": "POST", "transport": "http_json",
        "headers_source": ["EndpointSession auth.apply_auth", "ModelClient.build_subagent_headers originator",
                           "x-openai-subagent when a subagent", "x-openai-memgen-request=true for MemoryConsolidation"],
        "body": {"model": "gpt-test", "traces": [{"id": "trace-1", "metadata": {"source_path": "/fixture/trace.json"},
                 "items": [{"type": "message", "role": "user", "content": []}]}]},
        "optional_body_fields": {"reasoning": {"effort": "high"}},
        "expected_response": {"output": [{"trace_summary": "raw summary", "memory_summary": "memory summary"}]},
        "source_unit_test": "codex-api/src/endpoint/memories.rs:summarize_input_posts_expected_payload_and_parses_output",
        "source_unit_test_executed": False,
        "limitation": "No public AppServer or exec trigger established for this unary writer; source existence does not prove current runtime invocation."})
    result["claims"] = {"all_operating_system_egress_covered": False, "real_image_generation_performed": False,
                        "real_guardian_inference_performed": False, "real_accounts_loaded": False,
                        "writer_captured_endpoints": ["images/generations", "images/edits", "guardian", "guardian-classifier"],
                        "source_only_endpoints": ["memories/trace_summarize"]}
    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(json.dumps(result, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    from appserver_parity_conditional import PNG
    auxiliary = []
    for vector in result["vectors"]:
        if not vector["provider_relative_path"].startswith(("images/", "memories/")):
            continue
        row = dict(vector, response_status=200, response_content_type="application/json",
                   request_body_contract="Opaque provider-relative JSON; preserve all fields and nested arrays, and do not reinterpret tool execution.")
        if row["provider_relative_path"].startswith("images/"):
            row["expected_response"] = {"created": 1, "background": "opaque", "data": [{"b64_json": PNG, "generation_id": "fixture-image"}]}
            row["expected_response_evidence"] = "local_fixture_response_not_real_upstream"
            row["optional_response_header"] = "x-codex-imagegen-request-id"
        auxiliary.append(row)
    auxiliary_document = {"schema": "appserver-auxiliary-writer-vectors-v1", "official_binary_sha256": result["official_binary_sha256"],
        "capture_run": result["capture_run"], "network_namespace": result["network_namespace"],
        "source_files_sha256": result["source_files_sha256"], "vectors": auxiliary}
    args.auxiliary_output.parent.mkdir(parents=True, exist_ok=True)
    args.auxiliary_output.write_text(json.dumps(auxiliary_document, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    print(json.dumps({"vectors": len(result["vectors"]), "output": str(args.output), "auxiliary_vectors": len(auxiliary),
                      "auxiliary_output": str(args.auxiliary_output), "raw_real_credentials_exported": False}))


if __name__ == "__main__":
    main()
