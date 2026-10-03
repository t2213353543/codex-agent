package server

import (
	"context"
	"net/http"
	"time"
)

// Retention is an observed availability report, not a promise inferred from
// the unrelated rolling dashboard retention constant.
func (p *Proxy) handleManagedUsageCoverage(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	var count, first, last int64
	var err error
	storage := "file"
	if p.Metrics.db != nil {
		storage = "postgres"
		// Keep all three values in one snapshot. The exact retained count is
		// maintained with the detail writes; timestamps use the time index's
		// endpoints and never scan/decode the complete log history.
		err = p.Metrics.db.Pool.QueryRow(ctx, `SELECT
		 (SELECT COALESCE(sum(requests),0)::bigint FROM retained_log_minute),
		 COALESCE((SELECT extract(epoch FROM created_at)::bigint FROM request_logs ORDER BY created_at,id LIMIT 1),0),
		 COALESCE((SELECT extract(epoch FROM created_at)::bigint FROM request_logs ORDER BY created_at DESC,id DESC LIMIT 1),0)`).Scan(&count, &first, &last)
	} else {
		_, err = p.Metrics.walkManagedFile(ctx, 0, func(record RequestRecord) error {
			count++
			if first == 0 || record.Time < first {
				first = record.Time
			}
			if record.Time > last {
				last = record.Time
			}
			return nil
		})
	}
	if err != nil {
		p.storeError(w, err)
		return
	}
	writeJSON(w, map[string]any{"scope": managedCleanupScope, "storage": storage, "detail_count": count, "first_record_at": first, "last_record_at": last, "checked_at": time.Now().Unix(), "retention_days": nil, "retention_policy": "explicit_pruning_and_cleanup", "cumulative_events_preserved": p.Metrics.db != nil, "historical_completeness": "not_inferable_from_retained_details", "export_schema": managedUsageExportSchema})
}
