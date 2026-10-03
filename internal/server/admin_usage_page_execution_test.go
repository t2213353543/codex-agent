package server

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

type managedLogPageTestRows struct {
	pgx.Rows
	events *[]string
	left   int
	closed bool
}

func (r *managedLogPageTestRows) Close() {
	if !r.closed {
		*r.events = append(*r.events, "close")
		r.closed = true
	}
}
func (r *managedLogPageTestRows) Next() bool {
	if r.closed || r.left == 0 {
		return false
	}
	r.left--
	return true
}

type managedLogPageTestTx struct {
	pgx.Tx
	events         *[]string
	rows           *managedLogPageTestRows
	queryErr       error
	statement      string
	args           []any
	rollbacks      int
	rollbackErr    error
	rollbackCtxErr error
	rollbackLimit  time.Duration
}

func (tx *managedLogPageTestTx) Query(ctx context.Context, statement string, args ...any) (pgx.Rows, error) {
	*tx.events = append(*tx.events, "query")
	tx.statement, tx.args = statement, append([]any{}, args...)
	return tx.rows, tx.queryErr
}
func (tx *managedLogPageTestTx) Rollback(ctx context.Context) error {
	*tx.events = append(*tx.events, "rollback")
	tx.rollbacks++
	tx.rollbackCtxErr = ctx.Err()
	if deadline, ok := ctx.Deadline(); ok {
		tx.rollbackLimit = time.Until(deadline)
	}
	return tx.rollbackErr
}

type managedLogPageTestPool struct {
	tx       *managedLogPageTestTx
	options  []pgx.TxOptions
	beginErr error
	queries  int
}

func (p *managedLogPageTestPool) Query(ctx context.Context, statement string, args ...any) (pgx.Rows, error) {
	p.queries++
	return p.tx.Query(ctx, statement, args...)
}
func (p *managedLogPageTestPool) BeginTx(ctx context.Context, options pgx.TxOptions) (pgx.Tx, error) {
	*p.tx.events = append(*p.tx.events, "begin")
	p.options = append(p.options, options)
	return p.tx, p.beginErr
}

func newManagedLogPageTestPool() *managedLogPageTestPool {
	events := []string{}
	return &managedLogPageTestPool{tx: &managedLogPageTestTx{events: &events, rows: &managedLogPageTestRows{events: &events, left: 1}}}
}

func TestManagedLogPageCustomPlanScope(t *testing.T) {
	for _, spec := range []struct {
		name   string
		q      managedLogQuery
		custom bool
	}{
		{"default", managedLogQuery{}, false},
		{"time", managedLogQuery{SortBy: "time"}, false},
		{"tokens", managedLogQuery{SortBy: "tokens"}, false},
		{"latency", managedLogQuery{SortBy: "latency"}, false},
		{"identity", managedLogQuery{RequestID: "synthetic-id"}, false},
		{"other_model_field", managedLogQuery{SentModel: "synthetic"}, false},
		{"model_filter", managedLogQuery{Model: "synthetic"}, true},
		{"model_sort", managedLogQuery{SortBy: "model"}, true},
		{"model_both", managedLogQuery{Model: "synthetic", SortBy: "model"}, true},
	} {
		t.Run(spec.name, func(t *testing.T) {
			pool := newManagedLogPageTestPool()
			rows, err := queryManagedLogPage(t.Context(), pool, spec.q, "SELECT id,record FROM request_logs WHERE model=$1 LIMIT 11 OFFSET 20", "synthetic")
			if err != nil {
				t.Fatal(err)
			}
			rows.Close()
			if spec.custom {
				if pool.queries != 0 || len(pool.options) != 1 || pool.options[0] != (pgx.TxOptions{BeginQuery: managedLogPageCustomPlanBegin}) {
					t.Fatalf("not one fixed combined transaction begin: %+v", pool)
				}
				if got := *pool.tx.events; !reflect.DeepEqual(got, []string{"begin", "query", "close", "rollback"}) {
					t.Fatalf("unexpected execution order: %v", got)
				}
			} else if pool.queries != 1 || len(pool.options) != 0 || pool.tx.rollbacks != 0 {
				t.Fatalf("ordinary page gained a transaction or extra query: %+v", pool)
			}
		})
	}
}

func TestManagedLogPageCustomPlanKeepsParametersAndPageSQL(t *testing.T) {
	model := "'; SET plan_cache_mode=force_generic_plan; SELECT pg_sleep(9); --"
	for _, sortBy := range []string{"time", "model", "tokens"} {
		q := managedLogQuery{Model: model, SortBy: sortBy, SortOrder: "desc", Page: 3, Size: 10}
		source, where, args := q.readSourceSQL()
		statement, args := managedLogPageSQL(q, source, where, args)
		original := append([]any{}, args...)
		pool := newManagedLogPageTestPool()
		rows, err := queryManagedLogPage(t.Context(), pool, q, statement, args...)
		if err != nil {
			t.Fatal(err)
		}
		rows.Close()
		if pool.tx.statement != statement || !reflect.DeepEqual(pool.tx.args, original) || !reflect.DeepEqual(args, original) {
			t.Fatal("page SQL or bound parameters changed")
		}
		if strings.Contains(statement, model) || strings.Contains(pool.options[0].BeginQuery, model) || !strings.Contains(statement, "LIMIT 11 OFFSET 20") {
			t.Fatalf("unsafe input or changed pagination: %s", statement)
		}
		if sortBy == "model" && !strings.Contains(statement, "LIMIT 31") {
			t.Fatal("model branch lost offset+lookahead limit")
		}
	}
}

func TestManagedLogPageCustomPlanCleanup(t *testing.T) {
	for _, finish := range []string{"close", "exhausted", "canceled", "rollback_error"} {
		t.Run(finish, func(t *testing.T) {
			pool := newManagedLogPageTestPool()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			rows, err := queryManagedLogPage(ctx, pool, managedLogQuery{Model: "missing"}, "SELECT id,record FROM request_logs WHERE model=$1", "missing")
			if err != nil {
				t.Fatal(err)
			}
			if finish == "canceled" {
				cancel()
			}
			if finish == "rollback_error" {
				pool.tx.rollbackErr = errors.New("connection closed")
			}
			if finish == "exhausted" {
				if !rows.Next() || rows.Next() {
					t.Fatal("row iteration changed")
				}
			} else {
				rows.Close()
			}
			rows.Close()
			if pool.tx.rollbacks != 1 || !pool.tx.rows.closed || pool.tx.rollbackCtxErr != nil || pool.tx.rollbackLimit <= 0 || pool.tx.rollbackLimit > 5*time.Second {
				t.Fatalf("cleanup not idempotent, independent and bounded: %+v", pool.tx)
			}
			if got := *pool.tx.events; !reflect.DeepEqual(got, []string{"begin", "query", "close", "rollback"}) {
				t.Fatalf("rollback ran before rows closed: %v", got)
			}
		})
	}
}

func TestManagedLogPageCustomPlanErrors(t *testing.T) {
	for _, failAt := range []string{"begin", "query"} {
		t.Run(failAt, func(t *testing.T) {
			pool := newManagedLogPageTestPool()
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			if failAt == "begin" {
				pool.beginErr = context.Canceled
			} else {
				pool.tx.queryErr = context.Canceled
			}
			rows, err := queryManagedLogPage(ctx, pool, managedLogQuery{SortBy: "model"}, "SELECT id,record FROM request_logs", 1)
			if rows != nil || !errors.Is(err, context.Canceled) {
				t.Fatalf("lost failure: %v", err)
			}
			wantRollback := 0
			if failAt == "query" {
				wantRollback = 1
			}
			if pool.tx.rollbacks != wantRollback || pool.tx.rollbackCtxErr != nil {
				t.Fatalf("wrong failed-query cleanup: %+v", pool.tx)
			}
			if failAt == "query" && !pool.tx.rows.closed {
				t.Fatal("failed query rows were not closed before rollback")
			}
		})
	}
}
