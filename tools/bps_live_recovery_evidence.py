"""Verify one same-source 429 recovery and the three omitted live suites.

This preserves both original logs and their real exit codes. It cannot waive
unresolved failures, accept changed runtime/tests, or turn a failed run green.
"""
import hashlib
import json
from pathlib import Path
import re


def require(value, message):
    if not value:
        raise RuntimeError(message)


def test_results(log):
    return {name: status for status, name in re.findall(r'^\s*--- (PASS|FAIL|SKIP): (\S+) ', log, re.M)}


def verify_logs(original_log, recovery_log):
    original, recovery = test_results(original_log), test_results(recovery_log)
    failed = {name for name, status in original.items() if status == 'FAIL'}
    require(failed == {'TestBPSLiveCodexIntegration', 'TestBPSLiveCodexIntegration/apply_patch'},
            'Original failure is not the reviewed single apply_patch case')
    require('\nFAIL\n' in original_log, 'Original failed termination missing')
    require('\nPASS\n' in recovery_log and all(status == 'PASS' for status in recovery.values()),
            'Supplement contains an unresolved failure or skipped test')
    cases = []
    for line in original_log.splitlines():
        start = line.find('{"case":"apply_patch"')
        if start >= 0:
            cases.append(json.loads(line[start:]))
    require(len(cases) == 1, 'Original apply_patch observation must be unique')
    case = cases[0]
    errors = case.get('cli', {}).get('errors') or []
    require(case.get('passed') is False and errors and all('exceeded retry limit, last status: 429 Too Many Requests' in error for error in errors),
            'Original failure does not prove the observed upstream 429')
    require(case['cli'].get('timed_out') is False and case['cli'].get('exit_code') == 1,
            'Unexpected original CLI failure')
    metrics = case.get('gateway_metrics') or []
    require(any(row.get('http_code') == 429 and row.get('error_source') == 'upstream' for row in metrics),
            'Upstream rate-limit observation missing')
    require(all(row.get('status') == 'ok' or row.get('http_code') == 429 and row.get('error_source') == 'upstream' for row in metrics),
            'Original scenario contains another unresolved gateway failure')
    basic = ['TestBPSLiveCodexIntegration/'+name for name in ('file','resume','compact','search','mcp_parallel','candy_account_1','candy_account_2')]
    basic += ['TestBPSLiveCodexWebSocket/'+name for name in ('file','resume','compact')]
    basic += ['TestBPSLiveSharedAPIKeyCache','TestBPSLiveCodexWebSocket','TestBPSLiveCompatibleModelRoutes','TestBPSLiveStructuredOutput']
    basic += ['TestBPSLiveCompatibleModelRoutes/'+name for name in ('gpt-5.5','codex-auto-review','gpt-reserve','gpt-5.5-codex','unavailable-client-text-model')]
    additional = ['TestBPSLiveCodexIntegration','TestBPSLiveCodexIntegration/apply_patch',
                  'TestBPSLiveInlineImageAttachment','TestBPSLiveToolInlineImageAttachment','TestBPSLiveStructuredCLI',
                  'TestBPSLiveStructuredCLI/official_schema_sse_tool_workflow','TestBPSLiveStructuredCLI/official_schema_ws_tool_workflow']
    require(all(original.get(name) == 'PASS' for name in basic), 'Original passing coverage incomplete')
    require(all(recovery.get(name) == 'PASS' for name in additional), 'Supplement passing coverage incomplete')
    # No extra model cases are allowed to hide an expanded, repeated full run.
    require(set(recovery) == set(additional), 'Unexpected supplementary test scope')
    return {'passed': True, 'original_run_passed': False, 'original_failure': 'upstream_http_429',
            'recovered_case': 'TestBPSLiveCodexIntegration/apply_patch', 'base_passed_cases': basic,
            'supplement_passed_cases': additional, 'missing_live_suites_completed': additional[2:5]}


def verify(original_folder, recovery_folder, source_files):
    original_folder, recovery_folder = Path(original_folder), Path(recovery_folder)
    results, logs, files = [], [], []
    for folder in (original_folder, recovery_folder):
        result = json.loads((folder/'result.json').read_text(encoding='utf-8'))
        tested = json.loads((folder/'source.json').read_text(encoding='utf-8'))
        require(tested == source_files, 'Live source/runtime/test manifest changed')
        require(hashlib.sha256((folder/'runtime.test').read_bytes()).hexdigest() == result.get('binary_sha256'), 'Live binary digest differs')
        results.append(result)
        logs.append((folder/'live.log').read_text(encoding='utf-8'))
        files += [folder/'result.json',folder/'source.json',folder/'live.log']
    require(results[0].get('exit_code') == 1 and results[1].get('exit_code') == 0, 'Actual live run exit codes changed')
    require(results[0]['binary_sha256'] == results[1]['binary_sha256'], 'Live binary changed between attempts')
    summary = verify_logs(*logs)
    summary.update(original_folder=str(original_folder), recovery_folder=str(recovery_folder), binary_sha256=results[0]['binary_sha256'])
    return files, summary
