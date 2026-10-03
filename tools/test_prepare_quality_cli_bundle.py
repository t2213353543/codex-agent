"""Offline archive-integrity and extraction boundaries; no executable is run."""
import base64
import hashlib
import io
import json
from pathlib import Path
import tarfile
import tempfile
import unittest
from unittest.mock import patch

import prepare_quality_cli_bundle as bundle


class BundleTests(unittest.TestCase):
    def archive(self, root, extra=(), manifest=None, omit_host=False):
        path = root / "test.tgz"
        prefix = "/".join(bundle.PREFIX)
        data = manifest or {"version": bundle.VERSION, "layoutVersion": 1,
                            "target": bundle.TARGET, "entrypoint": "bin/codex"}
        entries = [(prefix + "/codex-package.json", json.dumps(data).encode(), 0o644),
                   (prefix + "/bin/codex", b"not-a-real-binary", 0o755)]
        if not omit_host:
            entries.append((prefix + "/bin/codex-code-mode-host", b"not-a-real-host", 0o755))
        with tarfile.open(path, "w:gz") as archive:
            for name, payload, mode in entries:
                info = tarfile.TarInfo(name)
                info.size = len(payload)
                info.mode = mode
                archive.addfile(info, io.BytesIO(payload))
            for info in extra:
                archive.addfile(info, io.BytesIO(b""))
        return path

    def test_digest_rejects_nonofficial_bytes(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "private-looking.tar"
            path.write_bytes(b"modified artifact")
            with self.assertRaisesRegex(bundle.InstallFailure, "integrity_mismatch"):
                bundle.archive_digest(path)
            with patch.object(bundle, "SHA512", base64.b64encode(hashlib.sha512(path.read_bytes()).digest()).decode()):
                self.assertEqual(bundle.archive_digest(path), len(b"modified artifact"))

    def test_extracts_complete_host_layout_and_hashes(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            archive = self.archive(root)
            report = bundle.extract_bundle(archive, root / "out")
            self.assertEqual(report["extracted_regular_files"], 3)
            self.assertTrue((root / "out" / "bin" / "codex-code-mode-host").is_file())
            self.assertEqual(report["asset_sha256"]["bin/codex"], hashlib.sha256(b"not-a-real-binary").hexdigest())

    def test_rejects_traversal_absolute_links_special_files_and_duplicates(self):
        bad = []
        for name in ("package/../../outside", "/absolute", "package\\..\\outside", "/".join(bundle.PREFIX) + "/bin/codex"):
            bad.append(tarfile.TarInfo(name))
        for kind in (tarfile.SYMTYPE, tarfile.LNKTYPE, tarfile.CHRTYPE, tarfile.FIFOTYPE):
            info = tarfile.TarInfo("package/link")
            info.type = kind
            info.linkname = "../../outside"
            bad.append(info)
        for info in bad:
            with self.subTest(name=info.name, kind=info.type), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                archive = self.archive(root, [info])
                with self.assertRaises(bundle.InstallFailure):
                    bundle.extract_bundle(archive, root / "out")
                self.assertFalse((root / "out").exists(), "all index validation must precede writes")

    def test_missing_host_and_wrong_manifest_fail_closed(self):
        for options in ({"omit_host": True}, {"manifest": {"version": "0.154.0"}}):
            with self.subTest(options=options), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                with self.assertRaises(bundle.InstallFailure):
                    bundle.extract_bundle(self.archive(root, **options), root / "out")

    def test_existing_directory_cannot_be_overwritten(self):
        with tempfile.TemporaryDirectory() as directory:
            with patch.object(bundle.sys, "platform", "linux"), patch.object(bundle.platform, "machine", return_value="x86_64"), \
                    patch.object(bundle, "download", side_effect=AssertionError("must not download")):
                with self.assertRaisesRegex(bundle.InstallFailure, "already_exists"):
                    bundle.execute(["--output-dir", directory])

    def test_download_redirect_never_follows_to_another_location(self):
        with self.assertRaisesRegex(bundle.InstallFailure, "unexpected_artifact_redirect"):
            bundle.NoRedirect().redirect_request(None, None, 302, "redirect", {}, "https://other.test")


if __name__ == "__main__":
    unittest.main()
