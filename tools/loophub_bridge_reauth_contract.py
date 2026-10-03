"""Real bridge against the Go loopback management API; synthetic tokens only."""
import base64
import hashlib
import importlib.util
import json
from pathlib import Path
import sys
import tempfile
import time
import urllib.parse


def main():
    params=json.load(sys.stdin)
    parsed=urllib.parse.urlsplit(params['base'])
    assert parsed.scheme=='http' and parsed.hostname=='127.0.0.1'
    spec=importlib.util.spec_from_file_location('reauth_bridge',Path(__file__).resolve().parents[1]/'deploy/loophub_codex_bridge.py')
    bridge=importlib.util.module_from_spec(spec);spec.loader.exec_module(bridge)
    def token(n):
        claims={'email':'synthetic@example.invalid','exp':int(time.time())+3600,'jti':str(n),
                'https://api.openai.com/auth':{'chatgpt_account_id':'synthetic-workspace','user_id':'synthetic-user'}}
        return 'e30.'+base64.urlsafe_b64encode(json.dumps(claims).encode()).decode().rstrip('=')+'.synthetic'
    old,new=token(1),token(2)
    filename='synthetic@example.invalid.json'
    def payload(value):return {'type':'codex','email':'synthetic@example.invalid','account_id':'synthetic-workspace','access_token':value}
    with tempfile.TemporaryDirectory() as folder:
        root=Path(folder);(root/'key').write_text('synthetic-key-'*4)
        api=bridge.API(params['base'],params['key'])
        instance=bridge.Bridge({'workspace_id':'synthetic-workspace','group_id':params['group_id'],
                'state_file':str(root/'state.json'),'ingress_key_file':str(root/'key')},api=api)
        assert instance.upload(filename,payload(old))['ok']
        account_id=instance.state['accounts'][filename]['account_id']
        api.call('/fixture/reject',{'id':account_id})
        assert instance.list_files()['files'][0]['needs_reauth'] is True
        assert instance.upload(filename,payload(new))['ok']
        current=api.call('/api/accounts/'+account_id)['account']
        exported=api.call('/api/accounts/export/credentials',{'items':[{'id':account_id,'version':current['version']}]})['accounts'][0]['credentials']
        if isinstance(exported,str):exported=json.loads(exported)
        assert exported['access_token']==new
        assert current['status']=='unverified' and current['credential_status']=='unverified' and not current.get('last_error')
        rows=api.call('/api/accounts/'+account_id+'/upstream')['snapshots']
        assert rows and all(row['stale'] for row in rows.values())
        assert instance.list_files()['files'][0]['status']=='active'
        # An external destination replacement must not be hidden by a matching
        # bridge checkpoint. Re-delivering the checkpoint token repairs it.
        api.call('/fixture/replace',{'id':account_id,'access_token':old})
        assert instance.upload(filename,payload(new))['ok']
        current=api.call('/api/accounts/'+account_id)['account']
        exported=api.call('/api/accounts/export/credentials',{'items':[{'id':account_id,'version':current['version']}]})['accounts'][0]['credentials']
        if isinstance(exported,str):exported=json.loads(exported)
        assert exported['access_token']==new and instance.list_files()['files'][0]['status']=='active'
        # Deleting an imported account must not poison its stable source
        # filename forever. A verified re-delivery creates a fresh account.
        api.call('/fixture/delete',{'id':account_id})
        assert instance.list_files()['files'][0]['status']=='error'
        assert instance.upload(filename,payload(new))['ok']
        replacement=instance.state['accounts'][filename]['account_id']
        assert replacement!=account_id
        current=api.call('/api/accounts/'+replacement)['account']
        assert current['enabled'] and params['group_id'] in current['groups'] and not current['has_refresh_token']
        exported=api.call('/api/accounts/export/credentials',{'items':[{'id':replacement,'version':current['version']}]})['accounts'][0]['credentials']
        if isinstance(exported,str):exported=json.loads(exported)
        assert exported['access_token']==new and instance.list_files()['files'][0]['status']=='active'
        # Source withdrawal also succeeds when the destination is already gone.
        api.call('/fixture/delete',{'id':replacement})
        assert instance.withdraw(filename)['ok'] and instance.list_files()['files']==[]
        assert instance.withdraw(filename)['ok']
        print(json.dumps({'ok':True,'checks':14,'model_calls':0,'oauth_refreshes':0}))


if __name__=='__main__':main()
