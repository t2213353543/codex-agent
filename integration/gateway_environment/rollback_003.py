"""Run the exact retained 000-002 binary against a synthetic schema with 003.

python integration/gateway_environment/rollback_003.py --fixture PRIVATE_FIXTURE

No production configuration, gateway inference, credential refresh, or production
process lifecycle is used. The schema is created/removed by migration_003.py's
owner; this tool only updates and restores one synthetic account's notes.
"""
from __future__ import annotations

import argparse
import datetime as dt
import hashlib
import http.cookiejar
import json
import os
from pathlib import Path
import re
import secrets
import shlex
import shutil
import socket
import subprocess
import sys
import time
import urllib.error
import urllib.request
from urllib.parse import parse_qs, urlsplit

ROOT = Path(__file__).resolve().parents[2]
REMOTE = "/opt/codex-agent-integration/migration-003"
PREVIOUS_BINARY = "/opt/codex-agent/releases/4d4c4dfd91de306b6d1f79bfba232df49c85d3a1dfdda06bbf199538a7322e38/codex-agent"
PREVIOUS_SHA = "a9bf68d1899ec1d58381c788b84cc63123c88cb8196d1acb0b4faafa1b65831d"
PORT = 28886
MIGRATIONS = ["000_core.sql", "001_accounts.sql", "002_metrics.sql", "003_management_cleanup.sql"]


class CheckFailure(RuntimeError):
    pass


def require(value, message):
    if not value:
        raise CheckFailure(message)


def sha(path):
    return hashlib.sha256(Path(path).read_bytes()).hexdigest()


def validate_fixture(fixture):
    require(fixture.get("synthetic_only") is True and fixture.get("all_accounts_disabled") is True, "Fixture must be explicitly synthetic and disabled")
    schema = fixture.get("schema", "")
    require(re.fullmatch(r"codex_agent_integration_m003_[0-9a-f]{12}", schema), "Unexpected isolated schema")
    dsn = urlsplit(fixture["DATABASE_URL"])
    require(dsn.hostname == "127.0.0.1" and dsn.port == 25432 and dsn.username == "codex_agent_test" and dsn.path == "/codex_agent_integration", "Database is outside integration target")
    require(parse_qs(dsn.query).get("search_path") == [schema], "Fixture search_path differs from isolated schema")
    redis = urlsplit(fixture["REDIS_URL"])
    require(redis.hostname in ("127.0.0.1", "localhost") and redis.port == 26379, "Redis is outside integration target")
    require(fixture["REDIS_KEY_PREFIX"].startswith("codex-agent:integration:m003:"), "Redis namespace is outside migration test")
    require(fixture.get("CREDENTIAL_ENCRYPTION_KEY_ID") == "synthetic-m003", "Unexpected synthetic key identity")
    checksums = fixture.get("target_checksums", {"migrations/" + key: value for key, value in fixture.get("sql_sha256", {}).items()})
    require(sorted(checksums) == ["migrations/" + key for key in MIGRATIONS] and all(re.fullmatch(r"[0-9a-f]{64}", v) for v in checksums.values()), "Expected exact four migration checksums")
    return checksums


def command(args, timeout=25):
    result = subprocess.run(args, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, timeout=timeout)
    # Never include command text, environments, database errors, or service logs.
    require(result.returncode == 0, "Isolated command failed: " + Path(args[0]).name)
    return result.stdout.strip()


def query(schema, statement):
    output = command(["docker", "exec", "-e", "PGOPTIONS=-c statement_timeout=5000 -c default_transaction_read_only=on", "codex-agent-integration-postgres", "psql", "-X", "-q", "-U", "codex_agent_test", "-d", "codex_agent_integration", "-At", "-v", "ON_ERROR_STOP=1", "-c", 'SET search_path TO "' + schema + '"; ' + statement])
    return json.loads(output)


def snapshot(schema):
    return query(schema, "SELECT json_build_object('accounts',(SELECT count(*) FROM accounts),'enabled',(SELECT count(*) FROM accounts WHERE enabled),'logs',(SELECT count(*) FROM request_logs),'events',(SELECT count(*) FROM usage_events),'targets',(SELECT json_agg(t ORDER BY preview_id,log_id) FROM management_cleanup_targets t),'ledger',(SELECT json_object_agg(name,checksum) FROM schema_migrations))")


def remote_run(fixture_path):
    fixture_path = fixture_path.resolve()
    directory = fixture_path.parent
    require(directory.parent == Path(REMOTE) and re.fullmatch(r"[0-9]{8}T[0-9]{6}\.[0-9]{6}Z", directory.name), "Unexpected remote run directory")
    fixture = json.loads(fixture_path.read_text())
    checksums = validate_fixture(fixture)
    schema = fixture["schema"]
    container = "codex-agent-integration-m003-old-" + schema.rsplit("_", 1)[1]
    report = {"schema": "codex-agent-pg-rollback-compatibility-v1", "schema_version": 1, "timestamp_utc": directory.name, "status": "fail", "synthetic_only": True,
              "previous_binary_sha256": PREVIOUS_SHA, "target_checksums": checksums,
              "checks": {key: False for key in ("old_binary_start", "read_write", "credential_decryption", "data_preserved", "process_restart", "cleanup_targets_preserved")},
              "production_touched": False, "production_migration_executed": False, "production_process_modified": False, "gateway_requests": 0,
              "schema_cleanup_owner": "migration_003.py caller", "resources": {"cpu": 0.5, "memory_bytes": 268435456, "pids_limit": 64, "pg_max_conns": 2},
              "listen": "127.0.0.1:28886", "limits": ["Synthetic schema compatibility only; does not prove production migration or real credential refresh."]}
    running = False
    initialized = False
    try:
        report["stage"] = "verify_retained_binary"
        require(sha(PREVIOUS_BINARY) == PREVIOUS_SHA, "Retained binary hash does not match authorized previous release")
        runtime = directory / "runtime"
        runtime.mkdir(mode=0o755)
        shutil.copyfile(PREVIOUS_BINARY, runtime / "codex-agent")
        (runtime / "codex-agent").chmod(0o755)
        require(sha(runtime / "codex-agent") == PREVIOUS_SHA, "Copied binary hash mismatch")
        report["copied_binary_sha256"] = sha(runtime / "codex-agent")
        with socket.socket() as probe:
            probe.bind(("127.0.0.1", PORT))
        report["stage"] = "inspect_synthetic_schema"
        before = snapshot(schema)
        require(before["accounts"] == 1 and before["enabled"] == 0 and before["logs"] == 1 and before["events"] == 1 and len(before["targets"]) == 2, "Synthetic fixture shape differs")
        require(before["ledger"] == checksums, "Actual migrated ledger differs from target checksums")
        admin_file = Path(REMOTE) / (schema + ".admin-key")
        if not admin_file.exists():
            with admin_file.open("x") as handle:
                handle.write(secrets.token_hex(32))
            admin_file.chmod(0o600)
        admin_key = admin_file.read_text()
        environment = {key: fixture[key] for key in ("DATABASE_URL", "REDIS_URL", "REDIS_KEY_PREFIX", "CREDENTIAL_ENCRYPTION_KEY", "CREDENTIAL_ENCRYPTION_KEY_ID")}
        environment.update(ADMIN_KEY=admin_key, ADMIN_SESSION_TTL="300", ADMIN_COOKIE_SECURE="false", PG_MAX_CONNS="2", STATS_TIMEZONE="UTC")
        require(all("\n" not in value and "\r" not in value for value in environment.values()), "Invalid fixture environment encoding")
        env_file = directory / "isolated.env"
        env_file.write_text("".join(key + "=" + value + "\n" for key, value in environment.items()))
        env_file.chmod(0o600)
        image = command(["docker", "image", "inspect", "postgres:18-alpine", "--format", "{{.Id}}"])
        require(re.fullmatch(r"sha256:[0-9a-f]{64}", image), "Existing runtime image identity unavailable")
        report["runtime_image"] = image
        common = ["docker", "run", "--read-only", "--network", "host", "--cpus", "0.5", "--memory", "256m", "--pids-limit", "64", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--user", "65534:65534", "--tmpfs", "/tmp:rw,noexec,nosuid,size=16m", "--env-file", str(env_file), "-v", str(runtime) + ":/work:ro", "--entrypoint", "/work/codex-agent"]
        report["stage"] = "start_old_binary"
        command(common + ["-d", "--name", container, "--label", "codex-agent.test=migration-003-old-binary", "--log-driver", "json-file", "--log-opt", "max-size=1m", "--log-opt", "max-file=1", image, "--listen", f"127.0.0.1:{PORT}", "--web", "/nonexistent"])
        running = initialized = True
        jar = http.cookiejar.CookieJar()
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), urllib.request.HTTPCookieProcessor(jar))

        def api(path, method="GET", body=None, expected=200):
            require(path.startswith(("/api/", "/health/")) and "refresh" not in path, "Non-management request refused")
            payload = None if body is None else json.dumps(body).encode()
            request = urllib.request.Request(f"http://127.0.0.1:{PORT}" + path, data=payload, method=method, headers={"Content-Type": "application/json", "Origin": f"http://127.0.0.1:{PORT}"})
            try:
                response = opener.open(request, timeout=5)
            except urllib.error.HTTPError as error:
                response = error
            require(response.status == expected, "Management HTTP status mismatch: " + path)
            raw = response.read()
            return json.loads(raw) if raw else {}

        def ready():
            for _ in range(60):
                try:
                    api("/health/ready")
                    return
                except (CheckFailure, OSError, urllib.error.URLError):
                    require(command(["docker", "inspect", "--format", "{{.State.Running}}", container]) == "true", "Old binary exited before readiness")
                    time.sleep(0.25)
            raise CheckFailure("Old binary readiness timeout")

        ready()
        pid = command(["docker", "inspect", "--format", "{{.State.Pid}}", container])
        require(sha(f"/proc/{pid}/exe") == PREVIOUS_SHA, "Runtime-loaded binary differs from copied previous release")
        report["runtime_binary_sha256"] = PREVIOUS_SHA
        report["checks"]["old_binary_start"] = True
        report["stage"] = "management_read_write"
        api("/api/accounts", expected=401)
        require(api("/api/session", "POST", {"admin_key": admin_key}).get("authenticated") is True, "Synthetic session login failed")
        accounts = api("/api/accounts?limit=10")["accounts"]
        require(len(accounts) == 1 and accounts[0]["name"] == "synthetic-m003" and accounts[0]["enabled"] is False, "Old binary account metadata mismatch")
        account = accounts[0]
        original_notes = account.get("notes", "")
        changed_notes = "synthetic-m003-old-binary-write-" + directory.name
        path = "/api/accounts/" + urllib.parse.quote(account["id"], safe="")
        api(path, "PUT", {"version": account["version"], "notes": changed_notes})
        updated = api(path)["account"]
        require(updated["notes"] == changed_notes and updated["version"] > account["version"] and updated["enabled"] is False, "Old binary versioned metadata write failed")
        require(len(api("/api/logs?page_size=10")["items"]) == 1 and api("/api/overview")["cards"]["tokens_total"] == 17, "Old binary logs or totals differ")
        report["checks"]["read_write"] = True
        report["stage"] = "process_restart"
        command(["docker", "stop", "--time", "90", container], timeout=100)
        running = False
        require(command(["docker", "inspect", "--format", "{{.State.ExitCode}}", container]) == "0", "Old binary graceful shutdown failed")
        command(["docker", "start", container])
        running = True
        ready()
        new_pid = command(["docker", "inspect", "--format", "{{.State.Pid}}", container])
        require(new_pid != pid and sha(f"/proc/{new_pid}/exe") == PREVIOUS_SHA, "Restart did not load exact previous binary in a new process")
        require(api(path)["account"]["notes"] == changed_notes, "Metadata or Redis-backed session failed after restart")
        require(api("/api/overview")["cards"]["tokens_total"] == 17 and len(api("/api/logs?page_size=10")["items"]) == 1, "Restart lost logs or totals")
        report["checks"]["process_restart"] = True
        report["stage"] = "restore_and_stop"
        updated = api(path)["account"]
        api(path, "PUT", {"version": updated["version"], "notes": original_notes})
        api("/api/session", "DELETE")
        command(["docker", "stop", "--time", "90", container], timeout=100)
        running = False
        require(command(["docker", "inspect", "--format", "{{.State.ExitCode}}", container]) == "0", "Final old binary graceful shutdown failed")
        report["stage"] = "credential_decryption_export"
        export_root = directory / "private-export"
        export_root.mkdir(mode=0o700)
        os.chown(export_root, 65534, 65534)
        command(common + ["--rm", "-v", str(export_root) + ":/exports:rw", image, "export-legacy", "--target", "/exports/legacy"], timeout=90)
        exported = json.loads((export_root / "legacy/accounts.json").read_text())
        rows = exported.get("accounts", []) if isinstance(exported, dict) else exported
        require(len(rows) == 1 and rows[0].get("access_token") == "synthetic-m003-access" and rows[0].get("refresh_token") == "refresh-synthetic-m003-access", "Old binary synthetic credential decryption differs")
        require(rows[0].get("enabled", False) is False and rows[0].get("notes", "") == original_notes, "Exported synthetic metadata not restored")
        report["checks"]["credential_decryption"] = True
        report["stage"] = "verify_schema_preserved"
        after = snapshot(schema)
        require(before == after, "Old binary changed legacy row counts, migration ledger, or 003 target contents")
        report["checks"]["data_preserved"] = True
        report["checks"]["cleanup_targets_preserved"] = True
        report["observed_counts"] = {"accounts": 1, "enabled_accounts": 0, "logs": 1, "usage_events": 1, "total_tokens": 17, "cleanup_targets": 2}
        report["status"] = "pass"
        report["stage"] = "complete"
    except Exception as error:
        report["error"] = str(error) if isinstance(error, CheckFailure) else type(error).__name__
    finally:
        if running:
            try:
                command(["docker", "stop", "--time", "90", container], timeout=100)
                running = False
            except Exception:
                report["status"] = "fail"
                report["cleanup_error"] = "Could not confirm isolated old binary stopped"
        if initialized and not running:
            try:
                command(["docker", "rm", container])
            except Exception:
                report["cleanup_error"] = "Stopped isolated container retained"
        report["isolated_process_stopped"] = not running
        (directory / "report.json").write_text(json.dumps(report, indent=2) + "\n")
        os.chmod(directory / "report.json", 0o600)
    print(json.dumps({"status": report["status"], "checks": report["checks"]}))
    return 0 if report["status"] == "pass" else 1


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--fixture", type=Path, required=True)
    parser.add_argument("--remote", action="store_true", help=argparse.SUPPRESS)
    args = parser.parse_args()
    if args.remote:
        return remote_run(args.fixture)
    fixture = json.loads(args.fixture.read_text(encoding="utf-8"))
    validate_fixture(fixture)
    stamp = dt.datetime.now(dt.timezone.utc).strftime("%Y%m%dT%H%M%S.%fZ")
    output = ROOT / ".local/migration-003-rollback" / stamp
    output.mkdir(parents=True)
    sys.path.insert(0, str(ROOT / "tools"))
    import integration_env
    client = integration_env.connect()
    directory = REMOTE + "/" + stamp
    try:
        integration_env.run(client, "test -f /opt/codex-agent-integration/.codex-agent-integration\numask 077\nmkdir -p " + shlex.quote(REMOTE) + "\nmkdir " + shlex.quote(directory))
        integration_env.write_remote(client, directory + "/runner.py", Path(__file__).read_text(encoding="utf-8"))
        integration_env.write_remote(client, directory + "/fixture.json", json.dumps(fixture))
        # Retrieve report even when a compatibility check fails. Logs and exports
        # remain private on the server; only the sanitized report is downloaded.
        _, stdout, stderr = client.exec_command("python3 " + shlex.quote(directory + "/runner.py") + " --remote --fixture " + shlex.quote(directory + "/fixture.json"), timeout=420)
        stdout.read()
        stderr.read()
        exit_code = stdout.channel.recv_exit_status()
        with client.open_sftp() as sftp:
            sftp.get(directory + "/report.json", str(output / "report.json"))
        report = json.loads((output / "report.json").read_text())
        report.update(runner_sha256=sha(__file__), fixture_sql_checksums=fixture.get("sql_sha256"), source_snapshot_sha256=fixture.get("snapshot_sha256"), remote_evidence_directory=directory)
        (output / "report.json").write_text(json.dumps(report, indent=2) + "\n")
        print(json.dumps({"status": report["status"], "report": str(output / "report.json"), "checks": report["checks"]}, indent=2))
        return 0 if exit_code == 0 and report["status"] == "pass" else 1
    finally:
        client.close()


if __name__ == "__main__":
    raise SystemExit(main())
