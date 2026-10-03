"""Exact retained binary compatibility with 003 using independent local Docker."""
from __future__ import annotations

import argparse
import base64
import datetime as dt
import hashlib
import http.cookiejar
import json
import os
from pathlib import Path
import secrets
import socket
import subprocess
import tarfile
import time
import urllib.error
import urllib.parse
import urllib.request

ROOT = Path(__file__).resolve().parents[2]
ARCHIVE = ROOT / ".local/logs-improvement/release-source/.local/upgrades/4d4c4dfd91de306b6d1f79bfba232df49c85d3a1dfdda06bbf199538a7322e38/release.tar.gz"
PREVIOUS_SHA = "a9bf68d1899ec1d58381c788b84cc63123c88cb8196d1acb0b4faafa1b65831d"
SNAPSHOT = ROOT / ".local/management-parity/linux-race-20260917T214152-2d29dc"


def require(value, message):
    if not value:
        raise AssertionError(message)


def sha(path):
    return hashlib.sha256(Path(path).read_bytes()).hexdigest()


def docker(*args, stdin=None, timeout=90):
    result = subprocess.run(["docker", *args], input=stdin, capture_output=True, text=True, encoding="utf-8", errors="replace", timeout=timeout)
    require(result.returncode == 0, "Docker operation failed: " + args[0])
    return result.stdout.strip()


def private_json(path, data):
    path.write_text(json.dumps(data, indent=2) + "\n", encoding="utf-8")
    path.chmod(0o600)


def envfile(path, values):
    path.write_text("".join(key + "=" + value + "\n" for key, value in values.items()), encoding="utf-8")
    path.chmod(0o600)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--candidate", type=Path, default=SNAPSHOT / "codex-agent-linux-amd64")
    parser.add_argument("--source", type=Path, default=SNAPSHOT / "local-source")
    parser.add_argument("--archive", type=Path, default=ARCHIVE)
    args = parser.parse_args()
    with socket.socket() as probe:
        probe.bind(("127.0.0.1", 28886))
    run_id = secrets.token_hex(5)
    stamp = dt.datetime.now(dt.timezone.utc).strftime("%Y%m%dT%H%M%S") + "-" + run_id
    directory = ROOT / ".local/migration-003-local" / stamp
    directory.mkdir(parents=True)
    with tarfile.open(args.archive, "r:gz") as archive:
        member = archive.getmember("codex-agent")
        require(member.isfile(), "Expected exact retained binary archive member")
        (directory / "old-binary").write_bytes(archive.extractfile(member).read())
    require(sha(directory / "old-binary") == PREVIOUS_SHA, "Retained binary hash mismatch")
    (directory / "candidate-binary").write_bytes(args.candidate.read_bytes())
    for name in ("old-binary", "candidate-binary"):
        (directory / name).chmod(0o755)
    files = sorted((args.source / "migrations").glob("*.sql"))
    require([p.name for p in files] == ["000_core.sql", "001_accounts.sql", "002_metrics.sql", "003_management_cleanup.sql"], "Expected exactly 000-003 candidate migrations")
    checksums = {"migrations/" + p.name: sha(p) for p in files}
    label = "codex-agent.integration.m003=" + run_id
    network, pg, redis, old = ["codex-m003-" + run_id + suffix for suffix in ("-net", "-pg", "-redis", "-old")]
    database, owner, runtime, schema = [prefix + run_id for prefix in ("m003_", "m003_owner_", "m003_runtime_", "m003_schema_")]
    pg_password, rt_password, redis_password, admin_key = [secrets.token_hex(24) for _ in range(4)]
    prefix = "codex-agent:integration:m003:" + run_id + ":"
    resources = {"network": network, "postgres": pg, "redis": redis, "old_app": old, "label": label}
    report = {"schema": "codex-agent-pg-rollback-compatibility-v1", "timestamp_utc": stamp, "status": "fail", "synthetic_only": True, "previous_binary_sha256": PREVIOUS_SHA, "candidate_binary_sha256": sha(args.candidate), "target_checksums": checksums, "production_touched": False, "gateway_requests": 0, "resources": resources, "checks": {k: False for k in ("old_binary_start", "read_write", "credential_decryption", "data_preserved", "process_restart", "cleanup_targets_preserved")}, "retained_for_parent": True}
    pg_image = docker("image", "inspect", "postgres:18-alpine", "--format", "{{.Id}}")
    redis_image = docker("image", "inspect", "redis:8-alpine", "--format", "{{.Id}}")
    report["images"] = {"postgres_runtime": pg_image, "redis": redis_image}
    private_json(directory / "report.json", report)
    initialized = False

    def sql(statement, readonly=False):
        options = "-c statement_timeout=10000" + (" -c default_transaction_read_only=on" if readonly else "")
        return docker("exec", "-i", "-e", "PGOPTIONS=" + options, pg, "psql", "-X", "-q", "-U", owner, "-d", database, "-At", "-v", "ON_ERROR_STOP=1", stdin='SET search_path TO "' + schema + '";\n' + statement)

    def snapshot():
        return json.loads(sql("SELECT json_build_object('accounts',(SELECT count(*) FROM accounts),'enabled',(SELECT count(*) FROM accounts WHERE enabled),'logs',(SELECT count(*) FROM request_logs),'events',(SELECT count(*) FROM usage_events),'targets',(SELECT json_agg(t ORDER BY preview_id,log_id) FROM management_cleanup_targets t),'ledger',(SELECT json_object_agg(name,checksum) FROM schema_migrations));", True))

    try:
        report["stage"] = "create_isolated_dependencies"
        docker("network", "create", "--label", label, network)
        pg_env = directory / "postgres.env"
        envfile(pg_env, {"POSTGRES_USER": owner, "POSTGRES_PASSWORD": pg_password, "POSTGRES_DB": database})
        docker("run", "-d", "--name", pg, "--label", label, "--network", network, "--network-alias", "pg", "--memory", "512m", "--cpus", "1", "--pids-limit", "128", "--env-file", str(pg_env), "--publish", "127.0.0.1::5432", "--mount", "type=volume,source=" + pg + "-data,target=/var/lib/postgresql", pg_image, "postgres", "-c", "max_connections=20", "-c", "shared_buffers=64MB", "-c", "statement_timeout=10000")
        redis_config = directory / "redis.conf"
        redis_config.write_text("bind 0.0.0.0\nprotected-mode yes\nport 6379\nsave \"\"\nappendonly no\nmaxmemory 64mb\nmaxmemory-policy noeviction\nuser default off\nuser " + runtime + " on >" + redis_password + " ~" + prefix + "* &" + prefix + "* +@all -@dangerous\n")
        docker("run", "-d", "--name", redis, "--label", label, "--network", network, "--network-alias", "redis", "--memory", "96m", "--cpus", "0.25", "--pids-limit", "64", "--publish", "127.0.0.1::6379", "--mount", "type=bind,source=" + str(redis_config) + ",target=/usr/local/etc/redis.conf,readonly", redis_image, "redis-server", "/usr/local/etc/redis.conf")
        for _ in range(80):
            if subprocess.run(["docker", "exec", pg, "pg_isready", "-U", owner, "-d", database], capture_output=True).returncode == 0:
                break
            time.sleep(0.25)
        else:
            raise AssertionError("Independent PostgreSQL not ready")
        sql('CREATE SCHEMA "' + schema + '"; CREATE ROLE "' + runtime + "\" LOGIN PASSWORD '" + rt_password + "' NOSUPERUSER NOCREATEDB NOCREATEROLE CONNECTION LIMIT 4; REVOKE CONNECT ON DATABASE \"" + database + '" FROM PUBLIC; GRANT CONNECT ON DATABASE "' + database + '" TO "' + runtime + '"; REVOKE ALL ON SCHEMA public FROM PUBLIC; GRANT USAGE ON SCHEMA "' + schema + '" TO "' + runtime + '";')
        query = "?sslmode=disable&search_path=" + schema
        migration_env = {"DATABASE_URL": f"postgres://{owner}:{pg_password}@pg:5432/{database}" + query, "CREDENTIAL_ENCRYPTION_KEY": base64.b64encode(secrets.token_bytes(32)).decode(), "CREDENTIAL_ENCRYPTION_KEY_ID": "synthetic-m003-" + run_id, "REDIS_URL": f"redis://{runtime}:{redis_password}@redis:6379/0", "REDIS_KEY_PREFIX": prefix, "ADMIN_KEY": admin_key, "ADMIN_SESSION_TTL": "900", "ADMIN_COOKIE_SECURE": "false", "PG_MAX_CONNS": "2", "STATS_TIMEZONE": "UTC"}
        app_env = {**migration_env, "DATABASE_URL": f"postgres://{runtime}:{rt_password}@pg:5432/{database}" + query}
        envfile(directory / "migration.env", migration_env)
        envfile(directory / "runtime.env", app_env)
        common = ["--network", network, "--label", label, "--read-only", "--memory", "256m", "--cpus", "0.5", "--pids-limit", "64", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--user", "65534:65534", "--tmpfs", "/tmp:rw,nosuid,size=16m", "--mount", "type=bind,source=" + str(directory) + ",target=/work"]

        def cli(binary, environment, *args):
            return docker("run", "--rm", *common, "--env-file", str(directory / environment), "--entrypoint", "/work/" + binary, pg_image, *args, timeout=120)

        report["stage"] = "candidate_migrate_and_seed"
        cli("candidate-binary", "migration.env", "migrate")
        require(json.loads(sql("SELECT json_object_agg(name,checksum) FROM schema_migrations;", True)) == checksums, "Candidate embedded migrations differ from exact supplied source")
        sql('GRANT SELECT,INSERT,UPDATE,DELETE ON ALL TABLES IN SCHEMA "' + schema + '" TO "' + runtime + '"; GRANT USAGE,SELECT ON ALL SEQUENCES IN SCHEMA "' + schema + '" TO "' + runtime + '";')
        legacy = directory / "legacy"
        legacy.mkdir()
        now = int(time.time())
        account = {"name": "synthetic-m003", "access_token": "synthetic-m003-access", "refresh_token": "refresh-synthetic-m003-access", "account_id": "synthetic-m003-workspace", "enabled": False, "expires_at": now + 86400, "created_at": now - 60, "concurrency": 1, "weight": 1, "window_days": 30}
        for name, value in {"accounts.json": {"active": "", "accounts": [account]}, "groups.json": {"groups": []}, "proxies.json": {"proxies": []}, "keys.json": {"keys": []}, "settings.json": {"admin_api": False}}.items():
            private_json(legacy / name, value)
        (legacy / "client-key").write_text(admin_key)
        (legacy / "logs.jsonl").write_text(json.dumps({"time": now - 1, "account": "synthetic-m003", "status": "ok", "http_code": 200, "model": "synthetic-no-inference", "input_tokens": 11, "output_tokens": 6, "cached_tokens": 4, "latency_ms": 20}) + "\n")
        cli("candidate-binary", "migration.env", "import-legacy", "--source", "/work/legacy")
        sql("INSERT INTO management_cleanup_targets(preview_id,log_id) VALUES('synthetic-a',900001),('synthetic-b',900001);")
        before = snapshot()
        require(before["accounts"] == before["logs"] == before["events"] == 1 and before["enabled"] == 0 and len(before["targets"]) == 2, "Wrong isolated fixture shape")
        pg_port = json.loads(docker("inspect", pg))[0]["NetworkSettings"]["Ports"]["5432/tcp"][0]["HostPort"]
        redis_port = json.loads(docker("inspect", redis))[0]["NetworkSettings"]["Ports"]["6379/tcp"][0]["HostPort"]
        fixture = {"schema": schema, "synthetic_only": True, "all_accounts_disabled": True, "resources": resources, "container_env": app_env, "migration_env": migration_env, "container_env_file": str(directory / "runtime.env"), "migration_env_file": str(directory / "migration.env"), "DATABASE_URL": app_env["DATABASE_URL"].replace("@pg:5432", "@127.0.0.1:" + pg_port), "REDIS_URL": app_env["REDIS_URL"].replace("@redis:6379", "@127.0.0.1:" + redis_port), "REDIS_KEY_PREFIX": prefix, "CREDENTIAL_ENCRYPTION_KEY": app_env["CREDENTIAL_ENCRYPTION_KEY"], "CREDENTIAL_ENCRYPTION_KEY_ID": app_env["CREDENTIAL_ENCRYPTION_KEY_ID"], "ADMIN_KEY": admin_key, "target_checksums": checksums, "directory": str(directory), "old_process_stopped": False}
        private_json(directory / "environment.json", fixture)
        report["stage"] = "old_binary_start"
        docker("run", "-d", "--name", old, *common, "--env-file", str(directory / "runtime.env"), "--publish", "127.0.0.1:28886:28886", "--entrypoint", "/work/old-binary", pg_image, "--listen", "0.0.0.0:28886", "--web", "/nonexistent")
        initialized = True
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))

        def api(path, method="GET", body=None, expected=200):
            require(path.startswith(("/api/", "/health/")) and "refresh" not in path, "Out-of-scope HTTP request")
            request = urllib.request.Request("http://127.0.0.1:28886" + path, method=method, data=json.dumps(body).encode() if body is not None else None, headers={"Content-Type": "application/json", "Origin": "http://127.0.0.1:28886"})
            try:
                response = opener.open(request, timeout=3)
            except urllib.error.HTTPError as error:
                response = error
            require(response.status == expected, "HTTP mismatch for " + path)
            return json.loads(response.read())

        def ready():
            for _ in range(80):
                require(docker("inspect", "--format", "{{.State.Running}}", old) == "true", "Exact old binary exited before readiness")
                try:
                    api("/health/ready")
                    return
                except (AssertionError, OSError, urllib.error.URLError):
                    time.sleep(0.25)
            raise AssertionError("Exact old binary readiness timeout")

        ready()
        require(docker("exec", old, "sha256sum", "/proc/1/exe").split()[0] == PREVIOUS_SHA, "Runtime-loaded binary mismatch")
        report["checks"]["old_binary_start"] = True
        report["runtime_binary_sha256"] = PREVIOUS_SHA
        api("/api/accounts", expected=401)
        api("/api/session", "POST", {"admin_key": admin_key})
        current = api("/api/accounts?limit=10")["accounts"][0]
        require(current["name"] == "synthetic-m003" and current["enabled"] is False, "Old account read mismatch")
        path = "/api/accounts/" + current["id"]
        original_notes = current.get("notes", "")
        api(path, "PUT", {"version": current["version"], "notes": "synthetic-old-binary-write"})
        changed = api(path)["account"]
        require(changed["version"] > current["version"] and changed["notes"] == "synthetic-old-binary-write", "Old metadata write failed")
        require(api("/api/overview")["cards"]["tokens_total"] == 17 and len(api("/api/logs?page_size=10")["items"]) == 1, "Old statistics mismatch")
        report["checks"]["read_write"] = True
        started = docker("inspect", "--format", "{{.State.StartedAt}}", old)
        docker("stop", "--time", "90", old, timeout=100)
        require(docker("inspect", "--format", "{{.State.ExitCode}}", old) == "0", "Old graceful stop failed")
        docker("start", old)
        ready()
        require(docker("inspect", "--format", "{{.State.StartedAt}}", old) != started, "Old process not restarted")
        require(api(path)["account"]["notes"] == "synthetic-old-binary-write" and api("/api/overview")["cards"]["tokens_total"] == 17, "Restart lost data/session")
        require(docker("exec", old, "sha256sum", "/proc/1/exe").split()[0] == PREVIOUS_SHA, "Restart loaded unexpected binary")
        report["checks"]["process_restart"] = True
        changed = api(path)["account"]
        api(path, "PUT", {"version": changed["version"], "notes": original_notes})
        api("/api/session", "DELETE")
        docker("stop", "--time", "90", old, timeout=100)
        require(docker("inspect", "--format", "{{.State.ExitCode}}", old) == "0", "Final old graceful stop failed")
        report["stage"] = "old_binary_export_decryption"
        cli("old-binary", "runtime.env", "export-legacy", "--target", "/work/exported")
        exported = json.loads((directory / "exported/accounts.json").read_text())
        rows = exported["accounts"] if isinstance(exported, dict) else exported
        require(len(rows) == 1 and rows[0]["access_token"] == account["access_token"] and rows[0]["refresh_token"] == account["refresh_token"] and not rows[0].get("enabled", False), "Old decrypted credentials or disabled state differ")
        report["checks"]["credential_decryption"] = True
        require(snapshot() == before, "Old binary changed migrated ledger, targets or account/log/event counts")
        report["checks"]["data_preserved"] = report["checks"]["cleanup_targets_preserved"] = True
        report["counts"] = {"accounts": 1, "logs": 1, "events": 1, "tokens": 17, "cleanup_targets": 2}
        report["status"] = "pass"
        report["stage"] = "complete"
    except Exception as error:
        report["error"] = str(error) if isinstance(error, AssertionError) else type(error).__name__
    finally:
        stopped = not initialized
        if initialized:
            try:
                docker("stop", "--time", "90", old, timeout=100)
                stopped = docker("inspect", "--format", "{{.State.Running}}", old) == "false"
            except Exception:
                report["status"] = "fail"
                report["cleanup_error"] = "Could not confirm own old-binary container stopped"
        report["isolated_process_stopped"] = stopped
        if (directory / "environment.json").exists():
            fixture["old_process_stopped"] = stopped
            private_json(directory / "environment.json", fixture)
        report["environment_file"] = str(directory / "environment.json")
        report["cleanup_commands"] = ["docker rm " + old, "docker rm -f " + pg + " " + redis, "docker volume rm " + pg + "-data", "docker network rm " + network]
        private_json(directory / "report.json", report)
    print(json.dumps({"status": report["status"], "report": str(directory / "report.json"), "stage": report.get("stage"), "error": report.get("error"), "checks": report["checks"], "isolated_process_stopped": report["isolated_process_stopped"]}, indent=2))
    return 0 if report["status"] == "pass" else 1


if __name__ == "__main__":
    raise SystemExit(main())
