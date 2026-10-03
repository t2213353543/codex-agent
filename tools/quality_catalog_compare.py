#!/usr/bin/env python3
"""Compare anonymous Codex model catalogs offline without retaining instructions.

python tools/quality_catalog_compare.py private-input.json --output report.json
Input: {schema: INPUT_SCHEMA, accounts: [{alias: "q0", catalog: {models: [...]},
provenance: {source: "network", client_version: "0.155.0", http_status: 200},
auth_plan: {claim_source: "id_token", chatgpt_plan_type: "plus"}}]}.
catalog_file (relative to the input file) may replace catalog. For server probes,
call compare_accounts(records) with raw catalogs in memory and persist only its
return value. This module never opens a network connection or decodes JWTs.
"""
from __future__ import annotations

import argparse
import hashlib
from itertools import combinations
import json
from pathlib import Path
import re
import sys

VERSION = "0.155.0"
INPUT_SCHEMA = "codex-quality-catalog-input-v1"
REPORT_SCHEMA = "codex-quality-catalog-report-v1"
MAX_BYTES = 32 * 1024 * 1024
MAX_ACCOUNTS = 64
MAX_MODELS = 512
MISSING = object()
SOURCES = {"network", "cache", "bundled", "fallback", "unknown"}
CLAIM_SOURCES = {"id_token", "access_token", "operator_metadata", "unknown"}
PLANS = {
    "free", "go", "plus", "pro", "prolite", "team", "business", "ent26",
    "self_serve_business_prolite", "self_serve_business_usage_based",
    "enterprise_cbp_automation", "enterprise_cbp_usage_based", "enterprise",
    "hc", "education", "edu", "edu_plus", "edu_pro",
}
EFFORTS = {"none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra", "persistent"}
ENUMS = {
    "default_reasoning_level": EFFORTS, "effort": EFFORTS,
    "multi_agent_reasoning_effort": EFFORTS,
    "visibility": {"list", "hide", "none"},
    "shell_type": {"unified_exec", "disabled", "default", "local", "shell_command"},
    "default_reasoning_summary": {"auto", "concise", "detailed", "none"},
    "default_verbosity": {"low", "medium", "high"},
    "apply_patch_tool_type": {"freeform"},
    "web_search_tool_type": {"text", "text_and_image"},
    "tool_mode": {"direct", "code_mode", "code_mode_only"},
    "default_service_tier": {"auto", "default", "flex", "priority", "fast"},
    "model_specialty": {"cyber"},
}
BOOLEAN_FIELDS = {
    "supported_in_api", "support_verbosity", "include_skills_usage_instructions",
    "include_plugin_usage_instructions", "include_apps_usage_instructions",
    "supports_reasoning_summary_parameter", "supports_image_detail_original",
    "used_fallback_model_metadata", "supports_search_tool",
    "supports_experimental_context", "use_responses_lite",
    "node_repl_auto_review_required", "node_repl_disabled", "supports_apikey",
}
INTEGER_FIELDS = {
    "priority", "context_window", "max_context_window", "auto_compact_token_limit",
    "effective_context_window_percent",
}
KNOWN_FIELDS = BOOLEAN_FIELDS | INTEGER_FIELDS | set(ENUMS) | {
    "slug", "display_name", "description", "supported_reasoning_levels", "guardian",
    "additional_speed_tiers", "service_tiers", "availability_nux", "upgrade",
    "model_messages", "base_instructions", "truncation_policy", "comp_hash",
    "experimental_supported_tools", "input_modalities", "auto_review_model_override",
    "multi_agent_version",
}
# These non-optional fields are required by the pinned ModelInfo wire schema.
# The tool is a raw comparison, not a replacement for serde's full deserializer.
REQUIRED_TYPES = {
    "slug": str, "display_name": str, "supported_reasoning_levels": list,
    "shell_type": str, "visibility": str, "supported_in_api": bool, "priority": int,
    "support_verbosity": bool, "truncation_policy": dict,
    "experimental_supported_tools": list,
}


class CatalogError(Exception):
    """Errors contain fixed codes only; never echo raw JSON or file paths."""


def unique_object(pairs):
    out = {}
    for name, value in pairs:
        if name in out:
            raise CatalogError("duplicate_json_key")
        out[name] = value
    return out


def parse_json(raw):
    if len(raw) > MAX_BYTES:
        raise CatalogError("input_too_large")
    try:
        return json.loads(raw, object_pairs_hook=unique_object,
                          parse_constant=lambda _: (_ for _ in ()).throw(CatalogError("nonfinite_json")))
    except (ValueError, UnicodeError, RecursionError) as exc:
        raise CatalogError("invalid_json") from exc


def digest(value):
    try:
        encoded = json.dumps(value, ensure_ascii=True, sort_keys=True, allow_nan=False,
                             separators=(",", ":")).encode("utf-8")
    except (ValueError, TypeError, RecursionError) as exc:
        raise CatalogError("invalid_json_value") from exc
    return hashlib.sha256(encoded).hexdigest()


def field_name(name):
    return name if name in KNOWN_FIELDS else "unknown_field_sha256:" + digest(name)


def model_name(slug):
    if (len(slug) <= 100 and re.fullmatch(
            r"(?:gpt-[a-zA-Z0-9._-]+|o[1-9][a-zA-Z0-9._-]*|codex(?:-[a-zA-Z0-9._-]+)?|chatgpt-[a-zA-Z0-9._-]+)", slug)):
        return slug
    return "model_sha256:" + digest(slug)


def describe(value=MISSING, field=None):
    if value is MISSING:
        return {"state": "missing"}
    kind = {type(None): "null", bool: "boolean", int: "integer", float: "number",
            str: "string", list: "array", dict: "object"}.get(type(value))
    if kind is None:
        raise CatalogError("invalid_json_value")
    out = {"state": "present", "type": kind, "sha256": digest(value)}
    if isinstance(value, (str, list, dict)):
        out["length"] = len(value)
    if (value is None or (field in BOOLEAN_FIELDS and type(value) is bool)
            or (field in INTEGER_FIELDS and type(value) is int)
            or (field in ENUMS and isinstance(value, str) and value in ENUMS[field])):
        out["value"] = value
    if field == "slug" and isinstance(value, str):
        out["model_key"] = model_name(value)
    if field == "input_modalities" and isinstance(value, list):
        out["items"] = [({"value": item} if type(item) is str and item in {"text", "image", "audio"}
                         else describe(item)) for item in value]
    if field == "supported_reasoning_levels" and isinstance(value, list):
        out["efforts"] = [describe(item.get("effort", MISSING), "effort")
                          if isinstance(item, dict) else describe(item) for item in value]
    return out


def auth_plan_summary(plan):
    if plan is MISSING:
        return {"state": "missing"}
    if not isinstance(plan, dict):
        return {"state": "invalid"}
    source = plan.get("claim_source", "unknown")
    out = {"claim_source": source if isinstance(source, str) and source in CLAIM_SOURCES else "unknown",
           "claim_verification": "not_verified_by_comparator"}
    value = plan.get("chatgpt_plan_type", MISSING)
    out["chatgpt_plan_type"] = describe(value)
    if isinstance(value, str) and value in PLANS:
        out["chatgpt_plan_type"]["value"] = value
    # This is a documented routing claim, not a quality/abuse/account-risk flag.
    value = plan.get("chatgpt_account_is_fedramp", MISSING)
    out["chatgpt_account_is_fedramp"] = describe(value)
    if type(value) is bool:
        out["chatgpt_account_is_fedramp"]["value"] = value
    # Do not hash the entire auth object: unknown claims can contain credentials/IDs.
    return out


def catalog_issues(catalog):
    issues = []
    if not isinstance(catalog, dict) or not isinstance(catalog.get("models"), list):
        return ["invalid_catalog_schema"]
    if "error" in catalog or "errors" in catalog:
        issues.append("error_envelope")
    models = catalog["models"]
    if len(models) > MAX_MODELS:
        return ["too_many_models"]
    seen = set()
    for model in models:
        if not isinstance(model, dict):
            issues.append("invalid_model_schema")
            continue
        if any(type(model.get(name)) is not kind for name, kind in REQUIRED_TYPES.items()):
            issues.append("invalid_model_schema")
        slug = model.get("slug")
        if not isinstance(slug, str) or not slug or slug.strip() != slug or len(slug) > 256:
            issues.append("invalid_model_slug")
        elif slug in seen:
            issues.append("duplicate_model_slug")
        else:
            seen.add(slug)
        levels = model.get("supported_reasoning_levels", [])
        if isinstance(levels, list) and any(
            not isinstance(item, dict) or not isinstance(item.get("effort"), str)
            or not item["effort"] or not isinstance(item.get("description"), str) for item in levels
        ):
            issues.append("invalid_reasoning_levels")
    return sorted(set(issues))


def summarize_account(record):
    if not isinstance(record, dict):
        raise CatalogError("invalid_account_record")
    alias = record.get("alias")
    if not isinstance(alias, str) or not re.fullmatch(r"q[0-9]{1,4}", alias):
        raise CatalogError("invalid_anonymous_alias")
    provenance = record.get("provenance")
    if not isinstance(provenance, dict):
        provenance = {}
    source = provenance.get("source", "unknown")
    if not isinstance(source, str) or source not in SOURCES:
        source = "unknown"
    version = provenance.get("client_version", MISSING)
    status = provenance.get("http_status", MISSING)
    reasons = []
    if source != "network":
        reasons.append("not_network_catalog")
    if version != VERSION:
        reasons.append("client_version_missing_or_mismatch")
    if status is not MISSING and (type(status) is not int or status != 200):
        reasons.append("http_status_not_200")
    catalog = record.get("catalog", MISSING)
    issues = catalog_issues(catalog)
    reasons += issues
    if isinstance(catalog, dict):
        if any(name in catalog for name in ("fetched_at", "identity", "etag", "client_version")):
            if source == "network":
                reasons.append("cache_envelope_as_network")
        if isinstance(catalog.get("models"), list) and any(
                isinstance(model, dict) and model.get("used_fallback_model_metadata") is True
                for model in catalog["models"]):
            reasons.append("fallback_model_metadata")
    out = {
        "alias": alias,
        "provenance": {"source": source, "client_version": describe(version),
                       "http_status": describe(status),
                       "http_status_observed": status is not MISSING},
        "auth_plan": auth_plan_summary(record.get("auth_plan", MISSING)),
        "comparable": not reasons, "exclusion_reasons": sorted(set(reasons)),
        "catalog_schema_issues": issues,
        "models": [],
    }
    if version == VERSION:
        out["provenance"]["client_version"]["value"] = VERSION
    if type(status) is int and 100 <= status <= 599:
        out["provenance"]["http_status"]["value"] = status
    if isinstance(catalog, dict):
        # Whole catalog metadata may contain identity; retain model hashes only.
        if isinstance(catalog.get("models"), list):
            out["raw_models_sha256"] = digest(catalog["models"])
        if source in {"cache", "fallback"}:
            out["cache_client_version"] = describe(catalog.get("client_version", MISSING))
    # Reject duplicates/malformed entries as a unit; do not select the last duplicate.
    if issues:
        return out
    for model in sorted(catalog["models"], key=lambda item: item["slug"]):
        out["models"].append({"model_key": model_name(model["slug"]),
                              "model_sha256": digest(model),
                              "fields": {field_name(name): describe(value, name)
                                         for name, value in sorted(model.items())}})
    out["model_count"] = len(out["models"])
    return out


def compare_pair(left, right):
    out = {"left": left["alias"], "right": right["alias"],
           "comparable": left["comparable"] and right["comparable"]}
    if not out["comparable"]:
        out["status"] = "excluded"
        return out
    a = {model["model_key"]: model for model in left["models"]}
    b = {model["model_key"]: model for model in right["models"]}
    out.update({"status": "compared", "only_left": sorted(a.keys() - b.keys()),
                "only_right": sorted(b.keys() - a.keys()), "shared_models": [],
                "auth_plan_equal": left["auth_plan"] == right["auth_plan"]})
    for slug in sorted(a.keys() & b.keys()):
        af, bf = a[slug]["fields"], b[slug]["fields"]
        differences = []
        for name in sorted(af.keys() | bf.keys()):
            av, bv = af.get(name, {"state": "missing"}), bf.get(name, {"state": "missing"})
            if av != bv:
                differences.append({"field": name, "left": av, "right": bv})
        out["shared_models"].append({"model_key": slug, "equal": not differences,
                                     "differences": differences})
    out["catalogs_equal_by_model"] = (not out["only_left"] and not out["only_right"]
                                      and all(item["equal"] for item in out["shared_models"]))
    return out


def compare_accounts(records):
    """Return a sanitized report. All raw objects remain only in caller memory."""
    if not isinstance(records, list) or not 1 <= len(records) <= MAX_ACCOUNTS:
        raise CatalogError("invalid_account_count")
    accounts = [summarize_account(record) for record in records]
    if len({item["alias"] for item in accounts}) != len(accounts):
        raise CatalogError("duplicate_alias")
    return {"schema": REPORT_SCHEMA, "client_version_expected": VERSION,
            "interpretation": "catalog_facts_only_not_cluster_or_account_flag_evidence",
            "schema_validation": "required_model_fields_not_full_cli_serde",
            "accounts": accounts,
            "comparisons": [compare_pair(a, b) for a, b in combinations(accounts, 2)]}


def read_json(path):
    try:
        with path.open("rb") as handle:
            return parse_json(handle.read(MAX_BYTES + 1))
    except OSError as exc:
        raise CatalogError("input_read_failed") from exc


def load_records(path):
    document = read_json(path)
    if not isinstance(document, dict) or document.get("schema") != INPUT_SCHEMA:
        raise CatalogError("invalid_input_schema")
    records = document.get("accounts")
    if not isinstance(records, list) or not 1 <= len(records) <= MAX_ACCOUNTS:
        raise CatalogError("invalid_account_count")
    for record in records:
        if not isinstance(record, dict):
            raise CatalogError("invalid_account_record")
        if "catalog_file" in record:
            if "catalog" in record or not isinstance(record["catalog_file"], str):
                raise CatalogError("ambiguous_catalog_input")
            record["catalog"] = read_json(path.parent / record.pop("catalog_file"))
    return records


class Parser(argparse.ArgumentParser):
    def error(self, message):
        raise CatalogError("invalid_arguments")


def main(argv=None):
    try:
        parser = Parser(description=__doc__)
        parser.add_argument("input", type=Path)
        parser.add_argument("--output", required=True, type=Path)
        args = parser.parse_args(argv)
        report = compare_accounts(load_records(args.input))
        try:
            args.output.write_text(json.dumps(report, indent=2, ensure_ascii=True) + "\n", encoding="utf-8")
        except OSError as exc:
            raise CatalogError("output_write_failed") from exc
        print(json.dumps({"status": "written", "account_count": len(report["accounts"])}))
        return 0
    except CatalogError as exc:
        print(json.dumps({"status": "error", "code": str(exc)}), file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
