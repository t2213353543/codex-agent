"""Server-side implementation for reviewed stable-ingress/bluegreen operations.

This file is a private candidate. It is executable, but this review does not run
mutating subcommands on production. No secrets are printed or passed via argv.
"""
from pathlib import Path
from datetime import datetime,timezone
import argparse,hashlib,http.client,json,os,re,secrets,shlex,socket,subprocess,sys,tempfile,time,urllib.request
try:
    import fcntl,pwd
except ImportError:
    fcntl=pwd=None

BASE=Path('/var/lib/codex-agent-release')
REGISTRY=BASE/'active-runtime.json'
CONTROL='/run/codex-agent-ingress/control.sock'
INGRESS_STATE=Path('/var/lib/codex-agent-ingress/target.json')
INGRESS_UNIT='codex-agent-ingress.service'
PUBLIC=18879
FRONT=18878
PREFERRED_PORTS=(18890,18891)
SHA=re.compile(r'^[a-f0-9]{64}$')
UNIT=re.compile(r'^(codex-agent|codex-agent-runtime-[a-f0-9]{12,64})\.service$')
UNBOUNDED_RESOURCES='TasksMax=infinity\nMemoryHigh=infinity\nMemoryMax=infinity\nMemorySwapMax=infinity'

class Blocked(RuntimeError):pass
def require(test,message):
    if not test:raise Blocked(message)
def now():return datetime.now(timezone.utc).isoformat()
def digest(path):return hashlib.sha256(Path(path).read_bytes()).hexdigest()
def command(args,input=None,timeout=60):
    p=subprocess.run(args,input=input,capture_output=True,text=True,timeout=timeout)
    if p.returncode:raise Blocked('operation failed: '+args[0]+' '+(args[1] if len(args)>1 else ''))
    return p.stdout
def atomic(path,value,mode=0o600):
    path=Path(path);path.parent.mkdir(parents=True,exist_ok=True)
    fd,name=tempfile.mkstemp(prefix='.'+path.name+'-',dir=path.parent)
    try:
        os.fchmod(fd,mode)
        with os.fdopen(fd,'w') as f:f.write(value);f.flush();os.fsync(f.fileno())
        os.replace(name,path)
        directory=os.open(path.parent,os.O_RDONLY)
        try:os.fsync(directory)
        finally:os.close(directory)
    finally:
        if os.path.exists(name):os.unlink(name)
def write_registry(v):atomic(REGISTRY,json.dumps(v,indent=2)+'\n')
def registry():return json.loads(REGISTRY.read_text())
def env(pid):return dict(item.split(b'=',1) for item in Path('/proc/'+str(pid)+'/environ').read_bytes().split(b'\0') if b'=' in item)
def scope(values):
    keys=(b'DATABASE_URL',b'REDIS_URL',b'REDIS_KEY_PREFIX',b'CREDENTIAL_ENCRYPTION_KEY',b'CREDENTIAL_ENCRYPTION_KEY_ID')
    require(all(values.get(k) for k in keys),'shared storage identity incomplete')
    return hashlib.sha256(b'\0'.join(k+b'='+values[k] for k in keys)).hexdigest()
def runtime(unit):
    require(bool(UNIT.fullmatch(unit)),'unit outside codex-agent scope')
    fields=dict(line.split('=',1) for line in command(['systemctl','show',unit,'-p','MainPID','-p','ActiveState','-p','SubState']).splitlines() if '=' in line)
    pid=int(fields['MainPID']);require(pid>0 and fields['ActiveState']=='active','runtime not active')
    args=[v.decode() for v in Path('/proc/'+str(pid)+'/cmdline').read_bytes().split(b'\0') if v]
    require('--listen' in args,'runtime listen identity missing')
    listen=args[args.index('--listen')+1];port=int(listen.rsplit(':',1)[1])
    return {'unit':unit,'pid':pid,'binary_sha256':digest('/proc/'+str(pid)+'/exe'),'listen_port':port,'listen':listen,'executable':args[0],'process_uid':Path('/proc/'+str(pid)).stat().st_uid,'storage_identity_sha256':scope(env(pid))}
class UnixHTTP(http.client.HTTPConnection):
    def __init__(self):super().__init__('localhost',timeout=10)
    def connect(self):self.sock=socket.socket(socket.AF_UNIX,socket.SOCK_STREAM);self.sock.settimeout(10);self.sock.connect(CONTROL)
def front(method='GET',path='/status',body=None):
    c=UnixHTTP()
    try:
        c.request(method,path,body=json.dumps(body) if body is not None else None,headers={'Content-Type':'application/json'})
        r=c.getresponse();raw=r.read();require(r.status==200,'ingress control rejected operation');return json.loads(raw)
    finally:c.close()
def update_front(target,held):
    state=front()['active'];next={'revision':state['revision']+1,'generation':target['instance'],'address':'127.0.0.1:'+str(target['listen_port']),'held':held}
    try:return front('POST','/switch',{'expected_revision':state['revision'],'target':next})
    except Exception:
        # A lost reply or committed-but-not-durable503 is not evidence of an
        # uncommitted switch. Re-read without repeating the old CAS revision.
        observed=front()
        require(observed.get('active')==next,'switch outcome differs; keep pending journal and reconcile')
        return {**observed,'switch_reply_reconciled':True}
def healthy(port):
    # Explicit no-proxy, local health only. Never calls a model or admin API.
    opener=urllib.request.build_opener(urllib.request.ProxyHandler({}))
    with opener.open('http://127.0.0.1:'+str(port)+'/health/ready',timeout=5) as r:
        require(r.status==200 and json.load(r).get('status')=='ready','candidate readiness failed')
def wait_healthy(port):
    deadline=time.monotonic()+30
    while True:
        try:healthy(port);return
        except Exception:
            if time.monotonic()>=deadline:raise Blocked('candidate readiness deadline exceeded')
            time.sleep(.2)
def parse_lifecycle_timestamp(value):
    # Python 3.9 accepts only 3/6 fractional digits; Go RFC3339Nano emits 1..9.
    # Normalize precision without rounding into the next second. Require an
    # explicit offset so the freshness comparison never uses host local time.
    require(isinstance(value,str),'invalid runtime lifecycle timestamp')
    match=re.fullmatch(r'(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2})(?:\.(\d{1,9}))?(Z|[+-]\d{2}:\d{2})',value)
    require(match is not None,'invalid runtime lifecycle timestamp')
    base,fraction,offset=match.groups()
    normalized=base+('.'+fraction.ljust(6,'0')[:6] if fraction else '')+('+00:00' if offset=='Z' else offset)
    try:return datetime.fromisoformat(normalized)
    except ValueError:raise Blocked('invalid runtime lifecycle timestamp') from None
def lifecycle(instance,drain=False):
    values=env(instance['pid']);token=values.get(b'CODEX_LIFECYCLE_TOKEN',b'')
    require(len(token)>=32,'runtime has no lifecycle observation endpoint; retain generation')
    request=urllib.request.Request('http://127.0.0.1:'+str(instance['listen_port'])+('/_internal/drain' if drain else '/_internal/lifecycle'),data=b'' if drain else None,headers={'X-Codex-Lifecycle-Token':token.decode()},method='POST' if drain else 'GET')
    opener=urllib.request.build_opener(urllib.request.ProxyHandler({}))
    try:
        with opener.open(request,timeout=5) as response:observed=json.load(response)
    finally:values.clear();token=b''
    require(observed.get('pid')==instance['pid'] and observed.get('binary_sha256')==instance['binary_sha256'],'runtime lifecycle identity differs')
    at=parse_lifecycle_timestamp(observed['observed_at'])
    require(0<=(datetime.now(timezone.utc)-at).total_seconds()<3,'runtime lifecycle observation stale')
    return observed
def port_free(port):
    s=socket.socket()
    try:s.bind(('127.0.0.1',port))
    except OSError:return False
    finally:s.close()
    return True
def private_backend_port(port):
    return type(port)is int and 1024<=port<=65535 and port not in (18878,18879,18191,28765)
def connected_sockets(pid,port):
    # Only inode/state and tuple hashes are persisted; no client address leaves
    # the server. Includes idle keepalive and WS, not merely active HTTP counts.
    owned=set()
    for path in Path('/proc/'+str(pid)+'/fd').iterdir():
        try:value=os.readlink(path)
        except OSError:continue
        if value.startswith('socket:['):owned.add(value[8:-1])
    found=[]
    for path in ('/proc/net/tcp','/proc/net/tcp6'):
        for line in Path(path).read_text().splitlines()[1:]:
            fields=line.split()
            if len(fields)<10 or fields[9] not in owned or int(fields[1].rsplit(':',1)[1],16)!=port or fields[3]=='0A':continue
            found.append({'inode':fields[9],'tuple_sha256':hashlib.sha256((fields[1]+' '+fields[2]).encode()).hexdigest(),'state':fields[3]})
    return sorted(found,key=lambda r:r['inode'])
def render_env(values):
    lines=[]
    for key,value in sorted(values.items()):
        name=key.decode();require(bool(re.fullmatch(r'[A-Za-z_][A-Za-z0-9_]*',name)),'invalid inherited environment name')
        text=value.decode();require('\x00' not in text and '\n' not in text and '\r' not in text,'multiline inherited environment requires explicit review')
        lines.append(name+'="'+text.replace('\\','\\\\').replace('"','\\"')+'"')
    return '\n'.join(lines)+'\n'
def nft_config(uid):
    require(type(uid)is int and uid>0,'invalid ingress uid')
    return f'''table inet codex_agent_ingress {{
 chain prerouting {{ type nat hook prerouting priority -101; policy accept;
  ct state new fib daddr type local tcp dport 18879 tcp flags & (syn | ack) == syn counter redirect to :18878
 }}
 chain output {{ type nat hook output priority -101; policy accept;
  meta skuid {uid} return
  ct state new fib daddr type local tcp dport 18879 tcp flags & (syn | ack) == syn counter redirect to :18878
 }}
 chain front_private {{ type filter hook input priority -1; policy accept;
  iifname "lo" return
  tcp dport 18878 ct status dnat ct original proto-dst 18879 return
  tcp dport 18878 drop
 }}
}}
'''
def verify_firewall():
    data=json.loads(command(['nft','-j','list','chain','inet','firewalld','filter_INPUT']))
    rules=[x['rule'] for x in data['nftables'] if 'rule' in x]
    require(any(any(e.get('match',{}).get('left')=={'ct':{'key':'status'}} and e.get('match',{}).get('right')=='dnat' for e in r['expr']) and any('accept'in e for e in r['expr']) for r in rules),'existing firewalld DNAT acceptance not verified')
    # No existing rule in any family may already own destination 18879 NAT.
    rules=json.loads(command(['nft','-j','list','ruleset']))['nftables']
    require(not any(x.get('table',{}).get('name')=='codex_agent_ingress' for x in rules),'ingress NAT already present; use status/recovery')
def scoped_nat_digest():
    text=command(['nft','--numeric','--stateless','list','table','inet','codex_agent_ingress'])
    return hashlib.sha256(text.encode()).hexdigest()
def ensure_nat(args):
    r=registry();expected=r['ingress'].get('nft_rules_sha256');config=BASE/'ingress.nft'
    require(expected and digest(config)==r['ingress'].get('nft_config_sha256'),'persisted NAT identity missing or changed')
    listed=subprocess.run(['nft','list','table','inet','codex_agent_ingress'],capture_output=True,text=True,timeout=10)
    if listed.returncode:
        command(['nft','--check','-f',str(config)]);command(['nft','-f',str(config)])
    require(scoped_nat_digest()==expected,'existing ingress NAT differs; refuse overwrite')
    return {'nat_verified':True,'scope':'inet codex_agent_ingress'}
def finish_bootstrap_nat(r):
    config=BASE/'ingress.nft'
    require(config.read_text()==nft_config(r['ingress']['uid']),'bootstrap NAT config differs from exact scoped template')
    r['ingress']['nft_config_sha256']=digest(config);r['ingress']['nft_rules_sha256']=scoped_nat_digest();write_registry(r)
    unit=f'''[Unit]
Description=Stable Codex Agent port mapping
After=network-online.target firewalld.service {INGRESS_UNIT}
Requires={INGRESS_UNIT}
[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/bin/python3 -B {Path(__file__).resolve()} ensure-nat
[Install]
WantedBy=multi-user.target
'''
    atomic('/etc/systemd/system/codex-agent-ingress-nat.service',unit,0o644)
    command(['systemctl','daemon-reload']);command(['systemctl','enable',INGRESS_UNIT,'codex-agent-ingress-nat.service'])
def bootstrap(args):
    require(not REGISTRY.exists(),'registry already exists')
    old=runtime('codex-agent.service');require(old['listen_port']==18879,'legacy is not direct 18879')
    require(old['binary_sha256']==args.expected_old_sha,'legacy binary changed')
    require(digest(args.front_binary)==args.front_sha and SHA.fullmatch(args.front_sha),'front binary evidence changed')
    require(port_free(FRONT),'front port occupied');verify_firewall()
    try:account=pwd.getpwnam('codex-agent-ingress')
    except KeyError:
        command(['useradd','--system','--no-create-home','--shell','/sbin/nologin','codex-agent-ingress']);account=pwd.getpwnam('codex-agent-ingress')
    require(account.pw_uid>0,'ingress must have a dedicated non-root UID')
    old.update(instance='legacy-'+old['binary_sha256'][:16],version=args.old_version,admission_mode='process_local')
    target={'revision':1,'generation':old['instance'],'address':'127.0.0.1:18879','held':False}
    INGRESS_STATE.parent.mkdir(parents=True,exist_ok=True);os.chown(INGRESS_STATE.parent,account.pw_uid,account.pw_gid)
    atomic(INGRESS_STATE,json.dumps(target)+'\n');os.chown(INGRESS_STATE,account.pw_uid,account.pw_gid)
    unit=f'''[Unit]
Description=Stable Codex Agent HTTP ingress
After=network-online.target
[Service]
Type=exec
User=codex-agent-ingress
Group=codex-agent-ingress
RuntimeDirectory=codex-agent-ingress
RuntimeDirectoryMode=0700
ExecStart={Path(args.front_binary).resolve()} --listen :18878 --admin-socket {CONTROL} --target-state {INGRESS_STATE}
Restart=on-failure
RestartSec=1
TimeoutStopSec=infinity
KillMode=mixed
{UNBOUNDED_RESOURCES}
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
ReadWritePaths=/var/lib/codex-agent-ingress
UMask=0077
[Install]
WantedBy=multi-user.target
'''
    atomic('/etc/systemd/system/'+INGRESS_UNIT,unit,0o644);command(['systemctl','daemon-reload']);command(['systemctl','start',INGRESS_UNIT])
    wait_healthy(FRONT);status=front();require(status['active']==target,'front not pointing at legacy')
    before=connected_sockets(old['pid'],18879)
    # Journal precedes atomic NAT transaction. A crash never auto-stops legacy.
    r={'schema':1,'revision':1,'public_listen_port':18879,'ingress':{'kind':'go_http','unit':INGRESS_UNIT,'listen_port':18878,'control_socket':CONTROL,'uid':account.pw_uid},'active':old,'draining':[],'pending':{'phase':'bootstrap_nat','direct_sockets_before':before},'updated_at':now()};write_registry(r)
    rules=nft_config(account.pw_uid);atomic(BASE/'ingress.nft',rules)
    command(['nft','--check','-f',str(BASE/'ingress.nft')]);command(['nft','-f',str(BASE/'ingress.nft')])
    finish_bootstrap_nat(r)
    r['pending']={'phase':'legacy_active_ingress_installed','direct_sockets_at_cutover':connected_sockets(old['pid'],18879),'network_external_ipv4_ipv6_validation_pending':True};write_registry(r)
    return {'phase':r['pending']['phase'],'old_stopped':False,'front':status,'direct_socket_count':len(r['pending']['direct_sockets_at_cutover'])}
def release_validator_name(manifest):
    # Performance releases carry their own functional/SLO evidence contract.
    # Keep the existing reliability validator for every prior release type.
    return 'performance_release.py' if manifest.get('backend')=='management_performance' else 'reliability_release.py'

def validate_stage_schema(release,manifest):
    import importlib.util
    sys.dont_write_bytecode=True
    spec=importlib.util.spec_from_file_location('candidate_release',release/'deploy'/release_validator_name(manifest));module=importlib.util.module_from_spec(spec);spec.loader.exec_module(module)
    module.validate(release);require(module.base.schema()==manifest['migration_checksums'],'bluegreen does not migrate/rebuild usage')

def stage(args):
    r=registry();active=r['active'];live=runtime(active['unit']);require(live['pid']==active['pid'] and live['binary_sha256']==active['binary_sha256'],'active runtime changed')
    release=Path(args.release).resolve();require(release.parent==Path('/opt/codex-agent/releases') and SHA.fullmatch(release.name),'release path outside immutable inventory')
    manifest=json.loads((release/'manifest.json').read_text());require(digest(release/'codex-agent')==manifest['files']['codex-agent'],'candidate binary digest mismatch')
    # Invoke existing immutable-release validator without activating its stop-
    # first path. Import helper creates no source mutations.
    validate_stage_schema(release,manifest)
    require(private_backend_port(args.port) and port_free(args.port),'candidate private port occupied or protected')
    instance=manifest['files']['codex-agent'][:20];unit='codex-agent-runtime-'+instance+'.service';values=env(live['pid']);values[b'CODEX_TRUSTED_LOOPBACK_INGRESS']=b'1';values[b'CODEX_RUNTIME_INSTANCE_ID']=instance.encode();values[b'CODEX_LIFECYCLE_TOKEN']=secrets.token_hex(32).encode()
    for transient in ('INVOCATION_ID','JOURNAL_STREAM','MAINPID','MANAGERPID','NOTIFY_SOCKET','LISTEN_PID','LISTEN_FDS','LISTEN_FDNAMES'):
        values.pop(transient.encode(),None)
    environment=Path('/etc/codex-agent/instances')/(instance+'.env');atomic(environment,render_env(values));values.clear()
    text=f'''[Unit]
Description=Codex Agent generation {instance}
After=network-online.target
[Service]
Type=exec
User=codex-agent
DynamicUser=yes
StateDirectory=codex-agent
StateDirectoryMode=0700
WorkingDirectory={release}
EnvironmentFile={environment}
ExecStart={release}/codex-agent --listen 127.0.0.1:{args.port} --web {release}/frontend/dist
Restart=on-failure
RestartSec=3
TimeoutStopSec=180
KillMode=control-group
{UNBOUNDED_RESOURCES}
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
UMask=0077
LimitNOFILE=65536
[Install]
WantedBy=multi-user.target
'''
    atomic('/etc/systemd/system/'+unit,text,0o644);command(['systemctl','daemon-reload']);command(['systemctl','start',unit]);wait_healthy(args.port)
    new=runtime(unit);require(new['binary_sha256']==manifest['files']['codex-agent'],'started wrong candidate binary');require(new['storage_identity_sha256']==live['storage_identity_sha256'],'candidate shared state differs');require(new['process_uid']==live['process_uid'],'shared StateDirectory user differs')
    observed=lifecycle(new)
    require(observed.get('admission_mode')=='redis_account_leases_v1','candidate shared account admission is not active')
    new.update(instance=instance,version=manifest['version'],admission_mode=observed['admission_mode'],release=str(release));r['candidate']=new;r['revision']+=1;r['updated_at']=now();write_registry(r)
    return {'staged':True,'candidate':new,'active_unchanged':True}
def hold_legacy(args):
    r=registry();require(r['active']['admission_mode']=='process_local','hold is only the one-time legacy transition');require('candidate'in r,'healthy candidate required before hold')
    status=update_front(r['active'],True);r['pending']={'phase':'legacy_drain_held','started_at':now(),'old':r['active'],'candidate':r['candidate']};write_registry(r)
    return {'phase':'legacy_drain_held','waiting_returns_503':False,'front':status,'old_stopped':False}
def resume_legacy(args):
    r=registry();require(r['active']['admission_mode']=='process_local','legacy resume only supports the initial generation')
    status=update_front(r['active'],False);r['pending']={'phase':'legacy_active_ingress_installed','resumed_at':now()};write_registry(r)
    return {'resumed':True,'active':r['active'],'front':status,'candidate_retained':r.get('candidate') is not None}
def expected_front_target(instance,revision,held):
    require(type(revision)is int and revision>0 and type(held)is bool,'invalid front revision/hold observation')
    return {'revision':revision,'generation':instance['instance'],'address':'127.0.0.1:'+str(instance['listen_port']),'held':held}

def exact_front_target(observed,expected):
    return (isinstance(observed,dict) and type(observed.get('revision'))is int and
            type(observed.get('held'))is bool and
            all(observed.get(key)==expected.get(key) for key in ('revision','generation','address','held')))

def require_durable_front(status):
    # Companion front commit-outcome contract is required for automatic recovery.
    # Missing/false is uncertainty, never proof that a target is durably committed.
    require(status.get('last_update_durable')is True,'front target durability not confirmed; preserve pending transition')

def prune_held_legacy_idle(old,status):
    # Health checks deliberately bypass Held, and can repopulate the backend's
    # idle pool after the original hold operation. The existing front closes
    # idle connections on every same-target Held CAS, never active SSE/WS.
    before=status.get('active') or {}
    require(exact_front_target(before,expected_front_target(old,before.get('revision'),True)),'idle prune requires exact held legacy target')
    after=expected_front_target(old,before['revision']+1,True)
    try:
        observed=front('POST','/switch',{'expected_revision':before['revision'],'target':after})
    except Exception:
        # No stale-CAS retry after a lost reply. A later invocation can re-read
        # the current revision; pending remains legacy_drain_held throughout.
        observed=front()
    require(exact_front_target(observed.get('active'),after),'idle prune outcome changed; keep legacy held and inspect')
    require_durable_front(observed)
    return observed

def reconcile(args):
    r=registry();status=front();pending=r.get('pending') or {}
    if pending.get('phase')=='switching':
        old,new=pending['old'],pending['new']
        before,after=pending.get('front_before'),pending.get('front_after')
        require(isinstance(before,dict) and isinstance(after,dict),'pending exact front intent missing')
        require(after==expected_front_target(new,before['revision']+1,False),'pending new target differs from candidate')
        require(before==expected_front_target(old,before['revision'],before['held']),'pending old target differs from active generation')
        if exact_front_target(status.get('active'),after):
            require_durable_front(status)
            actual=runtime(new['unit']);require(actual['pid']==new['pid'] and actual['binary_sha256']==new['binary_sha256'],'switched candidate identity changed')
            command(['systemctl','enable',new['unit']])
            r['active']=new
            if not any(x['instance']==old['instance'] for x in r['draining']):r['draining'].append(old)
            r.pop('candidate',None);r['pending']=None;r['revision']+=1;r['updated_at']=now();write_registry(r)
            return {'reconciled':'committed_already_switched_ingress','active':new}
        require(exact_front_target(status.get('active'),before),'ingress diverged from exact pending targets')
        require_durable_front(status)
        if old['admission_mode']=='process_local':
            resume=pending.get('resume_pending') or {}
            require(before['held']is True and resume.get('phase')=='legacy_drain_held','original legacy drain proof missing')
            require(resume.get('old')==old and resume.get('candidate')==new,'original drain generation identity changed')
            r['pending']=resume
        else:
            require(old['admission_mode']==new['admission_mode']=='redis_account_leases_v1','shared admission changed')
            r['pending']=pending.get('resume_pending')
        r['revision']+=1;r['updated_at']=now();write_registry(r)
        return {'reconciled':'old_exact_route_restored_to_retryable_phase','safe_next_action':'activate','front':status}
    if pending.get('phase')=='bootstrap_nat':
        table=json.loads(command(['nft','-j','list','table','inet','codex_agent_ingress']))
        require(any(x.get('table',{}).get('name')=='codex_agent_ingress' for x in table['nftables']),'bootstrap NAT absent; inspect before resuming')
        require(status['active']['generation']==r['active']['instance'],'bootstrap ingress changed')
        finish_bootstrap_nat(r)
        r['pending']={'phase':'legacy_active_ingress_installed','direct_sockets_at_cutover':connected_sockets(r['active']['pid'],18879),'network_external_ipv4_ipv6_validation_pending':True};write_registry(r)
        return {'reconciled':'bootstrap_nat_present','old_stopped':False}
    return {'reconciled':'no_pending_switch','front':status}
def activate(args):
    r=registry();old=r['active'];new=r.get('candidate');require(new is not None,'no candidate staged');live=runtime(new['unit']);require(live['pid']==new['pid'] and live['binary_sha256']==new['binary_sha256'],'candidate identity changed');healthy(new['listen_port'])
    require(lifecycle(new).get('admission_mode')=='redis_account_leases_v1','candidate admission changed since staging')
    status=front();before=status.get('active') or {}
    require(before==expected_front_target(old,before.get('revision'),before.get('held')),'front target does not match registry active')
    require_durable_front(status)
    if old['admission_mode']=='process_local':
        require(before['held'] and r.get('pending',{}).get('phase')=='legacy_drain_held','legacy admission requires held first migration')
        status=prune_held_legacy_idle(old,status);before=status['active']
        counts=status['generations'].get(old['instance'],{});direct=connected_sockets(old['pid'],old['listen_port'])
        if direct or counts.get('active_http_and_websocket',0):return {'activated':False,'phase':'waiting_for_legacy_natural_drain','old_backend_connections':len(direct),'old_front_requests':counts.get('active_http_and_websocket',0),'waiting_requests':status['waiting_requests'],'old_stopped':False}
    else:require(old['admission_mode']==new['admission_mode']=='redis_account_leases_v1','shared account admission not verified')
    require(new['admission_mode']=='redis_account_leases_v1','candidate shared account admission not verified')
    after=expected_front_target(new,before['revision']+1,False)
    r['pending']={'phase':'switching','old':old,'new':new,'front_before':before,'front_after':after,'resume_pending':r.get('pending')};write_registry(r)
    command(['systemctl','enable',new['unit']])
    # Send exactly the journaled CAS: do not fetch a newer revision and silently
    # overwrite an unrelated control transition. On error, reconcile keeps both.
    changed=front('POST','/switch',{'expected_revision':before['revision'],'target':after})
    require(exact_front_target(changed.get('active'),after),'front did not confirm exact target')
    require_durable_front(changed)
    r['active']=new;r['draining'].append(old);r.pop('candidate',None);r['pending']=None;r['revision']+=1;r['updated_at']=now();write_registry(r)
    return {'activated':True,'active':new,'draining':old,'old_stopped':False}
def retire(args):
    r=registry();matches=[v for v in r['draining'] if v['unit']==args.unit];require(len(matches)==1,'unit is not exactly one draining generation');old=matches[0];live=runtime(old['unit']);require(live['pid']==old['pid'] and live['binary_sha256']==old['binary_sha256'],'draining PID/SHA changed')
    status=front();require(status['active']['generation']!=old['instance'],'cannot retire active generation');sockets=connected_sockets(old['pid'],old['listen_port']);counts=status['generations'].get(old['instance'],{})
    if sockets or counts.get('active_http_and_websocket',0):return {'retired':False,'connections':len(sockets),'front_requests':counts.get('active_http_and_websocket',0)}
    # Root must supply verified zero operation/persistence/drain observation;
    # TCP zero alone misses detached OAuth exchanges after client disconnect.
    if old['admission_mode']=='process_local':return {'retired':False,'reason':'legacy_retirement_unverified_keep_running'}
    lifecycle(old,drain=True)
    observation=lifecycle(old)
    require(observation.get('draining') is True,'runtime workers not draining')
    require(all(type(observation.get(k))is int and observation[k]==0 for k in ('active_handlers','refresh_operations','pending_persistence')),'runtime work not drained')
    require(not connected_sockets(old['pid'],old['listen_port']),'new old-generation socket detected')
    live=runtime(old['unit'])
    require(live['pid']==old['pid'] and live['binary_sha256']==old['binary_sha256'],'draining PID/SHA changed before stop')
    command(['systemctl','stop',old['unit']],timeout=200);command(['systemctl','disable',old['unit']])
    r['draining']=[v for v in r['draining'] if v['unit']!=old['unit']];r['revision']+=1;r['updated_at']=now();write_registry(r)
    return {'retired':True,'unit':old['unit']}
def main():
    import sys
    parser=argparse.ArgumentParser(description=__doc__);parser.add_argument('action',choices=['status','bootstrap','stage','hold-legacy','resume-legacy','activate','retire','reconcile','ensure-nat']);parser.add_argument('--front-binary');parser.add_argument('--front-sha');parser.add_argument('--expected-old-sha');parser.add_argument('--old-version',default='0.4.20');parser.add_argument('--release');parser.add_argument('--port',type=int);parser.add_argument('--admission-mode',choices=['process_local','redis_account_leases_v1'],default='process_local');parser.add_argument('--unit');parser.add_argument('--drain-evidence');args=parser.parse_args()
    require(os.geteuid()==0,'server-side root required');BASE.mkdir(parents=True,exist_ok=True)
    with (BASE/'zero-downtime.lock').open('a') as lock:
        fcntl.flock(lock,fcntl.LOCK_EX)
        if args.action=='status':result={'registry':registry(),'front':front()}
        else:result={'bootstrap':bootstrap,'stage':stage,'hold-legacy':hold_legacy,'resume-legacy':resume_legacy,'activate':activate,'retire':retire,'reconcile':reconcile,'ensure-nat':ensure_nat}[args.action](args)
    print(json.dumps(result))
if __name__=='__main__':
    import sys
    try:main()
    except Exception as e:print(json.dumps({'error':str(e) if isinstance(e,Blocked) else type(e).__name__}),file=sys.stderr);raise SystemExit(1)
