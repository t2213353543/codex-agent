"""Run race checks on a frozen snapshot inside a resource-limited Linux container.

Uses the current master from sub2api-production's SSH helper. Only this project's
/opt/codex-agent-integration/checks directory and its temporary container change.
The compiler is installed inside the container, never on the host.
"""
from __future__ import annotations

import argparse
from datetime import datetime, timezone
import hashlib
import io
import json
from pathlib import Path
import re
import shlex
import tarfile
import time

from acceptance import HANDOFFS, ROOT, sha, source_manifest
from integration_env import LOCAL, REMOTE, connect, run, write_remote


def scrub(value: str, secrets: list[str]) -> str:
    for secret in sorted(secrets, key=len, reverse=True):
        if len(secret) > 6:
            value = value.replace(secret, "[REDACTED]")
    value = re.sub(r'(?i)(postgres(?:ql)?|redis(?:s)?)://[^\s"\']+', r'\1://[REDACTED]', value)
    value = re.sub(r'\beyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\b', '[REDACTED-JWT]', value)
    return re.sub(r'\b(?:ca_|sk-)[A-Za-z0-9_-]{20,}\b', '[REDACTED-KEY]', value)


def snapshot(output: Path, manifest: dict) -> Path:
    archive = output / "source.tar.gz"
    included = {}
    with tarfile.open(archive, "w:gz") as tar:
        for relative, expected in manifest["files"].items():
            path = Path(relative)
            if not (relative in ("go.mod", "go.sum") or path.suffix == ".go" or relative.startswith("migrations/")):
                continue
            raw = (ROOT / relative).read_bytes()
            if hashlib.sha256(raw).hexdigest() != expected:
                raise RuntimeError("Source changed while freezing the snapshot; wait for delivery and rerun")
            info = tarfile.TarInfo(relative)
            info.size, info.mode, info.mtime = len(raw), 0o644, 0
            tar.addfile(info, io.BytesIO(raw))
            included[relative] = expected
    if source_manifest()["sha256"] != manifest["sha256"]:
        raise RuntimeError("Source changed while freezing the snapshot; wait for delivery and rerun")
    (output / "source-manifest.json").write_text(json.dumps({**manifest, "uploaded_files": included,
                                                            "archive_sha256": sha(archive)}, indent=2), encoding="utf-8")
    return archive


def compiler_image(client, base_image: str, output: Path) -> str:
    base_id = run(client, "docker image inspect --format '{{.Id}}' " + shlex.quote(base_image)).strip()
    if not re.fullmatch(r"sha256:[0-9a-f]{64}", base_id):
        raise RuntimeError("Cannot resolve the pre-existing Linux image to an immutable ID")
    tag = "codex-agent-integration-race:" + base_id.split(":")[1][:16] + "-v1"
    exists = run(client, "docker image ls --no-trunc --format '{{.ID}}' " + shlex.quote(tag)).strip()
    if exists:
        return run(client, "docker image inspect --format '{{.Id}}' " + shlex.quote(tag)).strip()
    name = "codex-agent-race-runtime-" + datetime.now(timezone.utc).strftime("%Y%m%d%H%M%S")
    command = ["docker", "run", "-d", "--name", name, "--label", "codex-agent.integration=race-runtime",
               "--cpus", "2", "--memory", "2g", "--pids-limit", "128", "--cap-drop", "ALL",
               "--security-opt", "no-new-privileges", base_id, "sh", "-c",
               "set -eu\napk add --no-cache gcc musl-dev\ngcc --version\ngo version"]
    identifier = run(client, shlex.join(command)).strip()
    if not re.fullmatch(r"[0-9a-f]{64}", identifier):
        raise RuntimeError("Invalid compiler preparation container ID")
    print("Preparing container-only race compiler runtime", flush=True)
    try:
        deadline = time.monotonic() + 900
        while True:
            state = json.loads(run(client, "docker inspect --format '{{json .State}}' " + identifier))
            if not state["Running"]:
                break
            if time.monotonic() >= deadline:
                raise TimeoutError("Container compiler installation exceeded 900 seconds")
            time.sleep(10)
        log = run(client, "docker logs " + identifier + " 2>&1", timeout=60)
        (output / "compiler-runtime.log").write_text(log, encoding="utf-8")
        if state["ExitCode"]:
            raise RuntimeError("Compiler installation failed inside temporary container")
        run(client, "docker commit " + identifier + " " + shlex.quote(tag), timeout=120)
        image = run(client, "docker image inspect --format '{{.Id}}' " + shlex.quote(tag)).strip()
        (output / "compiler-runtime.json").write_text(json.dumps({"status": "pass", "base_image_id": base_id,
                                                                  "image_id": image, "tag": tag,
                                                                  "scope": "compiler installation inside integration container only"}, indent=2), encoding="utf-8")
        return image
    finally:
        run(client, "docker rm -f " + identifier, timeout=60)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--environment-file", type=Path, default=LOCAL / "environment.json")
    parser.add_argument("--image", default="golang:1.26-alpine")
    parser.add_argument("--timeout", type=int, default=1800)
    parser.add_argument("--prepare-only", action="store_true", help="freeze a local snapshot only; no SSH or container")
    parser.add_argument("--prepare-runtime", action="store_true", help="cache gcc/musl only in a private integration image; do not run Go tests")
    args = parser.parse_args()
    now = datetime.now(timezone.utc)
    run_id = now.strftime("%Y%m%dT%H%M%S")
    output = LOCAL / "linux-checks" / run_id
    output.mkdir(parents=True, exist_ok=False)
    if args.prepare_runtime:
        client = connect()
        try:
            run(client, "test -f " + shlex.quote(REMOTE + "/.codex-agent-integration"))
            image_id = compiler_image(client, args.image, output)
            print(f"Compiler runtime PASS; image={image_id}; output={output}")
            return 0
        finally:
            client.close()
    manifest = source_manifest()
    archive = snapshot(output, manifest)
    if args.prepare_only:
        print(f"Frozen snapshot: {output}; source_sha256={manifest['sha256']}")
        return 0
    missing = [path for path in HANDOFFS.values() if not (ROOT / path).is_file()]
    if missing:
        print("BLOCKED: awaiting delivery: " + ", ".join(missing))
        return 2
    config = json.loads(args.environment_file.read_text(encoding="utf-8-sig"))
    if config.get("purpose") != "codex-agent-integration" or config.get("synthetic_data") is not True:
        raise RuntimeError("Linux checks require the isolated synthetic integration environment")
    if config.get("remote_root") != REMOTE:
        raise RuntimeError("Integration remote_root differs from the owned test directory")
    values = {key: str(value) for key, value in config.items() if key.isupper() and isinstance(value, (str, int, float))}
    values.update({key: str(value) for key, value in config.get("env", {}).items()})
    values.update(CGO_ENABLED="1", GOMAXPROCS="2", GOFLAGS="-mod=readonly", GOCACHE="/tmp/go-build", GOMODCACHE="/tmp/go-mod")
    if any("\n" in value or "\r" in value for value in values.values()):
        raise ValueError("Environment values must not contain line breaks")
    secrets = [str(value) for key, value in config.items() if any(x in key.upper() for x in ("PASSWORD", "KEY", "TOKEN", "URL"))]
    secrets += [value for key, value in values.items() if any(x in key.upper() for x in ("PASSWORD", "KEY", "TOKEN", "URL"))]
    remote = f"{REMOTE}/checks/{manifest['sha256']}/{run_id}"
    container = "codex-agent-check-" + manifest["sha256"][:12] + "-" + run_id.lower()
    report = {"source_sha256": manifest["sha256"], "started_at": now.isoformat(), "status": "blocked",
              "scope": "isolated synthetic Go race; no deployment", "remote_directory": remote,
              "resources": {"cpus": 2, "memory": "2g", "pids": 128}, "checks": []}
    client, container_id, raw_log = None, "", ""
    started = time.perf_counter()
    try:
        client = connect()
        run(client, "test -f " + shlex.quote(REMOTE + "/.codex-agent-integration"))
        image_id = compiler_image(client, args.image, output)
        if not re.fullmatch(r"sha256:[0-9a-f]{64}", image_id):
            raise RuntimeError("Cannot resolve the pre-existing Linux image to an immutable ID")
        report["image_id"] = image_id
        run(client, "install -d -m 700 " + shlex.quote(remote) + " " + shlex.quote(remote + "/source"))
        with client.open_sftp() as sftp:
            sftp.put(str(archive), remote + "/source.tar.gz")
            sftp.chmod(remote + "/source.tar.gz", 0o600)
        checksum = run(client, "sha256sum " + shlex.quote(remote + "/source.tar.gz")).split()[0]
        if checksum != sha(archive):
            raise RuntimeError("Uploaded source archive SHA256 mismatch")
        write_remote(client, remote + "/source-manifest.json", (output / "source-manifest.json").read_text(encoding="utf-8"))
        write_remote(client, remote + "/test.env", "\n".join(f"{key}={value}" for key, value in sorted(values.items())) + "\n")
        run(client, "tar -xzf " + shlex.quote(remote + "/source.tar.gz") + " -C " + shlex.quote(remote + "/source"))
        script = "\n".join([
            "set -eu", "mkdir -p /work", "cp -a /src/. /work/",
            "cd /work", "go version", "go env GOOS GOARCH CGO_ENABLED CC",
            "go test -race -count=1 -timeout=10m -json ./...",
        ])
        command = ["docker", "run", "-d", "--name", container,
                   "--label", "codex-agent.integration=checks", "--cpus", "2", "--memory", "2g",
                   "--pids-limit", "128", "--network", "host", "--cap-drop", "ALL",
                   "--security-opt", "no-new-privileges", "--env-file", remote + "/test.env",
                   "-v", remote + "/source:/src:ro", image_id, "sh", "-c", script]
        container_id = run(client, shlex.join(command)).strip()
        if not re.fullmatch(r"[0-9a-f]{64}", container_id):
            raise RuntimeError("Docker did not return a valid temporary container ID")
        print(f"Linux race started; source={manifest['sha256'][:16]}, container={container_id[:12]}", flush=True)
        deadline = time.monotonic() + args.timeout
        while True:
            state = json.loads(run(client, "docker inspect --format '{{json .State}}' " + container_id))
            if not state["Running"]:
                break
            if time.monotonic() >= deadline:
                run(client, "docker stop --time 10 " + container_id)
                raise TimeoutError("Linux race exceeded the configured timeout")
            time.sleep(15)
        raw_log = run(client, "docker logs " + container_id + " 2>&1", timeout=60)
        (output / "race.log").write_text(scrub(raw_log, secrets), encoding="utf-8")
        skipped, passed_packages, tests = [], [], 0
        for line in raw_log.splitlines():
            try:
                event = json.loads(line)
            except ValueError:
                continue
            if not isinstance(event, dict):
                continue
            if event.get("Action") == "skip":
                skipped.append(event.get("Test") or event.get("Package"))
            if event.get("Action") == "pass" and not event.get("Test"):
                passed_packages.append(event.get("Package"))
            if event.get("Action") == "pass" and event.get("Test"):
                tests += 1
        status = "fail" if state["ExitCode"] != 0 else "blocked" if skipped or not passed_packages else "pass"
        report.update(status=status, exit_code=state["ExitCode"], oom_killed=state.get("OOMKilled", False),
                      skipped_tests=skipped, passed_packages=passed_packages, passed_test_events=tests)
        report["checks"].append({"id": "go.race", "status": status,
                                 "detail": f"Linux CGO_ENABLED=1 go test -race: exit={state['ExitCode']}, packages={len(passed_packages)}, passed_test_events={tests}, skipped={len(skipped)}",
                                 "artifacts": [{"path": str((output / "race.log").relative_to(ROOT)), "sha256": sha(output / "race.log")},
                                               {"path": str((output / "source-manifest.json").relative_to(ROOT)), "sha256": sha(output / "source-manifest.json")}]})
        report["checks"].append({**report["checks"][-1], "id": "go.test"})
    except Exception as error:
        report.update(status="fail", error=scrub(f"{type(error).__name__}: {error}", secrets))
        if client and container_id:
            try:
                raw_log = run(client, "docker logs " + container_id + " 2>&1", timeout=60)
                (output / "race.log").write_text(scrub(raw_log, secrets), encoding="utf-8")
            except Exception:
                pass
    finally:
        if client and container_id:
            try:
                run(client, "docker rm -f " + container_id, timeout=60)
            except Exception as error:
                report["cleanup_error"] = scrub(str(error), secrets)
                report["status"] = "fail"
        if client:
            client.close()
    if source_manifest()["sha256"] != manifest["sha256"]:
        report["source_changed_after_freeze"] = True
        # The frozen race result remains valid for that exact older snapshot;
        # acceptance.py rejects it for a different delivered source SHA256.
    report["duration_seconds"] = round(time.perf_counter() - started, 3)
    report["completed_at"] = datetime.now(timezone.utc).isoformat()
    (output / "report.json").write_text(json.dumps(report, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    (LOCAL / "linux-checks/latest.json").write_text(json.dumps(report, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    print(f"Linux race {report['status'].upper()}; report={output / 'report.json'}", flush=True)
    return 0 if report["status"] == "pass" else 2 if report["status"] == "blocked" else 1


if __name__ == "__main__":
    raise SystemExit(main())
