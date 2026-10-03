"""Freeze and build the native HTTP transport in a bounded local container.

Only synthetic loopback tests run here. No host mounts, ports, accounts, system
package changes, remote hosts or production services are involved. Re-running
start never replaces an existing run; resume requires a terminal failed Docker
container and keeps its dependency/compiler cache.
"""
from __future__ import annotations

import argparse
from datetime import datetime, timezone
import gzip
import hashlib
import io
import json
import os
from pathlib import Path
import re
import secrets
import shutil
import subprocess
import tarfile
import tempfile


ROOT = Path(__file__).resolve().parents[1]
SOURCE = ROOT / "native-transport"
OUTPUT = ROOT / ".local/native-local-build"
IMAGE = "rust:1.95.0-alpine3.23"
LABEL = "codex-agent.native.local"
EXCLUDED = {"target", ".git", "__pycache__"}
ARTIFACTS = ["Cargo.lock", "toolchain.txt", "lock-check.json", "resolved-features.txt",
             "test-results.json", "build-state.json", "elf-inspection.json", "elf-inspection.txt", "helper-ready.json", "toolchain-host.txt"]
RUN_PATTERN = re.compile(r"[0-9]{8}T[0-9]{6}Z-[0-9a-f]{8}")


ENTRYPOINT = """#!/bin/sh
set -eu
mkdir -p /work
if ! test -f /work/.packages-ready; then
    apk add --no-cache cmake make g++ perl python3 pkgconf openssl-dev openssl
    touch /work/.packages-ready
fi
if ! command -v file >/dev/null || ! command -v readelf >/dev/null; then
    apk add --no-cache file binutils
fi
exec python3 /snapshot/build_driver.py
"""


BUILD_DRIVER = r'''"""Container-local build state, checked on every restart."""
import hashlib
import json
import os
from pathlib import Path
import subprocess
import secrets
import struct
import sys
import tarfile
from datetime import datetime, timezone

WORK = Path(os.environ.get("CODEX_NATIVE_WORKDIR", "/work"))
WORK.mkdir(parents=True, exist_ok=True)
TARGET = Path(os.environ.get("CARGO_TARGET_DIR", str(WORK / "target")))
PROFILE = os.environ.get("CODEX_NATIVE_PROFILE", "debug")
if PROFILE not in {"debug", "release"}:
    raise SystemExit("invalid_build_profile")
PROFILE_ARGS = ["--release"] if PROFILE == "release" else []
BINARY = TARGET / PROFILE / "codex-native-transport"
STATE = WORK / "build-state.json"
manifest = json.loads(Path("/snapshot/source-manifest.json").read_text())
state = json.loads(STATE.read_text()) if STATE.exists() else {"completed": [], "attempts": []}

def now():
    return datetime.now(timezone.utc).isoformat()

def write():
    temporary = STATE.with_suffix(".tmp")
    temporary.write_text(json.dumps(state, indent=2) + "\n")
    temporary.replace(STATE)

def verify_sources(allow_resolved_lock=False):
    for name, expected in manifest["files"].items():
        if allow_resolved_lock and name == "Cargo.lock":
            continue
        path = WORK / name
        if not path.is_file() or path.is_symlink() or hashlib.sha256(path.read_bytes()).hexdigest() != expected:
            raise RuntimeError("frozen_source_mismatch")

def execute(phase, command, output=None):
    if phase in state["completed"]:
        return
    state.update(status="running", phase=phase, started_at=now())
    write()
    print(json.dumps({"phase": phase, "status": "running"}), flush=True)
    destination = (WORK / output).open("wb") if output else None
    try:
        result = subprocess.run(command, cwd=WORK, stdout=destination, check=False)
    finally:
        if destination:
            destination.close()
    if result.returncode:
        state.update(status="failed", phase=phase, exit_code=result.returncode, finished_at=now())
        write()
        raise SystemExit(result.returncode if result.returncode > 0 else 1)
    state["completed"].append(phase)
    state.update(status="running", phase=phase, exit_code=0, finished_at=now())
    write()

try:
    archive = Path("/snapshot/source.tar.gz")
    if hashlib.sha256(archive.read_bytes()).hexdigest() != manifest["archive_sha256"]:
        raise RuntimeError("snapshot_archive_mismatch")
    if not (WORK / ".source-ready").exists():
        with tarfile.open(archive, "r:gz") as source:
            members = source.getmembers()
            if {item.name for item in members} != set(manifest["files"]):
                raise RuntimeError("snapshot_members_mismatch")
            for member in members:
                target = (WORK / member.name).resolve()
                if not member.isfile() or not target.is_relative_to(WORK):
                    raise RuntimeError("snapshot_member_invalid")
            source.extractall(WORK, members=members, filter="data")
        verify_sources()
        (WORK / ".source-ready").touch()
    verify_sources(allow_resolved_lock=True)
    state["attempts"].append({"started_at": now(), "resume_after": state.get("phase")})
    state["archive_sha256"] = manifest["archive_sha256"]
    state["source_sha256"] = manifest["source_sha256"]
    state["profile"] = PROFILE
    state["target_artifact_path"] = str(BINARY)
    write()
    versions = []
    for program in ("rustc", "cargo"):
        version = subprocess.check_output([program, "--version"], text=True).strip()
        if not version.startswith(program + " 1.95.0 "):
            raise RuntimeError("toolchain_version_mismatch")
        versions.append(version)
    (WORK / "toolchain.txt").write_text("\n".join(versions) + "\n")
    (WORK / "toolchain-host.txt").write_text(subprocess.check_output(["rustc", "-vV"], text=True))
    os.environ["CARGO_BUILD_JOBS"] = "2"
    os.environ["AWS_LC_SYS_NO_JITTER_ENTROPY"] = "1"
    execute("fetch", ["cargo", "fetch"])
    # Reject any package/version/checksum not present in the official lock
    # before executing dependency build scripts.
    execute("lock_check", ["python3", "tools/check_lock.py"], "lock-check.json")
    # Deterministic archive mtimes are zero. Cargo's mtime-based local source
    # freshness must not reuse another snapshot's binary from inherited target.
    if "build" not in state["completed"]:
        for name in manifest["files"]:
            if name.endswith(".rs") or name.endswith("Cargo.toml"):
                os.utime(WORK / name, None)
    execute("clean_helper", ["cargo", "clean", "--locked", "-p", "codex-native-transport"])
    execute("build", ["cargo", "build", "--locked", *PROFILE_ARGS])
    execute("websocket_tests", ["cargo", "test", "--locked", *PROFILE_ARGS, "--bin", "codex-native-transport", "websocket::tests::", "--", "--nocapture"])
    execute("compression_tests", ["cargo", "test", "--locked", *PROFILE_ARGS, "--bin", "codex-native-transport", "request_compression::tests::", "--", "--nocapture"])
    execute("features", ["cargo", "tree", "--locked", "-e", "features"], "resolved-features.txt")
    execute("loopback", ["python3", "tests/loopback.py", "--binary",
            str(BINARY), "--report", str(WORK / "test-results.json")])
    inspection = {}
    raw_inspection = []
    for name, command in [("file", ["file", "-b", str(BINARY)]), ("header", ["readelf", "-h", str(BINARY)]),
                          ("program_headers", ["readelf", "-l", str(BINARY)]), ("dynamic", ["readelf", "-d", str(BINARY)])]:
        result = subprocess.run(command, capture_output=True, text=True, check=True)
        inspection[name] = result.stdout
        raw_inspection.append(name + "\n" + result.stdout)
    dynamic = inspection["dynamic"]
    inspection.update(profile=PROFILE, binary_sha256=hashlib.sha256(BINARY.read_bytes()).hexdigest(),
                      binary_bytes=BINARY.stat().st_size, interpreter_present="INTERP" in inspection["program_headers"],
                      needed_libraries=[line.split("[", 1)[1].split("]", 1)[0] for line in dynamic.splitlines() if "(NEEDED)" in line],
                      elf_machine_x86_64="Advanced Micro Devices X86-64" in inspection["header"])
    inspection["statically_linked_observed"] = not inspection["interpreter_present"] and not inspection["needed_libraries"]
    (WORK / "elf-inspection.json").write_text(json.dumps(inspection, indent=2) + "\n")
    (WORK / "elf-inspection.txt").write_text("\n".join(raw_inspection))
    ready_config = json.dumps({"protocol":1,"listen":"127.0.0.1:0","upstream":"http://127.0.0.1:1","ipc_token":secrets.token_hex(32)}).encode()
    ready_probe = subprocess.run([str(BINARY)], input=struct.pack("!I",len(ready_config))+ready_config, capture_output=True, timeout=5)
    ready = json.loads(ready_probe.stdout)
    if ready_probe.returncode or ready_probe.stderr or ready.get("http_client_lifecycle") != "cli_request" or "websocket_message_v1" not in ready.get("capabilities", []):
        raise RuntimeError("helper_ready_contract_failed")
    ready.pop("listen", None)
    (WORK / "helper-ready.json").write_text(json.dumps(ready, indent=2) + "\n")
    verify_sources(allow_resolved_lock=True)
    report = json.loads((WORK / "test-results.json").read_text())
    if not report.get("passed") or report.get("tests", 0) < 26 or report.get("skipped") != 0 or report.get("live_accounts") is not False:
        raise RuntimeError("loopback_report_incomplete")
    state.update(status="complete", phase="complete", exit_code=0, finished_at=now())
    write()
    print(json.dumps({"phase": "complete", "tests": report["tests"], "passed": True}), flush=True)
except SystemExit:
    raise
except Exception as error:
    # Exception strings and process environments never enter diagnostics.
    state.update(status="failed", phase=state.get("phase", "verify"), exit_code=1,
                 failure_class=type(error).__name__, finished_at=now())
    write()
    print("native-local-build: verification_failed", file=sys.stderr, flush=True)
    raise SystemExit(1)
'''


def now() -> str:
    return datetime.now(timezone.utc).isoformat()


def digest(raw: bytes) -> str:
    return hashlib.sha256(raw).hexdigest()


def save(path: Path, value: dict) -> None:
    temporary = path.with_suffix(path.suffix + ".tmp")
    temporary.write_text(json.dumps(value, indent=2) + "\n", encoding="utf-8")
    temporary.replace(path)


def docker(*arguments: str, timeout: int = 60, check: bool = True) -> subprocess.CompletedProcess:
    result = subprocess.run(["docker", *arguments], capture_output=True, timeout=timeout)
    if check and result.returncode:
        # Raw Docker diagnostics can include unrelated daemon settings. Preserve
        # only this operation name and status in the public command output.
        raise RuntimeError(f"docker_{arguments[0]}_failed_exit_{result.returncode}")
    return result


def docker_json(*arguments: str):
    return json.loads(docker(*arguments).stdout.decode("utf-8"))


def temp_ascii():
    folder = tempfile.TemporaryDirectory(prefix="codex-native-local-")
    if not str(folder.name).isascii():
        folder.cleanup()
        raise RuntimeError("ASCII_temporary_directory_required")
    return folder


def source_files() -> list[Path]:
    files = []
    for path in SOURCE.rglob("*"):
        relative = path.relative_to(SOURCE)
        if any(part in EXCLUDED for part in relative.parts):
            continue
        if path.is_symlink():
            raise RuntimeError("source_symlink_forbidden")
        if path.is_file():
            files.append(path)
    if not files or not (SOURCE / "Cargo.toml").is_file():
        raise RuntimeError("native_sources_missing")
    return sorted(files)


def freeze(folder: Path) -> dict:
    archive = folder / "source.tar.gz"
    hashes = {}
    with archive.open("wb") as raw, gzip.GzipFile(fileobj=raw, mode="wb", mtime=0, filename="") as compressed:
        with tarfile.open(fileobj=compressed, mode="w") as tar:
            for path in source_files():
                relative = path.relative_to(SOURCE).as_posix()
                content = path.read_bytes()
                hashes[relative] = digest(content)
                member = tarfile.TarInfo(relative)
                member.size, member.mode, member.mtime = len(content), 0o644, 0
                tar.addfile(member, io.BytesIO(content))
    current = {path.relative_to(SOURCE).as_posix(): digest(path.read_bytes()) for path in source_files()}
    if current != hashes:
        raise RuntimeError("sources_changed_during_freeze")
    manifest = {"files": hashes, "archive_sha256": digest(archive.read_bytes()),
                "source_sha256": digest(json.dumps(hashes, sort_keys=True, separators=(",", ":")).encode())}
    save(folder / "source-manifest.json", manifest)
    return manifest


def metadata(run_id: str | None) -> tuple[Path, dict]:
    if run_id is None:
        current = OUTPUT / "current.json"
        if not current.is_file():
            raise RuntimeError("no_local_build_handle")
        run_id = json.loads(current.read_text(encoding="utf-8"))["run_id"]
    if not RUN_PATTERN.fullmatch(run_id):
        raise RuntimeError("invalid_run_handle")
    folder = OUTPUT / run_id
    return folder, json.loads((folder / "run.json").read_text(encoding="utf-8"))


def inspect(meta: dict) -> dict:
    context = docker("context", "show").stdout.decode().strip()
    if context != meta["docker_context"]:
        raise RuntimeError("docker_context_changed")
    response = docker_json("inspect", meta["container"])
    if len(response) != 1:
        raise RuntimeError("container_identity_missing")
    container = response[0]
    labels = container.get("Config", {}).get("Labels", {})
    if (container["Id"] != meta["container"] or labels.get(LABEL) != meta["run_id"]
            or labels.get(LABEL + ".source") != meta["source_sha256"]
            or container.get("Image") != meta["image_id"]):
        raise RuntimeError("container_identity_mismatch")
    return container


def state_summary(container: dict) -> dict:
    state = container["State"]
    return {name: state.get(name) for name in ("Status", "Running", "ExitCode", "OOMKilled", "StartedAt", "FinishedAt")}


def copy_artifact(meta: dict, relative: str, local: Path) -> bool:
    with temp_ascii() as temporary:
        target = Path(temporary) / "artifact"
        if relative.startswith("target/"):
            source = meta.get("target_dir", "/work/target") + "/" + relative.removeprefix("target/")
        else:
            source = meta.get("workdir", "/work") + "/" + relative
        result = docker("cp", meta["container"] + ":" + source, str(target), check=False)
        if result.returncode:
            return False
        if not target.is_file():
            raise RuntimeError("artifact_not_regular_file")
        shutil.copyfile(target, local)
    return True


def build_state(meta: dict) -> dict | None:
    with temp_ascii() as temporary:
        path = Path(temporary) / "state.json"
        if not copy_artifact(meta, "build-state.json", path):
            return None
        value = json.loads(path.read_text(encoding="utf-8"))
        if value.get("source_sha256") not in (None, meta["source_sha256"]):
            raise RuntimeError("container_build_state_mismatch")
        return value


def start(new_run: bool = False, cache_from: str | None = None, profile: str = "debug") -> None:
    if profile not in {"debug", "release"}:
        raise RuntimeError("invalid_build_profile")
    OUTPUT.mkdir(parents=True, exist_ok=True)
    if (OUTPUT / "current.json").is_file():
        _, previous = metadata(None)
        container = inspect(previous)
        if container["State"]["Running"]:
            print(json.dumps({"run_id": previous["run_id"], "existing": True, "state": state_summary(container)}))
            return
        if not new_run:
            raise RuntimeError("existing_terminal_run_inspect_collect_or_resume_or_explicit_new")
    images = docker_json("image", "inspect", IMAGE)
    if len(images) != 1:
        raise RuntimeError("official_rust_image_missing")
    image = images[0]
    image_id = image["Id"]
    digests = image.get("RepoDigests", [])
    if not re.fullmatch(r"sha256:[0-9a-f]{64}", image_id) or not any(re.fullmatch(r"rust@sha256:[0-9a-f]{64}", value) for value in digests):
        raise RuntimeError("official_rust_image_digest_missing")
    base_image_id = image_id
    cache = None
    if cache_from:
        _, cache_meta = metadata(cache_from)
        previous = inspect(cache_meta)
        if previous["State"]["Running"] or previous["State"]["Status"] != "exited":
            raise RuntimeError("cache_source_requires_authoritatively_exited_container")
        if cache_meta.get("base_image_id", cache_meta["image_id"]) != base_image_id:
            raise RuntimeError("cache_base_image_mismatch")
        state = build_state(cache_meta)
        if not state or "lock_check" not in state.get("completed", []):
            raise RuntimeError("cache_source_requires_verified_official_lock")
        cache_tag = "codex-native-local-cache:" + cache_meta["run_id"].lower()
        cached = docker("image", "inspect", cache_tag, check=False)
        if cached.returncode:
            docker("commit", "--pause=false", cache_meta["container"], cache_tag, timeout=120)
        # Docker/BuildKit versions can mix progress output with commit's ID.
        # Inspect the run-scoped tag instead of parsing display output.
        cached_image = docker_json("image", "inspect", cache_tag)[0]
        cached_labels = cached_image.get("Config", {}).get("Labels", {})
        if (cached_labels.get(LABEL) != cache_meta["run_id"]
                or cached_labels.get(LABEL + ".source") != cache_meta["source_sha256"]):
            raise RuntimeError("cache_image_source_mismatch")
        image_id = cached_image["Id"]
        if not re.fullmatch(r"sha256:[0-9a-f]{64}", image_id):
            raise RuntimeError("cache_image_identity_invalid")
        cache = {"run_id": cache_meta["run_id"], "container": cache_meta["container"],
                 "image_id": image_id, "tag": cache_tag, "source_sha256": cache_meta["source_sha256"],
                 "target_dir": cache_meta.get("target_dir", "/work/target")}
    run_id = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ-") + secrets.token_hex(4)
    folder = OUTPUT / run_id
    folder.mkdir()
    manifest = freeze(folder)
    context = docker("context", "show").stdout.decode().strip()
    name = "codex-native-local-" + run_id.lower()
    workdir = "/build-" + run_id if cache else "/work"
    target_dir = cache["target_dir"] if cache else "/work/target"
    result = docker("create", "--name", name, "--label", LABEL + "=" + run_id,
                    "--label", LABEL + ".source=" + manifest["source_sha256"],
                    "--cpus", "2", "--memory", "3g", "--pids-limit", "256",
                    "--security-opt", "no-new-privileges", "--workdir", "/",
                    "--env", "CODEX_NATIVE_WORKDIR=" + workdir,
                    "--env", "CARGO_TARGET_DIR=" + target_dir,
                    "--env", "CODEX_NATIVE_PROFILE=" + profile,
                    image_id, "sh", "/snapshot/entrypoint.sh")
    identifier = result.stdout.decode().strip()
    if not re.fullmatch(r"[0-9a-f]{64}", identifier):
        raise RuntimeError("created_container_identity_invalid")
    meta = {"run_id": run_id, "container": identifier, "container_name": name,
            "docker_context": context, "image": IMAGE, "image_id": image_id,
            "base_image_id": base_image_id, "cache_from": cache, "workdir": workdir,
            "target_dir": target_dir,
            "profile": profile, "target_artifact_path": "target/" + profile + "/codex-native-transport",
            "repo_digests": digests, "source_sha256": manifest["source_sha256"],
            "archive_sha256": manifest["archive_sha256"], "started_at": now(),
            "limits": {"cpus": 2, "memory_bytes": 3 * 1024 ** 3, "pids": 256},
            "host_packages_modified": False, "production_service_modified": False,
            "host_mounts": False, "published_ports": False, "live_accounts": False}
    save(folder / "run.json", meta)
    save(OUTPUT / "current.json", {"run_id": run_id})
    with temp_ascii() as temporary:
        snapshot = Path(temporary) / "snapshot"
        snapshot.mkdir()
        shutil.copyfile(folder / "source.tar.gz", snapshot / "source.tar.gz")
        shutil.copyfile(folder / "source-manifest.json", snapshot / "source-manifest.json")
        (snapshot / "entrypoint.sh").write_text(ENTRYPOINT, encoding="utf-8", newline="\n")
        (snapshot / "build_driver.py").write_text(BUILD_DRIVER, encoding="utf-8", newline="\n")
        if cache:
            # docker cp of an existing directory nests it; replace each of the
            # four snapshot files explicitly in a cache-derived image instead.
            for path in snapshot.iterdir():
                docker("cp", str(path), identifier + ":/snapshot/" + path.name)
        else:
            docker("cp", str(snapshot), identifier + ":/snapshot")
    docker("start", identifier)
    print(json.dumps({"run_id": run_id, "container": identifier, "started": True,
                      "archive_sha256": manifest["archive_sha256"], "source_sha256": manifest["source_sha256"],
                      "image_id": image_id, "repo_digests": digests}), flush=True)


def status(run_id: str | None) -> None:
    folder, meta = metadata(run_id)
    container = inspect(meta)
    observation = {"run_id": meta["run_id"], "observed_at": now(),
                   "state": state_summary(container), "build": build_state(meta)}
    save(folder / "status.json", observation)
    logs = docker("logs", "--tail", "30", meta["container"])
    (folder / "tail.log").write_bytes(logs.stdout + logs.stderr)
    print(json.dumps(observation), flush=True)


def resume(run_id: str | None) -> None:
    folder, meta = metadata(run_id)
    container = inspect(meta)
    state = container["State"]
    if state["Running"] or state["Status"] not in {"exited", "dead"}:
        raise RuntimeError("resume_requires_authoritatively_terminal_container")
    if state["ExitCode"] == 0:
        raise RuntimeError("successful_container_must_not_restart")
    recorded = build_state(meta)
    if recorded and recorded.get("status") == "complete":
        raise RuntimeError("completed_build_must_not_restart")
    attempts = meta.setdefault("resumes", [])
    attempts.append({"observed_at": now(), "prior_state": state_summary(container),
                     "prior_phase": recorded.get("phase") if recorded else "packages"})
    save(folder / "run.json", meta)
    docker("start", meta["container"])
    print(json.dumps({"run_id": meta["run_id"], "container": meta["container"],
                      "resumed": True, "same_container_cache": True}), flush=True)


def collect(run_id: str | None) -> None:
    folder, meta = metadata(run_id)
    container = inspect(meta)
    if container["State"]["Running"]:
        raise RuntimeError("collect_requires_terminal_container")
    logs = docker("logs", meta["container"], timeout=120)
    (folder / "build.log").write_bytes(logs.stdout + logs.stderr)
    artifacts = []
    profile = meta.get("profile", "debug")
    binary_relative = meta.get("target_artifact_path", "target/debug/codex-native-transport")
    for relative in [*ARTIFACTS, binary_relative]:
        local = folder / relative.replace("/", "-")
        if copy_artifact(meta, relative, local):
            artifacts.append({"path": str(local.relative_to(ROOT)), "sha256": digest(local.read_bytes()), "bytes": local.stat().st_size})
    source_binary_check = None
    if meta.get("cache_from"):
        cache_folder, cache_meta = metadata(meta["cache_from"]["run_id"])
        previous = json.loads((cache_folder / "source-manifest.json").read_text(encoding="utf-8"))
        current = json.loads((folder / "source-manifest.json").read_text(encoding="utf-8"))
        same_profile = cache_meta.get("profile", "debug") == profile
        old_binary = cache_folder / cache_meta.get("target_artifact_path", "target/debug/codex-native-transport").replace("/", "-")
        new_binary = folder / binary_relative.replace("/", "-")
        main_changed = previous["files"].get("src/main.rs") != current["files"].get("src/main.rs")
        if same_profile and old_binary.is_file() and new_binary.is_file():
            binary_changed = digest(old_binary.read_bytes()) != digest(new_binary.read_bytes())
            source_binary_check = {"same_profile": True, "main_source_changed": main_changed, "binary_changed": binary_changed,
                                   "stale_binary_detected": main_changed and not binary_changed}
        else:
            source_binary_check = {"same_profile": same_profile, "main_source_changed": main_changed, "binary_comparison_available": False}
    report = {"run_id": meta["run_id"], "collected_at": now(), "state": state_summary(container),
              "build": build_state(meta), "artifacts": artifacts,
              "source_sha256": meta["source_sha256"], "archive_sha256": meta["archive_sha256"],
              "image_id": meta["image_id"], "repo_digests": meta["repo_digests"],
              "source_binary_check": source_binary_check,
              "profile": profile, "target_artifact_path": binary_relative,
              "production_service_modified": False, "live_accounts": False}
    save(folder / "result.json", report)
    if source_binary_check and source_binary_check.get("stale_binary_detected"):
        raise RuntimeError("changed_source_reused_stale_binary")
    print(json.dumps(report), flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=["start", "status", "collect", "resume"])
    parser.add_argument("--run", help="Previously returned run handle; defaults to current.json")
    parser.add_argument("--new", action="store_true", help="Start a fresh snapshot only after the current container is terminal")
    parser.add_argument("--cache-from", help="Use an exited, verified build container as compiler cache in a new isolated snapshot")
    parser.add_argument("--profile", choices=["debug", "release"], default=None)
    args = parser.parse_args()
    if args.action == "start":
        if args.run:
            parser.error("start does not accept --run")
        if args.cache_from and not args.new:
            parser.error("--cache-from requires --new")
        start(args.new, args.cache_from, args.profile or "debug")
    else:
        if args.new or args.cache_from or args.profile:
            parser.error("--new, --cache-from and --profile only apply to start")
        {"status": status, "collect": collect, "resume": resume}[args.action](args.run)


if __name__ == "__main__":
    try:
        main()
    except (RuntimeError, subprocess.TimeoutExpired) as error:
        message = str(error) if isinstance(error, RuntimeError) else "docker_observation_timeout_repoll_same_handle"
        print(json.dumps({"error": message}), flush=True)
        raise SystemExit(1)
