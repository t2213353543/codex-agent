"""Validate candidate UI against the running backend through a read-only SSH proxy."""
import argparse
from functools import partial
import hashlib
import http.client
import gzip
from http.server import SimpleHTTPRequestHandler, ThreadingHTTPServer
import json
from pathlib import Path
import select
import shlex
import socketserver
import subprocess
import threading
from urllib.parse import urlsplit

from integration_env import ROOT, connect, run


def main():
    import ui_release
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--dist', default=str(ROOT / 'frontend/dist'))
    parser.add_argument('--output', default=str(ROOT / '.local/console-ui-20260924/live-preflight.json'))
    parser.add_argument('--expected-backend-sha', default=ui_release.remote.BINARY_SHA)
    parser.add_argument('--deployed', action='store_true')
    args = parser.parse_args()
    dist = Path(args.dist).resolve(strict=True)
    expected_files = ui_release.dist_map(dist)
    binding = {'frontend_source_sha256': ui_release.frontend_source()['sha256'],
               'backend_source_sha256': ui_release.backend_source()['sha256'],
               'dist_sha256': ui_release.remote.object_sha(expected_files)}
    client, forward, server = None, None, None
    allowed_posts = ['/api/session', '/api/logs/snapshot', '/api/accounts/last-used', '/api/accounts/usage-batch', '/api/accounts/upstream/batch']
    result = {'passed': False, 'synthetic_data': False, 'data_mode': 'production-ui-validation',
              'business_writes': 0, 'gateway_requests': 0, 'session_only_mutations': False,
              'allowed_post_paths': allowed_posts, 'ephemeral_snapshot_writes': 0, **binding}
    allowed_requests, rejected_requests = [], []
    static_cache = {}
    try:
        client = connect()
        script = """
import hashlib,json,pathlib,subprocess
pid=subprocess.check_output(['systemctl','show','codex-agent.service','-p','MainPID','--value'],text=True).strip()
p=pathlib.Path('/proc')/pid
args=(p/'cmdline').read_bytes().decode().strip('\\0').split('\\0')
print(json.dumps({'sha256':hashlib.sha256((p/'exe').read_bytes()).hexdigest(),'pid':int(pid),'web':args[args.index('--web')+1]}))
"""
        runtime = json.loads(run(client, 'python3 -c ' + shlex.quote(script)))
        assert runtime['sha256'] == args.expected_backend_sha, 'Backend differs from expected verified release'
        result.update(backend_binary_sha256=runtime['sha256'], runtime_pid=runtime['pid'])
        with client.open_sftp() as sftp:
            with sftp.file('/var/lib/codex-agent/client-key', 'rb') as handle:
                admin_key = handle.read().decode().strip()
        transport = client.get_transport()
        transport.set_keepalive(15)

        class Tunnel(socketserver.BaseRequestHandler):
            def handle(self):
                channel = transport.open_channel('direct-tcpip', ('127.0.0.1', 18879), self.request.getpeername())
                try:
                    while True:
                        ready, _, _ = select.select([self.request, channel], [], [], 5)
                        for source, destination in ((self.request, channel), (channel, self.request)):
                            if source in ready:
                                data = source.recv(65536)
                                if not data:
                                    return
                                destination.sendall(data)
                finally:
                    channel.close()

        class Forward(socketserver.ThreadingTCPServer):
            allow_reuse_address = True
            daemon_threads = True
        forward = Forward(('127.0.0.1', 0), Tunnel)
        threading.Thread(target=forward.serve_forever, daemon=True).start()
        tunnel_port = forward.server_address[1]

        class Candidate(SimpleHTTPRequestHandler):
            def log_message(self, *args):
                pass

            def do_GET(self):
                request_path = urlsplit(self.path).path
                if args.deployed and (request_path in ('/', '/favicon.svg') or request_path.startswith('/assets/')):
                    self.public_asset(request_path)
                    return
                if args.deployed or urlsplit(self.path).path.startswith(('/api/', '/health')):
                    self.proxy()
                else:
                    super().do_GET()

            def do_POST(self):
                self.proxy()

            def do_DELETE(self):
                self.proxy()

            def do_PUT(self):
                self.proxy()

            def do_PATCH(self):
                self.proxy()

            def public_asset(self, request_path):
                # Freeze bytes fetched from the public live server for this
                # short verification run; every cached byte is SHA-checked.
                if request_path not in static_cache:
                    connection = http.client.HTTPConnection('43.225.196.170', 18879, timeout=60)
                    try:
                        connection.request('GET', request_path, headers={'Cache-Control': 'no-cache'})
                        response = connection.getresponse()
                        payload = response.read()
                        content_type = response.getheader('Content-Type', 'application/octet-stream')
                        static_cache[request_path] = (response.status, content_type, gzip.compress(payload, compresslevel=1))
                    finally:
                        connection.close()
                status, content_type, payload = static_cache[request_path]
                try:
                    self.send_response(status)
                    self.send_header('Content-Type', content_type)
                    self.send_header('Content-Encoding', 'gzip')
                    self.send_header('Content-Length', str(len(payload)))
                    self.end_headers()
                    self.wfile.write(payload)
                except (BrokenPipeError, ConnectionResetError, ConnectionAbortedError):
                    pass

            def proxy(self):
                request_path = urlsplit(self.path).path
                read = self.command in ('GET', 'HEAD')
                api_read = request_path.startswith(('/api/', '/health'))
                public_read = request_path == '/' or request_path == '/favicon.svg' or request_path.startswith('/assets/')
                allowed_write = (self.command == 'POST' and request_path in allowed_posts) or (self.command == 'DELETE' and request_path == '/api/session')
                if not ((read and (api_read or (args.deployed and public_read))) or allowed_write):
                    rejected_requests.append({'method': self.command, 'path': request_path})
                    self.send_error(405, 'Preview allows only management reads and session creation/removal')
                    return
                body = self.rfile.read(int(self.headers.get('Content-Length', '0'))) if not read else None
                headers = {k: v for k, v in self.headers.items() if k.lower() not in ('connection', 'accept-encoding', 'transfer-encoding')}
                # Public immutable assets can use the already verified public
                # endpoint; session and management traffic stay inside SSH.
                connection = (http.client.HTTPConnection('43.225.196.170', 18879, timeout=30)
                              if args.deployed and public_read
                              else http.client.HTTPConnection('127.0.0.1', tunnel_port, timeout=30))
                try:
                    connection.request(self.command, self.path, body=body, headers=headers)
                    response = connection.getresponse()
                    payload = response.read()
                    if args.deployed and public_read and request_path.endswith(('.js', '.css', '.html')):
                        payload = gzip.compress(payload, compresslevel=1)
                        gzip_payload = True
                    else:
                        gzip_payload = False
                    allowed_requests.append({'method': self.command, 'path': request_path, 'status': response.status})
                    self.send_response(response.status)
                    for key, value in response.getheaders():
                        if key.lower() not in ('connection', 'transfer-encoding', 'content-length', 'content-encoding'):
                            self.send_header(key, value)
                    if gzip_payload:
                        self.send_header('Content-Encoding', 'gzip')
                    self.send_header('Content-Length', str(len(payload)))
                    self.end_headers()
                    self.wfile.write(payload)
                except (BrokenPipeError, ConnectionResetError, ConnectionAbortedError):
                    pass
                finally:
                    connection.close()

        server = ThreadingHTTPServer(('127.0.0.1', 0), partial(Candidate, directory=str(dist)))
        threading.Thread(target=server.serve_forever, daemon=True).start()
        config = {'base_url': 'http://127.0.0.1:' + str(server.server_address[1]),
                  'admin_key': admin_key, 'expected_files': expected_files, 'deployed': args.deployed}
        process = subprocess.run(['node', str(ROOT / 'frontend/tests/console-live-readonly.mjs')],
                                 input=json.dumps(config), capture_output=True, text=True, encoding='utf-8', timeout=480, cwd=ROOT,
                                 creationflags=getattr(subprocess, 'CREATE_NO_WINDOW', 0))
        try:
            browser_result = json.loads(process.stdout)
        except json.JSONDecodeError:
            raise RuntimeError('Browser check did not return a structured result') from None
        result.update(browser=browser_result, requests=allowed_requests, rejected_requests=rejected_requests)
        result['ephemeral_snapshot_writes'] = sum(r['method'] == 'POST' and r['path'] == '/api/logs/snapshot' for r in allowed_requests)
        result['passed'] = process.returncode == 0 and browser_result.get('passed') is True and not rejected_requests
        assert ui_release.dist_map(dist) == expected_files, 'Candidate changed during check'
        assert ui_release.frontend_source()['sha256'] == binding['frontend_source_sha256'], 'Frontend source changed during check'
    except Exception as error:
        result['passed'] = False
        result['failure'] = str(error)
    finally:
        if server:
            server.shutdown()
            server.server_close()
        if forward:
            forward.shutdown()
            forward.server_close()
        if client:
            client.close()
        result['temporary_credentials_written'] = False
        result['ssh_forward_closed'] = True
        out = Path(args.output)
        out.parent.mkdir(parents=True, exist_ok=True)
        out.write_text(json.dumps(result, indent=2, ensure_ascii=False) + '\n', encoding='utf-8')
    print(json.dumps({'passed': result['passed'], 'checks': len(result.get('browser', {}).get('checks', [])),
                      'business_writes': 0, 'gateway_requests': 0, 'report': str(out)}, ensure_ascii=False))
    raise SystemExit(0 if result['passed'] else 1)


if __name__ == '__main__':
    main()
