"""Synthetic IPC and real loopback CONNECT/SOCKS tests; never real accounts."""
import base64,contextlib,io,json,os,pathlib,select,shutil,socket,socketserver,ssl,struct,subprocess,tempfile,threading,time,unittest
from types import SimpleNamespace
from unittest import mock
import quality_route_lifecycle_probe as probe
from test_quality_cookie_lifecycle_probe import BODY,TOKEN,WORKSPACE,HELPER_SHA,IDENTITY,captured,Child,Socket,final_wire,request

class RouteProbeTests(unittest.TestCase):
 def call(self,proxy_url=None,*,wire=None,child=None,operation='request',token=TOKEN,**kwargs):
  stream=Socket(final_wire() if wire is None else wire);child=child or Child(8001);seen=[]
  def popen(*args,**options):seen.append(options);return child
  def ready(*args):
   config=json.loads(child.stdin.getvalue()[4:]);return json.dumps({'protocol':1,'transport':'unix','unix_socket':config['unix_socket'],'http_client_lifecycle':'cli_request','capabilities':['request_zstd_v1']}).encode()
  fake_os=SimpleNamespace(name='posix',environ={'PATH':'/safe/path','HTTPS_PROXY':'ignored-private-proxy','CODEX_CA_CERTIFICATE':'ignored-private-ca'},chmod=os.chmod,geteuid=lambda:61787,readlink=lambda p:'net:[synthetic]')
  with mock.patch.object(probe,'os',fake_os),mock.patch.object(probe.base,'file_digest',return_value=HELPER_SHA),mock.patch.object(probe.subprocess,'Popen',side_effect=popen),mock.patch.object(probe.base,'_ready_line',side_effect=ready),mock.patch.object(probe.base.socket,'AF_UNIX',1,create=True),mock.patch.object(probe.base.socket,'socket',return_value=stream):
   if operation=='request':result=probe.native_route_request(pathlib.Path('synthetic'),HELPER_SHA,BODY,captured(),token,WORKSPACE,timeout=12,proxy_url=proxy_url,application_identity=IDENTITY,**kwargs)
   else:result=probe.native_route_usage(pathlib.Path('synthetic'),HELPER_SHA,token,WORKSPACE,timeout=12,proxy_url=proxy_url,application_identity=IDENTITY,**kwargs)
  return result,stream,child,seen
 def test_direct_and_explicit_HTTP_config_preserve_exact_request(self):
  for route in (None,'http://proxy-user:proxy-pass@127.0.0.1:3128','https://127.0.0.1:3129'):
   with self.subTest(route=bool(route)):
    result,stream,child,seen=self.call(route)
    self.assertTrue(result['protocol_success'],result);self.assertEqual(result['answer'],21);self.assertEqual(result['native_dispatch_count'],1)
    self.assertEqual(request(stream)[1],BODY);self.assertEqual(request(stream)[0]['headers'][:-2],captured()['headers'])
    self.assertEqual(json.loads(child.stdin.getvalue()[4:])['proxy'],route);self.assertEqual(len(seen),1)
    self.assertEqual(set(seen[0]['env']),{'PATH','LANG'});self.assertTrue(result['helper_reaped'] and result['local_socket_absent'])
    self.assertFalse(result['direct_fallback']);self.assertNotIn('proxy-pass',json.dumps(result));self.assertNotIn(TOKEN,json.dumps(result))
 def test_invalid_route_rejected_before_native_launch(self):
  for bad in ('socks5://127.0.0.1:1080','socks5h://alias-only:1080','http://127.0.0.1:1/path','http://127.0.0.1:1?x','http://a:bad','http://user@127.0.0.1:1','http://u:%0A@127.0.0.1:1'):
   with self.subTest(bad=bad):
    result,_,_,seen=self.call(bad);self.assertFalse(result['protocol_success']);self.assertEqual(len(seen),0);self.assertEqual(result['model_post_attempts'],0)
 def test_typed_proxy_failure_never_retries_or_falls_back(self):
  wire=struct.pack('!BI',4,len(b'upstream_connect'))+b'upstream_connect'
  result,_,_,seen=self.call('http://127.0.0.1:1',wire=wire)
  self.assertFalse(result['protocol_success']);self.assertEqual(len(seen),1);self.assertEqual(result['native_dispatch_count'],1);self.assertFalse(result['direct_fallback']);self.assertTrue(result['helper_reaped'])
 def test_completed_observed_evidence_survives_cleanup_error(self):
  result,_,_,_=self.call(child=Child(8001,stuck=True))
  self.assertFalse(result['protocol_success']);self.assertEqual(result['observed_completion']['answer'],21);self.assertEqual(result['failure_category'],'helper_cleanup_failed')
 def test_usage_same_route_exact_single_GET_and_safe_eligibility(self):
  claims={'exp':time.time()+9000,'https://api.openai.com/auth':{'chatgpt_user_id':'synthetic-user','chatgpt_account_id':WORKSPACE}}
  token='header.'+base64.urlsafe_b64encode(json.dumps(claims).encode()).decode().rstrip('=')+'.signature'
  from test_quality_cookie_lifecycle_probe import http_wire
  for allowed in (True,False):
   body=json.dumps({'account_id':WORKSPACE,'user_id':'synthetic-user','rate_limit':{'allowed':allowed,'limit_reached':not allowed}}).encode()
   result,stream,child,seen=self.call('http://127.0.0.1:3128',operation='usage',token=token,wire=http_wire(body))
   self.assertTrue(result['protocol_success'],result);self.assertEqual(result['eligibility']['allowed'],allowed)
   self.assertEqual(result['model_calls'],0);self.assertEqual(result['native_dispatch_count'],1);self.assertEqual(len(seen),1)
   head,body=request(stream);self.assertEqual((head['method'],head['path'],body),('GET','/backend-api/wham/usage',b''));self.assertEqual(json.loads(child.stdin.getvalue()[4:])['proxy'],'http://127.0.0.1:3128')
   self.assertNotIn(token,json.dumps(result));self.assertTrue(result['socket_removed']);self.assertFalse(result['direct_fallback'])
 def test_usage_unknown_owner_or_bad_frame_never_allows(self):
  claims={'exp':time.time()+9000,'https://api.openai.com/auth':{'user_id':'fallback-user','chatgpt_account_id':WORKSPACE}}
  token='header.'+base64.urlsafe_b64encode(json.dumps(claims).encode()).decode().rstrip('=')+'.signature'
  result,_,_,seen=self.call('http://127.0.0.1:3128',operation='usage',token=token,wire=struct.pack('!BI',2,2)+b'{}')
  self.assertFalse(result['protocol_success']);self.assertIsNone(result['eligibility']['allowed']);self.assertEqual(len(seen),1)
  claims['https://api.openai.com/auth']['chatgpt_user_id']=False
  token='header.'+base64.urlsafe_b64encode(json.dumps(claims).encode()).decode().rstrip('=')+'.signature'
  result,_,_,seen=self.call('http://127.0.0.1:3128',operation='usage',token=token)
  self.assertFalse(result['protocol_success']);self.assertEqual(len(seen),0)

class Server(socketserver.ThreadingTCPServer):
 allow_reuse_address=True
 daemon_threads=True

@contextlib.contextmanager
def socks_server(*,reject=False,require_auth=False,stalled=False,forward=None):
 observations=[];errors=[]
 class Handler(socketserver.BaseRequestHandler):
  def handle(self):
   remote=None
   try:
    self.request.settimeout(3);read=lambda n:probe._read(self.request,n,time.monotonic()+3)
    first=read(2);methods=read(first[1]);observations.append({'greeting':first+methods})
    if stalled:time.sleep(.5);return
    method=2 if require_auth else 0;self.request.sendall(bytes([5,method]))
    if require_auth:
     version,length=read(2);user=read(length);length=read(1)[0];password=read(length);observations[-1]['auth']=(user,password)
     self.request.sendall(b'\x01\x00' if (user,password)==(b'proxy-user',b'proxy-pass') else b'\x01\x01')
     if (user,password)!=(b'proxy-user',b'proxy-pass'):return
    header=read(4);kind=header[3];size=read(1)[0] if kind==3 else 4 if kind==1 else 16;host=read(size);port=struct.unpack('!H',read(2))[0];observations[-1].update(atyp=kind,target=host,port=port)
    if reject:self.request.sendall(b'\x05\x05\x00\x01'+b'\x00'*6);return
    self.request.sendall(b'\x05\x00\x00\x01'+b'\x00'*6)
    if forward is None:
     while True:
      data=self.request.recv(65536)
      if not data:return
      self.request.sendall(data)
    else:
     remote=socket.create_connection(forward,timeout=3)
     while True:
      ready,_,_=select.select([self.request,remote],[],[],3)
      if not ready:return
      for source in ready:
       data=source.recv(65536)
       if not data:return
       (remote if source is self.request else self.request).sendall(data)
   except Exception as error:errors.append(type(error).__name__)
   finally:
    if remote:remote.close()
 server=Server(('127.0.0.1',0),Handler);thread=threading.Thread(target=server.serve_forever,daemon=True);thread.start()
 try:yield server.server_address,observations,errors
 finally:server.shutdown();server.server_close();thread.join(timeout=3)

class SOCKSTunnelTests(unittest.TestCase):
 def bridge(self,address,**kwargs):
  url='socks5h://'+('proxy-user:proxy-pass@' if kwargs.pop('auth',False) else '')+'127.0.0.1:'+str(address[1]);record={};deadline=time.monotonic()+kwargs.pop('timeout',2)
  bridge=probe._SOCKSTunnel(probe._route(url),probe._target('https://chatgpt.com'),deadline,deadline+1,record);local=probe.urlsplit(bridge.start());conn=socket.create_connection((local.hostname,local.port),timeout=2)
  auth=base64.b64encode(('probe:'+probe.unquote(local.password)).encode())
  conn.sendall(b'CONNECT chatgpt.com:443 HTTP/1.1\r\nHost: chatgpt.com:443\r\nProxy-Authorization: Basic '+auth+b'\r\n\r\n')
  return bridge,conn,record,local
 def test_remoteDNS_exact_target_auth_transparent_bytes_single_connection(self):
  with socks_server(require_auth=True) as (address,seen,errors):
   b,c,r,local=self.bridge(address,auth=True)
   try:
    header=b''
    while not header.endswith(b'\r\n\r\n'):header+=c.recv(1)
    self.assertIn(b'200',header);payload=b'\x16\x03\x01synthetic-hello-unchanged';c.sendall(payload);self.assertEqual(probe._read(c,len(payload),time.monotonic()+2),payload)
    self.assertEqual(seen[0]['atyp'],3);self.assertEqual(seen[0]['target'],b'chatgpt.com');self.assertEqual(seen[0]['auth'],(b'proxy-user',b'proxy-pass'));self.assertEqual(r['socks_connect_attempts'],1)
    with self.assertRaises(OSError):socket.create_connection((local.hostname,local.port),timeout=.2)
   finally:c.close();self.assertTrue(b.close())
   self.assertFalse(r['target_tls_terminated_by_bridge']);self.assertFalse(errors,errors)
 def test_SOCKS_reject_returns_no_success_no_second_connection(self):
  with socks_server(reject=True) as (address,seen,_):
   b,c,r,_=self.bridge(address)
   try:self.assertEqual(c.recv(200),b'')
   finally:c.close();self.assertTrue(b.close())
   self.assertEqual(len(seen),1);self.assertEqual(r['bridge_error'],'socks_connect_rejected')
 def test_deadline_and_unused_listener_cleanup_are_bounded(self):
  with socks_server(stalled=True) as (address,_,_):
   b,c,r,_=self.bridge(address,timeout=.15);began=time.monotonic()
   try:self.assertEqual(c.recv(200),b'')
   finally:c.close();self.assertTrue(b.close())
   self.assertLess(time.monotonic()-began,1);self.assertEqual(r['bridge_error'],'proxy_bridge_timeout')
  record={};b=probe._SOCKSTunnel(probe._route('socks5h://127.0.0.1:1'),probe._target('https://chatgpt.com'),time.monotonic()+20,time.monotonic()+21,record);b.start();began=time.monotonic();self.assertTrue(b.close());self.assertLess(time.monotonic()-began,1)
 def test_wrong_local_auth_never_contacts_remote_proxy(self):
  record={};b=probe._SOCKSTunnel(probe._route('socks5h://127.0.0.1:1'),probe._target('https://chatgpt.com'),time.monotonic()+2,time.monotonic()+3,record);url=probe.urlsplit(b.start());c=socket.create_connection((url.hostname,url.port));c.sendall(b'CONNECT chatgpt.com:443 HTTP/1.1\r\nProxy-Authorization: Basic wrong\r\n\r\n')
  try:self.assertEqual(c.recv(10),b'')
  finally:c.close();self.assertTrue(b.close())
  self.assertEqual(record['proxy_connection_attempts'],0);self.assertEqual(record['bridge_error'],'proxy_connect_auth_invalid')

@contextlib.contextmanager
def trusted_tls_fixture():
 from http.server import BaseHTTPRequestHandler,ThreadingHTTPServer
 openssl=shutil.which('openssl')
 if not openssl:raise unittest.SkipTest('openssl needed for temporary fixture CA')
 with tempfile.TemporaryDirectory(prefix='route-tls-fixture-') as folder:
  root=pathlib.Path(folder);key=root/'key.pem';cert=root/'cert.pem';cakey=root/'ca-key.pem';ca=root/'ca.pem';csr=root/'leaf.csr';ext=root/'leaf.ext'
  commands=[['req','-x509','-newkey','rsa:2048','-nodes','-days','1','-subj','/CN=Synthetic route test CA','-addext','basicConstraints=critical,CA:TRUE','-addext','keyUsage=critical,keyCertSign,cRLSign','-keyout',str(cakey),'-out',str(ca)],['req','-new','-newkey','rsa:2048','-nodes','-subj','/CN=chatgpt.com','-keyout',str(key),'-out',str(csr)],['x509','-req','-in',str(csr),'-CA',str(ca),'-CAkey',str(cakey),'-CAcreateserial','-days','1','-extfile',str(ext),'-out',str(cert)]]
  ext.write_text('basicConstraints=critical,CA:FALSE\nsubjectAltName=DNS:chatgpt.com,DNS:localhost\nkeyUsage=critical,digitalSignature,keyEncipherment\nextendedKeyUsage=serverAuth\n')
  for cmd in commands:
   result=subprocess.run([openssl]+cmd,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,timeout=15)
   if result.returncode:raise AssertionError('synthetic certificate generation failed')
  seen=[];sni=[]
  class Handler(BaseHTTPRequestHandler):
   def respond(self,raw,content):
    self.send_response(200);self.send_header('Content-Type',content);self.send_header('Content-Length',str(len(raw)));self.end_headers();self.wfile.write(raw)
   def do_POST(self):
    body=self.rfile.read(int(self.headers['Content-Length']));seen.append({'method':'POST','path':self.path,'body':body,'headers':dict(self.headers)})
    event={'type':'response.completed','response':{'id':'synthetic-route-response','status':'completed','output':[{'type':'message','role':'assistant','content':[{'type':'output_text','text':'最终答案：21个'}]}]}}
    self.respond(b'data: '+json.dumps(event,ensure_ascii=False).encode()+b'\n\n','text/event-stream')
   def do_GET(self):
    seen.append({'method':'GET','path':self.path,'headers':dict(self.headers)})
    self.respond(json.dumps({'account_id':WORKSPACE,'user_id':'synthetic-user','rate_limit':{'allowed':True,'limit_reached':False}}).encode(),'application/json')
   def log_message(self,*args):pass
  server=ThreadingHTTPServer(('127.0.0.1',0),Handler);ctx=ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER);ctx.load_cert_chain(str(cert),str(key));ctx.set_servername_callback(lambda sock,name,context:sni.append(name));server.socket=ctx.wrap_socket(server.socket,server_side=True)
  worker=threading.Thread(target=server.serve_forever,daemon=True);worker.start()
  try:yield server.server_address,ca,seen,sni
  finally:server.shutdown();server.server_close();worker.join(timeout=3)

@contextlib.contextmanager
def http_connect_proxy(forward):
 seen=[]
 class Handler(socketserver.BaseRequestHandler):
  def handle(self):
   self.request.settimeout(5);data=b'';remote=None
   try:
    while not data.endswith(b'\r\n\r\n'):
     data+=probe._read(self.request,1,time.monotonic()+5)
     if len(data)>16384:raise AssertionError('oversized CONNECT')
    seen.append(data)
    if not data.startswith(b'CONNECT chatgpt.com:443 HTTP/1.1\r\n'):raise AssertionError('wrong target')
    remote=socket.create_connection(forward,timeout=5);self.request.sendall(b'HTTP/1.1 200 Connection Established\r\n\r\n')
    while True:
     ready,_,_=select.select([self.request,remote],[],[],5)
     if not ready:return
     for source in ready:
      value=source.recv(65536)
      if not value:return
      (remote if source is self.request else self.request).sendall(value)
   except OSError:pass
   finally:
    if remote:remote.close()
 server=Server(('127.0.0.1',0),Handler);worker=threading.Thread(target=server.serve_forever,daemon=True);worker.start()
 try:yield server.server_address,seen
 finally:server.shutdown();server.server_close();worker.join(timeout=3)

@unittest.skipUnless(os.name=='posix','actual native helper requires Linux')
class ActualNativeRouteTests(unittest.TestCase):
 def helper(self):
  name=os.environ.get('CODEX_NATIVE_HTTP_TEST_BIN')
  if not name or not pathlib.Path(name).is_file():self.skipTest('explicit native binary needed')
  expected=os.environ.get('CODEX_NATIVE_HTTP_TEST_SHA256')
  digest=probe.base.file_digest(pathlib.Path(name))
  if expected:self.assertEqual(digest,expected)
  return pathlib.Path(name),digest
 def test_actual_TLS_direct_HTTP_CONNECT_SOCKS5h_and_usage(self):
  helper,digest=self.helper();claims={'exp':time.time()+9000,'https://api.openai.com/auth':{'chatgpt_user_id':'synthetic-user','chatgpt_account_id':WORKSPACE}}
  token='header.'+base64.urlsafe_b64encode(json.dumps(claims).encode()).decode().rstrip('=')+'.signature'
  with trusted_tls_fixture() as (target,ca,seen,sni),http_connect_proxy(target) as (http,connects),socks_server(require_auth=True,forward=target) as (socks,observed,errors):
   original=probe.subprocess.Popen
   origin=probe.base._origin
   loopback='https://localhost:'+str(target[1])
   def allow_fixture_origin(value):
    if value!=loopback:origin(value)
   def start(*args,**kwargs):kwargs['env']=dict(kwargs['env'],CODEX_CA_CERTIFICATE=str(ca));return original(*args,**kwargs)
   # Only this fixture admits its trusted HTTPS loopback origin; the public
   # tool continues requiring the official ChatGPT HTTPS destination.
   with mock.patch.object(probe.subprocess,'Popen',side_effect=start),mock.patch.object(probe.base,'_origin',side_effect=allow_fixture_origin):
    for name,url,upstream in [('direct',None,'https://localhost:'+str(target[1])),('connect','http://127.0.0.1:'+str(http[1]),'https://chatgpt.com'),('socks','socks5h://proxy-user:proxy-pass@127.0.0.1:'+str(socks[1]),'https://chatgpt.com')]:
     with self.subTest(route=name):
      before=len(seen);report=probe.native_route_request(helper,digest,BODY,captured(),token,WORKSPACE,[token,WORKSPACE],timeout=20,proxy_url=url,upstream=upstream,application_identity=IDENTITY)
      self.assertTrue(report['protocol_success'],report);self.assertEqual(report['answer'],21);self.assertEqual(len(seen)-before,1);self.assertEqual(seen[-1]['method'],'POST');self.assertEqual(seen[-1]['body'],BODY)
      received={name.lower():value for name,value in seen[-1]['headers'].items()}
      self.assertEqual(received.get('authorization'),'Bearer '+token);self.assertIsNone(received.get('proxy-authorization'))
      for header_name,value in captured()['headers']:self.assertEqual(received.get(header_name.lower()),base64.b64decode(value).decode())
      self.assertTrue(report['helper_reaped'] and report['local_socket_absent']);self.assertFalse(report['helper_forced_kill']);self.assertEqual(report['actual_ipc_body_sha256'],probe.base.sha(BODY))
      if name=='socks':
       self.assertEqual(report['route']['connect_count'],1);self.assertTrue(report['route']['bridge_joined'] and report['route']['bridge_sockets_closed'] and report['route']['bridge_listener_closed']);self.assertTrue(report['route']['remote_dns_requested'])
      before=len(seen);u=probe.native_route_usage(helper,digest,token,WORKSPACE,timeout=20,proxy_url=url,upstream=upstream,application_identity=IDENTITY)
      self.assertTrue(u['protocol_success'],u);self.assertIs(u['eligibility']['allowed'],True);self.assertEqual(u['model_calls'],0);self.assertEqual(len(seen)-before,1);self.assertEqual((seen[-1]['method'],seen[-1]['path']),('GET','/backend-api/wham/usage'))
   self.assertEqual(len(connects),2);self.assertEqual(len(observed),2);self.assertTrue(all(r['atyp']==3 and r['target']==b'chatgpt.com' for r in observed));self.assertEqual(sni.count('localhost'),2);self.assertEqual(sni.count('chatgpt.com'),4)
 def test_actual_bad_SOCKS_auth_never_direct_fallback(self):
  helper,digest=self.helper()
  with trusted_tls_fixture() as (target,ca,seen,sni),socks_server(require_auth=True,forward=target) as (socks,observed,errors):
   original=probe.subprocess.Popen
   def start(*args,**kwargs):kwargs['env']=dict(kwargs['env'],CODEX_CA_CERTIFICATE=str(ca));return original(*args,**kwargs)
   with mock.patch.object(probe.subprocess,'Popen',side_effect=start):
    report=probe.native_route_request(helper,digest,BODY,captured(),TOKEN,WORKSPACE,timeout=10,proxy_url='socks5h://wrong:wrong@127.0.0.1:'+str(socks[1]),application_identity=IDENTITY)
   self.assertFalse(report['protocol_success']);self.assertEqual(report['route']['bridge_error'],'socks_auth_rejected');self.assertEqual(len(observed),1);self.assertEqual(seen,[]);self.assertEqual(sni,[]);self.assertFalse(report['direct_fallback']);self.assertTrue(report['route']['bridge_sockets_closed'] and report['route']['bridge_joined'])

if __name__=='__main__':unittest.main(verbosity=2)
