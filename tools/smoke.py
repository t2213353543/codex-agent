"""Run real-account checks on the deployed service; credentials stay on the server."""
import argparse
import json
from pathlib import Path
import textwrap

from deploy import ROOT, ssh_module


REMOTE = r'''
import json, pathlib, subprocess, urllib.request, urllib.error

key = pathlib.Path('/var/lib/codex-agent/client-key').read_text().strip()
base = 'http://127.0.0.1:18879'

def request(path, body=None, method=None):
    data = None if body is None else json.dumps(body).encode()
    req = urllib.request.Request(base+path, data=data, method=method,
          headers={'Authorization':'Bearer '+key, 'Content-Type':'application/json'})
    try:
        return urllib.request.urlopen(req, timeout=180)
    except urllib.error.HTTPError as error:
        detail = error.read(4096).decode(errors='replace')
        raise RuntimeError(f'{path}: HTTP {error.code}: {detail}') from None

def call(path, body=None, method=None):
    with request(path, body, method) as response:
        return json.load(response)

source_id = SOURCE_ID
model = MODEL_NAME
if source_id:
    sql = "SELECT jsonb_build_object('access_token',credentials->>'access_token','account_id',credentials->>'chatgpt_account_id','expires_at',(credentials->>'expires_at')::bigint) FROM accounts WHERE id="+str(source_id)+" AND deleted_at IS NULL AND status='active' AND schedulable;"
    raw = subprocess.check_output(['docker','exec','account-hub-postgres','psql','-U','account_hub','-d','account_hub','-Atc',sql])
    credentials = json.loads(raw)
    assert credentials.get('access_token'), 'source account missing'
    call('/api/accounts', {'name':'accounts-sub2-test','credentials':credentials})
    call('/api/active-account', {'name':'accounts-sub2-test'}, 'PUT')
    print('import: PASS (access token only; source unchanged)')

accounts = call('/api/accounts')
assert accounts['active'], 'no active account'
assert all('access_token' not in a and 'refresh_token' not in a for a in accounts['accounts'])
print('accounts: PASS (redacted)')
models = call('/v1/models')['models']
slugs = [m['slug'] for m in models]
assert model in slugs, slugs
print('models: PASS ('+str(len(slugs))+' models)')

def payload(text, stream=False):
    return {'model':model, 'instructions':'Follow the user request exactly.', 'input':text,
            'stream':stream, 'reasoning':{'effort':'low'}}

def output_text(response):
    return ''.join(c.get('text','') for item in response.get('output',[]) for c in item.get('content',[]))

try:
    response = call('/v1/responses', payload('Reply exactly CODEX_AGENT_OK.'))
except Exception:
    with request('/v1/responses', payload('Reply exactly CODEX_AGENT_OK.', True)) as diagnostic:
        for line in diagnostic:
            if line.startswith(b'data:') and line[5:].strip() != b'[DONE]':
                event = json.loads(line[5:])
                if event.get('type') in ('error','response.failed'):
                    print('upstream diagnostic:', json.dumps(event.get('error') or event.get('response',{}).get('error')), flush=True)
    raise
assert response.get('status') == 'completed', response.get('status')
assert 'CODEX_AGENT_OK' in output_text(response), output_text(response)
print('non-stream: PASS (completed, CODEX_AGENT_OK)')

events = []
with request('/v1/responses', payload('Reply exactly SSE_OK.', True)) as stream:
    assert stream.headers.get_content_type() == 'text/event-stream'
    for line in stream:
        if not line.startswith(b'data:'): continue
        data = line[5:].strip()
        if data == b'[DONE]': continue
        event = json.loads(data)
        events.append(event)
terminal = [e for e in events if e.get('type') in ('response.completed','response.done')]
assert terminal and 'SSE_OK' in output_text(terminal[-1]['response'])
assert any(e.get('type')=='response.output_text.delta' for e in events)
print('stream: PASS (deltas and terminal event)')

tool = {'type':'function','name':'add','description':'Add two integers.',
        'parameters':{'type':'object','properties':{'a':{'type':'integer'},'b':{'type':'integer'}},'required':['a','b'],'additionalProperties':False},'strict':True}
input_items = [{'role':'user','content':'Use add to calculate 1+2, then reply with the result only.'}]
body = payload(input_items)
body['tools'] = [tool]
body['tool_choice'] = {'type':'function','name':'add'}
body['include'] = ['reasoning.encrypted_content']
first = call('/v1/responses', body)
calls = [item for item in first['output'] if item.get('type')=='function_call']
assert len(calls)==1 and calls[0]['name']=='add'
arguments = json.loads(calls[0]['arguments'])
assert arguments == {'a':1,'b':2}, arguments
body['input'] = input_items + first['output'] + [{'type':'function_call_output','call_id':calls[0]['call_id'],'output':'3'}]
body['tool_choice'] = 'auto'
second = call('/v1/responses', body)
assert second.get('status')=='completed' and '3' in output_text(second)
print('tool round-trip: PASS (function call and output)')

compact = call('/v1/responses/compact', {'model':model,'instructions':'Preserve conversation context.',
                                      'input':body['input'] + second['output']})
assert compact.get('output'), 'compact output missing'
print('compact: PASS')
'''


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--import-account', type=int, default=0)
    parser.add_argument('--model', default='gpt-5.6-luna')
    args = parser.parse_args()
    if args.import_account < 0:
        parser.error('account id must be positive')
    client = ssh_module().connect('master')
    try:
        stdin, stdout, stderr = client.exec_command('python3 -', timeout=900)
        stdin.write(textwrap.dedent(REMOTE).replace('SOURCE_ID', str(args.import_account)).replace('MODEL_NAME', repr(args.model)))
        stdin.channel.shutdown_write()
        for line in stdout:
            print(line, end='', flush=True)
        error = stderr.read().decode(errors='replace')
        if stdout.channel.recv_exit_status():
            raise RuntimeError(error)
        local = ROOT / '.local'
        local.mkdir(exist_ok=True)
        with client.open_sftp() as sftp:
            sftp.get('/var/lib/codex-agent/client-key', str(local / 'production-client-key'))
        print('Client key saved to .local/production-client-key (not printed).')
    finally:
        client.close()


if __name__ == '__main__':
    main()
