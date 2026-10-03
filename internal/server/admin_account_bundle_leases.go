package server

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

type managedBundleLease struct {
	AccountIndex    int   `json:"account_index"`
	ExpiresAt       int64 `json:"expires_at"`
	AutoDisable     bool  `json:"auto_disable"`
	RestoreOnExtend bool  `json:"restore_on_extend"`
}
type managedBundleManagement struct {
	Version int                  `json:"version"`
	Leases  []managedBundleLease `json:"leases"`
}
type managedLeaseRestoreItem struct {
	managedBundleLease
	Name           string `json:"name"`
	ID             string `json:"id,omitempty"`
	AccountVersion int64  `json:"account_version"`
	LeaseVersion   int64  `json:"lease_version"`
	Status         string `json:"status"`
	Code           string `json:"code,omitempty"`
	Expired        bool   `json:"expired"`
	WouldDisable   bool   `json:"would_disable"`
	Replaces       bool   `json:"replaces_existing"`
	ClaimedAt      int64  `json:"claimed_at,omitempty"`
	ResultVersion  int64  `json:"result_account_version,omitempty"`
	ResultLease    int64  `json:"result_lease_version,omitempty"`
}
type managedLeaseRestoreReceipt struct {
	ID        string                    `json:"receipt_id,omitempty"`
	ExpiresAt int64                     `json:"expires_at,omitempty"`
	DryRun    bool                      `json:"dry_run"`
	Items     []managedLeaseRestoreItem `json:"items"`
	Error     string                    `json:"error,omitempty"`
}
type storedManagedLeaseReceipt struct {
	managedLeaseRestoreReceipt
	Owner           string `json:"owner"`
	Format          int    `json:"format,omitempty"`
	ActualOwner     string `json:"actual_owner,omitempty"`
	Generation      string `json:"generation,omitempty"`
	parentVersion   int64
	parentExpiresAt int64
	parentDigest    [32]byte
}
type managedLeaseRestoreTarget struct {
	AccountIndex   int    `json:"account_index"`
	ID             string `json:"id"`
	AccountVersion int64  `json:"account_version"`
	LeaseVersion   int64  `json:"lease_version"`
}

func validateManagedBundleLeases(management *managedBundleManagement, count int) error {
	if management == nil {
		return nil
	}
	if management.Version != 1 || len(management.Leases) > count {
		return fmt.Errorf("%w: invalid lease backup section", errInvalid)
	}
	seen := map[int]bool{}
	for _, lease := range management.Leases {
		if lease.AccountIndex < 1 || lease.AccountIndex > count || seen[lease.AccountIndex] || lease.ExpiresAt < 0 || lease.ExpiresAt > 253402300799 {
			return fmt.Errorf("%w: invalid lease backup entry", errInvalid)
		}
		seen[lease.AccountIndex] = true
	}
	return nil
}

// Read config and target versions from one storage snapshot; backup deliberately
// excludes DisabledByLease/DisabledVersion and all runtime ownership evidence.
func (p *Proxy) exportManagedBundleLeases(ctx context.Context, accounts []Account) (*managedBundleManagement, error) {
	out := &managedBundleManagement{Version: 1, Leases: []managedBundleLease{}}
	ids := make([]string, len(accounts))
	for i, a := range accounts {
		ids[i] = a.ID
	}
	snapshots, err := p.readManagedLeaseAccounts(ctx, ids, false)
	if err != nil {
		return nil, err
	}
	for i, account := range accounts {
		snapshot, found := snapshots[account.ID]
		if !found || snapshot.Account.Version != account.Version {
			return nil, errVersionConflict
		}
		lease, err := snapshot.lease(false)
		if err != nil {
			return nil, err
		}
		if lease.Version > 0 {
			out.Leases = append(out.Leases, managedBundleLease{i + 1, lease.ExpiresAt, lease.AutoDisable, lease.RestoreOnExtend})
		}
	}
	return out, nil
}

func (p *Proxy) prepareManagedBundleLeases(ctx context.Context, management *managedBundleManagement, report ImportReport, parsed []Account, dryRun bool) managedLeaseRestoreReceipt {
	out := managedLeaseRestoreReceipt{DryRun: dryRun, Items: []managedLeaseRestoreItem{}}
	targetCounts := map[string]int{}
	for _, config := range management.Leases {
		result := report.Results[config.AccountIndex-1]
		if result.Status != "failed" && result.ID != "" {
			targetCounts[result.ID]++
		}
	}
	var snapshots map[string]managedLeaseAccountSnapshot
	var snapshotErr error
	if !dryRun {
		ids := make([]string, 0, len(targetCounts))
		for id, count := range targetCounts {
			if count == 1 {
				ids = append(ids, id)
			}
		}
		snapshots, snapshotErr = p.readManagedLeaseAccounts(ctx, ids, true)
	}
	for _, config := range management.Leases {
		result := report.Results[config.AccountIndex-1]
		item := managedLeaseRestoreItem{managedBundleLease: config, Name: result.Name, Status: "pending_import", Expired: config.ExpiresAt > 0 && config.ExpiresAt <= time.Now().Unix()}
		item.WouldDisable = item.Expired && config.AutoDisable && parsed[config.AccountIndex-1].Enabled
		if result.Status == "failed" {
			item.Status, item.Code = "skipped", "account_import_failed"
		} else if !dryRun {
			item.ID = result.ID
			if targetCounts[result.ID] != 1 {
				item.Status, item.Code = "skipped", "ambiguous_import_target"
			} else {
				snapshot, found := snapshots[result.ID]
				current := snapshot.Account
				if snapshotErr != nil || !found || snapshot.CredentialErr != nil || current.AccessToken != parsed[config.AccountIndex-1].AccessToken {
					item.Status, item.Code = "skipped", "account_changed"
				} else {
					lease, leaseErr := snapshot.lease(true)
					if leaseErr != nil && !errors.Is(leaseErr, errNotFound) {
						item.Status, item.Code = "skipped", "lease_unavailable"
					} else {
						item.Name, item.AccountVersion, item.LeaseVersion = current.Name, current.Version, lease.Version
						item.Replaces, item.WouldDisable = lease.Version > 0, item.Expired && config.AutoDisable && current.Enabled
						item.Status = "pending"
					}
				}
			}
		}
		out.Items = append(out.Items, item)
	}
	if dryRun || len(out.Items) == 0 {
		return out
	}
	owner := managedOperationOwner(ctx)
	if owner == "" {
		out.Error = "lease_receipt_unavailable"
		return out
	}
	out.ID, out.ExpiresAt = newID("lease_restore_"), time.Now().Add(30*time.Minute).Unix()
	stored := storedManagedLeaseReceipt{managedLeaseRestoreReceipt: out, Owner: owner}
	if p.Store.DB != nil && len(out.Items) > managedLeaseSplitThreshold {
		stored.Format, stored.Owner, stored.ActualOwner, stored.Generation = managedLeaseSplitFormat, managedLeaseSplitOwnerGuard, owner, newID("lease_generation_")
	}
	err := p.managementState().Update(ctx, "lease-restore", out.ID, func(entry *managementStateEntry) error {
		entry.ExpiresAt = out.ExpiresAt
		entry.Payload, _ = json.Marshal(stored)
		return nil
	})
	if err != nil {
		out.ID = ""
		out.Error = "lease_receipt_unavailable"
	}
	return out
}

func decodeManagedLeaseReceipt(entry managementStateEntry, owner string) (storedManagedLeaseReceipt, error) {
	var receipt storedManagedLeaseReceipt
	if entry.Version == 0 || entry.ExpiresAt <= time.Now().Unix() {
		return receipt, errVersionConflict
	}
	if err := json.Unmarshal(entry.Payload, &receipt); err != nil {
		return receipt, err
	}
	actualOwner := receipt.Owner
	if receipt.Format != 0 {
		if receipt.Format != managedLeaseSplitFormat || receipt.Owner != managedLeaseSplitOwnerGuard || receipt.Generation == "" {
			return receipt, errInvalid
		}
		actualOwner = receipt.ActualOwner
		receipt.parentVersion, receipt.parentExpiresAt, receipt.parentDigest = entry.Version, entry.ExpiresAt, sha256.Sum256(entry.Payload)
	}
	if owner == "" || actualOwner != owner {
		return receipt, errNotFound
	}
	return receipt, nil
}
func (p *Proxy) readManagedLeaseReceipt(ctx context.Context, id string) (storedManagedLeaseReceipt, error) {
	entry, err := p.managementState().Get(ctx, "lease-restore", id)
	if errors.Is(err, errNotFound) {
		return storedManagedLeaseReceipt{}, errVersionConflict
	}
	if err != nil {
		return storedManagedLeaseReceipt{}, err
	}
	receipt, err := decodeManagedLeaseReceipt(entry, managedOperationOwner(ctx))
	if err == nil && receipt.Format == managedLeaseSplitFormat {
		err = p.hydrateManagedSplitLeaseReceipt(ctx, entry, &receipt)
	}
	return receipt, err
}
func presentManagedLeaseReceipt(receipt managedLeaseRestoreReceipt) managedLeaseRestoreReceipt {
	receipt.Items = append([]managedLeaseRestoreItem(nil), receipt.Items...)
	for i := range receipt.Items {
		if receipt.Items[i].Status == "running" && receipt.Items[i].ClaimedAt+90 <= time.Now().Unix() {
			receipt.Items[i].Status, receipt.Items[i].Code = "unknown", "result_not_confirmed"
		}
	}
	return receipt
}
func (p *Proxy) handleManagedBundleLeaseReceipt(w http.ResponseWriter, r *http.Request) {
	receipt, err := p.readManagedLeaseReceipt(r.Context(), r.PathValue("receipt"))
	if err != nil {
		p.storeError(w, err)
		return
	}
	writeJSON(w, presentManagedLeaseReceipt(receipt.managedLeaseRestoreReceipt))
}
func leaseTargetMatches(item managedLeaseRestoreItem, target managedLeaseRestoreTarget) bool {
	return item.AccountIndex == target.AccountIndex && item.ID == target.ID && item.AccountVersion == target.AccountVersion && item.LeaseVersion == target.LeaseVersion
}

func (p *Proxy) previewManagedLeaseRestore(ctx context.Context, item managedLeaseRestoreItem) managedLeaseRestoreItem {
	if item.Status != "pending" {
		return item
	}
	if p.Store.DB != nil {
		return p.previewManagedLeaseRestorePG(ctx, item)
	}
	a, err := p.Store.GetMetadata(item.ID)
	if err != nil || a.ID != item.ID {
		item.Status, item.Code = "failed", "account_missing"
		return item
	}
	lease, err := p.getManagedLease(ctx, item.ID)
	if err != nil && !errors.Is(err, errNotFound) {
		item.Status, item.Code = "failed", "lease_unavailable"
		return item
	}
	if a.Version != item.AccountVersion || lease.Version != item.LeaseVersion {
		item.Status, item.Code = "failed", "version_conflict"
		return item
	}
	item.Expired = item.ExpiresAt > 0 && item.ExpiresAt <= time.Now().Unix()
	item.WouldDisable = item.Expired && item.AutoDisable && a.Enabled
	item.Replaces = lease.Version > 0
	return item
}

func (p *Proxy) handleManagedBundleLeaseRestore(w http.ResponseWriter, r *http.Request) {
	var input struct {
		ReceiptID      string                      `json:"receipt_id"`
		Items          []managedLeaseRestoreTarget `json:"items"`
		DryRun         bool                        `json:"dry_run"`
		Confirm        bool                        `json:"confirm_restore"`
		ConfirmExpired bool                        `json:"confirm_expired"`
		ConfirmReplace bool                        `json:"confirm_replace"`
	}
	if err := readJSONLimit(w, r, &input, 128<<10); err != nil {
		adminError(w, err)
		return
	}
	if input.ReceiptID == "" || len(input.ReceiptID) > 100 || len(input.Items) < 1 || len(input.Items) > 100 || !input.DryRun && !input.Confirm {
		adminError(w, errInvalid)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	receipt, err := p.readManagedLeaseReceipt(ctx, input.ReceiptID)
	if err != nil {
		p.storeError(w, err)
		return
	}
	selected := make([]managedLeaseRestoreItem, 0, len(input.Items))
	positions := make(map[int]int, len(receipt.Items))
	for i, item := range receipt.Items {
		if _, exists := positions[item.AccountIndex]; !exists {
			positions[item.AccountIndex] = i
		}
	}
	seen := map[int]bool{}
	for _, target := range input.Items {
		if target.AccountIndex < 1 || target.ID == "" || target.AccountVersion < 1 || target.LeaseVersion < 0 || seen[target.AccountIndex] {
			adminError(w, errInvalid)
			return
		}
		seen[target.AccountIndex] = true
		position, found := positions[target.AccountIndex]
		if !found {
			adminError(w, errInvalid)
			return
		}
		item := receipt.Items[position]
		if !leaseTargetMatches(item, target) {
			p.storeError(w, errVersionConflict)
			return
		}
		selected = append(selected, item)
	}
	results := []managedLeaseRestoreItem{}
	receiptWriter := managedLeaseReceiptWriter{store: p.managementState(), id: input.ReceiptID, owner: managedOperationOwner(ctx), format: receipt.Format,
		parentVersion: receipt.parentVersion, parentExpiresAt: receipt.parentExpiresAt, parentDigest: receipt.parentDigest, generation: receipt.Generation}
	for _, item := range selected {
		item = p.previewManagedLeaseRestore(ctx, item)
		if input.DryRun || item.Status != "pending" {
			results = append(results, item)
			continue
		}
		if item.Expired && item.AutoDisable && !input.ConfirmExpired {
			item.Status, item.Code = "failed", "confirm_expired_required"
			results = append(results, item)
			continue
		}
		if item.Replaces && !input.ConfirmReplace {
			item.Status, item.Code = "failed", "confirm_replace_required"
			results = append(results, item)
			continue
		}
		claimed := false
		err = receiptWriter.updateItem(ctx, item.AccountIndex, func(current *storedManagedLeaseReceipt) error {
			for i := range current.Items {
				row := &current.Items[i]
				if row.AccountIndex == item.AccountIndex {
					if row.Status != "pending" {
						item = *row
						return errConflict
					}
					row.Status, row.ClaimedAt = "running", time.Now().Unix()
					item = *row
					claimed = true
					return nil
				}
			}
			return errNotFound
		})
		if err != nil || !claimed {
			if item.Status == "pending" {
				item.Status, item.Code = "failed", "receipt_unavailable"
			}
			results = append(results, item)
			continue
		}
		err = p.updateManagedLease(ctx, item.ID, "account.lease.restore", func(lease *managedAccountLease, a *Account) error {
			if a.ID != item.ID || a.Version != item.AccountVersion || lease.Version != item.LeaseVersion {
				return errVersionConflict
			}
			now := time.Now()
			if item.AutoDisable && item.ExpiresAt > 0 && item.ExpiresAt <= now.Unix() && !input.ConfirmExpired {
				return fmt.Errorf("%w: confirm expired lease", errInvalid)
			}
			// Restoring policy cannot acquire an old instance's right to re-enable
			// a disabled account. A fresh local expiry may establish new ownership.
			*lease = managedAccountLease{Version: lease.Version + 1, ExpiresAt: item.ExpiresAt, AutoDisable: item.AutoDisable, RestoreOnExtend: item.RestoreOnExtend}
			wasEnabled := a.Enabled
			applyManagedLeaseExpiry(lease, a, now)
			item.ResultVersion = a.Version
			if wasEnabled != a.Enabled {
				item.ResultVersion++
			}
			item.ResultLease = lease.Version
			item.WouldDisable = wasEnabled && !a.Enabled
			return nil
		})
		item.Status, item.Code = "applied", ""
		if err != nil {
			item.Status, item.Code = "unknown", "lease_restore_not_confirmed"
			if errors.Is(err, errVersionConflict) {
				item.Status, item.Code = "failed", "version_conflict"
			} else if errors.Is(err, errNotFound) || errors.Is(err, errInvalid) {
				item.Status, item.Code = "failed", "lease_restore_rejected"
			}
			item.ResultVersion, item.ResultLease = 0, 0
		}
		finalCtx, finalCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		persistErr := receiptWriter.updateItem(finalCtx, item.AccountIndex, func(current *storedManagedLeaseReceipt) error {
			for i := range current.Items {
				if current.Items[i].AccountIndex == item.AccountIndex {
					current.Items[i] = item
					return nil
				}
			}
			return errNotFound
		})
		finalCancel()
		if persistErr != nil {
			item.Status, item.Code = "unknown", "result_not_confirmed"
		}
		results = append(results, item)
	}
	if !input.DryRun {
		p.invalidateAccounts()
	}
	out := managedLeaseRestoreReceipt{ID: receipt.ID, ExpiresAt: receipt.ExpiresAt, DryRun: input.DryRun, Items: results}
	writeJSON(w, presentManagedLeaseReceipt(out))
}
