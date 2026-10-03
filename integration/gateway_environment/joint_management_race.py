"""Run the delivered management selection against a newer frozen joint source.

Uses the existing local-only Docker runner, with independent PG18/Redis8 and
bounded ephemeral resources. Does not read production environments or use SSH.
"""
import hashlib
import json
from pathlib import Path
import re
import sys

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / 'tools'))
import gateway_local_race as runner

BASELINE = ROOT / '.local/management-parity/linux-race-20260917T214152-2d29dc/report.json'
BASELINE_SHA = '0f64bbdff85bd6e3d653d9d74813442647a853b024bf726026a0a5b84e1d722b'


def selected(manifest, source_root):
    raw = BASELINE.read_bytes()
    if hashlib.sha256(raw).hexdigest() != BASELINE_SHA:
        raise RuntimeError('Delivered management selection evidence changed')
    baseline = json.loads(raw)
    if baseline['status'] != 'pass' or baseline['skipped_tests'] or baseline['failed_tests']:
        raise RuntimeError('Management selection was not accepted')
    names = set()
    for name, expected in manifest['files'].items():
        if not name.startswith('admin_') or not name.endswith('_test.go'):
            continue
        raw = (source_root / name).read_bytes()
        if hashlib.sha256(raw).hexdigest() != expected:
            raise RuntimeError('Frozen management test bytes changed')
        for test in re.findall(r'^func (Test\w+)\(t \*testing.T\)', raw.decode('utf-8'), re.M):
            if test.startswith(('TestManaged', 'TestPGManaged', 'TestManagementState', 'TestPGManagementState')):
                names.add(test)
    required = set(baseline['selected_top_level_tests'])
    if not required <= names:
        raise RuntimeError('Joint source lost delivered management coverage')
    return sorted(names)


if __name__ == '__main__':
    runner.selected_tests = selected
    raise SystemExit(runner.main())
