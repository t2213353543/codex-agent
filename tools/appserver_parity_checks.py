#!/usr/bin/env python3
"""Freeze AppServer sources, then race-test them in an offline temporary container.

No account, database, Redis or production environment is loaded. Use --plan,
then --freeze after the source owner finishes edits, then --run --snapshot DIR.
"""
import argparse
from datetime import datetime, timezone
import hashlib
import io
import json
from pathlib import Path
import re
import secrets
import shlex
import subprocess
import tarfile
import time

from integration_env import ROOT, REMOTE, connect, run, write_remote
from project_layout import backend_source_paths


IMAGE = "codex-agent-integration-race:ce864e7223ac17b1-v1"
IMAGE_ID = "sha256:eb1d21756fc64fbd95138b52362d6952f31898a666d57b5bdc02eb22bdf4f556"
OUTPUT = ROOT / ".local/appserver-parity/checks"
PATTERN = re.compile(r"^(TestAppServer|TestLightweightUnbounded|TestManagedUsageCacheKnowledge|TestOfficialFailover|TestGatewayAccountFailover|TestGatewayContentSession|TestGatewayGuardianWebSocket|TestGatewayWebSocket|TestGatewayNativeWS|TestGatewayNativePayload|TestNativeWSTransportState)")
EXCLUDE = {"TestOfficialFailoverRPCFixture": "Subprocess-only fixture; exercised by the selected parent tests.",
           "TestAppServerRealtimeNativeActualSideband": "Requires the separately hash-bound native helper; verified by the native integration run.",
           "TestGatewayNativePayloadActualHTTP33MiBPlainAndZstd": "Requires the separately hash-bound actual Rust helper; verified by the native integration run.",
           "TestGatewayNativePayloadActualWebSocket33MiBTextAndBinary": "Requires the separately hash-bound actual Rust helper; verified by the native integration run.",
           "TestOfficialRedisSessionFailoverCompareAndSwap": "External Redis is outside this offline unit/race scope.",
           "TestAppServerParityCapture": "Live capture is outside synthetic race scope."}


def sha(raw):
    return hashlib.sha256(raw).hexdigest()


def source_manifest():
    paths = set(backend_source_paths(ROOT))
    paths.update((ROOT / "internal/server/testdata").glob("appserver*.json"))
    files = {}
    selected = set()
    for path in sorted(paths):
        if path.is_symlink():
            raise ValueError("snapshot_symlink_rejected")
        raw = path.read_bytes()
        relative = path.relative_to(ROOT).as_posix()
        files[relative] = sha(raw)
        if path.name.endswith("_test.go"):
            for name in re.findall(r"^func (Test\w+)\(t \*testing.T\)", raw.decode("utf-8"), re.M):
                if (PATTERN.match(name) or path.name == "gateway_account_failover_test.go") and name not in EXCLUDE:
                    selected.add(name)
    if not selected or "TestAppServerRequestCapturedPilot" not in selected or "TestAppServerCLIFailoverAccountBoundary" not in selected:
        raise ValueError("required_test_selection_missing")
    return {"schema": "appserver-parity-frozen-source-v1", "files": files,
            "source_sha256": sha(json.dumps(files, sort_keys=True, separators=(",", ":")).encode()),
            "selected_tests": sorted(selected), "explicit_exclusions": EXCLUDE,
            "scope": "backend_source_paths plus internal/server/testdata/appserver*.json"}


def exclusive_json(path, value):
    with path.open("x", encoding="utf-8") as output:
        output.write(json.dumps(value, ensure_ascii=False, indent=2) + "\n")


def freeze_module_cache(directory, manifest):
    """Copy pinned public module artifacts already present locally; never fetch."""
    cache = Path(subprocess.check_output(["go", "env", "GOMODCACHE"], cwd=ROOT, text=True).strip()) / "cache/download"
    modules = re.findall(r"^\s*([A-Za-z0-9._/-]+)\s+(v[^\s]+)", (ROOT / "go.mod").read_text(), re.M)
    files = {}
    def escaped(value):
        return "".join("!" + char.lower() if char.isupper() else char for char in value)
    for module, version in modules:
        for suffix in (".mod", ".info", ".zip", ".ziphash"):
            name = escaped(module) + "/@v/" + escaped(version) + suffix
            path = cache / name
            if not path.is_file() or path.is_symlink():
                raise ValueError("pinned_module_missing_from_local_cache")
            files[name] = path
    for line in (ROOT / "go.sum").read_text().splitlines():
        module, version, _ = line.split()
        if version.endswith("/go.mod"):
            name = escaped(module) + "/@v/" + escaped(version[:-7]) + ".mod"
            if (cache / name).is_file():
                files[name] = cache / name
    hashes = {}
    with tarfile.open(directory / "modules.tar", "w") as bundle:
        for name, path in sorted(files.items()):
            raw = path.read_bytes()
            hashes[name] = sha(raw)
            entry = tarfile.TarInfo("cache/download/" + name)
            entry.size, entry.mode, entry.mtime = len(raw), 0o644, 0
            bundle.addfile(entry, io.BytesIO(raw))
    manifest["module_cache"] = {"archive_sha256": sha((directory / "modules.tar").read_bytes()), "files": hashes,
                                "source": "existing_local_pinned_public_module_cache_no_download"}


def freeze():
    manifest = source_manifest()
    stamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%S.%fZ") + "-" + secrets.token_hex(3)
    directory = OUTPUT / stamp
    directory.mkdir(parents=True, exist_ok=False)
    archive = directory / "source.tar.gz"
    with tarfile.open(archive, "w:gz") as bundle:
        for name, digest in manifest["files"].items():
            raw = (ROOT / name).read_bytes()
            if sha(raw) != digest:
                raise ValueError("source_changed_during_freeze")
            entry = tarfile.TarInfo(name)
            entry.size, entry.mode, entry.mtime = len(raw), 0o644, 0
            bundle.addfile(entry, io.BytesIO(raw))
    if source_manifest()["source_sha256"] != manifest["source_sha256"]:
        raise ValueError("source_changed_during_freeze")
    manifest["archive_sha256"] = sha(archive.read_bytes())
    freeze_module_cache(directory, manifest)
    exclusive_json(directory / "source-manifest.json", manifest)
    print(json.dumps({"frozen": True, "snapshot": str(directory), "source_sha256": manifest["source_sha256"],
                      "selected_tests": len(manifest["selected_tests"])}), flush=True)
    return directory


def execute(directory):
    directory = directory.resolve()
    if OUTPUT.resolve() not in directory.parents:
        raise ValueError("snapshot_outside_checks_directory")
    manifest = json.loads((directory / "source-manifest.json").read_text(encoding="utf-8"))
    if (directory / "report.json").exists() or (directory / "race.log").exists():
        raise ValueError("immutable_check_already_executed")
    archive = directory / "source.tar.gz"
    if sha(archive.read_bytes()) != manifest["archive_sha256"]:
        raise ValueError("snapshot_archive_hash_mismatch")
    modules = directory / "modules.tar"
    if sha(modules.read_bytes()) != manifest["module_cache"]["archive_sha256"]:
        raise ValueError("module_cache_archive_hash_mismatch")
    name = "codex-appserver-race-" + directory.name.lower().replace(".", "-")
    remote = REMOTE + "/checks/appserver-parity-" + directory.name.lower()
    modules_artifact = REMOTE + "/appserver-parity/artifacts/modules-" + manifest["module_cache"]["archive_sha256"] + ".tar"
    report = {"schema": "appserver-parity-race-v1", "status": "failed", "source_sha256": manifest["source_sha256"],
              "archive_sha256": manifest["archive_sha256"], "selected_tests": manifest["selected_tests"],
              "explicit_exclusions": EXCLUDE, "network": "none", "credentials_loaded": False,
              "production_services_modified": False, "resources": {"cpus": 2, "memory": "2g", "pids": 128},
              "remote_directory": remote, "temporary_container_removed": False}
    client, identifier, attempted = None, None, False
    started = time.monotonic()
    try:
        client = connect()
        run(client, "test -f " + shlex.quote(REMOTE + "/.codex-agent-integration"))
        image = run(client, "docker image inspect --format '{{.Id}}' " + shlex.quote(IMAGE)).strip()
        if image != IMAGE_ID:
            raise ValueError("existing_compiler_image_changed")
        report["image_id"] = image
        run(client, "install -d -m 700 " + shlex.quote(remote) + " " + shlex.quote(remote + "/source"))
        locate = "\n".join([
            "import hashlib,json,os,pathlib",
            "target=pathlib.Path(" + repr(modules_artifact) + ")",
            "expected=" + repr(manifest["module_cache"]["archive_sha256"]),
            "target.parent.mkdir(mode=0o700,parents=True,exist_ok=True)",
            "if not target.exists():",
            " for metadata in pathlib.Path('/opt/codex-agent-integration/checks').glob('appserver-parity-*/source-manifest.json'):",
            "  source=metadata.parent/'modules.tar'",
            "  if source.exists() and json.loads(metadata.read_text()).get('module_cache',{}).get('archive_sha256')==expected and hashlib.sha256(source.read_bytes()).hexdigest()==expected:",
            "   os.link(source,target);break",
            "print('ready' if target.exists() and hashlib.sha256(target.read_bytes()).hexdigest()==expected else 'upload')",
        ])
        modules_cached = run(client, "python3 -B -c " + shlex.quote(locate)).strip() == "ready"
        upload_progress_at = [0.0]
        def upload_progress(transferred, total):
            now = time.monotonic()
            if now - upload_progress_at[0] >= 30 or transferred == total:
                print(json.dumps({"module_cache_upload_bytes": transferred, "total_bytes": total}), flush=True)
                upload_progress_at[0] = now
        with client.open_sftp() as sftp:
            sftp.put(str(archive), remote + "/source.tar.gz")
            sftp.chmod(remote + "/source.tar.gz", 0o600)
            if not modules_cached:
                sftp.put(str(modules), modules_artifact, callback=upload_progress)
                sftp.chmod(modules_artifact, 0o600)
        if run(client, "sha256sum " + shlex.quote(remote + "/source.tar.gz")).split()[0] != manifest["archive_sha256"]:
            raise ValueError("uploaded_archive_hash_mismatch")
        if run(client, "sha256sum " + shlex.quote(modules_artifact)).split()[0] != manifest["module_cache"]["archive_sha256"]:
            raise ValueError("uploaded_module_cache_hash_mismatch")
        report["module_cache_reused"] = modules_cached
        write_remote(client, remote + "/source-manifest.json", json.dumps(manifest, indent=2))
        run(client, "tar -xzf " + shlex.quote(remote + "/source.tar.gz") + " -C " + shlex.quote(remote + "/source"))
        regex = "^(" + "|".join(manifest["selected_tests"]) + ")$"
        command = ["go", "test", "-race", "-count=1", "-timeout=5m", "-json", "-run", regex, "./internal/server"]
        script = "\n".join(["set -eu", "mkdir -p /work /tmp/go-mod", "cp -a /src/. /work/", "tar -xf /modules.tar -C /tmp/go-mod", "cd /work", "go version", "go env GOOS GOARCH CGO_ENABLED CC",
            "exec env -i PATH=\"$PATH\" HOME=/tmp CGO_ENABLED=1 GOMAXPROCS=2 GOFLAGS=-mod=readonly GOCACHE=/tmp/go-build GOMODCACHE=/tmp/go-mod GOPROXY=off GOSUMDB=off " + shlex.join(command)])
        container = ["docker", "run", "-d", "--name", name, "--label", "codex-agent.integration=appserver-parity-race",
                     "--cpus", "2", "--memory", "2g", "--pids-limit", "128", "--network", "none", "--cap-drop", "ALL",
                     "--security-opt", "no-new-privileges", "-v", remote + "/source:/src:ro", "-v", modules_artifact + ":/modules.tar:ro", image, "sh", "-c", script]
        attempted = True
        identifier = run(client, shlex.join(container)).strip()
        if not re.fullmatch(r"[0-9a-f]{64}", identifier):
            raise ValueError("invalid_temporary_container_identity")
        print(json.dumps({"started": True, "snapshot": str(directory), "tests": len(manifest["selected_tests"])}), flush=True)
        deadline, next_progress = time.monotonic() + 720, time.monotonic() + 45
        while True:
            state = json.loads(run(client, "docker inspect --format '{{json .State}}' " + identifier))
            if not state["Running"]:
                break
            if time.monotonic() > deadline:
                raise TimeoutError("offline_race_deadline")
            if time.monotonic() >= next_progress:
                print(json.dumps({"running": True, "elapsed_seconds": round(time.monotonic() - started)}), flush=True)
                next_progress = time.monotonic() + 45
            time.sleep(5)
        raw = run(client, "docker logs " + identifier + " 2>&1", timeout=60)
        with (directory / "race.log").open("x", encoding="utf-8", newline="") as handle:
            handle.write(raw)
        outcomes = {"pass": [], "fail": [], "skip": []}
        packages = []
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
            elif action == "pass" and not test:
                packages.append(event.get("Package"))
        missing = sorted(set(manifest["selected_tests"]) - set(outcomes["pass"]) - set(outcomes["fail"]) - set(outcomes["skip"]))
        passed = state["ExitCode"] == 0 and not state.get("OOMKilled") and not outcomes["fail"] and not outcomes["skip"] and not missing and bool(packages)
        report.update(status="passed" if passed else "failed", exit_code=state["ExitCode"], oom_killed=state.get("OOMKilled"),
                      passed_tests=outcomes["pass"], failed_tests=outcomes["fail"], skipped_tests=outcomes["skip"], missing_tests=missing,
                      passed_top_level_tests=sum("/" not in name for name in outcomes["pass"]), passed_packages=packages,
                      race_log_sha256=sha(raw.encode()))
    except Exception as error:
        report.update(status="failed", error_type=type(error).__name__, error_message=str(error))
    finally:
        if client is not None:
            if attempted:
                try:
                    cleanup = run(client, shlex.join(["docker", "ps", "-aq", "--no-trunc", "--filter", "name=^/" + name + "$",
                        "--filter", "label=codex-agent.integration=appserver-parity-race"])).strip()
                    if cleanup:
                        if not re.fullmatch(r"[0-9a-f]{64}", cleanup):
                            raise ValueError("invalid_cleanup_container_identity")
                        run(client, "docker rm -f " + shlex.quote(cleanup), timeout=60)
                    report["temporary_container_removed"] = True
                except Exception:
                    report["status"] = "failed"
            report["duration_seconds"] = round(time.monotonic() - started, 3)
            report["source_changed_after_freeze"] = source_manifest()["source_sha256"] != manifest["source_sha256"]
            exclusive_json(directory / "report.json", report)
            try:
                write_remote(client, remote + "/report.json", json.dumps(report, indent=2))
                if (directory / "race.log").exists():
                    write_remote(client, remote + "/race.log", (directory / "race.log").read_text(encoding="utf-8"))
            finally:
                client.close()
        elif not (directory / "report.json").exists():
            exclusive_json(directory / "report.json", report)
    print(json.dumps({"status": report["status"], "passed_top_level_tests": report.get("passed_top_level_tests", 0),
                      "skipped_tests": report.get("skipped_tests", []), "missing_tests": report.get("missing_tests", []),
                      "report": str(directory / "report.json")}), flush=True)
    return 0 if report["status"] == "passed" else 1


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    modes = parser.add_mutually_exclusive_group(required=True)
    modes.add_argument("--plan", action="store_true")
    modes.add_argument("--freeze", action="store_true")
    modes.add_argument("--run", action="store_true")
    parser.add_argument("--snapshot", type=Path)
    args = parser.parse_args()
    if args.plan:
        manifest = source_manifest()
        print(json.dumps({"source_sha256": manifest["source_sha256"], "files": len(manifest["files"]),
                          "selected_tests": manifest["selected_tests"], "explicit_exclusions": EXCLUDE}, indent=2))
        return 0
    if args.freeze:
        freeze()
        return 0
    if args.snapshot is None:
        parser.error("--run requires an existing --snapshot")
    return execute(args.snapshot)


if __name__ == "__main__":
    raise SystemExit(main())
