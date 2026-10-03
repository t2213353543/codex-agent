"""Isolated server-side egress capture driver. Invoked by its local runner.

All account material and raw captures stay in the private server directory.
No production settings, accounts, listeners, proxies or service are changed.
"""
import base64
import collections
import hashlib
import json
import os
import pathlib
import re
import socket
import subprocess
import time
import urllib.request


def emit(value):
    print(json.dumps(value, ensure_ascii=False, sort_keys=True), flush=True)


def readonly_sql(statement):
    return subprocess.check_output([
        'docker','exec','-e','PGOPTIONS=-c statement_timeout=10000 -c default_transaction_read_only=on',
        'codex-agent-postgres','psql','-X','-q','-At','-v','ON_ERROR_STOP=1','-U','postgres','-d','codex_agent','-c',statement
    ], text=True).strip()


def private_write(path, value):
    raw = value if isinstance(value, bytes) else json.dumps(value, sort_keys=True, indent=2).encode()
    fd = os.open(path, os.O_WRONLY|os.O_CREAT|os.O_EXCL, 0o600)
    with os.fdopen(fd, 'wb') as handle: handle.write(raw)


def main():
    root = pathlib.Path(RUN_DIR)
    assert root.parent == pathlib.Path('/opt/codex-agent-integration/appserver-parity/runs')
    root.mkdir(mode=0o700, parents=False, exist_ok=False)
    bootstrap = {'official_initialize_count': 0}
    identity_file = root/'appserver-runtime-identity.json'
    if LIGHTWEIGHT and 'official_codex_cli' in ROUTES:
        # A separate provisioning capture runs before account access and before
        # generation tracing. The helper requires an isolated network namespace.
        probe_env={k:v for k,v in os.environ.items() if k in ['PATH','LANG','LC_ALL','TZ']}
        probe=subprocess.run(['python3','-B',IDENTITY_CAPTURE_HELPER,'--binary',CODEX_BINARY,'--output',str(identity_file)],
                             env=probe_env,stdout=subprocess.PIPE,stderr=subprocess.PIPE,timeout=45)
        if probe.returncode!=0:
            emit({'kind':'identity_bootstrap_failed','exit_code':probe.returncode,'stderr_bytes':len(probe.stderr),'network_fallback_used':False})
            raise RuntimeError('offline_identity_capture_failed')
        bootstrap=json.loads(probe.stdout)
        assert bootstrap.get('network_namespace')=='isolated' and bootstrap.get('official_initialize_count')==1
        emit({'kind':'identity_bootstrap',**bootstrap})
    pid = subprocess.check_output(['systemctl','show','codex-agent','--property=MainPID','--value'],text=True).strip()
    runtime_env = dict(item.split(b'=',1) for item in pathlib.Path('/proc/'+pid+'/environ').read_bytes().split(b'\0') if b'=' in item)
    helper_override=globals().get('NATIVE_HELPER_OVERRIDE')
    helper_expected=globals().get('NATIVE_HELPER_SHA256_OVERRIDE')
    if helper_override:
        helper_path=pathlib.Path(helper_override).resolve(strict=True)
        assert str(helper_path).startswith('/opt/codex-agent-integration/') and helper_path.is_file()
        assert hashlib.sha256(helper_path.read_bytes()).hexdigest()==helper_expected,'candidate_helper_hash_changed'
    encrypted = json.loads(readonly_sql("SELECT value FROM settings WHERE key='admin_key'"))['ciphertext']
    params = {'Key':runtime_env[b'CREDENTIAL_ENCRYPTION_KEY'].decode(),'KeyID':runtime_env.get(b'CREDENTIAL_ENCRYPTION_KEY_ID',b'primary').decode(),'Ciphertext':encrypted}
    key = json.loads(subprocess.check_output(['/opt/codex-agent-integration/checks/decrypt-helper-5ca8b78cde0c9bc4'], input=json.dumps(params).encode()))
    http = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    def api(path, body=None):
        req=urllib.request.Request('http://127.0.0.1:18879'+path,headers={'Authorization':'Bearer '+key,'Content-Type':'application/json'},data=json.dumps(body).encode() if body is not None else None)
        with http.open(req,timeout=25) as response: return json.load(response)
    before = api('/api/settings')
    rows = [row for row in api('/api/accounts?limit=100')['accounts'] if row.get('enabled')]
    assert rows, 'no_enabled_account'
    row = sorted(rows,key=lambda value:value['id'])[0]
    credential = api('/api/accounts/export/credentials',{'items':[{'id':row['id'],'version':row['version']}]})['accounts'][0]['credentials']
    if isinstance(credential,str):credential=json.loads(credential)
    token = credential['access_token']
    claims=json.loads(base64.urlsafe_b64decode(token.split('.')[1]+'='*(-len(token.split('.')[1])%4)))
    auth=claims.get('https://api.openai.com/auth',{})
    account_id=credential.get('account_id') or row.get('account_id') or auth.get('chatgpt_account_id')
    assert account_id
    fixture=root/'accounts.json'
    private_write(fixture,{'accounts':[{'alias':'reference','access_token':token,'account_id':account_id}]})
    identity={'account_record_sha256':hashlib.sha256(row['id'].encode()).hexdigest(),'credential_sha256':hashlib.sha256(token.encode()).hexdigest(),'credential_exp':claims.get('exp')}
    binary = pathlib.Path(CODEX_BINARY)
    assert binary.is_file()
    binary_sha = hashlib.sha256(binary.read_bytes()).hexdigest()
    version=subprocess.check_output([str(binary),'--version'],text=True).strip()
    image = subprocess.check_output(['docker','image','inspect',IMAGE,'--format','{{.Id}}'],text=True).strip()
    emit({'kind':'start','run':root.name,'routes':ROUTES,'pilot':PILOT,'lightweight':LIGHTWEIGHT,'official_version':version,'official_sha256':binary_sha,'capture_image':image,**identity})
    outcomes=[]
    try:
        for index,mode in enumerate(ROUTES):
            route=root/mode;route.mkdir(mode=0o700)
            capture=route/'capture';capture.mkdir(mode=0o700)
            driver=route/'driver';driver.mkdir(mode=0o700)
            work=route/'work';work.mkdir(mode=0o700)
            ca=capture/'ca';ca.mkdir(mode=0o700)
            configuration=capture/'config.json'
            private_write(configuration,{'directory':str(capture),'allowed_hosts':['chatgpt.com','*.chatgpt.com','openai.com','*.openai.com','api.github.com','codeload.github.com'],'synthetic':False,'phase_file':str(driver/'phase.json')})
            with socket.socket() as listener:
                listener.bind(('127.0.0.1',0));port=listener.getsockname()[1]
            container='codex-parity-'+root.name.lower().replace('.','-')+'-'+str(index)
            command=['docker','run','--rm','--name',container,'--network','host','--user','0:0',
                     '-v',str(root)+':'+str(root),'-v',ADDON_PATH+':'+ADDON_PATH+':ro',
                     '-e','CODEX_PARITY_CAPTURE_CONFIG='+str(configuration),
                     '--entrypoint','mitmdump',IMAGE,'-q','--listen-host','127.0.0.1','--listen-port',str(port),
                     '--set','confdir='+str(ca),'--set','connection_strategy=lazy','--set','ssl_insecure=false',
                     '--set','termlog_verbosity=error','--set','flow_detail=0','-s',ADDON_PATH]
            proc = subprocess.Popen(command,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
            started=time.monotonic()
            try:
                while not (capture/'ready.json').exists():
                    if proc.poll() is not None:raise RuntimeError('capture_proxy_exited')
                    if time.monotonic()-started>45:raise RuntimeError('capture_proxy_not_ready')
                    time.sleep(.1)
                env={k:v for k,v in os.environ.items() if k in ['PATH','LANG','LC_ALL','TZ']}
                env.update(BPS_LIVE_ACCOUNTS_FILE=str(fixture),BPS_LIVE_CODEX_BINARY=str(binary),BPS_LIVE_WORK_ROOT=str(work),
                           APP_SERVER_PARITY_PROXY='http://127.0.0.1:'+str(port),APP_SERVER_PARITY_CA=str(ca/'mitmproxy-ca-cert.pem'),
                           APP_SERVER_PARITY_OUTPUT=str(driver),APP_SERVER_PARITY_ROUTE=mode,APP_SERVER_PARITY_PILOT='1' if PILOT else '0',
                           APP_SERVER_PARITY_MODEL=MODEL,APP_SERVER_PARITY_EFFORT=EFFORT)
                if LIGHTWEIGHT and mode == 'official_codex_cli':
                    env['APP_SERVER_PARITY_LIGHTWEIGHT']='1'
                    env['CODEX_APP_SERVER_IDENTITY_FILE']=str(identity_file)
                for name in ['CODEX_NATIVE_HTTP_HELPER','CODEX_NATIVE_HTTP_SHA256','CODEX_NATIVE_WEBSOCKET','CODEX_NATIVE_HTTP_LIFECYCLE']:
                    if name.encode() in runtime_env:env[name]=runtime_env[name.encode()].decode()
                if helper_override:
                    env.update(CODEX_NATIVE_HTTP_HELPER=str(helper_path),CODEX_NATIVE_HTTP_SHA256=helper_expected,CODEX_NATIVE_WEBSOCKET='1')
                with (route/'test.log').open('w',encoding='utf-8') as output:
                    tracer='/opt/codex-agent-integration/appserver-parity/strace-tool/root/usr/bin/strace'
                    with (route/'private-net.log').open('w') as network:
                        os.chmod(network.name,0o600)
                    driver_command=(['python3','-B',NATIVE_DRIVER] if mode in ['native_app_server','native_codex_exec'] else
                                    [TEST_BINARY,'-test.run=^TestAppServerParityCapture$','-test.v','-test.timeout=15m'])
                    result=subprocess.run([tracer,'-f','-qq','-ttt','-e','trace=connect,execve','-o',str(route/'private-net.log'),'--']+driver_command,cwd=work,env=env,stdout=output,stderr=subprocess.DEVNULL,timeout=950)
                outcome={'route':mode,'exit_code':result.returncode,'lightweight':LIGHTWEIGHT and mode == 'official_codex_cli','elapsed_ms':round((time.monotonic()-started)*1000)}
            finally:
                subprocess.run(['docker','stop','-t','10',container],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,timeout=20)
                try:proc.wait(timeout=15)
                except subprocess.TimeoutExpired:proc.kill();proc.wait(timeout=5)
            records=[json.loads(line) for line in (capture/'manifest.jsonl').read_text().splitlines()]
            outcome['capture_events']=dict(collections.Counter(event['event'] for event in records))
            outcome['upstream_http_status']=dict(collections.Counter(str(event['status']) for event in records if event['event']=='response_headers'))
            outcome['blocked_hosts']=sorted({event['host'] for event in records if event['event'] in ['connect','upstream_connect'] and not event['allowed']})
            outcome['cases']=json.loads((driver/'driver-summary.json').read_text()) if (driver/'driver-summary.json').exists() else []
            exec_paths=[]
            for line in (route/'private-net.log').read_text().splitlines():
                matched=re.search(r'execve\("([^\"]+)"',line)
                if matched and line.endswith('= 0'):exec_paths.append(matched.group(1))
            outcome['generation_official_exec_count']=sum(path==str(binary) for path in exec_paths)
            outcome['generation_native_helper_exec_count']=sum(pathlib.Path(path).name=='codex-native-transport' for path in exec_paths)
            outcome['passed']=outcome['exit_code']==0 and not outcome['blocked_hosts']
            outcomes.append(outcome);emit({'kind':'route_result',**outcome})
            # Keep the first failing capture intact. Repeated uninspected model
            # calls would obscure the exact protocol error that needs diagnosis.
            if outcome['exit_code']!=0:break
        after=api('/api/settings')
        assert all(before.get(k)==after.get(k) for k in ['settings_version','inference_mode']),'production_settings_changed_during_capture'
        fixture.unlink(missing_ok=True)
        summary={'run':root.name,'passed':len(outcomes)==len(ROUTES) and all(value['passed'] for value in outcomes),'outcomes':outcomes,
                 'lightweight':LIGHTWEIGHT,
                 'bootstrap_official_init_count':bootstrap.get('official_initialize_count',0),
                 'bootstrap_network_namespace':bootstrap.get('network_namespace'),
                 'bootstrap_user_agent_sha256':bootstrap.get('user_agent_sha256'),
                 'generation_official_exec_count':sum(value['generation_official_exec_count'] for value in outcomes),
                 'private_capture_directory':str(root),'production_settings_unchanged':True,'fixture_removed':not fixture.exists(),
                 'raw_private_captures_include_credentials':True,
                 'private_syscall_trace':'connect,execve',
                 'tls_fingerprint_comparison':False,'all_process_egress_proven':False,'official_binary_sha256':binary_sha,'capture_image':image,**identity}
        private_write(root/'summary.json',summary);emit({'kind':'summary',**summary})
    finally:
        fixture.unlink(missing_ok=True)
    return 0 if summary['passed'] else 1


if __name__=='__main__':
    raise SystemExit(main())
