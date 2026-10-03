"""Run remaining database tests from the exact joint candidate's skipped list."""
import hashlib
import json
from pathlib import Path
import sys

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / 'tools'))
import gateway_local_race as runner

JOINT = ROOT / '.local/joint-candidate/20260918T024841.878837Z/joint-candidate.json'


def read_role(candidate, name):
    entry = candidate['evidence_roles'][name]
    path = ROOT / entry['path']
    raw = path.read_bytes()
    if hashlib.sha256(raw).hexdigest() != entry['sha256']:
        raise RuntimeError('Joint evidence changed: ' + name)
    return json.loads(raw), path.parent


def selected(manifest, source_root):
    candidate = json.loads(JOINT.read_bytes())
    if candidate['go_source_sha256'] != manifest['sha256']:
        raise RuntimeError('Storage gap selection targets a different candidate')
    local, folder = read_role(candidate, 'local_verification')
    tests = next(row for row in local['checks'] if row['label'] == 'test')
    raw = (folder / tests['stdout']).read_bytes()
    if hashlib.sha256(raw).hexdigest() != tests['stdout_sha256']:
        raise RuntimeError('Full test raw log changed')
    skipped = set()
    for line in raw.splitlines():
        event = json.loads(line)
        if event.get('Action') == 'skip' and event.get('Test'):
            name = event['Test'].split('/')[0]
            if name.startswith('TestPG') or name.startswith('TestAdminKeyPG') or 'Redis' in name:
                skipped.add(name)
    already_passed = set()
    for role in ('gateway_race', 'management_race'):
        report, _ = read_role(candidate, role)
        if (report['status'] != 'pass' or report['failed_tests'] or report['skipped_tests']
                or report['go_source_sha256'] != manifest['sha256']):
            raise RuntimeError('Cannot subtract unproven prior coverage')
        already_passed.update(report['selected_top_level_tests'])
    gaps = skipped - already_passed
    if not gaps or 'TestPGShutdownPersistsRefreshAfterClientDisconnectAndSIGTERM' not in gaps:
        raise RuntimeError('Expected process-exit database boundary is missing from gap selection')
    return sorted(gaps)


if __name__ == '__main__':
    runner.selected_tests = selected
    raise SystemExit(runner.main())
