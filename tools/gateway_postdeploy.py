"""Read-only production checks and before/after comparison; never restarts.

Only temporary authenticated management sessions are created/revoked. Long-lived
keys and database credentials remain in server memory and never enter reports.
"""
from __future__ import annotations

import argparse
import base64
import datetime as dt
import hashlib
import json
from pathlib import Path, PurePosixPath
import re
import shlex

ROOT = Path(__file__).resolve().parents[1]
SHA = re.compile(r"[0-9a-f]{64}")

REMOTE = r'''
import base64,hashlib,http.cookiejar,json,os,pathlib,subprocess,time,urllib.error,urllib.parse,urllib.request
from http.cookies import SimpleCookie
expected=json.loads(base64.b64decode(EXPECTED))
base='http://127.0.0.1:18879'
result={'schema':'codex-agent-postdeploy-check-v1','status':'fail','snapshot':expected['snapshot'],'checked_at_unix':int(time.time()),'checks':{},'failures':[],'mutation_scope':'temporary management session create/revoke only','service_restart_executed':False,'account_writes':0,'gateway_or_refresh_requests':0}
class CheckFailure(Exception): pass
def require(value,code):
    if not value: raise CheckFailure(code)
def command(args,timeout=20):
    p=subprocess.run(args,capture_output=True,timeout=timeout)
    require(p.returncode==0,'command_failed_'+args[0])
    return p.stdout.decode('utf-8')
def sha(path): return hashlib.sha256(pathlib.Path(path).read_bytes()).hexdigest()
def sql(statement):
    require(command(['docker','port','codex-agent-postgres','5432/tcp']).strip()=='127.0.0.1:25433','pg_container_boundary_mismatch')
    raw=command(['docker','exec','-e','PGOPTIONS=-c statement_timeout=10000 -c default_transaction_read_only=on','codex-agent-postgres','psql','-X','-q','-At','-v','ON_ERROR_STOP=1','-U','postgres','-d','codex_agent','-c',statement],30)
    return json.loads(raw)
plain=urllib.request.build_opener(urllib.request.ProxyHandler({}))
cookies=urllib.request.build_opener(urllib.request.ProxyHandler({}),urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))
def http(path,body=None,method=None,token=None,session=False,want=200):
    allowed=path.startswith(('/api/session','/api/accounts','/api/logs','/api/overview','/health/')) or path=='/' or path.lstrip('/') in expected['frontend']
    require(allowed and 'refresh' not in path,'http_path_outside_check_scope')
    verb=method or ('POST' if body is not None else 'GET')
    require(verb=='GET' or path=='/api/session' and verb in ('POST','DELETE'),'write_operation_refused')
    headers={'Content-Type':'application/json','Origin':base}
    if token: headers['Authorization']='Bearer '+token
    request=urllib.request.Request(base+path,data=None if body is None else json.dumps(body).encode(),method=verb,headers=headers)
    try: response=(cookies if session else plain).open(request,timeout=20)
    except urllib.error.HTTPError as e: response=e
    require(response.status==want,'unexpected_http_status')
    raw=response.read(32*1024*1024+1)
    require(len(raw)<=32*1024*1024,'response_limit_exceeded')
    return raw,response.headers
def api(path,**kwargs):
    raw,headers=http(path,**kwargs)
    return json.loads(raw),headers
def step(name,operation):
    try:
        operation();result['checks'][name]=True
    except Exception as e:
        result['checks'][name]=False
        result['failures'].append({'check':name,'code':str(e) if isinstance(e,CheckFailure) else 'check_internal_failure'})
def inventory():
    values=dict(line.split('=',1) for line in command(['systemctl','show','codex-agent.service','-p','MainPID','-p','ActiveState','-p','SubState','-p','NRestarts','-p','TimeoutStopUSec']).splitlines() if '=' in line)
    require(values.get('ActiveState')=='active' and values.get('SubState')=='running' and int(values.get('MainPID','0'))>0,'service_not_running')
    pid=values['MainPID'];args=pathlib.Path('/proc/'+pid+'/cmdline').read_bytes().decode().split('\0');args=[x for x in args if x]
    require('--web' in args and '--listen' in args,'runtime_arguments_missing')
    binary=pathlib.Path(args[0]);web=pathlib.Path(args[args.index('--web')+1])
    require(str(binary).startswith('/opt/codex-agent/') and str(web).startswith('/opt/codex-agent/'),'runtime_path_boundary_mismatch')
    require(sha('/proc/'+pid+'/exe')==expected['binary_sha256'] and sha(binary)==expected['binary_sha256'],'runtime_binary_hash_mismatch')
    result['service']={**values,'binary_path':str(binary),'frontend_path':str(web),'runtime_binary_sha256':expected['binary_sha256']}
    result['unit_sha256']=sha('/etc/systemd/system/codex-agent.service')
def health():
    for path in ['/health/live','/health/ready']: http(path)
def database():
    result['database']=sql("SELECT json_build_object('migration_checksums',(SELECT json_object_agg(name,checksum) FROM schema_migrations),'accounts',(SELECT count(*) FROM accounts),'enabled_accounts',(SELECT count(*) FROM accounts WHERE enabled),'distinct_account_ids',(SELECT count(DISTINCT id) FROM accounts),'duplicate_display_name_groups',(SELECT count(*) FROM (SELECT display_name FROM accounts GROUP BY display_name HAVING count(*)>1) t),'credential_rows',(SELECT count(*) FROM account_credentials),'distinct_token_fingerprints',(SELECT count(DISTINCT token_fingerprint) FROM account_credentials),'account_groups',(SELECT count(*) FROM account_groups),'account_proxies',(SELECT count(*) FROM account_proxies),'logs',(SELECT count(*) FROM request_logs),'usage_events',(SELECT count(*) FROM usage_events),'credential_version_sum',(SELECT COALESCE(sum(credential_version),0) FROM account_credentials),'min_credential_expiry',(SELECT min(expires_at) FROM account_credentials),'max_credential_expiry',(SELECT max(expires_at) FROM account_credentials),'usage_totals',COALESCE((SELECT row_to_json(t) FROM (SELECT requests,ok,errors,tokens,input,output,cached FROM usage_totals WHERE scope='global' AND subject='') t),'{}'::json));")
    facts=result['database'];require(facts['migration_checksums']==expected['target_checksums'],'migration_checksums_mismatch')
    require(facts['accounts']==facts['distinct_account_ids']==facts['credential_rows'],'account_identity_or_credentials_count_mismatch')
    require(facts['distinct_token_fingerprints']==facts['credential_rows'],'duplicate_token_fingerprint_detected')
    state=sql("SELECT json_build_object('identity',COALESCE((SELECT json_agg(id ORDER BY id) FROM accounts),'[]'::json),'configuration',COALESCE((SELECT json_agg(t ORDER BY id) FROM (SELECT id,display_name,enabled,notes,tags,concurrency,priority,weight,quota,window_days,window_start FROM accounts) t),'[]'::json),'account_groups',COALESCE((SELECT json_agg(t ORDER BY account_id,group_id) FROM account_groups t),'[]'::json),'account_proxies',COALESCE((SELECT json_agg(t ORDER BY account_id,position,proxy_id) FROM account_proxies t),'[]'::json));")
    result['database']['state_hashes']={key:hashlib.sha256(json.dumps(value,sort_keys=True,separators=(',',':')).encode()).hexdigest() for key,value in state.items()}
def frontend():
    web=pathlib.Path(result['service']['frontend_path'])
    actual={p.relative_to(web).as_posix():sha(p) for p in web.rglob('*') if p.is_file()}
    require(actual==expected['frontend'],'frontend_disk_hash_mismatch')
    observed={}
    for name,digest in expected['frontend'].items():
        raw,_=http('/' if name=='index.html' else '/'+urllib.parse.quote(name,safe='/'))
        observed[name]=hashlib.sha256(raw).hexdigest()
        require(observed[name]==digest,'frontend_http_hash_mismatch')
    result['frontend']={'file_count':len(observed),'hashes':observed,'http_verified':True}
def current_key():
    path=pathlib.Path('/var/lib/codex-agent/client-key')
    require(path.is_file() and path.stat().st_mode&0o077==0,'management_key_file_not_private')
    key=path.read_text().strip()
    require(isinstance(key,str) and len(key)>=32,'invalid_current_admin_key')
    return key
def authentication_and_data():
    key=current_key();session_created=False
    try:
        http('/api/accounts',want=401)
        http('/api/accounts',token='synthetic-invalid-postdeploy-key',want=401)
        api('/api/session',token=key)
        login,headers=api('/api/session',body={'admin_key':key},session=True)
        session_created=True
        require(login.get('authenticated') is True,'cookie_login_failed')
        jar=SimpleCookie();jar.load(headers.get('Set-Cookie',''))
        require(len(jar)==1,'session_cookie_missing')
        cookie=next(iter(jar.values()))
        require(bool(cookie['httponly']) and cookie['samesite'].lower()=='strict' and cookie['path']=='/api','session_cookie_attributes_incorrect')
        result['session_cookie']={'httponly':True,'same_site':'Strict','path':'/api','secure':bool(cookie['secure'])}
        account_data,_=api('/api/accounts?limit=50',session=True)
        rows=account_data.get('accounts',[])
        require(len(rows)==min(50,result['database']['accounts']),'account_api_count_mismatch')
        require(len(set(row['id'] for row in rows))==len(rows),'account_api_duplicate_identity')
        forbidden={'access_token','refresh_token','credentials_cipher','password'}
        require(all(not forbidden.intersection(row) for row in rows),'account_api_sensitive_field_exposed')
        overview,_=api('/api/overview?from='+str(int(time.time())-86400)+'&to='+str(int(time.time())+60)+'&step=300',session=True)
        logs,_=api('/api/logs?page_size=50',session=True)
        cards=overview['cards'];observed=sql("SELECT json_build_object('accounts',(SELECT count(*) FROM accounts),'tokens',(SELECT tokens FROM usage_totals WHERE scope='global' AND subject=''));")
        low=result['database']['usage_totals'].get('tokens',0);high=observed['tokens'] or 0
        require(low<=cards['tokens_total']<=high,'overview_lifetime_tokens_outside_database_bracket')
        require(cards['accounts'] in (result['database']['accounts'],observed['accounts']),'overview_account_count_mismatch')
        items=logs.get('items',[]);times=[row.get('time',0) for row in items]
        require(times==sorted(times,reverse=True),'logs_not_latest_first')
        require(len(items)<=50 and (result['database']['logs']==0 or len(items)>0),'logs_api_count_mismatch')
        result['api']={'accounts_page_rows':len(rows),'logs_page_rows':len(items),'accounts':cards['accounts'],'tokens_total':cards['tokens_total'],'tokens_db_bracket':[low,high],'logs_descending':True,'timezone':overview.get('timezone'),'retention_days':overview.get('retention_days')}
        api('/api/session',method='DELETE',session=True);session_created=False
        http('/api/accounts',session=True,want=401)
    finally:
        if session_created:
            try: api('/api/session',method='DELETE',session=True)
            except Exception: result['failures'].append({'check':'session_cleanup','code':'session_revoke_failed'})
        key=None
step('service_binary',inventory)
step('health',health)
step('database_counts_schema',database)
if result['checks'].get('service_binary'): step('frontend_exact_http',frontend)
if result['checks'].get('database_counts_schema'): step('authentication_and_data',authentication_and_data)
if len(result['checks'])==5 and all(result['checks'].values()) and not result['failures']: result['status']='pass'
print(json.dumps(result))
'''


def manifest_expectations(path, snapshot, expected_binary=None, baseline_schema=False, baseline_report=None):
    document = json.loads(path.read_text(encoding="utf-8"))
    baseline = json.loads(baseline_report.read_text()) if baseline_report else None
    if baseline is not None:
        if snapshot != "before" or expected_binary or baseline_schema:
            raise ValueError("baseline_report_requires_before_without_other_overrides")
        expected_binary = baseline.get("baseline_health", {}).get("runtime_binary_sha256")
        if not expected_binary:
            raise ValueError("baseline_report_missing_runtime_binary")
    binary, frontend = expected_binary or document.get("binary_sha256"), document.get("frontend")
    if not isinstance(binary, str) or not SHA.fullmatch(binary) or not isinstance(frontend, dict) or "index.html" not in frontend:
        raise ValueError("explicit_release_manifest_missing_artifact_hashes")
    for name, digest in frontend.items():
        relative = PurePosixPath(name)
        if relative.is_absolute() or ".." in relative.parts or "\\" in name or str(relative) != name or not SHA.fullmatch(digest):
            raise ValueError("invalid_frontend_manifest_entry")
    target = document.get("target_checksums")
    if target is None:
        target = document.get("target_migrations")
    if target is None and document.get("schema") == "codex-agent-pg-upgrade-v2":
        from importlib.util import spec_from_file_location, module_from_spec
        spec = spec_from_file_location("postdeploy_upgrade_contract", ROOT / "deploy/upgrade_remote.py")
        module = module_from_spec(spec)
        spec.loader.exec_module(module)
        target = module.TARGET_MIGRATIONS
    if not isinstance(target, dict) or len(target) not in (3, 4) or any(not SHA.fullmatch(value) for value in target.values()):
        raise ValueError("manifest_target_checksums_required")
    if baseline is not None:
        target = baseline.get("migration_checksums")
        if not isinstance(target, dict) or len(target) != 3 or any(not SHA.fullmatch(value) for value in target.values()):
            raise ValueError("baseline_report_missing_migration_checksums")
    if baseline_schema:
        if snapshot != "before" or expected_binary is None:
            raise ValueError("baseline_schema_requires_before_and_explicit_old_binary")
        target = {key: value for key, value in target.items() if key in ("migrations/000_core.sql", "migrations/001_accounts.sql", "migrations/002_metrics.sql")}
        if len(target) != 3:
            raise ValueError("baseline_schema_migrations_missing")
    return {"binary_sha256": binary, "frontend": frontend, "target_checksums": target, "snapshot": snapshot}


def compare(before, after, strict=False, allow_additive_003=False):
    checks = {"both_checks_passed": before.get("status") == after.get("status") == "pass"}
    first, last = before.get("database", {}), after.get("database", {})
    checks["schema_preserved"] = first.get("migration_checksums") == last.get("migration_checksums")
    if allow_additive_003:
        baseline, target = first.get("migration_checksums", {}), last.get("migration_checksums", {})
        checks["schema_preserved"] = len(baseline) == 3 and len(target) == 4 and all(target.get(key) == value for key, value in baseline.items()) and target.get("migrations/003_management_cleanup.sql") == "31ca0e1e58bb846c9691bd505718a1c13ef3aba03f9abf157034317e85cde56e"
    checks["account_identity_configuration_relations_preserved"] = bool(first.get("state_hashes")) and first.get("state_hashes") == last.get("state_hashes")
    checks["frontend_preserved"] = before.get("frontend", {}).get("hashes") == after.get("frontend", {}).get("hashes")
    for key in ("accounts", "credential_rows", "account_groups", "account_proxies"):
        checks[key + "_preserved"] = first.get(key) == last.get(key)
    for key in ("requests", "tokens", "input", "output", "cached"):
        old = first.get("usage_totals", {}).get(key)
        new = last.get("usage_totals", {}).get(key)
        checks[key + ("_equal" if strict else "_nondecreasing")] = isinstance(old, int) and isinstance(new, int) and (new == old if strict else new >= old)
    checks["usage_events_preserved"] = isinstance(first.get("usage_events"), int) and isinstance(last.get("usage_events"), int) and (first["usage_events"] == last["usage_events"] if strict else last["usage_events"] >= first["usage_events"])
    credential_changes = {key: {"before": first.get(key), "after": last.get(key)} for key in ("credential_version_sum", "min_credential_expiry", "max_credential_expiry") if first.get(key) != last.get(key)}
    if strict:
        checks["quiet_window_credentials_unchanged"] = not credential_changes
    return {"schema": "codex-agent-postdeploy-comparison-v1", "status": "pass" if all(checks.values()) else "fail", "checks": checks, "credential_observation_changes": credential_changes, "mode": "strict_quiet_window" if strict else "nondecreasing_persistent_usage", "restart_proven": before.get("service", {}).get("MainPID") != after.get("service", {}).get("MainPID"), "restart_executed_by_this_tool": False, "limitations": ["Distinct PIDs plus before/after observations do not execute or authorize restart", "Log detail count can legitimately decrease under retention; durable usage is compared instead", "Concurrent administrator metadata edits require separate review if identity/count comparison fails"]}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="action", required=True)
    check = sub.add_parser("check")
    check.add_argument("--manifest", type=Path, required=True)
    check.add_argument("--snapshot", choices=["before", "after", "check"], default="check")
    check.add_argument("--expected-binary", help="Explicit old binary SHA for a before snapshot")
    check.add_argument("--baseline-schema", action="store_true", help="Before-only 000-002 schema, retaining the new release frontend manifest")
    check.add_argument("--baseline-report", type=Path, help="Actual preflight JSON with baseline_health and migration_checksums; before snapshot only")
    check.add_argument("--output", type=Path)
    comparison = sub.add_parser("compare")
    comparison.add_argument("--before", type=Path, required=True)
    comparison.add_argument("--after", type=Path, required=True)
    comparison.add_argument("--strict", action="store_true")
    comparison.add_argument("--allow-additive-003", action="store_true", help="Compare old 000-002 to exact reviewed 003 without treating it as unexplained drift")
    comparison.add_argument("--output", type=Path)
    args = parser.parse_args()
    if args.action == "compare":
        result = compare(json.loads(args.before.read_text()), json.loads(args.after.read_text()), args.strict, args.allow_additive_003)
    else:
        from integration_env import connect
        expected = manifest_expectations(args.manifest, args.snapshot, args.expected_binary, args.baseline_schema, args.baseline_report)
        script = "EXPECTED=" + repr(base64.b64encode(json.dumps(expected).encode()).decode()) + "\n" + REMOTE
        client = connect()
        try:
            _, stdout, stderr = client.exec_command("python3 -c " + shlex.quote(script), timeout=180)
            raw = stdout.read()
            stderr.read()
            if stdout.channel.recv_exit_status() != 0:
                raise RuntimeError("postdeploy_remote_check_failed")
            result = json.loads(raw)
            result["manifest_sha256"] = hashlib.sha256(args.manifest.read_bytes()).hexdigest()
            result["runner_sha256"] = hashlib.sha256(Path(__file__).read_bytes()).hexdigest()
        finally:
            client.close()
    target = args.output or ROOT / ".local/postdeploy" / (dt.datetime.now(dt.timezone.utc).strftime("%Y%m%dT%H%M%S.%fZ") + "-" + args.action + ".json")
    target.parent.mkdir(parents=True, exist_ok=True)
    target.write_text(json.dumps(result, indent=2) + "\n", encoding="utf-8")
    print(json.dumps({"status": result["status"], "report": str(target), "checks": result["checks"], "failures": result.get("failures", [])}, indent=2))
    return 0 if result["status"] == "pass" else 1


if __name__ == "__main__":
    raise SystemExit(main())
