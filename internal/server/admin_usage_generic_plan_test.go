package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func exerciseManagedIdentityGenericPlan(t *testing.T, db *Database, identity string, count int) {
	t.Helper()
	ctx := t.Context()
	for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
		t.Run(mode, func(t *testing.T) {
			trace := &managedUsagePlanTrace{}
			config := db.Pool.Config()
			config.MaxConns = 1
			config.ConnConfig.Tracer = trace
			config.ConnConfig.RuntimeParams["plan_cache_mode"] = mode
			config.ConnConfig.RuntimeParams["jit"] = "off"
			config.ConnConfig.RuntimeParams["statement_timeout"] = "2000"
			pool, err := pgxpool.NewWithConfig(ctx, config)
			if err != nil {
				t.Fatal(err)
			}
			defer pool.Close()
			m := &Metrics{db: &Database{Pool: pool}}
			q := managedQuery(t, "request_id="+identity+"&exact_total=true")
			var elapsed []float64
			var record RequestRecord
			for range count {
				start := time.Now()
				page, err := m.managedLogs(ctx, q)
				if err != nil || len(page.Items) != 1 || page.Total != 1 {
					t.Fatalf("identity warm query failed: total=%d items=%d err=%v", page.Total, len(page.Items), err)
				}
				elapsed = append(elapsed, float64(time.Since(start).Microseconds())/1000)
				record = page.Items[0].RequestRecord
			}
			trace.mu.Lock()
			statement, args := trace.sql, append([]any(nil), trace.args...)
			trace.mu.Unlock()
			var raw json.RawMessage
			if err := pool.QueryRow(ctx, "EXPLAIN (ANALYZE,BUFFERS,SETTINGS,FORMAT JSON) "+statement, args...).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			var plan []struct {
				Plan map[string]any `json:"Plan"`
			}
			if err := json.Unmarshal(raw, &plan); err != nil || len(plan) != 1 {
				t.Fatal("invalid query plan", err)
			}
			var probe bool
			var inspect func(map[string]any)
			inspect = func(node map[string]any) {
				if node["Relation Name"] == "request_logs" && node["Node Type"] == "Seq Scan" {
					t.Fatal("identity query scanned unrelated records")
				}
				if node["Index Name"] == "request_logs_pkey" {
					condition, _ := node["Index Cond"].(string)
					if !strings.Contains(condition, " = ") {
						t.Fatalf("generic query used an unbounded primary-key scan: %s", condition)
					}
					probe = true
				}
				if children, ok := node["Plans"].([]any); ok {
					for _, child := range children {
						inspect(child.(map[string]any))
					}
				}
			}
			inspect(plan[0].Plan)
			if !probe {
				t.Fatal("no identity primary-key probe")
			}
			evidence, _ := json.Marshal(map[string]any{"mode": mode, "samples_ms": elapsed, "sql": statement, "args": args, "plan": raw})
			t.Logf("MANAGED_IDENTITY_GENERIC_PLAN %s", evidence)
			for _, requestID := range []string{identity, strconv.FormatInt(record.ID, 10)} {
				q.RequestID = requestID
				stats, err := m.managedLogStats(ctx, q)
				if err != nil || stats.Requests != 1 {
					t.Fatalf("identity stats: %d %v", stats.Requests, err)
				}
				var exported bytes.Buffer
				n, err := m.exportManagedLogs(ctx, q, "jsonl", &exported)
				if err != nil || n != 1 {
					t.Fatalf("identity export: %d %v", n, err)
				}
				options, err := m.managedUsageOptions(ctx, q, "model", "", 10, nil)
				if err != nil || len(options) != 1 || options[0].Value != record.Model {
					t.Fatalf("identity options: %+v %v", options, err)
				}
			}
			canceled, cancel := context.WithCancel(ctx)
			cancel()
			if _, err := m.managedLogs(canceled, q); !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled query lost context error: %v", err)
			}
			q.Model = "missing"
			page, err := m.managedLogs(ctx, q)
			if err != nil || page.Total != 0 || len(page.Items) != 0 || pool.Stat().AcquiredConns() != 0 {
				t.Fatalf("identity filter/cancellation left wrong result or leaked connection: %+v %v", page, err)
			}
		})
	}
}

func TestPGManagedIdentityGenericPlan(t *testing.T) {
	db := newMetricsTestDB(t)
	seed, err := db.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer seed.Rollback(t.Context())
	if _, err = seed.Exec(t.Context(), "SET LOCAL statement_timeout='90s'"); err != nil {
		t.Fatal(err)
	}
	// Deliberately omit request/response IDs, matching sparse real-world data.
	// Populating every JSON identity masks the generic-plan selectivity defect.
	_, err = seed.Exec(t.Context(), `WITH events AS (
 INSERT INTO usage_events(event_id,created_at,status) SELECT 'generic-'||i,to_timestamp(1700000000+i),'ok' FROM generate_series(1,30000)i RETURNING *
 ) INSERT INTO request_logs(event_id,created_at,status,model,record) SELECT event_id,created_at,status,'synthetic',jsonb_build_object('event_id',event_id,'time',extract(epoch FROM created_at)::bigint,'status',status,'model','synthetic') FROM events`)
	if err != nil {
		t.Fatal(err)
	}
	if err := seed.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Pool.Exec(t.Context(), "ANALYZE request_logs"); err != nil {
		t.Fatal(err)
	}
	exerciseManagedIdentityGenericPlan(t, db, "generic-15000", 20)
	if _, err := db.Pool.Exec(t.Context(), `UPDATE request_logs SET record=record||'{"client_request_id":"one-client"}'::jsonb WHERE event_id IN ('generic-15000','generic-16000')`); err != nil {
		t.Fatal(err)
	}
	config := db.Pool.Config()
	config.MaxConns = 1
	config.ConnConfig.RuntimeParams["plan_cache_mode"] = "force_generic_plan"
	trace := &managedUsagePlanTrace{}
	config.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	m := &Metrics{db: &Database{Pool: pool}}
	for _, spec := range []struct {
		filter string
		count  int64
	}{
		{"client_request_id=one-client", 2},
		{"client_request_id=one-client&request_id=generic-15000", 1},
		{"client_request_id=one-client&request_id=generic-17000", 0},
		{"client_request_id=absent", 0},
	} {
		q := managedQuery(t, spec.filter+"&exact_total=true")
		for range 8 {
			page, err := m.managedLogs(t.Context(), q)
			if err != nil || page.Total != spec.count || int64(len(page.Items)) != spec.count {
				t.Fatalf("client identity intersection %q: %+v %v", spec.filter, page, err)
			}
		}
	}
}

func TestPerformanceManagedIdentityGenericPlan(t *testing.T) {
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
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	t.Log(fmt.Sprintf("read-only fixture %s", schema))
	exerciseManagedIdentityGenericPlan(t, &Database{Pool: pool}, "slo_e_500", 20)
}
