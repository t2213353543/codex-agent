package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// Measure only OAuth's local staging/apply/cancel operations. External code
// exchange, refresh-token and PAT validation require separate upstream timing.
func TestPerformanceOAuthLocalLifecycle(t *testing.T) {
	if os.Getenv("PERFORMANCE_SLO") != "1" {
		t.Skip("isolated synthetic PostgreSQL HTTP runner required")
	}
	p, handler := adminPGTestProxy(t)
	deny := new(localWriteNoUpstream)
	p.Client = &http.Client{Transport: deny}
	server := httptest.NewServer(handler)
	defer server.Close()
	client := &http.Client{Timeout: 15 * time.Second}
	defer client.CloseIdleConnections()
	request := func(name, method, path string, body any) map[string]any {
		t.Helper()
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		req, _ := http.NewRequest(method, server.URL+path, bytes.NewReader(raw))
		req.Header.Set("Authorization", "Bearer "+webTestKey)
		req.Header.Set("Content-Type", "application/json")
		before := p.Store.DB.Pool.Stat().AcquireCount()
		start := time.Now()
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		data, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		elapsed := float64(time.Since(start).Microseconds()) / 1000
		var result map[string]any
		failure := ""
		if readErr != nil || resp.StatusCode != 200 || json.Unmarshal(data, &result) != nil || strings.Contains(string(data), "synthetic-private-oauth") {
			failure = "response_contract"
		}
		performanceSLOEmit(t, "ACCOUNT_WRITE", map[string]any{"name": name, "method": method, "path": path, "sample": map[string]any{"ms": elapsed, "status": resp.StatusCode, "error": failure, "db_acquires": p.Store.DB.Pool.Stat().AcquireCount() - before}, "threshold_ms": 1000, "slo_pass": failure == "" && elapsed < 1000, "upstream_calls": deny.calls.Load(), "scope": "authenticated complete loopback HTTP; OAuth session import only, no external token exchange"})
		if failure != "" {
			t.Fatal(failure)
		}
		if elapsed >= 1000 {
			t.Errorf("%s exceeded1s: %.3fms", name, elapsed)
		}
		return result
	}
	for sample := 0; sample < 3; sample++ {
		started := request("oauth_start_local", "POST", "/api/account-oauth/start", map[string]any{})
		id, ok := started["session_id"].(string)
		if !ok || id == "" {
			t.Fatal("start omitted session")
		}
		canceled := request("oauth_cancel_local", "DELETE", "/api/account-oauth/"+id, nil)
		if canceled["ok"] != true {
			t.Fatal("cancel did not acknowledge")
		}
		entry, err := p.managementState().Get(t.Context(), managedOAuthNamespace, id)
		if err != nil {
			t.Fatal(err)
		}
		var stored managedOAuthSession
		if json.Unmarshal(entry.Payload, &stored) != nil || stored.Phase != "canceled" || stored.Verifier != "" || len(stored.Credentials) != 0 {
			t.Fatal("cancel retained staged secrets")
		}
		token := fmt.Sprintf("synthetic-private-oauth-%d", sample)
		staged := request("oauth_import_session_local", "POST", "/api/account-oauth/import", map[string]any{"method": "session", "credentials": map[string]any{"accessToken": token, "account": map[string]string{"id": fmt.Sprintf("synthetic-workspace-%d", sample)}, "expires": "2030-01-01T00:00:00Z"}})
		id, ok = staged["session_id"].(string)
		if !ok || id == "" || staged["phase"] != "ready" {
			t.Fatal("session was not staged")
		}
		name := fmt.Sprintf("local-oauth-scale-%d", sample)
		input := map[string]any{"session_id": id, "name": name, "enabled": false}
		applied := request("oauth_apply_local", "POST", "/api/account-oauth/apply", input)
		account, err := p.Store.Get(name)
		if err != nil || account.ID != applied["id"] || account.AccessToken != token || account.Enabled {
			t.Fatal("apply did not durably preserve local account")
		}
		replay := request("oauth_apply_idempotent", "POST", "/api/account-oauth/apply", input)
		after, err := p.Store.Get(name)
		if err != nil || after.Version != account.Version || replay["id"] != account.ID {
			t.Fatal("replay wrote another account revision")
		}
	}
	if deny.calls.Load() != 0 {
		t.Fatal("local OAuth path contacted upstream")
	}
}
