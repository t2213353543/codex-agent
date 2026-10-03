"""Run frozen synthetic BPS tests under Linux race detection.

Uses the existing integration compiler image over the project SSH helper. Default
mode has no container networking. --with-redis and --redis-only permit only the
existing isolated Redis fixture, whose credentials never enter reports or source
archives. Model account credentials and business databases are never loaded.
--include-official additionally runs synthetic official-runtime and mode tests,
excluding live-account, PostgreSQL and fixture-only subprocess entry points.
"""
from __future__ import annotations
import datetime as dt
import argparse
import hashlib
import io
import json
from pathlib import Path
import re
import shlex
import subprocess
import sys
import tarfile
import time
import os
from urllib.parse import urlparse

ROOT = Path(__file__).resolve().parents[1]
DEFAULT_IMAGE = "codex-agent-integration-race:ce864e7223ac17b1-v1"
sys.path.insert(0, str(ROOT / "tools"))
from integration_env import REMOTE, connect, run, write_remote
from project_layout import backend_source_paths


def digest(data):
    return hashlib.sha256(data).hexdigest()


def source_hashes():
    return {p.relative_to(ROOT).as_posix(): digest(p.read_bytes()) for p in backend_source_paths(ROOT)}


def freeze(output):
    folder = output / "source"
    folder.mkdir()
    hashes = {}
    for path in backend_source_paths(ROOT):
        name, raw = path.relative_to(ROOT).as_posix(), path.read_bytes()
        hashes[name] = digest(raw)
        target = folder / name
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_bytes(raw)
    if hashes != source_hashes():
        raise RuntimeError("Source changed during snapshot; freeze again")
    env = os.environ.copy()
    env.update(GOPROXY="off", GOSUMDB="off", GOWORK="off", GOFLAGS="-mod=readonly")
    proc = subprocess.run(["go", "mod", "vendor"], cwd=folder, env=env,
                          stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=180)
    (output / "vendor.log").write_bytes(proc.stdout)
    if proc.returncode:
        raise RuntimeError("Offline module vendoring failed")
    manifest = {"source_files": hashes, "source_sha256": digest(json.dumps(hashes, sort_keys=True, separators=(",", ":")).encode()),
                "runner_sha256": digest(Path(__file__).read_bytes()), "synthetic_only": True, "offline_dependencies": True}
    archive = output / "source.tar.gz"
    uploaded = {}
    with tarfile.open(archive, "w:gz") as tar:
        for path in sorted(folder.rglob("*")):
            if not path.is_file():
                continue
            name, raw = path.relative_to(folder).as_posix(), path.read_bytes()
            if name in hashes and digest(raw) != hashes[name]:
                raise RuntimeError("Frozen source changed during offline vendoring")
            info = tarfile.TarInfo(name)
            info.size, info.mode, info.mtime = len(raw), 0o644, 0
            tar.addfile(info, io.BytesIO(raw))
            uploaded[name] = digest(raw)
    manifest.update(archive_sha256=digest(archive.read_bytes()), archive_bytes=archive.stat().st_size, uploaded_files=uploaded)
    (output / "source-manifest.json").write_text(json.dumps(manifest, indent=2) + "\n", encoding="utf-8")
    return archive, manifest


def parse_events(raw):
    tests, packages = {}, {}
    for line in raw.splitlines():
        try:
            event = json.loads(line)
        except ValueError:
            continue
        if not isinstance(event, dict):
            continue
        if event.get("Action") in {"pass", "fail", "skip"}:
            if event.get("Test"):
                tests[event["Package"] + "/" + event["Test"]] = event["Action"]
            else:
                packages[event.get("Package", "unknown")] = event["Action"]
    return {"tests": tests, "package_results": packages, "race_warning": "WARNING: DATA RACE" in raw}


def official_synthetic_tests(source):
    names = set()
    for path in (source / "internal/server").glob("*_test.go"):
        for name in re.findall(r"^func (Test(?:Official|InferenceMode|DashboardRealtime)\w+)\(t \*testing.T\)", path.read_text(encoding="utf-8"), re.M):
            # PostgreSQL integration has its own isolated gate; this harness
            # deliberately never loads business-database credentials.
            if "Live" not in name and "Fixture" not in name and not (name.startswith("TestOfficialUsageRepair") and "PG" in name):
                names.add(name)
    if not names:
        raise RuntimeError("No synthetic official runtime tests were found")
    return sorted(names)


COLLABORATIVE_TESTS = {
    "gateway_bps_cooldown_handoff_test.go": (
        "TestTeamCRateLimitResolvedCooldownFailureBoundary",
        "TestTeamCRateLimitCreatedDoesNotBlockFailover",
        "TestTeamCRateLimitCooldownUsesAcceptedSSE",
        "TestTeamCRateLimitAllAccountsCooldownNextRequest",
        "TestTeamCRateLimitAliasChecksResolvedCooldownBeforeDispatch",
        "TestTeamCRateLimitAliasLocalSelectionAttemptAccounting",
    ),
    "gateway_failover_abort_record_test.go": (
        "TestGatewayFailoverAbortRecordPreservesFailureAndPanic",
        "TestGatewayFailoverAbortRecordRetainsEarlierAttemptsAndTTFT",
        "TestGatewayFailoverAbortRecordKeepsOriginalPanicWhenAccountingPanics",
        "TestGatewayFailoverAbortRecordHandlesGoexitAndMissingRecord",
    ),
    "gateway_bps_structured_transport_error_test.go": (
        "TestCollabStructuredWriteErrorNotValidationFailure",
        "TestCollabStructuredIncompleteTransportNotValidationFailure",
    ),
    "settings_rate_limit_cooldown_test.go": (
        "TestRateLimitCooldownDefaultsAndAdminPersistence",
        "TestRateLimitCooldownDisabledBypassesExistingAndPreservesTTL",
        "TestRateLimitCooldownDisabledAvoidsRedisAndSettingsFailureIs503",
        "TestRateLimitCooldownDisabledPreservesReal429AndSSEFailures",
        "TestRateLimitCooldownDisabledStillFailsOverCurrentQuotaRequest",
        "TestRateLimitCooldownDisabledStillFailsOverCurrentQuotaRequest",
    ),
    "gateway_bps_selection_attempt_test.go": (
        "TestCollabSessionOwnerLocalAttemptClassification",
        "TestCollabSessionOwnerLocalAttemptReplacementKeepsOnlyRealGeneration",
    ),
}

REDIS_COLLABORATIVE_TESTS = {
    "settings_rate_limit_cooldown_test.go": (
        "TestRateLimitCooldownRedisDisableDoesNotDeleteSharedDeadline",
    ),
}


def collaborative_synthetic_tests(source, include_redis=False):
    names = set()
    required_files = list(COLLABORATIVE_TESTS.items())
    if include_redis:
        required_files.extend(REDIS_COLLABORATIVE_TESTS.items())
    for filename, required in required_files:
        path = source / "internal/server" / filename
        declared = set(re.findall(r"^func (Test\w+)\(t \*testing.T\)", path.read_text(encoding="utf-8"), re.M))
        if not set(required) <= declared:
            raise RuntimeError("Required collaborative race tests missing: " + filename)
        names.update(required)
    return sorted(names)


def server_test_pattern(official_tests, collaborative_tests):
    return "^(" + "|".join(["TestBPS.*", "TestSharedAdmission.*", *sorted(set(official_tests) | set(collaborative_tests))]) + ")$"


def record_required_test_results(report, field, names):
    results = {name: report["tests"].get("codex-agent/internal/server/" + name) for name in names}
    report[field] = results
    if any(state != "pass" for state in results.values()):
        report["status"] = "fail"


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    mode = parser.add_mutually_exclusive_group()
    mode.add_argument("--redis-only", action="store_true", help="Only isolated synthetic Redis history tests; no inference credentials")
    mode.add_argument("--with-redis", action="store_true", help="Full BPS protocol/gateway tests and isolated synthetic Redis tests")
    parser.add_argument("--image", default=DEFAULT_IMAGE, help="Existing compiler image; resolved to its immutable image ID")
    parser.add_argument("--timeout", type=int, default=900, help="Whole container deadline, in seconds (60-1800)")
    parser.add_argument("--prepare-only", action="store_true", help="Freeze and vendor source locally; do not connect to a server")
    parser.add_argument("--include-official", action="store_true", help="Add synthetic official CLI, App Server and inference-mode tests; excludes Live, PostgreSQL and Fixture entry points")
    args = parser.parse_args()
    if not 60 <= args.timeout <= 1800:
        parser.error("--timeout must be between 60 and 1800")
    if args.redis_only and args.include_official:
        parser.error("--include-official requires the full suite, not --redis-only")
    uses_redis = args.redis_only or args.with_redis
    stamp = dt.datetime.now(dt.timezone.utc).strftime("%Y%m%dT%H%M%S.%fZ")
    output = ROOT / ".local/powerpoint-migration/race" / stamp
    output.mkdir(parents=True)
    report = {"status": "fail", "started_at": stamp, "synthetic_only": True,
              "production_requests": 0, "database_access": False, "network": "none",
              "scope": ["go test -race ./internal/bps", "go test -race -run ^TestBPS ./internal/server"]}
    client = None
    container = ""
    remote_env = ""
    private_values = []
    manifest = None
    started = time.monotonic()
    try:
        archive, manifest = freeze(output)
        print("Frozen source " + manifest["source_sha256"] + "; archive bytes=" + str(manifest["archive_bytes"]), flush=True)
        report.update(source_sha256=manifest["source_sha256"], archive_sha256=manifest["archive_sha256"])
        official_tests = official_synthetic_tests(output / "source") if args.include_official else []
        collaborative_tests = [] if args.redis_only else collaborative_synthetic_tests(output / "source", include_redis=args.with_redis)
        server_pattern = server_test_pattern(official_tests, collaborative_tests)
        # The real large-payload fixture alone can take ~245s with the race
        # detector. Leave room for the rest of this growing synthetic suite.
        # This test deadline does not change gateway/runtime request limits.
        server_command = "go test -mod=vendor -race -json -count=1 -timeout=600s -run " + shlex.quote(server_pattern) + " ./internal/server"
        ingress_command = "go test -mod=vendor -race -json -count=1 -timeout=90s ./cmd/codex-agent-ingress" if (ROOT / "cmd/codex-agent-ingress").is_dir() else None
        report.update(include_official=args.include_official, official_expected_tests=official_tests,
                      collaborative_expected_tests=collaborative_tests)
        report["scope"] = ["go test -race ./internal/bps", server_command]
        if args.prepare_only:
            report["status"] = "prepared"
            return 0
        client = connect()
        run(client, "test -f " + shlex.quote(REMOTE + "/.codex-agent-integration"))
        image = run(client, "docker image inspect --format '{{.Id}}' " + shlex.quote(args.image)).strip()
        if not re.fullmatch(r"sha256:[0-9a-f]{64}", image):
            raise RuntimeError("Cannot pin existing compiler image")
        remote = REMOTE + "/checks/powerpoint-race-" + stamp.lower()
        report.update(image_id=image, source_sha256=manifest["source_sha256"], archive_sha256=manifest["archive_sha256"], remote_directory=remote)
        run(client, "install -d -m 700 " + shlex.quote(remote + "/source"))
        with client.open_sftp() as sftp:
            sftp.put(str(archive), remote + "/source.tar.gz")
            sftp.chmod(remote + "/source.tar.gz", 0o600)
        actual = run(client, "sha256sum " + shlex.quote(remote + "/source.tar.gz")).split()[0]
        if actual != manifest["archive_sha256"]:
            raise RuntimeError("Upload digest mismatch")
        write_remote(client, remote + "/source-manifest.json", (output / "source-manifest.json").read_text())
        run(client, "tar -xzf " + shlex.quote(remote + "/source.tar.gz") + " -C " + shlex.quote(remote + "/source"))
        script = "\n".join([
            "set -eu", "cd /src", "go version", "gcc --version", "go env GOOS GOARCH CGO_ENABLED CC",
            "unset BPS_LIVE_ACCOUNTS_FILE BPS_LIVE_CODEX_BINARY BPS_LIVE_WORK_ROOT OPENAI_API_KEY CODEX_API_KEY CODEX_HOME CODEX_CLI_PATH CODEX_RUNTIME_DIR DATABASE_URL TEST_DATABASE_URL REDIS_URL TEST_REDIS_URL",
            "go test -mod=vendor -race -json -count=1 -timeout=90s ./internal/bps",
            server_command,
        ] + ([ingress_command] if ingress_command else []))
        if uses_redis:
            settings = json.loads((ROOT / ".local/integration/environment.json").read_text())
            redis_url = settings.get("TEST_REDIS_URL") or settings.get("REDIS_URL")
            parsed = urlparse(redis_url or "")
            if parsed.scheme != "redis" or parsed.hostname != "127.0.0.1" or parsed.port != 26379 or parsed.username != "codex_agent_test" or "\n" in redis_url or "\r" in redis_url:
                raise RuntimeError("Refusing any Redis except the existing isolated integration endpoint")
            private_values.extend(value for value in (redis_url, parsed.password) if value)
            remote_env = remote + "/redis-test.env"
            write_remote(client, remote_env, "TEST_REDIS_URL=" + redis_url + "\nTEST_REDIS_PREFIX=codex-agent:integration:bps-test:\n")
            commands = ["go test -mod=vendor -race -json -count=1 -timeout=90s -run '^TestBPSRedis' ./internal/server"] if args.redis_only else [
                "go test -mod=vendor -race -json -count=1 -timeout=90s ./internal/bps",
                server_command,
            ]
            if ingress_command and not args.redis_only:
                commands.append(ingress_command)
            script = "\n".join([
                "set -eu", "cd /src", "go version", "gcc --version", "go env GOOS GOARCH CGO_ENABLED CC",
                "unset BPS_LIVE_ACCOUNTS_FILE BPS_LIVE_CODEX_BINARY BPS_LIVE_WORK_ROOT OPENAI_API_KEY CODEX_API_KEY CODEX_HOME CODEX_CLI_PATH CODEX_RUNTIME_DIR DATABASE_URL TEST_DATABASE_URL REDIS_URL",
            ] + commands)
            report.update(network="host", database_access="isolated Redis 26379 only", scope=commands, integration_namespace="codex-agent:integration:bps-test:<random>")
        command = ["docker", "run", "-d", "--name", "codex-agent-bps-race-" + stamp.lower(),
                   "--label", "codex-agent.integration=powerpoint-race", "--cpus", "2", "--memory", "8g",
                   "--pids-limit", "128", "--network", "host" if uses_redis else "none", "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
                   "-e", "CGO_ENABLED=1", "-e", "GOMAXPROCS=2", "-e", "GOPROXY=off", "-e", "GOSUMDB=off",
                   "-e", "GOWORK=off", "-e", "GOCACHE=/tmp/go-build", "-e", "GOMODCACHE=/tmp/go-mod",
                   "-v", remote + "/source:/src:ro", image, "sh", "-c", script]
        if uses_redis:
            command[2:2] = ["--env-file", remote_env]
        report["command"] = command
        container = run(client, shlex.join(command)).strip()
        if not re.fullmatch(r"[0-9a-f]{64}", container):
            raise RuntimeError("Invalid isolated container id")
        report["container_id"] = container
        print("Isolated Linux race started: " + container[:12], flush=True)
        deadline = time.monotonic() + args.timeout
        last_update = time.monotonic()
        while True:
            state = json.loads(run(client, "docker inspect --format '{{json .State}}' " + container))
            if not state["Running"]:
                break
            if time.monotonic() > deadline:
                run(client, "docker stop --time 5 " + container)
                raise TimeoutError("Bounded race run exceeded its configured deadline")
            if time.monotonic() - last_update > 20:
                print("Race compiler/test container still running; elapsed=" + str(round(time.monotonic() - started)) + "s", flush=True)
                last_update = time.monotonic()
            time.sleep(3)
        raw = run(client, "docker logs " + container + " 2>&1", timeout=60)
        for value in private_values:
            raw = raw.replace(value, "[redacted]")
        (output / "go-test.jsonl").write_text(raw, encoding="utf-8")
        report.update(parse_events(raw), exit_code=state["ExitCode"], oom_killed=state.get("OOMKilled", False))
        expected = {"codex-agent/internal/server"} if args.redis_only else {"codex-agent/internal/bps", "codex-agent/internal/server"}
        if ingress_command and not args.redis_only:
            expected.add("codex-agent/cmd/codex-agent-ingress")
        report["status"] = "pass" if state["ExitCode"] == 0 and set(report["package_results"]) == expected and all(
            state == "pass" for state in report["package_results"].values()) and not report["race_warning"] else "fail"
        if args.redis_only and (len(report["tests"]) != 4 or any(status != "pass" for status in report["tests"].values())):
            report["status"] = "fail"
        if args.with_redis:
            redis_results = {name: status for name, status in report["tests"].items() if "/TestBPSRedis" in name}
            if len(redis_results) != 4 or any(status != "pass" for status in redis_results.values()):
                report["status"] = "fail"
            report["redis_test_results"] = redis_results
        if official_tests:
            record_required_test_results(report, "official_test_results", official_tests)
        if collaborative_tests:
            record_required_test_results(report, "collaborative_test_results", collaborative_tests)
    except Exception as error:
        message = str(error)
        for value in private_values:
            message = message.replace(value, "[redacted]")
        report.update(error_type=type(error).__name__, error=message)
        if client and re.fullmatch(r"[0-9a-f]{64}", container):
            try:
                raw = run(client, "docker logs " + container + " 2>&1", timeout=60)
                for value in private_values:
                    raw = raw.replace(value, "[redacted]")
                (output / "go-test.jsonl").write_text(raw, encoding="utf-8")
                report.update(parse_events(raw))
            except Exception:
                pass
    finally:
        if client and re.fullmatch(r"[0-9a-f]{64}", container):
            try:
                run(client, "docker rm -f " + container)
                report["temporary_container_removed"] = True
            except Exception:
                report.update(status="fail", container_cleanup_error="Temporary integration container cleanup failed")
        if client and remote_env:
            try:
                run(client, "rm -f -- " + shlex.quote(remote_env))
                report["temporary_redis_environment_removed"] = True
            except Exception:
                report.update(status="fail", redis_environment_cleanup_error="Temporary Redis environment cleanup failed")
        if client:
            client.close()
        if manifest:
            now = source_hashes()
            changed = sorted(name for name in set(now) | set(manifest["source_files"]) if now.get(name) != manifest["source_files"].get(name))
            report.update(source_changed_after_freeze=bool(changed), changed_local_files=changed)
        report.update(duration_seconds=round(time.monotonic() - started, 3), completed_at=dt.datetime.now(dt.timezone.utc).isoformat())
        (output / "report.json").write_text(json.dumps(report, indent=2) + "\n", encoding="utf-8")
        if args.prepare_only:
            print(json.dumps({"status": report["status"], "report": str(output / "report.json"), "source_sha256": report.get("source_sha256")}), flush=True)
    print(json.dumps({"status": report["status"], "source_sha256": report.get("source_sha256"), "source_changed_after_freeze": report.get("source_changed_after_freeze"), "report": str(output / "report.json"), "error": report.get("error")}), flush=True)
    return 0 if report["status"] == "pass" else 1


if __name__ == "__main__":
    raise SystemExit(main())
