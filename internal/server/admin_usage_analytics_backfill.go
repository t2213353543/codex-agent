package server

import (
	"context"
	"errors"
)

type logAnalyticsBackfillResult struct {
	Complete  bool  `json:"complete"`
	Scanned   int64 `json:"scanned"`
	Inserted  int64 `json:"inserted"`
	Remaining int64 `json:"remaining"`
	Batches   int64 `json:"batches"`
}

// BackfillLogAnalytics runs only as explicit maintenance. It never updates
// request_logs, pauses a writer table, or replaces an existing projection.
// Per-batch KEY SHARE prevents deletion of selected parents until insertion;
// concurrent record updates still proceed and their UPSERT wins over our
// ON CONFLICT DO NOTHING regardless of which transaction arrives first.
func (d *Database) BackfillLogAnalytics(ctx context.Context, batchSize int) (logAnalyticsBackfillResult, error) {
	var result logAnalyticsBackfillResult
	if batchSize < 1 || batchSize > 1000 {
		return result, errors.New("analytics batch size must be between 1 and 1000")
	}
	var ceiling int64
	if err := d.Pool.QueryRow(ctx, "SELECT COALESCE(max(id),0) FROM request_logs").Scan(&ceiling); err != nil {
		return result, err
	}
	var cursor int64
	first := true
	for {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		tx, err := d.Pool.Begin(ctx)
		if err != nil {
			return result, err
		}
		if _, err = tx.Exec(ctx, "SET LOCAL lock_timeout='1s'; SET LOCAL statement_timeout='30s'"); err != nil {
			_ = tx.Rollback(context.Background())
			return result, err
		}
		var next, scanned, inserted int64
		err = tx.QueryRow(ctx, `WITH selected AS MATERIALIZED (
 SELECT l.id,l.record FROM request_logs l WHERE ($4::boolean OR l.id>$1) AND l.id<=$2
 ORDER BY l.id LIMIT $3 FOR KEY SHARE OF l
 ), written AS (
 INSERT INTO request_log_analytics(log_id,record)
 SELECT selected.id,request_log_analytics_record(selected.record) FROM selected
 WHERE NOT EXISTS(SELECT 1 FROM request_log_analytics a WHERE a.log_id=selected.id)
 ON CONFLICT(log_id) DO NOTHING RETURNING log_id
 ) SELECT COALESCE(max(id),$1),count(*),(SELECT count(*) FROM written) FROM selected`, cursor, ceiling, batchSize, first).Scan(&next, &scanned, &inserted)
		if err == nil {
			err = tx.Commit(ctx)
		} else {
			_ = tx.Rollback(context.Background())
		}
		if err != nil {
			return result, err
		}
		if scanned == 0 {
			break
		}
		result.Scanned += scanned
		result.Inserted += inserted
		result.Batches++
		cursor = next
		first = false
	}
	if err := d.Pool.QueryRow(ctx, `SELECT count(*) FROM request_logs l WHERE NOT EXISTS(SELECT 1 FROM request_log_analytics a WHERE a.log_id=l.id)`).Scan(&result.Remaining); err != nil {
		return result, err
	}
	result.Complete = result.Remaining == 0
	return result, nil
}
