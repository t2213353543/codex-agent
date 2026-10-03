package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func managedLogPagePlanPool(t *testing.T, db *Database, mode string) (*pgxpool.Pool, *managedUsagePlanTrace) {
	t.Helper()
	config := db.Pool.Config()
	config.MaxConns = 1
	config.MinConns = 0
	config.ConnConfig.RuntimeParams["plan_cache_mode"] = mode
	config.ConnConfig.RuntimeParams["jit"] = "off"
	trace := new(managedUsagePlanTrace)
	config.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool, trace
}

func assertManagedLogPagePlanRestored(t *testing.T, pool *pgxpool.Pool, mode string) {
	t.Helper()
	// A cancelled page query closes its connection; puddle destroys it via a
	// goroutine that must still send a CancelRequest on a fresh socket before
	// removing the resource from the pool. The single-shot assertion races that
	// asynchronous teardown (stable on remote PG, sub-millisecond locally), so
	// poll briefly for the destroy to finish before declaring a leak.
	deadline := time.Now().Add(3 * time.Second)
	for {
		if acquired := pool.Stat().AcquiredConns(); acquired == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("page retained %d pool connections", pool.Stat().AcquiredConns())
		}
		time.Sleep(10 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal("pool did not recover:", err)
	}
	defer conn.Release()
	var got, readOnly string
	if err := conn.QueryRow(ctx, "SELECT current_setting('plan_cache_mode'),current_setting('transaction_read_only')").Scan(&got, &readOnly); err != nil {
		t.Fatal(err)
	}
	if got != mode || readOnly != "off" || conn.Conn().PgConn().TxStatus() != 'I' {
		t.Fatalf("transaction state escaped page: mode=%s read_only=%s status=%c", got, readOnly, conn.Conn().PgConn().TxStatus())
	}
}

func managedLogPagePreparedCounts(t *testing.T, pool *pgxpool.Pool, statement string) (custom, generic int64) {
	t.Helper()
	if err := pool.QueryRow(t.Context(), "SELECT custom_plans,generic_plans FROM pg_prepared_statements WHERE statement=$1", statement).Scan(&custom, &generic); err != nil {
		t.Fatal("actual pgx prepared page statement not found:", err)
	}
	return custom, generic
}

func warmManagedLogPageCustomPlan(t *testing.T, pool *pgxpool.Pool, trace *managedUsagePlanTrace, mode, raw string) ([]float64, string, []any) {
	t.Helper()
	m := &Metrics{db: &Database{Pool: pool}}
	q := managedQuery(t, raw)
	var samples []float64
	var statement string
	var args []any
	for i := range 8 {
		started := time.Now()
		page, err := m.managedLogs(t.Context(), q)
		if err != nil {
			t.Fatalf("warm request %d failed: %v", i+1, err)
		}
		samples = append(samples, float64(time.Since(started).Microseconds())/1000)
		if strings.Contains(q.Model, "missing") && len(page.Items) != 0 {
			t.Fatal("missing-model page was not empty")
		}
		if !strings.Contains(q.Model, "missing") && len(page.Items) == 0 {
			t.Fatal("synthetic existing-model/model-sort page was empty")
		}
		if len(page.Items) > q.Size {
			t.Fatal("lookahead escaped public page")
		}
		trace.mu.Lock()
		statement, args = trace.sql, append([]any{}, trace.args...)
		trace.mu.Unlock()
		assertManagedLogPagePlanRestored(t, pool, mode)
		if i == 5 || i == 7 {
			custom, generic := managedLogPagePreparedCounts(t, pool, statement)
			if custom < int64(i+1) || generic != 0 {
				t.Fatalf("warm request %d used generic page plan: custom=%d generic=%d", i+1, custom, generic)
			}
		}
	}
	return samples, statement, args
}

func TestPGManagedLogPageCustomPlanWarmupAndParity(t *testing.T) {
	db := newMetricsTestDB(t)
	modelPayload := "aaa-'; SET plan_cache_mode=force_generic_plan; --"
	_, err := db.Pool.Exec(t.Context(), `WITH fixture AS (
 SELECT i,'page-plan-'||i AS event_id,to_timestamp(1800000000+i/3) AS created_at,
 CASE WHEN i=1 THEN $1 WHEN i%8=0 THEN repeat('z',1801) WHEN i%3=0 THEN 'model-b' ELSE 'model-a' END AS model
 FROM generate_series(1,48)i
 ), events AS (
 INSERT INTO usage_events(event_id,created_at,status) SELECT event_id,created_at,'ok' FROM fixture RETURNING event_id
 ) INSERT INTO request_logs(event_id,created_at,status,model,record)
 SELECT f.event_id,f.created_at,'ok',f.model,jsonb_build_object('event_id',f.event_id,'time',extract(epoch FROM f.created_at)::bigint,'status','ok','model',f.model,'input_tokens',f.i)
 FROM fixture f JOIN events USING(event_id)`, modelPayload)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Pool.Exec(t.Context(), "ANALYZE request_logs"); err != nil {
		t.Fatal(err)
	}
	rows, err := db.Pool.Query(t.Context(), "SELECT id,record FROM request_logs")
	if err != nil {
		t.Fatal(err)
	}
	var fixture []RequestRecord
	for rows.Next() {
		var id int64
		var raw []byte
		if err := rows.Scan(&id, &raw); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		var record RequestRecord
		if err := json.Unmarshal(raw, &record); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		record.ID = id
		fixture = append(fixture, record)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"auto", "force_generic_plan"} {
		t.Run(mode, func(t *testing.T) {
			pool, trace := managedLogPagePlanPool(t, db, mode)
			for _, raw := range []string{"model=missing-synthetic", "model=model-a", "sort_by=model", "sort_by=model&sort_order=asc"} {
				warmManagedLogPageCustomPlan(t, pool, trace, mode, raw)
			}
			m := &Metrics{db: &Database{Pool: pool}}
			for _, raw := range []string{"model=model-a", "model=model-a&sort_by=tokens", "sort_by=model&sort_order=asc", "sort_by=model&sort_order=desc", "model=" + url.QueryEscape(modelPayload)} {
				q := managedQuery(t, raw+"&size=3")
				var want []int64
				ordered := append([]RequestRecord{}, fixture...)
				sort.Slice(ordered, func(i, j int) bool { return q.before(ordered[i], ordered[j]) })
				for _, record := range ordered {
					if q.matches(record) {
						want = append(want, record.ID)
					}
				}
				var got []int64
				for {
					page, err := m.managedLogs(t.Context(), q)
					if err != nil {
						t.Fatalf("cursor %s: %v", raw, err)
					}
					for _, record := range page.Items {
						got = append(got, record.ID)
					}
					assertManagedLogPagePlanRestored(t, pool, mode)
					if !page.HasMore {
						break
					}
					if len(got) > len(want) || page.NextCursor == "" {
						t.Fatal("cursor did not progress")
					}
					q.Cursor = page.NextCursor
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("cursor parity %s: got %v want %v", raw, got, want)
				}
				q.Cursor = ""
				q.Page = 2
				page, err := m.managedLogs(t.Context(), q)
				if err != nil {
					t.Fatal(err)
				}
				var numbered []int64
				for _, record := range page.Items {
					numbered = append(numbered, record.ID)
				}
				var numberedWant []int64
				if len(want) > q.Size {
					numberedWant = want[q.Size:min(q.Size*2, len(want))]
				}
				if !reflect.DeepEqual(numbered, numberedWant) {
					t.Fatalf("numbered parity %s: got %v want %v", raw, numbered, numberedWant)
				}
				assertManagedLogPagePlanRestored(t, pool, mode)
			}
		})
	}
}

func TestPGManagedLogPageCustomPlanReadOnlyAndRestoration(t *testing.T) {
	db := newMetricsTestDB(t)
	for _, mode := range []string{"auto", "force_generic_plan"} {
		t.Run(mode, func(t *testing.T) {
			pool, _ := managedLogPagePlanPool(t, db, mode)
			for _, q := range []managedLogQuery{{Model: "synthetic"}, {SortBy: "model"}, {SortBy: "time"}, {SortBy: "tokens"}} {
				rows, err := queryManagedLogPage(t.Context(), pool, q, "SELECT current_setting('plan_cache_mode'),current_setting('transaction_read_only')")
				if err != nil {
					t.Fatal(err)
				}
				var got, readOnly string
				if !rows.Next() {
					rows.Close()
					t.Fatalf("no transaction settings: %v", rows.Err())
				}
				if err := rows.Scan(&got, &readOnly); err != nil {
					rows.Close()
					t.Fatal(err)
				}
				rows.Close()
				wantMode, wantReadOnly := mode, "off"
				if q.Model != "" || q.SortBy == "model" {
					wantMode, wantReadOnly = "force_custom_plan", "on"
				}
				if got != wantMode || readOnly != wantReadOnly {
					t.Fatalf("wrong query-local policy: mode=%s read_only=%s", got, readOnly)
				}
				assertManagedLogPagePlanRestored(t, pool, mode)
			}
		})
	}
}
func TestPGManagedLogPageCustomPlanCancellationRecovery(t *testing.T) {
	db := newMetricsTestDB(t)
	pool, trace := managedLogPagePlanPool(t, db, "force_generic_plan")
	assertManagedLogPagePlanRestored(t, pool, "force_generic_plan")
	blocker, err := db.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(context.Background())
	if _, err = blocker.Exec(t.Context(), "LOCK request_logs IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	rows, err := queryManagedLogPage(ctx, pool, managedLogQuery{Model: "synthetic"}, "SELECT id,record FROM request_logs WHERE model=$1", "synthetic")
	if rows != nil {
		rows.Close()
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("blocked page lost cancellation: %v", err)
	}
	trace.mu.Lock()
	blockedSQL := trace.sql
	trace.mu.Unlock()
	if !strings.HasPrefix(blockedSQL, "SELECT id,record FROM request_logs WHERE model=") {
		t.Fatal("cancellation occurred before the page SELECT began")
	}
	if err := blocker.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertManagedLogPagePlanRestored(t, pool, "force_generic_plan")
	for _, statement := range []string{"SELECT no_such_page_column FROM request_logs", "SELECT id,record FROM request_logs WHERE model=$1"} {
		var args []any
		if strings.Contains(statement, "$1") {
			args = []any{"synthetic"}
		}
		rows, err = queryManagedLogPage(t.Context(), pool, managedLogQuery{SortBy: "model"}, statement, args...)
		if strings.Contains(statement, "no_such") {
			if err == nil {
				rows.Close()
				t.Fatal("expected query error")
			}
		} else {
			if err != nil {
				t.Fatal(err)
			}
			for rows.Next() {
			}
			rows.Close()
			if err = rows.Err(); err != nil {
				t.Fatal(err)
			}
		}
		assertManagedLogPagePlanRestored(t, pool, "force_generic_plan")
	}
}

// This opt-in diagnostic only reads the explicitly retained synthetic fixture.
// It verifies actual cached statement counters after the sixth and eighth real
// page requests, not an uncached EXPLAIN masquerading as a warm-plan check.
func TestPerformanceManagedLogPageCustomPlan(t *testing.T) {
	schema, dsn := os.Getenv("PERFORMANCE_SLO_SCHEMA"), os.Getenv("TEST_DATABASE_URL")
	if os.Getenv("PERFORMANCE_SLO") != "1" || !regexp.MustCompile(`^perf_slo_[a-z0-9_]{8,60}$`).MatchString(schema) || dsn == "" {
		t.Skip("retained synthetic performance fixture is required")
	}
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	config.MaxConns = 1
	config.ConnConfig.RuntimeParams["search_path"] = schema
	config.ConnConfig.RuntimeParams["statement_timeout"] = "15000"
	base, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close()
	for _, mode := range []string{"auto", "force_generic_plan"} {
		t.Run(mode, func(t *testing.T) {
			pool, trace := managedLogPagePlanPool(t, &Database{Pool: base}, mode)
			for _, raw := range []string{"model=missing-synthetic", "sort_by=model"} {
				samples, statement, args := warmManagedLogPageCustomPlan(t, pool, trace, mode, raw)
				custom, generic := managedLogPagePreparedCounts(t, pool, statement)
				q := managedQuery(t, raw)
				rows, err := queryManagedLogPage(t.Context(), pool, q, "EXPLAIN (ANALYZE,BUFFERS,SETTINGS,FORMAT JSON) "+statement, args...)
				if err != nil {
					t.Fatal(err)
				}
				var plan json.RawMessage
				if !rows.Next() {
					rows.Close()
					t.Fatalf("missing plan: %v", rows.Err())
				}
				if err = rows.Scan(&plan); err != nil {
					rows.Close()
					t.Fatal(err)
				}
				rows.Close()
				if err = rows.Err(); err != nil {
					t.Fatal(err)
				}
				assertManagedLogPagePlanRestored(t, pool, mode)
				evidence, _ := json.Marshal(map[string]any{"mode": mode, "filter": raw, "samples_ms": samples, "custom_plans": custom, "generic_plans": generic, "sql": statement, "args": args, "plan": plan, "scope": "real method warm-cache counters; captured SQL and args explained with identical transaction-local custom-plan policy; synthetic read-only fixture", "slo_proven": false})
				t.Logf("MANAGED_LOG_PAGE_CUSTOM_PLAN %s", evidence)
			}
		})
	}

}
