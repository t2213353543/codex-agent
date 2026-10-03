package server

// Model-conditioned summaries retain all output dimensions within each model.
// Additional conditions must still be absent or universal inside that model;
// partially overlapping key/group/endpoint predicates use exact raw records.
func managedUsageModelFilterSQL(q managedLogQuery, out managedUsageStats, rawSQL string, rawArgs []any) (string, []any, bool) {
	if q.Model == "" {
		return rawSQL, rawArgs, false
	}
	base, filters, eligible := managedUsageConstantFilters(q)
	if !eligible {
		return rawSQL, rawArgs, false
	}
	base.Model = q.Model
	remaining := make([]managedUsageDimensionFilter, 0, len(filters)-1)
	for _, filter := range filters {
		if filter.kind != "model" {
			remaining = append(remaining, filter)
		}
	}
	fastSQL, fastArgs := managedUsageRollupQuery(base, out)
	if len(remaining) == 0 {
		return fastSQL, fastArgs, true
	}
	guardSQL, guardArgs := managedUsageDimensionGateSQL(base, remaining)
	statement, args := managedUsageGuardedBranchesSQL(guardSQL, guardArgs, fastSQL, fastArgs, rawSQL, rawArgs)
	return statement, args, true
}
