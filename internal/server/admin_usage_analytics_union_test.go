package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestAnalyticsUnionBaselinesPreserveDecoder(t *testing.T) {
	for _, filter := range []string{"error_category=rate_limit", "request_id=union-value&error_severity=P1", "group_id=g1&model=m2"} {
		prefix, _, _, _ := managedUsageDecodedSource(managedQuery(t, filter))
		if strings.Count(prefix, "jsonb_to_record(record)") != 2 {
			t.Fatal("typed branches did not each get an exact decoder")
		}
		outside := analyticsDecodeOutsideBaselineSQL(prefix)
		if strings.Count(outside, "jsonb_to_record(record)") != 1 || !strings.Contains(outside, "UNION ALL SELECT missing.id") {
			t.Fatalf("decoder baseline omitted a branch or duplicated decode: %s", outside)
		}
		scalar := analyticsScalarBaselineSQL(prefix)
		if strings.Count(scalar, "jsonb_to_record(record)") != 1 || strings.Contains(scalar, "UNION ALL SELECT missing.id") || !strings.Contains(scalar, "original.id=selected.id") {
			t.Fatal("scalar baseline did not restore the pre-UNION source")
		}
		wide := analyticsWideBaselineSQL(prefix)
		if !strings.Contains(wide, "ELSE selected.record END") || strings.Contains(wide, "original.id=selected.id") {
			t.Fatal("wide baseline did not restore the production source")
		}
	}
}

func TestPGLogAnalyticsUnionPresentValuesStayPresent(t *testing.T) {
	db := newMetricsTestDB(t)
	if _, err := db.Pool.Exec(t.Context(), `WITH e AS (
 INSERT INTO usage_events(event_id,created_at,status) VALUES('union-value',to_timestamp(1790553600),'ok') RETURNING event_id)
 INSERT INTO request_logs(id,event_id,created_at,status,record)
 SELECT 0,event_id,to_timestamp(1790553600),'ok','{"input_tokens":991}'::jsonb FROM e`); err != nil {
		t.Fatal(err)
	}
	for _, record := range []string{`null`, `{}`, `7`, `[]`, `{"input_tokens":null}`} {
		tx, err := db.Pool.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(t.Context(), "UPDATE request_log_analytics SET record=$1::jsonb WHERE log_id=0", record); err != nil {
			t.Fatal(err)
		}
		statement := "SELECT count(*),bool_and(record=$1::jsonb) FROM " + managedUsageAnalyticsSource("request_logs", "id=0")
		var count int
		var equal bool
		if err := tx.QueryRow(t.Context(), statement, record).Scan(&count, &equal); err != nil || count != 1 || !equal {
			t.Fatalf("present projection treated as missing or duplicated: record=%s count=%d equal=%t err=%v", record, count, equal, err)
		}
		if err := tx.Rollback(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
}

func analyticsUnionPlanFeatures(raw json.RawMessage) map[string]any {
	var plans []map[string]any
	if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
		return map[string]any{"parse_error": fmt.Sprint(err)}
	}
	decoded := []map[string]any{}
	gathers, parallelHashes := 0, 0
	fallbackRows := float64(0)
	var walk func(map[string]any, bool)
	walk = func(node map[string]any, underGather bool) {
		loops, _ := node["Actual Loops"].(float64)
		if node["Node Type"] == "Gather" || node["Node Type"] == "Gather Merge" {
			if loops > 0 {
				gathers++
			}
			underGather = true
		}
		if node["Node Type"] == "Hash" && node["Parallel Aware"] == true && loops > 0 {
			parallelHashes++
		}
		if node["Function Name"] == "jsonb_to_record" && loops > 0 {
			decoded = append(decoded, map[string]any{"under_gather": underGather, "loops": loops, "ms_per_loop": node["Actual Total Time"]})
		}
		alias, _ := node["Alias"].(string)
		if node["Relation Name"] == "request_logs" && strings.HasPrefix(alias, "original") {
			rows, _ := node["Actual Rows"].(float64)
			fallbackRows += rows * loops
		}
		if children, ok := node["Plans"].([]any); ok {
			for _, child := range children {
				walk(child.(map[string]any), underGather)
			}
		}
	}
	plan := plans[0]["Plan"].(map[string]any)
	walk(plan, false)
	return map[string]any{"execution_ms": plans[0]["Execution Time"], "gathers": gathers, "parallel_hashes": parallelHashes, "decoded": decoded, "fallback_rows": fallbackRows, "temp_written_blocks": plan["Temp Written Blocks"], "settings": plans[0]["Settings"]}
}

func analyticsPlanLiteral(t *testing.T, value any) string {
	t.Helper()
	switch v := value.(type) {
	case nil:
		return "NULL"
	case string:
		return "'" + strings.ReplaceAll(v, "'", "''") + "'"
	case int, int64, int32:
		return fmt.Sprint(v)
	case time.Time:
		return "'" + v.Format(time.RFC3339Nano) + "'::timestamptz"
	default:
		t.Fatalf("unsupported synthetic query literal %T", value)
		return ""
	}
}

// Called after the retained fixture has been verified/seeded by the existing
// opt-in harness. Every mode uses distinct SQL text to prevent a prepared plan
// created under one planner GUC from leaking into the next mode's comparison.
func exerciseAnalyticsUnionPlans(t *testing.T, db *Database, conn *pgxpool.Conn, matching int64) {
	t.Helper()
	if _, err := conn.Exec(t.Context(), "SET statement_timeout='15s'; SET jit=off; SET max_parallel_workers_per_gather=2; SET plan_cache_mode=force_generic_plan"); err != nil {
		t.Fatal(err)
	}
	for _, snapshot := range []bool{false, true} {
		q := managedQuery(t, "error_category=rate_limit&timezone=Asia%2FShanghai")
		if snapshot {
			q.AsOf, q.SnapshotID = "usage-serial-plan", "usage-serial-plan"
		}
		capture := &analyticsNarrowCapture{pool: db.Pool}
		raw := q
		raw.statsQueryer = capture
		out, err := (&Metrics{db: db}).managedLogStats(t.Context(), raw)
		if err != nil {
			t.Fatal(err)
		}
		fast, fastArgs := managedUsageRollupQuery(q, out)
		statement, args := managedUsageAdaptiveClassSQL(q, fast, fastArgs, capture.sql, capture.args)
		var signature string
		for _, mode := range []string{"union_private_hash", "scalar_private_hash", "union_parallel_hash", "wide_private_hash"} {
			sql, parallelHash := statement, "off"
			if mode == "scalar_private_hash" {
				sql = analyticsScalarBaselineSQL(statement)
			}
			if mode == "wide_private_hash" {
				sql = analyticsWideBaselineSQL(statement)
			}
			if mode == "union_parallel_hash" {
				parallelHash = "on"
			}
			sql = "/* analytics union " + mode + " */ " + sql
			if _, err := conn.Exec(t.Context(), "SET enable_parallel_hash="+parallelHash); err != nil {
				t.Fatal(err)
			}
			name := fmt.Sprintf("analytics_union_%t_%s", snapshot, mode)
			if _, err := conn.Conn().Prepare(t.Context(), name, sql); err != nil {
				t.Fatal(err)
			}
			started := time.Now()
			rows := analyticsQueryRows(t, conn, name, args...)
			elapsed := float64(time.Since(started).Microseconds()) / 1000
			digest := sha256.Sum256([]byte(strings.Join(rows, "\n")))
			got := hex.EncodeToString(digest[:])
			if signature == "" {
				signature = got
			}
			if got != signature {
				t.Fatalf("disjoint union changed aggregate result snapshot=%t mode=%s", snapshot, mode)
			}
			literals := make([]string, len(args))
			for i, value := range args {
				literals[i] = analyticsPlanLiteral(t, value)
			}
			explain := "EXPLAIN (ANALYZE,BUFFERS,SETTINGS,FORMAT JSON) EXECUTE " + pgx.Identifier{name}.Sanitize() + "(" + strings.Join(literals, ",") + ")"
			var plan json.RawMessage
			if err := conn.QueryRow(t.Context(), explain, pgx.QueryExecModeSimpleProtocol).Scan(&plan); err != nil {
				t.Fatal(err)
			}
			var genericPlans, customPlans int64
			if err := conn.QueryRow(t.Context(), "SELECT generic_plans,custom_plans FROM pg_prepared_statements WHERE name=$1", name).Scan(&genericPlans, &customPlans); err != nil || genericPlans != 2 || customPlans != 0 {
				t.Fatalf("query and EXPLAIN did not share the named generic plan: generic=%d custom=%d err=%v", genericPlans, customPlans, err)
			}
			features := analyticsUnionPlanFeatures(plan)
			if mode == "union_private_hash" || mode == "union_parallel_hash" {
				if features["fallback_rows"] != float64(0) {
					t.Fatalf("complete thin projection read original fallback rows: %v", features)
				}
			}
			if parallelHash == "off" && features["parallel_hashes"] != 0 {
				t.Fatalf("private-hash policy still planned shared hash: %v", features)
			}
			performanceSLOEmit(t, "ANALYTICS_UNION_RESULT", map[string]any{"snapshot": snapshot, "mode": mode, "matching": matching, "elapsed_ms": elapsed, "result_rows": len(rows), "signature": got, "features": features, "plan": plan, "sql": sql, "args": args, "prepared_name": name, "generic_plans": genericPlans, "custom_plans": customPlans, "http_slo_proven": false})
		}
	}
}
