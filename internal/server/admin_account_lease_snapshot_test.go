package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"
)

func TestManagedLeaseAccountSnapshot(t *testing.T) {
	exerciseManagedLeaseAccountSnapshot(t, webTestSetup)
}

func TestPGManagedLeaseAccountSnapshot(t *testing.T) {
	exerciseManagedLeaseAccountSnapshot(t, adminPGTestProxy)
}

func exerciseManagedLeaseAccountSnapshot(t *testing.T, setup func(*testing.T) (*Proxy, http.Handler)) {
	p, _ := setup(t)
	a := managedTestAccount(t, p, "snapshot-a", nil)
	b := managedTestAccount(t, p, "snapshot-b", nil)
	want := managedAccountLease{Version: 3, ExpiresAt: 2100000000, AutoDisable: true, RestoreOnExtend: true}
	if err := p.updateManagedLease(t.Context(), b.ID, "", func(lease *managedAccountLease, _ *Account) error { *lease = want; return nil }); err != nil {
		t.Fatal(err)
	}
	for _, credentials := range []bool{false, true} {
		var before int64
		if p.Store.DB != nil {
			before = p.Store.DB.Pool.Stat().AcquireCount()
		}
		snapshots, err := p.readManagedLeaseAccounts(t.Context(), []string{b.ID, a.ID, "missing-snapshot", b.ID}, credentials)
		if err != nil || len(snapshots) != 2 {
			t.Fatalf("snapshot: count=%d err=%v", len(snapshots), err)
		}
		if p.Store.DB != nil && p.Store.DB.Pool.Stat().AcquireCount()-before != 1 {
			t.Fatal("snapshot must use one pool acquisition, not per-account reads")
		}
		for _, expected := range []Account{a, b} {
			snapshot := snapshots[expected.ID]
			if snapshot.Account.ID != expected.ID || snapshot.Account.Name != expected.Name || snapshot.Account.Version != expected.Version || snapshot.Account.Enabled != expected.Enabled || snapshot.CredentialErr != nil {
				t.Fatal("account metadata changed", snapshot.Account.ID)
			}
			if credentials && snapshot.Account.AccessToken != expected.AccessToken || !credentials && snapshot.Account.AccessToken != "" {
				t.Fatal("credential projection mismatch")
			}
		}
		lease, err := snapshots[b.ID].lease(true)
		if err != nil || lease != want {
			t.Fatal("lease changed", lease, err)
		}
		lease, err = snapshots[a.ID].lease(true)
		if err != nil || lease.Version != 0 {
			t.Fatal("missing lease is not zero", lease, err)
		}
	}
	bundle, err := p.exportManagedBundleLeases(t.Context(), []Account{b, a})
	if err != nil || len(bundle.Leases) != 1 || bundle.Leases[0].AccountIndex != 1 || bundle.Leases[0].ExpiresAt != want.ExpiresAt {
		t.Fatal("export order/policy changed", bundle, err)
	}
	b.Version++
	if _, err := p.exportManagedBundleLeases(t.Context(), []Account{b, a}); !errors.Is(err, errVersionConflict) {
		t.Fatal("stale version accepted", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := p.readManagedLeaseAccounts(ctx, []string{a.ID}, true); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation ignored", err)
	}
}

func TestManagedLeaseSnapshotEntryVisibility(t *testing.T) {
	s := managedLeaseAccountSnapshot{Entry: &managementStateEntry{Version: 1, ExpiresAt: time.Now().Unix() - 1, Payload: json.RawMessage(`{"version":"corrupt"}`)}}
	if lease, err := s.lease(true); err != nil || lease.Version != 0 {
		t.Fatal("expired lease must behave as missing before parsing payload", lease, err)
	}
	if _, err := s.lease(false); err == nil {
		t.Fatal("backup must retain raw-document validation")
	}
	s.Entry.Version = 0
	if lease, err := s.lease(false); err != nil || lease.Version != 0 {
		t.Fatal("zero-version document must behave as absent", lease, err)
	}
}

func TestPGManagedLeaseSnapshotCorruptionIsolated(t *testing.T) {
	p, _ := adminPGTestProxy(t)
	a := managedTestAccount(t, p, "snapshot-valid", nil)
	b := managedTestAccount(t, p, "snapshot-bad-credential", nil)
	c := managedTestAccount(t, p, "snapshot-bad-lease", nil)
	if _, err := p.Store.DB.Pool.Exec(t.Context(), `UPDATE account_credentials SET credentials_cipher=decode('00','hex') WHERE account_id=$1`, b.ID); err != nil {
		t.Fatal(err)
	}
	key, _ := managementStateKey("account-leases", c.ID)
	if _, err := p.Store.DB.Pool.Exec(t.Context(), `INSERT INTO settings(key,value) VALUES($1,'{"ciphertext":"AA=="}'::jsonb)`, key); err != nil {
		t.Fatal(err)
	}
	snapshots, err := p.readManagedLeaseAccounts(t.Context(), []string{a.ID, b.ID, c.ID}, true)
	if err != nil || len(snapshots) != 3 || snapshots[a.ID].CredentialErr != nil || snapshots[b.ID].CredentialErr == nil || snapshots[c.ID].LeaseErr == nil {
		t.Fatal("corruption was hidden or affected unrelated rows", err)
	}
	if _, err := p.exportManagedBundleLeases(t.Context(), []Account{b, a}); err != nil {
		t.Fatal("lease export unnecessarily decrypted account credentials", err)
	}
	if _, err := p.exportManagedBundleLeases(t.Context(), []Account{c}); err == nil {
		t.Fatal("export ignored unreadable lease")
	}
	management := &managedBundleManagement{Version: 1}
	report := ImportReport{}
	for i, account := range []Account{a, b, c} {
		management.Leases = append(management.Leases, managedBundleLease{AccountIndex: i + 1, ExpiresAt: 2100000000})
		report.Results = append(report.Results, ImportResult{ID: account.ID, Name: account.Name, Status: "duplicate"})
	}
	receipt := p.prepareManagedBundleLeases(t.Context(), management, report, []Account{a, b, c}, false)
	if len(receipt.Items) != 3 || receipt.Items[0].Status != "pending" || receipt.Items[1].Code != "account_changed" || receipt.Items[2].Code != "lease_unavailable" {
		t.Fatal("per-item preparation outcomes changed", receipt.Items)
	}
}
