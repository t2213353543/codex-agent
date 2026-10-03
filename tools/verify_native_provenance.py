"""Independently compare the helper to a local pinned official Codex snapshot.

No build, network, source mutations or executable invocation. Unlike check_lock,
this checks UPSTREAM.json and the allowlist against the original source itself.
"""
from __future__ import annotations

import argparse
import difflib
import hashlib
import json
from pathlib import Path
import sys
import tomllib

ROOT = Path(__file__).resolve().parents[1]
CRATES = {
    "codex-http-client": "http-client",
    "codex-utils-rustls-provider": "utils/rustls-provider",
    "codex-websocket-client": "websocket-client",
}


def require(condition: bool, message: str) -> None:
    if not condition:
        raise ValueError(message)


def sha(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def toml(path: Path) -> dict:
    return tomllib.loads(path.read_text(encoding="utf-8"))


def package_identity(item: dict) -> tuple:
    return tuple(item.get(key) for key in ("name", "version", "source", "checksum"))


def verify(source: Path, helper: Path, features: Path | None = None,
           build_manifest: Path | None = None, cli_bin: Path | None = None,
           helper_bin: Path | None = None) -> dict:
    upstream = json.loads((helper / "UPSTREAM.json").read_text(encoding="utf-8"))
    official_workspace = toml(source / "codex-rs/Cargo.toml")
    helper_workspace = toml(helper / "Cargo.toml")
    version = official_workspace["workspace"]["package"]["version"]
    require(version == "0.155.0" and upstream["tag"] == "rust-v" + version,
            "Source snapshot and declared tag must be 0.155.0")
    for name, field in [("Cargo.toml", "upstream_workspace_sha256"),
                        ("Cargo.lock", "upstream_lock_sha256")]:
        require(sha(source / "codex-rs" / name) == upstream[field],
                "Declared upstream hash disagrees with official " + name)
    for name in ("LICENSE", "NOTICE"):
        require((helper / name).read_bytes() == (source / name).read_bytes(),
                "Upstream legal notice changed: " + name)

    official_packages = toml(source / "codex-rs/Cargo.lock")["package"]
    official_external = {package_identity(p) for p in official_packages if p.get("source")}
    allowed = json.loads((helper / "LOCKED-PACKAGES.json").read_text(encoding="utf-8"))
    require({package_identity(p) for p in allowed} == official_external,
            "Package allowlist is not exactly the official external dependency set")
    packages = toml(helper / "Cargo.lock")["package"]
    external = [p for p in packages if p.get("source")]
    require(all(package_identity(p) in official_external for p in external),
            "Resolved helper has an external dependency absent from official lock")
    require({(p["name"], p["version"]) for p in packages if not p.get("source")} ==
            {("codex-native-transport", "0.1.0"), *((name, version) for name in CRATES)},
            "Unexpected local Cargo package")
    sources = upstream["sources"]
    tracked = {p.relative_to(helper).as_posix() for p in (helper / "vendor").rglob("*.rs")}
    require(tracked == set(sources), "Untracked or missing vendored Rust source")
    unchanged, patched = 0, 0
    for relative, expected in sources.items():
        original, vendored = source / expected["upstream"], helper / relative
        require(sha(original) == expected["sha256"], "Official source hash mismatch: " + relative)
        require(sha(vendored) == expected.get("patched_sha256", expected["sha256"]),
                "Vendored source hash mismatch: " + relative)
        if "patched_sha256" not in expected:
            unchanged += 1
            continue
        require(relative == "vendor/codex-websocket-client/src/lib.rs",
                "Unexpected modified upstream Rust source")
        before, after = original.read_text(encoding="utf-8"), vendored.read_text(encoding="utf-8")
        adapter = (helper / "tools/websocket_adapter.rsinc").read_text(encoding="utf-8")
        anchor = "    async fn connect_with_route("
        require(before.count(anchor) == 1 and after == before.replace(anchor, adapter + "\n" + anchor, 1),
                "Recorded WS adapter does not reconstruct vendored source")
        expected_patch = "".join(difflib.unified_diff(before.splitlines(True), after.splitlines(True),
                                  fromfile="upstream/websocket-client/src/lib.rs",
                                  tofile="vendor/codex-websocket-client/src/lib.rs"))
        require((helper / "WEBSOCKET-ADAPTER.patch").read_text(encoding="utf-8") == expected_patch,
                "Adapter patch does not describe the complete upstream difference")
        patched += 1
    require(patched == 1, "Expected exactly one explicit-route adapter")
    for local, original in CRATES.items():
        upstream_crate = toml(source / "codex-rs" / original / "Cargo.toml")
        vendored_crate = toml(helper / "vendor" / local / "Cargo.toml")
        upstream_crate.pop("dev-dependencies", None)
        upstream_crate["package"].update(autobins=False, autotests=False, autobenches=False)
        require(upstream_crate == vendored_crate, "Unexpected production crate manifest delta: " + local)
        require({p.relative_to(source / "codex-rs" / original / "src").as_posix()
                 for p in (source / "codex-rs" / original / "src").rglob("*.rs")} ==
                {p.relative_to(helper / "vendor" / local / "src").as_posix()
                 for p in (helper / "vendor" / local / "src").rglob("*.rs")},
                "Official crate contains omitted Rust files: " + local)

    pins = {name: next(p["version"] for p in external if p["name"] == name)
            for name in ("reqwest", "native-tls", "rustls", "hyper", "h2", "openssl-sys",
                         "openssl-src", "rustls-native-certs", "tokio-tungstenite", "tungstenite")}
    require(all(name in official_workspace["patch"] and
                all(key in official_workspace["patch"][name] and
                    value == official_workspace["patch"][name][key]
                    for key, value in entries.items())
                for name, entries in helper_workspace["patch"].items()),
            "Transport fork revision override differs from official workspace")
    feature_checks = {}
    if features:
        tree = features.read_text(encoding="utf-8")
        required = {"rustls": ["aws_lc_rs", "std", "prefer-post-quantum"],
                    "reqwest": ["default", "cookies", "json", "stream", "rustls-tls-native-roots"],
                    "tokio-tungstenite": ["proxy", "rustls-tls-native-roots"],
                    "tungstenite": ["deflate", "proxy"], "openssl-sys": ["vendored"]}
        for crate, names in required.items():
            for name in names:
                marker = f'{crate} feature "{name}"'
                feature_checks[marker] = marker in tree
        for name in ("socks", "gzip", "native-tls-alpn"):
            marker = f'reqwest feature "{name}"'
            feature_checks["absent " + marker] = marker not in tree
        require(all(feature_checks.values()), "Resolved transport feature tree disagrees with audited union")
    source_manifest_matches = None
    if build_manifest:
        frozen = json.loads(build_manifest.read_text(encoding="utf-8"))
        relevant = {name for name in frozen if name.endswith((".rs", ".rsinc")) or
                    name.endswith("Cargo.toml") or name in
                    ("Cargo.lock", "UPSTREAM.json", "WEBSOCKET-ADAPTER.patch", "rust-toolchain.toml")}
        source_manifest_matches = all(sha(helper / name) == frozen[name] for name in relevant)
        require(source_manifest_matches, "Current build inputs differ from captured build source manifest")
    return {"schema": "native-source-provenance-audit-v1", "passed": True,
            "upstream_tag": upstream["tag"], "official_lock_sha256": upstream["upstream_lock_sha256"],
            "official_workspace_sha256": upstream["upstream_workspace_sha256"],
            "packages": len(packages), "external_packages": len(external),
            "unchanged_official_rust_sources": unchanged, "recorded_adapter_sources": patched,
            "original_dependency_allowlist_exact": True, "all_official_crate_rust_files_accounted_for": True,
            "vendor_manifests_only_remove_dev_deps_and_automatic_targets": True,
            "adapter_exactly_reconstructed": True, "transport_pins": pins,
            "resolved_feature_checks": feature_checks, "build_source_manifest_matches": source_manifest_matches,
            "cli_sha256": sha(cli_bin) if cli_bin else None,
            "helper_sha256": sha(helper_bin) if helper_bin else None,
            "limits": ["Matching source/dependency hashes does not establish TLS wire or behavior parity.",
                       "Feature capture is supplied build evidence, not a new Cargo resolution or build.",
                       "CLI and helper binary hashes identify selected files; this does not establish reproducible builds."]}


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--source", type=Path, default=ROOT / ".local/codex-source-0.155.0")
    parser.add_argument("--helper-source", type=Path, default=ROOT / "native-transport")
    parser.add_argument("--features", type=Path)
    parser.add_argument("--build-manifest", type=Path)
    parser.add_argument("--cli-bin", type=Path)
    parser.add_argument("--helper-bin", type=Path)
    parser.add_argument("--output", type=Path)
    args = parser.parse_args()
    try:
        report = verify(args.source, args.helper_source, args.features, args.build_manifest,
                        args.cli_bin, args.helper_bin)
    except (ValueError, KeyError, OSError, StopIteration) as error:
        print(json.dumps({"passed": False, "error": str(error)}, ensure_ascii=False))
        return 1
    encoded = json.dumps(report, indent=2, ensure_ascii=False) + "\n"
    if args.output:
        args.output.write_text(encoded, encoding="utf-8")
    print(encoded, end="")
    return 0


if __name__ == "__main__":
    sys.exit(main())
