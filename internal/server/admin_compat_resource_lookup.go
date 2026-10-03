package server

import (
	"context"
	"strconv"
	"time"
)

// This is the exact first 48 bits used by compatNumericID, plus one. The
// compatibility ID is not an indexed native ID, but the database only hashes
// the ID column and returns matching metadata. Unrelated descriptions and
// proxy fields are no longer loaded/decrypted to resolve one resource.
const sub2ResourceNumericIDSQL = `(('x'||substr(encode(sha256(convert_to('admin-compat-id:'||id,'UTF8')),'hex'),1,12))::bit(48)::bigint+1)`

func (p *Proxy) sub2ResourceMetadataPG(ctx context.Context, ref string, group bool) (Group, ProxyEntry, error) {
	var g Group
	var proxy ProxyEntry
	id, err := strconv.ParseInt(ref, 10, 64)
	if err != nil || id <= 0 {
		return g, proxy, errInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	table, columns := "proxies", proxyColumns
	if group {
		table, columns = "groups", `id,name,description,extract(epoch from created_at)::bigint,extract(epoch from updated_at)::bigint,version`
	}
	rows, err := p.Store.DB.Pool.Query(ctx, "SELECT "+columns+" FROM "+table+" WHERE "+sub2ResourceNumericIDSQL+"=$1 LIMIT 2", id)
	if err != nil {
		return g, proxy, safeDBError(err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		count++
		if count > 1 {
			return g, proxy, errConflict
		}
		if group {
			err = rows.Scan(&g.ID, &g.Name, &g.Description, &g.CreatedAt, &g.UpdatedAt, &g.Version)
		} else {
			err = scanProxy(rows, &proxy, nil)
		}
		if err != nil {
			return g, proxy, safeDBError(err)
		}
	}
	if err := rows.Err(); err != nil {
		return g, proxy, safeDBError(err)
	}
	if count == 0 {
		return g, proxy, errNotFound
	}
	return g, proxy, nil
}
