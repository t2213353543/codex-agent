#!/usr/bin/env python3
"""Build a conservative App Server vs codex exec egress diff from pinned evidence.

This inventory never treats a source string as an observed request and never
labels a route complete merely because a handler name exists.
"""
from __future__ import annotations
import argparse,json,re,hashlib
from pathlib import Path

ROOT=Path(__file__).resolve().parents[1]
PROFILE_ROOT=ROOT/'.local/appserver-parity/client-profile'

def latest_profile():
    dirs=sorted([p for p in PROFILE_ROOT.iterdir() if p.is_dir() and (p/'app'/'requests.json').exists() and (p/'exec'/'requests.json').exists()])
    return dirs[-1] if dirs else None

def load_rows(path):
    if not path or not path.exists(): return []
    raw=json.loads(path.read_text(encoding='utf-8'))
    return raw if isinstance(raw,list) else []

def normalize(path):
    path=path.split('?',1)[0]
    path=re.sub(r'/tasks/[^/]+/turns/[^/]+/sibling_turns','/tasks/{id}/turns/{id}/sibling_turns',path)
    path=re.sub(r'/tasks/[^/]+','/tasks/{id}',path)
    path=re.sub(r'/plugins/[^/]+/(install|uninstall|shares)','/plugins/{id}/\\1',path)
    path=re.sub(r'/plugins/[^/]+','/plugins/{id}',path)
    path=re.sub(r'/workspace/[^/]+','/workspace/{id}',path)
    path=re.sub(r'/realtime/calls/[^/]+','/realtime/calls/{id}',path)
    path=re.sub(r'/live/[^/]+','/live/{id}',path)
    return path

def usable_path(value):
    value=value.strip()
    if not value or ' ' in value or '?' in value or '..' in value or '%' in value:
        return False
    if len(value)>220 or not re.fullmatch(r'/[A-Za-z0-9_./{}:-]+',value):
        return False
    return value not in {'/v1','/v1/','/models','/responses','/realtime','/live'}

DYNAMIC_KEYS={'id','call_id','response_id','session_id','thread_id','turn_id','stream_id','client_request_id','created_at','updated_at','timestamp','prompt_cache_key','task_id','installation_id'}
def body_shape(value, key=''):
    if key in DYNAMIC_KEYS:
        return '<dynamic>'
    if isinstance(value, dict):
        return {k: body_shape(value[k], k) for k in sorted(value)}
    if isinstance(value, list):
        return [body_shape(x) for x in value]
    if isinstance(value, (str,int,float,bool)) or value is None:
        return value
    return '<unknown>'

def body_shape_hash(row):
    value=row.get('body')
    raw=json.dumps(body_shape(value),ensure_ascii=False,sort_keys=True,separators=(',',':')).encode()
    return hashlib.sha256(raw).hexdigest()

def body_structure(value, key=''):
    if key in DYNAMIC_KEYS:
        return '<dynamic>'
    if isinstance(value, dict):
        return {k: body_structure(value[k], k) for k in sorted(value)}
    if isinstance(value, list):
        return {'array_len': len(value), 'item': body_structure(value[0]) if value else None}
    if isinstance(value, str): return '<string>'
    if isinstance(value, bool): return '<boolean>'
    if isinstance(value, (int,float)): return '<number>'
    if value is None: return None
    return '<unknown>'

def source_literals():
    roots=[ROOT/'internal/server',ROOT/'.local/codex-source-0.155.0/codex-rs']
    found=set()
    pattern=re.compile(r'(?P<q>["`])((?:/backend-api|/api/codex|/v1|/alpha|/wham|/guardian|/responses|/models|/images|/realtime|/live|/plugins|/connectors)[^"`\\\r\n]*)["`]')
    for root in roots:
        for p in root.rglob('*.go' if root.name=='server' else '*.rs'):
            if p.name.endswith('_test.go') or p.name.endswith('_test.rs') or 'tests' in p.parts:
                continue
            try:text=p.read_text(encoding='utf-8',errors='ignore')
            except OSError:continue
            for match in pattern.finditer(text):
                value=match.group(2)
                if len(value)<256 and usable_path(value): found.add(normalize(value))
    return found

def handler_paths():
    files=[ROOT/'internal/server/gateway_appserver_identity_bridge.go',ROOT/'internal/server/gateway_appserver_backend.go',ROOT/'internal/server/gateway_appserver_history.go',ROOT/'internal/server/gateway_appserver_auxiliary.go',ROOT/'internal/server/gateway_appserver_realtime.go',ROOT/'internal/server/gateway_appserver_turn_cost_http.go']
    values=set()
    for p in files:
        text=p.read_text(encoding='utf-8',errors='ignore')
        for value in re.findall(r'"(/(?:backend-api|v1|alpha|guardian|responses|models|images|realtime|live|plugins|connectors)[^"\r\n]*)"',text):
            if usable_path(value): values.add(normalize(value))
    # Model-path aliases are handled by appServerModelAlias rather than a
    # literal backend handler string.
    values.update({
        '/backend-api/codex/responses','/backend-api/codex/responses/compact',
        '/backend-api/codex/models','/backend-api/codex/guardian',
        '/backend-api/codex/guardian-classifier','/backend-api/codex/alpha/search',
        '/backend-api/codex/images/generations','/backend-api/codex/images/edits',
        '/backend-api/codex/memories/trace_summarize','/backend-api/codex/realtime/calls',
        '/backend-api/codex/alpha/history/v2/read_file','/backend-api/codex/alpha/notes/v2/read_file',
        '/v1/responses','/v1/responses/compact','/v1/models','/v1/guardian','/v1/guardian-classifier',
        '/v1/alpha/search','/v1/images/generations','/v1/images/edits','/v1/memories/trace_summarize',
        '/v1/realtime/calls','/v1/analytics/codex/turn-costs','/backend-api/wham/usage/thread-estimates/query',
        '/backend-api/ps/plugins/{id}/shares','/backend-api/ps/plugins/{id}/uninstall',
        '/backend-api/ps/plugins/{id}/install','/backend-api/ps/plugins/{id}',
        '/backend-api/public/plugins/workspace/{id}','/backend-api/wham/tasks/{id}',
        '/backend-api/wham/tasks/{id}/turns/{id}/sibling_turns','/v1/realtime/calls/{id}','/v1/live/{id}',
    })
    return values

def main():
    ap=argparse.ArgumentParser();ap.add_argument('--profile',type=Path);ap.add_argument('--output',type=Path,default=ROOT/'.local/latest-operations/appserver-cli-egress-diff.json');args=ap.parse_args()
    profile=(args.profile or latest_profile())
    rows={}
    for route in ('app','exec'):
        for row in load_rows(profile/route/'requests.json' if profile else None):
            key=normalize(row.get('path',''))
            item=rows.setdefault(key,{'path':key,'app_requests':0,'exec_requests':0,'methods':set(),'observed_sha256':set(),'app_methods':set(),'exec_methods':set(),'app_headers':set(),'exec_headers':set(),'app_body_sha256':set(),'exec_body_sha256':set(),'app_body_shape':set(),'exec_body_shape':set(),'app_structure':set(),'exec_structure':set()})
            item[route+'_requests']+=1
            item['methods'].add(row.get('method',''))
            item[route+'_methods'].add(row.get('method',''))
            item[route+'_headers'].update(str(k).lower() for k in (row.get('headers') or {}))
            item['observed_sha256'].add(row.get('body_sha256',''))
            item[route+'_body_sha256'].add(row.get('body_sha256',''))
            item[route+'_body_shape'].add(body_shape_hash(row))
            item[route+'_structure'].add(json.dumps(body_structure(row.get('body')),ensure_ascii=False,sort_keys=True,separators=(',',':')))
    source=source_literals();handlers=handler_paths()
    result=[]
    for key,item in sorted(rows.items()):
        for field in ('methods','observed_sha256','app_methods','exec_methods','app_headers','exec_headers','app_body_sha256','exec_body_sha256','app_body_shape','exec_body_shape','app_structure','exec_structure'):
            item[field]=sorted(x for x in item[field] if x)
        item['method_diff']=item['app_methods'] != item['exec_methods']
        item['body_hash_sets_equal']=item['app_body_sha256'] == item['exec_body_sha256']
        item['normalized_body_shape_sets_equal']=item['app_body_shape'] == item['exec_body_shape']
        item['body_structure_diff']=item['app_structure'] != item['exec_structure']
        item['header_key_diff']=sorted(set(item['app_headers']) ^ set(item['exec_headers']))
        item['source_seen']=key in source or any(key.endswith(x) for x in source)
        item['handler_seen']=key in handlers or any(key.endswith(x) for x in handlers)
        item['status']='observed_and_handler' if item['handler_seen'] else 'observed_missing_handler'
        result.append(item)
    out={'schema':'appserver-cli-egress-diff-v1','profile':str(profile.relative_to(ROOT)) if profile and profile.is_relative_to(ROOT) else None,'source_literal_count':len(source),'handler_literal_count':len(handlers),'observed_paths':result,'observed_missing_handler':[x['path'] for x in result if x['status']=='observed_missing_handler'],'wire_method_or_header_differences':[x['path'] for x in result if x['method_diff'] or x['header_key_diff']],'body_hash_differences':[x['path'] for x in result if not x['body_hash_sets_equal']],'normalized_body_shape_differences':[x['path'] for x in result if not x['normalized_body_shape_sets_equal']],'body_structure_differences':[x['path'] for x in result if x['body_structure_diff']],'implemented_observed_differences':{'/backend-api/ps/mcp':'appServerMirrorMCPMetadata removes measured CLI-only thread_source while preserving unknown fields and numbers'},'remaining_observed_differences':['/backend-api/connectors/directory/list: App-only executor discovery in this profile; no server-side fake request'],'unverified_source_only':sorted(source-handlers),'production_deployed':False}
    args.output.parent.mkdir(parents=True,exist_ok=True);args.output.write_text(json.dumps(out,ensure_ascii=False,indent=2),encoding='utf-8');print(json.dumps({'output':str(args.output),'observed':len(result),'missing_handler':len(out['observed_missing_handler'])},ensure_ascii=False))
if __name__=='__main__':main()
