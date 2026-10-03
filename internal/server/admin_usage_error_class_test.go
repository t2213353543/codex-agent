package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// Exact scalar indexes avoid a GIN classification recheck on every matched
// wide JSON record. Pin ordered list plans to these expression keys.
func TestPGErrorClassFilterUsesScalarIndexes(t *testing.T) {
	db := newMetricsTestDB(t)
	m := newTestPGMetrics(t, db)
	ctx := context.Background()
	seed, err := db.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer seed.Rollback(ctx)
	if _, err = seed.Exec(ctx, "SET LOCAL statement_timeout='60s'"); err != nil {
		t.Fatal(err)
	}
	_, err = seed.Exec(ctx, `WITH events AS (
 SELECT 'errclass-'||i AS event_id, to_timestamp(1700000000 + i/3) AS created_at,
 (CASE i%20 WHEN 0 THEN 'timeout' WHEN 1 THEN 'rate_limit' WHEN 2 THEN 'rate_limit'
  WHEN 3 THEN 'authentication' WHEN 4 THEN 'authentication' WHEN 5 THEN 'server_error'
  WHEN 6 THEN 'server_error' WHEN 7 THEN 'aborted' WHEN 8 THEN 'aborted' WHEN 9 THEN 'overloaded'
  WHEN 10 THEN 'overloaded' ELSE 'unknown' END) AS category
 FROM generate_series(1,30000) i
), classified AS (
 SELECT *, CASE category
  WHEN 'timeout' THEN 504 WHEN 'rate_limit' THEN 429 WHEN 'authentication' THEN 401
  WHEN 'server_error' THEN 500 WHEN 'aborted' THEN 499 WHEN 'overloaded' THEN 503 ELSE 200 END AS http_code,
 CASE category
  WHEN 'timeout' THEN 'upstream request timed out'
  WHEN 'rate_limit' THEN 'rate limit exceeded'
  WHEN 'authentication' THEN 'invalid api key'
  WHEN 'server_error' THEN 'internal server error'
  WHEN 'aborted' THEN 'context canceled'
  WHEN 'overloaded' THEN 'server is overloaded'
  ELSE 'unexpected failure' END AS message
 FROM events
), seeded AS (
 INSERT INTO usage_events(event_id,created_at,status)
 SELECT event_id,created_at,(CASE WHEN category='aborted' THEN 'aborted' ELSE 'error' END) FROM classified
 RETURNING event_id
)
INSERT INTO request_logs(event_id,created_at,status,model,account_name,key_name,record)
SELECT event_id,created_at,(CASE WHEN category='aborted' THEN 'aborted' ELSE 'error' END),'m','a','k',
 jsonb_build_object('event_id',event_id,'time',extract(epoch FROM created_at)::bigint,'error',message,'http_code',http_code,'model','m')
FROM classified`)
	if err != nil {
		t.Fatal(err)
	}
	if err := seed.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := m.HealthError(); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, "ANALYZE request_logs"); err != nil {
		t.Fatal(err)
	}
	indexNames := func(t *testing.T, statement string, args ...any) []string {
		t.Helper()
		var raw []byte
		if err := db.Pool.QueryRow(ctx, statement, args...).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var plans []struct {
			Plan map[string]any `json:"Plan"`
		}
		if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
			t.Fatal("invalid plan", err)
		}
		var indexes []string
		var visit func(map[string]any)
		visit = func(plan map[string]any) {
			if index, ok := plan["Index Name"].(string); ok {
				indexes = append(indexes, index)
			}
			if children, ok := plan["Plans"].([]any); ok {
				for _, child := range children {
					visit(child.(map[string]any))
				}
			}
		}
		visit(plans[0].Plan)
		return indexes
	}
	has := func(names []string, want string) bool {
		for _, name := range names {
			if name == want {
				return true
			}
		}
		return false
	}
	// The real planner must prefer the ordered scalar index for class pages.
	for _, filter := range []string{"error_category=timeout", "error_category=authentication"} {
		q := managedQuery(t, filter)
		where, args := q.whereSQL()
		args = append(args, 51)
		names := indexNames(t, "EXPLAIN (FORMAT JSON) SELECT id,record FROM request_logs WHERE "+where+fmt.Sprintf(" ORDER BY created_at DESC,id DESC LIMIT $%d", len(args)), args...)
		if !has(names, "request_logs_error_category_time_idx") {
			t.Fatalf("real plan for %q lacks the error-category scalar index: %v", filter, names)
		}
	}
	// Scalar expressions must remain indexable for broad severity/category
	// pages too, without evaluating an immutable JSON projection as a filter.
	conn, err := db.Pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SET enable_seqscan=off"); err != nil {
		t.Fatal(err)
	}
	for _, filter := range []string{"error_severity=P2", "error_category=unknown"} {
		q := managedQuery(t, filter)
		where, args := q.whereSQL()
		args = append(args, 51)
		statement := "EXPLAIN (FORMAT JSON) SELECT id,record FROM request_logs WHERE " + where + fmt.Sprintf(" ORDER BY created_at DESC,id DESC LIMIT $%d", len(args))
		var raw []byte
		if err := conn.QueryRow(ctx, statement, args...).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		expected := "request_logs_error_category_time_idx"
		if q.ErrorSeverity != "" {
			expected = "request_logs_error_severity_time_idx"
		}
		if !strings.Contains(string(raw), expected) {
			t.Fatalf("index plan for %q lacks scalar index %s: %s", filter, expected, raw)
		}
	}
}

// The dimension-backed exact total must equal a detail count over the same
// snapshot, including edge minutes, unbounded windows and unsafe minutes.
func TestPGErrorClassRetainedCountParity(t *testing.T) {
	db := newMetricsTestDB(t)
	m := newTestPGMetrics(t, db)
	const base int64 = 1700000000
	timeout := func(id string, stamp int64, keyName string) RequestRecord {
		return RequestRecord{EventID: id, Time: stamp, AccountID: "a", Account: "a", KeyID: "k", KeyName: keyName, Model: "m", Status: "error", HTTPCode: 504, Error: "request timed out"}
	}
	seed := []RequestRecord{
		timeout("t1", base+10, "k"),
		timeout("t2", base+70, "k"),
		{EventID: "r1", Time: base + 20, AccountID: "a", Account: "a", KeyID: "k", KeyName: "k", Model: "m", Status: "error", HTTPCode: 429, Error: "rate limit exceeded"},
		{EventID: "a1", Time: base + 130, AccountID: "a", Account: "a", KeyID: "k", KeyName: "k", Model: "m", Status: "error", HTTPCode: 401, Error: "bad key"},
		{EventID: "ok1", Time: base + 30, AccountID: "a", Account: "a", KeyID: "k", KeyName: "k", Model: "m", Status: "ok", HTTPCode: 200},
		// An oversized key name makes this minute unsafe: its dimensions are
		// not retained, so the count must read it from details.
		timeout("u1", base+300, strings.Repeat("k", 1700)),
	}
	for _, r := range seed {
		m.Record(r)
	}
	if err := m.HealthError(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	var ceiling int64
	if err := db.Pool.QueryRow(ctx, "SELECT COALESCE(max(id),0) FROM request_logs").Scan(&ceiling); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{
		"error_category=timeout",
		"error_category=timeout&status=failed",
		"error_category=rate_limit&status=failed",
		"error_severity=P1&status=failed",
		"error_severity=P3",
		"error_category=timeout&from=" + fmt.Sprint(base) + "&to=" + fmt.Sprint(base+400),
		"error_severity=P2&status=failed",
		"error_category=timeout&from=" + fmt.Sprint(base+1) + "&to=" + fmt.Sprint(base+59),
	} {
		q := managedQuery(t, raw)
		statement, args, ok := managedRetainedDimensionCountSQL(q, ceiling)
		where, wargs := q.whereSQL()
		wargs = append(wargs, ceiling)
		var want int64
		if err := db.Pool.QueryRow(ctx, fmt.Sprintf("SELECT count(*) FROM request_logs WHERE %s AND id<=$%d", where, len(wargs)), wargs...).Scan(&want); err != nil {
			t.Fatal(err)
		}
		if !ok {
			continue // The caller falls back to the same detail count above.
		}
		var got int64
		if err := db.Pool.QueryRow(ctx, statement, args...).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("%q: rollup count %d != detail count %d", raw, got, want)
		}
	}
	// Filters that cannot use a single dimension stay on the detail path.
	for _, raw := range []string{
		"error_category=timeout&status=ok",
		"error_category=timeout&error_severity=P1",
		"error_category=timeout&model=m",
	} {
		if _, _, ok := managedRetainedDimensionCountSQL(managedQuery(t, raw), ceiling); ok {
			t.Fatalf("unexpectedly eligible: %s", raw)
		}
	}
	// The errors-view endpoint must observe the same total through its exact
	// count path, including the unsafe minute.
	q := managedQuery(t, "error_category=timeout&status=failed&exact_total=true&page_size=10")
	page, err := m.managedLogs(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 3 {
		t.Fatalf("exact total through managedLogs: %d, want 3", page.Total)
	}
}
