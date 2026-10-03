"""Run the remaining phase-three diagnostics on unchanged af427 business code.

python tools/performance_phase4_diagnostics.py --output .local/performance-phase4/diagnostics-af427

The only source deltas are one new planner diagnostic and the corrected recent
fixture alias. Reports retain their own source/binary hashes and original SLO
failures are not overwritten. All remote work is isolated synthetic PG work.
"""
from __future__ import annotations

import argparse
import datetime as dt
import hashlib
import json
from pathlib import Path
import re
import shutil
import subprocess
import sys

from integration_env import ROOT, REMOTE
from performance_phase3_matrix import quiet_preflight


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--output', type=Path, required=True)
    parser.add_argument('--resume', action='store_true', help='Skip terminal reports, preserving failures; never restart a nonterminal job')
    args = parser.parse_args()
    output = args.output.resolve()
    output.mkdir(parents=True, exist_ok=args.resume)
    canonical = ROOT / '.local/performance-slo/20260929t013010-3bcf96'
    fixture = ROOT / '.local/performance-slo/20260928t234235-e4ee04/report.json'
    source = output / 'source'
    changed = ['internal/server/accounts_import_plan_test.go', 'internal/server/performance_operations_recent_test.go']
    if not source.exists():
        shutil.copytree(canonical / 'source', source)
        for name in changed:
            shutil.copy2(ROOT / name, source / name)
    baseline = json.loads((canonical / 'source-manifest.json').read_text(encoding='utf-8'))['files']
    unexpected = [name for name, digest in baseline.items() if name not in changed and hashlib.sha256((source / name).read_bytes()).hexdigest() != digest]
    if unexpected:
        raise RuntimeError('unexpected canonical source delta: ' + ','.join(unexpected))
    state = json.loads((output / 'handoff.json').read_text(encoding='utf-8')) if args.resume and (output / 'handoff.json').exists() else {'status': 'prepared', 'production_touched': False, 'business_source': 'af427ea3f5da32368f1e4b46a611ad36f19c3bed458abec100101ce2b6b9941c',
             'test_deltas': changed, 'source': str(source), 'runs': [], 'all_routes_slo_proven': False}
    def save():
        state['updated_at_utc'] = dt.datetime.now(dt.timezone.utc).isoformat()
        (output / 'handoff.json').write_text(json.dumps(state, ensure_ascii=False, indent=2), encoding='utf-8')
    save()
    def remote_binary(report):
        if report.get('remote_binary'):
            return report['remote_binary']
        stamp = report.get('recorded_at_utc', '')
        if not re.fullmatch(r'\d{8}t\d{6}-[a-f0-9]{6}', stamp) or not report.get('binary_sha256'):
            raise RuntimeError('no verified binary location to reuse')
        # Older runner reports omitted the path for newly built binaries.
        # The reused-binary runner verifies this exact path's SHA before use.
        return REMOTE + '/checks/performance-slo-' + stamp + '/server.test'
    previous = None
    for name, selector, samples in [('bulk_plans', '^TestPerformanceAccountImportPlan$', 1), ('recent_operations100k', '^TestPerformanceOperationsRecentScale$', 20)]:
        report = output / name / 'report.json'
        if report.exists():
            result = json.loads(report.read_text(encoding='utf-8'))
            if args.resume and result.get('status') in ('pass', 'fail') and 'exit_code' in result:
                previous = report
                for entry in state['runs']:
                    if entry['name'] == name:
                        entry.update(status=result['status'], resumed_terminal=True)
                save()
                continue
            raise RuntimeError('existing nonterminal report requires live process inspection, not restart: ' + str(report))
        preflight = quiet_preflight()
        command = [sys.executable, str(ROOT / 'tools/performance_slo.py'), '--reuse', str(fixture), '--test-run', selector,
                   '--samples', str(samples), '--timeout-minutes', '90', '--output', str(output / name)]
        if previous is None:
            command += ['--source', str(source), '--exact-source']
        else:
            command += ['--binary-report', str(previous), '--remote-binary', remote_binary(json.loads(previous.read_text(encoding='utf-8')))]
        entry = {'name': name, 'status': 'running', 'preflight': preflight, 'command': command}
        state['status'] = 'running'
        state['runs'].append(entry)
        save()
        print(json.dumps({'start': name, 'output': str(output / name)}, ensure_ascii=False), flush=True)
        entry['exit_code'] = subprocess.run(command, cwd=ROOT).returncode
        if not report.exists():
            entry['status'] = state['status'] = 'runner_failed_without_report'
            save()
            raise RuntimeError('runner ended without report')
        result = json.loads(report.read_text(encoding='utf-8'))
        entry.update(status=result['status'], report=str(report), source_sha256=result.get('source_sha256'), binary_sha256=result.get('binary_sha256'))
        save()
        remote_binary(result)
        previous = report
    state['status'] = 'finished'
    state['final_preflight'] = quiet_preflight()
    save()


if __name__ == '__main__':
    main()
