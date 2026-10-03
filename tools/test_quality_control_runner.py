import copy
import hashlib
import io
import json
from contextlib import redirect_stderr, redirect_stdout
import unittest

from quality_control_runner import run_control_batch


def sha(value):
    return hashlib.sha256(value.encode() if isinstance(value, str) else value).hexdigest()


class FakeClock:
    def __init__(self):
        self.value = 100.0

    def __call__(self):
        return self.value

    def advance(self, seconds):
        self.value += seconds


class Harness:
    def __init__(self):
        self.clock = FakeClock()
        self.body = b'{"model":"fixed-model","input":"same bytes"}'
        self.head = b'{"path":"/backend-api/codex/responses"}'
        self.secrets = {alias: {"access_token": "private-token-for-" + alias,
            "account_id": "shared-workspace-private", "user_id": "private-user-" + alias}
            for alias in ("q0", "q1")}
        self.epochs = {alias: {"identity_sha256": sha("identity-" + alias),
            "user_sha256": sha(secret["user_id"]), "token_sha256": sha(secret["access_token"]),
            "workspace_sha256": sha(secret["account_id"]), "assigned_at": "fixed-assignment",
            "oauth_updated_at": None} for alias, secret in self.secrets.items()}
        self.saved = []
        self.events = []
        self.model_count = 0
        self.usage_count = 0
        self.epoch_count = 0

    def save(self, path, value):
        self.saved.append((path, copy.deepcopy(value)))

    def epoch(self, **kw):
        self.epoch_count += 1
        self.events.append(("epoch", kw["alias"]))
        return copy.deepcopy(self.epochs[kw["alias"]])

    def usage(self, **kw):
        self.usage_count += 1
        self.events.append(("usage", kw["alias"], kw["phase"]))
        epoch = self.epochs[kw["alias"]]
        return {"schema": "codex-quality-usage-v1", "method": "GET",
            "endpoint": "/backend-api/wham/usage", "protocol_success": True,
            "automatic_retries": False, "model_calls": 0, "native_dispatch_count": 1,
            "token_sha256": epoch["token_sha256"], "workspace_sha256": epoch["workspace_sha256"],
            "eligibility": {"allowed": True, "reason": "ordinary_usage_allowed_no_observed_model_denial"}}

    def model(self, **kw):
        self.model_count += 1
        self.events.append(("model", kw["alias"]))
        assert kw["body"] == self.body and kw["head"] == self.head
        assert kw["pins"]["body_sha256"] == sha(self.body)
        assert kw["pins"]["head_sha256"] == sha(self.head)
        assert self.saved[0][0] == "execution.json"
        assert any(path == kw["cell_id"] + "/usage-before.json" for path, _ in self.saved)
        return {"protocol_success": True, "native_dispatch_count": 1, "final_output": "ok"}

    def run(self, **overrides):
        kwargs = {"order": [("01-q0", "q0"), ("02-q1", "q1"), ("03-q0", "q0")],
            "epochs": self.epochs, "secrets": self.secrets, "body": self.body, "head": self.head,
            "pins": {"body_sha256": sha(self.body), "head_sha256": sha(self.head)},
            "read_epoch": self.epoch, "read_usage": self.usage, "dispatch": self.model,
            "save": self.save, "deadline": self.clock() + 1000, "monotonic": self.clock}
        kwargs.update(overrides)
        return run_control_batch(**kwargs)


class ControlRunnerTests(unittest.TestCase):
    def test_shared_workspace_and_repeated_reference_allowed(self):
        harness = Harness()
        result = harness.run()
        self.assertEqual(result["status"], "completed")
        self.assertEqual(result["model_dispatch_attempts"], 3)
        self.assertEqual(harness.usage_count, 6)
        self.assertEqual([event for event in harness.events if event[0] != "epoch"], [
            ("usage", "q0", "before"), ("model", "q0"), ("usage", "q0", "after"),
            ("usage", "q1", "before"), ("model", "q1"), ("usage", "q1", "after"),
            ("usage", "q0", "before"), ("model", "q0"), ("usage", "q0", "after")])

    def test_each_collision_refuses_all_model_and_usage_calls(self):
        for field in ("identity_sha256", "user_sha256", "token_sha256"):
            with self.subTest(field=field):
                harness = Harness()
                harness.epochs["q1"][field] = harness.epochs["q0"][field]
                result = harness.run()
                self.assertEqual(result["status"], "stopped")
                self.assertEqual(harness.model_count, 0)
                self.assertEqual(harness.usage_count, 0)
                self.assertEqual(harness.epoch_count, 0)

    def test_same_user_with_different_workspace_and_identity_rejected(self):
        harness = Harness()
        harness.secrets["q1"]["account_id"] = "another-private-workspace"
        harness.epochs["q1"]["workspace_sha256"] = sha(harness.secrets["q1"]["account_id"])
        harness.epochs["q1"]["user_sha256"] = harness.epochs["q0"]["user_sha256"]
        result = harness.run()
        self.assertEqual(result["stop_reason"], "stable_user_identity_collision")
        self.assertEqual(harness.model_count, 0)

    def test_missing_identity_and_user_rejected(self):
        for field in ("identity_sha256", "user_sha256"):
            harness = Harness()
            del harness.epochs["q1"][field]
            self.assertEqual(harness.run()["status"], "stopped")
            self.assertEqual(harness.model_count, 0)

    def test_actual_secret_must_match_pinned_epoch(self):
        harness = Harness()
        harness.secrets["q1"]["access_token"] = "unexpected-new-credential"
        self.assertEqual(harness.run()["stop_reason"], "credential_epoch_mismatch")
        self.assertEqual(harness.model_count, 0)

    def test_late_quota_denial_skips_only_that_cell(self):
        harness = Harness()
        def usage(**kw):
            value = harness.usage(**kw)
            if kw["alias"] == "q1" and kw["phase"] == "before":
                value["eligibility"] = {"allowed": False, "reason": "ordinary_usage_denied"}
            return value
        result = harness.run(read_usage=usage)
        self.assertEqual([cell["status"] for cell in result["cases"]], ["completed", "skipped", "completed"])
        self.assertEqual(harness.model_count, 2)
        self.assertEqual(harness.usage_count, 5)
        self.assertTrue(any(path == "02-q1/usage-before.json" for path, _ in harness.saved))

    def test_unknown_or_nonboolean_usage_cannot_dispatch(self):
        for allowed in (None, False, 1, "true"):
            harness = Harness()
            def usage(**kw):
                value = harness.usage(**kw)
                value["eligibility"]["allowed"] = allowed
                return value
            harness.run(read_usage=usage)
            self.assertEqual(harness.model_count, 0)

    def test_management_or_wrong_credential_usage_cannot_dispatch(self):
        for mutation in ({"schema": "management-state"}, {"token_sha256": sha("wrong-token")},
                         {"native_dispatch_count": 0}, {"automatic_retries": True}):
            harness = Harness()
            def usage(**kw):
                return dict(harness.usage(**kw), **mutation)
            harness.run(read_usage=usage)
            self.assertEqual(harness.model_count, 0)

    def test_timeout_still_records_after_and_stops_by_declared_policy(self):
        harness = Harness()
        def timeout(**kw):
            harness.model(**kw)
            self.assertEqual(harness.saved[0][1]["failure_policy"], "stop")
            raise TimeoutError("private-exception-content")
        result = harness.run(dispatch=timeout, failure_policy="stop")
        self.assertEqual(harness.model_count, 1)
        self.assertEqual(harness.usage_count, 2)
        self.assertEqual(result["stop_reason"], "model_failure_policy_stop")
        self.assertEqual(result["cases"][0]["model_error_class"], "timeout")
        self.assertTrue(result["cases"][0]["usage_after_attempted"])

    def test_continue_policy_is_predeclared_and_never_retries(self):
        harness = Harness()
        def fail_first(**kw):
            self.assertEqual(harness.saved[0][1]["failure_policy"], "continue")
            result = harness.model(**kw)
            result["protocol_success"] = harness.model_count != 1
            return result
        result = harness.run(dispatch=fail_first, failure_policy="continue")
        self.assertEqual(harness.model_count, 3)
        self.assertEqual([cell["status"] for cell in result["cases"]], ["model_failed", "completed", "completed"])

    def test_max_requests_can_only_reduce_order_bound(self):
        harness = Harness()
        self.assertEqual(harness.run(max_requests=2)["model_dispatch_attempts"], 2)
        self.assertEqual(harness.model_count, 2)
        harness = Harness()
        self.assertEqual(harness.run(max_requests=4)["stop_reason"], "invalid_max_requests")
        self.assertEqual(harness.model_count, 0)

    def test_epoch_rotation_during_usage_stops_before_model(self):
        harness = Harness()
        def usage(**kw):
            result = harness.usage(**kw)
            harness.epochs[kw["alias"]]["oauth_updated_at"] = "rotated-during-read"
            return result
        result = harness.run(read_usage=usage)
        self.assertEqual(result["stop_reason"], "credential_epoch_changed")
        self.assertEqual(harness.model_count, 0)

    def test_epoch_rotation_after_model_gets_after_usage_then_stops(self):
        harness = Harness()
        def model(**kw):
            result = harness.model(**kw)
            harness.epochs[kw["alias"]]["token_sha256"] = sha("rotated-token")
            return result
        result = harness.run(dispatch=model)
        self.assertEqual(result["stop_reason"], "credential_epoch_changed")
        self.assertEqual(harness.model_count, 1)
        self.assertTrue(result["cases"][0]["usage_after_attempted"])

    def test_pins_reject_changed_capture_before_any_callback(self):
        harness = Harness()
        result = harness.run(body=harness.body + b" ")
        self.assertEqual(result["stop_reason"], "capture_pin_mismatch")
        self.assertEqual(harness.events, [])

    def test_absolute_deadline_leaves_after_and_cleanup_reserve(self):
        harness = Harness()
        absolute = harness.clock() + 100
        def model(**kw):
            self.assertEqual(kw["deadline"], absolute - 75)
            self.assertEqual(kw["timeout"], 15)
            result = harness.model(**kw)
            harness.clock.advance(kw["timeout"] + 10)
            raise TimeoutError("model consumed assigned time")
        def usage(**kw):
            result = harness.usage(**kw)
            self.assertLessEqual(kw["deadline"], absolute)
            if kw["phase"] == "after":
                harness.clock.advance(kw["timeout"] + 4)
            return result
        result = harness.run(dispatch=model, read_usage=usage, deadline=absolute)
        self.assertEqual(harness.model_count, 1)
        self.assertTrue(result["cases"][0]["usage_after_attempted"])
        self.assertFalse(result["deadline_exceeded"])

    def test_not_enough_deadline_never_starts_model(self):
        harness = Harness()
        result = harness.run(deadline=harness.clock() + 75)
        self.assertEqual(result["stop_reason"], "batch_deadline_reached")
        self.assertEqual(harness.model_count, 0)

    def test_before_persistence_failure_never_dispatches(self):
        harness = Harness()
        def save(path, value):
            if path.endswith("usage-before.json"):
                raise OSError("private-path")
            harness.save(path, value)
        result = harness.run(save=save)
        self.assertEqual(result["stop_reason"], "persistence_failed")
        self.assertEqual(harness.model_count, 0)

    def test_after_persistence_failure_stops_following_model(self):
        harness = Harness()
        def save(path, value):
            if path.endswith("usage-after.json"):
                raise OSError("private-path")
            harness.save(path, value)
        result = harness.run(save=save)
        self.assertEqual(result["stop_reason"], "persistence_failed")
        self.assertEqual(harness.model_count, 1)
        self.assertEqual(harness.usage_count, 2)

    def test_after_usage_failure_does_not_erase_model_result(self):
        harness = Harness()
        def usage(**kw):
            result = harness.usage(**kw)
            if kw["phase"] == "after":
                raise OSError("private-upstream-response")
            return result
        result = harness.run(read_usage=usage)
        self.assertEqual(result["cases"][0]["status"], "completed")
        self.assertTrue(result["cases"][0]["model"]["protocol_success"])
        self.assertIsNone(result["cases"][0]["usage_after"]["eligibility"]["allowed"])

    def test_receipts_and_stdout_contain_no_secrets(self):
        harness = Harness()
        def model(**kw):
            result = harness.model(**kw)
            result.update(access_token=kw["secret"]["access_token"],
                final_output="echo " + kw["secret"]["account_id"], authorization="new-unknown-secret")
            return result
        output = io.StringIO()
        with redirect_stdout(output), redirect_stderr(output):
            result = harness.run(dispatch=model)
        self.assertEqual(output.getvalue(), "")
        serialized = json.dumps([result, harness.saved])
        for secret in harness.secrets.values():
            for value in secret.values():
                self.assertNotIn(value, serialized)
        self.assertNotIn("new-unknown-secret", serialized)

    def test_redaction_retains_hashed_usage_identity_evidence(self):
        harness = Harness()
        def usage(**kw):
            result = harness.usage(**kw)
            result["usage"] = {"identity": {"user_id": {"state": "matched",
                "sha256": harness.epochs[kw["alias"]]["user_sha256"]}}}
            return result
        result = harness.run(read_usage=usage)
        identity = result["cases"][0]["usage_before"]["usage"]["identity"]["user_id"]
        self.assertEqual(identity, {"state": "matched", "sha256": harness.epochs["q0"]["user_sha256"]})


if __name__ == "__main__":
    unittest.main()
