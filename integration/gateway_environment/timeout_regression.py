"""Reproduce only the custom direct compact deadline defect on a frozen candidate."""
from __future__ import annotations

import argparse
from datetime import datetime, timezone
import difflib
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys


ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "tools"))
from project_layout import server_source, server_package
DEFAULT_CANDIDATE = ROOT / ".local/management-parity/linux-race-20260917T214152-2d29dc"
DEFAULT_SOURCE = ROOT / ".local/frozen-gateway-race/20260918T022116-6f87b7/source"
REQUIRED_TESTS = ["TestGatewayTimeoutPoolDefaultsAndCustomRoundTripper", "TestGatewayTimeoutPoolCompactTotalTimeoutLeavesHTTPBodyAlive"]
DEFECT_ASSERTION = "custom compact deadline was lost or its transport was replaced"


def digest(raw):
    return hashlib.sha256(raw).hexdigest()


def run_variant(source, output, label, replacements):
    overlay = output / (label + "-overlay.json")
    overlay.write_text(json.dumps({"Replace": replacements}, indent=2), encoding="utf-8")
    environment = os.environ.copy()
    for key in list(environment):
        if key.startswith("TEST_") or key in {"DATABASE_URL", "REDIS_URL"}:
            environment.pop(key, None)
    environment.update(GOPROXY="off", GOSUMDB="off", GOWORK="off", CGO_ENABLED="0",
                       HTTP_PROXY="", HTTPS_PROXY="", ALL_PROXY="", NO_PROXY="*")
    command = ["go", "test", "-mod=vendor", f"-overlay={overlay}", "-json", "-count=1", "-timeout=30s", "-run", "^(" + "|".join(REQUIRED_TESTS) + ")$", server_package(source)]
    completed = subprocess.run(command, cwd=source, env=environment, capture_output=True, text=True, encoding="utf-8", errors="replace", timeout=90)
    raw = completed.stdout + completed.stderr
    log = output / (label + ".jsonl")
    log.write_text(raw, encoding="utf-8")
    tests, started, chunks = {}, [], []
    for line in raw.splitlines():
        try:
            event = json.loads(line)
        except ValueError:
            continue
        if not isinstance(event, dict):
            continue
        if event.get("Test") and event.get("Action") == "run":
            started.append(event["Test"])
        if event.get("Test") and event.get("Action") in ("pass", "fail", "skip"):
            tests[event["Test"]] = event["Action"]
        chunks.append(event.get("Output", ""))
    text = "".join(chunks)
    defect = (completed.returncode != 0
              and tests.get(REQUIRED_TESTS[0]) == "fail"
              and tests.get(REQUIRED_TESTS[1]) == "pass"
              and DEFECT_ASSERTION in text
              and set(REQUIRED_TESTS) <= set(started)
              and "[build failed]" not in text
              and not any(result == "skip" for result in tests.values()))
    return {"exit_code": completed.returncode, "tests": tests, "started_tests": started,
            "known_defect": defect, "assertion_failure_observed": DEFECT_ASSERTION in text,
            "log_path": log.name, "log_sha256": digest(log.read_bytes()),
            "overlay_path": overlay.name, "overlay_sha256": digest(overlay.read_bytes()), "command": command}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--candidate", type=Path, default=DEFAULT_CANDIDATE)
    parser.add_argument("--source", type=Path, default=DEFAULT_SOURCE, help="Existing offline-vendored copy of the exact candidate; this tool never changes it")
    args = parser.parse_args()
    candidate, source = args.candidate.resolve(), args.source.resolve()
    manifest = json.loads((candidate / "source-manifest.json").read_text(encoding="utf-8"))
    expected_source = digest(json.dumps(manifest["files"], sort_keys=True, separators=(",", ":")).encode())
    if expected_source != manifest["sha256"]:
        raise RuntimeError("Invalid candidate manifest identity")
    for name, expected in manifest["files"].items():
        if digest((source / name).read_bytes()) != expected:
            raise RuntimeError("Frozen source mismatch: " + name)
    if not (source / "vendor/modules.txt").is_file():
        raise RuntimeError("An existing offline-vendored candidate copy is required")
    impl_name, tests_name = "gateway_client_pool.go", "gateway_timeout_pool_test.go"
    baseline_impl = server_source(source, impl_name).read_bytes()
    owner_impl, owner_tests = server_source(ROOT, impl_name).read_bytes(), server_source(ROOT, tests_name).read_bytes()
    before_text, after_text = baseline_impl.decode().replace("\r\n", "\n"), owner_impl.decode().replace("\r\n", "\n")
    old = "\t\treturn newGatewayProxyClient(base, nil, mode)\n"
    new = "\t\tclient := newGatewayProxyClient(base, nil, mode)\n\t\tif mode == gatewayClientCompact && headerTimeout != nil {\n\t\t\tclient.Timeout = *headerTimeout\n\t\t}\n\t\treturn client\n"
    if before_text.count(old) != 1 or before_text.replace(old, new, 1) != after_text:
        raise RuntimeError("Owner implementation differs beyond the explicitly reviewed single compact-timeout fix")
    if DEFECT_ASSERTION not in owner_tests.decode() or not all("func " + name + "(" in owner_tests.decode() for name in REQUIRED_TESTS):
        raise RuntimeError("Owner regression test missing required minimal scenarios")
    stamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%S.%fZ")
    output = ROOT / ".local/compact-timeout-regression" / stamp
    output.mkdir(parents=True, exist_ok=False)
    frozen_impl = output / "owner-gateway_client_pool.go.txt"
    frozen_test = output / "owner-gateway_timeout_pool_test.go.txt"
    frozen_impl.write_bytes(owner_impl)
    frozen_test.write_bytes(owner_tests)
    delta = "".join(difflib.unified_diff(before_text.splitlines(True), after_text.splitlines(True), fromfile="candidate/gateway_client_pool.go", tofile="owner/gateway_client_pool.go"))
    (output / "reviewed-fix.diff").write_text(delta, encoding="utf-8")
    baseline = run_variant(source, output, "baseline", {str(server_source(source, tests_name)): str(frozen_test)})
    repaired = run_variant(source, output, "repaired", {str(server_source(source, tests_name)): str(frozen_test), str(server_source(source, impl_name)): str(frozen_impl)})
    source_unchanged = all(digest((source / name).read_bytes()) == expected for name, expected in manifest["files"].items())
    owner_unchanged = (ROOT / impl_name).read_bytes() == owner_impl and (ROOT / tests_name).read_bytes() == owner_tests
    repaired_pass = repaired["exit_code"] == 0 and all(repaired["tests"].get(name) == "pass" for name in REQUIRED_TESTS) and not any(value != "pass" for value in repaired["tests"].values())
    report = {"schema": "codex-agent-compact-timeout-regression-v1",
        "status": "reproduced_and_fixed_in_overlay" if baseline["known_defect"] and repaired_pass and source_unchanged and owner_unchanged else "validation_failed",
        "frozen_source_sha256": manifest["sha256"], "candidate_binary_sha256": digest((candidate / "codex-agent-linux-amd64").read_bytes()),
        "required_tests": REQUIRED_TESTS, "baseline": baseline, "repaired_overlay": repaired,
        "source_impl_before": digest(baseline_impl), "source_impl_after": digest(owner_impl),
        "overlay_fix_files": {impl_name: {"path": frozen_impl.name, "sha256": digest(owner_impl)}},
        "owner_test_file_sha256": digest(owner_tests), "frozen_source_unchanged": source_unchanged,
        "owner_source_unchanged_during_capture": owner_unchanged, "synthetic_only": True, "external_requests": 0,
        "scope": "Only custom direct non-http.Transport compact explicit total timeout; owner single-file implementation overlay on otherwise unchanged frozen candidate",
        "candidate_known_defect": baseline["known_defect"], "candidate_binary_repaired": False,
        "joint_release_approved": False, "race_executed": False, "postgres_redis_executed": False,
        "impact": "Custom direct RoundTripper compact client ignores explicit timeout and retains 20-minute default; original base client is not mutated. Native http.Transport paths have separate regression coverage and passed.",
        "evidence": {path.name: digest(path.read_bytes()) for path in [frozen_impl, frozen_test, output / "reviewed-fix.diff", output / "baseline.jsonl", output / "repaired.jsonl"]},
        "completed_at": datetime.now(timezone.utc).isoformat(), "runner_sha256": digest(Path(__file__).read_bytes())}
    (output / "report.json").write_text(json.dumps(report, indent=2) + "\n", encoding="utf-8")
    print(json.dumps({"status": report["status"], "candidate_known_defect": report["candidate_known_defect"], "baseline_tests": baseline["tests"],
        "repaired_overlay_tests": repaired["tests"], "report": str(output / "report.json")}, indent=2))
    return 0 if report["status"] == "reproduced_and_fixed_in_overlay" else 1


if __name__ == "__main__":
    raise SystemExit(main())
