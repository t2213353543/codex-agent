"""Boot the release helper from a binary-only rootfs with no dynamic loader."""
from __future__ import annotations

import argparse
import io
import json
from pathlib import Path
import secrets
import struct
import tarfile
import tempfile

import native_transport_local as runner


def smoke(run_id: str) -> dict:
    folder, meta = runner.metadata(run_id)
    result = json.loads((folder / "result.json").read_text())
    if result.get("profile") != "release" or result["state"]["ExitCode"] != 0:
        raise RuntimeError("release_build_not_successful")
    binary = folder / meta["target_artifact_path"].replace("/", "-")
    elf = json.loads((folder / "elf-inspection.json").read_text())
    if not elf["statically_linked_observed"] or runner.digest(binary.read_bytes()) != elf["binary_sha256"]:
        raise RuntimeError("release_not_verified_static")
    label = "codex-agent.native.release-smoke"
    tag = "codex-native-release-smoke:" + run_id.lower()
    with tempfile.TemporaryDirectory(prefix="native-release-smoke-") as temporary:
        archive = Path(temporary) / "rootfs.tar"
        with tarfile.open(archive, "w") as tar:
            data = binary.read_bytes()
            member = tarfile.TarInfo("native-helper")
            member.size, member.mode, member.mtime = len(data), 0o500, 0
            tar.addfile(member, io.BytesIO(data))
        runner.docker("import", str(archive), tag)
    image = runner.docker_json("image", "inspect", tag)[0]["Id"]
    container = runner.docker("create", "-i", "--name", "codex-native-release-smoke-" + run_id.lower(),
                              "--label", label + "=" + run_id, "--network", "none", "--read-only",
                              "--cpus", "1", "--memory", "128m", "--pids-limit", "32", "--cap-drop", "ALL",
                              "--security-opt", "no-new-privileges", image, "/native-helper").stdout.decode().strip()
    evidence = {"schema": "native-release-binary-only-smoke-v1", "run_id": run_id, "binary_sha256": elf["binary_sha256"],
                "image_id": image, "rootfs_files": ["/native-helper"], "network": "none", "read_only": True,
                "dynamic_loader_present": False, "libc_files_present": False, "production_access": False}
    runner.save(folder / "release-smoke-start.json", dict(evidence, container=container))
    encoded = json.dumps({"protocol": 1, "listen": "127.0.0.1:0", "upstream": "http://127.0.0.1:1", "ipc_token": secrets.token_hex(32)}).encode()
    import subprocess
    executed = subprocess.run(["docker", "start", "-a", "-i", container], input=struct.pack("!I", len(encoded)) + encoded, capture_output=True, timeout=15)
    state = runner.docker_json("inspect", container)[0]["State"]
    ready = json.loads(executed.stdout)
    ready.pop("listen", None)
    evidence.update(exit_code=state["ExitCode"], oom_killed=state["OOMKilled"], ready=ready,
                    passed=executed.returncode == 0 and state["ExitCode"] == 0 and not executed.stderr
                    and ready.get("http_client_lifecycle") == "cli_request" and "websocket_message_v1" in ready.get("capabilities", []))
    runner.save(folder / "release-smoke.json", evidence)
    observed = runner.docker_json("inspect", container)[0]
    if observed["State"]["Running"] or observed["Config"]["Labels"].get(label) != run_id:
        raise RuntimeError("smoke_cleanup_identity_mismatch")
    runner.docker("rm", container)
    runner.docker("image", "rm", tag)
    evidence["container_and_image_removed"] = True
    runner.save(folder / "release-smoke.json", evidence)
    return evidence


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--run", required=True)
    args = parser.parse_args()
    result = smoke(args.run)
    print(json.dumps(result), flush=True)
    raise SystemExit(0 if result["passed"] else 1)
