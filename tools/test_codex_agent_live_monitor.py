import json
import os
from copy import deepcopy
import unittest
from unittest.mock import patch
import codex_agent_live_monitor as monitor
from codex_agent_live_monitor import AGGREGATE_SQL, INFERENCE_ENDPOINTS, INFERENCE_RECORD_SQL, cache_ratio, classify_error, parse_observed_timestamp, redact, sql, summarize_ingress_document
from datetime import datetime, timezone


class MonitorTests(unittest.TestCase):
    def test_active_runtime_registry_uses_only_gateway_units(self):
        with patch.object(monitor.Path, "exists", return_value=False):
            self.assertEqual(monitor.active_runtime_unit(), (monitor.SERVICE, 0))
        for unit in ("codex-agent.service", "codex-agent-runtime-0123456789abcdef.service"):
            with patch.object(monitor.Path, "exists", return_value=True), patch.object(monitor.Path, "read_text", return_value=json.dumps({"schema": 1, "active": {"unit": unit}, "draining": [{"unit": "old"}]})):
                self.assertEqual(monitor.active_runtime_unit(), (unit, 1))
        for unit in ("nginx.service", "../../private", "codex-agent.service;whoami"):
            with patch.object(monitor.Path, "exists", return_value=True), patch.object(monitor.Path, "read_text", return_value=json.dumps({"schema": 1, "active": {"unit": unit}})):
                with self.assertRaises(ValueError):
                    monitor.active_runtime_unit()

    def test_bound_selection_retains_only_fixed_observations(self):
        value={"bound_snapshot_state":"exact_disabled","eligible_candidate_count":0,"snapshot_origin":"cache_hit","snapshot_age_ms":0,"replay_class":"opaque_or_file","recovery_outcome":"blocked_input"}
        self.assertEqual(monitor.observed_bound_selection({"bound_selection":{**value,"owner":"private"}}),value)
        self.assertIsNone(monitor.observed_bound_selection({}))
        self.assertEqual(monitor.observed_bound_selection({"bound_selection":{"eligible_candidate_count":False,"snapshot_age_ms":-1,"recovery_outcome":"private"}}),{})
        self.assertEqual(monitor.observed_error_parameter({"error_param":"top_p"}),"top_p")
    def test_parameter_and_mixed_metadata_never_copy_freeform_content(self):
        self.assertEqual(monitor.observed_error_parameter({"error_param":"stream_options.reasoning_summary_delivery"}),"stream_options.reasoning_summary_delivery")
        self.assertIsNone(monitor.observed_error_parameter({"error_param":"stream_options.private-content"}))
        self.assertEqual(classify_error({"error_param":"text.format.schema","error_source":"client","error_phase":"request_validation"}),"request_parameter")
        self.assertEqual(classify_error({"error_param":"text.format.schema","error_source":"upstream"}),"other_error")
        known={"formatter_count":1,"client_call_count":0,"other_call_count":0,"has_text":True,"has_refusal":False}
        self.assertEqual(monitor.observed_structured_mixed({"structured_mixed":{**known,"private":"body"}}),known)
        self.assertEqual(monitor.observed_structured_mixed({"structured_mixed":{"formatter_count":True,"client_call_count":-1,"has_text":"private"}}),{})
        self.assertIsNone(monitor.observed_structured_mixed({}))
    def test_tool_policy_metadata_keeps_only_observed_enums_counts_and_booleans(self):
        policy={"effective_choice":"none","catalog_at_prepare_count":0,"named_namespaces_at_prepare_count":0,
                "tool_search_at_prepare":False,"structured":False,"rejection_phase":"normalize_item",
                "rejected_matches_exact_declaration":True,"rejected_identity":"exact"}
        self.assertEqual(monitor.observed_tool_policy({"tool_policy":policy}),policy)
        self.assertEqual(classify_error({"http_code":200,"tool_policy":policy}),"tool_protocol")
        self.assertIsNone(monitor.observed_tool_policy({}))
        self.assertEqual(monitor.observed_tool_policy({"tool_policy":{"name":"private","effective_choice":"private", "catalog_at_prepare_count":True,"structured":"false","rejected_family":"private"}}),{})
        self.assertEqual(classify_error({"tool_policy":{"effective_choice":"auto"}}),"other_error")
        self.assertEqual(classify_error({"tool_policy":{"rejection_phase":"private"}}),"other_error")

    def test_postgres_variable_fraction_preserves_exact_time_on_python39(self):
        for fraction, micros in (("", 0), (".1", 100000), (".12", 120000), (".123", 123000),
                                 (".1234", 123400), (".12345", 123450), (".123456", 123456)):
            for offset in ("Z", "+00:00"):
                with self.subTest(fraction=fraction, offset=offset):
                    self.assertEqual(parse_observed_timestamp("2026-09-28T15:03:50"+fraction+offset),
                                     datetime(2026, 9, 28, 15, 3, 50, micros, timezone.utc))
        self.assertEqual(parse_observed_timestamp("2026-09-28T23:03:50.12345+08:00"),
                         parse_observed_timestamp("2026-09-28T15:03:50.123450Z"))

    def test_invalid_observation_time_never_fabricates_a_current_time(self):
        for value in ("", "not-a-date", "2026-09-28T15:03:50", "2026-13-28T15:03:50Z",
                      "2026-09-28T15:03:50.1234567Z", "2026-09-28T15:03:50Z' OR true"):
            with self.subTest(value=value), self.assertRaises(ValueError):
                parse_observed_timestamp(value)

    def test_snapshot_accepts_variable_fraction_at_all_observed_boundaries(self):
        totals = {name: 0 for name in ("successful_usage_unreported", "input_tokens", "output_tokens",
                  "inference_requests", "cached_tokens", "cache_write_tokens", "cache_read_samples",
                  "cache_write_samples", "cache_read_unknown_requests", "control_requests")}
        sql_report = {"captured_at": "2026-09-28T15:03:50.12345+00:00", "last_id": 43,
                      "windows": [{"minutes": 1, "totals": totals}],
                      "errors": [{"id": 43, "created_at": "2026-09-28T15:03:49.1+00:00",
                                  "request_type": "synthetic", "http_code": "502", "error_source": "gateway",
                                  "error_phase": "response", "error": "synthetic failure"}]}
        runtime = {"process_start_utc": "2026-09-28T15:03:49.10000Z"}
        with patch.object(monitor, "read_runtime", return_value=(runtime, {})), \
             patch.object(monitor, "sql", return_value=sql_report), \
             patch.object(monitor, "read_ingress", return_value={}):
            report = monitor.snapshot(42)
        self.assertEqual([row["id"] for row in report["new_errors"]], [43])
        self.assertEqual([row["minutes"] for row in report["error_windows"]], [1, 20, 60, 360])
        self.assertEqual(report["errors_since_process_start"][0]["count"], 1)

    def test_only_explicit_protocol_busy_is_excluded(self):
        self.assertEqual(classify_error({"http_code": 502, "error": "bad gateway"}), "other_error")
        self.assertEqual(classify_error({"http_code": 502, "error": "Selected model is at capacity"}), "other_error")
        self.assertEqual(classify_error({"error": "official inference error (serverOverloaded): Selected model is at capacity"}), "upstream_busy")
        self.assertEqual(classify_error({"attempts": [{"error_code": "server_overloaded"}]}), "upstream_busy")
    def test_actual_errors_remain_actionable(self):
        self.assertEqual(classify_error({"error": "official runtime emitted an undeclared tool call"}), "tool_protocol")
        self.assertEqual(classify_error({"error": "invalid_encrypted_content"}), "encrypted_history")
        self.assertEqual(classify_error({"error": "all eligible accounts have been attempted"}), "account_selection")
        self.assertEqual(classify_error({"error": "bps response terminated with error (status failed) (code=rate_limit_exceeded)"}), "upstream_rate_limit")
        self.assertEqual(classify_error({"error": "bps response terminated with response.failed (status failed) (code=usage_limit_reached)"}), "upstream_rate_limit")
        self.assertEqual(classify_error({"error": "upstream mentioned usage_limit_reached in its free text"}), "other_error")
        self.assertEqual(classify_error({"attempts": [{"error_code": "rateLimitExceeded"}]}), "upstream_rate_limit")
        self.assertEqual(classify_error({"error": "bps: tool function/run_officejs has 0 matching namespaces; cannot route"}), "tool_protocol")
        self.assertEqual(classify_error({"error": "bps: tool custom/inspect has 2 matching namespaces; cannot route"}), "tool_protocol")

    def test_selected_generation_failure_survives_refresh_or_truncated_attempts(self):
        for attempts, truncated in (([{"error_code": "rateLimitExceeded"}, {"phase": "refresh", "error_code": "refresh_failed"}], False),
                                    ([{"error_code": "unauthorized"}] * 4, True)):
            row = {"http_code": 200, "attempts": attempts, "attempts_truncated": truncated,
                   "final_upstream_error_code": "rateLimitExceeded", "final_upstream_error_source": "upstream"}
            self.assertEqual(classify_error(row), "upstream_rate_limit")
            row["final_upstream_error_source"] = "gateway"
            self.assertEqual(classify_error(row), "other_error")
        for code, category in (("unauthorized", "authentication"), ("upstream_account_rejected", "account_rejected"),
                               ("serverOverloaded", "upstream_busy"), ("not-a-recognized-code", "other_error")):
            self.assertEqual(classify_error({"final_upstream_error_code": code, "final_upstream_error_source": "upstream"}), category)

    def test_final_local_cas_failure_keeps_upstream_provenance_without_using_its_category(self):
        # Actual loopback producer shape from the independent failed-terminal
        # CAS test (failed-terminal-cas.log): one HTTP 200 generation produced
        # quota evidence, then the gateway rejected ownership before delivery.
        row = {"status": "error", "http_code": 200, "error_source": "gateway",
               "error_phase": "session_binding",
               "error": "session account changed during authentication failover",
               "final_upstream_error_code": "rateLimitExceeded", "final_upstream_error_source": "upstream",
               "attempts": [{"phase": "request", "status_code": 200, "observed_http_status_code": 200,
                             "error_code": "rateLimitExceeded", "error_source": "upstream"}],
               "ttft_ms": None, "first_output_kind": ""}
        before = deepcopy(row)
        self.assertEqual(classify_error(row), "account_selection")
        self.assertEqual(row, before)  # Classification must not rewrite actual observations.
        self.assertNotIn("usage_known", row)

    def test_explicit_final_source_guards_all_upstream_code_inputs(self):
        for source in ("gateway", "client", "transport", "storage"):
            for code in ("rateLimitExceeded", "serverOverloaded", "upstream_account_rejected", "authentication_unavailable"):
                for provenance in ({"final_upstream_error_code": code, "final_upstream_error_source": "upstream"},
                                   {"error_code": code}, {"attempts": [{"error_code": code}]}):
                    with self.subTest(source=source, code=code, provenance=provenance):
                        row = {"status": "error", "error_source": source,
                               "error": "session account changed during authentication failover", **provenance}
                        self.assertEqual(classify_error(row), "account_selection")
        for error in ("official inference error (serverOverloaded): synthetic",
                      "bps response terminated with response.failed (status failed) (code=rate_limit_exceeded)"):
            with self.subTest(error=error):
                self.assertEqual(classify_error({"status": "error", "error_source": "client",
                                                "error_phase": "request_validation", "error_param": "top_p",
                                                "error": error}), "request_parameter")

    def test_upstream_and_legacy_final_sources_keep_machine_classification(self):
        for source in (None, "", "unreported", "upstream"):
            for code, category in (("rateLimitExceeded", "upstream_rate_limit"), ("serverOverloaded", "upstream_busy"),
                                   ("upstream_account_rejected", "account_rejected"), ("authentication_unavailable", "authentication")):
                for provenance in ({"final_upstream_error_code": code, "final_upstream_error_source": "upstream"},
                                   {"error_code": code}, {"attempts": [{"error_code": code}]}):
                    with self.subTest(source=source, code=code, provenance=provenance):
                        self.assertEqual(classify_error({"status": "error", "error_source": source, **provenance}), category)

    def test_aborted_final_status_is_not_an_upstream_failure(self):
        for source in (None, "", "unreported", "upstream", "client"):
            for code in ("rateLimitExceeded", "serverOverloaded", "upstream_account_rejected", "authentication_unavailable"):
                row = {"status": "aborted", "http_code": 200, "error_source": source, "error": "client request canceled",
                       "error_code": code, "final_upstream_error_code": code, "final_upstream_error_source": "upstream",
                       "attempts": [{"error_code": code}], "input_tokens": 100, "cached_tokens": 80, "cache_write_tokens": 7}
                before = deepcopy(row)
                with self.subTest(source=source, code=code):
                    self.assertEqual(classify_error(row), "other_error")
                    self.assertEqual(row, before)

    def test_snapshot_keeps_final_generation_diagnostics_on_local_cas_failure(self):
        totals = {name: 0 for name in ("successful_usage_unreported", "input_tokens", "output_tokens",
                  "inference_requests", "cached_tokens", "cache_write_tokens", "cache_read_samples",
                  "cache_write_samples", "cache_read_unknown_requests", "control_requests")}
        row = {"id": 43, "created_at": "2026-09-28T15:03:49.1Z", "request_type": "http_sse",
               "status": "error", "http_code": "200", "error_source": "gateway", "error_phase": "session_binding",
               "error": "session account changed during authentication failover",
               "final_upstream_error_code": "rateLimitExceeded", "final_upstream_error_source": "upstream",
               "attempts": [{"error_code": "rateLimitExceeded", "error_source": "upstream"}]}
        document = {"captured_at": "2026-09-28T15:03:50Z", "last_id": 43, "errors": [deepcopy(row)],
                    "windows": [{"minutes": 1, "totals": totals}]}
        with patch.object(monitor, "read_runtime", return_value=({}, {})), \
             patch.object(monitor, "sql", return_value=document), patch.object(monitor, "read_ingress", return_value={}):
            report = monitor.snapshot(42)
        observed = report["new_errors"][0]
        self.assertEqual(observed["category"], "account_selection")
        for field in ("status", "http_code", "error_source", "error_phase", "final_upstream_error_code", "final_upstream_error_source"):
            self.assertEqual(observed[field], row[field])
        self.assertNotIn("usage_known", observed)
        self.assertEqual(report["error_groups_6h"][0]["category"], "account_selection")
        self.assertTrue(all(item["category"] == "account_selection" for item in report["error_windows"]))

    def test_snapshot_sql_retains_final_generation_provenance(self):
        totals = {name: 0 for name in ("successful_usage_unreported", "input_tokens", "output_tokens",
                  "inference_requests", "cached_tokens", "cache_write_tokens", "cache_read_samples",
                  "cache_write_samples", "cache_read_unknown_requests", "control_requests")}
        def execute(query):
            self.assertIn("observed_final_upstream_error_code final_upstream_error_code", query)
            self.assertIn("observed_final_upstream_error_source final_upstream_error_source", query)
            self.assertIn("observed_tool_policy tool_policy", query)
            self.assertIn("observed_error_param error_param", query)
            self.assertIn("observed_structured_mixed structured_mixed", query)
            self.assertIn("observed_bound_selection bound_selection", query)
            return {"captured_at": "2026-09-28T15:03:50.12345Z", "last_id": 1, "errors": [], "windows": [{"minutes": 1, "totals": totals}]}
        with patch.object(monitor, "read_runtime", return_value=({}, {})), patch.object(monitor, "sql", side_effect=execute), patch.object(monitor, "read_ingress", return_value={}):
            self.assertEqual(monitor.snapshot()["last_id"], 1)
    def test_only_structured_account_rejection_is_classified(self):
        self.assertEqual(classify_error({"http_code":403,"error_code":"upstream_account_rejected"}),"account_rejected")
        self.assertEqual(classify_error({"http_code":403,"attempts":[{"error_code":"upstream_account_rejected"}]}),"account_rejected")
        self.assertEqual(classify_error({"http_code":403,"error":"upstream_account_rejected"}),"other_error")
        self.assertEqual(classify_error({"http_code":403,"error":"This request was blocked by our usage policy."}),"other_error")
    def test_cache_ratio_is_token_weighted_and_zero_unknown(self):
        self.assertAlmostEqual(cache_ratio(2904175, 424704), 14.6239)
        self.assertIsNone(cache_ratio(0, 0))
    def test_export_redacts_secrets_and_source_urls(self):
        value = redact('Bearer synthetic-token contact user@example.invalid image https://host.invalid/private?q=secret')
        self.assertNotIn("synthetic-token", value)
        self.assertNotIn("user@example.invalid", value)
        self.assertNotIn("q=secret", value)

    def test_pre_admission_failures_keep_their_separate_scope(self):
        now = datetime(2026, 9, 28, tzinfo=timezone.utc)
        report = summarize_ingress_document({"next_sequence": 3, "evicted_total": 4, "events": [
            {"time_ms": (now.timestamp()-5)*1000, "reason": "invalid_api_key", "http_code": 401, "client_request_id": "private-correlation"},
            {"time_ms": (now.timestamp()-3601)*1000, "reason": "unknown_endpoint", "http_code": 404},
        ]}, now)
        self.assertEqual(report["evicted_total"], 4)
        self.assertEqual(len(report["windows"]), 5)
        self.assertFalse(report["included_in_token_or_cache_totals"])
        self.assertNotIn("private-correlation", json.dumps(report))


@unittest.skipUnless(os.environ.get("CODEX_MONITOR_TEST_POSTGRES") == "1", "read-only PostgreSQL fixture requires server test opt-in")
class MonitorSQLTests(unittest.TestCase):
    """Execute production aggregation expressions over VALUES, never tables."""

    def aggregate(self, records):
        encoded = json.dumps(records, separators=(",", ":")).replace("'", "''")
        return sql("WITH fixture AS (SELECT value AS record FROM jsonb_array_elements('" + encoded + "'::jsonb)), recent AS ("
            "SELECT record,record->>'status' status,(" + INFERENCE_RECORD_SQL + ") is_inference,"
            "coalesce((record->>'input_tokens')::bigint,0) input_tokens,"
            "coalesce((record->>'output_tokens')::bigint,0) output_tokens,"
            "coalesce((record->>'cached_tokens')::bigint,0) cached_tokens FROM fixture) "
            "SELECT row_to_json(t) FROM (SELECT " + AGGREGATE_SQL + " FROM recent)t")

    def test_models_do_not_create_missing_usage_or_inference_traffic(self):
        result = self.aggregate([{"endpoint": "models", "status": "ok"}])
        self.assertEqual(result["requests"], 1)
        self.assertEqual(result["control_requests"], 1)
        self.assertEqual(result["inference_requests"], 0)
        self.assertEqual(result["successful_usage_unreported"], 0)
        self.assertEqual(result["input_tokens"] + result["output_tokens"], 0)
        self.assertIsNone(result["cache_percent"])

    def test_generation_errors_and_actual_usage_survive_control_separation(self):
        result = self.aggregate([
            {"endpoint": "models", "status": "ok", "input_tokens": 999999, "cached_tokens": 999999},
            {"endpoint": "models", "status": "error"},
            {"endpoint": "responses", "status": "error"},  # before model parsing
            {"endpoint": "/v1/responses", "status": "ok", "usage_known": True, "input_tokens": 8192, "cached_tokens": 7168, "output_tokens": 128},
            {"endpoint": "responses", "status": "ok", "websocket_warmup": True},
            {"endpoint": "compact", "status": "ok", "usage_known": True, "input_tokens": 4096, "cached_tokens": 2048, "output_tokens": 256},
        ])
        self.assertEqual(result["requests"], 6)
        self.assertEqual(result["errors"], 2)  # includes control failure
        self.assertEqual(result["control_errors"], 1)
        self.assertEqual(result["inference_requests"], 3)
        self.assertEqual(result["inference_ok"], 2)
        self.assertEqual(result["control_requests"], 3)
        self.assertEqual(result["successful_usage_unreported"], 0)
        self.assertEqual(result["input_tokens"], 12288)
        self.assertEqual(result["cached_tokens"], 9216)
        self.assertEqual(result["output_tokens"], 384)
        self.assertEqual(result["cache_percent"], 75)

    def test_inference_endpoint_aliases_and_missing_usage(self):
        aliases = ["responses", "/v1/responses", "/backend-api/codex/responses", "/v1/responses/compact", "compact", "realtime"]
        records = [{"endpoint": endpoint, "status": "ok"} for endpoint in (*INFERENCE_ENDPOINTS, *aliases)]
        records += [{"endpoint": endpoint, "status": "ok"} for endpoint in ("models", "/v1/models", "/backend-api/ps/mcp", "", "responses/other", "/v1/realtime/calls/call_example")]
        result = self.aggregate(records)
        self.assertEqual(result["inference_requests"], len(INFERENCE_ENDPOINTS) + len(aliases))
        self.assertEqual(result["successful_usage_unreported"], result["inference_requests"])
        self.assertEqual(result["control_requests"], 6)

    def test_creation_and_reads_are_independent_with_unknown_coverage(self):
        result = self.aggregate([
            {"endpoint": "responses", "status": "ok", "input_tokens": 2000, "cached_tokens": 1500, "cache_write_tokens": 300},
            {"endpoint": "responses", "status": "ok", "input_tokens": 1000, "cached_tokens": 0, "cached_tokens_known": True, "cache_write_tokens": 0},
            {"endpoint": "responses", "status": "ok", "input_tokens": 7000, "cached_tokens": 0},
            {"endpoint": "models", "status": "ok", "input_tokens": 9999, "cached_tokens": 9999, "cache_write_tokens": 9999},
        ])
        self.assertEqual(result["input_tokens"], 10000)
        self.assertEqual(result["cache_read_input_tokens"], 3000)
        self.assertEqual(result["cache_percent"], 50)
        self.assertEqual(result["cache_read_samples"], 2)
        self.assertEqual(result["cache_read_unknown_requests"], 1)
        self.assertEqual(result["cache_write_tokens"], 300)
        self.assertEqual(result["cache_write_samples"], 2)

    def test_unknown_creation_is_not_uncached_input_or_zero(self):
        result = self.aggregate([{"endpoint": "responses", "status": "ok", "input_tokens": 2000, "cached_tokens": 0}])
        self.assertIsNone(result["cache_percent"])
        self.assertIsNone(result["cache_write_tokens"])
        self.assertEqual(result["cache_write_samples"], 0)
        self.assertEqual(result["cache_read_samples"], 0)

    def test_contradictory_read_counts_do_not_override_explicit_unknown(self):
        result = self.aggregate([
            {"endpoint": "responses", "status": "ok", "input_tokens": 2000, "cached_tokens": 1000, "cached_tokens_known": False},
            {"endpoint": "responses", "status": "ok", "input_tokens": 2000, "cached_tokens": 2500, "cached_tokens_known": True},
        ])
        self.assertIsNone(result["cache_percent"])
        self.assertEqual(result["cache_read_samples"], 0)
        self.assertEqual(result["cache_read_unknown_requests"], 2)


if __name__ == "__main__": unittest.main()
