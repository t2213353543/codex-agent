package server

import "fmt"

// Global totals keep the minute-boundary contract: bucket >= from && bucket <
// to. Only complete UTC hours use the rollup; at most two partial hours read
// the account minute table. All branches share the query's MVCC snapshot.
func globalMetricsRangeSQL(from, to int64) (string, []any) {
	if from < 0 || to <= from || from > to-3600 {
		return "SELECT " + metricsColumns + " FROM usage_minute WHERE bucket >= $1 AND bucket < $2", []any{from, to}
	}
	first := from / 3600 * 3600
	if first < from {
		first += 3600
	}
	last := to / 3600 * 3600
	if first >= last {
		return "SELECT " + metricsColumns + " FROM usage_minute WHERE bucket >= $1 AND bucket < $2", []any{from, to}
	}
	return fmt.Sprintf(`SELECT %s FROM usage_hourly_global WHERE bucket >= $3 AND bucket < $4
UNION ALL SELECT %s FROM usage_minute WHERE bucket >= $1 AND bucket < $3
UNION ALL SELECT %s FROM usage_minute WHERE bucket >= $4 AND bucket < $2`, metricsColumns, metricsColumns, metricsColumns), []any{from, to, first, last}
}
