package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestManagedAccountControls(t *testing.T)   { runManagedAccountControls(t, webTestSetup) }
func TestPGManagedAccountControls(t *testing.T) { runManagedAccountControls(t, adminPGTestProxy) }
func runManagedAccountControls(t *testing.T, setup func(*testing.T) (*Proxy, http.Handler)) {
	p, h := setup(t)
	a := managedTestAccount(t, p, "controlled-account", nil)
	var resets atomic.Int32
	var privacy atomic.Int32
	var invalid atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+a.AccessToken {
			t.Error("control request missing credentials")
		}
		switch r.URL.Path {
		case "/backend-api/settings/user":
			privacy.Add(1)
			if r.Method != "PATCH" || r.URL.Query().Get("feature") != "training_allowed" || r.URL.Query().Get("value") != "false" {
				t.Error("privacy did not match reference contract")
			}
			w.WriteHeader(204)
		case "/backend-api/wham/rate-limit-reset-credits":
			fmt.Fprint(w, `{"available_count":1,"credits":[{"id":"credit-1","reset_type":"codex_rate_limits","status":"available","granted_at":"2026-09-18T00:00:00Z","expires_at":"2099-09-18T00:00:00Z"}]}`)
		case "/backend-api/wham/rate-limit-reset-credits/consume":
			resets.Add(1)
			var body map[string]string
			if json.NewDecoder(r.Body).Decode(&body) != nil || body["credit_id"] != "credit-1" || body["redeem_request_id"] == "" {
				t.Error("missing explicit targeted credit/idempotency key")
			}
			if invalid.Load() {
				fmt.Fprint(w, `{"private":"upstream-secret"}`)
			} else {
				fmt.Fprint(w, `{"code":"reset","windows_reset":2}`)
			}
		default:
			t.Errorf("unexpected control path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	p.BaseURL = server.URL + "/backend-api/codex"
	privacyBody := map[string]any{"version": a.Version, "disable_training": true}
	managedTestRequest(t, h, "POST", "/api/accounts/"+a.ID+"/privacy", privacyBody, 400)
	if privacy.Load() != 0 {
		t.Fatal("unconfirmed privacy change reached upstream")
	}
	privacyBody["confirm"] = true
	r := managedTestRequest(t, h, "POST", "/api/accounts/"+a.ID+"/privacy", privacyBody, 200)
	if !strings.Contains(r.Body.String(), "upstream_acknowledged") || privacy.Load() != 1 {
		t.Fatal("privacy change not acknowledged")
	}
	managedTestRequest(t, h, "POST", "/api/accounts/"+a.ID+"/upstream", map[string]any{"version": a.Version, "kind": "reset_credits"}, 200)
	resetBody := map[string]any{"version": a.Version, "credit_id": "credit-1", "request_id": "redeem-1"}
	managedTestRequest(t, h, "POST", "/api/accounts/"+a.ID+"/reset-credit", resetBody, 400)
	if resets.Load() != 0 {
		t.Fatal("unconfirmed credit consumption reached upstream")
	}
	resetBody["confirm"] = true
	r = managedTestRequest(t, h, "POST", "/api/accounts/"+a.ID+"/reset-credit", resetBody, 200)
	if !strings.Contains(r.Body.String(), `"code":"reset"`) {
		t.Fatal("successful reset result missing")
	}
	managedTestRequest(t, h, "POST", "/api/accounts/"+a.ID+"/reset-credit", resetBody, 200)
	if resets.Load() != 1 {
		t.Fatal("idempotency replay consumed another credit")
	}
	resetBody["credit_id"] = "different-credit"
	managedTestRequest(t, h, "POST", "/api/accounts/"+a.ID+"/reset-credit", resetBody, 409)
	if resets.Load() != 1 {
		t.Fatal("same idempotency key accepted different credit")
	}
	current, _ := p.Store.Get(a.ID)
	if current.Version != a.Version || current.Quota != a.Quota || current.WindowStart != a.WindowStart {
		t.Fatal("upstream reset changed local quota/scheduling")
	}
	r = managedTestRequest(t, h, "GET", "/api/accounts/"+a.ID+"/upstream", nil, 200)
	var status struct {
		Snapshots map[string]managedUpstreamSnapshot
	}
	_ = json.Unmarshal(r.Body.Bytes(), &status)
	if !status.Snapshots["reset_credits"].Stale {
		t.Fatal("pre-reset credit snapshot remained fresh")
	}
	invalid.Store(true)
	resetBody["credit_id"] = "credit-1"
	resetBody["request_id"] = "redeem-unknown"
	r = managedTestRequest(t, h, "POST", "/api/accounts/"+a.ID+"/reset-credit", resetBody, 200)
	if !strings.Contains(r.Body.String(), `"outcome_unknown":true`) || strings.Contains(r.Body.String(), "upstream-secret") {
		t.Fatal("ambiguous consumption outcome was fabricated or leaked")
	}
	managedTestRequest(t, h, "POST", "/api/accounts/"+a.ID+"/reset-credit", resetBody, 200)
	if resets.Load() != 2 {
		t.Fatal("unknown consumption outcome automatically retried")
	}
}
