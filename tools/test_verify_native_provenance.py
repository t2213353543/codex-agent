"""Mutation checks for the independent source provenance verifier."""
from __future__ import annotations

import hashlib
import json
from pathlib import Path
import shutil
import tempfile
import unittest

from verify_native_provenance import ROOT, verify


class ProvenanceMutationTest(unittest.TestCase):
    def setUp(self):
        self.source = ROOT / ".local/codex-source-0.155.0"
        if not self.source.is_dir():
            self.skipTest("requires local source-pinned official snapshot")
        self.temporary = tempfile.TemporaryDirectory(prefix="codex-provenance-")
        self.addCleanup(self.temporary.cleanup)
        self.helper = Path(self.temporary.name) / "helper"
        shutil.copytree(ROOT / "native-transport", self.helper,
                        ignore=shutil.ignore_patterns("target", "__pycache__", "ws-plan", "tests"))

    def test_baseline_independently_matches_snapshot(self):
        report = verify(self.source, self.helper)
        self.assertEqual((report["packages"], report["external_packages"],
                          report["unchanged_official_rust_sources"], report["recorded_adapter_sources"]),
                         (265, 261, 30, 1))

    def test_self_consistent_changed_source_and_declared_digest_is_rejected(self):
        relative = "vendor/codex-utils-rustls-provider/src/lib.rs"
        original = self.helper / relative
        original.write_bytes(original.read_bytes() + b"\n// unreviewed change\n")
        manifest = self.helper / "UPSTREAM.json"
        provenance = json.loads(manifest.read_text(encoding="utf-8"))
        provenance["sources"][relative]["sha256"] = hashlib.sha256(original.read_bytes()).hexdigest()
        manifest.write_text(json.dumps(provenance), encoding="utf-8")
        with self.assertRaisesRegex(ValueError, "Official source hash mismatch"):
            verify(self.source, self.helper)

    def test_self_consistent_dependency_and_allowlist_change_is_rejected(self):
        allowlist = self.helper / "LOCKED-PACKAGES.json"
        allowed = json.loads(allowlist.read_text(encoding="utf-8"))
        package = next(p for p in allowed if p["name"] == "rustls" and p["version"] == "0.23.36")
        checksum = package["checksum"]
        package["checksum"] = "0" * 64
        allowlist.write_text(json.dumps(allowed), encoding="utf-8")
        lock = self.helper / "Cargo.lock"
        lock.write_text(lock.read_text(encoding="utf-8").replace(checksum, "0" * 64), encoding="utf-8")
        with self.assertRaisesRegex(ValueError, "allowlist is not exactly"):
            verify(self.source, self.helper)

    def test_unrecorded_rust_file_is_rejected(self):
        (self.helper / "vendor/codex-http-client/src/unrecorded.rs").write_text("// stray module\n")
        with self.assertRaisesRegex(ValueError, "Untracked or missing"):
            verify(self.source, self.helper)

    def test_stale_adapter_patch_is_rejected(self):
        (self.helper / "WEBSOCKET-ADAPTER.patch").write_text("stale patch\n")
        with self.assertRaisesRegex(ValueError, "Adapter patch does not describe"):
            verify(self.source, self.helper)

    def test_production_manifest_drift_is_rejected(self):
        path = self.helper / "vendor/codex-websocket-client/Cargo.toml"
        path.write_text(path.read_text(encoding="utf-8").replace('"net", "rt", "time"', '"net", "rt"'), encoding="utf-8")
        with self.assertRaisesRegex(ValueError, "production crate manifest delta"):
            verify(self.source, self.helper)

    def test_resolved_feature_drift_is_rejected(self):
        source = ROOT / ".local/gateway-audit-20260924/native-resolved-features.txt"
        if not source.is_file():
            self.skipTest("requires captured resolved feature tree")
        changed = self.helper / "features.txt"
        changed.write_text(source.read_text(encoding="utf-8").replace('rustls feature "prefer-post-quantum"',
                                                                     'rustls feature "removed"'), encoding="utf-8")
        with self.assertRaisesRegex(ValueError, "feature tree disagrees"):
            verify(self.source, self.helper, features=changed)


if __name__ == "__main__":
    unittest.main()
