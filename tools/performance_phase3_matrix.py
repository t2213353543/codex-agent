"""Run the source-bound phase-three matrix serially, retaining every failure.

Requires the already uploaded canonical binary and completed retained fixtures.
Never restarts fixture append, raises resource limits, or touches production.
"""
from __future__ import annotations

import argparse
import datetime as dt
import json
from pathlib import Path
import shlex
import subprocess
import sys

from integration_env import ROOT, connect, run


def load(path):
    return json.loads(Path(path).read_text(encoding="utf-8"))


def quiet_preflight():
    client = connect()
    try:
        raw = run(client, "docker ps --format '{{.Names}}|{{.Label \"codex-agent.integration\"}}'")
        peers = [line for line in raw.splitlines() if line.split("|", 1)[1].strip()]
        if peers:
            raise RuntimeError("performance window is not exclusive: " + "; ".join(peers))
        query = "SELECT COALESCE(json_agg(json_build_object('pid',pid,'application_name',application_name,'active_seconds',extract(epoch FROM clock_timestamp()-query_start))),'[]'::json) FROM pg_stat_activity WHERE datname='codex_agent_integration' AND backend_type='client backend' AND state='active' AND pid<>pg_backend_pid()"
        active = json.loads(run(client, shlex.join(["docker", "exec", "codex-agent-integration-postgres", "psql", "-X", "-U", "postgres", "-d", "codex_agent_integration", "-Atc", query])))
        if active:
            raise RuntimeError("test database still has active client work: " + json.dumps(active))
        return {"checked_at_utc": dt.datetime.now(dt.timezone.utc).isoformat(), "active_labeled_test_containers": peers,
                "active_test_database_clients": active, "scope": "No competing labeled test container or active client in the isolated test database; unrelated host applications and OS caches are not controlled."}
    finally:
        client.close()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary-report", type=Path, required=True)
    parser.add_argument("--uniform10m-report", type=Path, required=True)
    parser.add_argument("--uniform-maintenance-report", type=Path, required=True)
    parser.add_argument("--phase2-report", type=Path, default=ROOT / "integration/performance/results/slo-20260929-phase2-final.json")
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--jobs", help="Comma-separated explicit jobs; omitted runs the entire matrix")
    parser.add_argument("--resume", action="store_true", help="Skip terminal job reports, including failures; never erase prior evidence")
    args = parser.parse_args()
    canonical = load(args.binary_report)
    extension = load(args.uniform10m_report)
    maintenance = load(args.uniform_maintenance_report)
    if canonical.get("synthetic_only") is not True or extension.get("status") != "fixture_complete" or extension.get("counts", {}).get("logs") != 10000000:
        raise RuntimeError("verified canonical binary and completed ten-million synthetic fixture are required")
    if maintenance.get("status") != "pass" or maintenance.get("source_sha256") != canonical.get("source_sha256"):
        raise RuntimeError("same-source uniform migrations/rebuild must finish successfully before this matrix")
    phase2 = load(args.phase2_report)["runs"]
    uniform100k = phase2["uniform_100k"]["evidence"]
    burst100k = phase2["burst_100k"]["evidence"]
    burst10m = phase2["burst_10m"]["evidence"]
    jobs = [
        ("maintenance_uniform100k", uniform100k, "^TestPerformanceUniformHTTP$", 1, ["--uniform-migrate", "--uniform-maintenance-only"]),
        ("maintenance_burst100k", burst100k, "^TestPerformanceMaintenanceMigrations$", 1, ["--apply-missing-migrations"]),
        ("uniform10m", str(args.uniform10m_report), "^TestPerformanceUniformHTTP$", 20, []),
        ("uniform100k", uniform100k, "^TestPerformanceUniformHTTP$", 20, []),
        ("complex_burst10m", burst10m, "^TestPerformanceComplexFilters$", 20, []),
        ("burst100k_reads", burst100k, "^TestPerformanceSLO(Scale|AdditionalReads)$", 20, []),
        ("burst10m_reads_writes", burst10m, "^TestPerformanceSLO(Scale|AdditionalReads|Writes)$", 20, []),
        ("mixed_model100k", burst100k, "^TestPerformanceMixedModelStatsBaseline$", 20, []),
        ("recent_operations100k", burst100k, "^TestPerformanceOperationsRecentScale$", 20, []),
        ("bulk_import", burst100k, "^TestPerformanceAccountBulkWrites$", 3, ["--import-sizes", "100,1000,10000"]),
        ("local_compat_metadata_writes", burst100k, "^TestPerformance(AccountBulkMetadata|ManagementLocalWrites|CompatLocalWrites)$", 20, []),
        ("model_summary_write_cost", burst100k, "^TestPerformanceManagedUsageModelWriteCost$", 1, []),
    ]
    if args.jobs:
        requested = set(args.jobs.split(","))
        unknown = requested - {job[0] for job in jobs}
        if unknown:
            raise ValueError("unknown jobs: " + ",".join(sorted(unknown)))
        jobs = [job for job in jobs if job[0] in requested]
    output = args.output.resolve()
    output.mkdir(parents=True, exist_ok=True)
    state_path = output / "matrix.json"
    state = load(state_path) if state_path.exists() and args.resume else {
        "source_sha256": canonical["source_sha256"], "binary_sha256": canonical["binary_sha256"],
        "canonical_report": str(args.binary_report.resolve()), "synthetic_only": True,
        "production_touched": False, "all_routes_slo_proven": False,
        "uniform_maintenance": str(args.uniform_maintenance_report.resolve()), "jobs": {},
        "notes": ["Labeled test containers must be absent before each job; only this matrix controls performance.",
                  "Independent-write/mixed/recent tests create and clean their own synthetic schemas.",
                  "Old rolling-24-hour retained traffic is empty; recent operations has an independent nonempty fixture.",
                  "Failures are retained and never rerun automatically; passing samples do not prove untested routes."]}
    if state["source_sha256"] != canonical["source_sha256"] or state["binary_sha256"] != canonical["binary_sha256"]:
        raise RuntimeError("cannot mix binaries in an existing matrix")
    def save():
        state_path.write_text(json.dumps(state, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    for name, fixture, selector, samples, extra in jobs:
        report_path = output / name / "report.json"
        if report_path.exists():
            previous = load(report_path)
            if args.resume and previous.get("status") in ("pass", "fail"):
                state["jobs"][name] = {"status": previous["status"], "report": str(report_path), "resumed_terminal": True}
                save()
                continue
            raise RuntimeError("existing nonterminal or non-resumable report: " + str(report_path))
        preflight = quiet_preflight()
        command = [sys.executable, str(ROOT / "tools/performance_slo.py"), "--reuse", fixture,
                   "--binary-report", str(args.binary_report.resolve()), "--remote-binary", canonical["remote_binary"],
                   "--test-run", selector, "--samples", str(samples), "--timeout-minutes", "90",
                   "--output", str(report_path.parent)] + extra
        state["jobs"][name] = {"status": "running", "report": str(report_path), "preflight": preflight,
                                "command": command, "started_at_utc": dt.datetime.now(dt.timezone.utc).isoformat()}
        save()
        print(json.dumps({"job_start": name, "report": str(report_path)}, ensure_ascii=False), flush=True)
        result = subprocess.run(command, cwd=ROOT)
        report = load(report_path) if report_path.exists() else {"status": "runner_failed_without_report"}
        state["jobs"][name].update(status=report["status"], exit_code=result.returncode,
                                  completed_at_utc=dt.datetime.now(dt.timezone.utc).isoformat())
        save()
        print(json.dumps({"job_complete": name, "status": report["status"]}, ensure_ascii=False), flush=True)
        if name.startswith("maintenance_") and report["status"] != "pass":
            raise RuntimeError("required maintenance failed; do not measure an incompatible schema")
    state["status"] = "pass" if all(job["status"] == "pass" for job in state["jobs"].values()) else "fail_or_incomplete"
    save()
    print(state_path, flush=True)
    return 0 if state["status"] == "pass" else 1


if __name__ == "__main__":
    raise SystemExit(main())
