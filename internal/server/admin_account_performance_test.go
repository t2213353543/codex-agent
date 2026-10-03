package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

func TestManagedAccountETagRepresentation(t *testing.T) {
	p, handler := webTestSetup(t)
	managedTestAccount(t, p, "etag-<safe>&", nil)
	path := "/api/accounts/manage?search=etag-"
	first := managedTestRequest(t, handler, http.MethodGet, path, nil, http.StatusOK)
	body := first.Body.Bytes()
	if !bytes.HasSuffix(body, []byte{'\n'}) || !json.Valid(body) || first.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("changed JSON representation: %s", body)
	}
	digest := sha256.Sum256(bytes.TrimSuffix(body, []byte{'\n'}))
	wantETag := `"` + hex.EncodeToString(digest[:]) + `"`
	if first.Header().Get("ETag") != wantETag {
		t.Fatal("ETag does not describe the returned JSON")
	}
	for _, value := range []string{wantETag, "W/" + wantETag, `"unrelated", W/` + wantETag, "*"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+webTestKey)
		req.Header.Set("If-None-Match", value)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		if response.Code != http.StatusNotModified || response.Body.Len() != 0 || response.Header().Get("ETag") != wantETag {
			t.Fatalf("conditional request %q: %d %s", value, response.Code, response.Body.String())
		}
	}
}

func TestPGManagedAccountUsageMatchesStableIDBatch(t *testing.T) {
	p, _ := adminPGTestProxy(t)
	a := managedTestAccount(t, p, "usage-current", nil)
	// Import under a distinct name first: managedTestAccount resolves its result
	// with Store.Get(name), which intentionally gives stable IDs precedence.
	b := managedTestAccount(t, p, "usage-other", nil)
	b, err := p.Store.UpdateAccountVersion(b.ID, b.Version, func(account *Account) error {
		account.Name = a.ID // A display name must never shadow a stable ID.
		return nil
	})
	if err != nil || a.ID == b.ID {
		t.Fatalf("independent account fixture: %v", err)
	}
	ctx := context.Background()
	day := time.Now().Unix() / 86400 * 86400
	records := []RequestRecord{
		{EventID: "summary-before-window", Time: day - 1, AccountID: a.ID, Account: "old-name", Status: "ok", InputTokens: 1000},
		{EventID: "summary-start-day", Time: day + 1, AccountID: a.ID, Account: "old-name", Status: "ok", InputTokens: 100, OutputTokens: 20, CachedTokens: 80, LatencyMs: 50},
		{EventID: "summary-error", Time: day + 2, AccountID: a.ID, Account: "old-name", Status: "error", InputTokens: 10, LatencyMs: 30},
		{EventID: "summary-other-id", Time: day + 3, AccountID: b.ID, Account: a.ID, Status: "ok", InputTokens: 999},
	}
	if _, _, err := p.Metrics.ImportRecords(ctx, records); err != nil {
		t.Fatal(err)
	}
	starts := map[string]int64{a.ID: day + 3600, b.ID: day + 86400, "missing-stable-id": day}
	want, err := p.Metrics.BatchAccountMetrics(ctx, starts)
	if err != nil {
		t.Fatal(err)
	}
	got, err := p.Metrics.managedAccountUsage(ctx, starts)
	if err != nil {
		t.Fatal(err)
	}
	for id, value := range want {
		if !reflect.DeepEqual(got[id].Usage, value.Usage) {
			t.Fatalf("stable usage changed for %s: got=%+v want=%+v", id, got[id].Usage, value.Usage)
		}
	}
	if got[a.ID].Usage.Tokens != 130 || got[a.ID].Usage.Requests != 2 || got[b.ID].Usage.Requests != 0 {
		t.Fatalf("window rounding or ID isolation changed: %+v", got)
	}
	if _, err = p.Store.DB.Pool.Exec(ctx, "DROP TABLE account_request_state"); err != nil {
		t.Fatal(err)
	}
	if _, err = p.Metrics.managedAccountUsage(ctx, starts); err != nil {
		t.Fatalf("usage still depends on unrelated historical request details: %v", err)
	}
}

func TestManagedBundleResourceMetadataReadOnly(t *testing.T) {
	exerciseManagedBundleResourceMetadataReadOnly(t, webTestSetup)
}

func TestPGManagedBundleResourceMetadataReadOnly(t *testing.T) {
	exerciseManagedBundleResourceMetadataReadOnly(t, adminPGTestProxy)
}

func exerciseManagedBundleResourceMetadataReadOnly(t *testing.T, setup func(*testing.T) (*Proxy, http.Handler)) {
	p, _ := setup(t)
	v, err := p.Proxies.Create(ProxyEntry{Name: "readonly-export-proxy", Type: "http", Host: "127.0.0.1", Port: 12345, Username: "synthetic-user", Password: "synthetic-password", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	a := managedTestAccount(t, p, "readonly-export-account", func(a *Account) { a.Proxies = []string{v.ID} })
	assertUnchanged := func(read func() error) {
		t.Helper()
		before, found := p.Proxies.Get(v.ID)
		if !found || before.Password != v.Password {
			t.Fatal("fixture has no live proxy credentials")
		}
		if err := read(); err != nil {
			t.Fatal(err)
		}
		after, found := p.Proxies.Get(v.ID)
		if !found || after != before || after.Password != v.Password {
			t.Fatal("metadata read rebuilt or redacted the live proxy index")
		}
	}
	assertUnchanged(func() error {
		_, err := p.accountSummaries(httptest.NewRequest(http.MethodGet, "/", nil), []Account{a})
		return err
	})
	assertUnchanged(func() error {
		resources, err := p.exportManagedBundleResources(t.Context(), []Account{a}, false)
		if err == nil && (len(resources.Proxies) != 1 || !resources.Proxies[0].HasCredentials || resources.Proxies[0].CredentialsIncluded || resources.Proxies[0].Password != "" || resources.Proxies[0].Username != "") {
			t.Fatalf("metadata export credential contract changed: %+v", resources)
		}
		return err
	})
	resources, err := p.exportManagedBundleResources(t.Context(), []Account{a}, true)
	if err != nil || len(resources.Proxies) != 1 || !resources.Proxies[0].CredentialsIncluded || resources.Proxies[0].Password != v.Password || resources.Proxies[0].Username != v.Username {
		t.Fatalf("explicit credential export changed: %+v %v", resources, err)
	}
	if p.Store.DB != nil {
		if _, err := p.Store.DB.Pool.Exec(context.Background(), "UPDATE proxies SET password_cipher='invalid-ciphertext'::bytea WHERE id=$1", v.ID); err != nil {
			t.Fatal(err)
		}
		assertUnchanged(func() error {
			_, err := p.exportManagedBundleResources(t.Context(), []Account{a}, false)
			return err
		})
		if _, err := p.exportManagedBundleResources(t.Context(), []Account{a}, true); err == nil {
			t.Fatal("credential export silently ignored invalid credentials")
		}
	}
}
