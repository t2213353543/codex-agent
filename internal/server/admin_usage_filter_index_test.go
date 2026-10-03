package server

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPGManagedFilterAndSortIndexPlans(t *testing.T) {
	db := newMetricsTestDB(t)
	tx, err := db.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if _, err = tx.Exec(t.Context(), "SET LOCAL statement_timeout='90s'"); err != nil {
		t.Fatal(err)
	}
	_, err = tx.Exec(t.Context(), `WITH e AS (INSERT INTO usage_events(event_id,created_at,status)
 SELECT 'filter-plan-'||i,to_timestamp(1800000000+i),'ok' FROM generate_series(1,20000)i RETURNING *)
 INSERT INTO request_logs(event_id,created_at,status,model,key_name,record)
 SELECT event_id,created_at,status,'model-'||(substring(event_id from 13)::bigint%10),'key',
 jsonb_build_object('event_id',event_id,'time',extract(epoch FROM created_at)::bigint,'status',status,'model','model-'||(substring(event_id from 13)::bigint%10),
 'input_tokens',substring(event_id from 13)::bigint%111,'output_tokens',substring(event_id from 13)::bigint%77,'cached_tokens',substring(event_id from 13)::bigint%50,
 'ip','192.0.2.'||(substring(event_id from 13)::bigint%250),'key_id','key-'||(substring(event_id from 13)::bigint%80)) FROM e`)
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Pool.Exec(t.Context(), "ANALYZE request_logs"); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
		config := db.Pool.Config()
		config.MaxConns = 1
		config.ConnConfig.RuntimeParams["jit"] = "off"
		config.ConnConfig.RuntimeParams["plan_cache_mode"] = mode
		trace := new(managedUsagePlanTrace)
		config.ConnConfig.Tracer = trace
		pool, err := pgxpool.NewWithConfig(t.Context(), config)
		if err != nil {
			t.Fatal(err)
		}
		m := &Metrics{db: &Database{Pool: pool}}
		for _, spec := range []struct{ filter, index string }{
			{"ip=192.0.2.241&key_id=key-1", "request_logs_filter_tags_idx"},
			{"ip=198.51.100.99", "request_logs_filter_tags_idx"},
			{"sort_by=input_tokens", "request_logs_input_tokens_time_idx"},
			{"sort_by=output_tokens", "request_logs_output_tokens_time_idx"},
			{"sort_by=cached_tokens", "request_logs_cached_tokens_time_idx"},
			{"sort_by=model", "request_logs_model_short_time_idx"},
		} {
			q := managedQuery(t, spec.filter)
			if _, err := m.managedLogs(t.Context(), q); err != nil {
				t.Fatalf("%s %s: %v", mode, spec.filter, err)
			}
			trace.mu.Lock()
			statement, args := trace.sql, append([]any{}, trace.args...)
			trace.mu.Unlock()
			var raw []byte
			if err := pool.QueryRow(t.Context(), "EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) "+statement, args...).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(raw), spec.index) {
				t.Fatalf("%s %s missing index %s: %s", mode, spec.filter, spec.index, raw)
			}
			var plans []struct{ Plan map[string]any }
			if err := json.Unmarshal(raw, &plans); err != nil {
				t.Fatal(err)
			}
			var walk func(map[string]any)
			walk = func(node map[string]any) {
				if node["Relation Name"] == "request_logs" && node["Node Type"] == "Seq Scan" && node["Actual Loops"].(float64) > 0 {
					t.Fatalf("%s %s full log scan: %s", mode, spec.filter, raw)
				}
				if children, ok := node["Plans"].([]any); ok {
					for _, child := range children {
						walk(child.(map[string]any))
					}
				}
			}
			walk(plans[0].Plan)
			t.Logf("FILTER_SORT_INDEX_PLAN mode=%s filter=%s plan=%s", mode, spec.filter, raw)
		}
		pool.Close()
	}
}
