"""Read-only current-production capacity and error audit; no upstream requests."""
import argparse
import datetime as dt
import json
from pathlib import Path
import shlex
from integration_env import connect, run, ROOT


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--output', type=Path)
    parser.add_argument('--since', help='Timezone-aware ISO timestamp; defaults to the last 48 hours')
    args = parser.parse_args()
    script = ROOT / 'integration/gateway_environment/inventory_remote.py'
    target = (args.output or ROOT / '.local/gateway-environment' / dt.datetime.now(dt.timezone.utc).strftime('%Y%m%dT%H%M%SZ') / 'inventory.json').resolve()
    target.relative_to(ROOT)
    if target.exists():
        raise RuntimeError('Refuse to overwrite captured environment evidence')
    client = connect()
    try:
        command = 'python3 -c ' + shlex.quote(script.read_text(encoding='utf-8'))
        if args.since:
            command += ' --since ' + shlex.quote(args.since)
        report = json.loads(run(client, command, timeout=90))
    finally:
        client.close()
    target.parent.mkdir(parents=True, exist_ok=True)
    target.write_text(json.dumps(report, indent=2) + '\n', encoding='utf-8')
    print(json.dumps({'report': str(target), 'health': report['health'], 'binary_sha256': report['binary_sha256'],
                      'log_rows': report['logs']['rows_examined'], 'error_groups': report['logs']['groups'],
                      'account_configuration': report['account_configuration']}, indent=2))


if __name__ == '__main__':
    main()
