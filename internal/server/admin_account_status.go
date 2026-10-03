package server

import (
	"context"
	"errors"
	"time"
)

// Management status follows stable account identity, not mutable display names
// or the bounded recent ring. Missing historical identity stays unverified.
func (p *Proxy) managedAccountLastStatuses(ctx context.Context, accounts []Account) (map[string]string, error) {
	statuses := make(map[string]string, len(accounts))
	versions := make(map[string]int64, len(accounts))
	ids := make([]string, 0, len(accounts))
	for _, account := range accounts {
		if account.ID != "" {
			if _, exists := statuses[account.ID]; !exists {
				statuses[account.ID] = ""
				versions[account.ID] = account.CredentialVersion
				ids = append(ids, account.ID)
			}
		}
	}
	if len(ids) == 0 {
		return statuses, ctx.Err()
	}
	if p.Metrics == nil {
		return nil, errors.New("request metrics unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if db := p.Metrics.db; db != nil {
		rows, err := db.Pool.Query(ctx, "SELECT account_ref,COALESCE(record->>'status',''),credential_version FROM account_credential_request_state WHERE account_ref=ANY($1::text[])", ids)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var id, status string
			var version int64
			if err := rows.Scan(&id, &status, &version); err != nil {
				return nil, err
			}
			if versions[id] > 0 && version == versions[id] {
				statuses[id] = status
			}
		}
		return statuses, rows.Err()
	}
	latest := make(map[string]RequestRecord, len(accounts))
	_, err := p.Metrics.walkManagedFile(ctx, 0, func(record RequestRecord) error {
		if _, wanted := statuses[record.AccountID]; !wanted || record.Time <= 0 || record.AccountCredentialVersion <= 0 || record.AccountCredentialVersion != versions[record.AccountID] {
			return nil
		}
		last, found := latest[record.AccountID]
		if !found || record.Time > last.Time || record.Time == last.Time && record.ID > last.ID {
			latest[record.AccountID] = record
			statuses[record.AccountID] = record.Status
		}
		return nil
	})
	return statuses, err
}
