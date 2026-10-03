package server

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
)

func assertOperationsRollupSQLParity(t *testing.T, db *Database) {
	t.Helper()
	for _, bounds := range [][3]int64{
		{1800000000, 1800000900, 15}, {1800000007, 1800000907, 15},
		{1800000007, 1800003607, 60}, {1800000007, 1800021607, 360},
		{1800000007, 1800086407, 1440}, {1799999941, 1800000118, 17},
		{1800000001, 1800000010, 1}, {-121, 100, 7}, {1900000000, 1900000900, 15},
	} {
		var differences int
		statement := "WITH expected AS (" + operationsDetailSQL() + "), actual AS (" + operationsRollupSQL() + ") SELECT count(*) FROM ((SELECT * FROM expected EXCEPT SELECT * FROM actual) UNION ALL (SELECT * FROM actual EXCEPT SELECT * FROM expected)) differences"
		if err := db.Pool.QueryRow(t.Context(), statement, bounds[0], bounds[1], bounds[2]).Scan(&differences); err != nil || differences != 0 {
			t.Fatalf("operations summary range %v differences=%d err=%v", bounds, differences, err)
		}
	}
}

func TestPGOperationsRollupMutations(t *testing.T) {
	db := newMetricsTestDB(t)
	m := newTestPGMetrics(t, db)
	for i := 0; i < 90; i++ {
		latency := []int64{-1, 0, 1, 1000, 1001, 5000, 15000, 30000, 60000, 180000, 180001}[i%11]
		ttft, known := int64(i%7-2), i%3 == 0
		r := RequestRecord{EventID: fmt.Sprintf("operations-rollup-%d", i), Time: 1800000000 + int64(i*17), Status: []string{"ok", "error", "aborted"}[i%3], Model: "synthetic", HTTPCode: []int{200, 401, 413, 429, 499, 500, 504}[i%7], LatencyMs: latency, TTFTMs: &ttft, UsageKnown: &known, InputTokens: int64(i), OutputTokens: 3}
		if i%2 == 0 {
			r.LatencyKnown = &known
		}
		m.Record(r)
	}
	if err := m.HealthError(); err != nil {
		t.Fatal(err)
	}
	assertOperationsRollupSQLParity(t, db)
	for _, statement := range []string{
		"UPDATE request_logs SET created_at=created_at+interval '0.75 second' WHERE id%5=0",
		"UPDATE request_logs SET created_at=created_at+interval '71 seconds',status='error',record=record||'{\"latency_ms\":7,\"latency_known\":true,\"http_code\":413,\"input_tokens\":-1}'::jsonb WHERE id%7=0",
		"UPDATE request_logs SET created_at=to_timestamp(-0.25) WHERE id=1",
		"DELETE FROM request_logs WHERE id%3=0",
	} {
		if _, err := db.Pool.Exec(t.Context(), statement); err != nil {
			t.Fatal(err)
		}
		assertOperationsRollupSQLParity(t, db)
	}
	for _, statement := range []string{"DELETE FROM request_logs", "UPDATE request_logs SET created_at=created_at+interval '1 day'", "TRUNCATE request_logs"} {
		err := pgx.BeginFunc(t.Context(), db.Pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(t.Context(), statement); err != nil {
				return err
			}
			return errInvalid
		})
		if err != errInvalid {
			t.Fatal(err)
		}
		assertOperationsRollupSQLParity(t, db)
	}
	if _, err := db.Pool.Exec(t.Context(), "TRUNCATE request_logs"); err != nil {
		t.Fatal(err)
	}
	assertOperationsRollupSQLParity(t, db)
	var n int
	if err := db.Pool.QueryRow(t.Context(), "SELECT count(*) FROM retained_operations").Scan(&n); err != nil || n != 0 {
		t.Fatalf("truncate retained summaries: %d %v", n, err)
	}
}

func TestPGOperationsRollupBackfill(t *testing.T) {
	db := newMetricsTestDB(t)
	m := newTestPGMetrics(t, db)
	for _, fn := range []string{"maintain_retained_operations_insert()", "maintain_retained_operations_update()", "maintain_retained_operations_delete()", "truncate_retained_operations()", "retained_operations_values(text,jsonb)"} {
		if _, err := db.Pool.Exec(t.Context(), "DROP FUNCTION "+fn+" CASCADE"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Pool.Exec(t.Context(), "DROP TABLE retained_operations"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		m.Record(RequestRecord{EventID: fmt.Sprintf("ops-backfill-%d", i), Time: 1800000000 + int64(i*13), Status: "error", HTTPCode: 429, LatencyMs: 25})
	}
	if err := m.HealthError(); err != nil {
		t.Fatal(err)
	}
	raw, err := migrationFiles.ReadFile("011_retained_operations.sql")
	if err != nil {
		t.Fatal(err)
	}
	if err := pgx.BeginFunc(t.Context(), db.Pool, func(tx pgx.Tx) error { _, err := tx.Exec(t.Context(), string(raw)); return err }); err != nil {
		t.Fatal(err)
	}
	assertOperationsRollupSQLParity(t, db)
	m.Record(RequestRecord{EventID: "ops-post-backfill", Time: 1800000009, Status: "ok", InputTokens: 11})
	if err := m.HealthError(); err != nil {
		t.Fatal(err)
	}
	assertOperationsRollupSQLParity(t, db)
}

func TestPGOperationsRollupConcurrentWrites(t *testing.T) {
	db := newMetricsTestDB(t)
	m := newTestPGMetrics(t, db)
	errors := make(chan error, 8)
	for worker := 0; worker < 8; worker++ {
		go func(worker int) {
			for i := 0; i < 8; i++ {
				if _, err := m.persistRecord(context.Background(), RequestRecord{EventID: fmt.Sprintf("ops-concurrent-%d-%d", worker, i), Time: 1800000007 + int64(i), Status: "ok", InputTokens: 1, LatencyMs: 5}); err != nil {
					errors <- err
					return
				}
			}
			errors <- nil
		}(worker)
	}
	for range 8 {
		if err := <-errors; err != nil {
			t.Fatal(err)
		}
	}
	assertOperationsRollupSQLParity(t, db)
	out, err := m.operationsTraffic(t.Context(), 1800000000, 1800000900)
	if err != nil || out.Traffic.Requests != 64 || out.Traffic.Tokens != 64 {
		t.Fatalf("lost operations contribution: %+v %v", out.Traffic, err)
	}
}

func TestPGOperationsRollupDenseWindowPlan(t *testing.T) {
	db := newMetricsTestDB(t)
	seed, err := db.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer seed.Rollback(context.Background())
	if _, err = seed.Exec(t.Context(), "SET LOCAL statement_timeout='60s'"); err != nil {
		t.Fatal(err)
	}
	_, err = seed.Exec(t.Context(), `WITH events AS (
 INSERT INTO usage_events(event_id,created_at,status) SELECT 'ops-dense-'||i,to_timestamp(1800000000+i/1000),'error' FROM generate_series(1,30000)i RETURNING *)
 INSERT INTO request_logs(event_id,created_at,status,model,record) SELECT event_id,created_at,status,'dense',
 jsonb_build_object('time',extract(epoch FROM created_at)::bigint,'event_id',event_id,'status',status,'http_code',429,'latency_ms',100) FROM events`)
	if err != nil {
		t.Fatal(err)
	}
	if err = seed.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Pool.Exec(t.Context(), "ANALYZE request_logs; ANALYZE retained_operations"); err != nil {
		t.Fatal(err)
	}
	conn, err := db.Pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
		if _, err = conn.Exec(t.Context(), "SET jit=off; SET plan_cache_mode="+mode); err != nil {
			t.Fatal(err)
		}
		var plan []byte
		if err = conn.QueryRow(t.Context(), "EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) "+operationsRollupSQL(), int64(1799999997), int64(1800086397), int64(1440)).Scan(&plan); err != nil {
			t.Fatal(err)
		}
		var nodes []struct{ Plan map[string]any }
		if err = json.Unmarshal(plan, &nodes); err != nil {
			t.Fatal(err)
		}
		var walk func(map[string]any)
		walk = func(node map[string]any) {
			if node["Relation Name"] == "request_logs" && node["Actual Loops"].(float64) > 0 {
				if node["Node Type"] == "Seq Scan" || node["Index Name"] != "request_logs_errors_time_idx" || node["Actual Rows"].(float64) > 8 {
					t.Fatalf("%s unbounded details scan: %s", mode, plan)
				}
			}
			if children, ok := node["Plans"].([]any); ok {
				for _, child := range children {
					walk(child.(map[string]any))
				}
			}
		}
		walk(nodes[0].Plan)
		t.Logf("OPERATIONS_ROLLUP_PLAN mode=%s plan=%s", mode, plan)
	}
}
