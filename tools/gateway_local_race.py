"""Run frozen gateway race tests in disposable local Docker PG/Redis containers."""
from __future__ import annotations

from project_layout import server_package

import argparse
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import re
import secrets
import shlex
import subprocess
import time

from gateway_source_race import ROOT, go_manifest, selected_tests, snapshot
from run_linux_checks import scrub


IMAGES = {"go": "golang:1.26.7-alpine3.23", "pg": "postgres:18-alpine", "redis": "redis:8-alpine"}
LABEL = "codex-agent.integration=local-source-race"


def docker(*arguments: str, timeout: int = 60, check: bool = True) -> subprocess.CompletedProcess:
    result = subprocess.run(["docker", *arguments], capture_output=True, text=True, encoding="utf-8",
                            errors="replace", timeout=timeout)
    if check and result.returncode:
        raise RuntimeError("Docker operation failed: " + result.stderr[-2000:])
    return result


def local_engine() -> dict:
    if os.environ.get("DOCKER_HOST") or os.environ.get("DOCKER_CONTEXT"):
        raise RuntimeError("Use the local Docker Desktop context without endpoint overrides")
    host = json.loads(docker("context", "inspect", "--format", "{{json .Endpoints.docker.Host}}").stdout)
    if not (host.startswith("npipe:////./pipe/docker") or host == "unix:///var/run/docker.sock"):
        raise RuntimeError("Only a local Docker engine is allowed")
    server = json.loads(docker("version", "--format", "{{json .Server}}").stdout)
    if server.get("Os") != "linux":
        raise RuntimeError("Local Docker must run Linux containers")
    return {"version": server["Version"], "os": server["Os"], "arch": server["Arch"]}


def image_id(tag: str, output: Path) -> str:
    exists = docker("image", "inspect", "--format", "{{.Id}}", tag, check=False)
    if exists.returncode:
        print(json.dumps({"pulling_image": tag}), flush=True)
        pull = docker("pull", tag, timeout=900)
        (output / (tag.split(":")[0] + "-pull.log")).write_text(pull.stdout + pull.stderr, encoding="utf-8")
        exists = docker("image", "inspect", "--format", "{{.Id}}", tag)
    identifier = exists.stdout.strip()
    if not re.fullmatch(r"sha256:[0-9a-f]{64}", identifier):
        raise RuntimeError("Unexpected local image identity")
    return identifier


def wait_ready(name: str, command: list[str], expected: str) -> None:
    deadline = time.monotonic() + 120
    while time.monotonic() < deadline:
        state = json.loads(docker("inspect", "--format", "{{json .State}}", name).stdout)
        if not state["Running"]:
            raise RuntimeError("Synthetic dependency exited before readiness")
        result = docker("exec", name, *command, check=False)
        if result.returncode == 0 and result.stdout.strip() == expected:
            return
        time.sleep(1)
    raise TimeoutError("Synthetic dependency did not become ready")


def test_outcomes(raw: str, selected: list[str]) -> dict:
    outcomes, packages = {"pass": [], "fail": [], "skip": []}, []
    for line in raw.splitlines():
        try:
            event = json.loads(line)
        except ValueError:
            continue
        if not isinstance(event, dict):
            continue
        action, test = event.get("Action"), event.get("Test")
        if test and action in outcomes:
            outcomes[action].append(test)
        elif not test and action == "pass":
            packages.append(event.get("Package"))
    missing = sorted(set(selected) - set(outcomes["pass"]) - set(outcomes["fail"]) - set(outcomes["skip"]))
    return {"passed_top_level_tests": sum("/" not in test for test in outcomes["pass"]),
            "passed_test_events": len(outcomes["pass"]), "failed_tests": outcomes["fail"],
            "skipped_tests": outcomes["skip"], "missing_tests": missing, "passed_packages": packages}


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    modes = parser.add_mutually_exclusive_group(required=True)
    modes.add_argument("--plan", action="store_true")
    modes.add_argument("--run", action="store_true")
    parser.add_argument("--expected-go-hash")
    parser.add_argument("--source-dir", type=Path, default=ROOT)
    args = parser.parse_args()
    source_root = args.source_dir.resolve()
    if not source_root.is_relative_to(ROOT.resolve()):
        raise RuntimeError("Source directory must be inside this workspace")
    manifest = go_manifest(source_root)
    selected = selected_tests(manifest, source_root)
    if args.plan:
        print(json.dumps({"go_sha256": manifest["sha256"], "selected_tests": len(selected),
                          "images": IMAGES, "host_ports": [], "mutations_performed": False}))
        return 0
    if args.expected_go_hash != manifest["sha256"]:
        raise RuntimeError("Run requires the explicitly frozen current Go-only source hash")
    engine = local_engine()
    stamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%S").lower() + "-" + secrets.token_hex(3)
    output = ROOT / ".local/source-20260918" / ("local-linux-race-" + stamp)
    output.mkdir(parents=True, exist_ok=False)
    archive = snapshot(output, manifest, source_root)
    prefix = "codex-agent:integration:local-source:" + stamp + ":"
    name = "codex-agent-local-race-" + stamp
    run_label = "codex-agent.integration.run=" + stamp
    names = {kind: name + "-" + kind for kind in IMAGES}
    private = output / "private"
    private.mkdir(mode=0o700)
    pg_password, pg_admin, redis_password = [secrets.token_hex(24) for _ in range(3)]
    hidden = [pg_password, pg_admin, redis_password]
    private_files: list[Path] = []

    def private_file(filename: str, content: str, mode: int = 0o600) -> Path:
        path = private / filename
        path.write_text(content, encoding="utf-8")
        path.chmod(mode)
        private_files.append(path)
        return path

    pg_env = private_file("pg.env", "POSTGRES_PASSWORD=" + pg_admin + "\n")
    pg_init = private_file("init.sql", "CREATE ROLE codex_agent_test LOGIN PASSWORD '" + pg_password +
                           "' NOSUPERUSER NOCREATEDB NOCREATEROLE;\nCREATE DATABASE codex_agent_integration OWNER codex_agent_test;\n", 0o644)
    redis_env = private_file("redis.env", "REDISCLI_AUTH=" + redis_password + "\n")
    redis_conf = private_file("redis.conf", "bind 0.0.0.0\nprotected-mode yes\nappendonly no\nsave \"\"\n"
                              "maxmemory 96mb\nmaxmemory-policy noeviction\nuser default off\nuser codex_agent_test on >" +
                              redis_password + " ~" + prefix + "* &" + prefix + "* +@all -@dangerous\n", 0o644)
    values = {"TEST_DATABASE_URL": "postgres://codex_agent_test:" + pg_password +
              "@pg:5432/codex_agent_integration?sslmode=disable", "TEST_REDIS_URL":
              "redis://codex_agent_test:" + redis_password + "@redis:6379/0",
              "TEST_REDIS_PREFIX": prefix, "REDIS_KEY_PREFIX": prefix, "REDIS_PREFIX": prefix,
              "REDIS_NAMESPACE": prefix, "CGO_ENABLED": "1", "GOMAXPROCS": "2", "GOFLAGS": "-mod=readonly"}
    test_env = private_file("test.env", "".join(key + "=" + value + "\n" for key, value in sorted(values.items())))
    hidden.extend(values.values())
    report = {"schema": "codex-agent-local-source-race-v1", "status": "fail", "engine": engine,
              "go_source_sha256": manifest["sha256"], "selected_top_level_tests": selected,
              "scope": "disposable local Linux containers; synthetic PG/Redis only; no production inference or deployment",
              "host_ports": [], "redis_prefix": prefix, "network": name, "run_label": run_label,
              "resources": {"go_cpus": 2, "go_memory": "2g", "pg_cpus": 1, "pg_memory": "512m",
                            "redis_cpus": 0.5, "redis_memory": "128m"}}
    started = time.monotonic()
    attempted: list[str] = []
    network_attempted = False
    print(json.dumps({"snapshot_frozen": True, "go_sha256": manifest["sha256"], "source_directory": str(source_root),
                      "selected_tests": len(selected), "report_directory": str(output)}), flush=True)

    def start(kind: str, arguments: list[str], image: str, command: list[str]) -> str:
        attempted.append(names[kind])
        result = docker("run", "-d", "--name", names[kind], "--label", LABEL, "--label", run_label,
                        "--network", name, "--network-alias", kind, "--pids-limit", "128",
                        "--security-opt", "no-new-privileges", *arguments, image, *command)
        identifier = result.stdout.strip()
        if not re.fullmatch(r"[0-9a-f]{64}", identifier):
            raise RuntimeError("Unexpected local container identity")
        details = json.loads(docker("inspect", identifier).stdout)[0]
        if details["HostConfig"].get("PortBindings"):
            raise RuntimeError("A synthetic container published a host port")
        print(json.dumps({"started": kind, "container": names[kind], "id": identifier,
                          "report_directory": str(output)}), flush=True)
        return identifier

    try:
        images = {kind: image_id(tag, output) for kind, tag in IMAGES.items()}
        report["images"] = {kind: {"tag": IMAGES[kind], "id": identifier} for kind, identifier in images.items()}
        network_attempted = True
        docker("network", "create", "--label", LABEL, "--label", run_label, name)
        start("pg", ["--cpus", "1", "--memory", "512m", "--env-file", str(pg_env),
                     "--tmpfs", "/var/lib/postgresql:rw,size=384m",
                     "--mount", "type=bind,source=" + str(pg_init) + ",target=/docker-entrypoint-initdb.d/01-user.sql,readonly"],
              images["pg"], ["postgres", "-c", "max_connections=30", "-c", "shared_buffers=64MB",
                              "-c", "statement_timeout=15000", "-c", "lock_timeout=3000"])
        start("redis", ["--cpus", "0.5", "--memory", "128m", "--env-file", str(redis_env),
                        "--tmpfs", "/data:rw,size=32m", "--mount", "type=bind,source=" + str(redis_conf) +
                        ",target=/usr/local/etc/redis/redis.conf,readonly"], images["redis"],
              ["redis-server", "/usr/local/etc/redis/redis.conf"])
        wait_ready(names["pg"], ["psql", "-U", "codex_agent_test", "-d", "codex_agent_integration", "-Atc",
                                  "SELECT rolsuper FROM pg_roles WHERE rolname=current_user"], "f")
        wait_ready(names["redis"], ["redis-cli", "--user", "codex_agent_test", "PING"], "PONG")
        outside = docker("exec", names["redis"], "redis-cli", "--user", "codex_agent_test", "SET",
                         "outside-test-namespace", "synthetic", check=False)
        if "NOPERM" not in outside.stdout + outside.stderr:
            raise RuntimeError("Redis synthetic ACL did not deny an out-of-namespace key")
        report.update(dependencies_ready=True, postgres_non_superuser_verified=True, redis_acl_boundary_verified=True)
        regex = "^(" + "|".join(selected) + ")$"
        command = ["go", "test", "-race", "-count=1", "-timeout=8m", "-json", "-run", regex, server_package(source_root)]
        script = "\n".join(["set -eu", "apk add --no-cache gcc musl-dev", "mkdir -p /work",
                            "tar -xzf /input/source.tar.gz -C /work", "cd /work", "go version",
                            "go env GOOS GOARCH CGO_ENABLED CC", shlex.join(command)])
        identifier = start("go", ["--cpus", "2", "--memory", "2g", "--cap-drop", "ALL", "--env-file", str(test_env),
                                  "--mount", "type=bind,source=" + str(archive) + ",target=/input/source.tar.gz,readonly"],
                           images["go"], ["sh", "-c", script])
        report["test_container_id"] = identifier
        deadline = time.monotonic() + 1800
        while True:
            state = json.loads(docker("inspect", "--format", "{{json .State}}", identifier).stdout)
            if not state["Running"]:
                break
            if time.monotonic() >= deadline:
                raise TimeoutError("Local compiler and race test exceeded 1800 seconds")
            time.sleep(10)
        logs = docker("logs", identifier)
        raw = logs.stdout + logs.stderr
        (output / "race.log").write_text(scrub(raw, hidden), encoding="utf-8")
        report.update(test_outcomes(raw, selected), exit_code=state["ExitCode"], oom_killed=state.get("OOMKilled", False))
        report["status"] = "pass" if (state["ExitCode"] == 0 and not report["failed_tests"] and
                                         not report["skipped_tests"] and not report["missing_tests"] and
                                         report["passed_packages"] and report["passed_top_level_tests"] == len(selected)) else "fail"
    except Exception as error:
        report["error"] = scrub(type(error).__name__ + ": " + str(error), hidden)
        for kind in IMAGES:
            if names[kind] in attempted:
                try:
                    logs = docker("logs", names[kind], check=False)
                    (output / (kind + "-failure.log")).write_text(scrub(logs.stdout + logs.stderr, hidden), encoding="utf-8")
                except Exception:
                    pass
    finally:
        cleanup_errors = []
        for container in reversed(attempted):
            try:
                result = docker("inspect", container, check=False)
                if result.returncode:
                    if "No such" not in result.stderr:
                        raise RuntimeError("Cannot verify temporary container state")
                    continue
                labels = json.loads(result.stdout)[0]["Config"]["Labels"]
                if labels.get("codex-agent.integration.run") != stamp or labels.get("codex-agent.integration") != "local-source-race":
                    raise RuntimeError("Refusing to remove an unowned container")
                docker("rm", "-f", "-v", container)
            except Exception as error:
                cleanup_errors.append(type(error).__name__ + ": " + str(error))
        if network_attempted:
            try:
                result = docker("network", "inspect", name, check=False)
                if result.returncode == 0:
                    labels = json.loads(result.stdout)[0]["Labels"]
                    if labels.get("codex-agent.integration.run") != stamp or labels.get("codex-agent.integration") != "local-source-race":
                        raise RuntimeError("Refusing to remove an unowned network")
                    docker("network", "rm", name)
                elif "not found" not in result.stderr:
                    raise RuntimeError("Cannot verify temporary network state")
            except Exception as error:
                cleanup_errors.append(type(error).__name__ + ": " + str(error))
        for path in private_files:
            try:
                path.unlink(missing_ok=True)
            except OSError as error:
                cleanup_errors.append(type(error).__name__ + ": " + str(error))
        try:
            private.rmdir()
        except OSError as error:
            cleanup_errors.append(type(error).__name__ + ": " + str(error))
        report["cleanup_complete"] = not cleanup_errors
        if cleanup_errors:
            report.update(status="fail", cleanup_errors=[scrub(value, hidden) for value in cleanup_errors])
    report["go_source_changed_after_freeze"] = go_manifest(source_root)["sha256"] != manifest["sha256"]
    report["duration_seconds"] = round(time.monotonic() - started, 3)
    report["completed_at"] = datetime.now(timezone.utc).isoformat()
    report["artifacts"] = [{"path": str(path.relative_to(ROOT)), "sha256": hashlib.sha256(path.read_bytes()).hexdigest()}
                           for path in sorted(output.glob("*.log"))] + [{"path": str((output / "source-manifest.json").relative_to(ROOT)),
                                                                        "sha256": hashlib.sha256((output / "source-manifest.json").read_bytes()).hexdigest()}]
    (output / "report.json").write_text(json.dumps(report, indent=2) + "\n", encoding="utf-8")
    print(json.dumps({"status": report["status"], "passed_top_level_tests": report.get("passed_top_level_tests", 0),
                      "cleanup_complete": report["cleanup_complete"], "go_source_changed": report["go_source_changed_after_freeze"],
                      "report": str(output / "report.json")}), flush=True)
    return 0 if report["status"] == "pass" else 1


if __name__ == "__main__":
    raise SystemExit(main())
