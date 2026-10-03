"""Freeze explicit Go changes over a verified gateway or management snapshot."""
from __future__ import annotations

from project_layout import valid_backend_source_path, server_package

import argparse
from datetime import datetime, timezone
import hashlib
import json
from pathlib import Path
import secrets

from gateway_source_race import ROOT, go_manifest, selected_tests


def digest(raw):
    return hashlib.sha256(raw).hexdigest()


def baseline_type(source: Path, root: Path = ROOT):
    source = source.resolve(strict=True)
    roots = {"gateway": root / ".local/source-20260918", "management": root / ".local/management-parity"}
    if source.name == "source" and source.is_dir():
        for kind, parent in roots.items():
            if source.is_relative_to(parent.resolve()):
                return kind
    raise RuntimeError("Baseline must be a frozen source under an allowed local snapshot root")


def verified_base(source: Path, expected: str, kind: str = "gateway"):
    def read(name, evidence_type):
        path = source.parent / name
        if path.is_symlink():
            raise RuntimeError("Baseline verification cannot contain symlinks")
        raw = path.read_bytes()
        value = json.loads(raw.decode("utf-8"))
        if not isinstance(value, dict):
            raise RuntimeError("Baseline verification must be a JSON object")
        return value, {"path": str(path.resolve()), "sha256": digest(raw), "type": evidence_type}

    manifest, manifest_evidence = read("source-manifest.json", "go-source-manifest")
    actual = go_manifest(source)
    if (actual["sha256"] != expected or manifest["sha256"] != expected
            or actual["files"] != manifest["files"]):
        raise RuntimeError("Baseline source or manifest differs")
    artifacts = [manifest_evidence]
    if kind == "gateway":
        build, evidence = read("build-verification.json", "gateway-build-verification")
        if build.get("status") != "pass" or build.get("go_source_sha256") != expected:
            raise RuntimeError("Gateway baseline verification differs")
        artifacts.append(evidence)
    elif kind == "management":
        race, race_evidence = read("report.json", "management-linux-race")
        selected = race.get("selected_top_level_tests")
        if (race.get("schema") != "codex-management-local-race-v1" or race.get("status") != "pass"
                or race.get("go_source_sha256") != expected or type(race.get("exit_code")) is not int
                or race["exit_code"] != 0 or race.get("oom_killed") is not False
                or race.get("go_source_changed_after_freeze") is not False
                or any(race.get(name) != [] for name in ("failed_tests", "skipped_tests", "missing_tests"))
                or not isinstance(selected, list) or not selected
                or any(not isinstance(name, str) or not name.startswith("Test") or "/" in name for name in selected)
                or len(set(selected)) != len(selected) or type(race.get("passed_top_level_tests")) is not int
                or race["passed_top_level_tests"] != len(selected)
                or not isinstance(race.get("passed_packages"), list) or not {"codex-agent", "codex-agent/internal/server"}.intersection(race["passed_packages"])):
            raise RuntimeError("Management baseline race verification differs")
        local, local_evidence = read("local-verification.json", "management-local-build-verification")
        checks = local.get("checks")
        if (local.get("schema") != "codex-agent-frozen-local-verification-v1"
                or local.get("manifest_sha256") != expected or local.get("source_after_sha256") != expected
                or local.get("source_unchanged") is not True or local.get("passed") is not True
                or local.get("extra_source_files") != [] or not isinstance(checks, list)
                or any(not isinstance(check, dict) or not isinstance(check.get("label"), str)
                       or type(check.get("exit_code")) is not int or check["exit_code"] != 0
                       or check.get("failed") != [] for check in checks)):
            raise RuntimeError("Management baseline local verification differs")
        labels = [check["label"] for check in checks]
        if len(labels) != len(set(labels)) or not {"test", "vet", "windows-build", "linux-build"} <= set(labels):
            raise RuntimeError("Management baseline omitted required local checks")
        artifacts.extend((race_evidence, local_evidence))
    else:
        raise RuntimeError("Unknown baseline verification type")
    return manifest, {"type": kind, "files": artifacts}


def overlay_names(names):
    if not names or len(names) != len(set(names)):
        raise ValueError("An explicit unique overlay list is required")
    for name in names:
        if not valid_backend_source_path(name) or not name.endswith(".go"):
            raise ValueError("Overlay entries must be explicit relative Go source paths")
    return sorted(names)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--base", type=Path, required=True)
    parser.add_argument("--expected-go-hash", required=True)
    parser.add_argument("--overlay-file", action="append", required=True)
    args = parser.parse_args()
    source = args.base.resolve(strict=True)
    kind = baseline_type(source, ROOT)
    manifest, verification = verified_base(source, args.expected_go_hash, kind)
    if server_package(source) != server_package(ROOT):
        raise RuntimeError("Source layout changed; freeze the complete current tree instead of mixing an old package-main baseline with internal/server")
    names = overlay_names(args.overlay_file)
    overlay = {}
    for name in names:
        path = ROOT / name
        if path.is_symlink():
            raise RuntimeError("Overlay cannot contain symlinks")
        overlay[name] = path.read_bytes()
    hashes = {name: digest(raw) for name, raw in overlay.items()}
    stamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%S").lower() + "-" + secrets.token_hex(3)
    output = ROOT / ".local/source-20260918" / ("gateway-delta-" + stamp)
    frozen = output / "source"
    frozen.mkdir(parents=True, exist_ok=False)
    for name, expected in manifest["files"].items():
        raw = (source / name).read_bytes()
        if digest(raw) != expected:
            raise RuntimeError("Baseline changed while copying")
        target = frozen / name
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_bytes(raw)
    for name, raw in overlay.items():
        (frozen / name).parent.mkdir(parents=True, exist_ok=True)
        (frozen / name).write_bytes(raw)
    _, after_verification = verified_base(source, args.expected_go_hash, kind)
    if after_verification != verification:
        raise RuntimeError("Baseline verification changed while freezing")
    for name, expected in hashes.items():
        if digest((ROOT / name).read_bytes()) != expected:
            raise RuntimeError("Explicit overlay changed while freezing")
    current = go_manifest(frozen)
    selected = selected_tests(current, frozen)
    production = lambda files: {name: value for name, value in files.items() if not name.endswith("_test.go")}
    provenance = {"schema": "codex-agent-gateway-explicit-delta-v1", "source_directory": str(frozen),
                  "base_source_directory": str(source), "base_go_sha256": manifest["sha256"],
                  "base_verification": verification,
                  "base_manifest_sha256": verification["files"][0]["sha256"],
                  "overlay_files": hashes, "go_source_sha256": current["sha256"],
                  "selected_tests": len(selected), "shared_worktree_mutated": False,
                  "production_files_identical_to_base": production(current["files"]) == production(manifest["files"]),
                  "scope": "only explicit overlay files; concurrent shared-tree changes are not included"}
    if kind == "gateway":
        provenance["base_build_sha256"] = verification["files"][1]["sha256"]
    (output / "overlay-manifest.json").write_text(json.dumps(provenance, indent=2) + "\n", encoding="utf-8")
    (output / "source-manifest.json").write_text(json.dumps(current, indent=2) + "\n", encoding="utf-8")
    print(json.dumps({key: provenance[key] for key in ("source_directory", "base_go_sha256", "go_source_sha256",
                                                       "selected_tests", "production_files_identical_to_base")}))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
