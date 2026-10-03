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

func TestManagedAccountETagBatchAndRotation(t *testing.T) {
	runManagedAccountETagBatchAndRotation(t, webTestSetup)
}
func TestPGManagedAccountETagBatchAndRotation(t *testing.T) {
	runManagedAccountETagBatchAndRotation(t, adminPGTestProxy)
}
func runManagedAccountETagBatchAndRotation(t *testing.T, setup func(*testing.T) (*Proxy, http.Handler)) {
	p, h := setup(t)
	a := managedTestAccount(t, p, "rotated-import", func(a *Account) { a.Notes = "retained"; a.Enabled = false })
	r := managedTestRequest(t, h, "GET", "/api/accounts/manage?search=rotated-import", nil, 200)
	etag := r.Header().Get("ETag")
	if etag == "" {
		t.Fatal("missing account ETag")
	}
	checkETag := func(want int) {
		t.Helper()
		request := httptest.NewRequest("GET", "/api/accounts/manage?search=rotated-import", nil)
		request.Header.Set("Authorization", "Bearer "+webTestKey)
		request.Header.Set("If-None-Match", etag)
		result := httptest.NewRecorder()
		h.ServeHTTP(result, request)
		if result.Code != want {
			t.Fatalf("etag returned %d expected %d", result.Code, want)
		}
		if want == 304 && result.Body.Len() != 0 {
			t.Fatal("304 contained body")
		}
	}
	checkETag(304)
	raw, _ := json.Marshal(map[string]any{"access_token": a.AccessToken, "refresh_token": "new-rotated-refresh", "client_id": "original-mobile-client", "account_id": a.AccountID})
	item := ImportItem{Name: "alternate-import-name", Credentials: raw}
	preview, err := p.Store.ImportDetailed([]ImportItem{item}, AccountDefaults{}, ImportOptions{DryRun: true})
	if err != nil || preview.Replaced != 1 {
		t.Fatal("rotated token preview not recognized")
	}
	current, _ := p.Store.Get(a.ID)
	if current.RefreshToken != a.RefreshToken {
		t.Fatal("dry run rotated credentials")
	}
	report, err := p.Store.ImportDetailed([]ImportItem{item}, AccountDefaults{}, ImportOptions{})
	if err != nil || report.Replaced != 1 {
		t.Fatal("same access token lost new refresh token")
	}
	current, _ = p.Store.Get(a.ID)
	if current.RefreshToken != "new-rotated-refresh" || current.ClientID != "original-mobile-client" || current.Name != a.Name || current.Notes != "retained" || current.Enabled || current.Version <= a.Version {
		t.Fatal("rotation import lost credentials or metadata")
	}
	checkETag(200)
	report, err = p.Store.ImportDetailed([]ImportItem{item}, AccountDefaults{}, ImportOptions{})
	if err != nil || report.Duplicate != 1 {
		t.Fatal("same rotation was not idempotent")
	}
	bad, _ := json.Marshal(map[string]any{"access_token": a.AccessToken, "refresh_token": "wrong-identity-refresh", "account_id": "wrong-identity"})
	report, err = p.Store.ImportDetailed([]ImportItem{{Name: a.Name, Credentials: bad}}, AccountDefaults{}, ImportOptions{})
	if err != nil || report.Failed != 1 {
		t.Fatal("same access token accepted conflicting identity")
	}
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		fmt.Fprint(w, `{"models":[{"slug":"synthetic-model"}]}`)
	}))
	defer server.Close()
	p.BaseURL = server.URL + "/backend-api/codex"
	body := map[string]any{"kind": "models", "items": []managedAccountItem{{ID: current.ID, Version: current.Version}, {ID: a.ID + "-missing", Version: 1}}}
	r = managedTestRequest(t, h, "POST", "/api/accounts/upstream/batch", body, 200)
	if !strings.Contains(r.Body.String(), `"succeeded":1`) || !strings.Contains(r.Body.String(), `"code":"not_found"`) || calls.Load() != 1 {
		t.Fatalf("batch per-item outcomes wrong: %s", r.Body.String())
	}
	body["kind"] = "stream"
	managedTestRequest(t, h, "POST", "/api/accounts/upstream/batch", body, 400)
	if calls.Load() != 1 {
		t.Fatal("batch allowed inference")
	}
}
