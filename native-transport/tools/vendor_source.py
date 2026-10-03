"""Copy the pinned transport crates; record every unchanged Rust source digest.

This maintainer tool never downloads code. It expects the already verified CLI
snapshot. Cargo manifests are adapted to omit upstream test-only dependencies.
"""
from __future__ import annotations

import hashlib
import difflib
import json
from pathlib import Path
import re
import shutil
import tomllib

ROOT = Path(__file__).resolve().parents[1]
SOURCE = ROOT.parent / ".local/codex-source-0.155.0"
RUST = SOURCE / "codex-rs"


def sha(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def main():
    workspace = tomllib.loads((RUST / "Cargo.toml").read_text())
    assert workspace["workspace"]["package"]["version"] == "0.155.0"
    sources = {}
    crates = {"http-client": "codex-http-client", "utils/rustls-provider": "codex-utils-rustls-provider", "websocket-client": "codex-websocket-client"}
    for original, destination in crates.items():
        upstream = RUST / original
        target = ROOT / "vendor" / destination
        for path in (upstream / "src").rglob("*.rs"):
            relative = path.relative_to(upstream)
            out = target / relative
            out.parent.mkdir(parents=True, exist_ok=True)
            shutil.copyfile(path, out)
            sources[out.relative_to(ROOT).as_posix()] = {
                "upstream": (Path("codex-rs") / original / relative).as_posix(),
                "sha256": sha(path),
            }
            if original == "websocket-client" and relative.as_posix() == "src/lib.rs":
                before = path.read_text(encoding="utf-8")
                anchor = "    async fn connect_with_route("
                assert before.count(anchor) == 1
                adapter = (ROOT / "tools/websocket_adapter.rsinc").read_text(encoding="utf-8")
                after = before.replace(anchor, adapter + "\n" + anchor, 1)
                out.write_text(after, encoding="utf-8", newline="\n")
                sources[out.relative_to(ROOT).as_posix()]["patched_sha256"] = sha(out)
                (ROOT / "WEBSOCKET-ADAPTER.patch").write_text("".join(difflib.unified_diff(before.splitlines(True), after.splitlines(True), fromfile="upstream/websocket-client/src/lib.rs", tofile="vendor/codex-websocket-client/src/lib.rs")), encoding="utf-8", newline="\n")
        manifest = (upstream / "Cargo.toml").read_text()
        manifest = re.sub(r"\[dev-dependencies\]\n.*?(?=\n\[|\Z)", "", manifest, flags=re.S)
        # These manifests contain no production targets outside src/lib.rs.
        manifest = manifest.replace("[package]\n", "[package]\nautobins = false\nautotests = false\nautobenches = false\n", 1)
        (target / "Cargo.toml").write_text(manifest, encoding="utf-8")
    for name in ("LICENSE", "NOTICE"):
        shutil.copyfile(SOURCE / name, ROOT / name)
    provenance = {
        "upstream": "https://github.com/openai/codex/tree/rust-v0.155.0",
        "tag": "rust-v0.155.0",
        "upstream_lock_sha256": sha(RUST / "Cargo.lock"),
        "upstream_workspace_sha256": sha(RUST / "Cargo.toml"),
        "source_modifications": "websocket-client/src/lib.rs has the explicit immutable-route adapter recorded in WEBSOCKET-ADAPTER.patch; all other Rust sources are byte-identical. Vendor Cargo manifests omit dev dependencies and auto target discovery; workspace dependencies retain official locked versions.",
        "sources": sources,
    }
    (ROOT / "UPSTREAM.json").write_text(json.dumps(provenance, indent=2) + "\n", encoding="utf-8")
    # Cargo normalizes/prunes this seed on the first isolated build. The checker
    # rejects any registry version/checksum not already in the upstream lock.
    shutil.copyfile(RUST / "Cargo.lock", ROOT / "Cargo.lock")
    locked = tomllib.loads((RUST / "Cargo.lock").read_text())["package"]
    packages = [{key: package[key] for key in ("name", "version", "source", "checksum") if key in package}
                for package in locked if package.get("source")]
    (ROOT / "LOCKED-PACKAGES.json").write_text(json.dumps(packages, indent=2) + "\n", encoding="utf-8")
    print(f"Vendored {len(sources)} Rust source files from three transport crates; one explicit-route adapter is separately recorded.")


if __name__ == "__main__":
    main()
