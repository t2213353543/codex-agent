"""One cold native HTTP inference via a pinned explicit route; no retry/fallback.

SOCKS5h uses a one-connection CONNECT byte tunnel because the fixed helper's
HTTP feature set deliberately does not enable reqwest SOCKS. Target TLS stays
inside the native helper. No browser headers, body fields or trust roots change.
The caller owns network namespace selection and supplies an already resolved
private proxy endpoint when container-only DNS names cannot resolve on the host.
"""
from __future__ import annotations
import base64,errno,hmac,ipaddress,json,math,os,re,secrets,select,socket,struct,subprocess,tempfile,threading,time
from pathlib import Path
from urllib.parse import urlsplit,unquote
import quality_cookie_lifecycle_probe as base
import quality_usage_probe as usage

SCHEMA='codex-quality-route-lifecycle-v1'
class RouteError(base.ExactProbeError):pass

def _route(value):
    if value is None:return None
    if type(value) is not str or len(value)>8192 or any(ord(x)<33 or ord(x)==127 for x in value):raise RouteError('invalid_proxy_route')
    try:
        parsed=urlsplit(value);port=parsed.port
        if parsed.scheme not in ('http','https','socks5h') or not parsed.hostname or port is None or not 1<=port<=65535 or parsed.path not in ('','/') or parsed.query or parsed.fragment:raise ValueError()
        if (parsed.username is None)!=(parsed.password is None):raise ValueError()
        username=unquote(parsed.username or '',errors='strict').encode();password=unquote(parsed.password or '',errors='strict').encode()
        if any(b<32 or b==127 for b in username+password):raise ValueError()
        if parsed.scheme=='socks5h' and (len(username)>255 or len(password)>255):raise ValueError()
        if parsed.scheme=='socks5h':ipaddress.ip_address(parsed.hostname)
        if parsed.username is not None and (not username or not password):raise ValueError()
        parsed.hostname.encode('idna')
    except (ValueError,UnicodeError):raise RouteError('invalid_proxy_route') from None
    return parsed,username,password

def _target(upstream):
    base._origin(upstream)
    parsed=urlsplit(upstream)
    if parsed.scheme!='https':raise RouteError('route_probe_requires_https_target')
    host=parsed.hostname;port=parsed.port or 443
    return host,port,('%s:%d'%('['+host+']' if ':' in host else host,port))

def _socket_deadline(stream,deadline):stream.settimeout(base._remaining(deadline))
def _read(stream,count,deadline):
    result=bytearray()
    while len(result)<count:
        _socket_deadline(stream,deadline);part=stream.recv(count-len(result))
        if not part:raise RouteError('proxy_protocol_eof')
        result.extend(part)
    return bytes(result)
def _send(stream,data,deadline):
    _socket_deadline(stream,deadline);stream.sendall(data)

class _SOCKSTunnel:
    """Exactly one authenticated CONNECT to one fixed target, finite lifetime."""
    def __init__(self,route,target,deadline,cleanup_end,record):
        self.route,self.target,self.deadline,self.cleanup_end,self.record=route,target,deadline,cleanup_end,record
        self.listener=None;self.thread=None;self.connections=[];self.lock=threading.Lock();self.stopped=threading.Event()
        self.secret=secrets.token_hex(24)
    def start(self):
        self.listener=socket.socket(socket.AF_INET,socket.SOCK_STREAM);self.listener.bind(('127.0.0.1',0));self.listener.listen(1);self.listener.settimeout(min(.2,base._remaining(self.deadline)))
        self.record.update(bridge_started=True,proxy_connection_attempts=0,local_connect_attempts=0,socks_connect_attempts=0,target_tls_terminated_by_bridge=False)
        self.thread=threading.Thread(target=self._run,name='quality-single-socks-tunnel',daemon=True);self.thread.start()
        return 'http://probe:'+self.secret+'@127.0.0.1:'+str(self.listener.getsockname()[1])
    def _track(self,stream):
        with self.lock:
            if self.stopped.is_set():stream.close();raise RouteError('proxy_bridge_closed')
            self.connections.append(stream)
        return stream
    def _run(self):
        try:
            while True:
                if self.stopped.is_set():return
                base._remaining(self.deadline)
                try:client,_=self.listener.accept();break
                except socket.timeout:continue
            self._track(client);self.listener.close();self.record['local_connect_attempts']=1
            raw=bytearray()
            while not raw.endswith(b'\r\n\r\n'):
                if len(raw)>=16384:raise RouteError('proxy_connect_header_limit')
                raw.extend(_read(client,1,self.deadline))
            lines=bytes(raw).split(b'\r\n');expected=('CONNECT '+self.target[2]+' HTTP/1.1').encode()
            if lines[0]!=expected:raise RouteError('proxy_connect_target_mismatch')
            headers={}
            for line in lines[1:-2]:
                if b':' not in line:raise RouteError('proxy_connect_header_invalid')
                name,value=line.split(b':',1);name=name.lower();headers.setdefault(name,[]).append(value.strip())
            auth=headers.get(b'proxy-authorization',[]);wanted=b'Basic '+base64.b64encode(('probe:'+self.secret).encode())
            if len(auth)!=1 or not hmac.compare_digest(auth[0],wanted):raise RouteError('proxy_connect_auth_invalid')
            parsed,user,password=self.route
            self.record['proxy_connection_attempts']=1
            address_ip=ipaddress.ip_address(parsed.hostname)
            proxy=self._track(socket.socket(socket.AF_INET if address_ip.version==4 else socket.AF_INET6,socket.SOCK_STREAM))
            proxy.setblocking(False)
            code=proxy.connect_ex((parsed.hostname,parsed.port))
            if code not in (0,errno.EINPROGRESS,errno.EWOULDBLOCK,errno.EALREADY,10035):raise RouteError('proxy_connect_failed')
            while code:
                if self.stopped.is_set():return
                _,ready,_=select.select([],[proxy],[],min(.2,base._remaining(self.deadline)))
                if ready:
                    if proxy.getsockopt(socket.SOL_SOCKET,socket.SO_ERROR):raise RouteError('proxy_connect_failed')
                    break
            proxy.setblocking(True)
            method=2 if parsed.username is not None else 0
            _send(proxy,bytes([5,1,method]),self.deadline)
            if _read(proxy,2,self.deadline)!=bytes([5,method]):raise RouteError('socks_method_rejected')
            if method==2:
                _send(proxy,bytes([1,len(user)])+user+bytes([len(password)])+password,self.deadline)
                if _read(proxy,2,self.deadline)!=b'\x01\x00':raise RouteError('socks_auth_rejected')
            host,port,_=self.target
            try:
                ip=ipaddress.ip_address(host);address=bytes([1 if ip.version==4 else 4])+ip.packed
            except ValueError:
                encoded=host.encode('idna')
                if not 1<=len(encoded)<=255:raise RouteError('socks_target_invalid')
                address=bytes([3,len(encoded)])+encoded
            self.record.update(socks_connect_attempts=1,socks_target_address_type=address[0],socks_target_sha256=base.sha(host.encode()),remote_dns_requested=address[0]==3)
            _send(proxy,b'\x05\x01\x00'+address+struct.pack('!H',port),self.deadline)
            response=_read(proxy,4,self.deadline)
            if response[:3]!=b'\x05\x00\x00':raise RouteError('socks_connect_rejected')
            size={1:4,4:16}.get(response[3])
            if response[3]==3:size=_read(proxy,1,self.deadline)[0]
            if size is None:raise RouteError('socks_reply_invalid')
            _read(proxy,size+2,self.deadline)
            self.record['socks_tunnel_established']=True
            _send(client,b'HTTP/1.1 200 Connection Established\r\n\r\n',self.deadline)
            counts=[0,0]
            while not self.stopped.is_set():
                ready,_,_=select.select([client,proxy],[],[],min(.2,base._remaining(self.deadline)))
                for source in ready:
                    _socket_deadline(source,self.deadline);data=source.recv(65536)
                    if not data:return
                    index=0 if source is client else 1;counts[index]+=len(data)
                    _send(proxy if index==0 else client,data,self.deadline)
                    self.record['tunnel_bytes_to_proxy'],self.record['tunnel_bytes_from_proxy']=counts
        except Exception as error:
            if not self.stopped.is_set():self.record['bridge_error']=error.args[0] if isinstance(error,RouteError) else 'proxy_bridge_timeout' if isinstance(error,(TimeoutError,socket.timeout)) else 'proxy_bridge_failed'
        finally:
            if self.listener is not None:self.listener.close()
            with self.lock:
                for stream in self.connections:
                    try:stream.close()
                    except OSError:pass
    def close(self):
        self.stopped.set()
        if self.listener is not None:self.listener.close()
        with self.lock:
            for stream in self.connections:
                try:stream.shutdown(socket.SHUT_RDWR)
                except OSError:pass
                stream.close()
        if self.thread is not None:self.thread.join(timeout=max(0,min(2,self.cleanup_end-time.monotonic())))
        joined=self.thread is None or not self.thread.is_alive()
        with self.lock:closed=all(stream.fileno()==-1 for stream in self.connections)
        listener_closed=self.listener is None or self.listener.fileno()==-1
        self.record.update(bridge_joined=joined,bridge_sockets_closed=closed,bridge_listener_closed=listener_closed)
        return joined and closed and listener_closed

class _RouteHelper(base._Helper):
    def __init__(self,*args,proxy_url=None):super().__init__(*args);self.proxy_url=proxy_url
    def start(self):
        base._remaining(self.operation_end)
        self.directory=tempfile.TemporaryDirectory(prefix='qroute-');os.chmod(self.directory.name,0o700);self.path=str(Path(self.directory.name)/'s')
        config={'protocol':1,'unix_socket':self.path,'upstream':self.upstream,'ipc_token':self.ipc_token,'headers_timeout_ms':min(45000,max(1,int(base._remaining(self.operation_end)*1000))),'idle_timeout_ms':0,'http_client_lifecycle':'cli_request','proxy':self.proxy_url}
        self.process=subprocess.Popen([str(self.helper)],stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=subprocess.DEVNULL,close_fds=True,bufsize=0,env={'PATH':os.environ.get('PATH','/usr/bin:/bin'),'LANG':'C.UTF-8'})
        self.record.update(pid=self.process.pid,process_started=True)
        encoded=json.dumps(config,separators=(',',':')).encode();base.write_all(self.process.stdin.write,struct.pack('!I',len(encoded))+encoded)
        ready=base._json(base._ready_line(self.process.stdout,base._remaining(self.operation_end,8)))
        if not isinstance(ready,dict) or type(ready.get('protocol')) is not int or ready['protocol']!=1 or ready.get('transport')!='unix' or ready.get('unix_socket')!=self.path or ready.get('http_client_lifecycle')!='cli_request' or not isinstance(ready.get('capabilities'),list):raise RouteError('helper_readiness_mismatch')
        runtime=base.file_digest(Path('/proc')/str(self.process.pid)/'exe');self.record['runtime_executable_sha256']=runtime
        if runtime!=self.digest:raise RouteError('helper_runtime_mismatch')
        self.capabilities=ready['capabilities'];self.record.update(readiness_verified=True,lifecycle='cli_request');return self

def native_route_request(helper,expected_helper_sha,body,captured,token,workspace,hidden=(),timeout=240,*,proxy_url=None,deadline=None,application_identity=None,upstream='https://chatgpt.com'):
    """Single exact first-turn POST. None proxy is an explicit direct control.

    Run this entire function in the desired namespace. No nsenter, DNS setup,
    CA injection or identity generation is performed here. Scheme/URL values
    stay private; the receipt contains only scheme and cryptographic summaries.
    """
    began=time.monotonic();report={'schema':SCHEMA,'transport':'http','mode':'cold','protocol_success':False,'eligible_for_scoring':False,'answer':None,'final_output':'','generated_answer_available':False,'response_completion_observed':False,'automatic_retries':False,'automatic_refresh':False,'http_fallback':False,'direct_fallback':False,'model_post_attempts':0,'native_dispatch_count':0,'metadata_get_count':0,'inference':{'native_dispatch_count':0},'helpers':[],'route':{},'upstream_wire_capture':False,'environment_policy':'PATH_LANG_only_explicit_IPC_route_no_proxy_environment_or_CA_override'}
    child=bridge=collector=None;end=began+240
    try:
        if type(timeout) not in (int,float) or not math.isfinite(timeout) or not 1<=timeout<=240 or deadline is not None and (type(deadline) not in (int,float) or not math.isfinite(deadline)):raise RouteError('invalid_deadline')
        end=min(began+timeout,deadline) if deadline is not None else began+timeout
        if end-began<1:raise RouteError('deadline_before_start')
        operation_end=end-min(8,(end-began)/4)
        if os.name!='posix' or not hasattr(socket,'AF_UNIX'):raise RouteError('linux_posix_required')
        helper=Path(helper)
        if type(expected_helper_sha) is not str or not re.fullmatch('[a-f0-9]{64}',expected_helper_sha) or base.file_digest(helper)!=expected_helper_sha:raise RouteError('helper_digest_mismatch')
        route=_route(proxy_url);target=_target(upstream);identity=base._validate_capture(body,captured,application_identity)
        if type(token) is not str or not token or not token.isascii() or len(token)>65536 or any(ord(c)<=32 or ord(c)==127 for c in token) or type(workspace) is not str or not re.fullmatch('[A-Za-z0-9_-]{1,160}',workspace):raise RouteError('invalid_credential_input')
        private_route=[] if route is None else [proxy_url,route[1].decode(),route[2].decode()]
        hidden=tuple(dict.fromkeys([token,workspace]+[v for v in list(hidden)+private_route if type(v) is str and v]));collector=base.SSECollector(hidden)
        captured=json.loads(json.dumps(captured))
        report.update(helper_sha256=expected_helper_sha,request_body_sha256=base.sha(body),request_body_bytes=len(body),token_sha256=base.sha(token.encode()),workspace_sha256=base.sha(workspace.encode()),application_identity_sha256=base.sha(json.dumps(identity,sort_keys=True,separators=(',',':')).encode()),caller_uid=os.geteuid(),caller_network_namespace_sha256=base.sha(os.readlink('/proc/self/ns/net').encode()))
        public=report['route'];public.update(mode='direct' if route is None else 'explicit_proxy',explicit_proxy=route is not None,scheme='direct' if route is None else route[0].scheme,proxy_url_sha256=base.sha(proxy_url.encode()) if route else None,route_configuration_scope='native_Config.proxy',no_proxy_environment_ignored=True)
        native_proxy=proxy_url
        if route and route[0].scheme=='socks5h':
            bridge=_SOCKSTunnel(route,target,operation_end,end,public);native_proxy=bridge.start();public['route_configuration_scope']='native_HTTP_CONNECT_to_single_use_SOCKS5h_byte_tunnel'
        record={'role':'post','scope_id':secrets.token_hex(16),'process_started':False};report['helpers'].append(record)
        child=_RouteHelper(helper,expected_helper_sha,upstream,operation_end,end,record,proxy_url=native_proxy);child.start()
        if captured['request_compression']=='zstd' and 'request_zstd_v1' not in child.capabilities:raise RouteError('helper_compression_unavailable')
        base._transaction(child,base.exact_head(captured,body,token,workspace,child.ipc_token,1),body,report['inference'],collector)
        report.update(base._collector_report(collector,report['inference'].get('http_status')));report.update(response_completion_observed=report['protocol_success'],eligible_for_scoring=report['protocol_success'] and report['generated_answer_available'])
    except Exception as error:
        if collector is not None:
            try:
                observed=base._collector_report(collector,report['inference'].get('http_status'));report.update(observed,response_completion_observed=observed['protocol_success'])
            except Exception:pass
        base._invalidate(report,'timeout' if isinstance(error,(TimeoutError,socket.timeout)) else 'probe_error',error.args[0] if isinstance(error,base.ExactProbeError) else 'route_probe_failed')
    finally:
        cleaned=True
        if child is not None:cleaned=child.close() and cleaned
        if bridge is not None:cleaned=bridge.close() and cleaned
        report['route'].update(connect_count=report['route'].get('local_connect_attempts') if bridge else None,connect_count_scope='accepted_local_CONNECT_tunnels' if bridge else 'native_transport_not_instrumented',tunnel_reaped=report['route'].get('bridge_joined') if bridge else None,route_cleanup_ok=cleaned)
        post=report['inference'];report.update(native_dispatch_count=post.get('native_dispatch_count',0),model_post_attempts=int(bool(post.get('ipc_dispatch_attempted'))),attempts=int(bool(post.get('ipc_dispatch_attempted'))),helper_reaped=bool(child) and child.record.get('reaped') is True,helper_forced_kill=bool(child) and child.record.get('forced_kill') is True,local_socket_absent=bool(child) and child.record.get('local_socket_absent') is True,duration_ms=round((time.monotonic()-began)*1000))
        for key in ('request_header_summary','request_body_summary','actual_ipc_body_sha256','local_ipc_write_complete','ipc_dispatch_attempted','ipc_bytes_accepted','http_version','response_header_hashes','set_cookie','retry_after','error_body'):
            if key in post:report[key]=post[key]
        if not cleaned:base._invalidate(report,'helper_cleanup_failed','route_or_helper_cleanup_failed')
        elif report['route'].get('bridge_error'):base._invalidate(report,'proxy_error',report['route']['bridge_error'])
    return report


def native_route_usage(helper,expected_helper_sha,token,workspace,timeout=35,*,proxy_url=None,deadline=None,application_identity=None,expected_model='gpt-5.6-sol',upstream='https://chatgpt.com'):
    """One GET /usage through the same explicit route, zero model/grant calls.

    Keeps the existing codex-quality-usage-v1 receipt contract, with additional
    route/lifecycle evidence. A cleanup failure makes eligibility unknown.
    """
    began=time.monotonic();report={'schema':usage.SCHEMA,'method':'GET','endpoint':usage.ENDPOINT,'model_calls':0,'attempts':0,'native_dispatch_count':0,'automatic_retries':False,'automatic_refresh':False,'direct_fallback':False,'started_at':usage.utc_now(),'protocol_success':False,'upstream_wire_capture':False,'route':{},'environment_policy':'PATH_LANG_only_explicit_IPC_route_no_proxy_environment_or_CA_override','eligibility':{'allowed':None,'reason':'probe_not_completed'}}
    child=bridge=stream=None;end=began+35
    try:
        if type(timeout) not in (int,float) or not math.isfinite(timeout) or not 1<=timeout<=35 or deadline is not None and (type(deadline) not in (int,float) or not math.isfinite(deadline)):raise RouteError('invalid_deadline')
        end=min(began+timeout,deadline) if deadline is not None else began+timeout
        if end-began<1:raise RouteError('deadline_before_start')
        operation_end=end-min(4,(end-began)/4)
        if os.name!='posix' or not hasattr(socket,'AF_UNIX'):raise RouteError('linux_posix_required')
        helper=Path(helper)
        if type(expected_helper_sha) is not str or not re.fullmatch('[a-f0-9]{64}',expected_helper_sha) or base.file_digest(helper)!=expected_helper_sha:raise RouteError('helper_digest_mismatch')
        identity=usage.validate_application_identity(application_identity)
        user=usage.validate_identity(token,workspace,timeout)
        claims=base._json(base64.urlsafe_b64decode(token.split('.')[1]+'='*(-len(token.split('.')[1])%4)))
        auth=claims['https://api.openai.com/auth']
        if user is None:user=auth.get('user_id')
        if type(user) is not str or not user or len(user)>512:raise RouteError('credential_stable_user_missing')
        if type(expected_model) is not str or not re.fullmatch('[A-Za-z0-9._:/-]{1,256}',expected_model):raise RouteError('invalid_expected_model')
        route=_route(proxy_url);target=_target(upstream)
        report.update(helper_sha256=expected_helper_sha,token_sha256=base.sha(token.encode()),workspace_sha256=base.sha(workspace.encode()),expected_user_sha256=base.sha(user.encode()),expected_model=expected_model,timeout_seconds=timeout,application_identity_sha256=base.sha(json.dumps(identity,sort_keys=True,separators=(',',':')).encode()),caller_uid=os.geteuid(),caller_network_namespace_sha256=base.sha(os.readlink('/proc/self/ns/net').encode()))
        public=report['route'];public.update(mode='direct' if route is None else 'explicit_proxy',explicit_proxy=route is not None,scheme='direct' if route is None else route[0].scheme,proxy_url_sha256=base.sha(proxy_url.encode()) if route else None,route_configuration_scope='native_Config.proxy',no_proxy_environment_ignored=True)
        native_proxy=proxy_url
        if route and route[0].scheme=='socks5h':
            bridge=_SOCKSTunnel(route,target,operation_end,end,public);native_proxy=bridge.start();public['route_configuration_scope']='native_HTTP_CONNECT_to_single_use_SOCKS5h_byte_tunnel'
        record={'role':'usage','scope_id':secrets.token_hex(16),'process_started':False};report['helper_lifecycle']=record
        child=_RouteHelper(helper,expected_helper_sha,upstream,operation_end,end,record,proxy_url=native_proxy);child.start()
        stream=child.connect(report)
        head=usage.usage_head(token,workspace,child.ipc_token,max(1,int(base._remaining(operation_end)*1000)),identity)
        report['request_header_hashes']=[{'name':name.lower(),'value_sha256':base.sha(base64.b64decode(value))} for name,value in head['headers']]
        raw=json.dumps(head,separators=(',',':')).encode();report.update(attempts=1,ipc_dispatch_attempted=True,dispatch_started_at=usage.utc_now(),ipc_bytes_accepted=0)
        def accepted(part):report['ipc_bytes_accepted']+=len(part)
        base.write_all(lambda value:base.deadline_io(stream,operation_end,'send',value),struct.pack('!I',len(raw))+raw,accepted)
        report['native_dispatch_count']=1
        response,raw=usage.read_response(stream,operation_end)
        report.update(http_status=response['status'],http_version=response['version'],body_bytes=len(raw),body_sha256=base.sha(raw),response_received_at=usage.utc_now(),ipc_end_observed=True)
        if response['status']!=200:report['error_body']=base.safe_error_body(raw);raise usage.UsageProbeError('usage_http_error')
        summary=usage.summarize_usage(usage.strict_json(raw),workspace,user,expected_model)
        report.update(protocol_success=True,usage=summary,eligibility=dict(summary['eligibility']))
    except Exception as error:
        code=error.args[0] if isinstance(error,(base.ExactProbeError,usage.UsageProbeError)) else 'timeout' if isinstance(error,(TimeoutError,socket.timeout)) else 'route_usage_probe_failed'
        report.update(error_class=code,eligibility={'allowed':None,'reason':code})
    finally:
        cleaned=True
        if stream is not None:
            try:stream.close()
            except OSError:cleaned=False
        if child is not None:cleaned=child.close() and cleaned
        if bridge is not None:cleaned=bridge.close() and cleaned
        report['route'].update(connect_count=report['route'].get('local_connect_attempts') if bridge else None,connect_count_scope='accepted_local_CONNECT_tunnels' if bridge else 'native_transport_not_instrumented',tunnel_reaped=report['route'].get('bridge_joined') if bridge else None,route_cleanup_ok=cleaned)
        report.update(helper_exit_code=child.record.get('returncode') if child else None,helper_reaped=bool(child) and child.record.get('reaped') is True,helper_forced_kill=bool(child) and child.record.get('forced_kill') is True,socket_removed=bool(child) and child.record.get('local_socket_absent') is True,elapsed_ms=round((time.monotonic()-began)*1000),finished_at=usage.utc_now(),cleanup_budget_seconds=min(4,(end-began)/4))
        if not cleaned:report.update(protocol_success=False,eligibility={'allowed':None,'reason':'helper_cleanup_failed'})
        elif report['route'].get('bridge_error'):report.update(protocol_success=False,eligibility={'allowed':None,'reason':report['route']['bridge_error']})
    return report
