package server

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
)

func TestParseImportInputCompleteDocumentsAndJSONL(t *testing.T) {
	for _, test := range []struct {
		name, raw string
		count     int
	}{
		{"pretty", "{\n  \"tokens\": {\n    \"access_token\": \"secret-a\"\n  }\n}", 1},
		{"credentials", `{"name":"named","credentials":{"access_token":"secret-a"}}`, 1},
		{"wrapped-auth", `{"name":"named","credentials":{"tokens":{"access_token":"secret-a","account_id":"workspace"}}}`, 1},
		{"array", `[{"access_token":"secret-a"},{"access_token":"secret-b"}]`, 2},
		{"wrapper", `{"accounts":[{"name":"one","credentials":{"access_token":"secret-a"}}]}`, 1},
		{"jsonl", "{\"access_token\":\"secret-a\"}\n{\"tokens\":{\"access_token\":\"secret-b\"}}\n", 2},
		{"tokens", "opaque-a\nopaque-b\n", 2},
		{"named-tokens", "First account ---- opaque-a\nsecond----opaque-b", 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			items, err := ParseImportInput([]byte(test.raw))
			if err != nil || len(items) != test.count {
				t.Fatalf("count=%d err=%v", len(items), err)
			}
			for _, item := range items {
				if _, err := parseAccount(item.Name, item.Credentials); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestSub2APIImportPreservesCompatibleSettings(t *testing.T) {
	raw := []byte(`{"exported_at":"2026-09-17T20:15:52+08:00","proxies":[],"accounts":[{"name":"imported","platform":"openai","type":"oauth","credentials":{"access_token":"synthetic-access","refresh_token":"synthetic-refresh","chatgpt_account_id":"workspace","client_id":"client","expires_at":1900000000,"model_mapping":{"gpt-6":"gpt-6"}},"notes":"keep these notes","tags":["personal","personal"],"enabled":false,"concurrency":50,"priority":1,"weight":3,"quota":0,"window_days":7,"rate_multiplier":2,"auto_pause_on_expired":true,"extra":{"privacy_mode":"training-disabled"}}]}`)
	items, err := ParseImportInput(raw)
	if err != nil || len(items) != 1 {
		t.Fatalf("Sub2API envelope not accepted: %v", err)
	}
	store := accountTestStore(t)
	defaults := AccountDefaults{Concurrency: 1, Priority: 9, Weight: 1, Quota: 1000, WindowDays: 30, Groups: []string{"local-group"}, Proxies: []string{"local-proxy"}}
	report, err := store.ImportDetailed(items, defaults, ImportOptions{})
	if err != nil || report.Added != 1 || report.Failed != 0 {
		t.Fatalf("import failed: %+v %v", report, err)
	}
	a, err := store.Get("imported")
	if err != nil {
		t.Fatal(err)
	}
	if a.Concurrency != 50 || a.Priority != 1 || a.Weight != 3 || a.Enabled || a.Quota != 0 || a.WindowDays != 7 || a.Notes != "keep these notes" || len(a.Tags) != 1 {
		t.Fatal("imported management settings were replaced by defaults")
	}
	if a.AccountID != "workspace" || a.ClientID != "client" || a.ExpiresAt != 1900000000 || !a.HasRefreshToken || len(a.Groups) != 1 || a.Groups[0] != "local-group" || len(a.Proxies) != 1 || a.Proxies[0] != "local-proxy" {
		t.Fatal("credentials or explicitly selected local bindings were lost")
	}
	if !containsImportWarning(report.Results[0], "sub2api_settings_not_imported") || !containsImportWarning(report.Results[0], "model_mapping_not_imported") {
		t.Fatal("unsupported Sub2API settings were silently discarded")
	}
	encoded, _ := json.Marshal(report)
	if strings.Contains(string(encoded), "synthetic-") || strings.Contains(string(encoded), "training-disabled") {
		t.Fatal("import report disclosed source credentials or extra data")
	}

	overrideStore := accountTestStore(t)
	defaults.OverrideImported = true
	if _, err := overrideStore.ImportDetailed(items, defaults, ImportOptions{}); err != nil {
		t.Fatal(err)
	}
	a, _ = overrideStore.Get("imported")
	if a.Concurrency != 1 || a.Priority != 9 || a.Weight != 1 || a.Quota != 1000 || a.WindowDays != 30 || a.Enabled {
		t.Fatal("explicit override did not apply selected defaults or incorrectly enabled the account")
	}
}

func containsImportWarning(result ImportResult, warning string) bool {
	for _, current := range result.Warnings {
		if current == warning {
			return true
		}
	}
	return false
}

func TestImportReportsInvalidRowsAndKeepsValidAccounts(t *testing.T) {
	for _, raw := range []string{
		`[{"name":"good","access_token":"valid-token"},{"name":"foreign","platform":"anthropic","credentials":{"access_token":"not-openai"}},{"name":"key","platform":"openai","type":"apikey","credentials":{"access_token":"not-oauth"}},{"name":"bad-setting","access_token":"bad-token","concurrency":-1},null]`,
		"{\"name\":\"good\",\"access_token\":\"valid-token\"}\n{\"name\":\"foreign\",\"platform\":\"anthropic\",\"access_token\":\"not-openai\"}\n{\"name\":\"key\",\"type\":\"apikey\",\"access_token\":\"not-oauth\"}\n{\"name\":\"bad-setting\",\"access_token\":\"bad-token\",\"concurrency\":-1}\n{}",
	} {
		items, err := ParseImportInput([]byte(raw))
		if err != nil || len(items) != 5 {
			t.Fatalf("partial validation aborted batch: %v", err)
		}
		store := accountTestStore(t)
		report, err := store.ImportDetailed(items, AccountDefaults{}, ImportOptions{DryRun: true})
		if err != nil || report.Added != 1 || report.Failed != 4 || report.Results[1].Code != "unsupported_platform" || report.Results[2].Code != "unsupported_type" || report.Results[3].Code != "invalid_metadata" || report.Results[4].Code != "invalid_credentials" {
			t.Fatalf("row errors not retained: %+v %v", report, err)
		}
		if _, err := os.Stat(store.Path); !os.IsNotExist(err) {
			t.Fatal("preview wrote a partial batch")
		}
		report, err = store.ImportDetailed(items, AccountDefaults{}, ImportOptions{})
		if err != nil || report.Added != 1 || report.Failed != 4 {
			t.Fatalf("valid rows were not importable: %+v %v", report, err)
		}
		_, accounts, err := store.List()
		if err != nil || len(accounts) != 1 || accounts[0].Name != "good" {
			t.Fatal("failed rows were persisted or valid account was lost")
		}
	}
}

func TestImportEnvelopeValidationAndExternalProxyBindings(t *testing.T) {
	for _, raw := range []string{
		`{"type":"unknown","accounts":[{"access_token":"token"}]}`,
		`{"version":2,"accounts":[{"access_token":"token"}]}`,
		`{"version":null,"accounts":[{"access_token":"token"}]}`,
		`{"accounts":{"access_token":"token"}}`,
		`{"proxies":null,"accounts":[{"access_token":"token"}]}`,
	} {
		if _, err := ParseImportInput([]byte(raw)); err == nil {
			t.Fatal("invalid envelope accepted")
		}
	}
	items, err := ParseImportInput([]byte(`{"type":"sub2api-bundle","version":1,"proxies":[{"proxy_key":"foreign","password":"proxy-secret"}],"accounts":[{"name":"proxied","platform":"openai","type":"oauth","proxy_key":"foreign","group_ids":[42],"credentials":{"access_token":"synthetic-token"}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	store := accountTestStore(t)
	report, err := store.ImportDetailed(items, AccountDefaults{}, ImportOptions{})
	if err != nil || report.Failed != 1 || report.Results[0].Code != "proxy_mapping_required" {
		t.Fatal("an unresolved foreign proxy was silently dropped")
	}
	report, err = store.ImportDetailed(items, AccountDefaults{Proxies: []string{"local-proxy"}}, ImportOptions{})
	if err != nil || report.Added != 1 || !containsImportWarning(report.Results[0], "external_proxy_replaced") || !containsImportWarning(report.Results[0], "external_bindings_not_imported") || !containsImportWarning(report.Results[0], "bundle_proxies_not_imported") {
		t.Fatalf("local proxy choice not applied: %+v %v", report, err)
	}
	a, _ := store.Get("proxied")
	if len(a.Groups) != 0 || len(a.Proxies) != 1 || a.Proxies[0] != "local-proxy" {
		t.Fatal("foreign IDs were copied as local relations")
	}
}

func TestImportIDTokenIdentityAndDistinctAccountExpiry(t *testing.T) {
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"exp":2000000000,"https://api.openai.com/auth":{"chatgpt_account_id":"id-token-workspace"}}`))
	raw, _ := json.Marshal(map[string]any{"platform": "openai", "type": "oauth", "expires_at": 1800000000, "credentials": map[string]any{"access_token": "opaque-token", "id_token": "header." + payload + ".signature"}})
	a, err := parseAccount("named", raw)
	if err != nil || a.AccountID != "id-token-workspace" || a.ExpiresAt != 0 {
		t.Fatal("identity fallback failed or account/id-token expiry was mistaken for access-token expiry")
	}
	items, err := ParseImportInput(raw)
	if err != nil {
		t.Fatal(err)
	}
	_, report, err := prepareImport(items, AccountDefaults{})
	if err != nil || !containsImportWarning(report.Results[0], "account_expiry_not_imported") {
		t.Fatal("ignored account expiry was not reported")
	}
}

func TestWrappedAuthImportKeepsTokenExpiryAndRejectsDeepWrappers(t *testing.T) {
	a, err := parseAccount("named", []byte(`{"credentials":{"expires_at":1900000000,"tokens":{"access_token":"synthetic-token","account_id":"workspace"}}}`))
	if err != nil || a.ExpiresAt != 1900000000 || a.AccountID != "workspace" {
		t.Fatal("historical credentials/auth.json wrapper lost token fields")
	}
	if _, err := parseAccount("named", []byte(`{"credentials":{"platform":"anthropic","credentials":{"access_token":"synthetic-token"}}}`)); err == nil {
		t.Fatal("nested credentials bypassed the supported platform check")
	}
	raw := `{"access_token":"synthetic-token"}`
	for range 8 {
		raw = `{"credentials":` + raw + `}`
	}
	if _, err := parseAccount("named", []byte(raw)); err != nil {
		t.Fatal("eight credential wrappers should be accepted")
	}
	raw = `{"credentials":` + raw + `}`
	if _, err := parseAccount("named", []byte(raw)); err == nil {
		t.Fatal("excessively nested credentials accepted")
	}
}

func TestImportRejectsInvalidMetadataAndDefaults(t *testing.T) {
	for _, field := range []string{`"concurrency":0`, `"concurrency":1001`, `"priority":2147483648`, `"weight":-1`, `"quota":-1`, `"window_days":366`, `"enabled":null`, `"notes":123`, `"tags":[true]`, `"expires_at":1.5`, `"expires_at":-1`, `"access_token":"contains whitespace"`} {
		if _, err := parseAccount("named", []byte(`{"access_token":"valid-token",`+field+`}`)); err == nil {
			t.Fatalf("invalid field accepted: %s", field)
		}
	}
	items, _ := ParseImportInput([]byte(`{"access_token":"valid-token"}`))
	if _, _, err := prepareImport(items, AccountDefaults{Concurrency: -1}); err == nil {
		t.Fatal("invalid defaults were silently normalized")
	}
}

func TestAdminStructuredSub2APIImportPreservesMetadataAndReportsRows(t *testing.T) {
	p, handler := webTestSetup(t)
	body := `{"accounts":[{"name":"migrated","platform":"openai","type":"oauth","concurrency":50,"priority":1,"notes":"retained","credentials":{"access_token":"synthetic-import-secret","refresh_token":"synthetic-refresh-secret","expires_at":1900000000}},{"name":"unsupported","platform":"anthropic","credentials":{"access_token":"synthetic-foreign-secret"}}],"defaults":{"concurrency":1,"priority":0,"weight":1,"window_days":30}}`
	for _, path := range []string{"/api/accounts/import/preview", "/api/accounts"} {
		response := webTestRequest(handler, http.MethodPost, path, body, "Bearer "+webTestKey)
		var report ImportReport
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &report) != nil || report.Added != 1 || report.Failed != 1 || report.Results[1].Code != "unsupported_platform" {
			t.Fatalf("structured batch failed: status=%d body=%s", response.Code, response.Body.String())
		}
		if strings.Contains(response.Body.String(), "synthetic-") {
			t.Fatal("API import report disclosed credentials")
		}
		_, accounts, err := p.Store.List()
		want := 1
		if path == "/api/accounts" {
			want = 2
		}
		if err != nil || len(accounts) != want {
			t.Fatalf("preview/import account count=%d want=%d", len(accounts), want)
		}
	}
	a, err := p.Store.Get("migrated")
	if err != nil || a.Concurrency != 50 || a.Priority != 1 || a.Notes != "retained" || a.ExpiresAt != 1900000000 {
		t.Fatal("API lost source management configuration")
	}
}

func TestPGSub2APIImportMetadataAndPartialValidation(t *testing.T) {
	s := accountPGTestStore(t)
	items, err := ParseImportInput([]byte(`{"accounts":[{"name":"migrated","platform":"openai","type":"oauth","concurrency":50,"priority":1,"enabled":false,"notes":"retained","tags":["local"],"credentials":{"access_token":"synthetic-import-secret","refresh_token":"synthetic-refresh-secret","expires_at":1900000000}},{"name":"invalid","access_token":"bad","concurrency":-1}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defaults := AccountDefaults{Concurrency: 1, Priority: 0, Weight: 1, WindowDays: 30}
	for _, dryRun := range []bool{true, false} {
		report, err := s.ImportDetailed(items, defaults, ImportOptions{DryRun: dryRun})
		if err != nil || report.Added != 1 || report.Failed != 1 {
			t.Fatalf("PostgreSQL import failed: %+v %v", report, err)
		}
		_, accounts, err := s.List()
		want := 1
		if dryRun {
			want = 0
		}
		if err != nil || len(accounts) != want {
			t.Fatalf("PostgreSQL preview/import count=%d want=%d", len(accounts), want)
		}
	}
	a, err := s.Get("migrated")
	if err != nil || a.Concurrency != 50 || a.Priority != 1 || a.Enabled || a.Notes != "retained" || len(a.Tags) != 1 || a.ExpiresAt != 1900000000 || !a.HasRefreshToken {
		t.Fatal("PostgreSQL lost imported settings")
	}
}

func TestParseImportInputRejectsDamagedJSONWithoutLeaking(t *testing.T) {
	for _, raw := range []string{`{"access_token":"secret-value"`, "{\n\"access_token\":\"secret-value\"\n", `{"access_token":"secret-value"} trailing`, "{\"access_token\":\"secret-value\"}\n{broken}", `null`, `true`, `123`, `"secret-value"`, `[]`, `{}`, `{"tokens":null}`, `{"credentials":{"access_token":42}}`} {
		items, err := ParseImportInput([]byte(raw))
		if err == nil {
			t.Fatalf("invalid document accepted with %d rows", len(items))
		}
		if strings.Contains(err.Error(), "secret-value") {
			t.Fatal("error disclosed credential")
		}
	}
}

func TestImportDetailedConflictsAndDryRun(t *testing.T) {
	store := accountTestStore(t)
	items := []ImportItem{{Name: "one", Credentials: json.RawMessage(`{"access_token":"a","account_id":"shared-workspace"}`)}, {Name: "one", Credentials: json.RawMessage(`{"access_token":"b","account_id":"shared-workspace"}`)}, {Name: "two", Credentials: json.RawMessage(`{"access_token":"b","account_id":"shared-workspace"}`)}, {Name: "again", Credentials: json.RawMessage(`{"access_token":"a"}`)}, {Name: "broken", Credentials: json.RawMessage(`{`)}}
	report, err := store.ImportDetailed(items, AccountDefaults{}, ImportOptions{DryRun: true})
	if err != nil || report.Added != 2 || report.Duplicate != 1 || report.Failed != 2 {
		t.Fatalf("unexpected preview: %+v %v", report, err)
	}
	if _, err := os.Stat(store.Path); !os.IsNotExist(err) {
		t.Fatal("dry run persisted accounts")
	}
	report, err = store.ImportDetailed(items, AccountDefaults{}, ImportOptions{})
	if err != nil || report.Added != 2 || report.Failed != 2 {
		t.Fatalf("unexpected import: %+v %v", report, err)
	}
	first, err := store.Get("one")
	if err != nil || first.AccessToken != "a" {
		t.Fatal("same-name token was silently replaced")
	}
	report, err = store.ImportDetailed([]ImportItem{{Name: "one", Credentials: json.RawMessage(`{"access_token":"replacement"}`)}}, AccountDefaults{}, ImportOptions{Replace: true})
	if err != nil || report.Replaced != 1 {
		t.Fatalf("explicit replacement failed: %+v %v", report, err)
	}
	updated, _ := store.Get("one")
	if updated.ID != first.ID || updated.AccessToken != "replacement" {
		t.Fatal("replacement lost stable identity")
	}
	encoded, _ := json.Marshal(report)
	if strings.Contains(string(encoded), "replacement") {
		t.Fatal("report disclosed token")
	}
}

func TestAccountMetadataPaginationAndVersion(t *testing.T) {
	store := accountTestStore(t)
	for _, name := range []string{"one", "two", "three"} {
		accountTestImport(t, store, name, "secret-"+name)
	}
	page, err := store.ListPage(AccountListOptions{Limit: 2})
	if err != nil || len(page.Accounts) != 2 || !page.HasMore || page.NextCursor == "" {
		t.Fatalf("unexpected first page: %+v %v", page, err)
	}
	for _, a := range page.Accounts {
		if a.AccessToken != "" || a.RefreshToken != "" {
			t.Fatal("metadata contains credentials")
		}
	}
	second, err := store.ListPage(AccountListOptions{Limit: 2, Cursor: page.NextCursor})
	if err != nil || len(second.Accounts) != 1 || second.HasMore {
		t.Fatalf("unexpected second page: %+v %v", second, err)
	}
	a := page.Accounts[0]
	updated, err := store.UpdateAccountVersion(a.ID, a.Version, func(a *Account) error { a.Notes = "persisted"; return nil })
	if err != nil || updated.Version != a.Version+1 {
		t.Fatalf("version update failed: %v", err)
	}
	if _, err := store.UpdateAccountVersion(a.ID, a.Version, func(a *Account) error { return nil }); err != errVersionConflict {
		t.Fatalf("stale update accepted: %v", err)
	}
}
