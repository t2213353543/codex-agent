import argparse,copy,json,tempfile,unittest
from pathlib import Path
from unittest.mock import Mock,patch
import importlib.util
_spec=importlib.util.spec_from_file_location('zero_downtime_release',Path(__file__).resolve().parents[1]/'deploy/zero_downtime_release.py')
a=importlib.util.module_from_spec(_spec);_spec.loader.exec_module(a)

class ProductionAdapterReview(unittest.TestCase):
    def test_performance_validator_requires_explicit_backend(self):
        self.assertEqual(a.release_validator_name({'backend':'management_performance'}),'performance_release.py')
        for backend in ('selectable_official_runtime','powerpoint_bps','unknown',None):
            self.assertEqual(a.release_validator_name({'backend':backend}),'reliability_release.py')
    def test_performance_stage_validates_package_and_exact_schema_without_commands(self):
        expected={'migrations/synthetic.sql':'a'*64};manifest={'backend':'management_performance','migration_checksums':expected}
        module=Mock();module.base.schema.return_value=expected
        with patch('importlib.util.spec_from_file_location') as spec,patch('importlib.util.module_from_spec',return_value=module),patch.object(a,'command') as command:
            a.validate_stage_schema(Path('/synthetic-release'),manifest)
            self.assertEqual(spec.call_args.args[1],Path('/synthetic-release/deploy/performance_release.py'))
            module.validate.assert_called_once_with(Path('/synthetic-release'));module.base.schema.assert_called_once_with();command.assert_not_called()
    def test_performance_stage_rejects_schema_delta_without_migrating(self):
        module=Mock();module.base.schema.return_value={'old':'a'*64}
        with patch('importlib.util.spec_from_file_location'),patch('importlib.util.module_from_spec',return_value=module),patch.object(a,'command') as command:
            with self.assertRaisesRegex(a.Blocked,'does not migrate'):
                a.validate_stage_schema(Path('/synthetic-release'),{'backend':'management_performance','migration_checksums':{'new':'b'*64}})
            module.validate.assert_called_once();command.assert_not_called()
    def setUp(self):
        lifecycle=patch.object(a,'lifecycle',lambda instance,drain=False:{'admission_mode':'redis_account_leases_v1','draining':True,'active_handlers':0,'refresh_operations':0,'pending_persistence':0});lifecycle.start();self.addCleanup(lifecycle.stop)
        self.old={'unit':'codex-agent.service','instance':'old','pid':111,'binary_sha256':'a'*64,'listen_port':18879,'admission_mode':'process_local'}
        self.new={'unit':'codex-agent-runtime-'+('b'*20)+'.service','instance':'new','pid':222,'binary_sha256':'b'*64,'listen_port':18890,'admission_mode':'redis_account_leases_v1'}
        self.record={'schema':1,'revision':2,'active':self.old,'candidate':self.new,'draining':[],'pending':{'phase':'legacy_drain_held','old':self.old,'candidate':self.new}}
        self.front={'last_update_durable':True,'active':{'revision':4,'held':True,'generation':'old','address':'127.0.0.1:18879'},'generations':{'old':{'active_http_and_websocket':0}},'waiting_requests':7}
    def simulated_front(self,method='GET',path='/status',body=None):
        if method=='POST':self.front['active']=body['target']
        return self.front
    def test_real_nat_template_scoped_and_uid_bypass(self):
        text=a.nft_config(65000)
        self.assertIn('meta skuid 65000 return',text)
        self.assertEqual(text.count('tcp dport 18879 tcp flags & (syn | ack) == syn counter redirect to :18878'),2)
        self.assertIn('ct original proto-dst 18879',text)
        self.assertIn('tcp dport 18878 drop',text)
        self.assertNotIn('18191',text);self.assertNotIn('flush',text)
    def test_env_quotes_preserve_literal_shell_chars(self):
        text=a.render_env({b'TEST':b'quote" dollar$ backtick` slash\\'})
        self.assertEqual(text,'TEST="quote\\" dollar$ backtick` slash\\\\"\n')
        with self.assertRaises(a.Blocked):a.render_env({b'TEST':b'a\nb'})
    def test_legacy_direct_sockets_wait_without_stop_or_switch(self):
        with patch.multiple(a,registry=lambda:copy.deepcopy(self.record),runtime=lambda u:self.new,healthy=lambda p:None,front=self.simulated_front,connected_sockets=lambda p,n:[{'inode':'17'}]),patch.object(a,'update_front') as switch,patch.object(a,'command') as command:
            result=a.activate(argparse.Namespace())
            self.assertFalse(result['activated']);self.assertEqual(result['phase'],'waiting_for_legacy_natural_drain');switch.assert_not_called();command.assert_not_called()
    def test_legacy_front_websocket_waits_even_if_direct_socket_snapshot_empty(self):
        self.front['generations']['old']['active_http_and_websocket']=1
        with patch.multiple(a,registry=lambda:copy.deepcopy(self.record),runtime=lambda u:self.new,healthy=lambda p:None,front=self.simulated_front,connected_sockets=lambda p,n:[]),patch.object(a,'update_front') as switch:
            self.assertFalse(a.activate(argparse.Namespace())['activated']);switch.assert_not_called()
    def test_fully_drained_legacy_switches_then_registry_never_stops(self):
        events=[]
        def switch_front(method='GET',path='/status',body=None):
            if method=='GET':return self.front
            if body['target']['held']:return {**self.front,'active':body['target']}
            events.append(('switch','new',False));return {'active':body['target'],'last_update_durable':True}
        with patch.multiple(a,registry=lambda:copy.deepcopy(self.record),runtime=lambda u:self.new,healthy=lambda p:None,front=switch_front,connected_sockets=lambda p,n:[],write_registry=lambda r:events.append(('registry',copy.deepcopy(r)))),patch.object(a,'command') as command:
            result=a.activate(argparse.Namespace());self.assertTrue(result['activated']);self.assertFalse(result['old_stopped']);command.assert_called_once_with(['systemctl','enable',self.new['unit']])
        self.assertEqual(events[0][1]['pending']['phase'],'switching');self.assertEqual(events[1],('switch','new',False));self.assertEqual(events[2][1]['active']['pid'],222);self.assertEqual(events[2][1]['draining'][0]['pid'],111)
    def test_regular_shared_overlap_does_not_require_zero_old_connections(self):
        self.old['admission_mode']='redis_account_leases_v1';self.front['active']['held']=False
        with patch.multiple(a,registry=lambda:copy.deepcopy(self.record),runtime=lambda u:self.new,healthy=lambda p:None,front=lambda method='GET',path='/status',body=None:self.front if method=='GET' else {'active':body['target'],'last_update_durable':True},connected_sockets=lambda p,n:[{'inode':'17'}],write_registry=lambda r:None,command=lambda args:None):
            self.assertTrue(a.activate(argparse.Namespace())['activated'])
    def test_candidate_mismatch_cannot_switch(self):
        changed={**self.new,'pid':999}
        with patch.multiple(a,registry=lambda:copy.deepcopy(self.record),runtime=lambda u:changed),patch.object(a,'update_front') as switch:
            with self.assertRaises(a.Blocked):a.activate(argparse.Namespace())
            switch.assert_not_called()
    def test_retirement_refuses_any_old_connection(self):
        self.record['active']=self.new;self.record['draining']=[self.old];self.front['active']['generation']='new'
        with patch.multiple(a,registry=lambda:self.record,runtime=lambda u:self.old,front=self.simulated_front,connected_sockets=lambda p,n:[{'inode':'17'}]),patch.object(a,'command') as command:
            self.assertFalse(a.retire(argparse.Namespace(unit=self.old['unit']))['retired']);command.assert_not_called()
    def test_pending_switch_reconciles_without_second_switch(self):
        before=copy.deepcopy(self.front['active']);after={'revision':5,'generation':'new','address':'127.0.0.1:18890','held':False}
        self.record['pending']={'phase':'switching','old':self.old,'new':self.new,'front_before':before,'front_after':after,'resume_pending':self.record['pending']};self.front['active']=after;saved=[]
        with patch.multiple(a,registry=lambda:copy.deepcopy(self.record),runtime=lambda u:self.new,front=self.simulated_front,write_registry=lambda r:saved.append(r),command=lambda args:None),patch.object(a,'update_front') as switch:
            result=a.reconcile(argparse.Namespace());self.assertEqual(result['reconciled'],'committed_already_switched_ingress');switch.assert_not_called();self.assertEqual(saved[0]['active']['pid'],222)
    def test_private_port_allows_extra_draining_generations_but_protects_unrelated_runtime(self):
        self.assertTrue(a.private_backend_port(18892));self.assertTrue(a.private_backend_port(34567));self.assertFalse(a.private_backend_port(18191));self.assertFalse(a.private_backend_port(18879))
    def test_legacy_retirement_stays_alive_without_lifecycle(self):
        self.record['active']=self.new;self.record['draining']=[self.old];self.front['active']['generation']='new'
        with patch.multiple(a,registry=lambda:self.record,runtime=lambda u:self.old,front=self.simulated_front,connected_sockets=lambda p,n:[]),patch.object(a,'command') as command:
            result=a.retire(argparse.Namespace(unit=self.old['unit']));self.assertFalse(result['retired']);self.assertEqual(result['reason'],'legacy_retirement_unverified_keep_running');command.assert_not_called()
    def test_shared_retirement_requires_drain_endpoint_and_zero_work(self):
        self.old['admission_mode']='redis_account_leases_v1';self.record['active']=self.new;self.record['draining']=[self.old];self.front['active']['generation']='new';calls=[]
        def lifecycle(instance,drain=False):calls.append(drain);return {'draining':True,'active_handlers':0,'refresh_operations':0,'pending_persistence':0}
        with patch.multiple(a,registry=lambda:self.record,runtime=lambda u:self.old,front=self.simulated_front,connected_sockets=lambda p,n:[],lifecycle=lifecycle,write_registry=lambda r:None),patch.object(a,'command') as command:
            self.assertTrue(a.retire(argparse.Namespace(unit=self.old['unit']))['retired']);self.assertEqual(calls,[True,False]);self.assertEqual(command.call_args_list[0].args[0],['systemctl','stop',self.old['unit']])
    def test_lost_switch_reply_rereads_status_without_retrying_post(self):
        active={'revision':4,'generation':'old','address':'127.0.0.1:18879','held':True}
        expected={'revision':5,'generation':'new','address':'127.0.0.1:18890','held':False};calls=[]
        def front(method='GET',path='/status',body=None):
            calls.append(method)
            if len(calls)==1:return {'active':active}
            if method=='POST':raise OSError('synthetic reply lost after commit')
            return {'active':expected,'last_update_durable':False}
        with patch.object(a,'front',front):
            result=a.update_front(self.new,False);self.assertTrue(result['switch_reply_reconciled']);self.assertEqual(calls,['GET','POST','GET']);self.assertFalse(result['last_update_durable'])
    def test_lost_switch_reply_wrong_target_preserves_pending(self):
        calls=[]
        def front(method='GET',path='/status',body=None):
            calls.append(method)
            if method=='POST':raise OSError('lost')
            return {'active':{'revision':4,'generation':'old','address':'127.0.0.1:18879','held':True}}
        with patch.object(a,'front',front):
            with self.assertRaises(a.Blocked):a.update_front(self.new,False)
            self.assertEqual(calls,['GET','POST','GET'])
    def test_new_services_keep_user_requested_unbounded_resources(self):
        self.assertEqual(a.UNBOUNDED_RESOURCES.splitlines(),['TasksMax=infinity','MemoryHigh=infinity','MemoryMax=infinity','MemorySwapMax=infinity'])
        self.assertNotIn('CPUQuota',a.UNBOUNDED_RESOURCES)
    def test_boot_nat_existing_correct_table_is_verified_not_duplicated(self):
        result=argparse.Namespace(returncode=0)
        reg={'ingress':{'nft_rules_sha256':'a'*64,'nft_config_sha256':'b'*64}}
        with patch.multiple(a,registry=lambda:reg,digest=lambda p:'b'*64,scoped_nat_digest=lambda:'a'*64),patch.object(a.subprocess,'run',lambda *x,**k:result),patch.object(a,'command') as command:
            self.assertTrue(a.ensure_nat(argparse.Namespace())['nat_verified']);command.assert_not_called()
    def test_boot_nat_drift_refuses_overwrite(self):
        result=argparse.Namespace(returncode=0);reg={'ingress':{'nft_rules_sha256':'a'*64,'nft_config_sha256':'b'*64}}
        with patch.multiple(a,registry=lambda:reg,digest=lambda p:'b'*64,scoped_nat_digest=lambda:'c'*64),patch.object(a.subprocess,'run',lambda *x,**k:result),patch.object(a,'command') as command:
            with self.assertRaises(a.Blocked):a.ensure_nat(argparse.Namespace())
            command.assert_not_called()

if __name__=='__main__':unittest.main()
