"""Run isolated synthetic gateway acceptance using a virtual package-main test."""
from __future__ import annotations

import argparse
import datetime as dt
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
import time

sys.path.insert(0, str(Path(__file__).resolve().parents[2] / "tools"))
from project_layout import backend_source_paths, server_source_dir, server_package, package_overlay


def sha256(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--race", action="store_true", help="Enable Go race detector; compiler/platform failures are reported, never hidden.")
    parser.add_argument("--output", type=Path, help="Optional new output directory; existing reports are not overwritten.")
    parser.add_argument("--source-dir", type=Path, help="Run the overlay against an independently frozen Go source tree.")
    args = parser.parse_args()
    workspace = Path(__file__).resolve().parents[2]
    root = args.source_dir.resolve() if args.source_dir else workspace
    stamp = dt.datetime.now(dt.timezone.utc).strftime("%Y%m%dT%H%M%S.%fZ")
    output = args.output or workspace / ".local" / "gateway-environment" / stamp
    output = output.resolve()
    output.mkdir(parents=True, exist_ok=False)
    source = Path(__file__).with_name("environment_test.go.txt")
    source = package_overlay(source, output / "environment-package-test.go.txt", root)
    virtual = server_source_dir(root) / "zz_gateway_environment_overlay_test.go"
    if virtual.exists():
        raise RuntimeError(f"Refusing to replace an existing source file: {virtual}")
    overlay = output / "overlay.json"
    overlay.write_text(json.dumps({"Replace": {str(virtual): str(source)}}, indent=2), encoding="utf-8")
    watched = backend_source_paths(root)
    watched += [root / "go.mod", root / "go.sum", source, Path(__file__).resolve()]
    before = {str(path.resolve()): sha256(path) for path in watched}
    environment = os.environ.copy()
    # No test database, Redis, official CLI, or live-account opt-in is used.
    environment["HTTP_PROXY"] = environment["HTTPS_PROXY"] = environment["ALL_PROXY"] = ""
    environment["NO_PROXY"] = "*"
    command = ["go", "test", f"-overlay={overlay}", "-json", "-count=1", "-timeout=90s"]
    if args.race:
        command.append("-race")
    command += ["-run", "^TestGatewayEnvironment", server_package(root)]
    started = time.monotonic()
    process = subprocess.run(command, cwd=root, env=environment, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True, encoding="utf-8", errors="replace", timeout=180)
    (output / "go-test.jsonl").write_text(process.stdout, encoding="utf-8")
    tests, metrics, errors = {}, [], []
    for line in process.stdout.splitlines():
        try:
            event = json.loads(line)
        except json.JSONDecodeError:
            if line.strip():
                errors.append(line)
            continue
        if event.get("Test") and event.get("Action") in {"pass", "fail", "skip"}:
            tests[event["Test"]] = event["Action"]
        message = event.get("Output", "")
        marker = "GATEWAY_ENVIRONMENT_METRIC "
        if marker in message:
            metrics.append(json.loads(message.split(marker, 1)[1]))
    changed = [name for name, digest in before.items() if sha256(Path(name)) != digest]
    report = {
        "schema_version": 1,
        "timestamp_utc": stamp,
        "status": "pass" if process.returncode == 0 and len(tests) == 5 and all(value == "pass" for value in tests.values()) and not changed else "fail",
        "synthetic_only": True,
        "production_requests": 0,
        "race_requested": args.race,
        "race_executed": args.race and process.returncode == 0,
        "command": command,
        "exit_code": process.returncode,
        "duration_seconds": round(time.monotonic() - started, 3),
        "tests": tests,
        "metrics": metrics,
        "source_sha256": before,
        "source_directory": str(root),
        "source_changed_during_run": changed,
        "toolchain": subprocess.check_output(["go", "version"], cwd=root, text=True).strip(),
        "go_environment": json.loads(subprocess.check_output(["go", "env", "-json", "GOOS", "GOARCH", "CGO_ENABLED", "CC"], cwd=root, text=True)),
        "non_json_output": errors,
        "limitations": [
            "Local HTTP/1.1 controlled transport test; no external upstream, TLS fingerprint, HTTP/2, SOCKS5, WebSocket, or live-model inference.",
            "Eight/four connection caps are explicit harness configuration, not a claim about product defaults; defaults are recorded separately.",
            "Account-route isolation exercises account proxy selection, shared client cache, real local sockets and synthetic headers; it does not exercise multi-tenant API-key authorization.",
            "The latency distribution is a local regression observation, not a production capacity estimate."
        ],
    }
    (output / "report.json").write_text(json.dumps(report, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    print(json.dumps({"status": report["status"], "tests": tests, "report": str(output / "report.json"), "source_changed_during_run": changed}, ensure_ascii=False, indent=2))
    if process.returncode:
        print(process.stdout[-16000:])
    return 0 if report["status"] == "pass" else 1


if __name__ == "__main__":
    sys.exit(main())
