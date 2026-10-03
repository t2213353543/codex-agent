package server

import (
	"context"
	"time"
)

// Overview needs a count, so descriptions and other metadata stay in storage.
func (g *GroupStore) managedCount(ctx context.Context) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if g.DB == nil {
		groups, err := g.List()
		return len(groups), err
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var count int
	err := g.DB.Pool.QueryRow(ctx, "SELECT count(*) FROM groups").Scan(&count)
	return count, safeDBError(err)
}

// Account and key bindings only need the referenced IDs and names, not every
// group's description, timestamps and version.
func (g *GroupStore) managedNames(ctx context.Context, wanted map[string]struct{}) (map[string]string, error) {
	names := make(map[string]string, len(wanted))
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(wanted) == 0 {
		return names, nil
	}
	if g.DB != nil {
		ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		return managedBindingNamesPG(ctx, g.DB.Pool, "groups", wanted)
	}
	groups, err := g.List()
	if err != nil {
		return nil, err
	}
	for _, group := range groups {
		if _, ok := wanted[group.ID]; ok {
			names[group.ID] = group.Name
		}
	}
	return names, nil
}

// managedSelectedMetadata reads only the referenced groups, description
// included, for resource exports: an export must not load the whole group
// directory to serialize the handful of groups its accounts reference.
func (g *GroupStore) managedSelectedMetadata(ctx context.Context, wanted map[string]struct{}) ([]Group, error) {
	result := []Group{}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(wanted) == 0 {
		return result, nil
	}
	if g.DB == nil {
		groups, err := g.List()
		if err != nil {
			return nil, err
		}
		for _, group := range groups {
			if _, ok := wanted[group.ID]; ok {
				result = append(result, group)
			}
		}
		return result, nil
	}
	ids := make([]string, 0, len(wanted))
	for id := range wanted {
		ids = append(ids, id)
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	rows, err := g.DB.Pool.Query(ctx, `SELECT id,name,description,extract(epoch from created_at)::bigint,extract(epoch from updated_at)::bigint,version FROM groups WHERE id=ANY($1::text[]) ORDER BY created_at,id`, ids)
	if err != nil {
		return nil, safeDBError(err)
	}
	defer rows.Close()
	for rows.Next() {
		var group Group
		if err := rows.Scan(&group.ID, &group.Name, &group.Description, &group.CreatedAt, &group.UpdatedAt, &group.Version); err != nil {
			return nil, safeDBError(err)
		}
		result = append(result, group)
	}
	return result, safeDBError(rows.Err())
}

// Read-only binding views must not scan unrelated proxies or rebuild the live
// routing index. Password ciphertext is never selected for metadata rendering.
func (p *ProxyStore) managedSelectedMetadata(ctx context.Context, wanted map[string]struct{}) ([]ProxyEntry, error) {
	result := []ProxyEntry{}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(wanted) == 0 {
		return result, nil
	}
	if p.DB == nil {
		entries, err := p.managedMetadata()
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if _, ok := wanted[entry.ID]; ok {
				result = append(result, entry)
			}
		}
		return result, nil
	}
	ids := make([]string, 0, len(wanted))
	for id := range wanted {
		ids = append(ids, id)
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	rows, err := p.DB.Pool.Query(ctx, "SELECT "+proxyColumns+" FROM proxies WHERE id=ANY($1::text[]) ORDER BY created_at,id", ids)
	if err != nil {
		return nil, safeDBError(err)
	}
	defer rows.Close()
	for rows.Next() {
		var entry ProxyEntry
		if err := scanProxy(rows, &entry, nil); err != nil {
			return nil, safeDBError(err)
		}
		result = append(result, entry)
	}
	return result, safeDBError(rows.Err())
}

// managedSelectedCredentials is reserved for explicit exports that include
// proxy credentials. It decrypts only selected IDs; metadata endpoints must
// continue using managedSelectedMetadata.
func (p *ProxyStore) managedSelectedCredentials(ctx context.Context, wanted map[string]struct{}) ([]ProxyEntry, error) {
	result := []ProxyEntry{}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(wanted) == 0 {
		return result, nil
	}
	if p.DB == nil {
		entries, err := p.List()
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if _, ok := wanted[entry.ID]; ok {
				result = append(result, entry)
			}
		}
		return result, nil
	}
	ids := make([]string, 0, len(wanted))
	for id := range wanted {
		ids = append(ids, id)
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	rows, err := p.DB.Pool.Query(ctx, "SELECT "+proxyColumns+",password_cipher FROM proxies WHERE id=ANY($1::text[]) ORDER BY created_at,id", ids)
	if err != nil {
		return nil, safeDBError(err)
	}
	defer rows.Close()
	for rows.Next() {
		var entry ProxyEntry
		var encrypted []byte
		if err := scanProxy(rows, &entry, &encrypted); err != nil {
			return nil, safeDBError(err)
		}
		if len(encrypted) > 0 {
			plain, err := p.DB.Decrypt(encrypted)
			if err != nil {
				return nil, err
			}
			entry.Password = string(plain)
		}
		result = append(result, entry)
	}
	return result, safeDBError(rows.Err())
}
