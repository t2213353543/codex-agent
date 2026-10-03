from contextlib import contextmanager
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
from pathlib import Path
import tempfile
import threading
import time
import unittest
from unittest.mock import patch

import monitor


@contextmanager
def accounts_server(status=200):
    class Handler(BaseHTTPRequestHandler):
        def log_message(self, *_):
            pass

        def do_GET(self):
            if self.headers.get("Authorization") != "Bearer synthetic-admin-only":
                self.send_error(401)
                return
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(json.dumps({"accounts": [
                {"id": "account-secret-1", "name": "private@example.invalid", "in_flight": 3, "concurrency": 10, "enabled": True,
                 "access_token": "synthetic-unexpected-secret", "last_error": "private-error-not-output"},
                {"id": "account-secret-2", "name": "private-name", "in_flight": 5, "concurrency": 10, "enabled": True}], "has_more": False}).encode())
    server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    worker = threading.Thread(target=server.serve_forever, daemon=True)
    worker.start()
    try:
        yield "http://127.0.0.1:" + str(server.server_port)
    finally:
        server.shutdown()
        server.server_close()
        worker.join(timeout=2)


class MonitorTests(unittest.TestCase):
    def test_aliases_stable_without_recording_private_data(self):
        with accounts_server() as base:
            first = monitor.account_sample(base, "synthetic-admin-only", b"stable-test-salt", 1)
            second = monitor.account_sample(base, "synthetic-admin-only", b"stable-test-salt", 1)
            self.assertEqual(first, second)
            self.assertEqual(first["gateway_admitted_observed"], 8)
            self.assertTrue(first["scope_complete"])
            serialized = json.dumps(first)
            for value in ("private", "account-secret", "synthetic-unexpected-secret", "synthetic-admin-only"):
                self.assertNotIn(value, serialized)

    def test_partial_results_and_unknown_inflight_are_not_full_coverage(self):
        value = monitor.anonymous_accounts({"accounts": [{"id": "one"}], "has_more": True}, b"salt")
        self.assertFalse(value["scope_complete"])
        self.assertEqual(value["unknown_rows"], 1)
        self.assertEqual(value["gateway_admitted_observed"], 0)

    def test_proc_cpu_rss_and_cgroup_from_synthetic_files(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            process = root / "proc" / "123"
            process.mkdir(parents=True)
            fields = ["0"] * 22
            fields[0], fields[11], fields[12], fields[17], fields[19], fields[21] = "S", "100", "50", "4", "12345", "20"
            (process / "stat").write_text("123 (synthetic (nested)) " + " ".join(fields))
            (process / "fd").mkdir()
            (process / "net").mkdir()
            (process / "net" / "tcp").write_text("header\n")
            value = monitor.process_sample(123, root / "proc", 100, 4096)
            self.assertEqual(value["cpu_ticks"], 150)
            self.assertEqual(value["rss_bytes"], 81920)
            self.assertEqual(value["threads"], 4)
            self.assertEqual(value["fd_count"], 0)
            previous = dict(value, cpu_ticks=100)
            self.assertEqual(monitor.derived_sample(value, previous, .5)["cpu_one_core_percent"], 100)
            (process / "cgroup").write_text("0::/synthetic.service\n")
            group = root / "cgroup" / "synthetic.service"
            group.mkdir(parents=True)
            (group / "cpu.stat").write_text("nr_periods 100\nnr_throttled 4\nthrottled_usec 700\nusage_usec 5000\n")
            cgroup = monitor.cgroup_sample(123, root / "proc", root / "cgroup")
            self.assertEqual(cgroup["version"], 2)
            self.assertEqual(cgroup["cpu_stat"]["nr_throttled"], 4)

    def test_stopfile_and_anonymous_summary(self):
        with tempfile.TemporaryDirectory() as temporary, accounts_server() as base:
            root = Path(temporary)
            stop = root / "stop"
            timer = threading.Timer(.13, lambda: stop.touch())
            fake_process = {"start_ticks": 1, "cpu_ticks": 0, "clock_ticks_per_second": 100, "rss_bytes": 2000, "threads": 2, "fd_count": 5, "process_owned_tcp_states": {"established": 2}}
            timer.start()
            with patch.object(monitor, "process_sample", return_value=fake_process.copy()), patch.object(monitor, "cgroup_sample", return_value={"available": False, "reason": "synthetic"}):
                report = monitor.run_monitor(123, base, "synthetic-admin-only", 2, .05, 1, root / "result", stop)
            timer.join()
            self.assertEqual(report["status"], "stopped_by_file")
            self.assertGreaterEqual(report["samples"], 1)
            self.assertEqual(report["gateway_admitted_peak_observed"], 8)
            self.assertEqual(len(report["by_account_peak_observed"]), 2)
            self.assertIsNone(report["upstream_peak_inflight"])
            output = (root / "result" / "samples.jsonl").read_text() + json.dumps(report)
            self.assertNotIn("private", output)
            self.assertNotIn("synthetic-admin-only", output)

    def test_admin_auth_failure_stops_after_one_sample(self):
        with tempfile.TemporaryDirectory() as temporary, accounts_server(401) as base:
            fake_process = {"start_ticks": 1, "cpu_ticks": 0, "clock_ticks_per_second": 100, "rss_bytes": 2000, "threads": 2}
            with patch.object(monitor, "process_sample", return_value=fake_process), patch.object(monitor, "cgroup_sample", return_value={"available": False}):
                report = monitor.run_monitor(123, base, "synthetic-admin-only", 2, .05, 1, Path(temporary) / "result")
            self.assertEqual(report["status"], "stopped_admin_auth_failure")
            self.assertEqual(report["samples"], 1)
            self.assertEqual(report["api_errors"], 1)


if __name__ == "__main__":
    unittest.main()
