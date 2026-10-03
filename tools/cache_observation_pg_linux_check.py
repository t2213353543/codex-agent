"""Run frozen cache/storage tests next to the isolated PostgreSQL instance.

Cross-compiles locally, uploads only a synthetic test binary, and sends the
isolated DSN over SSH stdin into the test process environment. No production
service, database, account, or credentials are touched.
"""
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import re
import shlex
import subprocess
import time
from urllib.parse import urlsplit

from integration_env import ROOT, REMOTE, connect, run
from project_layout import backend_source_paths


TESTS = [
    "TestCacheReadCreationStatisticsPG",
    "TestPGManagedUsageQueryStatsExport",
    "TestPGManagedUsageAggregationParityReview",
    "TestPGManagedAccountProjectionParity",
    "TestOverviewBreakdownPG",
    "TestPGManagedAccountWindowCosts",
    "TestPGManagedTimingCoverage",
    "TestPGBPSCacheScopeSettingsPartialUpdatePersistenceAndValidation",
]


def changed_backend_sources(root, manifest):
    current = {path.relative_to(root).as_posix(): hashlib.sha256(path.read_bytes()).hexdigest()
               for path in backend_source_paths(root)}
    return sorted(name for name in set(manifest) | set(current) if manifest.get(name) != current.get(name))


def main():
    stamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%S.%fZ")
    folder = ROOT / ".local/cache-observation-pg-linux" / stamp
    source = folder / "source"
    source.mkdir(parents=True)
    report = {"status": "fail", "started_at": stamp, "synthetic_only": True,
              "database": "codex_agent_integration", "database_user": "codex_agent_test",
              "remote_port": 25432, "selected_tests": TESTS, "test_outcomes": {}}
    client, remote_binary = None, None
    private = []
    began = time.monotonic()
    try:
        config = json.loads((ROOT / ".local/integration/environment.json").read_text(encoding="utf-8-sig"))
        if config.get("environment") != "isolated-synthetic-only" or config.get("remote_root") != REMOTE:
            raise RuntimeError("Isolated integration environment identity mismatch")
        dsn = config["TEST_DATABASE_URL"]
        target = urlsplit(dsn)
        if target.hostname != "127.0.0.1" or target.port != 25432 or target.username != "codex_agent_test" or target.path != "/codex_agent_integration":
            raise RuntimeError("Only the isolated integration role/database is allowed")
        private = [dsn, target.password or ""]
        redis_url = config.get("TEST_REDIS_URL") or config.get("REDIS_URL")
        redis_target = urlsplit(redis_url or "")
        if redis_target.hostname != "127.0.0.1" or redis_target.port != 26379 or redis_target.username != "codex_agent_test":
            raise RuntimeError("Only the isolated integration Redis is allowed")
        private.extend([redis_url, redis_target.password or ""])

        def scrub(value):
            for secret in private:
                if secret:
                    value = value.replace(secret, "[redacted]")
            return value

        manifest = {}
        for path in backend_source_paths(ROOT):
            raw = path.read_bytes()
            relative = path.relative_to(ROOT).as_posix()
            manifest[relative] = hashlib.sha256(raw).hexdigest()
            destination = source / relative
            destination.parent.mkdir(parents=True, exist_ok=True)
            destination.write_bytes(raw)
        (folder / "source-manifest.json").write_text(json.dumps(manifest, indent=2) + "\n", encoding="utf-8")
        report["source_sha256"] = hashlib.sha256(json.dumps(manifest, sort_keys=True).encode()).hexdigest()
        print(json.dumps({"phase": "frozen", "source_sha256": report["source_sha256"], "folder": str(folder)}), flush=True)
        binary = folder / "cache-observation.test"
        env = {key: value for key, value in os.environ.items()
               if not key.startswith(("TEST_", "BPS_", "CODEX_", "OPENAI_"))
               and key not in ("DATABASE_URL", "REDIS_URL")}
        env.update(GOOS="linux", GOARCH="amd64", CGO_ENABLED="0", GOWORK="off", GOPROXY="off", GOSUMDB="off", GOTOOLCHAIN="local")
        compiled = subprocess.run(["go", "test", "-c", "-trimpath", "-o", str(binary), "./internal/server"],
                                  cwd=source, env=env, capture_output=True, text=True, encoding="utf-8", timeout=240)
        (folder / "compile.log").write_text(scrub(compiled.stdout + compiled.stderr), encoding="utf-8")
        if compiled.returncode:
            raise RuntimeError("Linux test binary compilation failed; see compile.log")
        binary_sha = hashlib.sha256(binary.read_bytes()).hexdigest()
        report["binary_sha256"] = binary_sha
        client = connect()
        if run(client, "cat " + shlex.quote(REMOTE + "/.codex-agent-integration")).strip() != "codex-agent-integration":
            raise RuntimeError("Remote integration marker mismatch")
        inspection = json.loads(run(client, "docker inspect --format '{{json .}}' codex-agent-integration-postgres"))
        if not inspection["State"]["Running"] or inspection["NetworkSettings"]["Ports"]["5432/tcp"] != [{"HostIp": "127.0.0.1", "HostPort": "25432"}]:
            raise RuntimeError("Isolated PostgreSQL is not running on its expected loopback port")
        report["container_id"] = inspection["Id"]
        report["container_loopback_verified"] = True
        remote = REMOTE + "/checks/cache-observation-" + stamp.lower()
        run(client, "install -d -m 700 " + shlex.quote(remote))
        remote_binary = remote + "/" + binary_sha + ".test"
        with client.open_sftp() as sftp:
            sftp.put(str(binary), remote_binary)
            sftp.chmod(remote_binary, 0o700)
        if run(client, "sha256sum " + shlex.quote(remote_binary)).split()[0] != binary_sha:
            raise RuntimeError("Uploaded test binary hash mismatch")
        print(json.dumps({"phase": "executing", "tests": len(TESTS), "binary_sha256": binary_sha}), flush=True)
        remote_input = {"binary": remote_binary, "dsn": dsn, "redis_url": redis_url, "regex": "^(?:" + "|".join(TESTS) + ")$"}
        script = "CONFIG=" + repr(remote_input) + "\n" + r'''
import json,os,subprocess,time
from urllib.parse import urlsplit
target=urlsplit(CONFIG['dsn'])
assert target.hostname=='127.0.0.1' and target.port==25432 and target.username=='codex_agent_test' and target.path=='/codex_agent_integration'
assert CONFIG['binary'].startswith('/opt/codex-agent-integration/checks/cache-observation-')
env={key:value for key,value in os.environ.items() if key in ('PATH','LANG','LC_ALL','TZ')}
env['TEST_DATABASE_URL']=CONFIG['dsn']
redis_target=urlsplit(CONFIG['redis_url'])
assert redis_target.hostname=='127.0.0.1' and redis_target.port==26379 and redis_target.username=='codex_agent_test'
env['TEST_REDIS_URL']=CONFIG['redis_url']
env['TEST_REDIS_PREFIX']='codex-agent:integration:bps-test:'
start=time.monotonic()
result=subprocess.run([CONFIG['binary'],'-test.v','-test.run',CONFIG['regex'],'-test.timeout=120s'],env=env,capture_output=True,text=True,timeout=135)
print(json.dumps({'exit_code':result.returncode,'stdout':result.stdout,'stderr':result.stderr,'wall_ms':round((time.monotonic()-start)*1000)}),flush=True)
'''
        stdin, stdout, stderr = client.exec_command("python3 -B -", timeout=150)
        stdin.write(script)
        stdin.channel.shutdown_write()
        raw, remote_error = stdout.read().decode("utf-8"), stderr.read().decode("utf-8")
        remote_exit = stdout.channel.recv_exit_status()
        if remote_exit:
            raise RuntimeError("Remote test runner failed: " + scrub(remote_error))
        result = json.loads(raw)
        test_log = scrub(result["stdout"] + result["stderr"])
        (folder / "test.log").write_text(test_log, encoding="utf-8")
        converted = subprocess.run(["go", "tool", "test2json", "-t", "-p", "codex-agent/internal/server"],
                                   input=test_log, capture_output=True, text=True, encoding="utf-8", timeout=30)
        if converted.returncode:
            raise RuntimeError("Converting the real test log to JSON failed")
        (folder / "test.jsonl").write_text(converted.stdout, encoding="utf-8")
        events = [json.loads(line) for line in converted.stdout.splitlines() if line.strip()]
        outcomes = {event["Test"]: event["Action"] for event in events
                    if event.get("Test") and event["Action"] in ("pass", "fail", "skip")}
        changes = changed_backend_sources(ROOT, manifest)
        report.update(exit_code=result["exit_code"], wall_ms=result["wall_ms"], test_outcomes=outcomes,
                      source_changed_after_freeze=bool(changes), changed_files=changes)
        passed = result["exit_code"] == 0 and not changes and all(outcomes.get(name) == "pass" for name in TESTS)
        passed = passed and not any(event["Action"] in ("fail", "skip") for event in events)
        report["status"] = "pass" if passed else "fail"
    except Exception as error:
        message = str(error)
        for value in private:
            if value:
                message = message.replace(value, "[redacted]")
        report["error"] = type(error).__name__ + ": " + message
    finally:
        if client:
            if remote_binary:
                try:
                    with client.open_sftp() as sftp:
                        sftp.remove(remote_binary)
                    report["temporary_binary_removed"] = True
                except Exception:
                    report["temporary_binary_removed"] = False
            client.close()
        report["duration_seconds"] = round(time.monotonic() - began, 3)
        (folder / "report.json").write_text(json.dumps(report, indent=2) + "\n", encoding="utf-8")
        print(json.dumps({"report": str(folder / "report.json"), **report}), flush=True)
    return 0 if report["status"] == "pass" else 1


if __name__ == "__main__":
    raise SystemExit(main())
