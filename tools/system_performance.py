"""Freeze, verify and compare management endpoints on isolated synthetic PG/Redis.

Run: python tools/system_performance.py --baseline .local/system-perf-20260927/baseline-src
No deployment, real accounts, inference requests or production database access.
"""
from __future__ import annotations

import argparse
import datetime as dt
import gzip
import hashlib
import json
import os
from pathlib import Path
import re
import secrets
import shlex
import shutil
import statistics
import subprocess
import time

from integration_env import REMOTE, connect, run, write_remote
from gateway_source_race import isolated_environment
from run_linux_checks import scrub

ROOT = Path(__file__).resolve().parents[1]
BENCH_FILES = (
    "admin_resource_performance_benchmark_test.go",
    "admin_account_performance_benchmark_test.go",
    "admin_usage_performance_benchmark_test.go",
)
BENCH_PATTERN = "^(BenchmarkSystemPG(ResourceEndpoints|AccountSummaries|ManagedAccountsList|AccountBundleMetadata)|BenchmarkManagedUsage(AggregationSamples|Endpoints))$"


def freeze(source: Path, target: Path):
    target.mkdir(parents=True)
    for name in ("go.mod", "go.sum", "main.go"):
        shutil.copy2(source / name, target / name)
    for name in ("internal", "migrations"):
        shutil.copytree(source / name, target / name)
    for name in BENCH_FILES:
        shutil.copy2(ROOT / "internal/server" / name, target / "internal/server" / name)
    files = {p.relative_to(target).as_posix(): hashlib.sha256(p.read_bytes()).hexdigest()
             for p in sorted(target.rglob("*")) if p.is_file()}
    digest = hashlib.sha256(json.dumps(files, sort_keys=True).encode()).hexdigest()
    (target.parent / "source-manifest.json").write_text(json.dumps({"sha256": digest, "files": files}, indent=2))
    return digest


def management_tests(source: Path):
    names = set()
    for path in (source / "internal/server").glob("*_test.go"):
        if path.name.startswith(("admin", "accounts", "sessions", "stats", "store", "web", "settings", "storage", "database")):
            names.update(re.findall(r"^func (Test\w+)\(t \*testing.T\)", path.read_text(encoding="utf-8"), re.M))
    return sorted(names)


def build(source: Path, output: Path):
    env = os.environ.copy()
    env.update(GOOS="linux", GOARCH="amd64", CGO_ENABLED="0", GOWORK="off")
    binary = output / "server.test"
    result = subprocess.run(["go", "test", "-c", "-ldflags=-s -w", "-o", str(binary), "./internal/server"], cwd=source,
                            env=env, capture_output=True, timeout=240)
    (output / "build.log").write_bytes(result.stdout + result.stderr)
    if result.returncode:
        raise RuntimeError(f"Build failed; inspect {output / 'build.log'}")
    packed = output / "server.test.gz"
    packed.write_bytes(gzip.compress(binary.read_bytes(), compresslevel=6, mtime=0))
    return packed, hashlib.sha256(binary.read_bytes()).hexdigest()


def parse_bench(raw: str):
    rows = {}
    for line in raw.splitlines():
        fields = line.split()
        if len(fields) < 4 or not fields[0].startswith("Benchmark") or not fields[1].isdigit():
            continue
        name = re.sub(r"-\d+$", "", fields[0])
        metrics = {fields[i + 1]: float(fields[i]) for i in range(2, len(fields) - 1, 2)}
        rows.setdefault(name, []).append(metrics)
    results = {name: {"repetitions": len(values), "samples": values,
                   "median": {unit: statistics.median(v[unit] for v in values) for unit in values[0]}}
            for name, values in rows.items()}
    for match in re.findall(r"SYSTEM_SAMPLES (\{[^\n]+\})", raw):
        sample = json.loads(match)
        name = sample["name"]
        values = sample["samples_ms"]
        # Go may run a single calibration iteration before the requested count.
        if name in results and len(values) > 1:
            results[name].setdefault("request_samples_ms", []).extend(values)
    for name, item in results.items():
        if "AggregationSamples" in name:
            item["measurement_scope"] = "in-memory aggregation microbenchmark"
        elif "AccountSummaries" in name or "AccountBundleMetadata" in name or "/detail_" in name or "/page_" in name:
            item["measurement_scope"] = "storage/summary method; excludes HTTP routing and response encoding"
        else:
            item["measurement_scope"] = "HTTP handler via httptest; excludes network transit"
        values = sorted(item.get("request_samples_ms", []))
        if values:
            item["pooled_latency"] = {"samples":len(values), "p50_ms":values[(len(values)-1)//2],
                                      "p95_ms":values[(len(values)*95+99)//100-1]}
    return results


def remote_job(client, remote, envpath, binary, command, output, private, timeout=1500):
    if any(value.startswith("-test.bench=") for value in command):
        deadline = time.monotonic() + 1800
        while run(client, "docker ps -q --filter label=codex-agent.integration=system-performance-race").strip():
            if time.monotonic() > deadline:
                raise TimeoutError("Race checks still running; performance must run separately")
            time.sleep(5)
    name = "codex-system-perf-" + secrets.token_hex(6)
    image = run(client, "docker image inspect --format '{{.Id}}' postgres:18-alpine").strip()
    args = ["docker", "run", "-d", "--name", name, "--label", "codex-agent.integration=system-performance",
            "--cpus", "2", "--memory", "2g", "--pids-limit", "128", "--network", "host",
            "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--env-file", envpath,
            "-v", remote + ":/evidence:ro", "--entrypoint", "/evidence/" + binary, image, *command]
    identifier = ""
    try:
        identifier = run(client, shlex.join(args)).strip()
        if not re.fullmatch(r"[0-9a-f]{64}", identifier):
            raise RuntimeError("Unexpected isolated container ID")
        deadline = time.monotonic() + timeout
        while True:
            state = json.loads(run(client, "docker inspect --format '{{json .State}}' " + identifier))
            if not state["Running"]:
                break
            if time.monotonic() > deadline:
                raise TimeoutError("Synthetic check exceeded its deadline")
            time.sleep(5)
        raw = scrub(run(client, "docker logs " + identifier + " 2>&1", timeout=90), private)
        output.write_text(raw, encoding="utf-8")
        return state["ExitCode"], raw
    finally:
        if identifier and re.fullmatch(r"[0-9a-f]{64}", identifier):
            run(client, "docker rm -f " + identifier)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--baseline", type=Path, required=True)
    parser.add_argument("--candidate", type=Path, default=ROOT, help="Optionally reuse an already frozen implementation")
    parser.add_argument("--output", type=Path)
    parser.add_argument("--benchtime", default="10x")
    parser.add_argument("--count", type=int, default=5)
    parser.add_argument("--bench", default=BENCH_PATTERN)
    parser.add_argument("--verify-only", action="store_true")
    parser.add_argument("--skip-verify", action="store_true")
    args = parser.parse_args()
    stamp = dt.datetime.now(dt.timezone.utc).strftime("%Y%m%dT%H%M%S") + "-" + secrets.token_hex(3)
    output = (args.output or ROOT / ".local/system-perf-20260927" / stamp).resolve()
    output.mkdir(parents=True, exist_ok=False)
    remote = REMOTE + "/checks/system-performance-" + stamp.lower()
    values, private = isolated_environment("codex-agent:integration:system-performance:" + stamp.lower() + ":")
    values.update(CGO_ENABLED="0", GOMAXPROCS="2", STATS_TIMEZONE="UTC")
    report = {"status": "running", "output": str(output), "synthetic_only": True, "production_touched": False,
              "measurement": "per-benchmark scope recorded separately; same-host isolated PostgreSQL; no production inference",
              "driver_resources": {"cpus": 2, "memory": "2g"}, "go_version": subprocess.check_output(["go", "version"], text=True).strip(),
              "benchtime": args.benchtime, "count": args.count, "variants": {}}
    client = None
    envpath = remote + "/test.env"
    try:
        variants = ["candidate"] if args.verify_only else ["baseline", "candidate"]
        for label in variants:
            folder = output / label
            source = folder / "source"
            digest = freeze(args.baseline.resolve() if label == "baseline" else args.candidate.resolve(), source)
            packed, binary_hash = build(source, folder)
            report["variants"][label] = {"source_sha256": digest, "binary_sha256": binary_hash}
            print(json.dumps({"built": label, "sha256": digest}), flush=True)
        client = connect()
        run(client, "test -f " + shlex.quote(REMOTE + "/.codex-agent-integration"))
        report["database_container"] = json.loads(run(client, "docker inspect --format '{{json .HostConfig}}' codex-agent-integration-postgres"))
        report["database_container"] = {k:report["database_container"].get(k) for k in ("Memory","NanoCpus","CpuQuota","CpuPeriod")}
        report["database_version"] = run(client, "docker exec codex-agent-integration-postgres psql -U postgres -Atc 'SHOW server_version'").strip()
        run(client, "install -d -m 700 " + shlex.quote(remote))
        write_remote(client, envpath, "".join(k + "=" + v + "\n" for k, v in values.items()))
        for label in variants:
            folder = output / label
            with client.open_sftp() as sftp:
                sftp.put(str(folder / "server.test.gz"), remote + "/" + label + ".gz")
            run(client, "gzip -dc " + shlex.quote(remote + "/" + label + ".gz") + " > " + shlex.quote(remote + "/" + label) + "\nchmod 700 " + shlex.quote(remote + "/" + label))
            got = run(client, "sha256sum " + shlex.quote(remote + "/" + label)).split()[0]
            if got != report["variants"][label]["binary_sha256"]:
                raise RuntimeError("Transferred test binary hash mismatch")
        if not args.skip_verify:
            selected = management_tests(output / "candidate/source")
            code, raw = remote_job(client, remote, envpath, "candidate",
                ["-test.run", "^(" + "|".join(selected) + ")$", "-test.v", "-test.count=1", "-test.timeout=20m"],
                output / "correctness.log", private)
            report["correctness"] = {"exit_code": code, "selected": len(selected),
                "pass": re.findall(r"^--- PASS: (\S+)", raw, re.M), "fail": re.findall(r"^--- FAIL: (\S+)", raw, re.M),
                "skip": re.findall(r"^--- SKIP: (\S+)", raw, re.M)}
            print(json.dumps({"correctness_exit": code, "selected": len(selected), "failed": report["correctness"]["fail"]}), flush=True)
            if code:
                raise RuntimeError("Management correctness failed; inspect correctness.log")
        if not args.verify_only:
            for label in variants:
                print(json.dumps({"benchmark_started": label}), flush=True)
                code, raw = remote_job(client, remote, envpath, label,
                    ["-test.run=^$", "-test.bench=" + args.bench, "-test.benchmem", "-test.benchtime=" + args.benchtime,
                     "-test.count=" + str(args.count), "-test.timeout=20m"], output / label / "benchmark.log", private)
                report["variants"][label]["benchmarks"] = parse_bench(raw)
                if code or not report["variants"][label]["benchmarks"]:
                    raise RuntimeError(label + " benchmark failed or produced no measurements")
            before, after = (report["variants"][label]["benchmarks"] for label in variants)
            report["comparison"] = {name: {"before": before[name]["median"], "after": after[name]["median"],
                "measurement_scope": after[name]["measurement_scope"],
                "speedup": before[name]["median"]["ns/op"] / after[name]["median"]["ns/op"],
                "latency_reduction_percent": (1-after[name]["median"]["ns/op"]/before[name]["median"]["ns/op"])*100}
                for name in sorted(before.keys() & after.keys())}
            if before.keys() != after.keys():
                raise RuntimeError("Baseline and candidate benchmark coverage differ")
        report["status"] = "pass"
    except Exception as error:
        report.update(status="fail", error=scrub(str(error), private))
    finally:
        if client:
            try:
                with client.open_sftp() as sftp: sftp.remove(envpath)
                report["temporary_credentials_removed"] = True
            except Exception:
                report["temporary_credentials_removed"] = False
                report["status"] = "fail"
            client.close()
        (output / "report.json").write_text(json.dumps(report, indent=2), encoding="utf-8")
    print(json.dumps({"status":report["status"],"report":str(output / "report.json"),"error":report.get("error")}), flush=True)
    return 0 if report["status"] == "pass" else 1


if __name__ == "__main__":
    raise SystemExit(main())
