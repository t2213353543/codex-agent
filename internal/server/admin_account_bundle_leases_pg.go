package server

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Only the receipt decoder is cached for one HTTP request. Every mutation
// retains the original independent transaction and advisory lock. A narrow
// row identity check under that lock avoids re-reading/decrypting the same
// large receipt before each claim and finalization. Other writers, including
// updates that do not bump version, invalidate the tuple identity.
type managedLeaseReceiptWriter struct {
	store           managementStateStore
	id              string
	owner           string
	format          int
	parentVersion   int64
	parentExpiresAt int64
	parentDigest    [32]byte
	generation      string
	version         int64
	xmin            string
	ctid            string
	entry           managementStateEntry
	receipt         storedManagedLeaseReceipt
	positions       map[int]int
}

func (w *managedLeaseReceiptWriter) clear() {
	w.version, w.xmin, w.ctid = 0, "", ""
	w.entry, w.receipt = managementStateEntry{}, storedManagedLeaseReceipt{}
	w.positions = nil
}

func (w *managedLeaseReceiptWriter) updateItem(ctx context.Context, index int, change func(*storedManagedLeaseReceipt) error) error {
	if w.format == managedLeaseSplitFormat && w.store.db != nil {
		return w.updateSplitItem(ctx, index, change)
	}
	return w.update(ctx, change)
}

func (w *managedLeaseReceiptWriter) update(ctx context.Context, change func(*storedManagedLeaseReceipt) error) error {
	if w.store.db == nil {
		return w.store.Update(ctx, "lease-restore", w.id, func(entry *managementStateEntry) error {
			current, err := decodeManagedLeaseReceipt(*entry, w.owner)
			if err != nil {
				return err
			}
			if err := change(&current); err != nil {
				return err
			}
			entry.Payload, _ = json.Marshal(current)
			return nil
		})
	}
	if err := ctx.Err(); err != nil {
		w.clear()
		return err
	}
	name, err := managementStateKey("lease-restore", w.id)
	if err != nil {
		return err
	}
	db := w.store.db
	tx, err := db.Pool.Begin(ctx)
	if err != nil {
		w.clear()
		return safeDBError(err)
	}
	defer tx.Rollback(context.Background())
	confirmed := false
	defer func() {
		if !confirmed {
			w.clear()
		}
	}()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended(current_schema() || $1,91823017))`, name); err != nil {
		return safeDBError(err)
	}
	var version int64
	var xmin, ctid string
	var raw []byte
	if err := tx.QueryRow(ctx, `SELECT version,xmin::text,ctid::text,
 CASE WHEN version=$2 AND xmin::text=$3 AND ctid::text=$4 THEN NULL ELSE value END
 FROM settings WHERE key=$1`, name, w.version, w.xmin, w.ctid).Scan(&version, &xmin, &ctid, &raw); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return errVersionConflict
		}
		return safeDBError(err)
	}
	entry, current := w.entry, w.receipt
	if raw != nil {
		entry = managementStateEntry{}
		var envelope struct {
			Ciphertext []byte `json:"ciphertext"`
		}
		if err := json.Unmarshal(raw, &envelope); err != nil {
			return errors.New("invalid encrypted settings")
		}
		plain, err := db.Decrypt(envelope.Ciphertext)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(plain, &entry); err != nil {
			return errors.New("invalid settings document")
		}
		current, err = decodeManagedLeaseReceipt(entry, w.owner)
		if err != nil {
			return err
		}
	} else {
		// Expiry advances even when this exact database tuple is unchanged.
		if entry.Version == 0 || entry.ExpiresAt <= time.Now().Unix() {
			return errVersionConflict
		}
		if w.owner == "" || current.Owner != w.owner {
			return errNotFound
		}
	}
	// A rejected/ambiguous transaction must not mutate the cached receipt.
	current.Items = append([]managedLeaseRestoreItem(nil), current.Items...)
	if err := applyManagementStateChange(ctx, &entry, func(entry *managementStateEntry) error {
		if err := change(&current); err != nil {
			return err
		}
		var err error
		entry.Payload, err = json.Marshal(current)
		return err
	}); err != nil {
		return err
	}
	plain, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	encrypted, err := db.Encrypt(plain)
	if err != nil {
		return err
	}
	envelope, err := json.Marshal(map[string]any{"ciphertext": encrypted})
	if err != nil {
		return err
	}
	var nextVersion int64
	var nextXmin, nextCtid string
	var unchangedPayload bool
	if err := tx.QueryRow(ctx, `UPDATE settings SET value=$2,version=version+1,updated_at=now()
 WHERE key=$1 AND version=$3 AND xmin::text=$4 AND ctid::text=$5
 RETURNING version,xmin::text,ctid::text,value=$2::jsonb`, name, envelope, version, xmin, ctid).Scan(&nextVersion, &nextXmin, &nextCtid, &unchangedPayload); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return errVersionConflict
		}
		return safeDBError(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return safeDBError(err)
	}
	if unchangedPayload {
		w.version, w.xmin, w.ctid, w.entry, w.receipt = nextVersion, nextXmin, nextCtid, entry, current
	} else {
		// A database trigger may rewrite a stored document. Never cache the
		// proposed payload when the statement persisted something else.
		w.clear()
	}
	confirmed = true
	return nil
}

func (p *Proxy) previewManagedLeaseRestorePG(ctx context.Context, item managedLeaseRestoreItem) managedLeaseRestoreItem {
	snapshots, err := p.readManagedLeaseAccounts(ctx, []string{item.ID}, false)
	snapshot, found := snapshots[item.ID]
	if err != nil || !found || snapshot.Account.ID != item.ID {
		item.Status, item.Code = "failed", "account_missing"
		return item
	}
	lease, err := snapshot.lease(true)
	if err != nil && !errors.Is(err, errNotFound) {
		item.Status, item.Code = "failed", "lease_unavailable"
		return item
	}
	if snapshot.Account.Version != item.AccountVersion || lease.Version != item.LeaseVersion {
		item.Status, item.Code = "failed", "version_conflict"
		return item
	}
	item.Expired = item.ExpiresAt > 0 && item.ExpiresAt <= time.Now().Unix()
	item.WouldDisable = item.Expired && item.AutoDisable && snapshot.Account.Enabled
	item.Replaces = lease.Version > 0
	return item
}
