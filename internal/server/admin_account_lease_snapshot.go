package server

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// A bundle may contain ten thousand accounts. Read just their lease inputs in
// one statement/snapshot, rather than acquiring a connection and decrypting the
// full account document for every row. Per-row corruption stays per-row: an
// unusable credential or lease must not hide unrelated restorable accounts.
type managedLeaseAccountSnapshot struct {
	Account       Account
	CredentialErr error
	Lease         managedAccountLease
	Entry         *managementStateEntry
	LeaseErr      error
}

func (s managedLeaseAccountSnapshot) lease(activeOnly bool) (managedAccountLease, error) {
	if s.LeaseErr != nil {
		return managedAccountLease{}, s.LeaseErr
	}
	if s.Entry == nil {
		return s.Lease, nil
	}
	if s.Entry.Version == 0 || activeOnly && s.Entry.ExpiresAt > 0 && s.Entry.ExpiresAt <= time.Now().Unix() {
		return managedAccountLease{}, nil
	}
	var lease managedAccountLease
	err := json.Unmarshal(s.Entry.Payload, &lease)
	return lease, err
}

func (p *Proxy) readManagedLeaseAccounts(ctx context.Context, ids []string, credentials bool) (map[string]managedLeaseAccountSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	result := make(map[string]managedLeaseAccountSnapshot, len(ids))
	wanted := make(map[string]bool, len(ids))
	keys := make([]string, 0, len(ids))
	selected := make([]string, 0, len(ids))
	for _, id := range ids {
		if wanted[id] {
			continue
		}
		key, err := managementStateKey("account-leases", id)
		if err != nil {
			return nil, err
		}
		wanted[id] = true
		selected, keys = append(selected, id), append(keys, key)
	}
	if len(selected) == 0 {
		return result, nil
	}
	if p.Store.DB == nil {
		state, err := p.Store.readState()
		if err != nil {
			return nil, err
		}
		for _, a := range state.Accounts {
			if wanted[a.ID] {
				if !credentials {
					a = accountMetadata(a)
				}
				result[a.ID] = managedLeaseAccountSnapshot{Account: a, Lease: state.ManagedLeases[a.ID]}
			}
		}
		return result, ctx.Err()
	}
	secretColumn, secretJoin := "NULL::bytea", ""
	if credentials {
		secretColumn = "ac.credentials_cipher"
		secretJoin = " LEFT JOIN account_credentials ac ON ac.account_id=a.id"
	}
	rows, err := p.Store.DB.Pool.Query(ctx, `SELECT a.id,a.display_name,a.version,a.enabled,`+secretColumn+`,s.value
	 FROM unnest($1::text[],$2::text[]) AS wanted(id,lease_key)
	 JOIN accounts a ON a.id=wanted.id`+secretJoin+`
	 LEFT JOIN settings s ON s.key=wanted.lease_key`, selected, keys)
	if err != nil {
		return nil, safeDBError(err)
	}
	defer rows.Close()
	for rows.Next() {
		var snapshot managedLeaseAccountSnapshot
		var cipher, raw []byte
		a := &snapshot.Account
		if err := rows.Scan(&a.ID, &a.Name, &a.Version, &a.Enabled, &cipher, &raw); err != nil {
			return nil, err
		}
		if credentials {
			plain, err := p.Store.DB.Decrypt(cipher)
			var secret accountSecret
			if err == nil {
				err = json.Unmarshal(plain, &secret)
			}
			if err == nil && !validGatewayOwnerRevision(secret.OwnerRevision) {
				err = errors.New("invalid encrypted account ownership revision")
			}
			snapshot.CredentialErr = err
			if err == nil {
				a.AccessToken = secret.AccessToken
			}
		}
		if raw != nil {
			var envelope struct {
				Ciphertext []byte `json:"ciphertext"`
			}
			err := json.Unmarshal(raw, &envelope)
			var plain []byte
			if err == nil {
				plain, err = p.Store.DB.Decrypt(envelope.Ciphertext)
			}
			if err == nil {
				snapshot.Entry = &managementStateEntry{}
				err = json.Unmarshal(plain, snapshot.Entry)
			}
			snapshot.LeaseErr = err
		}
		result[a.ID] = snapshot
	}
	return result, rows.Err()
}
