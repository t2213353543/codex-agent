"""Check sealed helper readiness/EOF with zero request envelopes in a local container."""
from datetime import datetime, timezone
import json
from pathlib import Path
import secrets
import shutil
import tempfile

import native_transport_local as runner


def cleanup_owned_container(container, label, stamp):
    observed = runner.docker_json("inspect", container)[0]
    if observed["Id"] != container or observed["Config"]["Labels"].get(label) != stamp:
        raise RuntimeError("cleanup_identity_mismatch")
    # This is a disposable, network-none test job, never a build-cache or
    # production container. Cancel it on every exceptional path before removal.
    runner.docker("rm", "-f", container)


def main():
    stamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ-") + secrets.token_hex(4)
    output = runner.ROOT / ".local/native-startup-probe" / stamp
    output.mkdir(parents=True)
    helper = runner.ROOT / ".local/native-local-build/20260918T054243Z-b43907cb/target-release-codex-native-transport"
    expected = "6df426c0a8eb703929f888055192e09cec0d2f7eea9391f2ae891e1ca6213740"
    if runner.digest(helper.read_bytes()) != expected:
        raise RuntimeError("sealed_helper_changed")
    source = runner.SOURCE / "tests/startup_probe.py"
    image = "sha256:34c0bef27c9c6e8c1c1107b5453eec9f39115b0498ba91afdaeb96bc36a2b0ec"
    label = "codex-agent.native.startup-probe"
    container = ""
    evidence = {"schema": "native-startup-probe-run-v1", "run_id": stamp,
                "helper_sha256": expected, "probe_sha256": runner.digest(source.read_bytes()), "network": "none",
                "live_accounts": False, "production_access": False, "sealed_release_rebuilt": False,
                "probe_source_separate_from_release_source_snapshot": True}
    try:
        container = runner.docker("create", "--name", "codex-native-startup-probe-" + stamp.lower(),
                                  "--label", label + "=" + stamp, "--network", "none", "--cpus", "1", "--memory", "512m",
                                  "--pids-limit", "128", "--cap-drop", "ALL", "--cap-add", "NET_BIND_SERVICE",
                                  "--security-opt", "no-new-privileges", image, "sh", "-c",
                                  "chmod 500 /probe/helper\nexec python3 /probe/startup_probe.py --binary /probe/helper --report /probe/report.json").stdout.decode().strip()
        evidence["container"] = container
        runner.save(output / "run.json", evidence)
        with tempfile.TemporaryDirectory(prefix="native-startup-probe-") as temporary:
            folder = Path(temporary) / "probe"
            folder.mkdir()
            shutil.copyfile(helper, folder / "helper")
            shutil.copyfile(source, folder / "startup_probe.py")
            runner.docker("cp", str(folder), container + ":/probe")
        runner.docker("start", container)
        print(json.dumps({"container": container, "output": str(output)}), flush=True)
        runner.docker("wait", container, timeout=60)
        current = runner.docker_json("inspect", container)[0]
        evidence.update(exit_code=current["State"]["ExitCode"], oom_killed=current["State"]["OOMKilled"])
        log = runner.docker("logs", container)
        (output / "probe.log").write_bytes(log.stdout + log.stderr)
        with tempfile.TemporaryDirectory(prefix="native-startup-evidence-") as temporary:
            path = Path(temporary) / "report.json"
            if runner.docker("cp", container + ":/probe/report.json", str(path), check=False).returncode == 0:
                shutil.copyfile(path, output / "report.json")
                evidence["report_sha256"] = runner.digest(path.read_bytes())
                evidence["passed"] = json.loads(path.read_text())["passed"] and evidence["exit_code"] == 0
    finally:
        if container:
            try:
                cleanup_owned_container(container, label, stamp)
                evidence["container_removed"] = True
            except Exception as error:
                evidence["cleanup_failure_class"] = type(error).__name__
                raise
            finally:
                runner.save(output / "result.json", evidence)
        else:
            runner.save(output / "result.json", evidence)
    print(json.dumps(evidence), flush=True)
    return 0 if evidence.get("passed") else 1


if __name__ == "__main__":
    raise SystemExit(main())
