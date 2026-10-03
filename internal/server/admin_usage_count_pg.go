package server

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// The retained rollup covers time and the all/ok/failed status totals. Other
// predicates keep the full exact query; adding a future filter automatically
// makes whereSQL nonempty and therefore cannot accidentally ignore it here.
func managedRetainedCountColumn(q managedLogQuery) string {
	column := "requests"
	switch q.Status {
	case "":
	case "ok":
		column = "ok"
	case "failed":
		column = "errors"
	default:
		return ""
	}
	q.From, q.To, q.Status = 0, 0, ""
	where, args := q.whereSQL()
	if where != "TRUE" || len(args) != 0 {
		return ""
	}
	return column
}

func (m *Metrics) managedRetainedCount(ctx context.Context, q managedLogQuery, ceiling int64, fallbackWhere string, fallbackArgs []any) (int64, error) {
	column := managedRetainedCountColumn(q)
	if column == "" {
		if statement, dimArgs, ok := managedRetainedDimensionCountSQL(q, ceiling); ok {
			var count int64
			if err := m.db.Pool.QueryRow(ctx, statement, dimArgs...).Scan(&count); err != nil {
				return 0, err
			}
			return count, nil
		}
		var count int64
		err := m.db.Pool.QueryRow(ctx, "SELECT count(*) FROM request_logs WHERE "+fallbackWhere, fallbackArgs...).Scan(&count)
		return count, err
	}
	first, last := (q.From+59)/60*60, q.To/60*60
	if q.From != 0 && q.To != 0 && first >= last {
		var count int64
		err := m.db.Pool.QueryRow(ctx, "SELECT count(*) FROM request_logs WHERE "+fallbackWhere, fallbackArgs...).Scan(&count)
		return count, err
	}
	where, args := q.whereSQLAfter([]any{first, last, ceiling})
	whole := []string{"TRUE"}
	edges := []string{}
	if q.From != 0 {
		whole = append(whole, "bucket >= $1::bigint")
		if first != q.From {
			edges = append(edges, "created_at < to_timestamp($1::bigint)")
		}
	}
	if q.To != 0 {
		whole = append(whole, "bucket < $2::bigint")
		if last != q.To {
			edges = append(edges, "created_at >= to_timestamp($2::bigint)")
		}
	}
	selected := "SELECT " + column + " AS n FROM retained_log_minute WHERE " + strings.Join(whole, " AND ")
	if len(edges) > 0 {
		selected += " UNION ALL SELECT count(*) FROM request_logs WHERE " + where + " AND (" + strings.Join(edges, " OR ") + ")"
	}
	// A cursor may predate newer inserts, including out-of-order timestamps.
	// Subtract those IDs in the same MVCC statement snapshot as the rollup;
	// retaining only time bounds would silently change cursor exact totals.
	statement := fmt.Sprintf(`WITH bounds AS (SELECT $1::bigint AS first,$2::bigint AS last)
 SELECT (SELECT COALESCE(sum(n),0)::bigint FROM (%s) counts)
 - (SELECT count(*) FROM request_logs WHERE %s AND id>$3)`, selected, where)
	var count int64
	err := m.db.Pool.QueryRow(ctx, statement, args...).Scan(&count)
	return count, err
}

// managedRetainedDimensionCountSQL answers the exact total for a single
// error_category or error_severity filter from the retained minute dimensions
// instead of scanning details. Category and severity rows only exist for
// failed requests; minutes whose dimensions are not retained (unsafe) and
// sub-minute edges are read from details inside the same statement, mirroring
// the rollup source. Rows inserted after the cursor ceiling are subtracted
// from details so cursor exact totals stay stable.
func managedRetainedDimensionCountSQL(q managedLogQuery, ceiling int64) (string, []any, bool) {
	kind, name := "", ""
	switch {
	case q.ErrorCategory != "" && q.ErrorSeverity == "":
		kind, name = "category", q.ErrorCategory
	case q.ErrorSeverity != "" && q.ErrorCategory == "":
		kind, name = "severity", q.ErrorSeverity
	default:
		return "", nil, false
	}
	// The retained dimension rows aggregate status<>'ok'; counting a specific
	// status from them would include the other failed statuses. Only the
	// all/failed forms match the rollup's bucket population.
	if q.Status != "" && q.Status != "failed" {
		return "", nil, false
	}
	first, last := (q.From+59)/60*60, q.To/60*60
	if q.From != 0 && q.To != 0 && first >= last {
		// A sub-minute window has no complete minute; the bounded time-index
		// scan on details is already the fastest path.
		return "", nil, false
	}
	rest := q
	rest.From, rest.To, rest.Status, rest.ErrorCategory, rest.ErrorSeverity = 0, 0, "", "", ""
	where, restArgs := rest.whereSQL()
	if where != "TRUE" || len(restArgs) != 0 {
		return "", nil, false
	}
	// whereSQLAfter renumbers the detail predicates past the shared
	// $1..$4 parameters instead of reusing $1 for the first filter, and
	// returns the complete argument list including those shared values.
	rawWhere, args := q.whereSQLAfter([]any{first, last, kind, name})
	args = append(args, ceiling)
	ceilingIndex := len(args)
	parts := []string{fmt.Sprintf("SELECT sum(requests)::bigint AS n FROM retained_usage_minute WHERE kind=$3 AND name=$4 AND status<>'ok' AND NOT EXISTS(SELECT 1 FROM retained_usage_unsafe_minute u WHERE u.bucket=retained_usage_minute.bucket)")}
	if q.From != 0 {
		parts[0] += " AND bucket>=$1::bigint"
	}
	if q.To != 0 {
		parts[0] += " AND bucket<$2::bigint"
	}
	unsafeRange := []string{"requests>0"}
	if q.From != 0 {
		unsafeRange = append(unsafeRange, "bucket>=$1::bigint")
	}
	if q.To != 0 {
		unsafeRange = append(unsafeRange, "bucket<$2::bigint")
	}
	parts = append(parts, fmt.Sprintf("SELECT COALESCE(sum(c.n),0)::bigint FROM (SELECT bucket FROM retained_usage_unsafe_minute WHERE %s) u CROSS JOIN LATERAL (SELECT count(*) AS n FROM request_logs WHERE %s AND created_at>=to_timestamp(u.bucket) AND created_at<to_timestamp(u.bucket+60) OFFSET 0) c", strings.Join(unsafeRange, " AND "), rawWhere))
	edges := []string{}
	if q.From != 0 {
		edges = append(edges, "created_at < to_timestamp($1::bigint)")
	}
	if q.To != 0 {
		edges = append(edges, "created_at >= to_timestamp($2::bigint)")
	}
	if len(edges) > 0 {
		parts = append(parts, fmt.Sprintf("SELECT count(*)::bigint FROM request_logs WHERE %s AND (%s)", rawWhere, strings.Join(edges, " OR ")))
	}
	// bounds anchors $1/$2's types even when from/to are empty (they then never
	// appear in a predicate); pgx still sends every parameter position, so an
	// unanchored first/last slot would raise 42P18. Same pattern as
	// managedRetainedCount.
	statement := fmt.Sprintf(`WITH bounds AS (SELECT $1::bigint AS first,$2::bigint AS last)
 SELECT (SELECT COALESCE(sum(n),0)::bigint FROM (%s) counts)
 - (SELECT count(*) FROM request_logs WHERE %s AND id>$%d)`, strings.Join(parts, " UNION ALL "), rawWhere, ceilingIndex)
	return statement, args, true
}

// usageCountCacheKey mirrors overviewCacheKey: the key repeats the filter
// validation (filterHash hashes every predicate plus from/to/step/timezone),
// and cache errors always fall through to PostgreSQL. Snapshot/as-of queries
// are excluded because filterHash zeroes snapshot identity; review totals
// mutate as items are resolved; request-id paths are point probes that need
// no cache.
func usageCountCacheKey(d *Database, q managedLogQuery) string {
	if d == nil || d.Redis == nil || q.AsOf != "" || q.SnapshotID != "" || q.ReviewOnly != nil || q.RequestID != "" || q.ClientRequestID != "" {
		return ""
	}
	return d.Namespace + "usage:count:" + q.filterHash()
}

// managedExactTotal answers ExactTotal for the listing query. Point-probe
// request identities go straight to PostgreSQL; cacheable filters share a
// 10s Redis value keyed by the validated filter set, with the same staleness
// the overview dashboard already tolerates.
func (m *Metrics) managedExactTotal(ctx context.Context, q managedLogQuery, ceiling int64, source, base string, args []any) (int64, error) {
	if q.RequestID != "" || q.ClientRequestID != "" {
		var total int64
		err := m.db.Pool.QueryRow(ctx, "SELECT count(*) FROM "+source+" WHERE "+base, args...).Scan(&total)
		return total, err
	}
	if key := usageCountCacheKey(m.db, q); key != "" {
		cctx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
		var total int64
		err := m.db.Redis.Get(cctx, key).Scan(&total)
		cancel()
		if err == nil {
			return total, nil
		}
	}
	total, err := m.managedRetainedCount(ctx, q, ceiling, base, args)
	if err != nil {
		return 0, err
	}
	if key := usageCountCacheKey(m.db, q); key != "" {
		cctx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
		_ = m.db.Redis.Set(cctx, key, total, 10*time.Second).Err()
		cancel()
	}
	return total, nil
}
