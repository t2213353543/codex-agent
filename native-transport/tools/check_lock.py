"""Reject dependency drift and modified upstream Rust code after Cargo resolves.

Run from any directory. No network and no secret input/output.
"""
from __future__ import annotations

import argparse
import hashlib
import json
from pathlib import Path
import tomllib


ROOT = Path(__file__).resolve().parents[1]


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--lock", type=Path, default=ROOT / "Cargo.lock")
    args = parser.parse_args()
    allowed = json.loads((ROOT / "LOCKED-PACKAGES.json").read_text(encoding="utf-8"))
    allowed = {(p["name"], p["version"], p.get("source")): p.get("checksum") for p in allowed}
    packages = tomllib.loads(args.lock.read_text(encoding="utf-8"))["package"]
    local = {"codex-native-transport": "0.1.0", "codex-http-client": "0.155.0", "codex-utils-rustls-provider": "0.155.0", "codex-websocket-client": "0.155.0"}
    for package in packages:
        key = (package["name"], package["version"], package.get("source"))
        if not package.get("source"):
            assert local.get(package["name"]) == package["version"], f"Unexpected local package: {key[:2]}"
        else:
            assert key in allowed and package.get("checksum") == allowed[key], f"Dependency drift: {key[:2]}"
    assert set(local) <= {p["name"] for p in packages}, "Cargo lock still missing the helper package"
    for name, version in {"reqwest": "0.12.28", "native-tls": "0.2.14", "rustls": "0.23.36", "hyper": "1.8.1", "h2": "0.4.16", "openssl-sys": "0.9.111", "openssl-src": "300.6.1+3.6.3"}.items():
        assert [p["version"] for p in packages if p["name"] == name] == [version], f"Wrong transport pin: {name}"
    provenance = json.loads((ROOT / "UPSTREAM.json").read_text(encoding="utf-8"))
    for relative, expected in provenance["sources"].items():
        assert hashlib.sha256((ROOT / relative).read_bytes()).hexdigest() == expected.get("patched_sha256", expected["sha256"]), f"Modified upstream source: {relative}"
    manifest = tomllib.loads((ROOT / "Cargo.toml").read_text(encoding="utf-8"))
    features = set(manifest["workspace"]["dependencies"]["rustls"]["features"])
    assert {"aws_lc_rs", "std", "prefer-post-quantum"} <= features, "Missing verified CLI TLS feature union"
    forks = {"tokio-tungstenite": "0e5b2d73aa18dd9f0a50ee9ff199d5aef7594186", "tungstenite": "4fffad30fe373adbdcffab9545e9e9bf4f2fc19f"}
    for name, revision in forks.items():
        actual = [package for package in packages if package["name"] == name]
        assert len(actual) == 1 and actual[0]["source"].endswith("#" + revision), f"Wrong WebSocket fork: {name}"
    changed = sum("patched_sha256" in item for item in provenance["sources"].values())
    print(json.dumps({"dependency_versions_match_official_lock": True, "packages": len(packages), "unchanged_upstream_rust_sources": len(provenance["sources"]) - changed, "recorded_adapter_sources": changed, "verified_tls_features_present": True, "websocket_forks_match": True}))


if __name__ == "__main__":
    main()
