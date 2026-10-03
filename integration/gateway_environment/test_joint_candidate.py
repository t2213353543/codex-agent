"""Pure synthetic tests for joint-candidate ownership and log accounting."""
import importlib.util
import json
from pathlib import Path
import unittest

SPEC = importlib.util.spec_from_file_location('joint_under_test', Path(__file__).with_name('joint_candidate.py'))
joint = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(joint)


class JointCandidateTests(unittest.TestCase):
    def test_management_preserved_gateway_only_overlay(self):
        base = {'admin.go': 'a', 'stats.go': 'b', 'gateway.go': 'c'}
        overlay = {'gateway.go': 'd', 'gateway_observation_transport.go': 'e'}
        result = joint.delta(base, {**base, **overlay}, overlay)
        self.assertEqual(result['modified'], ['gateway.go'])
        self.assertEqual(result['added'], ['gateway_observation_transport.go'])
        self.assertTrue(result['management_and_stats_preserved'])

    def test_management_mutation_or_undeclared_file_rejected(self):
        base = {'admin.go': 'a', 'gateway.go': 'b'}
        for target, overlay in (({'admin.go': 'c', 'gateway.go': 'b'}, {'admin.go': 'c'}),
                ({**base, 'other.go': 'd'}, {}), ({'gateway.go': 'b'}, {})):
            with self.subTest(target=target), self.assertRaises(ValueError):
                joint.delta(base, target, overlay)

    def test_log_summary_keeps_skips_distinct(self):
        rows = [{'Test': 'TestOne', 'Action': 'pass'}, {'Test': 'TestTwo/storage', 'Action': 'skip'},
            {'Test': 'TestTwo', 'Action': 'pass'}, {'Action': 'pass', 'Package': 'synthetic'}]
        result = joint.summary('\n'.join(json.dumps(row) for row in rows))
        self.assertEqual(result['top_level'], {'pass': 2, 'fail': 0, 'skip': 0})
        self.assertEqual(result['events'], {'pass': 2, 'fail': 0, 'skip': 1})
        self.assertFalse(result['data_race'])

    def test_compact_digest_is_not_default_json_alias(self):
        files = {'a.go': 'a' * 64}
        self.assertNotEqual(joint.owner_digest(files), joint.sha(json.dumps(files, sort_keys=True).encode()))


if __name__ == '__main__':
    unittest.main()
