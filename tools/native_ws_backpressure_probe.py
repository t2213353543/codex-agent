"""Run an independent Go WS server against the sealed native release locally."""
import json
import os
from pathlib import Path
import secrets
import shutil
import subprocess
import tempfile
from datetime import datetime, timezone

import native_transport_local as r


def main():
    stamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ-") + secrets.token_hex(4)
    out = r.ROOT / ".local/native-ws-backpressure" / stamp
    out.mkdir(parents=True)
    helper = r.ROOT / ".local/native-local-build/20260918T054243Z-b43907cb/target-release-codex-native-transport"
    expected = "6df426c0a8eb703929f888055192e09cec0d2f7eea9391f2ae891e1ca6213740"
    if r.digest(helper.read_bytes()) != expected:
        raise RuntimeError("sealed_helper_hash_mismatch")
    source = r.SOURCE / "tests/ws_backpressure_probe.go"
    probe = out / "probe"
    built = subprocess.run(["go", "build", "-trimpath", "-o", str(probe), str(source)], cwd=r.ROOT,
                           env=dict(os.environ, GOOS="linux", GOARCH="amd64", CGO_ENABLED="0"), capture_output=True)
    if built.returncode:
        raise RuntimeError("probe_compile_failed:" + built.stderr.decode()[-1500:])
    label = "codex-agent.native.ws-backpressure"
    image = "sha256:34c0bef27c9c6e8c1c1107b5453eec9f39115b0498ba91afdaeb96bc36a2b0ec"
    created = r.docker("create", "--name", "codex-native-ws-backpressure-" + stamp.lower(), "--label", label + "=" + stamp,
                       "--network", "none", "--cpus", "2", "--memory", "1g", "--pids-limit", "256", "--cap-drop", "ALL",
                       "--security-opt", "no-new-privileges", image, "sh", "-c",
                       "chmod 500 /probe/helper /probe/probe\nexec /probe/probe -helper /probe/helper -report /probe/report.json")
    cid = created.stdout.decode().strip()
    with tempfile.TemporaryDirectory(prefix="native-ws-bp-") as temporary:
        folder = Path(temporary) / "probe"
        folder.mkdir()
        shutil.copyfile(helper, folder / "helper")
        shutil.copyfile(probe, folder / "probe")
        r.docker("cp", str(folder), cid + ":/probe")
    evidence = {"run_id": stamp, "container": cid, "helper_sha256": expected, "probe_sha256": r.digest(probe.read_bytes()),
                "source_sha256": r.digest(source.read_bytes()), "network": "none", "production_access": False}
    r.save(out / "run.json", evidence)
    r.docker("start", cid)
    print(json.dumps({"container": cid, "output": str(out)}), flush=True)
    r.docker("wait", cid, timeout=90)
    state = r.docker_json("inspect", cid)[0]["State"]
    evidence.update(exit_code=state["ExitCode"], oom_killed=state["OOMKilled"])
    log = r.docker("logs", cid)
    (out / "probe.log").write_bytes(log.stdout + log.stderr)
    with tempfile.TemporaryDirectory(prefix="native-ws-bp-report-") as temporary:
        report = Path(temporary) / "report.json"
        copied = r.docker("cp", cid + ":/probe/report.json", str(report), check=False)
        if copied.returncode == 0:
            shutil.copyfile(report, out / "report.json")
            evidence["report_sha256"] = r.digest(report.read_bytes())
            evidence["passed"] = json.loads(report.read_text())["passed"] and state["ExitCode"] == 0
    r.save(out / "result.json", evidence)
    observed = r.docker_json("inspect", cid)[0]
    if observed["State"]["Running"] or observed["Config"]["Labels"][label] != stamp:
        raise RuntimeError("cleanup_state_or_identity_mismatch")
    r.docker("rm", cid)
    evidence["container_removed"] = True
    r.save(out / "result.json", evidence)
    print(json.dumps(evidence), flush=True)
    return 0 if evidence.get("passed") else 1


if __name__ == "__main__":
    raise SystemExit(main())
