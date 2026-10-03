"""Freeze verified management sources plus only the gateway owner's current files."""
from __future__ import annotations

from datetime import datetime, timezone
import fnmatch
import hashlib
import json
from pathlib import PurePosixPath
import secrets
import tarfile

from gateway_source_race import ROOT, go_manifest, selected_tests
from project_layout import server_source_dir


BASE = ROOT / ".local/management-parity/local-race-20260918t065425-d36576"
BASE_HASH = "bdfbf92ba3102028440c00f75fb8d728f117fc5f4f3f227782429198b2147c82"
PATTERNS = ("gateway*.go", "proxy.go", "proxy_test.go", "stats.go", "stats_pg.go", "response*.go", "source_native_helper_test.go", "source_native_capture_test.go", "source_native_compression_test.go", "cli_transport_test.go", "codex_cli_workload_test.go", "real_cli_test.go", "real_cli_websocket_test.go", "main.go", "accounts_shutdown_test.go", "accounts.go", "accounts_pg.go")


def digest_files(files: dict) -> str:
    return hashlib.sha256(json.dumps(files, sort_keys=True, separators=(",", ":")).encode()).hexdigest()


def main() -> int:
    manifest = json.loads((BASE / "source-manifest.json").read_text(encoding="utf-8"))
    report = json.loads((BASE / "report.json").read_text(encoding="utf-8"))
    if (manifest["sha256"] != BASE_HASH or digest_files(manifest["files"]) != BASE_HASH
            or report.get("go_source_sha256") != BASE_HASH or report.get("status") != "pass"):
        raise RuntimeError("Management baseline is not the expected verified snapshot")
    archive = BASE / "source.tar.gz"
    if hashlib.sha256(archive.read_bytes()).hexdigest() != manifest["archive_sha256"]:
        raise RuntimeError("Management baseline archive checksum differs")
    baseline = {}
    with tarfile.open(archive, "r:gz") as tar:
        for entry in tar:
            relative = PurePosixPath(entry.name)
            if (not entry.isfile() or relative.is_absolute() or ".." in relative.parts or ":" in entry.name
                    or "\\" in entry.name or entry.name in baseline or entry.name not in manifest["files"]):
                raise RuntimeError("Baseline archive contains an unexpected source path")
            raw = tar.extractfile(entry).read()
            if hashlib.sha256(raw).hexdigest() != manifest["files"][entry.name]:
                raise RuntimeError("Baseline archive source checksum differs")
            baseline[entry.name] = raw
    if set(baseline) != set(manifest["files"]):
        raise RuntimeError("Baseline archive source set differs")
    if "admin_billing_disabled_test.go" not in baseline:
        raise RuntimeError("Management baseline must retain the verified non-billing scope")
    if server_source_dir(ROOT) != ROOT:
        raise RuntimeError("Legacy management overlay cannot mix package-main baseline with internal/server; freeze the complete current source tree")
    overlay = {path.name: path.read_bytes() for path in sorted(ROOT.glob("*.go"))
               if any(fnmatch.fnmatchcase(path.name, pattern) for pattern in PATTERNS)}
    if "source_native_helper_test.go" not in overlay:
        raise RuntimeError("Native helper comparison fixture is missing")
    overlay_hashes = {relative: hashlib.sha256(raw).hexdigest() for relative, raw in overlay.items()}
    stamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%S").lower() + "-" + secrets.token_hex(3)
    output = ROOT / ".local/source-20260918" / ("gateway-overlay-" + stamp)
    source = output / "source"
    source.mkdir(parents=True, exist_ok=False)
    for relative, raw in (baseline | overlay).items():
        path = source / relative
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(raw)
    for relative, expected in overlay_hashes.items():
        if hashlib.sha256((ROOT / relative).read_bytes()).hexdigest() != expected:
            raise RuntimeError("Gateway source changed while freezing overlay")
    frozen = go_manifest(source)
    selected = selected_tests(frozen, source)
    provenance = {"schema": "codex-agent-gateway-overlay-v1", "source_directory": str(source),
                  "base_go_sha256": BASE_HASH, "base_archive_sha256": manifest["archive_sha256"],
                  "base_report_sha256": hashlib.sha256((BASE / "report.json").read_bytes()).hexdigest(),
                  "overlay_patterns": PATTERNS, "overlay_files": overlay_hashes,
                  "overlay_sha256": digest_files(overlay_hashes), "go_source_sha256": frozen["sha256"],
                  "selected_tests": len(selected), "shared_worktree_mutated": False,
                  "management_origin": "verified management baseline, not current worktree",
                  "management_billing_scope": "retired; verified baseline keeps disabled-route tests",
                  "stats_origin": "current gateway-owned RequestRecord observation extension, included in overlay hashes"}
    (output / "overlay-manifest.json").write_text(json.dumps(provenance, indent=2) + "\n", encoding="utf-8")
    (output / "source-manifest.json").write_text(json.dumps(frozen, indent=2) + "\n", encoding="utf-8")
    print(json.dumps({key: provenance[key] for key in ("source_directory", "base_go_sha256", "overlay_sha256",
                                                       "go_source_sha256", "selected_tests")}))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
