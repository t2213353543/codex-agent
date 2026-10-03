package server

import (
	"context"

	"github.com/jackc/pgx/v5"
)

const managedClassStatsBegin = "BEGIN READ ONLY; SET LOCAL plan_cache_mode=force_generic_plan"

// Both fresh snapshot IDs and a rapidly changing error-class distribution can
// have stale per-value statistics. A custom plan can then repeatedly evaluate
// classification against wide historical records. Keep classification reads
// on a generic plan, scoped to this one read-only aggregate transaction.
type managedClassStatsQueryer struct{ pool managedLogPagePool }

func (queryer managedClassStatsQueryer) Query(ctx context.Context, statement string, args ...any) (pgx.Rows, error) {
	tx, err := queryer.pool.BeginTx(ctx, pgx.TxOptions{BeginQuery: managedClassStatsBegin})
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
