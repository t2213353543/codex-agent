package server

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
)

// A duplicate import only mutates stored credentials when its nonempty refresh
// token changes. Resolve the whole unchanged batch in one snapshot, preserving
// the original first account_id for a fingerprint and input/report ordering.
// Pure duplicates and a disjoint set of entirely new names/tokens can share
// the fast transaction. Conflicts/rotations retain the ordered writer path;
// no report field is changed before every fast-path decision is complete.
func (s Store) pgImportUnchangedAccounts(ctx context.Context, tx pgx.Tx, parsed []Account, report *ImportReport) (bool, error) {
	fingerprints := make([][]byte, 0, len(parsed))
	keys := make([]string, len(parsed))
	wanted := make(map[string]bool, len(parsed))
	valid := 0
	for i, account := range parsed {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if report.Results[i].Status == "failed" {
			continue
		}
		valid++
		fingerprint := tokenFingerprint(account.AccessToken)
		keys[i] = string(fingerprint)
		if !wanted[keys[i]] {
			wanted[keys[i]] = true
			fingerprints = append(fingerprints, fingerprint)
		}
	}
	if valid < 2 {
		return false, nil
	}
	// As with the new-account overlap probe, never reuse a generic array plan
	// inherited from a smaller batch/table. Keep parameters bound and avoid a
	// SET LOCAL that would leak into an externally owned import transaction.
	rows, err := tx.Query(ctx, `SELECT DISTINCT ON(token_fingerprint) token_fingerprint,account_id,credentials_cipher
		FROM account_credentials WHERE token_fingerprint=ANY($1::bytea[]) ORDER BY token_fingerprint,account_id`, pgx.QueryExecModeExec, fingerprints)
	if err != nil {
		return false, err
	}
	type candidate struct {
		id, refresh string
		ciphertext  []byte
	}
	current := make(map[string]candidate, len(wanted))
	for rows.Next() {
		var fingerprint []byte
		var row candidate
		if err := rows.Scan(&fingerprint, &row.id, &row.ciphertext); err != nil {
			rows.Close()
			return false, err
		}
		current[string(fingerprint)] = row
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, err
	}
	mixed := len(current) != len(wanted)
	if mixed {
		// Repeated input names/tokens can make insertion and replacement order
		// observable. Only independent items may be partitioned for bulk COPY.
		seenNames, seenTokens := map[string]bool{}, map[string]bool{}
		for i, incoming := range parsed {
			if report.Results[i].Status == "failed" {
				continue
			}
			if seenNames[incoming.Name] || seenTokens[keys[i]] {
				return false, nil
			}
			seenNames[incoming.Name], seenTokens[keys[i]] = true, true
		}
	}
	for key, row := range current {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		plain, err := s.DB.Decrypt(row.ciphertext)
		if err != nil {
			if mixed {
				return false, nil
			}
			return false, errors.New("decrypt account credentials failed")
		}
		var secret accountSecret
		if err := json.Unmarshal(plain, &secret); err != nil {
			if mixed {
				return false, nil
			}
			return false, errors.New("invalid encrypted account credentials")
		}
		if !validGatewayOwnerRevision(secret.OwnerRevision) {
			if mixed {
				return false, nil
			}
			return false, errors.New("invalid encrypted account ownership revision")
		}
		row.refresh, row.ciphertext = secret.RefreshToken, nil
		current[key] = row
	}
	for i, incoming := range parsed {
		if report.Results[i].Status == "failed" {
			continue
		}
		// This is importedRefreshRotation's exact no-write predicate. Metadata
		// or identity differences are deliberately ignored when refresh is equal.
		candidate, found := current[keys[i]]
		if found && incoming.RefreshToken != "" && incoming.RefreshToken != candidate.refresh {
			return false, nil
		}
	}
	var addedIndexes []int
	var addedReport ImportReport
	if mixed {
		var additions []Account
		for i, incoming := range parsed {
			if report.Results[i].Status == "failed" {
				continue
			}
			if _, found := current[keys[i]]; !found {
				addedIndexes = append(addedIndexes, i)
				additions = append(additions, incoming)
				addedReport.Results = append(addedReport.Results, report.Results[i])
			}
		}
		// Recheck new names/token fingerprints under the existing import lock.
		// A single new item is valid here, avoiding an N+1 cliff for 9999+1.
		inserted, err := s.pgImportNewAccountsAtLeast(ctx, tx, additions, &addedReport, 1)
		if err != nil {
			return false, err
		}
		if !inserted {
			return false, nil
		}
	}
	for i := range parsed {
		if report.Results[i].Status != "failed" {
			if row, found := current[keys[i]]; found {
				report.Results[i].Status, report.Results[i].ID = "duplicate", row.id
				report.Duplicate++
			}
		}
	}
	for i, original := range addedIndexes {
		report.Results[original] = addedReport.Results[i]
	}
	report.Added += addedReport.Added
	return true, nil
}
