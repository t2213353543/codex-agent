"""Run Linux race checks on the exact frozen system-performance candidate.

Prepare without starting a compiler:
  python tools/system_performance_race.py --source <candidate/source> --prepare-only
Run the prepared archive after performance measurements finish:
  python tools/system_performance_race.py --prepared <output-directory>
Select an explicit nonempty set of frozen regression tests:
  python tools/system_performance_race.py --source <candidate/source> --test-run '^TestPGKeySnapshot'
"""
from __future__ import annotations

import argparse
from datetime import datetime, timezone
import hashlib
import io
import json
from pathlib import Path
import re
import secrets
import shlex
import tarfile
import time

from gateway_source_race import isolated_environment, verify_remote
from integration_env import ROOT, REMOTE, connect, run, write_remote
from run_linux_checks import scrub
from system_performance import management_tests


def prepare(source: Path, output: Path, overlays: list[Path], test_run: str | None = None) -> dict:
    source = source.resolve()
    manifest = json.loads((source.parent / "source-manifest.json").read_text(encoding="utf-8"))
    if hashlib.sha256(json.dumps(manifest["files"], sort_keys=True).encode()).hexdigest() != manifest["sha256"]:
        raise RuntimeError("Frozen candidate manifest digest mismatch")
    if test_run is None:
        selected = management_tests(source)
    else:
        selection = re.compile(test_run)
        selected = []
        for relative, expected in manifest["files"].items():
            if not relative.startswith("internal/server/") or not relative.endswith("_test.go"):
                continue
            raw = (source / relative).read_bytes()
            if hashlib.sha256(raw).hexdigest() != expected:
                raise RuntimeError("Frozen test source checksum mismatch: " + relative)
            selected.extend(name for name in re.findall(r"^func (Test\w+)\(t \*testing.T\)", raw.decode("utf-8"), re.M)
                            if selection.search(name))
    overlay_files = {}
    for path in overlays:
        if not path.name.endswith("_test.go"):
            raise RuntimeError("Race overlays may only add Go test files")
        relative = "internal/server/" + path.name
        if relative in manifest["files"] or relative in overlay_files:
            raise RuntimeError("Race test overlay cannot replace a frozen source file")
        raw = path.read_bytes()
        overlay_files[relative] = raw
        selected.extend(name for name in re.findall(r"^func (Test\w+)\(t \*testing.T\)", raw.decode("utf-8"), re.M)
                        if test_run is None or selection.search(name))
    selected = sorted(set(selected))
    if not selected:
        raise RuntimeError("Race test selector matched no frozen top-level tests")
    required = {"TestPGManagedAccountUsageMatchesStableIDBatch", "TestPGManagedBundleResourceMetadataReadOnly",
                "TestManagedBundleResourceMetadataReadOnly"}
    if test_run is None and not required.issubset(selected):
        raise RuntimeError("Frozen candidate is missing required management regressions")
    output.mkdir(parents=True, exist_ok=False)
    archive = output / "source.tar.gz"
    with tarfile.open(archive, "w:gz") as tar:
        for relative, expected in sorted(manifest["files"].items()):
            target = (source / relative).resolve()
            if not target.is_relative_to(source) or target.is_symlink():
                raise RuntimeError("Invalid frozen source path")
            raw = target.read_bytes()
            if hashlib.sha256(raw).hexdigest() != expected:
                raise RuntimeError("Frozen source no longer matches performance snapshot: " + relative)
            entry = tarfile.TarInfo(relative)
            entry.size, entry.mode, entry.mtime = len(raw), 0o644, 0
            tar.addfile(entry, io.BytesIO(raw))
        for relative, raw in sorted(overlay_files.items()):
            entry = tarfile.TarInfo(relative)
            entry.size, entry.mode, entry.mtime = len(raw), 0o644, 0
            tar.addfile(entry, io.BytesIO(raw))
    manifest["test_overlays"] = {path: hashlib.sha256(raw).hexdigest() for path, raw in overlay_files.items()}
    effective_files = {**manifest["files"], **manifest["test_overlays"]}
    manifest["effective_source_sha256"] = hashlib.sha256(json.dumps(effective_files, sort_keys=True).encode()).hexdigest()
    (output / "source-manifest.json").write_text(json.dumps(manifest, indent=2), encoding="utf-8")
    report = {"status": "prepared", "source_sha256": manifest["sha256"], "source": str(source),
              "archive_sha256": hashlib.sha256(archive.read_bytes()).hexdigest(),
              "test_overlays": manifest["test_overlays"], "effective_source_sha256": manifest["effective_source_sha256"],
              "selected_top_level_tests": selected, "test_run": test_run, "synthetic_only": True, "production_touched": False,
              "resources": {"cpus": 2, "memory": "2g", "pids": 128}}
    (output / "report.json").write_text(json.dumps(report, indent=2), encoding="utf-8")
    return report


def execute(output: Path) -> dict:
    report = json.loads((output / "report.json").read_text(encoding="utf-8"))
    archive = output / "source.tar.gz"
    if hashlib.sha256(archive.read_bytes()).hexdigest() != report["archive_sha256"]:
        raise RuntimeError("Prepared source archive checksum mismatch")
    stamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%S") + "-" + secrets.token_hex(3)
    remote = REMOTE + "/checks/system-performance-race-" + stamp.lower()
    name = "codex-system-perf-race-" + stamp.lower()
    label = "codex-agent.integration=system-performance-race"
    values, private = isolated_environment("codex-agent:integration:system-performance-race:" + stamp.lower() + ":")
    values["STATS_TIMEZONE"] = "UTC"
    selected = report["selected_top_level_tests"]
    if not selected:
        raise RuntimeError("Prepared race selection is empty")
    selector = "^(" + "|".join(selected) + ")$"
    started = time.monotonic()
    report.update(status="running", remote_directory=remote, started_at=datetime.now(timezone.utc).isoformat())
    client, identifier, env_written, container_attempted = None, "", False, False
    try:
        client = connect()
        image = verify_remote(client)
        report["image_id"] = image
        run(client, "install -d -m 700 " + shlex.quote(remote) + " " + shlex.quote(remote + "/source"))
        with client.open_sftp() as sftp:
            sftp.put(str(archive), remote + "/source.tar.gz")
            sftp.chmod(remote + "/source.tar.gz", 0o600)
        if run(client, "sha256sum " + shlex.quote(remote + "/source.tar.gz")).split()[0] != report["archive_sha256"]:
            raise RuntimeError("Transferred race snapshot checksum mismatch")
        write_remote(client, remote + "/source-manifest.json", (output / "source-manifest.json").read_text(encoding="utf-8"))
        env_written = True
        write_remote(client, remote + "/test.env", "".join(k + "=" + v + "\n" for k, v in sorted(values.items())))
        run(client, "tar -xzf " + shlex.quote(remote + "/source.tar.gz") + " -C " + shlex.quote(remote + "/source"))
        test = ["go", "test", "-race", "-count=1", "-timeout=12m", "-json", "-run", selector, "./internal/server"]
        script = "\n".join(["set -eu", "mkdir -p /work", "cp -a /src/. /work/", "cd /work",
                            "go version", "go env GOOS GOARCH CGO_ENABLED CC", shlex.join(test)])
        command = ["docker", "run", "-d", "--name", name, "--label", label, "--cpus", "2", "--memory", "2g",
                   "--pids-limit", "128", "--network", "host", "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
                   "--env-file", remote + "/test.env", "-v", remote + "/source:/src:ro", image, "sh", "-c", script]
        container_attempted = True
        identifier = run(client, shlex.join(command)).strip()
        if not re.fullmatch(r"[0-9a-f]{64}", identifier):
            raise RuntimeError("Unexpected isolated race container identity")
        print(json.dumps({"race_started": True, "source_sha256": report["source_sha256"], "selected": len(selected)}), flush=True)
        deadline = time.monotonic() + 1500
        while True:
            state = json.loads(run(client, "docker inspect --format '{{json .State}}' " + identifier))
            if not state["Running"]:
                break
            if time.monotonic() > deadline:
                raise TimeoutError("Isolated race exceeded 1500 seconds")
            time.sleep(10)
        raw = run(client, "docker logs " + identifier + " 2>&1", timeout=90)
        (output / "race.log").write_text(scrub(raw, private), encoding="utf-8")
        outcomes, packages = {"pass": [], "fail": [], "skip": []}, []
        for line in raw.splitlines():
            try:
                event = json.loads(line)
            except ValueError:
                continue
            if not isinstance(event, dict):
                continue
            action, test_name = event.get("Action"), event.get("Test")
            if test_name and action in outcomes:
                outcomes[action].append(test_name)
            elif not test_name and action == "pass":
                packages.append(event.get("Package"))
        missing = sorted(set(selected) - set(outcomes["pass"]) - set(outcomes["fail"]) - set(outcomes["skip"]))
        clean = not state["ExitCode"] and not outcomes["fail"] and not outcomes["skip"] and not missing and packages
        report.update(status="pass" if clean else "fail", exit_code=state["ExitCode"], oom_killed=state.get("OOMKilled", False),
                      passed_top_level_tests=sum("/" not in x for x in outcomes["pass"]), failed_tests=outcomes["fail"],
                      skipped_tests=outcomes["skip"], missing_tests=missing, passed_packages=packages)
    except Exception as error:
        report.update(status="fail", error=scrub(type(error).__name__ + ": " + str(error), private))
        if client and identifier:
            try:
                (output / "race.log").write_text(scrub(run(client, "docker logs " + identifier + " 2>&1"), private), encoding="utf-8")
            except Exception:
                pass
    finally:
        if client:
            if container_attempted:
                try:
                    cleanup = run(client, shlex.join(["docker", "ps", "-aq", "--no-trunc", "--filter", "name=^/" + name + "$",
                                                     "--filter", "label=" + label])).strip()
                    if cleanup:
                        if not re.fullmatch(r"[0-9a-f]{64}", cleanup):
                            raise RuntimeError("Unexpected cleanup identity")
                        run(client, "docker rm -f " + cleanup)
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
        report.update(duration_seconds=round(time.monotonic()-started, 3), completed_at=datetime.now(timezone.utc).isoformat())
        (output / "report.json").write_text(json.dumps(report, indent=2), encoding="utf-8")
    return report


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--source", type=Path)
    parser.add_argument("--output", type=Path)
    parser.add_argument("--prepared", type=Path)
    parser.add_argument("--overlay", type=Path, action="append", default=[])
    parser.add_argument("--prepare-only", action="store_true")
    parser.add_argument("--test-run", help="Python regex selecting frozen top-level Go test names; an empty match is an error")
    args = parser.parse_args()
    if args.prepared:
        if args.test_run is not None:
            parser.error("--test-run is recorded during preparation; cannot change a prepared selection")
        output = args.prepared.resolve()
    else:
        if not args.source:
            parser.error("--source or --prepared is required")
        output = (args.output or ROOT / ".local/system-perf-20260927" / ("race-" + datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%S"))).resolve()
        report = prepare(args.source, output, args.overlay, args.test_run)
        print(json.dumps({"prepared": str(output), "source_sha256": report["source_sha256"], "selected": len(report["selected_top_level_tests"])}), flush=True)
    if args.prepare_only:
        return 0
    report = execute(output)
    print(json.dumps({"status": report["status"], "report": str(output / "report.json"),
                      "passed": report.get("passed_top_level_tests"), "error": report.get("error")}), flush=True)
    return 0 if report["status"] == "pass" else 1


if __name__ == "__main__":
    raise SystemExit(main())
