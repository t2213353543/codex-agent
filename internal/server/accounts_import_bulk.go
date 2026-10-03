package server

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// New-only imports have no order-dependent replacement/duplicate decisions.
// Keep the existing ordered path for any overlap, including duplicates inside
// this payload. The caller already holds the import/name-rename advisory lock.
func (s Store) pgImportNewAccounts(ctx context.Context, tx pgx.Tx, parsed []Account, report *ImportReport) (bool, error) {
	return s.pgImportNewAccountsAtLeast(ctx, tx, parsed, report, 2)
}

func (s Store) pgImportNewAccountsAtLeast(ctx context.Context, tx pgx.Tx, parsed []Account, report *ImportReport, minimum int) (bool, error) {
	names, fingerprints, indexes := []string{}, [][]byte{}, []int{}
	seenNames, seenTokens := map[string]bool{}, map[string]bool{}
	for i, account := range parsed {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if report.Results[i].Status == "failed" {
			continue
		}
		fingerprint := tokenFingerprint(account.AccessToken)
		if seenNames[account.Name] || seenTokens[string(fingerprint)] {
			return false, nil
		}
		seenNames[account.Name], seenTokens[string(fingerprint)] = true, true
		names = append(names, account.Name)
		fingerprints = append(fingerprints, fingerprint)
		indexes = append(indexes, i)
	}
	if len(indexes) < minimum {
		return false, nil
	}
	var overlaps, populated bool
	// These array sizes vary by import. A named statement can retain a generic
	// plan selected for a small table/batch and later scan every row against a
	// large ANY array. Exec binds the same typed parameters in one extended-
	// protocol round trip but reparses this probe, without changing the caller's
	// transaction-local planner settings (including an OAuth-owned transaction).
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM accounts WHERE display_name=ANY($1::text[]))
 OR EXISTS(SELECT 1 FROM account_credentials WHERE token_fingerprint=ANY($2::bytea[])),EXISTS(SELECT 1 FROM accounts)`, pgx.QueryExecModeExec, names, fingerprints).Scan(&overlaps, &populated); err != nil {
		return false, err
	}
	if overlaps {
		return false, nil
	}
	accounts := make([][]any, 0, len(indexes))
	credentials := make([][]any, 0, len(indexes))
	groups, proxies, audits := [][]any{}, [][]any{}, [][]any{}
	now := time.Now().Unix()
	for _, i := range indexes {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		account := parsed[i]
		normalizeAccount(&account)
		if err := advanceGatewayOwnerRevision(nil, &account); err != nil {
			return false, err
		}
		if account.CreatedAt == 0 {
			account.CreatedAt = now
		}
		account.UpdatedAt = now
		if account.Tags == nil {
			account.Tags = []string{}
		}
		encrypted, err := s.credentialCipher(account)
		if err != nil {
			return false, err
		}
		accounts = append(accounts, []any{account.ID, account.Name, account.Enabled, account.Notes, account.Tags, account.Concurrency, account.Priority, account.Weight, account.Quota, account.WindowDays, account.WindowStart, account.ExpiresAt, account.RefreshToken != "", account.CredentialStatus, account.LastCheckedAt, account.LastError, account.CreatedAt, account.UpdatedAt, account.Version})
		credentials = append(credentials, []any{account.ID, encrypted, tokenFingerprint(account.AccessToken), account.ExpiresAt, int64(1), s.DB.KeyID, now})
		for _, id := range uniqueStrings(account.Groups) {
			groups = append(groups, []any{account.ID, id})
		}
		for position, id := range uniqueStrings(account.Proxies) {
			proxies = append(proxies, []any{account.ID, id, position + 1})
		}
		audits = append(audits, []any{"account.import", account.ID})
	}
	for _, table := range []struct {
		name    string
		columns []string
		rows    [][]any
	}{
		{"accounts", []string{"id", "display_name", "enabled", "notes", "tags", "concurrency", "priority", "weight", "quota", "window_days", "window_start", "expires_at", "has_refresh_token", "credential_status", "last_checked_at", "last_error", "created_at", "updated_at", "version"}, accounts},
		{"account_credentials", []string{"account_id", "credentials_cipher", "token_fingerprint", "expires_at", "credential_version", "encryption_key_id", "updated_at"}, credentials},
		{"account_groups", []string{"account_id", "group_id"}, groups},
		{"account_proxies", []string{"account_id", "proxy_id", "position"}, proxies},
		{"audit_logs", []string{"action", "entity_id"}, audits},
	} {
		if len(table.rows) == 0 {
			continue
		}
		if _, err := tx.CopyFrom(ctx, pgx.Identifier{table.name}, table.columns, pgx.CopyFromRows(table.rows)); err != nil {
			return false, accountDBError(err)
		}
	}
	if !populated {
		if _, err := tx.Exec(ctx, `INSERT INTO settings(key,value,version) VALUES('active_account_id',to_jsonb($1::text),1) ON CONFLICT(key) DO NOTHING`, parsed[indexes[0]].ID); err != nil {
			return false, err
		}
	}
	for _, i := range indexes {
		report.Results[i].Status, report.Results[i].ID = "added", parsed[i].ID
		report.Added++
	}
	return true, nil
}
