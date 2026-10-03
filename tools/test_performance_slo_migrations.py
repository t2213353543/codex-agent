import unittest

import performance_slo


class AllowedMigrationDeltaTests(unittest.TestCase):
    def setUp(self):
        self.expected = {
            'migrations/000_core.sql': '0' * 64,
            performance_slo.ALLOWED_MIGRATION_DELTA_NAME:
                performance_slo.ALLOWED_MIGRATION_DELTA_SHA256,
        }
        self.actual = {
            'migrations/000_core.sql': '0' * 64,
        }

    def test_accepts_only_pinned_018_as_pending(self):
        self.assertEqual(
            performance_slo.validate_allowed_migration_delta(self.actual, self.expected),
            [performance_slo.ALLOWED_MIGRATION_DELTA_NAME],
        )

    def test_rejects_changed_applied_checksum(self):
        actual = dict(self.actual)
        actual['migrations/000_core.sql'] = '1' * 64
        with self.assertRaisesRegex(RuntimeError, 'checksum differs'):
            performance_slo.validate_allowed_migration_delta(actual, self.expected)

    def test_rejects_unknown_applied_migration(self):
        actual = dict(self.actual)
        actual['migrations/999_unknown.sql'] = '2' * 64
        with self.assertRaisesRegex(RuntimeError, 'checksum differs'):
            performance_slo.validate_allowed_migration_delta(actual, self.expected)

    def test_rejects_any_other_pending_migration(self):
        expected = dict(self.expected)
        expected['migrations/019_new.sql'] = '3' * 64
        with self.assertRaisesRegex(RuntimeError, 'not exactly 018'):
            performance_slo.validate_allowed_migration_delta(self.actual, expected)

    def test_rejects_unreviewed_018_source_digest(self):
        expected = dict(self.expected)
        expected[performance_slo.ALLOWED_MIGRATION_DELTA_NAME] = '4' * 64
        with self.assertRaisesRegex(RuntimeError, 'pinned reviewed digest'):
            performance_slo.validate_allowed_migration_delta(self.actual, expected)


if __name__ == '__main__':
    unittest.main()
