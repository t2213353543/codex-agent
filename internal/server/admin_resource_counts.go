package server

import (
	"context"
	"time"
)

// Resource lists only need relationship counts. Loading every account's
// metadata and credential status makes their cost grow with unrelated fields.
func (p *Proxy) managedAssociationCounts(ctx context.Context, proxies bool) (map[string]int, error) {
	counts := make(map[string]int)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if db := p.Store.DB; db != nil {
		ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		query := "SELECT group_id,count(*) FROM account_groups GROUP BY group_id"
		if proxies {
			query = "SELECT proxy_id,count(*) FROM account_proxies GROUP BY proxy_id"
		}
		rows, err := db.Pool.Query(ctx, query)
		if err != nil {
			return nil, safeDBError(err)
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			var count int
			if err := rows.Scan(&id, &count); err != nil {
				return nil, safeDBError(err)
			}
			counts[id] = count
		}
		return counts, safeDBError(rows.Err())
	}
	_, accounts, err := p.Store.ListMetadata()
	if err != nil {
		return nil, err
	}
	for _, account := range accounts {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if proxies {
			for _, id := range account.Proxies {
				counts[id]++
			}
		} else {
			addManagedGroupCounts(counts, account.Groups)
		}
	}
	return counts, nil
}

// Legacy documents may contain duplicate group IDs; count each entity once,
// matching the list response before aggregation was moved out of its loop.
func addManagedGroupCounts(counts map[string]int, ids []string) {
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		counts[id]++
	}
}

func (p *Proxy) managedAccountCounts(ctx context.Context) (total, enabled int, err error) {
	if err = ctx.Err(); err != nil {
		return
	}
	if db := p.Store.DB; db != nil {
		ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		err = safeDBError(db.Pool.QueryRow(ctx, "SELECT count(*),count(*) FILTER(WHERE enabled) FROM accounts").Scan(&total, &enabled))
		return
	}
	_, accounts, err := p.Store.ListMetadata()
	if err != nil {
		return 0, 0, err
	}
	for _, account := range accounts {
		if account.Enabled {
			enabled++
		}
	}
	return len(accounts), enabled, ctx.Err()
}

// One query replaces a usage_totals round trip for every displayed API key.
// Errors are surfaced instead of rendering successful zero usage on failures.
func (m *Metrics) managedKeyUsage(ctx context.Context, keys []APIKey) (map[string]usageCounts, error) {
	result := make(map[string]usageCounts, len(keys))
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(keys) == 0 {
		return result, nil
	}
	if m.db == nil {
		m.mu.Lock()
		defer m.mu.Unlock()
		for _, key := range keys {
			if usage := m.keyUsage[key.ID]; usage != nil {
				result[key.ID] = *usage
			}
		}
		return result, nil
	}
	ids := make([]string, len(keys))
	for i, key := range keys {
		ids[i] = key.ID
	}
	ctx, cancel := context.WithTimeout(ctx, metricsTimeout)
	defer cancel()
	rows, err := m.db.Pool.Query(ctx, "SELECT subject,requests,tokens,errors,last_at FROM usage_totals WHERE scope='key' AND subject=ANY($1::text[])", ids)
	if err != nil {
		m.reportReadError(err)
		return nil, safeDBError(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var value usageCounts
		if err := rows.Scan(&id, &value.Requests, &value.Tokens, &value.Errors, &value.LastAt); err != nil {
			m.reportReadError(err)
			return nil, safeDBError(err)
		}
		result[id] = value
	}
	if err := rows.Err(); err != nil {
		m.reportReadError(err)
		return nil, safeDBError(err)
	}
	return result, nil
}
