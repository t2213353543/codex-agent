import importlib.util
from pathlib import Path
import unittest

path = Path(__file__).resolve().parents[1] / 'deploy/error_monitor.py'
spec = importlib.util.spec_from_file_location('error_monitor_under_test', path)
monitor = importlib.util.module_from_spec(spec)
spec.loader.exec_module(monitor)


def row(identifier, status='error', http='503', older=True):
    return {'id': identifier, 'status': status, 'http': http, 'older_request': older,
            'source': 'gateway', 'phase': 'account_selection', 'parameter': '',
            'error_hash': 'a' * 32, 'transport': 'websocket'}


class MonitorCompletionTest(unittest.TestCase):
    def test_initial_baseline_does_not_recount_history(self):
        observed, state = monitor.summarize({'at': 'start', 'rows': [row(20), row(19)]}, {})
        self.assertTrue(observed['baseline_only'])
        self.assertEqual(observed['totals']['requests'], 0)
        self.assertEqual(state['seen_ids'], [19, 20])

    def test_slow_completion_is_counted_by_new_record_not_start_time(self):
        observed, state = monitor.summarize({'at': 'next', 'rows': [row(21, older=True), row(20)]},
                                            {'initialized': True, 'at': 'previous', 'seen_ids': [20]})
        self.assertEqual(observed['totals']['non_rate_errors'], 1)
        self.assertEqual(observed['late_completions_observed'], 1)
        again, _ = monitor.summarize({'at': 'later', 'rows': [row(21), row(20)]}, state)
        self.assertEqual(again['totals']['requests'], 0)

    def test_out_of_order_commit_below_high_water_mark_is_not_lost(self):
        observed, _ = monitor.summarize({'at': 'next', 'rows': [row(103), row(102), row(101)]},
                                        {'initialized': True, 'seen_ids': [103, 101]})
        self.assertEqual(observed['totals']['requests'], 1)
        self.assertEqual(observed['groups'][0]['count'], 1)

    def test_rate_errors_counted_but_not_repaired_or_mixed_into_groups(self):
        observed, _ = monitor.summarize({'at': 'next', 'rows': [row(3, http='429'), row(2, status='ok', http='200'), row(1)]},
                                        {'initialized': True, 'seen_ids': [1]})
        self.assertEqual(observed['totals']['rate_errors'], 1)
        self.assertEqual(observed['totals']['successful'], 1)
        self.assertEqual(observed['groups'], [])

    def test_loss_of_full_overlap_is_reported_not_called_complete(self):
        old_limit = monitor.SCAN_ROWS
        try:
            monitor.SCAN_ROWS = 2
            observed, _ = monitor.summarize({'at': 'next', 'rows': [row(5), row(4)]}, {'initialized': True, 'seen_ids': [1, 2]})
            self.assertEqual(observed['coverage'], 'gap_detected')
        finally:
            monitor.SCAN_ROWS = old_limit


if __name__ == '__main__':
    unittest.main()
