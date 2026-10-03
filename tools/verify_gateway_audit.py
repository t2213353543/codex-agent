"""Run reproducible local gateway checks, with explicit optional native/CLI tests.

Default: offline Go tests/vet and native source provenance. No deployment,
credential discovery, production configuration, live model calls, or pip install.
Go/Cargo dependencies must already be present locally. JSON on stdout and disk;
progress on stderr. Optional binaries are only exercised by loopback fixtures.
"""
from __future__ import annotations

from project_layout import backend_source_paths, server_package

import argparse
from collections import Counter
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
import time

ROOT = Path(__file__).resolve().parents[1]
FUZZ_TARGETS = (
    "FuzzAuditSSEFragmentedOpaqueResponse", "FuzzAuditOutputIdentityState",
    "FuzzGatewayAuditSSEChunkInvariant", "FuzzGatewayAuditNativeIPCFrames",
)


def source_manifest() -> dict:
    """Hash explicit build/test inputs, never config, account exports or secrets."""
    files = set(backend_source_paths(ROOT))
    native = ROOT / "native-transport"
    for directory in (native / "src", native / "vendor", native / "tools", native / "tests"):
        if directory.exists():
            files.update(p for p in directory.rglob("*") if p.is_file() and
                         p.suffix in {".rs", ".rsinc", ".py", ".toml", ".go"})
    for name in ("Cargo.toml", "Cargo.lock", "UPSTREAM.json", "LOCKED-PACKAGES.json",
                 "WEBSOCKET-ADAPTER.patch", "rust-toolchain.toml"):
        files.add(native / name)
    files.update(ROOT / "tools" / name for name in
                 ("verify_gateway_audit.py", "verify_native_provenance.py",
                  "test_verify_native_provenance.py", "test_verify_gateway_audit.py", "project_layout.py"))
    hashes = {p.relative_to(ROOT).as_posix(): hashlib.sha256(p.read_bytes()).hexdigest()
              for p in sorted(files) if p.is_file()}
    digest = hashlib.sha256(json.dumps(hashes, sort_keys=True, separators=(",", ":")).encode()).hexdigest()
    return {"sha256": digest, "file_count": len(hashes), "files": hashes}


def local_environment() -> dict[str, str]:
    # Clear every opt-in test environment, including inherited DB/live endpoints.
    # Keep OS/toolchain configuration and caches; do not read user config files.
    excluded = {"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY", "DATABASE_URL",
                "REDIS_URL", "PGHOST", "PGPORT", "PGUSER", "PGPASSWORD", "PGDATABASE",
                "GOFLAGS", "CARGO_BUILD_RUSTFLAGS", "RUSTFLAGS"}
    env = {key: value for key, value in os.environ.items()
           if not key.upper().startswith(("CODEX_", "GATEWAY_", "TEST_", "OPENAI_", "ANTHROPIC_"))
           and key.upper() not in excluded
           and not key.upper().endswith(("_API_KEY", "_ACCESS_TOKEN", "_SECRET"))}
    env.update(GOPROXY="off", GOSUMDB="off", GOTOOLCHAIN="local", CARGO_NET_OFFLINE="true",
               RUSTUP_AUTO_INSTALL="0", NO_PROXY="*", no_proxy="*")
    return env


def make_plan(args: argparse.Namespace) -> list[dict]:
    base = ["go", "test", "./...", "-json", "-count=1", "-timeout=20m"]
    if args.race:
        base.append("-race")
    provenance = [sys.executable, str(ROOT / "tools/verify_native_provenance.py"),
                  "--source", str(args.source)]
    if args.native_bin:
        provenance.extend(["--helper-bin", str(args.native_bin)])
    if args.cli_bin:
        provenance.extend(["--cli-bin", str(args.cli_bin)])
    plan = [
        {"name": "go_tests", "command": base, "cwd": str(ROOT), "go_json": True},
        {"name": "go_vet", "command": ["go", "vet", "./..."], "cwd": str(ROOT)},
        {"name": "native_source_provenance", "command": provenance, "cwd": str(ROOT)},
    ]
    if args.rust:
        plan.append({"name": "rust_tests", "command": ["cargo", "test", "--locked", "--offline", "--release", "--bin", "codex-native-transport"],
                     "cwd": str(ROOT / "native-transport")})
    for target in FUZZ_TARGETS if args.fuzz_seconds else ():
        plan.append({"name": target, "command": ["go", "test", server_package(ROOT), "-json", "-run=^$",
                     "-fuzz=^" + target + "$", f"-fuzztime={args.fuzz_seconds}s", "-parallel=2",
                     f"-timeout={args.fuzz_seconds + 180}s"], "cwd": str(ROOT), "go_json": True})
    return plan


def run_check(check: dict, env: dict[str, str], log_dir: Path) -> dict:
    started = time.monotonic()
    logfile = log_dir / (check["name"] + ".log")
    result = {"name": check["name"], "command": check["command"], "cwd": check["cwd"],
              "log": str(logfile), "status": "fail"}
    print("RUN " + check["name"], file=sys.stderr, flush=True)
    counts: Counter = Counter()
    terminal = []
    reasons = {}
    try:
        with logfile.open("w", encoding="utf-8") as log:
            process = subprocess.Popen(check["command"], cwd=check["cwd"], env=env,
                                       stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                                       text=True, encoding="utf-8", errors="replace", shell=False)
            try:
                for line in process.stdout:
                    log.write(line)
                    if check.get("go_json"):
                        try:
                            event = json.loads(line)
                        except json.JSONDecodeError:
                            continue
                        name = event.get("Test")
                        action = event.get("Action")
                        if name and action == "output":
                            reasons[name] = event.get("Output", "").strip()
                        if name and action in {"pass", "skip", "fail"}:
                            counts[action] += 1
                            record = {"test": name, "status": action}
                            if action in {"skip", "fail"}:
                                record["last_output"] = reasons.get(name, "")
                            terminal.append(record)
                code = process.wait()
            except BaseException:
                process.terminate()
                try:
                    process.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait()
                raise
        result.update(exit_code=code, status="pass" if code == 0 else "fail")
        if check.get("go_json"):
            result.update(test_counts={key: counts[key] for key in ("pass", "skip", "fail")}, tests=terminal)
            if code == 0 and not terminal:
                result.update(status="fail", error="Go returned success without any test/fuzz result")
        elif check["name"] == "native_source_provenance" and code == 0:
            evidence = json.loads(logfile.read_text(encoding="utf-8"))
            if evidence.get("passed") is not True:
                result.update(status="fail", error="Provenance did not attest a successful check")
            result["evidence"] = evidence
    except (OSError, ValueError) as error:
        result["error"] = str(error)
    result["seconds"] = round(time.monotonic() - started, 3)
    print(result["status"].upper() + " " + check["name"], file=sys.stderr, flush=True)
    return result


def parser() -> argparse.ArgumentParser:
    result = argparse.ArgumentParser(description=__doc__)
    result.add_argument("--native-bin", type=Path, help="explicit locally built helper; enables native loopback tests")
    result.add_argument("--cli-bin", type=Path, help="explicit official CLI; enables local CLI fixtures and TLS comparison")
    result.add_argument("--source", type=Path, default=ROOT / ".local/codex-source-0.155.0",
                        help="local pinned official CLI source snapshot")
    result.add_argument("--race", action="store_true", help="enable the Go race detector (requires supported C toolchain)")
    result.add_argument("--rust", action="store_true", help="run cached Rust helper tests --locked --offline --release")
    result.add_argument("--fuzz-seconds", type=int, default=0, help="run EACH of four fuzz targets for this many seconds")
    result.add_argument("--output", type=Path, help="JSON report path; default .local/gateway-audit-20260924/verification-TIMESTAMP.json")
    result.add_argument("--dry-run", action="store_true", help="print exact command plan without executing checks")
    return result


def main(argv: list[str] | None = None) -> int:
    argparser = parser()
    args = argparser.parse_args(argv)
    if args.fuzz_seconds < 0:
        argparser.error("--fuzz-seconds must be nonnegative")
    for name in ("source", "native_bin", "cli_bin"):
        value = getattr(args, name)
        if value:
            setattr(args, name, value.resolve())
    stamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%S.%fZ")
    output = (args.output or ROOT / ".local/gateway-audit-20260924" / f"verification-{stamp}.json").resolve()
    plan = make_plan(args)
    if args.dry_run:
        print(json.dumps({"mode": "dry_run", "checks_executed": 0, "plan": plan,
                          "explicit_native": bool(args.native_bin), "explicit_cli": bool(args.cli_bin),
                          "offline_dependencies": True}, indent=2))
        return 0
    before = source_manifest()
    output.parent.mkdir(parents=True, exist_ok=True)
    log_dir = output.parent / (output.stem + "-logs")
    log_dir.mkdir(exist_ok=True)
    env = local_environment()
    results = []
    valid_bins = True
    for name in ("native_bin", "cli_bin"):
        path = getattr(args, name)
        if path and not path.is_file():
            results.append({"name": name, "status": "fail", "error": "Explicit binary does not exist: " + str(path)})
            valid_bins = False
    if args.native_bin and valid_bins:
        env.update(CODEX_NATIVE_HTTP_TEST_BIN=str(args.native_bin), CODEX_NATIVE_HELPER_BIN=str(args.native_bin))
    if args.cli_bin and valid_bins:
        env.update(CODEX_NATIVE_CAPTURE_BIN=str(args.cli_bin), CODEX_REAL_CLI_BIN=str(args.cli_bin))
    for check in plan:
        results.append(run_check(check, env, log_dir))
    test_results = next((item.get("tests", []) for item in results if item["name"] == "go_tests"), [])
    parents = {"/".join(test["test"].split("/")[:index]) for test in test_results
               for index in range(1, len(test["test"].split("/")))}
    for name, requested, predicate in (
        ("native_runtime", bool(args.native_bin), lambda n: n.startswith("TestGatewayNativeActual")),
        ("cli_runtime", bool(args.cli_bin), lambda n: n in {"TestRealCodexCLI", "TestSourceNativeCLITransport"}),
        ("http_tls_parity", bool(args.native_bin and args.cli_bin), lambda n: n == "TestSourceNativeHelperTLSComparison"),
        ("ws_tls_parity", bool(args.native_bin and args.cli_bin), lambda n: n == "TestGatewayNativeActualWebSocketTLSParity"),
    ):
        # A Go parent passes even when every native subtest skips. Only leaf
        # results can establish that an optional runtime was actually exercised.
        selected = [test for test in test_results if test["test"] not in parents
                    and predicate(test["test"].split("/", 1)[0])]
        counts = Counter(test["status"] for test in selected)
        status = "skip" if not requested else "fail" if counts["fail"] else "pass" if counts["pass"] else "skip"
        results.append({"name": name, "status": status, "requested": requested,
                        "test_counts": {key: counts[key] for key in ("pass", "skip", "fail")},
                        "reason": "Results of matching real tests above; platform/tool prerequisites can skip individual cases." if requested
                        else "Not requested: supply explicit --native-bin and/or --cli-bin as required."})
    if not args.rust:
        results.append({"name": "rust_tests", "status": "skip", "reason": "Enable with --rust; cached Rust toolchain required."})
    if not args.race:
        results.append({"name": "race_detector", "status": "skip", "reason": "Enable with --race; supported C toolchain required."})
    if not args.fuzz_seconds:
        results.append({"name": "fuzz_mutations", "status": "skip", "reason": "Seed tests run normally; enable mutations with --fuzz-seconds N."})
    after = source_manifest()
    if before["sha256"] != after["sha256"]:
        results.append({"name": "source_stability", "status": "fail", "error": "Source changed during verification; rerun on stable inputs."})
    else:
        results.append({"name": "source_stability", "status": "pass"})
    checks = Counter(item["status"] for item in results)
    tests = Counter(test["status"] for item in results if item["name"] == "go_tests" or item["name"] in FUZZ_TARGETS
                    for test in item.get("tests", []))
    failed = checks["fail"] or tests["fail"]
    incomplete = checks["skip"] or tests["skip"]
    report = {"schema": "gateway-local-audit-v1", "created_utc": stamp,
              "status": "failed" if failed else "passed_with_skips" if incomplete else "passed",
              "passed": not bool(failed), "all_checks_executed": not bool(incomplete),
              "checks": {key: checks[key] for key in ("pass", "skip", "fail")},
              "tests": {key: tests[key] for key in ("pass", "skip", "fail")},
              "source_before": before, "source_after_sha256": after["sha256"],
              "results": results, "output": str(output),
              "limits": ["This runner never enables live model, database, Redis or production tests.",
                         "Source provenance alone does not establish TLS wire parity.",
                         "Missing optional binaries/platform prerequisites are reported as skips."]}
    encoded = json.dumps(report, indent=2, ensure_ascii=False) + "\n"
    output.write_text(encoded, encoding="utf-8")
    print(f"checks pass={checks['pass']} skip={checks['skip']} fail={checks['fail']}; "
          f"tests pass={tests['pass']} skip={tests['skip']} fail={tests['fail']}; "
          f"source={before['sha256']}; report={output}", file=sys.stderr)
    print(encoded, end="")
    return 1 if failed else 0


if __name__ == "__main__":
    raise SystemExit(main())
