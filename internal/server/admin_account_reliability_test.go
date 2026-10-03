package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"
)

func TestManagedCRSAtomicCommit(t *testing.T)   { exerciseManagedCRSAtomicCommit(t, webTestSetup) }
func TestPGManagedCRSAtomicCommit(t *testing.T) { exerciseManagedCRSAtomicCommit(t, adminPGTestProxy) }
func exerciseManagedCRSAtomicCommit(t *testing.T, setup func(*testing.T) (*Proxy, http.Handler)) {
	p, _ := setup(t)
	ctx := context.Background()
	source := "https://synthetic-crs.invalid"
	item := managedCRSAccount{Row: managedCRSRow{SourceID: "atomic-source", Name: "atomic-crs", Status: "ready", Enabled: true, Priority: 7}, Notes: "preserved operator notes", Credentials: json.RawMessage(`{"access_token":"synthetic-crs-original","refresh_token":"synthetic-crs-refresh","account_id":"stable-crs-identity"}`)}
	key := managedCRSLinkKey(source, item.Row.SourceID)
	setReject := func(enabled bool) {
		t.Helper()
		if p.Store.DB == nil {
			return
		}
		query := `DROP TRIGGER IF EXISTS reject_crs_commit ON audit_logs; DROP FUNCTION IF EXISTS reject_crs_commit()`
		if enabled {
			query += `; CREATE FUNCTION reject_crs_commit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.action='account.crs.sync' THEN RAISE EXCEPTION 'synthetic CRS audit failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER reject_crs_commit BEFORE INSERT ON audit_logs FOR EACH ROW EXECUTE FUNCTION reject_crs_commit()`
		}
		if _, err := p.Store.DB.Pool.Exec(ctx, query); err != nil {
			t.Fatal(err)
		}
	}
	if p.Store.DB != nil {
		setReject(true)
		failed := p.syncManagedCRSAccount(ctx, source, item, false, false, nil)
		if failed.Status != "failed" || failed.Code != "storage_error" || failed.ID != "" {
			t.Fatal("failed atomic create outcome", failed)
		}
		if _, err := p.Store.Get(item.Row.Name); !errors.Is(err, errNotFound) {
			t.Fatal("account committed before CRS audit", err)
		}
		if _, err := p.managedCRSLink(ctx, key); !errors.Is(err, errNotFound) {
			t.Fatal("source link committed before audit", err)
		}
		setReject(false)
	}
	var wg sync.WaitGroup
	results := make(chan managedCRSResult, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); results <- p.syncManagedCRSAccount(ctx, source, item, false, false, nil) }()
	}
	wg.Wait()
	close(results)
	id := ""
	for result := range results {
		if result.Status == "failed" || result.ID == "" {
			t.Fatal("concurrent sync", result)
		}
		if id != "" && id != result.ID {
			t.Fatal("duplicate source created different accounts")
		}
		id = result.ID
	}
	a, err := p.Store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	link, err := p.managedCRSLink(ctx, key)
	if err != nil || link.ID != id {
		t.Fatal("atomic source link", link, err)
	}
	if p.Store.DB == nil {
		state, err := p.Store.readState()
		if err != nil || state.ManagedCRSLinks[key].ID != id {
			t.Fatal("file account and source link not colocated", err)
		}
	}
	item.Row.TargetID, item.Row.TargetVersion = id, a.Version
	item.Credentials = json.RawMessage(`{"access_token":"synthetic-crs-replaced","refresh_token":"synthetic-crs-new-refresh","account_id":"stable-crs-identity"}`)
	if p.Store.DB != nil {
		before, _ := p.managementState().Get(ctx, "crs-links", key)
		setReject(true)
		failed := p.syncManagedCRSAccount(ctx, source, item, false, false, nil)
		if failed.Status != "failed" || failed.ID != "" {
			t.Fatal("failed atomic update", failed)
		}
		unchanged, _ := p.Store.Get(id)
		after, _ := p.managementState().Get(ctx, "crs-links", key)
		if unchanged.AccessToken != a.AccessToken || unchanged.RefreshToken != a.RefreshToken || unchanged.Version != a.Version || after.Version != before.Version {
			t.Fatal("credentials committed before CRS audit")
		}
		setReject(false)
	}
	updated := p.syncManagedCRSAccount(ctx, source, item, false, false, nil)
	if updated.Status != "updated" || updated.ID != id {
		t.Fatal("atomic update failed", updated)
	}
	current, _ := p.Store.Get(id)
	if current.Notes != a.Notes || current.Enabled != a.Enabled || current.Priority != a.Priority || current.RefreshToken != "synthetic-crs-new-refresh" {
		t.Fatal("managed metadata or refreshed credentials lost")
	}
	if replay := p.syncManagedCRSAccount(ctx, source, item, false, false, nil); replay.Status != "duplicate" || replay.ID != id || replay.Version != current.Version {
		t.Fatal("retry reauthorized twice", replay)
	}
	if p.Store.DB == nil {
		oldKey := managedCRSLinkKey(source, "legacy-source")
		if err := p.managementState().Update(ctx, "crs-links", oldKey, func(e *managementStateEntry) error { e.Payload, _ = json.Marshal(managedCRSLink{ID: id}); return nil }); err != nil {
			t.Fatal(err)
		}
		item.Row.SourceID, item.Row.TargetVersion = "legacy-source", current.Version
		result := p.syncManagedCRSAccount(ctx, source, item, false, false, nil)
		if result.ID != id || result.Status != "updated" {
			t.Fatal("legacy source mapping not retained", result)
		}
		state, _ := p.Store.readState()
		if state.ManagedCRSLinks[oldKey].ID != id {
			t.Fatal("legacy link not atomically migrated")
		}
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	item.Row.SourceID, item.Row.TargetID, item.Row.Name = "canceled-source", "", "canceled-crs"
	item.Credentials = json.RawMessage(`{"access_token":"synthetic-crs-canceled"}`)
	if got := p.syncManagedCRSAccount(canceled, source, item, false, false, nil); got.Status != "failed" {
		t.Fatal("canceled sync succeeded")
	}
	if _, err := p.Store.Get(item.Row.Name); !errors.Is(err, errNotFound) {
		t.Fatal("canceled sync wrote account")
	}
}

func TestManagedScheduleIdempotencyAndOrphans(t *testing.T) {
	exerciseManagedScheduleIdempotencyAndOrphans(t, webTestSetup)
}
func TestPGManagedScheduleIdempotencyAndOrphans(t *testing.T) {
	exerciseManagedScheduleIdempotencyAndOrphans(t, adminPGTestProxy)
}
func exerciseManagedScheduleIdempotencyAndOrphans(t *testing.T, setup func(*testing.T) (*Proxy, http.Handler)) {
	p, h := setup(t)
	ctx := context.Background()
	a := managedTestAccount(t, p, "reliable-plans", nil)
	body := managedScheduleInput{RequestID: "request-id-stable-1", Kind: "models", Cron: "* * * * *", Enabled: true, MaxResults: 2}
	path := "/api/accounts/" + a.ID + "/schedules"
	decode := func(raw []byte) managedAccountSchedule {
		t.Helper()
		var plan managedAccountSchedule
		if json.Unmarshal(raw, &plan) != nil || plan.ID == "" {
			t.Fatal("invalid plan")
		}
		return plan
	}
	first := decode(managedTestRequest(t, h, "POST", path, body, 200).Body.Bytes())
	before, _ := p.managementState().Get(ctx, "account-schedules", a.ID)
	encoded, _ := json.Marshal(body)
	var concurrent sync.WaitGroup
	for i := 0; i < 8; i++ {
		concurrent.Add(1)
		go func() {
			defer concurrent.Done()
			r := webTestRequest(h, "POST", path, string(encoded), "Bearer "+webTestKey)
			var got managedAccountSchedule
			if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &got) != nil || got.ID != first.ID {
				t.Errorf("concurrent create replay %d %s", r.Code, r.Body.String())
			}
		}()
	}
	concurrent.Wait()
	for i := 0; i < 3; i++ {
		replayed := decode(managedTestRequest(t, h, "POST", path, body, 200).Body.Bytes())
		if replayed != first {
			t.Fatal("lost response created another plan")
		}
	}
	after, _ := p.managementState().Get(ctx, "account-schedules", a.ID)
	if after.Version != before.Version {
		t.Fatal("idempotent retry rewrote state")
	}
	changed := body
	changed.Model = ""
	changed.Cron = "*/5 * * * *"
	managedTestRequest(t, h, "POST", path, changed, 409)
	for _, invalid := range []string{"short", "invalid request id", "request_id_with_符号"} {
		bad := body
		bad.RequestID = invalid
		managedTestRequest(t, h, "POST", path, bad, 400)
	}
	changed = body
	changed.RequestID = ""
	changed.Version = first.Version
	changed.Enabled = false
	updated := decode(managedTestRequest(t, h, "PUT", "/api/account-schedules/"+first.ID, changed, 200).Body.Bytes())
	replayed := decode(managedTestRequest(t, h, "POST", path, body, 200).Body.Bytes())
	if replayed != updated {
		t.Fatal("retry hid current plan state")
	}
	managedTestRequest(t, h, "DELETE", fmt.Sprintf("/api/account-schedules/%s?version=%d", first.ID, updated.Version), nil, 200)
	managedTestRequest(t, h, "POST", path, body, 409)
	body.RequestID = "request-id-stable-2"
	plan := decode(managedTestRequest(t, h, "POST", path, body, 200).Body.Bytes())
	managedTestRequest(t, h, "DELETE", fmt.Sprintf("/api/accounts/%s?version=%d", a.ID, a.Version), nil, 200)
	if _, err := p.managementState().Get(ctx, "account-schedules", a.ID); !errors.Is(err, errNotFound) {
		t.Fatal("deleted account retained schedule", err)
	}
	managedTestRequest(t, h, "GET", "/api/account-schedules/"+plan.ID+"/results", nil, 404)
	// Older direct deletions and interrupted cleanups are reconciled after restart.
	b := managedTestAccount(t, p, "orphan-plan", nil)
	body.RequestID = "request-id-stable-3"
	plan = decode(managedTestRequest(t, h, "POST", "/api/accounts/"+b.ID+"/schedules", body, 200).Body.Bytes())
	if err := p.Store.DeleteVersion(b.ID, b.Version); err != nil {
		t.Fatal(err)
	}
	p.processManagedAccountSchedulesAt(ctx, time.Now().Add(time.Hour))
	if _, err := p.managementState().Get(ctx, "account-schedules", b.ID); !errors.Is(err, errNotFound) {
		t.Fatal("restart retained orphan plan", err)
	}
	managedTestRequest(t, h, "PUT", "/api/account-schedules/"+plan.ID, changed, 404)
	c := managedTestAccount(t, p, "batch-deleted-plan", nil)
	body.RequestID = "request-id-stable-4"
	managedTestRequest(t, h, "POST", "/api/accounts/"+c.ID+"/schedules", body, 200)
	managedTestRequest(t, h, "POST", "/api/accounts/actions", map[string]any{"action": "delete", "items": []managedAccountItem{{ID: c.ID, Version: c.Version}}}, 200)
	if _, err := p.managementState().Get(ctx, "account-schedules", c.ID); !errors.Is(err, errNotFound) {
		t.Fatal("bulk delete retained schedule", err)
	}
}

func TestManagedExpiredLeaseNoop(t *testing.T) { exerciseManagedExpiredLeaseNoop(t, webTestSetup) }
func TestPGManagedExpiredLeaseNoop(t *testing.T) {
	exerciseManagedExpiredLeaseNoop(t, adminPGTestProxy)
}
func exerciseManagedExpiredLeaseNoop(t *testing.T, setup func(*testing.T) (*Proxy, http.Handler)) {
	p, h := setup(t)
	a := managedTestAccount(t, p, "expired-noop", nil)
	ctx := context.Background()
	managedTestRequest(t, h, "PUT", "/api/accounts/"+a.ID+"/lease", map[string]any{"version": 0, "account_version": a.Version, "expires_at": time.Now().Unix() - 60, "auto_disable": true, "restore_on_extend": true}, 200)
	current, _ := p.Store.Get(a.ID)
	lease, _ := p.getManagedLease(ctx, a.ID)
	var raw []byte
	var entry managementStateEntry
	if p.Store.DB == nil {
		raw, _ = os.ReadFile(p.Store.Path)
		stamp := time.Unix(1700000000, 0)
		if err := os.Chtimes(p.Store.Path, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	} else {
		entry, _ = p.managementState().Get(ctx, "account-leases", a.ID)
	}
	for i := 0; i < 4; i++ {
		p.processManagedAccountLease(ctx, a.ID, time.Now().Add(time.Duration(i)*time.Minute))
	}
	after, _ := p.Store.Get(a.ID)
	afterLease, _ := p.getManagedLease(ctx, a.ID)
	if after.Version != current.Version || afterLease != lease {
		t.Fatal("already-disabled expiry mutated provenance")
	}
	if p.Store.DB == nil {
		got, _ := os.ReadFile(p.Store.Path)
		if string(raw) != string(got) {
			t.Fatal("no-op rewrote account state")
		}
		info, err := os.Stat(p.Store.Path)
		if err != nil || info.ModTime().Unix() != 1700000000 {
			t.Fatal("no-op rewrote account file", err)
		}
	} else {
		got, _ := p.managementState().Get(ctx, "account-leases", a.ID)
		if got.Version != entry.Version || got.UpdatedAt != entry.UpdatedAt || string(got.Payload) != string(entry.Payload) {
			t.Fatal("no-op rewrote encrypted lease document")
		}
	}
	reenabled, err := p.Store.UpdateAccountVersion(a.ID, after.Version, func(a *Account) error { a.Enabled = true; return nil })
	if err != nil {
		t.Fatal(err)
	}
	p.processManagedAccountLease(ctx, a.ID, time.Now())
	again, _ := p.Store.Get(a.ID)
	owned, _ := p.getManagedLease(ctx, a.ID)
	if again.Enabled || again.Version != reenabled.Version+1 || owned.DisabledVersion != again.Version {
		t.Fatal("real expiry change was skipped")
	}
}
