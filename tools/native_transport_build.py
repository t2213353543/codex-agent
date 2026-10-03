"""Build the pinned Rust transport prototype in isolated, bounded containers."""
from __future__ import annotations

import argparse
from datetime import datetime, timezone
import hashlib
import io
import json
import re
import shlex
import tarfile

from integration_env import ROOT, REMOTE, connect, run

BASE_IMAGE = "rust:1.95.0-alpine3.23"
DIRECTORY = REMOTE + "/checks/native-transport-20260918"
LOCAL = ROOT / ".local/native-transport-build"
PREPARE = "codex-agent-native-prepare-20260918"
BUILD = "codex-agent-native-compile-20260918"
LABEL = "codex-agent.integration=native-transport"


def save(name: str, data: dict) -> None:
    LOCAL.mkdir(parents=True, exist_ok=True)
    (LOCAL / name).write_text(json.dumps(data, indent=2) + "\n", encoding="utf-8")


def container_state(client, name: str):
    identifiers = run(client, shlex.join(["docker", "ps", "-aq", "--no-trunc", "--filter",
        "name=^/" + name + "$", "--filter", "label=" + LABEL])).split()
    if not identifiers:
        return None
    if len(identifiers) != 1 or not re.fullmatch(r"[0-9a-f]{64}", identifiers[0]):
        raise RuntimeError("Unexpected build container identity")
    return json.loads(run(client, "docker inspect --format '{{json .State}}' " + identifiers[0]))


def prepare(client):
    run(client, "test -f " + shlex.quote(REMOTE + "/.codex-agent-integration"))
    prior = container_state(client, PREPARE)
    if prior is not None:
        print(json.dumps({"phase": "prepare", "existing": True, "state": prior}))
        return
    # Only the exact official Rust release image is fetched. No host packages,
    # production mounts, credentials or service ports enter these containers.
    run(client, shlex.join(["docker", "pull", BASE_IMAGE]), timeout=300)
    image = run(client, "docker image inspect --format '{{.Id}}' " + shlex.quote(BASE_IMAGE)).strip()
    digests = json.loads(run(client, "docker image inspect --format '{{json .RepoDigests}}' " + shlex.quote(BASE_IMAGE)))
    run(client, "install -d -m 700 " + shlex.quote(DIRECTORY))
    save("base-image.json", {"image": BASE_IMAGE, "id": image, "repo_digests": digests})
    command = ["docker", "run", "-d", "--name", PREPARE, "--label", LABEL,
        "--cpus", "2", "--memory", "2g", "--pids-limit", "256",
        "--security-opt", "no-new-privileges", image, "sh", "-c",
        "set -eu\napk add --no-cache cmake perl python3 pkgconf openssl-dev\nrustc --version\ncargo --version\ncmake --version\n"]
    identifier = run(client, shlex.join(command), timeout=45).strip()
    print(json.dumps({"phase": "prepare", "started": True, "container": identifier, "base_image": image}), flush=True)


def status(client, phase: str):
    name = PREPARE if phase == "prepare" else BUILD
    state = container_state(client, name)
    if state is None:
        print(json.dumps({"phase": phase, "exists": False}))
        return
    # Build and synthetic test logs contain source/package diagnostics only.
    log = run(client, shlex.join(["docker", "logs", "--tail", "45", name]) + " 2>&1", timeout=45)
    save(phase + "-state.json", {"phase": phase, "state": state, "observed_at": datetime.now(timezone.utc).isoformat()})
    (LOCAL / (phase + "-tail.log")).write_text(log, encoding="utf-8")
    print(json.dumps({"phase": phase, "running": state["Running"], "exit_code": state["ExitCode"],
        "oom_killed": state.get("OOMKilled", False), "log_tail": log}))


def build(client):
    state = container_state(client, PREPARE)
    if state is None or state["Running"] or state["ExitCode"] != 0:
        raise RuntimeError("Prepare container must finish successfully before compilation")
    if container_state(client, BUILD) is not None:
        raise RuntimeError("Build container already exists; inspect its state instead of restarting")
    source = ROOT / "native-transport"
    files = sorted(p for p in source.rglob("*") if p.is_file() and not any(x in {"target", ".git"} for x in p.parts))
    if not files or not (source / "Cargo.toml").is_file():
        raise RuntimeError("Native transport sources are not ready")
    archive = LOCAL / "source.tar.gz"
    hashes = {}
    with tarfile.open(archive, "w:gz") as tar:
        for path in files:
            if path.is_symlink():
                raise RuntimeError("Symlinks are forbidden in the native source snapshot")
            relative = path.relative_to(source).as_posix()
            raw = path.read_bytes()
            hashes[relative] = hashlib.sha256(raw).hexdigest()
            entry = tarfile.TarInfo(relative)
            entry.size, entry.mode, entry.mtime = len(raw), 0o644, 0
            tar.addfile(entry, io.BytesIO(raw))
    digest = hashlib.sha256(archive.read_bytes()).hexdigest()
    save("source-manifest.json", {"files": hashes, "archive_sha256": digest})
    with client.open_sftp() as sftp:
        sftp.put(str(archive), DIRECTORY + "/source.tar.gz")
        sftp.chmod(DIRECTORY + "/source.tar.gz", 0o600)
    if run(client, "sha256sum " + shlex.quote(DIRECTORY + "/source.tar.gz")).split()[0] != digest:
        raise RuntimeError("Uploaded native source checksum differs")
    image = run(client, shlex.join(["docker", "commit", PREPARE]), timeout=60).strip()
    if not re.fullmatch(r"sha256:[0-9a-f]{64}", image):
        raise RuntimeError("Unexpected isolated build image identity")
    script = """set -eu
mkdir -p /work
tar -xzf /snapshot/source.tar.gz -C /work
cd /work
export CARGO_BUILD_JOBS=2
export AWS_LC_SYS_NO_JITTER_ENTROPY=1
# The seed lock comes from the larger official workspace. Fetch only resolves
# the smaller graph; reject dependency drift before executing build scripts.
cargo fetch
python3 tools/check_lock.py
cargo build --locked
cargo tree --locked -e features > /work/resolved-features.txt
python3 tests/loopback.py --binary target/debug/codex-native-transport --report /work/test-results.json
"""
    command = ["docker", "run", "-d", "--name", BUILD, "--label", LABEL,
        "--cpus", "2", "--memory", "3g", "--pids-limit", "256", "--security-opt", "no-new-privileges",
        "--mount", "type=bind,src=" + DIRECTORY + ",dst=/snapshot,readonly", image, "sh", "-c", script]
    identifier = run(client, shlex.join(command), timeout=45).strip()
    save("build-start.json", {"image_id": image, "container": identifier, "archive_sha256": digest,
        "host_packages_modified": False, "production_service_modified": False})
    print(json.dumps({"phase": "build", "started": True, "container": identifier}), flush=True)


def collect(client):
    state = container_state(client, BUILD)
    if state is None or state["Running"]:
        raise RuntimeError("Inspect the existing build until it finishes before collecting")
    log = run(client, shlex.join(["docker", "logs", BUILD]) + " 2>&1", timeout=60)
    (LOCAL / "build.log").write_text(log, encoding="utf-8")
    artifacts = []
    for relative in ["Cargo.lock", "resolved-features.txt", "test-results.json", "target/debug/codex-native-transport"]:
        destination = DIRECTORY + "/" + relative.replace("/", "-")
        try:
            run(client, shlex.join(["docker", "cp", BUILD + ":/work/" + relative, destination]))
        except RuntimeError:
            continue
        local = LOCAL / relative.replace("/", "-")
        with client.open_sftp() as sftp:
            sftp.get(destination, str(local))
        artifacts.append({"path": str(local.relative_to(ROOT)), "sha256": hashlib.sha256(local.read_bytes()).hexdigest()})
    save("build-result.json", {"state": state, "artifacts": artifacts,
        "host_packages_modified": False, "production_service_modified": False})
    print(json.dumps({"phase": "collect", "exit_code": state["ExitCode"], "artifacts": artifacts}))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=["prepare", "build", "status", "collect"])
    parser.add_argument("--phase", choices=["prepare", "build"], default="build")
    args = parser.parse_args()
    client = connect()
    try:
        if args.action == "prepare": prepare(client)
        elif args.action == "build": build(client)
        elif args.action == "collect": collect(client)
        else: status(client, args.phase)
    finally:
        client.close()


if __name__ == "__main__":
    main()
