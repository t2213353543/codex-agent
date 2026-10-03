package server

import (
	"context"
	"net/http"

	"github.com/jackc/pgx/v5"
)

func sub2AccountPageRows(r *http.Request, rows []adminCompatAccount) []adminCompatAccount {
	page, size := sub2Pagination(r)
	// Avoid overflowing a hostile page integer before applying the bound.
	start := len(rows)
	if page-1 <= len(rows)/size {
		start = min((page-1)*size, len(rows))
	}
	return rows[start:min(start+size, len(rows))]
}

// Filtering needs metadata and compatibility fields. Credentials and DTOs
// only belong to the requested page; read them in the same repeatable snapshot.
func (p *Proxy) sub2AccountPage(r *http.Request) ([]adminCompatAccount, int, error) {
	ctx := r.Context()
	if db := p.Store.DB; db != nil {
		tx, err := db.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
		if err != nil {
			return nil, 0, err
		}
		defer tx.Rollback(context.Background())
		accounts, err := p.Store.queryAccounts(ctx, tx, "ORDER BY a.created_at DESC,a.id DESC", nil, false)
		if err != nil {
			return nil, 0, err
		}
		ids := make([]string, len(accounts))
		for i, account := range accounts {
			ids[i] = account.ID
		}
		records, err := compatRecordsPG(ctx, db, tx, ids)
		if err != nil {
			return nil, 0, err
		}
		rows, err := compatJoinAccountRecords(ctx, accounts, records)
		if err != nil {
			return nil, 0, err
		}
		rows = sub2FilterAccountRows(r, rows)
		total := len(rows)
		rows = sub2AccountPageRows(r, rows)
		selected := make([]Account, len(rows))
		for i, row := range rows {
			selected[i] = row.Account
		}
		selected, err = p.Store.queryAccountCredentials(ctx, tx, selected)
		if err != nil {
			return nil, 0, err
		}
		for i := range rows {
			rows[i].Account = selected[i]
		}
		return rows, total, tx.Commit(ctx)
	}
	rows, err := p.sub2FilteredAccounts(ctx, r, true)
	if err != nil {
		return nil, 0, err
	}
	return sub2AccountPageRows(r, rows), len(rows), nil
}
