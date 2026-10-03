package server

import (
	"fmt"
	"strings"
)

// A single class dimension can prove all/none inside one complete minute.
// Other filters must retain their exact raw path: two marginal dimensions
// cannot prove an intersection, even if their individual counts are equal.
func managedUsageClassRollup(q managedLogQuery) (kind, value string, eligible bool) {
	switch {
	case q.ErrorCategory != "" && q.ErrorSeverity == "":
		kind, value = "category", q.ErrorCategory
	case q.ErrorSeverity != "" && q.ErrorCategory == "":
		kind, value = "severity", q.ErrorSeverity
	default:
		return "", "", false
	}
	q.ErrorCategory, q.ErrorSeverity = "", ""
	return kind, value, managedUsageRollupEligible(q)
}

func managedUsageRollupOrClassEligible(q managedLogQuery) bool {
	if managedUsageRollupEligible(q) {
		return true
	}
	_, _, eligible := managedUsageClassRollup(q)
	return eligible
}

func managedUsageClassMinutesSQL(table string, conditions []string, kind, parameter string) string {
	return fmt.Sprintf(`class_minutes AS MATERIALIZED (
 SELECT bucket,COALESCE(sum(requests) FILTER(WHERE kind='total'),0) AS total,
 COALESCE(sum(requests) FILTER(WHERE kind='%s' AND name=%s),0) AS matched
 FROM %s WHERE %s AND (kind='total' OR (kind='%s' AND name=%s)) GROUP BY bucket
 ), `, kind, parameter, table, strings.Join(conditions, " AND "), kind, parameter)
}

// Wide records in mostly mixed minutes are cheaper through the scalar decode
// and GROUPING SETS query than per-record dimension expansion. The guard reads
// compact summary counts only; both exact branches share its MVCC statement.
func managedUsageAdaptiveClassSQL(q managedLogQuery, fastSQL string, fastArgs []any, rawSQL string, rawArgs []any) (string, []any) {
	guardSQL, guardArgs := managedUsageRollupSourceWithMaterialization(q, false)
	unknown := "requests>0"
	if q.From > 0 {
		unknown += fmt.Sprintf(" AND bucket>=%d", (q.From+59)/60*60)
	}
	if q.To > 0 {
		unknown += fmt.Sprintf(" AND bucket<%d", q.To/60*60)
	}
	guardSQL += ` SELECT true AS matched,(work>256 AND work*5>total) AS different FROM (
 SELECT COALESCE(sum(m.matched),0) AS total,
 COALESCE(sum(m.matched) FILTER(WHERE u.bucket IS NOT NULL),0)
 +(SELECT COALESCE(sum(requests),0) FROM retained_usage_unsafe_minute WHERE ` + unknown + `) AS work
 FROM class_minutes m LEFT JOIN unsafe u USING(bucket)) counts`
	return managedUsageGuardedBranchesSQL(guardSQL, guardArgs, fastSQL, fastArgs, rawSQL, rawArgs)
}

// The exact retained functions operate on the original JSON value types.
// Fetch and project the original only when the permanent projection is missing.
// Keeping that fallback in the same statement preserves its MVCC visibility
// without carrying wide original JSON through the raw_rows materialization.
func managedUsageRawRollupProjectionSQL() string {
	fields := append([]string{"groups"}, managedUsageDecodedTextFields...)
	quoted := make([]string, len(fields))
	for i, field := range fields {
		quoted[i] = "'" + field + "'"
	}
	return `raw_projected AS MATERIALIZED (
 SELECT r.created_at,r.status,r.model,r.account_name,r.key_name,
 CASE WHEN analytics.log_id IS NOT NULL THEN analytics.record ELSE
 (SELECT COALESCE(jsonb_object_agg(key,value),'{}'::jsonb) FROM request_logs original
 CROSS JOIN LATERAL jsonb_each(CASE WHEN jsonb_typeof(original.record)='object' THEN original.record ELSE '{}'::jsonb END)
 WHERE original.id=r.id AND key IN (` + strings.Join(quoted, ",") + `)) END AS record FROM raw_rows r
 LEFT JOIN request_log_analytics analytics ON analytics.log_id=r.id
 ), `
}
