"""Offline lifecycle and provenance tests for the local native build runner."""
from __future__ import annotations

import contextlib
import io
import json
from pathlib import Path
import tempfile
import unittest
from unittest import mock

import native_transport_local as runner


class NativeLocalRunnerTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="native-runner-test-")
        self.folder = Path(self.temporary.name)
        self.source = self.folder / "source"
        self.source.mkdir()
        (self.source / "Cargo.toml").write_text('[package]\nname = "synthetic"\n')
        (self.source / "main.rs").write_text("fn main() {}\n")
        self.source_patch = mock.patch.object(runner, "SOURCE", self.source)
        self.source_patch.start()

    def tearDown(self):
        self.source_patch.stop()
        self.temporary.cleanup()

    def snapshot(self, name):
        folder = self.folder / name
        folder.mkdir()
        return runner.freeze(folder)

    def test_snapshot_is_deterministic_and_excludes_generated_files(self):
        for name in ("target", "__pycache__", ".git"):
            (self.source / name).mkdir()
            (self.source / name / "generated").write_text("not-source")
        first = self.snapshot("one")
        second = self.snapshot("two")
        self.assertEqual(first, second)
        self.assertEqual(set(first["files"]), {"Cargo.toml", "main.rs"})

    def test_snapshot_rejects_concurrent_source_changes(self):
        original = runner.source_files
        calls = 0

        def changing_source():
            nonlocal calls
            calls += 1
            if calls == 2:
                (self.source / "main.rs").write_text("fn changed() {}\n")
            return original()

        with mock.patch.object(runner, "source_files", side_effect=changing_source):
            with self.assertRaisesRegex(RuntimeError, "sources_changed_during_freeze"):
                self.snapshot("changing")

    def test_run_handle_cannot_escape_output_root(self):
        for handle in ("../outside", "/tmp/outside", "20260918T000000Z-abcdefghi"):
            with self.assertRaisesRegex(RuntimeError, "invalid_run_handle"):
                runner.metadata(handle)

    def test_inspect_rejects_wrong_container_label(self):
        meta = dict(container="a" * 64, run_id="20260918T000000Z-12345678",
                    docker_context="fixture", source_sha256="b" * 64, image_id="sha256:" + "c" * 64)
        response = [{"Id": meta["container"], "Image": meta["image_id"],
                     "Config": {"Labels": {runner.LABEL: "someone-else"}}}]
        with mock.patch.object(runner, "docker", return_value=mock.Mock(stdout=b"fixture\n")), mock.patch.object(runner, "docker_json", return_value=response):
            with self.assertRaisesRegex(RuntimeError, "container_identity_mismatch"):
                runner.inspect(meta)

    def test_resume_rejects_running_successful_and_unstarted_containers(self):
        meta = dict(run_id="20260918T000000Z-12345678", container="a" * 64)
        cases = [dict(Running=True, Status="running", ExitCode=0),
                 dict(Running=False, Status="exited", ExitCode=0),
                 dict(Running=False, Status="created", ExitCode=0)]
        for state in cases:
            with mock.patch.object(runner, "metadata", return_value=(self.folder, meta)), mock.patch.object(runner, "inspect", return_value={"State": state}), mock.patch.object(runner, "docker") as docker:
                with self.assertRaises(RuntimeError):
                    runner.resume(None)
                docker.assert_not_called()

    def test_resume_terminal_failure_uses_same_container_once(self):
        meta = dict(run_id="20260918T000000Z-12345678", container="a" * 64)
        state = dict(Running=False, Status="exited", ExitCode=101, OOMKilled=False)
        with mock.patch.object(runner, "metadata", return_value=(self.folder, meta)), mock.patch.object(runner, "inspect", return_value={"State": state}), mock.patch.object(runner, "build_state", return_value={"status": "failed", "phase": "fetch"}), mock.patch.object(runner, "docker") as docker, contextlib.redirect_stdout(io.StringIO()):
            runner.resume(None)
            docker.assert_called_once_with("start", meta["container"])
        recorded = json.loads((self.folder / "run.json").read_text())
        self.assertEqual(recorded["resumes"][0]["prior_phase"], "fetch")

    def test_collect_does_not_copy_running_artifacts(self):
        with mock.patch.object(runner, "metadata", return_value=(self.folder, {})), mock.patch.object(runner, "inspect", return_value={"State": {"Running": True}}), mock.patch.object(runner, "docker") as docker:
            with self.assertRaisesRegex(RuntimeError, "collect_requires_terminal"):
                runner.collect(None)
            docker.assert_not_called()

    def test_container_driver_is_valid_python_and_checks_before_build(self):
        compile(runner.BUILD_DRIVER, "<container-driver>", "exec")
        self.assertLess(runner.BUILD_DRIVER.index('execute("lock_check"'), runner.BUILD_DRIVER.index('execute("build"'))
        self.assertIn('"--locked"', runner.BUILD_DRIVER)
        self.assertLess(runner.BUILD_DRIVER.index('execute("clean_helper"'), runner.BUILD_DRIVER.index('execute("build"'))
        self.assertIn("os.utime(WORK / name, None)", runner.BUILD_DRIVER)

    def test_profile_validation_precedes_docker_and_release_tests_use_same_profile(self):
        with mock.patch.object(runner, "docker") as docker:
            with self.assertRaisesRegex(RuntimeError, "invalid_build_profile"):
                runner.start(profile="untrusted-profile")
            docker.assert_not_called()
        self.assertIn('PROFILE_ARGS = ["--release"] if PROFILE == "release" else []', runner.BUILD_DRIVER)
        self.assertIn('["cargo", "build", "--locked", *PROFILE_ARGS]', runner.BUILD_DRIVER)
        self.assertIn('["cargo", "test", "--locked", *PROFILE_ARGS', runner.BUILD_DRIVER)
        self.assertIn('str(BINARY), "--report"', runner.BUILD_DRIVER)
        self.assertIn('"interpreter_present"', runner.BUILD_DRIVER)
        self.assertIn('"(NEEDED)"', runner.BUILD_DRIVER)


if __name__ == "__main__":
    unittest.main()
