package server

import (
	"context"
	"time"
)

// A native account bundle only needs to validate its referenced local IDs.
// Mapping entries that no account uses do not make unrelated resources part
// of the import. PostgreSQL still validates associations again when it writes
// each account, so a concurrent resource deletion cannot create a dangling ID.
func (p *Proxy) managedBundleBindingIDs(ctx context.Context, bundle managedAccountImportBundle, groupMapping, proxyMapping map[string]string) (map[string]bool, map[string]bool, error) {
	groups, proxies := map[string]bool{}, map[string]bool{}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if p.Store.DB == nil {
		allGroups, err := p.Groups.List()
		if err != nil {
			return nil, nil, err
		}
		allProxies, err := p.Proxies.ListMetadata()
		if err != nil {
			return nil, nil, err
		}
		for _, group := range allGroups {
			groups[group.ID] = true
		}
		for _, proxy := range allProxies {
			proxies[proxy.ID] = true
		}
		return groups, proxies, nil
	}
	wantedGroups, wantedProxies := map[string]struct{}{}, map[string]struct{}{}
	add := func(wanted map[string]struct{}, references []string, mapping map[string]string) {
		for _, id := range references {
			if mapped, ok := mapping[id]; ok {
				id = mapped // An explicit empty mapping remains invalid.
			}
			wanted[id] = struct{}{}
		}
	}
	for _, account := range bundle.Accounts {
		add(wantedGroups, account.Groups, groupMapping)
		add(wantedProxies, account.Proxies, proxyMapping)
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	read := func(table string, wanted map[string]struct{}, found map[string]bool) error {
		if len(wanted) == 0 {
			return nil
		}
		ids := make([]string, 0, len(wanted))
		for id := range wanted {
			ids = append(ids, id)
		}
		// table is one of the two fixed names below, never request data.
		rows, err := p.Store.DB.Pool.Query(ctx, "SELECT id FROM "+table+" WHERE id=ANY($1::text[])", ids)
		if err != nil {
			return safeDBError(err)
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return safeDBError(err)
			}
			found[id] = true
		}
		return safeDBError(rows.Err())
	}
	if err := read("groups", wantedGroups, groups); err != nil {
		return nil, nil, err
	}
	if err := read("proxies", wantedProxies, proxies); err != nil {
		return nil, nil, err
	}
	return groups, proxies, nil
}
