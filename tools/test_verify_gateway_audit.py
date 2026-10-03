"""Offline regression tests for the gateway audit runner's execution boundaries."""
from contextlib import redirect_stderr, redirect_stdout
import io
import json
import os
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch

import verify_gateway_audit as audit


class GatewayAuditRunnerTests(unittest.TestCase):
    def test_default_plan_does_not_invoke_binaries_or_fuzz_mutations(self):
        plan = audit.make_plan(audit.parser().parse_args([]))
        self.assertEqual([item["name"] for item in plan], ["go_tests", "go_vet", "native_source_provenance"])
        self.assertNotIn("-race", plan[0]["command"])
        self.assertNotIn("--cli-bin", plan[-1]["command"])

    def test_optional_plan_targets_helper_and_all_fuzz_boundaries(self):
        args = audit.parser().parse_args(["--native-bin", "helper", "--cli-bin", "cli", "--race", "--rust", "--fuzz-seconds", "2"])
        plan = audit.make_plan(args)
        self.assertIn("-race", plan[0]["command"])
        rust = next(item["command"] for item in plan if item["name"] == "rust_tests")
        self.assertIn("--offline", rust)
        self.assertEqual(rust[-2:], ["--bin", "codex-native-transport"])
        self.assertEqual({item["name"] for item in plan[4:]}, set(audit.FUZZ_TARGETS))
        self.assertTrue(all("-fuzztime=2s" in item["command"] for item in plan[4:]))

    def test_inherited_live_settings_and_credentials_cannot_enable_integration(self):
        inherited = {"PATH": "tools", "CODEX_CLI_ACCOUNT_FILE": "never-read.json", "CODEX_NATIVE_HTTP_TEST_BIN": "unexpected",
                     "CODEX_SETTINGS_MODEL": "live", "TEST_DATABASE_URL": "synthetic", "TEST_REDIS_URL": "synthetic",
                     "GATEWAY_NATIVE_FIXTURE": "1", "OPENAI_API_KEY": "synthetic", "CUSTOM_ACCESS_TOKEN": "synthetic",
                     "HTTP_PROXY": "synthetic", "https_proxy": "synthetic", "GOFLAGS": "-exec unexpected"}
        with patch.dict(os.environ, inherited, clear=True):
            env = audit.local_environment()
        self.assertEqual(env["PATH"], "tools")
        self.assertEqual(env["GOPROXY"], "off")
        self.assertEqual(env["CARGO_NET_OFFLINE"], "true")
        for key in inherited.keys() - {"PATH"}:
            self.assertNotIn(key, env)

    def test_source_hash_includes_embedded_sql_and_excludes_account_files(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "migrations").mkdir()
            sql = root / "migrations/001.sql"
            sql.write_text("select 1;", encoding="utf-8")
            (root / "accounts.json").write_text("synthetic secret", encoding="utf-8")
            with patch.object(audit, "ROOT", root):
                first = audit.source_manifest()
                sql.write_text("select 2;", encoding="utf-8")
                second = audit.source_manifest()
            self.assertEqual(set(first["files"]), {"migrations/001.sql"})
            self.assertNotEqual(first["sha256"], second["sha256"])

    def run_fake_check(self, events, exit_code=0):
        with tempfile.TemporaryDirectory() as directory:
            code = "import json,sys; events=" + repr(events) + "; [print(json.dumps(x)) for x in events]; sys.exit(" + str(exit_code) + ")"
            check = {"name": "fake_go", "command": [sys.executable, "-c", code], "cwd": directory, "go_json": True}
            with redirect_stderr(io.StringIO()):
                return audit.run_check(check, audit.local_environment(), Path(directory))

    def test_go_skip_is_counted_separately_from_pass(self):
        result = self.run_fake_check([{"Test": "TestLocal", "Action": "pass"},
                                      {"Test": "TestNative", "Action": "skip"}])
        self.assertEqual(result["status"], "pass")
        self.assertEqual(result["test_counts"], {"pass": 1, "skip": 1, "fail": 0})

    def test_go_empty_success_and_nonzero_exit_are_failures(self):
        self.assertEqual(self.run_fake_check([])["status"], "fail")
        result = self.run_fake_check([{"Test": "TestLocal", "Action": "fail"}], 1)
        self.assertEqual(result["status"], "fail")
        self.assertEqual(result["test_counts"]["fail"], 1)

    def test_final_report_distinguishes_skips_and_source_drift(self):
        for drift in (False, True):
            with self.subTest(drift=drift), tempfile.TemporaryDirectory() as directory:
                output = Path(directory) / "result.json"
                before = {"sha256": "first", "files": {}, "file_count": 0}
                after = {**before, "sha256": "different" if drift else "first"}
                with patch.object(audit, "source_manifest", side_effect=[before, after]), \
                     patch.object(audit, "make_plan", return_value=[]), \
                     redirect_stdout(io.StringIO()), redirect_stderr(io.StringIO()):
                    code = audit.main(["--output", str(output)])
                report = json.loads(output.read_text(encoding="utf-8"))
                self.assertEqual(code, int(drift))
                self.assertEqual(report["status"], "failed" if drift else "passed_with_skips")
                self.assertFalse(report["all_checks_executed"])
                self.assertGreater(report["checks"]["skip"], 0)

    def test_optional_runtime_never_passes_from_skipped_child_or_missing_opt_in(self):
        for requested, child_status in ((False, "pass"), (True, "skip"), (True, "pass")):
            with self.subTest(requested=requested, child_status=child_status), tempfile.TemporaryDirectory() as directory:
                output = Path(directory) / "result.json"
                binary = Path(directory) / "helper"
                binary.write_bytes(b"mocked; never executed")
                manifest = {"sha256": "stable", "files": {}, "file_count": 0}
                result = {"name": "go_tests", "status": "pass", "tests": [
                    {"test": "TestGatewayNativeActualExample/scenario", "status": child_status},
                    {"test": "TestGatewayNativeActualExample", "status": "pass"},
                ]}
                arguments = ["--output", str(output)]
                if requested:
                    arguments.extend(["--native-bin", str(binary)])
                with patch.object(audit, "source_manifest", return_value=manifest), \
                     patch.object(audit, "make_plan", return_value=[{"name": "go_tests"}]), \
                     patch.object(audit, "run_check", return_value=result), \
                     redirect_stdout(io.StringIO()), redirect_stderr(io.StringIO()):
                    self.assertEqual(audit.main(arguments), 0)
                report = json.loads(output.read_text(encoding="utf-8"))
                runtime = next(item for item in report["results"] if item["name"] == "native_runtime")
                self.assertEqual(runtime["status"], "pass" if requested and child_status == "pass" else "skip")
                self.assertEqual(sum(runtime["test_counts"].values()), 1)

    def test_dry_run_runs_no_checks_and_writes_no_report(self):
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory) / "not-created.json"
            with patch.object(audit, "run_check") as run, redirect_stdout(io.StringIO()) as stdout:
                self.assertEqual(audit.main(["--dry-run", "--output", str(output)]), 0)
            run.assert_not_called()
            self.assertFalse(output.exists())
            self.assertEqual(json.loads(stdout.getvalue())["checks_executed"], 0)


if __name__ == "__main__":
    unittest.main()
