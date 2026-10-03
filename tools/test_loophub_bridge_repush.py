"""Synthetic bridge upload/reauthorization tests. No production or OAuth calls."""
import base64
import copy
import importlib.util
import json
from pathlib import Path
import tempfile
import time
import unittest

ROOT=Path(__file__).resolve().parents[1]
SPEC=importlib.util.spec_from_file_location('bridge_repush',ROOT/'deploy/loophub_codex_bridge.py')
bridge=importlib.util.module_from_spec(SPEC);SPEC.loader.exec_module(bridge)


def token(user='actual-user',workspace='actual-workspace',*,generation=1,primary=True,extra=None):
    auth={'chatgpt_account_id':workspace}
    if primary:auth['chatgpt_user_id']=user
    else:auth['user_id']=user
    value={'exp':int(time.time())+3600,'iat':int(time.time())-60,'jti':'private-generation-'+str(generation),
        'sub':'auth0-unrelated-subject-'+str(generation),'https://api.openai.com/auth':auth,
        'https://api.openai.com/profile':{'email':'synthetic@example.invalid'}}
    if extra:value.update(extra)
    return 'header.'+base64.urlsafe_b64encode(json.dumps(value).encode()).decode().rstrip('=')+'.synthetic'


class Destination:
    def __init__(self):
        self.row={};self.secret={};self.calls=[];self.ignore_reauthorize=False;self.export_override=None
        self.export_conflict=False;self.change_after_patch=None
    def call(self,path,data=None,method=None):
        self.calls.append((path,copy.deepcopy(data),method))
        if path=='/api/accounts/export/credentials':
            if self.export_conflict:raise bridge.BridgeError('agent_http_409',409)
            if data!={'items':[{'id':self.row['id'],'version':self.row['version']}]}:raise bridge.BridgeError('agent_http_409',409)
            if self.export_override is not None:return copy.deepcopy(self.export_override)
            return {'accounts':[{'name':self.row['name'],'credentials':copy.deepcopy(self.secret)}]}
        if path.endswith('/metadata'):
            raw=self.secret['access_token'].split('.')[1];claims=json.loads(base64.urlsafe_b64decode(raw+'='*(-len(raw)%4)))
            auth=claims['https://api.openai.com/auth'];user=auth.get('chatgpt_user_id');user=auth.get('user_id') if user is None else user
            return {'id':self.row['id'],'version':self.row['version'],'identity':{'workspace_id':auth['chatgpt_account_id'],'user_id':user}}
        if path.endswith('/reauthorize'):
            if data['version']!=self.row['version']:raise bridge.BridgeError('agent_http_409',409)
            if not self.ignore_reauthorize:self.secret=copy.deepcopy(data['credentials'])
            self.row.update(version=self.row['version']+1,last_error='',credential_status='unverified')
            return {'id':self.row['id'],'version':self.row['version']}
        if method=='PUT':
            if data['version']!=self.row['version']:raise bridge.BridgeError('agent_http_409',409)
            self.row.update(copy.deepcopy(data));self.row['version']+=1
            if self.change_after_patch:self.secret['access_token']=self.change_after_patch
            return {'ok':True}
        if path.endswith('/upstream'):return {'id':self.row['id'],'snapshots':{}}
        if data is None and method in (None,'GET'):return {'account':copy.deepcopy(self.row)}
        raise AssertionError('unexpected bridge route')


class RePushTests(unittest.TestCase):
    def setUp(self):
        temp=tempfile.TemporaryDirectory();self.addCleanup(temp.cleanup);root=Path(temp.name)
        (root/'key').write_text('synthetic-ingress-key-'*3)
        self.api=Destination();self.instance=bridge.Bridge({'workspace_id':'actual-workspace','group_id':'bridge-group',
            'state_file':str(root/'state.json'),'ingress_key_file':str(root/'key')},api=self.api)
        self.filename='synthetic@example.invalid.json';self.payload={'type':'codex','email':'synthetic@example.invalid',
            'account_id':'actual-workspace','access_token':token()}
        credentials,facts=self.instance.credentials(self.filename,self.payload)
        name='Loophub-28765-'+bridge.digest('actual-workspace:synthetic@example.invalid')[:16]
        self.api.row={'id':'a_bridge','name':name,'version':5,'tags':[self.instance.tag],'has_refresh_token':False,
            'enabled':True,'groups':['operator-group','bridge-group'],'expires_at':credentials['expires_at'],
            'credential_status':'invalid','status':'error','last_error':'token_revoked'}
        self.api.secret=credentials
        self.instance.state['accounts'][self.filename]={**facts,'account_id':'a_bridge','name':name,'initialized':True}
        self.instance.save()

    def reauthorizations(self):return [entry for entry in self.api.calls if entry[0].endswith('/reauthorize')]

    def test_destination_old_token_is_replaced_even_when_checkpoint_matches_upload(self):
        self.api.secret['access_token']=token(generation=0)
        expected=self.payload['access_token']
        result=self.instance.upload(self.filename,self.payload)
        self.assertTrue(result['ok'])
        self.assertEqual(self.api.secret['access_token'],expected)
        self.assertEqual(len(self.reauthorizations()),1)
        self.assertFalse(self.reauthorizations()[0][1]['allow_identity_change'])
        self.assertEqual(self.api.row['groups'],['operator-group','bridge-group'])

    def test_same_actual_token_not_reauthorized_to_erase_a_real_revoked_observation(self):
        self.instance.upload(self.filename,self.payload)
        self.assertEqual(self.reauthorizations(),[])
        self.assertEqual(self.api.row['last_error'],'token_revoked')
        self.assertEqual(self.api.row['credential_status'],'invalid')

    def test_stale_checkpoint_alone_does_not_rewrite_matching_actual_token(self):
        self.instance.state['accounts'][self.filename]['token_sha256']=bridge.digest(token(generation=0))
        self.instance.upload(self.filename,self.payload)
        self.assertEqual(self.reauthorizations(),[])
        self.assertEqual(self.api.row['last_error'],'token_revoked')
        mapping=self.instance.state['accounts'][self.filename]
        self.assertEqual(mapping['token_sha256'],bridge.digest(self.payload['access_token']))
        self.assertEqual(mapping['destination_token_sha256'],mapping['token_sha256'])

    def test_readback_contains_only_hash_receipt_not_new_secret(self):
        self.payload['access_token']=token(generation=2)
        self.instance.upload(self.filename,self.payload)
        mapping=self.instance.state['accounts'][self.filename]
        self.assertTrue(mapping['initialized'])
        self.assertEqual(mapping['destination_readback_version'],self.api.row['version'])
        persisted=self.instance.state_path.read_text()
        self.assertNotIn(self.payload['access_token'],persisted)
        self.assertNotIn('refresh_token',persisted)

    def test_destination_wrong_owner_or_refresh_token_is_never_overwritten(self):
        for change in ({'access_token':token(user='other-user')},{'access_token':token(workspace='other-workspace'),'account_id':'other-workspace'},
                       {'refresh_token':'unexpected-destination-rt'}):
            original=copy.deepcopy(self.api.secret);self.api.secret.update(change)
            with self.assertRaisesRegex(bridge.BridgeError,'destination_ownership_changed'):self.instance.upload(self.filename,self.payload)
            self.assertEqual(self.reauthorizations(),[])
            self.assertFalse(any(method=='PUT' for _,_,method in self.api.calls))
            self.api.secret=original

    def test_export_schema_ambiguity_and_cas_error_do_not_write(self):
        for value in ({'accounts':[]},{'accounts':[{},{}]},{'accounts':[{'name':'other','credentials':self.api.secret}]},
                      {'accounts':[{'name':self.api.row['name'],'credentials':'invalid JSON'}]}):
            self.api.export_override=value
            with self.assertRaises(bridge.BridgeError):self.instance.upload(self.filename,self.payload)
            self.assertEqual(self.reauthorizations(),[])
        self.api.export_override=None;self.api.export_conflict=True
        with self.assertRaisesRegex(bridge.BridgeError,'agent_http_409'):self.instance.upload(self.filename,self.payload)
        self.assertFalse(any(method=='PUT' for _,_,method in self.api.calls))

    def test_destination_changes_during_metadata_patch_leaves_delivery_uninitialized(self):
        self.api.change_after_patch=token(generation=4)
        with self.assertRaisesRegex(bridge.BridgeError,'destination_token_changed_during_delivery'):
            self.instance.upload(self.filename,self.payload)
        self.assertFalse(self.instance.state['accounts'][self.filename]['initialized'])
        self.assertFalse(json.loads(self.instance.state_path.read_text())['accounts'][self.filename]['initialized'])

    def test_expired_destination_same_owner_can_be_repaired_with_valid_incoming(self):
        self.api.secret['access_token']=token(generation=0,extra={'exp':int(time.time())-100})
        self.instance.upload(self.filename,self.payload)
        self.assertEqual(len(self.reauthorizations()),1)
        self.assertEqual(self.api.secret['access_token'],self.payload['access_token'])

    def test_store_claiming_reauth_success_without_storing_token_never_returns_ok(self):
        self.payload['access_token']=token(generation=2)
        self.api.ignore_reauthorize=True
        with self.assertRaises(bridge.BridgeError):self.instance.upload(self.filename,self.payload)
        self.assertFalse(any(method=='PUT' for _,_,method in self.api.calls))

    def test_stable_user_fallback_is_auth_user_id_not_subject(self):
        self.payload['access_token']=token(primary=False,generation=2)
        _,facts=self.instance.credentials(self.filename,self.payload)
        self.assertEqual(facts['identity_sha256'],bridge.digest('actual-workspace:actual-user'))
        self.instance.upload(self.filename,self.payload)
        self.assertEqual(len(self.reauthorizations()),1)

    def test_null_primary_user_falls_back_but_changed_owner_does_not(self):
        self.payload['access_token']=token(extra={'https://api.openai.com/auth':{'chatgpt_account_id':'actual-workspace',
            'chatgpt_user_id':None,'user_id':'actual-user'}})
        self.instance.upload(self.filename,self.payload)
        self.payload['access_token']=token(user='other-user',generation=3)
        with self.assertRaisesRegex(bridge.BridgeError,'source_identity_changed'):self.instance.upload(self.filename,self.payload)

    def test_empty_or_wrong_primary_user_does_not_fall_back_to_sub_or_secondary(self):
        for primary in ('',123,True,{},[]):
            self.payload['access_token']=token(extra={'https://api.openai.com/auth':{'chatgpt_account_id':'actual-workspace',
                'chatgpt_user_id':primary,'user_id':'actual-user'}})
            with self.assertRaises(bridge.BridgeError):self.instance.credentials(self.filename,self.payload)

    def test_sub_only_owner_not_accepted(self):
        self.payload['access_token']=token(extra={'https://api.openai.com/auth':{'chatgpt_account_id':'actual-workspace'}})
        with self.assertRaises(bridge.BridgeError):self.instance.credentials(self.filename,self.payload)

    def test_duplicate_claims_or_noncanonical_token_envelope_rejected(self):
        raw=b'{"exp":4102444800,"https://api.openai.com/auth":{"chatgpt_account_id":"actual-workspace","chatgpt_user_id":"actual-user","chatgpt_user_id":"other"}}'
        bad='header.'+base64.urlsafe_b64encode(raw).decode().rstrip('=')+'.synthetic'
        for access in (bad,self.payload['access_token']+'.extra',self.payload['access_token'].replace('.', '.!',1)):
            self.payload['access_token']=access
            with self.assertRaises(bridge.BridgeError):self.instance.credentials(self.filename,self.payload)

    def test_expiry_bool_nan_fraction_email_wrongtype_and_whitespace_rejected(self):
        for extra in ({'exp':True},{'exp':float('nan')},{'exp':time.time()+3600},
                      {'email':[]},{'https://api.openai.com/profile':[]},
                      {'https://api.openai.com/auth':{'chatgpt_account_id':'actual-workspace','chatgpt_user_id':'bad user'}}):
            self.payload['access_token']=token(extra=extra)
            with self.assertRaisesRegex(bridge.BridgeError,'token_workspace_identity_or_expiry_mismatch'):
                self.instance.credentials(self.filename,self.payload)

    def test_multipart_duplicate_auth_fields_rejected_before_upload(self):
        body=(b'--boundary\r\nContent-Disposition: form-data; name="file"; filename="synthetic@example.invalid.json"\r\n'
              b'Content-Type: application/json\r\n\r\n{"type":"codex","access_token":"one","access_token":"two"}\r\n--boundary--\r\n')
        with self.assertRaisesRegex(bridge.BridgeError,'invalid_auth_json'):
            bridge.multipart_file('multipart/form-data; boundary=boundary',body)


if __name__=='__main__':unittest.main()
