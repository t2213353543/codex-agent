package server

import (
	"fmt"
	"testing"
)

func TestManagedRetainedCountFilterCoverage(t *testing.T) {
	for _, raw := range []string{"", "status=ok", "status=failed", "from=1700000001&to=1700000999&status=failed"} {
		if column := managedRetainedCountColumn(managedQuery(t, raw)); column == "" {
			t.Fatalf("supported count did not use rollup: %s", raw)
		}
	}
	for _, raw := range []string{"status=error", "status=aborted", "model=m", "request_id=1", "account_id=a", "key_id=k", "http_code=429", "group_id=g", "endpoint=responses", "stream=true", "websocket_warmup=false", "client_request_id=abc", "error_category=timeout"} {
		if column := managedRetainedCountColumn(managedQuery(t, raw)); column != "" {
			t.Fatalf("unsupported filter would be ignored: %s -> %s", raw, column)
		}
	}
	q := managedQuery(t, "")
	q.SnapshotID = "saved"
	if managedRetainedCountColumn(q) != "" {
		t.Fatal("snapshot membership ignored")
	}
}

func TestPGManagedRetainedCountParity(t *testing.T) {
	db := newMetricsTestDB(t)
	m := newTestPGMetrics(t, db)
	const base int64 = 1800000000
	for i := 0; i < 130; i++ {
		m.Record(RequestRecord{EventID: fmt.Sprintf("rollup-count-%d", i), Time: base + int64(i) - 5, Status: []string{"ok", "error", "aborted"}[i%3], Model: "synthetic", InputTokens: 1})
	}
	if err := m.HealthError(); err != nil {
		t.Fatal(err)
	}
	var ceiling int64
	if err := db.Pool.QueryRow(t.Context(), "SELECT max(id) FROM request_logs").Scan(&ceiling); err != nil {
		t.Fatal(err)
	}
	// New IDs can belong to any timestamp, including the middle of a page's
	// time interval. Counts must exclude them without dropping older IDs.
	for i, at := range []int64{base - 3600, base + 30, base + 3600} {
		m.Record(RequestRecord{EventID: fmt.Sprintf("rollup-new-%d", i), Time: at, Status: "error"})
	}
	if _, err := db.Pool.Exec(t.Context(), "UPDATE request_logs SET created_at=created_at+interval '0.9 second' WHERE id=1"); err != nil {
		t.Fatal(err)
	}
	for pass := 0; pass < 2; pass++ {
		for _, status := range []string{"", "ok", "failed", "error", "aborted"} {
			for _, bounds := range [][2]int64{{0, 0}, {0, base + 1}, {base, 0}, {base, base + 120}, {base + 1, base + 119}, {base + 3, base + 7}, {base - 3600, base - 1800}} {
				q := managedQuery(t, fmt.Sprintf("status=%s", status))
				q.From, q.To = bounds[0], bounds[1]
				where, args := q.whereSQL()
				args = append(args, ceiling)
				where += fmt.Sprintf(" AND id<=$%d", len(args))
				var want int64
				if err := db.Pool.QueryRow(t.Context(), "SELECT count(*) FROM request_logs WHERE "+where, args...).Scan(&want); err != nil {
					t.Fatal(err)
				}
				got, err := m.managedRetainedCount(t.Context(), q, ceiling, where, args)
				if err != nil || got != want {
					t.Fatalf("pass=%d status=%s range=%v count=%d want=%d err=%v", pass, status, bounds, got, want, err)
				}
			}
		}
		if pass == 0 {
			if _, err := db.Pool.Exec(t.Context(), "DELETE FROM request_logs WHERE id%4=0"); err != nil {
				t.Fatal(err)
			}
		}
	}
}
