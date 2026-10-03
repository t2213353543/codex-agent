package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

// codexCLIImportTestAccount exercises the same preview and import endpoints as
// the admin UI, without writing user credentials into any existing deployment.
// Refresh is intentionally disabled after import, so this acceptance run cannot
// rotate the user's refresh token and lose the updated credential at cleanup.
func codexCLIImportTestAccount(t *testing.T, p *Proxy, filename string) {
	t.Helper()
	raw, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal("cannot read the explicitly supplied account export file")
	}
	items, err := ParseImportInput(raw)
	if err != nil || len(items) != 1 {
		t.Fatal("live acceptance requires an export containing exactly one valid account")
	}
	parsed, err := parseAccount(items[0].Name, items[0].Credentials)
	if err != nil || parsed.ExpiresAt <= time.Now().Add(20*time.Minute).Unix() {
		t.Fatal("live acceptance requires an account with at least 20 minutes remaining; refresh is disabled for this test")
	}
	if err := p.Store.Delete("test"); err != nil {
		t.Fatal("cannot clear the synthetic account from the temporary store")
	}
	body, err := json.Marshal(map[string]any{"raw": string(raw)})
	if err != nil {
		t.Fatal("cannot encode the account import request")
	}
	for _, step := range []struct {
		path   string
		dryRun bool
		count  int
	}{
		{"/api/accounts/import/preview", true, 0},
		{"/api/accounts", false, 1},
	} {
		request := httptest.NewRequest(http.MethodPost, step.path, bytes.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer test-client-key")
		response := httptest.NewRecorder()
		p.adminRoutes().ServeHTTP(response, request)
		var result struct {
			Added  int  `json:"added"`
			Failed int  `json:"failed"`
			DryRun bool `json:"dry_run"`
		}
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &result) != nil ||
			result.Added != 1 || result.Failed != 0 || result.DryRun != step.dryRun {
			t.Fatalf("account import API returned HTTP %d, added=%d failed=%d dry_run=%t; expected added=1 failed=0 dry_run=%t (response omitted)", response.Code, result.Added, result.Failed, result.DryRun, step.dryRun)
		}
		_, accounts, err := p.Store.List()
		if err != nil || len(accounts) != step.count {
			t.Fatal("account preview/import did not preserve its persistence contract")
		}
	}
	if err := p.Store.UpdateAccount(items[0].Name, func(account *Account) error {
		if account.AccessToken != parsed.AccessToken || account.AccountID != parsed.AccountID ||
			account.RefreshToken != parsed.RefreshToken || account.ExpiresAt != parsed.ExpiresAt {
			t.Error("import changed credential fields (values omitted)")
		}
		account.RefreshToken = ""
		account.HasRefreshToken = false
		return nil
	}); err != nil {
		t.Fatal("cannot disable credential refresh in the temporary acceptance store")
	}
	if t.Failed() {
		t.FailNow()
	}
}
