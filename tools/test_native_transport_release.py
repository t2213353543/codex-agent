"""Release packager refuses incomplete/profile-mismatched evidence."""
import io
import json
from pathlib import Path
import tarfile
import tempfile
import unittest
from unittest import mock

import native_transport_release as release


class ReleaseEvidenceTests(unittest.TestCase):
    def test_debug_or_incomplete_result_never_creates_package(self):
        with tempfile.TemporaryDirectory() as temporary:
            folder = Path(temporary)
            baseline = {"profile": "release", "state": {"Running": False, "ExitCode": 0},
                        "build": {"status": "complete", "completed": ["lock_check", "clean_helper", "build", "websocket_tests", "loopback"]}}
            invalid = [dict(baseline, profile="debug"), dict(baseline, state={"Running": True, "ExitCode": 0}),
                       dict(baseline, state={"Running": False, "ExitCode": 1}),
                       dict(baseline, build={"status": "complete", "completed": ["build"]})]
            for result in invalid:
                (folder / "result.json").write_text(json.dumps(result))
                with mock.patch.object(release.runner, "metadata", return_value=(folder, {})):
                    with self.assertRaisesRegex(RuntimeError, "release_validation_incomplete"):
                        release.package("synthetic")

    def test_frozen_file_refuses_nonregular_member(self):
        with tempfile.TemporaryDirectory() as temporary:
            archive = Path(temporary) / "source.tar.gz"
            with tarfile.open(archive, "w:gz") as tar:
                member = tarfile.TarInfo("UPSTREAM.json")
                member.type = tarfile.SYMTYPE
                member.linkname = "outside"
                tar.addfile(member)
            with self.assertRaisesRegex(RuntimeError, "package_source_not_regular"):
                release.frozen_file(archive, "UPSTREAM.json")

    def test_frozen_file_returns_exact_recorded_bytes(self):
        with tempfile.TemporaryDirectory() as temporary:
            archive = Path(temporary) / "source.tar.gz"
            payload = b'{"tag":"synthetic"}\n'
            with tarfile.open(archive, "w:gz") as tar:
                member = tarfile.TarInfo("UPSTREAM.json")
                member.size = len(payload)
                tar.addfile(member, io.BytesIO(payload))
            self.assertEqual(release.frozen_file(archive, "UPSTREAM.json"), payload)


if __name__ == "__main__":
    unittest.main()
