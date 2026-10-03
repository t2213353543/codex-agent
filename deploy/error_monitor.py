"""Bounded, read-only gateway error observations. Stores metadata, never bodies."""
import collections
import datetime as dt
import json
import os
from pathlib import Path
import re
import subprocess
import tempfile

ROOT = Path('/var/lib/codex-agent-observation')
SCAN_ROWS = 20000
QUERY = """
WITH recent AS MATERIALIZED (
 SELECT id,created_at,status,record FROM request_logs ORDER BY id DESC LIMIT 20000
), rows AS (
 SELECT id,status,created_at<now()-interval '2 minutes' AS older_request,
 coalesce(record->>'http_code','') AS http,
 coalesce(record->>'error_source','') AS source,
 coalesce(record->>'error_phase','') AS phase,
 coalesce(record->>'error_param','') AS parameter,
 md5(coalesce(record->>'error','')) AS error_hash,
 coalesce(record->>'request_type','') AS transport
 FROM recent
)
SELECT json_build_object('at',now(),'rows',coalesce((SELECT json_agg(r ORDER BY id DESC) FROM rows r),'[]'::json))
"""


def summarize(snapshot, previous):
    """Count newly committed records, including late and out-of-order commits.

    created_at is request start time, so filtering it misses slow completions.
    Retain an overlap of IDs, not just max(id): concurrent transactions can
    commit a smaller allocated ID after a newer one was already observed.
    """
    rows = snapshot['rows']
    current_ids = {int(row['id']) for row in rows}
    prior_ids = {int(value) for value in previous.get('seen_ids', [])}
    initialized = previous.get('initialized') is True
    new = [row for row in rows if int(row['id']) not in prior_ids] if initialized else []
    overlap_lost = bool(initialized and prior_ids and len(rows) >= SCAN_ROWS and not current_ids.intersection(prior_ids))
    totals = dict(requests=len(new), successful=0, aborted=0, non_rate_errors=0, rate_errors=0)
    groups = collections.Counter()
    fields = ('http', 'source', 'phase', 'parameter', 'error_hash', 'transport')
    for row in new:
        if row['status'] == 'ok':
            totals['successful'] += 1
        elif row['status'] == 'aborted':
            totals['aborted'] += 1
        elif row['status'] == 'error':
            if row['http'] == '429':
                totals['rate_errors'] += 1
            else:
                totals['non_rate_errors'] += 1
                key = tuple(value if re.fullmatch(r'[A-Za-z0-9_./:-]{0,128}', value) else 'unreported' for value in (row[field] for field in fields))
                groups[key] += 1
    result = {
        'at': snapshot['at'], 'basis': 'newly_observed_completion_rows',
        'previous_observation_at': previous.get('at'), 'baseline_only': not initialized,
        'overlap_scan_rows': len(rows), 'overlap_limit': SCAN_ROWS,
        'coverage': 'gap_detected' if overlap_lost else 'bounded_overlap',
        'late_completions_observed': sum(bool(row['older_request']) for row in new),
        'totals': totals,
        'groups': [dict(zip(fields, key), count=count) for key, count in groups.most_common(40)],
    }
    next_state = {'initialized': True, 'at': snapshot['at'], 'seen_ids': sorted(current_ids)}
    return result, next_state


def atomic(path, value):
    raw = (json.dumps(value, ensure_ascii=True, separators=(',', ':')) + '\n').encode()
    fd, temporary = tempfile.mkstemp(prefix='.' + path.name, dir=path.parent)
    try:
        os.fchmod(fd, 0o600)
        with os.fdopen(fd, 'wb') as output:
            output.write(raw)
            output.flush()
            os.fsync(output.fileno())
        os.replace(temporary, path)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


def collect():
    args = ['docker', 'exec', '-e', 'PGOPTIONS=-c default_transaction_read_only=on -c statement_timeout=10000 -c lock_timeout=500 -c jit=off -c max_parallel_workers_per_gather=0',
            'codex-agent-postgres', 'psql', '-X', '-q', '-At', '-v', 'ON_ERROR_STOP=1', '-U', 'postgres', '-d', 'codex_agent', '-c', QUERY]
    result = subprocess.run(args, capture_output=True, text=True, timeout=15)
    if result.returncode:
        raise RuntimeError('read-only metadata query failed')
    snapshot = json.loads(result.stdout)
    try:
        previous = json.loads((ROOT / 'cursor.json').read_text())
    except (OSError, ValueError):
        previous = {}
    value, next_state = summarize(snapshot, previous)
    registry = json.loads(Path('/var/lib/codex-agent-release/active-runtime.json').read_text())
    active = registry['active']
    value['active_binary_sha256'] = active['binary_sha256']
    value['active_pid'] = active['pid']
    value['draining_generations'] = len(registry.get('draining', []))
    return value, next_state, previous


def main():
    ROOT.mkdir(parents=True, exist_ok=True, mode=0o700)
    try:
        current, next_state, previous = collect()
    except Exception as error:
        atomic(ROOT / 'health.json', {'status': 'error', 'at': dt.datetime.now(dt.timezone.utc).isoformat(), 'error_type': type(error).__name__})
        return 1
    history_path = ROOT / 'recent.json'
    try:
        if previous.get('initialized') is True:
            prior = previous['history']
        else:
            prior = json.loads(history_path.read_text())
        if not isinstance(prior, list):
            prior = []
    except (OSError, ValueError, KeyError):
        prior = []
    # At most 60 two-minute observations and 40 groups per observation.
    history = [*prior[-59:], current]
    next_state['history'] = history
    # Commit cursor and its observation together. Derived display files can be
    # recreated after interruption without skipping rows or double counting.
    atomic(ROOT / 'cursor.json', next_state)
    atomic(history_path, history)
    atomic(ROOT / 'latest.json', current)
    atomic(ROOT / 'health.json', {'status': 'ok', 'at': current['at'], 'retained_observations': len(history)})
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
