"""Offline regressions for observer exports and real aggregate expressions.

No remote program is imported or executed. PostgreSQL scalar casts/functions
are adapted to SQLite over synthetic rows; native PostgreSQL checks remain
separate in test_codex_agent_live_monitor.py.
"""
import ast
import copy
import json
import os
from pathlib import Path
import re
import sqlite3
import unittest


ROOT = Path(os.environ.get("CODEX_OBSERVABILITY_SOURCE_ROOT", Path(__file__).resolve().parents[1]))


def source_tree(name):
    return ast.parse((ROOT / "tools" / name).read_text(encoding="utf-8"))


def literal(tree, name):
    return ast.literal_eval(next(node.value for node in tree.body if isinstance(node, ast.Assign)
        and any(isinstance(target, ast.Name) and target.id == name for target in node.targets)))


def remote_tree(name):
    return ast.parse(literal(source_tree(name), "REMOTE"))


def assignments(tree, names, namespace=None):
    namespace = dict(namespace or {})
    for node in tree.body:
        if isinstance(node, ast.Assign) and any(isinstance(target, ast.Name) and target.id in names for target in node.targets):
            exec(compile(ast.Module(body=[node], type_ignores=[]), "<local-source-expression>", "exec"), namespace)
    return namespace


def pure_functions(tree, names, namespace=None):
    namespace = dict(namespace or {})
    nodes = [node for node in tree.body if isinstance(node, ast.FunctionDef) and node.name in names]
    exec(compile(ast.Module(body=nodes, type_ignores=[]), "<local-pure-functions>", "exec"), namespace)
    return namespace


def pg_bool(value):
    if value is None:
        return None
    if value in (True, "true"):
        return 1
    if value in (False, "false"):
        return 0
    raise ValueError(value)


def local_sql(query):
    query = re.sub(r"(\([^()]*?->>[^()]*?\))::boolean", r"pg_bool(\1)", query)
    query = re.sub(r"::(?:bigint|numeric)\b", "", query)
    return re.sub(r"now\(\)-interval '\d+ (?:hours?|minutes?)'", "0", query)


class ObserverContractTests(unittest.TestCase):
    def setUp(self):
        self.db = sqlite3.connect(":memory:")
        self.addCleanup(self.db.close)
        self.db.row_factory = sqlite3.Row
        self.db.create_function("pg_bool", 1, pg_bool)
        self.db.create_function("regexp_replace", 3, lambda value, pattern, replacement: re.sub(pattern, replacement, value, count=1))
        self.db.execute("CREATE TABLE fixture(record TEXT,input_tokens INTEGER,output_tokens INTEGER,cached_tokens INTEGER,status TEXT,model TEXT,event_id TEXT,created_at INTEGER)")
        self.db.execute("CREATE VIEW usage_events AS SELECT input_tokens,output_tokens,cached_tokens,status,event_id,created_at FROM fixture")
        self.db.execute("CREATE VIEW request_logs AS SELECT record,status,model,event_id,created_at FROM fixture")

    def fixture(self, rows):
        self.db.executemany("INSERT INTO fixture VALUES(?,?,?,?,?,?,?,?)", [
            (json.dumps(row), row.get("input_tokens", 0), row.get("output_tokens", 0), row.get("cached_tokens", 0),
             row.get("status", "ok"), row.get("model", "fixture-model"), str(index), 1) for index, row in enumerate(rows)])

    def cache_aggregate(self, rows, script="cache_rate_report.py"):
        self.db.execute("DELETE FROM fixture")
        self.fixture(rows)
        tree = remote_tree(script)
        scope = assignments(tree, {"cache_eligible", "read_known", "aggregate"})
        source = "SELECT e.*,l.record->'prompt_cache' d,(" + scope["read_known"] + ") read_known," \
            "(l.record->>'cache_write_tokens')::bigint cache_write"
        if "cache_eligible" in scope:
            source += ",(" + scope["cache_eligible"] + ") cache_eligible"
        source += " FROM fixture e JOIN fixture l USING(event_id)"
        query = "WITH r AS (" + source + ") SELECT " + scope["aggregate"] + " FROM r"
        return dict(self.db.execute(local_sql(query)).fetchone())

    def test_cache_report_excludes_control_and_warmup_without_losing_requests(self):
        result = self.cache_aggregate([
            {"endpoint": "responses", "input_tokens": 2000, "cached_tokens": 1500, "cache_write_tokens": 300},
            {"endpoint": "/v1/responses", "input_tokens": 1000, "cached_tokens": 0, "cached_tokens_known": True, "cache_write_tokens": 0},
            {"endpoint": "responses", "input_tokens": 7000},
            {"endpoint": "models", "input_tokens": 9000, "cached_tokens": 9000, "cache_write_tokens": 9000},
            {"endpoint": "responses", "websocket_warmup": True, "input_tokens": 9000, "cached_tokens": 9000, "cache_write_tokens": 9000},
            {"endpoint": "models"},
        ])
        self.assertEqual(result["requests"], 6)
        self.assertEqual(result["cache_percent"], 50)
        self.assertEqual(result["cache_read_samples"], 2)
        self.assertEqual(result["cache_read_unknown_requests"], 1)
        self.assertEqual(result["cache_read_input_tokens"], 3000)
        self.assertEqual(result["cache_write_tokens"], 300)
        self.assertEqual(result["cache_write_samples"], 2)

    def test_cache_report_unknown_and_creation_stay_independent(self):
        result = self.cache_aggregate([
            {"endpoint": "responses", "input_tokens": 2000, "cached_tokens_known": False, "cached_tokens": 1500, "cache_write_tokens": 0},
            {"endpoint": "responses", "input_tokens": 1000, "cached_tokens_known": True, "cached_tokens": 1500},
        ])
        self.assertIsNone(result["cache_percent"])
        self.assertEqual(result["cache_read_samples"], 0)
        self.assertEqual(result["cache_read_unknown_requests"], 2)
        self.assertEqual(result["cache_write_tokens"], 0)
        self.assertEqual(result["cache_write_samples"], 1)

    def test_missing_read_and_creation_remain_unknown_in_both_reports(self):
        for script in ("cache_rate_report.py", "recent_failures_cache_audit.py"):
            with self.subTest(script=script):
                result = self.cache_aggregate([{"endpoint": "responses", "input_tokens": 2000}], script)
                self.assertIsNone(result["cache_percent"])
                self.assertEqual(result["cache_read_samples"], 0)
                self.assertEqual(result["cache_read_unknown_requests"], 1)
                self.assertIsNone(result["cache_write_tokens"])
                self.assertEqual(result["cache_write_samples"], 0)

    def test_control_only_does_not_make_unknown_cache_samples(self):
        for script in ("cache_rate_report.py", "recent_failures_cache_audit.py"):
            with self.subTest(script=script):
                result = self.cache_aggregate([{"endpoint": "models", "status": "error", "cache_write_tokens": 0},
                    {"endpoint": "responses", "websocket_warmup": True}], script)
                self.assertEqual(result["requests"], 2)
                self.assertEqual(result["cache_read_unknown_requests"], 0)
                self.assertIsNone(result["cache_percent"])
                self.assertIsNone(result["cache_write_tokens"])
                self.assertEqual(result["cache_write_samples"], 0)

    def test_actual_ctes_keep_aliases_and_current_log_scope(self):
        self.fixture([{"endpoint": "/backend-api/codex/responses", "input_tokens": 1000,
            "cached_tokens_known": True, "cached_tokens": 0, "cache_write_tokens": 500},
            {"endpoint": "", "input_tokens": 1000, "cached_tokens": 500},
            {"endpoint": "/v1/models", "input_tokens": 9000, "cached_tokens": 9000, "cache_write_tokens": 9000}])
        for script in ("cache_rate_report.py", "recent_failures_cache_audit.py"):
            scope = assignments(remote_tree(script), {"since", "minutes", "condition", "cache_eligible", "read_known", "aggregate", "cte", "log_cte"},
                {"__SINCE__": "", "__MINUTES__": 10})
            for cte_name in ("cte", "log_cte") if "log_cte" in scope else ("cte",):
                with self.subTest(script=script, cte=cte_name):
                    result = dict(self.db.execute(local_sql(scope[cte_name] + "SELECT " + scope["aggregate"] + " FROM r")).fetchone())
                    self.assertEqual(result["requests"], 3)
                    self.assertEqual(result["cache_percent"], 25)
                    self.assertEqual(result["cache_read_samples"], 2)
                    self.assertEqual(result["cache_read_unknown_requests"], 0)
                    self.assertEqual(result["cache_write_tokens"], 500)

    def test_audit_windows_use_observed_input_only(self):
        tree = remote_tree("recent_failures_cache_audit.py")
        scope = assignments(tree, {"cache_eligible", "read_known", "aggregate", "cte"})
        windows = next(node for node in tree.body if isinstance(node, ast.Assign)
            and isinstance(node.targets[0], ast.Subscript) and ast.literal_eval(node.targets[0].slice) == "windows")
        query = eval(compile(ast.Expression(windows.value.args[0]), "<audit-windows-query>", "eval"), scope)
        self.fixture([
            {"endpoint": "responses", "input_tokens": 2000, "cached_tokens": 1500, "cache_write_tokens": 300},
            {"endpoint": "responses", "input_tokens": 1000, "cached_tokens_known": True},
            {"endpoint": "responses", "input_tokens": 7000},
        ])
        # Exercise the exact windows aggregate, reusing the source predicates on
        # fixtures instead of running its remote Docker/SSH wrapper.
        if "read_known" in scope:
            self.db.execute("DELETE FROM fixture")
            result = self.cache_aggregate([
                {"endpoint": "responses", "input_tokens": 2000, "cached_tokens": 1500, "cache_write_tokens": 300},
                {"endpoint": "responses", "input_tokens": 1000, "cached_tokens_known": True},
                {"endpoint": "responses", "input_tokens": 7000},
            ], "recent_failures_cache_audit.py")
            self.assertIn(scope["aggregate"], query)
            actual = result["cache_percent"]
            self.assertEqual(result["cache_read_unknown_requests"], 1)
            self.assertEqual(result["cache_write_tokens"], 300)
        else:
            expression = next(line.strip() for line in query.splitlines() if "AS cache_percent" in line)
            actual = self.db.execute("SELECT " + expression + " FROM fixture").fetchone()[0]
        self.assertEqual(actual, 50)

    def test_audit_attempt_export_retains_current_structured_fields(self):
        tree = remote_tree("recent_failures_cache_audit.py")
        attempt = {"attempt": 1, "phase": "request", "endpoint": "/responses", "status_code": 429,
            "duration_ms": 12, "header_ms": 3, "error_source": "upstream", "error_code": "usageLimitExceeded"}
        scope = pure_functions(tree, {"redact"}, {"re": re, "rows": [{"error": "fixture", "attempts": [copy.deepcopy(attempt)]}]})
        loop = next(node for node in tree.body if isinstance(node, ast.For) and isinstance(node.target, ast.Name) and node.target.id == "row")
        exec(compile(ast.Module(body=[loop], type_ignores=[]), "<audit-export-loop>", "exec"), scope)
        self.assertEqual(scope["rows"][0]["attempts"][0], attempt)

    def classifier(self):
        return pure_functions(source_tree("codex_agent_live_monitor.py"), {"classify_error"}, {"re": re})["classify_error"]

    def test_official_usage_limit_is_rate_limit_with_and_without_attempts(self):
        classify = self.classifier()
        for code in ("usageLimitExceeded", "rateLimitExceeded"):
            for attempts in ([], [{"error_code": code, "status_code": 429}]):
                with self.subTest(code=code, attempts=attempts):
                    self.assertEqual(classify({"http_code": 429,
                        "error": "official App Server inference error (" + code + "): Limit reached", "attempts": attempts}), "upstream_rate_limit")

    def test_previous_attempt_does_not_override_final_protocol_failure(self):
        self.assertEqual(self.classifier()({"http_code": 502, "error": "official runtime emitted an undeclared tool call",
            "attempts": [{"attempt": 1, "status_code": 429, "error_code": "rateLimitExceeded"},
                         {"attempt": 2, "status_code": 502, "error_code": "protocol"}]}), "tool_protocol")

    def test_truncated_attempts_are_not_the_terminal_request_error(self):
        self.assertEqual(self.classifier()({"http_code": 502, "error": "gateway failure", "attempts_truncated": True,
            "attempts": [{"attempt": 1, "status_code": 403, "error_code": "upstream_account_rejected"}]}), "other_error")


if __name__ == "__main__":
    unittest.main(verbosity=2)
