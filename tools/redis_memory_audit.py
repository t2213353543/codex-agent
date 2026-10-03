"""Read production Redis metadata without printing credentials or stored values."""
import argparse
import json
from pathlib import Path
import shlex
from integration_env import ROOT, connect, run


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--sample', type=int, default=2000)
    parser.add_argument('--output', type=Path, default=ROOT / '.local/redis-memory-20260930/inventory.json')
    args = parser.parse_args()
    source = (ROOT / 'integration/redis_memory/inventory_remote.py').read_text(encoding='utf-8')
    client = connect()
    try:
        result = run(client, 'python3 -B -c ' + shlex.quote(source) + ' --sample ' + str(args.sample), timeout=180)
    finally:
        client.close()
    value = json.loads(result)
    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(json.dumps(value, indent=2) + '\n', encoding='utf-8')
    print(json.dumps(value, indent=2))


if __name__ == '__main__':
    main()
