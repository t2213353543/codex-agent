package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestManagedCron(t *testing.T) {
	for _, row := range []struct{ expression, zone, after, want string }{
		{"*/15 9-17 * * MON-FRI", "UTC", "2026-09-18T09:07:00Z", "2026-09-18T09:15:00Z"},
		{"5,35 8 * JAN,SEP *", "Asia/Shanghai", "2026-09-18T00:06:00Z", "2026-09-18T00:35:00Z"},
		{"0 0 13 * MON", "UTC", "2026-09-12T12:00:00Z", "2026-09-13T00:00:00Z"},
		{"0 0 * * 7", "UTC", "2026-09-12T12:00:00Z", "2026-09-13T00:00:00Z"},
		{"30 1 * * *", "America/New_York", "2026-11-01T05:31:00Z", "2026-11-01T06:30:00Z"},
	} {
		cron, err := parseManagedCron(row.expression, row.zone)
		if err != nil {
			t.Fatal(err)
		}
		after, _ := time.Parse(time.RFC3339, row.after)
		next, err := cron.next(after)
		if err != nil || time.Unix(next, 0).UTC().Format(time.RFC3339) != row.want {
			t.Errorf("%s got %d %v", row.expression, next, err)
		}
	}
	for _, expression := range []string{"* * * *", "60 * * * *", "*/0 * * * *", "1,,2 * * * *", "* * * JAN-FEB-MAR *", "* * * * FRI-MON"} {
		if _, err := parseManagedCron(expression, "UTC"); err == nil {
			t.Errorf("accepted %q", expression)
		}
	}
	if _, err := parseManagedCron("* * * * *", "Unknown/Timezone"); err == nil {
		t.Fatal("invalid timezone")
	}
}

func TestManagedSchedulesAndLeases(t *testing.T)   { runManagedSchedulesAndLeases(t, webTestSetup) }
func TestPGManagedSchedulesAndLeases(t *testing.T) { runManagedSchedulesAndLeases(t, adminPGTestProxy) }
func runManagedSchedulesAndLeases(t *testing.T, setup func(*testing.T) (*Proxy, http.Handler)) {
	p, h := setup(t)
	a := managedTestAccount(t, p, "scheduled-account", nil)
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		fmt.Fprint(w, `{"models":[{"slug":"fixture-model"}]}`)
	}))
	defer upstream.Close()
	p.BaseURL = upstream.URL + "/backend-api/codex"
	body := managedScheduleInput{Kind: "models", Cron: "* * * * *", Enabled: true, MaxResults: 2}
	created := managedTestRequest(t, h, "POST", "/api/accounts/"+a.ID+"/schedules", body, 200)
	var plan managedAccountSchedule
	if err := json.Unmarshal(created.Body.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	if plan.Timezone != "UTC" || plan.Version != 1 {
		t.Fatal("schedule defaults")
	}
	managedTestRequest(t, h, "GET", "/api/accounts/"+a.ID+"/schedules", nil, 200)
	setDue := func(at int64) {
		t.Helper()
		if err := p.managementState().Update(context.Background(), "account-schedules", a.ID, func(entry *managementStateEntry) error {
			doc, err := decodeManagedSchedules(entry)
			if err != nil {
				return err
			}
			doc.Plans[plan.ID].Plan.NextRunAt = at
			entry.Payload, err = json.Marshal(doc)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().Truncate(time.Minute).Add(time.Second)
	setDue(now.Unix() - 1)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); p.processManagedAccountSchedulesAt(context.Background(), now) }()
	}
	wg.Wait()
	if requests.Load() != 1 {
		t.Fatalf("concurrent workers dispatched %d requests", requests.Load())
	}
	// Restart-like repeated work sees durable next-run/result, never replays.
	p.processManagedAccountSchedulesAt(context.Background(), now)
	if requests.Load() != 1 {
		t.Fatal("completed run replayed")
	}
	r := managedTestRequest(t, h, "GET", "/api/account-schedules/"+plan.ID+"/results", nil, 200)
	var results struct{ Results []managedScheduleRun }
	_ = json.Unmarshal(r.Body.Bytes(), &results)
	if len(results.Results) != 1 || results.Results[0].Status != "ok" {
		t.Fatalf("missing persisted result: %s", r.Body.String())
	}
	// A crashed claim is finalized unknown, without replaying its old turn.
	if err := p.managementState().Update(context.Background(), "account-schedules", a.ID, func(entry *managementStateEntry) error {
		doc, err := decodeManagedSchedules(entry)
		if err != nil {
			return err
		}
		row := doc.Plans[plan.ID]
		row.Active = &managedScheduleActive{Run: managedScheduleRun{ID: "crashed", Status: "running"}, LeaseUntil: now.Unix() - 1}
		row.Plan.Running = true
		entry.Payload, err = json.Marshal(doc)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	p.processManagedAccountSchedulesAt(context.Background(), now)
	if requests.Load() != 1 {
		t.Fatal("expired execution lease replayed")
	}
	r = managedTestRequest(t, h, "GET", "/api/account-schedules/"+plan.ID+"/results", nil, 200)
	_ = json.Unmarshal(r.Body.Bytes(), &results)
	if len(results.Results) != 2 || results.Results[0].Status != "unknown" {
		t.Fatal("crash was reported as known success")
	}
	body.Version = plan.Version
	body.Enabled = false
	managedTestRequest(t, h, "PUT", "/api/account-schedules/"+plan.ID, body, 200)
	managedTestRequest(t, h, "PUT", "/api/account-schedules/"+plan.ID, body, 409)
	setDue(now.Unix() - 1)
	p.processManagedAccountSchedulesAt(context.Background(), now)
	if requests.Load() != 1 {
		t.Fatal("disabled schedule ran")
	}
	managedTestRequest(t, h, "DELETE", "/api/account-schedules/"+plan.ID+"?version=2", nil, 200)
	p.processManagedAccountSchedulesAt(context.Background(), now)
	managedTestRequest(t, h, "GET", "/api/account-schedules/"+plan.ID+"/results", nil, 404)
	body.Kind = "stream"
	body.Model = "fixture-model"
	body.Enabled = true
	body.Version = 0
	managedTestRequest(t, h, "POST", "/api/accounts/"+a.ID+"/schedules", body, 400)
	if requests.Load() != 1 {
		t.Fatal("unconfirmed inference dispatched")
	}

	leaseURL := "/api/accounts/" + a.ID + "/lease"
	leaseInput := map[string]any{"version": 0, "account_version": a.Version, "expires_at": time.Now().Unix() - 60, "auto_disable": true, "restore_on_extend": true}
	r = managedTestRequest(t, h, "PUT", leaseURL, leaseInput, 200)
	var leaseResult struct {
		AccountVersion int64               `json:"account_version"`
		Lease          managedAccountLease `json:"lease"`
	}
	_ = json.Unmarshal(r.Body.Bytes(), &leaseResult)
	current, _ := p.Store.Get(a.ID)
	if current.Enabled || !leaseResult.Lease.DisabledByLease || current.ExpiresAt != a.ExpiresAt {
		t.Fatal("lease did not separately disable expired rental")
	}
	leaseInput["version"] = leaseResult.Lease.Version
	leaseInput["account_version"] = current.Version
	leaseInput["expires_at"] = time.Now().Unix() + 3600
	r = managedTestRequest(t, h, "PUT", leaseURL, leaseInput, 200)
	_ = json.Unmarshal(r.Body.Bytes(), &leaseResult)
	current, _ = p.Store.Get(a.ID)
	if !current.Enabled || leaseResult.Lease.DisabledByLease {
		t.Fatal("source-matched extension did not restore")
	}
	leaseInput["version"] = leaseResult.Lease.Version
	leaseInput["account_version"] = current.Version
	leaseInput["expires_at"] = time.Now().Unix() - 60
	r = managedTestRequest(t, h, "PUT", leaseURL, leaseInput, 200)
	_ = json.Unmarshal(r.Body.Bytes(), &leaseResult)
	current, _ = p.Store.Get(a.ID)
	current, err := p.Store.UpdateAccountVersion(a.ID, current.Version, func(a *Account) error { a.Notes = "manual change"; return nil })
	if err != nil {
		t.Fatal(err)
	}
	leaseInput["version"] = leaseResult.Lease.Version
	leaseInput["account_version"] = current.Version
	leaseInput["expires_at"] = time.Now().Unix() + 3600
	r = managedTestRequest(t, h, "PUT", leaseURL, leaseInput, 200)
	_ = json.Unmarshal(r.Body.Bytes(), &leaseResult)
	current, _ = p.Store.Get(a.ID)
	if current.Enabled || leaseResult.Lease.RestoreSkippedReason != "account_changed" {
		t.Fatal("lease overwrote newer manual management")
	}
	managedTestRequest(t, h, "PUT", leaseURL, leaseInput, 409)
	if requests.Load() != 1 {
		t.Fatal("lease changed inference traffic")
	}
}
