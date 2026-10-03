#!/usr/bin/env python3
"""Pinned official App Server Realtime capture inside a fresh offline netns."""
import argparse
import base64
from datetime import datetime, timezone
from email.parser import BytesParser
from email.policy import default as email_policy
import hashlib
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import queue
import signal
import struct
import subprocess
import sys
import threading
import time
from urllib.parse import urlsplit

PIN = "660e159a49e823ac8e5986cb238f73158ce4b957d40d9292f8de90862644b501"
BINARY = "/opt/codex-agent-integration/checks/quality-20260924/official-bundle/bin/codex"
MARKER = "SYNTHETIC_REALTIME_TEXT_MARKER"
CASES = ["websocket", "websocket-v1", "websocket-v3", "webrtc-api", "webrtc-backend",
         "webrtc-api-v3", "webrtc-backend-v3"]


def write(path, value):
    raw = value if isinstance(value, bytes) else json.dumps(value, ensure_ascii=False, indent=2).encode()
    path.write_bytes(raw)
    path.chmod(0o600)


def exact(reader, count):
    out = bytearray()
    while len(out) < count:
        raw = reader.read(count-len(out))
        if not raw:
            raise EOFError("fixture_peer_closed")
        out.extend(raw)
    return bytes(out)


class Fixture:
    def __init__(self, output, version):
        self.output = output
        self.version = version
        self.records = []
        self.lock = threading.Lock()
        self.text_seen = threading.Event()
        owner = self
        class Handler(BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"
            def log_message(self, *_): pass
            def reply(self, code, raw, content_type="application/json", headers=None):
                if not isinstance(raw, bytes):raw=json.dumps(raw).encode()
                self.send_response(code)
                self.send_header("Content-Type",content_type)
                self.send_header("Content-Length",str(len(raw)))
                for key,value in (headers or {}).items():self.send_header(key,value)
                self.end_headers()
                self.wfile.write(raw)
                self.wfile.flush()
            def do_CONNECT(self):
                owner.record(self,b"",blocked=True)
                self.reply(403,{"error":"offline_fixture_rejects_connect"})
            def do_POST(self):
                raw=exact(self.rfile,int(self.headers.get("Content-Length",0)))
                owner.record(self,raw)
                path=urlsplit(self.path).path
                if path.endswith("/realtime/calls") or path.endswith("/live"):
                    self.reply(201,b"v=answer\r\n","application/sdp",{"Location":path+"/rtc_fixture"})
                elif self.path.endswith("/responses"):
                    self.reply(200,b'data: {"type":"response.completed","response":{"id":"fixture-background","status":"completed","output":[]}}\n\n',"text/event-stream")
                else:self.reply(200,{})
            def do_GET(self):
                owner.record(self,b"")
                if self.headers.get("Upgrade","").lower()!="websocket":
                    self.reply(200,{"models":[]} if self.path.split("?")[0].endswith("/models") else {})
                    return
                accept=base64.b64encode(hashlib.sha1((self.headers["Sec-WebSocket-Key"]+"258EAFA5-E914-47DA-95CA-C5AB0DC85B11").encode()).digest()).decode()
                self.send_response(101)
                self.send_header("Upgrade","websocket")
                self.send_header("Connection","Upgrade")
                self.send_header("Sec-WebSocket-Accept",accept)
                self.end_headers()
                def send(value):
                    raw=json.dumps(value,separators=(",",":")).encode()
                    prefix=bytes([129,len(raw)]) if len(raw)<126 else bytes([129,126])+struct.pack("!H",len(raw))
                    self.wfile.write(prefix+raw)
                    self.wfile.flush()
                    owner.record(self,raw,kind="websocket_server_message")
                try:
                    if owner.version=="v3" and "rtc_fixture" in self.path:
                        send({"type":"session.started","session":{"id":"realtime_fixture"}})
                    while True:
                        head=exact(self.rfile,2)
                        opcode,length=head[0]&15,head[1]&127
                        if length in (126,127):length=int.from_bytes(exact(self.rfile,2 if length==126 else 8),"big")
                        mask=exact(self.rfile,4) if head[1]&128 else b""
                        payload=exact(self.rfile,length)
                        if mask:payload=bytes(value^mask[i%4] for i,value in enumerate(payload))
                        if opcode==8:
                            self.wfile.write(bytes([136,len(payload)])+payload);self.wfile.flush();break
                        if opcode==9:
                            self.wfile.write(bytes([138,len(payload)])+payload);self.wfile.flush();continue
                        if opcode!=1:continue
                        owner.record(self,payload,kind="websocket_client_message")
                        value=json.loads(payload)
                        if value.get("type")=="session.update":
                            send({"type":"session.updated","session":{"id":"realtime_fixture","instructions":"synthetic backend instructions"}})
                        if MARKER in payload.decode():
                            owner.text_seen.set()
                            if owner.version=="v3":
                                send({"type":"output_transcript.added","item":{"text":"SYNTHETIC_REALTIME_REPLY"}})
                                send({"type":"turn.done","turn":{"role":"assistant","transcript":"SYNTHETIC_REALTIME_REPLY"}})
                            else:
                                send({"type":"response.output_text.delta","item_id":"rt_item","delta":"SYNTHETIC_REALTIME_REPLY"})
                                send({"type":"response.output_text.done","item_id":"rt_item","text":"SYNTHETIC_REALTIME_REPLY"})
                except (OSError,EOFError,ValueError):
                    pass
                self.close_connection=True
        self.server=ThreadingHTTPServer(("127.0.0.1",0),Handler)
        self.thread=threading.Thread(target=self.server.serve_forever,daemon=True)
        self.thread.start()
    @property
    def url(self):return "http://127.0.0.1:"+str(self.server.server_port)
    def record(self,handler,raw,kind="http",blocked=False):
        try:body=json.loads(raw) if raw else None
        except ValueError:body=None
        headers={k:("<synthetic-auth-redacted>" if k.lower() in ("authorization","cookie") else v) for k,v in handler.headers.items()}
        row={"kind":kind,"method":handler.command,"path":handler.path,"headers":headers,
             "body":body,"body_base64":base64.b64encode(raw).decode() if body is None and raw else None,
             "body_sha256":hashlib.sha256(raw).hexdigest(),"body_bytes":len(raw),"blocked":blocked}
        with self.lock:
            self.records.append(row)
            write(self.output/"requests.json",self.records)
    def close(self):
        self.server.shutdown();self.server.server_close();self.thread.join(timeout=2)


def run_case(binary,root,case):
    output=root/case;output.mkdir(mode=0o700)
    home,work=output/"home",output/"work"
    home.mkdir(mode=0o700);work.mkdir(mode=0o700)
    is_websocket=case.startswith("websocket")
    version="v3" if case.endswith("-v3") else ("v2" if case=="websocket" else "v1")
    fixture=Fixture(output,version)
    callbase=fixture.url+("/backend-api/codex" if case.startswith("webrtc-backend") else "/v1")
    config=[
        'model = "mock-model"','model_provider = "fixture"','approval_policy = "never"','sandbox_mode = "read-only"',
        'web_search = "disabled"','cli_auth_credentials_store = "file"','project_doc_max_bytes = 0',
        'chatgpt_base_url = '+json.dumps(fixture.url),
        'experimental_realtime_ws_base_url = '+json.dumps(fixture.url+"/v1"),
        'experimental_realtime_webrtc_call_base_url = '+json.dumps(callbase),
        'experimental_realtime_ws_backend_prompt = "synthetic backend prompt"',
        'experimental_realtime_ws_startup_context = ""',
        'analytics.enabled = false','feedback.enabled = false','skills.include_instructions = false','skills.bundled.enabled = false',
        'features.skip_host_skill_discovery = true','features.plugins = false','features.remote_plugin = false',
        'features.apps = false','features.shell_tool = false','features.multi_agent = false','features.multi_agent_v2 = false',
        'features.hooks = false','features.enable_request_compression = false',
        '[realtime]','version = "v2"','type = "conversational"',
        '[model_providers.fixture]','name = "Fixture"','base_url = '+json.dumps(fixture.url+"/v1"),
        'wire_api = "responses"','env_key = "FIXTURE_API_KEY"','supports_websockets = false',
    ]
    write(home/"config.toml","\n".join(config).encode())
    env={k:v for k,v in os.environ.items() if k in ("PATH","LANG","LC_ALL","TZ")}
    env.update(CODEX_HOME=str(home),FIXTURE_API_KEY="sk-synthetic-not-real",HTTP_PROXY=fixture.url,HTTPS_PROXY=fixture.url,ALL_PROXY=fixture.url,
               NO_PROXY="127.0.0.1,localhost,::1",RUST_LOG="off")
    process=None;status="failed";failure=None;methods=[];sdp=False;reply=False
    started=time.monotonic()
    try:
        with (output/"stderr.log").open("wb") as stderr:
            process=subprocess.Popen([str(binary),"app-server"],cwd=work,env=env,stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=stderr,start_new_session=True)
            messages=queue.Queue()
            def reader():
                with (output/"events.jsonl").open("wb") as log:
                    for line in process.stdout:
                        log.write(line);log.flush()
                        messages.put(json.loads(line))
                messages.put(None)
            threading.Thread(target=reader,daemon=True).start()
            next_id=0;pending=[]
            def send(value):
                process.stdin.write(json.dumps(value).encode()+b"\n");process.stdin.flush()
            def receive():
                nonlocal sdp,reply
                item=messages.get(timeout=20)
                if item is None:raise RuntimeError("runtime_eof")
                if "method" in item:
                    methods.append(item["method"])
                    if item["method"]=="thread/realtime/sdp":sdp="v=answer" in json.dumps(item)
                    if item["method"]=="thread/realtime/error":
                        raise RuntimeError("realtime_error:"+json.dumps(item.get("params",{})))
                    if "SYNTHETIC_REALTIME_REPLY" in json.dumps(item):reply=True
                    if "id" in item:send({"id":item["id"],"error":{"code":-32000,"message":"synthetic client rejects unexpected request"}})
                return item
            def call(method,params):
                nonlocal next_id
                next_id+=1;send({"id":next_id,"method":method,"params":params})
                while True:
                    value=receive()
                    if value.get("id")==next_id and "method" not in value:
                        if "error" in value:raise RuntimeError("rpc_"+method.replace("/","_")+":"+json.dumps(value["error"]))
                        return value["result"]
                    pending.append(value)
            call("initialize",{"clientInfo":{"name":"realtime-loopback-fixture","version":"1"},"capabilities":{"experimentalApi":True}})
            send({"method":"initialized","params":{}})
            tid=call("thread/start",{"cwd":str(work),"model":"mock-model","modelProvider":"fixture","approvalPolicy":"never","sandbox":"read-only","ephemeral":True})["thread"]["id"]
            # The pinned official tests use Audio + V1 for WebRTC. V2 call
            # creation is rejected, and Text is only valid for V2. Audio here
            # selects the protocol shape; this fixture sends no audio bytes and
            # never opens an audio device.
            params={"threadId":tid,"outputModality":"text" if version=="v2" else "audio","version":version,"model":"gpt-realtime-1.5",
                    "includeStartupContext":False,"clientManagedHandoffs":True,"prompt":"synthetic backend prompt",
                    "realtimeStartInstructions":"","realtimeEndInstructions":"",
                    "transport":{"type":"websocket"} if is_websocket else {"type":"webrtc","sdp":"v=offer\r\n"}}
            call("thread/realtime/start",params)
            deadline=time.monotonic()+20
            while time.monotonic()<deadline:
                if "thread/realtime/started" in methods and (is_websocket or sdp):break
                receive()
            call("thread/realtime/appendText",{"threadId":tid,"text":MARKER,"role":"user"})
            deadline=time.monotonic()+20
            while time.monotonic()<deadline and not reply:receive()
            call("thread/realtime/stop",{"threadId":tid})
            status="completed" if reply and fixture.text_seen.is_set() and (is_websocket or sdp) else "incomplete"
    except Exception as error:
        failure=type(error).__name__+": "+str(error)
    finally:
        if process is not None and process.poll() is None:
            process.stdin.close()
            try:process.wait(timeout=3)
            except subprocess.TimeoutExpired:
                os.killpg(process.pid,signal.SIGKILL);process.wait(timeout=3)
        fixture.close()
    http=[r for r in fixture.records if r["kind"]=="http"]
    calls=[r for r in http if r["method"]=="POST" and urlsplit(r["path"]).path.endswith(("/realtime/calls","/live"))]
    sockets=[r for r in http if r["headers"].get("Upgrade","").lower()=="websocket"]
    def header(row,name):return next((v for k,v in row["headers"].items() if k.lower()==name.lower()),None)
    result={"case":case,"version":version,"status":status,"error":failure,"http_calls":[{"method":r["method"],"path":r["path"],"content_type":header(r,"Content-Type")} for r in calls],
            "websocket_upgrades":[{"path":r["path"],"subprotocol":r["headers"].get("Sec-WebSocket-Protocol")} for r in sockets],
            "client_text_observed":fixture.text_seen.is_set(),"reply_observed_by_official_rpc":reply,"sdp_answer_observed":sdp,
            "notification_methods":sorted(set(methods)),"blocked_connects":sum(r["blocked"] for r in fixture.records),
            "real_account":False,"real_model":False,"audio_device_used":False,"elapsed_ms":round((time.monotonic()-started)*1000)}
    write(output/"summary.json",result)
    return result


def execute(args):
    if not args.isolated_net or os.name!="posix" or os.readlink("/proc/self/ns/net")==os.readlink("/proc/1/ns/net"):
        raise RuntimeError("fresh_offline_network_namespace_required")
    subprocess.run(["ip","link","set","lo","up"],check=True,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
    links=json.loads(subprocess.check_output(["ip","-j","link","show"]))
    routes=json.loads(subprocess.check_output(["ip","-j","route","show","table","all"]))
    if any(link["ifname"]!="lo" for link in links) or any(route.get("dev")!="lo" for route in routes):
        raise RuntimeError("offline_namespace_has_non_loopback_network")
    binary=args.codex.resolve()
    pin=hashlib.sha256(binary.read_bytes()).hexdigest()
    if pin!=PIN:raise RuntimeError("official_binary_pin_mismatch")
    args.output.mkdir(mode=0o700,parents=True,exist_ok=False)
    outcomes=[run_case(binary,args.output,case) for case in args.case]
    result={"schema":"codex-realtime-loopback-v1","official_binary_sha256":pin,"official_version":subprocess.check_output([str(binary),"--version"],text=True).strip(),
            "network_namespace":"fresh_isolated","network_namespace_id":os.readlink("/proc/self/ns/net"),
            "host_network_namespace_id":os.readlink("/proc/1/ns/net"),"interfaces":links,"routes":routes,
            "only_loopback_available":True,"outcomes":outcomes}
    write(args.output/"summary.json",result)
    print(json.dumps(result),flush=True)
    return 0 if all(x["status"]=="completed" for x in outcomes) else 1


def export_oracle(local):
    """Export recorded bytes only; never manufacture missing fixture traffic."""
    summary=json.loads((local/"summary.json").read_text(encoding="utf-8"))
    run=json.loads((local/"run.json").read_text(encoding="utf-8"))
    cases=[]
    for outcome in summary.get("outcomes",[]):
        records_path=local/outcome["case"]/"requests.json"
        if outcome["status"]!="completed" or not records_path.exists():
            raise RuntimeError("cannot_export_incomplete_case:"+outcome["case"])
        records=json.loads(records_path.read_text(encoding="utf-8"))
        outgoing=[]
        for row in records:
            if row["kind"]=="websocket_server_message":continue
            if row["body_base64"] is not None:
                raw=base64.b64decode(row["body_base64"])
            elif row["body"] is not None:
                raw=json.dumps(row["body"],ensure_ascii=False,separators=(",",":")).encode()
            else:raw=b""
            if len(raw)!=row["body_bytes"] or hashlib.sha256(raw).hexdigest()!=row["body_sha256"]:
                raise RuntimeError("recorded_body_bytes_cannot_be_verified:"+outcome["case"])
            headers={k.lower():v for k,v in row["headers"].items()}
            for key in ("thread-id","session-id","x-session-id"):
                if key in headers:headers[key]="<THREAD_ID>"
            if "host" in headers:headers["host"]="<LOOPBACK_AUTHORITY>"
            if "sec-websocket-key" in headers:headers["sec-websocket-key"]="<RANDOM_WEBSOCKET_KEY>"
            item={"kind":row["kind"],"method":row["method"],"path":row["path"],"headers":headers,
                  "body_utf8":raw.decode(),"body":row["body"],"body_sha256":row["body_sha256"]}
            content_type=headers.get("content-type","")
            if content_type.startswith("multipart/"):
                message=BytesParser(policy=email_policy).parsebytes(("Content-Type: "+content_type+"\r\nMIME-Version: 1.0\r\n\r\n").encode()+raw)
                parts={}
                for part in message.iter_parts():
                    name=part.get_param("name",header="content-disposition")
                    payload=part.get_payload(decode=True).decode()
                    parts[name]=json.loads(payload) if part.get_content_type()=="application/json" else payload
                item["multipart_parts"]=parts
            outgoing.append(item)
        cases.append({"case":outcome["case"],"version":outcome.get("version"),
                      "source_requests_sha256":hashlib.sha256(records_path.read_bytes()).hexdigest(),
                      "client_text_observed":outcome["client_text_observed"],
                      "reply_observed_by_official_rpc":outcome["reply_observed_by_official_rpc"],
                      "sdp_answer_observed":outcome["sdp_answer_observed"],"outbound":outgoing})
    result={"schema":"codex-realtime-official-writer-oracle-v1","official_binary_sha256":summary["official_binary_sha256"],
            "official_version":summary["official_version"],"script_sha256":run["script_sha256"],
            "capture_id":local.name,"remote_output":run["remote_output"],"only_loopback_available":summary["only_loopback_available"],
            "auth_mode":"synthetic API key on custom provider; no real account", "real_model_called":False,
            "normalizations":["header names lowercased","thread/session IDs","loopback authority","random WebSocket key","synthetic Authorization redacted"],
            "body_bytes_normalized":False,"body_sha256_verified":True,"cases":cases}
    write(local/"writer-oracle.json",result)
    return result


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--remote",action="store_true")
    parser.add_argument("--isolated-net",action="store_true")
    parser.add_argument("--codex",type=Path,default=Path(BINARY))
    parser.add_argument("--output",type=Path)
    parser.add_argument("--export-run",type=Path,help="Export verified oracle from an existing downloaded capture; no network")
    parser.add_argument("--case",action="append",choices=CASES)
    args=parser.parse_args();args.case=args.case or CASES
    if args.export_run:
        result=export_oracle(args.export_run)
        print(json.dumps({"oracle":str(args.export_run/"writer-oracle.json"),"case_count":len(result["cases"]),"body_sha256_verified":True}))
        return 0
    if not args.remote:return execute(args)
    from integration_env import ROOT,connect
    import shlex
    stamp=datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%S.%fZ")
    code_sha=hashlib.sha256(Path(__file__).read_bytes()).hexdigest()
    remote_script="/opt/codex-agent-integration/appserver-parity/artifacts/realtime-"+code_sha+".py"
    remote_output="/opt/codex-agent-integration/appserver-parity/realtime/"+stamp
    local=ROOT/".local/appserver-parity/realtime"/stamp;local.mkdir(parents=True)
    client=connect()
    try:
        with client.open_sftp() as s:
            s.put(__file__,remote_script);s.chmod(remote_script,0o600)
        command=["unshare","--net","--","python3","-B",remote_script,"--isolated-net","--output",remote_output]
        for case in args.case:command+=["--case",case]
        _,out,err=client.exec_command(shlex.join(command),timeout=240)
        raw=out.read();code=out.channel.recv_exit_status();error=err.read()
        result=json.loads(raw) if raw else {"failed":True,"stderr_bytes":len(error)}
        write(local/"summary.json",result)
        write(local/"run.json",{"remote_output":remote_output,"script_sha256":code_sha,"exit_code":code})
        with client.open_sftp() as s:
            for case in args.case:
                case_output=local/case;case_output.mkdir()
                for name in ("requests.json","summary.json"):
                    try:s.get(remote_output+"/"+case+"/"+name,str(case_output/name))
                    except FileNotFoundError:pass
        if code==0:export_oracle(local)
        print(json.dumps(result),flush=True)
        print(json.dumps({"local":str(local),"remote":remote_output,"exit_code":code,"stderr_bytes":len(error)}),flush=True)
        return code
    finally:client.close()


if __name__=="__main__":raise SystemExit(main())
