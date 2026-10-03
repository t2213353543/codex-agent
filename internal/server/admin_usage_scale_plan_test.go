package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type managedStatsScalePlanQueryer struct {
	pool *pgxpool.Pool
	t    *testing.T
	name string
}

func (q managedStatsScalePlanQueryer) Query(ctx context.Context, statement string, args ...any) (pgx.Rows, error) {
	var plan json.RawMessage
	if err := q.pool.QueryRow(ctx, "EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) "+statement, args...).Scan(&plan); err != nil {
		return nil, err
	}
	evidence, _ := json.Marshal(map[string]any{"case": q.name, "sql": statement, "args": args, "plan": plan})
	q.t.Logf("MANAGED_STATS_SCALE_PLAN %s", evidence)
	// EXPLAIN already executed the real query. No need to execute it again
	// merely to have managedLogStats consume the same aggregate rows.
	return q.pool.Query(ctx, "SELECT 1 WHERE false")
}

// Use only the retained synthetic performance fixture. This is an opt-in plan
// diagnostic, not a claim that every endpoint meets an SLA at that row count.
func TestPerformanceManagedUsageStatsPlan(t *testing.T) {
	schema, dsn := os.Getenv("PERFORMANCE_SLO_SCHEMA"), os.Getenv("TEST_DATABASE_URL")
	if os.Getenv("PERFORMANCE_SLO") != "1" || !regexp.MustCompile(`^perf_slo_[a-z0-9_]{8,60}$`).MatchString(schema) || dsn == "" {
		t.Skip("synthetic performance fixture is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	config.MaxConns = 1
	config.ConnConfig.RuntimeParams["search_path"] = schema
	config.ConnConfig.RuntimeParams["statement_timeout"] = "180000"
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var first, last int64
	if err := pool.QueryRow(ctx, "SELECT extract(epoch FROM min(created_at))::bigint,extract(epoch FROM max(created_at))::bigint FROM request_logs").Scan(&first, &last); err != nil {
		t.Fatal(err)
	}
	m := &Metrics{db: &Database{Pool: pool}}
	for _, jit := range []string{"on", "off"} {
		if _, err := pool.Exec(ctx, "SET jit="+jit); err != nil {
			t.Fatal(err)
		}
		for _, accountsOnly := range []bool{false, true} {
			q, err := parseManagedLogQuery(url.Values{"from": {fmt.Sprint(first)}, "to": {fmt.Sprint(last + 1)}, "step": {"86400"}, "timezone": {"UTC"}})
			if err != nil {
				t.Fatal(err)
			}
			q.accountsOnly = accountsOnly
			q.statsQueryer = managedStatsScalePlanQueryer{pool: pool, t: t, name: fmt.Sprintf("jit_%s_accounts_only_%t", jit, accountsOnly)}
			if _, err := m.managedLogStats(ctx, q); err != nil {
				t.Fatal(err)
			}
		}
	}
}
