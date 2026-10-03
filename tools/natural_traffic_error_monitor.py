#!/usr/bin/env python3
"""Read-only natural-traffic error monitor for codex-agent production.

Classifies persisted request logs by transport (websocket / http_sse /
http_json / compact / other) and reports error shapes with redacted samples.
Queries run through SSH on the production database container with the same
read-only budget as the live monitor. No inference, no writes, no settings
changes.

python tools/natural_traffic_error_monitor.py --since 2026-09-29T18:34:41Z
python tools/natural_traffic_error_monitor.py --hours 12
"""
import argparse
from datetime import datetime, timedelta, timezone
import json
import shlex
from pathlib import Path
import sys

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "tools"))

from codex_agent_live_monitor import redact  # noqa: E402
from integration_env import connect, run  # noqa: E402

CONTAINER = "codex-agent-postgres"
DATABASE = "codex_agent"

TRANSPORT_CASE = """
CASE coalesce(record->>'request_type','')
  WHEN 'websocket' THEN 'websocket'
  WHEN 'http_sse' THEN 'http_sse'
  WHEN 'http_json' THEN 'http_json'
  WHEN 'compact' THEN 'compact'
  WHEN '' THEN 'unclassified'
  ELSE coalesce(record->>'request_type','other') END
"""


def query(client, statement: str):
    args = " ".join(shlex.quote(part) for part in (
        "docker", "exec", "-e", "PGOPTIONS=-c default_transaction_read_only=on -c statement_timeout=20000",
        CONTAINER, "psql", "-X", "-q", "-At", "-v", "ON_ERROR_STOP=1", "-U", "postgres",
        "-d", DATABASE, "-c", statement))
    return run(client, args, timeout=60)


def collect(client, since: datetime, sample_limit: int) -> dict:
    iso = since.astimezone(timezone.utc).isoformat()
    boundary = "created_at >= '" + iso + "'"
    report = {
        "schema": "natural-traffic-error-monitor-v1",
        "mode": "read_only_natural_traffic",
        "window_start_utc": iso,
        "window_end_utc": datetime.now(timezone.utc).isoformat(),
    }
    report["transport_status"] = query(client,
        "SELECT " + TRANSPORT_CASE + " transport, status, count(*) FROM request_logs WHERE "
        + boundary + " GROUP BY 1,2 ORDER BY 1,2")
    report["error_shapes"] = query(client,
        "SELECT " + TRANSPORT_CASE + " transport, coalesce(record->>'http_code',''),"
        " coalesce(record->>'error_source',''), coalesce(record->>'error_phase',''),"
        " coalesce(record->>'final_upstream_error_code',''),"
        " coalesce(nullif(regexp_replace(coalesce(record->>'error',''), E'[\\n\\r\\t]+', ' ', 'g'), ''),''),"
        " count(*) FROM request_logs WHERE " + boundary + " AND status='error'"
        " GROUP BY 1,2,3,4,5,6 ORDER BY 7 DESC LIMIT 60")
    report["aborted_shapes"] = query(client,
        "SELECT " + TRANSPORT_CASE + " transport, count(*) FROM request_logs WHERE "
        + boundary + " AND status='aborted' GROUP BY 1 ORDER BY 2 DESC")
    report["websocket_terminal"] = query(client,
        "SELECT coalesce(record->>'terminal_status',''), count(*) FROM request_logs WHERE "
        + boundary + " AND coalesce(record->>'request_type','')='websocket' GROUP BY 1 ORDER BY 2 DESC")
    # WebSocket dials also appear as attempts inside HTTP-classified requests
    # (preferred-WS with HTTP fallback). Aggregate the dial outcomes and the
    # per-account refusal split that drives the availability cooldown.
    report["ws_attempt_outcomes"] = query(client,
        "SELECT a->>'phase', coalesce(a->>'status_code',''), coalesce(a->>'error_code',''),"
        " coalesce(a->>'error_source',''), count(*) FROM request_logs l,"
        " jsonb_array_elements(l.record->'attempts') a WHERE " + boundary +
        " AND a->>'phase'='websocket' GROUP BY 1,2,3,4 ORDER BY 5 DESC LIMIT 30")
    report["ws_attempt_by_account"] = query(client,
        "SELECT l.account_name, count(*) FILTER (WHERE a->>'status_code'='404'),"
        " count(*) FILTER (WHERE a->>'status_code'='101') FROM request_logs l,"
        " jsonb_array_elements(l.record->'attempts') a WHERE " + boundary +
        " AND a->>'phase'='websocket' GROUP BY 1 ORDER BY 2 DESC LIMIT 15")
    rows = query(client,
        "SELECT id || '|' || to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD\"T\"HH24:MI:SSZ')"
        " || '|' || coalesce(record->>'request_type','')"
        " || '|' || coalesce(record->>'stream','')"
        " || '|' || coalesce(record->>'endpoint','')"
        " || '|' || coalesce(record->>'model','')"
        " || '|' || coalesce(record->>'http_code','')"
        " || '|' || coalesce(record->>'error_source','')"
        " || '|' || coalesce(record->>'error_phase','')"
        " || '|' || coalesce(record->>'final_upstream_error_code','')"
        " || '|' || left(coalesce(record->>'error',''),300)"
        " || '|' || left(coalesce(record->>'attempts','[]'),600)"
        " || '|' || coalesce(record->>'websocket_warmup','')"
        " || '|' || coalesce(record->>'incomplete_reason','')"
        " FROM request_logs WHERE " + boundary + " AND status='error'"
        " ORDER BY created_at DESC, id DESC LIMIT " + str(sample_limit))
    keys = ("id", "created_at_utc", "request_type", "stream", "endpoint", "model", "http_code",
            "error_source", "error_phase", "final_upstream_error_code", "error", "attempts",
            "websocket_warmup", "incomplete_reason")
    samples = []
    for line in (rows or "").splitlines():
        values = line.split("|", len(keys) - 1)
        item = {k: redact(v) for k, v in zip(keys, values)}
        samples.append(item)
    report["error_samples"] = samples
    return report


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    window = parser.add_mutually_exclusive_group(required=True)
    window.add_argument("--since", help="UTC ISO timestamp, e.g. 2026-09-29T18:34:41Z")
    window.add_argument("--hours", type=float, help="lookback window in hours")
    parser.add_argument("--output", default=str(ROOT / ".local" / "collab-d-20260930"))
    parser.add_argument("--samples", type=int, default=40)
    args = parser.parse_args()
    if args.since:
        since = datetime.fromisoformat(args.since.replace("Z", "+00:00"))
    else:
        since = datetime.now(timezone.utc) - timedelta(hours=args.hours)
    client = connect()
    try:
        report = collect(client, since, args.samples)
    finally:
        client.close()
    out = Path(args.output)
    out.mkdir(parents=True, exist_ok=True)
    stamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")
    path = out / f"natural-errors-{stamp}.json"
    path.write_text(json.dumps(report, indent=1, ensure_ascii=False) + "\n", encoding="utf-8")
    print(json.dumps({"status": "ok", "report": str(path),
                      "error_samples": len(report["error_samples"]),
                      "transport_rows": len((report["transport_status"] or "").splitlines()),
                      "error_shape_rows": len((report["error_shapes"] or "").splitlines())}, ensure_ascii=False))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
