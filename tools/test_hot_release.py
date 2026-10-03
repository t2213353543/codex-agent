"""Pure fake-controller and temporary-package tests; no SSH or deployments."""
import argparse
import contextlib
import copy
import hashlib
import io
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

import hot_release as hot


def release_fixture():
    return {"release_id": "b" * 64, "binary_sha256": "c" * 64,
            "remote": hot.REMOTE_RELEASES + "b" * 64, "manifest_path": "private-manifest.json"}


class FakeAdapter:
    def __init__(self, legacy=False):
        self.release = release_fixture()
        self.old = {"unit": "codex-agent.service", "instance": "old", "pid": 100,
                    "binary_sha256": "a" * 64, "listen_port": 18879 if legacy else 18890,
                    "admission_mode": "process_local" if legacy else hot.SHARED}
        self.new = {"unit": "codex-agent-runtime-" + "c" * 20 + ".service", "instance": "c" * 20,
                    "pid": 200, "binary_sha256": "c" * 64, "listen_port": 18891,
                    "admission_mode": hot.SHARED, "release": self.release["remote"]}
        self.record = {"active": copy.deepcopy(self.old), "draining": [], "pending": None}
        self.status = {"active": {"revision": 1, "generation": "old", "address": "127.0.0.1:" + str(self.old["listen_port"]), "held": False},
                       "last_update_durable": True, "generations": {}, "waiting_requests": 0}
        self.actions = []
        self.waits = 0
        self.error = None
        self.health_error = False
        self.retire_ready = False
        self.persisted = copy.deepcopy(self.status["active"])
        self.INGRESS_STATE = argparse.Namespace(read_text=lambda: json.dumps(self.persisted))
        self.persist_mode = None

    require = staticmethod(hot.require)

    def registry(self):
        return copy.deepcopy(self.record)

    def front(self, method="GET", path="/status", body=None):
        if method == "POST":
            self.actions.append(("persist-same-target", copy.deepcopy(body)))
            self.require(body["expected_revision"] == self.status["active"]["revision"], "synthetic stale revision")
            if self.persist_mode == "cross_control":
                self.status["active"]["revision"] += 1
                self.status["active"]["held"] = True
                self.persisted = copy.deepcopy(self.status["active"])
                self.status["last_update_durable"] = True
                raise hot.HotReleaseError("synthetic concurrent control")
            if self.persist_mode != "lost_before_commit":
                self.status["active"] = copy.deepcopy(body["target"])
                self.persisted = copy.deepcopy(body["target"])
                self.status["last_update_durable"] = self.persist_mode != "failed_durability"
            if self.persist_mode in ("lost_after_commit", "lost_before_commit"):
                raise hot.HotReleaseError("synthetic persistence reply lost")
        return copy.deepcopy(self.status)

    def runtime(self, unit):
        return copy.deepcopy(self.new if unit == self.new["unit"] else self.old)

    def healthy(self, port):
        self.actions.append(("healthy", port))
        if self.health_error:
            raise hot.HotReleaseError("synthetic not ready")

    def lifecycle(self, instance):
        return {"admission_mode": hot.SHARED}

    def port_free(self, port):
        return True

    def private_backend_port(self, port):
        return type(port) is int and 1024 <= port <= 65535 and port not in (18878, 18879, 18191, 28765)

    def stage(self, args):
        self.actions.append(("stage", args.port))
        self.new["listen_port"] = args.port
        self.record["candidate"] = copy.deepcopy(self.new)
        return {"staged": True}

    def hold_legacy(self, args):
        self.actions.append(("hold",))
        self.status["active"]["held"] = True
        self.status["last_update_durable"] = True
        self.persisted = copy.deepcopy(self.status["active"])
        self.record["pending"] = {"phase": "legacy_drain_held"}
        if self.error == "hold_reply_lost":
            raise hot.HotReleaseError("synthetic hold reply lost")

    def resume_legacy(self, args):
        self.actions.append(("resume",))
        self.status["active"]["held"] = False
        self.status["last_update_durable"] = True
        self.persisted = copy.deepcopy(self.status["active"])
        self.record["pending"] = {"phase": "legacy_active_ingress_installed"}

    def commit(self):
        self.record["active"] = copy.deepcopy(self.new)
        self.record["draining"].append(copy.deepcopy(self.old))
        self.record.pop("candidate", None)
        self.record["pending"] = None
        self.status["active"] = {"revision": self.status["active"]["revision"] + 1, "generation": self.new["instance"], "address": "127.0.0.1:" + str(self.new["listen_port"]), "held": False}
        self.status["last_update_durable"] = True
        self.persisted = copy.deepcopy(self.status["active"])

    def activate(self, args):
        self.actions.append(("activate",))
        if self.waits:
            self.waits -= 1
            return {"activated": False, "phase": "waiting_for_legacy_natural_drain",
                    "old_backend_connections": 19, "old_front_requests": 19, "waiting_requests": 2}
        if self.error in ("switch_reply_lost", "switch_not_committed"):
            self.record["pending"] = {"phase": "switching"}
            if self.error == "switch_reply_lost":
                self.status["active"].update(generation=self.new["instance"], address="127.0.0.1:" + str(self.new["listen_port"]), held=False)
            raise hot.HotReleaseError("synthetic switch reply lost")
        self.commit()
        return {"activated": True}

    def reconcile(self, args):
        self.actions.append(("reconcile",))
        if self.status["active"]["generation"] == self.new["instance"]:
            self.commit()
        else:
            self.record["pending"] = {"phase": "legacy_drain_held"} if self.old["admission_mode"] == "process_local" else None

    def retire(self, args):
        self.actions.append(("retire", args.unit))
        if self.retire_ready:
            self.record["draining"] = [row for row in self.record["draining"] if row["unit"] != args.unit]
        return {"retired": self.retire_ready}


class FakeOperations:
    def __init__(self, adapter=None, package="exact"):
        self.adapter = adapter or FakeAdapter()
        self.package = package
        self.calls = []
        self.upload_error = False

    def probe_package(self):
        self.calls.append("package-check")
        return self.package

    def upload(self):
        self.calls.append("upload")
        self.package = "exact"
        if self.upload_error:
            raise hot.HotReleaseError("synthetic upload reply lost")

    def control(self, action, **kwargs):
        self.calls.append(action)
        return hot.remote_operation(self.adapter, {"action": action, "release": self.adapter.release, **kwargs})


class HotReleaseTests(unittest.TestCase):
    def setUp(self):
        self.clock = 0
        self.stack = contextlib.ExitStack()
        self.addCleanup(self.stack.close)
        self.stack.enter_context(patch("time.monotonic", lambda: self.clock))
        self.stack.enter_context(patch("time.sleep", self.advance))

    def advance(self, seconds):
        self.clock += seconds

    def test_new_shared_release_uploads_once_and_does_not_hold_or_retire(self):
        operations = FakeOperations(package="absent")
        result = hot.run_release(operations, release_fixture())
        self.assertEqual(result["state"], "activated")
        self.assertEqual(operations.calls.count("upload"), 1)
        self.assertEqual(operations.calls.count("activate"), 1)
        self.assertNotIn("legacy-window", operations.calls)
        self.assertFalse(any(x[0] in ("hold", "resume", "retire") for x in operations.adapter.actions))

    def test_exact_package_and_candidate_are_reused(self):
        operations = FakeOperations()
        operations.adapter.record["candidate"] = copy.deepcopy(operations.adapter.new)
        self.assertEqual(hot.run_release(operations, release_fixture(), activate_only=True)["state"], "activated")
        self.assertNotIn("upload", operations.calls)
        self.assertNotIn("stage", operations.calls)

    def test_stage_only_checks_ready_but_never_switches(self):
        operations = FakeOperations()
        result = hot.run_release(operations, release_fixture(), stage_only=True)
        self.assertEqual(result["state"], "staged")
        self.assertIn("check-candidate", operations.calls)
        self.assertNotIn("activate", operations.calls)
        self.assertEqual(operations.adapter.record["active"], operations.adapter.old)

    def test_activate_only_missing_package_or_candidate_never_uploads(self):
        for package in ("absent", "exact"):
            operations = FakeOperations(package=package)
            with self.assertRaises(hot.HotReleaseError):
                hot.run_release(operations, release_fixture(), activate_only=True)
            self.assertNotIn("upload", operations.calls)
            self.assertNotIn("stage", operations.calls)

    def test_mismatched_package_is_not_overwritten(self):
        operations = FakeOperations(package="mismatch")
        with self.assertRaises(hot.HotReleaseError):
            hot.run_release(operations, release_fixture())
        self.assertEqual(operations.calls, ["package-check"])

    def test_upload_lost_reply_probes_exact_package_without_second_upload(self):
        operations = FakeOperations(package="absent")
        operations.upload_error = True
        self.assertEqual(hot.run_release(operations, release_fixture())["state"], "activated")
        self.assertEqual(operations.calls.count("upload"), 1)
        self.assertGreaterEqual(operations.calls.count("package-check"), 2)

    def test_different_staged_candidate_is_not_replaced(self):
        operations = FakeOperations()
        operations.adapter.record["candidate"] = {**operations.adapter.new, "release": hot.REMOTE_RELEASES + "d" * 64}
        with self.assertRaises(hot.HotReleaseError):
            hot.run_release(operations, release_fixture())
        self.assertNotIn("stage", operations.calls)
        self.assertNotIn("activate", operations.calls)

    def test_legacy_bounded_wait_resumes_without_stopping_old(self):
        adapter = FakeAdapter(legacy=True)
        adapter.waits = 100
        operations = FakeOperations(adapter)
        result = hot.run_release(operations, release_fixture(), legacy_wait_seconds=1)
        self.assertEqual(result["state"], "waiting_for_legacy_natural_drain")
        self.assertTrue(result["legacy_resumed"])
        self.assertEqual(self.clock, 1)
        self.assertEqual(adapter.record["active"], adapter.old)
        self.assertIn("candidate", adapter.record)
        self.assertEqual([x[0] for x in adapter.actions].count("hold"), 1)
        self.assertEqual([x[0] for x in adapter.actions].count("resume"), 1)
        self.assertFalse(any(x[0] in ("stop", "kill", "rollback", "retire") for x in adapter.actions))

    def test_legacy_drains_then_switches_once_without_resuming_old(self):
        adapter = FakeAdapter(legacy=True)
        adapter.waits = 1
        operations = FakeOperations(adapter)
        result = hot.run_release(operations, release_fixture(), retire_drained=True)
        self.assertEqual(result["state"], "activated")
        self.assertFalse(any(x[0] in ("resume", "retire") for x in adapter.actions))
        self.assertEqual(result["retained_generations"], 1)

    def test_candidate_not_ready_never_holds_legacy(self):
        adapter = FakeAdapter(legacy=True)
        adapter.record["candidate"] = copy.deepcopy(adapter.new)
        adapter.health_error = True
        result = hot.run_release(FakeOperations(adapter), release_fixture(), activate_only=True)
        self.assertEqual(result["state"], "not_switched_after_reconcile")
        self.assertFalse(any(x[0] == "hold" for x in adapter.actions))

    def test_lost_hold_reply_safely_resumes_exact_old_target(self):
        adapter = FakeAdapter(legacy=True)
        adapter.error = "hold_reply_lost"
        result = hot.run_release(FakeOperations(adapter), release_fixture())
        self.assertEqual(result["state"], "not_switched_after_reconcile")
        self.assertFalse(result["ingress_held"])
        self.assertEqual([x[0] for x in adapter.actions].count("resume"), 1)

    def test_lost_switch_reply_reconciles_committed_target_without_second_switch(self):
        for legacy in (False, True):
            with self.subTest(legacy=legacy):
                adapter = FakeAdapter(legacy)
                adapter.error = "switch_reply_lost"
                operations = FakeOperations(adapter)
                self.assertEqual(hot.run_release(operations, release_fixture())["state"], "activated")
                self.assertIn("reconcile", operations.calls)
                self.assertEqual([x[0] for x in adapter.actions].count("activate"), 1)
                self.assertFalse(any(x[0] in ("resume", "rollback") for x in adapter.actions))

    def test_uncommitted_switch_reconciles_and_exits_without_retry(self):
        for legacy in (False, True):
            adapter = FakeAdapter(legacy)
            adapter.error = "switch_not_committed"
            operations = FakeOperations(adapter)
            result = hot.run_release(operations, release_fixture())
            self.assertEqual(result["state"], "not_switched_after_reconcile")
            self.assertFalse(result["ingress_held"])
            self.assertEqual([x[0] for x in adapter.actions].count("activate"), 1)
            self.assertEqual([x[0] for x in adapter.actions].count("resume"), int(legacy))

    def test_same_release_already_active_performs_no_mutation(self):
        adapter = FakeAdapter()
        adapter.commit()
        operations = FakeOperations(adapter)
        self.assertEqual(hot.run_release(operations, release_fixture())["state"], "already_active")
        self.assertEqual(operations.calls, ["package-check", "status"])

    def test_retire_not_ready_is_retained_without_retry(self):
        operations = FakeOperations()
        result = hot.run_release(operations, release_fixture(), retire_drained=True)
        self.assertEqual(result["retained_generations"], 1)
        self.assertEqual(operations.calls.count("retire"), 1)
        self.assertNotIn("rollback", operations.calls)

    def test_under_lock_guards_prevent_switching_another_candidate(self):
        adapter = FakeAdapter()
        adapter.record["candidate"] = {**adapter.new, "binary_sha256": "d" * 64}
        with self.assertRaises(hot.HotReleaseError):
            hot.remote_operation(adapter, {"action": "activate", "release": release_fixture(), "expected_active": hot.instance_identity(adapter.old)})
        self.assertFalse(any(x[0] == "activate" for x in adapter.actions))
        adapter.record["candidate"] = copy.deepcopy(adapter.new)
        adapter.record["active"]["pid"] = 999
        with self.assertRaises(hot.HotReleaseError):
            hot.remote_operation(adapter, {"action": "activate", "release": release_fixture(), "expected_active": hot.instance_identity(adapter.old)})

    def test_status_requires_actual_pid_and_durable_exact_front(self):
        adapter = FakeAdapter()
        adapter.record["active"]["pid"] = 999
        with self.assertRaises(hot.HotReleaseError):
            hot.remote_operation(adapter, {"action": "status", "release": release_fixture()})
        adapter = FakeAdapter()
        adapter.status["last_update_durable"] = False
        with self.assertRaises(hot.HotReleaseError):
            hot.run_release(FakeOperations(adapter), release_fixture())
        adapter.status["last_update_durable"] = True
        adapter.status["active"]["address"] = "127.0.0.1:29999"
        with self.assertRaises(hot.HotReleaseError):
            hot.run_release(FakeOperations(adapter), release_fixture())

    def test_existing_hold_is_not_unconditionally_resumed(self):
        adapter = FakeAdapter(legacy=True)
        adapter.status["active"]["held"] = True
        with self.assertRaises(hot.HotReleaseError):
            hot.run_release(FakeOperations(adapter), release_fixture())
        self.assertFalse(any(x[0] == "resume" for x in adapter.actions))

    def test_wait_arguments_are_bounded_before_remote_mutation(self):
        for seconds in (-1, 61, float("nan"), float("inf")):
            operations = FakeOperations()
            with self.assertRaises(hot.HotReleaseError):
                hot.run_release(operations, release_fixture(), legacy_wait_seconds=seconds)
            self.assertEqual(operations.calls, [])

    def test_45_second_window_returns_actual_bounded_observations_after_resume(self):
        adapter = FakeAdapter(legacy=True)
        adapter.waits = 1000
        result = hot.run_release(FakeOperations(adapter), release_fixture(), legacy_wait_seconds=45)
        self.assertEqual(result["state"], "waiting_for_legacy_natural_drain")
        self.assertTrue(result["legacy_resumed"])
        self.assertFalse(result["ingress_held"])
        evidence = result["legacy_observation"]
        self.assertEqual(evidence["budget_ms"], 45000)
        self.assertEqual(evidence["elapsed_ms"], 45000)
        self.assertEqual(evidence["waiting_observations"], 91)
        self.assertEqual(evidence["first"], {"elapsed_ms": 0, "old_backend_connections": 19, "old_front_requests": 19, "waiting_requests": 2})
        self.assertEqual(evidence["last"], {"elapsed_ms": 45000, "old_backend_connections": 19, "old_front_requests": 19, "waiting_requests": 2})
        self.assertEqual(evidence["minimum_counts"]["old_front_requests"], {"value": 19, "elapsed_ms": 0})
        self.assertLess(len(json.dumps(evidence)), 800)
        self.assertEqual([x[0] for x in adapter.actions].count("hold"), 1)
        self.assertEqual([x[0] for x in adapter.actions].count("resume"), 1)

    def test_default_window_stays_ten_seconds_and_collects_real_minimum_times(self):
        adapter = FakeAdapter(legacy=True)
        adapter.waits = 1000
        activate = adapter.activate
        def observed(args):
            value = activate(args)
            value.update(old_backend_connections=19 - int(self.clock), old_front_requests=12 if self.clock >= 3.5 else 19,
                         waiting_requests=int(self.clock * 2), private="SECRET_NOT_AN_OBSERVATION")
            return value
        adapter.activate = observed
        result = hot.run_release(FakeOperations(adapter), release_fixture())
        evidence = result["legacy_observation"]
        self.assertEqual(self.clock, 10)
        self.assertEqual(evidence["budget_ms"], 10000)
        self.assertEqual(evidence["last"]["elapsed_ms"], 10000)
        self.assertEqual(evidence["minimum_counts"]["old_backend_connections"], {"value": 9, "elapsed_ms": 10000})
        self.assertEqual(evidence["minimum_counts"]["old_front_requests"], {"value": 12, "elapsed_ms": 3500})
        self.assertNotIn("SECRET", json.dumps(result))

    def test_success_after_wait_retains_samples_without_inventing_final_zero_counts(self):
        adapter = FakeAdapter(legacy=True)
        adapter.waits = 1
        result = hot.run_release(FakeOperations(adapter), release_fixture(), legacy_wait_seconds=45)
        self.assertEqual(result["state"], "activated")
        evidence = result["legacy_observation"]
        self.assertEqual(evidence["waiting_observations"], 1)
        self.assertEqual(evidence["elapsed_ms"], 500)
        self.assertEqual(evidence["last"]["old_backend_connections"], 19)
        self.assertEqual(evidence["last"]["elapsed_ms"], 0)
        self.assertFalse(any(x[0] == "resume" for x in adapter.actions))

    def test_server_rejects_61_seconds_before_hold(self):
        adapter = FakeAdapter(legacy=True)
        adapter.record["candidate"] = copy.deepcopy(adapter.new)
        with self.assertRaises(hot.HotReleaseError):
            hot.remote_operation(adapter, {"action": "legacy-window", "release": release_fixture(),
                                          "expected_active": hot.instance_identity(adapter.old), "legacy_wait_seconds": 61})
        self.assertEqual(adapter.actions, [])

    def test_summary_does_not_expose_controller_extras(self):
        adapter = FakeAdapter()
        adapter.record["private_token"] = "DO_NOT_LOG"
        result = hot.run_release(FakeOperations(adapter), release_fixture())
        self.assertNotIn("DO_NOT_LOG", json.dumps(result))

    def test_cli_status_does_not_stage_reconcile_or_activate(self):
        operations = FakeOperations()
        operations.close = lambda: None
        with patch.object(hot, "read_release", return_value=release_fixture()), patch.object(hot, "RemoteOperations", return_value=operations), contextlib.redirect_stdout(io.StringIO()) as output:
            self.assertEqual(hot.main(["--release", "manifest.json", "--status"]), 0)
        self.assertEqual(operations.calls, ["package-check", "status"])
        self.assertEqual(json.loads(output.getvalue())["state"], "status")

    def test_cli_waiting_returns_distinct_exit_two_after_resuming(self):
        operations = FakeOperations(FakeAdapter(legacy=True))
        operations.adapter.waits = 100
        operations.close = lambda: None
        with patch.object(hot, "read_release", return_value=release_fixture()), patch.object(hot, "RemoteOperations", return_value=operations), contextlib.redirect_stdout(io.StringIO()) as output:
            self.assertEqual(hot.main(["--release", "manifest.json", "--legacy-wait-seconds", "0"]), 2)
        result = json.loads(output.getvalue())
        self.assertTrue(result["legacy_resumed"])
        self.assertFalse(result["ingress_held"])

    def test_bootstrap_null_durability_status_and_stage_require_exact_disk_proof(self):
        adapter = FakeAdapter(legacy=True)
        adapter.status["last_update_durable"] = None
        operations = FakeOperations(adapter)
        observed = operations.control("status")
        hot.validate_status(observed)
        self.assertIsNone(observed["front"]["last_update_durable"])
        self.assertEqual(observed["front"]["verified_persisted_target"], adapter.persisted)
        result = hot.run_release(operations, release_fixture(), stage_only=True)
        self.assertEqual(result["state"], "staged")
        self.assertIsNone(result["ingress_last_update_durable"])
        self.assertEqual(result["verified_persisted_target"], adapter.persisted)
        self.assertFalse(any(x[0] in ("persist-same-target", "activate", "hold") for x in adapter.actions))

    def test_matching_disk_never_overrides_false_durability(self):
        adapter = FakeAdapter()
        adapter.status["last_update_durable"] = False
        adapter.status["verified_persisted_target"] = copy.deepcopy(adapter.persisted)
        with self.assertRaises(hot.HotReleaseError):
            hot.run_release(FakeOperations(adapter), release_fixture(), stage_only=True)
        self.assertEqual(adapter.actions, [])
        with self.assertRaises(hot.HotReleaseError):
            hot.validate_status({"registry": adapter.registry(), "front": adapter.status})

    def test_null_durability_mismatched_missing_or_invalid_disk_refuses_mutation(self):
        for field, value in (("revision", 2), ("revision", True), ("held", 0), ("held", True), ("generation", "foreign"), ("address", "127.0.0.1:29999")):
            adapter = FakeAdapter()
            adapter.status["last_update_durable"] = None
            adapter.persisted[field] = value
            with self.subTest(field=field, value=value), self.assertRaises(hot.HotReleaseError):
                hot.run_release(FakeOperations(adapter), release_fixture(), stage_only=True)
            self.assertEqual(adapter.actions, [])
        for error in (OSError("PRIVATE"), ValueError("PRIVATE")):
            adapter = FakeAdapter()
            adapter.status["last_update_durable"] = None
            adapter.INGRESS_STATE = argparse.Namespace(read_text=lambda: (_ for _ in ()).throw(error))
            with self.assertRaisesRegex(hot.HotReleaseError, "cannot be verified"):
                hot.remote_operation(adapter, {"action": "status", "release": release_fixture()})

    def test_shared_restart_null_is_repersisted_once_before_actual_activation(self):
        adapter = FakeAdapter()
        adapter.status["last_update_durable"] = None
        before = copy.deepcopy(adapter.persisted)
        result = hot.run_release(FakeOperations(adapter), release_fixture())
        self.assertEqual(result["state"], "activated")
        writes = [action for action in adapter.actions if action[0] == "persist-same-target"]
        self.assertEqual(writes, [("persist-same-target", {"expected_revision": before["revision"], "target": {**before, "revision": before["revision"] + 1}})])
        self.assertLess(adapter.actions.index(writes[0]), adapter.actions.index(("activate",)))

    def test_legacy_bootstrap_null_uses_real_hold_persistence_without_extra_cas(self):
        adapter = FakeAdapter(legacy=True)
        adapter.status["last_update_durable"] = None
        result = hot.run_release(FakeOperations(adapter), release_fixture())
        self.assertEqual(result["state"], "activated")
        self.assertEqual([x[0] for x in adapter.actions].count("hold"), 1)
        self.assertFalse(any(x[0] == "persist-same-target" for x in adapter.actions))

    def test_repersist_lost_reply_reads_exact_commit_without_second_post(self):
        adapter = FakeAdapter()
        adapter.status["last_update_durable"] = None
        adapter.persist_mode = "lost_after_commit"
        result = hot.run_release(FakeOperations(adapter), release_fixture())
        self.assertEqual(result["state"], "activated")
        self.assertEqual([x[0] for x in adapter.actions].count("persist-same-target"), 1)

    def test_repersist_no_commit_failed_sync_or_cross_control_does_not_activate(self):
        for mode in ("lost_before_commit", "failed_durability", "cross_control"):
            adapter = FakeAdapter()
            adapter.status["last_update_durable"] = None
            adapter.persist_mode = mode
            with self.subTest(mode=mode):
                if mode == "failed_durability":
                    with self.assertRaises(hot.HotReleaseError):
                        hot.run_release(FakeOperations(adapter), release_fixture())
                else:
                    result = hot.run_release(FakeOperations(adapter), release_fixture())
                    self.assertEqual(result["state"], "not_switched_after_reconcile")
                self.assertEqual([x[0] for x in adapter.actions].count("persist-same-target"), 1)
                self.assertFalse(any(x[0] in ("activate", "resume") for x in adapter.actions))

    def test_front_control_changes_after_read_are_rechecked_inside_stage_lock(self):
        for mutation in ("disk_only", "other_target", "hold"):
            adapter = FakeAdapter()
            adapter.status["last_update_durable"] = None
            operations = FakeOperations(adapter)
            original = operations.control
            def control(action, **kwargs):
                if action == "stage":
                    adapter.persisted["revision"] += 1
                    if mutation == "other_target":
                        adapter.persisted["generation"] = "other-generation"
                        adapter.status["active"] = copy.deepcopy(adapter.persisted)
                    elif mutation == "hold":
                        adapter.persisted["held"] = True
                        adapter.status["active"] = copy.deepcopy(adapter.persisted)
                return original(action, **kwargs)
            operations.control = control
            with self.subTest(mutation=mutation), self.assertRaises(hot.HotReleaseError):
                hot.run_release(operations, release_fixture(), stage_only=True)
            self.assertFalse(any(x[0] == "stage" for x in adapter.actions))


class HotReleasePackageTests(unittest.TestCase):
    def test_probe_requires_exact_inventory_and_contents(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder) / "package"
            root.mkdir()
            raw = b'{"release_id":"synthetic"}'
            (root / "manifest.json").write_bytes(raw)
            (root / "runtime").write_bytes(b"synthetic binary")
            release = {"remote": str(root), "manifest_sha256": hashlib.sha256(raw).hexdigest(),
                       "files": {"runtime": hashlib.sha256(b"synthetic binary").hexdigest()}}
            def probe():
                result = subprocess.run([sys.executable, "-c", hot.package_probe_script(release)], capture_output=True, text=True, check=True)
                return json.loads(result.stdout)["package"]
            self.assertEqual(probe(), "exact")
            (root / "runtime").write_bytes(b"changed")
            self.assertEqual(probe(), "mismatch")
            (root / "runtime").write_bytes(b"synthetic binary")
            (root / "extra").write_text("unmanifested")
            self.assertEqual(probe(), "mismatch")
            release["remote"] = str(root / "absent")
            self.assertEqual(probe(), "absent")

    def test_manifest_rejects_traversal_and_missing_controller(self):
        with tempfile.TemporaryDirectory() as folder:
            path = Path(folder) / "manifest.json"
            files = {name: "c" * 64 for name in ("codex-agent", "deploy/reliability_release.py", "deploy/zero_downtime_release.py")}
            document = {"release_id": "b" * 64, "files": files}
            path.write_text(json.dumps(document))
            self.assertEqual(hot.read_release(path)["release_id"], "b" * 64)
            for name in ("../outside", "/absolute", "a\\b", "a/../b"):
                path.write_text(json.dumps({**document, "files": {**files, name: "c" * 64}}))
                with self.assertRaises(hot.HotReleaseError):
                    hot.read_release(path)
            path.write_text(json.dumps({**document, "files": {"codex-agent": "c" * 64}}))
            with self.assertRaises(hot.HotReleaseError):
                hot.read_release(path)

    def test_cli_diagnostic_never_prints_arbitrary_connection_exception(self):
        with patch.object(hot, "read_release", return_value=release_fixture()), patch.object(hot, "RemoteOperations", side_effect=OSError("SECRET_TOKEN")), contextlib.redirect_stdout(io.StringIO()) as output:
            self.assertEqual(hot.main(["--release", "manifest.json"]), 1)
        self.assertNotIn("SECRET_TOKEN", output.getvalue())
        self.assertEqual(json.loads(output.getvalue())["state"], "blocked")


if __name__ == "__main__":
    unittest.main()
