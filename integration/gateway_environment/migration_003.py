"""Actual Database.Migrate validation in a random isolated PostgreSQL schema."""
from __future__ import annotations

import argparse
import datetime as dt
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
from urllib.parse import quote, urlsplit, urlunsplit


ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "tools"))
from project_layout import server_source, server_source_dir, server_package, package_overlay
DEFAULT_SNAPSHOT = ROOT / ".local/gateway-environment-linux-race/20260917T200319.786588Z"


def sha(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--snapshot", type=Path, default=DEFAULT_SNAPSHOT, help="Existing immutable source snapshot containing source/ and source-manifest.json")
    parser.add_argument("--environment-file", type=Path, default=ROOT / ".local/integration/environment.json")
    parser.add_argument("--keep-schema", action="store_true", help="Retain only the disabled synthetic schema and private fixture for separately authorized exact-binary rollback smoke")
    args = parser.parse_args()
    stamp = dt.datetime.now(dt.timezone.utc).strftime("%Y%m%dT%H%M%S.%fZ")
    output = ROOT / ".local/migration-003" / stamp
    output.mkdir(parents=True, exist_ok=False)
    snapshot = args.snapshot.resolve()
    source = snapshot / "source"
    manifest = json.loads((snapshot / "source-manifest.json").read_text(encoding="utf-8"))
    for name, expected in manifest["files"].items():
        if sha(source / name) != expected:
            raise RuntimeError("Immutable snapshot hash mismatch: " + name)
    if sha(server_source(source, "database.go")) != sha(server_source(ROOT, "database.go")):
        raise RuntimeError("Frozen Database.Migrate differs from current source; select a current coherent snapshot")
    config = json.loads(args.environment_file.read_text(encoding="utf-8"))
    if config.get("environment") != "isolated-synthetic-only" or config.get("remote_root") != "/opt/codex-agent-integration":
        raise RuntimeError("Refusing unmarked integration environment")
    owner_dsn = config["DATABASE_URL"]
    address = urlsplit(owner_dsn)
    if address.hostname != "127.0.0.1" or address.port != 25432 or address.path != "/codex_agent_integration" or address.username != "codex_agent_test":
        raise RuntimeError("Refusing database outside the fixed independent integration target")
    admin_dsn = urlunsplit((address.scheme, "postgres:" + quote(config["_pg_admin_password"], safe="") + "@127.0.0.1:25432", address.path, address.query, ""))
    test_source = Path(__file__).with_name("migration_003_test.go.txt")
    actual_overlay = package_overlay(test_source, output / "migration-package-test.go.txt", source)
    replacements = {str(server_source_dir(source) / "zz_management_cleanup_migration_overlay_test.go"): str(actual_overlay)}
    sql_hashes = {}
    sql_files = sorted((ROOT / "migrations").glob("*.sql"))
    if [path.name for path in sql_files] != ["000_core.sql", "001_accounts.sql", "002_metrics.sql", "003_management_cleanup.sql"]:
        raise RuntimeError("Migration set changed; review this bounded 000-003 test before proceeding")
    for path in sql_files:
        frozen = output / path.name
        frozen.write_bytes(path.read_bytes())
        sql_hashes[path.name] = sha(frozen)
        replacements[str(source / "migrations" / path.name)] = str(frozen)
    overlay = output / "overlay.json"
    overlay.write_text(json.dumps({"Replace": replacements}, indent=2), encoding="utf-8")
    environment = os.environ.copy()
    environment.update(CODEX_M003_OWNER_DSN=owner_dsn, CODEX_M003_ADMIN_DSN=admin_dsn, GOPROXY="off", GOSUMDB="off", GOWORK="off", CGO_ENABLED="0")
    if args.keep_schema:
        environment.update(CODEX_M003_KEEP_SCHEMA="1", CODEX_M003_FIXTURE_PATH=str(output / "fixture.json"))
    command = ["go", "test", "-mod=vendor", f"-overlay={overlay}", "-json", "-count=1", "-timeout=90s", "-run", "^TestManagementCleanupMigration$", server_package(source)]
    process = subprocess.run(command, cwd=source, env=environment, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True, encoding="utf-8", errors="replace", timeout=150)
    (output / "go-test.jsonl").write_text(process.stdout, encoding="utf-8")
    metric, tests, output_chunks = None, {}, []
    for line in process.stdout.splitlines():
        try:
            event = json.loads(line)
        except ValueError:
            continue
        output_chunks.append(event.get("Output", ""))
        if event.get("Test") and event.get("Action") in ("pass", "fail", "skip"):
            tests[event["Test"]] = event["Action"]
    for line in "".join(output_chunks).splitlines():
        marker = "MIGRATION_003_METRIC "
        if marker in line:
            metric = json.loads(line.split(marker, 1)[1])
    unchanged = all(sha(path) == sql_hashes[path.name] for path in sql_files)
    checks = metric.get("checks", {}) if metric else {}
    release_checks = {"migration_apply": checks.get("ledger_matches_all_four_sql_sha256", False),
        "idempotent": checks.get("concurrent_idempotent_actual_migrator", False),
        "checksum_guard": checks.get("checksum_drift_rejected", False),
        "credential_decryption": checks.get("credential_decryption_before_after_restart", False),
        "data_preserved": checks.get("legacy_account_logs_totals_unchanged", False),
        "runtime_permissions": checks.get("runtime_crud_after_explicit_grant", False) and checks.get("readonly_and_runtime_cannot_ddl", False),
        "owner_verified": checks.get("table_index_owner_and_no_fk", False),
        "failure_atomicity": checks.get("failed_003_rollback_keeps_prior_three_commits", False)}
    target_checksums = {"migrations/" + name: value for name, value in sql_hashes.items()}
    fixture_path = output / "fixture.json"
    if fixture_path.exists() and process.returncode == 0:
        fixture = json.loads(fixture_path.read_text(encoding="utf-8"))
        fixture.update(REDIS_URL=config["REDIS_URL"], REDIS_KEY_PREFIX="codex-agent:integration:m003:" + stamp + ":",
                       snapshot_sha256=manifest["snapshot_sha256"], target_checksums=target_checksums)
        fixture_path.write_text(json.dumps(fixture, indent=2) + "\n", encoding="utf-8")
    report = {"schema": "codex-agent-pg-additive-migration-v1", "schema_version": 1, "scope": "migration-focused", "synthetic_only": True,
              "source_sha256": manifest["snapshot_sha256"], "migration_runner_sha256": sha(server_source(source, "database.go")),
              "baseline_checksums": {name:value for name,value in target_checksums.items() if not name.endswith("003_management_cleanup.sql")},
              "target_checksums": target_checksums, "checks": release_checks,
              "timestamp_utc": stamp, "status": "pass" if process.returncode == 0 and metric and unchanged and all(release_checks.values()) else "fail",
              "tests": tests, "exit_code": process.returncode, "results": metric, "sql_sha256": sql_hashes,
              "snapshot_sha256": manifest["snapshot_sha256"], "database_go_sha256": sha(server_source(source, "database.go")),
              "test_sha256": sha(test_source), "runner_sha256": sha(Path(__file__)), "command": command,
              "environment": "codex-agent-integration-postgres / codex_agent_integration / random schema",
              "schema_role_prefix": "codex_agent_integration_", "production_migration_executed": False,
              "isolated_migration_executed": process.returncode == 0 and bool(metric), "retained_disabled_synthetic_schema": args.keep_schema and fixture_path.exists(),
              "current_sql_unchanged_during_run": unchanged,
              "limits": ["Fresh compiled test process restart, not production service or PostgreSQL container restart.",
                         "Legacy account/log/aggregate runtime reads use the recorded immutable source snapshot; exact current production binary smoke remains a release gate.",
                         "Does not alter deploy/upgrade_remote.py; its 000-002 schema-preserving gate still rejects this four-migration release."]}
    (output / "report.json").write_text(json.dumps(report, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    print(json.dumps({"status": report["status"], "report": str(output / "report.json"), "tests": tests}, ensure_ascii=False, indent=2))
    if report["status"] != "pass":
        print(process.stdout[-14000:])
    return 0 if report["status"] == "pass" else 1


if __name__ == "__main__":
    raise SystemExit(main())
