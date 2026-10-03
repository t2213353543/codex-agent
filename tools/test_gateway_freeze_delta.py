import copy
import hashlib
import io
import json
from pathlib import Path
import tempfile
import unittest
from contextlib import redirect_stdout
from unittest.mock import patch

import gateway_freeze_delta as delta
from gateway_freeze_delta import baseline_type, go_manifest, overlay_names, verified_base


class ExplicitDeltaTests(unittest.TestCase):
    def make_management(self, parent):
        source = parent / "source"
        source.mkdir(parents=True)
        (source / "main.go").write_text("package main\n", encoding="ascii")
        manifest = go_manifest(source)
        (parent / "source-manifest.json").write_text(json.dumps(manifest), encoding="ascii")
        race = {"schema": "codex-management-local-race-v1", "status": "pass", "go_source_sha256": manifest["sha256"],
                "exit_code": 0, "oom_killed": False, "go_source_changed_after_freeze": False,
                "failed_tests": [], "skipped_tests": [], "missing_tests": [], "selected_top_level_tests": ["TestSynthetic"],
                "passed_top_level_tests": 1, "passed_packages": ["codex-agent"]}
        local = {"schema": "codex-agent-frozen-local-verification-v1", "manifest_sha256": manifest["sha256"],
                 "source_after_sha256": manifest["sha256"], "source_unchanged": True, "passed": True, "extra_source_files": [],
                 "checks": [{"label": label, "exit_code": 0, "failed": []} for label in ("test", "vet", "windows-build", "linux-build")]}
        self.write_management(parent, race, local)
        return source, manifest, race, local

    def write_management(self, parent, race, local):
        (parent / "report.json").write_text(json.dumps(race), encoding="ascii")
        (parent / "local-verification.json").write_text(json.dumps(local), encoding="ascii")

    def test_explicit_overlay_names_reject_globs_paths_duplicates_and_empty(self):
        self.assertEqual(overlay_names(["b_test.go", "a.go"]), ["a.go", "b_test.go"])
        for names in [[], ["a.go", "a.go"], ["../a.go"], ["..\\a.go"], ["C:a.go"], ["*.go"], ["main.py"]]:
            with self.assertRaises(ValueError):
                overlay_names(names)

    def test_verified_baseline_requires_exact_source_and_passing_build(self):
        with tempfile.TemporaryDirectory() as directory:
            parent = Path(directory)
            source = parent / "source"
            source.mkdir()
            (source / "main.go").write_text("package main\n", encoding="ascii")
            manifest = go_manifest(source)
            (parent / "source-manifest.json").write_text(json.dumps(manifest), encoding="ascii")
            verification = parent / "build-verification.json"
            verification.write_text(json.dumps({"status": "pass", "go_source_sha256": manifest["sha256"]}), encoding="ascii")
            verified_base(source, manifest["sha256"])
            (source / "extra.go").write_text("package main\n", encoding="ascii")
            with self.assertRaises(RuntimeError):
                verified_base(source, manifest["sha256"])
            (source / "extra.go").unlink()
            verification.write_text(json.dumps({"status": "fail", "go_source_sha256": manifest["sha256"]}), encoding="ascii")
            with self.assertRaises(RuntimeError):
                verified_base(source, manifest["sha256"])

    def test_management_requires_both_verified_reports_and_records_provenance(self):
        with tempfile.TemporaryDirectory() as directory:
            parent = Path(directory)
            source, manifest, _, _ = self.make_management(parent)
            actual, verification = verified_base(source, manifest["sha256"], "management")
            self.assertEqual(actual, manifest)
            self.assertEqual(verification["type"], "management")
            self.assertEqual([entry["type"] for entry in verification["files"]],
                             ["go-source-manifest", "management-linux-race", "management-local-build-verification"])
            for entry in verification["files"]:
                path = Path(entry["path"])
                self.assertEqual(path.parent, parent.resolve())
                self.assertEqual(entry["sha256"], hashlib.sha256(path.read_bytes()).hexdigest())
            (parent / "local-verification.json").unlink()
            with self.assertRaises(OSError):
                verified_base(source, manifest["sha256"], "management")

    def test_management_rejects_failed_mismatched_or_incomplete_race(self):
        with tempfile.TemporaryDirectory() as directory:
            parent = Path(directory)
            source, manifest, race, local = self.make_management(parent)
            for field, value in [("schema", "wrong"), ("status", "fail"), ("go_source_sha256", "wrong"),
                                 ("exit_code", 1), ("exit_code", False), ("oom_killed", True),
                                 ("go_source_changed_after_freeze", True), ("failed_tests", ["TestFailed"]),
                                 ("skipped_tests", ["TestSkipped"]), ("missing_tests", ["TestMissing"]),
                                 ("selected_top_level_tests", []), ("selected_top_level_tests", ["TestSynthetic"] * 2),
                                 ("selected_top_level_tests", ["TestSynthetic/sub"]), ("passed_top_level_tests", 0), ("passed_top_level_tests", True),
                                 ("passed_packages", [])]:
                with self.subTest(field=field, value=value):
                    altered = dict(race, **{field: value})
                    self.write_management(parent, altered, local)
                    with self.assertRaises(RuntimeError):
                        verified_base(source, manifest["sha256"], "management")

    def test_management_rejects_source_changes_and_forged_manifest(self):
        with tempfile.TemporaryDirectory() as directory:
            parent = Path(directory)
            source, manifest, race, local = self.make_management(parent)
            (source / "extra.go").write_text("package main\n", encoding="ascii")
            with self.assertRaises(RuntimeError):
                verified_base(source, manifest["sha256"], "management")
            (source / "extra.go").unlink()
            for mutation in ("hash", "files"):
                altered = copy.deepcopy(manifest)
                if mutation == "hash":
                    altered["sha256"] = "0" * 64
                else:
                    altered["files"]["missing.go"] = "0" * 64
                (parent / "source-manifest.json").write_text(json.dumps(altered), encoding="ascii")
                with self.assertRaises(RuntimeError):
                    verified_base(source, manifest["sha256"], "management")

    def test_management_requires_same_source_and_all_successful_local_checks(self):
        with tempfile.TemporaryDirectory() as directory:
            parent = Path(directory)
            source, manifest, race, local = self.make_management(parent)
            for field, value in [("schema", "wrong"), ("manifest_sha256", "wrong"), ("source_after_sha256", "wrong"),
                                 ("source_unchanged", False), ("source_unchanged", 1), ("passed", False),
                                 ("extra_source_files", ["extra.go"]), ("checks", []), ("checks", None)]:
                with self.subTest(field=field, value=value):
                    self.write_management(parent, race, dict(local, **{field: value}))
                    with self.assertRaises(RuntimeError):
                        verified_base(source, manifest["sha256"], "management")
            for index in range(4):
                for mutation in ("failure", "missing", "duplicate", "boolean_exit", "failed_tests"):
                    with self.subTest(check=index, mutation=mutation):
                        altered = copy.deepcopy(local)
                        if mutation == "failure":
                            altered["checks"][index]["exit_code"] = 1
                        elif mutation == "boolean_exit":
                            altered["checks"][index]["exit_code"] = False
                        elif mutation == "missing":
                            altered["checks"].pop(index)
                        elif mutation == "failed_tests":
                            altered["checks"][index]["failed"] = ["TestFailure"]
                        else:
                            altered["checks"].append(altered["checks"][index].copy())
                        self.write_management(parent, race, altered)
                        with self.assertRaises(RuntimeError):
                            verified_base(source, manifest["sha256"], "management")

    def test_only_gateway_and_management_snapshot_roots_are_allowed(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            for folder, kind in (("source-20260918", "gateway"), ("management-parity", "management")):
                source = root / ".local" / folder / "snapshot" / "source"
                source.mkdir(parents=True)
                self.assertEqual(baseline_type(source, root), kind)
                with self.assertRaises(RuntimeError):
                    baseline_type(source.parent, root)
            for folder in ("management-parity-other", "source-20260918-other", "other"):
                source = root / ".local" / folder / "snapshot" / "source"
                source.mkdir(parents=True)
                with self.assertRaises(RuntimeError):
                    baseline_type(source, root)

    def test_management_cli_freeze_keeps_exact_overlay_and_verification_provenance(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory).resolve()
            source, manifest, _, _ = self.make_management(root / ".local/management-parity/verified")
            (root / "main.go").write_text("package main\n// explicit fixture delta\n", encoding="ascii")
            (root / "unrelated.go").write_text("package main\n// concurrent change\n", encoding="ascii")
            argv = ["gateway_freeze_delta.py", "--base", str(source), "--expected-go-hash", manifest["sha256"],
                    "--overlay-file", "main.go"]
            stdout = io.StringIO()
            with patch.object(delta, "ROOT", root), patch.object(delta, "selected_tests", return_value=["TestSynthetic"]), \
                    patch("sys.argv", argv), redirect_stdout(stdout):
                self.assertEqual(delta.main(), 0)
            summary = json.loads(stdout.getvalue())
            frozen = Path(summary["source_directory"])
            provenance = json.loads((frozen.parent / "overlay-manifest.json").read_text())
            self.assertEqual(set(go_manifest(frozen)["files"]), {"main.go"})
            self.assertEqual((frozen / "main.go").read_bytes(), (root / "main.go").read_bytes())
            self.assertNotEqual(go_manifest(frozen)["sha256"], manifest["sha256"])
            self.assertEqual(provenance["base_verification"]["type"], "management")
            self.assertEqual(len(provenance["base_verification"]["files"]), 3)
            self.assertNotIn("base_build_sha256", provenance)
            self.assertEqual(go_manifest(source), manifest)


if __name__ == "__main__":
    unittest.main()
