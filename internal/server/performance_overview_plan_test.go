package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type overviewPlanStatement struct {
	SQL       string  `json:"sql"`
	Args      []any   `json:"args"`
	ElapsedMS float64 `json:"elapsed_ms"`
}
type overviewPlanTrace struct {
	mu         sync.Mutex
	statements []overviewPlanStatement
}
type overviewPlanTraceKey struct{}
type overviewPlanTraceStart struct {
	statement overviewPlanStatement
	at        time.Time
}

func (tr *overviewPlanTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	return context.WithValue(ctx, overviewPlanTraceKey{}, overviewPlanTraceStart{overviewPlanStatement{SQL: data.SQL, Args: append([]any(nil), data.Args...)}, time.Now()})
}
func (tr *overviewPlanTrace) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	start, ok := ctx.Value(overviewPlanTraceKey{}).(overviewPlanTraceStart)
	if !ok {
		return
	}
	start.statement.ElapsedMS = float64(time.Since(start.at).Nanoseconds()) / 1e6
	tr.mu.Lock()
	tr.statements = append(tr.statements, start.statement)
	tr.mu.Unlock()
}

func TestPerformanceOverviewPlan(t *testing.T) {
	schema, dsn := os.Getenv("PERFORMANCE_SLO_SCHEMA"), os.Getenv("TEST_DATABASE_URL")
	if os.Getenv("PERFORMANCE_SLO") != "1" || !regexp.MustCompile(`^perf_(slo|uniform)_[a-z0-9_]{8,60}$`).MatchString(schema) || dsn == "" {
		t.Skip("synthetic fixture required")
	}
	u, err := url.Parse(dsn)
	if err != nil || u.Path != "/codex_agent_integration" || u.User == nil || u.User.Username() != "codex_agent_test" {
		t.Fatal("isolated database required")
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	ctx := t.Context()
	keyByte, keyID := byte(17), "slo-synthetic"
	if strings.HasPrefix(schema, "perf_uniform_") {
		keyByte, keyID = 19, "uniform-synthetic"
	}
	db, err := OpenDatabase(ctx, u.String(), base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{keyByte}, 32)), keyID)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	trace := new(overviewPlanTrace)
	config := db.Pool.Config()
	config.MaxConns = 1
	config.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	db.Pool.Close()
	db.Pool = pool
	metrics, err := NewPGMetrics(db)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := NewPGKeyStore(db)
	if err != nil {
		t.Fatal(err)
	}
	p := &Proxy{Store: Store{DB: db}, Groups: NewPGGroupStore(db), Keys: keys, Metrics: metrics}
	var anchor int64
	endOffset := int64(60)
	if strings.HasPrefix(schema, "perf_uniform_") {
		if err := pool.QueryRow(ctx, "SELECT (floor(extract(epoch FROM max(created_at))/86400)::bigint+1)*86400 FROM request_logs").Scan(&anchor); err != nil {
			t.Fatal(err)
		}
		endOffset = 0
	} else {
		if err := pool.QueryRow(ctx, "SELECT anchor FROM performance_slo_fixture").Scan(&anchor); err != nil {
			t.Fatal(err)
		}
	}
	path := fmt.Sprintf("/api/overview?from=%d&to=%d&step=86400&timezone=UTC", anchor-30*86400, anchor+endOffset)
	for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
		if _, err := pool.Exec(ctx, "SET plan_cache_mode="+mode); err != nil {
			t.Fatal(err)
		}
		trace.mu.Lock()
		trace.statements = nil
		trace.mu.Unlock()
		start := time.Now()
		response := httptest.NewRecorder()
		p.handleOverview(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusOK {
			t.Fatalf("overview: %d %s", response.Code, response.Body.String())
		}
		trace.mu.Lock()
		statements := append([]overviewPlanStatement(nil), trace.statements...)
		trace.mu.Unlock()
		performanceSLOEmit(t, "OVERVIEW_CHAIN", map[string]any{"mode": mode, "elapsed_ms": float64(time.Since(start).Nanoseconds()) / 1e6, "statements": statements})
		for _, s := range statements {
			if s.ElapsedMS < 10 && !strings.Contains(s.SQL, "retained_log_minute") {
				continue
			}
			if !strings.HasPrefix(s.SQL, "SELECT ") && !strings.HasPrefix(s.SQL, "WITH ") {
				continue
			}
			var plan json.RawMessage
			if err := pool.QueryRow(ctx, "EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) "+s.SQL, s.Args...).Scan(&plan); err != nil {
				t.Fatal(err)
			}
			performanceSLOEmit(t, "OVERVIEW_PLAN", map[string]any{"mode": mode, "sql": s.SQL, "args": s.Args, "plan": plan})
		}
	}
}
