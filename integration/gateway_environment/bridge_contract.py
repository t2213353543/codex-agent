"""Frozen Go handlers + current Python bridge over real local HTTP, no PG."""
import argparse
import datetime as dt
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "tools"))
from project_layout import server_source_dir, server_package, package_overlay
DEFAULT = ROOT / ".local/management-parity/linux-race-20260917T214152-2d29dc"


def sha(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--snapshot", type=Path, default=DEFAULT)
    args = parser.parse_args()
    source = args.snapshot.resolve() / "local-source"
    manifest = json.loads((args.snapshot / "source-manifest.json").read_text())
    for name, digest in manifest["files"].items():
        if sha(source / name) != digest:
            raise RuntimeError("Frozen source mismatch: " + name)
    here = Path(__file__).resolve().parent
    overlay_test = here / "bridge_contract_test.go.txt"
    client = here / "bridge_contract_client.py"
    bridge = ROOT / "deploy/workbench_bridge.py"
    hashes = {"go_overlay": sha(overlay_test), "python_client": sha(client), "bridge": sha(bridge)}
    stamp = dt.datetime.now(dt.timezone.utc).strftime("%Y%m%dT%H%M%S.%fZ")
    output = ROOT / ".local/bridge-contract" / stamp
    output.mkdir(parents=True)
    actual_overlay = package_overlay(overlay_test, output / "bridge-package-test.go.txt", source)
    overlay = output / "overlay.json"
    overlay.write_text(json.dumps({"Replace": {str(server_source_dir(source) / "zz_bridge_real_http_contract_test.go"): str(actual_overlay)}}))
    env = os.environ.copy()
    for key in list(env):
        if key.startswith(("TEST_DATABASE", "TEST_REDIS", "DATABASE_URL", "REDIS_URL")):
            env.pop(key)
    env.update(BRIDGE_CONTRACT_PYTHON=sys.executable, BRIDGE_CONTRACT_CLIENT=str(client), BRIDGE_CONTRACT_BRIDGE=str(bridge), BRIDGE_CONTRACT_OUTPUT=str(output), GOPROXY="off", GOSUMDB="off", GOWORK="off", CGO_ENABLED="0")
    command = ["go", "test", "-mod=readonly", "-overlay=" + str(overlay), "-json", "-count=1", "-timeout=120s", "-run", "^TestBridgeRealManagementContract$", server_package(source)]
    process = subprocess.run(command, cwd=source, env=env, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True, encoding="utf-8", errors="replace", timeout=180)
    (output / "go-test.jsonl").write_text(process.stdout, encoding="utf-8")
    tests = {}
    for line in process.stdout.splitlines():
        try:
            event = json.loads(line)
        except ValueError:
            continue
        if event.get("Test") and event.get("Action") in ("pass", "fail", "skip"):
            tests[event["Test"]] = event["Action"]
    unchanged = hashes == {"go_overlay": sha(overlay_test), "python_client": sha(client), "bridge": sha(bridge)}
    report = {"schema": "codex-agent-python-go-bridge-contract-v1", "status": "pass" if process.returncode == 0 and unchanged and len(tests) == 7 and all(value == "pass" for value in tests.values()) else "fail", "timestamp_utc": stamp, "source_sha256": manifest["sha256"], "artifacts_sha256": hashes, "artifacts_unchanged": unchanged, "tests": tests, "command": command, "storage": "temporary file store", "transport": "actual Python urllib to actual Go httptest management HTTP", "source_api": "synthetic fake", "production_touched": False, "database_migration_executed": False, "gateway_or_refresh_requests": 0, "limits": ["Not PostgreSQL/Redis or production deployment evidence", "Go CGO disabled; this run does not claim race coverage", "reauthorize is explicit credential replacement, not OAuth refresh: omitted RT is intentionally removed; bridge must reject RT ownership and rely on version CAS for concurrent RT addition"], "contract_sources": ["frozen admin_accounts_management.go:540", "frozen admin_accounts_management_test.go:237", "docs/ACCOUNT-PARITY-MATRIX.md A08"], "prior_failed_expectation": "20260918T022735.204169Z incorrectly expected explicit replacement to inherit missing RT; historical result retained, test premise corrected after inspecting implementation contract and existing regression assertion"}
    (output / "report.json").write_text(json.dumps(report, indent=2) + "\n")
    print(json.dumps({"status": report["status"], "report": str(output / "report.json"), "tests": tests}, indent=2))
    if report["status"] != "pass":
        print(process.stdout[-18000:])
    return 0 if report["status"] == "pass" else 1


if __name__ == "__main__":
    raise SystemExit(main())
