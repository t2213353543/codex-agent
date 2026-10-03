"""Pure lifecycle timestamp checks, including production Python 3.9 precision."""
from datetime import datetime,timezone
import importlib.util
from pathlib import Path
import unittest

ROOT=Path(__file__).resolve().parents[1]
spec=importlib.util.spec_from_file_location('zero_downtime_lifecycle_review',ROOT/'deploy/zero_downtime_release.py')
a=importlib.util.module_from_spec(spec);spec.loader.exec_module(a)

class LifecycleTimestampTests(unittest.TestCase):
    def test_rfc3339nano_all_fractional_widths_normalize_without_rounding(self):
        for fraction in ('1','12','123','1234','12345','123456','1234567','12345678','123456789','999999999'):
            with self.subTest(fraction=fraction):
                expected=int(fraction.ljust(6,'0')[:6])
                self.assertEqual(a.parse_lifecycle_timestamp('2026-09-29T01:02:03.'+fraction+'Z'),datetime(2026,9,29,1,2,3,expected,tzinfo=timezone.utc))
    def test_seconds_and_explicit_offsets(self):
        self.assertEqual(a.parse_lifecycle_timestamp('2026-09-29T01:02:03Z'),datetime(2026,9,29,1,2,3,tzinfo=timezone.utc))
        self.assertEqual(a.parse_lifecycle_timestamp('2026-09-29T09:02:03.1234+08:00').astimezone(timezone.utc),datetime(2026,9,29,1,2,3,123400,tzinfo=timezone.utc))
    def test_invalid_or_timezone_free_values_are_rejected(self):
        for value in (None,17,'2026-09-29T01:02:03','2026-09-29T01:02:03.Z','2026-09-29T01:02:03.1234567890Z','2026-13-29T01:02:03Z','2026-09-29T01:02:03+25:00','PRIVATE_VALUE'):
            with self.subTest(value=value):
                with self.assertRaisesRegex(a.Blocked,'invalid runtime lifecycle timestamp'):a.parse_lifecycle_timestamp(value)

if __name__=='__main__':unittest.main()
