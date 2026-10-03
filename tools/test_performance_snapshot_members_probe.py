"""Offline preparation/guard checks. These tests never connect to PostgreSQL."""
import bisect
import contextlib
import io
import json
import pathlib
import tempfile
import unittest
from unittest.mock import patch

import performance_snapshot_members_probe as probe


class SnapshotMemberPrototypeTests(unittest.TestCase):
    def test_default_prepares_without_importing_network_capabilities(self):
        with tempfile.TemporaryDirectory() as directory, patch.dict("sys.modules", {"integration_env": None}):
            with patch("sys.argv", ["probe", "--output", directory]), contextlib.redirect_stdout(io.StringIO()) as output:
                probe.main()
            result = json.loads(output.getvalue())
            self.assertEqual(result["status"], "prepared_only")
            self.assertEqual(result["postgres_connections"], 0)
            self.assertEqual(result["rows"], 100_000)
            folder = pathlib.Path(directory)
            payload = json.loads((folder / "prepared-payload.json").read_text())
            self.assertEqual(payload["schema"], "perf_snapshot_members_probe")
            compile((folder / "remote_runner.py").read_text(), "prepared_runner", "exec")

    def test_scale_and_target_guards_are_fixed(self):
        with tempfile.TemporaryDirectory() as directory:
            for rows in (-1, 0, 99, 1_000_001, 10_000_000):
                with self.subTest(rows=rows), self.assertRaises(ValueError):
                    probe.prepare(pathlib.Path(directory), rows)
        self.assertEqual(probe.CONTAINER, "codex-agent-integration-postgres")
        self.assertEqual(probe.DATABASE, "codex_agent_integration")
        for guard in ("integration_container_label_mismatch", "database_role_guard", "fixed_schema_already_exists_no_overwrite", "cleanup_owner_marker_mismatch"):
            self.assertIn(guard, probe.REMOTE)
        self.assertNotIn("codex-agent-postgres", probe.REMOTE)
        self.assertIn("statement_timeout=30000", probe.OPTIONS)
        self.assertIn("enable_hashagg=off", probe.OPTIONS)

    def test_chunk_and_detail_contract_full_bigint_domain(self):
        # Independent arithmetic characterization: sorted membership carries
        # exact IDs, never first/last bounds alone or subtraction from min-ID.
        ids = sorted(set(probe.BOUNDARIES + list(range(2, 40000, 2))))
        chunks = [ids[i:i + 4096] for i in range(0, len(ids), 4096)]
        lower = [chunk[0] for chunk in chunks]
        for target in ids + [-3, -2, 1, 3, 4097, 9223372036854775806]:
            offset = bisect.bisect_right(lower, target) - 1
            found = offset >= 0 and target <= chunks[offset][-1] and target in chunks[offset]
            self.assertEqual(found, target in ids)
        self.assertTrue(all(0 < len(chunk) <= 4096 for chunk in chunks))
        text = probe.setup_sql(100_000)
        self.assertIn("'-9223372036854775808'::bigint", text)
        self.assertIn("PRIMARY KEY(snapshot_id,first_id)", text)
        self.assertIn("CHECK(valid_packed_ids(ids))", text)

    def test_captures_and_reference_share_one_statement_snapshot(self):
        sql = probe.simultaneous_capture("semantic", "status='selected'", True)
        self.assertEqual(sql.count("FROM fixture_logs"), 1)
        self.assertIn("selected AS MATERIALIZED", sql)
        self.assertIn("FROM selected RETURNING 1", sql)
        self.assertIn("row_number() OVER(ORDER BY id)-1)/4096", sql)
        self.assertIn("FROM ranked GROUP BY chunk_no", sql)
        self.assertNotIn("max(id)", sql.split("), written AS", 1)[0])
        self.assertNotIn("xmin", sql)
        self.assertNotIn("txid", sql)
        generated = probe.scripts(100_000)
        self.assertIn("JOIN fixture_logs l ON l.id=member.log_id", generated["21_semantic_mutation.sql"])
        self.assertIn("late_uncommitted_low_id_excluded", generated["25_concurrent_assert.sql"])
        self.assertIn("capture_did_not_reach_statement_snapshot_barrier", probe.REMOTE)
        self.assertIn("explicit_same_id_reuse", generated["40_same_id_reuse_characterization.sql"])

    def test_fair_row_capture_matches_insert_command_tag_boundary(self):
        for predicate in ("true", "status='selected'"):
            sql = probe.capture("row_all", predicate, False)
            self.assertTrue(sql.startswith("INSERT INTO exact_members("))
            for extra in ("RETURNING", "WITH", "count(", "MATERIALIZED"):
                self.assertNotIn(extra, sql)
            self.assertIn("FROM fixture_logs WHERE " + predicate, sql)
        self.assertIn("PROBE_ROW_COUNT :ROW_COUNT", probe.REMOTE)
        self.assertIn("counts[0]==tags[0]", probe.REMOTE)
        self.assertIn("range(6)", probe.REMOTE)
        self.assertIn("if repetition%2==0 else ['packed','row']", probe.REMOTE)

    def test_fresh_generic_sparse_and_dedup_are_separate_experiments(self):
        text = probe.fair_scripts(100000)
        self.assertIn("autovacuum_enabled=false", text["01_setup.sql"])
        self.assertIn("generate_series(0,4095)", text["26_sparse_full.sql"])
        self.assertIn("cardinality(ids)=4096", text["26_sparse_full.sql"])
        self.assertIn("generate_series(3997,4096)", probe.fair_queries()["sparse_page_each"])
        self.assertIn("SELECT DISTINCT located.first_id", probe.fair_queries()["page_dedup"])
        self.assertIn("page_dedup_membership_parity", text["27_page_parity.sql"])
        self.assertIn("['fresh_no_analyze','analyzed']", probe.REMOTE)
        self.assertIn("['force_custom_plan','force_generic_plan']", probe.REMOTE)

    def test_original_evidence_cannot_be_overwritten(self):
        with tempfile.TemporaryDirectory() as directory:
            folder=pathlib.Path(directory)
            (folder/"report.json").write_text('{"original":true}')
            with self.assertRaises(ValueError):
                probe.prepare(folder,100000)
            self.assertEqual((folder/"report.json").read_text(),'{"original":true}')


if __name__ == "__main__":
    unittest.main()
