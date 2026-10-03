"""Bounded loopback Chat Completions streaming probe for an isolated App Server.

This is an experiment adapter for one user text message, not a general gateway.
Authentication remains inside the already authenticated App Server process.
"""


def bridge_turn(rpc, workspace, case, alias, repetition, app_turn, MODEL, EFFORT, emit):
    import hashlib
    from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
    import json
    import threading
    import time
    import urllib.request
    import uuid

    started = time.monotonic()
    deadline = started + (660 if EFFORT == 'max' else 480)
    state = {'record': None, 'handler_error': None, 'delta_count': 0,
             'first_delta_seconds': None, 'upstream_completed_seconds': None}
    handler_done = threading.Event()
    request_taken = threading.Event()
    completion_id = 'chatcmpl-probe-' + uuid.uuid4().hex
    created = int(time.time())
    max_body = 1024 * 1024
    max_output = 16 * 1024 * 1024

    def chunk(delta, finish_reason=None):
        return {'id': completion_id, 'object': 'chat.completion.chunk',
                'created': created, 'model': MODEL,
                'choices': [{'index': 0, 'delta': delta, 'finish_reason': finish_reason}]}

    class Handler(BaseHTTPRequestHandler):
        protocol_version = 'HTTP/1.1'

        def log_message(self, *args):
            pass

        def reject(self, status, code):
            body = json.dumps({'error': {'type': 'invalid_request_error', 'code': code}}).encode()
            self.send_response(status)
            self.send_header('Content-Type', 'application/json')
            self.send_header('Content-Length', str(len(body)))
            self.send_header('Connection', 'close')
            self.end_headers()
            self.wfile.write(body)
            self.close_connection = True

        def write_event(self, value):
            if time.monotonic() >= deadline:
                raise TimeoutError('bridge_deadline')
            data = value if isinstance(value, str) else json.dumps(value, ensure_ascii=False)
            self.wfile.write(('data: ' + data + '\n\n').encode('utf-8'))
            self.wfile.flush()

        def do_POST(self):
            streaming = False
            try:
                self.connection.settimeout(15)
                if self.path != '/v1/chat/completions':
                    self.reject(404, 'unknown_path')
                    return
                if self.headers.get('Authorization') or self.headers.get('Transfer-Encoding'):
                    self.reject(400, 'unexpected_auth_or_transfer_encoding')
                    return
                if request_taken.is_set():
                    self.reject(409, 'probe_accepts_one_request')
                    return
                request_taken.set()
                length = int(self.headers.get('Content-Length', '0'))
                if not 0 < length <= max_body:
                    self.reject(400, 'invalid_content_length')
                    return
                body = json.loads(self.rfile.read(length))
                if not isinstance(body, dict) or set(body) != {'model', 'messages', 'reasoning_effort', 'stream'}:
                    self.reject(400, 'unsupported_request_fields')
                    return
                messages = body.get('messages')
                if (body.get('model') != MODEL or body.get('reasoning_effort') != EFFORT
                        or body.get('stream') is not True or not isinstance(messages, list)
                        or len(messages) != 1 or not isinstance(messages[0], dict)
                        or set(messages[0]) != {'role', 'content'}
                        or messages[0].get('role') != 'user'
                        or not isinstance(messages[0].get('content'), str)):
                    self.reject(400, 'unsupported_request_shape')
                    return
                prompt = messages[0]['content']
                state['request_prompt_sha256'] = hashlib.sha256(prompt.encode()).hexdigest()
                state['request_model'] = body['model']
                state['request_effort'] = body['reasoning_effort']
                self.send_response(200)
                self.send_header('Content-Type', 'text/event-stream; charset=utf-8')
                self.send_header('Cache-Control', 'no-cache')
                self.send_header('Connection', 'close')
                self.end_headers()
                self.close_connection = True
                streaming = True
                self.write_event(chunk({'role': 'assistant', 'content': ''}))

                forwarded_parts = []
                forwarded_bytes = 0

                def on_delta(text):
                    nonlocal forwarded_bytes
                    if not isinstance(text, str):
                        raise TypeError('non_text_delta')
                    forwarded_bytes += len(text.encode('utf-8'))
                    if forwarded_bytes > max_output:
                        raise RuntimeError('upstream_output_boundary')
                    forwarded_parts.append(text)
                    if text and state['first_delta_seconds'] is None:
                        state['first_delta_seconds'] = round(time.monotonic() - started, 6)
                    state['delta_count'] += 1
                    self.write_event(chunk({'content': text}))

                forwarded_case = dict(case, prompt=prompt)
                record = app_turn(rpc, workspace, forwarded_case, alias, repetition,
                                  on_delta=on_delta, route='app_server_openai_bridge')
                state['record'] = record
                state['upstream_completed_seconds'] = round(time.monotonic() - started, 6)
                identity = record.get('identity') or {}
                if (record.get('status') != 'completed' or record.get('terminal_error')
                        or record.get('tool_items') or record.get('errors')
                        or identity.get('model') != body['model']
                        or identity.get('reasoningEffort', identity.get('effort')) != body['reasoning_effort']
                        or record.get('prompt_sha256') != state['request_prompt_sha256']
                        or ''.join(forwarded_parts) != record.get('final_output')):
                    raise RuntimeError('upstream_probe_not_valid')
                self.write_event(chunk({}, 'stop'))
                self.write_event('[DONE]')
            except Exception as error:
                state['handler_error'] = type(error).__name__
                try:
                    if streaming:
                        self.write_event({'error': {'type': 'upstream_error', 'code': 'probe_failed'}})
                    else:
                        self.reject(400, 'invalid_probe_request')
                except (OSError, TimeoutError):
                    pass
            finally:
                handler_done.set()

    server = ThreadingHTTPServer(('127.0.0.1', 0), Handler)
    server.daemon_threads = True
    server.block_on_close = False
    serving = threading.Thread(target=server.serve_forever, kwargs={'poll_interval': 0.05}, daemon=True)
    serving.start()
    parts, chunks, finishes, done, failure = [], 0, [], False, None
    bytes_read = 0
    try:
        request = urllib.request.Request(
            'http://127.0.0.1:' + str(server.server_port) + '/v1/chat/completions',
            headers={'Content-Type': 'application/json', 'Accept': 'text/event-stream'},
            data=json.dumps({'model': MODEL, 'reasoning_effort': EFFORT, 'stream': True,
                             'messages': [{'role': 'user', 'content': case['prompt']}]}, ensure_ascii=False).encode())
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
        with opener.open(request, timeout=max(1, deadline - time.monotonic())) as response:
            if response.status != 200 or not response.headers.get('Content-Type', '').startswith('text/event-stream'):
                raise RuntimeError('bridge_not_sse')
            for raw in response:
                bytes_read += len(raw)
                if bytes_read > max_output or time.monotonic() >= deadline:
                    raise TimeoutError('bridge_client_boundary')
                if not raw.startswith(b'data:'):
                    continue
                data = raw[5:].strip()
                if data == b'[DONE]':
                    done = True
                    break
                item = json.loads(data)
                if item.get('error'):
                    raise RuntimeError('bridge_upstream_failed')
                if item.get('model') != MODEL or item.get('id') != completion_id:
                    raise RuntimeError('bridge_identity_changed')
                choices = item.get('choices')
                if not isinstance(choices, list) or len(choices) != 1 or choices[0].get('index') != 0:
                    raise RuntimeError('bridge_invalid_choices')
                choice = choices[0]
                delta = choice.get('delta', {})
                if 'content' in delta:
                    if not isinstance(delta['content'], str):
                        raise RuntimeError('bridge_invalid_content')
                    parts.append(delta['content'])
                if choice.get('finish_reason') is not None:
                    finishes.append(choice['finish_reason'])
                chunks += 1
        if not handler_done.wait(timeout=5):
            raise TimeoutError('bridge_handler_not_done')
        record = state['record']
        text = ''.join(parts)
        if (not done or finishes != ['stop'] or state['handler_error'] or not record
                or text != record.get('final_output') or not state['delta_count']):
            raise RuntimeError('bridge_stream_mismatch')
    except Exception as error:
        failure = error
    finally:
        server.shutdown()
        server.server_close()
        serving.join(timeout=2)
        if failure is not None or (request_taken.is_set() and not handler_done.is_set()):
            # A failed client must not leave its isolated model turn running.
            rpc.close()
            handler_done.wait(timeout=5)
        record = state['record']
        reassembled = ''.join(parts)
        emit({'kind': 'bridge_validation', 'route': 'app_server_openai_bridge',
              'account': alias, 'case': case['id'], 'repetition': repetition,
              'endpoint': '/v1/chat/completions', 'transport': 'loopback_http_sse',
              'request_model': MODEL, 'request_effort': EFFORT,
              'prompt_sha256': state.get('request_prompt_sha256'),
              'status': 'passed' if failure is None else 'failed',
              'sse_chunks': chunks, 'content_delta_callbacks': state['delta_count'],
              'finish_reasons': finishes, 'done_received': done,
              'text_matches_app_server': bool(record) and reassembled == record.get('final_output'),
              'reassembled_sha256': hashlib.sha256(reassembled.encode()).hexdigest(),
              'first_delta_seconds': state['first_delta_seconds'],
              'upstream_completed_seconds': state['upstream_completed_seconds'],
              'elapsed_seconds': round(time.monotonic() - started, 3),
              'server_stopped': not serving.is_alive(), 'handler_done': handler_done.is_set(),
              'error_type': type(failure).__name__ if failure else state['handler_error'],
              'scope': 'one user text message; no tools or multi-turn compatibility claimed'})
    if failure is not None:
        raise failure
    return record
