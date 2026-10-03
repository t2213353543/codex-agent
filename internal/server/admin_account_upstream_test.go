package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestManagedUpstreamChecks(t *testing.T)   { runManagedUpstreamChecks(t, webTestSetup) }
func TestPGManagedUpstreamChecks(t *testing.T) { runManagedUpstreamChecks(t, adminPGTestProxy) }
func runManagedUpstreamChecks(t *testing.T, setup func(*testing.T) (*Proxy, http.Handler)) {
	p, h := setup(t)
	account := managedTestAccount(t, p, "upstream-managed", nil)
	var calls atomic.Int32
	var failMode atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+account.AccessToken || r.Header.Get("ChatGPT-Account-ID") != account.AccountID {
			t.Error("management request missing target credentials")
		}
		if failMode.Load() {
			w.WriteHeader(429)
			fmt.Fprint(w, `{"error":"secret-access-upstream-managed"}`)
			return
		}
		switch r.URL.Path {
		case "/backend-api/codex/models":
			if r.Method != "GET" || r.URL.Query().Get("client_version") != clientVersion {
				t.Error("incorrect model discovery contract")
			}
			fmt.Fprint(w, `{"models":[{"slug":"exact-model","display_name":"Exact model","context_window":100000,"private_token":"upstream-secret"}]}`)
		case "/backend-api/wham/usage":
			if r.Method != "GET" {
				t.Error("usage check inferred instead of reading")
			}
			fmt.Fprint(w, `{"plan_type":"pro","rate_limit":{"allowed":false,"limit_reached":true,"primary_window":{"used_percent":100,"limit_window_seconds":18000,"reset_at":2100000000}},"credits":null,"account_id":"private-upstream-id"}`)
		case "/backend-api/codex/responses", "/backend-api/codex/responses/compact":
			var body map[string]any
			if json.NewDecoder(r.Body).Decode(&body) != nil || body["model"] != "exact-model" {
				t.Error("test model was changed")
			}
			if body["max_output_tokens"] != nil || body["temperature"] != nil {
				t.Error("test contains unsupported Codex controls")
			}
			if r.URL.Path == "/backend-api/codex/responses" {
				if body["stream"] != true || body["store"] != false {
					t.Error("test did not use native stream contract")
				}
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"OK\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":10,\"output_tokens\":1,\"total_tokens\":11}}}\n\n")
			} else {
				fmt.Fprint(w, `{"output":[{"type":"compaction","encrypted_content":"private-synthetic-compaction"}]}`)
			}
		default:
			t.Errorf("unexpected upstream path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer upstream.Close()
	p.BaseURL = upstream.URL + "/backend-api/codex"
	managedTestRequest(t, h, "GET", "/api/accounts/"+account.ID+"/upstream", nil, 200)
	if calls.Load() != 0 {
		t.Fatal("GET snapshot called the upstream")
	}
	for _, kind := range []string{"models", "usage", "stream", "compact"} {
		r := managedTestRequest(t, h, "POST", "/api/accounts/"+account.ID+"/upstream", map[string]any{"version": account.Version, "kind": kind, "model": "exact-model", "confirm_inference": true}, 200)
		var result managedUpstreamSnapshot
		if json.Unmarshal(r.Body.Bytes(), &result) != nil || !result.OK || result.Kind != kind || result.HTTPCode != 200 {
			t.Fatalf("%s failed: %s", kind, r.Body.String())
		}
		for _, private := range []string{account.AccessToken, account.AccountID, "upstream-secret", "private-upstream-id", "private-synthetic-compaction"} {
			if strings.Contains(r.Body.String(), private) {
				t.Fatalf("check exposed private field %s", private)
			}
		}
	}
	if calls.Load() != 4 {
		t.Fatal("unexpected hidden upstream requests")
	}
	current, _ := p.Store.Get(account.ID)
	if current.Version != account.Version || current.CredentialStatus != account.CredentialStatus {
		t.Fatal("management check mutated account scheduling state")
	}
	if metrics, err := p.Metrics.BatchAccountMetrics(context.Background(), map[string]int64{account.ID: 1, account.Name: 1}); err != nil {
		t.Fatal(err)
	} else {
		for _, m := range metrics {
			if m.Usage.Requests != 0 || m.HasLast {
				t.Fatal("management test polluted business usage")
			}
		}
	}
	// A new Proxy reading the same storage sees durable snapshots.
	r := managedTestRequest(t, h, "GET", "/api/accounts/"+account.ID+"/upstream", nil, 200)
	var cached struct {
		Snapshots  map[string]managedUpstreamSnapshot
		Persistent bool
	}
	if json.Unmarshal(r.Body.Bytes(), &cached) != nil || !cached.Persistent || len(cached.Snapshots) != 4 {
		t.Fatal("missing persistent check snapshots")
	}
	for _, s := range cached.Snapshots {
		if s.Stale {
			t.Fatal("fresh snapshot was already stale")
		}
	}
	for _, body := range []any{
		map[string]any{"version": account.Version, "kind": "stream", "model": "exact-model"},
		map[string]any{"version": account.Version, "kind": "compact", "confirm_inference": true},
		map[string]any{"version": account.Version, "kind": "invalid"},
	} {
		managedTestRequest(t, h, "POST", "/api/accounts/"+account.ID+"/upstream", body, 400)
	}
	managedTestRequest(t, h, "POST", "/api/accounts/"+account.ID+"/upstream", map[string]any{"version": account.Version + 1, "kind": "usage"}, 409)
	if calls.Load() != 4 {
		t.Fatal("invalid or unconfirmed test reached upstream")
	}
	failMode.Store(true)
	r = managedTestRequest(t, h, "POST", "/api/accounts/"+account.ID+"/upstream", map[string]any{"version": account.Version, "kind": "usage"}, 200)
	var failed managedUpstreamSnapshot
	_ = json.Unmarshal(r.Body.Bytes(), &failed)
	if failed.OK || failed.HTTPCode != 429 || failed.Error == nil || strings.Contains(r.Body.String(), account.AccessToken) {
		t.Fatal("upstream failure incorrectly reported or not redacted")
	}
	if calls.Load() != 5 {
		t.Fatal("upstream failure automatically retried")
	}
	credentials := Account{AccessToken: "replacement-upstream-token", AccountID: account.AccountID}
	if _, err := p.Store.reauthorizeManagedAccount(context.Background(), account.ID, account.Version, credentials, false); err != nil {
		t.Fatal(err)
	}
	r = managedTestRequest(t, h, "GET", "/api/accounts/"+account.ID+"/upstream", nil, 200)
	_ = json.Unmarshal(r.Body.Bytes(), &cached)
	for _, s := range cached.Snapshots {
		if !s.Stale {
			t.Fatal("old credential snapshot shown as current")
		}
	}
}

func TestManagedUpstreamStreamFailureAndRedirect(t *testing.T) {
	for _, s := range []string{
		"data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\"}}\n\n",
		"data: {\"type\":\"response.incomplete\"}\n\n",
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\ndata: [DONE]\n\n",
		"data: {\"type\":\"response.completed\",\"response\":{\"status\":\"failed\"}}\n\n",
		"data: malformed\n\n",
	} {
		if _, err := readManagedTestStream(strings.NewReader(s)); err == nil {
			t.Fatal("failed or truncated SSE passed")
		}
	}
	if _, err := parseManagedCompact([]byte(`{"output":[]}`)); err == nil {
		t.Fatal("empty successful HTTP compact response passed")
	}
	p, _ := webTestSetup(t)
	a := managedTestAccount(t, p, "redirect-managed", nil)
	var destinationCalls atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { destinationCalls.Add(1) }))
	defer destination.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, destination.URL, 302) }))
	defer redirect.Close()
	p.BaseURL = redirect.URL
	result := p.checkManagedUpstream(context.Background(), a, "models", "")
	if result.OK || result.HTTPCode != 302 || destinationCalls.Load() != 0 {
		t.Fatal("management followed credential-bearing redirect")
	}
}
