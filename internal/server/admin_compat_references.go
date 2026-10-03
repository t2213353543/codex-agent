package server

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Resolving a foreign ID/alias only uses ID, display name and preserved file
// name. Do not hydrate notes, every account binding or credential generation.
func (p *Proxy) compatAccountReferences(ctx context.Context) ([]adminCompatAccount, error) {
	if p.Store.DB == nil {
		return p.compatListAccounts(ctx, false)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	db := p.Store.DB
	tx, err := db.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.Background())
	rows, err := tx.Query(ctx, "SELECT id,display_name FROM accounts ORDER BY created_at DESC,id DESC")
	if err != nil {
		return nil, err
	}
	accounts := []Account{}
	ids := []string{}
	for rows.Next() {
		var account Account
		if err := rows.Scan(&account.ID, &account.Name); err != nil {
			rows.Close()
			return nil, err
		}
		accounts = append(accounts, account)
		ids = append(ids, account.ID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	records, err := compatRecordsPG(ctx, db, tx, ids)
	if err != nil {
		return nil, err
	}
	// Retain the catalog's numeric collision checks and ordering, including
	// collisions unrelated to the selected alias.
	result, err := compatJoinAccountRecords(ctx, accounts, records)
	if err != nil {
		return nil, err
	}
	return result, tx.Commit(ctx)
}

// compatNumericIDTarget resolves one numeric compatibility ID from a plain
// account-id scan. Numeric IDs are a stable hash of the native ID, so the scan
// never decrypts compatibility records or credentials, and any hash collision
// across the catalog keeps the same ambiguity error as the full projection.
func (p *Proxy) compatNumericIDTarget(ctx context.Context, want int64) (string, error) {
	rows, err := p.Store.DB.Pool.Query(ctx, "SELECT id FROM accounts")
	if err != nil {
		return "", accountDBError(err)
	}
	defer rows.Close()
	seen := map[int64]string{}
	id := ""
	for rows.Next() {
		var current string
		if err := rows.Scan(&current); err != nil {
			return "", err
		}
		numeric := compatNumericID(current)
		if prior, ok := seen[numeric]; ok && prior != current {
			return "", errConflict
		}
		seen[numeric] = current
		if numeric == want {
			id = current
		}
	}
	if err := rows.Err(); err != nil {
		return "", accountDBError(err)
	}
	if id == "" {
		return "", errNotFound
	}
	return id, nil
}

// compatNameCandidates returns the accounts a display name or auth-file name
// can refer to. Bare display names resolve only where callers accept them
// (sub2 aliases do, the CPA list filter matches IDs and file names only); the
// implicit "<name>.json" form always applies. Preserved file names resolve
// through the reverse link written with every compatibility mutation, so
// unrelated records stay encrypted.
func (p *Proxy) compatNameCandidates(ctx context.Context, ref string, bareName bool) (map[string]struct{}, error) {
	names := []string{}
	if bareName || strings.HasSuffix(ref, ".json") {
		names = append(names, ref)
	}
	if stem := strings.TrimSuffix(ref, ".json"); stem != "" && stem != ref {
		names = append(names, stem)
	}
	candidates := map[string]struct{}{}
	if len(names) > 0 {
		rows, err := p.Store.DB.Pool.Query(ctx, "SELECT id FROM accounts WHERE display_name=ANY($1::text[])", names)
		if err != nil {
			return nil, accountDBError(err)
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return nil, err
			}
			candidates[id] = struct{}{}
		}
		if err := rows.Err(); err != nil {
			return nil, accountDBError(err)
		}
	}
	link, err := p.managementState().Get(ctx, "admin-compat-files", ref)
	if err != nil && !errors.Is(err, errNotFound) && !errors.Is(err, errInvalid) {
		return nil, err
	}
	if err == nil {
		var id string
		if json.Unmarshal(link.Payload, &id) != nil || id == "" {
			return nil, errInvalid
		}
		candidates[id] = struct{}{}
	}
	return candidates, nil
}

// compatResolveAliasPG resolves a single non-native alias without
// materializing the compatibility catalog. Ambiguous references keep the
// conflict semantics of the full projection, and stale reverse links are
// rejected by an existence check against the account table.
func (p *Proxy) compatResolveAliasPG(ctx context.Context, ref string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if n, err := strconv.ParseInt(ref, 10, 64); err == nil && n > 0 {
		return p.compatNumericIDTarget(ctx, n)
	}
	candidates, err := p.compatNameCandidates(ctx, ref, true)
	if err != nil {
		return "", err
	}
	delete(candidates, "")
	if len(candidates) == 0 {
		return "", errNotFound
	}
	if len(candidates) > 1 {
		return "", errConflict
	}
	id := ""
	for current := range candidates {
		id = current
	}
	metadata, err := p.Store.metadataByIDsContext(ctx, []string{id})
	if err != nil {
		return "", err
	}
	if _, ok := metadata[id]; !ok {
		return "", errNotFound
	}
	return id, nil
}

// compatAuthIndexTargets resolves one auth index from the same bounded id scan
// used for numeric IDs. Unlike cpaFind the list filter keeps every match, so
// hash collisions surface as multiple rows rather than an error. The scan
// keeps the catalogue ordering of the full projection so first-match
// resolution stays stable.
func (p *Proxy) compatAuthIndexTargets(ctx context.Context, index string) ([]string, error) {
	if p.Store.DB == nil {
		accounts, err := p.compatAccountReferences(ctx)
		if err != nil {
			return nil, err
		}
		ids := []string{}
		for _, row := range accounts {
			if compatAuthIndex(row.Account.ID) == index {
				ids = append(ids, row.Account.ID)
			}
		}
		return ids, nil
	}
	rows, err := p.Store.DB.Pool.Query(ctx, "SELECT id FROM accounts ORDER BY created_at DESC,id DESC")
	if err != nil {
		return nil, accountDBError(err)
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		if compatAuthIndex(id) == index {
			ids = append(ids, id)
		}
	}
	return ids, accountDBError(rows.Err())
}

// compatAccountsByIDs hydrates only the requested accounts, their
// compatibility records and their credentials in one repeatable snapshot.
func (p *Proxy) compatAccountsByIDs(ctx context.Context, ids []string, credentials bool) ([]adminCompatAccount, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return []adminCompatAccount{}, nil
	}
	db := p.Store.DB
	tx, err := db.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.Background())
	accounts, err := p.Store.queryAccounts(ctx, tx, "WHERE a.id=ANY($1::text[]) ORDER BY a.created_at DESC,a.id DESC", []any{ids}, false)
	if err != nil {
		return nil, err
	}
	found := map[string]struct{}{}
	for _, a := range accounts {
		found[a.ID] = struct{}{}
	}
	records, err := compatRecordsPG(ctx, db, tx, ids)
	if err != nil {
		return nil, err
	}
	rows, err := compatJoinAccountRecords(ctx, accounts, records)
	if err != nil {
		return nil, err
	}
	if credentials {
		selected := make([]Account, len(rows))
		for i, row := range rows {
			selected[i] = row.Account
		}
		selected, err = p.Store.queryAccountCredentials(ctx, tx, selected)
		if err != nil {
			return nil, err
		}
		for i := range rows {
			rows[i].Account = selected[i]
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	// A requested id without an account row is a not-found reference, not an
	// empty result the caller could misread as a successful empty selection.
	if len(found) != len(ids) {
		return nil, errNotFound
	}
	return rows, nil
}
