"""Orchestration failure tests with mocked processes; no network or credentials."""
import contextlib
import importlib.util
import io
import json
from pathlib import Path
import subprocess
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

SPEC = importlib.util.spec_from_file_location('load_runner', Path(__file__).resolve().parents[2] / 'deploy/gateway_load_remote.py')
runner = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(runner)


class Process:
    def __init__(self, polls, wait_timeout=False):
        self.polls = iter(polls)
        self.returncode = None
        self.terminated = self.killed = False
        self.wait_timeout = wait_timeout

    def poll(self):
        self.returncode = next(self.polls, self.returncode)
        return self.returncode

    def wait(self, timeout):
        if self.wait_timeout:
            raise subprocess.TimeoutExpired('synthetic', timeout)
        self.returncode = 0 if self.returncode is None else self.returncode
        return self.returncode

    def terminate(self):
        self.terminated = True
        if not self.wait_timeout:
            self.returncode = -15

    def kill(self):
        self.killed = True
        self.returncode = -9
        self.wait_timeout = False


class RunnerTests(unittest.TestCase):
    def run_case(self, monitor, load, identities=None, clock=None):
        args = SimpleNamespace(scope='single', model='synthetic', levels='30', waves=1)
        with tempfile.TemporaryDirectory() as name:
            folder = Path(name)
            with patch.object(runner, 'runtime', side_effect=identities or [(12, 'a' * 64)] * 2), \
                    patch.object(runner.subprocess, 'Popen', side_effect=[monitor, load]) as spawn, \
                    patch.object(runner.time, 'sleep'), \
                    patch.object(runner.time, 'monotonic', side_effect=clock or [0, 1, 2, 3]), \
                    contextlib.redirect_stdout(io.StringIO()):
                code = runner.execute(folder, args)
            return code, json.loads((folder / 'runner.json').read_text()), spawn.call_count

    def test_complete_load_and_monitor_with_stable_binary_pass(self):
        code, report, _ = self.run_case(Process([None, None]), Process([None, 0]))
        self.assertEqual(code, 0)
        self.assertEqual(report['status'], 'pass')

    def test_failed_monitor_never_starts_load(self):
        code, report, count = self.run_case(Process([1]), Process([0]))
        self.assertEqual((code, count), (1, 1))
        self.assertEqual(report['error_code'], 'monitor_exited_before_load')

    def test_failed_monitor_terminates_active_load(self):
        load = Process([None, None])
        code, report, _ = self.run_case(Process([None, 1]), load)
        self.assertEqual(code, 1)
        self.assertTrue(load.terminated)
        self.assertEqual(report['error_code'], 'monitor_exited_during_load')

    def test_deadline_still_writes_report_and_cleans_processes(self):
        load = Process([None, None])
        code, report, _ = self.run_case(Process([None, None]), load, clock=[0, 1651])
        self.assertEqual(code, 1)
        self.assertEqual(report['error_code'], 'load_deadline_exceeded')
        self.assertTrue(load.terminated)

    def test_hung_monitor_is_killed_and_not_reported_success(self):
        monitor = Process([None, None], wait_timeout=True)
        code, report, _ = self.run_case(monitor, Process([0]))
        self.assertEqual(code, 1)
        self.assertTrue(monitor.killed)
        self.assertTrue(report['monitor_forced_stop'])

    def test_runtime_change_invalidates_successful_load(self):
        code, report, _ = self.run_case(Process([None]), Process([0]), identities=[(12, 'a'), (13, 'b')])
        self.assertEqual(code, 1)
        self.assertFalse(report['runtime_stable'])

    def test_runtime_unavailable_does_not_leak_error_or_spawn(self):
        code, report, count = self.run_case(Process([None]), Process([0]), identities=[RuntimeError('synthetic-secret'), OSError()])
        self.assertEqual((code, count), (1, 0))
        self.assertNotIn('synthetic-secret', json.dumps(report))
        self.assertTrue(report['runtime_after_unavailable'])


if __name__ == '__main__':
    unittest.main()
