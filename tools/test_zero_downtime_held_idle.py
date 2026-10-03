import copy,importlib.util,unittest
from pathlib import Path
from unittest.mock import patch

HERE=Path(__file__).resolve().parent
spec=importlib.util.spec_from_file_location('held_adapter',HERE.parent/'deploy/zero_downtime_release.py')
a=importlib.util.module_from_spec(spec);spec.loader.exec_module(a)

class HeldIdleReview(unittest.TestCase):
    def setUp(self):
        self.old={'instance':'old','listen_port':18879}
        self.before={'active':{'revision':9,'generation':'old','address':'127.0.0.1:18879','held':True},'last_update_durable':True,'generations':{'old':{'active_http_and_websocket':2}},'waiting_requests':4}
        self.after=copy.deepcopy(self.before);self.after['active']['revision']=10
    def test_exact_same_held_target_never_unholds_or_switches(self):
        with patch.object(a,'front',return_value=self.after) as front:
            result=a.prune_held_legacy_idle(self.old,self.before)
            self.assertEqual(result,self.after)
            front.assert_called_once_with('POST','/switch',{'expected_revision':9,'target':self.after['active']})
            self.assertEqual(result['generations']['old']['active_http_and_websocket'],2)
    def test_lost_reply_reads_once_without_stale_cas_retry(self):
        with patch.object(a,'front',side_effect=[OSError('lost reply'),self.after]) as front:
            self.assertEqual(a.prune_held_legacy_idle(self.old,self.before),self.after)
            self.assertEqual([x.args[0] if x.args else 'GET' for x in front.call_args_list],['POST','GET'])
    def test_false_or_unknown_durability_never_claims_cleanup_succeeded(self):
        for durable in (False,None):
            with self.subTest(durable=durable),patch.object(a,'front',return_value={**self.after,'last_update_durable':durable}):
                with self.assertRaises(a.Blocked):a.prune_held_legacy_idle(self.old,self.before)
    def test_wrong_ack_target_or_revision_is_not_a_success(self):
        for field,value in [('generation','other'),('address','127.0.0.1:18891'),('held',False),('revision',11)]:
            after=copy.deepcopy(self.after);after['active'][field]=value
            with self.subTest(field=field),patch.object(a,'front',return_value=after):
                with self.assertRaises(a.Blocked):a.prune_held_legacy_idle(self.old,self.before)
    def test_unheld_or_wrong_target_does_not_send_control_request(self):
        for field,value in [('held',False),('generation','other'),('address','127.0.0.1:18891')]:
            before=copy.deepcopy(self.before);before['active'][field]=value
            with self.subTest(field=field),patch.object(a,'front') as front:
                with self.assertRaises(a.Blocked):a.prune_held_legacy_idle(self.old,before)
                front.assert_not_called()
    def test_lost_reply_without_commit_remains_error_and_never_retries(self):
        with patch.object(a,'front',side_effect=[OSError('lost reply'),self.before]) as front:
            with self.assertRaises(a.Blocked):a.prune_held_legacy_idle(self.old,self.before)
            self.assertEqual(front.call_count,2)

if __name__=='__main__':unittest.main()
