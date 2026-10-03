"""Offline source identity regressions for composed release acceptance."""
import unittest

from reliability_acceptance import powerpoint_fixtures, require_runtime_sources, source_differences


class ReliabilityAcceptanceSourceTests(unittest.TestCase):
    def test_deleted_test_is_a_change_for_full_and_integration_gates(self):
        current={'internal/server/gateway.go':'same'}
        tested={**current,'internal/server/deleted_test.go':'tested'}
        self.assertEqual(source_differences(tested,current),{'internal/server/deleted_test.go'})
        with self.assertRaisesRegex(RuntimeError,'fixture changed'):
            require_runtime_sources(tested,current)
        self.assertNotEqual(powerpoint_fixtures(tested),powerpoint_fixtures(current))

    def test_added_and_modified_fixtures_invalidate_both_directions(self):
        tested={'internal/server/gateway.go':'same','internal/server/fixture_test.go':'before'}
        current={**tested,'internal/server/fixture_test.go':'after','internal/server/added_test.go':'new'}
        expected={'internal/server/fixture_test.go','internal/server/added_test.go'}
        self.assertEqual(source_differences(tested,current),expected)
        self.assertEqual(source_differences(current,tested),expected)
        with self.assertRaises(RuntimeError):require_runtime_sources(tested,current)

    def test_reviewed_test_exception_never_allows_runtime_changes(self):
        tested={'internal/server/gateway.go':'same','internal/server/gateway_account_failover_test.go':'old'}
        current={**tested,'internal/server/gateway_account_failover_test.go':'new'}
        require_runtime_sources(tested,current)
        current['internal/server/gateway.go']='changed'
        with self.assertRaisesRegex(RuntimeError,'runtime differs'):
            require_runtime_sources(tested,current)

    def test_unrelated_scripts_do_not_change_backend_fixture_scope(self):
        tested={'internal/server/gateway.go':'same','tools/probe.py':'old',
                'internal/server/official_fixture_test.go':'old'}
        current={**tested,'tools/probe.py':'new','internal/server/official_fixture_test.go':'new'}
        self.assertEqual(powerpoint_fixtures(tested),powerpoint_fixtures(current))
        self.assertEqual(source_differences(tested,current),{'internal/server/official_fixture_test.go'})


if __name__ == '__main__':unittest.main()
