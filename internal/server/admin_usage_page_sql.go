package server

import "fmt"

func managedLogPageSQL(q managedLogQuery, source, where string, args []any) (string, []any) {
	params := append([]any{}, args...)
	limit, offset := q.Size+1, (q.Page-1)*q.Size
	order := q.orderSQL()
	if q.SortBy != "model" {
		// Size/page are parsed bounded integers, not raw SQL. Literal bounds
		// let generic plans cost a real page instead of assuming LIMIT $n may
		// need a large fraction of every matching bitmap before sorting.
		return fmt.Sprintf("SELECT id,record FROM %s WHERE %s ORDER BY %s LIMIT %d OFFSET %d", source, where, order, limit, offset), params
	}
	// Text larger than a BTree item must remain readable and writable. Take at
	// most offset+page from each disjoint population, then merge their original
	// total order. The long population has its own small partial index.
	branchLimit := offset + limit
	branch := func(length string) string {
		return fmt.Sprintf("(SELECT id,record,model,created_at FROM %s WHERE %s AND octet_length(model)%s1700 ORDER BY %s LIMIT %d)", source, where, length, order, branchLimit)
	}
	statement := fmt.Sprintf("SELECT id,record FROM (%s UNION ALL %s) sorted_models ORDER BY %s LIMIT %d OFFSET %d", branch("<="), branch(">"), order, limit, offset)
	return statement, params
}
