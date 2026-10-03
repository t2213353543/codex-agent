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

func TestManagedCRSSync(t *testing.T)   { runManagedCRSSync(t, webTestSetup) }
func TestPGManagedCRSSync(t *testing.T) { runManagedCRSSync(t, adminPGTestProxy) }
func runManagedCRSSync(t *testing.T, setup func(*testing.T) (*Proxy, http.Handler)) {
	p, h := setup(t)
	var generation atomic.Int32
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		switch r.URL.Path {
		case "/web/auth/login":
			if r.Method != "POST" {
				t.Error("login method")
			}
			fmt.Fprint(w, `{"success":true,"token":"synthetic-crs-admin-secret"}`)
		case "/admin/sync/export-accounts":
			if r.Header.Get("Authorization") != "Bearer synthetic-crs-admin-secret" || r.URL.Query().Get("include_secrets") != "true" {
				t.Error("export credentials missing")
			}
			fmt.Fprintf(w, `{"success":true,"data":{"openaiOAuthAccounts":[{"id":"source-1","name":"synced-codex","authType":"oauth","isActive":false,"priority":4,"description":"original notes","credentials":{"access_token":"synthetic-access-%d","refresh_token":"synthetic-refresh-secret","account_id":"crs-identity"},"proxy":{"protocol":"http","host":"127.0.0.1","port":9999,"username":"synthetic-proxy-user","password":"synthetic-proxy-secret"}},{"id":"source-invalid","name":"invalid","authType":"oauth","credentials":{}},{"id":"not-oauth","name":"other","authType":"api-key","credentials":{"access_token":"ignored"}}]}}`, generation.Load())
		default:
			t.Error("unexpected CRS network path")
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	previewBody := map[string]any{"base_url": server.URL, "username": "synthetic-admin", "password": "synthetic-password", "allow_insecure_http": true}
	preview := func() string {
		t.Helper()
		r := managedTestRequest(t, h, "POST", "/api/accounts/sync/crs/preview", previewBody, 200)
		for _, secret := range []string{"synthetic-access", "synthetic-refresh", "synthetic-crs-admin", "synthetic-proxy-secret", "synthetic-password"} {
			if strings.Contains(r.Body.String(), secret) {
				t.Fatal("preview exposed credentials")
			}
		}
		var value struct {
			PreviewID string          `json:"preview_id"`
			Accounts  []managedCRSRow `json:"accounts"`
			Total     int             `json:"total"`
		}
		if json.Unmarshal(r.Body.Bytes(), &value) != nil || value.Total != 2 || len(value.Accounts) != 2 {
			t.Fatalf("incorrect scope: %s", r.Body.String())
		}
		return value.PreviewID
	}
	_, before, _ := p.Store.ListMetadata()
	id := preview()
	_, after, _ := p.Store.ListMetadata()
	if len(before) != len(after) {
		t.Fatal("preview wrote accounts")
	}
	body := map[string]any{"preview_id": id, "selected_account_ids": []string{"source-1", "source-invalid"}, "sync_proxies": true}
	r := managedTestRequest(t, h, "POST", "/api/accounts/sync/crs", body, 200)
	var result struct {
		Results   []managedCRSResult `json:"results"`
		Succeeded int                `json:"succeeded"`
		Failed    int                `json:"failed"`
	}
	_ = json.Unmarshal(r.Body.Bytes(), &result)
	if result.Succeeded != 1 || result.Failed != 1 || result.Results[0].Status != "added" {
		t.Fatalf("sync result: %s", r.Body.String())
	}
	a, err := p.Store.Get(result.Results[0].ID)
	if err != nil || a.Enabled || a.Priority != 4 || len(a.Proxies) != 1 || a.Notes != "original notes" {
		t.Fatal("CRS source metadata not preserved")
	}
	r = managedTestRequest(t, h, "POST", "/api/accounts/sync/crs", body, 200)
	if !strings.Contains(r.Body.String(), `"status":"added"`) {
		t.Fatal("same submit did not replay same outcome")
	}
	body["sync_proxies"] = false
	managedTestRequest(t, h, "POST", "/api/accounts/sync/crs", body, 409)
	generation.Store(1)
	second := preview()
	a, err = p.Store.UpdateAccountVersion(a.ID, a.Version, func(a *Account) error { a.Notes = "manual notes"; return nil })
	if err != nil {
		t.Fatal(err)
	}
	body = map[string]any{"preview_id": second, "selected_account_ids": []string{"source-1"}}
	r = managedTestRequest(t, h, "POST", "/api/accounts/sync/crs", body, 200)
	if !strings.Contains(r.Body.String(), "version_conflict") {
		t.Fatal("stale preview overwrote account")
	}
	third := preview()
	body["preview_id"] = third
	r = managedTestRequest(t, h, "POST", "/api/accounts/sync/crs", body, 200)
	if !strings.Contains(r.Body.String(), `"status":"updated"`) {
		t.Fatalf("linked source not updated: %s", r.Body.String())
	}
	a, err = p.Store.Get(a.ID)
	if err != nil || a.AccessToken != "synthetic-access-1" || a.Notes != "manual notes" || a.Enabled || len(a.Proxies) != 1 {
		t.Fatal("resync lost managed fields or credentials")
	}
	if calls.Load() != 6 {
		t.Fatalf("sync performed implicit network/refresh: %d", calls.Load())
	}
}
