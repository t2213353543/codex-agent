package server

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// Function results are already persisted in the shipped 020 GIN index. The
// faster implementation must exactly preserve them, including legacy shapes.
func TestPGErrorClassDecodedFunctionPreservesOriginalIndexValues(t *testing.T) {
	db := newMetricsTestDB(t)
	raw, err := migrationFiles.ReadFile("019_request_log_error_classes_function.sql")
	if err != nil {
		t.Fatal(err)
	}
	legacy := strings.Replace(string(raw), "CREATE FUNCTION request_log_error_classes(", "CREATE FUNCTION legacy_error_classes(", 1)
	if _, err := db.Pool.Exec(t.Context(), legacy); err != nil {
		t.Fatal(err)
	}
	records, _ := managedSeverityContractRows()
	for _, code := range []string{"rateLimitExceeded", "usageLimitExceeded", "unauthorized", "authentication_error", "serverOverloaded", "unmatched"} {
		for _, source := range []string{"", "gateway", "upstream"} {
			for _, status := range []string{"ok", "error", "aborted"} {
				records = append(records, RequestRecord{Status: status, HTTPCode: 200, Error: "unsupported parameter lower-priority hint", ErrorSource: source, FinalUpstreamErrorCode: code, FinalUpstreamErrorSource: "upstream"})
			}
		}
	}
	encoded, _ := json.Marshal(records)
	var mismatch int
	if err := db.Pool.QueryRow(t.Context(), `SELECT count(*) FROM jsonb_array_elements($1::jsonb) row(record)
 WHERE request_log_error_classes(record,record->>'status') IS DISTINCT FROM legacy_error_classes(record,record->>'status')`, encoded).Scan(&mismatch); err != nil || mismatch != 0 {
		t.Fatalf("classification matrix changed %d indexed values: %v", mismatch, err)
	}
	for _, record := range []string{`{}`, `null`, `[]`, `42`, `true`, `"rate limit"`, `{"http_code":null,"error":null}`, `{"http_code":"429","error":["rate limit"]}`, `{"error":{"message":"unsupported parameter"}}`, `{"http_code":"00429","error":false}`, `{"http_code":"+429","error":"unknown"}`, `{"http_code":"-1"}`} {
		for _, status := range []any{"ok", "error", "aborted", "", nil} {
			var equal bool
			if err := db.Pool.QueryRow(t.Context(), `SELECT request_log_error_classes($1::jsonb,$2::text) IS NOT DISTINCT FROM legacy_error_classes($1::jsonb,$2::text)`, record, status).Scan(&equal); err != nil || !equal {
				t.Fatalf("legacy shape changed record=%s status=%v: %v", record, status, err)
			}
		}
	}
	// Invalid numeric metadata was never parsed in the status=ok CASE branch.
	var equal bool
	if err := db.Pool.QueryRow(t.Context(), `SELECT request_log_error_classes('{"http_code":"invalid"}','ok')=legacy_error_classes('{"http_code":"invalid"}','ok')`).Scan(&equal); err != nil || !equal {
		t.Fatalf("successful record changed lazy-cast contract: %v", err)
	}
}

func TestPGErrorClassSnapshotWideMajorityUsesScalarIndex(t *testing.T) {
	p, handler := adminPGTestProxy(t)
	tx, err := p.Store.DB.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if _, err := tx.Exec(t.Context(), "SET LOCAL statement_timeout='120s'"); err != nil {
		t.Fatal(err)
	}
	// Dense errors match the production distribution instead of assuming that
	// a class filter selects a tiny minority. A wide unrelated field exposes
	// unnecessary JSON evaluation in the snapshot's ID-only query.
	_, err = tx.Exec(t.Context(), `WITH e AS (
 INSERT INTO usage_events(event_id,created_at,status) SELECT 'scalar-wide-'||i,to_timestamp(1790553600+i),'error' FROM generate_series(1,600)i RETURNING *
 ) INSERT INTO request_logs(event_id,created_at,status,model,record)
 SELECT event_id,created_at,status,'scalar-wide',jsonb_build_object('event_id',event_id,'time',extract(epoch FROM created_at)::bigint,
 'status',status,'model','scalar-wide','http_code',CASE WHEN substring(event_id from 13)::int%2=0 THEN 429 ELSE 403 END,
 'error',CASE WHEN substring(event_id from 13)::int%2=0 THEN 'rate limit exceeded' ELSE 'unauthorized' END,
 'unrelated_protocol_payload',repeat('large unobserved protocol payload ',3000)) FROM e`)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	filters := map[string]string{"error_category": "rate_limit"}
	response := managedTestRequest(t, handler, "POST", "/api/logs/snapshot", map[string]any{"filters": filters}, 200)
	var snapshot managedUsageSnapshot
	if err := json.Unmarshal(response.Body.Bytes(), &snapshot); err != nil || snapshot.Total != 300 {
		t.Fatalf("dense class snapshot: %s %v", response.Body.String(), err)
	}
	response = managedTestRequest(t, handler, "GET", "/api/logs/stats?error_category=rate_limit&as_of="+snapshot.ID, nil, 200)
	var stats managedUsageStats
	if err := json.Unmarshal(response.Body.Bytes(), &stats); err != nil || stats.Requests != 300 {
		t.Fatalf("dense class snapshot stats: %s %v", response.Body.String(), err)
	}
	q := managedQuery(t, "error_category=rate_limit")
	where, args := q.whereSQL()
	conn, err := p.Store.DB.Pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(t.Context(), "SET enable_seqscan=off"); err != nil {
		t.Fatal(err)
	}
	var plan []byte
	if err := conn.QueryRow(t.Context(), "EXPLAIN (ANALYZE,FORMAT JSON) SELECT id FROM request_logs WHERE "+where+" ORDER BY created_at DESC,id DESC", args...).Scan(&plan); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(plan), "request_logs_error_category_time_idx") || strings.Contains(string(plan), `"Filter"`) {
		t.Fatalf("class scan re-evaluates wide JSON instead of exact scalar index: %s", plan)
	}
	// EXPLAIN prints Recheck Cond even for an exact BTree bitmap. It is only
	// evaluated on lossy heap blocks; the exact production/test plan must not
	// be rejected merely because PostgreSQL reports the dormant expression.
	var decoded []struct{ Plan map[string]any }
	if err := json.Unmarshal(plan, &decoded); err != nil || len(decoded) != 1 {
		t.Fatalf("invalid scalar index plan: %s %v", plan, err)
	}
	var inspect func(map[string]any)
	inspect = func(node map[string]any) {
		if lossy, ok := node["Lossy Heap Blocks"].(float64); ok && lossy != 0 {
			t.Fatalf("scalar class bitmap rechecks wide JSON on lossy pages: %s", plan)
		}
		if children, ok := node["Plans"].([]any); ok {
			for _, child := range children {
				inspect(child.(map[string]any))
			}
		}
	}
	inspect(decoded[0].Plan)
	t.Log(fmt.Sprintf("dense class snapshot retained=%d scalar_plan=%s", stats.Requests, plan))
}
