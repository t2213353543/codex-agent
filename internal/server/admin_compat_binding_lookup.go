package server

import (
	"context"
	"time"
)

// Account writes historically resolve compatibility IDs by iterating the
// ordered resource catalogue and retaining its last match. Keep that collision
// rule here; standalone resource GET has its own ambiguity rejection contract.
// PostgreSQL returns just requested identities instead of hydrating every
// unrelated proxy/group for each imported account.
func (p *Proxy) sub2BindingIDs(ctx context.Context, ids []int64, group bool) (map[int64]string, error) {
	result := make(map[int64]string, len(ids))
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if p.Store.DB == nil {
		wanted := make(map[int64]bool, len(ids))
		for _, id := range ids {
			wanted[id] = true
		}
		if group {
			rows, err := p.Groups.List()
			if err != nil {
				return nil, err
			}
			for _, row := range rows {
				if id := compatNumericID(row.ID); wanted[id] {
					result[id] = row.ID
				}
			}
		} else {
			rows, err := p.Proxies.managedMetadata()
			if err != nil {
				return nil, err
			}
			for _, row := range rows {
				if id := compatNumericID(row.ID); wanted[id] {
					result[id] = row.ID
				}
			}
		}
		return result, nil
	}
	if len(ids) == 0 {
		return result, nil
	}
	table := "proxies"
	if group {
		table = "groups"
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	rows, err := p.Store.DB.Pool.Query(ctx, "SELECT id FROM "+table+" WHERE "+sub2ResourceNumericIDSQL+"=ANY($1::bigint[]) ORDER BY created_at,id", ids)
	if err != nil {
		return nil, safeDBError(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, safeDBError(err)
		}
		result[compatNumericID(id)] = id
	}
	return result, safeDBError(rows.Err())
}
