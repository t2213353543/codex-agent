package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func managedSplitLeaseFixture(t *testing.T, p *Proxy, h http.Handler, count int) (storedManagedLeaseReceipt, []managedLeaseRestoreTarget) {
	t.Helper()
	bundle := managedAccountBundle{Type: "codex-agent-accounts", Version: 1, Management: &managedBundleManagement{Version: 1}}
	for i := range count {
		a := managedTestAccount(t, p, fmt.Sprintf("split-target-%d", i), nil)
		bundle.Accounts = append(bundle.Accounts, managedAccountBackup{Name: a.Name, Credentials: canonicalManagedCredentials(a), Enabled: true, Concurrency: 1, Weight: 1, WindowDays: 1})
		bundle.Management.Leases = append(bundle.Management.Leases, managedBundleLease{AccountIndex: i + 1, ExpiresAt: 2100000000, AutoDisable: true, RestoreOnExtend: true})
	}
	r := managedTestRequest(t, h, http.MethodPost, "/api/accounts/import-bundle", map[string]any{"data": bundle}, 200)
	var imported struct {
		LeaseRestore managedLeaseRestoreReceipt `json:"lease_restore"`
	}
	if json.Unmarshal(r.Body.Bytes(), &imported) != nil || len(imported.LeaseRestore.Items) != count {
		t.Fatal("missing receipt")
	}
	var receipt storedManagedLeaseReceipt
	if err := p.managementState().Update(t.Context(), "lease-restore", imported.LeaseRestore.ID, func(entry *managementStateEntry) error {
		if err := json.Unmarshal(entry.Payload, &receipt); err != nil {
			return err
		}
		if count > managedLeaseSplitThreshold && receipt.Format != managedLeaseSplitFormat {
			t.Fatal("large PG receipt did not use split format")
		}
		if receipt.Format == 0 {
			receipt.Format, receipt.ActualOwner, receipt.Owner, receipt.Generation = managedLeaseSplitFormat, receipt.Owner, managedLeaseSplitOwnerGuard, newID("lease_generation_")
		}
		entry.Payload, _ = json.Marshal(receipt)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	entry, err := p.managementState().Get(t.Context(), "lease-restore", receipt.ID)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err = decodeManagedLeaseReceipt(entry, receipt.ActualOwner)
	if err != nil {
		t.Fatal(err)
	}
	targets := make([]managedLeaseRestoreTarget, len(receipt.Items))
	for i, item := range receipt.Items {
		targets[i] = managedLeaseRestoreTarget{item.AccountIndex, item.ID, item.AccountVersion, item.LeaseVersion}
	}
	return receipt, targets
}

func splitLeaseWriter(p *Proxy, receipt storedManagedLeaseReceipt) *managedLeaseReceiptWriter {
	return &managedLeaseReceiptWriter{store: p.managementState(), id: receipt.ID, owner: receipt.ActualOwner, format: managedLeaseSplitFormat,
		parentVersion: receipt.parentVersion, parentExpiresAt: receipt.parentExpiresAt, parentDigest: receipt.parentDigest, generation: receipt.Generation}
}

func TestPGManagedSplitLeaseLargeReceiptImmutableAndLegacyRejected(t *testing.T) {
	p, h := adminPGTestProxy(t)
	receipt, targets := managedSplitLeaseFixture(t, p, h, 101)
	key, _ := managementStateKey("lease-restore", receipt.ID)
	var before []byte
	if err := p.Store.DB.Pool.QueryRow(t.Context(), "SELECT value FROM settings WHERE key=$1", key).Scan(&before); err != nil {
		t.Fatal(err)
	}
	parent, err := p.managementState().Get(t.Context(), "lease-restore", receipt.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Precisely the old decoder's accepted shape and owner check: unknown v2
	// fields cannot turn immutable pending rows into executable old receipts.
	var legacy struct {
		managedLeaseRestoreReceipt
		Owner string `json:"owner"`
	}
	if json.Unmarshal(parent.Payload, &legacy) != nil || legacy.Owner == receipt.ActualOwner || legacy.Owner == "" {
		t.Fatal("old reader could accept new receipt owner")
	}
	body := map[string]any{"receipt_id": receipt.ID, "items": targets[1:], "confirm_restore": true}
	response := managedTestRequest(t, h, http.MethodPost, "/api/accounts/import-leases", body, 200)
	var restored managedLeaseRestoreReceipt
	if json.Unmarshal(response.Body.Bytes(), &restored) != nil || len(restored.Items) != 100 {
		t.Fatal("selected tail response mismatch")
	}
	for _, item := range restored.Items {
		if item.Status != "applied" || item.ResultLease != 1 {
			t.Fatalf("tail not applied: %+v", item)
		}
	}
	var unchanged bool
	var children, audits int
	if err := p.Store.DB.Pool.QueryRow(t.Context(), `SELECT value=$2::jsonb,(SELECT count(*) FROM settings WHERE key LIKE 'management/lease-restore/%')-1,(SELECT count(*) FROM audit_logs WHERE action='account.lease.restore') FROM settings WHERE key=$1`, key, before).Scan(&unchanged, &children, &audits); err != nil || !unchanged || children != 100 || audits != 100 {
		t.Fatalf("parent rewritten or state/audit mismatch %t/%d/%d %v", unchanged, children, audits, err)
	}
	get := managedTestRequest(t, h, http.MethodGet, "/api/accounts/import-leases/receipts/"+receipt.ID, nil, 200)
	var shown managedLeaseRestoreReceipt
	if json.Unmarshal(get.Body.Bytes(), &shown) != nil || len(shown.Items) != 101 || shown.Items[0].Status != "pending" || shown.Items[100].Status != "applied" {
		t.Fatal("GET did not merge selected state", get.Body.String())
	}
	if strings.Contains(get.Body.String(), "actual_owner") || strings.Contains(get.Body.String(), receipt.Generation) || strings.Contains(get.Body.String(), managedLeaseSplitOwnerGuard) {
		t.Fatal("private receipt format leaked")
	}
	managedTestRequest(t, h, http.MethodGet, "/api/accounts/import-leases/receipts/"+receipt.ID+":item:2", nil, 404)
	managedTestRequest(t, h, http.MethodPost, "/api/accounts/import-leases", body, 200)
	if err := p.Store.DB.Pool.QueryRow(t.Context(), "SELECT count(*) FROM audit_logs WHERE action='account.lease.restore'").Scan(&audits); err != nil || audits != 100 {
		t.Fatal("replay applied twice", err)
	}
}

func TestPGManagedSplitLeaseConcurrentClaimsAndPartialAuditFailure(t *testing.T) {
	p, h := adminPGTestProxy(t)
	receipt, targets := managedSplitLeaseFixture(t, p, h, 3)
	if _, err := p.Store.DB.Pool.Exec(t.Context(), fmt.Sprintf(`CREATE FUNCTION reject_split_apply() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.action='account.lease.restore' AND NEW.entity_id='%s' THEN RAISE EXCEPTION 'synthetic split audit failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER reject_split_apply BEFORE INSERT ON audit_logs FOR EACH ROW EXECUTE FUNCTION reject_split_apply()`, receipt.Items[0].ID)); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"receipt_id": receipt.ID, "items": targets, "confirm_restore": true})
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			r := webTestRequest(h, http.MethodPost, "/api/accounts/import-leases", string(body), "Bearer "+webTestKey)
			if r.Code != 200 {
				t.Errorf("concurrent split restore status%d", r.Code)
			}
		}()
	}
	close(start)
	wg.Wait()
	get := managedTestRequest(t, h, http.MethodGet, "/api/accounts/import-leases/receipts/"+receipt.ID, nil, 200)
	var shown managedLeaseRestoreReceipt
	if json.Unmarshal(get.Body.Bytes(), &shown) != nil || shown.Items[0].Status != "unknown" || shown.Items[1].Status != "applied" || shown.Items[2].Status != "applied" {
		t.Fatal("partial/concurrent state lost", get.Body.String())
	}
	for i, item := range receipt.Items {
		lease, err := p.getManagedLease(t.Context(), item.ID)
		if i == 0 {
			if !errors.Is(err, errNotFound) {
				t.Fatal("audit failed lease persisted", err)
			}
		} else if err != nil || lease.Version != 1 {
			t.Fatal("concurrent lease duplicated", err)
		}
	}
	var count int
	if err := p.Store.DB.Pool.QueryRow(t.Context(), "SELECT count(*) FROM audit_logs WHERE action='account.lease.restore'").Scan(&count); err != nil || count != 2 {
		t.Fatal("independent apply audit count", count, err)
	}
}

func TestPGManagedSplitLeaseParentMutationAndExpiredCleanup(t *testing.T) {
	for _, mutation := range []string{"owner", "generation", "expires", "delete"} {
		t.Run(mutation, func(t *testing.T) {
			p, h := adminPGTestProxy(t)
			receipt, _ := managedSplitLeaseFixture(t, p, h, 2)
			w := splitLeaseWriter(p, receipt)
			if err := w.updateItem(t.Context(), 1, func(r *storedManagedLeaseReceipt) error { r.Items[0].Status = "running"; return nil }); err != nil {
				t.Fatal(err)
			}
			key, _ := managementStateKey("lease-restore", receipt.ID)
			if mutation == "delete" {
				if _, err := p.Store.DB.Pool.Exec(t.Context(), "DELETE FROM settings WHERE key=$1", key); err != nil {
					t.Fatal(err)
				}
			} else if err := p.managementState().Update(t.Context(), "lease-restore", receipt.ID, func(entry *managementStateEntry) error {
				var root storedManagedLeaseReceipt
				if err := json.Unmarshal(entry.Payload, &root); err != nil {
					return err
				}
				switch mutation {
				case "owner":
					root.ActualOwner = "other-owner"
				case "generation":
					root.Generation = newID("different_generation_")
				case "expires":
					entry.ExpiresAt = time.Now().Unix() - 1
				}
				entry.Payload, _ = json.Marshal(root)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if err := w.updateItem(t.Context(), 1, func(r *storedManagedLeaseReceipt) error { t.Fatal("changed parent reached callback"); return nil }); err == nil {
				t.Fatal("changed parent accepted cached child")
			}
			childKey, _ := managedLeaseSplitKey(receipt.ID, 1)
			var childEntry managementStateEntry
			if err := p.Store.DB.readDocument(t.Context(), p.Store.DB.Pool, childKey, &childEntry); err != nil {
				t.Fatal(err)
			}
			var state managedLeaseSplitItem
			_ = json.Unmarshal(childEntry.Payload, &state)
			if state.Item.Status != "running" {
				t.Fatal("parent mutation rewrote child")
			}
			// Expire both encrypted entry envelopes, then exercise unchanged
			// physical cleanup; root may already be absent in the delete case.
			for _, stateKey := range []string{key, childKey} {
				tx, err := p.Store.DB.Pool.Begin(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				var entry managementStateEntry
				err = p.Store.DB.readDocument(t.Context(), tx, stateKey, &entry)
				if err == nil && entry.Version > 0 {
					entry.ExpiresAt = time.Now().Unix() - 1
					err = p.Store.DB.writeDocument(t.Context(), tx, stateKey, entry)
				}
				if err == nil {
					err = tx.Commit(t.Context())
				} else {
					_ = tx.Rollback(context.Background())
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := p.managementState().PruneExpired(t.Context(), "lease-restore"); err != nil {
				t.Fatal(err)
			}
			var remaining int
			if err := p.Store.DB.Pool.QueryRow(t.Context(), "SELECT count(*) FROM settings WHERE key=ANY($1::text[])", []string{key, childKey}).Scan(&remaining); err != nil || remaining != 0 {
				t.Fatal("expired split metadata retained", remaining, err)
			}
		})
	}
}

func TestPGManagedSplitLeaseItemBindingAndIndependentPhases(t *testing.T) {
	p, h := adminPGTestProxy(t)
	receipt, _ := managedSplitLeaseFixture(t, p, h, 2)
	w := splitLeaseWriter(p, receipt)
	if err := w.updateItem(t.Context(), 2, func(r *storedManagedLeaseReceipt) error {
		r.Items[0].Status = "running"
		r.Items[0].ClaimedAt = time.Now().Unix()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	key, _ := managedLeaseSplitKey(receipt.ID, 2)
	var claimedXmin string
	if err := p.Store.DB.Pool.QueryRow(t.Context(), "SELECT xmin::text FROM settings WHERE key=$1", key).Scan(&claimedXmin); err != nil {
		t.Fatal(err)
	}
	if err := w.updateItem(t.Context(), 2, func(r *storedManagedLeaseReceipt) error {
		if r.Items[0].Status != "running" {
			t.Fatal("claim not durable")
		}
		r.Items[0].Status = "unknown"
		r.Items[0].Code = "result_not_confirmed"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var finalizedXmin string
	if err := p.Store.DB.Pool.QueryRow(t.Context(), "SELECT xmin::text FROM settings WHERE key=$1", key).Scan(&finalizedXmin); err != nil || claimedXmin == finalizedXmin {
		t.Fatal("claim and finalize not independent", err)
	}
	wrongKey, _ := managedLeaseSplitKey(receipt.ID, 1)
	if _, err := p.Store.DB.Pool.Exec(t.Context(), "INSERT INTO settings(key,value) SELECT $1,value FROM settings WHERE key=$2", wrongKey, key); err != nil {
		t.Fatal(err)
	}
	if err := w.updateItem(t.Context(), 1, func(*storedManagedLeaseReceipt) error { t.Fatal("mismatched item reached callback"); return nil }); err == nil {
		t.Fatal("state moved to another AccountIndex accepted")
	}
}

func TestPGManagedSplitLeaseParentDeletionBlocksCachedClaim(t *testing.T) {
	p, h := adminPGTestProxy(t)
	receipt, _ := managedSplitLeaseFixture(t, p, h, 2)
	w := splitLeaseWriter(p, receipt)
	if err := w.updateItem(t.Context(), 1, func(r *storedManagedLeaseReceipt) error { r.Items[0].Status = "running"; return nil }); err != nil {
		t.Fatal(err)
	}
	key, _ := managementStateKey("lease-restore", receipt.ID)
	deleter, err := p.Store.DB.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer deleter.Rollback(context.Background())
	if _, err := deleter.Exec(t.Context(), "DELETE FROM settings WHERE key=$1", key); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- w.updateItem(t.Context(), 2, func(*storedManagedLeaseReceipt) error { t.Error("deleted parent reached item callback"); return nil })
	}()
	select {
	case err := <-done:
		t.Fatal("root row lock bypassed", err)
	case <-time.After(30 * time.Millisecond):
	}
	if err := deleter.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, errVersionConflict) {
			t.Fatal("deleted parent not rejected", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("parent deletion left claim blocked")
	}
	child, _ := managedLeaseSplitKey(receipt.ID, 2)
	var count int
	if err := p.Store.DB.Pool.QueryRow(t.Context(), "SELECT count(*) FROM settings WHERE key=$1", child).Scan(&count); err != nil || count != 0 {
		t.Fatal("orphan child claim created", err)
	}
}

func TestPGManagedSplitLeaseHydrationRejectsExpiryAcrossChildCleanup(t *testing.T) {
	p, h := adminPGTestProxy(t)
	receipt, _ := managedSplitLeaseFixture(t, p, h, 2)
	if err := p.managementState().Update(t.Context(), "lease-restore", receipt.ID, func(entry *managementStateEntry) error { entry.ExpiresAt = time.Now().Unix() + 2; return nil }); err != nil {
		t.Fatal(err)
	}
	parent, err := p.managementState().Get(t.Context(), "lease-restore", receipt.ID)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err = decodeManagedLeaseReceipt(parent, receipt.ActualOwner)
	if err != nil {
		t.Fatal(err)
	}
	w := splitLeaseWriter(p, receipt)
	if err := w.updateItem(t.Context(), 1, func(r *storedManagedLeaseReceipt) error { r.Items[0].Status = "applied"; return nil }); err != nil {
		t.Fatal(err)
	}
	// Hold the valid parent's decoded snapshot, then let normal TTL cleanup
	// remove the item before the separate hydration query.
	for time.Now().Unix() < parent.ExpiresAt {
		time.Sleep(10 * time.Millisecond)
	}
	if err := p.managementState().PruneExpired(t.Context(), "lease-restore"); err != nil {
		t.Fatal(err)
	}
	if err := p.hydrateManagedSplitLeaseReceipt(t.Context(), parent, &receipt); !errors.Is(err, errVersionConflict) {
		t.Fatal("expired child presented as pending", err)
	}
}

func TestPGManagedSplitLeaseSingleConnectionAndCanceledApply(t *testing.T) {
	p, h := adminPGTestProxy(t)
	receipt, targets := managedSplitLeaseFixture(t, p, h, 3)
	original := p.Store.DB.Pool
	config := original.Config()
	config.MaxConns, config.MinConns = 1, 0
	one, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	p.Store.DB.Pool = one
	response := managedTestRequest(t, h, http.MethodPost, "/api/accounts/import-leases", map[string]any{"receipt_id": receipt.ID, "items": targets[:1], "confirm_restore": true}, 200)
	p.Store.DB.Pool = original
	one.Close()
	if !strings.Contains(response.Body.String(), `"status":"applied"`) {
		t.Fatal("split single-connection restore failed", response.Body.String())
	}
	if _, err := p.Store.DB.Pool.Exec(t.Context(), `CREATE FUNCTION block_split_apply() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.action='account.lease.restore' THEN PERFORM pg_advisory_xact_lock(761051); END IF; RETURN NEW; END $$; CREATE TRIGGER block_split_apply BEFORE INSERT ON audit_logs FOR EACH ROW EXECUTE FUNCTION block_split_apply()`); err != nil {
		t.Fatal(err)
	}
	blocker, err := p.Store.DB.Pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, _ = blocker.Exec(ctx, `SELECT pg_advisory_unlock(761051)`)
		blocker.Release()
	}()
	if _, err := blocker.Exec(t.Context(), `SELECT pg_advisory_lock(761051)`); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"receipt_id": receipt.ID, "items": targets[1:], "confirm_restore": true})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	request := httptest.NewRequest(http.MethodPost, "/api/accounts/import-leases", bytes.NewReader(raw)).WithContext(ctx)
	request.Header.Set("Authorization", "Bearer "+webTestKey)
	recorder := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { h.ServeHTTP(recorder, request); close(done) }()
	childKey, _ := managedLeaseSplitKey(receipt.ID, 2)
	deadline := time.Now().Add(3 * time.Second)
	for {
		var entry managementStateEntry
		if err := p.Store.DB.readDocument(t.Context(), p.Store.DB.Pool, childKey, &entry); err != nil {
			t.Fatal(err)
		}
		var state managedLeaseSplitItem
		_ = json.Unmarshal(entry.Payload, &state)
		if state.Item.Status == "running" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("split claim never committed")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("split cancel ignored finalization deadline")
	}
	get := managedTestRequest(t, h, http.MethodGet, "/api/accounts/import-leases/receipts/"+receipt.ID, nil, 200)
	var shown managedLeaseRestoreReceipt
	if json.Unmarshal(get.Body.Bytes(), &shown) != nil || shown.Items[0].Status != "applied" || shown.Items[1].Status != "unknown" || shown.Items[1].Code != "lease_restore_not_confirmed" || shown.Items[2].Status != "pending" {
		t.Fatal("split cancel changed acknowledged prefix/unknown/tail", get.Body.String())
	}
	for _, item := range receipt.Items[1:] {
		if _, err := p.getManagedLease(t.Context(), item.ID); !errors.Is(err, errNotFound) {
			t.Fatal("canceled lease persisted", err)
		}
	}
	replay := managedTestRequest(t, h, http.MethodPost, "/api/accounts/import-leases", map[string]any{"receipt_id": receipt.ID, "items": targets[1:2], "confirm_restore": true}, 200)
	if !strings.Contains(replay.Body.String(), `"status":"unknown"`) {
		t.Fatal("ambiguous split apply was retried")
	}
}
