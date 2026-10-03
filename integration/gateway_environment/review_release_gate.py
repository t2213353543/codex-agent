"""Read-only gate review probes; generated counterexample reports stay in .local."""
from copy import deepcopy
from datetime import datetime, timezone
import importlib.util
import json
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location("reviewed_release_gate", ROOT / "tools/gateway_release_check.py")
audit_module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(audit_module)


def main():
    output = ROOT / ".local/release-gate-review" / datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%S.%fZ")
    output.mkdir(parents=True)
    candidate_path = ROOT / ".local/joint-candidate/20260918T024841.878837Z/joint-candidate.json"
    candidate = json.loads(candidate_path.read_text())
    findings = []

    altered_candidate = deepcopy(candidate)
    altered_candidate["linux_binary"] = ".local/management-parity/linux-race-20260917T214152-2d29dc/codex-agent-linux-amd64"
    altered_candidate["linux_binary_sha256"] = "6ea3efc086d510399fe149def6b3e56941462f5531f8875967a7ed0c43e28918"
    mixed = output / "mixed-source-binary-candidate.json"
    mixed.write_text(json.dumps(altered_candidate), encoding="utf-8")
    checker = audit_module.Audit(ROOT)
    try:
        checker.source(mixed, source_dir=ROOT / candidate["source_directory"])
        checker.owner_artifacts()
        findings.append({"case": "source_binary_mix", "accepted": True, "detail": "956fc90 source with real older 5a818 binary passes source and owner-artifact integrity gates; build provenance is not compared"})
    except Exception as error:
        findings.append({"case": "source_binary_mix", "accepted": False, "error_type": type(error).__name__})

    checker = audit_module.Audit(ROOT)
    checker.source(candidate_path)
    checker.preserved_frontend(audit_module.DEFAULT_FRONTEND)
    browser_path = ROOT / ".local/legacy-candidate-browser/20260918T025248-5f0e36/browser/report.json"
    wrapper_path = browser_path.parent.parent / "report.json"
    browser = json.loads(browser_path.read_text())
    wrapper = json.loads(wrapper_path.read_text())
    browser["errors"].append("synthetic uncaught page error")
    browser["api_responses"].append({"path": "/api/overview", "status": 500})
    browser["checks"].append({"id": "postcheck", "status": "fail", "detail": "synthetic additional failure"})
    for shot in browser["screenshots"]:
        shot["path"] = str(browser_path.parent / shot["path"])
    mutated_browser = output / "contradictory-browser.json"
    raw = json.dumps(browser).encode()
    mutated_browser.write_bytes(raw)
    wrapper["browser_report"] = str(mutated_browser)
    wrapper["browser_report_sha256"] = audit_module.digest(raw)
    mutated_wrapper = output / "contradictory-browser-runtime.json"
    mutated_wrapper.write_text(json.dumps(wrapper), encoding="utf-8")
    try:
        result = checker.candidate_browser(mutated_wrapper)
        findings.append({"case": "contradictory_browser", "accepted": result["status"] == "pass", "detail": "Hash-consistent report with known required checks PASS but extra failed check, JS error and HTTP500 still passes browser gate"})
    except Exception as error:
        findings.append({"case": "contradictory_browser", "accepted": False, "error_type": type(error).__name__})

    race_path = ROOT / candidate["evidence_roles"]["gateway_race"]["path"]
    race = json.loads(race_path.read_text())
    names = ["TestGatewayTimeoutPoolDefaultsAndCustomRoundTripper", "TestGatewayTimeoutPoolCompactTotalTimeoutLeavesHTTPBodyAlive"]
    raw = "go version go1.26 linux/amd64\nlinux\namd64\n1\ngcc\n"
    raw += "".join(json.dumps({"Action": "pass", "Test": name, "Package": "codex-agent"}) + "\n" for name in names)
    raw += json.dumps({"Action": "pass", "Package": "codex-agent"}) + "\n"
    short_log = output / "race.log"
    short_log.write_text(raw, encoding="utf-8")
    race.update(selected_top_level_tests=names, passed_top_level_tests=2, passed_test_events=2)
    for artifact in race["artifacts"]:
        if Path(artifact["path"].replace("\\", "/")).name == "race.log":
            artifact.update(path=str(short_log), sha256=audit_module.digest(short_log.read_bytes()))
    short_report = output / "underscoped-gateway-race.json"
    short_report.write_text(json.dumps(race), encoding="utf-8")
    try:
        result = checker.gateway(str(short_report))
        timeout = checker.compact_timeout(audit_module.DEFAULT_COMPACT_REGRESSION)
        findings.append({"case": "underscoped_gateway_role", "accepted": result["status"] == timeout["status"] == "pass", "detail": "Explicit gateway override containing only two timeout tests passes gateway+timeout gates; required 159-test role coverage is not checked"})
    except Exception as error:
        findings.append({"case": "underscoped_gateway_role", "accepted": False, "error_type": type(error).__name__})
    report = {"reviewed_file_sha256": audit_module.digest((ROOT / "tools/gateway_release_check.py").read_bytes()),
              "synthetic_counterexamples_only": True, "production_touched": False, "findings": findings}
    (output / "report.json").write_text(json.dumps(report, indent=2) + "\n", encoding="utf-8")
    print(json.dumps({"report": str(output / "report.json"), **report}, indent=2))


if __name__ == "__main__":
    main()
