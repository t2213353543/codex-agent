package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

// A management lease never changes the OAuth token expiry.
type managedAccountLease struct {
	Version              int64  `json:"version"`
	ExpiresAt            int64  `json:"expires_at"`
	AutoDisable          bool   `json:"auto_disable"`
	RestoreOnExtend      bool   `json:"restore_on_extend"`
	DisabledByLease      bool   `json:"disabled_by_lease"`
	DisabledVersion      int64  `json:"disabled_version"`
	DisabledAt           int64  `json:"disabled_at"`
	RestoreSkippedReason string `json:"restore_skipped_reason,omitempty"`
}

// unchangedFrom compares every persisted field except Version, which callers
// bump inside the update callback before deciding whether to keep the change.
func (l managedAccountLease) unchangedFrom(other managedAccountLease) bool {
	return l.ExpiresAt == other.ExpiresAt && l.AutoDisable == other.AutoDisable &&
		l.RestoreOnExtend == other.RestoreOnExtend && l.DisabledByLease == other.DisabledByLease &&
		l.DisabledVersion == other.DisabledVersion && l.DisabledAt == other.DisabledAt &&
		l.RestoreSkippedReason == other.RestoreSkippedReason
}

func (p *Proxy) getManagedLease(ctx context.Context, id string) (managedAccountLease, error) {
	if p.Store.DB == nil {
		state, err := p.Store.readState()
		if err != nil {
			return managedAccountLease{}, err
		}
		lease, ok := state.ManagedLeases[id]
		if !ok {
			return lease, errNotFound
		}
		return lease, nil
	}
	entry, err := p.managementState().Get(ctx, "account-leases", id)
	if err != nil {
		return managedAccountLease{}, err
	}
	var lease managedAccountLease
	err = json.Unmarshal(entry.Payload, &lease)
	return lease, err
}

// Account enabled state, lease provenance and audit commit together. File mode
// keeps lease state in the same atomically replaced account document; PG keeps
// the encrypted management document in the same transaction.
func (p *Proxy) updateManagedLease(ctx context.Context, id, action string, change func(*managedAccountLease, *Account) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if p.Store.DB == nil {
		return p.Store.modify(func(state *accountState) error {
			for i := range state.Accounts {
				a := &state.Accounts[i]
				if a.ID != id {
					continue
				}
				lease := state.ManagedLeases[id]
				enabled := a.Enabled
				if err := change(&lease, a); err != nil {
					return err
				}
				if err := ctx.Err(); err != nil {
					return err
				}
				if enabled != a.Enabled {
					a.Version++
					a.UpdatedAt = time.Now().Unix()
				}
				if state.ManagedLeases == nil {
					state.ManagedLeases = map[string]managedAccountLease{}
				}
				state.ManagedLeases[id] = lease
				return nil
			}
			return errNotFound
		})
	}
	db := p.Store.DB
	tx, err := db.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	name, _ := managementStateKey("account-leases", id)
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended(current_schema() || $1,91823017))", name); err != nil {
		return err
	}
	var entry managementStateEntry
	if err = db.readDocument(ctx, tx, name, &entry); err != nil {
		return err
	}
	var lease managedAccountLease
	if entry.Version > 0 {
		if err = json.Unmarshal(entry.Payload, &lease); err != nil {
			return err
		}
	}
	var version int64
	if err = tx.QueryRow(ctx, "SELECT version FROM accounts WHERE id=$1 FOR NO KEY UPDATE", id).Scan(&version); err != nil {
		return accountDBError(err)
	}
	a, err := p.Store.pgGetFrom(ctx, tx, id, false)
	if err != nil {
		return err
	}
	prior := a
	prior.Groups = append([]string(nil), a.Groups...)
	prior.Proxies = append([]string(nil), a.Proxies...)
	enabled := a.Enabled
	if err = change(&lease, &a); err != nil {
		return err
	}
	if a.Enabled != enabled {
		if err = p.Store.pgSaveMetadataChanges(ctx, tx, a, version, &prior); err != nil {
			return err
		}
	}
	entry.Version++
	entry.UpdatedAt = time.Now().Unix()
	entry.Payload, err = json.Marshal(lease)
	if err != nil {
		return err
	}
	if err = db.writeDocument(ctx, tx, name, entry); err != nil {
		return err
	}
	if action != "" {
		if err = db.audit(ctx, tx, action, id); err != nil {
			return err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	db.invalidate()
	return nil
}

func (p *Proxy) handleManagedAccountLeaseGet(w http.ResponseWriter, r *http.Request) {
	a, err := p.Store.GetMetadata(r.PathValue("name"))
	if err != nil {
		p.storeError(w, err)
		return
	}
	if a.ID != r.PathValue("name") {
		p.storeError(w, errNotFound)
		return
	}
	lease, err := p.getManagedLease(r.Context(), a.ID)
	if errors.Is(err, errNotFound) {
		writeJSON(w, map[string]any{"id": a.ID, "account_version": a.Version, "lease": nil})
		return
	}
	if err != nil {
		p.storeError(w, err)
		return
	}
	writeJSON(w, map[string]any{"id": a.ID, "account_version": a.Version, "lease": lease})
}

func applyManagedLeaseExpiry(lease *managedAccountLease, a *Account, now time.Time) {
	if !lease.AutoDisable || lease.ExpiresAt == 0 || lease.ExpiresAt > now.Unix() || !a.Enabled {
		return
	}
	a.Enabled = false
	lease.DisabledByLease = true
	lease.DisabledVersion = a.Version + 1
	lease.DisabledAt = now.Unix()
	lease.RestoreSkippedReason = ""
}
func (p *Proxy) handleManagedAccountLeaseUpdate(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Version         int64 `json:"version"`
		AccountVersion  int64 `json:"account_version"`
		ExpiresAt       int64 `json:"expires_at"`
		AutoDisable     bool  `json:"auto_disable"`
		RestoreOnExtend bool  `json:"restore_on_extend"`
	}
	if err := readJSONLimit(w, r, &input, 8<<10); err != nil {
		adminError(w, err)
		return
	}
	if input.Version < 0 || input.AccountVersion < 1 || input.ExpiresAt < 0 || input.ExpiresAt > 253402300799 {
		adminError(w, errInvalid)
		return
	}
	id := r.PathValue("name")
	now := time.Now()
	err := p.updateManagedLease(r.Context(), id, "account.lease.update", func(lease *managedAccountLease, a *Account) error {
		if lease.Version != input.Version || a.Version != input.AccountVersion {
			return errVersionConflict
		}
		before, enabledBefore := *lease, a.Enabled
		lease.Version++
		lease.ExpiresAt = input.ExpiresAt
		lease.AutoDisable = input.AutoDisable
		lease.RestoreOnExtend = input.RestoreOnExtend
		lease.RestoreSkippedReason = ""
		if lease.DisabledByLease && input.RestoreOnExtend && (!input.AutoDisable || input.ExpiresAt == 0 || input.ExpiresAt > now.Unix()) {
			if a.Enabled {
				lease.DisabledByLease = false
			} else if a.Version != lease.DisabledVersion {
				lease.RestoreSkippedReason = "account_changed"
			} else {
				a.Enabled = true
				lease.DisabledByLease = false
			}
		}
		applyManagedLeaseExpiry(lease, a, now)
		// A PUT that reproduces the stored lease state rewrites nothing: the
		// version bump only persists when some field or the account's enabled
		// state actually moved.
		if a.Enabled == enabledBefore && lease.unchangedFrom(before) {
			return errManagedLeaseNoChange
		}
		return nil
	})
	if err != nil && !errors.Is(err, errManagedLeaseNoChange) {
		p.storeError(w, err)
		return
	}
	if err == nil {
		p.invalidateAccounts()
	}
	p.handleManagedAccountLeaseGet(w, r)
}

func (p *Proxy) processManagedAccountLeases(ctx context.Context, now time.Time) {
	leases := map[string]managedAccountLease{}
	if p.Store.DB == nil {
		state, err := p.Store.readState()
		if err != nil {
			return
		}
		leases = state.ManagedLeases
	} else {
		entries, err := p.managementState().List(ctx, "account-leases")
		if err != nil {
			return
		}
		for id, entry := range entries {
			var lease managedAccountLease
			if json.Unmarshal(entry.Payload, &lease) == nil {
				leases[id] = lease
			}
		}
	}
	for id, lease := range leases {
		if ctx.Err() != nil {
			return
		}
		if lease.AutoDisable && lease.ExpiresAt > 0 && lease.ExpiresAt <= now.Unix() {
			p.processManagedAccountLease(ctx, id, now)
		}
	}
}
func (p *Proxy) processManagedAccountLease(ctx context.Context, id string, now time.Time) {
	err := p.updateManagedLease(ctx, id, "", func(lease *managedAccountLease, a *Account) error {
		if lease.Version == 0 {
			return errNotFound
		}
		if !lease.AutoDisable || lease.ExpiresAt == 0 || lease.ExpiresAt > now.Unix() || !a.Enabled {
			return errManagedLeaseNoChange
		}
		applyManagedLeaseExpiry(lease, a, now)
		return nil
	})
	if err == nil {
		p.invalidateAccounts()
	}
}

var errManagedLeaseNoChange = errors.New("management lease expiry made no change")
