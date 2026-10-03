package server

// Filters keep the authoritative request_logs columns and expression indexes.
// Only after those predicates select rows do analytical readers substitute the
// transactionally maintained compact JSON. A missing projection row falls back
// to the retained original, so repairing/backfilling derived state cannot omit
// a request or silently change its observations. Keep the original record out
// of the common join: its wide value otherwise follows every selected row into
// hash batches even when the compact projection is present. The two branches
// are disjoint under this statement's MVCC snapshot. In particular, a correlated
// scalar fallback in the common record expression would stop JSON decoding
// from running below a parallel Gather even when every projection is present.
func managedUsageAnalyticsSource(source, where string) string {
	present, missing := managedUsageAnalyticsBranches(source, where)
	return "(" + present + " UNION ALL " + missing + ") analytical_rows"
}

func managedUsageAnalyticsBranches(source, where string) (present, missing string) {
	selection := `SELECT id,event_id,account_ref,account_name,created_at,status,model,key_name FROM ` + source + ` WHERE ` + where
	present = `SELECT selected.id,selected.event_id,selected.account_ref,selected.account_name,
 selected.created_at,selected.status,selected.model,selected.key_name,
 analytics.record AS record FROM (` + selection + `) selected
 JOIN request_log_analytics analytics ON analytics.log_id=selected.id`
	missing = `SELECT missing.id,missing.event_id,missing.account_ref,missing.account_name,
 missing.created_at,missing.status,missing.model,missing.key_name,original.record
 FROM (SELECT selected.* FROM (` + selection + `) selected
 LEFT JOIN request_log_analytics absent ON absent.log_id=selected.id
 WHERE absent.log_id IS NULL OFFSET 0) missing
 CROSS JOIN LATERAL (SELECT original.record FROM request_logs original
 WHERE original.id=missing.id OFFSET 0) original`
	return present, missing
}
