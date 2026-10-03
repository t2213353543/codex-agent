package server

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

const managedLeaseSplitFormat = 2
const managedLeaseSplitThreshold = 100

// An old binary only accepts owner==authenticated-owner. This non-hash marker
// makes it reject a v2 receipt instead of replaying the immutable pending rows.
const managedLeaseSplitOwnerGuard = "lease-receipt-v2:requires-item-state"

type managedLeaseSplitItem struct {
	Format     int                     `json:"state_format"`
	ReceiptID  string                  `json:"receipt_id"`
	Generation string                  `json:"generation"`
	Item       managedLeaseRestoreItem `json:"item"`
}

func managedLeaseSplitKey(id string, index int) (string, error) {
	if index < 1 || len(id) > 100 {
		return "", errInvalid
	}
	// Same namespace and exact parent TTL let the existing expired-document
	// cleanup remove both parent and item documents, including after downgrade.
	return managementStateKey("lease-restore", id+":item:"+strconv.Itoa(index))
}

func decodeManagedLeaseEntry(db *Database, raw []byte) (managementStateEntry, error) {
	var entry managementStateEntry
	var envelope struct {
		Ciphertext []byte `json:"ciphertext"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return entry, errors.New("invalid encrypted settings")
	}
	plain, err := db.Decrypt(envelope.Ciphertext)
	if err != nil {
		return entry, err
	}
	if err := json.Unmarshal(plain, &entry); err != nil {
		return managementStateEntry{}, errors.New("invalid settings document")
	}
	return entry, nil
}

func encodeManagedLeaseEntry(db *Database, entry managementStateEntry) ([]byte, error) {
	plain, err := json.Marshal(entry)
	if err != nil {
		return nil, err
	}
	cipher, err := db.Encrypt(plain)
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{"ciphertext": cipher})
}

func validateManagedSplitItem(parent managementStateEntry, receipt storedManagedLeaseReceipt, original managedLeaseRestoreItem, entry managementStateEntry) (managedLeaseRestoreItem, error) {
	if entry.Version < 1 || entry.ExpiresAt != parent.ExpiresAt {
		return managedLeaseRestoreItem{}, errVersionConflict
	}
	var state managedLeaseSplitItem
	if err := json.Unmarshal(entry.Payload, &state); err != nil {
		return managedLeaseRestoreItem{}, err
	}
	if state.Format != managedLeaseSplitFormat || state.ReceiptID != receipt.ID || state.Generation != receipt.Generation || state.Item.managedBundleLease != original.managedBundleLease || state.Item.ID != original.ID || state.Item.AccountVersion != original.AccountVersion || state.Item.LeaseVersion != original.LeaseVersion || state.Item.Name != original.Name {
		return managedLeaseRestoreItem{}, errVersionConflict
	}
	switch state.Item.Status {
	case "running", "applied", "failed", "unknown":
		return state.Item, nil
	default:
		return managedLeaseRestoreItem{}, errInvalid
	}
}

func (p *Proxy) hydrateManagedSplitLeaseReceipt(ctx context.Context, parent managementStateEntry, receipt *storedManagedLeaseReceipt) error {
	if p.Store.DB == nil {
		return errInvalid
	}
	keys := make([]string, 0, len(receipt.Items))
	positions := make(map[string]int, len(receipt.Items))
	for i, item := range receipt.Items {
		key, err := managedLeaseSplitKey(receipt.ID, item.AccountIndex)
		if err != nil {
			return err
		}
		if _, duplicate := positions[key]; duplicate {
			return errInvalid
		}
		keys, positions[key] = append(keys, key), i
	}
	if len(keys) == 0 {
		return nil
	}
	rows, err := p.Store.DB.Pool.Query(ctx, `SELECT key,value FROM settings WHERE key=ANY($1::text[])`, keys)
	if err != nil {
		return safeDBError(err)
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		var raw []byte
		if err := rows.Scan(&key, &raw); err != nil {
			return err
		}
		position, found := positions[key]
		if !found {
			return errInvalid
		}
		entry, err := decodeManagedLeaseEntry(p.Store.DB, raw)
		if err != nil {
			return err
		}
		item, err := validateManagedSplitItem(parent, *receipt, receipt.Items[position], entry)
		if err != nil {
			return err
		}
		receipt.Items[position] = item
	}
	if err := rows.Err(); err != nil {
		return err
	}
	// Cleanup can remove an expired child after the parent was read. Reject
	// that crossing instead of presenting its immutable pending baseline.
	if parent.ExpiresAt <= time.Now().Unix() {
		return errVersionConflict
	}
	return nil
}

// A v2 parent is immutable. Each state change still commits on its own before
// the existing independent account/lease/audit transaction runs. The root row
// is locked against deletion/replacement while the chosen item is written.
func (w *managedLeaseReceiptWriter) updateSplitItem(ctx context.Context, index int, change func(*storedManagedLeaseReceipt) error) error {
	if err := ctx.Err(); err != nil {
		w.clear()
		return err
	}
	rootKey, err := managementStateKey("lease-restore", w.id)
	if err != nil {
		return err
	}
	itemKey, err := managedLeaseSplitKey(w.id, index)
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
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended(current_schema() || $1,91823017))`, rootKey); err != nil {
		return safeDBError(err)
	}
	var version int64
	var xmin, ctid string
	var raw []byte
	if err := tx.QueryRow(ctx, `SELECT version,xmin::text,ctid::text,
 CASE WHEN version=$2 AND xmin::text=$3 AND ctid::text=$4 THEN NULL ELSE value END
 FROM settings WHERE key=$1 FOR SHARE`, rootKey, w.version, w.xmin, w.ctid).Scan(&version, &xmin, &ctid, &raw); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return errVersionConflict
		}
		return safeDBError(err)
	}
	parent, receipt, positions := w.entry, w.receipt, w.positions
	if raw != nil {
		parent, err = decodeManagedLeaseEntry(db, raw)
		if err != nil {
			return err
		}
		receipt, err = decodeManagedLeaseReceipt(parent, w.owner)
		if err != nil {
			return err
		}
		if receipt.Format != managedLeaseSplitFormat || receipt.ID != w.id {
			return errVersionConflict
		}
		positions = make(map[int]int, len(receipt.Items))
		for i, item := range receipt.Items {
			if _, duplicate := positions[item.AccountIndex]; duplicate {
				return errInvalid
			}
			positions[item.AccountIndex] = i
		}
	} else if parent.Version < 1 || receipt.Format != managedLeaseSplitFormat || receipt.ActualOwner != w.owner || w.owner == "" {
		return errVersionConflict
	}
	if receipt.Generation != w.generation || receipt.parentVersion != w.parentVersion || receipt.parentExpiresAt != w.parentExpiresAt || receipt.parentDigest != w.parentDigest {
		return errVersionConflict
	}
	position, found := positions[index]
	if !found {
		return errNotFound
	}
	original := receipt.Items[position]
	if original.Status != "pending" {
		return errConflict
	}
	// Cleanup uses this same item advisory lock. Check TTL after waiting so
	// an expired and just-pruned item can never be recreated as pending.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended(current_schema() || $1,91823017))`, itemKey); err != nil {
		return safeDBError(err)
	}
	if parent.ExpiresAt <= time.Now().Unix() {
		return errVersionConflict
	}
	var itemRaw []byte
	var itemVersion int64
	var itemXmin, itemCtid string
	itemErr := tx.QueryRow(ctx, `SELECT value,version,xmin::text,ctid::text FROM settings WHERE key=$1 FOR UPDATE`, itemKey).Scan(&itemRaw, &itemVersion, &itemXmin, &itemCtid)
	if itemErr != nil && !errors.Is(itemErr, pgx.ErrNoRows) {
		return safeDBError(itemErr)
	}
	entry := managementStateEntry{ExpiresAt: parent.ExpiresAt}
	item := original
	if itemErr == nil {
		entry, err = decodeManagedLeaseEntry(db, itemRaw)
		if err != nil {
			return err
		}
		item, err = validateManagedSplitItem(parent, receipt, original, entry)
		if err != nil {
			return err
		}
	}
	current := receipt
	current.Items = []managedLeaseRestoreItem{item}
	if err := change(&current); err != nil {
		return err
	}
	if len(current.Items) != 1 {
		return errInvalid
	}
	state := managedLeaseSplitItem{Format: managedLeaseSplitFormat, ReceiptID: receipt.ID, Generation: receipt.Generation, Item: current.Items[0]}
	if err := applyManagementStateChange(ctx, &entry, func(entry *managementStateEntry) error {
		var err error
		entry.Payload, err = json.Marshal(state)
		return err
	}); err != nil {
		return err
	}
	if _, err := validateManagedSplitItem(parent, receipt, original, entry); err != nil {
		return err
	}
	envelope, err := encodeManagedLeaseEntry(db, entry)
	if err != nil {
		return err
	}
	var unchanged bool
	if errors.Is(itemErr, pgx.ErrNoRows) {
		err = tx.QueryRow(ctx, `INSERT INTO settings(key,value) VALUES($1,$2) ON CONFLICT(key) DO NOTHING RETURNING value=$2::jsonb`, itemKey, envelope).Scan(&unchanged)
	} else {
		err = tx.QueryRow(ctx, `UPDATE settings SET value=$2,version=version+1,updated_at=now()
 WHERE key=$1 AND version=$3 AND xmin::text=$4 AND ctid::text=$5 RETURNING value=$2::jsonb`, itemKey, envelope, itemVersion, itemXmin, itemCtid).Scan(&unchanged)
	}
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return errVersionConflict
		}
		return safeDBError(err)
	}
	if !unchanged {
		return errVersionConflict
	}
	if err := tx.Commit(ctx); err != nil {
		return safeDBError(err)
	}
	// Cache only the verified immutable parent, never the mutable item state.
	w.version, w.xmin, w.ctid, w.entry, w.receipt, w.positions = version, xmin, ctid, parent, receipt, positions
	confirmed = true
	return nil
}
