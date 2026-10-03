"""Run retained gateway and management race suites on one local frozen source.

Uses existing local compiler/PG/Redis images without downloads or production
access. The gateway selector preserves its explicit native-runtime exclusions;
this combines its established regression scope with all passive/management tests.
"""
from pathlib import Path

import gateway_source_race
import management_local_race
from management_parity_race import management_tests


def combined_tests(manifest, source_root=None):
    root = Path(source_root or gateway_source_race.ROOT)
    return sorted(set(management_tests(manifest, root)) |
                  set(gateway_source_race.selected_tests(manifest, root)))


if __name__ == '__main__':
    management_local_race.management_tests = combined_tests
    raise SystemExit(management_local_race.main())
