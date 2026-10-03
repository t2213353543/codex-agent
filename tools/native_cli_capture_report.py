"""Combine sanitized local CLI captures while retaining the gateway baseline.

This script reads metadata emitted by TestSourceNativeCLITransport only. It does
not contact an endpoint, inspect a live process or read CLI credentials.
"""

import argparse
import copy
import json
from pathlib import Path


ORIGIN = "official_npm_release"
GATEWAY = "custom_ca_via_go_gateway_us_env"


def read_capture(path):
    report = json.loads(Path(path).read_text(encoding="utf-8-sig"))
    if not report.get("passed") or not report.get("controlled_upstream"):
        raise ValueError("capture must be a passing controlled-upstream run")
    if report.get("live_model_inference") is not False:
        raise ValueError("this report combiner accepts local fixture runs only")
    return report


def find_scenario(report, name):
    matches = [
        item for item in report["scenarios"]
        if item["client"]["origin"] == ORIGIN and item["name"] == name
    ]
    if len(matches) != 1:
        raise ValueError("expected exactly one matching official CLI scenario")
    return matches[0]


def combine(baseline, followup):
    before = find_scenario(baseline, GATEWAY)
    after = find_scenario(followup, GATEWAY)
    direct = find_scenario(baseline, "custom_ca_direct_us_env")
    if before["client"] != after["client"] or before["client"] != direct["client"]:
        raise ValueError("gateway comparison requires the same official binary")
    if len(after["requests"]) != 2 or len(direct["requests"]) != 2:
        raise ValueError("both runs must include the initial and tool-output request")
    headers_equal = all(
        one["whitelisted_application_headers"] == two["whitelisted_application_headers"]
        for one, two in zip(direct["requests"], after["requests"])
    )
    if not headers_equal:
        raise ValueError("followup gateway headers still differ from native CLI")
    for item in after["requests"]:
        headers = item["whitelisted_application_headers"]
        if "version" in headers or "accept-encoding" in headers:
            raise ValueError("followup unexpectedly synthesizes a transport header")

    report = copy.deepcopy(baseline)
    report["completed_at"] = followup["completed_at"]
    report["scenarios"] = [
        copy.deepcopy(after) if item["client"]["origin"] == ORIGIN and item["name"] == GATEWAY else item
        for item in report["scenarios"]
    ]
    comparison = report["comparisons"][ORIGIN + "_gateway"]
    comparison["same_whitelisted_application_headers"] = headers_equal
    comparison["same_whitelisted_application_headers_all_tool_rounds"] = headers_equal
    comparison["same_request_schema_keys"] = all(
        one["request_schema_keys"] == two["request_schema_keys"]
        for one, two in zip(direct["requests"], after["requests"])
    )
    comparison["same_user_agent"] = (
        direct["requests"][0]["whitelisted_application_headers"]["user-agent"]
        == after["requests"][0]["whitelisted_application_headers"]["user-agent"]
    )
    report["gateway_header_correction"] = {
        "baseline_completed_at": baseline["completed_at"],
        "followup_completed_at": followup["completed_at"],
        "before_gateway_scenario": before,
        "before_comparison": baseline["comparisons"][ORIGIN + "_gateway"],
        "after_scenario_name": GATEWAY,
        "removed_synthetic_headers": ["accept-encoding", "version"],
        "after_matches_native_headers_in_both_tool_rounds": headers_equal,
        "conclusion_scope": "Application header whitelist equality only; Go TLS remains different from official native and rustls transports.",
    }
    report["verification"] = {
        "baseline_passed_scenarios": len(baseline["scenarios"]),
        "followup_passed_scenarios": len(followup["scenarios"]),
        "application_requests_observed_total": sum(len(item["requests"]) for item in baseline["scenarios"] + followup["scenarios"]),
        "baseline_go_test_command": "go test -run '^TestSourceNativeCLITransport$' -json -count=1 -timeout 6m .",
        "followup_go_test_command": "go test -run '^TestSourceNativeCLITransport$/official_npm_release/custom_ca_via_go_gateway_us_env$' -json -count=1 -timeout 2m .",
        "baseline_raw_test_log": ".local/native-transport-capture.jsonl",
        "followup_raw_test_log": ".local/native-transport-after-header-fix.jsonl",
        "baseline_sanitized_capture": ".local/native-transport-before-header-fix.json",
        "followup_sanitized_capture": ".local/native-transport-after-header-fix.json",
        "default_native_client_hello_complete": all(
            item["client_hellos"] and all(not hello.get("parse_error") for hello in item["client_hellos"])
            for item in baseline["scenarios"] if item["name"] == "default_trust_client_hello"
        ),
        "client_hello_completeness_basis": "The listener buffers all TLS records until the complete 4-byte handshake header plus its declared body length is available, then parses all vectors and rejects truncation. Certificate rejection or deliberate probe cancellation occurs after this complete ClientHello has been received.",
        "default_native_client_hello_bytes": [
            {"origin": item["client"]["origin"], "lengths": [hello["client_hello_bytes"] for hello in item["client_hellos"]]}
            for item in baseline["scenarios"] if item["name"] == "default_trust_client_hello"
        ],
        "default_native_probe_exit_code_explanation": "Exit 1 is expected after intentional cancellation following ClientHello; it is not a successful native TLS handshake or a model execution result.",
    }
    report["source_findings"].extend([
        "0.154.0 login/src/auth/default_client.rs:335-348 adds originator, User-Agent and an optional configured residency requirement; it does not synthesize a separate version header or a device-country identity.",
        "Gateway proxy.go:28-32 now disables automatic Go response-gzip negotiation; proxy.go:135-152 preserves an absent version header for native CLI identities while retaining explicitly supplied versions.",
        "The baseline observed gateway-only version=0.154.0 and accept-encoding=gzip. Recompiled followup observes neither header, matching both native CLI application request rounds.",
    ])
    return report


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--baseline", required=True)
    parser.add_argument("--followup", required=True)
    parser.add_argument("--output", required=True)
    args = parser.parse_args()
    report = combine(read_capture(args.baseline), read_capture(args.followup))
    Path(args.output).write_text(json.dumps(report, indent=2, ensure_ascii=False) + "\n", encoding="utf-8")
    print(json.dumps({
        "passed": report["passed"],
        "baseline_scenarios": report["verification"]["baseline_passed_scenarios"],
        "followup_scenarios": report["verification"]["followup_passed_scenarios"],
        "gateway_headers_match_native": report["gateway_header_correction"]["after_matches_native_headers_in_both_tool_rounds"],
    }))


if __name__ == "__main__":
    main()
