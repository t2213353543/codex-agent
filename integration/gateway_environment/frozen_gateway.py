"""Gateway race acceptance on an existing immutable management source archive."""
from __future__ import annotations

import argparse
from datetime import datetime, timezone
import fnmatch
import hashlib
import io
import json
import os
from pathlib import Path
import re
import secrets
import shlex
import subprocess
import sys
import tarfile
import time


ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "tools"))
from integration_env import REMOTE, connect, run, write_remote
from gateway_source_race import TEST_FILES, REQUIRED_TESTS, isolated_environment, verify_remote, missing_required_tests
from run_linux_checks import scrub
from project_layout import server_source_dir, server_package, package_overlay

DEFAULT_CANDIDATE = ROOT / ".local/management-parity/linux-race-20260917T214152-2d29dc"
EXTRA_PATTERNS = ("gateway_timeout_pool_test.go", "gateway_feature_settings_test.go", "gateway_compact_test.go", "accounts_shutdown_test.go", "cli_timeout_test.go", "cli_sse_test.go", "source_observation*test.go")


def sha(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def inspect_candidate(candidate):
    manifest = json.loads((candidate / "source-manifest.json").read_text(encoding="utf-8"))
    canonical = hashlib.sha256(json.dumps(manifest["files"], sort_keys=True, separators=(",", ":")).encode()).hexdigest()
    if canonical != manifest["sha256"] or sha(candidate / "source.tar.gz") != manifest["archive_sha256"]:
        raise RuntimeError("Candidate source/manifest archive identity differs")
    with tarfile.open(candidate / "source.tar.gz", "r:gz") as archive:
        contents = {}
        for member in archive.getmembers():
            if not member.isfile() or member.name not in manifest["files"]:
                raise RuntimeError("Archive contains unmanifested or non-file entry")
            raw = archive.extractfile(member).read()
            if hashlib.sha256(raw).hexdigest() != manifest["files"][member.name]:
                raise RuntimeError("Archived source hash differs: " + member.name)
            contents[member.name] = raw
    if set(contents) != set(manifest["files"]):
        raise RuntimeError("Archive missing manifested files")
    selected = set()
    for name, raw in contents.items():
        if any(fnmatch.fnmatchcase(Path(name).name, pattern) for pattern in TEST_FILES + EXTRA_PATTERNS):
            selected.update(re.findall(r"^func (Test\w+)\(t \*testing.T\)", raw.decode(), re.M))
    selected.discard("TestPGShutdownProcessHelper")
    if missing_required_tests(selected, REQUIRED_TESTS):
        raise RuntimeError("Frozen gateway selector missing required existing tests")
    overlay = Path(__file__).with_name("environment_test.go.txt")
    selected.update(re.findall(r"^func (Test\w+)\(t \*testing.T\)", overlay.read_text(encoding="utf-8"), re.M))
    return manifest, contents, sorted(selected), overlay


def parse_log(raw, selected):
    outcomes = {"pass": [], "fail": [], "skip": []}
    chunks = []
    packages = []
    for line in raw.splitlines():
        try:
            event = json.loads(line)
        except ValueError:
            continue
        if not isinstance(event, dict):
            continue
        chunks.append(event.get("Output", ""))
        action, name = event.get("Action"), event.get("Test")
        if name and action in outcomes:
            outcomes[action].append(name)
        elif not name and action == "pass":
            packages.append(event.get("Package"))
    metrics = []
    for line in "".join(chunks).splitlines():
        marker = "GATEWAY_ENVIRONMENT_METRIC "
        if marker in line:
            metrics.append(json.loads(line.split(marker, 1)[1]))
    missing = sorted(set(selected) - set(outcomes["pass"]) - set(outcomes["fail"]) - set(outcomes["skip"]))
    return outcomes, metrics, missing, packages


def run_local(output, source, manifest, candidate, selected, overlay_source, environment):
    """A useful local fallback that cannot turn skipped PG/race checks into PASS."""
    local_overlay = output / "local-overlay.json"
    local_overlay.write_text(json.dumps({"Replace": {str(server_source_dir(source) / "zz_gateway_environment_overlay_test.go"): str(source / "environment-package-test.go.txt")}}), encoding="utf-8")
    environment = environment.copy()
    for key in list(environment):
        if key.startswith("TEST_") or key in {"DATABASE_URL", "REDIS_URL", "CODEX_REAL_CLI_BIN", "CODEX_CLI_ACCOUNT_FILE", "CODEX_CLI_UPSTREAM_KEY", "CODEX_CLI_UPSTREAM_URL"}:
            environment.pop(key, None)
    environment.update(CGO_ENABLED="0", HTTP_PROXY="", HTTPS_PROXY="", ALL_PROXY="", NO_PROXY="*")
    regex = "^(" + "|".join(selected) + ")$"
    command = ["go", "test", "-mod=vendor", f"-overlay={local_overlay}", "-json", "-count=1", "-timeout=4m", "-run", regex, server_package(source)]
    started = time.monotonic()
    tests = subprocess.run(command, cwd=source, env=environment, capture_output=True, text=True, encoding="utf-8", errors="replace", timeout=300)
    raw = tests.stdout + tests.stderr
    (output / "local-go-test.jsonl").write_text(raw, encoding="utf-8")
    outcomes, metrics, missing, packages = parse_log(raw, selected)
    vet = subprocess.run(["go", "vet", "-mod=vendor", f"-overlay={local_overlay}", server_package(source)], cwd=source, env=environment, capture_output=True, text=True, encoding="utf-8", errors="replace", timeout=180)
    (output / "local-vet.log").write_text(vet.stdout + vet.stderr, encoding="utf-8")
    unchanged = all(sha(source / name) == expected for name, expected in manifest["files"].items())
    report = {"schema": "codex-agent-frozen-gateway-local-v1", "source_sha256": manifest["sha256"],
        "candidate_binary_sha256": sha(candidate / "codex-agent-linux-amd64"), "candidate_archive_sha256": manifest["archive_sha256"],
        "overlay_sha256": sha(overlay_source), "runner_sha256": sha(Path(__file__)),
        "sql_sha256": {key: value for key, value in manifest["files"].items() if key.startswith("migrations/") and key.endswith(".sql")},
        "status": "fail" if tests.returncode or vet.returncode or outcomes["fail"] or missing or not packages or not unchanged else "blocked",
        "local_tests_status": "pass" if tests.returncode == 0 and not outcomes["fail"] and not missing and packages else "fail",
        "local_vet_status": "pass" if vet.returncode == 0 else "fail", "scope": "local synthetic gateway checks only; storage and Linux race remain unexecuted",
        "selected_tests": selected, "passed_test_events": len(outcomes["pass"]), "passed_top_level_tests": sum("/" not in item for item in outcomes["pass"]),
        "failed_tests": outcomes["fail"], "skipped_tests": outcomes["skip"], "missing_tests": missing,
        "actual_pg_redis": False, "race_executed": False, "production_requests": 0, "synthetic_only": True,
        "environment_metrics": metrics, "frozen_source_unchanged": unchanged,
        "management_evidence_reused": {"path": str(candidate / "report.json"), "sha256": sha(candidate / "report.json")},
        "blocked_checks": ["Same-snapshot gateway integration with PostgreSQL/Redis", "Same-snapshot Linux race"],
        "duration_seconds": round(time.monotonic() - started, 3), "completed_at": datetime.now(timezone.utc).isoformat(), "command": command,
        "artifacts": {name: sha(output / name) for name in ("local-go-test.jsonl", "local-vet.log")}}
    (output / "report.json").write_text(json.dumps(report, indent=2) + "\n", encoding="utf-8")
    print(json.dumps({"status": report["status"], "local_tests_status": report["local_tests_status"], "local_vet_status": report["local_vet_status"],
        "passed_top_level_tests": report["passed_top_level_tests"], "failed_tests": outcomes["fail"], "skipped_tests": outcomes["skip"], "report": str(output / "report.json")}), flush=True)
    return 1 if report["status"] == "fail" else 2


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--candidate", type=Path, default=DEFAULT_CANDIDATE)
    parser.add_argument("--plan", action="store_true")
    parser.add_argument("--local", action="store_true", help="No SSH; run ordinary synthetic Go tests and vet, explicitly leaving PG/Redis/race blocked")
    args = parser.parse_args()
    candidate = args.candidate.resolve()
    manifest, contents, selected, overlay_source = inspect_candidate(candidate)
    if args.plan:
        print(json.dumps({"source_sha256": manifest["sha256"], "selected_tests": len(selected), "candidate_binary_sha256": sha(candidate / "codex-agent-linux-amd64"), "tests": selected}, indent=2))
        return 0
    stamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%S") + "-" + secrets.token_hex(3)
    output = ROOT / ".local/frozen-gateway-race" / stamp
    output.mkdir(parents=True, exist_ok=False)
    source = output / "source"
    source.mkdir()
    for name, raw in contents.items():
        path = source / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(raw)
    overlay_name = "integration/gateway_environment/environment_test.go.txt"
    (source / overlay_name).parent.mkdir(parents=True, exist_ok=True)
    (source / overlay_name).write_bytes(overlay_source.read_bytes())
    actual_overlay = package_overlay(overlay_source, source / "environment-package-test.go.txt", source)
    virtual = server_source_dir(source) / "zz_gateway_environment_overlay_test.go"
    (source / "overlay.json").write_text(json.dumps({"Replace": {"/src/" + virtual.relative_to(source).as_posix(): "/src/" + actual_overlay.name}}), encoding="utf-8")
    environment = os.environ.copy()
    environment.update(GOPROXY="off", GOSUMDB="off", GOWORK="off", GOFLAGS="-mod=readonly")
    vendor = subprocess.run(["go", "mod", "vendor"], cwd=source, env=environment, capture_output=True, text=True, timeout=180)
    (output / "vendor.log").write_text(vendor.stdout + vendor.stderr, encoding="utf-8")
    if vendor.returncode:
        raise RuntimeError("Offline vendoring failed, inspect vendor.log")
    for name, expected in manifest["files"].items():
        if sha(source / name) != expected:
            raise RuntimeError("Offline setup modified frozen source")
    if args.local:
        return run_local(output, source, manifest, candidate, selected, overlay_source, environment)
    archive_path = output / "source-with-overlay.tar.gz"
    with tarfile.open(archive_path, "w:gz") as archive:
        for path in sorted(source.rglob("*")):
            if path.is_file():
                raw = path.read_bytes()
                entry = tarfile.TarInfo(path.relative_to(source).as_posix())
                entry.size, entry.mode, entry.mtime = len(raw), 0o644, 0
                archive.addfile(entry, io.BytesIO(raw))
    prefix = "codex-agent:integration:frozen-gateway:" + stamp.lower() + ":"
    values, private = isolated_environment(prefix)
    values.update(GOPROXY="off", GOSUMDB="off", GOWORK="off", GOFLAGS="-mod=vendor")
    remote = REMOTE + "/checks/frozen-gateway-" + stamp.lower()
    name = "codex-agent-frozen-gateway-" + stamp.lower()
    report = {"schema": "codex-agent-frozen-gateway-race-v1", "source_sha256": manifest["sha256"],
        "candidate_binary_sha256": sha(candidate / "codex-agent-linux-amd64"), "candidate_archive_sha256": manifest["archive_sha256"],
        "overlay_sha256": sha(overlay_source), "runner_sha256": sha(Path(__file__)),
        "sql_sha256": {key: value for key, value in manifest["files"].items() if key.startswith("migrations/") and key.endswith(".sql")},
        "status": "running", "scope": "frozen candidate gateway integration; no management-suite rerun, production inference or deployment",
        "selected_tests": selected, "resources": {"cpus": 2, "memory": "2g", "pids": 128},
        "synthetic_only": True, "production_requests": 0, "redis_prefix": prefix, "remote_directory": remote,
        "management_evidence_reused": {"path": str(candidate / "report.json"), "sha256": sha(candidate / "report.json")}}
    client = None
    identifier = ""
    env_written = False
    started = time.monotonic()
    try:
        client = connect()
        image = verify_remote(client)
        report["image_id"] = image
        run(client, "install -d -m 700 " + shlex.quote(remote) + " " + shlex.quote(remote + "/source"))
        with client.open_sftp() as sftp:
            sftp.put(str(archive_path), remote + "/source.tar.gz")
            sftp.chmod(remote + "/source.tar.gz", 0o600)
        if run(client, "sha256sum " + shlex.quote(remote + "/source.tar.gz")).split()[0] != sha(archive_path):
            raise RuntimeError("Uploaded archive hash mismatch")
        write_remote(client, remote + "/test.env", "".join(key + "=" + value + "\n" for key, value in sorted(values.items())))
        env_written = True
        run(client, "tar -xzf " + shlex.quote(remote + "/source.tar.gz") + " -C " + shlex.quote(remote + "/source"))
        regex = "^(" + "|".join(selected) + ")$"
        command = ["go", "test", "-mod=vendor", "-overlay=/src/overlay.json", "-race", "-count=1", "-timeout=8m", "-json", "-run", regex, server_package(source)]
        script = "\n".join(["set -eu", "cd /src", "go version", "go env GOOS GOARCH CGO_ENABLED CC", shlex.join(command)])
        run_args = ["docker", "run", "-d", "--name", name, "--label", "codex-agent.integration=frozen-gateway-race",
            "--cpus", "2", "--memory", "2g", "--pids-limit", "128", "--network", "host", "--cap-drop", "ALL",
            "--security-opt", "no-new-privileges", "--env-file", remote + "/test.env", "-v", remote + "/source:/src:ro", image, "sh", "-c", script]
        identifier = run(client, shlex.join(run_args)).strip()
        if not re.fullmatch(r"[a-f0-9]{64}", identifier):
            raise RuntimeError("Unexpected container identifier")
        report["container_id"] = identifier
        (output / "running.json").write_text(json.dumps(report, indent=2), encoding="utf-8")
        print(json.dumps({"started": True, "source_sha256": manifest["sha256"], "selected_tests": len(selected), "output": str(output), "container_id": identifier}), flush=True)
        deadline = time.monotonic() + 900
        while True:
            state = json.loads(run(client, "docker inspect --format '{{json .State}}' " + identifier))
            if not state["Running"]:
                break
            if time.monotonic() > deadline:
                raise TimeoutError("bounded synthetic suite exceeded 900 seconds")
            time.sleep(10)
        raw = run(client, "docker logs " + identifier + " 2>&1", timeout=60)
        (output / "race.log").write_text(scrub(raw, private), encoding="utf-8")
        outcomes, metrics, missing, packages = parse_log(raw, selected)
        status = "fail" if state["ExitCode"] or outcomes["fail"] or "WARNING: DATA RACE" in raw else "blocked" if outcomes["skip"] or missing or not packages else "pass"
        report.update(status=status, exit_code=state["ExitCode"], oom_killed=state.get("OOMKilled", False),
            passed_test_events=len(outcomes["pass"]), passed_top_level_tests=sum("/" not in value for value in outcomes["pass"]),
            failed_tests=outcomes["fail"], skipped_tests=outcomes["skip"], missing_tests=missing, environment_metrics=metrics,
            data_race_reported="WARNING: DATA RACE" in raw, actual_pg_redis=True)
    except Exception as error:
        report.update(status="fail", error=scrub(type(error).__name__ + ": " + str(error), private))
        if client and identifier:
            try:
                raw = run(client, "docker logs " + identifier + " 2>&1", timeout=60)
                (output / "race.log").write_text(scrub(raw, private), encoding="utf-8")
            except Exception:
                pass
    finally:
        if client:
            if identifier:
                try:
                    run(client, "docker rm -f " + shlex.quote(identifier), timeout=60)
                    report["temporary_container_removed"] = True
                except Exception:
                    report.update(status="fail", temporary_container_removed=False)
            if env_written:
                try:
                    with client.open_sftp() as sftp:
                        sftp.remove(remote + "/test.env")
                    report["temporary_credentials_removed"] = True
                except Exception:
                    report.update(status="fail", temporary_credentials_removed=False)
            client.close()
    report.update(duration_seconds=round(time.monotonic() - started, 3), completed_at=datetime.now(timezone.utc).isoformat())
    report["source_manifest_sha256"] = sha(candidate / "source-manifest.json")
    if (output / "race.log").exists():
        report["race_log_sha256"] = sha(output / "race.log")
    (output / "report.json").write_text(json.dumps(report, indent=2) + "\n", encoding="utf-8")
    print(json.dumps({"status": report["status"], "report": str(output / "report.json"), "passed_top_level_tests": report.get("passed_top_level_tests", 0), "failed_tests": report.get("failed_tests", []), "skipped_tests": report.get("skipped_tests", [])}), flush=True)
    return 0 if report["status"] == "pass" else 1


if __name__ == "__main__":
    raise SystemExit(main())
