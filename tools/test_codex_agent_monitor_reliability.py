"""Offline freshness/error-boundary checks plus optional native SQL equivalence."""
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

import codex_agent_live_monitor as monitor


class MonitorReliabilityTests(unittest.TestCase):
    def test_journal_failure_keeps_last_good_cursor_and_error_evidence(self):
        with tempfile.TemporaryDirectory() as directory:
            folder = Path(directory)
            original = json.dumps({"last_id": 42, "captured_at": "old"}).encode()
            (folder / "latest.json").write_bytes(original)
            report = {"last_id": 57, "captured_at": "new", "runtime": {}, "new_errors": [{"id": 56, "category": "other_error"}]}
            for name in ("settings", "windows", "mode_windows", "realtime", "new_nonbusy_errors", "new_busy_errors", "first_observation_baseline", "ingress_errors"):
                report[name] = None
            original_open = Path.open
            def fail_journal(path, *args, **kwargs):
                if path.suffix == ".jsonl":
                    raise OSError("private filesystem detail")
                return original_open(path, *args, **kwargs)
            with patch.object(monitor, "snapshot", return_value=report), patch.object(Path, "open", fail_journal):
                with self.assertRaises(OSError):
                    monitor.observe_once(folder)
            self.assertEqual((folder / "latest.json").read_bytes(), original)
            status = json.loads((folder / "status.json").read_text())
            self.assertEqual(status["last_id"], 42)
            self.assertEqual(status["status"], "failed")
            self.assertNotIn("private", json.dumps(status))

    def test_failed_attempt_updates_fresh_status_without_replacing_snapshot(self):
        with tempfile.TemporaryDirectory() as directory:
            folder = Path(directory)
            saved = {"last_id": 42, "captured_at": "2026-09-28T13:42:44Z", "retained_errors": [1, 2]}
            raw = json.dumps(saved).encode()
            (folder / "latest.json").write_bytes(raw)
            with patch.object(monitor, "snapshot", side_effect=monitor.MonitorReadError("statement_timeout")) as snapshot:
                with self.assertRaises(monitor.MonitorReadError):
                    monitor.observe_once(folder)
            snapshot.assert_called_once_with(42)
            self.assertEqual((folder / "latest.json").read_bytes(), raw)
            status = json.loads((folder / "status.json").read_text())
            self.assertEqual(status["status"], "failed")
            self.assertEqual(status["error_category"], "statement_timeout")
            self.assertEqual(status["last_id"], 42)
            self.assertEqual(status["last_success_captured_at"], saved["captured_at"])
            self.assertGreater(status["checked_at"], saved["captured_at"])

    def test_recovery_keeps_pending_errors_cursor_and_clears_failure(self):
        with tempfile.TemporaryDirectory() as directory:
            folder = Path(directory)
            (folder / "latest.json").write_text(json.dumps({"last_id": 42, "captured_at": "old"}))
            (folder / "status.json").write_text(json.dumps({"status": "failed", "error_category": "statement_timeout"}))
            report = {"last_id": 57, "captured_at": "2026-09-28T14:00:00Z", "new_errors": [{"id": 56}]}
            with patch.object(monitor, "snapshot", return_value=report) as snapshot, patch.object(monitor, "persist") as persist:
                self.assertIs(monitor.observe_once(folder), report)
            snapshot.assert_called_once_with(42)
            persist.assert_called_once_with(folder, report)
            status = json.loads((folder / "status.json").read_text())
            self.assertEqual(status["status"], "ok")
            self.assertEqual(status["last_id"], 57)
            self.assertNotIn("error_category", status)

    def test_subprocess_errors_export_only_fixed_categories(self):
        private = b"postgres://secret.example/token password=PRIVATE\nERROR: canceling statement due to statement timeout\nSELECT private_prompt"
        with patch.object(monitor.subprocess, "run", return_value=subprocess.CompletedProcess([], 1, b"", private)):
            with self.assertRaises(monitor.MonitorReadError) as raised:
                monitor.command(["private-command"], input_bytes=b"private-key")
        self.assertEqual(raised.exception.category, "statement_timeout")
        self.assertNotIn("PRIVATE", str(raised.exception))
        with patch.object(monitor.subprocess, "run", side_effect=subprocess.TimeoutExpired("private", 35)):
            with self.assertRaises(monitor.MonitorReadError) as timed_out:
                monitor.command(["private-command"])
        self.assertEqual(timed_out.exception.category, "subprocess_timeout")


@unittest.skipUnless(os.environ.get("CODEX_MONITOR_TEST_POSTGRES") == "1", "requires isolated native PostgreSQL fixture")
class MonitorProjectionSQLTests(unittest.TestCase):
    def test_projection_preserves_every_aggregate_field_and_unknowns(self):
        records = [
            {"endpoint": "responses", "status": "ok", "input_tokens": 2000, "cached_tokens": 1500, "cache_write_tokens": 300, "ttft_ms": 4, "latency_ms": 20},
            {"endpoint": "responses", "status": "ok", "input_tokens": 1000, "cached_tokens_known": True, "cache_write_tokens": 0},
            {"endpoint": "responses", "status": "error", "input_tokens": 7000},
            {"endpoint": "responses", "status": "ok", "input_tokens": 1000, "cached_tokens": 1500, "cached_tokens_known": True},
            {"endpoint": "responses", "status": "ok", "input_tokens": 1000, "cached_tokens": 500, "cached_tokens_known": False},
            {"endpoint": "models", "status": "error", "input_tokens": 9999, "cached_tokens": 9999, "cache_write_tokens": 9999},
            {"endpoint": "responses", "status": "ok", "websocket_warmup": True},
            {"endpoint": "responses", "status": "aborted", "usage_known": True, "cached_tokens_known": True, "cache_write_tokens": 0},
            {"endpoint": "responses", "status": "error", "input_tokens": -1, "cached_tokens": -2, "cache_write_tokens": -1, "ttft_ms": -1, "latency_ms": -1},
        ]
        for rows in (records, [], [records[2]], [records[5]]):
            with self.subTest(size=len(rows)):
                raw = json.dumps(rows).replace("'", "''")
                source = "WITH fixture AS (SELECT value record FROM jsonb_array_elements('" + raw + "'::jsonb)), source AS (SELECT record,record->>'status' status,(" + monitor.INFERENCE_RECORD_SQL + ") is_inference,coalesce((record->>'input_tokens')::bigint,0) input_tokens,coalesce((record->>'output_tokens')::bigint,0) output_tokens,coalesce((record->>'cached_tokens')::bigint,0) cached_tokens FROM fixture),recent AS MATERIALIZED (SELECT source.*," + monitor.CACHE_OBSERVATION_PROJECTION_SQL + " FROM source) "
                result = monitor.sql(source + "SELECT json_build_object('old',(SELECT row_to_json(t) FROM (SELECT " + monitor.AGGREGATE_SQL + " FROM recent)t),'new',(SELECT row_to_json(t) FROM (SELECT " + monitor.SNAPSHOT_AGGREGATE_SQL + " FROM recent)t))")
                self.assertEqual(result["old"], result["new"])


if __name__ == "__main__":
    unittest.main(verbosity=2)
