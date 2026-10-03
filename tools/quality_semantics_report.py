"""Offline verification and separate scoring of the four-call semantics block.

python tools/quality_semantics_report.py RUN_DIRECTORY --output REPORT.json
Reads local artifacts only. Configured input hashes and predicted upstream body
hashes are not independent upstream wire captures or evidence of a server cluster.
"""
from __future__ import annotations

import argparse
from collections import Counter
import hashlib
import json
from pathlib import Path
import re

from quality_probe_report import redact
from quality_semantics_variants import content_hash, request_content, score_variant_record


SCHEMA = "codex-quality-semantics-report-v1"
MAX_BYTES = 8 * 1024 * 1024
HASH = re.compile(r"[0-9a-f]{64}\Z")
REGISTERED_CASES = (
    ("baseline-direct", "baseline", "direct"),
    ("baseline-transparent", "baseline", "transparent"),
    ("clarified-transparent", "clarified_shape_choice", "transparent"),
    ("blind-transparent", "blind_control", "transparent"),
)
EVIDENCE_KIND = "configured input checked against harness serialized request SHA, not upstream capture"
EPOCH_FIELDS = ("identity_sha256", "token_sha256", "assigned_at", "oauth_updated_at")
ENV_KEYS = {"PATH", "LANG", "CODEX_QUALITY_PROBE_CONFIG", "CODEX_NATIVE_HTTP_TEST_BIN",
            "CODEX_NATIVE_HTTP_TEST_LIFECYCLE"}
TASK_CATEGORIES = {"correct", "wrong_answer", "ambiguous"}


def pairs_no_duplicates(pairs):
    value = {}
    for key, item in pairs:
        if key in value:
            raise ValueError("duplicate_json_key")
        value[key] = item
    return value


def parse_json(raw):
    return json.loads(raw, object_pairs_hook=pairs_no_duplicates,
                      parse_constant=lambda _: (_ for _ in ()).throw(ValueError("nonfinite_json_number")))


def read_artifact(root: Path, relative: str, jsonl=False):
    target = (root / relative).resolve()
    if not target.is_relative_to(root.resolve()):
        raise ValueError("artifact_path_escapes_run")
    if target.stat().st_size > MAX_BYTES:
        raise ValueError("artifact_exceeds_size_limit")
    raw = target.read_text(encoding="utf-8")
    if jsonl:
        lines = [line for line in raw.splitlines() if line.strip()]
        if len(lines) != 1:
            raise ValueError("expected_exactly_one_result")
        raw = lines[0]
    value = parse_json(raw)
    if not isinstance(value, dict):
        raise ValueError("artifact_not_object")
    return value


def go_request_sha256(request: dict) -> str:
    """Match encoding/json for this finite string/int/bool Responses request.

    Go sorts map keys and escapes HTML and U+2028/U+2029 even with UTF-8 Chinese.
    Reject floats rather than assume Python and Go number serialization match.
    """
    def check(value):
        if value is None or isinstance(value, (str, bool, int)):
            return
        if isinstance(value, list):
            for child in value:
                check(child)
            return
        if isinstance(value, dict) and all(isinstance(key, str) for key in value):
            for child in value.values():
                check(child)
            return
        raise ValueError("unsupported_go_json_value")
    check(request)
    text = json.dumps(request, ensure_ascii=False, sort_keys=True, separators=(",", ":"), allow_nan=False)
    for before, after in (("&", "\\u0026"), ("<", "\\u003c"), (">", "\\u003e"),
                          ("\u2028", "\\u2028"), ("\u2029", "\\u2029")):
        text = text.replace(before, after)
    return hashlib.sha256(text.encode("utf-8")).hexdigest()


def has_hash(value):
    return isinstance(value, str) and HASH.fullmatch(value) is not None


def same_epoch(before, after, alias):
    return (isinstance(before, dict) and isinstance(after, dict)
            and before.get("alias") == after.get("alias") == alias
            and all(key in before and key in after and before[key] == after[key] for key in EPOCH_FIELDS)
            and all(has_hash(before[key]) for key in EPOCH_FIELDS[:2]))


def numeric_usage(value):
    if not isinstance(value, dict):
        return {}
    result = {key: value[key] for key in ("input_tokens", "output_tokens", "total_tokens")
              if isinstance(value.get(key), int) and not isinstance(value[key], bool)}
    for group, key in (("input_tokens_details", "cached_tokens"), ("output_tokens_details", "reasoning_tokens")):
        child = value.get(group)
        if isinstance(child, dict) and isinstance(child.get(key), int) and not isinstance(child[key], bool):
            result[key] = child[key]
    return result


def visible_metadata(value):
    """Preserve allowlisted observations without inheriting schema-1 diagnoses."""
    if not isinstance(value, dict):
        return {"present": False}
    signals = []
    for signal in value.get("signals", []) if isinstance(value.get("signals"), list) else []:
        if not isinstance(signal, dict) or signal.get("field") not in {"request_id", "model", "reasoning_effort", "service_tier"}:
            continue
        out = {"field": signal["field"]}
        if isinstance(signal.get("source"), str) and len(signal["source"]) < 128:
            out["source"] = signal["source"]
        if has_hash(signal.get("value_sha256")):
            out["value_sha256"] = signal["value_sha256"]
        if signal["field"] != "request_id" and isinstance(signal.get("value"), str) and len(signal["value"]) < 128:
            out["value"] = signal["value"]
        signals.append(out)
    tiers = {item.get("value") for item in signals if item["field"] == "service_tier" and "value" in item}
    result = {"present": True, "collector_schema": value.get("schema"), "signals": signals,
              "capture_complete": value.get("capture_complete") is True,
              "signals_dropped": value.get("signals_dropped"),
              "interpretation": "visible metadata only; no account-flag, quality or internal-cluster proof"}
    if value.get("schema") == 1:
        conflicts = value.get("conflicts")
        result["collector_reported_conflicts"] = {key: conflicts[key] for key in
            ("model", "reasoning_effort", "request_id", "service_tier") if isinstance(conflicts, dict) and isinstance(conflicts.get(key), bool)}
        result["schema1_service_tier_flag_is_not_a_server_conflict_diagnosis"] = True
    if tiers == {"auto", "default"}:
        result["service_tier_note"] = "auto to default is compatible with normal lifecycle tier selection; it does not prove downgrade or a server conflict"
    return result


def audit_case(root, case, plan, before, execution):
    label, variant, arm = (case[key] for key in ("label", "variant", "arm"))
    errors = []
    result = {"label": label, "variant_id": variant, "arm": arm, "category": "evidence_invalid",
              "original_benchmark_eligible": False, "original_benchmark_pass": False,
              "evidence_errors": errors}
    try:
        row = read_artifact(root, f"{label}/results.jsonl", jsonl=True)
        evidence = read_artifact(root, f"{label}-input-evidence.json")
        after = read_artifact(root, f"{label}-identity-after.json")
        process = read_artifact(root, f"{label}-process.json")
    except (OSError, ValueError, UnicodeError) as error:
        errors.append("missing_or_invalid_case_artifact:" + type(error).__name__)
        return result
    request = evidence.get("request")
    if not isinstance(request, dict):
        errors.append("request_object_missing")
        return result
    expected_request = {"model": plan.get("model"),
                        "input": [{"role": "user", "content": [{"type": "input_text", "text": request_content(variant)["input"]}]}],
                        "instructions": request_content(variant)["instructions"],
                        "stream": True, "store": False, "reasoning": {"effort": plan.get("effort")}}
    if request != expected_request:
        errors.append("request_not_exact_registered_settings_and_variant")
    try:
        digest = go_request_sha256(request)
    except (ValueError, UnicodeError):
        digest = None
    if not digest or digest != row.get("request_sha256") or digest != evidence.get("request_sha256"):
        errors.append("configured_and_harness_request_hash_mismatch")
    if evidence.get("evidence_kind") != EVIDENCE_KIND:
        errors.append("request_evidence_kind_missing_or_changed")
    registered = next((x for x in plan["calls"] if x["label"] == label), {})
    if registered.get("content_sha256") != content_hash(request_content(variant)):
        errors.append("planned_variant_content_hash_mismatch")
    if row.get("arm") != arm or row.get("account_alias") != plan.get("account_alias"):
        errors.append("result_arm_or_account_mismatch")
    if row.get("model") != plan.get("model") or row.get("effort") != plan.get("effort"):
        errors.append("result_requested_model_or_effort_mismatch")
    if row.get("attempts") != 1 or isinstance(row.get("attempts"), bool):
        errors.append("client_attempt_count_not_one")
    if row.get("round") != 1 or row.get("sequence") != 1:
        errors.append("unexpected_harness_round_or_sequence")
    if row.get("prompt_sha256") != hashlib.sha256(request_content(variant)["input"].encode("utf-8")).hexdigest():
        errors.append("harness_prompt_hash_mismatch")
    if not (same_epoch(before, after, plan.get("account_alias"))
            and same_epoch(before, process.get("source_before"), plan.get("account_alias"))
            and row.get("credential_sha256") == before.get("token_sha256")):
        errors.append("identity_or_credential_epoch_unverified")
    env = process.get("environment")
    if not (isinstance(env, dict) and set(env) == ENV_KEYS
            and all(isinstance(value, str) and value for value in env.values())
            and env.get("CODEX_NATIVE_HTTP_TEST_LIFECYCLE") == "cli_request"
            and Path(env.get("CODEX_QUALITY_PROBE_CONFIG", "")).name == label + "-config.json"):
        errors.append("probe_environment_not_registered_clean_environment")
    for field in ("helper_sha256", "probe_sha256"):
        if not has_hash(execution.get(field)) or process.get(field) != execution[field]:
            errors.append(field + "_mismatch_or_missing")
    if row.get("http_status") != case.get("http_status") or bool(row.get("protocol_success")) != case.get("protocol_success"):
        errors.append("execution_summary_conflicts_with_raw_result")
    response_effort = (row.get("response_reasoning") or {}).get("effort") if isinstance(row.get("response_reasoning"), dict) else None
    if row.get("response_model") and row["response_model"] != plan.get("model"):
        errors.append("returned_model_mismatch")
    if response_effort and response_effort != plan.get("effort"):
        errors.append("returned_effort_mismatch")
    gateway_attempts = row.get("gateway_attempt_count")
    if arm == "transparent" and gateway_attempts is not None and (isinstance(gateway_attempts, bool)
            or not isinstance(gateway_attempts, int) or gateway_attempts < 0 or gateway_attempts > 1):
        errors.append("gateway_attempt_count_invalid_or_multiple")
    try:
        score = score_variant_record(row, variant, request)
        result.update(score)
        result["observed_task_category"] = score["category"]
    except (ValueError, TypeError):
        errors.append("variant_scoring_input_invalid")
    result.update(label=label, arm=arm, exit_code=case.get("exit_code"),
                  request_sha256=digest, predicted_upstream_body_sha256=row.get("predicted_upstream_body_sha256") if has_hash(row.get("predicted_upstream_body_sha256")) else None,
                  request_evidence_kind=EVIDENCE_KIND,
                  upstream_wire_capture=False,
                  identity_and_credential_epoch_verified="identity_or_credential_epoch_unverified" not in errors,
                  environment_verified="probe_environment_not_registered_clean_environment" not in errors,
                  environment_sha256=content_hash({key: value for key, value in env.items() if key != "CODEX_QUALITY_PROBE_CONFIG"}) if isinstance(env, dict) else None,
                  credential_epoch=content_hash({key: before.get(key) for key in EPOCH_FIELDS}),
                  account_alias=plan.get("account_alias"), model=row.get("model"), effort=row.get("effort"),
                  response_model=row.get("response_model"), response_effort=response_effort,
                  response_status=row.get("response_status"),
                  http_status=row.get("http_status"), attempts=row.get("attempts"), gateway_attempt_count=gateway_attempts,
                  started_at=row.get("started_at"), duration_ms=row.get("duration_ms"),
                  request_id=row.get("request_id"),
                  response_service_tier=row.get("response_service_tier"), usage=numeric_usage(row.get("response_usage")),
                  upstream_metadata=visible_metadata(row.get("upstream_metadata")),
                  evidence_errors=errors, evidence_valid=not errors)
    if errors:
        result.update(category="evidence_invalid", original_benchmark_eligible=False, original_benchmark_pass=False)
    return result


def baseline_comparison(cases, integrity_valid):
    pair = [item for item in cases if item["variant_id"] == "baseline"]
    reasons = []
    if len(pair) != 2 or {item["arm"] for item in pair} != {"direct", "transparent"}:
        reasons.append("requires_both_baseline_arms")
    elif not integrity_valid or not all(item.get("evidence_valid") for item in pair):
        reasons.append("artifact_evidence_invalid")
    else:
        for field in ("request_sha256", "predicted_upstream_body_sha256", "prompt_sha256", "credential_epoch",
                      "model", "effort", "environment_sha256"):
            if not pair[0].get(field) or pair[0][field] != pair[1].get(field):
                reasons.append(field + "_missing_or_changed")
        gateway_case = next(item for item in pair if item["arm"] == "transparent")
        if gateway_case.get("gateway_attempt_count") != 1:
            reasons.append("gateway_attempt_count_not_proven_one")
    return {"cases": [item["label"] for item in pair], "body_comparable": not reasons,
            "body_comparability_scope": "same verified configured request and same predicted upstream body; not wire capture",
            "upstream_wire_capture": False, "exclusions": reasons,
            "outcomes": [{key: item.get(key) for key in ("label", "answer", "category")} for item in pair],
            "causal_attribution": "none; single ordered observations cannot establish gateway quality, account flags, or clusters"}


def counts(cases):
    return {"attempted_cases": len(cases), "categories": dict(Counter(item["category"] for item in cases)),
            "scorable_completed": sum(item["category"] in {"correct", "wrong_answer"} for item in cases),
            "correct": sum(item["category"] == "correct" for item in cases)}


def build_report(directory: Path) -> dict:
    root = directory.resolve()
    errors = []
    report = {"schema": SCHEMA, "network_requests_by_reporter": 0,
              "interpretation": "Separate original-task and diagnostic-control observations; no causal or hidden-cluster conclusion.",
              "integrity_errors": errors, "cases": []}
    try:
        execution = read_artifact(root, "execution-report.json")
        plan = read_artifact(root, "plan.json")
        before = read_artifact(root, "identity-before.json")
    except (OSError, ValueError, UnicodeError) as error:
        errors.append("missing_or_invalid_block_artifact:" + type(error).__name__)
        report.update(integrity_valid=False, execution_state="evidence_invalid")
        return report
    planned = plan.get("calls")
    executed = execution.get("cases")
    if not isinstance(planned, list) or not all(isinstance(item, dict) for item in planned):
        planned = []
        errors.append("invalid_plan_cases")
    tuples = [tuple(item.get(key) for key in ("label", "variant", "arm")) for item in planned]
    if tuples != list(REGISTERED_CASES) or plan.get("max_calls") != 4 or isinstance(plan.get("max_calls"), bool):
        errors.append("plan_not_registered_four_call_block")
    if (plan.get("automatic_retries") is not False or plan.get("stop_on_any_incomplete_request") is not True
            or plan.get("controls_count_as_original_success") is not False
            or plan.get("no_expected_answers_in_model_context") is not True):
        errors.append("plan_execution_or_scoring_policy_changed")
    if plan.get("account_alias") != "q4" or not isinstance(plan.get("model"), str) or not isinstance(plan.get("effort"), str):
        errors.append("plan_account_or_model_or_effort_invalid")
    if not isinstance(executed, list) or not all(isinstance(item, dict) for item in executed):
        executed = []
        errors.append("invalid_execution_cases")
    executed_tuples = [tuple(item.get(key) for key in ("label", "variant", "arm")) for item in executed]
    if len(executed) > 4 or executed_tuples != list(REGISTERED_CASES[:len(executed)]):
        errors.append("execution_not_unique_registered_prefix")
    expected_results = {f"{label}/results.jsonl" for label, _, _ in executed_tuples if isinstance(label, str)}
    observed_results = {path.relative_to(root).as_posix() for path in root.rglob("results.jsonl")}
    if expected_results != observed_results:
        errors.append("missing_or_unreported_case_results")
    for suffix in ("-input-evidence.json", "-process.json", "-identity-after.json"):
        expected_names = {label + suffix for label, _, _ in executed_tuples if isinstance(label, str)}
        observed_names = {path.name for path in root.glob("*" + suffix)}
        if expected_names != observed_names:
            errors.append("missing_or_unreported_case_sidecars:" + suffix)
    if execution.get("credential_snapshot_removed") is not True or (root / "credentials.json").exists():
        errors.append("credential_snapshot_cleanup_unverified")
    seen = set()
    for case, registered in zip(executed, REGISTERED_CASES):
        key = tuple(case.get(field) for field in ("label", "variant", "arm"))
        if key != registered or key in seen or not planned:
            continue
        seen.add(key)
        report["cases"].append(audit_case(root, case, plan, before, execution))
    for index, case in enumerate(report["cases"][:-1]):
        if case.get("observed_task_category") not in TASK_CATEGORIES or case.get("exit_code") != 0:
            errors.append("calls_continued_after_incomplete_request")
    if errors:
        for case in report["cases"]:
            case.update(category="evidence_invalid", original_benchmark_eligible=False, original_benchmark_pass=False)
    valid = not errors and all(case.get("evidence_valid") for case in report["cases"])
    all_done = len(report["cases"]) == 4 and all(case["category"] in TASK_CATEGORIES and case.get("exit_code") == 0 for case in report["cases"])
    stopped = execution.get("stopped_after_incomplete_request") is True
    if valid and stopped and all_done:
        errors.append("stop_flag_conflicts_with_all_completed_cases")
        valid = False
        for case in report["cases"]:
            case.update(category="evidence_invalid", original_benchmark_eligible=False, original_benchmark_pass=False)
    state = ("evidence_invalid" if not valid else "completed" if all_done and execution.get("finished") is True
             else "stopped_incomplete" if stopped else "partial")
    originals = [case for case in report["cases"] if case["variant_id"] == "baseline"]
    controls = [case for case in report["cases"] if case["variant_id"] != "baseline"]
    report.update(integrity_valid=valid, execution_state=state, planned_max_calls=4,
                  observed_case_count=len(executed), runner_finished=execution.get("finished") is True,
                  stopped_after_incomplete_request=stopped,
                  missing_cases=[label for label, variant, arm in REGISTERED_CASES if (label, variant, arm) not in seen],
                  original_task=counts(originals), diagnostic_controls=counts(controls),
                  controls_count_as_original_success=False,
                  baseline_comparison=baseline_comparison(report["cases"], valid),
                  process_evidence={key: execution.get(key) for key in ("helper_sha256", "probe_sha256")},
                  credential_snapshot_removed=execution.get("credential_snapshot_removed") is True,
                  runner_error_class=execution.get("error_class"))
    return redact(report)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("run_directory", type=Path)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    report = build_report(args.run_directory)
    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(json.dumps(report, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    print(json.dumps({"report": str(args.output.resolve()), "integrity_valid": report["integrity_valid"],
                      "execution_state": report["execution_state"], "observed_cases": len(report["cases"])}, ensure_ascii=False))
    return 0 if report["integrity_valid"] else 2


if __name__ == "__main__":
    raise SystemExit(main())
