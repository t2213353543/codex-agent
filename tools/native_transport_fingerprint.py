"""Run pinned h2/TLS observations in disposable local containers only."""
from __future__ import annotations

from datetime import datetime, timezone
import argparse
import json
from pathlib import Path
import secrets
import shutil
import tempfile

import native_transport_local as runner


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--prepared-image", help="Existing isolated pinned-h2 image ID, avoiding another dependency download")
    parser.add_argument("--native-run", default="20260918T035634Z-80cf0e2a")
    args = parser.parse_args()
    root = runner.ROOT
    stamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ-") + secrets.token_hex(4)
    output = root / ".local/native-fingerprint" / stamp
    output.mkdir(parents=True)
    native = (root / ".local/native-local-build" / args.native_run).resolve()
    if not native.is_relative_to((root / ".local/native-local-build").resolve()):
        raise RuntimeError("native_run_invalid")
    helper = native / "target-debug-codex-native-transport"
    cli = root / ".local/native-transport-build/official-codex-linux-0.155.0"
    result = json.loads((native / "result.json").read_text())
    expected = next(item["sha256"] for item in result["artifacts"] if item["path"].endswith(helper.name))
    if runner.digest(helper.read_bytes()) != expected or result["state"]["ExitCode"] != 0:
        raise RuntimeError("helper_hash_mismatch")
    if runner.digest(cli.read_bytes()) != "660e159a49e823ac8e5986cb238f73158ce4b957d40d9292f8de90862644b501":
        raise RuntimeError("official_cli_hash_mismatch")
    evidence = {"run_id": stamp, "helper_sha256": runner.digest(helper.read_bytes()), "cli_sha256": runner.digest(cli.read_bytes()),
                "network_probe": "none", "production_access": False, "output": str(output)}
    base = "sha256:be2419204d24d244b6f51a751a796606b7a51abf6c80bb6037bb26943adab344"
    common = ["--cpus", "2", "--memory", "1g", "--pids-limit", "256", "--security-opt", "no-new-privileges"]
    if args.prepared_image:
        image = runner.docker_json("image", "inspect", args.prepared_image)[0]["Id"]
    else:
        prepare = runner.docker("create", "--name", "codex-native-fingerprint-prepare-" + stamp.lower(), "--label", "codex-agent.native.fingerprint=" + stamp,
                                *common, base, "sh", "-c", "set -eu\napk add --no-cache py3-pip\npython3 -m venv /h2env\n/h2env/bin/pip install --disable-pip-version-check h2==4.3.0 hyperframe==6.1.0 hpack==4.1.0\n").stdout.decode().strip()
        evidence["prepare_container"] = prepare
        runner.save(output / "run.json", evidence)
        runner.docker("start", prepare)
        print(json.dumps({"phase": "prepare", "container": prepare, "output": str(output)}), flush=True)
        runner.docker("wait", prepare, timeout=240)
        state = runner.docker_json("inspect", prepare)[0]["State"]
        if state["ExitCode"] != 0:
            runner.save(output / "prepare-state.json", state)
            (output / "prepare.log").write_bytes(runner.docker("logs", prepare).stdout)
            raise RuntimeError("dependency_prepare_failed")
        tag = "codex-native-fingerprint:" + stamp.lower()
        runner.docker("commit", prepare, tag, timeout=120)
        image = runner.docker_json("image", "inspect", tag)[0]["Id"]
    evidence["image_id"] = image
    container = runner.docker("create", "--name", "codex-native-fingerprint-" + stamp.lower(), "--label", "codex-agent.native.fingerprint=" + stamp,
                              "--network", "none", *common, image, "sh", "-c", "chmod 500 /probe/helper /probe/cli\nexec /h2env/bin/python /probe/transport_fingerprint.py --cli /probe/cli --helper /probe/helper --report /probe/report.json").stdout.decode().strip()
    evidence["container"] = container
    with tempfile.TemporaryDirectory(prefix="native-fingerprint-") as temporary:
        folder = Path(temporary) / "probe"
        folder.mkdir()
        for source, name in [(helper, "helper"), (cli, "cli"), (root / "native-transport/tests/loopback.py", "loopback.py"),
                             (root / "native-transport/tests/transport_fingerprint.py", "transport_fingerprint.py")]:
            shutil.copyfile(source, folder / name)
        runner.docker("cp", str(folder), container + ":/probe")
    runner.save(output / "run.json", evidence)
    runner.docker("start", container)
    print(json.dumps({"phase": "probe", "container": container, "output": str(output)}), flush=True)
    runner.docker("wait", container, timeout=360)
    state = runner.docker_json("inspect", container)[0]["State"]
    evidence["state"] = {key: state[key] for key in ("Status", "Running", "ExitCode", "OOMKilled", "StartedAt", "FinishedAt")}
    log = runner.docker("logs", container)
    (output / "probe.log").write_bytes(log.stdout + log.stderr)
    with tempfile.TemporaryDirectory(prefix="native-fingerprint-result-") as temporary:
        target = Path(temporary) / "report.json"
        copied = runner.docker("cp", container + ":/probe/report.json", str(target), check=False)
        if copied.returncode == 0:
            shutil.copyfile(target, output / "report.json")
            evidence["report_sha256"] = runner.digest(target.read_bytes())
            evidence["passed"] = json.loads(target.read_text())["passed"] and state["ExitCode"] == 0
    runner.save(output / "result.json", evidence)
    removed = []
    for identity in [container, evidence.get("prepare_container")]:
        if not identity:
            continue
        inspected = runner.docker_json("inspect", identity)[0]
        if inspected["State"]["Running"] or inspected["Config"].get("Labels", {}).get("codex-agent.native.fingerprint") != stamp:
            raise RuntimeError("cleanup_identity_or_state_mismatch")
        runner.docker("rm", identity)
        removed.append(identity)
    evidence["removed_containers"] = removed
    runner.save(output / "result.json", evidence)
    print(json.dumps(evidence), flush=True)
    return 0 if evidence.get("passed") else 1


if __name__ == "__main__":
    raise SystemExit(main())
