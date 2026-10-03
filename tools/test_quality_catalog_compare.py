"""Offline catalog evidence boundaries; no credentials, networking, or model calls."""
from __future__ import annotations

import copy
import io
import json
from pathlib import Path
import tempfile
import unittest
from contextlib import redirect_stderr, redirect_stdout

import quality_catalog_compare as catalog


def model(slug="gpt-5.4", **changes):
    out = {"slug": slug, "display_name": "Test", "description": None,
           "default_reasoning_level": "medium",
           "supported_reasoning_levels": [{"effort": "medium", "description": "test"}],
           "shell_type": "unified_exec", "visibility": "list", "supported_in_api": True,
           "priority": 1, "support_verbosity": True,
           "truncation_policy": {"mode": "tokens", "limit": 10000},
           "experimental_supported_tools": [], "input_modalities": ["text", "image"],
           "context_window": 272000, "tool_mode": "direct",
           "model_messages": {"instructions_template": "private system instructions"}}
    out.update(changes)
    return out


def account(alias="q0", models=None, **provenance):
    return {"alias": alias, "catalog": {"models": [model()] if models is None else models},
            "provenance": {"source": "network", "client_version": catalog.VERSION,
                           "http_status": 200, **provenance},
            "auth_plan": {"claim_source": "id_token", "chatgpt_plan_type": "plus"}}


class CatalogCompareTests(unittest.TestCase):
    def pair(self, a=None, b=None):
        return catalog.compare_accounts([a or account(), b or account("q1")])["comparisons"][0]

    def test_complete_same_model_comparison_and_reordered_catalog(self):
        a = account(models=[model(), model("gpt-5.3-codex")])
        b = account("q1", models=list(reversed(a["catalog"]["models"])))
        self.assertTrue(self.pair(a, b)["catalogs_equal_by_model"])

    def test_different_models_not_silently_ignored(self):
        pair = self.pair(account(models=[model("gpt-5.4")]), account("q1", [model("gpt-5.3-codex")]))
        self.assertFalse(pair["catalogs_equal_by_model"])
        self.assertEqual(pair["only_left"], ["gpt-5.4"])
        self.assertEqual(pair["only_right"], ["gpt-5.3-codex"])
        self.assertEqual(pair["shared_models"], [])

    def test_missing_empty_null_differ(self):
        a, b, c = account(), account("q1"), account("q2")
        del a["catalog"]["models"][0]["input_modalities"]
        b["catalog"]["models"][0]["input_modalities"] = []
        c["catalog"]["models"][0]["input_modalities"] = None
        pairs = catalog.compare_accounts([a, b, c])["comparisons"]
        self.assertTrue(all(not item["catalogs_equal_by_model"] for item in pairs))
        difference = pairs[0]["shared_models"][0]["differences"][0]
        self.assertEqual(difference["left"], {"state": "missing"})
        self.assertEqual(difference["right"]["type"], "array")

    def test_nested_instructions_and_unknown_fields_hashed_without_echo(self):
        a, b = account(), account("q1")
        b["catalog"]["models"][0]["model_messages"]["instructions_template"] = "secret instruction two"
        b["catalog"]["models"][0]["user@example.test"] = {"access_token": "Bearer never-copy-me"}
        report = catalog.compare_accounts([a, b])
        encoded = json.dumps(report)
        for forbidden in ("private system instructions", "secret instruction", "user@example.test", "never-copy-me", "access_token"):
            self.assertNotIn(forbidden, encoded)
        fields = report["comparisons"][0]["shared_models"][0]["differences"]
        self.assertEqual(len(fields), 2)
        self.assertEqual(fields[0]["field"], "model_messages")
        self.assertTrue(fields[1]["field"].startswith("unknown_field_sha256:"))

    def test_duplicate_slug_excludes_entire_catalog(self):
        a = account(models=[model(), model(context_window=1)])
        report = catalog.compare_accounts([a, account("q1")])
        self.assertIn("duplicate_model_slug", report["accounts"][0]["catalog_schema_issues"])
        self.assertEqual(report["accounts"][0]["models"], [])
        self.assertEqual(report["comparisons"][0]["status"], "excluded")

    def test_wrong_directory_schema_and_null_do_not_crash(self):
        for raw in (None, [], {"data": [model()]}, {"models": None}, {"models": [None]}, {"models": [{"slug": "gpt-5.4"}]}):
            with self.subTest(raw=type(raw).__name__):
                a = account()
                a["catalog"] = raw
                report = catalog.compare_accounts([a, account("q1")])
                self.assertFalse(report["accounts"][0]["comparable"])
                self.assertEqual(report["comparisons"][0]["status"], "excluded")

    def test_http_error_not_compared_even_with_models(self):
        for status in (429, 401, 403, 500, 304, True, "200", None):
            with self.subTest(status=status):
                report = catalog.compare_accounts([account(http_status=status), account("q1")])
                self.assertEqual(report["comparisons"][0]["status"], "excluded")
        a = account()
        a["catalog"]["error"] = {"message": "secret upstream error"}
        report = catalog.compare_accounts([a, account("q1")])
        self.assertEqual(report["comparisons"][0]["status"], "excluded")
        self.assertNotIn("secret upstream error", json.dumps(report))

    def test_missing_optional_status_is_distinguished(self):
        a = account()
        del a["provenance"]["http_status"]
        report = catalog.compare_accounts([a, account("q1")])
        self.assertTrue(report["comparisons"][0]["comparable"])
        self.assertFalse(report["accounts"][0]["provenance"]["http_status_observed"])
        self.assertEqual(report["accounts"][0]["provenance"]["http_status"], {"state": "missing"})

    def test_version_missing_old_or_wrong_type_excluded(self):
        for version in ("0.154.0", None, {"secret": "private"}, "0.155.0-dev.1"):
            with self.subTest(version=type(version).__name__):
                self.assertFalse(self.pair(account(client_version=version))["comparable"])

    def test_cache_bundled_and_fallback_not_live_evidence(self):
        for source in ("cache", "bundled", "fallback", "unknown"):
            self.assertEqual(self.pair(account(source=source))["status"], "excluded")
        a = account()
        a["catalog"]["fetched_at"] = "2026-09-24T00:00:00Z"
        a["catalog"]["identity"] = "private-account"
        report = catalog.compare_accounts([a, account("q1")])
        self.assertIn("cache_envelope_as_network", report["accounts"][0]["exclusion_reasons"])
        self.assertNotIn("private-account", json.dumps(report))
        a = account(models=[model(used_fallback_model_metadata=True)])
        self.assertFalse(self.pair(a)["comparable"])

    def test_auth_claim_whitelist_without_identifier_or_account_flag_inference(self):
        a = account()
        a["auth_plan"].update({"chatgpt_account_id": "account-secret", "email": "private@example.test",
                               "access_token": "token-secret", "risk_flag": True,
                               "chatgpt_account_is_fedramp": False})
        report = catalog.compare_accounts([a])
        summary = report["accounts"][0]["auth_plan"]
        self.assertEqual(summary["chatgpt_plan_type"]["value"], "plus")
        self.assertIs(summary["chatgpt_account_is_fedramp"]["value"], False)
        self.assertEqual(summary["claim_verification"], "not_verified_by_comparator")
        for forbidden in ("account-secret", "private@example.test", "token-secret", "risk_flag"):
            self.assertNotIn(forbidden, json.dumps(report))

    def test_unknown_plan_and_effort_retained_by_hash(self):
        a, b = account(), account("q1")
        b["auth_plan"]["chatgpt_plan_type"] = "private_new_plan"
        b["catalog"]["models"][0]["default_reasoning_level"] = "private_custom_effort"
        report = catalog.compare_accounts([a, b])
        self.assertNotIn("private_new_plan", json.dumps(report))
        self.assertNotIn("private_custom_effort", json.dumps(report))
        self.assertFalse(report["comparisons"][0]["catalogs_equal_by_model"])

    def test_capabilities_not_limited_to_requested_model(self):
        a = account(models=[model(), model("gpt-5.3-codex")])
        b = copy.deepcopy(a)
        b["alias"] = "q1"
        b["catalog"]["models"][1].update(context_window=128000, supported_in_api=False, tool_mode="code_mode")
        differences = self.pair(a, b)["shared_models"][0]["differences"]
        self.assertEqual([item["field"] for item in differences], ["context_window", "supported_in_api", "tool_mode"])
        self.assertEqual(differences[2]["right"]["value"], "code_mode")

    def test_unknown_slug_not_dropped_or_exposed(self):
        pair = self.pair(account(models=[model("private-user@example.test")]), account("q1", [model("private-user@example.test", priority=2)]))
        self.assertFalse(pair["catalogs_equal_by_model"])
        self.assertTrue(pair["shared_models"][0]["model_key"].startswith("model_sha256:"))
        self.assertNotIn("private-user", json.dumps(pair))

    def test_duplicate_json_key_and_nonfinite_values_rejected(self):
        for raw in ('{"models": [], "models": []}', '{"foo":NaN}', '{"foo":Infinity}'):
            with self.assertRaises(catalog.CatalogError):
                catalog.parse_json(raw)
        with self.assertRaises(catalog.CatalogError):
            catalog.compare_accounts([account(models=[model(priority=float("nan"))])])

    def test_invalid_alias_and_duplicate_alias_rejected_without_echo(self):
        with self.assertRaisesRegex(catalog.CatalogError, "invalid_anonymous_alias"):
            catalog.compare_accounts([account("private@example.test")])
        with self.assertRaisesRegex(catalog.CatalogError, "duplicate_alias"):
            catalog.compare_accounts([account(), account()])

    def test_file_cli_and_relative_catalog_only_write_sanitized_report(self):
        with tempfile.TemporaryDirectory() as directory:
            folder = Path(directory)
            (folder / "raw.json").write_text(json.dumps(account()["catalog"]), encoding="utf-8")
            item = account()
            item.pop("catalog")
            item["catalog_file"] = "raw.json"
            (folder / "input.json").write_text(json.dumps({"schema": catalog.INPUT_SCHEMA, "accounts": [item]}), encoding="utf-8")
            stdout = io.StringIO()
            with redirect_stdout(stdout):
                result = catalog.main([str(folder / "input.json"), "--output", str(folder / "out.json")])
            self.assertEqual(result, 0)
            report = (folder / "out.json").read_text(encoding="utf-8")
            self.assertNotIn("private system instructions", report)
            self.assertNotIn(str(folder), stdout.getvalue())
            self.assertEqual(json.loads(report)["schema"], catalog.REPORT_SCHEMA)

    def test_cli_errors_do_not_echo_paths_or_json(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "private@example.test.json"
            path.write_text('{"secret":"Bearer do-not-echo"', encoding="utf-8")
            stderr = io.StringIO()
            with redirect_stderr(stderr):
                result = catalog.main([str(path), "--output", str(path.with_suffix(".out"))])
            self.assertEqual(result, 2)
            self.assertEqual(json.loads(stderr.getvalue()), {"status": "error", "code": "invalid_json"})


if __name__ == "__main__":
    unittest.main()
