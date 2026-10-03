package server

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

func dbContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 20*time.Second)
}

func (f *fileStore[T]) modifyPG(change func(*T) error) error {
	ctx, cancel := dbContext()
	defer cancel()
	tx, err := f.db.Pool.Begin(ctx)
	if err != nil {
		return safeDBError(err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended(current_schema() || ':' || $1,0))", "document:"+f.key); err != nil {
		return safeDBError(err)
	}
	var value T
	if err = f.db.readDocument(ctx, tx, f.key, &value); err != nil {
		return err
	}
	if err = change(&value); err != nil && !errors.Is(err, errStoreNoChange) {
		return err
	}
	unchanged := errors.Is(err, errStoreNoChange)
	if keys, ok := any(&value).(*keyState); ok {
		var groups []string
		for _, key := range keys.Keys {
			groups = append(groups, key.Groups...)
		}
		groups = uniqueStrings(groups)
		if len(groups) > 0 {
			var count int
			if err = tx.QueryRow(ctx, "SELECT count(*) FROM groups WHERE id=ANY($1::text[])", groups).Scan(&count); err != nil {
				return safeDBError(err)
			}
			if count != len(groups) {
				return errInvalid
			}
		}
	}
	if !unchanged {
		if err = f.db.writeDocument(ctx, tx, f.key, value); err != nil {
			return err
		}
	}
	if err = f.db.audit(ctx, tx, "settings.update", f.key); err != nil {
		return safeDBError(err)
	}
	if err = tx.Commit(ctx); err != nil {
		return safeDBError(err)
	}
	if !unchanged {
		f.db.invalidate()
	}
	return nil
}

func NewPGGroupStore(db *Database) *GroupStore { return &GroupStore{DB: db} }
func NewPGProxyStore(db *Database) (*ProxyStore, error) {
	p := &ProxyStore{DB: db, index: map[string]*ProxyEntry{}}
	_, err := p.List()
	return p, err
}
func NewPGKeyStore(db *Database) (*KeyStore, error) {
	k := &KeyStore{store: &fileStore[keyState]{db: db, key: "gateway_keys"}, index: map[string]*APIKey{}}
	_, err := k.List()
	return k, err
}
func NewPGSettingsStore(db *Database) *SettingsStore {
	return &SettingsStore{store: &fileStore[settingsState]{db: db, key: "admin_settings"}}
}

func (g *GroupStore) getPG(id string) (Group, error) {
	ctx, cancel := dbContext()
	defer cancel()
	var v Group
	err := g.DB.Pool.QueryRow(ctx, `SELECT id,name,description,extract(epoch from created_at)::bigint,extract(epoch from updated_at)::bigint,version FROM groups WHERE id=$1`, id).Scan(&v.ID, &v.Name, &v.Description, &v.CreatedAt, &v.UpdatedAt, &v.Version)
	if errors.Is(err, pgx.ErrNoRows) {
		return Group{}, fmt.Errorf("%w: group %q", errNotFound, id)
	}
	return v, safeDBError(err)
}

func (g *GroupStore) listPG() ([]Group, error) {
	ctx, cancel := dbContext()
	defer cancel()
	rows, err := g.DB.Pool.Query(ctx, `SELECT id,name,description,extract(epoch from created_at)::bigint,extract(epoch from updated_at)::bigint,version FROM groups ORDER BY created_at,id`)
	if err != nil {
		return nil, safeDBError(err)
	}
	defer rows.Close()
	result := []Group{}
	for rows.Next() {
		var v Group
		if err = rows.Scan(&v.ID, &v.Name, &v.Description, &v.CreatedAt, &v.UpdatedAt, &v.Version); err != nil {
			return nil, safeDBError(err)
		}
		result = append(result, v)
	}
	return result, safeDBError(rows.Err())
}

// listPGPage keeps the compatibility list endpoint from materializing the
// complete group catalogue before applying its page window. The total remains
// exact because the public response exposes it; the data query itself is
// bounded by LIMIT/OFFSET.
func (g *GroupStore) listPGPage(search string, offset, limit int) ([]Group, int, error) {
	ctx, cancel := dbContext()
	defer cancel()
	tx, err := g.DB.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, 0, safeDBError(err)
	}
	defer tx.Rollback(context.Background())
	var total int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM groups WHERE ($1='' OR strpos(lower(name),lower($1))>0)`, search).Scan(&total); err != nil {
		return nil, 0, safeDBError(err)
	}
	rows, err := tx.Query(ctx, `SELECT id,name,description,extract(epoch from created_at)::bigint,extract(epoch from updated_at)::bigint,version
		FROM groups WHERE ($1='' OR strpos(lower(name),lower($1))>0) ORDER BY created_at,id LIMIT $2 OFFSET $3`, search, limit, offset)
	if err != nil {
		return nil, 0, safeDBError(err)
	}
	defer rows.Close()
	result := make([]Group, 0, min(limit, total))
	for rows.Next() {
		var v Group
		if err := rows.Scan(&v.ID, &v.Name, &v.Description, &v.CreatedAt, &v.UpdatedAt, &v.Version); err != nil {
			return nil, 0, safeDBError(err)
		}
		result = append(result, v)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, safeDBError(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, 0, safeDBError(err)
	}
	return result, total, nil
}
func (g *GroupStore) createPG(name, description string) (Group, error) {
	if err := validGroupName(name); err != nil {
		return Group{}, err
	}
	ctx, cancel := dbContext()
	defer cancel()
	tx, err := g.DB.Pool.Begin(ctx)
	if err != nil {
		return Group{}, safeDBError(err)
	}
	defer tx.Rollback(ctx)
	v := Group{ID: newID("g_"), Name: name, Description: description}
	// Return the same persisted timestamp projection as Get/List/Update. The
	// database defaults carry fractional seconds and can use a different clock.
	err = tx.QueryRow(ctx, `INSERT INTO groups(id,name,description) VALUES($1,$2,$3)
		RETURNING extract(epoch from created_at)::bigint,extract(epoch from updated_at)::bigint,version`, v.ID, v.Name, v.Description).Scan(&v.CreatedAt, &v.UpdatedAt, &v.Version)
	if err != nil {
		return Group{}, safeDBError(err)
	}
	if err = g.DB.audit(ctx, tx, "group.create", v.ID); err != nil {
		return Group{}, safeDBError(err)
	}
	err = tx.Commit(ctx)
	g.DB.invalidate()
	return v, safeDBError(err)
}
func (g *GroupStore) UpdateVersion(id, name, description string, expected int64) (Group, error) {
	if g.DB == nil {
		return g.updateFileVersion(id, name, description, expected)
	}
	if err := validGroupName(name); err != nil {
		return Group{}, err
	}
	ctx, cancel := dbContext()
	defer cancel()
	tx, err := g.DB.Pool.Begin(ctx)
	if err != nil {
		return Group{}, safeDBError(err)
	}
	defer tx.Rollback(ctx)
	var v Group
	err = tx.QueryRow(ctx, `SELECT id,name,description,extract(epoch from created_at)::bigint,extract(epoch from updated_at)::bigint,version FROM groups WHERE id=$1 FOR UPDATE`, id).Scan(&v.ID, &v.Name, &v.Description, &v.CreatedAt, &v.UpdatedAt, &v.Version)
	if errors.Is(err, pgx.ErrNoRows) {
		return v, errNotFound
	}
	if err != nil {
		return v, safeDBError(err)
	}
	if expected != 0 && expected != v.Version {
		return v, errVersionConflict
	}
	changed := v.Name != name || v.Description != description
	if changed {
		err = tx.QueryRow(ctx, `UPDATE groups SET name=$2,description=$3,updated_at=now(),version=version+1 WHERE id=$1 RETURNING id,name,description,extract(epoch from created_at)::bigint,extract(epoch from updated_at)::bigint,version`, id, name, description).Scan(&v.ID, &v.Name, &v.Description, &v.CreatedAt, &v.UpdatedAt, &v.Version)
		if err != nil {
			return v, safeDBError(err)
		}
	}
	if err = g.DB.audit(ctx, tx, "group.update", id); err != nil {
		return v, safeDBError(err)
	}
	err = tx.Commit(ctx)
	if changed && err == nil {
		g.DB.invalidate()
	}
	return v, safeDBError(err)
}
func (g *GroupStore) deletePG(id string) error {
	ctx, cancel := dbContext()
	defer cancel()
	tx, err := g.DB.Pool.Begin(ctx)
	if err != nil {
		return safeDBError(err)
	}
	defer tx.Rollback(ctx)
	// Same document lock as gateway-key writers: prevent a concurrent write resurrecting a removed association.
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended(current_schema() || ':document:gateway_keys',0))"); err != nil {
		return safeDBError(err)
	}
	var keys keyState
	if err = g.DB.readDocument(ctx, tx, "gateway_keys", &keys); err != nil {
		return err
	}
	keysChanged := false
	for i := range keys.Keys {
		previous := len(keys.Keys[i].Groups)
		keys.Keys[i].Groups = removeString(keys.Keys[i].Groups, id)
		keysChanged = keysChanged || previous != len(keys.Keys[i].Groups)
	}
	if keysChanged {
		if err = g.DB.writeDocument(ctx, tx, "gateway_keys", keys); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(ctx, `UPDATE accounts SET version=version+1,updated_at=extract(epoch from now())::bigint WHERE id IN(SELECT account_id FROM account_groups WHERE group_id=$1)`, id); err != nil {
		return safeDBError(err)
	}
	tag, err := tx.Exec(ctx, "DELETE FROM groups WHERE id=$1", id)
	if err != nil {
		return safeDBError(err)
	}
	if tag.RowsAffected() == 0 {
		return errNotFound
	}
	if err = g.DB.audit(ctx, tx, "group.delete", id); err != nil {
		return safeDBError(err)
	}
	err = tx.Commit(ctx)
	g.DB.invalidate()
	return safeDBError(err)
}

const proxyColumns = `id,name,type,host,port,username,enabled,extract(epoch from created_at)::bigint,extract(epoch from updated_at)::bigint,version,octet_length(password_cipher)>0`

func scanProxy(row interface{ Scan(...any) error }, v *ProxyEntry, encrypted *[]byte) error {
	args := []any{&v.ID, &v.Name, &v.Type, &v.Host, &v.Port, &v.Username, &v.Enabled, &v.CreatedAt, &v.UpdatedAt, &v.Version, &v.HasPassword}
	if encrypted != nil {
		args = append(args, encrypted)
	}
	return row.Scan(args...)
}
func (p *ProxyStore) listPG(secret bool) ([]ProxyEntry, error) {
	ctx, cancel := dbContext()
	defer cancel()
	columns := proxyColumns
	if secret {
		columns += ",password_cipher"
	}
	rows, err := p.DB.Pool.Query(ctx, "SELECT "+columns+" FROM proxies ORDER BY created_at,id")
	if err != nil {
		return nil, safeDBError(err)
	}
	defer rows.Close()
	result := []ProxyEntry{}
	for rows.Next() {
		var v ProxyEntry
		var encrypted []byte
		var target *[]byte
		if secret {
			target = &encrypted
		}
		if err = scanProxy(rows, &v, target); err != nil {
			return nil, safeDBError(err)
		}
		if len(encrypted) > 0 {
			plain, e := p.DB.Decrypt(encrypted)
			if e != nil {
				return nil, e
			}
			v.Password = string(plain)
		}
		result = append(result, v)
	}
	if err = rows.Err(); err != nil {
		return nil, safeDBError(err)
	}
	if secret {
		p.mu.Lock()
		p.index = map[string]*ProxyEntry{}
		for i := range result {
			v := result[i]
			p.index[v.ID] = &v
		}
		p.mu.Unlock()
	}
	return result, nil
}

// listPGMetadataPage is the bounded counterpart used by the Sub2 metadata
// list. Secrets are deliberately excluded, matching ListMetadata and avoiding
// unnecessary decryptions on a paginated read.
func (p *ProxyStore) listPGMetadataPage(status, protocol, search string, offset, limit int) ([]ProxyEntry, int, error) {
	ctx, cancel := dbContext()
	defer cancel()
	tx, err := p.DB.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, 0, safeDBError(err)
	}
	defer tx.Rollback(context.Background())
	var total int
	const where = `($1='' OR enabled=($1='active')) AND ($2='' OR type=$2) AND ($3='' OR strpos(lower(name),lower($3))>0 OR strpos(lower(host),lower($3))>0)`
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM proxies WHERE `+where, status, protocol, search).Scan(&total); err != nil {
		return nil, 0, safeDBError(err)
	}
	rows, err := tx.Query(ctx, `SELECT `+proxyColumns+` FROM proxies WHERE `+where+` ORDER BY created_at,id LIMIT $4 OFFSET $5`, status, protocol, search, limit, offset)
	if err != nil {
		return nil, 0, safeDBError(err)
	}
	defer rows.Close()
	result := make([]ProxyEntry, 0, min(limit, total))
	for rows.Next() {
		var v ProxyEntry
		if err := scanProxy(rows, &v, nil); err != nil {
			return nil, 0, safeDBError(err)
		}
		result = append(result, v)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, safeDBError(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, 0, safeDBError(err)
	}
	return result, total, nil
}
func (p *ProxyStore) ListMetadata() ([]ProxyEntry, error) {
	if p.DB != nil {
		return p.listPG(false)
	}
	entries, err := p.List()
	for i := range entries {
		entries[i].HasPassword = entries[i].Password != ""
		entries[i].Password = ""
	}
	return entries, err
}
func (p *ProxyStore) GetMetadata(id string) (ProxyEntry, error) {
	if p.DB == nil {
		v, err := p.GetForAdmin(id)
		v.HasPassword = v.Password != ""
		v.Password = ""
		return v, err
	}
	ctx, cancel := dbContext()
	defer cancel()
	var v ProxyEntry
	err := scanProxy(p.DB.Pool.QueryRow(ctx, "SELECT "+proxyColumns+" FROM proxies WHERE id=$1", id), &v, nil)
	if errors.Is(err, pgx.ErrNoRows) {
		return v, errNotFound
	}
	return v, safeDBError(err)
}
func (p *ProxyStore) GetForAdmin(id string) (ProxyEntry, error) {
	if p.DB == nil {
		entries, err := p.List()
		if err != nil {
			return ProxyEntry{}, err
		}
		for _, v := range entries {
			if v.ID == id {
				return v, nil
			}
		}
		return ProxyEntry{}, errNotFound
	}
	ctx, cancel := dbContext()
	defer cancel()
	var v ProxyEntry
	var encrypted []byte
	err := scanProxy(p.DB.Pool.QueryRow(ctx, "SELECT "+proxyColumns+",password_cipher FROM proxies WHERE id=$1", id), &v, &encrypted)
	if errors.Is(err, pgx.ErrNoRows) {
		return v, errNotFound
	}
	if err != nil {
		return v, safeDBError(err)
	}
	if len(encrypted) > 0 {
		plain, e := p.DB.Decrypt(encrypted)
		if e != nil {
			return v, e
		}
		v.Password = string(plain)
	}
	return v, nil
}
func (p *ProxyStore) createPG(v ProxyEntry) (ProxyEntry, error) {
	if err := validProxy(v); err != nil {
		return ProxyEntry{}, err
	}
	ctx, cancel := dbContext()
	defer cancel()
	tx, err := p.DB.Pool.Begin(ctx)
	if err != nil {
		return v, safeDBError(err)
	}
	defer tx.Rollback(ctx)
	v.ID = newID("p_")
	v.CreatedAt = time.Now().Unix()
	v.UpdatedAt = v.CreatedAt
	v.Version = 1
	v.Enabled = true
	v.HasPassword = v.Password != ""
	encrypted := []byte{}
	if v.HasPassword {
		encrypted, err = p.DB.Encrypt([]byte(v.Password))
		if err != nil {
			return v, err
		}
	}
	_, err = tx.Exec(ctx, `INSERT INTO proxies(id,name,type,host,port,username,password_cipher,enabled) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, v.ID, v.Name, v.Type, v.Host, v.Port, v.Username, encrypted, v.Enabled)
	if err != nil {
		return ProxyEntry{}, safeDBError(err)
	}
	if err = p.DB.audit(ctx, tx, "proxy.create", v.ID); err != nil {
		return ProxyEntry{}, safeDBError(err)
	}
	if err = tx.Commit(ctx); err != nil {
		return ProxyEntry{}, safeDBError(err)
	}
	p.mu.Lock()
	if current := p.index[v.ID]; current == nil || current.Version <= v.Version {
		cached := v
		p.index[v.ID] = &cached
	}
	p.mu.Unlock()
	p.DB.invalidate()
	return v, nil
}
func (p *ProxyStore) UpdateVersion(id string, expected int64, apply func(*ProxyEntry) error) (ProxyEntry, error) {
	if p.DB == nil {
		return p.Update(id, func(v *ProxyEntry) error {
			if expected > 0 && v.Version != expected {
				return errVersionConflict
			}
			original := *v
			if err := apply(v); err != nil {
				return err
			}
			if *v == original && original.Version > 0 {
				return nil
			}
			// Legacy file proxies begin at version0. Establishing their first
			// real CAS revision is a persisted normalization, not a no-op.
			v.Version = max(int64(1), v.Version+1)
			v.UpdatedAt = time.Now().Unix()
			return nil
		})
	}
	v, err := p.GetForAdmin(id)
	if err != nil {
		return v, err
	}
	if expected > 0 && v.Version != expected {
		return ProxyEntry{}, errVersionConflict
	}
	prior := v
	original := v.Version
	if err = apply(&v); err != nil {
		return ProxyEntry{}, err
	}
	if err = validProxy(v); err != nil {
		return ProxyEntry{}, err
	}
	v.HasPassword = v.Password != ""
	changed := v != prior
	passwordChanged := v.Password != prior.Password
	encrypted := []byte{}
	if passwordChanged && v.HasPassword {
		encrypted, err = p.DB.Encrypt([]byte(v.Password))
		if err != nil {
			return ProxyEntry{}, err
		}
	}
	ctx, cancel := dbContext()
	defer cancel()
	tx, err := p.DB.Pool.Begin(ctx)
	if err != nil {
		return ProxyEntry{}, safeDBError(err)
	}
	defer tx.Rollback(ctx)
	if changed {
		tag, err := tx.Exec(ctx, `UPDATE proxies SET name=$2,type=$3,host=$4,port=$5,username=$6,password_cipher=CASE WHEN $10 THEN $7::bytea ELSE password_cipher END,enabled=$8,version=version+1,updated_at=now() WHERE id=$1 AND version=$9`, id, v.Name, v.Type, v.Host, v.Port, v.Username, encrypted, v.Enabled, original, passwordChanged)
		if err != nil {
			return ProxyEntry{}, safeDBError(err)
		}
		if tag.RowsAffected() != 1 {
			return ProxyEntry{}, errVersionConflict
		}
	} else {
		var currentVersion int64
		if err := tx.QueryRow(ctx, "SELECT version FROM proxies WHERE id=$1 FOR UPDATE", id).Scan(&currentVersion); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ProxyEntry{}, errVersionConflict
			}
			return ProxyEntry{}, safeDBError(err)
		}
		if currentVersion != original {
			return ProxyEntry{}, errVersionConflict
		}
	}
	if err = p.DB.audit(ctx, tx, "proxy.update", id); err != nil {
		return ProxyEntry{}, safeDBError(err)
	}
	if err = tx.Commit(ctx); err != nil {
		return ProxyEntry{}, safeDBError(err)
	}
	if changed {
		v.Version++
		v.UpdatedAt = time.Now().Unix()
	}
	p.publishCurrentEntry(v)
	if changed {
		p.DB.invalidate()
	}
	return v, nil
}
func (p *ProxyStore) deletePG(id string) error {
	ctx, cancel := dbContext()
	defer cancel()
	tx, err := p.DB.Pool.Begin(ctx)
	if err != nil {
		return safeDBError(err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `UPDATE accounts SET version=version+1,updated_at=extract(epoch from now())::bigint WHERE id IN(SELECT account_id FROM account_proxies WHERE proxy_id=$1)`, id); err != nil {
		return safeDBError(err)
	}
	tag, err := tx.Exec(ctx, "DELETE FROM proxies WHERE id=$1", id)
	if err != nil {
		return safeDBError(err)
	}
	if tag.RowsAffected() == 0 {
		return errNotFound
	}
	if err = p.DB.audit(ctx, tx, "proxy.delete", id); err != nil {
		return safeDBError(err)
	}
	if err = tx.Commit(ctx); err != nil {
		return safeDBError(err)
	}
	p.mu.Lock()
	delete(p.index, id)
	p.mu.Unlock()
	p.DB.invalidate()
	return nil
}
