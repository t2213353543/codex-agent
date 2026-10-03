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

func TestManagedAutoReset(t *testing.T)   { exerciseManagedAutoReset(t, webTestSetup) }
func TestPGManagedAutoReset(t *testing.T) { exerciseManagedAutoReset(t, adminPGTestProxy) }
func exerciseManagedAutoReset(t *testing.T, setup func(*testing.T) (*Proxy, http.Handler)) {
	p, h := setup(t)
	a := managedTestAccount(t, p, "auto-reset-fixture", nil)
	var consumed, checked atomic.Int32
	var unknown atomic.Bool
	expires := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/backend-api/wham/usage":
			checked.Add(1)
			fmt.Fprint(w, `{"plan_type":"pro","rate_limit":{"primary_window":{"used_percent":98,"reset_after_seconds":300}}}`)
		case "/backend-api/wham/rate-limit-reset-credits":
			fmt.Fprintf(w, `{"available_count":1,"credits":[{"id":"auto-credit","status":"available","reset_type":"codex_rate_limits","expires_at":%q}]}`, expires)
		case "/backend-api/wham/rate-limit-reset-credits/consume":
			consumed.Add(1)
			if unknown.Load() {
				fmt.Fprint(w, `{"unrecognized":true}`)
			} else {
				fmt.Fprint(w, `{"code":"reset","windows_reset":2}`)
			}
		default:
			t.Errorf("unexpected operation %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	p.BaseURL = server.URL + "/backend-api/codex"
	path := "/api/accounts/" + a.ID + "/auto-reset"
	managedTestRequest(t, h, "GET", path, nil, 200)
	if checked.Load() != 0 {
		t.Fatal("GET performed upstream operation")
	}
	input := map[string]any{"version": 0, "account_version": a.Version, "enabled": true, "threshold_percent": 95, "interval_minutes": 5}
	managedTestRequest(t, h, "PUT", path, input, 400)
	input["confirm_auto_consume"] = true
	managedTestRequest(t, h, "PUT", path, input, 200)
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); p.processManagedAutoResets(context.Background(), time.Now()) }()
	}
	wg.Wait()
	read := func() managedAutoResetPolicy {
		entry, e := p.managementState().Get(context.Background(), "account-auto-reset", a.ID)
		if e != nil {
			t.Fatal(e)
		}
		row, e := decodeManagedAutoReset(entry)
		if e != nil {
			t.Fatal(e)
		}
		return row
	}
	policy := read()
	if consumed.Load() != 1 || policy.Phase != "succeeded" || policy.LastResetAt == 0 {
		t.Fatalf("concurrent policy did not consume once: %d %+v", consumed.Load(), policy)
	}
	modify := func(change func(*managedAutoResetPolicy)) {
		err := p.managementState().Update(context.Background(), "account-auto-reset", a.ID, func(entry *managementStateEntry) error {
			row, e := decodeManagedAutoReset(*entry)
			if e != nil {
				return e
			}
			change(&row)
			entry.Payload, e = json.Marshal(row)
			return e
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	modify(func(row *managedAutoResetPolicy) { row.NextRunAt = 0 })
	p.processManagedAutoResets(context.Background(), time.Now())
	if consumed.Load() != 1 || read().Phase != "no_credit" {
		t.Fatal("stale upstream credit was consumed again")
	}
	// An interrupted write lease is terminal unknown until explicit acknowledgement.
	modify(func(row *managedAutoResetPolicy) {
		row.Phase = "resetting"
		row.LeaseUntil = time.Now().Add(-time.Minute).Unix()
	})
	p.processManagedAutoResets(context.Background(), time.Now())
	policy = read()
	if policy.Phase != "unknown" || policy.Enabled || consumed.Load() != 1 {
		t.Fatal("crash recovery repeated write")
	}
	input["version"] = policy.Version
	managedTestRequest(t, h, "PUT", path, input, 409)
	input["enabled"] = false
	managedTestRequest(t, h, "PUT", path, input, 200)
	policy = read()
	if policy.Phase != "unknown" {
		t.Fatal("disable erased unknown outcome")
	}
	input["version"] = policy.Version
	input["enabled"] = true
	managedTestRequest(t, h, "PUT", path, input, 409)
	input["acknowledge_unknown"] = true
	managedTestRequest(t, h, "PUT", path, input, 200)
	modify(func(row *managedAutoResetPolicy) { row.CreditID = ""; row.NextRunAt = 0 })
	unknown.Store(true)
	p.processManagedAutoResets(context.Background(), time.Now())
	if read().Phase != "unknown" || read().Enabled || consumed.Load() != 2 {
		t.Fatal("ambiguous upstream result fabricated success")
	}
	current, _ := p.Store.Get(a.ID)
	if current.Version != a.Version || current.Enabled != a.Enabled || current.Quota != a.Quota {
		t.Fatal("automatic reset changed gateway account state")
	}
}

func TestManagedAutoResetUnknownAndExpiredWindows(t *testing.T) {
	percent := 100.0
	expired := time.Now().Add(-time.Minute).Unix()
	usage := managedUsage{RateLimit: &managedRateLimit{PrimaryWindow: &managedRateWindow{UsedPercent: &percent, ResetAt: &expired}}}
	if reached, known := managedAutoResetThreshold(usage, 95, time.Now()); reached || known {
		t.Fatal("expired window triggered consumption")
	}
	count := int64(3)
	past := time.Now().Add(-time.Hour).Format(time.RFC3339)
	soon := time.Now().Add(time.Hour).Format(time.RFC3339)
	later := time.Now().Add(2 * time.Hour).Format(time.RFC3339)
	credits := managedResetCredits{AvailableCount: &count, Credits: []managedResetCredit{{ID: "expired", ResetType: "codex_rate_limits", Status: "available", ExpiresAt: &past}, {ID: "later", ResetType: "codex_rate_limits", Status: "available", ExpiresAt: &later}, {ID: "sooner", ResetType: "codex_rate_limits", Status: "available", ExpiresAt: &soon}}}
	if id := managedAutoResetCredit(credits, time.Now()); id != "sooner" {
		t.Fatal("did not prefer earliest actual expiry", id)
	}
}
