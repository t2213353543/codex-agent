import unittest
from pathlib import Path
import tempfile

from native_transport_compare import comparison_reports_passed, go_manifest, required_comparison_reports, validate_frozen_source


class RequiredReportTests(unittest.TestCase):
    def setUp(self):
        self.required = ["comparison", "native_ws_cli", "native_compressed_cli", "native_edit_cli", "native_compact_cli", "native_fallback_cli", "native_parallel_cli"]
        self.evidence = {"exit_code": 0, "oom_killed": False, "skipped_tests": []}
        for name in self.required:
            self.evidence[name + "_report_present"] = True
            self.evidence[name + "_passed"] = True

    def test_all_required_reports_must_exist_and_pass(self):
        self.assertTrue(comparison_reports_passed(self.evidence, self.required))
        for name in self.required:
            for suffix in ("_report_present", "_passed"):
                for value in (None, False, "true", 1):
                    changed = dict(self.evidence, **{name + suffix: value})
                    self.assertFalse(comparison_reports_passed(changed, self.required))

    def test_failure_skip_or_oom_cannot_pass_on_reports_alone(self):
        for key, value in (("exit_code", 1), ("oom_killed", True), ("skipped_tests", ["skipped"]), ("skipped_tests", None)):
            self.assertFalse(comparison_reports_passed(dict(self.evidence, **{key: value}), self.required))

    def test_missing_new_report_allowed_only_for_older_source_without_requirement(self):
        for report in ["native_edit_cli", "native_compact_cli", "native_fallback_cli", "native_parallel_cli"]:
            old = dict(self.evidence)
            del old[report + "_passed"]
            del old[report + "_report_present"]
            self.assertFalse(comparison_reports_passed(old, self.required))
            self.assertTrue(comparison_reports_passed(old, [name for name in self.required if name != report]))

    def test_snapshot_report_scope_is_explicit(self):
        old = {"gateway_native_cli_test.go", "real_cli_websocket_test.go"}
        self.assertEqual(required_comparison_reports("gateway", old), self.required[:2])
        latest = old | {"gateway_native_compression_cli_test.go", "gateway_native_cli_edit_test.go", "gateway_native_cli_compact_test.go", "gateway_native_cli_fallback_test.go", "gateway_native_cli_parallel_test.go"}
        self.assertEqual(required_comparison_reports("gateway", latest), self.required)
        self.assertEqual(required_comparison_reports("tls", latest), ["comparison"])

    def test_parallel_cli_report_is_required_only_with_its_source_and_must_pass(self):
        source = {"gateway_native_cli_parallel_test.go"}
        required = required_comparison_reports("gateway", source)
        self.assertEqual(required, ["comparison", "native_parallel_cli"])
        self.assertTrue(comparison_reports_passed(self.evidence, required))
        missing = dict(self.evidence)
        del missing["native_parallel_cli_report_present"]
        del missing["native_parallel_cli_passed"]
        self.assertFalse(comparison_reports_passed(missing, required))
        for field in ("native_parallel_cli_report_present", "native_parallel_cli_passed"):
            self.assertFalse(comparison_reports_passed(dict(self.evidence, **{field: False}), required))
        self.assertTrue(comparison_reports_passed(missing, required_comparison_reports("gateway", set())))
        self.assertTrue(comparison_reports_passed(missing, required_comparison_reports("tls", source)))

    def test_manifest_rejects_extra_source_missing_source_and_forged_hash(self):
        with tempfile.TemporaryDirectory() as directory:
            source = Path(directory)
            original = source / "probe_test.go"
            original.write_text("package probe\n", encoding="ascii")
            manifest = go_manifest(source)
            validate_frozen_source(source, manifest)
            with self.assertRaisesRegex(RuntimeError, "checksum changed"):
                validate_frozen_source(source, dict(manifest, sha256="0" * 64))
            extra = source / "extra_test.go"
            extra.write_text("package probe\n", encoding="ascii")
            with self.assertRaisesRegex(RuntimeError, "source set"):
                validate_frozen_source(source, manifest)
            extra.unlink()
            original.unlink()
            with self.assertRaisesRegex(RuntimeError, "source set"):
                validate_frozen_source(source, manifest)


if __name__ == "__main__":
    unittest.main()
