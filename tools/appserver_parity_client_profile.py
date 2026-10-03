#!/usr/bin/env python3
"""Official Codex 0.155 delegated PAT profile and credential-free loopback proof.

The profile uses the supported personalAccessToken auth mode. Its credential
is a gateway key; the configured gateway must implement the official whoami
metadata shape and bind that key to one upstream owner. No JWT is fabricated.
"""
import argparse
import base64
from datetime import datetime, timezone
import hashlib
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import queue
import shlex
import signal
import subprocess
import sys
import threading
import time
from urllib.parse import urlsplit

PIN = "660e159a49e823ac8e5986cb238f73158ce4b957d40d9292f8de90862644b501"
BINARY = "/opt/codex-agent-integration/checks/quality-20260924/official-bundle/bin/codex"
KEY = "sk-synthetic-gateway-key-not-an-upstream-token"
PNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR4nGP4z8DwHwAFAAH/iZk9HQAAAABJRU5ErkJggg=="
METADATA = {"email": "fixture@example.invalid", "chatgpt_user_id": "fixture-user",
            "chatgpt_account_id": "fixture-account", "chatgpt_plan_type": "pro",
            "chatgpt_account_is_fedramp": False}


def write(path, value):
    path.write_bytes(value if isinstance(value, bytes) else (json.dumps(value, indent=2)+"\n").encode())
    if os.name == "posix":
        path.chmod(0o600)


def profile(base, model="gpt-6-astra", fixture=False):
    url = urlsplit(base)
    if url.scheme not in {"http", "https"} or not url.netloc or url.username or url.password or url.path not in {"", "/"} or url.query or url.fragment:
        raise ValueError("gateway origin must be an absolute HTTP(S) origin without a path")
    base = base.rstrip("/")
    values = ["model = "+json.dumps(model), 'model_provider = "gateway-appserver"',
              'cli_auth_credentials_store = "file"', 'chatgpt_base_url = '+json.dumps(base+"/backend-api"),
              'features.apps = true', 'features.image_generation = true',
              'features.token_budget.enabled = true', 'features.token_budget.use_history_notes_extension = true']
    if fixture:
        values += ['approval_policy = "never"', 'sandbox_mode = "danger-full-access"', 'web_search = "disabled"',
                   'project_doc_max_bytes = 0', 'analytics.enabled = false', 'feedback.enabled = false',
                   'skills.include_instructions = false', 'skills.bundled.enabled = false',
                   'features.skip_host_skill_discovery = true', 'features.plugins = false', 'features.remote_plugin = false',
                   'features.shell_tool = false', 'features.multi_agent = false', 'features.multi_agent_v2 = false',
                   'features.hooks = false', 'features.enable_request_compression = false']
    values += ['[model_providers.gateway-appserver]', 'name = "OpenAI"',
               'base_url = '+json.dumps(base+"/backend-api/codex"), 'wire_api = "responses"',
               'requires_openai_auth = true', 'supports_websockets = '+("false" if fixture else "true"),
               'supports_standalone_web_search = true']
    if fixture:
        values += ['request_max_retries = 0', 'stream_max_retries = 0', 'stream_idle_timeout_ms = 10000']
    return "\n".join(values)+"\n"


def sse(items, number):
    rid = "resp-client-profile-"+str(number)
    events = [{"type": "response.created", "response": {"id": rid}}]
    events += [{"type": "response.output_item.done", "output_index": i, "item": item} for i, item in enumerate(items)]
    events += [{"type": "response.completed", "response": {"id": rid, "status": "completed", "output": [],
               "usage": {"input_tokens": 1, "output_tokens": 1, "total_tokens": 2}}}]
    return b"".join(b"data: "+json.dumps(event).encode()+b"\n\n" for event in events)


class Fixture:
    def __init__(self, output):
        self.output, self.rows, self.lock, self.calls = output, [], threading.Lock(), 0
        owner = self
        class Handler(BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"
            def log_message(self, *args): pass
            def reply(self, status, value, content_type="application/json"):
                raw = value if isinstance(value, bytes) else json.dumps(value).encode()
                self.send_response(status)
                self.send_header("Content-Type", content_type)
                self.send_header("Content-Length", str(len(raw)))
                self.send_header("Connection", "close")
                self.end_headers()
                self.wfile.write(raw)
                self.close_connection = True
            def do_CONNECT(self):
                owner.record(self, b"", True)
                self.reply(502, {"error": "external_transport_disabled"})
            def do_GET(self):
                owner.record(self, b"")
                path = self.path.split("?", 1)[0]
                if path == "/v1/user-auth-credential/whoami":
                    self.reply(200 if self.headers.get("Authorization") == "Bearer "+KEY else 401, METADATA)
                elif path.endswith("/models"): self.reply(200, {"models": []})
                elif "/connectors/directory/list" in path: self.reply(200, {"apps": [], "next_token": None})
                elif path.endswith("/ps/mcp"): self.reply(405, {"error":"no_sse_stream"})
                else: self.reply(200, {})
            def do_DELETE(self):
                owner.record(self, b""); self.reply(200, {})
            def do_POST(self):
                raw = self.rfile.read(int(self.headers.get("Content-Length", 0)))
                owner.record(self, raw)
                path = self.path.split("?", 1)[0]
                if path.endswith("/ps/mcp"):
                    body = json.loads(raw)
                    if "id" not in body: self.reply(202, b""); return
                    method = body.get("method")
                    value = {}
                    if method == "initialize": value = {"protocolVersion": body.get("params", {}).get("protocolVersion", "2024-11-05"), "capabilities":{"tools":{}}, "serverInfo":{"name":"synthetic-codex-apps","version":"1"}}
                    elif method == "tools/list": value = {"tools": [{"name":"echo_marker","description":"Return a synthetic marker for gateway auth proof.","inputSchema":{"type":"object","properties":{}},"annotations":{"readOnlyHint":True,"openWorldHint":False},"_meta":{"connector_id":"connector_profile_fixture","connector_name":"profile_fixture","connector_description":"Synthetic gateway profile fixture"}}]}
                    elif method == "tools/call": value = {"content":[{"type":"text","text":"SYNTHETIC_APP_TOOL_RESULT"}],"isError":False}
                    self.reply(200, {"jsonrpc":"2.0","id":body["id"],"result":value}); return
                if path.endswith("/images/generations"):
                    self.reply(200, {"created":1,"background":"opaque","data":[{"b64_json":PNG,"generation_id":"fixture-image"}]}); return
                if path.endswith("/alpha/notes/v2/thread_hint"):
                    self.reply(200,{"text":""});return
                if path.endswith("/alpha/history/v2/list_windows"):
                    self.reply(200,{"windows":[],"fixture":"SYNTHETIC_HISTORY_RESULT"});return
                if path.endswith("/responses"):
                    owner.calls += 1
                    if owner.calls == 1:
                        items = [{"type":"function_call","id":"fc-profile","call_id":"call-profile","namespace":"image_gen","name":"imagegen","arguments":json.dumps({"prompt":"SYNTHETIC_CLIENT_PROFILE_IMAGE"})}]
                    elif owner.calls == 2:
                        items=[{"type":"function_call","id":"fc-app","call_id":"call-app","namespace":"mcp__codex_apps__profile_fixture","name":"echo_marker","arguments":"{}"}]
                    elif owner.calls == 3:
                        items=[{"type":"function_call","id":"fc-history","call_id":"call-history","namespace":"history","name":"list_windows","arguments":"{}"}]
                    else:
                        items = [{"type":"message","id":"msg-profile","role":"assistant","content":[{"type":"output_text","text":"CLIENT_PROFILE_DONE"}]}]
                    self.reply(200, sse(items, owner.calls), "text/event-stream"); return
                self.reply(200, {})
        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()

    @property
    def url(self): return "http://127.0.0.1:"+str(self.server.server_port)
    def record(self, request, raw, blocked=False):
        try: body = json.loads(raw) if raw else None
        except ValueError: body = None
        headers = {k.lower(): v for k,v in request.headers.items()}
        row = {"method":request.command,"path":request.path,"headers":headers,"body":body,
               "body_sha256":hashlib.sha256(raw).hexdigest(),"blocked":blocked}
        with self.lock:
            self.rows.append(row); write(self.output/"requests.json", self.rows)
    def close(self):
        self.server.shutdown(); self.server.server_close(); self.thread.join(timeout=2)


def run_case(binary, output, route, external_gateway=None):
    output.mkdir(mode=0o700)
    home, work = output/"client-home", output/"work"
    home.mkdir(mode=0o700); work.mkdir(mode=0o700)
    fixture = None if external_gateway else Fixture(output)
    base = external_gateway or fixture.url
    key = "test-client-key" if external_gateway else KEY
    write(home/"config.toml", profile(base, "mock-model", True).encode())
    write(home/"auth.json", {"auth_mode":"personalAccessToken","personal_access_token":key})
    env = {k:v for k,v in os.environ.items() if k in ["PATH","LANG","LC_ALL","TZ"]}
    proxy = fixture.url if fixture else "http://127.0.0.1:1"
    env.update(CODEX_HOME=str(home), CODEX_AUTHAPI_BASE_URL=base, CODEX_CONNECTORS_TOKEN=key, RUST_LOG="off",
               HTTP_PROXY=proxy, HTTPS_PROXY=proxy, ALL_PROXY=proxy, NO_PROXY="127.0.0.1,localhost,::1")
    write(output/"environment.json", {k:("<synthetic-gateway-key>" if k=="CODEX_CONNECTORS_TOKEN" else v) for k,v in env.items() if k.startswith("CODEX_")})
    process, result = None, {"route":route,"completed":False,"account":None,"rpc_results":{}}
    try:
        command = [str(binary), "app-server"] if route == "app" else [str(binary),"exec","--json","--skip-git-repo-check","-"]
        with (output/"stderr.log").open("wb") as errors:
            process = subprocess.Popen(command, cwd=work, env=env, stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=errors, start_new_session=True)
            if route == "exec":
                raw, _ = process.communicate(b"Generate the synthetic fixture image. Use [$Profile Fixture](app://connector_profile_fixture) to echo the synthetic marker, then list history windows.", timeout=60)
                write(output/"events.jsonl",raw)
                result["completed"] = process.returncode == 0
                result["process_returncode"] = process.returncode
            else:
                messages, pending = queue.Queue(), []
                def reader():
                    with (output/"events.jsonl").open("wb") as log:
                        for raw in process.stdout:
                            log.write(raw); log.flush(); messages.put(json.loads(raw))
                        messages.put(None)
                threading.Thread(target=reader,daemon=True).start()
                counter = 0
                def send(value): process.stdin.write(json.dumps(value).encode()+b"\n"); process.stdin.flush()
                def receive():
                    value = messages.get(timeout=35)
                    if value is None: raise RuntimeError("official_process_eof")
                    if "method" in value and "id" in value:
                        send({"id":value["id"],"error":{"code":-32000,"message":"synthetic fixture does not support callback"}})
                    return value
                def call(method, params):
                    nonlocal counter
                    counter += 1; send({"id":counter,"method":method,"params":params})
                    while True:
                        value = receive()
                        if value.get("id") == counter and "method" not in value:
                            result["rpc_results"][method] = value
                            if "error" in value: raise RuntimeError("rpc_"+method+"_failed")
                            return value["result"]
                        pending.append(value)
                call("initialize",{"clientInfo":{"name":"gateway-profile-fixture","version":"1"},"capabilities":{"experimentalApi":True}})
                send({"method":"initialized","params":{}})
                result["account"] = call("account/read",{})
                call("model/list",{})
                call("app/list",{"forceRefetch":True})
                tid = call("thread/start",{"cwd":str(work),"model":"mock-model","modelProvider":"gateway-appserver","approvalPolicy":"never","sandbox":"danger-full-access"})["thread"]["id"]
                call("turn/start",{"threadId":tid,"input":[{"type":"text","text":"Generate the synthetic fixture image. Use [$Profile Fixture](app://connector_profile_fixture) to echo the synthetic marker, then list history windows."}]})
                deadline=time.monotonic()+50
                while time.monotonic()<deadline:
                    value=pending.pop(0) if pending else receive()
                    if value.get("method")=="turn/completed":
                        result["completed"]=value.get("params",{}).get("turn",{}).get("status")=="completed";break
                call("thread/read",{"threadId":tid,"includeTurns":True})
    except Exception as exc: result["error"] = type(exc).__name__+": "+str(exc)
    finally:
        if process is not None and process.poll() is None:
            process.stdin.close()
            try:process.wait(timeout=3)
            except subprocess.TimeoutExpired:os.killpg(process.pid,signal.SIGKILL);process.wait(timeout=3)
        if fixture: fixture.close()
        (home/"auth.json").unlink(missing_ok=True)
    if external_gateway:
        result.update(passed=result["completed"],external_gateway=True,all_upstream_assertions_owned_by_go_integration_test=True)
        write(output/"summary.json",result)
        return result
    rows=fixture.rows
    result.update(request_count=len(rows), whoami_count=sum("/whoami" in r["path"] for r in rows),
                  image_requests=[r["path"] for r in rows if "/images/" in r["path"]],
                  app_requests=[r["path"] for r in rows if "/ps/mcp" in r["path"] or "/connectors/" in r["path"]],
                  app_tool_calls=sum(isinstance(r["body"],dict) and r["body"].get("method")=="tools/call" for r in rows),
                  history_requests=[r["path"] for r in rows if "/alpha/history/" in r["path"]],
                  notes_requests=[r["path"] for r in rows if "/alpha/notes/" in r["path"]],
                  remote_connects_blocked=sum(r["blocked"] for r in rows),
                  authentication_values=sorted(set(r["headers"].get("authorization","") for r in rows)),
                  account_header_values=sorted(set(r["headers"].get("chatgpt-account-id","") for r in rows)),
                  real_credentials_loaded=False, real_upstream=False, synthetic_jwt_used=False)
    result["passed"] = result["completed"] and result["whoami_count"]>0 and bool(result["image_requests"]) and result["app_tool_calls"]>0 and bool(result["history_requests"]) and result["authentication_values"]==["Bearer "+KEY]
    write(output/"summary.json",result)
    return result


def execute(args):
    if os.name != "posix" or os.readlink("/proc/self/ns/net")==os.readlink("/proc/1/ns/net"):
        raise RuntimeError("fresh_network_namespace_required")
    subprocess.run(["ip","link","set","lo","up"],check=True,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
    binary=args.codex.resolve(); assert hashlib.sha256(binary.read_bytes()).hexdigest()==PIN
    args.output.mkdir(parents=True,mode=0o700,exist_ok=False)
    if args.external_gateway:
        parsed=urlsplit(args.external_gateway)
        if parsed.scheme!="http" or parsed.hostname!="127.0.0.1" or parsed.path or parsed.query or parsed.fragment or parsed.username:
            raise ValueError("external fixture gateway must be a loopback HTTP origin")
    results=[run_case(binary,args.output/route,route,args.external_gateway) for route in args.route]
    result={"official_binary_sha256":PIN,"network_namespace":"isolated","results":results,"passed":all(r["passed"] for r in results)}
    write(args.output/"summary.json",result);print(json.dumps(result),flush=True)
    return 0 if result["passed"] else 1


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--remote",action="store_true");parser.add_argument("--isolated-net",action="store_true")
    parser.add_argument("--codex",type=Path,default=Path(BINARY));parser.add_argument("--output",type=Path)
    parser.add_argument("--route",action="append",choices=["app","exec"])
    parser.add_argument("--external-gateway",help=argparse.SUPPRESS)
    parser.add_argument("--write-profile",type=Path);parser.add_argument("--gateway-origin");parser.add_argument("--gateway-key-env",default="CODEX_GATEWAY_KEY")
    parser.add_argument("--run-profile",type=Path);parser.add_argument("client_args",nargs=argparse.REMAINDER)
    parser.add_argument("--model",default="gpt-6-astra")
    args=parser.parse_args();args.route=args.route or ["app","exec"]
    if args.run_profile:
        client_home=args.run_profile.resolve()
        auth=json.loads((client_home/"auth.json").read_bytes())
        if auth.get("auth_mode")!="personalAccessToken" or not auth.get("personal_access_token"):
            raise ValueError("profile does not contain delegated gateway credentials")
        values=json.loads((client_home/"gateway-environment.json").read_bytes())
        env={**os.environ,"CODEX_HOME":str(client_home),"CODEX_AUTHAPI_BASE_URL":values["CODEX_AUTHAPI_BASE_URL"],"CODEX_CONNECTORS_TOKEN":auth["personal_access_token"]}
        # Do not let a preexisting API-key environment silently select a
        # different auth mode from this explicit delegated backend profile.
        for name in ["OPENAI_API_KEY","CODEX_API_KEY","OPENAI_BASE_URL"]:env.pop(name,None)
        forwarded=args.client_args[1:] if args.client_args[:1]==["--"] else args.client_args
        return subprocess.call([str(args.codex),*forwarded],env=env)
    if args.write_profile:
        if not args.gateway_origin:parser.error("--gateway-origin is required")
        key=os.environ.get(args.gateway_key_env,"").strip()
        if not key:parser.error("gateway key environment variable is empty")
        config=profile(args.gateway_origin,args.model)
        args.write_profile.mkdir(mode=0o700,parents=True,exist_ok=False)
        write(args.write_profile/"config.toml",config.encode());write(args.write_profile/"auth.json",{"auth_mode":"personalAccessToken","personal_access_token":key})
        write(args.write_profile/"gateway-environment.json",{"CODEX_AUTHAPI_BASE_URL":args.gateway_origin.rstrip("/")})
        print(json.dumps({"client_home":str(args.write_profile.resolve()),"authapi_base_url":args.gateway_origin.rstrip("/"),"upstream_credentials_written":False}))
        return 0
    if not args.remote:
        if not args.output:parser.error("--output is required")
        return execute(args)
    from integration_env import ROOT,connect
    stamp=datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%S.%fZ")
    digest=hashlib.sha256(Path(__file__).read_bytes()).hexdigest()
    remote="/opt/codex-agent-integration/appserver-parity/client-profile/"+stamp
    script="/opt/codex-agent-integration/appserver-parity/artifacts/client-profile-"+digest+".py"
    output=ROOT/".local/appserver-parity/client-profile"/stamp;output.mkdir(parents=True)
    client=connect()
    try:
        with client.open_sftp() as s:s.put(__file__,script);s.chmod(script,0o600)
        command=["unshare","--net","--","python3","-B",script,"--isolated-net","--output",remote]
        for route in args.route:command += ["--route",route]
        _,stdout,stderr=client.exec_command(shlex.join(command),timeout=180)
        raw=stdout.read();error=stderr.read();code=stdout.channel.recv_exit_status()
        write(output/"stdout.json",raw);write(output/"stderr.log",error)
        write(output/"run.json",{"remote":remote,"script_sha256":digest,"exit_code":code})
        with client.open_sftp() as s:
            for route in args.route:
                (output/route).mkdir()
                for name in ["summary.json","requests.json","events.jsonl","stderr.log","environment.json"]:
                    try:s.get(remote+"/"+route+"/"+name,str(output/route/name))
                    except FileNotFoundError:pass
        result=json.loads(raw) if raw else {"passed":False}
        print(json.dumps({"local":str(output),"remote":remote,"exit_code":code,"passed":result.get("passed"),"results":[{k:v for k,v in r.items() if k!="rpc_results"} for r in result.get("results",[])]}),flush=True)
        return code
    finally:client.close()


if __name__=="__main__":raise SystemExit(main())
