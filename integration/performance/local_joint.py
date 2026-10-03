"""Synthetic local management latency and actual PostgreSQL query evidence."""
from __future__ import annotations

import argparse
import base64
import datetime as dt
import hashlib
import http.cookiejar
import json
import math
from pathlib import Path
import re
import secrets
import shutil
import socket
import subprocess
import sys
import time
import urllib.parse
import urllib.request

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "integration/fixtures"))
sys.path.insert(0, str(ROOT / "integration/gateway_environment"))
import synthetic
from rollback_003_local import docker, envfile, private_json, require, sha

FROZEN = ROOT / ".local/source-20260918/gateway-overlay-20260918t023414-8e3565"
EXPECTED_BINARY = "a66c7be3de344221d6433a6bc7140523d95a6a068fd2312757c80f2dc50c073d"
EXPECTED_SOURCE = "956fc90be91a922ac1593b283102bc5149674dac337564e6023479ec8e60a6f9"


def summarize(values):
    ordered = sorted(values)
    return {"samples": len(values), "p50_ms": ordered[math.ceil(len(values) * 0.5) - 1], "p95_ms": ordered[math.ceil(len(values) * 0.95) - 1], "min_ms": ordered[0], "max_ms": ordered[-1], "raw_ms": values}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--snapshot", type=Path, default=FROZEN)
    parser.add_argument("--samples", type=int, default=100)
    args = parser.parse_args()
    require(100 <= args.samples <= 200, "Use 100 to 200 serial samples")
    with socket.socket() as probe:
        probe.bind(("127.0.0.1", 0))
        published_port = probe.getsockname()[1]
    source = args.snapshot / "source"
    binary = args.snapshot / "codex-agent-linux-amd64"
    manifest = json.loads((args.snapshot / "source-manifest.json").read_text())
    require(manifest["sha256"] == EXPECTED_SOURCE and sha(binary) == EXPECTED_BINARY, "Frozen candidate identity mismatch")
    for name, digest in manifest["files"].items():
        require(sha(source / name) == digest, "Frozen source changed: " + name)
    run_id = secrets.token_hex(5)
    stamp = dt.datetime.now(dt.timezone.utc).strftime("%Y%m%dT%H%M%S") + "-" + run_id
    output = ROOT / ".local/performance-local-joint" / stamp
    output.mkdir(parents=True)
    dataset = output / "dataset"
    anchor = int(time.time()) // 60 * 60
    generated = synthetic.build(dataset, 1000, 50000, anchor)
    accounts = json.loads((dataset / "accounts.json").read_text())
    accounts["active"] = ""
    for account in accounts["accounts"]:
        account["enabled"] = False
    private_json(dataset / "accounts.json", accounts)
    private_json(dataset / "settings.json", {"admin_api": False})
    admin_key = secrets.token_hex(24)
    (dataset / "client-key").write_text(admin_key)
    dataset_hashes = {p.name: sha(p) for p in sorted(dataset.iterdir()) if p.name in ("accounts.json", "groups.json", "proxies.json", "logs.jsonl", "keys.json", "settings.json")}
    dataset_sha = hashlib.sha256(json.dumps(dataset_hashes, sort_keys=True).encode()).hexdigest()
    shutil.copyfile(binary, output / "candidate")
    (output / "candidate").chmod(0o755)
    namespace = "codex-agent:integration:performance:" + run_id + ":"
    names = {key: "codex-perf-" + run_id + "-" + suffix for key, suffix in (("network", "net"), ("pg", "pg"), ("redis", "redis"), ("app", "app"), ("volume", "data"))}
    label_key, label = "codex-agent.integration.performance", "codex-agent.integration.performance=" + run_id
    database, owner, runtime, schema = [prefix + run_id for prefix in ("perf_", "perf_owner_", "perf_runtime_", "perf_schema_")]
    owner_password, runtime_password, redis_password = [secrets.token_hex(24) for _ in range(3)]
    report = {"schema": "codex-agent-local-management-performance-v1", "status": "fail", "timestamp_utc": stamp, "candidate_binary_sha256": EXPECTED_BINARY, "source_sha256": EXPECTED_SOURCE, "runner_sha256": sha(Path(__file__)), "dataset_sha256": dataset_sha, "dataset_files_sha256": dataset_hashes, "synthetic_only": True, "production_touched": False, "gateway_or_refresh_requests": 0, "resources": names, "label": label, "latency": {}, "checks": {}, "limits": ["Serial local management load only; not upstream throughput or concurrent production capacity", "Process-cold means restarted application, not cold PostgreSQL or operating-system buffers", "Cache-cold overview clears only its exact isolated Redis overview key; database buffers remain warm", "EXPLAIN covers selected account, log and overview SQL shapes, not complex reference-price/snapshot queries", "Retention/rebuild is intentionally covered by separate candidate-956 PG evidence, not rerun here"]}
    created = []
    started = time.monotonic()

    def persist():
        private_json(output / "report.json", report)

    def sql(statement, readonly=False):
        options = "-c statement_timeout=60000" + (" -c default_transaction_read_only=on" if readonly else "")
        return docker("exec", "-i", "-e", "PGOPTIONS=" + options, names["pg"], "psql", "-X", "-q", "-U", owner, "-d", database, "-At", "-v", "ON_ERROR_STOP=1", stdin='SET search_path TO "' + schema + '",public;\n' + statement, timeout=90)

    def inspect(name):
        return json.loads(docker("inspect", name))[0]

    try:
        report["stage"] = "isolated_dependencies"
        persist()
        pg_image = docker("image", "inspect", "postgres:18-alpine", "--format", "{{.Id}}")
        redis_image = docker("image", "inspect", "redis:8-alpine", "--format", "{{.Id}}")
        docker("network", "create", "--label", label, names["network"])
        created.append(("network", names["network"]))
        docker("volume", "create", "--label", label, names["volume"])
        created.append(("volume", names["volume"]))
        envfile(output / "pg.env", {"POSTGRES_USER": owner, "POSTGRES_PASSWORD": owner_password, "POSTGRES_DB": database})
        docker("run", "-d", "--name", names["pg"], "--label", label, "--network", names["network"], "--network-alias", "pg", "--memory", "768m", "--cpus", "1", "--pids-limit", "128", "--env-file", str(output / "pg.env"), "--mount", "type=volume,source=" + names["volume"] + ",target=/var/lib/postgresql", pg_image, "postgres", "-c", "max_connections=24", "-c", "shared_buffers=128MB", "-c", "shared_preload_libraries=pg_stat_statements", "-c", "pg_stat_statements.track=all")
        created.append(("container", names["pg"]))
        redis_config = output / "redis.conf"
        redis_config.write_text("bind 0.0.0.0\nprotected-mode yes\nport 6379\nsave \"\"\nappendonly no\nmaxmemory 64mb\nmaxmemory-policy noeviction\nuser default off\nuser " + runtime + " on >" + redis_password + " ~" + namespace + "* &" + namespace + "* +@all -@dangerous\n")
        docker("run", "-d", "--name", names["redis"], "--label", label, "--network", names["network"], "--network-alias", "redis", "--memory", "96m", "--cpus", "0.25", "--pids-limit", "64", "--mount", "type=bind,source=" + str(redis_config) + ",target=/usr/local/etc/redis.conf,readonly", redis_image, "redis-server", "/usr/local/etc/redis.conf")
        created.append(("container", names["redis"]))
        for _ in range(80):
            if subprocess.run(["docker", "exec", names["pg"], "pg_isready", "-U", owner, "-d", database], capture_output=True).returncode == 0:
                break
            time.sleep(0.25)
        else:
            raise AssertionError("Dedicated PostgreSQL startup timeout")
        sql('CREATE EXTENSION pg_stat_statements WITH SCHEMA public; CREATE SCHEMA "' + schema + '"; CREATE ROLE "' + runtime + "\" LOGIN PASSWORD '" + runtime_password + "' NOSUPERUSER NOCREATEDB NOCREATEROLE CONNECTION LIMIT 6; REVOKE CONNECT ON DATABASE \"" + database + '" FROM PUBLIC; GRANT CONNECT ON DATABASE "' + database + '" TO "' + runtime + '"; REVOKE ALL ON SCHEMA public FROM PUBLIC; GRANT USAGE ON SCHEMA "' + schema + '" TO "' + runtime + '";')
        migration = {"DATABASE_URL": f"postgres://{owner}:{owner_password}@pg:5432/{database}?sslmode=disable&search_path={schema}", "CREDENTIAL_ENCRYPTION_KEY": base64.b64encode(secrets.token_bytes(32)).decode(), "CREDENTIAL_ENCRYPTION_KEY_ID": "synthetic-perf-" + run_id, "REDIS_URL": f"redis://{runtime}:{redis_password}@redis:6379/0", "REDIS_KEY_PREFIX": namespace, "ADMIN_KEY": admin_key, "ADMIN_COOKIE_SECURE": "false", "ADMIN_SESSION_TTL": "1800", "PG_MAX_CONNS": "4", "STATS_TIMEZONE": "UTC"}
        environment = {**migration, "DATABASE_URL": f"postgres://{runtime}:{runtime_password}@pg:5432/{database}?sslmode=disable&search_path={schema}"}
        envfile(output / "migration.env", migration)
        envfile(output / "runtime.env", environment)
        envfile(output / "redis-client.env", {"REDISCLI_AUTH": redis_password})
        common = ["--network", names["network"], "--label", label, "--read-only", "--memory", "512m", "--cpus", "1", "--pids-limit", "128", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--user", "65534:65534", "--tmpfs", "/tmp:rw,nosuid,size=32m", "--mount", "type=bind,source=" + str(output) + ",target=/work,readonly"]

        def cli(*args):
            result = subprocess.run(["docker", "run", "--rm", *common, "--env-file", str(output / "migration.env"), "--entrypoint", "/work/candidate", pg_image, *args], capture_output=True, text=True, encoding="utf-8", errors="replace", timeout=900)
            require(result.returncode == 0, "Candidate CLI failed: " + args[0])
            return json.loads(result.stdout) if result.stdout.lstrip().startswith("{") else {}

        report["stage"] = "migration_and_50000_log_import"
        persist()
        cli("migrate")
        checksums = {"migrations/" + p.name: sha(p) for p in (source / "migrations").glob("*.sql")}
        require(json.loads(sql("SELECT json_object_agg(name,checksum) FROM schema_migrations;", True)) == checksums, "Embedded migrations differ from frozen source")
        before = int(sql("SELECT count(*) FROM accounts;", True))
        preview = cli("import-legacy", "--source", "/work/dataset", "--dry-run")
        require(int(sql("SELECT count(*) FROM accounts;", True)) == before == 0, "Dry-run wrote accounts")
        import_start = time.monotonic()
        imported = cli("import-legacy", "--source", "/work/dataset")
        report["import_seconds"] = round(time.monotonic() - import_start, 3)
        for key in ("accounts", "groups", "proxies", "account_groups", "account_proxies", "logs", "metadata_hash", "manifest"):
            require(preview[key] == imported[key], "Dry-run/import mismatch: " + key)
        require((imported["accounts"], imported["logs"], imported["account_groups"], imported["account_proxies"]) == (1000, 50000, 1000, 1000), "Imported counts differ")
        private_json(output / "dry-run.json", preview)
        private_json(output / "import.json", imported)
        sql('GRANT SELECT,INSERT,UPDATE,DELETE ON ALL TABLES IN SCHEMA "' + schema + '" TO "' + runtime + '"; GRANT USAGE,SELECT ON ALL SEQUENCES IN SCHEMA "' + schema + '" TO "' + runtime + '"; ANALYZE;')
        actual = json.loads(sql("SELECT json_build_object('accounts',(SELECT count(*) FROM accounts),'enabled',(SELECT count(*) FROM accounts WHERE enabled),'groups',(SELECT count(*) FROM groups),'proxies',(SELECT count(*) FROM proxies),'account_groups',(SELECT count(*) FROM account_groups),'account_proxies',(SELECT count(*) FROM account_proxies),'min_expiry',(SELECT min(expires_at) FROM accounts),'max_expiry',(SELECT max(expires_at) FROM accounts),'logs',(SELECT count(*) FROM request_logs),'events',(SELECT count(*) FROM usage_events),'input',(SELECT sum(input_tokens) FROM usage_events),'output',(SELECT sum(output_tokens) FROM usage_events),'cached',(SELECT sum(cached_tokens) FROM usage_events),'tokens',(SELECT tokens FROM usage_totals WHERE scope='global' AND subject=''),'usage_minute',(SELECT count(*) FROM usage_minute),'usage_daily',(SELECT count(*) FROM usage_daily));", True))
        expected = {"accounts": 1000, "enabled": 0, "groups": 1, "proxies": 1, "account_groups": 1000, "account_proxies": 1000, "min_expiry": synthetic.EXPIRY, "max_expiry": synthetic.EXPIRY, "logs": 50000, "events": 50000, "input": 5000000, "output": 1250000, "cached": 4000000, "tokens": 6250000}
        require(all(actual[k] == v for k, v in expected.items()), "Actual dataset metadata or accounting differs")
        report["actual_dataset"] = actual
        report["checks"]["dry_run_import_counts_relationships_expiry_accounting"] = True
        print("Synthetic import verified: 1000 disabled accounts, 50000 logs, 6250000 tokens", flush=True)
        docker("run", "-d", "--name", names["app"], *common, "--env-file", str(output / "runtime.env"), "--publish", f"127.0.0.1:{published_port}:18879", "--entrypoint", "/work/candidate", pg_image, "--listen", "0.0.0.0:18879", "--web", "/nonexistent")
        created.append(("container", names["app"]))
        port = int(inspect(names["app"])["NetworkSettings"]["Ports"]["18879/tcp"][0]["HostPort"])
        base = "http://127.0.0.1:" + str(port)
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))

        def request(path, body=None):
            require(path.startswith(("/api/", "/health/")) and "refresh" not in path, "Non-management endpoint refused")
            req = urllib.request.Request(base + path, data=json.dumps(body).encode() if body is not None else None, headers={"Content-Type": "application/json", "Origin": base})
            begin = time.perf_counter()
            with opener.open(req, timeout=30) as response:
                raw = response.read()
                require(response.status == 200, "Unexpected HTTP status")
            elapsed = round((time.perf_counter() - begin) * 1000, 4)
            return elapsed, json.loads(raw)

        def ready():
            for _ in range(12):
                try:
                    request("/health/ready")
                    return
                except OSError:
                    require(inspect(names["app"])["State"]["Running"], "Performance app exited")
                    time.sleep(0.25)
            raise AssertionError("Performance readiness timeout")

        ready()
        require(docker("exec", names["app"], "sha256sum", "/proc/1/exe").split()[0] == EXPECTED_BINARY, "Runtime binary mismatch")
        request("/api/session", {"admin_key": admin_key})
        endpoints = {"accounts_legacy_50": "/api/accounts?limit=50", "accounts_management_50": "/api/accounts/manage?page=1&page_size=50", "logs_50": f"/api/logs?from={anchor-86400}&to={anchor+60}&page_size=50&sort_by=time&sort_order=desc", "overview_warm_cache": "/api/overview", "overview_uncached_range": f"/api/overview?from={anchor-86400}&to={anchor+60}&step=300"}
        report["query_sha256"] = {key: hashlib.sha256(value.encode()).hexdigest() for key, value in endpoints.items()}
        report["endpoints"] = endpoints
        report["stage"] = "serial_management_samples"
        persist()
        for name, path in endpoints.items():
            docker("restart", "--time", "90", names["app"], timeout=100)
            ready()
            first_ms, first = request(path)
            values = []
            for _ in range(args.samples):
                elapsed, value = request(path)
                values.append(elapsed)
                if name.startswith("accounts"):
                    require(len(value["accounts"]) == 50, "Account sample row count mismatch")
                elif name.startswith("logs"):
                    require(len(value["items"]) == 50, "Log sample row count mismatch")
                else:
                    require(value["cards"]["tokens_total"] == 6250000, "Overview sample accounting changed")
            report["latency"][name] = {"process_cold_first_ms": first_ms, "warm": summarize(values), "http_status": 200, "concurrency": 1}
            print(name + " P50/P95 ms: " + str(report["latency"][name]["warm"]["p50_ms"]) + "/" + str(report["latency"][name]["warm"]["p95_ms"]), flush=True)
            persist()
        # Invalidate only the exact isolated overview key outside the HTTP timer.
        cold = []
        for _ in range(args.samples):
            docker("exec", "-e", "REDISCLI_AUTH=" + redis_password, names["redis"], "redis-cli", "--user", runtime, "DEL", namespace + "overview")
            elapsed, value = request("/api/overview")
            require(value["cards"]["tokens_total"] == 6250000, "Cache-cold overview differs")
            cold.append(elapsed)
        report["latency"]["overview_cache_cold"] = {"definition": "Exact overview Redis key deleted before each request; database warm", "warm": summarize(cold), "http_status": 200, "concurrency": 1}
        report["stage"] = "query_plans_and_call_counts"
        persist()
        def pg_stat():
            return json.loads(sql("SELECT COALESCE(json_agg(t),'[]'::json) FROM (SELECT queryid::text,query,calls,rows,total_exec_time FROM public.pg_stat_statements WHERE userid=(SELECT oid FROM pg_roles WHERE rolname='" + runtime + "')) t;", True))
        call_evidence = {}
        for name, path in {"legacy_50": "/api/accounts?limit=50", "legacy_500": "/api/accounts?limit=500", "management_50": "/api/accounts/manage?page=1&page_size=50", "management_500": "/api/accounts/manage?page=1&page_size=500", "logs_50": endpoints["logs_50"]}.items():
            oldstats = {row["queryid"]: row for row in pg_stat()}
            request(path)
            changes = []
            for row in pg_stat():
                oldrow = oldstats.get(row["queryid"], {})
                delta = row["calls"] - oldrow.get("calls", 0)
                if delta:
                    changes.append({"query": row["query"], "calls": delta, "rows": row["rows"] - oldrow.get("rows", 0), "execution_ms": row["total_exec_time"] - oldrow.get("total_exec_time", 0)})
            call_evidence[name] = {"total_sql_calls": sum(row["calls"] for row in changes), "statements": changes}
        private_json(output / "sql-call-deltas.json", call_evidence)
        account_source = (source / "accounts_pg.go").read_text()
        columns = re.search(r"const accountColumns = `([^`]+)`", account_source).group(1)
        metric_source = (source / "stats_pg.go").read_text()
        sums = re.search(r'const metricsSums = "([^"]+)"', metric_source).group(1)
        statements = {
            "accounts_50": ("accounts_pg.go queryAccounts/ListPage", "SELECT " + columns + " FROM accounts a WHERE true ORDER BY a.created_at DESC,a.id DESC LIMIT 51"),
            "managed_accounts_offset_950": ("admin_accounts_management.go managedAccounts, maximum dataset page", "SELECT " + columns + " FROM accounts a WHERE true ORDER BY a.created_at desc,a.id desc LIMIT 50 OFFSET 950"),
            "managed_account_count": ("admin_accounts_management.go exact count", "SELECT count(*) FROM accounts a WHERE true"),
            "managed_logs_page": ("admin_usage_store.go and admin_usage_query.go time sort / first page SQL shape", f"SELECT id,record FROM request_logs WHERE created_at >= to_timestamp({anchor-86400}) AND created_at < to_timestamp({anchor+60}) AND id<=50000 ORDER BY extract(epoch FROM created_at)::bigint desc,created_at desc,id desc LIMIT 51"),
            "managed_log_count": ("admin_usage_store.go optional exact_total=true filtered count; not default list", f"SELECT count(*) FROM request_logs WHERE created_at >= to_timestamp({anchor-86400}) AND created_at < to_timestamp({anchor+60}) AND id<=50000"),
            "overview_totals": ("stats_pg.go totalsPG", f"SELECT {sums} FROM usage_minute WHERE bucket>={anchor-86400} AND bucket<{anchor+60}"),
            "overview_series": ("stats_pg.go seriesPG", f"SELECT (bucket-{anchor-86400})/300 AS slot,{sums} FROM usage_minute WHERE bucket>={anchor-86400} AND bucket<{anchor+60} GROUP BY slot"),
        }
        plans = {}
        for name, (origin, statement) in statements.items():
            result = json.loads(sql("EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) " + statement + ";", True))
            plans[name] = {"source": origin, "sql": statement, "sql_sha256": hashlib.sha256(statement.encode()).hexdigest(), "explain": result}
        private_json(output / "explain.json", plans)
        report["sql_call_deltas"] = {key: value["total_sql_calls"] for key, value in call_evidence.items()}
        report["sql_evidence_sha256"] = sha(output / "sql-call-deltas.json")
        report["explain_sha256"] = sha(output / "explain.json")
        report["explain_execution_ms"] = {key: value["explain"][0]["Execution Time"] for key, value in plans.items()}
        report["observations"] = ["Legacy list is cursor/LIMIT+1; managed list explicitly runs an exact COUNT and supports bounded OFFSET (source verified)", "Account relationships use correlated SQL array subplans, so stable round-trip counts do not prove constant database work", "SQL call deltas compare real 50/500-row requests in this serial dataset only; not a blanket no-N+1 claim", "Managed log exact COUNT is optional exact_total=true, not default list; its plan can scan filtered rows; this dataset has 49999 recent and one 120-day-old record", "Overview enumerates account metadata; latency at 1000 rows does not establish unbounded-scale behavior", "No cold filesystem-buffer benchmark or concurrent capacity limit was attempted"]
        report["pool"] = {"pg_max_conns": 4, "runtime_role_connection_limit": 6, "server_max_connections": 24, "observed_app_connections": int(sql("SELECT count(*) FROM pg_stat_activity WHERE usename='" + runtime + "';", True))}
        report["actual_limits"] = {key: {field: inspect(names[key])["HostConfig"][field] for field in ("Memory", "NanoCpus", "PidsLimit")} for key in ("pg", "redis", "app")}
        report["checks"]["management_latency_and_query_plans"] = True
        report["status"] = "pass"
        report["stage"] = "complete"
    except Exception as error:
        report["error"] = str(error) if isinstance(error, AssertionError) else type(error).__name__
    finally:
        failures = []
        for kind, name in reversed(created):
            try:
                info = json.loads(docker("inspect" if kind == "container" else kind, *([] if kind == "container" else ["inspect"]), name))[0]
                labels = info["Config"]["Labels"] if kind == "container" else info["Labels"]
                require(labels.get(label_key) == run_id, "Cleanup label mismatch")
                if kind == "container":
                    docker("stop", "--time", "90", name, timeout=100)
                    docker("rm", name)
                else:
                    docker(kind, "rm", name)
            except Exception:
                failures.append(name)
        report["cleanup"] = {"complete": not failures, "retained_resources": failures}
        if failures:
            report["status"] = "fail"
        report["elapsed_seconds"] = round(time.monotonic() - started, 3)
        persist()
    print(json.dumps({"status": report["status"], "report": str(output / "report.json"), "stage": report.get("stage"), "error": report.get("error"), "cleanup": report["cleanup"]}, indent=2))
    return 0 if report["status"] == "pass" else 1


if __name__ == "__main__":
    raise SystemExit(main())
