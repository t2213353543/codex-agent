"""Index measured route evidence without treating old/narrow runs as full SLO proof."""
import argparse
from collections import Counter, defaultdict
import hashlib
import json
from pathlib import Path
import re
import subprocess
from urllib.parse import urlsplit

from performance_slo import inventory


def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def route_pattern(path):
    parts = []
    for part in path.split('/'):
        if part.startswith('{') and part.endswith('}'):
            parts.append('.*' if part.endswith('...}') else '[^/]+')
        else:
            parts.append(re.escape(part))
    return re.compile('^' + '/'.join(parts) + '$')


def samples(report):
    for kind, values in report.get('events', {}).items():
        if kind not in {'FIRST', 'RESULT', 'FILTER_SAMPLE', 'ACCOUNT_WRITE', 'ADDITIONAL_READ', 'WRITE_RESULT'}:
            continue
        for row in values if isinstance(values, list) else []:
            if not isinstance(row, dict):
                continue
            route = row.get('route') if isinstance(row.get('route'), dict) else {}
            path = row.get('path') or route.get('path')
            if not path:
                continue
            # FIRST/RESULT/FILTER_SAMPLE are the read harness' GET events.
            method = row.get('method') or route.get('method')
            if not method and kind in {'FIRST', 'RESULT', 'FILTER_SAMPLE', 'ADDITIONAL_READ'}:
                method = 'GET'
            if not method:
                continue
            points = row.get('samples') or [row.get('sample', row)]
            points = [v for v in points if isinstance(v, dict) and isinstance(v.get('ms'), (int, float))]
            if not points:
                continue
            threshold = row.get('threshold_ms')
            yield dict(method=method, path=path, case=row.get('name', kind),
                       event=kind, samples=len(points), max_ms=max(v['ms'] for v in points),
                       non_2xx=sum(not 200 <= (v.get('status') or 0) < 300 for v in points),
                       semantic_or_transport_failures=sum(bool(v.get('error')) for v in points),
                       threshold_ms=threshold,
                       over_threshold=sum(v['ms'] >= threshold for v in points) if isinstance(threshold, (int, float)) else None,
                       concurrency=row.get('concurrency'), functional_only=row.get('functional_only', False))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--source', type=Path, required=True)
    parser.add_argument('--release', type=Path, required=True)
    parser.add_argument('--output', type=Path, default=Path('.local/performance-coverage/report.json'))
    args = parser.parse_args()
    root = Path(__file__).resolve().parents[1]
    source = args.source.resolve()
    release_path = args.release.resolve()
    release = json.loads(release_path.read_text(encoding='utf-8'))
    source_manifest = json.loads((release_path.parent / 'source-manifest.json').read_text(encoding='utf-8'))
    for name, expected in source_manifest['files'].items():
        if name.startswith('internal/server/') and name.endswith('.go') and not name.endswith('_test.go'):
            if digest(source / name) != expected:
                raise ValueError('route inventory source differs from specified release: ' + name)
    routes = inventory(source)
    # These writes include an external service operation. Their local portion
    # and upstream wait require separate evidence; route spelling alone misses
    # them in the generic inventory's name-based classification.
    external_handlers = {'handleManagedAccountPrivacy', 'handleManagedAccountResetCredit',
                         'handleManagedOAuthExchange', 'handleManagedOAuthImport',
                         'sub2TestAccount', 'sub2Privacy', 'handleManagedIPGeo'}
    for route in routes:
        compat_external_action = (route['handler'] == 'sub2AccountAction' and
                                  route['path'].rsplit('/', 1)[-1] in {'refresh', 'test', 'set-privacy'})
        if route['handler'] in external_handlers or compat_external_action:
            route['category'] = 'upstream_or_streaming'
            if route['handler'] == 'handleManagedOAuthImport':
                route['scope_note'] = 'session import is local; refresh_token/mobile_refresh_token/pat contact upstream'
    # The health middleware and frontend root are wired outside adminRoutes.
    # Include these explicit paths while leaving dynamic assets out of API counts.
    main_source = (source / 'internal/server/main.go').read_text(encoding='utf-8')
    for path in ('/health/live', '/health/ready'):
        if 'r.URL.Path == "' + path + '"' in main_source:
            routes.append(dict(method='ANY', path=path, handler='runtime health middleware',
                               category='infrastructure', file='internal/server/main.go'))
    routes.append(dict(method='ANY', path='/', handler='staticHandler',
                       category='infrastructure', file='internal/server/web.go'))
    patterns = [(route, route_pattern(route['path'])) for route in routes]
    evidence = defaultdict(list)
    rejected, unmatched = [], []

    def add(value):
        path = urlsplit(value['path']).path
        matched = [(r, p) for r, p in patterns if r['method'] in {value['method'], 'ANY'} and p.fullmatch(path)]
        if not matched:
            unmatched.append(value)
            return
        # Go ServeMux chooses a literal route over a same-length wildcard.
        rank = max(len(r['path']) - sum(len(x) for x in re.findall(r'\{[^}]+\}', r['path'])) for r, _ in matched)
        for route, _ in matched:
            if len(route['path']) - sum(len(x) for x in re.findall(r'\{[^}]+\}', route['path'])) == rank:
                evidence[(route['method'], route['path'])].append(value)

    listed = subprocess.run(['rg', '--files', '.local', '-g', 'report.json'], cwd=root,
                            check=True, capture_output=True, text=True).stdout.splitlines()
    report_count = 0
    for name in listed:
        path = root / name
        relative = path.relative_to(root)
        if len(relative.parts) < 3 or not relative.parts[1].startswith('performance'):
            continue
        if {'source', 'package', 'packages', 'evidence'}.intersection(relative.parts):
            continue
        try:
            report = json.loads(path.read_text(encoding='utf-8'))
        except (ValueError, OSError):
            rejected.append(relative.as_posix())
            continue
        report_count += 1
        for item in samples(report):
            item.update(report=relative.as_posix(), report_sha256=digest(path),
                        report_status=report.get('status'), requested_rows=report.get('requested_rows'),
                        source_sha256=report.get('source_sha256'),
                        binding='exact_source' if report.get('source_sha256') == source_manifest['sha256'] else 'requires_source_binding',
                        scope=report.get('scope'), limitations=report.get('limitations', []))
            add(item)
    verify_path = release_path.parent.parent / 'verify.json'
    if verify_path.exists():
        report = json.loads(verify_path.read_text(encoding='utf-8'))
        if report.get('active_binary_sha256') != release['files']['codex-agent']:
            raise ValueError('production verification belongs to another binary')
        for row in report.get('checks', []):
            add(dict(method='GET', path=row['path'], case='production_read', samples=1,
                     max_ms=row['ms'], non_2xx=int(row['status'] != 200), threshold_ms=1000,
                     over_threshold=int(row['ms'] >= 1000), binding='active_binary',
                     report=str(verify_path.relative_to(root)), report_sha256=digest(verify_path),
                     scope='one server-loopback read per endpoint; not full-route, scale or concurrent acceptance'))
    for route in routes:
        items = evidence[(route['method'], route['path'])]
        route['evidence'] = items
        route['coverage'] = 'current_sample_only' if any(x['binding'] in {'active_binary', 'exact_source'} for x in items) else 'source_binding_pending' if items else 'unmeasured'
    value = dict(schema='performance-route-evidence-index-v1', release_id=release['release_id'],
                 source_sha256=source_manifest['sha256'], routes=routes,
                 route_count=len(routes), coverage=dict(Counter(r['coverage'] for r in routes)),
                 reports_inspected=report_count, rejected_reports=rejected, unmatched=unmatched,
                 full_goal_proven=False,
                 limitations=['Route registrations can share a handler but require distinct auth/input cases.',
                              'Reports with a different source digest need dependency review or a current rerun; a different digest alone does not prove that their runtime implementation is older or changed.',
                              'A 10M log fixture does not prove 10M accounts, all dimensions or all operations.',
                              'Upstream/network/stream completion latency is outside local processing timing.'])
    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(json.dumps(value, ensure_ascii=False, indent=2) + '\n', encoding='utf-8')
    print(json.dumps({k: value[k] for k in ('release_id', 'route_count', 'coverage', 'reports_inspected', 'full_goal_proven')}))


if __name__ == '__main__':
    main()
