import json
from pathlib import Path
import tempfile
import unittest

from performance_phase3_report import aggregate, inspect, render_markdown, summary


class PerformanceReportTests(unittest.TestCase):
    def inspect(self, **changes):
        value = {'status': 'pass', 'source_sha256': 'source-a', 'binary_sha256': 'binary-a', 'slo_ms': 1000,
                 'events': {'FIRST': [{'name': 'test', 'path': '/test', 'sample': {'ms': 3, 'status': 200}}]}}
        value.update(changes)
        with tempfile.TemporaryDirectory() as folder:
            path = Path(folder) / 'report.json'
            path.write_text(json.dumps(value), encoding='utf-8')
            return inspect(path)

    def test_empty_is_not_pass(self):
        self.assertEqual(summary([], 1000)['status'], 'fail')
        self.assertEqual(aggregate([])['status'], 'fail_or_incomplete')
        self.assertEqual(aggregate([self.inspect(events={})])['status'], 'fail_or_incomplete')

    def test_strict_threshold_and_http_status(self):
        for sample in ({'ms': 1000, 'status': 200}, {'ms': 1, 'status': 302}, {'ms': 1}):
            self.assertEqual(summary([sample], 1000)['status'], 'fail')
        self.assertEqual(summary([{'ms': 999.9, 'status': 204}], 1000)['status'], 'pass')

    def test_invalid_times_and_thresholds(self):
        for value in (-1, float('inf'), float('nan'), True, '1', None):
            self.assertEqual(summary([{'ms': value, 'status': 200}], 1000)['status'], 'fail')
        for threshold in (0, -1, float('nan'), '1000', True):
            self.assertEqual(summary([{'ms': 1, 'status': 200}], threshold)['status'], 'fail')

    def test_source_and_binary_mismatch_blocks_pass(self):
        first = self.inspect()
        self.assertEqual(aggregate([first])['status'], 'pass')
        for change in ({'source_sha256': 'source-b'}, {'binary_sha256': 'binary-b'}, {'source_sha256': None}):
            self.assertEqual(aggregate([first, self.inspect(**change)])['status'], 'fail_or_incomplete')

    def test_functional_only_is_not_slo_and_retains_errors(self):
        result = self.inspect(functional_only=True)
        self.assertEqual(result['groups'][0]['status'], 'functional_only_not_slo')
        self.assertFalse(result['evidence_valid'])
        result = self.inspect(events={'FIRST': [{'name': 'test', 'path': '/test', 'functional_only': True,
                                               'slo_pass': False, 'sample': {'ms': 1, 'status': 500}}]})
        self.assertEqual(result['groups'][0]['status'], 'functional_fail_not_slo')

    def test_reported_failure_not_hidden_by_fast_samples(self):
        result = self.inspect(events={'RESULT': [{'name': 'test', 'path': '/test', 'concurrency': 1,
                                                'failed': 1, 'samples': [{'ms': 1, 'status': 200}]}]})
        self.assertEqual(result['groups'][0]['status'], 'fail')

    def test_duplicate_groups_extend_not_overwrite(self):
        item = {'name': 'test', 'path': '/test', 'concurrency': 1, 'samples': [{'ms': 1001, 'status': 200}]}
        good = dict(item, samples=[{'ms': 1, 'status': 200}])
        result = self.inspect(events={'RESULT': [item, good]})
        self.assertEqual(result['groups'][0]['samples'], 2)
        self.assertEqual(result['groups'][0]['status'], 'fail')

    def test_missing_http_threshold_fails(self):
        self.assertFalse(self.inspect(slo_ms=None)['evidence_valid'])

    def test_empty_result_is_recorded(self):
        result = self.inspect(events={'RESULT': [{'name': 'empty', 'path': '/test', 'concurrency': 1, 'samples': []}]})
        self.assertEqual(result['groups'][0]['samples'], 0)
        self.assertFalse(result['evidence_valid'])
        self.assertIn('empty', render_markdown(aggregate([result])))

    def test_maintenance_only_not_slo(self):
        maintenance = self.inspect(events={'MAINTENANCE_COMPLETE': [{'elapsed_s': 5}]})
        self.assertTrue(maintenance['evidence_valid'])
        self.assertEqual(aggregate([maintenance])['status'], 'fail_or_incomplete')
        self.assertEqual(aggregate([maintenance, self.inspect()])['status'], 'pass')

    def test_skip_or_failure_blocks_pass(self):
        for values in ({'skipped_tests': ['TestSkip']}, {'failed_tests': ['TestFail']}, {'status': 'running'}):
            self.assertFalse(self.inspect(**values)['evidence_valid'])

    def test_cost_experiment_separate_and_paired(self):
        events = {'MODEL_WRITE_COST': [
            {'model_enabled': enabled, 'repetition': 0, 'concurrency': 1, 'samples_ms': [2 if enabled else 1], 'wall_ms': 2}
            for enabled in (False, True)]}
        result = self.inspect(events=events)
        self.assertEqual(result['evidence_kind'], 'write_cost_only_not_http')
        self.assertEqual(result['model_write_cost_comparisons'][0]['p50_ratio'], 2)
        self.assertEqual(aggregate([result])['status'], 'fail_or_incomplete')
        events['MODEL_WRITE_COST'].pop()
        self.assertFalse(self.inspect(events=events)['evidence_valid'])


if __name__ == '__main__':
    unittest.main()
