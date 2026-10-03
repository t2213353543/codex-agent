"""Compose one proven package-timeout race run with exact-source missing tests.

The original failure is retained verbatim. No individual failure, race warning,
new skip, source drift or unexecuted test can be waived by this verifier.
"""
from __future__ import annotations

import argparse
import hashlib
import json
from pathlib import Path
import re

SERVER = "codex-agent/internal/server"
BPS = "codex-agent/internal/bps"
REDIS = {
    "TestBPSRedisHistoryCorruptionExpiryAndMissingFailClosed",
    "TestBPSRedisHistoryIndexAndEntryLimitRemainBounded",
    "TestBPSRedisHistoryPersistsAcrossFreshProxyAndClient",
    "TestBPSRedisHistoryPrunesExpiredAndOverBudgetIndexEntries",
}
EXTRA_REQUIRED = {
    "TestBPSSequentialCutoffHTTPJSONSSEAndStructured",
    "TestBPSSequentialCutoffWSWarmupHistoryAndCompact",
    "TestBPSDispatchedWebSocketCompactionObservation",
}
OPTIONAL_SKIPS = {
    "TestBPSGatewayE2EPerformance", "TestBPSGatewayOfficialCLIStatuslessWSErrorTerminates",
    "TestBPSPerformanceClockResolution", "TestBPSLiveAccountPolicyBoundary",
    "TestBPSLiveCodexIntegration", "TestBPSLiveCodexWebSocket",
    "TestBPSLiveCompatibleModelRoutes", "TestBPSLiveGeneratedDefaultCache",
    "TestBPSLiveInlineImageAttachment", "TestBPSLiveRejectedAccountFailover",
    "TestBPSLiveSequentialSummaryCLI", "TestBPSLiveSharedAPIKeyCache",
    "TestBPSLiveStablePrefixCache", "TestBPSLiveStructuredCLI",
    "TestBPSLiveStructuredOutput", "TestBPSLiveToolInlineImageAttachment",
}
COLLABORATIVE = {
    "TestCollabSessionOwnerLocalAttemptClassification",
    "TestCollabSessionOwnerLocalAttemptReplacementKeepsOnlyRealGeneration",
    "TestCollabStructuredIncompleteTransportNotValidationFailure",
    "TestCollabStructuredWriteErrorNotValidationFailure",
    "TestGatewayFailoverAbortRecordHandlesGoexitAndMissingRecord",
    "TestGatewayFailoverAbortRecordKeepsOriginalPanicWhenAccountingPanics",
    "TestGatewayFailoverAbortRecordPreservesFailureAndPanic",
    "TestGatewayFailoverAbortRecordRetainsEarlierAttemptsAndTTFT",
    "TestRateLimitCooldownDefaultsAndAdminPersistence",
    "TestRateLimitCooldownDisabledAvoidsRedisAndSettingsFailureIs503",
    "TestRateLimitCooldownDisabledBypassesExistingAndPreservesTTL",
    "TestRateLimitCooldownDisabledPreservesReal429AndSSEFailures",
    "TestRateLimitCooldownDisabledStillFailsOverCurrentQuotaRequest",
    "TestRateLimitCooldownRedisDisableDoesNotDeleteSharedDeadline",
    "TestTeamCRateLimitAliasChecksResolvedCooldownBeforeDispatch",
    "TestTeamCRateLimitAliasLocalSelectionAttemptAccounting",
    "TestTeamCRateLimitAllAccountsCooldownNextRequest",
    "TestTeamCRateLimitCooldownUsesAcceptedSSE",
    "TestTeamCRateLimitCreatedDoesNotBlockFailover",
    "TestTeamCRateLimitResolvedCooldownFailureBoundary",
}


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def read(path):
    return json.loads(Path(path).read_text(encoding="utf-8-sig"))


def sha(path):
    return hashlib.sha256(Path(path).read_bytes()).hexdigest()


def map_sha(mapping):
    return hashlib.sha256(json.dumps(mapping, sort_keys=True, separators=(",", ":")).encode()).hexdigest()


def parse_log(raw):
    events = []
    for line in raw.splitlines():
        try:
            event = json.loads(line)
        except ValueError:
            continue  # Go/compiler inventory precedes the JSON event stream.
        if isinstance(event, dict) and "Action" in event:
            events.append(event)
    require(events, "Race raw JSON events missing")
    output = "".join(str(event.get("Output", "")) for event in events)
    require("WARNING: DATA RACE" not in raw and "DATA RACE" not in output,
            "Race detector reported a race")
    require(not re.search(r"(?m)^\s*--- FAIL:", output), "Individual race test failed")
    require(not any(e.get("Action") == "fail" and e.get("Test") for e in events),
            "Individual fail event cannot be recovered by composition")
    terminal = {}
    packages = {}
    for event in events:
        if event["Action"] not in {"pass", "skip", "fail"}:
            continue
        package, name = event.get("Package"), event.get("Test")
        if name and "/" not in name:
            key = package + "/" + name
            require(key not in terminal, "Duplicate top-level terminal test outcome")
            terminal[key] = event["Action"]
        elif not name:
            require(package not in packages, "Duplicate package terminal outcome")
            packages[package] = event["Action"]
    return {"events": events, "output": output, "tests": terminal, "packages": packages}


def verify_log_contract(report, parsed, original):
    require(report.get("synthetic_only") is True and report.get("production_requests") == 0,
            "Race scope is not synthetic-only")
    require(report.get("race_warning") is False and report.get("oom_killed") is False,
            "Race/OOM state is not clean")
    require(report.get("source_changed_after_freeze") is False and not report.get("changed_local_files"),
            "Frozen race source changed")
    require(report.get("package_results") == parsed["packages"], "Report/raw package outcomes disagree")
    scopes = report.get("scope") or []
    require(scopes and all("-race" in command and "-count=1" in command for command in scopes),
            "Executed scope does not identify noncached race tests")
    panic_lines = re.findall(r"(?m)^panic:.*$", parsed["output"])
    if original:
        require(report.get("status") == "fail" and report.get("exit_code") == 1,
                "Original failed exit/status must remain unchanged")
        require(parsed["packages"] == {BPS: "pass", SERVER: "fail"}, "Original package outcomes differ")
        require(panic_lines == ["panic: test timed out after 5m0s"],
                "Original failure is not exactly the verified 300-second package timeout")
        package_events = [e for e in parsed["events"] if e.get("Action") == "fail"]
        require(len(package_events) == 1 and not package_events[0].get("Test") and
                package_events[0].get("Package") == SERVER and
                299 <= package_events[0].get("Elapsed", 0) < 330,
                "Original raw failure is not a 300-second package deadline")
        require(any("-timeout=300s" in command and "./internal/server" in command for command in scopes),
                "Original declared package timeout differs")
    else:
        require(report.get("status") == "pass" and report.get("exit_code") == 0,
                "Supplement did not finish successfully")
        require(not panic_lines and all(v == "pass" for v in parsed["packages"].values()) and
                parsed["packages"].get(SERVER) == "pass", "Supplement package completion missing")
        require(not any(e.get("Action") in {"fail", "skip"} for e in parsed["events"]),
                "Supplement failure/skip cannot cover a missing selected test")


def expected_from_source(source):
    names = {}
    for package, directory in ((SERVER, "internal/server"), (BPS, "internal/bps")):
        found = set()
        for path in (source / directory).glob("*_test.go"):
            found.update(re.findall(r"^func (Test\w+)\(t \*testing.T\)", path.read_text(encoding="utf-8"), re.M))
        names[package] = found
    official = {n for n in names[SERVER] if n.startswith(("TestOfficial", "TestInferenceMode", "TestDashboardRealtime"))
                and "Live" not in n and "Fixture" not in n and not (n.startswith("TestOfficialUsageRepair") and "PG" in n)}
    require(COLLABORATIVE <= names[SERVER], "Frozen collaborative test registry incomplete")
    selected = {n for n in names[SERVER] if n.startswith("TestBPS")} | official | COLLABORATIVE
    require(REDIS | EXTRA_REQUIRED <= selected, "Required Redis/summary/WS-compaction scope missing")
    return selected, official, names[BPS]


def compose_outcomes(original, supplements, selected):
    outcomes = {key.removeprefix(SERVER + "/"): action for key, action in original.items()
                if key.startswith(SERVER + "/")}
    require(set(outcomes) <= selected, "Original executed unexpected top-level test")
    skips = {name for name, action in outcomes.items() if action == "skip"}
    require(skips <= OPTIONAL_SKIPS, "Unexpected original skip is not an approved optional fixture")
    missing = selected - set(outcomes)
    allowed_recovery = missing | REDIS
    recovered = set()
    for supplement in supplements:
        rows = {key.removeprefix(SERVER + "/"): action for key, action in supplement.items()
                if key.startswith(SERVER + "/")}
        require(set(rows) <= allowed_recovery, "Supplement reran non-Redis completed or optional tests")
        require(all(action == "pass" for action in rows.values()), "Supplement contains a non-pass selected test")
        require(not ((set(rows) - REDIS) & recovered), "Non-Redis missing test executed in multiple supplements")
        recovered.update(rows)
    require(missing <= recovered, "Selected tests remain without an actual race pass: " + ",".join(sorted(missing-recovered)))
    for name in recovered:
        require(name not in skips, "Original skipped test cannot be upgraded by composition")
        outcomes[name] = "pass"
    require(set(outcomes) == selected, "Composed selected test registry incomplete")
    return outcomes, sorted(missing), sorted(skips), sorted(recovered)


def verify(original_folder, recovery_folders, source_files):
    original_folder = Path(original_folder).resolve()
    recovery_folders = [Path(p).resolve() for p in recovery_folders]
    require(recovery_folders and len(set(recovery_folders)) == len(recovery_folders) and original_folder not in recovery_folders,
            "One or more distinct supplementary run directories required")
    manifest = read(original_folder / "source-manifest.json")
    require(manifest.get("source_files") == source_files and len(source_files) == 710,
            "Exact reviewed 710-file backend source differs")
    digest = map_sha(source_files)
    require(manifest.get("source_sha256") == digest, "Original source digest differs")
    for name, expected in source_files.items():
        require(sha(original_folder / "source" / name) == expected, "Original frozen source content differs: " + name)
    selected, official, bps_selected = expected_from_source(original_folder / "source")
    results, parsed_runs, evidence = [], [], []
    for index, folder in enumerate([original_folder, *recovery_folders]):
        report = read(folder / "report.json")
        tested = read(folder / "source-manifest.json")
        require(tested.get("source_files") == source_files and tested.get("source_sha256") == digest and
                report.get("source_sha256") == digest, "Supplement source/runtime/test files differ")
        for name, expected in source_files.items():
            require(sha(folder / "source" / name) == expected, "Run's frozen source content differs: " + name)
        require(tested.get("archive_sha256") == report.get("archive_sha256") == sha(folder / "source.tar.gz"),
                "Uploaded race archive digest differs")
        command = report.get("command") or []
        require(command and isinstance(command[-1], str) and
                all(scope in command[-1] for scope in report.get("scope", [])) and
                "-race=false" not in command[-1], "Declared race command differs from executed container script")
        parsed = parse_log((folder / "go-test.jsonl").read_text(encoding="utf-8"))
        verify_log_contract(report, parsed, index == 0)
        if index == 0:
            require(set(report.get("official_expected_tests", [])) == official and
                    set(report.get("collaborative_expected_tests", [])) == COLLABORATIVE,
                    "Original declared official/collaborative coverage differs from source registry")
        else:
            require(report.get("image_id") == results[0].get("image_id"), "Supplement race compiler image differs")
        require(report.get("temporary_container_removed") is True and report.get("temporary_redis_environment_removed") is True,
                "Race integration cleanup did not complete")
        results.append(report)
        parsed_runs.append(parsed)
        evidence.extend(folder / name for name in ("report.json", "source-manifest.json", "go-test.jsonl"))
    for name in bps_selected:
        require(parsed_runs[0]["tests"].get(BPS + "/" + name) == "pass", "Original complete BPS race test missing: " + name)
    outcomes, missing, skips, recovered = compose_outcomes(parsed_runs[0]["tests"], [p["tests"] for p in parsed_runs[1:]], selected)
    for name in official | COLLABORATIVE | REDIS | EXTRA_REQUIRED:
        require(outcomes.get(name) == "pass", "Required race test not actually passed: " + name)
    summary = {
        "passed": True, "composition_kind": "same_source_package_timeout_recovery",
        "original_run_passed": False, "original_status": results[0]["status"], "original_exit_code": results[0]["exit_code"],
        "original_package_results": parsed_runs[0]["packages"], "original_failure": "go_test_server_package_300s_timeout",
        "source_sha256": digest, "source_file_count": len(source_files),
        "original_directory": str(original_folder), "recovery_directories": [str(p) for p in recovery_folders],
        "server_expected_count": len(selected), "server_pass_count": sum(v == "pass" for v in outcomes.values()),
        "server_skip_count": len(skips), "preserved_optional_skips": skips,
        "original_missing_tests": missing, "supplement_passed_tests": recovered,
        "bps_actual_pass_count": len(bps_selected), "official_test_results": {n: outcomes[n] for n in sorted(official)},
        "collaborative_test_results": {n: outcomes[n] for n in sorted(COLLABORATIVE)},
        "redis_test_results": {n: outcomes[n] for n in sorted(REDIS)},
        "extra_required_results": {n: outcomes[n] for n in sorted(EXTRA_REQUIRED)},
        "raw_evidence": [{"path": str(p), "sha256": sha(p)} for p in evidence],
        "original_failed_artifacts_unchanged": True,
    }
    return evidence, summary


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--original", type=Path, required=True)
    parser.add_argument("--recovery", type=Path, action="append", required=True)
    parser.add_argument("--current-manifest", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    current = read(args.current_manifest)
    mapping = current.get("source_files", current.get("files", current))
    _, summary = verify(args.original, args.recovery, mapping)
    args.output.write_text(json.dumps(summary, indent=2), encoding="utf-8")
    print(json.dumps({"passed": True, "output": str(args.output), "server_pass_count": summary["server_pass_count"], "preserved_skip_count": summary["server_skip_count"]}))


if __name__ == "__main__":
    main()
