"""Start a frozen candidate against the retained local synthetic rollback fixture."""
from __future__ import annotations

import argparse
from datetime import datetime, timezone
import hashlib
import json
from pathlib import Path
import re
import secrets
import socket
import subprocess
import time
import urllib.error
import urllib.request


ROOT = Path(__file__).resolve().parents[2]
CANDIDATE = ROOT / ".local/source-20260918/gateway-overlay-20260918t023414-8e3565"
FRONTEND = ROOT / ".local/runtime-frontend/d87032e071e7c72343ccb178857d4e56a0200ea7af87aa3b262083e73d15d1c6"


def sha(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def docker(*arguments):
    result = subprocess.run(["docker", *arguments], capture_output=True, text=True, encoding="utf-8", errors="replace", timeout=90)
    if result.returncode:
        raise RuntimeError("Scoped Docker operation failed: " + arguments[0])
    return result.stdout.strip()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--environment-file", type=Path, required=True)
    args = parser.parse_args()
    fixture = json.loads(args.environment_file.read_text(encoding="utf-8"))
    if fixture.get("synthetic_only") is not True or fixture.get("all_accounts_disabled") is not True or fixture.get("old_process_stopped") is not True:
        raise RuntimeError("Requires retained synthetic fixture after old process stopped")
    resources = fixture["resources"]
    if not all(re.fullmatch(r"codex-m003-[0-9a-f]+-(?:net|pg|redis|old)", resources[name]) for name in ("network", "postgres", "redis", "old_app")):
        raise RuntimeError("Fixture resource scope differs")
    if docker("inspect", "--format", "{{.State.Running}}", resources["old_app"]) != "false":
        raise RuntimeError("Previous test binary still running")
    binary = CANDIDATE / "codex-agent-linux-amd64"
    expected = "a66c7be3de344221d6433a6bc7140523d95a6a068fd2312757c80f2dc50c073d"
    if sha(binary) != expected:
        raise RuntimeError("Candidate binary differs from frozen release")
    with socket.socket() as probe:
        probe.bind(("127.0.0.1", 28887))
    stamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%S") + "-" + secrets.token_hex(3)
    output = ROOT / ".local/legacy-candidate-browser" / stamp
    output.mkdir(parents=True, exist_ok=False)
    name = "codex-m003-browser-" + stamp.lower()
    image = docker("image", "inspect", "--format", "{{.Id}}", "alpine:3.21")
    key_file = Path(fixture["directory"]) / "legacy/client-key"
    if key_file.read_text(encoding="utf-8").strip() != fixture["ADMIN_KEY"]:
        raise RuntimeError("Synthetic admin key source differs")
    config = {"base_url": "http://127.0.0.1:28887", "key_file": str(key_file), "output": str(output / "browser"),
              "source_sha256": "956fc90be91a922ac1593b283102bc5149674dac337564e6023479ec8e60a6f9",
              "candidate_binary_sha256": expected, "frontend_evidence": str(FRONTEND / "frontend-evidence.json"),
              "frontend_dist": str(FRONTEND / "frontend/dist")}
    (output / "run-config.json").write_text(json.dumps(config, indent=2), encoding="utf-8")
    report = {"schema": "codex-agent-local-candidate-browser-runtime-v1", "status": "running", "source_sha256": config["source_sha256"],
              "candidate_binary_sha256": expected, "old_process_stopped_verified": True, "synthetic_only": True,
              "production_requests": 0, "fixture_schema": fixture["schema"], "resources": {"cpus": .5, "memory": "256m", "pids": 64},
              "container": name, "image_id": image, "frontend_evidence_sha256": sha(FRONTEND / "frontend-evidence.json"),
              "fixture_preserved": True}
    identifier = None
    try:
        identifier = docker("run", "-d", "--name", name, "--label", "codex-agent.integration=legacy-candidate-browser",
            "--network", resources["network"], "--read-only", "--memory", "256m", "--cpus", "0.5", "--pids-limit", "64",
            "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--user", "65534:65534", "--tmpfs", "/tmp:rw,nosuid,size=16m",
            "--env-file", fixture["container_env_file"], "--publish", "127.0.0.1:28887:28887",
            "--mount", "type=bind,source=" + str(binary) + ",target=/app/codex-agent,readonly",
            "--mount", "type=bind,source=" + str(FRONTEND / "frontend/dist") + ",target=/app/web,readonly",
            image, "/app/codex-agent", "--listen", "0.0.0.0:28887", "--web", "/app/web")
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
        deadline = time.monotonic() + 30
        while True:
            try:
                with opener.open(config["base_url"] + "/health/ready", timeout=2) as response:
                    if response.status == 200:
                        break
            except (OSError, urllib.error.URLError):
                pass
            if time.monotonic() >= deadline:
                raise RuntimeError("Candidate did not become ready on isolated fixture")
            time.sleep(.25)
        actual = docker("exec", identifier, "sha256sum", "/app/codex-agent").split()[0]
        if actual != expected:
            raise RuntimeError("Runtime executable SHA differs")
        report["runtime_binary_verified"] = True
        browser = subprocess.run(["node", str(Path(__file__).with_suffix(".cjs")), str(output / "run-config.json")], cwd=ROOT,
            capture_output=True, text=True, encoding="utf-8", errors="replace", timeout=240)
        (output / "browser.log").write_text(browser.stdout + browser.stderr, encoding="utf-8")
        browser_report = output / "browser/report.json"
        if browser_report.exists():
            result = json.loads(browser_report.read_text(encoding="utf-8"))
            report.update(status="pass" if browser.returncode == 0 and result["status"] == "pass" else "fail",
                          browser_report=str(browser_report.relative_to(ROOT)), browser_report_sha256=sha(browser_report),
                          checks=result["checks"], screenshots=result["screenshots"])
        else:
            raise RuntimeError("Browser runner produced no report")
    except Exception as error:
        report.update(status="fail", error=type(error).__name__ + ": " + str(error))
    finally:
        if identifier:
            try:
                docker("stop", "--time", "20", identifier)
                report["candidate_process_stopped"] = docker("inspect", "--format", "{{.State.Running}}", identifier) == "false"
                docker("rm", identifier)
                report["candidate_container_removed"] = True
            except Exception:
                report.update(status="fail", candidate_process_stopped=False)
        report["completed_at"] = datetime.now(timezone.utc).isoformat()
        (output / "report.json").write_text(json.dumps(report, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    print(json.dumps({"status": report["status"], "report": str(output / "report.json"), "checks": report.get("checks", []), "error": report.get("error")}, ensure_ascii=False, indent=2))
    return 0 if report["status"] == "pass" else 1


if __name__ == "__main__":
    raise SystemExit(main())
