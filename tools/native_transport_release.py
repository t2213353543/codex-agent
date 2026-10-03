"""Package a verified release helper with frozen-source and ELF provenance."""
from __future__ import annotations

import argparse
import gzip
import io
import json
from pathlib import Path
import tarfile

import native_transport_local as runner


def frozen_file(archive: Path, name: str) -> bytes:
    with tarfile.open(archive, "r:gz") as source:
        member = source.getmember(name)
        if not member.isfile():
            raise RuntimeError("package_source_not_regular")
        return source.extractfile(member).read()


def package(run_id: str) -> dict:
    folder, meta = runner.metadata(run_id)
    result = json.loads((folder / "result.json").read_text(encoding="utf-8"))
    required = {"lock_check", "clean_helper", "build", "websocket_tests", "loopback"}
    if (result.get("profile") != "release" or result["state"]["Running"]
            or result["state"]["ExitCode"] != 0 or result["build"]["status"] != "complete"
            or not required <= set(result["build"]["completed"])):
        raise RuntimeError("release_validation_incomplete")
    for artifact in result["artifacts"]:
        path = runner.ROOT / artifact["path"]
        if not path.is_file() or runner.digest(path.read_bytes()) != artifact["sha256"]:
            raise RuntimeError("release_artifact_changed")
    archive = folder / "source.tar.gz"
    if runner.digest(archive.read_bytes()) != meta["archive_sha256"]:
        raise RuntimeError("release_source_archive_changed")
    ready = json.loads((folder / "helper-ready.json").read_text())
    elf = json.loads((folder / "elf-inspection.json").read_text())
    tests = json.loads((folder / "test-results.json").read_text())
    toolchain_host = (folder / "toolchain-host.txt").read_text()
    if "host: x86_64-unknown-linux-musl" not in toolchain_host:
        raise RuntimeError("release_target_not_verified_musl")
    if (ready.get("http_client_lifecycle") != "cli_request" or "websocket_message_v1" not in ready.get("capabilities", [])
            or not elf.get("elf_machine_x86_64") or not tests.get("passed") or tests.get("skipped") != 0 or tests.get("tests", 0) < 25):
        raise RuntimeError("release_runtime_contract_incomplete")
    compression_tests = 3 if "request_zstd_v1" in ready.get("capabilities", []) else 0
    if compression_tests and ("compression_tests" not in result["build"]["completed"] or tests["tests"] < 26):
        raise RuntimeError("release_compression_validation_incomplete")
    binary_relative = meta["target_artifact_path"]
    binary = folder / binary_relative.replace("/", "-")
    binary_hash = runner.digest(binary.read_bytes())
    if binary_hash != elf["binary_sha256"]:
        raise RuntimeError("release_elf_binary_mismatch")
    output = runner.ROOT / ".local/native-releases" / (ready["cli_source"] + "-linux-x86_64-" + binary_hash[:12])
    if output.exists():
        raise RuntimeError("release_package_already_exists")
    output.mkdir(parents=True)
    payload = {
        "codex-native-transport": binary.read_bytes(),
        "Cargo.lock": (folder / "Cargo.lock").read_bytes(),
        "source-manifest.json": (folder / "source-manifest.json").read_bytes(),
        "build-result.json": (folder / "result.json").read_bytes(),
        "toolchain.txt": (folder / "toolchain.txt").read_bytes(),
        "toolchain-host.txt": (folder / "toolchain-host.txt").read_bytes(),
        "resolved-features.txt": (folder / "resolved-features.txt").read_bytes(),
        "lock-check.json": (folder / "lock-check.json").read_bytes(),
        "test-results.json": (folder / "test-results.json").read_bytes(),
        "elf-inspection.json": (folder / "elf-inspection.json").read_bytes(),
        "elf-inspection.txt": (folder / "elf-inspection.txt").read_bytes(),
        "helper-ready.json": (folder / "helper-ready.json").read_bytes(),
    }
    smoke_path = folder / "release-smoke.json"
    smoke = None
    if smoke_path.is_file():
        smoke = json.loads(smoke_path.read_text())
        if not smoke.get("passed") or smoke.get("binary_sha256") != binary_hash:
            raise RuntimeError("release_smoke_evidence_invalid")
        payload["release-smoke.json"] = smoke_path.read_bytes()
    for name in ("UPSTREAM.json", "WEBSOCKET-ADAPTER.patch", "Cargo.toml", "LICENSE", "NOTICE"):
        payload[name] = frozen_file(archive, name)
    provenance = json.loads(payload["UPSTREAM.json"])
    static = elf["statically_linked_observed"]
    linkage = "ELF inspection found no PT_INTERP and no DT_NEEDED entries." if static else "This artifact is dynamically linked; see elf-inspection.json for interpreter and DT_NEEDED dependencies."
    readme = f'''# Native Transport Release

This is a verified local release artifact, not a production deployment.

CLI source: {ready["cli_source"]}
Platform: Linux x86_64, Rust musl target
Profile: release, original Cargo thin LTO/codegen-units=4/strip=symbols
Binary SHA256: {binary_hash}
Binary bytes: {binary.stat().st_size}
Linkage: {linkage}
Binary-only read-only rootfs startup: {"passed" if smoke else "not exercised"}; no libc or dynamic loader was included.

Validation: {tests["tests"]} HTTP/UDS tests ran against this release executable, with zero
skips. Four real WebSocket socket tests ran under cargo test --release using
the same production Rust modules. {compression_tests} official request-compression tests ran under the same profile.
Go/official-CLI integration results for a
different debug binary do not automatically certify this binary; the release
digest must be supplied explicitly in any final integration/deployment check.

The helper has no standalone CLI. The Go gateway starts it with private stdin
configuration and a same-UID Unix socket in a 0700 directory. Do not place
credentials on argv or in this package. Example gateway environment only:

```sh
CODEX_NATIVE_HTTP_HELPER=/opt/codex-agent/codex-native-transport
CODEX_NATIVE_HTTP_SHA256={binary_hash}
CODEX_NATIVE_HTTP_LIFECYCLE=cli_request
# Enable only with the independently verified native WS gateway integration:
CODEX_NATIVE_WEBSOCKET=1
```

Keep the default cli_request lifecycle for CLI source alignment. Explicit
pooled mode changes connection/TLS-session lifetime and is a performance
option, not identical CLI behavior. Native WS lifetime is independent.

The target host must support this architecture, Unix peer credentials and
the gateway runtime limits. Static ELF linkage is not a claim that CentOS or
every Linux kernel was exercised. Native CA roots still come from the target
environment; custom CA selection changes the HTTP TLS backend to rustls.
No global trust/proxy configuration is installed by this package.

manifest.json identifies the binary, locked dependency graph, exact upstream
source provenance, the one recorded WS explicit-route adapter, capabilities,
lifecycle and build evidence. Validate all hashes before use. No signature is
claimed. No production accounts, inference payloads or secrets are included.
'''
    payload["README.md"] = readme.encode()
    for name, data in payload.items():
        (output / name).write_bytes(data)
    manifest = {
        "schema": "codex-native-release-v1", "run_id": run_id, "profile": "release",
        "cli_source": ready["cli_source"], "platform": "linux-x86_64-musl", "binary": "codex-native-transport",
        "binary_sha256": binary_hash, "binary_bytes": binary.stat().st_size,
        "source_sha256": meta["source_sha256"], "source_archive_sha256": meta["archive_sha256"],
        "upstream": provenance["upstream"], "upstream_tag": provenance["tag"],
        "upstream_lock_sha256": provenance["upstream_lock_sha256"],
        "upstream_workspace_sha256": provenance["upstream_workspace_sha256"],
        "lock_sha256": runner.digest(payload["Cargo.lock"]), "provenance_sha256": runner.digest(payload["UPSTREAM.json"]),
        "adapter_patch_sha256": runner.digest(payload["WEBSOCKET-ADAPTER.patch"]),
        "websocket_forks": {"tokio-tungstenite": "0e5b2d73aa18dd9f0a50ee9ff199d5aef7594186", "tungstenite": "4fffad30fe373adbdcffab9545e9e9bf4f2fc19f"},
        "capabilities": ready["capabilities"], "http_client_lifecycle_default": ready["http_client_lifecycle"],
        "max_body": ready["max_body"], "max_active": ready["max_active"],
        "linkage": {key: elf[key] for key in ("statically_linked_observed", "interpreter_present", "needed_libraries", "elf_machine_x86_64")},
        "http_uds_release_binary_tests": tests, "websocket_release_profile_tests": 4,
        "request_compression_release_profile_tests": compression_tests,
        "production_deployed": False, "target_centos_runtime_verified": False,
        "binary_only_rootfs_smoke_passed": bool(smoke),
        "image_id": meta["image_id"], "official_base_image_digests": meta["repo_digests"],
        "files": {name: {"sha256": runner.digest(data), "bytes": len(data)} for name, data in payload.items()},
    }
    runner.save(output / "manifest.json", manifest)
    package_path = Path(str(output) + ".tar.gz")
    with package_path.open("wb") as raw, gzip.GzipFile(fileobj=raw, mode="wb", mtime=0, filename="") as compressed:
        with tarfile.open(fileobj=compressed, mode="w") as tar:
            for path in sorted(output.iterdir()):
                data = path.read_bytes()
                entry = tarfile.TarInfo(path.name)
                entry.size, entry.mode, entry.mtime = len(data), 0o755 if path.name == "codex-native-transport" else 0o644, 0
                tar.addfile(entry, io.BytesIO(data))
    summary = {"package": str(package_path), "package_sha256": runner.digest(package_path.read_bytes()),
               "binary_sha256": binary_hash, "binary_bytes": binary.stat().st_size,
               "manifest_sha256": runner.digest((output / "manifest.json").read_bytes()), "statically_linked_observed": static}
    runner.save(output / "package-result.json", summary)
    return summary


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--run", required=True)
    args = parser.parse_args()
    print(json.dumps(package(args.run)), flush=True)


if __name__ == "__main__":
    main()
