package server

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPerformanceManagedFilteredListPlan(t *testing.T) {
	schema := os.Getenv("PERFORMANCE_SLO_SCHEMA")
	if os.Getenv("PERFORMANCE_SLO") != "1" || !regexp.MustCompile(`^perf_slo_[a-z0-9_]{8,60}$`).MatchString(schema) {
		t.Skip("explicit retained synthetic fixture")
	}
	config, err := pgxpool.ParseConfig(os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	trace := new(managedUsagePlanTrace)
	config.MaxConns = 1
	config.ConnConfig.Tracer = trace
	config.ConnConfig.RuntimeParams["search_path"] = schema
	config.ConnConfig.RuntimeParams["jit"] = "off"
	config.ConnConfig.RuntimeParams["statement_timeout"] = "15000"
	for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
		modeConfig := config.Copy()
		modeConfig.ConnConfig.RuntimeParams["plan_cache_mode"] = mode
		pool, err := pgxpool.NewWithConfig(t.Context(), modeConfig)
		if err != nil {
			t.Fatal(err)
		}
		m := &Metrics{db: &Database{Pool: pool}}
		for _, filter := range []string{"model=synthetic-model&status=error&sort_by=tokens", "model=missing-synthetic", "ip=192.0.2.123"} {
			if selected := os.Getenv("PERFORMANCE_SLO_CASES"); selected != "" && !strings.Contains(filter, selected) {
				continue
			}
			_, readErr := m.managedLogs(t.Context(), managedQuery(t, filter))
			trace.mu.Lock()
			statement, args := trace.sql, append([]any{}, trace.args...)
			trace.mu.Unlock()
			var raw json.RawMessage
			conn, err := pool.Acquire(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			name := "filter_list_plan"
			if _, err := conn.Conn().Prepare(t.Context(), name, statement); err != nil {
				t.Fatal(err)
			}
			literals := make([]string, len(args))
			for i, arg := range args {
				switch value := arg.(type) {
				case []byte:
					literals[i] = "'" + strings.ReplaceAll(string(value), "'", "''") + "'::jsonb"
				case string:
					literals[i] = "'" + strings.ReplaceAll(value, "'", "''") + "'"
				case int, int64:
					literals[i] = fmt.Sprint(value)
				default:
					t.Fatalf("unsupported synthetic plan argument %T", arg)
				}
			}
			execute := "EXECUTE " + pgx.Identifier{name}.Sanitize() + "(" + strings.Join(literals, ",") + ")"
			if readErr != nil {
				if err := conn.QueryRow(t.Context(), "EXPLAIN (FORMAT JSON) "+execute).Scan(&raw); err != nil {
					t.Fatal(err)
				}
				conn.Release()
				performanceSLOEmit(t, "FILTER_LIST_PLAN", map[string]any{"filter": filter, "mode": mode, "sql": statement, "args": args, "plan": raw, "read_failed": true, "scope": "estimated plan after failed real read; not execution latency"})
				t.Error(readErr)
				continue
			}
			if err := conn.QueryRow(t.Context(), "EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) "+execute).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			if err := conn.Conn().Deallocate(t.Context(), name); err != nil {
				t.Fatal(err)
			}
			conn.Release()
			performanceSLOEmit(t, "FILTER_LIST_PLAN", map[string]any{"filter": filter, "mode": mode, "sql": statement, "args": args, "plan": raw})
		}
		pool.Close()
	}
}
