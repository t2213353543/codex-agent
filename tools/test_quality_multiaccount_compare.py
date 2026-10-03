"""Synthetic offline artifacts; no network, credentials or model invocation."""
import copy
import hashlib
import json
from pathlib import Path
import tempfile
import unittest

from quality_multiaccount_compare import BODY_BASIS, build_report


def h(value):
    return hashlib.sha256(value.encode()).hexdigest()


def summary(value):
    raw = json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":")).encode()
    kind = "null" if value is None else "boolean" if isinstance(value, bool) else "number" if isinstance(value, (float, int)) else "string" if isinstance(value, str) else "array" if isinstance(value, list) else "object"
    return {"content_sha256": hashlib.sha256(raw).hexdigest(), "json_type": kind, "canonical_json_bytes": len(raw)}


class Experiment:
    def __init__(self, root, answers=(21, 29, 29, 21), order=None):
        self.root = root
        self.index_path = root / "index.json"
        expected = {"cli_sha256": h("official-cli"), "helper_sha256": h("real-native-helper"),
                    "model": "gpt-5.6-sol", "effort": "medium", "prompt_sha256": h("original-candy-prompt"),
                    "gateway_binary_sha256": h("gateway"), "fixture_test_binary_sha256": h("fixture")}
        order = order or [("01-q0", "q0"), ("02-q4", "q4"), ("03-q4", "q4"), ("04-q0", "q0")]
        self.index = {"schema": 1, "expected": expected,
                      "planned_order": [{"id": cell_id, "account_alias": alias} for cell_id, alias in order],
                      "cells": [], "finished": True, "credentials_removed": True, "automatic_retries": False,
                      "max_window_seconds": 1800}
        for sequence, ((cell_id, alias), answer) in enumerate(zip(order, answers)):
            directory = root / cell_id
            directory.mkdir()
            (directory / "ipc").mkdir()
            cell = {"id": cell_id, "account_alias": alias, "server_returncode": 0,
                    "wrapper_processes_gone": True, "helper_lifecycle_complete": True}
            for role in ("cli_report", "server_report", "identity_before", "identity_after", "config_evidence"):
                cell[role] = cell_id + "/" + role + ".json"
            cell["ipc_dir"] = cell_id + "/ipc"
            self.index["cells"].append(cell)
            identity = {"alias": alias, "identity_sha256": h(alias + "-identity"), "token_sha256": h(alias + "-token"),
                        "workspace_sha256": h(alias + "-workspace"), "assigned_at": 100, "oauth_updated_at": 200}
            cli = {"reference_clean": True, "status": "completed", "turn_completed": True, "returncode": 0,
                   "generated_answer_available": True, "error_events": 0, "startup_warning_events": 0,
                   "model_reroute_observed": False, "prompt_sha256": expected["prompt_sha256"],
                   "prompt_modified": False, "expected_answer_supplied": False,
                   "cli_sha256": expected["cli_sha256"], "cli_version": "0.155.0",
                   "model_requested": expected["model"], "effort_requested": "medium",
                   "model_catalog_initial_state": "fresh_isolated_home_no_injected_cache",
                   "model_catalog_override": False, "session_persisted": False,
                   "retry_policy": "custom_http_no_retries", "transport_requested": "http_custom_provider",
                   "tool_observed": False, "tool_item_events": 0, "cli_bundle": {"code_mode_host_present": True},
                   "final_output": "最终答案：%d个" % answer, "config_sha256": h(cell_id + "-config"),
                   "started_at_utc": "2026-09-25T00:0%d:00Z" % sequence, "elapsed_seconds": 30,
                   "usage": {"input_tokens": 6000, "output_tokens": 700 + sequence, "reasoning_output_tokens": 350 + sequence}}
            config = {"config_without_endpoint_sha256": h("fixed-normalized-config"), "fresh_home": True,
                      "config_sha256": cli["config_sha256"], "gateway_binary_sha256": expected["gateway_binary_sha256"],
                      "fixture_test_binary_sha256": expected["fixture_test_binary_sha256"]}
            body = {"model": expected["model"], "instructions": "fixed system instructions", "tools": [{"type": "function", "name": "fixture"}],
                    "reasoning": {"effort": "medium"}, "include": ["reasoning.encrypted_content"], "text": {"verbosity": "low"},
                    "store": False, "stream": True, "input": [{"type": "message", "content": "original candy", "session": cell_id}],
                    "client_metadata": {"turn_id": cell_id}, "prompt_cache_key": cell_id}
            fields = {key: summary(value) for key, value in body.items()}
            body_raw = json.dumps(body, sort_keys=True).encode()
            body_hash = hashlib.sha256(body_raw).hexdigest()
            wire_headers = {"authorization": [h("Bearer " + alias)], "chatgpt-account-id": [identity["workspace_sha256"]],
                            "x-client-request-id": [h(cell_id + "-request")], "user-agent": [h("official-cli-agent")],
                            "originator": [h("codex_cli_rs")], "x-codex-routing-hint": [h("model=gpt-5.6-sol")],
                            "session-id": [h(cell_id + "-session")], "thread-id": [h(cell_id + "-thread")]}
            all_headers = [{"name": name, "value_sha256": value} for name, values in wire_headers.items() for value in values]
            base = {"schema": 3, "instance": cell_id + "-instance", "helper_pid": 1000 + sequence,
                    "runtime_executable_matches_helper": True, "configured_helper_sha256": expected["helper_sha256"],
                    "runtime_executable_sha256": expected["helper_sha256"]}
            start = dict(base, event="native_ipc_helper_start", http_client_lifecycle="cli_request")
            request = dict(base, event="native_ipc_request", method="POST", endpoint="/backend-api/codex/responses",
                           request_forwarded_complete=True, ipc_head_complete=True, body_digest_scope="complete_pre_rust_compression_body",
                           body_summary_schema=3, body_summary_complete=True, body_summary_error=None, observation_error=None,
                           body_summary_basis=BODY_BASIS, actual_ipc_body_sha256=body_hash, body_wire_sha256=body_hash,
                           body_wire_bytes=len(body_raw), body_bytes_forwarded=len(body_raw), body_length_declared=len(body_raw),
                           authorization=[{"token_sha256": identity["token_sha256"]}], workspace_header_sha256=[identity["workspace_sha256"]],
                           header_summary_complete=True, all_headers=all_headers, header_value_count=len(all_headers), header_names=sorted(wire_headers),
                           model=expected["model"], reasoning_effort="medium", body_fields=fields, body_field_count=len(fields),
                           body_fields_captured=len(fields), body_field_names=sorted(fields),
                           client_metadata_summary={"keys": {"turn_id": summary(cell_id)}, "complete": True},
                           input_summary={"items": [{"index": 0, "content_sha256": h(cell_id + "-input"), "item_type": "message"}], "complete": True})
            end = dict(base, event="native_ipc_helper_exit", helper_returncode=0, helper_reaped=True, helper_forced_kill=False,
                       internal_socket_absent=True, external_socket_absent=True, observation_healthy=True)
            server = {"inference_requests": 1, "extra_requests_refused": 0,
                      "ingress": [{"authorization_token_sha256": identity["token_sha256"], "workspace_sha256": identity["workspace_sha256"],
                                   "request_id_sha256": wire_headers["x-client-request-id"][0], "decoded_sha256": body_hash, "decoded_bytes": len(body_raw)}],
                      "outcomes": [{"request_type": "http_sse", "attempts": 1, "http_code": 200, "status": "ok",
                                    "terminal_status": "completed", "sent_model": expected["model"], "returned_model": expected["model"],
                                    "reasoning_effort": "medium", "service_tier": "default", "error_source": "", "error_phase": ""},
                                   {"request_type": "models", "status": "ok", "http_code": 200, "attempts": 1}]}
            for role, value in (("cli_report", cli), ("server_report", server), ("identity_before", identity),
                                ("identity_after", identity), ("config_evidence", config)):
                self.write(sequence, role, value)
            self.write_ipc(sequence, [start, request, end])
        self.save()

    def save(self):
        self.index_path.write_text(json.dumps(self.index))

    def read(self, index, role):
        return json.loads((self.root / self.index["cells"][index][role]).read_text())

    def write(self, index, role, value):
        (self.root / self.index["cells"][index][role]).write_text(json.dumps(value, ensure_ascii=False), encoding="utf-8")

    def ipc(self, index):
        return [json.loads(line) for line in (self.root / self.index["cells"][index]["ipc_dir"] / "capture.jsonl").read_text().splitlines()]

    def write_ipc(self, index, rows):
        (self.root / self.index["cells"][index]["ipc_dir"] / "capture.jsonl").write_text("".join(json.dumps(row) + "\n" for row in rows))

    def report(self):
        self.save()
        return build_report(self.index_path)


class MultiaccountTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.experiment = Experiment(Path(self.temporary.name))

    def test_complete_abba_groups_by_current_answers_and_retains_differences(self):
        report = self.experiment.report()
        self.assertTrue(report["complete_controlled_comparison"], report["failed_experiment_checks"])
        self.assertTrue(report["concurrent_correct_and_wrong_account_classes"])
        self.assertEqual([row["classification"] for row in report["account_summaries"]], ["all_correct_this_experiment", "all_wrong_this_experiment"])
        pair = report["pairwise_comparisons"][0]
        self.assertEqual(pair["fixed_fields_changed"], [])
        self.assertEqual(set(pair["session_fields_changed"]), {"input", "client_metadata", "prompt_cache_key"})
        self.assertIn("session-id", pair["header_fields_changed"])
        self.assertEqual(pair["client_metadata_fields_changed"], ["turn_id"])
        self.assertEqual(pair["input_item_indices_changed"], ["0"])
        self.assertFalse(pair["complete_request_equivalent"])
        self.assertEqual(report["network_requests"], 0)

    def test_all_wrong_all_correct_and_mixed_do_not_create_two_account_classes(self):
        for answers in ((29, 29, 29, 29), (21, 21, 21, 21), (21, 29, 29, 29)):
            with self.subTest(answers=answers):
                for index, answer in enumerate(answers):
                    cli = self.experiment.read(index, "cli_report")
                    cli["final_output"] = "最终答案：%d个" % answer
                    self.experiment.write(index, "cli_report", cli)
                report = self.experiment.report()
                self.assertTrue(report["complete_controlled_comparison"])
                self.assertFalse(report["concurrent_correct_and_wrong_account_classes"])
                self.assertIn("没有形成", report["conclusion"])

    def test_historical_labels_are_not_used(self):
        self.experiment.index["historical_labels"] = {"q0": "abnormal", "q4": "normal"}
        report = self.experiment.report()
        self.assertEqual(report["account_summaries"][0]["classification"], "all_correct_this_experiment")
        self.assertNotIn("historical_labels", report)

    def test_six_cell_abccba_supports_three_preselected_accounts(self):
        root = self.experiment.root / "six"
        root.mkdir()
        order = [("01-q0", "q0"), ("02-q5", "q5"), ("03-q6", "q6"), ("04-q6", "q6"), ("05-q5", "q5"), ("06-q0", "q0")]
        experiment = Experiment(root, (21, 29, 29, 29, 29, 21), order)
        report = experiment.report()
        self.assertEqual(report["planned_design"], "ABCCBA")
        self.assertTrue(report["complete_controlled_comparison"], report["failed_experiment_checks"])
        self.assertTrue(report["concurrent_correct_and_wrong_account_classes"])
        self.assertEqual(len(report["pairwise_comparisons"]), 15)

    def test_six_cell_registered_account_skip_keeps_global_incomplete(self):
        root = self.experiment.root / "six-skip"
        root.mkdir()
        order = [("01-q0", "q0"), ("02-q5", "q5"), ("03-q6", "q6"), ("04-q6", "q6"), ("05-q5", "q5"), ("06-q0", "q0")]
        experiment = Experiment(root, (21, 29, 29, 29, 29, 21), order)
        cli = experiment.read(2, "cli_report")
        cli.update(reference_clean=False, status="failed", final_output=None)
        experiment.write(2, "cli_report", cli)
        skipped = experiment.index["cells"].pop(3)
        experiment.index["skipped_cells"] = [{"id": skipped["id"], "account_alias": skipped["account_alias"], "reason": "prior_invalid_reference_same_account"}]
        report = experiment.report()
        self.assertTrue(report["experiment_checks"]["registered_skips_valid"])
        self.assertTrue(report["experiment_checks"]["execution_matches_plan_after_registered_skips"])
        self.assertFalse(report["complete_controlled_comparison"])
        contrasts = [pair for pair in report["pairwise_comparisons"] if pair["contrast_with_matched_fixed_controls"]]
        self.assertTrue(contrasts)
        self.assertEqual(report["skipped_cells"][0]["id"], "04-q6")

    def test_valid_answer_cannot_be_a_reason_to_skip_repeat(self):
        skipped = self.experiment.index["cells"].pop(2)
        self.experiment.index["skipped_cells"] = [{"id": skipped["id"], "account_alias": skipped["account_alias"], "reason": "prior_invalid_reference_same_account"}]
        self.assertFalse(self.experiment.report()["experiment_checks"]["registered_skips_valid"])

    def test_schema4_breaks_down_item_fields_without_hiding_full_hash_changes(self):
        for index in range(4):
            rows = self.experiment.ipc(index)
            for row in rows:
                row["schema"] = 4
            post = rows[1]
            post["body_summary_schema"] = 4
            item = post["input_summary"]["items"][0]
            fields = {"id": summary("item-%d" % index), "content": summary("identical prompt content"),
                      "internal_chat_message_metadata_passthrough": summary({"turn_id": "turn-%d" % index})}
            item["field_summary"] = {"complete": True, "fields": fields, "field_names": sorted(fields),
                                     "field_count": len(fields), "fields_captured": len(fields)}
            metadata = {"turn_id": summary("turn-%d" % index), "content_item_kinds": summary(["text"])}
            item["internal_metadata_summary"] = {"complete": True, "fields": metadata, "field_names": sorted(metadata),
                                                 "field_count": len(metadata), "fields_captured": len(metadata)}
            self.experiment.write_ipc(index, rows)
        report = self.experiment.report()
        detail = report["pairwise_comparisons"][0]["input_item_details"][0]
        self.assertTrue(report["complete_controlled_comparison"])
        self.assertFalse(detail["full_item_hash_equal"])
        self.assertTrue(detail["field_comparison_available"])
        self.assertEqual(detail["fields_changed"], ["id", "internal_chat_message_metadata_passthrough"])
        self.assertEqual(detail["internal_metadata_keys_changed"], ["turn_id"])

    def test_schema3_item_field_absence_stays_unknown(self):
        detail = self.experiment.report()["pairwise_comparisons"][0]["input_item_details"][0]
        self.assertFalse(detail["field_comparison_available"])
        self.assertIsNone(detail["fields_changed"])

    def test_invalid_reference_cannot_be_scored_even_if_final_text_says_21(self):
        cli = self.experiment.read(0, "cli_report")
        cli.update(reference_clean=False, status="invalid_reference", startup_warning_events=1)
        self.experiment.write(0, "cli_report", cli)
        report = self.experiment.report()
        self.assertIsNone(report["cells"][0]["answer"])
        self.assertEqual(report["cells"][0]["category"], "unscorable_invalid_evidence")
        self.assertFalse(report["complete_controlled_comparison"])

    def test_existing_answer_parser_preserves_ambiguity(self):
        cli = self.experiment.read(0, "cli_report")
        cli["final_output"] = "如果可以选择形状，最终答案：21个；否则29个"
        self.experiment.write(0, "cli_report", cli)
        row = self.experiment.report()["cells"][0]
        self.assertTrue(row["evidence_valid"])
        self.assertEqual(row["category"], "ambiguous_answer")
        self.assertIsNone(row["answer"])

    def test_protocol_failure_is_not_wrong_answer(self):
        server = self.experiment.read(1, "server_report")
        server["outcomes"][0].update(http_code=429, status="error", terminal_status="")
        self.experiment.write(1, "server_report", server)
        row = self.experiment.report()["cells"][1]
        self.assertIsNone(row["answer"])
        self.assertIn("single_completed_server_outcome", row["failed_checks"])

    def test_extra_post_refused_or_two_native_posts_invalidate_cell(self):
        server = self.experiment.read(0, "server_report")
        server["extra_requests_refused"] = 1
        self.experiment.write(0, "server_report", server)
        self.assertFalse(self.experiment.report()["cells"][0]["evidence_valid"])
        rows = self.experiment.ipc(1)
        rows.insert(2, copy.deepcopy(rows[1]))
        self.experiment.write_ipc(1, rows)
        self.assertIn("one_actual_native_post", self.experiment.report()["cells"][1]["failed_checks"])

    def test_token_workspace_and_requestid_must_match_actual_dispatch(self):
        for position, key, value in ((0, "authorization", [{"token_sha256": h("wrong")}]),
                                     (1, "workspace_header_sha256", [h("wrong")])):
            rows = self.experiment.ipc(position)
            rows[1][key] = value
            self.experiment.write_ipc(position, rows)
        server = self.experiment.read(2, "server_report")
        server["ingress"][0]["request_id_sha256"] = h("wrong-request")
        self.experiment.write(2, "server_report", server)
        report = self.experiment.report()
        self.assertFalse(report["cells"][0]["checks"]["actual_credential_workspace_and_ingress_match"])
        self.assertFalse(report["cells"][1]["checks"]["actual_credential_workspace_and_ingress_match"])
        self.assertFalse(report["cells"][2]["checks"]["actual_request_id_correlated"])

    def test_identity_must_stay_stable_before_and_after(self):
        after = self.experiment.read(0, "identity_after")
        after["oauth_updated_at"] += 1
        self.experiment.write(0, "identity_after", after)
        self.assertFalse(self.experiment.report()["cells"][0]["checks"]["identity_and_credential_epoch_unchanged"])

    def test_same_alias_credential_epoch_change_between_cells_is_reported(self):
        for role in ("identity_before", "identity_after"):
            row = self.experiment.read(3, role)
            row["assigned_at"] += 1
            self.experiment.write(3, role, row)
        report = self.experiment.report()
        self.assertTrue(report["cells"][3]["evidence_valid"])
        self.assertFalse(report["experiment_checks"]["same_alias_epoch_stable_across_cells"])

    def test_two_aliases_for_same_account_are_not_independent_accounts(self):
        for i in (1, 2):
            for role in ("identity_before", "identity_after"):
                row = self.experiment.read(i, role)
                row["identity_sha256"] = h("q0-identity")
                self.experiment.write(i, role, row)
        self.assertFalse(self.experiment.report()["experiment_checks"]["aliases_are_distinct_accounts"])

    def test_incomplete_capture_and_helper_failure_are_not_valid_evidence(self):
        for i, mutation in enumerate((lambda rows: rows[1].update(body_summary_complete=False),
                                      lambda rows: rows[2].update(helper_reaped=False),
                                      lambda rows: rows[2].update(observation_healthy=False),
                                      lambda rows: rows[0].update(runtime_executable_sha256=h("different")))):
            rows = self.experiment.ipc(i)
            mutation(rows)
            self.experiment.write_ipc(i, rows)
        self.assertTrue(all(not row["evidence_valid"] for row in self.experiment.report()["cells"]))

    def test_invalid_marker_and_truncated_jsonl_fail_closed(self):
        path = self.experiment.root / self.experiment.index["cells"][0]["ipc_dir"]
        (path / "capture.invalid").write_text("invalid")
        other = self.experiment.root / self.experiment.index["cells"][1]["ipc_dir"] / "capture.jsonl"
        with other.open("a") as stream:
            stream.write('{"partial":')
        report = self.experiment.report()
        self.assertFalse(report["cells"][0]["checks"]["no_capture_invalid_marker"])
        self.assertFalse(report["cells"][1]["checks"]["artifacts_parse_valid"])

    def test_fixed_tools_and_user_agent_differences_block_controlled_conclusion(self):
        rows = self.experiment.ipc(1)
        rows[1]["body_fields"]["tools"] = summary([{"type": "another-tool"}])
        for header in rows[1]["all_headers"]:
            if header["name"] == "user-agent":
                header["value_sha256"] = h("different-ua")
        self.experiment.write_ipc(1, rows)
        report = self.experiment.report()
        self.assertTrue(report["cells"][1]["evidence_valid"])
        self.assertFalse(report["experiment_checks"]["fixed_input_fields_equal"])
        self.assertFalse(report["experiment_checks"]["fixed_transport_headers_equal"])
        self.assertFalse(report["concurrent_correct_and_wrong_account_classes"])

    def test_absence_and_explicit_null_are_not_collapsed(self):
        rows = self.experiment.ipc(1)
        post = rows[1]
        post["body_fields"]["service_tier"] = summary(None)
        post["body_field_count"] += 1
        post["body_fields_captured"] += 1
        post["body_field_names"] = sorted(post["body_fields"])
        self.experiment.write_ipc(1, rows)
        self.assertIn("service_tier", self.experiment.report()["pairwise_comparisons"][0]["fixed_fields_changed"])

    def test_native_request_summary_must_match_its_actual_bytes(self):
        rows = self.experiment.ipc(0)
        rows[1]["actual_ipc_body_sha256"] = h("body-mutated")
        self.experiment.write_ipc(0, rows)
        self.assertFalse(self.experiment.report()["cells"][0]["checks"]["native_body_digest_and_lengths_consistent"])

    def test_ingress_lexical_difference_is_reported_without_discarding_completed_answer(self):
        server = self.experiment.read(0, "server_report")
        server["ingress"][0]["decoded_sha256"] = h("differently-serialized-input")
        server["ingress"][0]["decoded_bytes"] -= 18
        self.experiment.write(0, "server_report", server)
        cell = self.experiment.report()["cells"][0]
        self.assertTrue(cell["evidence_valid"])
        self.assertEqual(cell["answer"], 21)
        self.assertFalse(cell["ingress_to_ipc"]["raw_bytes_equal"])
        self.assertFalse(cell["ingress_to_ipc"]["semantic_equivalence_verified"])

    def test_responses_lite_absent_tools_is_valid_and_presence_is_compared(self):
        for index in range(4):
            rows = self.experiment.ipc(index)
            post = rows[1]
            post["body_fields"].pop("tools")
            post["body_field_count"] -= 1
            post["body_fields_captured"] -= 1
            post["body_field_names"] = sorted(post["body_fields"])
            self.experiment.write_ipc(index, rows)
        report = self.experiment.report()
        self.assertTrue(report["complete_controlled_comparison"])
        self.assertNotIn("tools", report["cells"][0]["body_fields"])

    def test_query_catalog_path_and_descriptive_identity_alias_do_not_invalidate_post(self):
        rows = self.experiment.ipc(0)
        catalog = dict(rows[1], method="GET", path_sha256=h("/backend-api/codex/models?client_version=0.155.0"))
        catalog.pop("endpoint")
        rows.insert(1, catalog)
        self.experiment.write_ipc(0, rows)
        for role in ("identity_before", "identity_after"):
            value = self.experiment.read(0, role)
            value["alias"] = "q0-current-epoch"
            self.experiment.write(0, role, value)
        cell = self.experiment.report()["cells"][0]
        self.assertTrue(cell["evidence_valid"], cell["failed_checks"])
        self.assertFalse(cell["identity_alias_label_matches_index"])

    def test_stopped_prefix_is_not_complete_abba(self):
        self.experiment.index["cells"] = self.experiment.index["cells"][:2]
        report = self.experiment.report()
        self.assertTrue(report["experiment_checks"]["executed_order_is_planned_prefix"])
        self.assertFalse(report["experiment_checks"]["all_planned_cells_executed"])
        self.assertFalse(report["complete_controlled_comparison"])

    def test_order_and_time_window_are_not_silently_ignored(self):
        self.experiment.index["cells"][0], self.experiment.index["cells"][1] = self.experiment.index["cells"][1], self.experiment.index["cells"][0]
        self.experiment.index["max_window_seconds"] = 60
        report = self.experiment.report()
        self.assertFalse(report["experiment_checks"]["executed_order_is_planned_prefix"])
        self.assertFalse(report["experiment_checks"]["cell_times_sequential"])
        self.assertFalse(report["experiment_checks"]["within_declared_time_window"])

    def test_cleanup_runner_failure_and_config_fresh_home_are_checked(self):
        self.experiment.index["credentials_removed"] = False
        self.experiment.index["cells"][0]["server_returncode"] = 1
        config = self.experiment.read(1, "config_evidence")
        config["fresh_home"] = False
        self.experiment.write(1, "config_evidence", config)
        report = self.experiment.report()
        self.assertFalse(report["experiment_checks"]["credentials_removed"])
        self.assertFalse(report["cells"][0]["checks"]["runner_lifecycle_complete"])
        self.assertFalse(report["cells"][1]["checks"]["runner_fresh_home"])

    def test_normalized_config_difference_is_a_control_failure(self):
        config = self.experiment.read(1, "config_evidence")
        config["config_without_endpoint_sha256"] = h("different-prompting-config")
        self.experiment.write(1, "config_evidence", config)
        self.assertFalse(self.experiment.report()["experiment_checks"]["normalized_config_equal"])

    def test_artifact_paths_cannot_escape_or_relabel_duplicate_cells(self):
        for path in ("../secret.json", "/absolute/path.json", "C:/secret.json", "dir\\secret.json"):
            with self.subTest(path=path):
                saved = self.experiment.index["cells"][0]["cli_report"]
                self.experiment.index["cells"][0]["cli_report"] = path
                with self.assertRaises(ValueError):
                    self.experiment.report()
                self.experiment.index["cells"][0]["cli_report"] = saved
        self.experiment.index["cells"][1]["id"] = self.experiment.index["cells"][0]["id"]
        with self.assertRaises(ValueError):
            self.experiment.report()

    def test_missing_fields_or_duplicate_artifact_member_not_inferred_as_pass(self):
        config = self.experiment.root / self.experiment.index["cells"][0]["config_evidence"]
        config.write_text('{"fresh_home":true,"fresh_home":false}')
        (self.experiment.root / self.experiment.index["cells"][1]["cli_report"]).unlink()
        report = self.experiment.report()
        self.assertFalse(report["cells"][0]["evidence_valid"])
        self.assertFalse(report["cells"][1]["evidence_valid"])

    def test_no_raw_text_credentials_headers_or_unknown_source_objects_copied(self):
        secrets = ("synthetic-access-token-secret", "sensitive-user@example.invalid", "sk-synthetic-secret" )
        cli = self.experiment.read(0, "cli_report")
        cli["final_output"] = " ".join(secrets) + "\n最终答案：21个"
        cli["access_token"] = secrets[0]
        self.experiment.write(0, "cli_report", cli)
        rows = self.experiment.ipc(0)
        rows[1]["headers"] = {"authorization": [{"value": "Bearer " + secrets[0]}]}
        rows[1]["input_summary"]["raw_prompt"] = secrets[1]
        self.experiment.write_ipc(0, rows)
        report = self.experiment.report()
        encoded = json.dumps(report)
        for secret in secrets:
            self.assertNotIn(secret, encoded)
        self.assertEqual(report["cells"][0]["answer"], 21)


if __name__ == "__main__":
    unittest.main()
