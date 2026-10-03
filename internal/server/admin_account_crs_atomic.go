package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

func (p *Proxy) managedCRSLink(ctx context.Context, key string) (managedCRSLink, error) {
	if p.Store.DB == nil {
		state, err := p.Store.readState()
		if err != nil {
			return managedCRSLink{}, err
		}
		if link, ok := state.ManagedCRSLinks[key]; ok {
			return link, nil
		}
	}
	entry, err := p.managementState().Get(ctx, "crs-links", key)
	if err != nil {
		return managedCRSLink{}, err
	}
	var link managedCRSLink
	err = json.Unmarshal(entry.Payload, &link)
	return link, err
}

// managedCRSLinks resolves many source→account links in one store round trip
// instead of one document query per exported account. File mode keeps the
// historical precedence: links persisted inside the accounts state shadow the
// crs-links namespace.
func (p *Proxy) managedCRSLinks(ctx context.Context, keys []string) (map[string]managedCRSLink, error) {
	unique := make(map[string]bool, len(keys))
	wanted := make([]string, 0, len(keys))
	for _, key := range keys {
		if !unique[key] {
			unique[key] = true
			wanted = append(wanted, key)
		}
	}
	result := make(map[string]managedCRSLink, len(wanted))
	entries, err := p.managementState().GetMany(ctx, "crs-links", wanted)
	if err != nil {
		return nil, err
	}
	for key, entry := range entries {
		var link managedCRSLink
		if json.Unmarshal(entry.Payload, &link) == nil && link.ID != "" {
			result[key] = link
		}
	}
	if p.Store.DB == nil {
		state, err := p.Store.readState()
		if err != nil {
			return nil, err
		}
		for key := range unique {
			if link, ok := state.ManagedCRSLinks[key]; ok {
				result[key] = link
			}
		}
	}
	return result, nil
}

func managedCRSReplacement(current, incoming Account, row managedCRSRow, allowIdentity bool) (Account, string, error) {
	if row.TargetID != current.ID || row.TargetVersion != current.Version {
		if bytes.Equal(canonicalManagedCredentials(current), canonicalManagedCredentials(incoming)) {
			return current, "duplicate", nil
		}
		return Account{}, "", errVersionConflict
	}
	if managedIdentityChanged(current, incoming) && !allowIdentity {
		return Account{}, "", errManagedIdentityChange
	}
	updated := mergeImport(current, incoming)
	updated.Name, updated.CredentialStatus, updated.LastCheckedAt = current.Name, "unverified", 0
	updated.HasRefreshToken = updated.RefreshToken != ""
	return updated, "updated", nil
}

func (p *Proxy) commitManagedCRSAccount(ctx context.Context, source string, item managedCRSAccount, incoming Account, proxyID string, allowIdentity bool) (string, int64, string, error) {
	key := managedCRSLinkKey(source, item.Row.SourceID)
	// Match generic import initialization and metadata validation without writing.
	var credentials map[string]any
	if json.Unmarshal(item.Credentials, &credentials) != nil {
		return "", 0, "", errInvalid
	}
	credentials["enabled"], credentials["priority"], credentials["notes"] = item.Row.Enabled, item.Row.Priority, item.Notes
	raw, _ := json.Marshal(credentials)
	defaults := AccountDefaults{}
	if proxyID != "" {
		defaults.Proxies = []string{proxyID}
	}
	parsed, report, err := prepareImport([]ImportItem{{Name: item.Row.Name, Credentials: raw}}, defaults)
	if err != nil {
		return "", 0, "", err
	}
	if len(parsed) != 1 || report.Failed != 0 {
		return "", 0, "", errInvalid
	}
	created := parsed[0]
	if p.Store.DB != nil {
		return p.commitManagedCRSAccountPG(ctx, key, item.Row, incoming, created, allowIdentity)
	}
	legacy, err := p.managedCRSLink(ctx, key)
	if err != nil && !errors.Is(err, errNotFound) {
		return "", 0, "", err
	}
	var result Account
	status := ""
	err = p.Store.modify(func(state *accountState) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		link, exists := state.ManagedCRSLinks[key]
		if !exists {
			link = legacy
		}
		index := -1
		for i, account := range state.Accounts {
			if account.ID == link.ID {
				index = i
				break
			}
		}
		if index >= 0 {
			var err error
			result, status, err = managedCRSReplacement(state.Accounts[index], incoming, item.Row, allowIdentity)
			if err != nil {
				return err
			}
			for _, account := range state.Accounts {
				if account.ID != result.ID && account.AccessToken == result.AccessToken {
					return errConflict
				}
			}
			state.Accounts[index] = result
		} else {
			if item.Row.TargetID != "" {
				return errNotFound
			}
			for i, account := range state.Accounts {
				if account.AccessToken == created.AccessToken {
					updated, rotated, err := importedRefreshRotation(account, created)
					if err != nil {
						return err
					}
					result, status = updated, "duplicate"
					if rotated {
						state.Accounts[i], status = updated, "updated"
					}
					break
				}
			}
			if result.ID == "" {
				for _, account := range state.Accounts {
					if account.Name == created.Name {
						return errConflict
					}
				}
				if len(state.Accounts) == 0 {
					state.Active = created.Name
				}
				state.Accounts = append(state.Accounts, created)
				result, status = created, "added"
			}
		}
		if state.ManagedCRSLinks == nil {
			state.ManagedCRSLinks = map[string]managedCRSLink{}
		}
		state.ManagedCRSLinks[key] = managedCRSLink{ID: result.ID}
		return ctx.Err()
	})
	return result.ID, result.Version, status, err
}

func (p *Proxy) commitManagedCRSAccountPG(ctx context.Context, key string, row managedCRSRow, incoming, created Account, allowIdentity bool) (string, int64, string, error) {
	db := p.Store.DB
	tx, err := db.Pool.Begin(ctx)
	if err != nil {
		return "", 0, "", err
	}
	defer tx.Rollback(context.Background())
	name, _ := managementStateKey("crs-links", key)
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended(current_schema() || $1,91823017))", name); err != nil {
		return "", 0, "", err
	}
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended('codex-agent:import:' || current_schema(),0))"); err != nil {
		return "", 0, "", err
	}
	var entry managementStateEntry
	if err = db.readDocument(ctx, tx, name, &entry); err != nil {
		return "", 0, "", err
	}
	var link managedCRSLink
	if entry.Version > 0 {
		if err = json.Unmarshal(entry.Payload, &link); err != nil {
			return "", 0, "", err
		}
	}
	readLocked := func(id string) (Account, error) {
		if _, err := tx.Exec(ctx, "SELECT 1 FROM account_credentials WHERE account_id=$1 FOR UPDATE", id); err != nil {
			return Account{}, err
		}
		if _, err := tx.Exec(ctx, "SELECT 1 FROM accounts WHERE id=$1 FOR NO KEY UPDATE", id); err != nil {
			return Account{}, err
		}
		a, err := p.Store.pgGetFrom(ctx, tx, id, true)
		if err == nil && a.ID != id {
			return Account{}, errNotFound
		}
		return a, err
	}
	var current Account
	err = errNotFound
	if link.ID != "" {
		current, err = readLocked(link.ID)
	}
	var result Account
	status := ""
	if err == nil {
		result, status, err = managedCRSReplacement(current, incoming, row, allowIdentity)
		if err != nil {
			return "", 0, "", err
		}
		var duplicate bool
		if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM account_credentials WHERE token_fingerprint=$1 AND account_id<>$2)", tokenFingerprint(result.AccessToken), result.ID).Scan(&duplicate); err != nil {
			return "", 0, "", err
		}
		if duplicate {
			return "", 0, "", errConflict
		}
		if status != "duplicate" {
			if err = p.Store.pgWriteTokens(ctx, tx, current, result); err != nil {
				return "", 0, "", err
			}
		}
	} else if errors.Is(err, errNotFound) {
		if row.TargetID != "" {
			return "", 0, "", errNotFound
		}
		var duplicate string
		err = tx.QueryRow(ctx, "SELECT account_id FROM account_credentials WHERE token_fingerprint=$1 ORDER BY account_id LIMIT 1", tokenFingerprint(created.AccessToken)).Scan(&duplicate)
		if err == nil {
			current, err = readLocked(duplicate)
			if err != nil {
				return "", 0, "", err
			}
			var rotated bool
			result, rotated, err = importedRefreshRotation(current, created)
			if err != nil {
				return "", 0, "", err
			}
			status = "duplicate"
			if rotated {
				if err = p.Store.pgWriteTokens(ctx, tx, current, result); err != nil {
					return "", 0, "", err
				}
				status = "updated"
			}
		} else if errors.Is(err, pgx.ErrNoRows) {
			var nameExists bool
			if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM accounts WHERE display_name=$1)", created.Name).Scan(&nameExists); err != nil {
				return "", 0, "", err
			}
			if nameExists {
				return "", 0, "", errConflict
			}
			if err = p.Store.pgInsertAccount(ctx, tx, created); err != nil {
				return "", 0, "", err
			}
			if _, err = tx.Exec(ctx, "INSERT INTO settings(key,value,version) SELECT 'active_account_id',to_jsonb($1::text),1 WHERE NOT EXISTS(SELECT 1 FROM accounts WHERE id<>$1) ON CONFLICT(key) DO NOTHING", created.ID); err != nil {
				return "", 0, "", err
			}
			result, status = created, "added"
		} else {
			return "", 0, "", err
		}
	} else {
		return "", 0, "", err
	}
	link.ID = result.ID
	entry.Version++
	entry.UpdatedAt = time.Now().Unix()
	entry.Payload, err = json.Marshal(link)
	if err != nil {
		return "", 0, "", err
	}
	if err = db.writeDocument(ctx, tx, name, entry); err != nil {
		return "", 0, "", err
	}
	if err = db.audit(ctx, tx, "account.crs.sync", key); err != nil {
		return "", 0, "", err
	}
	if err = tx.Commit(ctx); err != nil {
		return "", 0, "", err
	}
	db.invalidate()
	return result.ID, result.Version, status, nil
}
