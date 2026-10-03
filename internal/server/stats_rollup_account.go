package server

import (
	"fmt"
	"time"
)

func managedAccountProjectionBase(q managedLogQuery) (managedLogQuery, bool) {
	base := q
	if !q.accountsOnly || len(q.AccountIDs) == 0 {
		return base, false
	}
	for _, id := range q.AccountIDs {
		if id == "" {
			return base, false
		}
	}
	base.accountsOnly, base.AccountIDs = false, nil
	return base, managedUsageRollupEligible(base)
}

func managedUsageAccountProjectionSQL(q managedLogQuery, rawSQL string, rawArgs []any) (string, []any, bool) {
	base, eligible := managedAccountProjectionBase(q)
	if !eligible {
		return rawSQL, rawArgs, false
	}
	// A mismatch outside the selected accounts is conservatively enough to
	// reject the projection, because JSON IDs could otherwise leak a different
	// account_ref into the selected dimension. Time bounds remain exact.
	// Preserve the ordered, bounded probe below EXISTS. Without this subquery
	// PostgreSQL can discard ORDER BY and estimate a mismatch will be found on
	// the first heap page, scanning all ten million healthy rows instead of
	// the empty mismatch partial index. OFFSET 0 preserves the LIMIT's order in
	// both custom and generic prepared plans, without a session planner change.
	guard := "SELECT NOT EXISTS(SELECT 1 FROM (SELECT created_at FROM request_logs WHERE account_ref IS DISTINCT FROM COALESCE(record->>'account_id','')"
	guardArgs := []any{}
	add := func(expr string, value any) {
		guardArgs = append(guardArgs, value)
		guard += fmt.Sprintf(expr, len(guardArgs))
	}
	if base.From != 0 {
		add(" AND created_at >= $%d", time.Unix(base.From, 0).UTC())
	}
	if base.To != 0 {
		add(" AND created_at < $%d", time.Unix(base.To, 0).UTC())
	}
	if base.Status == "failed" {
		guard += " AND status<>'ok'"
	} else if base.Status != "" {
		add(" AND status=$%d", base.Status)
	}
	guard += " ORDER BY created_at LIMIT 1 OFFSET 0) inconsistent) AS consistent"
	base.Step = 0
	source, fastArgs := managedUsageRollupSourceWithMaterialization(base, false)
	fastArgs = append(fastArgs, q.AccountIDs)
	fastSQL := source + ` SELECT 'account',max(name COLLATE "C"),resource_id,0::bigint,` + managedUsageRollupAggregateSQL("0", "0") + fmt.Sprintf(` FROM source WHERE kind='account' AND resource_id=ANY($%d::text[]) GROUP BY resource_id`, len(fastArgs))
	// Use the same caller-visible account label/ID cleanup as the original raw
	// account aggregate (including its historical name: prefix behavior).
	guard = managedSQLShiftParameters(guard, len(rawArgs))
	fastSQL = managedSQLShiftParameters(fastSQL, len(rawArgs)+len(guardArgs))
	args := append(append(append([]any{}, rawArgs...), guardArgs...), fastArgs...)
	statement := `WITH account_identity_guard AS MATERIALIZED (` + guard + `)
 SELECT result.* FROM account_identity_guard g CROSS JOIN LATERAL (
 SELECT candidate.* FROM (` + fastSQL + `) candidate WHERE g.consistent OFFSET 0
 ) result
 UNION ALL SELECT result.* FROM account_identity_guard g CROSS JOIN LATERAL (
 SELECT candidate.* FROM (` + rawSQL + `) candidate WHERE NOT g.consistent OFFSET 0
 ) result`
	return statement, args, true
}
