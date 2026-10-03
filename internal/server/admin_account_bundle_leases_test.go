package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestManagedBundleLeaseRestore(t *testing.T) { exerciseManagedBundleLeaseRestore(t, webTestSetup) }
func TestPGManagedBundleLeaseRestore(t *testing.T) {
	exerciseManagedBundleLeaseRestore(t, adminPGTestProxy)
}
func exerciseManagedBundleLeaseRestore(t *testing.T, setup func(*testing.T) (*Proxy, http.Handler)) {
	p, h := setup(t)
	ctx := context.Background()
	a := managedTestAccount(t, p, "lease-backup-source", nil)
	future := time.Now().Add(24 * time.Hour).Unix()
	r := managedTestRequest(t, h, "PUT", "/api/accounts/"+a.ID+"/lease", map[string]any{"version": 0, "account_version": a.Version, "expires_at": future, "auto_disable": true, "restore_on_extend": true}, 200)
	if !strings.Contains(r.Body.String(), "restore_on_extend") {
		t.Fatal("source lease missing")
	}
	export := map[string]any{"items": []managedAccountItem{{ID: a.ID, Version: a.Version}}, "include_leases": true}
	managedTestRequest(t, h, "POST", "/api/accounts/export", export, 400)
	r = managedTestRequest(t, h, "POST", "/api/accounts/export/credentials", export, 200)
	if strings.Contains(r.Body.String(), "disabled_version") || strings.Contains(r.Body.String(), "disabled_by_lease") {
		t.Fatal("backup copied runtime ownership")
	}
	var bundle managedAccountBundle
	if json.Unmarshal(r.Body.Bytes(), &bundle) != nil || bundle.Management == nil || len(bundle.Management.Leases) != 1 || bundle.Management.Leases[0].ExpiresAt != future {
		t.Fatal("backup did not include lease")
	}
	delete(export, "include_leases")
	r = managedTestRequest(t, h, "POST", "/api/accounts/export/credentials", export, 200)
	if strings.Contains(r.Body.String(), "management") {
		t.Fatal("leases exported without selection")
	}
	// New credentials create a new stable target; duplicate names require the
	// existing explicit account replacement path, never name-based lease writes.
	bundle.Accounts[0].Name = "lease-restored-target"
	bundle.Accounts[0].Credentials = json.RawMessage(`{"access_token":"synthetic-lease-restore-token","refresh_token":"synthetic-lease-restore-refresh","expires_at":2100000000}`)
	request := map[string]any{"data": bundle, "dry_run": true}
	r = managedTestRequest(t, h, "POST", "/api/accounts/import-bundle", request, 200)
	var imported struct {
		Results      []ImportResult             `json:"results"`
		LeaseRestore managedLeaseRestoreReceipt `json:"lease_restore"`
	}
	if json.Unmarshal(r.Body.Bytes(), &imported) != nil || imported.LeaseRestore.ID != "" || imported.LeaseRestore.Items[0].Status != "pending_import" {
		t.Fatal("account preview produced executable lease target")
	}
	if _, err := p.Store.GetMetadata("lease-restored-target"); !errors.Is(err, errNotFound) {
		t.Fatal("dryrun created account")
	}
	request["dry_run"] = false
	r = managedTestRequest(t, h, "POST", "/api/accounts/import-bundle", request, 200)
	if json.Unmarshal(r.Body.Bytes(), &imported) != nil || imported.LeaseRestore.ID == "" || imported.LeaseRestore.Items[0].Status != "pending" {
		t.Fatal("receipt missing", r.Body.String())
	}
	item := imported.LeaseRestore.Items[0]
	if item.ID == a.ID || item.ID == "" || item.AccountVersion < 1 {
		t.Fatal("new target ID missing")
	}
	if _, err := p.getManagedLease(ctx, item.ID); !errors.Is(err, errNotFound) {
		t.Fatal("account import applied lease without consent")
	}
	target := managedLeaseRestoreTarget{item.AccountIndex, item.ID, item.AccountVersion, item.LeaseVersion}
	body := map[string]any{"receipt_id": imported.LeaseRestore.ID, "items": []managedLeaseRestoreTarget{target}, "dry_run": true}
	r = managedTestRequest(t, h, "POST", "/api/accounts/import-leases", body, 200)
	if !strings.Contains(r.Body.String(), `"status":"pending"`) {
		t.Fatal("lease preview failed")
	}
	if _, err := p.getManagedLease(ctx, item.ID); !errors.Is(err, errNotFound) {
		t.Fatal("lease dryrun wrote")
	}
	body["dry_run"] = false
	managedTestRequest(t, h, "POST", "/api/accounts/import-leases", body, 400)
	body["confirm_restore"] = true
	// Another owner cannot inspect or use this receipt.
	raw, _ := json.Marshal(body)
	unauthorized := webTestRequest(h, "POST", "/api/accounts/import-leases", string(raw), "")
	if unauthorized.Code != 401 {
		t.Fatal("anonymous lease restore")
	}
	wrong := target
	wrong.ID = a.ID
	body["items"] = []managedLeaseRestoreTarget{wrong}
	managedTestRequest(t, h, "POST", "/api/accounts/import-leases", body, 409)
	body["items"] = []managedLeaseRestoreTarget{target}
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := webTestRequest(h, "POST", "/api/accounts/import-leases", string(raw), "Bearer "+webTestKey)
			if r.Code != 200 {
				t.Errorf("concurrent restore HTTP%d", r.Code)
			}
		}()
	}
	wg.Wait()
	lease, err := p.getManagedLease(ctx, item.ID)
	if err != nil || lease.Version != 1 || lease.ExpiresAt != future || !lease.AutoDisable || lease.DisabledByLease || lease.DisabledVersion != 0 {
		t.Fatal("lease duplicate/provenance", lease, err)
	}
	r = managedTestRequest(t, h, "POST", "/api/accounts/import-leases", body, 200)
	if !strings.Contains(r.Body.String(), `"status":"applied"`) {
		t.Fatal("replay not persisted", r.Body.String())
	}
	current, _ := p.Store.Get(item.ID)
	if current.ExpiresAt != 2100000000 || !current.Enabled {
		t.Fatal("lease changed token expiry or enable state")
	}
	managedTestRequest(t, h, "GET", "/api/accounts/import-leases/receipts/"+imported.LeaseRestore.ID, nil, 200)
	// A duplicate import may explicitly restore the same ID, but overwriting
	// an existing lease requires a separate confirmation and both versions.
	bundle.Management.Leases[0].ExpiresAt = time.Now().Unix() - 60
	request["data"] = bundle
	r = managedTestRequest(t, h, "POST", "/api/accounts/import-bundle", request, 200)
	_ = json.Unmarshal(r.Body.Bytes(), &imported)
	item = imported.LeaseRestore.Items[0]
	target = managedLeaseRestoreTarget{item.AccountIndex, item.ID, item.AccountVersion, item.LeaseVersion}
	body = map[string]any{"receipt_id": imported.LeaseRestore.ID, "items": []managedLeaseRestoreTarget{target}, "dry_run": false, "confirm_restore": true}
	r = managedTestRequest(t, h, "POST", "/api/accounts/import-leases", body, 200)
	if !strings.Contains(r.Body.String(), "confirm_expired_required") {
		t.Fatal("expired restore lacked confirmation")
	}
	body["confirm_expired"] = true
	r = managedTestRequest(t, h, "POST", "/api/accounts/import-leases", body, 200)
	if !strings.Contains(r.Body.String(), "confirm_replace_required") {
		t.Fatal("replace restore lacked confirmation")
	}
	body["confirm_replace"] = true
	r = managedTestRequest(t, h, "POST", "/api/accounts/import-leases", body, 200)
	if !strings.Contains(r.Body.String(), `"status":"applied"`) {
		t.Fatal("expired restore failed", r.Body.String())
	}
	current, _ = p.Store.Get(item.ID)
	lease, _ = p.getManagedLease(ctx, item.ID)
	if current.Enabled || !lease.DisabledByLease || lease.DisabledVersion != current.Version {
		t.Fatal("fresh local expiry did not establish exact ownership")
	}
	// Restoring a future lease cannot re-enable even a lease-disabled account:
	// imported configuration carries no proof authorizing that restoration.
	bundle.Management.Leases[0].ExpiresAt = future
	request["data"] = bundle
	r = managedTestRequest(t, h, "POST", "/api/accounts/import-bundle", request, 200)
	_ = json.Unmarshal(r.Body.Bytes(), &imported)
	item = imported.LeaseRestore.Items[0]
	body = map[string]any{"receipt_id": imported.LeaseRestore.ID, "items": []managedLeaseRestoreTarget{{item.AccountIndex, item.ID, item.AccountVersion, item.LeaseVersion}}, "confirm_restore": true, "confirm_replace": true}
	managedTestRequest(t, h, "POST", "/api/accounts/import-leases", body, 200)
	current, _ = p.Store.Get(item.ID)
	lease, _ = p.getManagedLease(ctx, item.ID)
	if current.Enabled || lease.DisabledByLease || lease.DisabledVersion != 0 || !lease.RestoreOnExtend {
		t.Fatal("imported policy improperly re-enabled or inherited ownership")
	}
	// Cross-admin and expiration are rejected without changing lease state.
	err = p.managementState().Update(ctx, "lease-restore", imported.LeaseRestore.ID, func(entry *managementStateEntry) error {
		var stored storedManagedLeaseReceipt
		_ = json.Unmarshal(entry.Payload, &stored)
		stored.Owner = "different-owner"
		entry.Payload, _ = json.Marshal(stored)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	managedTestRequest(t, h, "GET", "/api/accounts/import-leases/receipts/"+imported.LeaseRestore.ID, nil, 404)
	err = p.managementState().Update(ctx, "lease-restore", imported.LeaseRestore.ID, func(entry *managementStateEntry) error { entry.ExpiresAt = time.Now().Unix() - 1; return nil })
	if err != nil {
		t.Fatal(err)
	}
	managedTestRequest(t, h, "POST", "/api/accounts/import-leases", body, 409)
}

func TestManagedBundleLeaseValidationAndPartial(t *testing.T) {
	exerciseManagedBundleLeasePartial(t, webTestSetup)
}
func TestPGManagedBundleLeaseValidationAndPartial(t *testing.T) {
	exerciseManagedBundleLeasePartial(t, adminPGTestProxy)
}
func exerciseManagedBundleLeasePartial(t *testing.T, setup func(*testing.T) (*Proxy, http.Handler)) {
	p, h := setup(t)
	a := managedTestAccount(t, p, "lease-partial-a", nil)
	b := managedTestAccount(t, p, "lease-partial-b", nil)
	makeBundle := func() managedAccountBundle {
		return managedAccountBundle{Type: "codex-agent-accounts", Version: 1, Accounts: []managedAccountBackup{{Name: a.Name, Concurrency: 1, Weight: 1, WindowDays: 1, Enabled: true, Credentials: canonicalManagedCredentials(a)}, {Name: b.Name, Concurrency: 1, Weight: 1, WindowDays: 1, Enabled: true, Credentials: canonicalManagedCredentials(b)}}, Management: &managedBundleManagement{Version: 1, Leases: []managedBundleLease{{1, 2100000000, true, true}, {2, 2100000000, false, false}}}}
	}
	bundle := makeBundle()
	bundle.Management.Leases[1].AccountIndex = 1
	managedTestRequest(t, h, "POST", "/api/accounts/import-bundle", map[string]any{"data": bundle, "dry_run": true}, 400)
	bundle = makeBundle()
	r := managedTestRequest(t, h, "POST", "/api/accounts/import-bundle", map[string]any{"data": bundle}, 200)
	var response struct {
		LeaseRestore managedLeaseRestoreReceipt `json:"lease_restore"`
	}
	_ = json.Unmarshal(r.Body.Bytes(), &response)
	_, err := p.Store.UpdateAccountVersion(a.ID, a.Version, func(a *Account) error { a.Notes = "manual intervening edit"; return nil })
	if err != nil {
		t.Fatal(err)
	}
	targets := []managedLeaseRestoreTarget{}
	for _, item := range response.LeaseRestore.Items {
		targets = append(targets, managedLeaseRestoreTarget{item.AccountIndex, item.ID, item.AccountVersion, item.LeaseVersion})
	}
	r = managedTestRequest(t, h, "POST", "/api/accounts/import-leases", map[string]any{"receipt_id": response.LeaseRestore.ID, "items": targets, "confirm_restore": true}, 200)
	var result managedLeaseRestoreReceipt
	_ = json.Unmarshal(r.Body.Bytes(), &result)
	if result.Items[0].Status != "failed" || result.Items[0].Code != "version_conflict" || result.Items[1].Status != "applied" {
		t.Fatal("partial conflict hidden", r.Body.String())
	}
	if _, err = p.getManagedLease(context.Background(), a.ID); !errors.Is(err, errNotFound) {
		t.Fatal("conflicted lease changed")
	}
	// Receipt validation rejects old ownership fields before account import.
	raw, _ := json.Marshal(makeBundle())
	var unsafe map[string]any
	_ = json.Unmarshal(raw, &unsafe)
	management := unsafe["management"].(map[string]any)
	management["leases"].([]any)[0].(map[string]any)["disabled_by_lease"] = true
	managedTestRequest(t, h, "POST", "/api/accounts/import-bundle", map[string]any{"data": unsafe}, 400)
	if p.Store.DB != nil {
		_, err = p.Store.DB.Pool.Exec(context.Background(), `CREATE FUNCTION reject_lease_restore_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.action='account.lease.restore' THEN RAISE EXCEPTION 'synthetic lease restore write failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER reject_lease_restore BEFORE INSERT ON audit_logs FOR EACH ROW EXECUTE FUNCTION reject_lease_restore_audit()`)
		if err != nil {
			t.Fatal(err)
		}
		current, _ := p.Store.Get(a.ID)
		bundle = makeBundle()
		bundle.Accounts[0].Credentials = canonicalManagedCredentials(current)
		r = managedTestRequest(t, h, "POST", "/api/accounts/import-bundle", map[string]any{"data": bundle}, 200)
		_ = json.Unmarshal(r.Body.Bytes(), &response)
		item := response.LeaseRestore.Items[0]
		r = managedTestRequest(t, h, "POST", "/api/accounts/import-leases", map[string]any{"receipt_id": response.LeaseRestore.ID, "items": []managedLeaseRestoreTarget{{item.AccountIndex, item.ID, item.AccountVersion, item.LeaseVersion}}, "confirm_restore": true}, 200)
		if !strings.Contains(r.Body.String(), `"status":"unknown"`) {
			t.Fatal("uncertain persistence called successful")
		}
		if _, err = p.getManagedLease(context.Background(), a.ID); !errors.Is(err, errNotFound) {
			t.Fatal("failed lease audit did not roll back")
		}
	}
}
