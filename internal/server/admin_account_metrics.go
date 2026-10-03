package server

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Summary callers already hold account metadata and stable IDs. Resolving the
// IDs again and decoding historical request records does not contribute to
// usage; current credential status is read separately by accountSummaries.
func (m *Metrics) managedAccountUsage(ctx context.Context, starts map[string]int64) (map[string]AccountMetricsSummary, error) {
	result := make(map[string]AccountMetricsSummary, len(starts))
	if len(starts) > 1000 {
		return nil, errors.New("account metrics batch exceeds 1000 accounts")
	}
	if m.db == nil {
		for ref, start := range starts {
			result[ref] = AccountMetricsSummary{Usage: m.AccountWindow(ref, start)}
		}
		return result, ctx.Err()
	}
	refs := make([]string, 0, len(starts))
	bounds := make([]int64, 0, len(starts))
	minStart := int64(1<<63 - 1)
	for ref, start := range starts {
		start = start / 86400 * 86400
		refs = append(refs, ref)
		bounds = append(bounds, start)
		minStart = min(minStart, start)
		result[ref] = AccountMetricsSummary{}
	}
	if len(refs) == 0 {
		return result, ctx.Err()
	}
	rows, err := m.db.Pool.Query(ctx, `WITH wanted AS (SELECT * FROM unnest($1::text[],$2::bigint[]) AS w(account_ref,start_at)),
		selected AS MATERIALIZED (SELECT * FROM usage_daily WHERE account_ref=ANY($1::text[]) AND bucket >= $3)
		SELECT w.account_ref,`+metricsSums+` FROM wanted w JOIN selected u ON u.account_ref=w.account_ref AND u.bucket>=w.start_at GROUP BY w.account_ref`, refs, bounds, minStart)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var ref string
		var totals rangeTotals
		if err := rows.Scan(append([]any{&ref}, totalsTargets(&totals)...)...); err != nil {
			return nil, err
		}
		result[ref] = AccountMetricsSummary{Usage: dayCounts(totals)}
	}
	return result, rows.Err()
}

// Read only names referenced by this page. Proxy credentials and unrelated
// resources need not be read or decoded to render account bindings.
func (p *Proxy) managedAccountBindingNames(ctx context.Context, accounts []Account) (map[string]string, map[string]string, error) {
	groupIDs, proxyIDs := map[string]struct{}{}, map[string]struct{}{}
	for _, account := range accounts {
		for _, id := range account.Groups {
			groupIDs[id] = struct{}{}
		}
		for _, id := range account.Proxies {
			proxyIDs[id] = struct{}{}
		}
	}
	groups, proxies := map[string]string{}, map[string]string{}
	if len(groupIDs) > 0 {
		var err error
		groups, err = p.Groups.managedNames(ctx, groupIDs)
		if err != nil {
			return nil, nil, err
		}
	}
	if len(proxyIDs) > 0 {
		if p.Proxies.DB != nil {
			var err error
			proxies, err = managedBindingNamesPG(ctx, p.Proxies.DB.Pool, "proxies", proxyIDs)
			if err != nil {
				return nil, nil, err
			}
		} else {
			entries, err := p.Proxies.managedMetadata()
			if err != nil {
				return nil, nil, err
			}
			for _, entry := range entries {
				proxies[entry.ID] = entry.Name
			}
		}
	}
	return groups, proxies, nil
}

// File-mode ListMetadata refreshes the shared proxy index before redacting its
// backing slice. Management reads must neither rebuild that index nor erase the
// credentials that active gateway requests read from it.
func (p *ProxyStore) managedMetadata() ([]ProxyEntry, error) {
	if p.DB != nil {
		return p.ListMetadata()
	}
	state, err := p.store.Read()
	if err != nil {
		return nil, err
	}
	for i := range state.Proxies {
		state.Proxies[i].HasPassword = state.Proxies[i].HasPassword || state.Proxies[i].Password != ""
		state.Proxies[i].Password = ""
	}
	return state.Proxies, nil
}

// table is one of the two constant names above, never request input.
func managedBindingNamesPG(ctx context.Context, pool *pgxpool.Pool, table string, wanted map[string]struct{}) (map[string]string, error) {
	ids := make([]string, 0, len(wanted))
	for id := range wanted {
		ids = append(ids, id)
	}
	rows, err := pool.Query(ctx, "SELECT id,name FROM "+table+" WHERE id=ANY($1::text[])", ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	names := make(map[string]string, len(ids))
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		names[id] = name
	}
	return names, rows.Err()
}
