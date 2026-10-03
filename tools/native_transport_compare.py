"""Compare the official Linux CLI and built helper in a loopback-only container."""
from __future__ import annotations

from project_layout import server_package

import argparse
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import secrets
import subprocess

from gateway_source_race import go_manifest

ROOT = Path(__file__).resolve().parents[1]
LABEL = "codex-agent.integration=native-tls-compare"


def sha(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def command(arguments, timeout=120, check=True, **kwargs):
    result = subprocess.run(arguments, capture_output=True, timeout=timeout, **kwargs)
    if check and result.returncode:
        raise RuntimeError("Comparison command failed: " + result.stderr.decode(errors="replace")[-1600:])
    return result


def comparison_reports_passed(evidence, required):
    return (evidence.get("exit_code") == 0 and evidence.get("oom_killed") is False
            and evidence.get("skipped_tests") == []
            and all(evidence.get(name + "_report_present") is True
                    and evidence.get(name + "_passed") is True for name in required))


def required_comparison_reports(suite, files):
    required = ["comparison"]
    if suite == "gateway":
        for source, report in (
            ("real_cli_websocket_test.go", "native_ws_cli"),
            ("gateway_native_compression_cli_test.go", "native_compressed_cli"),
            ("gateway_native_cli_edit_test.go", "native_edit_cli"),
            ("gateway_native_cli_compact_test.go", "native_compact_cli"),
            ("gateway_native_cli_fallback_test.go", "native_fallback_cli"),
            ("gateway_native_cli_parallel_test.go", "native_parallel_cli"),
        ):
            if source in files:
                required.append(report)
    return required


def validate_frozen_source(source, manifest):
    actual = go_manifest(source)
    if actual["files"] != manifest["files"] or actual["sha256"] != manifest["sha256"]:
        raise RuntimeError("Frozen Go source set or manifest checksum changed")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--native-run", required=True)
    parser.add_argument("--source-dir", required=True, type=Path)
    parser.add_argument("--suite", choices=["tls", "gateway"], default="tls")
    parser.add_argument("--cli-version", choices=["0.154.0", "0.155.0"], default="0.155.0")
    parser.add_argument("--with-go-cli", action="store_true", help="also run the official CLI compatibility scenarios over Go transport")
    args = parser.parse_args()
    if args.with_go_cli and args.suite != "gateway":
        parser.error("--with-go-cli requires --suite gateway")
    native = (ROOT / ".local/native-local-build" / args.native_run).resolve()
    if not native.is_relative_to((ROOT / ".local/native-local-build").resolve()):
        raise RuntimeError("Invalid native run path")
    result = json.loads((native / "result.json").read_text())
    if "build" not in result["build"]["completed"]:
        raise RuntimeError("Native helper compilation has not completed")
    run = json.loads((native / "run.json").read_text())
    profile = run.get("profile", "debug")
    if profile not in {"debug", "release"}: raise RuntimeError("Unknown native build profile")
    helper = native / ("target-" + profile + "-codex-native-transport")
    recorded = next(a["sha256"] for a in result["artifacts"] if a["path"].endswith(helper.name))
    if sha(helper) != recorded:
        raise RuntimeError("Built helper artifact changed")
    official = ROOT / (".local/native-transport-build/official-codex-linux-" + args.cli_version)
    provenance = "official-cli-source.json" if args.cli_version == "0.154.0" else "official-cli-" + args.cli_version + "-source.json"
    source = json.loads((official.parent / provenance).read_text())
    if sha(official) != source["binary_sha256"]:
        raise RuntimeError("Official CLI artifact changed")
    frozen = args.source_dir.resolve()
    manifest = json.loads((frozen.parent / "source-manifest.json").read_text())
    validate_frozen_source(frozen, manifest)
    stamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%S") + "-" + secrets.token_hex(3)
    output = ROOT / ".local/native-transport-build" / (args.suite + "-compare-" + stamp)
    output.mkdir(parents=True)
    probe = output / "native-helper-probe.test"
    env = dict(os.environ, GOOS="linux", GOARCH="amd64", CGO_ENABLED="0")
    command(["go", "test", "-c", "-o", str(probe), server_package(frozen)], cwd=frozen, env=env)
    validate_frozen_source(frozen, manifest)
    context = command(["docker", "context", "inspect", "--format", "{{json .Endpoints.docker.Host}}"])
    endpoint = json.loads(context.stdout)
    if os.environ.get("DOCKER_HOST") or not endpoint.startswith("npipe:////./pipe/docker"):
        raise RuntimeError("Comparison requires the local Docker Desktop engine")
    name = "codex-native-compare-" + stamp.lower()
    identifier = ""
    evidence = {"suite": args.suite, "source_sha256": manifest["sha256"], "helper_sha256": sha(helper),
        "official_cli_sha256": sha(official), "probe_sha256": sha(probe),
        "native_build_run": args.native_run, "network": "none", "production_access": False,
        "go_cli_compatibility_requested": args.with_go_cli,
        "native_loopback_passed_before_comparison": result["state"]["ExitCode"] == 0}
    try:
        test_selector = "^TestSourceNativeHelperTLSComparison$" if args.suite == "tls" else "^TestGatewayNativeActual"
        if args.with_go_cli:
            test_selector += "|^TestRealCodexCLI$"
        created = command(["docker", "create", "--name", name, "--label", LABEL, "--network", "none",
            "--cpus", "1", "--memory", "1g", "--pids-limit", "128", "--cap-drop", "ALL",
            "--security-opt", "no-new-privileges", "-e", "CODEX_NATIVE_CAPTURE_BIN=/official-codex",
            "-e", "CODEX_REAL_CLI_BIN=/official-codex",
            "-e", "CODEX_NATIVE_HELPER_BIN=/native-helper", "-e", "CODEX_NATIVE_HELPER_REPORT=/comparison.json",
            "-e", "CODEX_NATIVE_HTTP_TEST_BIN=/native-helper", "-e", "CODEX_NATIVE_HTTP_CLI_REPORT=/gateway-cli.json",
            "-e", "CODEX_NATIVE_WS_CLI_REPORT=/gateway-ws-cli.json",
            "-e", "CODEX_NATIVE_EDIT_CLI_REPORT=/gateway-edit-cli.json",
            "-e", "CODEX_NATIVE_COMPACT_CLI_REPORT=/gateway-compact-cli.json",
            "-e", "CODEX_NATIVE_FALLBACK_CLI_REPORT=/gateway-fallback-cli.json",
            "-e", "CODEX_NATIVE_PARALLEL_CLI_REPORT=/gateway-parallel-cli.json",
            "-e", "CODEX_NATIVE_COMPRESSION_NETWORK_NONE=1", "-e", "CODEX_NATIVE_COMPRESSION_CHAIN_REPORT=/gateway-compression-cli.json",
            result["image_id"], "sh", "-c", "chmod 500 /official-codex /native-helper /probe.test\n"
            "exec /probe.test -test.v -test.timeout=4m -test.run='" + test_selector + "'"])
        identifier = created.stdout.decode().strip()
        evidence["container"] = identifier
        (output / "run.json").write_text(json.dumps(evidence, indent=2) + "\n")
        for local, remote in [(official, "/official-codex"), (helper, "/native-helper"), (probe, "/probe.test")]:
            command(["docker", "cp", str(local), identifier + ":" + remote])
        command(["docker", "start", identifier])
        print(json.dumps({"started": True, "container": identifier, "report_directory": str(output)}), flush=True)
        command(["docker", "wait", identifier], timeout=300)
        state = json.loads(command(["docker", "inspect", "--format", "{{json .State}}", identifier]).stdout)
        evidence["exit_code"], evidence["oom_killed"] = state["ExitCode"], state["OOMKilled"]
        log = command(["docker", "logs", identifier])
        (output / "probe.log").write_bytes(log.stdout + log.stderr)
        lines = (log.stdout + log.stderr).decode(errors="replace").splitlines()
        evidence["skipped_tests"] = [line.strip() for line in lines if "--- SKIP:" in line]
        evidence["passed_top_level_tests"] = [line for line in lines if line.startswith("--- PASS:")]
        evidence["passed_native_top_level_tests"] = [line for line in evidence["passed_top_level_tests"]
                                                    if line.startswith("--- PASS: TestGatewayNativeActual")]
        evidence["passed_go_cli_compatibility_top_level_tests"] = [line for line in evidence["passed_top_level_tests"]
                                                                  if line.startswith("--- PASS: TestRealCodexCLI ")]
        filename = "comparison.json" if args.suite == "tls" else "gateway-cli.json"
        copied = command(["docker", "cp", identifier + ":/" + filename, str(output / filename)], check=False)
        evidence["comparison_report_present"] = copied.returncode == 0
        if copied.returncode == 0:
            comparison = json.loads((output / filename).read_text())
            evidence["comparison_passed"] = comparison.get("passed") is True
        required_reports = required_comparison_reports(args.suite, manifest["files"])
        if args.suite == "gateway":
            ws_report = output / "gateway-ws-cli.json"
            ws_copy = command(["docker", "cp", identifier + ":/gateway-ws-cli.json", str(ws_report)], check=False)
            evidence["native_ws_cli_report_present"] = ws_copy.returncode == 0
            if ws_copy.returncode == 0:
                evidence["native_ws_cli_passed"] = json.loads(ws_report.read_text()).get("passed") is True
            compression_report = output / "gateway-compression-cli.json"
            compression_copy = command(["docker", "cp", identifier + ":/gateway-compression-cli.json", str(compression_report)], check=False)
            evidence["native_compressed_cli_report_present"] = compression_copy.returncode == 0
            if compression_copy.returncode == 0:
                evidence["native_compressed_cli_passed"] = json.loads(compression_report.read_text()).get("passed") is True
            if "gateway_native_cli_edit_test.go" in manifest["files"]:
                edit_report = output / "gateway-edit-cli.json"
                edit_copy = command(["docker", "cp", identifier + ":/gateway-edit-cli.json", str(edit_report)], check=False)
                evidence["native_edit_cli_report_present"] = edit_copy.returncode == 0
                if edit_copy.returncode == 0:
                    evidence["native_edit_cli_passed"] = json.loads(edit_report.read_text()).get("passed") is True
            if "gateway_native_cli_compact_test.go" in manifest["files"]:
                compact_report = output / "gateway-compact-cli.json"
                compact_copy = command(["docker", "cp", identifier + ":/gateway-compact-cli.json", str(compact_report)], check=False)
                evidence["native_compact_cli_report_present"] = compact_copy.returncode == 0
                if compact_copy.returncode == 0:
                    evidence["native_compact_cli_passed"] = json.loads(compact_report.read_text()).get("passed") is True
            if "gateway_native_cli_fallback_test.go" in manifest["files"]:
                fallback_report = output / "gateway-fallback-cli.json"
                fallback_copy = command(["docker", "cp", identifier + ":/gateway-fallback-cli.json", str(fallback_report)], check=False)
                evidence["native_fallback_cli_report_present"] = fallback_copy.returncode == 0
                if fallback_copy.returncode == 0:
                    evidence["native_fallback_cli_passed"] = json.loads(fallback_report.read_text()).get("passed") is True
            if "gateway_native_cli_parallel_test.go" in manifest["files"]:
                parallel_report = output / "gateway-parallel-cli.json"
                parallel_copy = command(["docker", "cp", identifier + ":/gateway-parallel-cli.json", str(parallel_report)], check=False)
                evidence["native_parallel_cli_report_present"] = parallel_copy.returncode == 0
                if parallel_copy.returncode == 0:
                    evidence["native_parallel_cli_passed"] = json.loads(parallel_report.read_text()).get("passed") is True
        evidence["required_reports"] = required_reports
        validate_frozen_source(frozen, manifest)
        evidence["source_hash_unchanged_after_validation"] = True
        evidence["passed"] = comparison_reports_passed(evidence, required_reports)
        if args.with_go_cli:
            evidence["passed"] = evidence["passed"] and len(evidence["passed_go_cli_compatibility_top_level_tests"]) == 1
    finally:
        if identifier:
            command(["docker", "rm", "-f", identifier])
            evidence["container_removed"] = True
        (output / "result.json").write_text(json.dumps(evidence, indent=2) + "\n")
    print(json.dumps(evidence), flush=True)
    return 0 if evidence.get("passed") else 1


if __name__ == "__main__":
    raise SystemExit(main())
