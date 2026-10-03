package server

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// Only model-dependent page reads need their actual parameter selectivity. A
// cached generic plan can scan the entire retained time/primary-key index for a
// missing model or the empty long-model branch. Other reads intentionally keep
// their normal plan-cache behavior (some are faster with generic plans).
const managedLogPageCustomPlanBegin = "BEGIN READ ONLY; SET LOCAL plan_cache_mode=force_custom_plan"

type managedLogPagePool interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error)
}

func queryManagedLogPage(ctx context.Context, pool managedLogPagePool, q managedLogQuery, statement string, args ...any) (pgx.Rows, error) {
	if q.Model == "" && q.SortBy != "model" {
		return pool.Query(ctx, statement, args...)
	}
	// BeginQuery combines the fixed, input-free BEGIN and SET LOCAL in one
	// round trip. Query still uses the original parameterized SQL and pgx
	// statement cache/tracing. No setting escapes this read-only transaction.
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{BeginQuery: managedLogPageCustomPlanBegin})
	if err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, statement, args...)
	if err != nil {
		if rows != nil {
			rows.Close()
		}
		rollbackManagedLogPage(tx)
		return nil, err
	}
	return &managedLogPageRows{Rows: rows, tx: tx}, nil
}

type managedLogPageRows struct {
	pgx.Rows
	tx pgx.Tx
}

func (rows *managedLogPageRows) Close() {
	if rows.tx == nil {
		return
	}
	// pgx must finish/close the active result before issuing ROLLBACK. The
	// pool releases the connection only after rollback restores SET LOCAL.
	rows.Rows.Close()
	rollbackManagedLogPage(rows.tx)
	rows.tx = nil
}

func (rows *managedLogPageRows) Next() bool {
	if rows.Rows.Next() {
		return true
	}
	rows.Close()
	return false
}

func rollbackManagedLogPage(tx pgx.Tx) {
	// Request cancellation must not cancel cleanup. Bound that independent
	// cleanup; pgx closes a connection whose rollback fails instead of reusing
	// an aborted transaction or a connection with transaction-local settings.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = tx.Rollback(ctx)
}
