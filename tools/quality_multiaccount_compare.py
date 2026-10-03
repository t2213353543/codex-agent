"""Offline, evidence-gated multi-account candy comparison; never sends requests.

python tools/quality_multiaccount_compare.py /experiment/index.json --output comparison.json

Index schema 1 contains planned_order [{id,account_alias}], cells with the same
identity plus relative cli_report/server_report/ipc_dir/identity_before/
identity_after/config_evidence paths, expected CLI/helper/model/effort/prompt
pins, finished and credentials_removed. See tests for a complete synthetic index.
No historical normal/abnormal labels are accepted. Scoring uses parse_answer.
"""
from __future__ import annotations

import argparse
from collections import Counter, defaultdict
from datetime import datetime, timedelta, timezone
import hashlib
from itertools import combinations
import json
import math
from pathlib import Path, PurePosixPath
import re
import sys

from quality_probe_report import parse_answer


SCHEMA = "codex-quality-multiaccount-v2"
HASH = re.compile(r"[0-9a-f]{64}\Z")
LABEL = re.compile(r"[A-Za-z0-9][A-Za-z0-9_-]{0,79}\Z")
BODY_BASIS = "canonical_json_sorted_object_keys_normalized_string_escapes_exact_number_lexemes"
FIXED_FIELDS = ("model", "instructions", "tools", "tool_choice", "parallel_tool_calls",
                "reasoning", "store", "stream", "stream_options", "include", "service_tier",
                "text", "access_programs")
SESSION_FIELDS = ("input", "client_metadata", "prompt_cache_key", "prompt_cache_options",
                  "previous_response_id")
FIXED_HEADERS = ("user-agent", "originator", "version", "accept", "content-type",
                 "x-codex-routing-hint", "openai-beta", "x-openai-internal-codex-residency")
IDENTITY_FIELDS = ("identity_sha256", "token_sha256", "workspace_sha256", "assigned_at", "oauth_updated_at")
USAGE_FIELDS = ("input_tokens", "cached_input_tokens", "cache_write_input_tokens",
                "output_tokens", "reasoning_output_tokens", "total_tokens")
MAX_ARTIFACT_BYTES = 16 << 20


def digest(raw):
    return hashlib.sha256(raw).hexdigest()


def unique_object(pairs):
    value = {}
    for key, item in pairs:
        if key in value:
            raise ValueError("duplicate_artifact_member")
        value[key] = item
    return value


def reject_constant(_):
    raise ValueError("nonfinite_artifact_number")


def json_value(raw):
    return json.loads(raw.decode("utf-8-sig"), object_pairs_hook=unique_object, parse_constant=reject_constant)


def safe_path(root, relative):
    if not isinstance(relative, str) or not relative or "\\" in relative:
        raise ValueError("artifact_path_not_relative")
    path = PurePosixPath(relative)
    if path.is_absolute() or ".." in path.parts or ":" in relative:
        raise ValueError("artifact_path_escapes_experiment")
    result = (root / relative).resolve()
    if root.resolve() not in result.parents:
        raise ValueError("artifact_path_escapes_experiment")
    return result


def bounded_read(path):
    with path.open("rb") as source:
        raw = source.read(MAX_ARTIFACT_BYTES + 1)
    if len(raw) > MAX_ARTIFACT_BYTES:
        raise ValueError("artifact_too_large")
    return raw


def hashed(value):
    return isinstance(value, str) and HASH.fullmatch(value) is not None


def text_id(value):
    return value if isinstance(value, str) and re.fullmatch(r"[A-Za-z0-9_.:/-]{1,128}", value) else None


def timestamp(value):
    if not isinstance(value, str):
        return None
    try:
        parsed = datetime.fromisoformat(value.replace("Z", "+00:00"))
        return parsed.astimezone(timezone.utc) if parsed.tzinfo is not None else None
    except ValueError:
        return None


def finite_nonnegative(value):
    return isinstance(value, (float, int)) and not isinstance(value, bool) and math.isfinite(value) and value >= 0


def safe_usage(value):
    if not isinstance(value, dict):
        return {}
    return {key: value[key] for key in USAGE_FIELDS if key in value and finite_nonnegative(value[key])}


def field_summary(value):
    if not isinstance(value, dict) or not hashed(value.get("content_sha256")):
        return None
    if value.get("json_type") not in ("number", "string", "object", "array", "boolean", "null") or not finite_nonnegative(value.get("canonical_json_bytes")):
        return None
    return {key: value[key] for key in ("content_sha256", "json_type", "canonical_json_bytes")}


def summary_key(name):
    return isinstance(name, str) and (re.fullmatch(r"[a-z][a-z0-9_-]{0,127}", name) or re.fullmatch(r"sha256:[0-9a-f]{64}", name)) is not None


def object_field_summary(value):
    if not isinstance(value, dict) or value.get("complete") is not True or not isinstance(value.get("fields"), dict):
        return None
    fields = value["fields"]
    if value.get("field_count") != value.get("fields_captured") or value.get("fields_captured") != len(fields) or value.get("field_names") != sorted(fields):
        return None
    if any(not summary_key(key) or field_summary(item) is None for key, item in fields.items()):
        return None
    return {key: field_summary(item) for key, item in fields.items()}


def load_cell(root, cell, expected):
    checks, evidence, errors = {}, {}, []
    artifacts = {}
    for role in ("cli_report", "server_report", "identity_before", "identity_after", "config_evidence"):
        path = safe_path(root, cell.get(role))
        try:
            raw = bounded_read(path)
            parsed = json_value(raw)
            if not isinstance(parsed, dict):
                raise ValueError("artifact_not_object")
            artifacts[role], evidence[role] = parsed, digest(raw)
        except (OSError, ValueError, UnicodeError, RecursionError):
            artifacts[role] = {}
            errors.append(role + "_missing_or_invalid")
    directory = safe_path(root, cell.get("ipc_dir"))
    records, ipc_hashes = [], []
    try:
        paths = sorted(directory.iterdir())
        if not paths or len(paths) > 256:
            raise ValueError("ipc_files_missing_or_unbounded")
        invalid_marker = any(path.name.endswith(".invalid") for path in paths)
        checks["no_capture_invalid_marker"] = not invalid_marker
        for path in paths:
            if path.suffix != ".jsonl":
                continue
            if path.is_symlink() or path.parent.resolve() != directory:
                raise ValueError("ipc_path_invalid")
            raw = bounded_read(path)
            ipc_hashes.append(digest(raw))
            for line in raw.splitlines():
                if not line.strip():
                    continue
                record = json_value(line)
                if not isinstance(record, dict):
                    raise ValueError("ipc_record_not_object")
                records.append(record)
        evidence["ipc_files_sha256"] = ipc_hashes
    except (OSError, ValueError, UnicodeError, RecursionError):
        checks["no_capture_invalid_marker"] = False
        errors.append("ipc_missing_or_invalid")
    checks["artifacts_parse_valid"] = not errors
    cli, server, before, after, config = (artifacts[key] for key in
        ("cli_report", "server_report", "identity_before", "identity_after", "config_evidence"))
    checks["cli_clean_completed"] = (cli.get("reference_clean") is True and cli.get("status") == "completed"
        and cli.get("turn_completed") is True and cli.get("returncode") == 0
        and cli.get("generated_answer_available") is True and cli.get("error_events") == 0
        and cli.get("startup_warning_events") == 0 and cli.get("model_reroute_observed") is False)
    checks["prompt_unmodified_and_pinned"] = (hashed(expected.get("prompt_sha256"))
        and cli.get("prompt_sha256") == expected.get("prompt_sha256")
        and cli.get("prompt_modified") is False and cli.get("expected_answer_supplied") is False)
    checks["cli_and_controls_pinned"] = (hashed(expected.get("cli_sha256"))
        and cli.get("cli_sha256") == expected.get("cli_sha256")
        and cli.get("cli_version") == "0.155.0" and cli.get("model_requested") == expected.get("model")
        and cli.get("effort_requested") == expected.get("effort"))
    checks["fresh_home_and_http_no_retry"] = (cli.get("model_catalog_initial_state") == "fresh_isolated_home_no_injected_cache"
        and cli.get("model_catalog_override") is False and cli.get("session_persisted") is False
        and cli.get("retry_policy") == "custom_http_no_retries"
        and cli.get("transport_requested") == "http_custom_provider")
    checks["no_observed_tool_execution"] = cli.get("tool_observed") is False and cli.get("tool_item_events") == 0
    bundle = cli.get("cli_bundle", {})
    checks["full_cli_bundle"] = isinstance(bundle, dict) and bundle.get("code_mode_host_present") is True
    checks["configuration_evidence"] = (hashed(config.get("config_without_endpoint_sha256"))
        and hashed(config.get("config_sha256")) and config.get("config_sha256") == cli.get("config_sha256"))
    checks["runner_lifecycle_complete"] = (cell.get("server_returncode") == 0
        and cell.get("wrapper_processes_gone") is True and cell.get("helper_lifecycle_complete") is True)
    checks["runner_fresh_home"] = config.get("fresh_home") is True
    for key in ("gateway_binary_sha256", "fixture_test_binary_sha256", "capture_tool_sha256"):
        if key in expected:
            checks[key + "_pinned"] = hashed(expected[key]) and config.get(key) == expected[key]
    checks["identity_evidence_complete"] = (all(hashed(before.get(k)) and hashed(after.get(k)) for k in IDENTITY_FIELDS[:3])
        and all(k in before and k in after for k in IDENTITY_FIELDS)
        and all(before[k] is None or finite_nonnegative(before[k]) for k in IDENTITY_FIELDS[3:])
        and all(after[k] is None or finite_nonnegative(after[k]) for k in IDENTITY_FIELDS[3:]))
    checks["identity_and_credential_epoch_unchanged"] = checks["identity_evidence_complete"] and all(before.get(k) == after.get(k) for k in IDENTITY_FIELDS)
    ingress = server.get("ingress", [])
    ingress = ingress if isinstance(ingress, list) else []
    checks["one_server_inference_no_refusals"] = server.get("inference_requests") == 1 and server.get("extra_requests_refused") == 0 and len(ingress) == 1
    first = ingress[0] if len(ingress) == 1 and isinstance(ingress[0], dict) else {}
    outcomes = server.get("outcomes", [])
    outcomes = outcomes if isinstance(outcomes, list) else []
    inference = [row for row in outcomes if isinstance(row, dict) and row.get("request_type") in ("http_sse", "http_json")]
    outcome = inference[0] if len(inference) == 1 else {}
    checks["single_completed_server_outcome"] = (len(inference) == 1 and outcome.get("attempts") == 1
        and outcome.get("http_code") == 200 and outcome.get("status") == "ok"
        and outcome.get("terminal_status") == "completed")
    checks["server_model_effort_preserved"] = (outcome.get("sent_model") == expected.get("model")
        and outcome.get("returned_model") == expected.get("model")
        and outcome.get("reasoning_effort") == expected.get("effort"))
    requests = [row for row in records if row.get("event") == "native_ipc_request"]
    posts = [row for row in requests if row.get("method") == "POST"]
    post = posts[0] if len(posts) == 1 else {}
    checks["one_actual_native_post"] = len(posts) == 1 and post.get("endpoint") == "/backend-api/codex/responses"
    catalog_paths = {digest(path.encode()) for path in ("/models", "/backend-api/codex/models",
        "/models?client_version=0.155.0", "/backend-api/codex/models?client_version=0.155.0")}
    checks["only_catalog_other_ipc"] = all(row is post or row.get("method") == "GET" and
        (row.get("endpoint") in ("/models", "/backend-api/codex/models") or row.get("path_sha256") in catalog_paths) for row in requests)
    checks["complete_supported_native_body"] = (post.get("schema") in (3, 4) and post.get("body_summary_schema") == post.get("schema")
        and post.get("request_forwarded_complete") is True and post.get("ipc_head_complete") is True
        and post.get("body_digest_scope") == "complete_pre_rust_compression_body"
        and post.get("body_summary_complete") is True and post.get("body_summary_error") is None
        and post.get("observation_error") is None and post.get("body_summary_basis") == BODY_BASIS
        and hashed(post.get("actual_ipc_body_sha256")))
    starts = [row for row in records if row.get("event") == "native_ipc_helper_start"]
    exits = [row for row in records if row.get("event") == "native_ipc_helper_exit"]
    start_ids, exit_ids = [row.get("instance") for row in starts], [row.get("instance") for row in exits]
    checks["all_helper_lifecycles_closed"] = (bool(starts) and None not in start_ids
        and len(set(start_ids)) == len(start_ids) and len(set(exit_ids)) == len(exit_ids) and set(start_ids) == set(exit_ids)
        and post.get("instance") in start_ids and all(row.get("helper_returncode") == 0
            and row.get("helper_reaped") is True and row.get("helper_forced_kill") is False
            and row.get("internal_socket_absent") is True and row.get("external_socket_absent") is True
            and row.get("observation_healthy") is True for row in exits))
    checks["pinned_running_native_helper"] = (hashed(expected.get("helper_sha256")) and bool(starts)
        and all(row.get("runtime_executable_matches_helper") is True
            and row.get("runtime_executable_sha256") == expected.get("helper_sha256")
            and row.get("configured_helper_sha256") == expected.get("helper_sha256") for row in [*starts, post]))
    checks["native_lifecycle_cli_request"] = bool(starts) and all(row.get("http_client_lifecycle") == "cli_request" for row in starts)
    headers, header_spellings = defaultdict(list), defaultdict(list)
    all_headers = post.get("all_headers", [])
    header_valid = isinstance(all_headers, list)
    if header_valid:
        for row in all_headers:
            name = row.get("name") if isinstance(row, dict) else None
            if not isinstance(row, dict) or not isinstance(name, str) or not (re.fullmatch(r"[!#$%&'*+.^_`|~0-9a-z-]{1,256}", name) or re.fullmatch(r"sha256:[0-9a-f]{64}", name)) or not hashed(row.get("value_sha256")):
                header_valid = False
                break
            headers[row["name"]].append(row["value_sha256"])
            if hashed(row.get("original_name_sha256")):
                header_spellings[row["name"]].append(row["original_name_sha256"])
    checks["headers_completely_captured"] = (header_valid and post.get("header_summary_complete") is True
        and post.get("header_value_count") == len(all_headers)
        and post.get("header_names") == sorted(headers))
    auth = post.get("authorization", [])
    token_hash = auth[0].get("token_sha256") if isinstance(auth, list) and len(auth) == 1 and isinstance(auth[0], dict) else None
    checks["actual_credential_workspace_and_ingress_match"] = (hashed(token_hash) and token_hash == before.get("token_sha256") == first.get("authorization_token_sha256")
        and post.get("workspace_header_sha256") == [before.get("workspace_sha256")]
        and before.get("workspace_sha256") == first.get("workspace_sha256"))
    checks["all_catalog_requests_use_same_identity"] = all(row.get("authorization")
        and isinstance(row["authorization"], list) and len(row["authorization"]) == 1
        and isinstance(row["authorization"][0], dict) and row["authorization"][0].get("token_sha256") == before.get("token_sha256")
        and row.get("workspace_header_sha256") == [before.get("workspace_sha256")]
        and row.get("instance") in start_ids for row in requests)
    checks["actual_request_id_correlated"] = hashed(first.get("request_id_sha256")) and headers.get("x-client-request-id") == [first.get("request_id_sha256")]
    checks["native_body_digest_and_lengths_consistent"] = (hashed(post.get("actual_ipc_body_sha256"))
        and post.get("actual_ipc_body_sha256") == post.get("body_wire_sha256")
        and post.get("body_bytes_forwarded") == post.get("body_length_declared") == post.get("body_wire_bytes"))
    checks["ingress_body_recorded"] = hashed(first.get("decoded_sha256")) and finite_nonnegative(first.get("decoded_bytes"))
    checks["actual_model_effort_preserved"] = post.get("model") == expected.get("model") and post.get("reasoning_effort") == expected.get("effort")
    body_fields, original_fields = {}, post.get("body_fields", {})
    if isinstance(original_fields, dict):
        for key, value in original_fields.items():
            if summary_key(key) and field_summary(value) is not None:
                body_fields[key] = field_summary(value)
    checks["body_fields_completely_captured"] = (isinstance(original_fields, dict) and len(body_fields) == len(original_fields)
        and post.get("body_field_count") == post.get("body_fields_captured") == len(body_fields)
        and post.get("body_field_names") == sorted(body_fields)
        and all(key in body_fields for key in ("model", "input", "reasoning")))
    text = cli.get("final_output")
    answer, extraction = parse_answer(text) if isinstance(text, str) else (None, "missing_final_output")
    evidence_valid = all(checks.values())
    dispatch_checks = {key: value for key, value in checks.items() if key not in
                       ("cli_clean_completed", "single_completed_server_outcome", "server_model_effort_preserved", "no_observed_tool_execution")}
    category = "unscorable_invalid_evidence" if not evidence_valid else "ambiguous_answer" if answer is None else "correct" if answer == 21 else "wrong_answer"
    public = {"id": cell["id"], "account_alias": cell["account_alias"], "evidence_valid": evidence_valid,
              "dispatch_evidence_valid": all(dispatch_checks.values()),
              "native_capture_schema": post.get("schema") if post.get("schema") in (3, 4) else None,
              "normalized_config_sha256": config.get("config_without_endpoint_sha256") if hashed(config.get("config_without_endpoint_sha256")) else None,
              "checks": checks, "evidence_errors": errors, "failed_checks": [key for key, value in checks.items() if not value],
              "answer": answer if evidence_valid else None, "category": category, "expected_answer": 21,
              "extraction": extraction, "final_text_sha256": digest(text.encode()) if isinstance(text, str) else None,
              "cli_status": text_id(cli.get("status")), "started_at_utc": cli.get("started_at_utc") if timestamp(cli.get("started_at_utc")) else None,
              "elapsed_seconds": cli.get("elapsed_seconds") if finite_nonnegative(cli.get("elapsed_seconds")) else None,
              "request_body_sha256": post.get("actual_ipc_body_sha256") if hashed(post.get("actual_ipc_body_sha256")) else None,
              "ingress_to_ipc": {"raw_bytes_equal": checks["ingress_body_recorded"] and checks["native_body_digest_and_lengths_consistent"] and first.get("decoded_sha256") == post.get("actual_ipc_body_sha256"),
                  "decoded_ingress_bytes": first.get("decoded_bytes") if finite_nonnegative(first.get("decoded_bytes")) else None,
                  "ipc_bytes": post.get("body_wire_bytes") if finite_nonnegative(post.get("body_wire_bytes")) else None,
                  "semantic_equivalence_verified": checks["ingress_body_recorded"] and checks["native_body_digest_and_lengths_consistent"] and first.get("decoded_sha256") == post.get("actual_ipc_body_sha256"),
                  "interpretation": "different_byte_hashes_need_separate_ingress_semantic_evidence_not_assumed_corruption"},
              "identity_alias_label_matches_index": before.get("alias", cell["account_alias"]) == after.get("alias", cell["account_alias"]) == cell["account_alias"],
              "usage": safe_usage(cli.get("usage")), "response": {key: outcome.get(key) for key in
                  ("http_code", "attempts", "status", "terminal_status", "sent_model", "returned_model", "reasoning_effort", "service_tier", "error_source", "error_phase")
                  if isinstance(outcome.get(key), (int, float)) or text_id(outcome.get(key)) is not None or outcome.get(key) == ""},
              "catalog_outcome_count": sum(isinstance(row, dict) and row.get("request_type") == "models" for row in outcomes),
              "catalog_failures": sum(isinstance(row, dict) and row.get("request_type") == "models" and row.get("status") != "ok" for row in outcomes),
              "body_fields": body_fields, "header_hashes": dict(headers) if header_valid else {},
              "header_original_name_hashes": dict(header_spellings) if header_valid else {},
              "evidence_sha256": evidence}
    indicators = cli.get("diagnostic_indicators", {})
    public["cli_diagnostic_indicators"] = {key: indicators[key] for key in
        ("rate_limit", "authentication", "model_unavailable", "model_rerouted", "transport", "forbidden", "reconnecting", "model_capacity")
        if isinstance(indicators, dict) and isinstance(indicators.get(key), bool)}
    public["account_metadata"] = {key: before[key] for key in ("token_issued_at", "token_expires_at", "primary_used_percent", "secondary_used_percent")
                                  if finite_nonnegative(before.get(key))}
    if text_id(before.get("auth_plan")) is not None:
        public["account_metadata"]["auth_plan"] = before["auth_plan"]
    metadata_summary = post.get("client_metadata_summary", {})
    metadata = metadata_summary.get("keys", {}) if isinstance(metadata_summary, dict) else {}
    public["client_metadata_fields"] = {key: field_summary(value) for key, value in metadata.items()
        if summary_key(key) and field_summary(value) is not None} if isinstance(metadata, dict) else {}
    inp = post.get("input_summary", {})
    public["input_items"] = [{"index": row["index"], "content_sha256": row["content_sha256"], "item_type": text_id(row.get("item_type"))}
        for row in inp.get("items", []) if isinstance(row, dict) and isinstance(row.get("index"), int)
        and hashed(row.get("content_sha256"))] if isinstance(inp, dict) and isinstance(inp.get("items", []), list) else []
    if post.get("schema") == 4 and isinstance(inp, dict) and isinstance(inp.get("items"), list):
        input_by_index = {row.get("index"): row for row in inp["items"] if isinstance(row, dict) and isinstance(row.get("index"), int)}
        for item in public["input_items"]:
            source = input_by_index.get(item["index"], {})
            item["fields"] = object_field_summary(source.get("field_summary"))
            item["internal_metadata_fields"] = object_field_summary(source.get("internal_metadata_summary"))
    private = {"identity": tuple(before.get(key) for key in IDENTITY_FIELDS),
               "config_sha256": config.get("config_without_endpoint_sha256"),
               "cli_sha256": cli.get("cli_sha256"), "prompt_sha256": cli.get("prompt_sha256"),
               "start": timestamp(cli.get("started_at_utc")), "elapsed": public["elapsed_seconds"]}
    return public, private


def compare_cells(left, right):
    def differences(a, b, names=None):
        keys = sorted(set(a) | set(b)) if names is None else names
        return [key for key in keys if a.get(key) != b.get(key) or (key in a) != (key in b)]
    body_changes = differences(left["body_fields"], right["body_fields"])
    left_items = {str(row["index"]): row for row in left["input_items"]}
    right_items = {str(row["index"]): row for row in right["input_items"]}
    item_details = []
    for index in sorted(set(left_items) | set(right_items), key=int):
        a, b = left_items.get(index, {}), right_items.get(index, {})
        full_equal = bool(a) and bool(b) and a.get("content_sha256") == b.get("content_sha256")
        fields_known = isinstance(a.get("fields"), dict) and isinstance(b.get("fields"), dict)
        metadata_known = isinstance(a.get("internal_metadata_fields"), dict) and isinstance(b.get("internal_metadata_fields"), dict)
        item_details.append({"index": int(index), "left_present": bool(a), "right_present": bool(b),
                             "full_item_hash_equal": full_equal, "field_comparison_available": fields_known,
                             "fields_changed": differences(a["fields"], b["fields"]) if fields_known else None,
                             "internal_metadata_comparison_available": metadata_known,
                             "internal_metadata_keys_changed": differences(a["internal_metadata_fields"], b["internal_metadata_fields"]) if metadata_known else None})
    fixed_headers = differences(left["header_hashes"], right["header_hashes"], FIXED_HEADERS)
    fixed_equal = not any(key in FIXED_FIELDS for key in body_changes)
    current_contrast = (left["evidence_valid"] and right["evidence_valid"] and left["account_alias"] != right["account_alias"]
                        and left["answer"] is not None and right["answer"] is not None and (left["answer"] == 21) != (right["answer"] == 21))
    return {"left": left["id"], "right": right["id"], "same_account_alias": left["account_alias"] == right["account_alias"],
            "both_evidence_valid": left["evidence_valid"] and right["evidence_valid"],
            "fixed_fields_equal": fixed_equal,
            "fixed_fields_changed": [key for key in body_changes if key in FIXED_FIELDS],
            "session_fields_changed": [key for key in body_changes if key in SESSION_FIELDS],
            "other_fields_changed": [key for key in body_changes if key not in (*FIXED_FIELDS, *SESSION_FIELDS)],
            "header_fields_changed": differences(left["header_hashes"], right["header_hashes"]),
            "header_name_spelling_comparison_available": bool(left["header_original_name_hashes"]) and bool(right["header_original_name_hashes"]),
            "header_name_spelling_changed": differences(left["header_original_name_hashes"], right["header_original_name_hashes"])
                if left["header_original_name_hashes"] and right["header_original_name_hashes"] else None,
            "fixed_header_fields_changed": fixed_headers,
            "current_correct_wrong_contrast": current_contrast,
            "contrast_with_matched_fixed_controls": current_contrast and fixed_equal and not fixed_headers
                and left["normalized_config_sha256"] == right["normalized_config_sha256"],
            "client_metadata_fields_changed": differences(left["client_metadata_fields"], right["client_metadata_fields"]),
            "input_item_indices_changed": differences(left_items, right_items),
            "input_item_details": item_details,
            "usage_changed": differences(left["usage"], right["usage"]),
            "response_fields_changed": differences(left["response"], right["response"]),
            "answers": [left["answer"], right["answer"]],
            "complete_request_equivalent": left["request_body_sha256"] == right["request_body_sha256"] and left["request_body_sha256"] is not None and left["header_hashes"] == right["header_hashes"],
            "complete_body_bytes_equal": left["request_body_sha256"] == right["request_body_sha256"] and left["request_body_sha256"] is not None}


def build_report(index_path):
    index_path = Path(index_path).resolve()
    raw_index = bounded_read(index_path)
    index = json_value(raw_index)
    if not isinstance(index, dict) or index.get("schema") != 1:
        raise ValueError("unsupported_experiment_index_schema")
    planned, cells, expected = index.get("planned_order"), index.get("cells"), index.get("expected", {})
    skipped = index.get("skipped_cells", [])
    if not isinstance(planned, list) or not isinstance(cells, list) or not 1 <= len(planned) <= 64 or len(cells) > len(planned) or not isinstance(expected, dict):
        raise ValueError("invalid_experiment_index")
    if not isinstance(skipped, list):
        raise ValueError("invalid_skipped_cells")
    for entries in (planned, cells, skipped):
        if any(not isinstance(row, dict) or not isinstance(row.get("id"), str) or LABEL.fullmatch(row["id"]) is None
               or not isinstance(row.get("account_alias"), str) or LABEL.fullmatch(row["account_alias"]) is None for row in entries):
            raise ValueError("invalid_cell_identifier")
        if len({row["id"] for row in entries}) != len(entries):
            raise ValueError("duplicate_cell_identifier")
    result, details = [], []
    for cell in cells:
        public, private = load_cell(index_path.parent, cell, expected)
        result.append(public)
        details.append(private)
    executed = [{key: row[key] for key in ("id", "account_alias")} for row in cells]
    desired = [{key: row[key] for key in ("id", "account_alias")} for row in planned]
    omissions = [{key: row[key] for key in ("id", "account_alias")} for row in skipped]
    aliases = [row["account_alias"] for row in planned]
    design = "ABBA" if len(aliases) == 4 and aliases[0] == aliases[3] and aliases[1] == aliases[2] and aliases[0] != aliases[1] else (
        "ABCCBA" if len(aliases) == 6 and aliases == list(reversed(aliases)) and len(set(aliases[:3])) == 3 else "unsupported")
    ids, skipped_ids = {row["id"] for row in executed}, {row["id"] for row in omissions}
    remaining_plan = [row for row in desired if row["id"] not in skipped_ids]
    skip_valid = (not ids.intersection(skipped_ids) and all(row in desired for row in omissions)
                  and omissions == [row for row in desired if row["id"] in skipped_ids])
    cell_map = {row["id"]: row for row in result}
    for row in skipped:
        if row.get("reason") == "prior_invalid_reference_same_account":
            position = next((i for i, item in enumerate(desired) if item == {key: row[key] for key in ("id", "account_alias")}), -1)
            skip_valid = skip_valid and any(item["account_alias"] == row["account_alias"] and item["id"] in cell_map
                and not cell_map[item["id"]]["evidence_valid"] for item in desired[:position])
        elif row.get("reason") != "preflight_source_quota_exhausted":
            skip_valid = False
    checks = {"executed_order_is_planned_prefix": executed == desired[:len(executed)],
              "supported_balanced_plan": design != "unsupported",
              "registered_skips_valid": skip_valid,
              "execution_matches_plan_after_registered_skips": executed == remaining_plan[:len(executed)],
              "all_planned_cells_executed": len(cells) == len(planned), "runner_finished": index.get("finished") is True,
              "credentials_removed": index.get("credentials_removed") is True,
              "all_cells_evidence_valid": bool(result) and all(row["evidence_valid"] for row in result),
              "all_cells_answer_unambiguous": bool(result) and all(row["category"] in ("correct", "wrong_answer") for row in result),
              "normalized_config_equal": bool(details) and all(hashed(row["config_sha256"]) for row in details) and len({row["config_sha256"] for row in details}) == 1}
    checks["runner_automatic_retries_disabled"] = index.get("automatic_retries") is False
    started = [row["start"] for row in details]
    elapsed = [row["elapsed"] for row in details]
    valid_times = bool(started) and all(value is not None for value in started) and all(value is not None for value in elapsed)
    checks["cell_times_sequential"] = valid_times and all(started[i] + timedelta(seconds=elapsed[i]) <= started[i+1] for i in range(len(started)-1))
    window = (max(started[i] + timedelta(seconds=elapsed[i]) for i in range(len(started))) - min(started)).total_seconds() if valid_times else None
    if "max_window_seconds" in index:
        checks["within_declared_time_window"] = finite_nonnegative(index["max_window_seconds"]) and window is not None and window <= index["max_window_seconds"]
    pairs = [compare_cells(left, right) for left, right in combinations(result, 2)]
    checks["fixed_input_fields_equal"] = bool(result) and all(pair["fixed_fields_equal"] for pair in pairs)
    checks["fixed_transport_headers_equal"] = bool(result) and all(not pair["fixed_header_fields_changed"] for pair in pairs)
    by_alias, private_by_alias = defaultdict(list), defaultdict(list)
    for row, detail in zip(result, details):
        by_alias[row["account_alias"]].append(row)
        private_by_alias[row["account_alias"]].append(detail)
    checks["same_alias_epoch_stable_across_cells"] = all(len({repr(row["identity"]) for row in rows}) == 1 for rows in private_by_alias.values())
    identities = [rows[0]["identity"][0] for rows in private_by_alias.values()]
    checks["aliases_are_distinct_accounts"] = len(identities) >= 2 and all(hashed(value) for value in identities) and len(set(identities)) == len(identities)
    summaries = []
    for alias, rows in sorted(by_alias.items()):
        scored = [row for row in rows if row["category"] in ("correct", "wrong_answer")]
        correct = sum(row["category"] == "correct" for row in scored)
        classification = ("insufficient_valid_cells" if len(scored) != len(rows) or len(scored) < 2 else
                          "all_correct_this_experiment" if correct == len(scored) else
                          "all_wrong_this_experiment" if correct == 0 else "mixed_this_experiment")
        summaries.append({"account_alias": alias, "executed_cells": len(rows), "scored_cells": len(scored),
                          "answers": [row["answer"] for row in scored], "correct": correct,
                          "wrong": len(scored)-correct, "classification": classification})
    classes = {row["classification"] for row in summaries}
    paired_classes = {"all_correct_this_experiment", "all_wrong_this_experiment"} <= classes
    verified = all(checks.values())
    conclusion = ("同期观察到两类答题结果，固定请求控制字段通过核对；仍需核对完整input差异，不能据此推断隐藏集群或账号标记。" if verified and paired_classes else
                  "没有形成可验证的同期两类账号对照；以本轮逐格答案和证据缺口为准。")
    return {"schema": SCHEMA, "network_requests": 0, "index_sha256": digest(raw_index), "expected_answer": 21,
            "planned_design": design, "planned_cell_count": len(planned), "executed_cell_count": len(cells),
            "skipped_cells": [{"id": row["id"], "account_alias": row["account_alias"], "reason": row.get("reason")
                if row.get("reason") in ("prior_invalid_reference_same_account", "preflight_source_quota_exhausted") else "unsupported_reason"} for row in skipped],
            "offline_tool_sha256": digest(Path(__file__).read_bytes()),
            "answer_parser_sha256": digest(Path(__file__).with_name("quality_probe_report.py").read_bytes()),
            "experiment_checks": checks, "failed_experiment_checks": [key for key, value in checks.items() if not value],
            "complete_controlled_comparison": verified, "concurrent_correct_and_wrong_account_classes": verified and paired_classes,
            "conclusion": conclusion, "execution_window_seconds": window, "cells": result, "account_summaries": summaries,
            "pairwise_comparisons": pairs, "limits": [
                "历史账号标签不参与分类；仅本轮完整、干净、身份核对通过的结果评分。",
                "actual IPC 是 Rust 压缩/TLS 之前边界，不是提供方内部路由或实际算力证明。",
                "固定字段相同不等于整份请求相同；input、metadata、cache/session 差异逐项保留。",
                "入站与IPC字节不同不直接等于语义损坏；缺少入站字段摘要时逐格标记语义等价未证明。",
                "fresh home、无重试与执行状态依赖已保存的 runner/CLI 证据；摘要不是服务端证明。",
                "回答21的两次小样本不建立普遍正常账号结论；同错或同对也不证明不存在差异。",
                "中断/跳过的全局设计不完整；有效跨账号单格差异可观察但不能替代缺失的预登记重复。",
                "没有把工具项未被观察到解释为请求不含任何工具；tools 实际摘要独立比较。"]}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("index", type=Path)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    try:
        report = build_report(args.index)
    except (OSError, ValueError, TypeError, RecursionError):
        print("multiaccount_comparison_failed_invalid_or_unreadable_index", file=sys.stderr)
        return 1
    if args.output.resolve() == args.index.resolve():
        print("output_must_not_overwrite_index", file=sys.stderr)
        return 1
    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(json.dumps(report, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    print(json.dumps({key: report[key] for key in ("complete_controlled_comparison", "concurrent_correct_and_wrong_account_classes", "conclusion")}, ensure_ascii=False))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
