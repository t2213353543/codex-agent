package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Capture the statement executed by the real read methods, so this test cannot
// accidentally validate an index-friendly SQL copy while the endpoint regresses.
type managedUsagePlanTrace struct {
	mu   sync.Mutex
	sql  string
	args []any
}

func (tr *managedUsagePlanTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.HasPrefix(data.SQL, "SELECT id,record FROM ") {
		tr.mu.Lock()
		tr.sql, tr.args = data.SQL, append([]any(nil), data.Args...)
		tr.mu.Unlock()
	}
	return ctx
}
func (*managedUsagePlanTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestPGManagedUsageReadIndexPlans(t *testing.T) {
	db := newMetricsTestDB(t)
	ctx := context.Background()
	seed, err := db.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer seed.Rollback(ctx)
	// Building tens of thousands of fully indexed fixture rows is maintenance,
	// not the read being tested. Keep the normal read deadline after commit.
	if _, err = seed.Exec(ctx, "SET LOCAL statement_timeout='90s'"); err != nil {
		t.Fatal(err)
	}
	_, err = seed.Exec(ctx, `WITH events AS (
        INSERT INTO usage_events(event_id,created_at,status)
        SELECT 'detail-plan-'||i,to_timestamp(1700000000+i/3),'ok' FROM generate_series(1,30000)i
        RETURNING event_id,created_at,status
    ) INSERT INTO request_logs(event_id,created_at,status,model,record)
    SELECT event_id,created_at,status,'synthetic',jsonb_build_object('event_id',event_id,'time',extract(epoch FROM created_at)::bigint,'status',status,'model','synthetic',
        'request_id','request-'||event_id,'response_id','response-'||event_id,'client_request_id','client-'||event_id,
        'input_tokens',substring(event_id from 13)::bigint,'latency_ms',substring(event_id from 13)::bigint%1000) FROM events`)
	if err != nil {
		t.Fatal(err)
	}
	if err := seed.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Pool.Exec(ctx, "ANALYZE request_logs"); err != nil {
		t.Fatal(err)
	}
	trace := &managedUsagePlanTrace{}
	config := db.Pool.Config()
	config.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	m := &Metrics{db: &Database{Pool: pool}}
	explain := func(t *testing.T, label, statement string, args []any, requiredIndexes ...string) {
		t.Helper()
		var raw []byte
		if err := db.Pool.QueryRow(ctx, "EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) "+statement, args...).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var plans []struct {
			Plan map[string]any `json:"Plan"`
		}
		if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
			t.Fatal("invalid plan", err)
		}
		var indexes []string
		fullScan := false
		var walk func(map[string]any)
		walk = func(node map[string]any) {
			if node["Node Type"] == "Seq Scan" && node["Relation Name"] == "request_logs" {
				fullScan = true
			}
			if index, ok := node["Index Name"].(string); ok {
				indexes = append(indexes, index)
			}
			if children, ok := node["Plans"].([]any); ok {
				for _, child := range children {
					walk(child.(map[string]any))
				}
			}
		}
		walk(plans[0].Plan)
		// Retain the complete plan (actual rows, filters and buffers) as raw
		// evidence. Its single execution time is not a performance benchmark.
		compact := json.RawMessage(raw)
		evidence, _ := json.Marshal(map[string]any{"case": label, "sql": statement, "args": args, "plan": compact})
		t.Logf("SYSTEM_PLAN %s", evidence)
		for _, index := range requiredIndexes {
			if !stringIn(index, indexes...) {
				t.Fatalf("%s missing index %s: %s", label, index, raw)
			}
		}
		if len(requiredIndexes) > 0 && fullScan {
			t.Fatalf("%s scanned the full request_logs table: %s", label, raw)
		}
	}
	for _, id := range []string{"detail-plan-15000", "15000"} {
		t.Run("detail_"+id, func(t *testing.T) {
			if _, err := m.managedLogDetail(ctx, id); err != nil {
				t.Fatal(err)
			}
			trace.mu.Lock()
			statement, args := trace.sql, append([]any(nil), trace.args...)
			trace.mu.Unlock()
			if statement == "" {
				t.Fatal("real detail statement not captured")
			}
			required := []string{"request_logs_event_id_key"}
			if id == "15000" {
				required = append(required, "request_logs_pkey")
			}
			explain(t, "current_detail_"+id, statement, args, required...)
			explain(t, "baseline_detail_"+id, "SELECT id,record FROM request_logs WHERE id::text=$1 OR event_id=$1 ORDER BY id DESC LIMIT 1", []any{id})
		})
	}
	for _, direction := range []string{"asc", "desc"} {
		q := managedQuery(t, "page=100&size=50&sort_by=time&sort_order="+direction)
		page, err := m.managedLogs(ctx, q)
		if err != nil || len(page.Items) != 50 {
			t.Fatalf("page result: %d %v", len(page.Items), err)
		}
		trace.mu.Lock()
		statement, args := trace.sql, append([]any(nil), trace.args...)
		trace.mu.Unlock()
		if !strings.Contains(statement, " OFFSET ") {
			t.Fatal("real numbered-page statement not captured")
		}
		explain(t, fmt.Sprintf("current_numbered_page100_%s", direction), statement, args, "request_logs_time_idx")
	}
	for _, spec := range []struct {
		label, query string
		indexes      []string
	}{
		{"request_id", "request_id=request-detail-plan-15000", []string{"request_logs_event_id_key", "request_logs_request_id_idx", "request_logs_response_id_idx"}},
		{"numeric_request_id", "request_id=15000", []string{"request_logs_pkey", "request_logs_event_id_key", "request_logs_request_id_idx", "request_logs_response_id_idx"}},
		{"client_request_id", "client_request_id=client-detail-plan-15000", []string{"request_logs_client_request_id_idx"}},
		{"tokens_rank", "size=50&page=100&sort_by=tokens", []string{"request_logs_tokens_time_idx"}},
		{"latency_rank", "size=50&page=100&sort_by=latency", []string{"request_logs_latency_time_idx"}},
	} {
		t.Run(spec.label, func(t *testing.T) {
			page, err := m.managedLogs(ctx, managedQuery(t, spec.query))
			if err != nil || len(page.Items) == 0 {
				t.Fatalf("indexed query returned no rows: %v", err)
			}
			trace.mu.Lock()
			statement, args := trace.sql, append([]any(nil), trace.args...)
			trace.mu.Unlock()
			explain(t, "current_"+spec.label, statement, args, spec.indexes...)
		})
	}
}
