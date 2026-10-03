#!/usr/bin/env python3
"""Read-only codex-agent production observer. No inference or remediation.

python tools/codex_agent_live_monitor.py --remote
python tools/codex_agent_live_monitor.py --prepare-units .local/live-monitor/units
"""
import argparse
import collections
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import re
import shlex
import subprocess
import sys
import tempfile

CONTAINER = "codex-agent-postgres"
SERVICE = "codex-agent.service"
DECRYPT = "/opt/codex-agent-integration/checks/decrypt-helper-5ca8b78cde0c9bc4"
REMOTE_SCRIPT = "/opt/codex-agent-integration/live-monitor/codex_agent_live_monitor.py"
REMOTE_STATE = "/var/lib/codex-agent-observer"
MAX_SAMPLE_ROWS = 5000
MAX_ERROR_CHARS = 1024
MONITOR_PGOPTIONS = "-c default_transaction_read_only=on -c statement_timeout=10000 -c lock_timeout=1000 -c work_mem=4MB -c hash_mem_multiplier=1 -c temp_file_limit=128MB -c max_parallel_workers_per_gather=0 -c jit=off"
OBSERVATION_KEYS = {
    "tool_policy": ("effective_choice", "rejection_phase", "rejected_family", "rejected_identity",
                    "catalog_at_prepare_count", "named_namespaces_at_prepare_count", "catalog_at_rejection_count",
                    "named_namespaces_at_rejection_count", "tool_search_at_prepare", "structured", "rejected_matches_exact_declaration"),
    "structured_mixed": ("formatter_count", "client_call_count", "other_call_count", "has_text", "has_refusal"),
    "bound_selection": ("bound_snapshot_state", "snapshot_origin", "replay_class", "recovery_outcome",
                        "eligible_candidate_count", "snapshot_age_ms"),
}
SAFE_SETTINGS = ("settings_version", "inference_mode", "sticky_sessions_enabled", "sticky_session_ttl_seconds",
    "prompt_cache_key_enabled", "bps_prompt_cache_scope", "cache_creation_as_read_enabled", "builtin_prompt_enabled", "builtin_prompt_mode", "upstream_stream_idle_timeout_seconds")

# Keep this endpoint boundary aligned with stats_realtime.go. A model value is
# not evidence of generation: it is absent on early Responses failures and on
# control requests such as /models. The observer counts persisted requests,
# whereas the dashboard's in-memory RPM counts ingress; label that distinction.
INFERENCE_ENDPOINTS = (
    "/responses", "/responses/compact", "/guardian", "/guardian-classifier",
    "/alpha/search", "/images/generations", "/images/edits",
    "/memories/trace_summarize", "/realtime/calls", "/live", "/compact", "/realtime",
)
INFERENCE_RECORD_SQL = (
    "regexp_replace('/'||ltrim(coalesce(record->>'endpoint',''),'/'), "
    "'^/(backend-api/codex|v1)(/|$)', '/') IN ("
    + ",".join("'" + endpoint + "'" for endpoint in INFERENCE_ENDPOINTS)
    + ") AND coalesce(record->>'websocket_warmup','false') <> 'true'"
)

# Existing request/error totals include control traffic so no error disappears.
# Generation-only counts and usage are separate and are used for RPM/TPM/cache.
CACHE_READ_OBSERVED_SQL = """coalesce((record->>'usage_known')::boolean,input_tokens<>0 OR output_tokens<>0 OR cached_tokens<>0)
AND coalesce((record->>'cached_tokens_known')::boolean,cached_tokens>0)
AND input_tokens>=cached_tokens AND input_tokens>=0 AND cached_tokens>=0"""
CACHE_WRITE_VALUE_SQL = """CASE WHEN jsonb_typeof(record->'cache_write_tokens')='number'
THEN (record->>'cache_write_tokens')::bigint END"""
AGGREGATE_SQL = """count(*) requests,count(*) FILTER(WHERE status='ok') ok,
count(*) FILTER(WHERE status='error') errors,count(*) FILTER(WHERE status='aborted') aborted,
count(*) FILTER(WHERE is_inference) inference_requests,
count(*) FILTER(WHERE is_inference AND status='ok') inference_ok,
count(*) FILTER(WHERE NOT is_inference) control_requests,
count(*) FILTER(WHERE NOT is_inference AND status='error') control_errors,
coalesce(sum(input_tokens) FILTER(WHERE is_inference),0) input_tokens,
coalesce(sum(output_tokens) FILTER(WHERE is_inference),0) output_tokens,
coalesce(sum(cached_tokens) FILTER(WHERE is_inference),0) cached_tokens,
round(100.0*sum(cached_tokens) FILTER(WHERE is_inference AND ({read_known}))/nullif(sum(input_tokens) FILTER(WHERE is_inference AND ({read_known})),0),4) cache_percent,
count(*) FILTER(WHERE is_inference AND ({read_known})) cache_read_samples,
count(*) FILTER(WHERE is_inference AND NOT coalesce(({read_known}),false)) cache_read_unknown_requests,
coalesce(sum(input_tokens) FILTER(WHERE is_inference AND ({read_known})),0) cache_read_input_tokens,
sum(({write_value})) FILTER(WHERE is_inference AND ({write_value})>=0) cache_write_tokens,
count(*) FILTER(WHERE is_inference AND ({write_value})>=0) cache_write_samples,
count(*) FILTER(WHERE is_inference AND cached_tokens>0) cache_hit_requests,
count(*) FILTER(WHERE is_inference AND record->>'usage_known'='true') usage_known_requests,
count(*) FILTER(WHERE is_inference AND status='ok' AND coalesce(record->>'usage_known','false')<>'true' AND input_tokens=0 AND output_tokens=0) successful_usage_unreported,
round(avg((record->>'ttft_ms')::numeric) FILTER(WHERE is_inference),2) ttft_ms_avg,
round(avg((record->>'latency_ms')::numeric) FILTER(WHERE is_inference),2) latency_ms_avg""".format(
    read_known=CACHE_READ_OBSERVED_SQL, write_value=CACHE_WRITE_VALUE_SQL)

# Each report scans the same recent rows for multiple time/group windows. Parse
# these JSON scalars once per request, rather than inside every aggregate scan.
# Keep one statement/MVCC snapshot and a bounded read-only execution budget.
CACHE_OBSERVATION_PROJECTION_SQL = "(" + CACHE_READ_OBSERVED_SQL + ") cache_read_observed,(" + CACHE_WRITE_VALUE_SQL + ") cache_write_observed,(record->>'ttft_ms')::numeric observed_ttft_ms,(record->>'latency_ms')::numeric observed_latency_ms"
SNAPSHOT_AGGREGATE_SQL = AGGREGATE_SQL.replace(CACHE_READ_OBSERVED_SQL, "cache_read_observed").replace(
    CACHE_WRITE_VALUE_SQL, "cache_write_observed").replace("(record->>'ttft_ms')::numeric", "observed_ttft_ms").replace(
    "(record->>'latency_ms')::numeric", "observed_latency_ms")


class MonitorReadError(RuntimeError):
    def __init__(self, category):
        super().__init__("monitor_read_operation_failed")
        self.category = category


def read_error_category(stderr):
    text = stderr.decode("utf-8", "replace").lower() if isinstance(stderr, bytes) else str(stderr).lower()
    for marker, category in (("canceling statement due to statement timeout", "statement_timeout"),
                             ("canceling statement due to lock timeout", "lock_timeout"),
                             ("connection refused", "connection_refused"), ("permission denied", "permission_denied")):
        if marker in text:
            return category
    return "read_operation_failed"


def command(args, input_bytes=None, timeout=35):
    try:
        value = subprocess.run(args, input=input_bytes, capture_output=True, timeout=timeout)
    except subprocess.TimeoutExpired:
        raise MonitorReadError("subprocess_timeout") from None
    if value.returncode:
        # Command, database DSN, server stderr and plaintext keys stay private.
        raise MonitorReadError(read_error_category(value.stderr))
    return value.stdout.decode("utf-8", "replace").strip()


def sql(query):
    text = command(["docker", "exec", "-e", "PGOPTIONS=" + MONITOR_PGOPTIONS,
        CONTAINER, "psql", "-X", "-q", "-At", "-v", "ON_ERROR_STOP=1", "-U", "postgres", "-d", "codex_agent", "-c", query])
    return json.loads(text or "null")


def redact(value):
    value = str(value or "")
    value = re.sub(r"(?i)Bearer\s+\S+", "Bearer [redacted]", value)
    value = re.sub(r"https?://[^\s\"<>]+", "[url]", value)
    value = re.sub(r"[\w.+-]+@[\w.-]+\.[A-Za-z]{2,}", "[email]", value)
    value = re.sub(r"\b(?:sk-|eyJ)[A-Za-z0-9_.-]{15,}", "[credential]", value)
    return value[:800]


def observed_error_parameter(row):
    value = row.get("error_param")
    allowed = {"stream_options", "stream_options.include_usage", "stream_options.include_obfuscation", "stream_options.reasoning_summary_delivery", "stream_options.unknown",
               "service_tier", "reasoning.effort", "max_output_tokens", "top_p", "text", "text.format", "text.format.type", "text.format.name", "text.format.strict", "text.format.description", "text.format.schema"}
    return value if isinstance(value, str) and value in allowed else None


def observed_structured_mixed(row):
    value = row.get("structured_mixed")
    if not isinstance(value, dict):
        return None
    result = {}
    for name in ("formatter_count", "client_call_count", "other_call_count"):
        count = value.get(name)
        if type(count) is int and 0 <= count <= 9223372036854775807:
            result[name] = count
    for name in ("has_text", "has_refusal"):
        if type(value.get(name)) is bool:
            result[name] = value[name]
    return result


def observed_bound_selection(row):
    value = row.get("bound_selection")
    if not isinstance(value, dict):
        return None
    result = {}
    enums = {
        "bound_snapshot_state": {"exact_disabled", "exact_group_denied", "missing_or_replaced", "unclassified"},
        "snapshot_origin": {"fresh_read", "cache_hit", "stale_on_read_error"},
        "replay_class": {"full_plaintext", "previous_response", "opaque_or_file", "unobserved"},
        "recovery_outcome": {"blocked_input", "blocked_context", "blocked_delegation", "blocked_other_guard", "candidates_exhausted", "replacement_failed", "recovered"},
    }
    for name, allowed in enums.items():
        if isinstance(value.get(name), str) and value[name] in allowed:
            result[name] = value[name]
    for name in ("eligible_candidate_count", "snapshot_age_ms"):
        count = value.get(name)
        if type(count) is int and 0 <= count <= 9223372036854775807:
            result[name] = count
    return result


def observed_tool_policy(row):
    value = row.get("tool_policy")
    if not isinstance(value, dict):
        return None
    result = {}
    enums = {
        "effective_choice": {"auto", "none", "required", "forced"},
        "rejection_phase": {"normalize_item", "parallel_policy", "validate_response"},
        "rejected_family": {"function_call", "custom_tool_call", "tool_search_call", "tool_search_output", "web_search_call", "other_call", "other"},
        "rejected_identity": {"exact", "unique_namespace", "ambiguous_namespace", "undeclared", "unobserved", "internal_formatter", "not_client_action"},
    }
    for key, allowed in enums.items():
        if isinstance(value.get(key), str) and value[key] in allowed:
            result[key] = value[key]
    for key in ("catalog_at_prepare_count", "named_namespaces_at_prepare_count", "catalog_at_rejection_count", "named_namespaces_at_rejection_count"):
        count = value.get(key)
        if type(count) is int and 0 <= count <= 9223372036854775807:
            result[key] = count
    for key in ("tool_search_at_prepare", "structured", "rejected_matches_exact_declaration"):
        if type(value.get(key)) is bool:
            result[key] = value[key]
    return result


def classify_error(row):
    error = str(row.get("error") or "")
    # These are final local outcomes with explicit provenance, not provider
    # quota. Do not infer them from messages or an earlier upstream attempt.
    if row.get("status") == "error" and row.get("error_source") == "gateway":
        if row.get("error_phase") == "account_selection" and str(row.get("http_code")) == "429":
            return "gateway_capacity"
        mixed = observed_structured_mixed(row) or {}
        if (row.get("error_phase") == "response_validation" and str(row.get("http_code")) in {"200", "502"}
                and len(mixed) == 5 and (mixed["formatter_count"] > 1 or
                    mixed["formatter_count"] == 1 and (mixed["client_call_count"] > 0 or
                        mixed["other_call_count"] > 0 or mixed["has_text"] or mixed["has_refusal"]))):
            return "structured_validation"
    # Keep prior upstream provenance for diagnosis, but a final local failure
    # (or caller abort) owns the request category after generation has ended.
    upstream_failure = row.get("status") != "aborted" and str(row.get("error_source") or "") in {"", "unreported", "upstream"}
    # The selected generation failure is independent of chronological network
    # attempts: a later OAuth failure may precede returning an earlier 429.
    final = str(row.get("final_upstream_error_code") or "")
    if upstream_failure and row.get("final_upstream_error_source") == "upstream":
        if final == "upstream_account_rejected":
            return "account_rejected"
        if final in {"serverOverloaded", "server_overloaded", "model_overloaded", "overloaded_error"}:
            return "upstream_busy"
        if final in {"rateLimitExceeded", "usageLimitExceeded", "rate_limit_exceeded", "rate_limit_error", "insufficient_quota", "billing_hard_limit_reached", "basispoints_route_rate_limit_exceeded"}:
            return "upstream_rate_limit"
        if final in {"unauthorized", "authentication_error", "invalid_api_key", "authentication_unavailable"}:
            return "authentication"
    codes = {str(row.get("error_code") or "")} if upstream_failure else set()
    # Earlier failed accounts are not the final request failure. A truncated
    # list contains the first attempts, so its last element is not final either.
    attempts = row.get("attempts") or []
    if upstream_failure and attempts and not row.get("attempts_truncated"):
        codes.add(str(attempts[-1].get("error_code") or ""))
    if "upstream_account_rejected" in codes:
        return "account_rejected"
    # Busy is an explicit protocol code, never inferred from HTTP 502 alone.
    busy_codes = {"serverOverloaded", "server_overloaded", "model_overloaded", "overloaded_error"}
    explicit_text = any(re.search(r"(?:\(|\bcode[\"']?\s*[:=]\s*[\"']?)" + re.escape(code) + r"(?:\)|[\"']|\b)", error) for code in busy_codes)
    if upstream_failure and (codes & busy_codes or explicit_text):
        return "upstream_busy"
    limit_codes = {"rate_limit_exceeded", "rate_limit_error", "usage_limit_reached", "rateLimitExceeded", "usageLimitExceeded", "insufficient_quota", "billing_hard_limit_reached", "basispoints_route_rate_limit_exceeded"}
    explicit_limit = any(re.search(r"\bcode=" + re.escape(code) + r"\)", error) or
        re.match(r"^official (?:App Server (?:inference error|turn failed)|compaction(?: turn)? failed) \(" + re.escape(code) + r"\)(?::|$)", error)
        for code in limit_codes)
    if upstream_failure and (codes & limit_codes or explicit_limit):
        return "upstream_rate_limit"
    if observed_error_parameter(row) and row.get("error_source") == "client" and row.get("error_phase") == "request_validation":
        return "request_parameter"
    policy = observed_tool_policy(row)
    if policy and policy.get("rejection_phase") or "undeclared tool call" in error or "custom tool wrapper" in error or re.fullmatch(r"bps: tool (?:function|custom)/[^\r\n]+ has [0-9]+ matching namespaces; cannot route", error):
        return "tool_protocol"
    if "remote image URLs" in error or "image_fetch_failed" in error:
        return "image_protocol"
    if "invalid_encrypted_content" in error:
        return "encrypted_history"
    if "all eligible accounts" in error or "session account" in error or "no available accounts" in error:
        return "account_selection"
    if "different inference mode" in error:
        return "mode_binding_conflict"
    if "management operation could not be persisted" in error:
        return "management_persistence"
    if str(row.get("http_code")) == "401" or "authentication_unavailable" in codes:
        return "authentication"
    return "other_error"


def cache_ratio(input_tokens, cached_tokens):
    return round(100 * cached_tokens / input_tokens, 4) if input_tokens else None


def active_runtime_unit():
    path = Path("/var/lib/codex-agent-release/active-runtime.json")
    if not path.exists():
        return SERVICE, 0
    registry = json.loads(path.read_text())
    active = registry.get("active") or {}
    unit = active.get("unit")
    if registry.get("schema") != 1 or not isinstance(unit, str) or not re.fullmatch(r"codex-agent(?:@[A-Za-z0-9_.-]+|-runtime-[a-f0-9]{12,64})?\.service", unit):
        raise ValueError("invalid active runtime registry")
    draining = registry.get("draining") or []
    if not isinstance(draining, list):
        raise ValueError("invalid draining runtime registry")
    return unit, len(draining)


def read_runtime():
    service, draining_count = active_runtime_unit()
    state = command(["env", "TZ=UTC", "systemctl", "show", service, "--property=MainPID,ActiveState,SubState,NRestarts,ExecMainStartTimestamp"])
    fields = dict(line.split("=", 1) for line in state.splitlines() if "=" in line)
    pid = fields.get("MainPID", "0")
    if not pid.isdigit() or int(pid) <= 0:
        return {"service": service, "state": fields}, {}
    env = dict(item.split(b"=", 1) for item in Path("/proc/" + pid + "/environ").read_bytes().split(b"\0") if b"=" in item)
    # Read the current encrypted settings directly. No stale /client-key and no
    # API login request are needed; no admin key is decrypted or exported.
    stored = sql("SELECT json_build_object('value',value,'version',version) FROM settings WHERE key='admin_settings'")
    encrypted = stored["value"]
    value = {"Key": env[b"CREDENTIAL_ENCRYPTION_KEY"].decode(), "KeyID": env.get(b"CREDENTIAL_ENCRYPTION_KEY_ID", b"primary").decode(),
             "Ciphertext": encrypted["ciphertext"]}
    settings = json.loads(command([DECRYPT], json.dumps(value).encode()))
    if isinstance(settings, str):
        settings = json.loads(settings)
    if "settings" in settings:
        settings = settings["settings"]
    settings["settings_version"] = stored["version"]
    digest = hashlib.sha256()
    with Path("/proc/" + pid + "/exe").open("rb") as executable:
        for chunk in iter(lambda: executable.read(1024 * 1024), b""):
            digest.update(chunk)
    process_start = fields.get("ExecMainStartTimestamp", "")
    process_start_utc = command(["date", "--date", process_start, "-u", "+%Y-%m-%dT%H:%M:%SZ"]) if process_start else None
    managed_registry = Path("/var/lib/codex-agent-release/active-runtime.json")
    uses_registry = managed_registry.exists()
    if uses_registry:
        active = json.loads(managed_registry.read_text()).get("active") or {}
        if active.get("unit") != service or active.get("binary_sha256") != digest.hexdigest():
            raise ValueError("active runtime identity changed during observation")
    return {"service": service, "pid": int(pid), "binary_sha256": digest.hexdigest(), "process_start_utc": process_start_utc, "state": fields, "expected_application_port": 18879,
            "draining_instances": draining_count, "process_time_window_may_include_draining_requests": uses_registry,
            "process_window_scope": "shared_database_time_window" if uses_registry else "legacy_time_window"}, settings


def summarize_ingress_document(document, captured_at):
    """Pre-admission failures are separate from model usage; never double-count."""
    events = document.get("events") or []
    allowed = {"invalid_api_key", "settings_unavailable", "key_store_unavailable",
               "method_not_allowed", "unknown_endpoint", "websocket_disabled"}
    groups = collections.Counter()
    for event in events:
        reason = event.get("reason")
        if reason not in allowed or not isinstance(event.get("time_ms"), (int, float)):
            continue
        age = captured_at.timestamp() - event["time_ms"] / 1000
        for minutes in (1, 20, 60, 360):
            if 0 <= age <= minutes * 60:
                groups[(minutes, reason, event.get("http_code"))] += 1
    return {"scope": "separately_retained_pre_admission_errors", "retained_events": len(events),
        "next_sequence": document.get("next_sequence", 0), "evicted_total": document.get("evicted_total", 0),
        "windows": [{"minutes": key[0], "reason": key[1], "http_code": key[2], "count": count}
                    for key, count in sorted(groups.items())], "included_in_token_or_cache_totals": False}


def read_ingress(runtime, captured_at):
    stored = sql("SELECT value FROM settings WHERE key='management/ingress-events/cmVjZW50'")
    if not stored:
        return summarize_ingress_document({}, captured_at)
    env = dict(item.split(b"=", 1) for item in Path("/proc/" + str(runtime["pid"]) + "/environ").read_bytes().split(b"\0") if b"=" in item)
    value = {"Key": env[b"CREDENTIAL_ENCRYPTION_KEY"].decode(), "KeyID": env.get(b"CREDENTIAL_ENCRYPTION_KEY_ID", b"primary").decode(),
             "Ciphertext": stored["ciphertext"]}
    entry = json.loads(command([DECRYPT], json.dumps(value).encode()))
    if isinstance(entry, str):
        entry = json.loads(entry)
    return summarize_ingress_document(entry.get("payload") or {}, captured_at)


def parse_observed_timestamp(value):
    """Parse PostgreSQL/systemd UTC observations on the server's Python 3.9.

    PostgreSQL omits trailing fractional zeros; Python before 3.11 only accepts
    three or six digits in fromisoformat. Pad without rounding or losing time.
    Invalid/naive values remain errors rather than silently using the clock.
    """
    match = re.fullmatch(r"(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2})(?:\.([0-9]{1,6}))?(Z|[+-]\d{2}:\d{2})", value)
    if not match:
        raise ValueError("invalid observed timestamp")
    base, fraction, offset = match.groups()
    normalized = base + (("." + fraction.ljust(6, "0")) if fraction else "")
    return datetime.fromisoformat(normalized + ("+00:00" if offset == "Z" else offset))


def snapshot_query(runtime):
    # Limit narrow indexed IDs before touching TOASTed JSON. One extra ID gives
    # an honest coverage boundary without counting/scanning the full window.
    cte = """WITH candidates AS MATERIALIZED (
 SELECT id,created_at FROM request_logs WHERE created_at>=now()-interval '6 hours' AND created_at<=now()
 ORDER BY created_at DESC,id DESC LIMIT 5001
), sample_ids AS MATERIALIZED (
 SELECT id,created_at FROM candidates ORDER BY created_at DESC,id DESC LIMIT 5000
), source AS (SELECT l.id,
 l.created_at,
 l.account_ref,
 l.status,
 left(l.model,256) model,
 (regexp_replace('/'||ltrim(coalesce(j.endpoint,''),'/'), '^/(backend-api/codex|v1)(/|$)', '/') IN ('/responses','/responses/compact','/guardian','/guardian-classifier','/alpha/search','/images/generations','/images/edits','/memories/trace_summarize','/realtime/calls','/live','/compact','/realtime') AND coalesce(j.websocket_warmup,'false') <> 'true') is_inference,
 coalesce(j.input_tokens::bigint,0) input_tokens,
 coalesce(j.output_tokens::bigint,0) output_tokens,
 coalesce(j.cached_tokens::bigint,0) cached_tokens,
 j.usage_known observed_usage_known,
 j.cached_tokens_known observed_cached_tokens_known,
 j.cache_write_tokens observed_write_json,
 j.ttft_ms::numeric observed_ttft_ms,
 j.latency_ms::numeric observed_latency_ms,
 left(j.request_type,128) observed_request_type,
 left(coalesce(j.user_agent,'unreported'),300) observed_user_agent,
 CASE WHEN l.status<>'ok' THEN left(j.http_code,128) END observed_http_code,
 CASE WHEN l.status<>'ok' THEN left(j.error_code,128) END observed_error_code,
 CASE WHEN l.status<>'ok' THEN left(j.error_source,128) END observed_error_source,
 CASE WHEN l.status<>'ok' THEN left(j.error_phase,128) END observed_error_phase,
 CASE WHEN l.status<>'ok' THEN left(j.final_upstream_error_code,128) END observed_final_upstream_error_code,
 CASE WHEN l.status<>'ok' THEN left(j.final_upstream_error_source,128) END observed_final_upstream_error_source,
 CASE WHEN l.status<>'ok' THEN left(j.error_param,128) END observed_error_param,
 CASE WHEN l.status<>'ok' THEN left(j.error,1024) END observed_error,
 CASE WHEN l.status<>'ok' THEN CASE WHEN pg_column_size(j.tool_policy)<=2048 THEN j.tool_policy END END observed_tool_policy,
 CASE WHEN l.status<>'ok' THEN CASE WHEN pg_column_size(j.structured_mixed)<=2048 THEN j.structured_mixed END END observed_structured_mixed,
 CASE WHEN l.status<>'ok' THEN CASE WHEN pg_column_size(j.bound_selection)<=2048 THEN j.bound_selection END END observed_bound_selection,
 CASE WHEN l.status<>'ok' THEN CASE WHEN jsonb_typeof(j.attempts)='array' AND jsonb_array_length(j.attempts)>0 THEN jsonb_build_array(jsonb_build_object('error_code',left(j.attempts->-1->>'error_code',128))) END END observed_attempts,
 CASE WHEN l.status<>'ok' THEN CASE WHEN jsonb_typeof(j.attempts_truncated)='boolean' THEN j.attempts_truncated END END observed_attempts_truncated,
 coalesce(length(j.error)>1024,false) OR coalesce(pg_column_size(j.tool_policy)>2048,false) OR coalesce(pg_column_size(j.structured_mixed)>2048,false) OR coalesce(pg_column_size(j.bound_selection)>2048,false) OR coalesce(length(j.attempts->-1->>'error_code')>128,false) observed_diagnostic_truncated,
 coalesce(length(l.model)>256,false) OR coalesce(length(j.request_type)>128,false) observed_dimension_truncated
 FROM sample_ids selected JOIN request_logs l ON l.id=selected.id CROSS JOIN LATERAL jsonb_to_record(l.record) AS j(endpoint text,websocket_warmup text,input_tokens text,output_tokens text,cached_tokens text,usage_known text,cached_tokens_known text,ttft_ms text,latency_ms text,request_type text,user_agent text,http_code text,error_code text,error_source text,error_phase text,final_upstream_error_code text,final_upstream_error_source text,error_param text,error text,cache_write_tokens jsonb,tool_policy jsonb,structured_mixed jsonb,bound_selection jsonb,attempts jsonb,attempts_truncated jsonb)
),
recent AS MATERIALIZED (SELECT source.*,
 coalesce(observed_usage_known::boolean,input_tokens<>0 OR output_tokens<>0 OR cached_tokens<>0) AND coalesce(observed_cached_tokens_known::boolean,cached_tokens>0) AND input_tokens>=cached_tokens AND input_tokens>=0 AND cached_tokens>=0 cache_read_observed,
 CASE WHEN jsonb_typeof(observed_write_json)='number' THEN (observed_write_json#>>'{}')::bigint END cache_write_observed
 FROM source),
windows AS (SELECT * FROM (VALUES (1),(20),(60),(360)) AS w(minutes)) """
    # Nested envelopes may contain arbitrary producer metadata. The Python
    # consumer already accepts only these keys; remove the rest before json_agg
    # so 5000 small-but-opaque objects cannot inflate its allocation state.
    for field, keys in OBSERVATION_KEYS.items():
        expression = "j." + field
        bounded = ("CASE WHEN jsonb_typeof(" + expression + ")='object' AND pg_column_size(" + expression + ")<=2048 THEN "
                   "(SELECT coalesce(jsonb_object_agg(key,value),'{}'::jsonb) FROM jsonb_each(" + expression + ") "
                   "WHERE key IN (" + ",".join("'"+key+"'" for key in keys) + ") AND pg_column_size(value)<=128) END")
        cte = cte.replace("CASE WHEN pg_column_size(" + expression + ")<=2048 THEN " + expression + " END", bounded)
    aggregate = SNAPSHOT_AGGREGATE_SQL
    sections = {
        "captured_at": "SELECT to_json(now())",
        "last_id": "SELECT coalesce(max(id),0) FROM request_logs",
        "sampling": "SELECT json_build_object('scope','latest_5000_retained_records_within_6h','row_limit',5000,'rows',(SELECT count(*) FROM sample_ids),'truncated',(SELECT count(*)>5000 FROM candidates),'oldest_included_at',(SELECT min(created_at) FROM sample_ids),'newest_included_at',(SELECT max(created_at) FROM sample_ids),'first_excluded_at',(SELECT created_at FROM candidates ORDER BY created_at DESC,id DESC OFFSET 5000 LIMIT 1),'error_text_limit_chars',1024,'diagnostics_truncated',(SELECT count(*) FROM recent WHERE observed_diagnostic_truncated),'dimension_values_truncated',(SELECT count(*) FROM recent WHERE observed_dimension_truncated))",
        "windows": "SELECT coalesce(json_agg(t),'[]'::json) FROM (SELECT w.minutes,(SELECT row_to_json(a) FROM (SELECT " + aggregate + " FROM recent r WHERE r.created_at>=now()-w.minutes*interval '1 minute')a) totals FROM windows w ORDER BY minutes)t",
        "mode_windows": "SELECT coalesce(json_agg(t),'[]'::json) FROM (SELECT w.minutes,coalesce(record->>'request_type','unreported') request_type,"+aggregate+" FROM recent r CROSS JOIN windows w WHERE r.created_at>=now()-w.minutes*interval '1 minute' GROUP BY w.minutes,request_type ORDER BY w.minutes,request_type)t",
        "length_windows": "SELECT coalesce(json_agg(t),'[]'::json) FROM (SELECT w.minutes,b.minimum_input_tokens,"+aggregate+" FROM recent r CROSS JOIN windows w CROSS JOIN (VALUES (1024),(4096))b(minimum_input_tokens) WHERE r.created_at>=now()-w.minutes*interval '1 minute' AND input_tokens>=minimum_input_tokens GROUP BY w.minutes,b.minimum_input_tokens ORDER BY 1,2)t",
        "cache_groups_20m": "SELECT coalesce(json_agg(t),'[]'::json) FROM (SELECT model,md5(account_ref) account_hash,left(coalesce(record->>'user_agent','unreported'),300) user_agent,coalesce(record->>'request_type','unreported') request_type,CASE WHEN input_tokens<1024 THEN '<1024' WHEN input_tokens<4096 THEN '1024-4095' ELSE '>=4096' END input_bucket,"+aggregate+" FROM recent WHERE created_at>=now()-interval '20 minutes' GROUP BY 1,2,3,4,5 ORDER BY sum(input_tokens) DESC)t",
        "errors": "SELECT coalesce(json_agg(t),'[]'::json) FROM (SELECT id,created_at,model,status,md5(account_ref) account_hash,record->>'request_type' request_type,record->>'http_code' http_code,record->>'error_code' error_code,record->>'error_source' error_source,record->>'error_phase' error_phase,record->>'final_upstream_error_code' final_upstream_error_code,record->>'final_upstream_error_source' final_upstream_error_source,record->'tool_policy' tool_policy,record->>'error_param' error_param,record->'structured_mixed' structured_mixed,record->'bound_selection' bound_selection,record->>'error' error,record->'attempts' attempts,record->'attempts_truncated' attempts_truncated,observed_diagnostic_truncated diagnostic_truncated FROM recent WHERE status<>'ok' ORDER BY id)t",
        "minute_series": "SELECT coalesce(json_agg(t),'[]'::json) FROM (SELECT date_trunc('minute',created_at) AS bucket,count(*) requests,count(*) FILTER(WHERE is_inference) inference_requests,count(*) FILTER(WHERE NOT is_inference) control_requests,coalesce(sum(input_tokens+output_tokens) FILTER(WHERE is_inference),0) tokens,coalesce(sum(input_tokens) FILTER(WHERE is_inference),0) input_tokens,coalesce(sum(cached_tokens) FILTER(WHERE is_inference),0) cached_tokens FROM recent WHERE created_at>=now()-interval '20 minutes' GROUP BY 1 ORDER BY 1)t",
    }
    started = runtime.get("process_start_utc")
    if started:
        # Parsed/reformatted UTC time comes only from systemd. Never interpolate
        # an unchecked caller string into the read-only SQL statement.
        since = parse_observed_timestamp(started).isoformat()
        deployment_where = "created_at >= '" + since + "'::timestamptz"
        sections["since_process_start"] = "SELECT row_to_json(t) FROM (SELECT " + aggregate + " FROM recent WHERE " + deployment_where + ")t"
        sections["since_process_start_types"] = "SELECT coalesce(json_agg(t),'[]'::json) FROM (SELECT coalesce(record->>'request_type','unreported') request_type," + aggregate + " FROM recent WHERE " + deployment_where + " GROUP BY 1)t"
    # recent never materializes the full potentially TOASTed record. All
    # consumers reuse narrow scalar columns within the same MVCC statement.
    projection = {
        "left(coalesce(record->>'user_agent','unreported'),300)": "observed_user_agent",
        "record->>'usage_known'": 'observed_usage_known',
        "record->>'request_type'": 'observed_request_type',
        "record->>'http_code'": 'observed_http_code',
        "record->>'error_code'": 'observed_error_code',
        "record->>'error_source'": 'observed_error_source',
        "record->>'error_phase'": 'observed_error_phase',
        "record->>'final_upstream_error_code'": 'observed_final_upstream_error_code',
        "record->>'final_upstream_error_source'": 'observed_final_upstream_error_source',
        "record->>'error_param'": 'observed_error_param',
        "record->>'error'": 'observed_error',
        "record->'tool_policy'": 'observed_tool_policy',
        "record->'structured_mixed'": 'observed_structured_mixed',
        "record->'bound_selection'": 'observed_bound_selection',
        "record->'attempts'": 'observed_attempts',
        "record->'attempts_truncated'": 'observed_attempts_truncated',
    }
    for name, query in sections.items():
        for expression, column in projection.items():
            query = query.replace(expression, column)
        sections[name] = query
    return cte + "SELECT json_build_object(" + ",".join("'"+name+"',("+query+")" for name, query in sections.items()) + ")"


def snapshot(after_id=0):
    runtime, settings = read_runtime()
    started = runtime.get("process_start_utc")
    report = sql(snapshot_query(runtime))
    report["runtime"] = runtime
    report["settings"] = {key: settings.get(key) for key in SAFE_SETTINGS}
    report["formula"] = "cache_percent=100*sum(observed cache-read tokens)/sum(input tokens on those same sampled inference requests); cache-write tokens are independent explicit upstream observations; missing values remain unknown; RPM/TPM are populated only when the trailing 60-second sample window is complete; request/error totals retain control traffic within the declared sample"
    report["inference_endpoint_scope"] = list(INFERENCE_ENDPOINTS)
    report["cache_target_percent"] = 70
    report["request_types_are_observed_not_inferred_from_current_mode"] = True
    report["inference_calls"] = 0
    report["database_access"] = "default_transaction_read_only=on"
    grouped = collections.Counter()
    errors_by_window = collections.Counter()
    errors_since_start = collections.Counter()
    new_errors = []
    captured = parse_observed_timestamp(report["captured_at"])
    report["ingress_errors"] = read_ingress(runtime, captured)
    for row in report.pop("errors"):
        row["category"] = "truncated_diagnostic" if row.get("diagnostic_truncated") else classify_error(row)
        row["classification_complete"] = not row.get("diagnostic_truncated", False)
        for field, observe in (("error_param", observed_error_parameter), ("structured_mixed", observed_structured_mixed), ("bound_selection", observed_bound_selection)):
            value = observe(row)
            if value is None:
                row.pop(field, None)
            else:
                row[field] = value
        policy = observed_tool_policy(row)
        if policy is None:
            row.pop("tool_policy", None)
        else:
            row["tool_policy"] = policy
        row["error"] = redact(row["error"])
        row.pop("attempts", None)
        grouped[(row["request_type"], row["category"], row["http_code"], row["error_source"], row["error_phase"], row["error"])] += 1
        created_at = parse_observed_timestamp(row["created_at"])
        age = (captured - created_at).total_seconds()
        for minutes in (1, 20, 60, 360):
            if age <= minutes * 60:
                errors_by_window[(minutes, row["request_type"], row["category"], row["http_code"], row["error"])] += 1
        if started and created_at >= parse_observed_timestamp(started):
            errors_since_start[(row["request_type"], row["category"], row["http_code"], row["error"])] += 1
        if int(row["id"]) > after_id:
            new_errors.append(row)
    report["error_groups_6h"] = [dict(zip(("request_type", "category", "http_code", "source", "phase", "error"), keys), count=count) for keys, count in grouped.most_common()]
    report["error_windows"] = [dict(zip(("minutes", "request_type", "category", "http_code", "error"), keys), count=count) for keys, count in sorted(errors_by_window.items(), key=lambda item: (item[0][0], tuple(str(x or "") for x in item[0][1:])))]
    report["errors_since_process_start"] = [dict(zip(("request_type", "category", "http_code", "error"), keys), count=count) for keys,count in errors_since_start.most_common()]
    sampling = report.get("sampling", {"truncated": False, "rows": 0, "row_limit": MAX_SAMPLE_ROWS})
    report["sampling"] = sampling
    excluded = parse_observed_timestamp(sampling["first_excluded_at"]) if sampling.get("first_excluded_at") else None
    for window in report["windows"]:
        window["complete"] = not sampling["truncated"] or excluded is not None and (captured-excluded).total_seconds() > window["minutes"]*60
        window["scope"] = "complete_time_window" if window["complete"] else "latest_record_sample_lower_bound"
    report["process_window_coverage"] = "at most latest 5000 records within 6 hours; process-start filter applied" if started else "process not running at snapshot"
    report["new_errors_complete"] = not sampling["truncated"]
    report["new_errors_scope"] = "new IDs within the bounded recent sample; gaps may exist when sampling is truncated"
    report["error_windows_complete"] = not sampling["truncated"] and not sampling.get("diagnostics_truncated", 0) and not sampling.get("dimension_values_truncated", 0)
    report["new_errors"] = new_errors if after_id else []
    report["first_observation_baseline"] = after_id == 0
    report["new_nonbusy_errors"] = sum(row["category"] != "upstream_busy" for row in report["new_errors"])
    report["new_busy_errors"] = sum(row["category"] == "upstream_busy" for row in report["new_errors"])
    minute = next(item for item in report["windows"] if item["minutes"] == 1)
    first = minute["totals"]
    tpm_known = minute["complete"] and first["successful_usage_unreported"] == 0
    reported_tpm = first["input_tokens"] + first["output_tokens"]
    report["realtime"] = {"rpm": first["inference_requests"] if minute["complete"] else None, "observed_inference_requests": first["inference_requests"], "window_complete": minute["complete"], "tpm": reported_tpm if tpm_known else None,
        "tpm_known": tpm_known, "reported_tpm": reported_tpm, "successful_usage_unreported": first["successful_usage_unreported"],
        "input_tpm": first["input_tokens"] if tpm_known else None, "output_tpm": first["output_tokens"] if tpm_known else None,
        "reported_cached_tpm": first["cached_tokens"], "reported_cache_read_tpm": first["cached_tokens"],
        "reported_cache_write_tpm": first["cache_write_tokens"],
        "cache_read_samples": first["cache_read_samples"], "cache_write_samples": first["cache_write_samples"],
        "cache_read_unknown_requests": first["cache_read_unknown_requests"], "control_requests": first["control_requests"],
        "request_basis": "persisted_inference_records", "token_basis": "reported_inference_usage"}
    return report


def persist(folder, report):
    folder.mkdir(parents=True, exist_ok=True, mode=0o700)
    if os.name == "posix":
        folder.chmod(0o700)
    report["new_nonbusy_categories"] = sorted({x["category"] for x in report["new_errors"] if x["category"] != "upstream_busy"})
    now = datetime.now(timezone.utc).strftime("%Y%m%d")
    journal = folder / (now + ".jsonl")
    compact = {key: report[key] for key in ("captured_at", "last_id", "settings", "windows", "mode_windows", "realtime", "new_errors", "new_nonbusy_errors", "new_busy_errors", "new_nonbusy_categories", "first_observation_baseline", "ingress_errors")}
    for key in ("sampling", "new_errors_complete", "new_errors_scope", "error_windows_complete"):
        if key in report: compact[key] = report[key]
    compact["runtime"] = {key: report["runtime"].get(key) for key in ("pid", "binary_sha256")}
    with journal.open("a", encoding="utf-8") as stream:
        stream.write(json.dumps(compact, ensure_ascii=False) + "\n")
    if os.name == "posix": journal.chmod(0o600)
    # Retention concerns only this observer's own YYYYMMDD.jsonl files.
    today = datetime.now(timezone.utc).date()
    for path in folder.glob("????????.jsonl"):
        if re.fullmatch(r"\d{8}\.jsonl", path.name) and path.is_file() and path.parent.resolve() == folder.resolve():
            try: day = datetime.strptime(path.stem, "%Y%m%d").date()
            except ValueError: continue
            if (today - day).days >= 7:
                path.unlink()
    # Commit the cursor only after journaling succeeds. A failed journal write
    # must not cause the next run to skip newly observed errors.
    with tempfile.NamedTemporaryFile(mode="w", encoding="utf-8", prefix="latest-", suffix=".tmp", dir=folder, delete=False) as stream:
        temporary = Path(stream.name)
        json.dump(report, stream, ensure_ascii=False, indent=2)
    try:
        if os.name == "posix": temporary.chmod(0o600)
        temporary.replace(folder / "latest.json")
    finally:
        temporary.unlink(missing_ok=True)


def persist_observer_status(folder, status, last_id, last_success_at=None, error=None):
    """Publish this attempt's freshness without replacing a good data snapshot."""
    value = {"status": status, "checked_at": datetime.now(timezone.utc).isoformat(),
             "last_success_captured_at": last_success_at, "last_id": last_id}
    if error is not None:
        value["error_category"] = error.category if isinstance(error, MonitorReadError) else "observation_failed"
        value["exception"] = type(error).__name__
    with tempfile.NamedTemporaryFile(mode="w", encoding="utf-8", prefix="status-", suffix=".tmp", dir=folder, delete=False) as stream:
        temporary = Path(stream.name)
        json.dump(value, stream, ensure_ascii=False, indent=2)
    try:
        if os.name == "posix": temporary.chmod(0o600)
        temporary.replace(folder / "status.json")
    finally:
        temporary.unlink(missing_ok=True)


def observe_once(folder):
    """Caller holds observer.lock; failures retain the last good cursor/data."""
    after, last_success = 0, None
    try:
        previous = folder / "latest.json"
        if previous.exists():
            saved = json.loads(previous.read_text(encoding="utf-8"))
            after, last_success = int(saved["last_id"]), saved.get("captured_at")
        report = snapshot(after)
        persist(folder, report)
    except Exception as error:
        persist_observer_status(folder, "failed", after, last_success, error)
        raise
    persist_observer_status(folder, "ok", report["last_id"], report["captured_at"])
    return report


def prepare_units(folder):
    folder.mkdir(parents=True, exist_ok=True)
    service = """[Unit]
Description=Read-only codex-agent error and cache observer
After=codex-agent.service docker.service

[Service]
Type=oneshot
User=root
UMask=0077
ExecStart=/usr/bin/python3 -B /opt/codex-agent-integration/live-monitor/codex_agent_live_monitor.py --server-once --state-dir /var/lib/codex-agent-observer
TimeoutStartSec=55
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ReadWritePaths=/var/lib/codex-agent-observer
StateDirectory=codex-agent-observer
StateDirectoryMode=0700
StandardOutput=journal
StandardError=journal
"""
    timer = """[Unit]
Description=Observe codex-agent every minute without remediation

[Timer]
OnBootSec=30s
OnUnitActiveSec=60s
AccuracySec=1s
Persistent=true
Unit=codex-agent-observer.service

[Install]
WantedBy=timers.target
"""
    (folder / "codex-agent-observer.service").write_text(service, encoding="utf-8")
    (folder / "codex-agent-observer.timer").write_text(timer, encoding="utf-8")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--remote", action="store_true")
    parser.add_argument("--server-once", action="store_true")
    parser.add_argument("--state-dir", type=Path, default=Path(REMOTE_STATE))
    parser.add_argument("--prepare-units", type=Path)
    args = parser.parse_args()
    if args.prepare_units:
        prepare_units(args.prepare_units)
        print(json.dumps({"prepared": str(args.prepare_units), "installed": False}))
        return
    if args.server_once:
        import fcntl
        args.state_dir.mkdir(parents=True, exist_ok=True, mode=0o700)
        with (args.state_dir / "observer.lock").open("a") as lock:
            # Timer and manual observers share one cursor and atomic snapshot.
            fcntl.flock(lock.fileno(), fcntl.LOCK_EX)
            report = observe_once(args.state_dir)
        print(json.dumps({"captured_at": report["captured_at"], "mode": report["settings"].get("inference_mode"), "last_id": report["last_id"], "new_busy_errors": report["new_busy_errors"], "new_nonbusy_errors": report["new_nonbusy_errors"], "realtime": report["realtime"]}))
        return
    if not args.remote: parser.error("choose --remote, --server-once or --prepare-units")
    from integration_env import ROOT, connect
    client = connect()
    try:
        _, out, err = client.exec_command("install -d -m 700 /opt/codex-agent-integration/live-monitor", timeout=20)
        if out.channel.recv_exit_status(): raise RuntimeError("monitor_private_directory_unavailable")
        with client.open_sftp() as sftp:
            staged = REMOTE_SCRIPT + "." + hashlib.sha256(Path(__file__).read_bytes()).hexdigest() + ".tmp"
            sftp.put(__file__, staged); sftp.chmod(staged, 0o600)
            sftp.posix_rename(staged, REMOTE_SCRIPT)
        _, out, err = client.exec_command("python3 -B " + shlex.quote(REMOTE_SCRIPT) + " --server-once", timeout=90)
        stdout = out.read().decode(); error = err.read(); code = out.channel.recv_exit_status()
        if code:
            diagnostic = {}
            for line in error.decode("utf-8", "replace").splitlines():
                try:
                    item = json.loads(line)
                    if isinstance(item, dict) and "monitor_error" in item: diagnostic = {"exception": item["monitor_error"]}
                except ValueError: pass
            print(json.dumps({"failed": True, "exit_code": code, "stderr_bytes": len(error), "diagnostic": diagnostic})); return 1
        stamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")
        local = ROOT / ".local/live-monitor" / stamp; local.mkdir(parents=True)
        with client.open_sftp() as sftp: sftp.get(REMOTE_STATE + "/latest.json", str(local / "snapshot.json"))
        print(stdout.strip())
        print(json.dumps({"snapshot": str(local / "snapshot.json"), "timer_installed": False}))
    finally: client.close()


if __name__ == "__main__":
    try: sys.exit(main())
    except Exception as error:
        print(json.dumps({"monitor_error": type(error).__name__, "message": "read-only observation failed; no remediation performed"}), file=sys.stderr)
        sys.exit(1)
