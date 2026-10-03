"""Build a read-only route coverage ledger from actual HTTP evidence.

python tools/performance_route_coverage.py --reports <report.json> <report.json>
This script does not connect to PostgreSQL, SSH or any external service.
"""
from __future__ import annotations

import argparse
import csv
import hashlib
import json
from pathlib import Path
import re
from urllib.parse import urlsplit

from integration_env import ROOT
from performance_slo import inventory


GAPS = {
    'handleSessionGet': ['cookie-authenticated session and expiry; malformed/revoked credential latency'],
    'handleAccountDetail': ['account with many bindings/tags; current failed credentials; missing ID'],
    'handleManagedAccounts': ['deep page, status/name/priority sorting, text/group/proxy/privacy filters; large account count'],
    'handleManagedAccountMetadata': ['fully populated compatibility/identity metadata; missing ID'],
    'handleManagedAccountRuntime': ['Redis configured/unavailable and populated cooldowns; requested model'],
    'handleManagedUpstreamSnapshot': ['saved usage/reset snapshots and local-window-cost enrichment; stale credentials'],
    'handleManagedAutoResetGet': ['enabled policy with recent error/history; missing policy'],
    'handleManagedScheduleList': ['max-size per-account document; missing schedules'],
    'handleManagedAccountLeaseGet': ['expired/provenance-rich lease; no lease'],
    'handleManagedScheduleResults': ['running lease and error snapshot payload; missing schedule'],
    'handleManagedBundleLeaseReceipt': ['large receipt item count; wrong owner; expired receipt'],
    'handleManagedOAuthGet': ['ready with staged credentials/identity comparison; applied/canceled/expired/wrong owner'],
    'handleOverview': ['include_breakdown=true; custom bounds/step; high cardinality and missing metrics'],
    'handleDashboardRealtime': ['populated ring at many concurrent streams; quiet empty ring'],
    'handleDiagnostics': ['Redis unavailable/timeouts; PostgreSQL unavailable within HTTP budget'],
    'handleOperations': ['15m/1h/6h windows; large category counts; concurrent pruning'],
    'handleAuditLogs': ['time/action/entity filters; cursor/deep history; long entity strings'],
    'handleAccountsList': ['cursor second page; group/proxy/status/enabled filters; large account population'],
    'handleAccountRequests': ['cursor, time bounds, exact_total and sort variations; high-volume one account'],
    'handleGroupsList': ['large group count and skewed many-account memberships'],
    'handleKeysList': ['10000+ keys and large group memberships; simultaneous key rotation'],
    'handleProxiesList': ['large proxy count and skewed memberships'],
    'handleManagedLogs': ['combined filters; cursor follow-up; all optional sorts; resolved snapshots; high selectivity misses'],
    'handleManagedIngressEvents': ['full ring; alternate limit; populated failures'],
    'handleManagedLogStats': ['combined model/account/group filters; multiple calendar zones/DST; full dimension high cardinality'],
    'handleManagedLogDetail': ['numeric primary ID; missing ID; large optional observation payload'],
    'handleSettingsGet': ['large builtin prompt and route lists; settings read failure'],
    'handleCPAList': ['large account population/compat payloads; preserved filename collisions; bound proxies'],
    'sub2ListAccounts': ['search/privacy/group/status filters; deep page and all supported sorts; high account count'],
    'sub2GetAccount': ['populated compat credentials metadata and bindings; missing/conflicting numeric ID'],
    'sub2Resources': ['high resource count; search/status/protocol filters; missing/conflicting numeric ID'],
    'handleManagedCleanupList': ['many completed/active cleanup jobs; deep page'],
    'handleManagedUsageErrors': ['resolved=true; many populated reviews; cursor invalidation; severity/time filters'],
    'handleManagedUsageReview': ['populated history; missing/deleted log and wrong identity'],
    'handleManagedUsageCoverage': ['after large cleanup/truncate; empty retained set; independent long-lived events'],
    'handleManagedUsageOptions': ['field=key/group; q search/current rename/deleted labels; limit=200; combined filters'],
    'http.HandlerFunc(p.health)': ['GET /health success; non-GET rejection (outside read scope)'],
}


def match_route(route: dict, path: str) -> bool:
    parts = route['path'].strip('/').split('/')
    actual = path.strip('/').split('/')
    if len(parts) != len(actual):
        return False
    return all(expected == value or expected.startswith('{') and expected.endswith('}') and bool(value)
               for expected, value in zip(parts, actual))


def specific(route: dict) -> tuple:
    parts = route['path'].strip('/').split('/')
    return (sum(not part.startswith('{') for part in parts), len(parts), route['method'] == 'GET')


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--reports', nargs='+', type=Path, required=True)
    parser.add_argument('--source', type=Path, default=ROOT)
    parser.add_argument('--output', type=Path, default=ROOT/'integration/performance/results/local-get-coverage-20260929')
    parser.add_argument('--exclude-report-case', action='append', default=[], metavar='REPORT_BASENAME:CASE',
                        help='Exclude a superseded shape from a combined report; keep later evidence instead.')
    parser.add_argument('--prefer-later-case', action='store_true',
                        help='For repeated case names use the last supplied report, e.g. post-migration AdditionalReads.')
    args = parser.parse_args()
    exclusions = set()
    for value in args.exclude_report_case:
        folder, separator, case = value.partition(':')
        if not separator or not folder or not case:
            parser.error('--exclude-report-case requires REPORT_BASENAME:CASE')
        exclusions.add((folder, case))
    routes = inventory(args.source.resolve())
    read_routes = [dict(row) for row in routes if row['method'] == 'GET' or row['method'] == 'ANY' and row['path'] == '/health']
    by_key = {(r['method'], r['path']): r for r in read_routes}
    for route in read_routes:
        route.pop('coverage', None)
        route.update(measured=False, unmeasured=True, measurements=[], useful_unmeasured_shapes=GAPS.get(route['handler'], []))
        if route['handler'] == 'handleCPAModels':
            route['category'] = 'upstream_or_streaming'
            route['classification_reason'] = 'handleCPAModels constructs an upstream request and calls client.Do'
        if route['handler'] == 'handleManagedUpstreamSnapshot':
            route['category'] = 'local_read'
            route['classification_reason'] = 'GET reads saved account-upstream management state; POST is upstream work'
        if route['path'] == '/health':
            route['effective_read_method'] = 'GET'
    sources, unmatched = [], []
    latest_cases = {}
    if args.prefer_later_case:
        for path in args.reports:
            data = json.loads(path.read_text(encoding='utf-8'))
            for event in data.get('events',{}).get('FIRST',[]) + data.get('events',{}).get('RESULT',[]):
                if 'path' in event:
                    latest_cases[event['name']] = str(path.resolve())
    for report_path in args.reports:
        report_path = report_path.resolve()
        report = json.loads(report_path.read_text(encoding='utf-8'))
        business_source = report.get('base_source_sha256') or report.get('source_sha256')
        reference = {
            'source_sha256': business_source,
            'basis': 'declared base source with test-only overlay' if report.get('base_source_sha256') else 'effective frozen source',
            'effective_source_sha256': report.get('source_sha256'),
            'test_overlay_sha256': report.get('test_overlay_sha256'),
        }
        sources.append({
            'report': str(report_path), 'report_sha256': hashlib.sha256(report_path.read_bytes()).hexdigest(),
            'status': report.get('status'), 'source_sha256': report.get('source_sha256'),
            'binary_sha256': report.get('binary_sha256'), 'schema': report.get('schema'),
            'samples_per_concurrency': report.get('samples_per_concurrency'),
            'business_source_reference': reference,
            'measurement_scope': report.get('scope'),
            'runtime_environment': 'Windows loopback, no database' if report.get('base_source_sha256') and 'Windows' in report.get('scope','') else 'Linux container loopback with isolated PostgreSQL' if report.get('driver_resources') else 'see measurement scope',
            'excluded_superseded_cases': sorted({case for folder,case in exclusions if folder == report_path.parent.name} |
                                                {event['name'] for event in report.get('events',{}).get('FIRST',[]) if args.prefer_later_case and latest_cases.get(event['name']) != str(report_path)}),
            'note': 'Read evidence only; measurement is bound to this report/source, not automatically to current implementation.',
        })
        evidence = {}
        for first in report.get('events', {}).get('FIRST', []):
            if 'path' not in first:
                continue
            evidence.setdefault((first['name'], first['path']), {'first': first, 'results': []})
        for result in report.get('events', {}).get('RESULT', []):
            if 'path' not in result:
                continue
            evidence.setdefault((result['name'], result['path']), {'results': []})['results'].append(result)
        for (name, path), events in evidence.items():
            if (report_path.parent.name, name) in exclusions:
                continue
            if args.prefer_later_case and latest_cases.get(name) != str(report_path):
                continue
            # Match the path without its query. An exact static registration
            # owns /manage, /errors, /options etc before any {id} wildcard.
            matched = [row for row in read_routes if match_route(row, urlsplit(path).path)]
            if not matched:
                unmatched.append({'report': str(report_path), 'name': name, 'path': path})
                continue
            matched.sort(key=specific, reverse=True)
            if len(matched)>1 and specific(matched[0]) == specific(matched[1]):
                raise ValueError(f'ambiguous route evidence {path}: {matched[:2]}')
            row = by_key[(matched[0]['method'], matched[0]['path'])]
            result = {'case': name, 'request_method': 'GET', 'request_path': path,
                      'report': str(report_path), 'source_sha256': report.get('source_sha256'),
                      'business_source_reference': reference,
                      'measurement_scope': report.get('scope'),
                      'report_status': report.get('status'), 'concurrency_levels': [], 'sample_count': 0,
                      'all_captured_samples_pass': True}
            if 'first' in events:
                first = events['first']; sample = first.get('sample', {})
                result['sample_count'] += 1
                result['first_ms'] = sample.get('ms')
                result['all_captured_samples_pass'] &= first.get('slo_pass', False)
            for item in events['results']:
                result['concurrency_levels'].append(item.get('concurrency'))
                result['sample_count'] += len(item.get('samples', []))
                result['all_captured_samples_pass'] &= not item.get('failed', 0) and not item.get('over_slo', 0)
            result['request_count_is_complete'] = sorted(result['concurrency_levels']) == [1,4] and 'first' in events
            row['measurements'].append(result)
            row['measured'], row['unmeasured'] = True, False
    if unmatched:
        raise ValueError('unmatched actual HTTP paths: '+json.dumps(unmatched, ensure_ascii=False))
    read_routes.sort(key=lambda r:(r['category'],r['path']))
    local = [row for row in read_routes if row['category'] == 'local_read']
    output = args.output.resolve(); output.parent.mkdir(parents=True, exist_ok=True)
    payload = {
        'scope': 'GET management registrations and effective GET /health; gateway protocol dispatch and static files are separately excluded.',
        'route_matching': 'exact segment count, static segment specificity before dynamic placeholder, one concrete request belongs to exactly one route',
        'current_registration_count': len(routes), 'local_get_count': len(local),
        'local_get_measured_count': sum(row['measured'] for row in local),
        'local_get_unmeasured_count': sum(row['unmeasured'] for row in local),
        'all_parameter_shapes_proven': False, 'mixed_sources': len({source['source_sha256'] for source in sources}) > 1,
        'business_source_reference': {
            'source_sha256_values': sorted({source['business_source_reference']['source_sha256'] for source in sources}),
            'same_business_source_reference': len({source['business_source_reference']['source_sha256'] for source in sources}) == 1,
            'same_effective_binary_asserted': False,
            'note': 'A test-only overlay may share the business-source reference while retaining a different effective source hash and runtime environment.',
        },
        'source_reports': sources, 'routes': read_routes,
        'registration_files_sha256': {str(path.relative_to(args.source.resolve())):hashlib.sha256(path.read_bytes()).hexdigest()
                                     for path in sorted((args.source.resolve()/'internal/server').glob('*.go'))
                                     if not path.name.endswith('_test.go') and ('mux.HandleFunc(' in path.read_text(encoding='utf-8') or path.name=='web.go')},
    }
    output.with_suffix('.json').write_text(json.dumps(payload, ensure_ascii=False, indent=2)+'\n', encoding='utf-8')
    with output.with_suffix('.csv').open('w', encoding='utf-8-sig', newline='') as handle:
        writer = csv.DictWriter(handle, fieldnames=['method','route','handler','category','measured','unmeasured','actual_request_paths','cases','reports','effective_source_sha256','business_source_sha256','measurement_scopes','useful_unmeasured_shapes'])
        writer.writeheader()
        for row in read_routes:
            writer.writerow({'method': row.get('effective_read_method', row['method']), 'route':row['path'], 'handler':row['handler'], 'category':row['category'],
                             'measured':str(row['measured']).lower(),'unmeasured':str(row['unmeasured']).lower(),
                             'actual_request_paths':'\n'.join(sorted({x['request_path'] for x in row['measurements']})),
                             'cases':'\n'.join(sorted({x['case'] for x in row['measurements']})),
                             'reports':'\n'.join(sorted({x['report'] for x in row['measurements']})),
                             'effective_source_sha256':'\n'.join(sorted({x['source_sha256'] for x in row['measurements']})),
                             'business_source_sha256':'\n'.join(sorted({x['business_source_reference']['source_sha256'] for x in row['measurements']})),
                             'measurement_scopes':'\n'.join(sorted({x['measurement_scope'] for x in row['measurements']})),
                             'useful_unmeasured_shapes':'\n'.join(row['useful_unmeasured_shapes'])})
    print(json.dumps({key:payload[key] for key in ['current_registration_count','local_get_count','local_get_measured_count','local_get_unmeasured_count','mixed_sources']},ensure_ascii=False))
    print('unmeasured_local_routes='+json.dumps([row['path'] for row in local if row['unmeasured']],ensure_ascii=False))


if __name__ == '__main__':
    main()
