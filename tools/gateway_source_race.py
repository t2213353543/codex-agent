"""Frozen Go-only race checks against isolated synthetic PG/Redis; no deployment."""
from __future__ import annotations

from project_layout import server_package

import argparse
from datetime import datetime, timezone
import fnmatch
import hashlib
import io
import json
import os
from pathlib import Path
import re
import secrets
import shlex
import tarfile
import time
from urllib.parse import urlsplit

from integration_env import ROOT, REMOTE, connect, run, write_remote
from run_linux_checks import scrub


IMAGE = "codex-agent-integration-race:ce864e7223ac17b1-v1"
EXPECTED_IMAGE_ID = "sha256:eb1d21756fc64fbd95138b52362d6952f31898a666d57b5bdc02eb22bdf4f556"
OUTPUT_ROOT = ROOT / ".local/source-20260918"
TEST_FILES = (
    "gateway_admission_identity_test.go", "gateway_client_pool_test.go", "gateway_http1_burst_test.go",
    "gateway_timeout_pool_test.go", "gateway_h1_terminal_diagnostic_test.go",
    "gateway_native_http_test.go", "gateway_native_factory_test.go",
    "gateway_native_startup_test.go",
    "gateway_native_start_wait_test.go",
    "gateway_native_proxy_dispatch_test.go",
    "gateway_native_socks_test.go", "gateway_native_ws_test.go",
    "gateway_native_ws_rejection_test.go",
    "gateway_native_ws_budget_test.go",
    "gateway_owner_identity_test.go", "accounts_shutdown_test.go",
    "gateway_unknown_owner_test.go", "gateway_native_resource_test.go",
    "gateway_owner_revision*test.go", "gateway_owner_cache_test.go",
    "gateway_request_compression*test.go",
    "gateway_session_authorization*test.go",
    "gateway_selection_authorization*test.go",
    "gateway_key_authorization*test.go",
    "gateway_recovery_authorization*test.go",
    "gateway_bound_cancellation*test.go",
    "gateway_ws_response*test.go",
    "gateway_refresh_wait*test.go", "gateway_refresh_preflight*test.go",
    "gateway_refresh_context*test.go", "gateway_refresh_admission*test.go",
    "gateway_warmup*test.go", "admin_billing_disabled_test.go",
    "gateway_custom_tool_test.go", "response_output_test.go",
    "accounts_import_test.go", "accounts_review_test.go", "admin_accounts_management_test.go",
    "gateway_attempt*test.go",
    "gateway_request_route*test.go", "gateway_cache*test.go", "gateway_policy_test.go",
    "gateway_observation*test.go", "gateway_refresh_cooldown_test.go", "gateway_cooldown_test.go",
    "gateway_retry_after_test.go", "source_ready_test.go", "source_refresh_test.go",
    "source_unauthorized_test.go", "gateway_cli_test.go", "gateway_session_lease_test.go",
    "gateway_websocket_test.go", "cli_transport_test.go", "codex_fingerprint_test.go",
    "accounts_pg_test.go", "storage_runtime_test.go", "settings_runtime_review_test.go",
)
REQUIRED_TESTS = {
    "TestPGGatewayRefreshCooldownSharesFailureAndReimportClears", "TestGatewayCooldownRedisSharing",
    "TestPGGatewayCooldownSelectionBindingAndReplacement",
    "TestGatewayRedisSessionSharing", "TestPGAccountCredentialsAndMetadataIsolation",
    "TestGatewayAdmissionSurvivesAccountRename", "TestGatewayHTTP1BurstIdleExperiment",
    "TestGatewayClientPoolReusesHTTP2TunnelAfterTerminalSSE",
    "TestGatewayTimeoutPoolComparedWithPrivateVariants", "TestGatewayTimeoutPoolReusesTwoBatches",
    "TestGatewayTimeoutPoolKeepsIndependentDeadlines", "TestGatewayTimeoutPoolRotationGuardsAllVariants",
    "TestGatewayTimeoutPoolBoundAndActiveStream", "TestGatewayTimeoutPoolDefaultsAndCustomRoundTripper",
    "TestGatewayTimeoutPoolCompactTotalTimeoutLeavesHTTPBodyAlive",
    "TestGatewayTimeoutPoolDisabledOrDeletedGuardsAllVariants",
    "TestGatewayH1TerminalConnectionDiagnostic",
    "TestGatewayObservationHooksHTTPProvenanceAndBytes", "TestGatewayObservationHooksFinalHeadersAfter401",
    "TestGatewayObservationHooksTimeoutHasNoHeaders", "TestGatewayObservationHooksErrorsKeepExistingRecords",
    "TestGatewayObservationHooksWebSocketFirstHandshakeOnly", "TestGatewayObservationHooksTransportErrorPreservesProxyOrigin",
    "TestGatewayObservationHooksStreamingReadAndWriteFailures", "TestGatewayObservationHooksWebSocketAbortOrigins",
    "TestGatewayObservationHooksNativeCompactionEndpointScope",
    "TestGatewayNativeHTTPRoundTripAndRouteIsolation", "TestGatewayNativeHTTPProtocolFailuresDoNotBecomeEOF",
    "TestGatewayNativeHTTPContextCapacityAndRetirement", "TestGatewayNativeHTTPConcurrentPoolCreation",
    "TestGatewayNativeStartupCapabilitiesAndLifecycle", "TestGatewayNativeStartupRequiresCleanExit",
    "TestGatewayNativeStartupReapsUnreadyUnixSocket", "TestGatewayNativeStartupPreservesReplacedUnixSocket",
    "TestGatewayNativeStartupFirstCallerCancellationKeepsOtherWaiter",
    "TestGatewayNativeHTTPProxyChangeBeforeDispatch",
    "TestGatewayWebSocketRejectedStatusesPreserveRoutingBoundary",
    "TestGatewayNativeProxyGuardAndRequestPin", "TestGatewayNativeLocalCapacityIsNotAnUpstream502",
    "TestGatewayNativeSOCKSBridgeTunnelAuthTargetAndRemoteDNS",
    "TestGatewayNativeWSIPCUnsolicitedMessageBeforeWriteAck",
    "TestGatewayNativeWSIPCAmbiguousWriteIsNotSuccess",
    "TestGatewayNativeWSIPCBackpressureDoesNotBlockCancellation",
    "TestPGGatewayOwnerIdentityReplacementRejectsSharedSession",
    "TestPGShutdownPersistsRefreshAfterClientDisconnectAndSIGTERM",
    "TestPGGatewayUnknownOwnerCredentialReplacementAndRestart",
    "TestGatewayNativeResourceExitedChildIsReapedWithoutNewTraffic",
    "TestPGGatewayOwnerRevisionReauthorizeABASurvivesFreshGateway",
    "TestPGGatewayOwnerCacheReauthorizeABAWithoutInvalidation",
    "TestPGGatewayWarmupStorageRestartAndRebuild", "TestPGManagedBillingDisabled",
    "TestPGGatewayBoundAccountGroupRevocationWithWarmCache",
    "TestPGGatewaySelectionAuthorizationRejectsStaleCandidates",
    "TestPGGatewaySelectionAuthorizationCancelsBlockedAuthoritativeRead",
    "TestPGGatewayKeyAuthorizationRevokesStaleIndex",
    "TestPGGatewayKeyAuthorizationCancelsBlockedAuthoritativeRead",
    "TestPGGatewayRecoveryAuthorizationRechecksAfterConcurrentOAuthMutation",
    "TestGatewayRecoveryAuthorizationPrecedesBodyCloneAndRetry",
    "TestPGGatewayBoundCancellationInterruptsAuthoritativeRead",
    "TestGatewayBoundCancellationRefreshPreservesCallerAndAccountState",
    "TestPGGatewayBoundCancellationWebSocketDoesNotInventAuthorizationFailure",
    "TestGatewayBoundCancellationPrecedesAccountRead",
    "TestGatewayWSResponseIdentityCompletedIDCannotConsumeNextTurn",
    "TestGatewayWSResponseHistoryBoundedFIFO",
    "TestPGGatewayRefreshWaitCancellation",
    "TestPGGatewayRefreshPreflightCancellationDoesNotPersistFailure",
    "TestGatewayRefreshPreflightRejectsCancelledExchange",
    "TestGatewayCustomToolExplicitIndexCannotDuplicateCompletedCall",
    "TestGatewayCustomToolConflictFailsJSONButKeepsStreamingBytes",
}


def source_files(source_root: Path = ROOT) -> dict[str, str]:
    paths = []
    for directory, subdirs, names in os.walk(source_root):
        subdirs[:] = sorted(name for name in subdirs
                           if not name.startswith(".") and name not in {"frontend", "node_modules", "vendor", "data", "dist", "__pycache__"})
        for name in names:
            path = Path(directory) / name
            relative = path.relative_to(source_root).as_posix()
            if (name.endswith(".go") or relative in {"go.mod", "go.sum",
                    "tools/loophub_bridge_contract_q01.py", "tools/loophub_bridge_reauth_contract.py",
                    "tools/codex_cli_smoke.py", "tools/codex_cli_workload.py",
                    "deploy/loophub_codex_bridge.py"}
                    or relative.startswith("migrations/")):
                if path.is_symlink():
                    raise RuntimeError("Source snapshot cannot contain symlinks")
                paths.append((relative, path))
    return {relative: hashlib.sha256(path.read_bytes()).hexdigest() for relative, path in sorted(paths)}


def go_manifest(source_root: Path = ROOT) -> dict:
    files = source_files(source_root)
    encoded = json.dumps(files, sort_keys=True, separators=(",", ":")).encode()
    return {"schema": "codex-agent-go-only-source-v1", "sha256": hashlib.sha256(encoded).hexdigest(),
            "scope": "Go sources, go.mod, go.sum, migrations and cross-language bridge fixtures; frontend excluded", "files": files}


def missing_required_tests(selected, required=None):
    """Retain the historical coverage gate across an explicitly renamed test."""
    missing = set(REQUIRED_TESTS if required is None else required) - set(selected)
    if "TestGatewayNativeHTTPContextElasticRoutesAndRetirement" in selected:
        missing.discard("TestGatewayNativeHTTPContextCapacityAndRetirement")
    return missing


def selected_tests(manifest: dict, source_root: Path = ROOT) -> list[str]:
    selected = set()
    for relative, expected in manifest["files"].items():
        if not any(fnmatch.fnmatchcase(Path(relative).name, pattern) for pattern in TEST_FILES):
            continue
        raw = (source_root / relative).read_bytes()
        if hashlib.sha256(raw).hexdigest() != expected:
            raise RuntimeError("Go source changed while selecting tests")
        selected.update(name for name in re.findall(r"^func (Test\w+)\(t \*testing.T\)", raw.decode("utf-8"), re.M)
                        if not name.startswith("TestGatewayNativeActual"))
    if missing_required_tests(selected) or len(selected) < 80:
        raise RuntimeError("Focused selector omitted required completed tests")
    return sorted(selected)


def snapshot(output: Path, manifest: dict, source_root: Path = ROOT) -> Path:
    archive = output / "source.tar.gz"
    with tarfile.open(archive, "w:gz") as tar:
        for relative, expected in manifest["files"].items():
            raw = (source_root / relative).read_bytes()
            if hashlib.sha256(raw).hexdigest() != expected:
                raise RuntimeError("Go source changed while freezing")
            entry = tarfile.TarInfo(relative)
            entry.size, entry.mode, entry.mtime = len(raw), 0o644, 0
            tar.addfile(entry, io.BytesIO(raw))
    if go_manifest(source_root)["sha256"] != manifest["sha256"]:
        raise RuntimeError("Go source changed while freezing")
    manifest["archive_sha256"] = hashlib.sha256(archive.read_bytes()).hexdigest()
    (output / "source-manifest.json").write_text(json.dumps(manifest, indent=2) + "\n", encoding="utf-8")
    return archive


def isolated_environment(prefix: str) -> tuple[dict[str, str], list[str]]:
    config = json.loads((ROOT / ".local/integration/environment.json").read_text(encoding="utf-8-sig"))
    if (config.get("purpose") != "codex-agent-integration" or config.get("synthetic_data") is not True
            or config.get("environment") != "isolated-synthetic-only" or config.get("remote_root") != REMOTE):
        raise RuntimeError("Only the isolated synthetic integration environment is allowed")
    configured = {key: str(value) for key, value in config.items()
                  if key.isupper() and isinstance(value, (str, int, float))}
    configured.update({key: str(value) for key, value in config.get("env", {}).items()})
    pg = urlsplit(configured.get("TEST_DATABASE_URL", ""))
    redis = urlsplit(configured.get("TEST_REDIS_URL", ""))
    if (pg.scheme not in {"postgres", "postgresql"} or pg.username != "codex_agent_test"
            or pg.path != "/codex_agent_integration" or pg.hostname not in {"localhost", "127.0.0.1", "::1"}
            or pg.port != 25432):
        raise RuntimeError("PostgreSQL target must use the isolated integration role and database")
    if (redis.scheme != "redis" or redis.username != "codex_agent_test" or redis.port != 26379
            or redis.hostname not in {"localhost", "127.0.0.1", "::1"}):
        raise RuntimeError("Redis target must be the isolated integration instance")
    # Only test endpoint credentials enter this process/container, never live
    # model/account exports, deployment configuration or production API keys.
    values = {key: configured[key] for key in ("TEST_DATABASE_URL", "TEST_REDIS_URL")}
    values.update(TEST_REDIS_PREFIX=prefix, REDIS_KEY_PREFIX=prefix, REDIS_PREFIX=prefix, REDIS_NAMESPACE=prefix,
                  CGO_ENABLED="1", GOMAXPROCS="2", GOFLAGS="-mod=readonly", GOCACHE="/tmp/go-build", GOMODCACHE="/tmp/go-mod")
    if any("\r" in value or "\n" in value for value in values.values()):
        raise RuntimeError("Test environment values cannot contain line breaks")
    private = [*values.values(), pg.password or "", redis.password or ""]
    return values, private


def verify_remote(client) -> str:
    run(client, "test -f " + shlex.quote(REMOTE + "/.codex-agent-integration"))
    image = run(client, "docker image inspect --format '{{.Id}}' " + shlex.quote(IMAGE)).strip()
    if image != EXPECTED_IMAGE_ID:
        raise RuntimeError("Pre-existing compiler image changed; no installation or image build is allowed")
    return image


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    modes = parser.add_mutually_exclusive_group(required=True)
    modes.add_argument("--plan", action="store_true", help="inspect Go-only hash/test selection locally")
    modes.add_argument("--preflight", action="store_true", help="read-only SSH/image check")
    modes.add_argument("--run", action="store_true", help="run after the source owner announces Go freeze")
    parser.add_argument("--expected-go-hash")
    args = parser.parse_args()
    if args.preflight:
        client = connect()
        try:
            print(json.dumps({"status": "ready", "existing_image_id": verify_remote(client), "mutation_performed": False}))
        finally:
            client.close()
        return 0
    manifest = go_manifest()
    selected = selected_tests(manifest)
    if args.plan:
        print(json.dumps({"go_sha256": manifest["sha256"], "files": len(manifest["files"]),
                          "selected_tests": len(selected), "required_tests": sorted(REQUIRED_TESTS), "frontend_included": False}))
        return 0
    if args.expected_go_hash != manifest["sha256"]:
        raise RuntimeError("Run requires the explicitly frozen current Go-only source hash")
    stamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%S") + "-" + secrets.token_hex(3)
    output = OUTPUT_ROOT / ("linux-race-" + stamp)
    output.mkdir(parents=True, exist_ok=False)
    archive = snapshot(output, manifest)
    print(json.dumps({"snapshot_frozen": True, "go_sha256": manifest["sha256"],
                      "manifest": str(output / "source-manifest.json"), "selected_tests": len(selected)}), flush=True)
    prefix = "codex-agent:integration:source-20260918:" + stamp.lower() + ":"
    values, private = isolated_environment(prefix)
    remote = REMOTE + "/checks/source-20260918-" + stamp.lower()
    if not remote.startswith("/opt/codex-agent-integration/checks/"):
        raise RuntimeError("Remote path is outside the allowed checks directory")
    name = "codex-agent-source-race-" + stamp.lower()
    regex = "^(" + "|".join(selected) + ")$"
    report = {"schema": "codex-agent-source-race-v1", "go_source_sha256": manifest["sha256"],
              "source_scope": manifest["scope"], "status": "blocked", "started_at": datetime.now(timezone.utc).isoformat(),
              "scope": "isolated synthetic PG/Redis, no production inference, load test or deployment",
              "selected_top_level_tests": selected, "redis_prefix": prefix, "remote_directory": remote,
              "resources": {"cpus": 2, "memory": "2g", "pids": 128}, "database_role": "codex_agent_test",
              "database_name": "codex_agent_integration"}
    client, identifier, env_written, container_attempted = None, "", False, False
    started = time.monotonic()
    try:
        client = connect()
        image = verify_remote(client)
        report["image_id"] = image
        run(client, "install -d -m 700 " + shlex.quote(remote) + " " + shlex.quote(remote + "/source"))
        with client.open_sftp() as sftp:
            sftp.put(str(archive), remote + "/source.tar.gz")
            sftp.chmod(remote + "/source.tar.gz", 0o600)
        if run(client, "sha256sum " + shlex.quote(remote + "/source.tar.gz")).split()[0] != manifest["archive_sha256"]:
            raise RuntimeError("Uploaded source archive checksum differs")
        write_remote(client, remote + "/source-manifest.json", (output / "source-manifest.json").read_text(encoding="utf-8"))
        env_written = True
        write_remote(client, remote + "/test.env", "".join(key + "=" + value + "\n" for key, value in sorted(values.items())))
        run(client, "tar -xzf " + shlex.quote(remote + "/source.tar.gz") + " -C " + shlex.quote(remote + "/source"))
        test_command = ["go", "test", "-race", "-count=1", "-timeout=8m", "-json", "-run", regex, server_package(source_root)]
        script = "\n".join(["set -eu", "mkdir -p /work", "cp -a /src/. /work/", "cd /work",
                            "go version", "go env GOOS GOARCH CGO_ENABLED CC", shlex.join(test_command)])
        command = ["docker", "run", "-d", "--name", name, "--label", "codex-agent.integration=source-race",
                   "--cpus", "2", "--memory", "2g", "--pids-limit", "128", "--network", "host", "--cap-drop", "ALL",
                   "--security-opt", "no-new-privileges", "--env-file", remote + "/test.env",
                   "-v", remote + "/source:/src:ro", image, "sh", "-c", script]
        container_attempted = True
        identifier = run(client, shlex.join(command)).strip()
        if not re.fullmatch(r"[0-9a-f]{64}", identifier):
            raise RuntimeError("Unexpected temporary container identity")
        print(json.dumps({"started": True, "go_sha256": manifest["sha256"], "selected_tests": len(selected),
                          "report_directory": str(output)}), flush=True)
        deadline = time.monotonic() + 900
        while True:
            state = json.loads(run(client, "docker inspect --format '{{json .State}}' " + identifier))
            if not state["Running"]:
                break
            if time.monotonic() >= deadline:
                raise TimeoutError("Isolated race exceeded 900 seconds")
            time.sleep(10)
        raw = run(client, "docker logs " + identifier + " 2>&1", timeout=60)
        (output / "race.log").write_text(scrub(raw, private), encoding="utf-8")
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
        status = "fail" if state["ExitCode"] or outcomes["fail"] else "blocked" if outcomes["skip"] or missing or not packages else "pass"
        report.update(status=status, exit_code=state["ExitCode"], oom_killed=state.get("OOMKilled", False),
                      passed_test_events=len(outcomes["pass"]), passed_top_level_tests=sum("/" not in name for name in outcomes["pass"]),
                      failed_tests=outcomes["fail"], skipped_tests=outcomes["skip"], missing_tests=missing, passed_packages=packages)
    except Exception as error:
        report.update(status="fail", error=scrub(type(error).__name__ + ": " + str(error), private))
        if client and identifier:
            try:
                raw = run(client, "docker logs " + identifier + " 2>&1", timeout=60)
                (output / "race.log").write_text(scrub(raw, private), encoding="utf-8")
            except Exception:
                pass
    finally:
        if client:
            if container_attempted:
                try:
                    # A dropped SSH result may hide docker run's returned ID.
                    # Recover only our unique, labelled temporary container.
                    cleanup_id = run(client, shlex.join(["docker", "ps", "-aq", "--no-trunc",
                                                        "--filter", "name=^/" + name + "$",
                                                        "--filter", "label=codex-agent.integration=source-race"])).strip()
                    if cleanup_id:
                        if not re.fullmatch(r"[0-9a-f]{64}", cleanup_id):
                            raise RuntimeError("Unexpected cleanup container identity")
                        run(client, "docker rm -f " + shlex.quote(cleanup_id), timeout=60)
                    report["temporary_container_removed"] = True
                except Exception:
                    report.update(status="fail", temporary_container_removed=False)
            if env_written:
                try:
                    with client.open_sftp() as sftp:
                        sftp.remove(remote + "/test.env")
                    report["temporary_credentials_removed"] = True
                except FileNotFoundError:
                    report["temporary_credentials_removed"] = True
                except Exception:
                    report.update(status="fail", temporary_credentials_removed=False)
            client.close()
    report["go_source_changed_after_freeze"] = go_manifest()["sha256"] != manifest["sha256"]
    report["duration_seconds"] = round(time.monotonic() - started, 3)
    report["completed_at"] = datetime.now(timezone.utc).isoformat()
    report["artifacts"] = [{"path": str((output / filename).relative_to(ROOT)),
                            "sha256": hashlib.sha256((output / filename).read_bytes()).hexdigest()}
                           for filename in ("race.log", "source-manifest.json") if (output / filename).is_file()]
    (output / "report.json").write_text(json.dumps(report, indent=2) + "\n", encoding="utf-8")
    print(json.dumps({"status": report["status"], "go_source_changed": report["go_source_changed_after_freeze"],
                      "passed_top_level_tests": report.get("passed_top_level_tests", 0), "report": str(output / "report.json")}), flush=True)
    return 0 if report["status"] == "pass" else 1


if __name__ == "__main__":
    raise SystemExit(main())
