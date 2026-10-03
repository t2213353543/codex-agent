"""Evidence-boundary tests for the standalone, offline semantics report."""
import copy
import hashlib
import json
from pathlib import Path
import tempfile
import unittest

import quality_semantics_report as report
from quality_semantics_variants import content_hash, request_content


def write(root, name, value):
    path = root / name
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(value, ensure_ascii=False) + "\n", encoding="utf-8")


def read(root, name):
    return json.loads((root / name).read_text(encoding="utf-8"))


def fixture(root, case_count=4):
    identity = {"alias": "q4", "identity_sha256": "a" * 64, "token_sha256": "b" * 64,
                "assigned_at": 123.0, "oauth_updated_at": None}
    binary = {"helper_sha256": "c" * 64, "probe_sha256": "d" * 64}
    plan = {"account_alias": "q4", "model": "gpt-5.6-sol", "effort": "medium", "max_calls": 4,
            "automatic_retries": False, "stop_on_any_incomplete_request": True,
            "controls_count_as_original_success": False, "no_expected_answers_in_model_context": True,
            "calls": [{"label": label, "variant": variant, "arm": arm,
                       "content_sha256": content_hash(request_content(variant))}
                      for label, variant, arm in report.REGISTERED_CASES]}
    execution = {"schema": "q4-semantics-live-v1", "cases": [], "finished": True,
                 "credential_snapshot_removed": True, **binary}
    write(root, "plan.json", plan)
    write(root, "identity-before.json", identity)
    for label, variant, arm in report.REGISTERED_CASES[:case_count]:
        content = request_content(variant)
        request = {"model": plan["model"], "input": [{"role": "user", "content": [{"type": "input_text", "text": content["input"]}]}],
                   "instructions": content["instructions"], "stream": True, "store": False,
                   "reasoning": {"effort": "medium"}}
        digest = report.go_request_sha256(request)
        answer = 21 if variant == "clarified_shape_choice" else 29
        row = {"account_alias": "q4", "arm": arm, "attempts": 1, "round": 1, "sequence": 1,
               "model": plan["model"], "effort": "medium", "request_sha256": digest,
               "predicted_upstream_body_sha256": digest, "credential_sha256": identity["token_sha256"],
               "prompt_sha256": hashlib.sha256(content["input"].encode()).hexdigest(),
               "live_model": True, "http_status": 200, "protocol_success": True, "response_status": "completed",
               "event_counts": {"response.completed": 1}, "response_model": plan["model"],
               "response_reasoning": {"effort": "medium"}, "output_text": f"最终答案：{answer}个"}
        if arm == "transparent":
            row["gateway_attempt_count"] = 1
        process = {"environment": {"PATH": "/usr/bin", "LANG": "C.UTF-8", "CODEX_QUALITY_PROBE_CONFIG": f"/tmp/{label}-config.json",
                                   "CODEX_NATIVE_HTTP_TEST_BIN": "/tmp/helper", "CODEX_NATIVE_HTTP_TEST_LIFECYCLE": "cli_request"},
                   "source_before": copy.deepcopy(identity), **binary}
        write(root, f"{label}/results.jsonl", row)
        write(root, f"{label}-input-evidence.json", {"request": request, "request_sha256": digest, "evidence_kind": report.EVIDENCE_KIND})
        write(root, f"{label}-identity-after.json", identity)
        write(root, f"{label}-process.json", process)
        execution["cases"].append({"label": label, "variant": variant, "arm": arm, "http_status": 200,
                                   "protocol_success": True, "exit_code": 0})
    write(root, "execution-report.json", execution)


class SemanticsReportTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name)
        fixture(self.root)

    def tearDown(self):
        self.temp.cleanup()

    def change(self, path, **changes):
        value = read(self.root, path)
        value.update(changes)
        write(self.root, path, value)

    def test_full_block_keeps_original_and_controls_separate(self):
        value = report.build_report(self.root)
        self.assertTrue(value["integrity_valid"], value)
        self.assertEqual(value["execution_state"], "completed")
        self.assertEqual(value["missing_cases"], [])
        self.assertEqual(value["original_task"]["categories"], {"wrong_answer": 2})
        self.assertEqual(value["diagnostic_controls"]["categories"], {"correct": 2})
        self.assertTrue(value["baseline_comparison"]["body_comparable"])
        self.assertFalse(value["baseline_comparison"]["upstream_wire_capture"])
        self.assertTrue(all(not item["original_benchmark_pass"] for item in value["cases"]))

    def test_recomputed_request_hash_must_match_both_sources(self):
        self.change("baseline-direct-input-evidence.json", request_sha256="0" * 64)
        value = report.build_report(self.root)
        self.assertFalse(value["integrity_valid"])
        self.assertIn("configured_and_harness_request_hash_mismatch", value["cases"][0]["evidence_errors"])
        self.assertEqual(value["cases"][0]["category"], "evidence_invalid")

    def test_captured_request_cannot_include_hidden_or_extra_settings(self):
        evidence = read(self.root, "baseline-direct-input-evidence.json")
        evidence["request"]["max_output_tokens"] = 20
        evidence["request_sha256"] = report.go_request_sha256(evidence["request"])
        write(self.root, "baseline-direct-input-evidence.json", evidence)
        self.change("baseline-direct/results.jsonl", request_sha256=evidence["request_sha256"])
        value = report.build_report(self.root)
        self.assertIn("request_not_exact_registered_settings_and_variant", value["cases"][0]["evidence_errors"])

    def test_epoch_uses_raw_before_process_and_after_not_summary_boolean(self):
        self.change("baseline-direct-identity-after.json", token_sha256="e" * 64)
        value = report.build_report(self.root)
        self.assertFalse(value["cases"][0]["identity_and_credential_epoch_verified"])
        self.assertFalse(value["baseline_comparison"]["body_comparable"])

    def test_row_credential_must_match_epoch(self):
        self.change("baseline-direct/results.jsonl", credential_sha256="e" * 64)
        value = report.build_report(self.root)
        self.assertIn("identity_or_credential_epoch_unverified", value["cases"][0]["evidence_errors"])

    def test_assignment_change_is_an_epoch_change(self):
        self.change("baseline-direct-identity-after.json", assigned_at=124.0)
        self.assertFalse(report.build_report(self.root)["integrity_valid"])

    def test_process_source_before_and_binary_hashes_are_checked(self):
        process = read(self.root, "baseline-direct-process.json")
        process["source_before"]["oauth_updated_at"] = 10
        process["helper_sha256"] = "0" * 64
        write(self.root, "baseline-direct-process.json", process)
        errors = report.build_report(self.root)["cases"][0]["evidence_errors"]
        self.assertIn("identity_or_credential_epoch_unverified", errors)
        self.assertIn("helper_sha256_mismatch_or_missing", errors)

    def test_tls_environment_injection_is_not_clean(self):
        process = read(self.root, "baseline-direct-process.json")
        process["environment"]["SSL_CERT_FILE"] = "/some/cert"
        write(self.root, "baseline-direct-process.json", process)
        self.assertFalse(report.build_report(self.root)["cases"][0]["environment_verified"])

    def test_predicted_body_change_excludes_pair_without_rewriting_answer(self):
        self.change("baseline-transparent/results.jsonl", predicted_upstream_body_sha256="0" * 64)
        value = report.build_report(self.root)
        self.assertTrue(value["integrity_valid"])
        self.assertFalse(value["baseline_comparison"]["body_comparable"])
        self.assertIn("predicted_upstream_body_sha256_missing_or_changed", value["baseline_comparison"]["exclusions"])
        self.assertEqual(value["cases"][1]["category"], "wrong_answer")

    def test_unreported_case_or_extra_rows_invalidates_block(self):
        write(self.root, "unexpected/results.jsonl", {})
        value = report.build_report(self.root)
        self.assertIn("missing_or_unreported_case_results", value["integrity_errors"])
        self.assertTrue(all(item["category"] == "evidence_invalid" for item in value["cases"]))

    def test_unreported_process_sidecar_is_not_ignored(self):
        write(self.root, "extra-process.json", {})
        value = report.build_report(self.root)
        self.assertIn("missing_or_unreported_case_sidecars:-process.json", value["integrity_errors"])

    def test_stop_flag_cannot_reclassify_four_completed_cases(self):
        self.change("execution-report.json", stopped_after_incomplete_request=True)
        value = report.build_report(self.root)
        self.assertFalse(value["integrity_valid"])
        self.assertTrue(all(case["category"] == "evidence_invalid" for case in value["cases"]))

    def test_second_result_is_not_silently_ignored(self):
        path = self.root / "baseline-direct/results.jsonl"
        path.write_text(path.read_text(encoding="utf-8") * 2, encoding="utf-8")
        self.assertFalse(report.build_report(self.root)["integrity_valid"])

    def test_plan_maximum_and_unique_order_are_enforced(self):
        plan = read(self.root, "plan.json")
        plan["max_calls"] = 5
        plan["calls"][1] = copy.deepcopy(plan["calls"][0])
        write(self.root, "plan.json", plan)
        self.assertIn("plan_not_registered_four_call_block", report.build_report(self.root)["integrity_errors"])

    def test_execution_extra_or_duplicate_case_is_rejected(self):
        execution = read(self.root, "execution-report.json")
        execution["cases"].append(copy.deepcopy(execution["cases"][0]))
        write(self.root, "execution-report.json", execution)
        self.assertIn("execution_not_unique_registered_prefix", report.build_report(self.root)["integrity_errors"])

    def test_path_labels_cannot_escape_workspace(self):
        execution = read(self.root, "execution-report.json")
        execution["cases"][0]["label"] = "../outside"
        write(self.root, "execution-report.json", execution)
        value = report.build_report(self.root)
        self.assertFalse(value["integrity_valid"])
        self.assertEqual(len(value["cases"]), 3)

    def test_nonfinite_and_duplicate_json_rejected(self):
        for raw in ('{"a":1,"a":2}', '{"a":NaN}'):
            with self.assertRaises(ValueError):
                report.parse_json(raw)

    def test_go_json_escaping_and_float_rejection(self):
        self.assertEqual(report.go_request_sha256({"b": "中<&>\u2028\u2029", "a": True}),
                         hashlib.sha256('{"a":true,"b":"中\\u003c\\u0026\\u003e\\u2028\\u2029"}'.encode()).hexdigest())
        with self.assertRaises(ValueError):
            report.go_request_sha256({"floating": 1.0})

    def test_first_call_403_429_or_capacity_stops_with_no_wrong_answer(self):
        for status, category in ((403, "auth"), (429, "rate_limit"), (503, "http_error")):
            with self.subTest(status=status), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                fixture(root, case_count=1)
                row = read(root, "baseline-direct/results.jsonl")
                row.update(http_status=status, protocol_success=False, response_status=None, event_counts={})
                write(root, "baseline-direct/results.jsonl", row)
                execution = read(root, "execution-report.json")
                execution["cases"][0].update(http_status=status, protocol_success=False)
                execution["stopped_after_incomplete_request"] = True
                write(root, "execution-report.json", execution)
                value = report.build_report(root)
                self.assertTrue(value["integrity_valid"], value)
                self.assertEqual(value["execution_state"], "stopped_incomplete")
                self.assertEqual(value["cases"][0]["category"], category)
                self.assertEqual(value["diagnostic_controls"]["attempted_cases"], 0)
                self.assertEqual(len(value["missing_cases"]), 3)

    def test_calls_after_failure_violate_stop_rule(self):
        self.change("baseline-direct/results.jsonl", http_status=429, protocol_success=False, response_status=None, event_counts={})
        execution = read(self.root, "execution-report.json")
        execution["cases"][0].update(http_status=429, protocol_success=False)
        write(self.root, "execution-report.json", execution)
        self.assertIn("calls_continued_after_incomplete_request", report.build_report(self.root)["integrity_errors"])

    def test_credential_cleanup_checked_without_reading_secret(self):
        (self.root / "credentials.json").write_text("should not read this", encoding="utf-8")
        value = report.build_report(self.root)
        self.assertIn("credential_snapshot_cleanup_unverified", value["integrity_errors"])
        self.assertNotIn("should not read this", json.dumps(value))

    def test_service_tier_auto_default_keeps_signals_and_avoids_causal_claim(self):
        signals = [{"field": "service_tier", "source": "sse_response", "value": value} for value in ("auto", "default")]
        self.change("baseline-direct/results.jsonl", upstream_metadata={"schema": 1, "signals": signals,
                    "conflicts": {"service_tier": True}, "capture_complete": True, "signals_dropped": 0})
        metadata = report.build_report(self.root)["cases"][0]["upstream_metadata"]
        self.assertEqual(metadata["signals"], signals)
        self.assertTrue(metadata["collector_reported_conflicts"]["service_tier"])
        self.assertIn("does not prove downgrade", metadata["service_tier_note"])

    def test_missing_file_can_be_reported_without_crashing(self):
        (self.root / "blind-transparent-input-evidence.json").unlink()
        value = report.build_report(self.root)
        self.assertFalse(value["integrity_valid"])
        self.assertEqual(value["cases"][-1]["category"], "evidence_invalid")


if __name__ == "__main__":
    unittest.main()
