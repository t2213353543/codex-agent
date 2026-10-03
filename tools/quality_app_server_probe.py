"""Run isolated real App Server/BPS reasoning comparisons on authorized accounts."""
import argparse
from datetime import datetime, timezone
import hashlib
import json
from pathlib import Path
from integration_env import ROOT, connect


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--cases', type=Path, default=ROOT / 'integration/app_server_quality/cases.json')
    parser.add_argument('--pilot', action='store_true')
    parser.add_argument('--effort', choices=['xhigh', 'max'], default='xhigh')
    parser.add_argument('--case', action='append', dest='case_ids', help='Run selected case IDs only')
    parser.add_argument('--route', choices=['both', 'app', 'bps', 'bridge'], default='both')
    args = parser.parse_args()
    cases = json.loads(args.cases.read_text(encoding='utf-8-sig'))
    if isinstance(cases, dict): cases = cases['cases']
    public_cases = [{'id': case['id'], 'prompt': case['prompt']} for case in cases]
    if args.case_ids:
        public_cases = [case for case in public_cases if case['id'] in args.case_ids]
        if len(public_cases) != len(set(args.case_ids)): parser.error('Unknown or duplicated selected case')
    folder = ROOT / '.local/appserver-quality' / datetime.now(timezone.utc).strftime('%Y%m%dT%H%M%SZ')
    folder.mkdir(parents=True, exist_ok=False)
    source = (ROOT / 'tools/quality_app_server_remote.py').read_text(encoding='utf-8')
    (folder / 'remote-source.py').write_text(source, encoding='utf-8')
    routes = ('app', 'bps') if args.route == 'both' else (args.route,)
    if args.route == 'bridge':
        bridge_source = (ROOT / 'tools/quality_app_server_bridge.py').read_text(encoding='utf-8')
        (folder / 'bridge-source.py').write_text(bridge_source, encoding='utf-8')
        source = 'BRIDGE_SOURCE = ' + repr(bridge_source) + '\n' + source
    (folder / 'executed-source.py').write_text(source, encoding='utf-8')
    script = source + '\nmain(' + repr(public_cases) + ', pilot=' + repr(args.pilot) + ', effort=' + repr(args.effort) + ', routes=' + repr(routes) + ')\n'
    (folder / 'inputs.json').write_text(json.dumps({'cases': public_cases, 'pilot': args.pilot, 'effort': args.effort, 'routes': routes, 'remote_source_sha256': hashlib.sha256(source.encode()).hexdigest()}, ensure_ascii=False, indent=2), encoding='utf-8')
    client = connect()
    try:
        stdin, stdout, stderr = client.exec_command('python3 -B -', timeout=2200)
        stdin.write(script); stdin.channel.shutdown_write()
        with (folder / 'results.jsonl').open('w', encoding='utf-8') as log:
            for line in stdout:
                log.write(line); log.flush()
                try:
                    value = json.loads(line)
                    if value.get('kind') == 'case':
                        value = {k: v for k, v in value.items() if k in ['kind', 'route', 'account', 'case', 'repetition', 'status', 'elapsed_seconds']}
                    print(json.dumps(value, ensure_ascii=False), flush=True)
                except ValueError:
                    print('Non-JSON output retained in local evidence', flush=True)
        code = stdout.channel.recv_exit_status(); error = stderr.read().decode('utf-8', 'replace')
        result = {'exit_code': code, 'stderr_bytes': len(error.encode()), 'output': str(folder)}
        if code:
            result['error_type'] = error.strip().splitlines()[-1].split(':', 1)[0] if error.strip() else 'unknown'
            result['trace_locations'] = [line.strip() for line in error.splitlines() if line.lstrip().startswith('File "<stdin>"')]
        (folder / 'result.json').write_text(json.dumps(result, indent=2), encoding='utf-8')
        print(json.dumps(result), flush=True)
        if code: raise SystemExit(code)
    finally:
        client.close()


if __name__ == '__main__':
    main()
