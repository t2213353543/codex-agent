package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A missing primary window must not count as recovery of the primary window
// that triggered the preceding reset merely because another window is low.
func TestManagedAutoResetUnknownTriggerWindowDoesNotRearm(t *testing.T) {
	p, handler := webTestSetup(t)
	a := managedTestAccount(t, p, "auto-reset-partial-window", nil)
	var stage, consumed atomic.Int32
	expires := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/backend-api/wham/usage":
			if stage.Load() == 1 {
				fmt.Fprint(w, `{"plan_type":"pro","rate_limit":{"primary_window":null,"secondary_window":{"used_percent":10,"reset_after_seconds":300}}}`)
			} else if stage.Load() == 3 {
				fmt.Fprint(w, `{"plan_type":"pro","rate_limit":{"primary_window":{"used_percent":10,"reset_after_seconds":300},"secondary_window":null}}`)
			} else {
				fmt.Fprint(w, `{"plan_type":"pro","rate_limit":{"primary_window":{"used_percent":98,"reset_after_seconds":300},"secondary_window":{"used_percent":10,"reset_after_seconds":300}}}`)
			}
		case "/backend-api/wham/rate-limit-reset-credits":
			id := "credit-first"
			if stage.Load() > 0 {
				id = "credit-second"
			}
			fmt.Fprintf(w, `{"available_count":1,"credits":[{"id":%q,"status":"available","reset_type":"codex_rate_limits","expires_at":%q}]}`, id, expires)
		case "/backend-api/wham/rate-limit-reset-credits/consume":
			consumed.Add(1)
			fmt.Fprint(w, `{"code":"reset","windows_reset":1}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer upstream.Close()
	p.BaseURL = upstream.URL + "/backend-api/codex"
	managedTestRequest(t, handler, "PUT", "/api/accounts/"+a.ID+"/auto-reset", map[string]any{"version": 0, "account_version": a.Version, "enabled": true, "threshold_percent": 95, "interval_minutes": 5, "confirm_auto_consume": true}, 200)
	run := func() {
		err := p.managementState().Update(context.Background(), "account-auto-reset", a.ID, func(entry *managementStateEntry) error {
			policy, err := decodeManagedAutoReset(*entry)
			if err != nil {
				return err
			}
			policy.NextRunAt = 0
			entry.Payload, err = json.Marshal(policy)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		p.processManagedAutoResets(context.Background(), time.Now())
	}
	run()
	if consumed.Load() != 1 {
		t.Fatalf("initial reset count = %d", consumed.Load())
	}
	stage.Store(1)
	run()
	stage.Store(2)
	run()
	if consumed.Load() != 1 {
		t.Fatalf("unknown trigger window incorrectly rearmed the recovery cycle: consumed %d credits without observing primary-window recovery", consumed.Load())
	}
	stage.Store(3)
	run()
	if consumed.Load() != 1 {
		t.Fatalf("recovered primary window should not consume a credit: count = %d", consumed.Load())
	}
	stage.Store(4)
	run()
	if consumed.Load() != 2 {
		t.Fatalf("known primary-window recovery did not permit the next recovery cycle: count = %d", consumed.Load())
	}
}

func TestManagedEmptyUsageSnapshotStaysEmpty(t *testing.T) {
	exerciseManagedEmptyUsageSnapshot(t, webTestSetup)
}

func TestPGManagedEmptyUsageSnapshotStaysEmpty(t *testing.T) {
	exerciseManagedEmptyUsageSnapshot(t, adminPGTestProxy)
}

// Zero is a valid captured file boundary and must not mean "read live data".
// The same requirement applies to a PostgreSQL snapshot with no target IDs.
func exerciseManagedEmptyUsageSnapshot(t *testing.T, setup func(*testing.T) (*Proxy, http.Handler)) {
	p, handler := setup(t)
	filters := map[string]string{"from": "1700000000", "to": "1700000002"}
	response := managedTestRequest(t, handler, "POST", "/api/logs/snapshot", map[string]any{"filters": filters}, 200)
	var snapshot managedUsageSnapshot
	if err := json.Unmarshal(response.Body.Bytes(), &snapshot); err != nil || snapshot.ID == "" || snapshot.Total != 0 {
		t.Fatal("capture empty snapshot", err, response.Body.String())
	}
	p.Metrics.Record(RequestRecord{EventID: "after-empty-snapshot", Time: 1700000001, Model: "snapshot-late-model", Status: "ok", InputTokens: 17})
	values := url.Values{"from": {filters["from"]}, "to": {filters["to"]}, "as_of": {snapshot.ID}, "exact_total": {"true"}, "sort_by": {"tokens"}, "sort_order": {"asc"}, "timezone": {"Asia/Shanghai"}, "step": {"60"}}
	response = managedTestRequest(t, handler, "GET", "/api/logs?"+values.Encode(), nil, 200)
	var page managedLogPage
	if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil || page.Total != 0 || len(page.Items) != 0 || page.HasMore {
		t.Fatal("empty snapshot admitted later row", err, response.Body.String())
	}
	response = managedTestRequest(t, handler, "GET", "/api/logs/stats?"+values.Encode(), nil, 200)
	var stats managedUsageStats
	if err := json.Unmarshal(response.Body.Bytes(), &stats); err != nil || stats.Requests != 0 || stats.Input != 0 {
		t.Fatal("empty snapshot stats admitted later row", err, response.Body.String())
	}
	response = managedTestRequest(t, handler, "GET", "/api/logs/options?field=model&"+values.Encode(), nil, 200)
	var options struct {
		Items []managedUsageOption `json:"items"`
		More  bool                 `json:"has_more"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &options); err != nil || len(options.Items) != 0 || options.More {
		t.Fatal("empty snapshot options admitted later row", err, response.Body.String())
	}
	response = managedTestRequest(t, handler, "GET", "/api/logs/export?format=jsonl&"+values.Encode(), nil, 200)
	if response.Header().Get("X-Export-Count") != "0" || strings.TrimSpace(response.Body.String()) != "" {
		t.Fatal("empty snapshot export admitted later row", response.Body.String())
	}
	values.Del("as_of")
	response = managedTestRequest(t, handler, "GET", "/api/logs?"+values.Encode(), nil, 200)
	if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil || page.Total != 1 || len(page.Items) != 1 {
		t.Fatal("live query should expose the later row", err, response.Body.String())
	}
}
