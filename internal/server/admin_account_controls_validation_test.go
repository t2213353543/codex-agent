package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestManagedResetCreditFormats(t *testing.T) {
	for _, raw := range []string{
		`[{"creditId":"c","resetType":"codex_rate_limits","status":"available","expiresAt":"2099-01-01T00:00:00Z"}]`,
		`{"availableCount":"1","items":[{"credit_id":"c","reset_type":"codex_rate_limits","status":"available","expires_at":"2099-01-01T00:00:00Z"}]}`,
		`{"data":[{"id":"c","reset_type":"codex_rate_limits","status":"available","expires_at":"2099-01-01T00:00:00Z"}]}`,
	} {
		value, err := parseManagedResetCredits([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		credits := value.(managedResetCredits)
		if credits.AvailableCount == nil || *credits.AvailableCount != 1 || len(credits.Credits) != 1 || credits.Credits[0].ID != "c" {
			t.Fatal("reference credit shape not decoded")
		}
	}
	if _, err := parseManagedResetCredits([]byte(`{}`)); err == nil {
		t.Fatal("missing credit source presented as empty")
	}
}
func TestManagedResetCreditFreshValidation(t *testing.T) {
	p, _ := webTestSetup(t)
	a := managedTestAccount(t, p, "fresh-credit", nil)
	var consumes atomic.Int32
	var mode atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			consumes.Add(1)
			fmt.Fprint(w, `{"code":"reset","windows_reset":1}`)
			return
		}
		credit := map[string]any{"id": "c", "reset_type": "codex_rate_limits", "status": "available", "expires_at": "2099-01-01T00:00:00Z"}
		switch mode.Load() {
		case 0:
			credit["expires_at"] = "2000-01-01T00:00:00Z"
		case 1:
			delete(credit, "expires_at")
		case 2:
			credit["status"] = "redeemed"
		case 3:
			credit["reset_type"] = "other"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"available_count": 1, "credits": []any{credit}})
	}))
	defer server.Close()
	p.BaseURL = server.URL + "/backend-api/codex"
	for i := 0; i < 4; i++ {
		mode.Store(int32(i))
		snapshot, err := p.managedConsumeResetCredit(context.Background(), a, "c", fmt.Sprintf("request-%d", i))
		if err != nil || snapshot.OK || snapshot.Error == nil || snapshot.Error.Code != "credit_unavailable" {
			t.Fatal("invalid credit was not refused")
		}
	}
	if consumes.Load() != 0 {
		t.Fatal("expired/unknown/unavailable/wrong-type credit consumed")
	}
	mode.Store(4)
	snapshot, err := p.managedConsumeResetCredit(context.Background(), a, "c", "request-live")
	if err != nil || !snapshot.OK || consumes.Load() != 1 {
		t.Fatal("valid credit not consumed")
	}
	_, err = p.managedConsumeResetCredit(context.Background(), a, "c", "request-live")
	if err != nil || consumes.Load() != 1 {
		t.Fatal("consumption was not idempotent")
	}
}

func TestManagedLeaseCanceledWriteAtomic(t *testing.T) {
	p, _ := webTestSetup(t)
	a := managedTestAccount(t, p, "lease-cancel", nil)
	ctx, cancel := context.WithCancel(context.Background())
	err := p.updateManagedLease(ctx, a.ID, "", func(lease *managedAccountLease, a *Account) error {
		lease.Version = 1
		lease.AutoDisable = true
		lease.ExpiresAt = 1
		applyManagedLeaseExpiry(lease, a, time.Now())
		cancel()
		return nil
	})
	if err == nil {
		t.Fatal("canceled write succeeded")
	}
	current, _ := p.Store.Get(a.ID)
	if current.Enabled != a.Enabled || current.Version != a.Version {
		t.Fatal("failed lease write changed account")
	}
	if _, err := p.getManagedLease(context.Background(), a.ID); err != errNotFound {
		t.Fatal("failed lease write stored provenance")
	}
}
func TestPGManagedLeaseWriteFailureAtomic(t *testing.T) {
	p, _ := adminPGTestProxy(t)
	a := managedTestAccount(t, p, "lease-atomic", nil)
	ctx := context.Background()
	// A test-schema-only constraint fails encrypted lease persistence after the
	// account UPDATE. Both writes must roll back in the same transaction.
	_, err := p.Store.DB.Pool.Exec(ctx, `ALTER TABLE settings ADD CONSTRAINT reject_lease_test CHECK (NOT starts_with(key,'management/account-leases/')) NOT VALID`)
	if err != nil {
		t.Fatal(err)
	}
	change := func(lease *managedAccountLease, a *Account) error {
		lease.Version++
		lease.AutoDisable = true
		lease.ExpiresAt = 1
		applyManagedLeaseExpiry(lease, a, time.Now())
		return nil
	}
	if err = p.updateManagedLease(ctx, a.ID, "account.lease.update", change); err == nil {
		t.Fatal("injected write failure succeeded")
	}
	current, _ := p.Store.Get(a.ID)
	if !current.Enabled || current.Version != a.Version {
		t.Fatal("account disable escaped lease rollback")
	}
	_, err = p.Store.DB.Pool.Exec(ctx, `ALTER TABLE settings DROP CONSTRAINT reject_lease_test`)
	if err != nil {
		t.Fatal(err)
	}
	if err = p.updateManagedLease(ctx, a.ID, "account.lease.update", change); err != nil {
		t.Fatal(err)
	}
	current, _ = p.Store.Get(a.ID)
	disabledVersion := current.Version
	_, err = p.Store.DB.Pool.Exec(ctx, `ALTER TABLE settings ADD CONSTRAINT reject_lease_test CHECK (NOT starts_with(key,'management/account-leases/')) NOT VALID`)
	if err != nil {
		t.Fatal(err)
	}
	if err = p.updateManagedLease(ctx, a.ID, "account.lease.update", func(lease *managedAccountLease, a *Account) error {
		lease.ExpiresAt = time.Now().Add(time.Hour).Unix()
		lease.DisabledByLease = false
		a.Enabled = true
		return nil
	}); err == nil {
		t.Fatal("restore persisted despite state failure")
	}
	current, _ = p.Store.Get(a.ID)
	lease, err := p.getManagedLease(ctx, a.ID)
	if err != nil || current.Enabled || current.Version != disabledVersion || !lease.DisabledByLease {
		t.Fatal("restore escaped atomic rollback")
	}
}
