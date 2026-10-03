"""Observe normal bridge timer cycles without requesting credential rotation."""
import argparse
import hashlib
import json
from pathlib import Path
import shlex

from integration_env import connect

ROOT = Path(__file__).resolve().parents[1]
REMOTE = r'''
import collections,datetime,hashlib,importlib.util,json,pathlib,re,subprocess,time,urllib.parse
root=pathlib.Path('/var/lib/codex-agent-workbench-bridge')
def digest(value): return hashlib.sha256(json.dumps(value,sort_keys=True,separators=(',',':')).encode()).hexdigest()
def command(args):
    p=subprocess.run(args,capture_output=True,text=True,timeout=20)
    if p.returncode: raise RuntimeError('read_only_command_failed')
    return p.stdout
def properties(unit,fields):
    raw=command(['systemctl','show',unit,*['--property='+x for x in fields]])
    return dict(line.split('=',1) for line in raw.splitlines() if '=' in line)
def snapshot():
    state=json.loads((root/'state.json').read_text())
    latest=json.loads((root/'latest.json').read_text())
    accounts=[]
    for source_id,mapping in state.get('accounts',{}).items():
        account_id=mapping['account_id']
        if not re.fullmatch(r'a_[a-f0-9]{12,64}',account_id): raise RuntimeError('unexpected_stable_account_id')
        raw=command(['docker','exec','-e','PGOPTIONS=-c statement_timeout=5000 -c default_transaction_read_only=on','codex-agent-postgres','psql','-X','-q','-At','-v','ON_ERROR_STOP=1','-U','postgres','-d','codex_agent','-c',"SELECT row_to_json(t) FROM (SELECT a.id,a.display_name,a.enabled,a.notes,a.tags,a.concurrency,a.priority,a.weight,a.quota,a.window_days,a.window_start,a.has_refresh_token,a.version,a.expires_at,c.credential_version,c.expires_at AS credential_expiry,ARRAY(SELECT group_id FROM account_groups WHERE account_id=a.id ORDER BY group_id) AS groups,ARRAY(SELECT proxy_id FROM account_proxies WHERE account_id=a.id ORDER BY position,proxy_id) AS proxies FROM accounts a JOIN account_credentials c ON c.account_id=a.id WHERE a.id='"+account_id+"') t;"])
        row=json.loads(raw)
        configuration={k:v for k,v in row.items() if k not in ('version','expires_at','credential_version','credential_expiry')}
        accounts.append({'source_ref_sha256':hashlib.sha256(source_id.encode()).hexdigest(),'account_id_sha256':hashlib.sha256(account_id.encode()).hexdigest(),'configuration_sha256':digest(configuration),'version':row['version'],'credential_version':row['credential_version'],'expires_at':row['expires_at'],'credential_expiry':row['credential_expiry'],'has_refresh_token':row['has_refresh_token'],'checkpoint_token_sha256':mapping.get('token_sha256'),'checkpoint_identity_sha256':mapping.get('identity_sha256'),'initialized':mapping.get('initialized',False)})
    source_status=[]
    source_error=None
    try:
        spec=importlib.util.spec_from_file_location('bridge_observed','/opt/codex-agent/workbench-bridge/workbench_bridge.py')
        module=importlib.util.module_from_spec(spec);spec.loader.exec_module(module)
        api,image=module.source_api()
        for source_id in state.get('accounts',{}):
            # Read metadata only. Never call export-sub, OAuth, recharge or run().
            job=module.checked_job(api.call('/api/admin/registrations/'+urllib.parse.quote(source_id,safe='')),detail=True,expected_id=source_id)
            source_status.append({'source_ref_sha256':hashlib.sha256(source_id.encode()).hexdigest(),'status':job['status'],'recharge_status':job['recharge_status'],'plan':job['recharge_plan_type'],'has_refresh_token':job['has_refresh_token'],'is_refunded':job['is_refunded'],'has_active_refund':job['has_active_refund'],'eligible':module.eligible(job)})
    except Exception: source_error='source_metadata_read_unavailable'
    return {'observed_at':datetime.datetime.now(datetime.timezone.utc).isoformat(),'timer':properties('codex-agent-workbench-bridge.timer',['ActiveState','SubState','LastTriggerUSec','NextElapseUSecRealtime','NextElapseUSecMonotonic']),'service':properties('codex-agent-workbench-bridge.service',['ActiveState','Result','ExecMainStatus','ExecMainStartTimestamp','ExecMainExitTimestamp']),'latest':{k:latest.get(k) for k in ['schema','checked_at','eligible','counts','refresh_tokens_forwarded']},'accounts':sorted(accounts,key=lambda x:x['account_id_sha256']),'source_metadata':source_status,'source_metadata_error':source_error}
result={'schema':'codex-agent-bridge-live-observation-v1','status':'observing','credential_rotation_requested':False,'source_export_requested':False,'bridge_run_forced':False,'inference_requests':0,'source_changes_requested':False,'snapshots':[]}
for index in range(3):
    if index: time.sleep(70)
    result['snapshots'].append(snapshot())
raw=command(['journalctl','-u','codex-agent-workbench-bridge.service','--since','2026-09-18 04:00:00 UTC','--no-pager','-o','json','-n','1000'])
cycles=[];errors=collections.Counter()
for line in raw.splitlines():
    try:
        message=json.loads(line)['MESSAGE'];entry=json.loads(message)
    except Exception: continue
    if entry.get('schema')=='codex-agent-workbench-bridge-v1':
        cycles.append({k:entry.get(k) for k in ['checked_at','eligible','counts','refresh_tokens_forwarded']})
    elif entry.get('ok') is False: errors[entry.get('error_code','unknown')]+=1
result['journal']={'since_utc':'2026-09-18T04:00:00Z','cycles':cycles,'error_counts':dict(errors),'reauthorized_total':sum(x.get('counts',{}).get('reauthorized',0) for x in cycles)}
first,last=result['snapshots'][0],result['snapshots'][-1]
previous={x['account_id_sha256']:x for x in first['accounts']};changes=[]
for current in last['accounts']:
    prior=previous.get(current['account_id_sha256'])
    if prior and prior['checkpoint_token_sha256']!=current['checkpoint_token_sha256']:
        changes.append({'account_id_sha256':current['account_id_sha256'],'version_advanced':current['version']>prior['version'],'credential_version_advanced':current['credential_version']>prior['credential_version'],'configuration_unchanged':current['configuration_sha256']==prior['configuration_sha256'],'refresh_token_absent':not current['has_refresh_token'],'identity_unchanged':current['checkpoint_identity_sha256']==prior['checkpoint_identity_sha256']})
result['observed_changes']=changes
checked_at={x['latest']['checked_at'] for x in result['snapshots']}
result['normal_timer_cycles_observed']=len(checked_at)
result['timer_healthy']=all(x['timer'].get('ActiveState')=='active' and x['service'].get('ExecMainStatus')=='0' for x in result['snapshots'])
result['status']='natural_rotation_observed' if changes and all(all(v for k,v in change.items() if k!='account_id_sha256') for change in changes) else 'rotation_not_observed'
if not result['timer_healthy']: result['status']='timer_unhealthy'
result['limits']=['Only naturally scheduled bridge execution was observed; no AT change was forced.','Historical journal reauthorized counts alone do not prove metadata preservation during this observation window.','Source metadata queries may create a temporary source login session, but never request credentials or alter registration state.']
print(json.dumps(result))
'''


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    client = connect()
    try:
        _, stdout, stderr = client.exec_command("python3 -c " + shlex.quote(REMOTE), timeout=240)
        raw = stdout.read()
        stderr.read()
        if stdout.channel.recv_exit_status():
            raise RuntimeError("bridge_read_only_observation_failed")
        result = json.loads(raw)
        result["runner_sha256"] = hashlib.sha256(Path(__file__).read_bytes()).hexdigest()
        args.output.parent.mkdir(parents=True, exist_ok=True)
        args.output.write_text(json.dumps(result, indent=2) + "\n")
        print(json.dumps({"status": result["status"], "report": str(args.output), "normal_timer_cycles_observed": result["normal_timer_cycles_observed"], "timer_healthy": result["timer_healthy"], "journal_reauthorized_total": result["journal"]["reauthorized_total"], "observed_changed_accounts": len(result["observed_changes"])}))
    finally:
        client.close()


if __name__ == "__main__":
    main()
