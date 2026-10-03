package server

import (
	"context"
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type managedFilteredStatsPlanTrace struct {
	statement string
	args      []any
}

func (trace *managedFilteredStatsPlanTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.HasPrefix(data.SQL, "WITH selection_guard AS MATERIALIZED") || strings.HasPrefix(data.SQL, "WITH account_identity_guard AS MATERIALIZED") || strings.HasPrefix(data.SQL, "WITH unsafe AS MATERIALIZED") {
		trace.statement, trace.args = data.SQL, append([]any(nil), data.Args...)
	}
	return ctx
}
func (*managedFilteredStatsPlanTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {
}

// Opt-in read-only verification of the actual filtered reader on a retained
// synthetic fixture. The assertion checks inactive raw branches, not just a
// warm response time which could hide an accidental large detail scan.
func TestPerformanceManagedFilteredStatsPlan(t *testing.T) {
	schema, dsn := os.Getenv("PERFORMANCE_SLO_SCHEMA"), os.Getenv("TEST_DATABASE_URL")
	if os.Getenv("PERFORMANCE_SLO") != "1" || !regexp.MustCompile(`^perf_slo_[a-z0-9_]{8,60}$`).MatchString(schema) || dsn == "" {
		t.Skip("retained synthetic fixture is required")
	}
	trace := &managedFilteredStatsPlanTrace{}
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	config.MaxConns = 1
	config.ConnConfig.RuntimeParams["search_path"] = schema
	config.ConnConfig.RuntimeParams["jit"] = "off"
	config.ConnConfig.RuntimeParams["statement_timeout"] = "5000"
	config.ConnConfig.RuntimeParams["plan_cache_mode"] = "force_generic_plan"
	config.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	m := &Metrics{db: &Database{Pool: pool}}
	for _, filter := range []string{"model=synthetic-model&timezone=UTC", "model=missing&timezone=UTC", "key_id=slo_key&timezone=UTC", "model=synthetic-model&key_id=slo_key&timezone=UTC", "model=synthetic-model&status=error&timezone=UTC"} {
		trace.statement, trace.args = "", nil
		q := managedQuery(t, filter)
		if _, err := m.managedLogStats(t.Context(), q); err != nil {
			t.Fatalf("actual filtered stats %q: %v", filter, err)
		}
		if trace.statement == "" {
			t.Fatal("filtered statement was not captured")
		}
		var raw json.RawMessage
		if err := pool.QueryRow(t.Context(), "EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) "+trace.statement, trace.args...).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var plans []struct {
			Plan map[string]any `json:"Plan"`
		}
		if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
			t.Fatal("invalid filtered plan", err)
		}
		var walk func(map[string]any)
		walk = func(node map[string]any) {
			if node["Relation Name"] == "request_logs" {
				loops, _ := node["Actual Loops"].(float64)
				rows, _ := node["Actual Rows"].(float64)
				if loops > 0 && (node["Node Type"] == "Seq Scan" || rows > 1) {
					t.Fatalf("constant filter scanned raw details: filter=%s node=%v", filter, node)
				}
			}
			if children, ok := node["Plans"].([]any); ok {
				for _, child := range children {
					walk(child.(map[string]any))
				}
			}
		}
		walk(plans[0].Plan)
		evidence, _ := json.Marshal(map[string]any{"filter": filter, "sql": trace.statement, "args": trace.args, "plan": raw})
		t.Logf("FILTERED_STATS_PLAN %s", evidence)
	}
}
