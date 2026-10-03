"""Run official CLI compression fixtures with synthetic auth and network=none."""

from project_layout import server_package, server_source, package_overlay
from datetime import datetime, timezone
import argparse
import hashlib
import json
import os
from pathlib import Path
import secrets
import subprocess
import sys

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "tools"))
from gateway_source_race import go_manifest, snapshot


def run(args, **kwargs):
    result = subprocess.run(args, capture_output=True, **kwargs)
    if result.returncode:
        raise RuntimeError("compression fixture command failed: " + result.stderr.decode(errors="replace")[-1200:])
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--source-dir", type=Path, default=ROOT)
    args = parser.parse_args()
    source = args.source_dir.resolve()
    if not source.is_relative_to(ROOT):
        raise RuntimeError("compression fixture source must be workspace-local")
    if os.environ.get("DOCKER_HOST") or os.environ.get("DOCKER_CONTEXT"):
        raise RuntimeError("local Docker context required")
    endpoint = json.loads(run(["docker", "context", "inspect", "--format", "{{json .Endpoints.docker.Host}}"], timeout=30).stdout)
    if not endpoint.startswith("npipe:////./pipe/docker"):
        raise RuntimeError("local Windows Docker Desktop required")
    official = ROOT / ".local/native-transport-build/official-codex-linux-0.155.0"
    provenance = json.loads((official.parent / "official-cli-0.155.0-source.json").read_text())
    official_sha = hashlib.sha256(official.read_bytes()).hexdigest()
    if official_sha != provenance["binary_sha256"]:
        raise RuntimeError("official CLI artifact differs from verified provenance")
    image = json.loads((ROOT / ".local/native-local-build/20260918T035634Z-80cf0e2a/result.json").read_text())["image_id"]
    run(["docker", "image", "inspect", image], timeout=30)
    stamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%S") + "-" + secrets.token_hex(3)
    output = ROOT / ".local/native-compression" / stamp
    output.mkdir(parents=True)
    probe = output / "compression.test"
    baseline = go_manifest(source)
    frozen = output / "source"
    frozen.mkdir()
    for name, expected in baseline["files"].items():
        raw = (source / name).read_bytes()
        if hashlib.sha256(raw).hexdigest() != expected:
            raise RuntimeError("source changed while creating compression fixture snapshot")
        destination = frozen / name
        destination.parent.mkdir(parents=True, exist_ok=True)
        destination.write_bytes(raw)
    if go_manifest(frozen)["sha256"] != baseline["sha256"]:
        raise RuntimeError("frozen compression fixture source differs")
    package_overlay(server_source(ROOT, "source_native_compression_test.go"), server_source(frozen, "source_native_compression_test.go"), frozen)
    before = go_manifest(frozen)
    snapshot(output, before, frozen)
    env = dict(os.environ, GOOS="linux", GOARCH="amd64", CGO_ENABLED="0")
    run(["go", "test", "-c", "-o", str(probe), server_package(frozen)], cwd=frozen, env=env, timeout=180)
    after = go_manifest(frozen)
    evidence = {"schema":"native-cli-compression-run-v1", "network":"none", "live_accounts":False,
                "official_cli_sha256":official_sha, "source_before":before["sha256"], "source_after":after["sha256"],
                "baseline_source_sha256":baseline["sha256"], "overlay_files":["source_native_compression_test.go"],
                "source_stable_during_compile":before["sha256"] == after["sha256"],
                "test_file_sha256":hashlib.sha256((frozen / "source_native_compression_test.go").read_bytes()).hexdigest(),
                "probe_sha256":hashlib.sha256(probe.read_bytes()).hexdigest(), "image_id":image}
    container = ""
    try:
        name = "codex-cli-compression-" + stamp.lower()
        created = run(["docker","create","--name",name,"--label","codex-agent.integration=synthetic-cli-compression",
                       "--network","none","--cpus","1","--memory","1g","--pids-limit","128","--cap-drop","ALL",
                       "--security-opt","no-new-privileges","-e","CODEX_NATIVE_CAPTURE_BIN=/official-codex",
                       "-e","CODEX_NATIVE_COMPRESSION_NETWORK_NONE=1","-e","CODEX_NATIVE_COMPRESSION_REPORT=/report.json",
                       image,"sh","-c","chmod 500 /official-codex /compression.test\nexec /compression.test -test.v -test.timeout=4m -test.run='^TestSourceNativeCLIRequestCompression$'"], timeout=30)
        container = created.stdout.decode().strip()
        evidence["container"] = container
        for local, remote in [(official,"/official-codex"),(probe,"/compression.test")]:
            run(["docker","cp",str(local),container+":"+remote],timeout=90)
        run(["docker","start",container],timeout=30)
        print(json.dumps({"started":True,"container":container,"output":str(output)}),flush=True)
        run(["docker","wait",container],timeout=300)
        state = json.loads(run(["docker","inspect","--format","{{json .State}}",container],timeout=30).stdout)
        logs = run(["docker","logs",container],timeout=30)
        (output / "probe.log").write_bytes(logs.stdout+logs.stderr)
        run(["docker","cp",container+":/report.json",str(output/"report.json")],timeout=30)
        report = json.loads((output / "report.json").read_text())
        evidence.update(exit_code=state["ExitCode"],oom_killed=state["OOMKilled"],passed=state["ExitCode"]==0 and report["passed"],
                        skipped="--- SKIP:" in (logs.stdout+logs.stderr).decode(errors="replace"))
    finally:
        if container:
            run(["docker","rm","-f",container],timeout=30)
            evidence["container_removed"] = True
        (output / "result.json").write_text(json.dumps(evidence,indent=2)+"\n")
    print(json.dumps(evidence),flush=True)
    return 0 if evidence.get("passed") and not evidence.get("skipped") else 1


if __name__ == "__main__":
    raise SystemExit(main())
