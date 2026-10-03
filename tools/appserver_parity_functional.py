#!/usr/bin/env python3
"""Read private captures on their host and export only bounded functional proof."""
import argparse
import hashlib
import json
import re
from pathlib import Path
import shlex
from integration_env import ROOT, connect

INSTALL_COMMAND_PATTERN = r'\bnpm\s+(?:install|i)\s+(?:-g|--global)\s+@openai/codex\b'

def codex_install_command_present(text):
    return bool(re.search(INSTALL_COMMAND_PATTERN, text))


AUDIT = r'''
import base64,collections,hashlib,importlib.util,json,os,pathlib,re,urllib.parse
spec=importlib.util.spec_from_file_location('report',REPORT_PROGRAM);m=importlib.util.module_from_spec(spec);spec.loader.exec_module(m)
root=pathlib.Path(RUN_DIR);assert root.parent==pathlib.Path('/opt/codex-agent-integration/appserver-parity/runs')
route=root/'official_codex_cli';summary=json.loads((root/'summary.json').read_text());manifest=json.loads((route/'driver/driver-summary.json').read_text());cap=route/'capture';records=[json.loads(x)for x in(cap/'manifest.jsonl').read_text().splitlines()];public,inferences,_=m.route_report(route)
cases=[];markers={'text':'SAVED','resume':'PARITY_MARKER','compact-resume':'PARITY_MARKER','function-result':'PARITY_TOOL_MARKER','custom-result':'CUSTOM_TOOL_RESULT'}
for index,entry in enumerate(manifest,1):
 name=entry['case'];raw=(route/'driver'/('%02d-'%index+name+'.response')).read_bytes();events=[];terminal=None;acc=m.CompletedOutput()
 if raw.lstrip().startswith(b'{'):terminal=json.loads(raw)
 else:
  for line in raw.splitlines():
   if not line.startswith(b'data:'):continue
   try:event=json.loads(line[5:].strip())
   except ValueError:continue
   events.append(event);kind=event.get('type')
   if kind=='response.output_item.added':acc.started(event)
   if kind=='response.output_item.done':acc.observe(event)
   if kind in['response.created','response.in_progress']:acc.accept_response((event.get('response')or{}).get('id'))
   if kind=='response.completed':terminal=event.get('response')
 if terminal is None:raise ValueError('missing_terminal')
 items=terminal.get('output')or[]
 if not isinstance(items,list):items=[]
 provenance='json_response'
 if events:
  merged,provenance=acc.complete(terminal)
  if merged is not None:items=merged
 answer='\n'.join(c.get('text','')for x in items if x.get('type')=='message'and x.get('role')=='assistant'for c in(x.get('content')or[])if isinstance(c,dict)and c.get('type')=='output_text');usage=terminal.get('usage')or{}
 annotations=[a for x in items for c in(x.get('content')or[])if isinstance(c,dict)for a in(c.get('annotations')or[])if isinstance(a,dict)]
 checks={'http_200':entry['http']==200,'no_failed_terminal':not any(e.get('type')in['error','response.failed','response.incomplete']for e in events)}
 row={'case':name,'elapsed_ms':entry['elapsed_ms'],'model':terminal.get('model'),'output_types':dict(collections.Counter(x.get('type','unknown')for x in items)),'output_provenance':provenance,'answer_sha256':m.digest(answer.encode()),'answer_bytes':len(answer.encode()),'usage':{k:v for k,v in usage.items()if k in['input_tokens','output_tokens','total_tokens','input_tokens_details','output_tokens_details']},'usage_fields_present':{'input_tokens':'input_tokens'in usage,'output_tokens':'output_tokens'in usage,'cached_tokens':'cached_tokens'in(usage.get('input_tokens_details')or{}),'reasoning_tokens':'reasoning_tokens'in(usage.get('output_tokens_details')or{})}}
 if name in markers:checks['marker_exact']=answer.strip()==markers[name]
 if name not in['models','native-web-search']:checks['usage_fields_present']=all(row['usage_fields_present'].values())
 if name not in['models','native-web-search','compact']:checks['response_model_matches']=terminal.get('model')=='gpt-6-astra'
 if name=='compact':
  row['upstream_models']=sorted({x['body'].get('model')for x in inferences if x['case']=='compact'and not x.get('warmup')});checks['upstream_model_matches']=row['upstream_models']==['gpt-6-astra'];checks['compaction_output_present']=sum(x.get('type')=='compaction'for x in items)==1
 if name in['function-call','required-function']:checks['lookup_marker_called_once']=sum(x.get('type')=='function_call'and x.get('name')=='lookup_marker'for x in items)==1
 if name=='namespace-function':checks['namespace_ops_preserved']=sum(x.get('type')=='function_call'and x.get('name')=='lookup_marker'and x.get('namespace')=='ops'for x in items)==1
 if name=='custom-tool':checks['custom_grammar_input_exact']=sum(x.get('type')=='custom_tool_call'and x.get('name')=='emit_marker'and x.get('input')=='EXACT_MARKER'for x in items)==1
 if name=='structured-output':
  try:checks['structured_marker_exact']=json.loads(answer)=={'marker':'STRUCTURED_MARKER'}
  except ValueError:checks['structured_marker_exact']=False
  checks['strict_schema_retained']=(terminal.get('text')or{}).get('format',{}).get('strict')is True
 if name=='web-search':
  row['completed_web_search_calls']=sum(x.get('type')=='web_search_call'and x.get('status')=='completed'for x in items);row['citation_count']=sum(a.get('type')=='url_citation'for a in annotations);checks['actual_search_completed']=row['completed_web_search_calls']>0;checks['actual_citation_present']=row['citation_count']>0;checks['install_command_present']=bool(re.search(INSTALL_COMMAND_PATTERN,answer))
 if name=='native-web-search':
  output=terminal.get('output');checks['output_nonempty']=isinstance(output,str)and bool(output.strip());checks['results_present']=isinstance(terminal.get('results'),list)and bool(terminal['results']);checks['encrypted_output_present']=bool(terminal.get('encrypted_output'));row['result_count']=len(terminal.get('results')or[]);row['output_bytes']=len(output.encode())if isinstance(output,str)else 0;row['encrypted_output_bytes']=len(terminal.get('encrypted_output','').encode())
 row['checks']=checks;row['passed']=all(checks.values());cases.append(row)
identity=json.loads((root/'appserver-runtime-identity.json').read_text())if(root/'appserver-runtime-identity.json').exists()else None
request_headers={};handshakes=[];client_frames=[];completions={};last_request={};generation_phases=[]
for r in records:
 if r['event']=='request':
  private=json.loads((cap/r['private_head']['file']).read_bytes());h={base64.b64decode(k).decode().lower():base64.b64decode(v).decode()for k,v in private['headers']};request_headers[r['flow_id']]=h
  if h.get('upgrade','').lower()=='websocket':handshakes.append({'flow_id':r['flow_id'],'phase':r.get('phase',{}).get('case'),'originator':h.get('originator'),'user_agent_sha256':m.digest(h.get('user-agent','').encode()),'identity_matches_snapshot':bool(identity)and h.get('originator')==identity['originator']and h.get('user-agent')==identity['user_agent'],'turn_metadata':json.loads(h.get('x-codex-turn-metadata','{}'))})
 if r['event']=='websocket_message':
  body=json.loads((cap/r['body']['file']).read_bytes());flow=r['flow_id'];phase=r.get('phase',{}).get('case')
  if r.get('from_client')and body.get('type')=='response.create':
   metadata=body.get('client_metadata')or{};turn=json.loads(metadata.get('x-codex-turn-metadata','{}'));previous=body.get('previous_response_id');prior=completions.get((flow,previous));kind='warmup'if body.get('generate')is False else'generation';last_request[flow]={'kind':kind,'phase':phase}
   client_frames.append({'phase':phase,'kind':kind,'flow_id':flow,'input_count':len(body.get('input')or[]),'input_types':[x.get('type')for x in(body.get('input')or[])],'previous_response_present':bool(previous),'previous_response_matches_completed_same_flow':bool(prior),'previous_completed_kind':prior['kind']if prior else None,'previous_completed_phase':prior['phase']if prior else None,'request_kind':turn.get('request_kind'),'turn_id_empty':not bool(turn.get('turn_id')),'node_repl_disabled':turn.get('node_repl_disabled'),'node_repl_auto_review_required':turn.get('node_repl_auto_review_required'),'auto_review_enabled':turn.get('auto_review_enabled')})
  elif not r.get('from_client')and body.get('type')=='response.completed':
   rid=(body.get('response')or{}).get('id')
   if rid:completions[(flow,rid)]=last_request.get(flow,{})
for h in handshakes:
 turn=h.pop('turn_metadata');h['request_kind']=turn.get('request_kind');h['turn_id_empty']=not bool(turn.get('turn_id'));h['node_repl_disabled']=turn.get('node_repl_disabled');h['node_repl_auto_review_required']=turn.get('node_repl_auto_review_required');h['auto_review_enabled']=turn.get('auto_review_enabled')
trace=(route/'private-net.log').read_text();execs=[]
for line in trace.splitlines():
 match=re.search(r'execve\("([^\"]+)"',line)
 if match and line.endswith('= 0'):execs.append(pathlib.Path(match[1]).name)
counts=collections.Counter(execs)
result={'schema':'appserver-functional-proof-v2','run':root.name,'cases':cases,'functional_contract_passed':all(x['passed']for x in cases),'flow_errors':public['flow_errors'],'network':public['network'],'handshakes':handshakes,'client_frames':client_frames,'captured_http_statuses':dict(collections.Counter(str(x['response_status'])for x in public['operations'])),'bootstrap_official_init_count':summary.get('bootstrap_official_init_count',0),'generation_official_exec_count':sum(v for k,v in counts.items()if k in['codex','codex.exe','codex-with-capture-ca']),'generation_native_helper_exec_count':counts['codex-native-transport'],'production_settings_unchanged':summary.get('production_settings_unchanged'),'fixture_removed':summary.get('fixture_removed'),'account_record_sha256':summary.get('account_record_sha256'),'credential_sha256':summary.get('credential_sha256'),'identity':{'originator':identity['originator'],'user_agent_sha256':m.digest(identity['user_agent'].encode()),'source_cli_sha256':identity['source_cli_sha256']}if identity else None,'limits':['TLS/HTTP2 original fingerprint not proven by MITM.','Usage field presence does not promise cache hits.','Zero generation official exec excludes separate offline identity bootstrap.','Only traced connect and execve activity is observed; not every possible OS egress mechanism.']}
output=root/REPORT_NAME
with os.fdopen(os.open(output,os.O_WRONLY|os.O_CREAT|os.O_EXCL,0o600),'wb')as f:f.write(json.dumps(result,ensure_ascii=False,indent=2).encode())
print(json.dumps({'remote':str(output),'passed':result['functional_contract_passed'],'cases':len(cases),'bootstrap_official_init_count':result['bootstrap_official_init_count'],'generation_official_exec_count':result['generation_official_exec_count'],'generation_native_helper_exec_count':result['generation_native_helper_exec_count'],'flow_errors':result['flow_errors']}))
'''


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("run")
    parser.add_argument("--output", required=True, type=Path)
    args = parser.parse_args()
    program = ROOT / "tools/appserver_parity_report.py"
    program_hash = hashlib.sha256(program.read_bytes()).hexdigest()
    remote_program = "/opt/codex-agent-integration/appserver-parity/artifacts/report-" + program_hash + ".py"
    client = connect()
    try:
        with client.open_sftp() as sftp:
            sftp.put(str(program), remote_program)
            sftp.chmod(remote_program, 0o600)
        config = {"REPORT_PROGRAM": remote_program, "RUN_DIR": "/opt/codex-agent-integration/appserver-parity/runs/" + args.run,
                  "INSTALL_COMMAND_PATTERN":INSTALL_COMMAND_PATTERN,
                  "REPORT_NAME": args.output.name}
        stdin, stdout, stderr = client.exec_command("python3 -B -", timeout=120)
        stdin.write("\n".join(key + "=" + repr(value) for key, value in config.items()) + "\n" + AUDIT)
        stdin.channel.shutdown_write()
        raw = stdout.read()
        status = stdout.channel.recv_exit_status()
        error = stderr.read()
        if status:
            raise RuntimeError("functional_audit_failed_stderr_bytes=" + str(len(error)))
        result = json.loads(raw)
        args.output.parent.mkdir(parents=True, exist_ok=True)
        with client.open_sftp() as sftp:
            sftp.get(result["remote"], str(args.output))
        print(json.dumps(result))
    finally:
        client.close()


if __name__ == "__main__":
    main()
