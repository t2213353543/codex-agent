package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func leaseWriterFixture(t *testing.T, p *Proxy) *managedLeaseReceiptWriter {
	t.Helper()
	w := &managedLeaseReceiptWriter{store: p.managementState(), id: newID("lease_restore_"), owner: "synthetic-owner"}
	receipt := storedManagedLeaseReceipt{Owner: w.owner, managedLeaseRestoreReceipt: managedLeaseRestoreReceipt{ID: w.id, Items: []managedLeaseRestoreItem{{managedBundleLease: managedBundleLease{AccountIndex: 1}, ID: "synthetic-account", Status: "pending"}}}}
	if err := w.store.Update(t.Context(), "lease-restore", w.id, func(entry *managementStateEntry) error {
		entry.ExpiresAt = time.Now().Add(time.Minute).Unix()
		entry.Payload, _ = json.Marshal(receipt)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return w
}

func TestPGManagedLeaseReceiptCachePreservesCommitAndConcurrentUpdates(t *testing.T) {
	p, _ := adminPGTestProxy(t)
	w := leaseWriterFixture(t, p)
	if err := w.update(t.Context(), func(r *storedManagedLeaseReceipt) error { r.Items[0].Status = "running"; return nil }); err != nil {
		t.Fatal(err)
	}
	firstVersion, firstXmin := w.version, w.xmin
	if err := w.update(t.Context(), func(r *storedManagedLeaseReceipt) error { r.Items[0].Status = "applied"; return nil }); err != nil {
		t.Fatal(err)
	}
	if w.version != firstVersion+1 || w.xmin == firstXmin {
		t.Fatal("receipt writes did not retain independent commits")
	}
	if err := w.store.Update(t.Context(), "lease-restore", w.id, func(entry *managementStateEntry) error {
		var r storedManagedLeaseReceipt
		if err := json.Unmarshal(entry.Payload, &r); err != nil {
			return err
		}
		r.Items = append(r.Items, managedLeaseRestoreItem{managedBundleLease: managedBundleLease{AccountIndex: 2}, Status: "applied"})
		entry.Payload, _ = json.Marshal(r)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := w.update(t.Context(), func(r *storedManagedLeaseReceipt) error {
		if len(r.Items) != 2 || r.Items[1].Status != "applied" {
			t.Fatal("cached receipt overwrote another writer's item")
		}
		r.Items[0].Code = "reviewed"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := w.update(t.Context(), func(r *storedManagedLeaseReceipt) error {
		r.Items[0].Status = "uncommitted"
		return errConflict
	}); !errors.Is(err, errConflict) || w.version != 0 {
		t.Fatal("failed mutation retained a speculative cache", err)
	}
	if err := w.update(t.Context(), func(r *storedManagedLeaseReceipt) error {
		if r.Items[0].Status != "applied" || len(r.Items) != 2 {
			t.Fatal("failed callback leaked slice changes")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPGManagedLeaseReceiptCacheRejectsChangedMalformedDocuments(t *testing.T) {
	p, _ := adminPGTestProxy(t)
	for _, plain := range []string{`{}`, `null`, `{"payload":{}}`, `{"version":1}`, `{"version":3,"expires_at":253402300799}`} {
		t.Run(plain, func(t *testing.T) {
			w := leaseWriterFixture(t, p)
			if err := w.update(t.Context(), func(*storedManagedLeaseReceipt) error { return nil }); err != nil {
				t.Fatal(err)
			}
			key, _ := managementStateKey("lease-restore", w.id)
			cipher, err := p.Store.DB.Encrypt([]byte(plain))
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(map[string]any{"ciphertext": cipher})
			// Deliberately omit the SQL version bump; xmin/ctid must notice.
			if _, err := p.Store.DB.Pool.Exec(t.Context(), `UPDATE settings SET value=$2 WHERE key=$1`, key, raw); err != nil {
				t.Fatal(err)
			}
			called := false
			if err := w.update(t.Context(), func(*storedManagedLeaseReceipt) error { called = true; return nil }); err == nil || called || w.version != 0 {
				t.Fatal("cache inherited missing authoritative document fields", err)
			}
			var unchanged bool
			if err := p.Store.DB.Pool.QueryRow(t.Context(), `SELECT value=$2::jsonb FROM settings WHERE key=$1`, key, raw).Scan(&unchanged); err != nil || !unchanged {
				t.Fatal("rejected malformed document was overwritten", err)
			}
		})
	}
}

func TestPGManagedLeaseReceiptCacheCASAgainstUncoordinatedWriter(t *testing.T) {
	p, _ := adminPGTestProxy(t)
	w := leaseWriterFixture(t, p)
	if err := w.update(t.Context(), func(*storedManagedLeaseReceipt) error { return nil }); err != nil {
		t.Fatal(err)
	}
	key, _ := managementStateKey("lease-restore", w.id)
	outerVersion := w.version
	// This direct SQL writer deliberately does not acquire the application
	// advisory lock, and recreates the same visible revision (ABA).
	err := w.update(t.Context(), func(r *storedManagedLeaseReceipt) error {
		r.Items[0].Status = "should-not-commit"
		_, err := p.Store.DB.Pool.Exec(t.Context(), `WITH gone AS (DELETE FROM settings WHERE key=$1 RETURNING key,value,version)
 INSERT INTO settings(key,value,version) SELECT key,value,version FROM gone`, key)
		return err
	})
	if !errors.Is(err, errVersionConflict) || w.version != 0 {
		t.Fatal("tuple CAS did not reject same-revision replacement", err)
	}
	var current int64
	if err := p.Store.DB.Pool.QueryRow(t.Context(), "SELECT version FROM settings WHERE key=$1", key).Scan(&current); err != nil || current != outerVersion {
		t.Fatal("CAS rejection wrote a new revision", err)
	}
	if err := w.update(t.Context(), func(r *storedManagedLeaseReceipt) error {
		if r.Items[0].Status != "pending" {
			t.Fatal("CAS-rejected callback leaked into authoritative receipt")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPGManagedLeaseReceiptCacheRejectsOwnerExpiryAndMissing(t *testing.T) {
	p, _ := adminPGTestProxy(t)
	for _, kind := range []string{"owner", "expiry", "missing"} {
		t.Run(kind, func(t *testing.T) {
			w := leaseWriterFixture(t, p)
			if err := w.update(t.Context(), func(*storedManagedLeaseReceipt) error { return nil }); err != nil {
				t.Fatal(err)
			}
			key, _ := managementStateKey("lease-restore", w.id)
			if kind == "missing" {
				if _, err := p.Store.DB.Pool.Exec(t.Context(), "DELETE FROM settings WHERE key=$1", key); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := w.store.Update(t.Context(), "lease-restore", w.id, func(entry *managementStateEntry) error {
					if kind == "expiry" {
						entry.ExpiresAt = time.Now().Unix() - 1
					} else {
						var receipt storedManagedLeaseReceipt
						_ = json.Unmarshal(entry.Payload, &receipt)
						receipt.Owner = "other-owner"
						entry.Payload, _ = json.Marshal(receipt)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			if err := w.update(t.Context(), func(*storedManagedLeaseReceipt) error { t.Fatal("unauthorized callback executed"); return nil }); err == nil || w.version != 0 {
				t.Fatal("stale owner/TTL/deletion bypassed", err)
			}
		})
	}
}

func TestPGManagedLeaseReceiptCacheChecksExpiryWithoutTupleChange(t *testing.T) {
	p, _ := adminPGTestProxy(t)
	w := leaseWriterFixture(t, p)
	if err := w.store.Update(t.Context(), "lease-restore", w.id, func(entry *managementStateEntry) error {
		entry.ExpiresAt = time.Now().Unix() + 1
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := w.update(t.Context(), func(*storedManagedLeaseReceipt) error { return nil }); err != nil {
		t.Fatal(err)
	}
	expires := w.entry.ExpiresAt
	for time.Now().Unix() < expires {
		time.Sleep(10 * time.Millisecond)
	}
	if err := w.update(t.Context(), func(*storedManagedLeaseReceipt) error { t.Fatal("expired cached callback executed"); return nil }); !errors.Is(err, errVersionConflict) {
		t.Fatal("unchanged tuple extended expired receipt", err)
	}
}

func TestPGManagedLeaseReceiptCacheDoesNotPublishTriggerChangedPayload(t *testing.T) {
	p, _ := adminPGTestProxy(t)
	w := leaseWriterFixture(t, p)
	if err := w.update(t.Context(), func(*storedManagedLeaseReceipt) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Store.DB.Pool.Exec(t.Context(), `CREATE FUNCTION preserve_receipt_value() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN NEW.value=OLD.value; RETURN NEW; END $$; CREATE TRIGGER preserve_receipt_value BEFORE UPDATE ON settings FOR EACH ROW EXECUTE FUNCTION preserve_receipt_value()`); err != nil {
		t.Fatal(err)
	}
	if err := w.update(t.Context(), func(r *storedManagedLeaseReceipt) error { r.Items[0].Status = "unpersisted"; return nil }); err != nil || w.version != 0 {
		t.Fatal("trigger-modified value was cached as original proposal", err)
	}
	if _, err := p.Store.DB.Pool.Exec(t.Context(), "DROP TRIGGER preserve_receipt_value ON settings"); err != nil {
		t.Fatal(err)
	}
	if err := w.update(t.Context(), func(r *storedManagedLeaseReceipt) error {
		if r.Items[0].Status != "pending" {
			t.Fatal("trigger-modified receipt was not re-read")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPGManagedLeaseReceiptCacheAuditFailureAndFinalizeRecovery(t *testing.T) {
	p, _ := adminPGTestProxy(t)
	w := leaseWriterFixture(t, p)
	if err := w.update(t.Context(), func(r *storedManagedLeaseReceipt) error { r.Items[0].Status = "running"; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Store.DB.Pool.Exec(t.Context(), `CREATE FUNCTION reject_receipt_write() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic receipt failure'; END $$; CREATE TRIGGER reject_receipt_write BEFORE UPDATE ON settings FOR EACH ROW EXECUTE FUNCTION reject_receipt_write()`); err != nil {
		t.Fatal(err)
	}
	if err := w.update(t.Context(), func(r *storedManagedLeaseReceipt) error { r.Items[0].Status = "applied"; return nil }); err == nil || w.version != 0 {
		t.Fatal("rejected finalization acknowledged/cached success")
	}
	if _, err := p.Store.DB.Pool.Exec(t.Context(), "DROP TRIGGER reject_receipt_write ON settings"); err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	finalCtx, stop := context.WithTimeout(context.WithoutCancel(cancelled), 5*time.Second)
	defer stop()
	if err := w.update(finalCtx, func(r *storedManagedLeaseReceipt) error {
		if r.Items[0].Status != "running" {
			t.Fatal("failed finalization changed durable status")
		}
		r.Items[0].Status, r.Items[0].Code = "unknown", "result_not_confirmed"
		return nil
	}); err != nil {
		t.Fatal("independent final context lost canceled-request cleanup", err)
	}
}

func TestPGManagedBundleLeaseRestoreCachedPoolOne(t *testing.T) {
	p, h := adminPGTestProxy(t)
	original := p.Store.DB.Pool
	config := original.Config()
	config.MaxConns, config.MinConns = 1, 0
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	p.Store.DB.Pool = pool
	t.Cleanup(func() { p.Store.DB.Pool = original; pool.Close() })
	a := managedTestAccount(t, p, "lease-pool-one", nil)
	bundle := managedAccountBundle{Type: "codex-agent-accounts", Version: 1, Accounts: []managedAccountBackup{{Name: a.Name, Credentials: canonicalManagedCredentials(a), Enabled: true, Concurrency: 1, Weight: 1, WindowDays: 1}}, Management: &managedBundleManagement{Version: 1, Leases: []managedBundleLease{{AccountIndex: 1, ExpiresAt: 2100000000, AutoDisable: true}}}}
	response := managedTestRequest(t, h, http.MethodPost, "/api/accounts/import-bundle", map[string]any{"data": bundle}, 200)
	var imported struct {
		LeaseRestore managedLeaseRestoreReceipt `json:"lease_restore"`
	}
	if json.Unmarshal(response.Body.Bytes(), &imported) != nil || len(imported.LeaseRestore.Items) != 1 {
		t.Fatal("receipt missing")
	}
	item := imported.LeaseRestore.Items[0]
	before := pool.Stat().AcquireCount()
	response = managedTestRequest(t, h, http.MethodPost, "/api/accounts/import-leases", map[string]any{"receipt_id": imported.LeaseRestore.ID, "items": []managedLeaseRestoreTarget{{item.AccountIndex, item.ID, item.AccountVersion, item.LeaseVersion}}, "confirm_restore": true}, 200)
	if !strings.Contains(response.Body.String(), `"status":"applied"`) {
		t.Fatal("one-connection restore failed", response.Body.String())
	}
	if acquires := pool.Stat().AcquireCount() - before; acquires != 6 {
		t.Fatalf("single-item restore expectedpreview+3transactions+auth+receipt: %d", acquires)
	}
}

func TestPGManagedBundleLeaseRestoreCancelAfterClaimKeepsUnknownAndPendingTail(t *testing.T) {
	p, h := adminPGTestProxy(t)
	a := managedTestAccount(t, p, "lease-cancel-first", nil)
	b := managedTestAccount(t, p, "lease-cancel-tail", nil)
	bundle := managedAccountBundle{Type: "codex-agent-accounts", Version: 1, Management: &managedBundleManagement{Version: 1}}
	for i, account := range []Account{a, b} {
		bundle.Accounts = append(bundle.Accounts, managedAccountBackup{Name: account.Name, Credentials: canonicalManagedCredentials(account), Enabled: true, Concurrency: 1, Weight: 1, WindowDays: 1})
		bundle.Management.Leases = append(bundle.Management.Leases, managedBundleLease{AccountIndex: i + 1, ExpiresAt: 2100000000, AutoDisable: true})
	}
	response := managedTestRequest(t, h, http.MethodPost, "/api/accounts/import-bundle", map[string]any{"data": bundle}, 200)
	var imported struct {
		LeaseRestore managedLeaseRestoreReceipt `json:"lease_restore"`
	}
	if json.Unmarshal(response.Body.Bytes(), &imported) != nil || len(imported.LeaseRestore.Items) != 2 {
		t.Fatal("receipt missing")
	}
	if _, err := p.Store.DB.Pool.Exec(t.Context(), `CREATE FUNCTION block_lease_apply() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.action='account.lease.restore' THEN PERFORM pg_advisory_xact_lock(761047); END IF; RETURN NEW; END $$; CREATE TRIGGER block_lease_apply BEFORE INSERT ON audit_logs FOR EACH ROW EXECUTE FUNCTION block_lease_apply()`); err != nil {
		t.Fatal(err)
	}
	blocker, err := p.Store.DB.Pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, _ = blocker.Exec(ctx, `SELECT pg_advisory_unlock(761047)`)
		blocker.Release()
	}()
	if _, err := blocker.Exec(t.Context(), `SELECT pg_advisory_lock(761047)`); err != nil {
		t.Fatal(err)
	}
	targets := []managedLeaseRestoreTarget{}
	for _, item := range imported.LeaseRestore.Items {
		targets = append(targets, managedLeaseRestoreTarget{item.AccountIndex, item.ID, item.AccountVersion, item.LeaseVersion})
	}
	body, _ := json.Marshal(map[string]any{"receipt_id": imported.LeaseRestore.ID, "items": targets, "confirm_restore": true})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	request := httptest.NewRequest(http.MethodPost, "/api/accounts/import-leases", bytes.NewReader(body)).WithContext(ctx)
	request.Header.Set("Authorization", "Bearer "+webTestKey)
	recorder := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { h.ServeHTTP(recorder, request); close(done) }()
	key, _ := managementStateKey("lease-restore", imported.LeaseRestore.ID)
	readReceipt := func() storedManagedLeaseReceipt {
		t.Helper()
		var entry managementStateEntry
		if err := p.Store.DB.readDocument(t.Context(), p.Store.DB.Pool, key, &entry); err != nil {
			t.Fatal(err)
		}
		var receipt storedManagedLeaseReceipt
		if err := json.Unmarshal(entry.Payload, &receipt); err != nil {
			t.Fatal(err)
		}
		return receipt
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if readReceipt().Items[0].Status == "running" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("claim never committed")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("canceled restore did not complete finalization")
	}
	stored := readReceipt()
	if stored.Items[0].Status != "unknown" || stored.Items[0].Code != "lease_restore_not_confirmed" || stored.Items[1].Status != "pending" {
		t.Fatalf("cancellation changed committed claim/tail boundary: %+v", stored.Items)
	}
	if _, err := p.getManagedLease(t.Context(), a.ID); !errors.Is(err, errNotFound) {
		t.Fatal("canceled apply persisted lease", err)
	}
	if _, err := p.getManagedLease(t.Context(), b.ID); !errors.Is(err, errNotFound) {
		t.Fatal("pending tail persisted lease", err)
	}
	// The unknown first item cannot be replayed into a second application.
	replay := managedTestRequest(t, h, http.MethodPost, "/api/accounts/import-leases", map[string]any{"receipt_id": imported.LeaseRestore.ID, "items": targets[:1], "confirm_restore": true}, 200)
	if !strings.Contains(replay.Body.String(), `"status":"unknown"`) {
		t.Fatal("ambiguous canceled item was auto-retried")
	}
}
