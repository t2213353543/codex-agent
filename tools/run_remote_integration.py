"""Run synthetic migration acceptance beside the isolated PG/Redis services.

Only the integration directory, a private Python runtime image and temporary
resource-limited containers are changed. Downloaded outputs contain reports and
redacted logs only, never legacy/exported credentials or database data.
"""
from __future__ import annotations

import argparse
from concurrent.futures import ThreadPoolExecutor
from datetime import datetime, timezone
import hashlib
import io
import json
from pathlib import Path
import re
import shlex
import tarfile
import time

from acceptance import ROOT, sha, source_manifest
from integration_env import LOCAL, REMOTE, connect, run, write_remote
from run_linux_checks import scrub


def image_identity(client, name):
    result = run(client, "docker image inspect --format '{{.Id}}' " + shlex.quote(name)).strip()
    if not re.fullmatch(r"sha256:[0-9a-f]{64}", result):
        raise RuntimeError("Docker image did not resolve to an immutable ID")
    return result


def wait_container(client, identifier, timeout):
    if not re.fullmatch(r"[0-9a-f]{64}", identifier):
        raise RuntimeError("Invalid integration container ID")
    deadline = time.monotonic() + timeout
    while True:
        state = json.loads(run(client, "docker inspect --format '{{json .State}}' " + identifier))
        if not state["Running"]:
            return state
        if time.monotonic() >= deadline:
            run(client, "docker stop --time 10 " + identifier)
            raise TimeoutError("Temporary integration container exceeded its time limit")
        time.sleep(10)


def ensure_runtime(client, base_image, output):
    base_id = image_identity(client, base_image)
    tag = "codex-agent-integration-python:" + base_id.split(":")[1][:16] + "-v1"
    existing = run(client, "docker image ls --no-trunc --format '{{.ID}}' " + shlex.quote(tag)).strip()
    if existing:
        return image_identity(client, tag)
    container = "codex-agent-python-" + datetime.now(timezone.utc).strftime("%Y%m%d%H%M%S")
    script = "\n".join([
        "set -eu", "apk add --no-cache python3 py3-pip",
        "python3 -m pip install --no-cache-dir --target /opt/codex-agent-runtime/.local/performance-python 'psycopg[binary]'",
        "python3 --version",
        "PYTHONPATH=/opt/codex-agent-runtime/.local/performance-python python3 -c 'import psycopg; print(psycopg.__version__)'",
    ])
    command = ["docker", "run", "-d", "--name", container,
               "--label", "codex-agent.integration=python-runtime", "--cpus", "1", "--memory", "1g",
               "--pids-limit", "128", "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
               base_id, "sh", "-c", script]
    identifier = run(client, shlex.join(command)).strip()
    print("Preparing isolated Python runtime container", flush=True)
    try:
        state = wait_container(client, identifier, 900)
        log = run(client, "docker logs " + identifier + " 2>&1", timeout=60)
        (output / "runtime.log").write_text(log, encoding="utf-8")
        if state["ExitCode"] != 0:
            raise RuntimeError("Container-only Python dependency installation failed; inspect runtime.log")
        run(client, "docker commit " + identifier + " " + shlex.quote(tag), timeout=120)
        return image_identity(client, tag)
    finally:
        run(client, "docker rm -f " + identifier, timeout=60)


def freeze_payload(output, binary, manifest):
    selected = [ROOT / "tools/migration_verify.py", ROOT / "integration/tests/contract.py",
                ROOT / "integration/fixtures/synthetic.py"]
    selected.extend(p for p in (ROOT / "frontend/dist").rglob("*") if p.is_file())
    if not (ROOT / "frontend/dist/index.html").is_file():
        raise RuntimeError("Frontend build output is missing")
    payload = {"source_sha256": manifest["sha256"], "binary_sha256": sha(binary), "files": {}}
    archive = output / "acceptance.tar.gz"
    with tarfile.open(archive, "w:gz") as tar:
        for path in [*selected, binary]:
            name = "codex-agent" if path == binary else path.relative_to(ROOT).as_posix()
            raw = path.read_bytes()
            digest = hashlib.sha256(raw).hexdigest()
            expected = manifest["files"].get(name)
            if expected and digest != expected:
                raise RuntimeError("Source changed while freezing remote integration payload")
            info = tarfile.TarInfo(name)
            info.size, info.mode, info.mtime = len(raw), 0o755 if name == "codex-agent" else 0o644, 0
            tar.addfile(info, io.BytesIO(raw))
            payload["files"][name] = digest
    if source_manifest()["sha256"] != manifest["sha256"]:
        raise RuntimeError("Source changed while freezing remote integration payload; rerun")
    payload["archive_sha256"] = sha(archive)
    (output / "payload-manifest.json").write_text(json.dumps(payload, indent=2), encoding="utf-8")
    return archive, payload


def upload_parts(client, archive, remote):
    chunk_size = 4 * 1024 * 1024
    parts = []
    with archive.open("rb") as handle:
        while content := handle.read(chunk_size):
            digest = hashlib.sha256(content).hexdigest()
            parts.append((f"{remote}/part-{len(parts):04d}-{digest}", content, digest))

    def upload(part):
        path, content, expected = part
        for attempt in range(2):
            connection = connect()
            try:
                # Completed hash-named parts can be reused on an interrupted upload.
                result = run(connection, "if test -f " + shlex.quote(path) + "; then sha256sum " + shlex.quote(path) + "; fi").strip()
                if result and result.split()[0] == expected:
                    return
                with connection.open_sftp() as sftp:
                    with sftp.file(path, "wb") as destination:
                        destination.set_pipelined(True)
                        destination.write(content)
                    sftp.chmod(path, 0o600)
                if run(connection, "sha256sum " + shlex.quote(path)).split()[0] != expected:
                    raise RuntimeError("Uploaded archive part checksum mismatch")
                return
            except Exception:
                if attempt:
                    raise
            finally:
                connection.close()
    with ThreadPoolExecutor(max_workers=4) as executor:
        list(executor.map(upload, parts))
    destination = remote + "/acceptance.tar.gz"
    run(client, "cat " + " ".join(shlex.quote(part[0]) for part in parts) + " > " + shlex.quote(destination))
    if run(client, "sha256sum " + shlex.quote(destination)).split()[0] != sha(archive):
        raise RuntimeError("Assembled integration archive checksum mismatch")
    return destination


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", type=Path, help="explicit candidate Linux binary; never defaults to production dist")
    parser.add_argument("--environment-file", type=Path, default=LOCAL / "migration-environment.json")
    parser.add_argument("--image", default="golang:1.26-alpine")
    parser.add_argument("--prepare-runtime", action="store_true", help="install private container-only Python dependencies and stop")
    parser.add_argument("--timeout", type=int, default=900)
    args = parser.parse_args()
    if not args.prepare_runtime and (not args.binary or not args.binary.is_file()):
        parser.error("--binary must point to an explicit existing candidate Linux binary")
    run_id = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%S")
    output = LOCAL / "remote-migration" / run_id
    output.mkdir(parents=True, exist_ok=False)
    report = {"status": "blocked", "started_at": datetime.now(timezone.utc).isoformat(),
              "scope": "synthetic remote migration acceptance; no production migration or deployment"}
    secrets, client, identifier = [], None, ""
    try:
        client = connect()
        run(client, "test -f " + shlex.quote(REMOTE + "/.codex-agent-integration"))
        runtime_id = ensure_runtime(client, args.image, output)
        report["runtime_image_id"] = runtime_id
        if args.prepare_runtime:
            report.update(status="pass", detail="Python and psycopg installed only in a private integration runtime image")
        else:
            config = json.loads(args.environment_file.read_text(encoding="utf-8-sig"))
            if config.get("purpose") != "codex-agent-integration" or config.get("synthetic_data") is not True:
                raise RuntimeError("Migration acceptance requires purpose=codex-agent-integration and synthetic_data=true")
            if config.get("database", {}).get("name") != "codex_agent_migration":
                raise RuntimeError("Remote migration acceptance requires the dedicated codex_agent_migration database")
            from urllib.parse import urlsplit
            if urlsplit(config.get("DATABASE_URL", "")).path != "/codex_agent_migration":
                raise RuntimeError("DATABASE_URL does not select the dedicated migration database")
            secrets = [str(value) for key, value in config.items() if any(x in key.upper() for x in ("PASSWORD", "KEY", "TOKEN", "URL"))]
            if run(client, "ss -H -ltn '( sport = :28881 )'").strip():
                raise RuntimeError("Integration port 28881 is already occupied")
            manifest = source_manifest()
            archive, payload = freeze_payload(output, args.binary.resolve(), manifest)
            remote = f"{REMOTE}/acceptance/{manifest['sha256']}/{run_id}"
            run(client, "install -d -m 700 " + " ".join(shlex.quote(remote + suffix) for suffix in ("", "/source", "/work", "/private")))
            print(f"Uploading frozen integration payload ({archive.stat().st_size} bytes)", flush=True)
            destination = upload_parts(client, archive, remote)
            run(client, "tar -xzf " + shlex.quote(destination) + " -C " + shlex.quote(remote + "/source"))
            write_remote(client, remote + "/private/migration-environment.json", json.dumps(config))
            write_remote(client, remote + "/payload-manifest.json", json.dumps(payload, indent=2))
            script = "\n".join([
                "set -eu", "cp -a /src/. /work/", "cp -a /opt/codex-agent-runtime/. /work/", "cd /work",
                "python3 tools/migration_verify.py --binary ./codex-agent --environment-file private/migration-environment.json",
            ])
            name = "codex-agent-migration-" + run_id.lower()
            command = ["docker", "run", "-d", "--name", name, "--label", "codex-agent.integration=migration",
                       "--cpus", "1", "--memory", "1g", "--pids-limit", "128", "--network", "host",
                       "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
                       "-v", remote + "/source:/src:ro", "-v", remote + "/work:/work",
                       "-v", remote + "/private:/work/private:ro", runtime_id, "sh", "-c", script]
            identifier = run(client, shlex.join(command)).strip()
            report.update(source_sha256=manifest["sha256"], binary_sha256=payload["binary_sha256"], remote_directory=remote)
            print(f"Remote synthetic acceptance started; source={manifest['sha256'][:16]}, binary={payload['binary_sha256'][:16]}", flush=True)
            state = wait_container(client, identifier, args.timeout)
            raw_log = run(client, "docker logs " + identifier + " 2>&1", timeout=60)
            log = output / "migration.log"
            log.write_text(scrub(raw_log, secrets), encoding="utf-8")
            raw = run(client, "cat " + shlex.quote(remote + "/work/.local/integration/migration-latest.json"))
            result = json.loads(scrub(raw, secrets))
            report.update(result)
            report.update(source_sha256=manifest["sha256"], exit_code=state["ExitCode"], oom_killed=state.get("OOMKilled", False))
            if result.get("binary_sha256") != payload["binary_sha256"]:
                raise RuntimeError("Executed migration binary does not match the uploaded candidate hash")
            artifacts = [{"path": str(log.relative_to(ROOT)), "sha256": sha(log)},
                         {"path": str((output / "payload-manifest.json").relative_to(ROOT)), "sha256": sha(output / "payload-manifest.json")}]
            for check in report.get("checks", []):
                check["artifacts"] = artifacts
            checks = report.get("checks", [])
            report["status"] = "pass" if state["ExitCode"] == 0 and checks and all(c.get("status") == "pass" for c in checks) else "fail"
            report["source_changed_after_freeze"] = source_manifest()["sha256"] != manifest["sha256"]
    except Exception as error:
        report.update(status="fail", error=scrub(f"{type(error).__name__}: {error}", secrets))
        if client and identifier:
            try:
                log = run(client, "docker logs " + identifier + " 2>&1", timeout=60)
                (output / "migration.log").write_text(scrub(log, secrets), encoding="utf-8")
            except Exception:
                pass
    finally:
        if client and identifier:
            try:
                run(client, "docker rm -f " + identifier, timeout=60)
            except Exception as error:
                report.update(status="fail", cleanup_error=scrub(str(error), secrets))
        if client:
            client.close()
    report["completed_at"] = datetime.now(timezone.utc).isoformat()
    raw_report = json.dumps(report, ensure_ascii=False, indent=2) + "\n"
    (output / "report.json").write_text(raw_report, encoding="utf-8")
    if not args.prepare_runtime:
        (LOCAL / "remote-migration/report.json").write_text(raw_report, encoding="utf-8")
    print(f"Remote integration {report['status'].upper()}; report={output / 'report.json'}", flush=True)
    return 0 if report["status"] == "pass" else 1


if __name__ == "__main__":
    raise SystemExit(main())
